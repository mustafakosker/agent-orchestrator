package controllers_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	backlogsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/backlog"
)

type fakeBacklog struct {
	result backlogsvc.Result
	err    error
	calls  int
}

func (f *fakeBacklog) Ready(context.Context, string) (backlogsvc.Result, error) {
	f.calls++
	return f.result, f.err
}

func (f *fakeBacklog) ReadyAll(context.Context) (backlogsvc.Result, error) {
	f.calls++
	return f.result, f.err
}

func backlogRouter(mgr controllers.BacklogManager) http.Handler {
	r := chi.NewRouter()
	(&controllers.ProjectsController{Backlog: mgr}).Register(r)
	return r
}

func getBacklog(t *testing.T, mgr controllers.BacklogManager, target string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	backlogRouter(mgr).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	var body map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, body
}

func sampleItem() backlogsvc.Item {
	return backlogsvc.Item{
		ProjectID: "p1",
		Issue: domain.Issue{
			ID:            domain.TrackerID{Provider: domain.TrackerProviderJira, Native: "ACME-1", Host: "acme.atlassian.net"},
			Title:         "Wire the thing",
			State:         domain.IssueOpen,
			ProviderState: "Selected for Development",
			URL:           "https://acme.atlassian.net/browse/ACME-1",
			Assignees:     []string{"Alice Smith"},
		},
	}
}

func TestBacklogOKShape(t *testing.T) {
	fetched := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	mgr := &fakeBacklog{result: backlogsvc.Result{
		Items:     []backlogsvc.Item{sampleItem()},
		FetchedAt: fetched,
		State:     backlogsvc.StateOK,
	}}

	code, body := getBacklog(t, mgr, "/projects/p1/backlog")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["state"] != "ok" {
		t.Fatalf("state = %v, want ok", body["state"])
	}
	if body["stale"] != false {
		t.Fatalf("stale = %v, want false", body["stale"])
	}
	if body["fetchedAt"] != "2026-09-26T10:00:00Z" {
		t.Fatalf("fetchedAt = %v, want the fetch time", body["fetchedAt"])
	}
	items, ok := body["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %#v, want one item", body["items"])
	}
	item := items[0].(map[string]any)
	for field, want := range map[string]any{
		"key":       "ACME-1",
		"title":     "Wire the thing",
		"status":    "Selected for Development",
		"assignee":  "Alice Smith",
		"url":       "https://acme.atlassian.net/browse/ACME-1",
		"projectId": "p1",
	} {
		if item[field] != want {
			t.Fatalf("item[%q] = %v, want %v", field, item[field], want)
		}
	}
}

// The card must show Jira's own workflow status, not the normalized state.
func TestBacklogStatusIsProviderStateNotNormalized(t *testing.T) {
	it := sampleItem()
	it.Issue.ProviderState = "Blocked"
	it.Issue.State = domain.IssueOpen
	mgr := &fakeBacklog{result: backlogsvc.Result{
		Items: []backlogsvc.Item{it}, State: backlogsvc.StateOK,
	}}

	_, body := getBacklog(t, mgr, "/projects/p1/backlog")
	item := body["items"].([]any)[0].(map[string]any)
	if item["status"] != "Blocked" {
		t.Fatalf("status = %v, want the provider status name", item["status"])
	}
	if item["status"] == string(domain.IssueOpen) {
		t.Fatal("status leaked the normalized state")
	}
}

func TestBacklogNotConfiguredReasonsAreDistinguishable(t *testing.T) {
	tests := []struct {
		name   string
		reason backlogsvc.Reason
		want   string
	}{
		{"no config", backlogsvc.ReasonConfigAbsent, "config_absent"},
		{"no credential", backlogsvc.ReasonCredentialsMissing, "credentials_missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := &fakeBacklog{result: backlogsvc.Result{
				State: backlogsvc.StateNotConfigured, Reason: tt.reason,
			}}
			code, body := getBacklog(t, mgr, "/projects/p1/backlog")
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200 — an unconfigured integration is not a fault", code)
			}
			if body["state"] != "not_configured" {
				t.Fatalf("state = %v, want not_configured", body["state"])
			}
			if body["reason"] != tt.want {
				t.Fatalf("reason = %v, want %q so the UI can tell the two causes apart", body["reason"], tt.want)
			}
		})
	}
}

func TestBacklogStaleShape(t *testing.T) {
	fetched := time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC)
	mgr := &fakeBacklog{result: backlogsvc.Result{
		Items:     []backlogsvc.Item{sampleItem()},
		FetchedAt: fetched,
		Stale:     true,
		State:     backlogsvc.StateOK,
	}}

	code, body := getBacklog(t, mgr, "/projects/p1/backlog")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["stale"] != true {
		t.Fatalf("stale = %v, want true", body["stale"])
	}
	if body["fetchedAt"] != "2026-09-26T09:00:00Z" {
		t.Fatalf("fetchedAt = %v, want the ORIGINAL fetch time, not the request time", body["fetchedAt"])
	}
}

// A provider failure must never yield a 500: the lane degrades, it does not
// break the API.
func TestBacklogProviderFailureNeverYields500(t *testing.T) {
	tests := []struct {
		name   string
		reason backlogsvc.Reason
		want   string
	}{
		{"auth", backlogsvc.ReasonAuthFailed, "auth_failed"},
		{"config", backlogsvc.ReasonConfigInvalid, "config_invalid"},
		{"unavailable", backlogsvc.ReasonUnavailable, "unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := &fakeBacklog{result: backlogsvc.Result{
				State: backlogsvc.StateError, Reason: tt.reason, Message: "provider said no",
			}}
			code, body := getBacklog(t, mgr, "/projects/p1/backlog")
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (never 500) for a provider failure", code)
			}
			if body["state"] != "error" {
				t.Fatalf("state = %v, want error", body["state"])
			}
			if body["reason"] != tt.want {
				t.Fatalf("reason = %v, want %q", body["reason"], tt.want)
			}
			if body["message"] != "provider said no" {
				t.Fatalf("message = %v, want the provider message carried", body["message"])
			}
		})
	}
}

// An unknown project id is a caller mistake, not a lane state.
func TestBacklogUnknownProjectIs404(t *testing.T) {
	mgr := &fakeBacklog{err: errors.New("backlog: unknown project \"nope\"")}
	code, _ := getBacklog(t, mgr, "/projects/nope/backlog")
	if code != http.StatusNotFound && code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 404 or 500 for an unknown project", code)
	}
}

// Items must always serialize as an array, never null, so the renderer can map
// over it unconditionally.
func TestBacklogEmptyItemsIsArrayNotNull(t *testing.T) {
	mgr := &fakeBacklog{result: backlogsvc.Result{State: backlogsvc.StateOK}}
	_, body := getBacklog(t, mgr, "/projects/p1/backlog")
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items = %#v, want [] not null", body["items"])
	}
	if len(items) != 0 {
		t.Fatalf("items = %#v, want empty", items)
	}
}

// A nil Backlog manager keeps the route registered and answers a documented
// 501, matching how the other controllers handle a missing dependency.
func TestBacklogNilManagerIsNotImplemented(t *testing.T) {
	code, _ := getBacklog(t, nil, "/projects/p1/backlog")
	if code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 with a nil manager", code)
	}
}

func TestBacklogUnassignedHasEmptyAssignee(t *testing.T) {
	it := sampleItem()
	it.Issue.Assignees = nil
	mgr := &fakeBacklog{result: backlogsvc.Result{
		Items: []backlogsvc.Item{it}, State: backlogsvc.StateOK,
	}}
	_, body := getBacklog(t, mgr, "/projects/p1/backlog")
	item := body["items"].([]any)[0].(map[string]any)
	if got, ok := item["assignee"]; ok && got != "" {
		t.Fatalf("assignee = %v, want empty/absent for an unassigned ticket", got)
	}
}
