package config

import (
	"testing"
)

func clearJiraEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"AO_JIRA_ALLOWED_HOSTS", "AO_JIRA_HOST_CREDENTIALS"} {
		t.Setenv(k, "")
	}
}

func TestLoadJiraDefaults(t *testing.T) {
	clearJiraEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Jira.AllowedHosts != nil {
		t.Errorf("Jira.AllowedHosts = %v, want nil", cfg.Jira.AllowedHosts)
	}
	if cfg.Jira.HostCredentials != nil {
		t.Errorf("Jira.HostCredentials = %v, want nil", cfg.Jira.HostCredentials)
	}
}

func TestLoadJiraAllowedHosts(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want []string
	}{
		{"single site", "acme.atlassian.net", []string{"acme.atlassian.net"}},
		{"comma-separated", "acme.atlassian.net,jira.internal.corp", []string{"acme.atlassian.net", "jira.internal.corp"}},
		{"trimmed whitespace", " acme.atlassian.net , jira.internal.corp ", []string{"acme.atlassian.net", "jira.internal.corp"}},
		{"empty entries skipped", "acme.atlassian.net,,jira.internal.corp,", []string{"acme.atlassian.net", "jira.internal.corp"}},
		{"with port", "jira.internal:8443", []string{"jira.internal:8443"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearJiraEnv(t)
			t.Setenv("AO_JIRA_ALLOWED_HOSTS", tc.env)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(cfg.Jira.AllowedHosts) != len(tc.want) {
				t.Fatalf("AllowedHosts = %v, want %v", cfg.Jira.AllowedHosts, tc.want)
			}
			for i, h := range tc.want {
				if cfg.Jira.AllowedHosts[i] != h {
					t.Errorf("AllowedHosts[%d] = %q, want %q", i, cfg.Jira.AllowedHosts[i], h)
				}
			}
		})
	}
}

// Per-site credentials carry both Jira shapes: "email:token" is Cloud Basic,
// a bare token is a Data Center PAT.
func TestLoadJiraHostCredentials(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want map[string]JiraCredential
	}{
		{
			name: "cloud email and api token",
			env:  "acme.atlassian.net=me@acme.test:api-tok",
			want: map[string]JiraCredential{
				"acme.atlassian.net": {Email: "me@acme.test", Token: "api-tok"},
			},
		},
		{
			name: "data center bare pat",
			env:  "jira.internal.corp=pat-tok",
			want: map[string]JiraCredential{
				"jira.internal.corp": {Token: "pat-tok"},
			},
		},
		{
			name: "both shapes together",
			env:  "acme.atlassian.net=me@acme.test:api-tok,jira.internal.corp=pat-tok",
			want: map[string]JiraCredential{
				"acme.atlassian.net": {Email: "me@acme.test", Token: "api-tok"},
				"jira.internal.corp": {Token: "pat-tok"},
			},
		},
		{
			name: "trimmed whitespace",
			env:  " acme.atlassian.net = me@acme.test:api-tok ",
			want: map[string]JiraCredential{
				"acme.atlassian.net": {Email: "me@acme.test", Token: "api-tok"},
			},
		},
		{
			name: "site with port keeps its port",
			env:  "jira.internal:8443=pat-tok",
			want: map[string]JiraCredential{
				"jira.internal:8443": {Token: "pat-tok"},
			},
		},
		{
			name: "entry without equals skipped",
			env:  "acme.atlassian.net=pat,not-a-pair",
			want: map[string]JiraCredential{"acme.atlassian.net": {Token: "pat"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearJiraEnv(t)
			t.Setenv("AO_JIRA_HOST_CREDENTIALS", tc.env)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(cfg.Jira.HostCredentials) != len(tc.want) {
				t.Fatalf("HostCredentials = %#v, want %#v", cfg.Jira.HostCredentials, tc.want)
			}
			for site, want := range tc.want {
				got, ok := cfg.Jira.HostCredentials[site]
				if !ok {
					t.Errorf("HostCredentials missing site %q", site)
					continue
				}
				if got != want {
					t.Errorf("HostCredentials[%q] = %#v, want %#v", site, got, want)
				}
			}
		})
	}
}

func TestLoadJiraInvalidHostCredentials(t *testing.T) {
	clearJiraEnv(t)
	// parseHostTokenMap rejects a value containing '=' as ambiguous. Jira
	// inherits that rule so a malformed entry fails loud rather than silently
	// producing a wrong credential.
	t.Setenv("AO_JIRA_HOST_CREDENTIALS", "acme.atlassian.net=tok=with=equals")
	if _, err := Load(); err == nil {
		t.Fatal("Load() = nil error, want error for malformed AO_JIRA_HOST_CREDENTIALS")
	}
}
