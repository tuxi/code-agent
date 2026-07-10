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
