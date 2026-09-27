package jira_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	trackerjira "github.com/aoagents/agent-orchestrator/backend/internal/adapters/tracker/jira"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// TestLiveList exercises a real Jira site when explicitly configured. It is
// skipped in ordinary and CI test runs: AGENTS.md forbids network calls in
// tests without an established opt-in pattern, and this follows the one set by
// cloud/internal/sandbox/coder/live_test.go.
//
// It is read-only — List and Preflight only — because the adapter has no write
// path at all. Configure with:
//
//	JIRA_LIVE_SITE=acme.atlassian.net
//	JIRA_LIVE_PROJECT_KEY=ACME
//	JIRA_LIVE_STATUSES="Ready,In Review"   # optional, defaults to "Ready"
//	AO_JIRA_EMAIL=... AO_JIRA_API_TOKEN=... # or AO_JIRA_PAT=...
func TestLiveList(t *testing.T) {
	site := os.Getenv("JIRA_LIVE_SITE")
	if site == "" {
		t.Skip("JIRA_LIVE_SITE is not set")
	}
	projectKey := os.Getenv("JIRA_LIVE_PROJECT_KEY")
	if projectKey == "" {
		t.Fatal("JIRA_LIVE_PROJECT_KEY is required alongside JIRA_LIVE_SITE")
	}
	statuses := []string{"Ready"}
	if raw := strings.TrimSpace(os.Getenv("JIRA_LIVE_STATUSES")); raw != "" {
		statuses = nil
		for _, s := range strings.Split(raw, ",") {
			if s = strings.TrimSpace(s); s != "" {
				statuses = append(statuses, s)
			}
		}
	}

	tr, err := trackerjira.New(trackerjira.Options{
		Credential:   trackerjira.DefaultCredentialSource(),
		AllowedHosts: []string{site},
	})
	if err != nil {
		t.Fatalf("new live tracker (set AO_JIRA_EMAIL+AO_JIRA_API_TOKEN or AO_JIRA_PAT): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := tr.Preflight(ctx); err != nil {
		t.Fatalf("live Preflight: %v", err)
	}

	issues, err := tr.List(ctx, domain.TrackerRepo{
		Provider: domain.TrackerProviderJira,
		Native:   projectKey,
		Host:     site,
	}, domain.ListFilter{Statuses: statuses, Limit: 25})
	if err != nil {
		t.Fatalf("live List: %v", err)
	}
	t.Logf("live List returned %d issue(s) for %s statuses=%v", len(issues), projectKey, statuses)

	for _, iss := range issues {
		if iss.ID.Provider != domain.TrackerProviderJira {
			t.Fatalf("issue %q provider = %q, want jira", iss.ID.Native, iss.ID.Provider)
		}
		if iss.ID.Host != site {
			t.Fatalf("issue %q host = %q, want %q", iss.ID.Native, iss.ID.Host, site)
		}
		if !strings.HasPrefix(iss.ID.Native, projectKey+"-") {
			t.Fatalf("issue key %q is not in project %q", iss.ID.Native, projectKey)
		}
		if iss.Title == "" {
			t.Fatalf("issue %q has an empty title", iss.ID.Native)
		}
		if !strings.HasPrefix(iss.URL, "https://"+site+"/browse/") {
			t.Fatalf("issue %q url = %q", iss.ID.Native, iss.URL)
		}
		// Body is descoped by decision; a live site must not start filling it.
		if iss.Body != "" {
			t.Fatalf("issue %q body = %q, want empty (ADF is descoped)", iss.ID.Native, iss.Body)
		}
		// Neither of these can be synthesized from Jira's statusCategory.
		if iss.State == domain.IssueInReview || iss.State == domain.IssueCancelled {
			t.Fatalf("issue %q state = %q, want neither review nor cancelled", iss.ID.Native, iss.State)
		}
	}
}
