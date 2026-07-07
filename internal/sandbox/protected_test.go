package sandbox

import (
	"testing"
)

func TestMatchPathPattern(t *testing.T) {
	tests := []struct {
		name, pattern string
		want          bool
	}{
		// Exact (case-insensitive)
		{".env", ".env", true},
		{".ENV", ".env", true},
		{".env.local", ".env", false},
		{"main.go", ".env", false},

		// Glob
		{"server.key", "*.key", true},
		{"ca.key", "*.key", true},
		{"key.pem", "*.pem", true},
		{"server.crt", "*.key", false},
		{"key.backup", "*.key", false},

		// No wildcard, exact only
		{"credentials", "credentials", true},
		{"CREDENTIALS", "credentials", true},
		{"my-credentials", "credentials", false},
	}
	for _, tt := range tests {
		t.Run(tt.name+"_vs_"+tt.pattern, func(t *testing.T) {
			if got := matchPathPattern(tt.name, tt.pattern); got != tt.want {
				t.Errorf("matchPathPattern(%q, %q) = %v, want %v", tt.name, tt.pattern, got, tt.want)
			}
		})
	}
}

func TestIsPathProtected(t *testing.T) {
	protected := ProtectedPaths(nil) // defaults only

	t.Run("built-in defaults", func(t *testing.T) {
		for _, path := range []string{
			".env", ".env.local", ".env.production",
			".git-credentials",
			"credentials", "secrets", "tokens",
			"private.key", "id_rsa", "id_ed25519",
			"server.key", "ca.pem",
		} {
			if !IsPathProtected(path, protected) {
				t.Errorf("%q should be protected", path)
			}
		}
	})

	t.Run("deep paths protected by base name", func(t *testing.T) {
		if !IsPathProtected("apps/backend/.env.production", protected) {
			t.Error("deep path should be protected by base name")
		}
	})

	t.Run("normal files not protected", func(t *testing.T) {
		for _, path := range []string{"main.go", "README.md", "cmd/server/main.go"} {
			if IsPathProtected(path, protected) {
				t.Errorf("%q should not be protected", path)
			}
		}
	})
}

func TestProtectedPathsMergesExtras(t *testing.T) {
	extra := []string{".npmrc", "*.toml"}
	merged := ProtectedPaths(extra)

	for _, want := range []string{".npmrc", "*.toml"} {
		found := false
		for _, p := range merged {
			if p == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("merged list missing %q", want)
		}
	}

	// Built-in defaults still present
	for _, want := range []string{".env", "credentials", "private.key"} {
		found := false
		for _, p := range merged {
			if p == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("merged list missing built-in %q", want)
		}
	}
}

func TestProtectedPathsDeduplicates(t *testing.T) {
	// Adding a duplicate should not double it.
	extra := []string{".env"} // already in defaults
	merged := ProtectedPaths(extra)

	count := 0
	for _, p := range merged {
		if p == ".env" {
			count++
		}
	}
	if count != 1 {
		t.Errorf(".env appears %d times in merged list, want 1", count)
	}
}
