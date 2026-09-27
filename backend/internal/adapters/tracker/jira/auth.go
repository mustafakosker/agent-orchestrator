package jira

import (
	"context"
	"errors"
	"os"
	"strings"
)

// ErrNoToken is returned when no credential source could yield a usable Jira
// credential. It is checked by newMultiTracker's logTrackerDisabled so a
// missing Jira credential is reported as "no usable token" (a normal
// unconfigured state) rather than "setup failed" (a fault).
var ErrNoToken = errors.New("jira tracker: no credential configured")

// SchemeKind is the HTTP authentication scheme a Credential is presented with.
type SchemeKind string

// The two schemes Jira accepts.
const (
	// SchemeBasic is Jira Cloud's email:api_token over HTTP Basic.
	SchemeBasic SchemeKind = "basic"
	// SchemeBearer is Jira Data Center's personal access token.
	SchemeBearer SchemeKind = "bearer"
)

// Credential is one resolved Jira credential and the scheme it is presented
// with.
//
// Scheme is derived from the credential's own shape rather than from a host
// heuristic: a credential carrying an email is Basic (Jira Cloud); a token-only
// credential must represent a Data Center PAT. Sources must reject incomplete
// Cloud email/API-token pairs rather than treating them as PATs. Callers
// construct Credential with Email and Token and let Scheme resolve itself.
type Credential struct {
	Scheme SchemeKind
	Email  string
	Token  string
}

// resolve normalizes a credential and picks its scheme, or reports ErrNoToken
// when the pair cannot form either scheme. An email without a token is a
// configuration error, not a Bearer credential.
func (c Credential) resolve() (Credential, error) {
	email := strings.TrimSpace(c.Email)
	token := strings.TrimSpace(c.Token)
	if token == "" {
		return Credential{}, ErrNoToken
	}
	if email != "" {
		return Credential{Scheme: SchemeBasic, Email: email, Token: token}, nil
	}
	return Credential{Scheme: SchemeBearer, Token: token}, nil
}

// CredentialSource yields a Jira credential on demand.
//
// It mirrors the contract of github.TokenSource and scmgitlab.TokenSource, but
// returns both parts of the credential because Jira's two supported shapes use
// different HTTP schemes and Basic needs an email as well as a token. The
// Tracker calls Credential once at construction (fail-fast, no network call)
// and again per request, so a rotated token is picked up without a daemon
// restart.
type CredentialSource interface {
	Credential(ctx context.Context) (Credential, error)
}

// StaticCredentialSource is a literal credential, typically used in tests and
// for per-host overrides resolved from daemon config.
type StaticCredentialSource struct {
	Email string
	Token string
}

// Credential returns the literal credential, or ErrNoToken when it cannot form
// either scheme.
func (s StaticCredentialSource) Credential(context.Context) (Credential, error) {
	return Credential{Email: s.Email, Token: s.Token}.resolve()
}

// Environment variables the default credential chain reads. These are variable
// NAMES, not secrets; the values they hold are never logged or persisted.
const (
	EnvEmail = "AO_JIRA_EMAIL"
	// #nosec G101 -- an env var name, not a credential.
	EnvAPIToken = "AO_JIRA_API_TOKEN"
	// #nosec G101 -- an env var name, not a credential.
	EnvPAT = "AO_JIRA_PAT"
)

// EnvCredentialSource reads the default credential from the environment:
// AO_JIRA_EMAIL + AO_JIRA_API_TOKEN for Jira Cloud, or AO_JIRA_PAT for Data
// Center. A PAT wins when both are present, because it is unambiguous about
// which scheme was intended.
//
// Credentials are read from the environment rather than from project config:
// project config is a plaintext JSON column echoed back over the loopback API
// on every project read, which is no place for a long-lived credential. This
// matches AO_GITHUB_TOKEN and AO_GITLAB_TOKEN.
type EnvCredentialSource struct{}

// Credential resolves the environment credential, or ErrNoToken when neither
// shape is fully configured.
func (EnvCredentialSource) Credential(context.Context) (Credential, error) {
	if pat := strings.TrimSpace(os.Getenv(EnvPAT)); pat != "" {
		return Credential{Scheme: SchemeBearer, Token: pat}, nil
	}
	if strings.TrimSpace(os.Getenv(EnvEmail)) == "" {
		return Credential{}, ErrNoToken
	}
	return Credential{
		Email: os.Getenv(EnvEmail),
		Token: os.Getenv(EnvAPIToken),
	}.resolve()
}

// FallbackCredentialSource returns the first source that yields a usable
// credential, mirroring gitlab.FallbackTokenSource. A source that reports
// ErrNoToken is skipped; any other error stops the chain, because a broken
// source must not be silently masked by a later one.
type FallbackCredentialSource []CredentialSource

// Credential walks the chain in order.
func (f FallbackCredentialSource) Credential(ctx context.Context) (Credential, error) {
	for _, src := range f {
		if src == nil {
			continue
		}
		cred, err := src.Credential(ctx)
		if err == nil {
			return cred, nil
		}
		if !errors.Is(err, ErrNoToken) {
			return Credential{}, err
		}
	}
	return Credential{}, ErrNoToken
}

// DefaultCredentialSource is the standard chain: the environment only. Unlike
// GitHub's `gh auth token` and GitLab's `glab` fallbacks there is no Jira CLI
// to probe, so the chain has one link today and exists so adding one later
// needs no call-site change.
func DefaultCredentialSource() CredentialSource {
	return FallbackCredentialSource{EnvCredentialSource{}}
}
