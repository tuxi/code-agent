package session

import (
	"strings"
	"testing"
)

func TestBuildInjectsSkillsIndex(t *testing.T) {
	idx := "Skills — task-specific playbooks.\n- verify-change: verify then fix the source."
	sess, err := NewBuilder(t.TempDir()).WithSkillsIndex(idx).Build()
	if err != nil {
		t.Fatal(err)
	}
	sys := sess.Messages[0].Content
	if !strings.Contains(sys, "verify-change: verify then fix the source.") {
		t.Errorf("system prompt missing the skills index:\n%s", sys)
	}
}

func TestBuildOmitsSkillsSectionWhenEmpty(t *testing.T) {
	sess, err := NewBuilder(t.TempDir()).Build()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sess.Messages[0].Content, "task-specific playbooks") {
		t.Error("an empty skills index should add no section to the prompt")
	}
}
