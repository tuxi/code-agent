package skills

import (
	"os"
	"path/filepath"
	"testing"
)

// Many bare .md files in a flat directory must all be recognized.
func TestLoad_ManyFlatFiles(t *testing.T) {
	dir := t.TempDir()

	files := map[string]string{
		"alpha.md":   "---\nname: alpha\ndescription: first skill\n---\nbody alpha",
		"beta.md":    "---\nname: beta\ndescription: second skill\n---\nbody beta",
		"gamma.md":   "---\nname: gamma\ndescription: third skill\n---\nbody gamma",
		"delta.md":   "---\nname: delta\ndescription: fourth skill\n---\nbody delta",
		"epsilon.md": "---\nname: epsilon\ndescription: fifth skill\n---\nbody epsilon",
		// This one has no "name" field — falls back to filename "zeta".
		"zeta.md":    "---\ndescription: no name field\n---\nbody zeta",
		// Non-.md file — must be silently skipped.
		"README.txt": "ignore me",
		// Directory-style — must coexist with flat files.
	}

	subDir := filepath.Join(dir, "sub-dir")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files[filepath.Join("sub-dir", "SKILL.md")] = "---\nname: sub-dir\ndescription: from subdirectory\n---\nsub body"

	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	r, err := Load("", dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Len() != 7 {
		t.Fatalf("Len = %d, want 7 (alpha, beta, gamma, delta, epsilon, zeta, sub-dir)", r.Len())
	}
	for _, name := range []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "sub-dir"} {
		if _, ok := r.Get(name); !ok {
			t.Errorf("skill %q not found", name)
		}
	}
	if len(r.Skipped) != 0 {
		t.Errorf("unexpected skipped: %v", r.Skipped)
	}
}
