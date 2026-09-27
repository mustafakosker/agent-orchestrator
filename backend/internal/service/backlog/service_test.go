package backlog_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	trackerjira "github.com/aoagents/agent-orchestrator/backend/internal/adapters/tracker/jira"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	backlogsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/backlog"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeTracker struct {
	calls  atomic.Int32
	mu     sync.Mutex
	issues []domain.Issue
	err    error
	// block, when non-nil, holds every call until closed. Used to force real
	// concurrency for the single-flight test.
	block chan struct{}
	last  domain.ListFilter
	repos []domain.TrackerRepo
}

func (f *fakeTracker) Get(context.Context, domain.TrackerID) (domain.Issue, error) {
	return domain.Issue{}, errors.New("backlog must never call Get")
}

func (f *fakeTracker) List(_ context.Context, repo domain.TrackerRepo, filter domain.ListFilter) ([]domain.Issue, error) {
	f.calls.Add(1)
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	f.last = filter
	f.repos = append(f.repos, repo)
	issues, err := f.issues, f.err
	f.mu.Unlock()
	return issues, err
}

func (f *fakeTracker) Preflight(context.Context) error { return nil }

func (f *fakeTracker) setResult(issues []domain.Issue, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues, f.err = issues, err
}

func (f *fakeTracker) lastFilter() domain.ListFilter {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

func (f *fakeTracker) seenRepos() []domain.TrackerRepo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.TrackerRepo(nil), f.repos...)
}

type fakeStore struct {
	projects []domain.ProjectRecord
	err      error
}

func (s *fakeStore) GetProject(_ context.Context, id string) (domain.ProjectRecord, bool, error) {
	if s.err != nil {
		return domain.ProjectRecord{}, false, s.err
	}
	for _, p := range s.projects {
		if p.ID == id {
			return p, true, nil
		}
	}
	return domain.ProjectRecord{}, false, nil
}

func (s *fakeStore) ListProjects(context.Context) ([]domain.ProjectRecord, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.projects, nil
}

func enabledProject(id string) domain.ProjectRecord {
	return domain.ProjectRecord{
		ID:          id,
		DisplayName: id,
		Config: domain.ProjectConfig{
			TrackerBacklog: domain.TrackerBacklogConfig{
				Enabled:    true,
				Provider:   domain.TrackerProviderJira,
				Site:       "acme.atlassian.net",
				ProjectKey: "ACME",
				Statuses:   []string{"Ready", "In Review"},
			},
		},
	}
}

func issues(keys ...string) []domain.Issue {
	out := make([]domain.Issue, 0, len(keys))
	for _, k := range keys {
		out = append(out, domain.Issue{
			ID:    domain.TrackerID{Provider: domain.TrackerProviderJira, Native: k, Host: "acme.atlassian.net"},
			Title: "title " + k,
			State: domain.IssueOpen,
			URL:   "https://acme.atlassian.net/browse/" + k,
		})
	}
	return out
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *testClock {
	return &testClock{now: time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)}
}
func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newService(t *testing.T, tr *fakeTracker, store *fakeStore, clk *testClock) *backlogsvc.Service {
	t.Helper()
	return backlogsvc.New(backlogsvc.Deps{Tracker: tr, Store: store, Clock: clk.Now})
}

// ---------------------------------------------------------------------------
// Happy path + the filter the service builds
// ---------------------------------------------------------------------------

func TestReadyReturnsIssuesAndScopesTheQuery(t *testing.T) {
	tr := &fakeTracker{}
	tr.setResult(issues("ACME-1", "ACME-2"), nil)
	clk := newClock()
	svc := newService(t, tr, &fakeStore{projects: []domain.ProjectRecord{enabledProject("p1")}}, clk)

	got, err := svc.Ready(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if got.State != backlogsvc.StateOK {
		t.Fatalf("State = %q, want ok", got.State)
	}
	if len(got.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(got.Items))
	}
	if got.Stale {
		t.Fatal("Stale = true on a fresh fetch")
	}
	if !got.FetchedAt.Equal(clk.Now()) {
		t.Fatalf("FetchedAt = %v, want the fetch time %v", got.FetchedAt, clk.Now())
	}

	// The configured site, project key, and statuses must all reach the tracker.
	repos := tr.seenRepos()
	if len(repos) != 1 {
		t.Fatalf("tracker saw %d repos, want 1", len(repos))
	}
	if repos[0].Provider != domain.TrackerProviderJira ||
		repos[0].Native != "ACME" || repos[0].Host != "acme.atlassian.net" {
		t.Fatalf("repo = %#v", repos[0])
	}
	f := tr.lastFilter()
	if len(f.Statuses) != 2 || f.Statuses[0] != "Ready" || f.Statuses[1] != "In Review" {
		t.Fatalf("filter.Statuses = %#v, want the configured statuses", f.Statuses)
	}
}

// ---------------------------------------------------------------------------
// Cache + single flight
// ---------------------------------------------------------------------------

// N concurrent callers inside one TTL window must produce exactly ONE provider
// call. This is the load-bearing property: it decouples how often the UI asks
// from how often AO calls Jira.
func TestReadyConcurrentCallersProduceExactlyOneProviderCall(t *testing.T) {
	tr := &fakeTracker{block: make(chan struct{})}
	tr.setResult(issues("ACME-1"), nil)
	svc := newService(t, tr, &fakeStore{projects: []domain.ProjectRecord{enabledProject("p1")}}, newClock())

	const n = 24
	var wg sync.WaitGroup
	results := make([]backlogsvc.Result, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = svc.Ready(context.Background(), "p1")
		}(i)
	}
	// Let every goroutine reach the tracker call before releasing it.
	time.Sleep(50 * time.Millisecond)
	close(tr.block)
	wg.Wait()

	if got := tr.calls.Load(); got != 1 {
		t.Fatalf("tracker calls = %d, want exactly 1 for %d concurrent callers", got, n)
	}
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("caller %d err = %v", i, errs[i])
		}
		if len(results[i].Items) != 1 {
			t.Fatalf("caller %d got %d items, want the shared result", i, len(results[i].Items))
		}
	}
}

func TestReadyServesCacheInsideTTLAndRefetchesAfter(t *testing.T) {
	tr := &fakeTracker{}
	tr.setResult(issues("ACME-1"), nil)
	clk := newClock()
	svc := newService(t, tr, &fakeStore{projects: []domain.ProjectRecord{enabledProject("p1")}}, clk)

	for i := 0; i < 5; i++ {
		if _, err := svc.Ready(context.Background(), "p1"); err != nil {
			t.Fatalf("Ready #%d: %v", i, err)
		}
		clk.advance(10 * time.Second)
	}
	if got := tr.calls.Load(); got != 1 {
		t.Fatalf("tracker calls = %d within the 60s TTL, want 1", got)
	}

	clk.advance(30 * time.Second) // now past 60s total
	if _, err := svc.Ready(context.Background(), "p1"); err != nil {
		t.Fatalf("Ready after TTL: %v", err)
	}
	if got := tr.calls.Load(); got != 2 {
		t.Fatalf("tracker calls = %d after the TTL expired, want 2", got)
	}
}

// ---------------------------------------------------------------------------
// Failure behavior
// ---------------------------------------------------------------------------

// A failure with a warm cache serves the stale data with a truthful fetchedAt.
func TestReadyWarmCacheFailureServesStale(t *testing.T) {
	tr := &fakeTracker{}
	tr.setResult(issues("ACME-1"), nil)
	clk := newClock()
	svc := newService(t, tr, &fakeStore{projects: []domain.ProjectRecord{enabledProject("p1")}}, clk)

	first, err := svc.Ready(context.Background(), "p1")
	if err != nil {
		t.Fatalf("warm the cache: %v", err)
	}
	fetchedAt := first.FetchedAt

	clk.advance(2 * time.Minute)
	tr.setResult(nil, errors.New("jira tracker: 503 unavailable"))

	got, err := svc.Ready(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Ready with a warm cache must not error: %v", err)
	}
	if got.State != backlogsvc.StateOK {
		t.Fatalf("State = %q, want ok with stale data", got.State)
	}
	if !got.Stale {
		t.Fatal("Stale = false, want true when serving a retained entry after a failed refresh")
	}
	if len(got.Items) != 1 {
		t.Fatalf("got %d items, want the retained entry", len(got.Items))
	}
	if !got.FetchedAt.Equal(fetchedAt) {
		t.Fatalf("FetchedAt = %v, want the ORIGINAL fetch time %v, never the failed refresh", got.FetchedAt, fetchedAt)
	}
}

// Only a cold cache surfaces an error state.
func TestReadyColdCacheFailureReturnsErrorState(t *testing.T) {
	tr := &fakeTracker{}
	tr.setResult(nil, errors.New("jira tracker: 503 unavailable"))
	svc := newService(t, tr, &fakeStore{projects: []domain.ProjectRecord{enabledProject("p1")}}, newClock())

	got, err := svc.Ready(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Ready should report state, not a Go error: %v", err)
	}
	if got.State != backlogsvc.StateError {
		t.Fatalf("State = %q, want error", got.State)
	}
	if got.Reason != backlogsvc.ReasonUnavailable {
		t.Fatalf("Reason = %q, want unavailable", got.Reason)
	}
	if len(got.Items) != 0 {
		t.Fatalf("got %d items on a cold failure, want 0", len(got.Items))
	}
}

func TestReadyMapsProviderErrorsToReasons(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want backlogsvc.Reason
	}{
		{"auth failed", fmt.Errorf("wrapped: %w", trackerjira.ErrAuthFailed), backlogsvc.ReasonAuthFailed},
		{"bad query is a config error", fmt.Errorf("wrapped: %w", trackerjira.ErrBadQuery), backlogsvc.ReasonConfigInvalid},
		{"bad project key is a config error", fmt.Errorf("wrapped: %w", trackerjira.ErrBadKey), backlogsvc.ReasonConfigInvalid},
		{"host not allowed is a config error", fmt.Errorf("wrapped: %w", trackerjira.ErrHostNotAllowed), backlogsvc.ReasonConfigInvalid},
		{"missing credential is not_configured", fmt.Errorf("wrapped: %w", trackerjira.ErrNoToken), backlogsvc.ReasonCredentialsMissing},
		{"rate limited", fmt.Errorf("wrapped: %w", trackerjira.ErrRateLimited), backlogsvc.ReasonUnavailable},
		{"anything else", errors.New("boom"), backlogsvc.ReasonUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := &fakeTracker{}
			tr.setResult(nil, tt.err)
			svc := newService(t, tr, &fakeStore{projects: []domain.ProjectRecord{enabledProject("p1")}}, newClock())

			got, err := svc.Ready(context.Background(), "p1")
			if err != nil {
				t.Fatalf("Ready: %v", err)
			}
			if got.Reason != tt.want {
				t.Fatalf("Reason = %q, want %q", got.Reason, tt.want)
			}
			if tt.want == backlogsvc.ReasonCredentialsMissing {
				if got.State != backlogsvc.StateNotConfigured {
					t.Fatalf("State = %q, want not_configured for a missing credential", got.State)
				}
			} else if got.State != backlogsvc.StateError {
				t.Fatalf("State = %q, want error", got.State)
			}
		})
	}
}

// A 400 must never look like an empty successful result: "zero tickets" and
// "wrong project key" are indistinguishable on screen otherwise.
func TestReadyConfigErrorIsNotAnEmptyOKResult(t *testing.T) {
	tr := &fakeTracker{}
	tr.setResult(nil, fmt.Errorf("jira tracker: query rejected: %w", trackerjira.ErrBadQuery))
	svc := newService(t, tr, &fakeStore{projects: []domain.ProjectRecord{enabledProject("p1")}}, newClock())

	got, err := svc.Ready(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if got.State == backlogsvc.StateOK {
		t.Fatal("a rejected query returned state ok — indistinguishable from zero tickets")
	}
	if got.Message == "" {
		t.Fatal("Message is empty, want the provider message so the error names the real problem")
	}
}

// A rejected credential is not transient: the service must back off rather than
// retry at the normal cadence.
func TestReadyAuthFailureBacksOff(t *testing.T) {
	tr := &fakeTracker{}
	tr.setResult(nil, fmt.Errorf("wrapped: %w", trackerjira.ErrAuthFailed))
	clk := newClock()
	svc := newService(t, tr, &fakeStore{projects: []domain.ProjectRecord{enabledProject("p1")}}, clk)

	if _, err := svc.Ready(context.Background(), "p1"); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if got := tr.calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}

	// Inside the backoff window a second call must not hit the provider again.
	clk.advance(90 * time.Second) // past the normal 60s TTL
	got, err := svc.Ready(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if n := tr.calls.Load(); n != 1 {
		t.Fatalf("calls = %d after %v, want still 1 (auth failure must back off past the TTL)", n, 90*time.Second)
	}
	if got.Reason != backlogsvc.ReasonAuthFailed {
		t.Fatalf("Reason = %q, want auth_failed to persist through the backoff", got.Reason)
	}

	// Past the backoff it retries.
	clk.advance(backlogsvc.AuthBackoff)
	if _, err := svc.Ready(context.Background(), "p1"); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if n := tr.calls.Load(); n != 2 {
		t.Fatalf("calls = %d past the backoff window, want 2", n)
	}
}

// 429 must honor Retry-After, extend the TTL, and keep serving stale data.
func TestReadyRateLimitExtendsTTLAndKeepsServingStale(t *testing.T) {
	tr := &fakeTracker{}
	tr.setResult(issues("ACME-1"), nil)
	clk := newClock()
	svc := newService(t, tr, &fakeStore{projects: []domain.ProjectRecord{enabledProject("p1")}}, clk)

	if _, err := svc.Ready(context.Background(), "p1"); err != nil {
		t.Fatalf("warm the cache: %v", err)
	}

	clk.advance(2 * time.Minute)
	tr.setResult(nil, &trackerjira.RateLimitError{
		RetryAfter: 10 * time.Minute,
		Sentinel:   trackerjira.ErrRateLimited,
		Message:    "slow down",
	})
	got, err := svc.Ready(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !got.Stale || len(got.Items) != 1 {
		t.Fatalf("Result = %+v, want the retained entry served as stale", got)
	}
	callsAfterLimit := tr.calls.Load()

	// Inside the Retry-After window the provider must not be called again.
	clk.advance(5 * time.Minute)
	if _, err := svc.Ready(context.Background(), "p1"); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if n := tr.calls.Load(); n != callsAfterLimit {
		t.Fatalf("calls = %d inside the Retry-After window, want still %d", n, callsAfterLimit)
	}

	// Past it, the TTL applies again.
	clk.advance(6 * time.Minute)
	tr.setResult(issues("ACME-1", "ACME-2"), nil)
	fresh, err := svc.Ready(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if fresh.Stale || len(fresh.Items) != 2 {
		t.Fatalf("Result = %+v, want a fresh refetch past the Retry-After window", fresh)
	}
}

// ---------------------------------------------------------------------------
// Not configured
// ---------------------------------------------------------------------------

func TestReadyDisabledOrAbsentConfigMakesZeroTrackerCalls(t *testing.T) {
	disabled := domain.ProjectRecord{ID: "p1"}
	partial := domain.ProjectRecord{ID: "p2", Config: domain.ProjectConfig{
		TrackerBacklog: domain.TrackerBacklogConfig{Enabled: false, ProjectKey: "ACME"},
	}}

	for _, p := range []domain.ProjectRecord{disabled, partial} {
		tr := &fakeTracker{}
		tr.setResult(issues("ACME-1"), nil)
		svc := newService(t, tr, &fakeStore{projects: []domain.ProjectRecord{p}}, newClock())

		got, err := svc.Ready(context.Background(), p.ID)
		if err != nil {
			t.Fatalf("Ready(%s): %v", p.ID, err)
		}
		if got.State != backlogsvc.StateNotConfigured {
			t.Fatalf("State = %q, want not_configured", got.State)
		}
		if got.Reason != backlogsvc.ReasonConfigAbsent {
			t.Fatalf("Reason = %q, want config_absent", got.Reason)
		}
		if len(got.Items) != 0 {
			t.Fatalf("got %d items, want 0", len(got.Items))
		}
		if n := tr.calls.Load(); n != 0 {
			t.Fatalf("tracker calls = %d, want ZERO when the lane is not configured", n)
		}
	}
}

// A nil tracker is normal: newMultiTracker returns nil when nothing is
// configured. It must not panic and must not be an error.
func TestReadyNilTrackerDoesNotPanic(t *testing.T) {
	svc := backlogsvc.New(backlogsvc.Deps{
		Tracker: nil,
		Store:   &fakeStore{projects: []domain.ProjectRecord{enabledProject("p1")}},
		Clock:   newClock().Now,
	})

	got, err := svc.Ready(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Ready with a nil tracker: %v", err)
	}
	if got.State != backlogsvc.StateNotConfigured {
		t.Fatalf("State = %q, want not_configured", got.State)
	}
	if got.Reason != backlogsvc.ReasonCredentialsMissing {
		t.Fatalf("Reason = %q, want credentials_missing (config is present; the tracker is not)", got.Reason)
	}
}

func TestReadyUnknownProject(t *testing.T) {
	tr := &fakeTracker{}
	svc := newService(t, tr, &fakeStore{}, newClock())

	got, err := svc.Ready(context.Background(), "nope")
	if err == nil && got.State == backlogsvc.StateOK {
		t.Fatal("unknown project returned state ok")
	}
	if n := tr.calls.Load(); n != 0 {
		t.Fatalf("tracker calls = %d for an unknown project, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// All projects
// ---------------------------------------------------------------------------

// With no project id the service fetches per configured project and merges.
// Results are independent: one failing project must not blank the others.
func TestReadyAllProjectsMergesAndIsolatesFailures(t *testing.T) {
	p1 := enabledProject("p1")
	p2 := enabledProject("p2")
	p2.Config.TrackerBacklog.ProjectKey = "OTHER"
	p3 := domain.ProjectRecord{ID: "p3"} // not configured; contributes nothing

	tr := &failPerProjectTracker{failKey: "OTHER"}
	clk := newClock()
	svc := backlogsvc.New(backlogsvc.Deps{
		Tracker: tr,
		Store:   &fakeStore{projects: []domain.ProjectRecord{p1, p2, p3}},
		Clock:   clk.Now,
	})

	got, err := svc.ReadyAll(context.Background())
	if err != nil {
		t.Fatalf("ReadyAll: %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("got %d items, want the healthy project's item (a failure must not blank the others)", len(got.Items))
	}
	if got.Items[0].ProjectID != "p1" {
		t.Fatalf("item ProjectID = %q, want p1 — each card must be badged with its project", got.Items[0].ProjectID)
	}
	if got.State != backlogsvc.StateOK {
		t.Fatalf("State = %q, want ok when at least one project resolved", got.State)
	}
	if len(got.Failures) != 1 {
		t.Fatalf("Failures = %#v, want the one failing project reported", got.Failures)
	}
	if got.Failures[0].ProjectID != "p2" {
		t.Fatalf("Failures[0].ProjectID = %q, want p2", got.Failures[0].ProjectID)
	}
}

// The merged fetchedAt must be the OLDEST contributor and stale must be true if
// any contributor is stale, so the indicator never overstates freshness.
func TestReadyAllReportsOldestFetchedAt(t *testing.T) {
	p1 := enabledProject("p1")
	p2 := enabledProject("p2")
	p2.Config.TrackerBacklog.ProjectKey = "OTHER"

	tr := &fakeTracker{}
	tr.setResult(issues("ACME-1"), nil)
	clk := newClock()
	svc := backlogsvc.New(backlogsvc.Deps{
		Tracker: tr,
		Store:   &fakeStore{projects: []domain.ProjectRecord{p1, p2}},
		Clock:   clk.Now,
	})

	// Warm p1 only, then advance so p2 fetches later.
	if _, err := svc.Ready(context.Background(), "p1"); err != nil {
		t.Fatalf("warm p1: %v", err)
	}
	oldest := clk.Now()
	clk.advance(30 * time.Second)

	got, err := svc.ReadyAll(context.Background())
	if err != nil {
		t.Fatalf("ReadyAll: %v", err)
	}
	if !got.FetchedAt.Equal(oldest) {
		t.Fatalf("FetchedAt = %v, want the OLDEST contributor %v", got.FetchedAt, oldest)
	}
	if got.Stale {
		t.Fatal("fresh contributors reported stale")
	}
	// Expire p1 only; its failed refresh retains its older tickets while p2 is fresh.
	clk.advance(31 * time.Second)
	tr.setResult(nil, errors.New("offline"))
	got, err = svc.ReadyAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Stale || !got.FetchedAt.Equal(oldest) || len(got.Items) != 2 || got.State != backlogsvc.StateOK {
		t.Fatalf("mixed stale/fresh aggregate = %#v", got)
	}
}

// failPerProjectTracker fails only for one project key.
type failPerProjectTracker struct {
	failKey string
	calls   atomic.Int32
}

func (f *failPerProjectTracker) Get(context.Context, domain.TrackerID) (domain.Issue, error) {
	return domain.Issue{}, errors.New("unused")
}

func (f *failPerProjectTracker) List(_ context.Context, repo domain.TrackerRepo, _ domain.ListFilter) ([]domain.Issue, error) {
	f.calls.Add(1)
	if repo.Native == f.failKey {
		return nil, fmt.Errorf("wrapped: %w", trackerjira.ErrAuthFailed)
	}
	return issues("ACME-1"), nil
}

func (f *failPerProjectTracker) Preflight(context.Context) error { return nil }
