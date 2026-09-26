package domain

import (
	"fmt"
	"net/url"
	"strings"
)

// TrackerProvider identifies an issue-tracker provider implementation.
type TrackerProvider string

// TrackerProviderGitHub, TrackerProviderGitLab, and TrackerProviderJira are the
// supported issue-tracker providers.
const (
	TrackerProviderGitHub TrackerProvider = "github"
	TrackerProviderGitLab TrackerProvider = "gitlab"
	TrackerProviderJira   TrackerProvider = "jira"
)

// TrackerID identifies one issue. Native is the provider's own canonical form
// ("owner/repo#123" for GitHub, "group/project#123" for GitLab, "ACME-123" for
// Jira) and is parsed by the adapter.
//
// Host is the GitLab instance host (e.g. "gitlab.example.com") or the Jira site
// (e.g. "acme.atlassian.net"). For GitLab the zero value "" means the default
// host gitlab.com, so all existing call sites that construct TrackerID without
// setting Host continue to work unchanged. Jira has no default site: the Jira
// adapter rejects an empty Host.
type TrackerID struct {
	Provider TrackerProvider `json:"provider"`
	Native   string          `json:"native"`
	// Host is the GitLab instance host ("" means gitlab.com) or the Jira site
	// (required).
	Host string `json:"host,omitempty"`
}

// NormalizedIssueState is the cross-provider issue-state vocabulary every
// adapter must implement. The closed list is intentional — adding a value
// here is a port-level decision because every adapter must map it.
type NormalizedIssueState string

// The normalized cross-provider issue states.
const (
	IssueOpen       NormalizedIssueState = "open"
	IssueInProgress NormalizedIssueState = "in_progress"
	IssueInReview   NormalizedIssueState = "review"
	IssueDone       NormalizedIssueState = "done"
	IssueCancelled  NormalizedIssueState = "cancelled"
)

// Issue is the minimum projection every tracker can produce. Provider-specific
// metadata stays inside provider-specific code paths.
type Issue struct {
	ID        TrackerID            `json:"id"`
	Title     string               `json:"title"`
	Body      string               `json:"body"`
	State     NormalizedIssueState `json:"state"`
	URL       string               `json:"url"`
	Labels    []string             `json:"labels,omitempty"`
	Assignees []string             `json:"assignees,omitempty"`
}

// TrackerRepo identifies a repository for cross-issue queries like Tracker.List.
// Native is the provider's canonical owner/project form, e.g. "owner/repo"
// for GitHub or "group/project" for GitLab. For Jira it is not a repository
// path at all: it is the Jira PROJECT KEY (e.g. "ACME").
//
// Host is the GitLab instance host (e.g. "gitlab.example.com") or the Jira site
// (e.g. "acme.atlassian.net"). For GitLab the zero value "" means the default
// host gitlab.com, so all existing call sites that construct TrackerRepo
// without setting Host continue to work unchanged. Jira has no default site:
// the Jira adapter rejects an empty Host.
type TrackerRepo struct {
	Provider TrackerProvider `json:"provider"`
	Native   string          `json:"native"`
	// Host is the GitLab instance host ("" means gitlab.com) or the Jira site
	// (required).
	Host string `json:"host,omitempty"`
}

// ListStateFilter narrows Tracker.List results by the provider's coarse
// state (open vs closed). It is intentionally NOT the 5-value normalized
// enum — finer filtering (e.g. "only in-review issues") goes through the
// Labels field of ListFilter.
type ListStateFilter string

// Coarse list-state filters for Tracker.List.
const (
	// ListAll is the zero value and returns issues in any state.
	ListAll    ListStateFilter = ""
	ListOpen   ListStateFilter = "open"
	ListClosed ListStateFilter = "closed"
)

// ListFilter is the query the Session Manager passes to Tracker.List.
// Empty / zero values mean "no filter on this dimension".
//
// Limit is an optional total-result cap. Adapters choose their own provider
// page size.
type ListFilter struct {
	State    ListStateFilter `json:"state,omitempty"`
	Labels   []string        `json:"labels,omitempty"`
	Assignee string          `json:"assignee,omitempty"`
	// Statuses narrows Tracker.List to the provider's own workflow status
	// names. Like Assignee, provider-specific values are passed through
	// unchanged — these are the provider's status strings (e.g. Jira's
	// "Ready", "In Review"), not NormalizedIssueState values. Adapters that
	// have no equivalent concept ignore it.
	Statuses []string `json:"statuses,omitempty"`
	Limit    int      `json:"limit,omitempty"`
}

// TrackerIntakeConfig controls issue-driven worker spawning for a project.
// Enabled requires an explicit assignee eligibility rule so turning intake on
// cannot accidentally drain an entire issue backlog.
type TrackerIntakeConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	// Provider defaults to github when Enabled is true. Supported values:
	// "github" and "gitlab".
	Provider TrackerProvider `json:"provider,omitempty" enum:"github,gitlab"`
	// Repo is the provider-native repository key ("owner/repo" for GitHub,
	// "group/project" for GitLab). When empty, the intake loop derives it from
	// the project's repo origin URL.
	Repo string `json:"repo,omitempty"`
	// Assignee narrows eligible issues to one assignee. Provider-specific values
	// such as "*" are passed through unchanged.
	Assignee string `json:"assignee,omitempty"`
}

// WithDefaults leaves the provider empty when not explicitly set so the
// caller can infer it from the project's repo origin URL at use time (see
// InferTrackerProvider). Disabled intake leaves the zero value untouched so
// empty project configs still store as NULL.
func (c TrackerIntakeConfig) WithDefaults() TrackerIntakeConfig {
	return c
}

// InferTrackerProvider guesses the tracker provider from a repo origin URL.
// GitHub hosts (github.com, *.github.com, *.ghe.io) map to github; any other
// host maps to gitlab (self-managed GitLab uses arbitrary hostnames). The
// empty-string fallback is github for backward compatibility.
func InferTrackerProvider(repoURL string) TrackerProvider {
	raw := strings.TrimSpace(repoURL)
	if raw == "" {
		return TrackerProviderGitHub
	}
	// SSH form git@host:path → normalise to https://host/path
	if strings.HasPrefix(raw, "git@") {
		if _, rest, ok := strings.Cut(raw, "@"); ok {
			if host, path, ok := strings.Cut(rest, ":"); ok {
				raw = "https://" + host + "/" + path
			}
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return TrackerProviderGitHub
	}
	host := strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
	if host == "github.com" || strings.HasSuffix(host, ".github.com") || strings.HasSuffix(host, ".ghe.io") {
		return TrackerProviderGitHub
	}
	return TrackerProviderGitLab
}

// Validate rejects accidental broad intake and unknown providers. An empty
// provider is accepted here and inferred from the repo URL at use time.
func (c TrackerIntakeConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Provider != "" && c.Provider != TrackerProviderGitHub && c.Provider != TrackerProviderGitLab {
		return fmt.Errorf("trackerIntake.provider: unsupported provider %q", c.Provider)
	}
	if err := validateNoWhitespaceField("trackerIntake.repo", c.Repo); err != nil {
		return err
	}
	if err := validateNoWhitespaceField("trackerIntake.assignee", c.Assignee); err != nil {
		return err
	}
	if strings.TrimSpace(c.Assignee) == "" {
		return fmt.Errorf("trackerIntake: assignee is required when enabled")
	}
	return nil
}

// TrackerBacklogConfig controls the read-only backlog lane on the sessions
// board. It is display-only: eligible tickets are listed and linked, and
// nothing else.
//
// It is deliberately NOT part of TrackerIntakeConfig, whose Enabled flag arms
// issue-driven worker spawning — see
// backend/internal/observe/trackerintake/observer.go, which starts one worker
// session per eligible issue. Enabling display must never be able to enable
// spawning, so the two live in independent structs with independent Enabled
// flags rather than sharing one.
type TrackerBacklogConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	// Provider must be set explicitly when Enabled: unlike TrackerIntakeConfig
	// there is nothing to infer from, because InferTrackerProvider maps every
	// non-GitHub host to gitlab and a Jira site never appears in a git origin.
	Provider TrackerProvider `json:"provider,omitempty" enum:"jira"`
	// Site is the Jira site (e.g. "acme.atlassian.net"). There is no default.
	Site string `json:"site,omitempty"`
	// ProjectKey is the Jira project key (e.g. "ACME"), not a repo path.
	ProjectKey string `json:"projectKey,omitempty"`
	// Statuses are the provider's own workflow status names that make a ticket
	// eligible for the lane (e.g. "Ready"). At least one is required when
	// Enabled: an empty list would scope the query to the whole Jira project.
	Statuses []string `json:"statuses,omitempty"`
}

// WithDefaults leaves the zero value untouched. Unlike TrackerIntakeConfig
// there is no deferred inference either — an absent provider cannot be derived
// from the project's repo origin URL, so Validate requires it explicitly.
func (c TrackerBacklogConfig) WithDefaults() TrackerBacklogConfig {
	return c
}

// Validate rejects an enabled backlog config that cannot produce a scoped
// query. Site and ProjectKey are required explicitly because no derivation
// exists, and Statuses must be non-empty because a project-key-only query
// would return every ticket in the Jira project.
func (c TrackerBacklogConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Provider != TrackerProviderJira {
		return fmt.Errorf("trackerBacklog.provider: must be %q when enabled, got %q", TrackerProviderJira, c.Provider)
	}
	if err := validateNoWhitespaceField("trackerBacklog.site", c.Site); err != nil {
		return err
	}
	if strings.TrimSpace(c.Site) == "" {
		return fmt.Errorf("trackerBacklog.site: is required when enabled")
	}
	if err := validateNoWhitespaceField("trackerBacklog.projectKey", c.ProjectKey); err != nil {
		return err
	}
	if strings.TrimSpace(c.ProjectKey) == "" {
		return fmt.Errorf("trackerBacklog.projectKey: is required when enabled")
	}
	if len(c.Statuses) == 0 {
		return fmt.Errorf("trackerBacklog.statuses: at least one status is required when enabled")
	}
	for i, s := range c.Statuses {
		name := fmt.Sprintf("trackerBacklog.statuses[%d]", i)
		if err := validateNoWhitespaceField(name, s); err != nil {
			return err
		}
		if s == "" {
			return fmt.Errorf("%s: must not be blank", name)
		}
	}
	return nil
}
