package credential

import (
	"context"
	"testing"
	"time"
)

func TestStaticResolverHit(t *testing.T) {
	target := Target{Namespace: "gateway", Name: "default"}
	expiry := time.Now().Add(1 * time.Hour)
	r := StaticResolver{
		target: {Type: Bearer, Secret: "tok", ExpiresAt: &expiry},
	}

	ctx := context.Background()
	c, err := r.Resolve(ctx, target)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.IsZero() {
		t.Fatal("expected non-zero credential")
	}
	if c.Secret != "tok" {
		t.Errorf("Secret = %q, want %q", c.Secret, "tok")
	}
	if c.Type != Bearer {
		t.Errorf("Type = %q, want %q", c.Type, Bearer)
	}
	if c.ExpiresAt == nil || !c.ExpiresAt.Equal(expiry) {
		t.Error("ExpiresAt not preserved")
	}
	if c.Source != "static:"+target.String() {
		t.Errorf("Source = %q, want %q", c.Source, "static:"+target.String())
	}
}

func TestStaticResolverMiss(t *testing.T) {
	r := StaticResolver{
		{Namespace: "llm", Name: "deepseek"}: {Type: Bearer, Secret: "sk"},
	}

	ctx := context.Background()
	c, err := r.Resolve(ctx, Target{Namespace: "gateway", Name: "default"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.IsZero() {
		t.Errorf("expected zero credential for unknown target, got %+v", c)
	}
}

func TestStaticResolverEmpty(t *testing.T) {
	r := StaticResolver{}
	ctx := context.Background()
	c, err := r.Resolve(ctx, Target{Namespace: "llm", Name: "deepseek"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.IsZero() {
		t.Errorf("expected zero credential from empty resolver, got %+v", c)
	}
}

func TestStaticResolverMultipleTargets(t *testing.T) {
	r := StaticResolver{
		{Namespace: "llm", Name: "deepseek"}:    {Type: Bearer, Secret: "sk-ds"},
		{Namespace: "gateway", Name: "default"}: {Type: Bearer, Secret: "jwt"},
		{Namespace: "mcp", Name: "github"}:      {Type: Bearer, Secret: "gho"},
	}
	ctx := context.Background()

	tests := []struct {
		target Target
		secret string
	}{
		{Target{Namespace: "llm", Name: "deepseek"}, "sk-ds"},
		{Target{Namespace: "gateway", Name: "default"}, "jwt"},
		{Target{Namespace: "mcp", Name: "github"}, "gho"},
	}
	for _, tt := range tests {
		c, err := r.Resolve(ctx, tt.target)
		if err != nil {
			t.Errorf("%v: unexpected error: %v", tt.target, err)
			continue
		}
		if c.Secret != tt.secret {
			t.Errorf("%v: Secret = %q, want %q", tt.target, c.Secret, tt.secret)
		}
	}
}
