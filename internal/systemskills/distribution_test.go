package systemskills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/skills"
	"code-agent/internal/tools"
	skilltool "code-agent/internal/tools/skill"
)

func TestEmbeddedBundleMaterializesAndDiscoversUIAdaptation(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir() // deliberately contains no skills/ directory
	if evidenceWorkspace := os.Getenv("CODEAGENT_SYSTEM_SKILL_WORKSPACE"); evidenceWorkspace != "" {
		workspace = evidenceWorkspace
	}
	if err := InstallForHome(home, Embedded(), "test:codeagentd"); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(home, ".codeagent", "skills")
	registry, err := skills.LoadScoped(skills.WorkspaceLoadOptions(global, workspace))
	if err != nil {
		t.Fatal(err)
	}
	skill, ok := registry.Get("ui-adaptation")
	if !ok {
		t.Fatalf("ui-adaptation not discovered; skipped=%v", registry.Skipped)
	}
	if skill.Scope != skills.ScopeSystem {
		t.Fatalf("scope = %q, want system", skill.Scope)
	}
	if skill.ContentHash == "" || skill.Path == "" || skill.Body == "" {
		t.Fatalf("incomplete loaded skill: %#v", skill)
	}
	if _, err := os.Stat(filepath.Join(global, ".system", "ui-adaptation", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	load := skilltool.NewLoadSkillToolWithOptions(registry, skills.WorkspaceLoadOptions(global, workspace))
	result, err := load.Execute(t.Context(), tools.ExecutionContext{WorkspaceRoot: workspace}, []byte(`{"name":"ui-adaptation"}`))
	if err != nil {
		t.Fatalf("load_skill: %v", err)
	}
	if result.Content == "" || !strings.Contains(result.Content, "Loaded skill: ui-adaptation") {
		t.Fatalf("unexpected load_skill result: %q", result.Content)
	}
}
