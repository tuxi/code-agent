// Package credential provides abstractions for obtaining service credentials.
// The Runtime uses these to authenticate to model providers, gateways, and MCP
// servers without knowing where the credentials come from.
//
// This package has zero external dependencies beyond the Go standard library.
package credential

import (
	"net/url"
	"time"
)

// Target uniquely identifies a service that requires a credential.
//
// Namespace is the service category ("gateway", "llm", "mcp").
// Name is the specific instance ("default", "deepseek", "github").
//
// Why Namespace instead of Type: Credential itself also has a Type field
// ("bearer", "secret", "none"). Using different names avoids ambiguity
// when both appear in the same scope.
type Target struct {
	Namespace string
	Name      string
}

// String returns a stable string encoding of the target suitable for use as a
// map key or wire format identifier. It uses url.PathEscape on each component
// so that names containing slashes or other special characters are safely
// encoded. The format is "{namespace}/{name}".
//
// This encoding MUST match the Host-side encoding (Swift's
// addingPercentEncoding(withAllowedCharacters: .urlPathAllowed)).
func (t Target) String() string {
	return url.PathEscape(t.Namespace) + "/" + url.PathEscape(t.Name)
}

// CredentialType describes how a credential is transmitted at the HTTP layer.
// It answers "how do I put this into an HTTP request?", not "where did this
// credential come from?".
type CredentialType string

const (
	// Bearer credentials are sent as "Authorization: Bearer <Secret>".
	// This covers JWT, OAuth2 access tokens, and API keys.
	Bearer CredentialType = "bearer"

	// Secret credentials use a non-Bearer mechanism (future: AWS SigV4,
	// mTLS client certificates). The HTTP transport layer handles the
	// details; the provider only needs to know the secret value.
	Secret CredentialType = "secret"

	// None indicates no credential is required (local models, no-auth MCP).
	None CredentialType = "none"
)

// Credential is a value object holding the credentials for a service.
// It does not contain refresh logic — that lives in Resolver implementations
// (specifically CachedResolver).
type Credential struct {
	Type   CredentialType // bearer | secret | none
	Secret string         // the token or key — must not be logged

	// ExpiresAt is an optional expiry time. nil means "never expires"
	// (static API keys). CachedResolver uses this to decide when to refresh.
	ExpiresAt *time.Time

	// Metadata carries extra credential context (refresh_token, scope,
	// protocol, provider). Resolver implementations may store arbitrary
	// key-value pairs here; consumers should only read known keys.
	//
	// Important: Metadata containing refresh_token MUST be stripped before
	// injection into the Runtime via secretsJSON (§6.1 of the injection
	// contract). The Runtime never sees refresh_token values.
	Metadata map[string]string
}

// IsZero reports whether c is an empty credential.
func (c Credential) IsZero() bool {
	return c.Type == "" && c.Secret == ""
}

// IsExpired reports whether the credential has passed its expiry time.
// A nil ExpiresAt means the credential never expires.
func (c Credential) IsExpired() bool {
	if c.ExpiresAt == nil {
		return false
	}
	return time.Now().After(*c.ExpiresAt)
}

// ResolvedCredential wraps a Credential with provenance information.
// Source records which Resolver produced this credential, for debug
// logging and audit trails (e.g. "env:DEEPSEEK_API_KEY",
// "chain[1]=env", "agentkit:gateway").
type ResolvedCredential struct {
	Credential
	Source string
}
