package systemskills

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func copyBundledSkill(t *testing.T, bundle fs.FS, name, destination string) {
	t.Helper()
	if err := copyFS(mustSub(t, bundle, name), destination); err != nil {
		t.Fatal(err)
	}
}

func mustSub(t *testing.T, fsys fs.FS, dir string) fs.FS {
	t.Helper()
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func TestLegacyBundleOwnedSkillsAreMigrated(t *testing.T) {
	home := t.TempDir()
	skillsRoot := filepath.Join(home, ".codeagent", "skills")
	for _, name := range []string{"review-change", "verify-change", "skill-creator"} {
		copyBundledSkill(t, Embedded(), name, filepath.Join(skillsRoot, name))
	}
	report, err := InstallWithReport(Embedded(), skillsRoot, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Legacy) != 3 {
		t.Fatalf("legacy migrations = %#v", report.Legacy)
	}
	for _, migration := range report.Legacy {
		if migration.State != LegacyMigrated || migration.Warning != "" {
			t.Fatalf("migration = %#v", migration)
		}
		if _, err := os.Stat(filepath.Join(skillsRoot, migration.Name)); !os.IsNotExist(err) {
			t.Fatalf("legacy skill %q still exists", migration.Name)
		}
	}
	for _, name := range []string{"review-change", "verify-change", "skill-creator", "ui-adaptation"} {
		if _, err := os.Stat(filepath.Join(skillsRoot, ".system", name, "SKILL.md")); err != nil {
			t.Fatalf("system skill %q: %v", name, err)
		}
	}
}

func TestModifiedLegacySkillIsPreservedAndShadowsSystem(t *testing.T) {
	home := t.TempDir()
	skillsRoot := filepath.Join(home, ".codeagent", "skills")
	legacy := filepath.Join(skillsRoot, "verify-change")
	copyBundledSkill(t, Embedded(), "verify-change", legacy)
	file, err := os.OpenFile(filepath.Join(legacy, "SKILL.md"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\nuser modification\n"); err != nil {
		t.Fatal(err)
	}
	file.Close()

	report, err := InstallWithReport(Embedded(), skillsRoot, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Legacy) != 1 || report.Legacy[0].State != LegacyPreserved {
		t.Fatalf("migration = %#v", report.Legacy)
	}
	if !strings.Contains(report.Legacy[0].Warning, "shadows system skill") {
		t.Fatalf("warning = %q", report.Legacy[0].Warning)
	}
	if _, err := os.Stat(filepath.Join(legacy, "SKILL.md")); err != nil {
		t.Fatal("modified legacy skill was removed")
	}
	if _, err := os.Stat(filepath.Join(skillsRoot, ".system", "verify-change", "SKILL.md")); err != nil {
		t.Fatal("system copy missing")
	}
}

func TestLegacyMigrationIsIdempotentWithMatchingMarker(t *testing.T) {
	home := t.TempDir()
	skillsRoot := filepath.Join(home, ".codeagent", "skills")
	copyBundledSkill(t, Embedded(), "review-change", filepath.Join(skillsRoot, "review-change"))
	if _, err := InstallWithReport(Embedded(), skillsRoot, "test"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(skillsRoot, ".system", MarkerFilename)
	before, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	report, err := InstallWithReport(Embedded(), skillsRoot, "test")
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	if report.Installed || len(report.Legacy) != 0 {
		t.Fatalf("second report = %#v", report)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("matching marker was rewritten")
	}
}

func TestAgentKitInitialLegacyVariantIsMigrated(t *testing.T) {
	home := t.TempDir()
	skillsRoot := filepath.Join(home, ".codeagent", "skills")
	destination := filepath.Join(skillsRoot, "verify-change")
	if err := copyTree("testdata/legacy-agentkit/verify-change", destination); err != nil {
		t.Fatal(err)
	}
	report, err := InstallWithReport(Embedded(), skillsRoot, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Legacy) != 1 || report.Legacy[0].State != LegacyMigrated || report.Legacy[0].Name != "verify-change" {
		t.Fatalf("migration = %#v", report.Legacy)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("AgentKit initial legacy verify-change was not migrated")
	}
}
