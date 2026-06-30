package skill

import (
	"code-agent/internal/skills"
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadReg(t *testing.T) *skills.Registry {
	t.Helper()
	dir := t.TempDir()
	sd := filepath.Join(dir, "verify-change")
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	const md = "---\nname: verify-change\nversion: \"1\"\ndescription: verify then fix the source.\n---\n\nFix the source, not the test.\n"
	if err := os.WriteFile(filepath.Join(sd, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := skills.Load("", dir)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestLoadSkillExecute(t *testing.T) {
	tool := NewLoadSkillTool(loadReg(t))
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"name":"verify-change"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.HasPrefix(res.Content, "Loaded skill: verify-change (v1)") {
		t.Errorf("result missing the versioned header:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "Fix the source, not the test.") {
		t.Errorf("result missing the skill body:\n%s", res.Content)
	}
}

func TestLoadSkillUnknown(t *testing.T) {
	tool := NewLoadSkillTool(loadReg(t))
	_, err := tool.Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"name":"nope"}`))
	if err == nil {
		t.Error("expected an error for an unknown skill")
	} else if !strings.Contains(err.Error(), "verify-change") {
		t.Errorf("error should list available skills, got: %v", err)
	}
}

func TestAnnounceSkill(t *testing.T) {
	tool := NewLoadSkillTool(loadReg(t))
	name, ver, ok := tool.AnnounceSkill(json.RawMessage(`{"name":"verify-change"}`))
	if !ok || name != "verify-change" || ver != "1" {
		t.Errorf("AnnounceSkill = (%q, %q, %v), want (verify-change, 1, true)", name, ver, ok)
	}
	if _, _, ok := tool.AnnounceSkill(json.RawMessage(`{"name":"nope"}`)); ok {
		t.Error("AnnounceSkill should report false for an unknown skill")
	}
}
