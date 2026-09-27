// Package jira implements ports.Tracker against the Jira REST API v3 as a
// read-only adapter. It was added for the display-only backlog lane on the
// sessions board, but it implements the shared tracker port and must behave
// correctly for every consumer of that port.
//
// # Every ListFilter narrowing dimension is translated into JQL
//
// List translates Statuses, State, Labels, and Assignee into JQL conjuncts, and
// refuses a query that narrows nothing with ErrUnscopedQuery.
//
// This is deliberate and unconditional, and the reason is a different caller
// than the one this adapter was written for.
// backend/internal/observe/trackerintake/observer.go starts one worker session
// per issue returned by List. An adapter that silently dropped Assignee would
// turn an assignee-scoped intake poll into "spawn a session for every ticket in
// the project" — the exact runaway TrackerIntakeConfig.Validate's assignee
// requirement exists to prevent. A project-key-only query has the same shape,
// hence ErrUnscopedQuery.
//
// Anyone adding a field to domain.ListFilter must either add its translation
// here or record in this comment that this adapter ignores it deliberately.
// Silence is not an acceptable third option.
//
// # domain.Issue.Body is always empty
//
// Jira Cloud returns fields.description as Atlassian Document Format: nested
// JSON, not text. The backlog lane is display-only and has no agent prompt to
// hydrate, so the body was descoped rather than flattened, and the adapter does
// not request the description field at all.
//
// The empty Body is therefore a decision, not a bug. A future caller that needs
// it should flatten the ADF node tree to plain text inside this adapter — never
// in a renderer, and never by handing raw ADF JSON to an agent.
//
// # TrackerRepo and TrackerID carry Jira meanings
//
// These two differ from every other tracker adapter in the tree:
//
//   - TrackerRepo.Native is the Jira PROJECT KEY (e.g. "ACME"), not a
//     repository path. Jira projects are not repositories and a Jira project
//     key cannot be derived from a git origin URL.
//   - TrackerRepo.Host and TrackerID.Host are the Jira SITE (e.g.
//     "acme.atlassian.net") and are REQUIRED. Unlike the GitLab adapter, where
//     the zero value means gitlab.com, Jira has no default site: an empty Host
//     is rejected with ErrHostNotAllowed before any credential is attached.
//   - TrackerID.Native is the issue key (e.g. "ACME-123").
//
// Because domain.InferTrackerProvider maps every non-GitHub host to gitlab and
// can never return jira, neither the site nor the project key can be inferred.
// Both must be configured explicitly; see domain.TrackerBacklogConfig.
//
// # Normalized state is lossy here by design
//
// mapStatusCategory maps Jira's three statusCategory values onto the normalized
// vocabulary and never synthesizes IssueInReview or IssueCancelled, because Jira
// offers no instance-independent signal for either. Callers that need to show a
// workflow status to a person should not render the normalized state.
package jira
