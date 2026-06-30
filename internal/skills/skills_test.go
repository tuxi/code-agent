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

const licensedSkill = `---
name: proprietary-skill
version: "1"
description: A skill with a license.
license: Proprietary. LICENSE.txt has complete terms
---

# Licensed

Proprietary content.
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
	// Directory-style skills have Dir set and Resources scanned (even if empty).
	if s.Dir == "" {
		t.Error("directory-style skill should have Dir set")
	}
	if !strings.HasSuffix(s.Dir, "verify-change") {
		t.Errorf("Dir should point to the skill root, got: %s", s.Dir)
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

func TestLoad_MissingNameIsSkipped(t *testing.T) {
	dir := t.TempDir()
	// Name missing from frontmatter → skipped, no fallback.
	writeSkill(t, dir, "my-skill", "---\ndescription: a useful skill\n---\ncontents")

	r, err := Load("", dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Len() != 0 {
		t.Fatalf("Len = %d, want 0 (missing name → skipped)", r.Len())
	}
	if _, ok := r.Get("my-skill"); ok {
		t.Error("skill with missing name should not be loaded")
	}
	if len(r.Skipped) != 1 {
		t.Errorf("Skipped = %v, want 1 entry", r.Skipped)
	}
	if reason := r.Skipped["my-skill"]; !strings.Contains(reason, "name") {
		t.Errorf("skip reason should mention 'name', got: %s", reason)
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
	// Bare-file skills have no directory and therefore no resources.
	if s.Dir != "" {
		t.Errorf("bare-file skill Dir = %q, want empty", s.Dir)
	}
	if len(s.Resources) != 0 {
		t.Errorf("bare-file skill Resources = %v, want empty", s.Resources)
	}
}

func TestLoad_BareFileMissingNameIsSkipped(t *testing.T) {
	dir := t.TempDir()
	// Bare .md file with no "name" field → skipped, filename is not a fallback.
	if err := os.WriteFile(filepath.Join(dir, "industry-funnel.md"),
		[]byte("---\ndescription: funnel analysis\n---\nFunnel body."), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := Load("", dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Len() != 0 {
		t.Fatalf("Len = %d, want 0 (missing name in bare file → skipped)", r.Len())
	}
	if _, ok := r.Get("industry-funnel"); ok {
		t.Error("bare-file skill with missing name should not be loaded by filename fallback")
	}
	if len(r.Skipped) != 1 {
		t.Errorf("Skipped = %v, want 1 entry", r.Skipped)
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
	if s, ok := r.Get("dir-skill"); !ok {
		t.Error("directory-style skill missing")
	} else if s.Dir == "" {
		t.Error("directory-style skill should have Dir set")
	}
	if s, ok := r.Get("file-skill"); !ok {
		t.Error("bare-file skill missing")
	} else if s.Dir != "" {
		t.Errorf("bare-file skill Dir = %q, want empty", s.Dir)
	}
}

func TestLoad_ResourcesIndexed(t *testing.T) {
	dir := t.TempDir()
	// Create a directory-style skill with references, scripts, and assets.
	skillDir := filepath.Join(dir, "rich-skill")
	writeSkill(t, dir, "rich-skill", "---\nname: rich-skill\ndescription: has resources\n---\nSee references/api.md.")

	// L3 resource directories.
	refs := filepath.Join(skillDir, "references")
	scripts := filepath.Join(skillDir, "scripts")
	assets := filepath.Join(skillDir, "assets")
	for _, d := range []string{refs, scripts, assets} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Write resource files.
	os.WriteFile(filepath.Join(refs, "api.md"), []byte("# API"), 0o644)
	os.WriteFile(filepath.Join(refs, "examples.md"), []byte("# Examples"), 0o644)
	os.WriteFile(filepath.Join(scripts, "build.sh"), []byte("#!/bin/bash\necho ok"), 0o755)
	os.WriteFile(filepath.Join(assets, "logo.png"), []byte("PNG..."), 0o644)
	// A subdirectory inside a resource dir should be ignored.
	os.MkdirAll(filepath.Join(scripts, "helpers"), 0o755)
	os.WriteFile(filepath.Join(scripts, "helpers", "util.py"), []byte("print('ok')"), 0o644)

	r, err := Load("", dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1", r.Len())
	}
	s, ok := r.Get("rich-skill")
	if !ok {
		t.Fatal("rich-skill not found")
	}
	if s.Dir == "" {
		t.Fatal("directory-style skill should have Dir set")
	}
	if len(s.Resources) != 4 {
		t.Fatalf("Resources = %d, want 4 (2 refs + 1 script + 1 asset)", len(s.Resources))
	}

	// Verify sorting: references first, then assets, then scripts (alphabetical by kind).
	want := []struct {
		kind     string
		pathEnds string // suffix match so test is portable across temp dirs
	}{
		{"asset", "/assets/logo.png"},
		{"reference", "/references/api.md"},
		{"reference", "/references/examples.md"},
		{"script", "/scripts/build.sh"},
	}
	for i, w := range want {
		if s.Resources[i].Kind != w.kind {
			t.Errorf("Resource[%d] kind = %q, want %q", i, s.Resources[i].Kind, w.kind)
		}
		if !strings.HasSuffix(s.Resources[i].Path, w.pathEnds) {
			t.Errorf("Resource[%d] path = %q, want suffix %q", i, s.Resources[i].Path, w.pathEnds)
		}
	}
	// Subdirectory files must not appear.
	for _, res := range s.Resources {
		if strings.Contains(res.Path, "helpers") {
			t.Errorf("subdirectory file leaked into resources: %s", res.Path)
		}
	}
}

func TestLoad_LicenseRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "proprietary-skill", licensedSkill)

	r, err := Load("", dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s, ok := r.Get("proprietary-skill")
	if !ok {
		t.Fatal("proprietary-skill not found")
	}
	if s.Meta.License != "Proprietary. LICENSE.txt has complete terms" {
		t.Errorf("License = %q, want %q", s.Meta.License, "Proprietary. LICENSE.txt has complete terms")
	}

	// PromptIndex must include the license.
	out := r.PromptIndex()
	if !strings.Contains(out, "[Proprietary. LICENSE.txt has complete terms]") {
		t.Errorf("PromptIndex missing license:\n%s", out)
	}
}

func TestPromptIndex_NoLicenseNoise(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "verify-change", verifyChange) // no license field

	r, _ := Load("", dir)
	out := r.PromptIndex()
	if strings.Contains(out, "[") && strings.Contains(out, "]") {
		t.Errorf("PromptIndex with no-license skill should not contain bracket noise:\n%s", out)
	}
}

func TestParseSkillErrors(t *testing.T) {
	if _, err := parseSkill("no fence here"); err == nil {
		t.Error("expected error for missing frontmatter")
	}
	if _, err := parseSkill("---\nname: x\ndescription: y\n"); err == nil {
		t.Error("expected error for unterminated frontmatter")
	}
	if _, err := parseSkill("---\ndescription: y\n---\nbody"); err == nil {
		t.Error("expected error for missing name")
	}
	if _, err := parseSkill("---\nname: x\n---\nbody"); err == nil {
		t.Error("expected error for missing description")
	}
}
