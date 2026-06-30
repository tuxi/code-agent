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

func loadRegWithResources(t *testing.T) *skills.Registry {
	t.Helper()
	dir := t.TempDir()
	sd := filepath.Join(dir, "tool-skill")
	for _, d := range []string{sd, filepath.Join(sd, "references"), filepath.Join(sd, "scripts")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const md = "---\nname: tool-skill\nversion: \"1\"\ndescription: skill with resources.\n---\n\nUse the references and scripts.\n"
	if err := os.WriteFile(filepath.Join(sd, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(sd, "references", "guide.md"), []byte("# Guide"), 0o644)
	os.WriteFile(filepath.Join(sd, "scripts", "run.sh"), []byte("#!/bin/bash\necho ok"), 0o755)

	reg, err := skills.Load("", dir)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestLoadSkill_ResourceManifest(t *testing.T) {
	tool := NewLoadSkillTool(loadRegWithResources(t))
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"name":"tool-skill"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Body must be present.
	if !strings.Contains(res.Content, "Use the references and scripts.") {
		t.Errorf("result missing the skill body:\n%s", res.Content)
	}
	// Resource manifest must follow the body.
	if !strings.Contains(res.Content, "Resources available in this skill:") {
		t.Errorf("result missing the resource manifest header:\n%s", res.Content)
	}
	// Both resources must be listed with absolute paths.
	if !strings.Contains(res.Content, "references/guide.md  (reference)") {
		t.Errorf("result missing the reference resource:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "scripts/run.sh  (script)") {
		t.Errorf("result missing the script resource:\n%s", res.Content)
	}
}

func TestLoadSkill_NoResourceManifestWhenEmpty(t *testing.T) {
	// A skill without L3 resources must not show the manifest section.
	tool := NewLoadSkillTool(loadReg(t)) // verify-change has no resources
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"name":"verify-change"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(res.Content, "Resources available") {
		t.Errorf("skill without resources should not have a manifest:\n%s", res.Content)
	}
}

func loadRegLicensed(t *testing.T) *skills.Registry {
	t.Helper()
	dir := t.TempDir()
	sd := filepath.Join(dir, "licensed")
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	const md = "---\nname: licensed\nversion: \"1\"\ndescription: licensed skill.\nlicense: Proprietary\n---\n\nLicensed body.\n"
	if err := os.WriteFile(filepath.Join(sd, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := skills.Load("", dir)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestLoadSkill_LicenseInHeader(t *testing.T) {
	tool := NewLoadSkillTool(loadRegLicensed(t))
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"name":"licensed"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.HasPrefix(res.Content, "Loaded skill: licensed (v1) [Proprietary]") {
		t.Errorf("header missing license:\n%s", res.Content)
	}
}

func TestLoadSkill_NoLicenseBracketWhenEmpty(t *testing.T) {
	tool := NewLoadSkillTool(loadReg(t)) // verify-change has no license
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"name":"verify-change"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(res.Content, "Loaded skill:") && strings.Contains(res.Content, " [") {
		t.Errorf("header should not have empty license bracket:\n%s", res.Content)
	}
}

func TestLoadSkill_ListMode(t *testing.T) {
	dir := t.TempDir()
	// Create two skills so the list has content.
	for name, desc := range map[string]string{"alpha": "first skill", "beta": "second skill"} {
		sd := filepath.Join(dir, name)
		os.MkdirAll(sd, 0o755)
		os.WriteFile(filepath.Join(sd, "SKILL.md"),
			[]byte("---\nname: "+name+"\ndescription: "+desc+"\n---\nbody"), 0o644)
	}
	reg, _ := skills.Load("", dir)
	tool := NewLoadSkillTool(reg)

	res, err := tool.Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"name":""}`))
	if err != nil {
		t.Fatalf("list mode should not error: %v", err)
	}
	if !strings.Contains(res.Content, "Available skills:") {
		t.Errorf("list mode missing header:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "- alpha: first skill") {
		t.Errorf("list mode missing alpha:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "- beta: second skill") {
		t.Errorf("list mode missing beta:\n%s", res.Content)
	}
}

func TestLoadSkill_EmptyList(t *testing.T) {
	reg, _ := skills.Load("", t.TempDir())
	tool := NewLoadSkillTool(reg)
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"name":""}`))
	if err != nil {
		t.Fatalf("empty list should not error: %v", err)
	}
	if !strings.Contains(res.Content, "(no skills loaded)") {
		t.Errorf("empty list message:\n%s", res.Content)
	}
}

func TestLoadSkill_ResourceAccess(t *testing.T) {
	tool := NewLoadSkillTool(loadRegWithResources(t))
	res, err := tool.Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"name":"tool-skill","resource":"references/guide.md"}`))
	if err != nil {
		t.Fatalf("resource access: %v", err)
	}
	if !strings.HasPrefix(res.Content, "Loaded resource: tool-skill/references/guide.md") {
		t.Errorf("resource header wrong:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "# Guide") {
		t.Errorf("resource content missing:\n%s", res.Content)
	}
}

func TestLoadSkill_ResourceNotFound(t *testing.T) {
	tool := NewLoadSkillTool(loadRegWithResources(t))
	_, err := tool.Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"name":"tool-skill","resource":"references/nope.md"}`))
	if err == nil {
		t.Error("expected error for missing resource")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should say 'not found', got: %v", err)
	}
}

func TestLoadSkill_ResourcePathTraversal(t *testing.T) {
	tool := NewLoadSkillTool(loadRegWithResources(t))
	_, err := tool.Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"name":"tool-skill","resource":"../../../etc/passwd"}`))
	if err == nil {
		t.Error("expected error for path traversal")
	} else if !strings.Contains(err.Error(), "escapes") {
		t.Errorf("error should say 'escapes', got: %v", err)
	}
}

func TestLoadSkill_ResourceOnBareFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "flat.md"),
		[]byte("---\nname: flat\ndescription: a flat skill\n---\nBody."), 0o644)
	reg, _ := skills.Load("", dir)
	tool := NewLoadSkillTool(reg)

	_, err := tool.Execute(context.Background(), tools.ExecutionContext{}, json.RawMessage(`{"name":"flat","resource":"references/foo.md"}`))
	if err == nil {
		t.Error("expected error for resource on bare-file skill")
	} else if !strings.Contains(err.Error(), "bare file") {
		t.Errorf("error should mention bare file, got: %v", err)
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
