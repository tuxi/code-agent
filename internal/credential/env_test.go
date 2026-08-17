package credential

import (
	"context"
	"os"
	"testing"
)

func TestEnvResolverDefaultMapping(t *testing.T) {
	r := &EnvResolver{}

	// Set up test env vars.
	os.Setenv("DEEPSEEK_API_KEY", "sk-deepseek")
	os.Setenv("OPENAI_API_KEY", "sk-openai")
	defer func() {
		os.Unsetenv("DEEPSEEK_API_KEY")
		os.Unsetenv("OPENAI_API_KEY")
	}()

	ctx := context.Background()

	// Known llm target.
	c, err := r.Resolve(ctx, Target{Namespace: "llm", Name: "deepseek"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.IsZero() {
		t.Fatal("expected non-zero credential for deepseek")
	}
	if c.Type != Bearer {
		t.Errorf("Type = %q, want %q", c.Type, Bearer)
	}
	if c.Secret != "sk-deepseek" {
		t.Errorf("Secret = %q, want %q", c.Secret, "sk-deepseek")
	}
	if c.Source != "env:DEEPSEEK_API_KEY" {
		t.Errorf("Source = %q, want %q", c.Source, "env:DEEPSEEK_API_KEY")
	}
	if c.ExpiresAt != nil {
		t.Error("env credential should have nil ExpiresAt")
	}
}

func TestEnvResolverNotMyTarget(t *testing.T) {
	r := &EnvResolver{}
	ctx := context.Background()

	// Non-llm namespace — not handled by default.
	c, err := r.Resolve(ctx, Target{Namespace: "gateway", Name: "default"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.IsZero() {
		t.Errorf("expected zero credential for gateway target, got %+v", c)
	}
}

func TestEnvResolverMissingVar(t *testing.T) {
	r := &EnvResolver{}
	ctx := context.Background()

	// Unset env var — should return zero, not error.
	os.Unsetenv("UNKNOWN_API_KEY")
	c, err := r.Resolve(ctx, Target{Namespace: "llm", Name: "unknown"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.IsZero() {
		t.Errorf("expected zero credential for unset env var, got %+v", c)
	}
}

func TestEnvResolverCustomMapping(t *testing.T) {
	os.Setenv("MY_CUSTOM_KEY", "sk-custom")
	defer os.Unsetenv("MY_CUSTOM_KEY")

	r := &EnvResolver{
		Mapping: map[string][]Target{
			"MY_CUSTOM_KEY": {{Namespace: "gateway", Name: "default"}},
		},
	}
	ctx := context.Background()

	c, err := r.Resolve(ctx, Target{Namespace: "gateway", Name: "default"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Secret != "sk-custom" {
		t.Errorf("Secret = %q, want %q", c.Secret, "sk-custom")
	}
	if c.Source != "env:MY_CUSTOM_KEY" {
		t.Errorf("Source = %q, want %q", c.Source, "env:MY_CUSTOM_KEY")
	}
}

func TestEnvResolverCustomMappingNotMatched(t *testing.T) {
	r := &EnvResolver{
		Mapping: map[string][]Target{
			"MY_CUSTOM_KEY": {{Namespace: "gateway", Name: "default"}},
		},
	}
	ctx := context.Background()

	// A target not in the custom mapping should return zero.
	c, err := r.Resolve(ctx, Target{Namespace: "llm", Name: "deepseek"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.IsZero() {
		t.Errorf("expected zero credential, got %+v", c)
	}
}

// TestEnvResolverUseDefault verifies the explicit "mapping plus convention"
// mode (R1.4): with UseDefault set, a target absent from the mapping still
// falls back to the default <NAME>_API_KEY convention.
func TestEnvResolverUseDefault(t *testing.T) {
	os.Setenv("DEEPSEEK_API_KEY", "sk-convention")
	defer os.Unsetenv("DEEPSEEK_API_KEY")

	r := &EnvResolver{
		Mapping: map[string][]Target{
			"MY_CUSTOM_KEY": {{Namespace: "gateway", Name: "default"}},
		},
		UseDefault: true,
	}
	ctx := context.Background()

	// llm/deepseek is not in the mapping — UseDefault falls back to the
	// convention (DEEPSEEK_API_KEY).
	c, err := r.Resolve(ctx, Target{Namespace: "llm", Name: "deepseek"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Secret != "sk-convention" {
		t.Errorf("Secret = %q, want %q (default convention fallback)", c.Secret, "sk-convention")
	}
	if c.Source != "env:DEEPSEEK_API_KEY" {
		t.Errorf("Source = %q, want %q", c.Source, "env:DEEPSEEK_API_KEY")
	}

	// Non-llm targets still resolve via the explicit mapping only.
	c, err = r.Resolve(ctx, Target{Namespace: "gateway", Name: "default"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Secret != "" {
		t.Errorf("gateway/default should be zero without MY_CUSTOM_KEY set, got %q", c.Secret)
	}
}

// TestEnvResolverUseDefaultStillHonorsMapping verifies that an explicitly
// mapped target wins over the convention when UseDefault is set.
func TestEnvResolverUseDefaultStillHonorsMapping(t *testing.T) {
	os.Setenv("MY_CUSTOM_KEY", "sk-custom")
	defer os.Unsetenv("MY_CUSTOM_KEY")
	os.Setenv("DEEPSEEK_API_KEY", "sk-convention")
	defer os.Unsetenv("DEEPSEEK_API_KEY")

	r := &EnvResolver{
		Mapping: map[string][]Target{
			"MY_CUSTOM_KEY": {{Namespace: "llm", Name: "deepseek"}},
		},
		UseDefault: true,
	}
	ctx := context.Background()

	c, err := r.Resolve(ctx, Target{Namespace: "llm", Name: "deepseek"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Secret != "sk-custom" {
		t.Errorf("Secret = %q, want %q (explicit mapping must win)", c.Secret, "sk-custom")
	}
}

// TestEnvResolverDefaultMappingHyphen verifies the default convention
// normalizes "-" to "_" so a hyphenated provider name resolves to a usable
// env var (env vars cannot contain "-"). Regression test for opencode-go:
// the registry declares OPENCODE_GO_API_KEY, so the default convention must
// not look for the unusable OPENCODE-GO_API_KEY.
func TestEnvResolverDefaultMappingHyphen(t *testing.T) {
	os.Setenv("OPENCODE_GO_API_KEY", "sk-opencode")
	defer os.Unsetenv("OPENCODE_GO_API_KEY")

	r := &EnvResolver{}
	c, err := r.Resolve(context.Background(), Target{Namespace: "llm", Name: "opencode-go"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Secret != "sk-opencode" {
		t.Errorf("Secret = %q, want %q (hyphen must map to underscore)", c.Secret, "sk-opencode")
	}
	if c.Source != "env:OPENCODE_GO_API_KEY" {
		t.Errorf("Source = %q, want %q", c.Source, "env:OPENCODE_GO_API_KEY")
	}
}
