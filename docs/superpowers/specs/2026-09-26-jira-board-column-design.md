# Jira Board Column Design

**Date:** 2026-09-26

## Goal

Add a read-only Jira backlog lane to the AO sessions board: a leading column, left of the four session lanes, listing the Jira tickets a project has declared eligible for work. Clicking a card opens the ticket in Jira in the system browser.

The lane answers one question — "what is queued for this project that nobody has started yet" — and answers it by displaying provider data. It is display-only in the strongest sense: no ticket in the lane can cause AO to create, change, or terminate anything.

## Governing invariant

**The lane is a view, never a source of truth.** Nothing in AO's session lifecycle, Kanban derivation, notification path, or intake path reads it. No AO write ever reaches Jira.

Every design decision below follows from that invariant, and it is what licenses the deliberately simple error handling: every failure mode degrades to "the lane is empty, stale, or shows a message" and affects nothing else in the product. A Jira outage cannot delay a spawn, blank a session card, move a session between lanes, or block daemon boot.

## Out of scope

These exclusions are deliberate decisions, not omissions. A later reader must not treat any of them as forgotten work.

- **No spawning.** An eligible ticket does not create a worker session, and there is no button, menu item, drag target, or keyboard path on a Jira card that creates one. Issue-driven spawning already exists behind `domain.TrackerIntakeConfig` and `backend/internal/observe/trackerintake/observer.go`; this feature neither extends nor reuses it.
- **No writes to Jira.** No comments, no field edits, no worklogs, no attachments, no assignment.
- **No status transitions.** AO never moves a ticket between Jira workflow statuses, in either direction, for any reason.
- **No ticket/session correlation.** The lane does not know whether a session exists for a ticket, does not hide tickets that have one, does not badge them, and does not link them. A ticket and a session are unrelated objects in this design.
- **No Jira issue as a spawn prompt.** The ticket body never becomes agent input, so no prompt-hydration path is touched.
- **No `ports.Tracker.Get` usage by this feature.** The Jira adapter implements `Get` because the port requires it; the backlog path calls only `List`.
- **No settings-form UI.** `frontend/src/renderer/components/ProjectSettingsForm.tsx` gains no Jira fields. Configuration in this iteration is written through the existing `PUT /api/v1/projects/{id}/config` route. The lane's not-configured state links out to documentation rather than opening an in-app form.
- **No Jira lane on the archive panel.** `SessionsArchiveView` is untouched.
- **No sprint, epic, board, or filter concepts.** Eligibility is Jira workflow status and nothing else.
- **No Jira webhooks or push delivery.** The lane polls.

## Eligibility

A project declares a Jira site, a project key, and a set of Jira workflow status names (e.g. `Ready`). A ticket is eligible when it belongs to that project key and its current status is one of those names. There is no other filter — no assignee rule, no label rule, no age rule.

The status list is deliberately provider-native: it holds Jira's own workflow status *names*, not AO's normalized states, because "which statuses count as ready for work" is an instance-specific workflow question that no normalized vocabulary can answer. `Ready` on one instance is `Selected for Development` on another.

---

## Component 1 — Jira read adapter

New package `backend/internal/adapters/tracker/jira/` with `tracker.go`, `auth.go`, `doc.go`, and `tracker_test.go`, mirroring the existing shape of `backend/internal/adapters/tracker/github/` and `backend/internal/adapters/tracker/gitlab/`. It implements `ports.Tracker` (`Get`, `List`, `Preflight`; see `backend/internal/ports/tracker.go`) and reuses `backend/internal/adapters/tracker/httpkit` for rate-limit error construction, error-body message extraction, the issue-append-with-limit helper, and the preflight cache — the same way both existing adapters do.

### Provider identity and domain vocabulary

`backend/internal/domain/tracker.go` gains:

```go
TrackerProviderJira TrackerProvider = "jira"
```

alongside the existing `TrackerProviderGitHub` and `TrackerProviderGitLab`.

The existing `TrackerRepo` and `TrackerID` fields are reused with Jira meanings, and the adapter is the only code that interprets them:

- `TrackerRepo.Native` is the **Jira project key** (e.g. `ACME`), not a repository path.
- `TrackerRepo.Host` is the **Jira site** (e.g. `acme.atlassian.net`). Unlike GitLab, where `Host: ""` means `gitlab.com`, Jira has no default site: an empty `Host` is a configuration error, rejected before any request.
- `TrackerID.Native` is the Jira issue key (e.g. `ACME-123`), and `TrackerID.Host` carries the site so the id round-trips.

The doc comments on `TrackerRepo.Native`, `TrackerRepo.Host`, `TrackerID.Native`, and `TrackerID.Host` are updated to state the Jira meanings alongside the GitHub and GitLab ones. This is a documentation change only; no field is added and no existing call site changes.

### `List` and JQL construction

`List` rejects a `repo.Provider` that is not `jira` with an `ErrWrongProvider` sentinel, matching both existing adapters, then builds one JQL query from exactly two conjuncts:

1. the project key from `repo.Native`, and
2. the status set from `filter.Statuses`.

Both are always present in the query — `filter.Statuses` empty means "no status conjunct", which would return the entire project and is therefore rejected by config validation before it can reach the adapter (see Component 3).

Requirements on query construction:

- Status names are quoted, because Jira status names routinely contain spaces (`In Review`, `Selected for Development`). A double quote or backslash inside a status name is escaped rather than concatenated raw.
- Multiple statuses become one `status IN (...)` clause, not several `OR`-ed comparisons.
- The project key is validated against a conservative Jira project-key shape before interpolation; a key that does not match is rejected with a `ErrBadKey`-style sentinel rather than sent to Jira.
- `filter.State` (the coarse open/closed `ListStateFilter`) is **not** translated into JQL. Jira's open/closed notion is `statusCategory`, and the caller has already specified exact statuses; adding a second, coarser state filter would silently narrow the explicit one. The adapter ignores `filter.State` and says so in its doc comment.
- `filter.Labels` and `filter.Assignee` are ignored for the same reason — the backlog path does not set them, and honoring them would create filter combinations with no configuration surface.

`List` uses `/rest/api/3/search/jql` with a fixed page size and paginates until the response reports no further page. `filter.Limit` is a **total** cap across pages, applied via `httpkit.AppendIssuesWithLimit`, exactly as `gitlab.List` does — the page size stays at the provider maximum. A `maxListPages` guard matching the GitLab adapter's pattern fails loud rather than looping if pagination never terminates.

Each returned issue carries `ID.Provider = jira`, `ID.Native` = the issue key, `ID.Host` = `repo.Host`, and a `URL` of `https://<host>/browse/<key>`.

### State mapping

Jira's `fields.status.statusCategory.key` maps to `domain.NormalizedIssueState`:

| Jira `statusCategory.key` | `domain.NormalizedIssueState` |
|---|---|
| `new` | `IssueOpen` |
| `indeterminate` | `IssueInProgress` |
| `done` | `IssueDone` |
| anything else, or absent | `IssueOpen` |

**`IssueInReview` and `IssueCancelled` are never synthesized.** Jira's status-category vocabulary has exactly three values and offers no instance-independent signal for either one. "In Review" on one instance is an `indeterminate` workflow status named anything at all; "Cancelled" is a `done` status distinguishable from "Done" only by its name, which varies per project. The GitHub adapter can produce `IssueInReview` because it reads conventional labels, and `IssueCancelled` because GitHub has a first-class `not_planned` close reason; Jira has neither. Guessing from status names would be wrong on most instances and wrong silently. An unknown or missing category maps to `IssueOpen` rather than erroring, because a display-only lane must not fail on an unrecognized workflow.

The normalized state is carried for port conformance. **The card renders Jira's own status name**, not the normalized state, because the normalized state would collapse the exact distinction the lane exists to show.

### `Preflight`

`Preflight` issues one `GET /rest/api/3/myself` and caches success for the lifetime of the tracker via `httpkit`'s preflight cache; failures are not cached, so a user who fixes credentials later is picked up without a daemon restart. This matches both existing adapters.

`ports.Tracker.Preflight` currently has **no production caller** — `backend/internal/daemon/tracker_wiring.go:164` only forwards it from `lazyTracker`, and `backend/internal/adapters/tracker/multi/tracker.go:82` only fans it out. This feature must not add one. In particular, nothing at daemon boot may call `Preflight` on the multi-tracker: `multi.Preflight` probes every registered sub-tracker and returns the first error, so an unconfigured Jira would begin reporting a boot-time failure that does not exist today.

### Authentication

Jira supports two credential shapes and they use different HTTP schemes:

- **Jira Cloud**: HTTP Basic with `email:api_token`.
- **Jira Data Center**: Bearer with a personal access token.

The existing `github.TokenSource` and `scmgitlab.TokenSource` interfaces both yield a single opaque string, which cannot express "which scheme, and an email as well". The Jira package therefore defines its own minimal source returning both parts:

```go
// Credential is one resolved Jira credential and the scheme it is presented with.
type Credential struct {
    Scheme SchemeKind // SchemeBasic or SchemeBearer
    Email  string     // required for SchemeBasic, empty for SchemeBearer
    Token  string
}

type CredentialSource interface {
    Credential(ctx context.Context) (Credential, error)
}
```

This mirrors the existing `TokenSource` contract in every respect that matters:

- `StaticCredentialSource` for tests, `EnvCredentialSource` for `AO_JIRA_EMAIL`/`AO_JIRA_API_TOKEN` and `AO_JIRA_PAT`, and `FallbackCredentialSource` composing them in precedence order — the same three-shape set both existing adapters provide.
- `New` performs a **fail-fast presence check with no network call**, returning `ErrNoToken` when no source yields a credential, exactly as `github.New` does.
- The credential is **re-read on every request**, not captured at construction, so a rotated token is picked up without a daemon restart. This is the explicit contract in `github.TokenSource`'s doc comment and the Jira adapter keeps it.

Scheme selection is by the resolved credential's own shape, not by host heuristics: a credential carrying an email is Basic, one without is Bearer. An `AO_JIRA_EMAIL` set without a token, or a token set without an email while no PAT is present, is a configuration error surfaced as `ErrNoToken` rather than a malformed request.

The package defines a `ErrNoToken` sentinel. Because `logTrackerDisabled` in `backend/internal/daemon/tracker_wiring.go` currently recognizes only `trackergithub.ErrNoToken` and `trackergitlab.ErrNoToken`, it gains a `trackerjira.ErrNoToken` arm — without it, a missing Jira credential would be logged as "tracker disabled: setup failed" rather than "no usable token", conflating a normal unconfigured state with a fault.

### Host allowlisting

Like the GitLab adapter, the Jira adapter is constructed with an explicit host allowlist and per-host credentials, and rejects any host not on the list **before attaching a credential** with an `ErrHostNotAllowed` sentinel. There is no implicit default host. This prevents a project config from pointing AO's Jira credential at an arbitrary server.

### Error mapping

`classifyError` follows the GitLab adapter's structure, mapping status codes to sentinels so no caller reads raw HTTP codes:

| Status | Sentinel | Notes |
|---|---|---|
| 400 | `ErrBadQuery` | Jira answers 400 for an unknown project key or an unparseable JQL clause. Carries the Jira error message so the config error shown on screen names the actual problem. |
| 401, 403 | `ErrAuthFailed` | Credential rejected or lacks Browse Projects permission. |
| 404 | `ErrNotFound` | Issue-level lookups only. |
| 429 | `httpkit.BuildRateLimitError(resp, msg, ErrRateLimited)` | Carries `Retry-After`. |
| other non-2xx | unwrapped status error | |

### Wiring

`backend/internal/daemon/tracker_wiring.go` gains `newJiraTracker(jiraCfg config.JiraConfig) (ports.Tracker, error)`, following `newGitLabTracker`'s host-aware shape: it maps each configured host's credential into a `CredentialSource` and passes the allowlist through.

`config.JiraConfig` is a new struct in `backend/internal/config/config.go`, modeled on the existing `config.GitLabConfig` (`AllowedHosts []string`, `HostTokens map[string]string`):

```go
type JiraConfig struct {
    // AllowedHosts is the list of Jira sites the tracker may talk to
    // (e.g. "acme.atlassian.net", "jira.internal:8443"). There is no
    // implicit default site; a host absent from this list is rejected
    // before any credential is attached.
    AllowedHosts []string
    // HostCredentials maps a site to its credential. Hosts in AllowedHosts
    // without an explicit entry fall back to the default environment
    // credential.
    HostCredentials map[string]JiraCredential
}
```

`newMultiTracker` gains a `"jira"` entry. The key must be the literal `"jira"` because `multi.Tracker` routes on `string(repo.Provider)`.

**The Jira entry is always wrapped in the existing `lazyTracker`.** Unlike `newGitLabTracker`, which is called eagerly, Jira construction is deferred until the first tracker call, so an unconfigured Jira never blocks daemon boot and never emits a boot-time warning. `lazyTracker` already provides exactly the semantics needed: construction is attempted at most once successfully, and failures are not cached, so a user who configures Jira later is picked up without a daemon restart.

`newMultiTracker` continues to return `nil` when no sub-tracker could be registered, and callers continue to nil-guard.

### Note on Atlassian Document Format

Jira Cloud returns `fields.description` as Atlassian Document Format — nested JSON, not text. For most tracker consumers this would matter, because the issue body becomes agent prompt input; here it does not, because this feature is display-only and no agent prompt is involved.

**Recommendation, adopted: descope the ticket body.** `List` leaves `domain.Issue.Body` empty for Jira, and the card renders key, title, status, and assignee only. This is recorded in the adapter's doc comment so a later reader knows the field is empty by decision rather than by bug.

If a future feature needs the body, the flattening belongs in the adapter — walking the ADF node tree to plain text — and not in the renderer. It is not built now.

---

## Component 2 — Backlog service and endpoint

### Package

New package `backend/internal/service/backlog`, imported as `backlogsvc` following the established alias convention (`projectsvc` for `backend/internal/service/project`, as in `backend/internal/httpd/api.go:20`).

It holds a `ports.Tracker` and the project store, and exposes:

```go
func (s *Service) Ready(ctx context.Context, projectID string) (Result, error)
```

`Ready` reads the project's backlog config, calls `Tracker.List` with the configured project key, site, and statuses, and returns the issues.

### Cache

Results are cached with a **60-second TTL and single-flight**, keyed by project id.

The cache is load-bearing, not an optimization. It is the only thing that decouples how often the UI asks from how often AO calls Jira. Several board surfaces, several windows, and a route prefetch can all ask at once; Jira Cloud's rate limits are per-user and unforgiving, and a board left open all day must not trip them. Concretely:

- N concurrent callers inside one TTL window produce **exactly one** provider call; the rest receive the same result.
- A cache entry is retained after its TTL expires, not evicted, so it remains available as stale data when a refresh fails (see Failure behavior).
- The TTL can be extended by a rate-limit response's `Retry-After`.

### Nil-guard

`backlogsvc` must tolerate a `nil` `ports.Tracker`, because `newMultiTracker` returns `nil` when nothing is configured. This mirrors `backend/internal/service/session/issue_context.go:17`, which checks `s.tracker == nil` before use. A nil tracker yields the `not_configured` state, never a panic and never an error.

### Endpoint

```
GET /api/v1/projects/{id}/backlog
```

registered in `ProjectsController.Register` (`backend/internal/httpd/controllers/projects.go:26`) alongside the existing project routes. The response body:

```json
{
  "items": [ { "key": "...", "title": "...", "status": "...", "assignee": "...", "url": "...", "projectId": "..." } ],
  "fetchedAt": "2026-09-26T10:00:00Z",
  "stale": false,
  "state": "ok"
}
```

`state` is one of `ok`, `not_configured`, or `error`; `error` carries a machine-readable `reason` and a message. `fetchedAt` always describes when `items` was actually fetched from Jira, never when the request was served.

Response shapes go in `backend/internal/httpd/controllers/dto.go`, following the repository's existing DTO boundary.

### Contract regeneration

A later implementation must:

1. add the operation to the registry in `backend/internal/httpd/apispec/specgen/build.go`, including a `schemaNames` entry for each new named type; then
2. run `npm run api` (`go generate ./internal/httpd/apispec/...` followed by `openapi-typescript`) to regenerate `backend/internal/httpd/apispec/openapi.yaml` and `frontend/src/api/schema.ts`.

Step 1 is not optional and `npm run api` does not perform it. `TestRouteSpecParity` in `backend/internal/httpd/apispec/parity_test.go` asserts a 1:1 correspondence between mounted `/api/v1` routes and OpenAPI operations, so a route added without a registry entry fails CI. `openapi.yaml` is generated and must never be hand-edited.

---

## Component 3 — Configuration

### A new sibling field, deliberately not inside `TrackerIntakeConfig`

Backlog display is configured by a **new field on `domain.ProjectConfig`, a sibling of `TrackerIntake` and not a field inside it.**

**This is the most important constraint in the design.** Setting `enabled: true` on `domain.TrackerIntakeConfig` arms the intake observer in `backend/internal/observe/trackerintake/observer.go`, which polls the project's tracker and **starts one worker session per eligible issue** (`observer.go:133` gates on `project.Config.TrackerIntake.Enabled`; `observer.go:206` calls `Spawner.Spawn`). That is precisely what this feature must never do.

Nesting backlog configuration inside `TrackerIntakeConfig` would create a field whose enablement is one edit — or one mis-set default, or one partially-populated update — away from spawning a session per ticket in a Jira backlog. **There must be no code path, no default, and no shared flag by which showing tickets can arm spawning.** Two sibling structs with independent `Enabled` flags make that structurally impossible rather than merely intended.

`TrackerIntakeConfig` is not modified by this feature.

```go
// TrackerBacklogConfig controls the read-only backlog lane on the sessions
// board. It is display-only: eligible tickets are listed and linked, and
// nothing else. It is deliberately NOT part of TrackerIntakeConfig, whose
// Enabled flag arms issue-driven worker spawning — see
// backend/internal/observe/trackerintake/observer.go. Enabling display must
// never be able to enable spawning.
type TrackerBacklogConfig struct {
    Enabled    bool            `json:"enabled,omitempty"`
    Provider   TrackerProvider `json:"provider,omitempty" enum:"jira"`
    Site       string          `json:"site,omitempty"`
    ProjectKey string          `json:"projectKey,omitempty"`
    Statuses   []string        `json:"statuses,omitempty"`
}
```

added to `domain.ProjectConfig` in `backend/internal/domain/projectconfig.go` as `TrackerBacklog TrackerBacklogConfig \`json:"trackerBacklog,omitempty"\``.

**No database migration is required.** Project configuration is stored as a single nullable JSON `config` column on `projects` (`backend/internal/storage/sqlite/migrations/0008_add_project_config.sql`), so a new field is additive on read and write.

### Validation

`TrackerBacklogConfig.Validate()` is called from `ProjectConfig.Validate()` next to the existing `c.TrackerIntake.Validate()` call. A disabled config validates trivially, matching `TrackerIntakeConfig`'s shape. When enabled:

- `Provider` must be `jira`, explicitly. There is no inference and no default.
- `Site` is **required** and must be non-empty with no surrounding whitespace.
- `ProjectKey` is **required** and must be non-empty with no surrounding whitespace.
- `Statuses` must be non-empty, and each entry non-blank after trimming. An empty status list would mean "no status conjunct in the JQL", which would return the entire Jira project.
- Field-level whitespace checks reuse `validateNoWhitespaceField`, matching `TrackerIntakeConfig.Validate`.

`Site` and `ProjectKey` must be explicit because **`domain.InferTrackerProvider` can never infer Jira.** It maps GitHub hosts (`github.com`, `*.github.com`, `*.ghe.io`) to `github` and **every other host to `gitlab`**, with an empty-string fallback of `github`. A Jira site is not a git origin and appears nowhere in one; a repo on `github.com` can be tracked in Jira and a repo on a self-managed GitLab can be too. There is no derivation to fall back on, so omitting either field is a validation error rather than a silent default.

`validateScratchProjectConfig` in `backend/internal/service/project/service.go:780` gains a rejection for an enabled backlog config, matching its existing `TrackerIntake` rejection — a scratch project has no repository and no tracker.

### `domain.ListFilter.Statuses`

`domain.ListFilter` gains one field:

```go
// Statuses narrows Tracker.List to the provider's own workflow status
// names. Like Assignee, provider-specific values are passed through
// unchanged — these are the provider's status strings (e.g. Jira's
// "Ready", "In Review"), not domain.NormalizedIssueState values.
// Adapters that have no equivalent concept ignore it.
Statuses []string `json:"statuses,omitempty"`
```

This is a provider-native pass-through field in the same sense as the existing `Assignee` field, whose values (including provider-specific forms such as `*`) are passed through unchanged. It is explicitly **not** the normalized state vocabulary: `ListStateFilter`'s doc comment already records that the coarse state filter is intentionally not the normalized enum, and `Statuses` sits on the other side of that same line — even more provider-specific, and correspondingly documented as such.

The GitHub and GitLab adapters ignore `Statuses`; only the Jira adapter reads it. It is an additive field on a struct that already documents empty values as "no filter on this dimension", so no existing call site changes.

### Why no fail-closed adapter guards

`TrackerIntakeConfig.Validate` requires an explicit assignee rule specifically so that "turning intake on cannot accidentally drain an entire issue backlog" — the hazard being one config flag spawning an unbounded number of sessions. That hazard motivates defense in depth: guards at the adapter layer as well as at config validation, because the cost of a mistake is runaway session creation with real side effects.

**That hazard does not exist here.** The worst outcome of a wrong or over-broad backlog config is that the lane displays too many tickets, or the wrong ones. Nothing is created, nothing is written, nothing is transitioned, and nothing is spent. A **config-level validation check is therefore sufficient**, and no fail-closed adapter guard is added. Validation exists to keep the configuration honest and the error messages useful, not to contain a blast radius — because there is none.

The one place this design *is* defensive is the boundary with intake, and that defense is structural (two independent structs) rather than a guard.

---

## Component 4 — Renderer

### Why the backlog lane cannot be a `KanbanColumn`

**This is the central architectural point of the feature.**

`contract.KanbanColumn` (`backend/pkg/contract/kanban.go`) is the *derived delivery-lifecycle placement of a session*. Its doc comment states it plainly: it answers where a session sits between first commit and merge, which loop is turning it, and it is **never persisted**. Every value is produced by `DeriveKanbanPresentation` from `KanbanSessionFacts` and `KanbanPRFacts` — worker activity, PR draft/merged/closed state, CI state, review decisions, mergeability, AO's own review runs. There is no path into a `KanbanColumn` that does not begin with a session.

`SessionsBoardGridView` (`packages/product-ui/src/SessionsBoardView.tsx:141-147`) groups its input strictly by `toKanbanColumn(session.kanbanColumn, session.status)` into a `Map<KanbanColumn, TSession[]>`, then renders one `BoardColumnView` per entry in its `columns: KanbanColumnView[]` prop.

A Jira ticket is not a session. It has no worker, no activity state, no PR, no CI, no review, no `updatedAt` in the board's sense, and no id the session routes can resolve. Modeling the backlog lane as a fifth `KanbanColumn` would require either inventing a `KanbanColumn` value with no derivation rule — breaking the invariant that columns are derived from session facts — or fabricating session-shaped objects for tickets, which would make them indistinguishable from sessions to every consumer downstream of the grouping map: the card renderer, the attention sort, the archive split, and the session-open handler.

**So the lane sits beside the four session lanes, outside their model**, with its own data source, its own item type, and its own card renderer. The grid renders it as a sibling of the session columns, not as one of them.

### `SessionsBoardGridView` changes

`SessionsBoardGridView` gains one optional prop group describing a leading column that is rendered from its own items with its own renderer, entirely bypassing the `byColumn` grouping:

```ts
leadingColumn?: {
    label: string;
    items: readonly TItem[];
    renderItem: (item: TItem) => ReactNode;
    // presentation (swatch colour, count) supplied by the caller
};
```

The prop is optional and absent by default, so every existing caller — including `packages/product-ui/src/SessionsBoardView.test.tsx` — renders exactly as today.

The lane's **label is passed in as a string by the caller**, not looked up from a `labelKey`. The existing lane labels are resolved through `getKanbanColumnView`, which reads a `SessionPresentationMessageKey` out of `packages/product-ui/src/session-presentation.ts`. Taking the backlog label as a prop keeps product-ui's message catalog and `KanbanColumnView` type free of a non-Kanban concept, and keeps the renderer as the single owner of the new strings.

The hardcoded layout classes on `packages/product-ui/src/SessionsBoardView.tsx:153` —

```tsx
<div className="relative grid h-full min-w-[64rem] grid-cols-4 divide-x divide-border-strong xl:min-w-0">
```

— become derived from the rendered column count, which is `columns.length + (leadingColumn ? 1 : 0)`. Note that `grid-cols-4` is today hardcoded independently of the `columns` prop: the grid renders four lanes only because `boardKanbanColumnOrder` (`packages/product-ui/src/session-presentation.ts:333`) lists four (`archive` is excluded and surfaced separately by `SessionsArchiveView`). Deriving the class and the `min-w` floor from the actual count removes that latent coupling, and makes the five-column case correct rather than clipped.

### Data hook

New `useProjectBacklog(projectId)` in the renderer hooks directory, backed by TanStack Query: a 60-second `refetchInterval` plus an explicit manual refresh action surfaced on the lane header.

**It is deliberately not merged into `workspaceQueryOptions`** (`frontend/src/renderer/hooks/useWorkspaceQuery.ts:263-270`). That query is tuned for a live session board — `staleTime: 10_000`, `retry: 1`, and a `refetchInterval` that drops to **300 ms** while any session status is still checking, otherwise 15 s. Those numbers are right for local session state, which is cheap to read and changes constantly. Applied to a Jira-backed lane they would burn the site's per-user rate limit within minutes of leaving a board open, and a Jira failure would sit inside the query that every session surface depends on.

Keeping it a separate query also means a Jira failure keeps its own error and its own retained data, exactly as the existing `cloudProjectsQueryKey` query is kept separate so "a control-plane failure can never break the local list".

The server-side 60 s cache and the client-side 60 s interval are independent and both intentional: the cache bounds calls to Jira across all clients, the interval bounds how often one client asks AO.

### `JiraTicketCard`

A new card component, **visibly distinct from session cards** — a different surface treatment and an explicit Jira provider mark — so no one reads a ticket as a session at a glance. It shows:

- the issue key (e.g. `ACME-123`),
- the title,
- the Jira workflow status name as sent by the adapter (not the normalized state),
- the assignee, or an explicit unassigned indication,
- on the all-projects board, a project badge.

Clicking the card **opens the ticket in the system browser** via `openLinkInSystemBrowser` from `frontend/src/renderer/lib/external-link-policy.ts:78`. It must not navigate in-app, must not route through the session router, and must not open a webview.

### `SessionsBoard` wiring

`frontend/src/renderer/components/SessionsBoard.tsx` calls `useProjectBacklog`, and passes `leadingColumn` to `SessionsBoardGridView` (`SessionsBoard.tsx:236`) only when the project has backlog display enabled. The lane's header renders the count, the refresh action, and — depending on `state` — the connect affordance, the "as of" staleness indicator, or the config-error message.

The existing empty and welcome states are unchanged: the backlog lane appears only on the branch that already renders the grid, so `BoardWelcome` and `ProjectBoardEmpty` keep taking precedence over it.

### Localization

New lane strings — lane label, "Connect Jira", the "as of {{time}}" staleness indicator, the config-error and auth-error messages, the unassigned indication, and the refresh action's accessible label — are added to **all eight locale catalogs** in `frontend/src/renderer/i18n/`: `en.json`, `zh-CN.json`, `ja.json`, `ko.json`, `es.json`, `fr.json`, `de.json`, `pt-BR.json` (`APP_LOCALES` in `frontend/src/shared/ui-locale.ts:2`).

Three existing tests enforce this and none may be weakened: `frontend/src/renderer/i18n/instance.test.ts` asserts every locale covers every English key with a non-empty value, and that interpolation variables match English per key; `frontend/src/renderer/i18n/renderer-coverage.test.ts` fails on new English literals in renderer chrome. The "as of" string is the one with an interpolated variable, so its placeholder must be identical across all eight files.

No strings are added to product-ui's `englishLabels` catalog, because the lane label arrives as a prop.

---

## Failure behavior

All six cases below are required, and all of them are survivable precisely because of the governing invariant: the lane is a view.

**No credentials configured.** `lazyTracker.resolve` fails to build the Jira adapter and returns `ErrNoToken`. The endpoint returns **HTTP 200** with `state: "not_configured"` — **not a 500.** An integration nobody has set up is a normal state, not a fault, and a 500 would make an unconfigured install look broken and pollute error telemetry. The lane renders a "Connect Jira" affordance in place of cards. The same path serves a `nil` tracker from `newMultiTracker`.

**Jira unreachable, 5xx, or timeout.** The service serves the **retained stale cache** with a truthful `fetchedAt` and `stale: true`, and the lane shows an "as of {{time}}" indicator. `fetchedAt` never reports the time of the failed refresh. **Only a cold cache** — a failure with nothing retained to serve — surfaces `state: "error"`.

**401 or 403.** Categorically different from a transient failure, and treated as such. A rejected credential will be rejected again on the next tick, so the service **backs off** rather than retrying at the normal cadence, and surfaces a distinct "reauthenticate" message instead of a generic error. This mirrors the distinction `logTrackerDisabled` already draws between "no usable token" and "setup failed": a credential problem needs a person, not a retry.

**429.** The service honors `Retry-After` from `httpkit.RateLimitError`, **extends the cache TTL** to at least that duration, and **keeps serving stale** data meanwhile. The client refetch interval is also suppressed until the window passes. Jira Cloud's limits are per-user and unforgiving, and a board left open all day must not trip them; retrying into a 429 is the one behavior guaranteed to make it worse.

**Unknown project key or missing Browse Projects permission.** Jira answers **400** to the JQL rather than returning an empty result set. This maps to a **visible configuration error** — `state: "error"` with a config reason, carrying Jira's own message — and **never to an empty list.** On screen, "this project has zero ready tickets" and "the project key is wrong" are indistinguishable; conflating them sends someone to debug their Jira workflow when the actual fault is one string in AO's config. This mapping is the difference between a five-second fix and an afternoon.

**All-projects board view (`projectId` undefined).** The service fetches per configured project and merges the results, and each card is badged with its project. The per-project results are **independent**: one project's failure must not blank the others. The merged response reports the oldest contributing `fetchedAt` and is `stale: true` if any contributor is stale, so the indicator never overstates freshness. A failing project contributes its own error state, surfaced in the lane, without suppressing the tickets that did load.

---

## Testing plan

### Adapter

`httptest`-backed table tests in `backend/internal/adapters/tracker/jira/tracker_test.go`, following the structure of `backend/internal/adapters/tracker/github/tracker_test.go` and `backend/internal/adapters/tracker/gitlab/tracker_test.go`:

- **JQL construction**: the query contains both the project key and the status conjunct; multiple statuses become one `IN` clause; status names containing spaces (`In Review`, `Selected for Development`) are correctly quoted; a status name containing a quote or backslash is escaped; `filter.State`, `filter.Labels`, and `filter.Assignee` are ignored.
- **State mapping**: all three `statusCategory` keys (`new`, `indeterminate`, `done`) plus an unknown key and an absent category; and an assertion that `IssueInReview` and `IssueCancelled` are **never** produced.
- **Pagination**: `ListFilter.Limit` is honored as a **total cap across pages**, not per page; the `maxListPages` guard fails loud rather than looping.
- **Auth**: Basic is selected for an email-bearing credential and Bearer for a PAT; the credential is **re-read on every request**, proven by a source that returns a different value on the second call and a server that asserts the second request's header changed; `New` fails fast with `ErrNoToken` and no network call when no credential resolves.
- **Host allowlist**: a host absent from the allowlist is rejected with `ErrHostNotAllowed` **before any request is made** (asserted by the test server recording zero requests); an empty `Host` is rejected.
- **Error mapping**: 400 with an unknown-project-key body maps to `ErrBadQuery` and preserves Jira's message; 401 and 403 map to `ErrAuthFailed`; 429 maps to a `RateLimitError` carrying the parsed `Retry-After`; a 5xx maps to a plain status error.
- **Body descoping**: `domain.Issue.Body` is empty for a Jira issue whose response carries an ADF `description`, and decoding the nested ADF JSON does not error.
- **Provider guard**: `List` and `Get` reject a non-Jira `Provider` with `ErrWrongProvider`.

Plus an **opt-in real-credential test** at `backend/internal/adapters/tracker/jira/live_test.go`, gated on environment variables and skipped by default, following the precedent of `cloud/internal/sandbox/coder/live_test.go`. It must never run in CI and must never be required for a green build. AGENTS.md prohibits adding network calls to tests absent an established pattern for them; the gated live test is that pattern, and the `httptest` tests above are the ones that actually gate the build.

### Service

Tests against a fake `ports.Tracker` that counts calls:

- **Single-flight**: N concurrent `Ready` calls inside one TTL window produce **exactly one** tracker call, and all N receive the same result.
- **Warm-cache failure**: a tracker failure with retained data returns the cached items with `stale: true` and the original `fetchedAt`.
- **Cold-cache failure**: a tracker failure with nothing retained returns the error state.
- **Auth failure**: a 401/403 produces the reauthenticate reason and backs off — a second call inside the backoff window does not hit the tracker again.
- **Rate limit**: a 429 with `Retry-After` extends the TTL and keeps serving stale data for at least that long.
- **Config error**: a 400 from the adapter produces a visible config-error state, asserted explicitly **not** to be an empty successful result.
- **Disabled or absent config**: returns an empty result and makes **zero** tracker calls.
- **Nil tracker**: returns `not_configured` and does not panic.
- **All-projects merge**: with two configured projects where one fails, the successful project's items are returned and badged, the failure is reported, and neither result suppresses the other.

### HTTP

Controller tests in `backend/internal/httpd/controllers/`:

- 200 with `state: "ok"` and correctly shaped items for a configured project;
- 200 with `state: "not_configured"` for an unconfigured project and for a nil tracker;
- 200 with `stale: true` and a truthful `fetchedAt` when serving stale data;
- **a provider failure never yields a 500** — asserted for the unreachable, 401, 429, and 400 cases;
- `fetchedAt` is the fetch time, not the response time.

`TestRouteSpecParity` covers route/spec correspondence and needs no new test.

### Renderer

vitest + React Testing Library, in the style of `packages/product-ui/src/SessionsBoardView.test.tsx`, `frontend/src/renderer/components/SessionsBoard.test.tsx`, and `frontend/src/renderer/__tests__/integration/board-empty-states.test.tsx`:

- the leading lane renders Jira cards while **all four session lanes stay unaffected** — same sessions, same lanes, same order as without the prop;
- the grid resolves to **five columns with the backlog enabled and four without** — this is the regression guard on replacing the hardcoded `grid-cols-4`, and it asserts the resolved class and the rendered column count, not just the prop;
- `state: "not_configured"` renders the connect affordance and no cards;
- `state: "ok"` with `stale: true` renders the "as of" indicator;
- `state: "error"` renders **visibly**, asserted specifically not to be indistinguishable from an empty lane;
- clicking a Jira card calls `openLinkInSystemBrowser` with the ticket URL and does **not** trigger in-app navigation (asserted against a spy on the router's navigate);
- the manual refresh action triggers exactly one refetch;
- **load-bearing regression test**: no session card can render inside the backlog lane and no Jira card inside any session lane. Asserted by querying each lane by its `data-column` / lane test id and checking the card test ids present, with a fixture containing both sessions and tickets. This is the test that would catch a future refactor collapsing the two models back together.

### Commands

`go test ./...` from `backend/`, and frontend typecheck, tests, and build — mirroring the commands in `.github/workflows/`.

---

## Delivery sequencing

1. `domain.TrackerProviderJira`, `domain.ListFilter.Statuses`, `domain.TrackerBacklogConfig` with validation and tests. No behavior change yet.
2. The Jira adapter with its `httptest` table tests, written test-first.
3. `config.JiraConfig`, `newJiraTracker`, the `"jira"` entry in `newMultiTracker` behind `lazyTracker`, and the `logTrackerDisabled` arm.
4. `backlogsvc` with its cache, single-flight, and failure semantics, against a fake tracker.
5. The controller, the `specgen/build.go` registry entry, and `npm run api`.
6. `SessionsBoardGridView`'s optional leading column and derived grid sizing, with the four-vs-five-column test.
7. `useProjectBacklog`, `JiraTicketCard`, `SessionsBoard` wiring, and the eight locale catalogs.

Each step is independently testable, and steps 1 through 5 change nothing a user can see. The board is not affected until step 6, and no Jira call is made until a project is configured.
