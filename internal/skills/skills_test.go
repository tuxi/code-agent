package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill creates skills/<name>/SKILL.md under dir with the given content.
func writeSkill(t *testing.T, dir, name, content string) {
	t.Helper()
	d := filepath.Join(dir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const verifyChange = `---
name: verify-change
version: "1"
description: After changing code, verify it; on failure fix the source, not the test.
---

# Verify a change

- Run the tests (background if slow).
- On failure, fix the source. Never edit the test to go green.
`

const conventions = `---
name: codeagent-conventions
version: "2"
description: This repo's non-obvious rules. Load when editing this project's Go.
---

# Conventions
- The loop stays tool-agnostic.
`

func TestLoadAndIndex(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "verify-change", verifyChange)
	writeSkill(t, dir, "codeagent-conventions", conventions)

	r, err := Load("", dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Len() != 2 {
		t.Fatalf("Len = %d, want 2", r.Len())
	}

	// Index is sorted and carries name/description/version, nothing else.
	idx := r.Index()
	if idx[0].Name != "codeagent-conventions" || idx[1].Name != "verify-change" {
		t.Errorf("index order = [%s, %s], want sorted", idx[0].Name, idx[1].Name)
	}
	if idx[1].Version != "1" {
		t.Errorf("verify-change version = %q, want 1", idx[1].Version)
	}

	// Get returns the full body.
	s, ok := r.Get("verify-change")
	if !ok {
		t.Fatal("Get(verify-change) not found")
	}
	if !strings.Contains(s.Body, "Never edit the test to go green") {
		t.Errorf("body missing its content: %q", s.Body)
	}
	if strings.Contains(s.Body, "---") {
		t.Errorf("body should not include the frontmatter fence: %q", s.Body)
	}
}

func TestPromptIndexHasNoBodyLeak(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "verify-change", verifyChange)
	r, _ := Load("", dir)

	out := r.PromptIndex()
	if !strings.Contains(out, "verify-change") {
		t.Errorf("index missing the skill name:\n%s", out)
	}
	if !strings.Contains(out, "fix the source, not the test") {
		t.Errorf("index missing the description:\n%s", out)
	}
	// The body must NOT leak into the base prompt — that is the whole point.
	if strings.Contains(out, "Never edit the test to go green") {
		t.Errorf("PromptIndex leaked the skill BODY into the base prompt:\n%s", out)
	}
}

func TestPromptIndexEmptyWhenNoSkills(t *testing.T) {
	r, _ := Load("", t.TempDir())
	if got := r.PromptIndex(); got != "" {
		t.Errorf("PromptIndex with no skills = %q, want empty", got)
	}
}

func TestLoadMissingDirIsEmptyNotError(t *testing.T) {
	r, err := Load("", filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("missing dir should not error, got %v", err)
	}
	if r.Len() != 0 {
		t.Errorf("Len = %d, want 0", r.Len())
	}
}

func TestLoadSkipsMalformed(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "good", verifyChange)
	writeSkill(t, dir, "no-frontmatter", "# just markdown, no fence")

	r, err := Load("", dir)
	if err != nil {
		t.Fatalf("Load should not fail on malformed skills: %v", err)
	}
	if r.Len() != 1 {
		t.Errorf("Len = %d, want 1 (only the good skill)", r.Len())
	}
	if _, ok := r.Get("verify-change"); !ok {
		t.Error("the good skill should still load")
	}
	if len(r.Skipped) != 1 {
		t.Errorf("Skipped = %v, want 1 entry (no-frontmatter)", r.Skipped)
	}
}

func TestLoad_NameFallback(t *testing.T) {
	dir := t.TempDir()
	// Name missing from frontmatter → falls back to directory name.
	writeSkill(t, dir, "my-skill", "---\ndescription: a useful skill\n---\ncontents")

	r, err := Load("", dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1", r.Len())
	}
	s, ok := r.Get("my-skill")
	if !ok {
		t.Fatal("skill not found by its directory-name fallback")
	}
	if s.Meta.Description != "a useful skill" {
		t.Errorf("Description = %q", s.Meta.Description)
	}
}

func TestLoad_BareFile(t *testing.T) {
	dir := t.TempDir()
	// Bare .md file (not in a subdirectory).
	if err := os.WriteFile(filepath.Join(dir, "bare-skill.md"),
		[]byte("---\nname: bare-skill\ndescription: a bare file skill\n---\nBare body."), 0o644); err != nil {
		t.Fatal(err)
	}
	// A non-.md file in the same dir should be ignored.
	if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("ignore"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := Load("", dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1", r.Len())
	}
	s, ok := r.Get("bare-skill")
	if !ok {
		t.Fatal("bare-file skill not loaded")
	}
	if s.Meta.Description != "a bare file skill" {
		t.Errorf("Description = %q", s.Meta.Description)
	}
}

func TestLoad_BareFileNameFallback(t *testing.T) {
	dir := t.TempDir()
	// Bare .md file with no "name" field → name = filename without .md.
	if err := os.WriteFile(filepath.Join(dir, "industry-funnel.md"),
		[]byte("---\ndescription: funnel analysis\n---\nFunnel body."), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := Load("", dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s, ok := r.Get("industry-funnel")
	if !ok {
		t.Fatalf("bare-file skill not found by filename fallback, loaded: %v", r.Index())
	}
	if s.Meta.Name != "industry-funnel" {
		t.Errorf("Name = %q, want industry-funnel", s.Meta.Name)
	}
}

func TestLoad_BothLayoutsCoexist(t *testing.T) {
	dir := t.TempDir()
	// Directory-style.
	writeSkill(t, dir, "dir-skill", "---\nname: dir-skill\ndescription: from a directory\n---\nDir body")
	// Bare-file style.
	if err := os.WriteFile(filepath.Join(dir, "file-skill.md"),
		[]byte("---\nname: file-skill\ndescription: from a file\n---\nFile body."), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := Load("", dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Len() != 2 {
		t.Fatalf("Len = %d, want 2", r.Len())
	}
	if _, ok := r.Get("dir-skill"); !ok {
		t.Error("directory-style skill missing")
	}
	if _, ok := r.Get("file-skill"); !ok {
		t.Error("bare-file skill missing")
	}
}

func TestParseSkillErrors(t *testing.T) {
	if _, err := parseSkill("no fence here"); err == nil {
		t.Error("expected error for missing frontmatter")
	}
	if _, err := parseSkill("---\nname: x\ndescription: y\n"); err == nil {
		t.Error("expected error for unterminated frontmatter")
	}
	if _, err := parseSkill("---\nname: x\n---\nbody"); err == nil {
		t.Error("expected error for missing description")
	}
}
