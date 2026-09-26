package jira

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/tracker/httpkit"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	defaultUserAgent = "ao-agent-orchestrator/tracker-jira"

	// Jira's maxResults for the JQL search endpoint caps at 100.
	// ListFilter.Limit is an optional total-result cap; page size stays at the
	// provider max.
	listPageSize = 100
	// Guard against a server that always claims another page. At Jira's max
	// page size this still permits a 5k-issue sweep before failing loud.
	maxListPages = 50

	searchPath    = "/rest/api/3/search/jql"
	issuePath     = "/rest/api/3/issue/"
	myselfPath    = "/rest/api/3/myself"
	requestFields = "summary,status,assignee,labels"
)

// Sentinel errors. Adapter-level callers should match on these via errors.Is;
// the orchestrator's lifecycle code is intentionally insulated from raw HTTP
// status codes.
var (
	ErrNotFound      = errors.New("jira tracker: issue not found")
	ErrRateLimited   = errors.New("jira tracker: rate limited")
	ErrAuthFailed    = errors.New("jira tracker: authentication failed")
	ErrWrongProvider = errors.New("jira tracker: id is not a jira tracker id")
	ErrBadID         = errors.New("jira tracker: malformed issue key")
	ErrBadKey        = errors.New("jira tracker: malformed project key")
	// ErrBadQuery is Jira's 400: an unknown project key, an operand the site
	// rejects, or otherwise unparseable JQL. It must reach the user as a
	// visible configuration error, because "zero tickets" and "wrong project
	// key" look identical on screen.
	ErrBadQuery       = errors.New("jira tracker: query rejected")
	ErrHostNotAllowed = errors.New("jira tracker: host not in allowlist")
	// ErrUnscopedQuery refuses a query with no narrowing conjunct. See the
	// package doc comment for why this is unconditional.
	ErrUnscopedQuery = errors.New("jira tracker: refusing unscoped query")
)

// RateLimitError is an alias for httpkit.RateLimitError so callers can use
// errors.As with *RateLimitError. The sentinel field is set to ErrRateLimited
// by classifyError.
type RateLimitError = httpkit.RateLimitError

// projectKeyRe is a conservative Jira project-key shape: an uppercase letter
// followed by uppercase letters, digits, or underscores. Keys are validated
// before interpolation into JQL rather than being sent to Jira.
var projectKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,59}$`)

// issueKeyRe is a project key, a hyphen, and a positive issue number.
var issueKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,59}-[1-9]\d*$`)

// Options configures a Tracker. Only Credential is required; tests inject
// HTTPClient and HostBaseURLs to point at an httptest fake.
//
// AllowedHosts is the list of Jira sites the tracker may talk to. There is no
// implicit default site — unlike GitLab, where Host "" means gitlab.com — so a
// host absent from this list is rejected before any credential is attached.
//
// HostCredentials maps a site to a credential override. Sites in AllowedHosts
// without an explicit entry fall back to Credential.
type Options struct {
	Credential      CredentialSource
	HTTPClient      *http.Client
	UserAgent       string
	AllowedHosts    []string
	HostCredentials map[string]CredentialSource
	// HostBaseURLs overrides the derived https://<host> base URL per site. It
	// exists for tests pointing at an httptest server; production leaves it nil.
	HostBaseURLs map[string]string
}

// hostEntry holds the per-site base URL and credential source.
type hostEntry struct {
	baseURL string
	creds   CredentialSource
}

// Tracker implements ports.Tracker against the Jira REST API v3.
//
// Construction performs a fail-fast credential presence check (no network
// call). The first Preflight call validates the credential against Jira
// itself; success is cached for the lifetime of the Tracker while failures are
// intentionally NOT cached, so fixing a credential later is picked up without
// a daemon restart.
type Tracker struct {
	http      *http.Client
	userAgent string
	hosts     map[string]hostEntry
	preflight httpkit.PreflightCache
}

// Statically assert Tracker satisfies the port. If this stops compiling, the
// port shape changed and the adapter needs to follow.
var _ ports.Tracker = (*Tracker)(nil)

// New returns a Tracker. It fails fast with ErrNoToken when no credential can
// be obtained, without making a network call.
func New(opts Options) (*Tracker, error) {
	src := opts.Credential
	if src == nil {
		return nil, ErrNoToken
	}
	if _, err := src.Credential(context.Background()); err != nil {
		return nil, err
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}

	hosts := make(map[string]hostEntry, len(opts.AllowedHosts))
	for _, raw := range opts.AllowedHosts {
		h := normalizeHost(raw)
		if h == "" {
			continue
		}
		he := hostEntry{baseURL: "https://" + h, creds: src}
		if override, ok := opts.HostBaseURLs[h]; ok && override != "" {
			he.baseURL = strings.TrimSuffix(override, "/")
		}
		if cs, ok := opts.HostCredentials[h]; ok && cs != nil {
			he.creds = cs
		}
		hosts[h] = he
	}

	t := &Tracker{http: opts.HTTPClient, userAgent: ua, hosts: hosts}
	if t.http == nil {
		t.http = &http.Client{Timeout: 30 * time.Second}
	}
	return t, nil
}

func normalizeHost(raw string) string {
	h := strings.ToLower(strings.TrimSpace(raw))
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	return strings.TrimSuffix(h, "/")
}

// configForHost resolves the per-site config. Jira has no default site, so an
// empty or unlisted host fails closed before any credential is attached.
func (t *Tracker) configForHost(host string) (hostEntry, error) {
	h := normalizeHost(host)
	if h == "" {
		return hostEntry{}, fmt.Errorf("jira tracker: no site configured: %w", ErrHostNotAllowed)
	}
	if he, ok := t.hosts[h]; ok {
		return he, nil
	}
	return hostEntry{}, fmt.Errorf("jira tracker: host %q not in allowlist: %w", h, ErrHostNotAllowed)
}

// ConfigForHost reports whether a site is allowed, without any network call, so
// wiring tests can assert host routing without DNS resolution.
func (t *Tracker) ConfigForHost(host string) error {
	_, err := t.configForHost(host)
	return err
}

// ---------------------------------------------------------------------------
// Payloads
// ---------------------------------------------------------------------------

// jiraIssue is the subset of the Jira issue payload the adapter reads.
//
// Description is deliberately absent: Jira Cloud returns it as Atlassian
// Document Format (nested JSON, not text) and domain.Issue.Body is descoped by
// decision. See the package doc comment.
type jiraIssue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary string   `json:"summary"`
		Labels  []string `json:"labels"`
		Status  struct {
			Name           string `json:"name"`
			StatusCategory struct {
				Key string `json:"key"`
			} `json:"statusCategory"`
		} `json:"status"`
		Assignee *struct {
			DisplayName  string `json:"displayName"`
			EmailAddress string `json:"emailAddress"`
			AccountID    string `json:"accountId"`
		} `json:"assignee"`
	} `json:"fields"`
}

type jiraSearchResponse struct {
	Issues        []jiraIssue `json:"issues"`
	NextPageToken string      `json:"nextPageToken"`
}

func issueFromJira(host string, raw jiraIssue) domain.Issue {
	out := domain.Issue{
		ID: domain.TrackerID{
			Provider: domain.TrackerProviderJira,
			Native:   raw.Key,
			Host:     host,
		},
		Title:  raw.Fields.Summary,
		State:  mapStatusCategory(raw.Fields.Status.StatusCategory.Key),
		URL:    "https://" + host + "/browse/" + raw.Key,
		Labels: raw.Fields.Labels,
	}
	if a := raw.Fields.Assignee; a != nil {
		if name := firstNonEmpty(a.DisplayName, a.EmailAddress, a.AccountID); name != "" {
			out.Assignees = []string{name}
		}
	}
	// Body stays empty: see jiraIssue and the package doc comment.
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// mapStatusCategory maps Jira's statusCategory onto the normalized vocabulary.
//
// IssueInReview and IssueCancelled are never produced: Jira's category
// vocabulary has exactly three values and offers no instance-independent signal
// for either one. An unknown or absent category maps to IssueOpen rather than
// erroring, because a display-only lane must not fail on an unrecognized
// workflow.
func mapStatusCategory(category string) domain.NormalizedIssueState {
	switch strings.ToLower(strings.TrimSpace(category)) {
	case "indeterminate":
		return domain.IssueInProgress
	case "done":
		return domain.IssueDone
	default:
		// "new", anything unrecognized, and absent all mean open.
		return domain.IssueOpen
	}
}

// ---------------------------------------------------------------------------
// JQL
// ---------------------------------------------------------------------------

// quoteJQL renders a JQL string operand, escaping backslashes and double
// quotes. Jira status names routinely contain spaces, so every operand is
// quoted rather than concatenated raw.
func quoteJQL(v string) string {
	var b strings.Builder
	b.Grow(len(v) + 2)
	b.WriteByte('"')
	for _, r := range v {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func quoteJQLList(vals []string) string {
	parts := make([]string, 0, len(vals))
	for _, v := range vals {
		parts = append(parts, quoteJQL(v))
	}
	return strings.Join(parts, ", ")
}

// buildJQL renders the query for one project key and filter.
//
// Every narrowing dimension of the filter is translated. A filter that narrows
// nothing is refused with ErrUnscopedQuery rather than silently returning every
// ticket in the project — see the package doc comment for why that is
// unconditional rather than left to callers.
func buildJQL(projectKey string, f domain.ListFilter) (string, error) {
	conjuncts := []string{"project = " + quoteJQL(projectKey)}
	narrowed := false

	if len(f.Statuses) > 0 {
		conjuncts = append(conjuncts, "status IN ("+quoteJQLList(f.Statuses)+")")
		narrowed = true
	}
	switch f.State {
	case domain.ListOpen:
		// statusCategory, not a status-name list: category is the only
		// instance-independent open/closed signal, and "Done" is the only
		// category name that is not localizable.
		conjuncts = append(conjuncts, "statusCategory != Done")
		narrowed = true
	case domain.ListClosed:
		conjuncts = append(conjuncts, "statusCategory = Done")
		narrowed = true
	case domain.ListAll:
		// Zero value: "no filter on this dimension".
	}
	if len(f.Labels) > 0 {
		conjuncts = append(conjuncts, "labels IN ("+quoteJQLList(f.Labels)+")")
		narrowed = true
	}
	if assignee := strings.TrimSpace(f.Assignee); assignee != "" {
		// "*" is the provider-agnostic "assigned to anyone" that
		// TrackerIntakeConfig.Assignee documents as passed through unchanged.
		// Jira has no "*"; its equivalent is IS NOT EMPTY.
		if assignee == "*" {
			conjuncts = append(conjuncts, "assignee IS NOT EMPTY")
		} else {
			conjuncts = append(conjuncts, "assignee = "+quoteJQL(assignee))
		}
		narrowed = true
	}

	if !narrowed {
		// Limit is not a narrowing dimension: it caps results, it does not
		// scope the query.
		return "", fmt.Errorf(
			"jira tracker: project %q with no status, label, assignee, or state filter would return the whole project: %w",
			projectKey, ErrUnscopedQuery)
	}
	return strings.Join(conjuncts, " AND "), nil
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

// List returns the issues in one Jira project matching the filter.
func (t *Tracker) List(ctx context.Context, repo domain.TrackerRepo, filter domain.ListFilter) ([]domain.Issue, error) {
	if repo.Provider != domain.TrackerProviderJira {
		return nil, fmt.Errorf("%w: provider=%q", ErrWrongProvider, repo.Provider)
	}
	projectKey, err := parseProjectKey(repo.Native)
	if err != nil {
		return nil, err
	}
	he, err := t.configForHost(repo.Host)
	if err != nil {
		return nil, err
	}
	jql, err := buildJQL(projectKey, filter)
	if err != nil {
		return nil, err
	}

	out := make([]domain.Issue, 0)
	if filter.Limit > 0 {
		out = make([]domain.Issue, 0, filter.Limit)
	}
	host := normalizeHost(repo.Host)
	pageToken := ""
	for page := 0; ; page++ {
		if page >= maxListPages {
			return nil, fmt.Errorf("jira tracker: list pagination exceeded %d pages", maxListPages)
		}
		q := url.Values{}
		q.Set("jql", jql)
		q.Set("maxResults", strconv.Itoa(listPageSize))
		q.Set("fields", requestFields)
		if pageToken != "" {
			q.Set("nextPageToken", pageToken)
		}
		body, err := t.do(ctx, he, http.MethodGet, searchPath+"?"+q.Encode())
		if err != nil {
			return nil, err
		}
		var resp jiraSearchResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("jira tracker: decode list: %w", err)
		}
		pageIssues := make([]domain.Issue, 0, len(resp.Issues))
		for _, raw := range resp.Issues {
			pageIssues = append(pageIssues, issueFromJira(host, raw))
		}
		var done bool
		out, done = httpkit.AppendIssuesWithLimit(out, pageIssues, filter.Limit)
		if done || resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Get
// ---------------------------------------------------------------------------

// Get fetches one issue by key. The backlog lane does not use it; it exists
// because ports.Tracker requires it.
func (t *Tracker) Get(ctx context.Context, id domain.TrackerID) (domain.Issue, error) {
	if id.Provider != domain.TrackerProviderJira {
		return domain.Issue{}, fmt.Errorf("%w: provider=%q", ErrWrongProvider, id.Provider)
	}
	key, err := parseIssueKey(id.Native)
	if err != nil {
		return domain.Issue{}, err
	}
	he, err := t.configForHost(id.Host)
	if err != nil {
		return domain.Issue{}, err
	}

	q := url.Values{}
	q.Set("fields", requestFields)
	body, err := t.do(ctx, he, http.MethodGet, issuePath+url.PathEscape(key)+"?"+q.Encode())
	if err != nil {
		return domain.Issue{}, err
	}
	var raw jiraIssue
	if err := json.Unmarshal(body, &raw); err != nil {
		return domain.Issue{}, fmt.Errorf("jira tracker: decode issue: %w", err)
	}
	return issueFromJira(normalizeHost(id.Host), raw), nil
}

func parseProjectKey(native string) (string, error) {
	key := strings.TrimSpace(native)
	if !projectKeyRe.MatchString(key) {
		return "", fmt.Errorf("%w: %q is not a Jira project key", ErrBadKey, native)
	}
	return key, nil
}

func parseIssueKey(native string) (string, error) {
	key := strings.TrimSpace(native)
	if !issueKeyRe.MatchString(key) {
		return "", fmt.Errorf("%w: %q is not a Jira issue key", ErrBadID, native)
	}
	return key, nil
}

// ---------------------------------------------------------------------------
// Preflight
// ---------------------------------------------------------------------------

// Preflight verifies the credential is currently accepted by Jira (one GET
// /rest/api/3/myself). It does NOT prove the credential can browse any
// specific project — those calls may still fail with ErrAuthFailed.
//
// Successful checks are cached; failures are intentionally not, so a transient
// startup glitch is recoverable on a later call. There is no production caller:
// nothing at daemon boot may call this, because multi.Tracker fans Preflight
// out to every sub-tracker and returns the first error, which would make an
// unconfigured Jira report a boot failure that does not exist today.
func (t *Tracker) Preflight(ctx context.Context) error {
	return t.preflight.Run(ctx, func(ctx context.Context) error {
		for _, he := range t.hosts {
			if _, err := t.do(ctx, he, http.MethodGet, myselfPath); err != nil {
				return err
			}
			return nil
		}
		return fmt.Errorf("jira tracker: no site configured: %w", ErrHostNotAllowed)
	})
}

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

func (t *Tracker) do(ctx context.Context, he hostEntry, method, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, he.baseURL+path, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("jira tracker: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", t.userAgent)

	// Re-read the credential per request so a rotated token is picked up
	// without a daemon restart.
	cred, err := he.creds.Credential(ctx)
	if err != nil {
		return nil, err
	}
	resolved, err := cred.resolve()
	if err != nil {
		return nil, err
	}
	switch resolved.Scheme {
	case SchemeBasic:
		raw := resolved.Email + ":" + resolved.Token
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(raw)))
	case SchemeBearer:
		req.Header.Set("Authorization", "Bearer "+resolved.Token)
	}

	resp, err := t.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jira tracker: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("jira tracker: read response body: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, nil
	}
	return nil, classifyError(resp, body)
}

func classifyError(resp *http.Response, body []byte) error {
	msg := jiraMessage(body)
	switch resp.StatusCode {
	case http.StatusBadRequest:
		// Jira answers 400 for an unknown project key. This must surface as a
		// visible config error, never as an empty list.
		return fmt.Errorf("%w: %s", ErrBadQuery, msg)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrAuthFailed, msg)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, msg)
	case http.StatusTooManyRequests:
		return httpkit.BuildRateLimitError(resp, msg, ErrRateLimited)
	}
	return fmt.Errorf("jira tracker: %d %s", resp.StatusCode, msg)
}

// jiraMessage extracts a human-readable message from a Jira error body. Jira
// uses errorMessages/errors rather than the message field httpkit.Message
// reads, so it falls back to httpkit for anything unrecognized.
func jiraMessage(body []byte) string {
	var payload struct {
		ErrorMessages []string          `json:"errorMessages"`
		Errors        map[string]string `json:"errors"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		if len(payload.ErrorMessages) > 0 {
			return strings.Join(payload.ErrorMessages, "; ")
		}
		if len(payload.Errors) > 0 {
			parts := make([]string, 0, len(payload.Errors))
			for field, msg := range payload.Errors {
				parts = append(parts, field+": "+msg)
			}
			return strings.Join(parts, "; ")
		}
	}
	return httpkit.Message(body)
}
