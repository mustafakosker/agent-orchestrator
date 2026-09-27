# Jira Board Column Design

**Date:** 2026-09-26

## Goal

Add a read-only Jira backlog lane to the AO sessions board: a leading column, left of the four session lanes, listing the Jira tickets a project has declared eligible for work. Clicking a card opens the ticket in Jira in the system browser.

The lane answers one question — "what is queued for this project that nobody has started yet" — and answers it by displaying provider data. It is display-only in the strongest sense: no ticket in the lane can cause AO to create, change, or terminate anything.

It is configured in the app, in a new Backlog section of project settings, following the existing tracker-intake settings pattern.

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
- **No credential entry in the UI.** Jira credentials stay environment-based and are never typed into a form or persisted to project config. See Component 5.
- **No Jira fields in the create-project flow.** `CreateProjectAgentSheet` is untouched; backlog configuration is settings-only. Justified in Component 5.
- **No Jira lane on the archive panel.** `SessionsArchiveView` is untouched.
- **No sprint, epic, Jira-board, or saved-filter concepts.** The lane's *configuration surface* is workflow status and nothing else. This is about what a project can configure, not about what the adapter can express: the adapter honors every `domain.ListFilter` dimension because it implements a shared port (Component 1), but the backlog path sets only statuses.
- **No Jira webhooks or push delivery.** The lane polls.

## Eligibility

A project declares a Jira site, a project key, and a set of Jira workflow status names (e.g. `Ready`), through the settings pane described in Component 5. A ticket is eligible when it belongs to that project key and its current status is one of those names.

The backlog service sets **no other filter** — no assignee rule, no label rule, no age rule — and there is no configuration surface for one. (The adapter itself can express those filters, because it implements a port shared with other callers; the backlog path simply never asks for them. See Component 1.)

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

`List` rejects a `repo.Provider` that is not `jira` with an `ErrWrongProvider` sentinel, matching both existing adapters, then builds one JQL query whose conjuncts are the project key from `repo.Native` plus **every** narrowing dimension the `ListFilter` actually sets.

Requirements on query construction:

- Status names are quoted, because Jira status names routinely contain spaces (`In Review`, `Selected for Development`). A double quote or backslash inside a status name is escaped rather than concatenated raw. The same escaping applies to every quoted operand below.
- Multiple statuses become one `status IN (...)` clause, not several `OR`-ed comparisons.
- The project key is validated against a conservative Jira project-key shape before interpolation; a key that does not match is rejected with an `ErrBadKey`-style sentinel rather than sent to Jira.

#### Every `ListFilter` narrowing dimension is honored

**The adapter must not silently drop a filter that narrows results.** Each field of `domain.ListFilter` gets an explicit JQL translation:

| `ListFilter` field | JQL conjunct |
|---|---|
| `Statuses` (non-empty) | `status IN ("Ready", "In Review")` |
| `State` = `ListOpen` | `statusCategory != Done` |
| `State` = `ListClosed` | `statusCategory = Done` |
| `State` = `ListAll` (zero value) | no conjunct — this is the documented "no filter on this dimension" case |
| `Labels` (non-empty) | `labels IN ("backend", "p1")` |
| `Assignee` = `*` | `assignee IS NOT EMPTY` |
| `Assignee` = any other value | `assignee = "<value>"` |
| `Limit` | not a JQL concern; see pagination below |

`State` maps to `statusCategory` rather than to a status-name list because category is the only instance-independent open/closed signal Jira has. `statusCategory != Done` is used for open rather than `statusCategory IN ("To Do", "In Progress")` because the category *names* are themselves localizable while the Done category is stable.

`Assignee` needs the `*` special case because `TrackerIntakeConfig.Assignee` documents that "provider-specific values such as `*` are passed through unchanged", and GitHub's API reads `*` as "assigned to anyone". Jira has no `*`; its equivalent is `assignee IS NOT EMPTY`. Any other value is passed through as a quoted JQL operand — an accountId on Jira Cloud, a username on Data Center — matching the pass-through contract of the field. The adapter does not attempt to resolve names to accountIds; a value the site does not accept produces a 400, which maps to `ErrBadQuery` and is surfaced, not swallowed.

#### Why honoring, not erroring

The alternative — rejecting `State`, `Labels`, and `Assignee` with an explicit error when set — was considered and rejected. Honoring them is better on every axis that matters here:

- **Both options remove the hazard; only one leaves the adapter usable.** An error is safe but converts any future intake wiring into a hard failure that must be fixed by implementing this translation anyway. The translation is a handful of lines and one test each; deferring it buys nothing and leaves a landmine phrased as a guard.
- **The port contract means what it says.** `ports.Tracker.List` documents `ListFilter` as the query the caller passes; an adapter that answers "I refuse queries with an assignee" is conforming to the signature but not the contract, and `multi.Tracker` routes to it by provider with no way for a caller to know which subset each sub-tracker accepts.
- **Erroring is only fail-closed for callers that check the error.** Honoring is fail-closed for all of them.

#### Unconditional narrowing guard

`List` returns an explicit `ErrUnscopedQuery` when the resulting JQL would have **no narrowing conjunct at all** — that is, `Statuses`, `Labels`, and `Assignee` are all empty and `State` is `ListAll`. A project-key-only query returns every ticket in the Jira project.

This guard is unconditional and lives in the adapter, not in a caller, because the runaway shape it prevents is caller-specific: `backend/internal/observe/trackerintake/observer.go` spawns **one worker session per issue returned by `List`** (`observer.go:206`), so an unscoped query reaching intake means one session per ticket in the project. The backlog lane cannot trip this — its config validation already requires a non-empty status list — but the adapter implements the shared port, and the guard costs one condition.

#### Pagination

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

**Amendment (2026-09-26, found during implementation).** As first written this section was impossible: `domain.Issue` had no field for a provider-native status string, so `ports.Tracker.List` — the adapter's only output — could not carry the status name the lane is required to render. `ports/tracker.go` compounded it by stating that "richer per-provider metadata belongs behind a separate port".

Resolved by adding one additive field, `domain.Issue.ProviderState string`, carrying the provider's own status string **for display only, never to branch on**. The Jira adapter populates it from `fields.status.name`. `ports.Tracker`'s doc comment now records this as the single, bounded exception to its normalize-everything rule, so the boundary stays honestly documented rather than quietly violated.

Rejected alternatives: a second port for one string (disproportionate, and `backlogsvc` would have to call two ports and join); rendering the normalized state (guts the feature — a lane configured for `Ready` would show every card as "Open"); and a Jira-only method outside the port (reintroduces the "callers cannot know what each sub-tracker supports" problem this spec argues against for filter handling).

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

`ErrUnscopedQuery` and `ErrBadKey` are raised before any request is made.

### `doc.go`

The package doc comment must record, prominently and as the reason rather than the rule, the two constraints a future caller is most likely to trip over:

1. **Every `ListFilter` narrowing dimension is translated into JQL, and `List` refuses an unscoped query with `ErrUnscopedQuery`.** State why: `backend/internal/observe/trackerintake/observer.go` spawns one worker session per returned issue, so a silently dropped `Assignee` would turn an assignee-scoped poll into one session per ticket in the project. Anyone adding a field to `domain.ListFilter` must add its translation here or accept that this adapter ignores it deliberately.
2. **`domain.Issue.Body` is always empty for Jira.** State why — Jira Cloud returns `description` as Atlassian Document Format and this feature had no prompt consumer that needed it — so a future reader treats the empty field as a decision with a known remedy (flatten ADF in the adapter) rather than a bug.

It must also record that `TrackerRepo.Native` is a project key and `TrackerRepo.Host` is a required site with no default, because those two meanings differ from every other tracker adapter in the tree.

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

`config.Load` populates it from the environment, following the existing GitLab block at `backend/internal/config/config.go:323-341` exactly: `AO_JIRA_ALLOWED_HOSTS` is a comma-separated host list parsed with the same trim-and-skip-empty loop, and `AO_JIRA_HOST_CREDENTIALS` is parsed by the existing `parseHostTokenMap` helper. The default credential comes from `AO_JIRA_EMAIL` + `AO_JIRA_API_TOKEN` (Cloud) or `AO_JIRA_PAT` (Data Center), read by the adapter's `EnvCredentialSource` rather than by `config.Load`, matching how the GitHub and GitLab default tokens are resolved.

Credentials therefore live in daemon configuration and the environment, never in project config. Component 5 explains why the settings form must not offer to store them.

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
GET /api/v1/projects/backlog
```

registered in `ProjectsController.Register` (`backend/internal/httpd/controllers/projects.go:26`) alongside the existing project routes. The response body:

```json
{
  "items": [ { "key": "...", "title": "...", "status": "...", "assignee": "...", "url": "...", "projectId": "..." } ],
  "fetchedAt": "2026-09-26T10:00:00Z",
  "stale": false,
  "state": "ok",
  "reason": null
}
```

`state` is one of `ok`, `not_configured`, or `error`. `fetchedAt` always describes when `items` was actually fetched from Jira, never when the request was served.

`reason` is a machine-readable discriminator carried by **both** non-`ok` states, because each has two causes the UI must tell apart:

| `state` | `reason` | Cause |
|---|---|---|
| `not_configured` | `config_absent` | The project has no backlog config, or it is disabled. |
| `not_configured` | `credentials_missing` | Backlog config is present and enabled, but no Jira credential resolved. |
| `error` | `auth_failed` | 401/403 — the credential was rejected or lacks Browse Projects. |
| `error` | `config_invalid` | 400 — unknown project key, or an operand the site rejects. Carries Jira's message. |
| `error` | `unavailable` | 5xx, timeout, or 429 with a cold cache. |

The `config_absent` / `credentials_missing` split is load-bearing for the UI: one is fixed in the settings form and the other is fixed in the environment, and a single "not configured" state would point half of all users at the wrong place.

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

### Where the guards go, and why

`TrackerIntakeConfig.Validate` requires an explicit assignee rule specifically so that "turning intake on cannot accidentally drain an entire issue backlog" — the hazard being one config flag spawning an unbounded number of sessions. That hazard motivates defense in depth, because the cost of a mistake is runaway session creation with real side effects.

**For the backlog path, that hazard does not exist.** The worst outcome of a wrong or over-broad backlog config is that the lane displays too many tickets, or the wrong ones. Nothing is created, nothing is written, nothing is transitioned, and nothing is spent. A **config-level validation check is therefore sufficient for this feature**: validation exists to keep the configuration honest and the error messages useful, not to contain a blast radius, because on this path there is none.

The distinction that matters is **which layer the reasoning applies to**:

- **Project config** is consumed only by the backlog service. Its validation is therefore scoped to keeping the lane's configuration coherent, and it needs no blast-radius guard.
- **The adapter** implements the shared `ports.Tracker`, and `backend/internal/observe/trackerintake/observer.go` is an existing consumer of that port. It therefore carries two unconditional guards that have nothing to do with the backlog lane: every `ListFilter` narrowing dimension is translated into JQL, and `ErrUnscopedQuery` rejects a query with no narrowing conjunct at all. Both are specified in Component 1.

So the design is defensive in exactly two places, and for two different reasons: at the intake boundary, structurally (two independent config structs, so display can never arm spawning), and inside the adapter, behaviorally (so a future intake wiring cannot inherit a silently over-broad query). Neither guard exists to protect the lane.

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

`frontend/src/renderer/components/SessionsBoard.tsx` calls `useProjectBacklog`, and passes `leadingColumn` to `SessionsBoardGridView` (`SessionsBoard.tsx:236`) unconditionally on the grid branch, even when backlog configuration is absent or disabled. The lane's header renders the count, the refresh action, and — depending on `state` and `reason` — one of the states below.

**Explicit human decision (2026-09-27):** unconditional visibility overrides the original opt-in design and the reviewers' recommendation. The accepted tradeoff is that users who never use Jira see a Connect Jira lane. Scratch project boards are the exception: their config cannot contain backlog settings, so they omit the lane. The all-projects board offers named non-scratch project choices for Connect Jira rather than silently picking a project; if there are no eligible projects it explains that a repository project is needed.

The aggregate route delegates to `ReadyAll` and carries `failures` with each affected projectId, state, reason and message alongside successful items. Aggregate fetchedAt is the oldest contributing fetch and stale is true if any contributor is stale. The UI reports each failure with its project name without hiding healthy tickets.

The existing empty and welcome states are unchanged: the backlog lane appears only on the branch that already renders the grid, so `BoardWelcome` and `ProjectBoardEmpty` keep taking precedence over it.

### Lane body by state

| `state` / `reason` | Lane body |
|---|---|
| `ok`, items present | Jira cards |
| `ok`, items empty | "No ready tickets" — an honest empty result, visually distinct from every state below |
| `not_configured` / `config_absent` | **"Connect Jira"** affordance: opens project settings at the Backlog section |
| `not_configured` / `credentials_missing` | A distinct message naming the environment variables to set, with a documentation link. **No settings deep-link**, because the form has no credential field to point at |
| `error` / `auth_failed` | "Reauthenticate" message |
| `error` / `config_invalid` | The configuration error, carrying Jira's message, plus a link to the Backlog settings section |
| `error` / `unavailable` | An unavailable message (cold cache only; a warm cache serves stale instead) |
| `ok` with `stale: true` | Cards plus an "as of {{time}}" indicator |

The **"Connect Jira" affordance routes into the app, not out of it.** It opens the settings dialog on the project's Backlog section via the existing UI store's settings action (`useUiStore`'s settings open/close actions, as already used by `ProjectSettingsForm` for `closeSettings`) with `ProjectSettingsSection` set to `"backlog"` — the same mechanism `SettingsDialog` already uses to select a project section (`SettingsDialog.tsx:72`). An external documentation link would be a worse answer to "this is not set up" when the thing that sets it up is two panes away.

The one state that *does* link out is `credentials_missing`, because its fix is an environment variable and no in-app surface owns it.

### Localization

New lane strings — lane label, "Connect Jira", the credentials-missing message, the "as of {{time}}" staleness indicator, the empty-result message, the config-error and auth-error messages, the unassigned indication, and the refresh action's accessible label — are added to **all eight locale catalogs** in `frontend/src/renderer/i18n/`: `en.json`, `zh-CN.json`, `ja.json`, `ko.json`, `es.json`, `fr.json`, `de.json`, `pt-BR.json` (`APP_LOCALES` in `frontend/src/shared/ui-locale.ts:2`). The settings strings from Component 5 are added to the same eight files.

Three existing tests enforce this and none may be weakened: `frontend/src/renderer/i18n/instance.test.ts` asserts every locale covers every English key with a non-empty value, and that interpolation variables match English per key; `frontend/src/renderer/i18n/renderer-coverage.test.ts` fails on new English literals in renderer chrome. The "as of" string is the one with an interpolated variable, so its placeholder must be identical across all eight files.

No strings are added to product-ui's `englishLabels` catalog, because the lane label arrives as a prop.

---

## Component 5 — Project settings UI

Backlog display is configured in the app, in the project settings dialog. There is an established pattern for exactly this shape of setting sitting next to it, and the feature follows it rather than expecting anyone to reach for an HTTP client.

### A fifth section, not fields inside `intake`

`ProjectSettingsSection` (`frontend/src/renderer/components/ProjectSettingsForm.tsx:58`) is today:

```ts
export type ProjectSettingsSection = "general" | "agents" | "workflow" | "intake";
```

**Decision: add a fifth section, `"backlog"`.** Not fields inside `intake`.

The `intake` section is titled "Tracker intake" (`ProjectSettingsForm.tsx:658`) and its entire content is the switch that arms per-issue worker spawning. Putting the Jira backlog toggle under that heading would place two switches in one pane where one spawns sessions and one does not, under a title that describes only the first. The domain model went to the trouble of making these two independent structs specifically so that display can never arm spawning (Component 3); undoing that separation in the one place a person actually reads and clicks would defeat the point. Config-model separation with UI co-location is the worst of both: it reads as one feature with two checkboxes.

Three practical arguments point the same way:

- The nav label would become wrong. `SettingsDialog.tsx:62` renders one label per section (`t("settings.project.intake")`, `Inbox` icon); a second unrelated group under it either mislabels the pane or forces a vague relabel like "Integrations" that describes neither thing well.
- The "Connect Jira" affordance needs a stable deep-link target. A section id is one; "the second card inside the intake pane" is not.
- The scratch-project fallback is per-section (`ProjectSettingsForm.tsx:667`). Both features reject scratch projects, but for the same reason via independent guards — keeping them in separate sections keeps those guards independent too.

Cost: one more nav item. That is cheap next to the cost of someone reading a display toggle as a spawn toggle, or the reverse.

Implementation touchpoints: extend the `ProjectSettingsSection` union; add the entry to `projectSections` in `SettingsDialog.tsx:62` with its own icon and label; add the `{section === "backlog" && ...}` branch in `ProjectSettingsForm`'s body, wrapped in `ProjectSettingsSection` with `grouped`, alongside a scratch-project fallback matching the `intake` branch's shape.

### `JiraBacklogFields`

New component `frontend/src/renderer/components/JiraBacklogFields.tsx`, modeled on `frontend/src/renderer/components/IntakeFields.tsx` and matching its structure point for point:

- **Flat, string-backed form type.** `IntakeForm` is deliberately flat strings plus one boolean so the settings form can hold it in its single `useState` object and patch it field-by-field. `BacklogForm` is the same shape:

  ```ts
  export type BacklogForm = {
      enabled: boolean;
      site: string;
      projectKey: string;
      statuses: string;
  };
  ```

  `statuses` is a `string`, not `string[]`, to keep the type flat — see the entry format below.

- **A `buildBacklog` serializer** paired with the component, mirroring `buildIntake`: it takes the form plus the existing `TrackerBacklogConfig` so unmodeled fields set by a future CLI flag survive a UI save, scrubs empty values to `undefined`, and returns `undefined` entirely when every field is empty so a blank config serializes to an omitted key rather than an empty object the daemon would persist. It is the function that splits `statuses` into the array the API expects.

- **A client-side mirror of the backend guard**, `backlogNeedsScope(form)`, mirroring `TrackerBacklogConfig.Validate` exactly as `intakeNeedsRule` mirrors `TrackerIntakeConfig.Validate`: when `enabled`, `site`, `projectKey`, and `statuses` must all be non-blank. The mirror is a convenience, not the authority — the server validates independently and its error is surfaced if the two ever disagree.

- **A `variant` prop (`"default" | "settings"`) and a `compact` mode**, matching `IntakeFields`' signature so the component is card-agnostic and reusable. The settings form uses `variant="settings"`, which renders each field as a `SettingsRow` + `SettingsInlineInput` pair from `frontend/src/renderer/components/settings/SettingsRow.tsx`. The `compact` path is implemented for signature parity but has no caller in this iteration (see below).

- **Enable-then-reveal.** A `Switch` in a `SettingsRow` for `enabled`; the site, project key, and statuses rows render only when `enabled` is true — the same disclosure `IntakeFields` uses.

- **An inline validation message** in the style of `IntakeAssigneeError`: a `TriangleAlert` icon plus error text, rendered when `backlogNeedsScope` is true and suppressed in `compact` mode.

Fields, in order: enable toggle; **Site** (placeholder `acme.atlassian.net`); **Project key** (placeholder `PROJ`); **Statuses** (placeholder `Ready`).

### Statuses: one comma-separated input

**Decision: a single comma-separated text input**, split on `,`, each entry trimmed, empties dropped. Not repeatable rows.

- The settings form has **no repeatable-row primitive**. `SettingsRow.tsx` exports `SettingsRow`, `SettingsInlineInput`, and `SettingsInputRow` — all single-value. Even `reviewers`, the one genuinely list-shaped project config field, is rendered as a single default-reviewer select rather than a repeatable list. Introducing add/remove-row machinery for a field that usually holds one value would be new UI for no gain, and it would be the only control of its kind in the dialog.
- It keeps the form type flat, which is the explicit design of `IntakeForm` and what lets `patchBacklog` stay a three-line object spread.
- There is an established comma-separated convention for exactly this data: `AO_JIRA_ALLOWED_HOSTS` follows `AO_GITLAB_ALLOWED_HOSTS` (`config.go:323-333`), which is a comma-separated list parsed with the same trim-and-skip-empty loop. The UI and the environment then read the same way.
- Comma is safe as a separator here: Jira status names routinely contain **spaces** (`In Review`, `Selected for Development`) — which is why the adapter quotes them — but not commas.

**Accepted limitation, stated rather than hidden:** a Jira status name containing a literal comma cannot be expressed through this input. It is vanishingly rare, the field's help text says the separator is a comma, and the workaround is the config API. If it ever comes up, the fix is to upgrade this one control to repeatable rows without touching the wire format, since `buildBacklog` already emits an array.

### Settings only, not the create-project flow

**Decision: settings only.** `CreateProjectAgentSheet` is not modified, and the `compact` variant ships without a caller.

- The create sheet is deliberately minimal — `compact` mode exists precisely to strip prose down to a tooltip (`IntakeFields.tsx`). Backlog config needs **two required free-text values with no derivation fallback**: a Jira site and a project key. Intake gets away with one field in the sheet because its repo is derived from the git origin server-side; there is nothing to derive here, and `domain.InferTrackerProvider` cannot help (Component 3).
- Someone creating a project frequently does not have the site and project key to hand. Two required fields with no defaults, in the flow that gates project creation, trades a real cost for a setting that is purely additive and equally available five seconds later.
- It would widen the create path's failure surface: `canSubmitProjectSetup` in `packages/product-ui/src/project-models.ts:63` would need the same new validation, so a half-filled optional integration could block project creation.
- Nothing about the lane is first-run valuable. An empty board has no backlog lane worth looking at yet.

### Credentials are never in the form

The form collects **site, project key, and statuses only**. It never collects an email, an API token, or a PAT, and there is no hidden or advanced field that does.

Project config is persisted as a plaintext JSON blob in the `config` column on `projects` (`migrations/0008_add_project_config.sql`), readable by any local client through `GET /api/v1/projects/{id}` and returned inside the project DTO the renderer already caches. A Jira API token stored there would be a long-lived credential in a plaintext column, echoed back over the loopback API on every project read. The repository has no precedent for a secret in project config, and every existing tracker credential — `AO_GITHUB_TOKEN`, `AO_GITLAB_TOKEN`, `AO_GITLAB_HOST_TOKENS` — is resolved from the environment or daemon config instead. Jira follows that precedent: `AO_JIRA_EMAIL` + `AO_JIRA_API_TOKEN`, or `AO_JIRA_PAT`, or `AO_JIRA_HOST_CREDENTIALS` for multi-site setups.

This is why the two not-configured causes are distinct states, and why the settings pane must say so. When `enabled` is on and the endpoint reports `credentials_missing`, the Backlog section renders an informational row — not a validation error, because the project config is valid — naming the environment variables and linking to documentation. The form does not block save on it: configuring which tickets to show and providing the credential to fetch them are two separate acts, often by two different people, and the config is legitimately correct before the credential exists.

### Form plumbing

Following the existing intake plumbing in `ProjectSettingsForm`:

- `SettingsBody` reads `const backlog: TrackerBacklogConfig = config.trackerBacklog ?? {}` next to the existing `config.trackerIntake ?? {}` (`ProjectSettingsForm.tsx:138`).
- Four keys join the single `useState` form object (`:139`): `backlogEnabled`, `backlogSite`, `backlogProjectKey`, `backlogStatuses` — the last initialized by joining the persisted array with `", "`.
- A `backlogForm` projection and a `patchBacklog` patcher mirror `intakeForm` / `patchIntake` (`:178-188`).
- The save payload adds `trackerBacklog: buildBacklog(backlogForm, config.trackerBacklog)` next to the existing `trackerIntake: buildIntake(...)` (`:263`).

The payload goes through the route the form already uses: **`PUT /api/v1/projects/{id}`** with a `{ displayName, config }` body (`ProjectSettingsForm.tsx:266`). The separate `PUT /api/v1/projects/{id}/config` route exists but is not the settings form's path, and this feature does not change which route the form uses.

`scratchSupportedConfig` (`ProjectSettingsForm.tsx:866`) adds `trackerBacklog` to the keys it strips, alongside the `trackerIntake` it already strips, so a scratch project cannot submit a field the server would reject.

### Save-blocking validation

`validateProjectSettings` in `packages/product-ui/src/project-models.ts:34` is the shared guard the form calls before mutating (`ProjectSettingsForm.tsx:389`). It gains:

- three values on `ProjectSettingsValues`: `backlogEnabled: boolean`, `backlogSite: string`, `backlogProjectKey: string`, `backlogStatuses: string`;
- new codes on `ProjectSettingsValidationCode`: `backlog_site_required`, `backlog_project_key_required`, `backlog_statuses_required`;
- a guard, gated by a `validateBacklog` option mirroring the existing `validateIntake` option so scratch projects skip it, that returns the matching code when `backlogEnabled` is set and the corresponding field is blank.

The form maps each new code to a translated message in the existing `validation === ...` chain (`ProjectSettingsForm.tsx:393-402`). The codes live in product-ui; the **messages stay in the renderer**, so product-ui's `englishLabels` catalog is untouched — consistent with the lane label arriving as a prop (Component 4).

Because `validateProjectSettings` returns the first failure it finds, and the existing checks (`agents_required`, `name_required`, `name_too_long`, `intake_assignee_required`) run first, the new codes cannot mask an existing one.

### A note on the existing `IntakeFields` comment

`IntakeFields.tsx` carries this comment today:

> Adding Linear/Jira later means: the backend enum grows, `IntakeFields` gains a provider `<Select>` + per-provider scope fields, and `buildIntake` switches the scope field it emits.

That describes adding Jira to **intake** — issue-driven *spawning* — which is a different, unbuilt feature. This spec does not implement it and does not contradict it: `IntakeFields`, `IntakeForm`, `buildIntake`, `intakeNeedsRule`, and `TrackerIntakeConfig` are all left exactly as they are. A reader who finds that comment while implementing this spec should understand that the Jira work here is the read-only board lane, that it lives in its own component and its own config struct, and that the comment's future is still open.

---

## Failure behavior

All six cases below are required, and all of them are survivable precisely because of the governing invariant: the lane is a view.

**Nothing configured.** The project has no backlog config, or it is disabled. The endpoint returns **HTTP 200** with `state: "not_configured"`, `reason: "config_absent"`, and makes zero tracker calls. The lane renders the **"Connect Jira"** affordance, which opens the Backlog settings section.

**No credentials configured.** Backlog config is present and enabled, but `lazyTracker.resolve` fails to build the Jira adapter and returns `ErrNoToken`. The endpoint returns **HTTP 200** with `state: "not_configured"`, `reason: "credentials_missing"` — **not a 500.** An integration nobody has set up is a normal state, not a fault, and a 500 would make an unconfigured install look broken and pollute error telemetry. The lane names the environment variables to set and links to documentation; it does **not** offer the settings deep-link, because the fix is not in the form. The same path serves a `nil` tracker from `newMultiTracker`, which reports `credentials_missing` when config is present.

These two are deliberately distinct. Both mean "no tickets yet", and they are fixed in entirely different places — one in the settings dialog, one in the environment. Collapsing them into one state would send half of all affected users to the wrong place, which is the same failure mode the 400-vs-empty-list mapping below exists to prevent.

**Jira unreachable, 5xx, or timeout.** The service serves the **retained stale cache** with a truthful `fetchedAt` and `stale: true`, and the lane shows an "as of {{time}}" indicator. `fetchedAt` never reports the time of the failed refresh. **Only a cold cache** — a failure with nothing retained to serve — surfaces `state: "error"`.

**401 or 403.** Categorically different from a transient failure, and treated as such. A rejected credential will be rejected again on the next tick, so the service **backs off** rather than retrying at the normal cadence, and surfaces a distinct "reauthenticate" message instead of a generic error. This mirrors the distinction `logTrackerDisabled` already draws between "no usable token" and "setup failed": a credential problem needs a person, not a retry.

**429.** The service honors `Retry-After` from `httpkit.RateLimitError`, **extends the cache TTL** to at least that duration, and **keeps serving stale** data meanwhile. Server-side backoff is the sole rate-limit mechanism: the service refuses provider calls before its private `notBefore` deadline. Client polling continues every 60 seconds, reaching only the local daemon and returning cached data during that window, with zero provider calls. No client-visible `retryAt` is added; it would add contract surface without additional provider protection.

**Unknown project key or missing Browse Projects permission.** Jira answers **400** to the JQL rather than returning an empty result set. This maps to a **visible configuration error** — `state: "error"` with a config reason, carrying Jira's own message — and **never to an empty list.** On screen, "this project has zero ready tickets" and "the project key is wrong" are indistinguishable; conflating them sends someone to debug their Jira workflow when the actual fault is one string in AO's config. This mapping is the difference between a five-second fix and an afternoon.

**All-projects board view (`projectId` undefined).** The service fetches per configured project and merges the results, and each card is badged with its project. The per-project results are **independent**: one project's failure must not blank the others. The merged response reports the oldest contributing `fetchedAt` and is `stale: true` if any contributor is stale, so the indicator never overstates freshness. A failing project contributes its own error state, surfaced in the lane, without suppressing the tickets that did load.

---

## Testing plan

### Adapter

`httptest`-backed table tests in `backend/internal/adapters/tracker/jira/tracker_test.go`, following the structure of `backend/internal/adapters/tracker/github/tracker_test.go` and `backend/internal/adapters/tracker/gitlab/tracker_test.go`:

- **JQL construction**: the query contains both the project key and the status conjunct; multiple statuses become one `IN` clause; status names containing spaces (`In Review`, `Selected for Development`) are correctly quoted; a status name containing a quote or backslash is escaped.
- **Every narrowing filter is honored** — one case per row of the translation table in Component 1, asserting the conjunct is present in the outgoing query: `State` open → `statusCategory != Done`; `State` closed → `statusCategory = Done`; `State` unset → no category conjunct; `Labels` → `labels IN (...)`; `Assignee` = `*` → `assignee IS NOT EMPTY`; `Assignee` = a literal value → `assignee = "<value>"`. Plus a combined case asserting all conjuncts co-exist in one query. **These tests are the regression guard against a silently dropped filter**, which is why each is asserted individually rather than through one composite query.
- **Unscoped-query guard**: a `ListFilter` with no `Statuses`, no `Labels`, no `Assignee`, and `State` unset returns `ErrUnscopedQuery` and the test server records **zero requests**.
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
- 200 with `state: "not_configured"`, `reason: "config_absent"` for a project with no backlog config;
- 200 with `state: "not_configured"`, `reason: "credentials_missing"` for a configured project whose tracker build fails with `ErrNoToken`, and for a nil tracker with config present — **asserting the two reasons are distinguishable**, since the UI branches on them;
- 200 with `stale: true` and a truthful `fetchedAt` when serving stale data;
- **a provider failure never yields a 500** — asserted for the unreachable, 401, 429, and 400 cases;
- each `error` reason (`auth_failed`, `config_invalid`, `unavailable`) is returned for its corresponding adapter sentinel;
- `fetchedAt` is the fetch time, not the response time.

`TestRouteSpecParity` covers route/spec correspondence and needs no new test.

### Renderer — board lane

vitest + React Testing Library, in the style of `packages/product-ui/src/SessionsBoardView.test.tsx`, `frontend/src/renderer/components/SessionsBoard.test.tsx`, and `frontend/src/renderer/__tests__/integration/board-empty-states.test.tsx`:

- the leading lane renders Jira cards while **all four session lanes stay unaffected** — same sessions, same lanes, same order as without the prop;
- the grid resolves to **five columns with a leadingColumn and four without it** — this is the regression guard on replacing the hardcoded `grid-cols-4`, and it asserts the resolved class and the rendered column count, not just the prop;
- `reason: "config_absent"` renders the connect affordance and no cards, and clicking it opens project settings at the `"backlog"` section (asserted against the UI store's settings action, not a navigation spy);
- `reason: "credentials_missing"` renders the environment-variable message and **no** settings deep-link — asserted as distinct from the `config_absent` body, since a single shared "not configured" rendering is exactly the regression this guards;
- `state: "ok"` with `stale: true` renders the "as of" indicator;
- `state: "ok"` with zero items renders the empty-result message, asserted distinct from every error and not-configured body;
- `state: "error"` renders **visibly**, asserted specifically not to be indistinguishable from an empty lane;
- clicking a Jira card calls `openLinkInSystemBrowser` with the ticket URL and does **not** trigger in-app navigation (asserted against a spy on the router's navigate);
- the manual refresh action triggers exactly one refetch;
- **load-bearing regression test**: no session card can render inside the backlog lane and no Jira card inside any session lane. Asserted by querying each lane by its `data-column` / lane test id and checking the card test ids present, with a fixture containing both sessions and tickets. This is the test that would catch a future refactor collapsing the two models back together.

### Renderer — settings

Following the tracker-intake cases already in `frontend/src/renderer/components/ProjectSettingsForm.test.tsx` (`:1593` saves intake settings, `:1635` preserves unmodeled fields, `:1677` blocks save on a missing rule) and `frontend/src/renderer/components/CreateProjectAgentSheet.test.tsx`:

- **saves backlog settings**: render at `section="backlog"`, enable the switch, fill site / project key / statuses, save, and assert the `PUT /api/v1/projects/{id}` body's `config.trackerBacklog` equals the expected object — including `statuses` as a **split, trimmed array**, from an input containing spaces after the commas;
- **statuses round-trip**: a persisted `statuses` array renders joined in the input, and saving without editing produces the identical array (no drift from join/split);
- **preserves unmodeled fields**: an existing `trackerBacklog` carrying a field the form does not model survives a save, mirroring the intake test at `:1635`;
- **scrubs to omitted**: a disabled, blank backlog form serializes `trackerBacklog` to `undefined`, not `{}`;
- **blocks save on incomplete scope**: enabling with a blank site, then a blank project key, then blank statuses each block the save and surface the matching message — three cases, mirroring `:1677`;
- **`validateProjectSettings` unit cases** in `packages/product-ui/src/ProjectViews.test.tsx`: each new code is returned for its blank field; `validateBacklog: false` skips all three; the existing codes still take precedence;
- **section isolation**: the Backlog section renders no intake controls and the Intake section renders no Jira controls — the UI-level counterpart to the config-model separation, and the test that fails if someone later merges the two panes;
- **scratch projects**: the Backlog section renders the fallback and no controls, and `scratchSupportedConfig` strips `trackerBacklog` from a submitted config (mirroring `ProjectSettingsForm.test.tsx:1569`, which asserts the intake pane is absent);
- **credentials notice**: with config enabled and the endpoint reporting `credentials_missing`, the section renders the informational row and **save is not blocked** — valid config plus a missing credential is not a validation error;
- **create sheet unchanged**: `CreateProjectAgentSheet.test.tsx` gains one assertion that no Jira backlog controls are present, pinning the settings-only decision so it is not silently reversed.

### Commands

`go test ./...` from `backend/`, and frontend typecheck, tests, and build — mirroring the commands in `.github/workflows/`.

---

## Delivery sequencing

1. `domain.TrackerProviderJira`, `domain.ListFilter.Statuses`, `domain.TrackerBacklogConfig` with validation and tests. No behavior change yet.
2. The Jira adapter with its `httptest` table tests, written test-first.
3. `config.JiraConfig`, `newJiraTracker`, the `"jira"` entry in `newMultiTracker` behind `lazyTracker`, and the `logTrackerDisabled` arm.
4. `backlogsvc` with its cache, single-flight, and failure semantics, against a fake tracker.
5. The controller, the `specgen/build.go` registry entry, and `npm run api`.
6. `validateProjectSettings`'s new codes in product-ui, then `JiraBacklogFields`, `buildBacklog`, the `"backlog"` section in `ProjectSettingsForm` and `SettingsDialog`, and the `scratchSupportedConfig` strip — so the feature is configurable before it is visible.
7. `SessionsBoardGridView`'s optional leading column and derived grid sizing, with the four-vs-five-column test.
8. `useProjectBacklog`, `JiraTicketCard`, `SessionsBoard` wiring, the state-to-body mapping including the settings deep-link, and the eight locale catalogs.

Each step is independently testable, and steps 1 through 5 change nothing a user can see. Step 6 makes the setting reachable while the lane is still absent, which is deliberate: it is inert without step 8, and it means the first person to see the lane can already have configured it. The board is not affected until step 7, and no Jira call is made until a project is configured.
