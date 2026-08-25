package skills

import (
	"path/filepath"
	"testing"
)

// TestLoad_AutomationSkill verifies the repo's skills/automation/SKILL.md parses
// and appears in the index (T5 acceptance).
func TestLoad_AutomationSkill(t *testing.T) {
	dir := filepath.Join("..", "..", "skills")
	r, err := Load("", dir)
	if err != nil {
		t.Fatalf("load skills dir: %v", err)
	}
	skill, ok := r.Get("automation")
	if !ok {
		t.Fatalf("automation skill not found; skipped=%v", r.Skipped)
	}
	if skill.Meta.Description == "" {
		t.Fatal("automation skill has empty description")
	}
	if len(skill.Body) < 100 {
		t.Fatalf("automation skill body too short: %d chars", len(skill.Body))
	}
	// It must appear in the prompt index.
	idx := r.PromptIndex()
	if !containsStr(idx, "automation") {
		t.Fatal("automation skill missing from PromptIndex")
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
