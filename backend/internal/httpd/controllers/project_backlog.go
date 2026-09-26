package controllers

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	backlogsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/backlog"
)

// BacklogManager is the read surface for the display-only tracker backlog lane.
type BacklogManager interface {
	Ready(ctx context.Context, projectID string) (backlogsvc.Result, error)
	ReadyAll(ctx context.Context) (backlogsvc.Result, error)
}

// BacklogItem is one ticket on the backlog lane.
//
// Status is the provider's OWN workflow status name (domain.Issue.ProviderState),
// not the normalized state: the lane is configured by workflow status, and the
// normalized vocabulary collapses every pre-work status onto "open".
//
// ProjectID badges the card on the all-projects board, where several projects'
// tickets share one lane.
type BacklogItem struct {
	Key       string `json:"key" description:"Provider-native issue key, e.g. ACME-123."`
	Title     string `json:"title" description:"Issue summary."`
	Status    string `json:"status" description:"The provider's own workflow status name."`
	Assignee  string `json:"assignee,omitempty" description:"Assignee display name; empty when unassigned."`
	URL       string `json:"url" description:"Link to the issue in the provider."`
	ProjectID string `json:"projectId" description:"AO project the ticket belongs to."`
}

// BacklogResponse is the backlog lane read model.
//
// A provider failure never produces a non-2xx: the lane is a view, so failures
// are reported in State/Reason and degrade to an empty or stale lane. An
// unconfigured integration is likewise a normal state, not a fault.
//
// FetchedAt always describes when Items was fetched from the provider, never
// when the request was served, so a stale lane cannot claim freshness.
type BacklogResponse struct {
	Items     []BacklogItem `json:"items" description:"Eligible tickets; always an array, never null."`
	FetchedAt *time.Time    `json:"fetchedAt" description:"When Items was fetched from the provider; null when never fetched."`
	Stale     bool          `json:"stale" description:"True when serving a retained entry after a failed refresh."`
	State     string        `json:"state" enum:"ok,not_configured,error" description:"Lane outcome."`
	Reason    string        `json:"reason,omitempty" enum:"config_absent,credentials_missing,auth_failed,config_invalid,unavailable" description:"Cause discriminator within a non-ok state."`
	Message   string        `json:"message,omitempty" description:"Provider message for an error state."`
}

func (c *ProjectsController) backlog(w http.ResponseWriter, r *http.Request) {
	if c.Backlog == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/backlog")
		return
	}
	projectID := strings.TrimSpace(chi.URLParam(r, "id"))
	result, err := c.Backlog.Ready(r.Context(), projectID)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, backlogResponse(result))
}

func backlogResponse(result backlogsvc.Result) BacklogResponse {
	items := make([]BacklogItem, 0, len(result.Items))
	for _, it := range result.Items {
		var assignee string
		if len(it.Issue.Assignees) > 0 {
			assignee = it.Issue.Assignees[0]
		}
		items = append(items, BacklogItem{
			Key:       it.Issue.ID.Native,
			Title:     it.Issue.Title,
			Status:    it.Issue.ProviderState,
			Assignee:  assignee,
			URL:       it.Issue.URL,
			ProjectID: it.ProjectID,
		})
	}
	out := BacklogResponse{
		Items:   items,
		Stale:   result.Stale,
		State:   string(result.State),
		Reason:  string(result.Reason),
		Message: result.Message,
	}
	if !result.FetchedAt.IsZero() {
		fetched := result.FetchedAt.UTC()
		out.FetchedAt = &fetched
	}
	return out
}
