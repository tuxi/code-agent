package credential

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A missing secrets file yields a nil resolver (env-only fallback, never an
// error that bricks startup).
func TestSecretsFileMissingYieldsNil(t *testing.T) {
	r, err := (SecretsFile{Path: filepath.Join(t.TempDir(), "nope.json")}).Load()
	if err != nil {
		t.Fatalf("Load(missing): %v", err)
	}
	if r != nil {
		t.Error("expected nil resolver for missing file")
	}
}

// llm/<id> entries parse into a resolver keyed by Target.
func TestSecretsFileLoadsLLMEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")
	doc := `{"llm/qwen": {"type":"bearer","secret":"sk-qwen"}, "llm/deepseek": {"type":"bearer","secret":"sk-ds"}}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := (SecretsFile{Path: path}).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r == nil {
		t.Fatal("expected non-nil resolver")
	}
	c, err := r.Resolve(context.Background(), Target{Namespace: "llm", Name: "qwen"})
	if err != nil || c.Secret != "sk-qwen" {
		t.Errorf("llm/qwen = %q, %v; want sk-qwen", c.Secret, err)
	}
	c, err = r.Resolve(context.Background(), Target{Namespace: "llm", Name: "deepseek"})
	if err != nil || c.Secret != "sk-ds" {
		t.Errorf("llm/deepseek = %q, %v; want sk-ds", c.Secret, err)
	}
}

// Non-llm namespaces and empty secrets are ignored.
func TestSecretsFileFiltersNonLLM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")
	doc := `{"gateway/default": {"type":"bearer","secret":"jwt"}, "llm/empty": {"type":"bearer","secret":""}}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := (SecretsFile{Path: path}).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r != nil {
		// gateway and empty-secret entries filtered out → empty resolver → nil.
		t.Errorf("expected nil (nothing consumable), got %T", r)
	}
}

// A malformed file is treated as absent (best-effort, never bricks startup).
func TestSecretsFileMalformedYieldsNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := (SecretsFile{Path: path}).Load()
	if err != nil {
		t.Fatalf("Load(malformed): %v", err)
	}
	if r != nil {
		t.Error("expected nil resolver for malformed file")
	}
}
