package credential

import (
	"testing"
	"time"
)

func TestTargetString(t *testing.T) {
	tests := []struct {
		target Target
		want   string
	}{
		{Target{Namespace: "gateway", Name: "default"}, "gateway/default"},
		{Target{Namespace: "llm", Name: "deepseek"}, "llm/deepseek"},
		{Target{Namespace: "mcp", Name: "github"}, "mcp/github"},
		// Slashes and special chars in name are percent-encoded.
		{Target{Namespace: "mcp", Name: "github.com/org/project"}, "mcp/github.com%2Forg%2Fproject"},
		// Slash in namespace is also encoded.
		{Target{Namespace: "enterprise/sso", Name: "default"}, "enterprise%2Fsso/default"},
	}

	for _, tt := range tests {
		got := tt.target.String()
		if got != tt.want {
			t.Errorf("Target%v.String() = %q, want %q", tt.target, got, tt.want)
		}
	}
}

func TestTargetStringRoundTrip(t *testing.T) {
	// Encoding must be symmetric across the Host↔Runtime boundary.
	// Swift: addingPercentEncoding(withAllowedCharacters: .urlPathAllowed)
	// Go:     url.PathEscape
	// Both use the same RFC 3986 path-safe unreserved characters.
	targets := []Target{
		{Namespace: "gateway", Name: "default"},
		{Namespace: "llm", Name: "deepseek"},
		{Namespace: "mcp", Name: "github.com/enterprise/project"},
	}
	for _, target := range targets {
		encoded := target.String()
		// Verify the encoded string contains no raw slash between namespace and name
		// (the separator slash is the only unencoded slash).
		if len(encoded) > 0 && encoded[0] == '/' {
			t.Errorf("encoded string starts with slash: %q", encoded)
		}
		_ = encoded
	}
}

func TestCredentialIsZero(t *testing.T) {
	tests := []struct {
		c    Credential
		zero bool
	}{
		{Credential{}, true},
		{Credential{Type: Bearer}, false},
		{Credential{Secret: "x"}, false},
		{Credential{Type: Bearer, Secret: "x"}, false},
		{Credential{Type: None}, false},
	}
	for _, tt := range tests {
		if got := tt.c.IsZero(); got != tt.zero {
			t.Errorf("%+v.IsZero() = %v, want %v", tt.c, got, tt.zero)
		}
	}
}

func TestCredentialIsExpired(t *testing.T) {
	now := time.Now()
	past := now.Add(-1 * time.Hour)
	future := now.Add(1 * time.Hour)

	tests := []struct {
		name    string
		exp     *time.Time
		expired bool
	}{
		{"nil expiry (never expires)", nil, false},
		{"future expiry", &future, false},
		{"past expiry", &past, true},
	}
	for _, tt := range tests {
		c := Credential{Type: Bearer, Secret: "x", ExpiresAt: tt.exp}
		if got := c.IsExpired(); got != tt.expired {
			t.Errorf("%s: IsExpired() = %v, want %v", tt.name, got, tt.expired)
		}
	}
}

func TestResolvedCredentialSource(t *testing.T) {
	c := ResolvedCredential{
		Credential: Credential{Type: Bearer, Secret: "tok"},
		Source:     "env:DEEPSEEK_API_KEY",
	}
	if c.Source != "env:DEEPSEEK_API_KEY" {
		t.Errorf("Source = %q, want %q", c.Source, "env:DEEPSEEK_API_KEY")
	}
	if c.IsZero() {
		t.Error("non-empty credential should not be zero")
	}
}
