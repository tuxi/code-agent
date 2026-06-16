package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/model"
)

func TestBuilderInjectsProjectMemory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CODEAGENT.md"),
		[]byte("Project: DreamAI\nBackend: Go\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sess, err := NewBuilder(dir).Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Messages) != 1 || sess.Messages[0].Role != model.RoleSystem {
		t.Fatalf("expected a single system message, got %+v", sess.Messages)
	}

	sys := sess.Messages[0].Content
	if !strings.Contains(sys, "Project: DreamAI") {
		t.Errorf("system message is missing the project memory:\n%s", sys)
	}
	if !strings.Contains(sys, "CODEAGENT.md") {
		t.Error("expected a project-memory header referencing CODEAGENT.md")
	}
}

func TestBuilderNoMemoryFile(t *testing.T) {
	dir := t.TempDir() // no CODEAGENT.md
	sess, err := NewBuilder(dir).Build()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sess.Messages[0].Content, "Project memory") {
		t.Error("should not add a project-memory header when CODEAGENT.md is absent")
	}
}
