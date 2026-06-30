package skills

import (
	"path/filepath"
	"testing"
)

// The two-level load: global skills form a shared pool; project skills can override
// them by name. The user's own ~/.claude-equivalent directory is the global tier.
func TestLoad_GlobalPlusProject(t *testing.T) {
	globalDir := t.TempDir()
	projDir := t.TempDir()

	// Global skill: available everywhere.
	writeSkill(t, globalDir, "shared", "---\nname: shared\ndescription: global shared skill\n---\nglobal body")

	// Project skill: same name, different body — should override global.
	writeSkill(t, projDir, "override", "---\nname: shared\ndescription: project override\n---\nproject body")

	// Project-only skill.
	writeSkill(t, projDir, "project-only", "---\nname: project-only\ndescription: only in this project\n---\nproj body")

	r, err := Load(globalDir, projDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Len() != 2 {
		t.Fatalf("Len = %d, want 2 (shared+project-only)", r.Len())
	}

	s, ok := r.Get("shared")
	if !ok {
		t.Fatal("shared skill not found")
	}
	// The project override must win.
	if s.Meta.Description != "project override" {
		t.Errorf("Description = %q, want 'project override' (project should override global)", s.Meta.Description)
	}

	if _, ok := r.Get("project-only"); !ok {
		t.Error("project-only skill not found")
	}
}

func TestLoad_GlobalOnly(t *testing.T) {
	globalDir := t.TempDir()
	writeSkill(t, globalDir, "shared", "---\nname: shared\ndescription: shared skill\n---\nbody")

	// No project dir → empty string passes cleanly.
	r, err := Load(globalDir, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1", r.Len())
	}
}

func TestLoad_EmptyGlobalIsFine(t *testing.T) {
	projDir := t.TempDir()
	writeSkill(t, projDir, "project-only", "---\nname: project-only\ndescription: only here\n---\nbody")

	// Empty global dir — no-op, just project skills.
	r, err := Load("", projDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1", r.Len())
	}
}

func TestLoad_GlobalDirDoesNotExist(t *testing.T) {
	projDir := t.TempDir()
	writeSkill(t, projDir, "project-only", "---\nname: project-only\ndescription: only here\n---\nbody")

	r, err := Load(filepath.Join(t.TempDir(), "does-not-exist"), projDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1", r.Len())
	}
}

// writeSkill is defined in skills_test.go — this file reuses it.
