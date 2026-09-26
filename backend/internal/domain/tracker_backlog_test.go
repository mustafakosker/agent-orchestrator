package domain

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestTrackerProviderJira(t *testing.T) {
	if TrackerProviderJira != "jira" {
		t.Fatalf("TrackerProviderJira = %q, want %q", TrackerProviderJira, "jira")
	}
}

// InferTrackerProvider can never return jira: a Jira site is not a git origin
// and appears nowhere in one. This pins the reason TrackerBacklogConfig
// requires Site and ProjectKey explicitly.
func TestInferTrackerProviderNeverInfersJira(t *testing.T) {
	for _, origin := range []string{
		"",
		"https://github.com/acme/demo.git",
		"git@github.com:acme/demo.git",
		"https://acme.atlassian.net/acme/demo.git",
		"https://gitlab.example.com/group/demo.git",
		"git@jira.internal:acme/demo.git",
	} {
		if got := InferTrackerProvider(origin); got == TrackerProviderJira {
			t.Fatalf("InferTrackerProvider(%q) = %q, want never jira", origin, got)
		}
	}
}

func TestListFilterStatusesPassThrough(t *testing.T) {
	f := ListFilter{Statuses: []string{"Ready", "In Review"}}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"statuses":["Ready","In Review"]`) {
		t.Fatalf("marshal = %s, want statuses passed through verbatim", raw)
	}

	// Empty means "no filter on this dimension" and must be omitted.
	raw, err = json.Marshal(ListFilter{})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if strings.Contains(string(raw), "statuses") {
		t.Fatalf("marshal empty = %s, want statuses omitted", raw)
	}

	var back ListFilter
	if err := json.Unmarshal([]byte(`{"statuses":["Selected for Development"]}`), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.Statuses) != 1 || back.Statuses[0] != "Selected for Development" {
		t.Fatalf("round-trip Statuses = %#v", back.Statuses)
	}
}

func TestTrackerBacklogConfigValidate(t *testing.T) {
	valid := TrackerBacklogConfig{
		Enabled:    true,
		Provider:   TrackerProviderJira,
		Site:       "acme.atlassian.net",
		ProjectKey: "ACME",
		Statuses:   []string{"Ready"},
	}
	withSite := func(mutate func(*TrackerBacklogConfig)) TrackerBacklogConfig {
		c := valid
		c.Statuses = append([]string(nil), valid.Statuses...)
		mutate(&c)
		return c
	}

	tests := []struct {
		name    string
		cfg     TrackerBacklogConfig
		wantErr bool
	}{
		{"zero value", TrackerBacklogConfig{}, false},
		{"disabled ignores everything else", TrackerBacklogConfig{Provider: "linear", Site: " x "}, false},
		{"fully specified", valid, false},
		{"multiple statuses", withSite(func(c *TrackerBacklogConfig) {
			c.Statuses = []string{"Ready", "In Review"}
		}), false},

		{"enabled without provider", withSite(func(c *TrackerBacklogConfig) { c.Provider = "" }), true},
		{"enabled with github provider", withSite(func(c *TrackerBacklogConfig) {
			c.Provider = TrackerProviderGitHub
		}), true},
		{"enabled with unknown provider", withSite(func(c *TrackerBacklogConfig) { c.Provider = "linear" }), true},

		{"missing site", withSite(func(c *TrackerBacklogConfig) { c.Site = "" }), true},
		{"blank site", withSite(func(c *TrackerBacklogConfig) { c.Site = "   " }), true},
		{"site with leading whitespace", withSite(func(c *TrackerBacklogConfig) {
			c.Site = " acme.atlassian.net"
		}), true},
		{"site with trailing whitespace", withSite(func(c *TrackerBacklogConfig) {
			c.Site = "acme.atlassian.net "
		}), true},

		{"missing project key", withSite(func(c *TrackerBacklogConfig) { c.ProjectKey = "" }), true},
		{"blank project key", withSite(func(c *TrackerBacklogConfig) { c.ProjectKey = "  " }), true},
		{"project key with whitespace", withSite(func(c *TrackerBacklogConfig) { c.ProjectKey = " ACME" }), true},

		{"nil statuses", withSite(func(c *TrackerBacklogConfig) { c.Statuses = nil }), true},
		{"empty statuses", withSite(func(c *TrackerBacklogConfig) { c.Statuses = []string{} }), true},
		{"blank status entry", withSite(func(c *TrackerBacklogConfig) {
			c.Statuses = []string{"Ready", "  "}
		}), true},
		{"status entry with surrounding whitespace", withSite(func(c *TrackerBacklogConfig) {
			c.Statuses = []string{" Ready"}
		}), true},
		{"status name with an inner space is fine", withSite(func(c *TrackerBacklogConfig) {
			c.Statuses = []string{"Selected for Development"}
		}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

// The error messages must name the field a person has to fix.
func TestTrackerBacklogConfigValidateMessages(t *testing.T) {
	tests := []struct {
		name string
		cfg  TrackerBacklogConfig
		want string
	}{
		{"provider", TrackerBacklogConfig{Enabled: true}, "trackerBacklog.provider"},
		{"site", TrackerBacklogConfig{
			Enabled: true, Provider: TrackerProviderJira,
		}, "trackerBacklog.site"},
		{"project key", TrackerBacklogConfig{
			Enabled: true, Provider: TrackerProviderJira, Site: "acme.atlassian.net",
		}, "trackerBacklog.projectKey"},
		{"statuses", TrackerBacklogConfig{
			Enabled: true, Provider: TrackerProviderJira, Site: "acme.atlassian.net", ProjectKey: "ACME",
		}, "trackerBacklog.statuses"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error naming %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() = %q, want it to name %q", err, tt.want)
			}
		})
	}
}

// WithDefaults must not invent a provider. An absent provider stays absent so a
// disabled/empty config still stores as NULL, matching TrackerIntakeConfig.
func TestTrackerBacklogConfigWithDefaults(t *testing.T) {
	if got := (TrackerBacklogConfig{}).WithDefaults(); !reflect.DeepEqual(got, TrackerBacklogConfig{}) {
		t.Fatalf("zero WithDefaults() = %#v, want zero value", got)
	}
	got := (TrackerBacklogConfig{Enabled: true}).WithDefaults()
	if got.Provider != "" {
		t.Fatalf("WithDefaults().Provider = %q, want empty (no inference is possible)", got.Provider)
	}
}

// Backlog display and issue intake are independent structs. Enabling display
// must never touch intake, and vice versa: there is no shared flag.
func TestTrackerBacklogAndIntakeAreIndependent(t *testing.T) {
	cfg := ProjectConfig{
		TrackerBacklog: TrackerBacklogConfig{
			Enabled:    true,
			Provider:   TrackerProviderJira,
			Site:       "acme.atlassian.net",
			ProjectKey: "ACME",
			Statuses:   []string{"Ready"},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("backlog-only config Validate() = %v, want nil", err)
	}
	if cfg.TrackerIntake.Enabled {
		t.Fatal("enabling backlog display armed tracker intake")
	}
	out := cfg.WithDefaults()
	if out.TrackerIntake.Enabled {
		t.Fatal("WithDefaults armed tracker intake from a backlog-only config")
	}

	// And the reverse: intake alone leaves the backlog lane off.
	intakeOnly := ProjectConfig{TrackerIntake: TrackerIntakeConfig{Enabled: true, Assignee: "alice"}}
	if err := intakeOnly.Validate(); err != nil {
		t.Fatalf("intake-only config Validate() = %v, want nil", err)
	}
	if intakeOnly.TrackerBacklog.Enabled {
		t.Fatal("enabling tracker intake turned on the backlog lane")
	}
}

func TestProjectConfigTrackerBacklogJSONKey(t *testing.T) {
	raw, err := json.Marshal(ProjectConfig{TrackerBacklog: TrackerBacklogConfig{Enabled: true}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"trackerBacklog":{"enabled":true}`) {
		t.Fatalf("marshal = %s, want trackerBacklog under its own key", raw)
	}
	// The two configs are independent keys. An enabled backlog must never
	// serialize anything into trackerIntake — `omitempty` has no effect on a
	// struct field, so intake is present but must stay empty.
	if !strings.Contains(string(raw), `"trackerIntake":{}`) {
		t.Fatalf("marshal = %s, want trackerIntake left empty by a backlog-only config", raw)
	}
}

// ProviderState carries the provider's own status string for display. The
// normalized State cannot serve that purpose: Jira's three statusCategory
// values collapse "Ready", "Blocked", and "Selected for Development" all onto
// IssueOpen, so a lane configured by workflow status has nothing to render.
func TestIssueProviderStateIsSeparateFromNormalizedState(t *testing.T) {
	iss := Issue{State: IssueOpen, ProviderState: "Selected for Development"}
	if iss.State != IssueOpen {
		t.Fatalf("State = %q, want the normalized value", iss.State)
	}
	if iss.ProviderState != "Selected for Development" {
		t.Fatalf("ProviderState = %q, want the provider's own status string", iss.ProviderState)
	}

	raw, err := json.Marshal(iss)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"providerState":"Selected for Development"`) {
		t.Fatalf("marshal = %s, want providerState carried", raw)
	}
	// Absent for providers that do not supply one.
	raw, err = json.Marshal(Issue{State: IssueOpen})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "providerState") {
		t.Fatalf("marshal = %s, want providerState omitted when unset", raw)
	}
}
