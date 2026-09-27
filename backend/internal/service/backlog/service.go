// Package backlog serves the read-only tracker backlog lane on the sessions
// board. It is display-only: it reads a project's backlog config, lists the
// eligible issues through ports.Tracker, and caches the result. It never
// spawns a session, never writes to the provider, and nothing in AO's session
// lifecycle or kanban derivation reads it.
//
// The cache is load-bearing rather than an optimization: it is the only thing
// decoupling how often the UI asks from how often AO calls the provider.
// Several board surfaces, several windows, and a route prefetch can all ask at
// once, and Jira Cloud's rate limits are per-user and unforgiving, so a board
// left open all day must not trip them.
//
// Because the lane is a view and never a source of truth, every failure
// degrades to "the lane is empty, stale, or shows a message" and affects
// nothing else in the product. That is what licenses the simple error
// handling here: Ready reports state, and reserves a Go error for a caller
// mistake such as an unknown project id.
package backlog

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	trackerjira "github.com/aoagents/agent-orchestrator/backend/internal/adapters/tracker/jira"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// DefaultTTL bounds how often the provider is called per project.
const DefaultTTL = 60 * time.Second

// AuthBackoff suppresses refetches after a rejected credential. 401/403 is
// categorically different from a transient failure: it will be rejected again
// on the next tick and needs a person, not a retry.
const AuthBackoff = 5 * time.Minute

// State is the lane's top-level outcome.
type State string

// The three lane states. An unconfigured integration is a normal state, not a
// fault, which is why it is distinct from StateError.
const (
	StateOK            State = "ok"
	StateNotConfigured State = "not_configured"
	StateError         State = "error"
)

// Reason discriminates the cause within a non-ok State.
type Reason string

// Reasons. The two not_configured reasons are load-bearing for the UI: one is
// fixed in the settings form and the other in the environment, so a single
// "not configured" state would point half of all affected users at the wrong
// place.
const (
	ReasonNone               Reason = ""
	ReasonConfigAbsent       Reason = "config_absent"
	ReasonCredentialsMissing Reason = "credentials_missing"
	ReasonAuthFailed         Reason = "auth_failed"
	ReasonConfigInvalid      Reason = "config_invalid"
	ReasonUnavailable        Reason = "unavailable"
)

// Item is one backlog ticket. ProjectID badges the card on the all-projects
// board, where several projects' tickets share one lane.
type Item struct {
	Issue     domain.Issue
	ProjectID string
}

// Failure records one project's failure on the all-projects board, so a
// failing project is reported without suppressing the ones that resolved.
type Failure struct {
	ProjectID string
	State     State
	Reason    Reason
	Message   string
}

// Result is the lane's read model.
//
// FetchedAt always describes when Items was actually fetched from the provider,
// never when the request was served: a stale lane that reported the serving
// time would claim a freshness it does not have.
type Result struct {
	Items     []Item
	FetchedAt time.Time
	Stale     bool
	State     State
	Reason    Reason
	Message   string
	// Failures is populated only by ReadyAll, where per-project results are
	// independent.
	Failures []Failure
}

// Store is the durable read surface the service needs.
type Store interface {
	GetProject(ctx context.Context, id string) (domain.ProjectRecord, bool, error)
	ListProjects(ctx context.Context) ([]domain.ProjectRecord, error)
}

// Deps wires the service. Tracker may be nil: newMultiTracker returns nil when
// nothing is configured, and the lane treats that as not-configured rather
// than panicking, mirroring the session service's nil-guard.
type Deps struct {
	Tracker ports.Tracker
	Store   Store
	Clock   func() time.Time
	TTL     time.Duration
}

// Service serves the backlog lane.
type Service struct {
	tracker ports.Tracker
	store   Store
	now     func() time.Time
	ttl     time.Duration

	mu      sync.Mutex
	entries map[string]*entry
}

// entry is one project's cache slot. It is retained after expiry rather than
// evicted, so it remains available as stale data when a refresh fails.
type entry struct {
	// inflight guards a single provider call per project. Concurrent callers
	// wait on it rather than each issuing their own request.
	inflight sync.Mutex

	items     []Item
	fetchedAt time.Time
	hasData   bool

	// notBefore suppresses refetching until this time, used for auth backoff
	// and rate-limit Retry-After.
	notBefore time.Time

	lastState   State
	lastReason  Reason
	lastMessage string
}

// New builds a Service.
func New(deps Deps) *Service {
	s := &Service{
		tracker: deps.Tracker,
		store:   deps.Store,
		now:     deps.Clock,
		ttl:     deps.TTL,
		entries: make(map[string]*entry),
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.ttl <= 0 {
		s.ttl = DefaultTTL
	}
	return s
}

// Ready returns the eligible backlog issues for one project.
//
// It reports outcome through Result.State and reserves a Go error for a caller
// mistake (an unknown project id) or a store failure. A provider failure is
// never a Go error: the lane degrades instead.
func (s *Service) Ready(ctx context.Context, projectID string) (Result, error) {
	project, ok, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return Result{}, fmt.Errorf("backlog: load project %q: %w", projectID, err)
	}
	if !ok {
		return Result{}, fmt.Errorf("backlog: unknown project %q", projectID)
	}
	return s.readyForProject(ctx, project), nil
}

// ReadyAll merges every configured project's backlog, badging each item with
// its project.
//
// Per-project results are independent: one project's failure must not blank
// the others, so failures are collected rather than returned. The merged
// FetchedAt is the oldest contributor and Stale is true if any contributor is
// stale, so the freshness indicator never overstates.
func (s *Service) ReadyAll(ctx context.Context) (Result, error) {
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("backlog: list projects: %w", err)
	}

	out := Result{State: StateNotConfigured, Reason: ReasonConfigAbsent}
	var (
		oldest    time.Time
		anyOK     bool
		anyStale  bool
		anyActive bool
	)
	for _, project := range projects {
		if !enabled(project.Config.TrackerBacklog) {
			continue
		}
		anyActive = true
		res := s.readyForProject(ctx, project)
		if res.State != StateOK {
			out.Failures = append(out.Failures, Failure{
				ProjectID: project.ID,
				State:     res.State,
				Reason:    res.Reason,
				Message:   res.Message,
			})
			continue
		}
		anyOK = true
		anyStale = anyStale || res.Stale
		out.Items = append(out.Items, res.Items...)
		if oldest.IsZero() || res.FetchedAt.Before(oldest) {
			oldest = res.FetchedAt
		}
	}

	switch {
	case anyOK:
		out.State, out.Reason = StateOK, ReasonNone
		out.FetchedAt, out.Stale = oldest, anyStale
	case anyActive:
		// Every configured project failed. Surface the first failure's shape so
		// the lane shows something actionable rather than an empty success.
		first := out.Failures[0]
		out.State, out.Reason, out.Message = first.State, first.Reason, first.Message
	}
	return out, nil
}

func enabled(cfg domain.TrackerBacklogConfig) bool {
	return cfg.Enabled && cfg.Validate() == nil
}

func (s *Service) readyForProject(ctx context.Context, project domain.ProjectRecord) Result {
	cfg := project.Config.TrackerBacklog
	if !enabled(cfg) {
		// Zero provider calls: the lane is off.
		return Result{State: StateNotConfigured, Reason: ReasonConfigAbsent}
	}
	// Config is present but there is no tracker at all. That is a credential
	// problem, not a config problem, and the two are fixed in different places.
	if s.tracker == nil {
		return Result{State: StateNotConfigured, Reason: ReasonCredentialsMissing}
	}

	e := s.entryFor(project.ID)

	// Single-flight: one provider call per project. Concurrent callers queue
	// here and the ones that arrive after the fetch find a fresh cache.
	e.inflight.Lock()
	defer e.inflight.Unlock()

	now := s.now()
	if snap, ok := e.fresh(now, s.ttl); ok {
		return snap
	}

	issues, err := s.tracker.List(ctx, domain.TrackerRepo{
		Provider: cfg.Provider,
		Native:   cfg.ProjectKey,
		Host:     cfg.Site,
	}, domain.ListFilter{Statuses: cfg.Statuses})
	if err != nil {
		return e.recordFailure(now, err)
	}

	items := make([]Item, 0, len(issues))
	for _, issue := range issues {
		items = append(items, Item{Issue: issue, ProjectID: project.ID})
	}
	return e.recordSuccess(now, items)
}

func (s *Service) entryFor(projectID string) *entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[projectID]
	if !ok {
		e = &entry{}
		s.entries[projectID] = e
	}
	return e
}

// fresh reports a cache hit. An entry is fresh inside the TTL, and also while
// notBefore holds — a rate-limit or auth backoff window suppresses refetching
// past the ordinary TTL.
func (e *entry) fresh(now time.Time, ttl time.Duration) (Result, bool) {
	insideTTL := e.hasData && now.Sub(e.fetchedAt) < ttl
	backingOff := now.Before(e.notBefore)
	if !insideTTL && !backingOff {
		return Result{}, false
	}
	if !e.hasData {
		// Backing off with nothing cached: replay the recorded failure rather
		// than hammering a provider that just rejected us.
		if backingOff {
			return Result{State: e.lastState, Reason: e.lastReason, Message: e.lastMessage}, true
		}
		return Result{}, false
	}
	return Result{
		Items:     e.items,
		FetchedAt: e.fetchedAt,
		// Inside the TTL the data is current. Past it, we are only here because
		// of a backoff window, so the data is stale.
		Stale: !insideTTL,
		State: StateOK,
	}, true
}

func (e *entry) recordSuccess(now time.Time, items []Item) Result {
	e.items = items
	e.fetchedAt = now
	e.hasData = true
	e.notBefore = time.Time{}
	e.lastState, e.lastReason, e.lastMessage = StateOK, ReasonNone, ""
	return Result{Items: items, FetchedAt: now, State: StateOK}
}

// recordFailure classifies a provider failure and decides whether to serve the
// retained entry. A warm cache degrades to stale; only a cold cache surfaces an
// error state.
func (e *entry) recordFailure(now time.Time, err error) Result {
	state, reason, message := classify(err)
	e.lastState, e.lastReason, e.lastMessage = state, reason, message

	switch reason {
	case ReasonAuthFailed:
		// Not transient: back off rather than retry at the normal cadence.
		e.notBefore = now.Add(AuthBackoff)
	case ReasonUnavailable:
		// Honor Retry-After and extend the TTL. Retrying into a 429 is the one
		// behavior guaranteed to make it worse.
		var rl *trackerjira.RateLimitError
		if errors.As(err, &rl) && rl.RetryAfter > 0 {
			e.notBefore = now.Add(rl.RetryAfter)
		}
	case ReasonNone, ReasonConfigAbsent, ReasonCredentialsMissing, ReasonConfigInvalid:
		// Nothing to back off: a config or credential problem is fixed by a
		// person editing config, and the next call should see the change.
	}

	if e.hasData {
		// Serve the retained entry with its ORIGINAL fetchedAt: reporting the
		// failed refresh time would claim freshness we do not have.
		return Result{
			Items:     e.items,
			FetchedAt: e.fetchedAt,
			Stale:     true,
			State:     StateOK,
		}
	}
	return Result{State: state, Reason: reason, Message: message}
}

// classify maps a provider error onto the lane's state vocabulary.
//
// A missing credential is not_configured rather than error: an integration
// nobody has finished setting up is a normal state. A rejected query is a
// visible config error and must never be mistaken for an empty result, because
// "zero tickets" and "wrong project key" look identical on screen.
func classify(err error) (State, Reason, string) {
	msg := err.Error()
	switch {
	case errors.Is(err, trackerjira.ErrNoToken):
		return StateNotConfigured, ReasonCredentialsMissing, msg
	case errors.Is(err, trackerjira.ErrAuthFailed):
		return StateError, ReasonAuthFailed, msg
	case errors.Is(err, trackerjira.ErrBadQuery),
		errors.Is(err, trackerjira.ErrBadKey),
		errors.Is(err, trackerjira.ErrHostNotAllowed),
		errors.Is(err, trackerjira.ErrUnscopedQuery),
		errors.Is(err, trackerjira.ErrWrongProvider):
		return StateError, ReasonConfigInvalid, msg
	default:
		// 5xx, timeouts, and 429 are all "try again later".
		return StateError, ReasonUnavailable, msg
	}
}
