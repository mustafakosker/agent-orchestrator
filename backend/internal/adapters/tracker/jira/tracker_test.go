package jira_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	trackerjira "github.com/aoagents/agent-orchestrator/backend/internal/adapters/tracker/jira"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

const testSite = "acme.atlassian.net"

// newTracker wires a Tracker at an httptest server, with testSite allowlisted
// and routed to the fake. Returns the tracker and a pointer to the last
// received request's JQL.
func newTracker(t *testing.T, handler http.HandlerFunc, cred trackerjira.CredentialSource) *trackerjira.Tracker {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	if cred == nil {
		cred = trackerjira.StaticCredentialSource{Email: "me@acme.test", Token: "tok"}
	}
	tr, err := trackerjira.New(trackerjira.Options{
		Credential:   cred,
		AllowedHosts: []string{testSite},
		HostBaseURLs: map[string]string{testSite: srv.URL},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return tr
}

func jiraRepo() domain.TrackerRepo {
	return domain.TrackerRepo{Provider: domain.TrackerProviderJira, Native: "ACME", Host: testSite}
}

// onePageIssues answers any search with a single closed page of the given raw
// issue JSON objects, recording the JQL it was asked for.
func onePageIssues(jql *atomic.Value, issues string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jql.Store(r.URL.Query().Get("jql"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"issues":[%s]}`, issues)
	}
}

func issueJSON(key, title, status, category, assignee string) string {
	return fmt.Sprintf(`{"key":%q,"fields":{"summary":%q,
		"status":{"name":%q,"statusCategory":{"key":%q}},
		"assignee":{"displayName":%q},
		"description":{"type":"doc","version":1,"content":[
			{"type":"paragraph","content":[{"type":"text","text":"body text"}]}]}}}`,
		key, title, status, category, assignee)
}

// ---------------------------------------------------------------------------
// JQL construction
// ---------------------------------------------------------------------------

func TestListJQLProjectAndStatuses(t *testing.T) {
	var jql atomic.Value
	tr := newTracker(t, onePageIssues(&jql, issueJSON("ACME-1", "One", "Ready", "new", "Alice")), nil)

	if _, err := tr.List(context.Background(), jiraRepo(), domain.ListFilter{
		Statuses: []string{"Ready"},
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	got, _ := jql.Load().(string)
	if !strings.Contains(got, `project = "ACME"`) {
		t.Fatalf("jql = %q, want a project conjunct", got)
	}
	if !strings.Contains(got, `status IN ("Ready")`) {
		t.Fatalf("jql = %q, want a status IN conjunct", got)
	}
	if !strings.Contains(got, " AND ") {
		t.Fatalf("jql = %q, want conjuncts joined with AND", got)
	}
}

func TestListJQLQuotesStatusNamesWithSpaces(t *testing.T) {
	var jql atomic.Value
	tr := newTracker(t, onePageIssues(&jql, ""), nil)

	if _, err := tr.List(context.Background(), jiraRepo(), domain.ListFilter{
		Statuses: []string{"In Review", "Selected for Development"},
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	got, _ := jql.Load().(string)
	// One IN clause, not several OR-ed comparisons.
	if !strings.Contains(got, `status IN ("In Review", "Selected for Development")`) {
		t.Fatalf("jql = %q, want one quoted IN clause", got)
	}
	if strings.Contains(got, " OR ") {
		t.Fatalf("jql = %q, want no OR-ed status comparisons", got)
	}
}

func TestListJQLEscapesQuotesAndBackslashes(t *testing.T) {
	var jql atomic.Value
	tr := newTracker(t, onePageIssues(&jql, ""), nil)

	if _, err := tr.List(context.Background(), jiraRepo(), domain.ListFilter{
		Statuses: []string{`Need "input"`, `back\slash`},
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	got, _ := jql.Load().(string)
	if !strings.Contains(got, `"Need \"input\""`) {
		t.Fatalf("jql = %q, want the inner quotes escaped", got)
	}
	if !strings.Contains(got, `"back\\slash"`) {
		t.Fatalf("jql = %q, want the backslash escaped", got)
	}
}

// Every narrowing dimension of ListFilter must appear in the JQL. A silently
// dropped filter that NARROWS is the hazard this table guards: trackerintake
// spawns one session per returned issue, so a dropped Assignee would turn an
// assignee-scoped poll into one session per ticket in the project.
func TestListJQLHonorsEveryNarrowingFilter(t *testing.T) {
	tests := []struct {
		name    string
		filter  domain.ListFilter
		want    string
		notWant string
	}{
		{
			name:   "statuses",
			filter: domain.ListFilter{Statuses: []string{"Ready"}},
			want:   `status IN ("Ready")`,
		},
		{
			name:   "state open maps to statusCategory",
			filter: domain.ListFilter{State: domain.ListOpen},
			want:   `statusCategory != Done`,
		},
		{
			name:   "state closed maps to statusCategory",
			filter: domain.ListFilter{State: domain.ListClosed},
			want:   `statusCategory = Done`,
		},
		{
			name:    "state all adds no category conjunct",
			filter:  domain.ListFilter{State: domain.ListAll, Statuses: []string{"Ready"}},
			want:    `status IN ("Ready")`,
			notWant: "statusCategory",
		},
		{
			name:   "labels",
			filter: domain.ListFilter{Labels: []string{"backend", "p1"}},
			want:   `labels IN ("backend", "p1")`,
		},
		{
			name:   "assignee star means anyone",
			filter: domain.ListFilter{Assignee: "*"},
			want:   `assignee IS NOT EMPTY`,
		},
		{
			name:   "assignee literal passes through quoted",
			filter: domain.ListFilter{Assignee: "557058:abc-123"},
			want:   `assignee = "557058:abc-123"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var jql atomic.Value
			tr := newTracker(t, onePageIssues(&jql, ""), nil)
			if _, err := tr.List(context.Background(), jiraRepo(), tt.filter); err != nil {
				t.Fatalf("List: %v", err)
			}
			got, _ := jql.Load().(string)
			if !strings.Contains(got, tt.want) {
				t.Fatalf("jql = %q, want it to contain %q", got, tt.want)
			}
			if tt.notWant != "" && strings.Contains(got, tt.notWant) {
				t.Fatalf("jql = %q, want it NOT to contain %q", got, tt.notWant)
			}
		})
	}
}

func TestListJQLPreservesWhitespaceAssignee(t *testing.T) {
	var jql atomic.Value
	tr := newTracker(t, onePageIssues(&jql, ""), nil)
	_, err := tr.List(context.Background(), jiraRepo(), domain.ListFilter{
		State: domain.ListOpen, Assignee: "   ",
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got, _ := jql.Load().(string)
	want := `project = "ACME" AND statusCategory != Done AND assignee = "   "`
	if got != want {
		t.Fatalf("jql = %q, want %q (the assignee must not be dropped)", got, want)
	}
}

func TestListJQLCombinesAllConjuncts(t *testing.T) {
	var jql atomic.Value
	tr := newTracker(t, onePageIssues(&jql, ""), nil)

	if _, err := tr.List(context.Background(), jiraRepo(), domain.ListFilter{
		State:    domain.ListOpen,
		Statuses: []string{"Ready"},
		Labels:   []string{"backend"},
		Assignee: "alice",
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	got, _ := jql.Load().(string)
	for _, want := range []string{
		`project = "ACME"`,
		`status IN ("Ready")`,
		`statusCategory != Done`,
		`labels IN ("backend")`,
		`assignee = "alice"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("jql = %q, want it to contain %q", got, want)
		}
	}
}

// A project-key-only query returns every ticket in the Jira project. List must
// refuse it before making any request.
func TestListRefusesUnscopedQuery(t *testing.T) {
	var calls atomic.Int32
	tr := newTracker(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"issues":[]}`)
	}, nil)

	_, err := tr.List(context.Background(), jiraRepo(), domain.ListFilter{})
	if !errors.Is(err, trackerjira.ErrUnscopedQuery) {
		t.Fatalf("List err = %v, want ErrUnscopedQuery", err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("server saw %d requests, want 0 — the guard must precede the request", n)
	}
	// Limit alone does not scope a query: it caps results, it does not narrow them.
	_, err = tr.List(context.Background(), jiraRepo(), domain.ListFilter{Limit: 10})
	if !errors.Is(err, trackerjira.ErrUnscopedQuery) {
		t.Fatalf("List with Limit only err = %v, want ErrUnscopedQuery", err)
	}
}

func TestListRejectsWrongProviderAndBadKey(t *testing.T) {
	var calls atomic.Int32
	tr := newTracker(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"issues":[]}`)
	}, nil)
	f := domain.ListFilter{Statuses: []string{"Ready"}}

	_, err := tr.List(context.Background(), domain.TrackerRepo{
		Provider: domain.TrackerProviderGitHub, Native: "ACME", Host: testSite,
	}, f)
	if !errors.Is(err, trackerjira.ErrWrongProvider) {
		t.Fatalf("wrong provider err = %v, want ErrWrongProvider", err)
	}

	for _, bad := range []string{"", "acme/demo", "ACME-1", "a b", "9ACME", strings.Repeat("A", 200)} {
		_, err := tr.List(context.Background(), domain.TrackerRepo{
			Provider: domain.TrackerProviderJira, Native: bad, Host: testSite,
		}, f)
		if !errors.Is(err, trackerjira.ErrBadKey) {
			t.Fatalf("List(project key %q) err = %v, want ErrBadKey", bad, err)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("server saw %d requests, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// State mapping
// ---------------------------------------------------------------------------

func TestListMapsStatusCategory(t *testing.T) {
	tests := []struct {
		category string
		want     domain.NormalizedIssueState
	}{
		{"new", domain.IssueOpen},
		{"indeterminate", domain.IssueInProgress},
		{"done", domain.IssueDone},
		{"totally-unknown", domain.IssueOpen},
		{"", domain.IssueOpen},
	}
	for _, tt := range tests {
		t.Run(tt.category, func(t *testing.T) {
			var jql atomic.Value
			tr := newTracker(t, onePageIssues(&jql,
				issueJSON("ACME-1", "One", "Whatever", tt.category, "Alice")), nil)
			out, err := tr.List(context.Background(), jiraRepo(), domain.ListFilter{
				Statuses: []string{"Ready"},
			})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(out) != 1 {
				t.Fatalf("got %d issues, want 1", len(out))
			}
			if out[0].State != tt.want {
				t.Fatalf("category %q -> %q, want %q", tt.category, out[0].State, tt.want)
			}
		})
	}
}

// Jira has no instance-independent signal for review or cancelled, so the
// adapter must never synthesize either.
func TestListNeverSynthesizesReviewOrCancelled(t *testing.T) {
	var jql atomic.Value
	tr := newTracker(t, onePageIssues(&jql, strings.Join([]string{
		issueJSON("ACME-1", "In review please", "In Review", "indeterminate", "Alice"),
		issueJSON("ACME-2", "Cancelled work", "Cancelled", "done", "Bob"),
		issueJSON("ACME-3", "Won't do", "Won't Do", "done", ""),
		issueJSON("ACME-4", "Review", "Review", "new", ""),
	}, ",")), nil)

	out, err := tr.List(context.Background(), jiraRepo(), domain.ListFilter{Statuses: []string{"Ready"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 4 {
		t.Fatalf("got %d issues, want 4", len(out))
	}
	for _, iss := range out {
		if iss.State == domain.IssueInReview || iss.State == domain.IssueCancelled {
			t.Fatalf("issue %s state = %q, want neither review nor cancelled", iss.ID.Native, iss.State)
		}
	}
}

func TestListIssueProjection(t *testing.T) {
	var jql atomic.Value
	tr := newTracker(t, onePageIssues(&jql,
		issueJSON("ACME-42", "Wire the thing", "Ready", "new", "Alice Smith")), nil)

	out, err := tr.List(context.Background(), jiraRepo(), domain.ListFilter{Statuses: []string{"Ready"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := out[0]
	if got.ID.Provider != domain.TrackerProviderJira {
		t.Fatalf("ID.Provider = %q, want jira", got.ID.Provider)
	}
	if got.ID.Native != "ACME-42" {
		t.Fatalf("ID.Native = %q, want the issue key", got.ID.Native)
	}
	if got.ID.Host != testSite {
		t.Fatalf("ID.Host = %q, want %q so the id round-trips", got.ID.Host, testSite)
	}
	if got.Title != "Wire the thing" {
		t.Fatalf("Title = %q", got.Title)
	}
	if want := "https://" + testSite + "/browse/ACME-42"; got.URL != want {
		t.Fatalf("URL = %q, want %q", got.URL, want)
	}
	if len(got.Assignees) != 1 || got.Assignees[0] != "Alice Smith" {
		t.Fatalf("Assignees = %#v", got.Assignees)
	}
}

// Jira Cloud returns description as Atlassian Document Format (nested JSON).
// The body is descoped by decision: it must be empty, and decoding the nested
// ADF must not error.
func TestListLeavesBodyEmptyForADF(t *testing.T) {
	var jql atomic.Value
	tr := newTracker(t, onePageIssues(&jql,
		issueJSON("ACME-1", "One", "Ready", "new", "Alice")), nil)

	out, err := tr.List(context.Background(), jiraRepo(), domain.ListFilter{Statuses: []string{"Ready"}})
	if err != nil {
		t.Fatalf("List with an ADF description: %v", err)
	}
	if out[0].Body != "" {
		t.Fatalf("Body = %q, want empty (ADF is descoped by decision)", out[0].Body)
	}
}

// ---------------------------------------------------------------------------
// Pagination
// ---------------------------------------------------------------------------

// Limit is a TOTAL cap across pages, not a per-page cap.
func TestListLimitIsTotalCapAcrossPages(t *testing.T) {
	var calls atomic.Int32
	tr := newTracker(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		// Two issues per page, always claiming another page follows.
		fmt.Fprintf(w, `{"nextPageToken":"page%d","issues":[%s,%s]}`, n+1,
			issueJSON(fmt.Sprintf("ACME-%d", n*2-1), "a", "Ready", "new", ""),
			issueJSON(fmt.Sprintf("ACME-%d", n*2), "b", "Ready", "new", ""))
	}, nil)

	out, err := tr.List(context.Background(), jiraRepo(), domain.ListFilter{
		Statuses: []string{"Ready"}, Limit: 3,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d issues, want exactly the total cap of 3", len(out))
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("server saw %d pages, want 2 (stop as soon as the cap is met)", n)
	}
}

func TestListFollowsNextPageToken(t *testing.T) {
	var tokens []string
	var calls atomic.Int32
	tr := newTracker(t, func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.URL.Query().Get("nextPageToken"))
		if calls.Add(1) == 1 {
			fmt.Fprintf(w, `{"nextPageToken":"tok2","issues":[%s]}`,
				issueJSON("ACME-1", "a", "Ready", "new", ""))
			return
		}
		fmt.Fprintf(w, `{"issues":[%s]}`, issueJSON("ACME-2", "b", "Ready", "new", ""))
	}, nil)

	out, err := tr.List(context.Background(), jiraRepo(), domain.ListFilter{Statuses: []string{"Ready"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d issues, want 2 across both pages", len(out))
	}
	if len(tokens) != 2 || tokens[0] != "" || tokens[1] != "tok2" {
		t.Fatalf("page tokens = %#v, want [\"\", \"tok2\"]", tokens)
	}
}

// A pathological server that always claims another page must fail loud rather
// than loop forever.
func TestListPaginationGuardFailsLoud(t *testing.T) {
	var calls atomic.Int32
	tr := newTracker(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprintf(w, `{"nextPageToken":"always","issues":[%s]}`,
			issueJSON("ACME-1", "a", "Ready", "new", ""))
	}, nil)

	_, err := tr.List(context.Background(), jiraRepo(), domain.ListFilter{Statuses: []string{"Ready"}})
	if err == nil {
		t.Fatal("List = nil error, want a loud pagination-guard failure")
	}
	if !strings.Contains(err.Error(), "pagination") {
		t.Fatalf("List err = %v, want it to name pagination", err)
	}
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

func TestAuthBasicVersusBearer(t *testing.T) {
	tests := []struct {
		name string
		cred trackerjira.CredentialSource
		want func(t *testing.T, authz string)
	}{
		{
			name: "cloud email plus api token uses Basic",
			cred: trackerjira.StaticCredentialSource{Email: "me@acme.test", Token: "api-tok"},
			want: func(t *testing.T, authz string) {
				rest, ok := strings.CutPrefix(authz, "Basic ")
				if !ok {
					t.Fatalf("Authorization = %q, want Basic", authz)
				}
				raw, err := base64.StdEncoding.DecodeString(rest)
				if err != nil {
					t.Fatalf("decode basic: %v", err)
				}
				if string(raw) != "me@acme.test:api-tok" {
					t.Fatalf("basic payload = %q, want email:token", raw)
				}
			},
		},
		{
			name: "data center PAT uses Bearer",
			cred: trackerjira.StaticCredentialSource{Token: "pat-tok"},
			want: func(t *testing.T, authz string) {
				if authz != "Bearer pat-tok" {
					t.Fatalf("Authorization = %q, want Bearer pat-tok", authz)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var authz atomic.Value
			tr := newTracker(t, func(w http.ResponseWriter, r *http.Request) {
				authz.Store(r.Header.Get("Authorization"))
				fmt.Fprint(w, `{"issues":[]}`)
			}, tt.cred)
			if _, err := tr.List(context.Background(), jiraRepo(), domain.ListFilter{
				Statuses: []string{"Ready"},
			}); err != nil {
				t.Fatalf("List: %v", err)
			}
			got, _ := authz.Load().(string)
			tt.want(t, got)
		})
	}
}

// rotatingCredential returns a different token on each call.
type rotatingCredential struct{ n atomic.Int32 }

func (r *rotatingCredential) Credential(context.Context) (trackerjira.Credential, error) {
	return trackerjira.Credential{Token: fmt.Sprintf("tok-%d", r.n.Add(1))}, nil
}

// The credential is re-read per request so a rotated token is picked up without
// a daemon restart.
func TestAuthReReadsCredentialPerRequest(t *testing.T) {
	var seen []string
	var calls atomic.Int32
	tr := newTracker(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		if calls.Add(1) == 1 {
			fmt.Fprintf(w, `{"nextPageToken":"t2","issues":[%s]}`,
				issueJSON("ACME-1", "a", "Ready", "new", ""))
			return
		}
		fmt.Fprint(w, `{"issues":[]}`)
	}, &rotatingCredential{})

	if _, err := tr.List(context.Background(), jiraRepo(), domain.ListFilter{
		Statuses: []string{"Ready"},
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("saw %d requests, want 2", len(seen))
	}
	if seen[0] == seen[1] {
		t.Fatalf("both requests used %q — the credential must be re-read per request", seen[0])
	}
}

func TestNewWithOnlyHostCredentials(t *testing.T) {
	for _, key := range []string{trackerjira.EnvEmail, trackerjira.EnvAPIToken, trackerjira.EnvPAT} {
		t.Setenv(key, "")
	}
	for _, tc := range []struct {
		name   string
		source trackerjira.CredentialSource
	}{
		{"nil default", nil},
		{"missing environment default", trackerjira.EnvCredentialSource{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			var authz atomic.Value
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				authz.Store(r.Header.Get("Authorization"))
				fmt.Fprint(w, `{"issues":[]}`)
			}))
			defer srv.Close()
			tr, err := trackerjira.New(trackerjira.Options{
				Credential:   tc.source,
				AllowedHosts: []string{testSite, "missing.example.com"},
				HostCredentials: map[string]trackerjira.CredentialSource{
					testSite: trackerjira.StaticCredentialSource{Token: "host-pat"},
				},
				HostBaseURLs: map[string]string{testSite: srv.URL, "missing.example.com": srv.URL},
			})
			if err != nil {
				t.Fatalf("New with usable host credential: %v", err)
			}
			if calls.Load() != 0 {
				t.Fatal("New made a network request")
			}
			filter := domain.ListFilter{State: domain.ListOpen}
			if _, err := tr.List(context.Background(), jiraRepo(), filter); err != nil {
				t.Fatalf("List with host credential: %v", err)
			}
			if got := authz.Load(); got != "Bearer host-pat" {
				t.Fatalf("Authorization = %v, want host credential", got)
			}
			repo := jiraRepo()
			repo.Host = "missing.example.com"
			if _, err := tr.List(context.Background(), repo, filter); !errors.Is(err, trackerjira.ErrNoToken) {
				t.Fatalf("List without host credential = %v, want ErrNoToken", err)
			}
			if calls.Load() != 1 {
				t.Fatal("credential-less host made a network request")
			}
		})
	}
}

func TestNewDoesNotUseUnlistedHostCredential(t *testing.T) {
	_, err := trackerjira.New(trackerjira.Options{
		AllowedHosts: []string{testSite},
		HostCredentials: map[string]trackerjira.CredentialSource{
			"unlisted.example.com": trackerjira.StaticCredentialSource{Token: "pat"},
		},
	})
	if !errors.Is(err, trackerjira.ErrNoToken) {
		t.Fatalf("New with only unlisted host credential = %v, want ErrNoToken", err)
	}
}

func TestNewFailsFastWithoutCredential(t *testing.T) {
	_, err := trackerjira.New(trackerjira.Options{AllowedHosts: []string{testSite}})
	if !errors.Is(err, trackerjira.ErrNoToken) {
		t.Fatalf("New(no source) err = %v, want ErrNoToken", err)
	}

	_, err = trackerjira.New(trackerjira.Options{
		Credential:   trackerjira.StaticCredentialSource{},
		AllowedHosts: []string{testSite},
	})
	if !errors.Is(err, trackerjira.ErrNoToken) {
		t.Fatalf("New(blank credential) err = %v, want ErrNoToken", err)
	}

	// An email with no token cannot form either scheme.
	_, err = trackerjira.New(trackerjira.Options{
		Credential:   trackerjira.StaticCredentialSource{Email: "me@acme.test"},
		AllowedHosts: []string{testSite},
	})
	if !errors.Is(err, trackerjira.ErrNoToken) {
		t.Fatalf("New(email without token) err = %v, want ErrNoToken", err)
	}
}

// New must not make a network call: a credential presence check is enough.
func TestNewMakesNoNetworkCall(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer srv.Close()

	if _, err := trackerjira.New(trackerjira.Options{
		Credential:   trackerjira.StaticCredentialSource{Token: "pat"},
		AllowedHosts: []string{testSite},
		HostBaseURLs: map[string]string{testSite: srv.URL},
	}); err != nil {
		t.Fatalf("New: %v", err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("New made %d requests, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// Host allowlist
// ---------------------------------------------------------------------------

func TestHostAllowlistRejectsBeforeAttachingCredential(t *testing.T) {
	var calls atomic.Int32
	tr := newTracker(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"issues":[]}`)
	}, nil)
	f := domain.ListFilter{Statuses: []string{"Ready"}}

	_, err := tr.List(context.Background(), domain.TrackerRepo{
		Provider: domain.TrackerProviderJira, Native: "ACME", Host: "evil.example.com",
	}, f)
	if !errors.Is(err, trackerjira.ErrHostNotAllowed) {
		t.Fatalf("List(unlisted host) err = %v, want ErrHostNotAllowed", err)
	}

	// Jira has no default site: an empty Host is a configuration error.
	_, err = tr.List(context.Background(), domain.TrackerRepo{
		Provider: domain.TrackerProviderJira, Native: "ACME",
	}, f)
	if !errors.Is(err, trackerjira.ErrHostNotAllowed) {
		t.Fatalf("List(empty host) err = %v, want ErrHostNotAllowed", err)
	}

	if n := calls.Load(); n != 0 {
		t.Fatalf("server saw %d requests, want 0 — the allowlist must precede the credential", n)
	}
}

// ---------------------------------------------------------------------------
// Error mapping
// ---------------------------------------------------------------------------

func TestErrorMapping(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		headers map[string]string
		want    error
		wantMsg string
	}{
		{
			name:    "400 unknown project key",
			status:  http.StatusBadRequest,
			body:    `{"errorMessages":["The value 'NOPE' does not exist for the field 'project'."]}`,
			want:    trackerjira.ErrBadQuery,
			wantMsg: "does not exist for the field 'project'",
		},
		{
			name:   "401 rejected credential",
			status: http.StatusUnauthorized,
			body:   `{"errorMessages":["Client must be authenticated"]}`,
			want:   trackerjira.ErrAuthFailed,
		},
		{
			name:   "403 missing browse permission",
			status: http.StatusForbidden,
			body:   `{"errorMessages":["No permission"]}`,
			want:   trackerjira.ErrAuthFailed,
		},
		{
			name:    "429 rate limited",
			status:  http.StatusTooManyRequests,
			body:    `{"errorMessages":["Rate limit"]}`,
			headers: map[string]string{"Retry-After": "42"},
			want:    trackerjira.ErrRateLimited,
		},
		{
			name:   "500 server error",
			status: http.StatusInternalServerError,
			body:   `{"errorMessages":["boom"]}`,
			want:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := newTracker(t, func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tt.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			}, nil)

			_, err := tr.List(context.Background(), jiraRepo(), domain.ListFilter{
				Statuses: []string{"Ready"},
			})
			if err == nil {
				t.Fatalf("List = nil error, want a failure for status %d", tt.status)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("List err = %v, want errors.Is %v", err, tt.want)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("List err = %v, want it to carry Jira's message %q", err, tt.wantMsg)
			}
		})
	}
}

func TestRateLimitErrorCarriesRetryAfter(t *testing.T) {
	tr := newTracker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"errorMessages":["slow down"]}`)
	}, nil)

	_, err := tr.List(context.Background(), jiraRepo(), domain.ListFilter{Statuses: []string{"Ready"}})
	var rl *trackerjira.RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("List err = %v, want a *RateLimitError", err)
	}
	if rl.RetryAfter != 42*time.Second {
		t.Fatalf("RetryAfter = %v, want 42s", rl.RetryAfter)
	}
}

// ---------------------------------------------------------------------------
// Get and Preflight
// ---------------------------------------------------------------------------

func TestGetIssue(t *testing.T) {
	var path atomic.Value
	tr := newTracker(t, func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		fmt.Fprint(w, issueJSON("ACME-7", "Seven", "Ready", "new", "Alice"))
	}, nil)

	got, err := tr.Get(context.Background(), domain.TrackerID{
		Provider: domain.TrackerProviderJira, Native: "ACME-7", Host: testSite,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if p, _ := path.Load().(string); !strings.Contains(p, "/rest/api/3/issue/ACME-7") {
		t.Fatalf("request path = %q, want the issue endpoint", p)
	}
	if got.ID.Native != "ACME-7" || got.Title != "Seven" {
		t.Fatalf("Get = %#v", got)
	}
	if got.Body != "" {
		t.Fatalf("Get Body = %q, want empty", got.Body)
	}
}

func TestGetRejectsWrongProviderAndBadID(t *testing.T) {
	tr := newTracker(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("Get made a request for an invalid id")
	}, nil)

	_, err := tr.Get(context.Background(), domain.TrackerID{
		Provider: domain.TrackerProviderGitLab, Native: "ACME-1", Host: testSite,
	})
	if !errors.Is(err, trackerjira.ErrWrongProvider) {
		t.Fatalf("Get(wrong provider) err = %v, want ErrWrongProvider", err)
	}

	for _, bad := range []string{"", "ACME", "acme/demo#1", "ACME-", "-1"} {
		_, err := tr.Get(context.Background(), domain.TrackerID{
			Provider: domain.TrackerProviderJira, Native: bad, Host: testSite,
		})
		if !errors.Is(err, trackerjira.ErrBadID) {
			t.Fatalf("Get(%q) err = %v, want ErrBadID", bad, err)
		}
	}
}

func TestGetNotFound(t *testing.T) {
	tr := newTracker(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"errorMessages":["Issue does not exist"]}`)
	}, nil)

	_, err := tr.Get(context.Background(), domain.TrackerID{
		Provider: domain.TrackerProviderJira, Native: "ACME-9", Host: testSite,
	})
	if !errors.Is(err, trackerjira.ErrNotFound) {
		t.Fatalf("Get err = %v, want ErrNotFound", err)
	}
}

func TestPreflightCallsMyselfAndCachesSuccess(t *testing.T) {
	var calls atomic.Int32
	var path atomic.Value
	tr := newTracker(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		path.Store(r.URL.Path)
		fmt.Fprint(w, `{"accountId":"557058:abc"}`)
	}, nil)

	for i := 0; i < 3; i++ {
		if err := tr.Preflight(context.Background()); err != nil {
			t.Fatalf("Preflight #%d: %v", i, err)
		}
	}
	if p, _ := path.Load().(string); !strings.HasSuffix(p, "/rest/api/3/myself") {
		t.Fatalf("preflight path = %q, want /rest/api/3/myself", p)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("Preflight made %d calls, want 1 (success is cached)", n)
	}
}

// Failures are NOT cached, so fixing credentials later is picked up without a
// daemon restart.
func TestPreflightDoesNotCacheFailure(t *testing.T) {
	var calls atomic.Int32
	tr := newTracker(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"errorMessages":["nope"]}`)
			return
		}
		fmt.Fprint(w, `{"accountId":"557058:abc"}`)
	}, nil)

	if err := tr.Preflight(context.Background()); !errors.Is(err, trackerjira.ErrAuthFailed) {
		t.Fatalf("first Preflight err = %v, want ErrAuthFailed", err)
	}
	if err := tr.Preflight(context.Background()); err != nil {
		t.Fatalf("second Preflight = %v, want nil (failures are not cached)", err)
	}
}

// ---------------------------------------------------------------------------
// Credential sources
// ---------------------------------------------------------------------------

func TestEnvCredentialSource(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string
		wantEmail string
		wantToken string
		wantErr   bool
	}{
		{
			name:      "cloud email and api token",
			env:       map[string]string{"AO_JIRA_EMAIL": "me@acme.test", "AO_JIRA_API_TOKEN": "api"},
			wantEmail: "me@acme.test",
			wantToken: "api",
		},
		{
			name:      "data center pat",
			env:       map[string]string{"AO_JIRA_PAT": "pat"},
			wantToken: "pat",
		},
		{
			name:    "nothing set",
			env:     map[string]string{},
			wantErr: true,
		},
		{
			name:    "email without token",
			env:     map[string]string{"AO_JIRA_EMAIL": "me@acme.test"},
			wantErr: true,
		},
		{
			name:    "api token without email is incomplete",
			env:     map[string]string{"AO_JIRA_API_TOKEN": "api"},
			wantErr: true,
		},
		{
			name:    "api token with blank email is incomplete",
			env:     map[string]string{"AO_JIRA_API_TOKEN": "api", "AO_JIRA_EMAIL": "   "},
			wantErr: true,
		},
		{
			name:      "pat remains usable with incomplete cloud credentials",
			env:       map[string]string{"AO_JIRA_API_TOKEN": "api", "AO_JIRA_PAT": "pat"},
			wantToken: "pat",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, k := range []string{"AO_JIRA_EMAIL", "AO_JIRA_API_TOKEN", "AO_JIRA_PAT"} {
				t.Setenv(k, "")
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			got, err := trackerjira.EnvCredentialSource{}.Credential(context.Background())
			if tt.wantErr {
				if !errors.Is(err, trackerjira.ErrNoToken) {
					t.Fatalf("Credential() err = %v, want ErrNoToken", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Credential(): %v", err)
			}
			if got.Email != tt.wantEmail || got.Token != tt.wantToken {
				t.Fatalf("Credential() = %+v, want email %q token %q", got, tt.wantEmail, tt.wantToken)
			}
		})
	}
}

func TestFallbackCredentialSourceUsesFirstSuccess(t *testing.T) {
	src := trackerjira.FallbackCredentialSource{
		trackerjira.StaticCredentialSource{},
		trackerjira.StaticCredentialSource{Token: "second"},
		trackerjira.StaticCredentialSource{Token: "third"},
	}
	got, err := src.Credential(context.Background())
	if err != nil {
		t.Fatalf("Credential(): %v", err)
	}
	if got.Token != "second" {
		t.Fatalf("Credential().Token = %q, want the first non-empty source", got.Token)
	}

	empty := trackerjira.FallbackCredentialSource{trackerjira.StaticCredentialSource{}}
	if _, err := empty.Credential(context.Background()); !errors.Is(err, trackerjira.ErrNoToken) {
		t.Fatalf("empty chain err = %v, want ErrNoToken", err)
	}
}

// The JQL must be sent as a query parameter that survives URL encoding
// round-trip, so a status name with spaces arrives intact.
func TestListJQLSurvivesURLEncoding(t *testing.T) {
	var raw atomic.Value
	tr := newTracker(t, func(w http.ResponseWriter, r *http.Request) {
		raw.Store(r.URL.RawQuery)
		fmt.Fprint(w, `{"issues":[]}`)
	}, nil)

	if _, err := tr.List(context.Background(), jiraRepo(), domain.ListFilter{
		Statuses: []string{"In Review"},
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	got, _ := raw.Load().(string)
	vals, err := url.ParseQuery(got)
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", got, err)
	}
	if !strings.Contains(vals.Get("jql"), `status IN ("In Review")`) {
		t.Fatalf("decoded jql = %q, want the quoted status intact", vals.Get("jql"))
	}
}

// The lane renders Jira's own workflow status name, so the adapter must carry
// it. The normalized State is lossy on purpose and cannot stand in for it.
func TestListCarriesJiraStatusName(t *testing.T) {
	var jql atomic.Value
	tr := newTracker(t, onePageIssues(&jql, strings.Join([]string{
		issueJSON("ACME-1", "One", "Selected for Development", "new", "Alice"),
		issueJSON("ACME-2", "Two", "Blocked", "new", "Bob"),
	}, ",")), nil)

	out, err := tr.List(context.Background(), jiraRepo(), domain.ListFilter{Statuses: []string{"Ready"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if out[0].ProviderState != "Selected for Development" {
		t.Fatalf("ProviderState = %q, want Jira's own status name", out[0].ProviderState)
	}
	if out[1].ProviderState != "Blocked" {
		t.Fatalf("ProviderState = %q, want Jira's own status name", out[1].ProviderState)
	}
	// Both collapse to IssueOpen: this is exactly why ProviderState exists.
	if out[0].State != domain.IssueOpen || out[1].State != domain.IssueOpen {
		t.Fatalf("states = %q/%q, want both normalized to open", out[0].State, out[1].State)
	}
}

func TestGetCarriesJiraStatusName(t *testing.T) {
	tr := newTracker(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, issueJSON("ACME-7", "Seven", "In Review", "indeterminate", "Alice"))
	}, nil)

	got, err := tr.Get(context.Background(), domain.TrackerID{
		Provider: domain.TrackerProviderJira, Native: "ACME-7", Host: testSite,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ProviderState != "In Review" {
		t.Fatalf("ProviderState = %q, want \"In Review\"", got.ProviderState)
	}
}
