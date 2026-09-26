package ports

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Tracker is the outbound read-only port for issue trackers:
//
//   - Get returns a normalized snapshot of one issue, used by spawn-bootstrap
//     to hydrate the agent prompt.
//   - List returns a filtered slice of issues in a repo, used when the SM
//     needs to enumerate work (e.g. backlog view, status sweeps).
//   - Preflight verifies the configured credential is actually valid against
//     the provider so daemons fail fast at startup, not at first request.
//
// Provider differences are absorbed inside each adapter via
// domain.NormalizedIssueState. The one exception is
// domain.Issue.ProviderState, which carries the provider's own status string
// for display because normalization is lossy and some surfaces must show the
// unreduced value; it must never be branched on. Any richer per-provider
// metadata than that belongs behind a separate port.
type Tracker interface {
	Get(ctx context.Context, id domain.TrackerID) (domain.Issue, error)
	List(ctx context.Context, repo domain.TrackerRepo, filter domain.ListFilter) ([]domain.Issue, error)
	Preflight(ctx context.Context) error
}
