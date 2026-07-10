package credential

import (
	"context"
	"errors"
	"testing"
)

func TestChainResolverFirstWins(t *testing.T) {
	r := &ChainResolver{
		Resolvers: []Resolver{
			StaticResolver{
				{Namespace: "llm", Name: "deepseek"}: {Type: Bearer, Secret: "first"},
			},
			StaticResolver{
				{Namespace: "llm", Name: "deepseek"}: {Type: Bearer, Secret: "second"},
			},
		},
	}
	ctx := context.Background()
	c, err := r.Resolve(ctx, Target{Namespace: "llm", Name: "deepseek"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Secret != "first" {
		t.Errorf("Secret = %q, want %q", c.Secret, "first")
	}
}

func TestChainResolverFallsThrough(t *testing.T) {
	r := &ChainResolver{
		Resolvers: []Resolver{
			StaticResolver{}, // empty — returns zero for everything
			StaticResolver{
				{Namespace: "llm", Name: "deepseek"}: {Type: Bearer, Secret: "found"},
			},
		},
	}
	ctx := context.Background()
	c, err := r.Resolve(ctx, Target{Namespace: "llm", Name: "deepseek"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Secret != "found" {
		t.Errorf("Secret = %q, want %q", c.Secret, "found")
	}
}

func TestChainResolverAllMiss(t *testing.T) {
	r := &ChainResolver{
		Resolvers: []Resolver{
			StaticResolver{},
			StaticResolver{},
		},
	}
	ctx := context.Background()
	c, err := r.Resolve(ctx, Target{Namespace: "llm", Name: "deepseek"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.IsZero() {
		t.Errorf("expected zero credential when all miss, got %+v", c)
	}
}

func TestChainResolverFailFast(t *testing.T) {
	r := &ChainResolver{
		FailFast: true,
		Resolvers: []Resolver{
			&errorResolver{err: errors.New("boom")},
			StaticResolver{
				{Namespace: "llm", Name: "deepseek"}: {Type: Bearer, Secret: "sk"},
			},
		},
	}
	ctx := context.Background()
	_, err := r.Resolve(ctx, Target{Namespace: "llm", Name: "deepseek"})
	if err == nil {
		t.Fatal("expected error with FailFast, got nil")
	}
}

func TestChainResolverFailFastOff(t *testing.T) {
	r := &ChainResolver{
		FailFast: false,
		Resolvers: []Resolver{
			&errorResolver{err: errors.New("boom")},
			StaticResolver{
				{Namespace: "llm", Name: "deepseek"}: {Type: Bearer, Secret: "recovered"},
			},
		},
	}
	ctx := context.Background()
	c, err := r.Resolve(ctx, Target{Namespace: "llm", Name: "deepseek"})
	if err != nil {
		t.Fatalf("unexpected error with FailFast off: %v", err)
	}
	if c.Secret != "recovered" {
		t.Errorf("Secret = %q, want %q", c.Secret, "recovered")
	}
	if c.Source != "chain[1]=static:llm/deepseek" {
		t.Errorf("Source = %q, want %q", c.Source, "chain[1]=static:llm/deepseek")
	}
}

func TestChainResolverAllFailNoFast(t *testing.T) {
	r := &ChainResolver{
		FailFast: false,
		Resolvers: []Resolver{
			&errorResolver{err: errors.New("err1")},
			&errorResolver{err: errors.New("err2")},
		},
	}
	ctx := context.Background()
	_, err := r.Resolve(ctx, Target{Namespace: "llm", Name: "deepseek"})
	if err == nil {
		t.Fatal("expected aggregated error, got nil")
	}
}

func TestChainResolverEmpty(t *testing.T) {
	r := &ChainResolver{}
	ctx := context.Background()
	c, err := r.Resolve(ctx, Target{Namespace: "llm", Name: "deepseek"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.IsZero() {
		t.Errorf("expected zero credential from empty chain, got %+v", c)
	}
}

// errorResolver always returns an error for every target.
type errorResolver struct{ err error }

func (r *errorResolver) Resolve(_ context.Context, _ Target) (ResolvedCredential, error) {
	return ResolvedCredential{}, r.err
}
