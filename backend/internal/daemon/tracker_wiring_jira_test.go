package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	trackergitlab "github.com/aoagents/agent-orchestrator/backend/internal/adapters/tracker/gitlab"
	trackerjira "github.com/aoagents/agent-orchestrator/backend/internal/adapters/tracker/jira"
	trackermulti "github.com/aoagents/agent-orchestrator/backend/internal/adapters/tracker/multi"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

const jiraSite = "acme.atlassian.net"

func setJiraEnv(t *testing.T, email, apiToken, pat string) {
	t.Helper()
	t.Setenv("AO_JIRA_EMAIL", email)
	t.Setenv("AO_JIRA_API_TOKEN", apiToken)
	t.Setenv("AO_JIRA_PAT", pat)
}

// AllowedHosts must flow from JiraConfig into the adapter. ConfigForHost makes
// no network call, so this asserts routing without DNS.
func TestNewJiraTracker_PassesAllowedHosts(t *testing.T) {
	setJiraEnv(t, "", "", "default-pat")

	tracker, err := newJiraTracker(config.JiraConfig{AllowedHosts: []string{jiraSite}})
	if err != nil {
		t.Fatalf("newJiraTracker: %v", err)
	}
	jt, ok := tracker.(*trackerjira.Tracker)
	if !ok {
		t.Fatalf("expected *trackerjira.Tracker, got %T", tracker)
	}
	if err := jt.ConfigForHost(jiraSite); err != nil {
		t.Fatalf("allowlisted site %q was rejected: %v", jiraSite, err)
	}
	if err := jt.ConfigForHost("evil.atlassian.net"); !errors.Is(err, trackerjira.ErrHostNotAllowed) {
		t.Fatalf("unlisted site err = %v, want ErrHostNotAllowed", err)
	}
}

// Jira has no default site, so an empty host must be rejected. This is the
// deliberate difference from the GitLab tracker, where "" means gitlab.com.
func TestNewJiraTracker_NoDefaultSite(t *testing.T) {
	setJiraEnv(t, "", "", "default-pat")

	tracker, err := newJiraTracker(config.JiraConfig{AllowedHosts: []string{jiraSite}})
	if err != nil {
		t.Fatalf("newJiraTracker: %v", err)
	}
	jt := tracker.(*trackerjira.Tracker)
	if err := jt.ConfigForHost(""); !errors.Is(err, trackerjira.ErrHostNotAllowed) {
		t.Fatalf("empty site err = %v, want ErrHostNotAllowed (Jira has no default site)", err)
	}
}

// Per-site credential overrides from config must reach the adapter, and a
// mixed-case config key must still match the adapter's lowercased lookup.
func TestNewJiraTracker_HostCredentialsCaseInsensitive(t *testing.T) {
	setJiraEnv(t, "", "", "default-pat")

	mixed := "ACME.Atlassian.NET"
	tracker, err := newJiraTracker(config.JiraConfig{
		AllowedHosts: []string{mixed},
		HostCredentials: map[string]config.JiraCredential{
			mixed: {Email: "me@acme.test", Token: "api-tok"},
		},
	})
	if err != nil {
		t.Fatalf("newJiraTracker: %v", err)
	}
	jt := tracker.(*trackerjira.Tracker)
	if err := jt.ConfigForHost(jiraSite); err != nil {
		t.Fatalf("mixed-case config site should be accepted after lowercasing: %v", err)
	}
}

// Without any credential the adapter must fail to construct with ErrNoToken, so
// lazyTracker keeps retrying rather than caching a broken tracker.
func TestNewJiraTracker_NoCredentialIsErrNoToken(t *testing.T) {
	setJiraEnv(t, "", "", "")

	_, err := newJiraTracker(config.JiraConfig{AllowedHosts: []string{jiraSite}})
	if !errors.Is(err, trackerjira.ErrNoToken) {
		t.Fatalf("newJiraTracker err = %v, want ErrNoToken", err)
	}
}

// The multi-tracker must route provider "jira" to the Jira sub-tracker. An
// unlisted site returns ErrHostNotAllowed (not ErrUnknownProvider), proving the
// sub-tracker is registered and wired with the allowlist — and it fires before
// any network I/O.
func TestNewMultiTracker_RoutesJira(t *testing.T) {
	t.Setenv("AO_GITHUB_TOKEN", "")
	t.Setenv("AO_GITLAB_TOKEN", "gl-token")
	setJiraEnv(t, "", "", "default-pat")

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tracker := newMultiTracker(config.GitLabConfig{}, config.JiraConfig{
		AllowedHosts: []string{jiraSite},
	}, log)
	if tracker == nil {
		t.Fatal("newMultiTracker = nil, want non-nil")
	}
	if _, ok := tracker.(*trackermulti.Tracker); !ok {
		t.Fatalf("expected *trackermulti.Tracker, got %T", tracker)
	}

	_, err := tracker.List(context.Background(), domain.TrackerRepo{
		Provider: domain.TrackerProviderJira,
		Native:   "ACME",
		Host:     "evil.atlassian.net",
	}, domain.ListFilter{Statuses: []string{"Ready"}})
	if !errors.Is(err, trackerjira.ErrHostNotAllowed) {
		t.Fatalf("multi List to an unlisted Jira site: err = %v, want ErrHostNotAllowed", err)
	}
	if errors.Is(err, trackermulti.ErrUnknownProvider) {
		t.Fatal("jira was not registered with the multi-tracker")
	}
}

// An unconfigured Jira must never block daemon boot: the entry stays lazy, so
// newMultiTracker still returns a usable tracker and GitLab keeps working.
func TestNewMultiTracker_UnconfiguredJiraDoesNotBlockBoot(t *testing.T) {
	t.Setenv("AO_GITHUB_TOKEN", "")
	t.Setenv("AO_GITLAB_TOKEN", "gl-token")
	setJiraEnv(t, "", "", "")

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	tracker := newMultiTracker(config.GitLabConfig{}, config.JiraConfig{}, log)
	if tracker == nil {
		t.Fatal("newMultiTracker = nil, want non-nil with no Jira configured")
	}

	// Nothing may be logged about Jira at construction: the entry is lazy, so
	// no credential is resolved until something actually asks for Jira data.
	if strings.Contains(logs.String(), "jira") {
		t.Fatalf("newMultiTracker logged about jira at boot:\n%s", logs.String())
	}

	// GitLab still resolves.
	_, err := tracker.List(context.Background(), domain.TrackerRepo{
		Provider: domain.TrackerProviderGitLab,
		Native:   "group/project",
		Host:     "gitlab.attacker.example",
	}, domain.ListFilter{})
	if !errors.Is(err, trackergitlab.ErrHostNotAllowed) {
		t.Fatalf("gitlab leg broken by the jira entry: err = %v", err)
	}

	// Asking for Jira now fails with ErrNoToken, having deferred until use.
	_, err = tracker.List(context.Background(), domain.TrackerRepo{
		Provider: domain.TrackerProviderJira, Native: "ACME", Host: jiraSite,
	}, domain.ListFilter{Statuses: []string{"Ready"}})
	if !errors.Is(err, trackerjira.ErrNoToken) {
		t.Fatalf("unconfigured jira List err = %v, want ErrNoToken", err)
	}
}

// lazyTracker must not cache a Jira construction failure: configuring
// AO_JIRA_PAT later is picked up without a daemon restart.
func TestJiraLazyTracker_RetriesAfterCredentialAppears(t *testing.T) {
	t.Setenv("AO_GITHUB_TOKEN", "")
	t.Setenv("AO_GITLAB_TOKEN", "gl-token")
	setJiraEnv(t, "", "", "")

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tracker := newMultiTracker(config.GitLabConfig{}, config.JiraConfig{
		AllowedHosts: []string{jiraSite},
	}, log)

	repo := domain.TrackerRepo{Provider: domain.TrackerProviderJira, Native: "ACME", Host: jiraSite}
	filter := domain.ListFilter{Statuses: []string{"Ready"}}

	if _, err := tracker.List(context.Background(), repo, filter); !errors.Is(err, trackerjira.ErrNoToken) {
		t.Fatalf("first List err = %v, want ErrNoToken", err)
	}

	// The credential appears. The next call must get past construction — it
	// will fail later at the network, which is proof enough that the build was
	// retried rather than a cached failure being replayed.
	setJiraEnv(t, "", "", "now-configured")
	_, err := tracker.List(context.Background(), repo, filter)
	if errors.Is(err, trackerjira.ErrNoToken) {
		t.Fatal("List still returns ErrNoToken after the credential appeared: the failure was cached")
	}
}

// logTrackerDisabled must classify a Jira ErrNoToken as "no usable token" (a
// normal unconfigured state), not "setup failed" (a fault).
func TestLogTrackerDisabled_JiraNoTokenIsNotAFault(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))

	logTrackerDisabled(log, "jira", trackerjira.ErrNoToken)
	if got := logs.String(); !strings.Contains(got, "no usable token") {
		t.Fatalf("log = %q, want it to report a missing Jira credential as 'no usable token'", got)
	}
	if got := logs.String(); strings.Contains(got, "setup failed") {
		t.Fatalf("log = %q, want a missing credential NOT reported as a setup failure", got)
	}

	logs.Reset()
	logTrackerDisabled(log, "jira", errors.New("something actually broke"))
	if got := logs.String(); !strings.Contains(got, "setup failed") {
		t.Fatalf("log = %q, want a real Jira failure reported as 'setup failed'", got)
	}
}
