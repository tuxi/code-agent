package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/model"
)

func TestBuilderInjectsProjectMemory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"),
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
	if !strings.Contains(sys, "<project_instructions") {
		t.Error("expected a <project_instructions> wrapper")
	}
	if !strings.Contains(sys, "AGENTS.md") {
		t.Error("expected a project_instructions path referencing AGENTS.md")
	}
	if !strings.Contains(sys, "<project_context>") {
		t.Error("expected a <project_context> wrapper")
	}
}

func TestBuilderNoMemoryFile(t *testing.T) {
	dir := t.TempDir() // no AGENTS.md or CLAUDE.md
	sess, err := NewBuilder(dir).Build()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sess.Messages[0].Content, "<project_context>") {
		t.Error("should not add project_context when no context files are present")
	}
}

func TestBuilderCLAUDE_mdFallback(t *testing.T) {
	dir := t.TempDir()
	// Only CLAUDE.md exists (not AGENTS.md).
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"),
		[]byte("Claude-specific rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sess, err := NewBuilder(dir).Build()
	if err != nil {
		t.Fatal(err)
	}

	sys := sess.Messages[0].Content
	if !strings.Contains(sys, "Claude-specific rules") {
		t.Errorf("should load CLAUDE.md when AGENTS.md is absent:\n%s", sys)
	}
}

func TestBuilderAGENTS_mdPreferredOverCLAUDE_md(t *testing.T) {
	dir := t.TempDir()
	// Both exist — AGENTS.md should win (higher priority in candidates list).
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"),
		[]byte("AGENTS content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"),
		[]byte("CLAUDE content"), 0o644); err != nil {
		t.Fatal(err)
	}

	sess, err := NewBuilder(dir).Build()
	if err != nil {
		t.Fatal(err)
	}
	sys := sess.Messages[0].Content
	if !strings.Contains(sys, "AGENTS content") {
		t.Errorf("should prefer AGENTS.md over CLAUDE.md:\n%s", sys)
	}
	if strings.Contains(sys, "CLAUDE content") {
		t.Error("should NOT load CLAUDE.md when AGENTS.md is present")
	}
}

func TestBuilderAncestorContextFile(t *testing.T) {
	// Create a nested directory structure:
	//   tmp/
	//     AGENTS.md  (ancestor rules)
	//     sub/
	//       (no context file — should inherit from parent)
	rootDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootDir, "AGENTS.md"),
		[]byte("Root-level rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	subDir := filepath.Join(rootDir, "sub", "deep")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sess, err := NewBuilder(subDir).Build()
	if err != nil {
		t.Fatal(err)
	}
	sys := sess.Messages[0].Content
	if !strings.Contains(sys, "Root-level rules") {
		t.Errorf("should inherit AGENTS.md from ancestor:\n%s", sys)
	}
}

func TestBuilderClosestAncestorWins(t *testing.T) {
	//   tmp/
	//     AGENTS.md  ("root rules")
	//     sub/
	//       AGENTS.md  ("closer rules")
	//       deep/       (workspace here)
	rootDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootDir, "AGENTS.md"),
		[]byte("root rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	subDir := filepath.Join(rootDir, "sub")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "AGENTS.md"),
		[]byte("closer rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	deepDir := filepath.Join(subDir, "deep")
	if err := os.MkdirAll(deepDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sess, err := NewBuilder(deepDir).Build()
	if err != nil {
		t.Fatal(err)
	}
	sys := sess.Messages[0].Content
	if !strings.Contains(sys, "closer rules") {
		t.Errorf("should load closest ancestor's AGENTS.md:\n%s", sys)
	}
	if !strings.Contains(sys, "root rules") {
		t.Errorf("should also load root ancestor's AGENTS.md:\n%s", sys)
	}
	// "closer rules" should appear before "root rules" in the prompt.
	closerIdx := strings.Index(sys, "closer rules")
	rootIdx := strings.Index(sys, "root rules")
	if closerIdx < 0 || rootIdx < 0 || closerIdx > rootIdx {
		t.Error("closer ancestor's rules should appear before root's rules")
	}
}

func TestBuilderNoContextFilesFlag(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"),
		[]byte("should not appear"), 0o644); err != nil {
		t.Fatal(err)
	}

	sess, err := NewBuilder(dir).WithNoContextFiles(true).Build()
	if err != nil {
		t.Fatal(err)
	}
	sys := sess.Messages[0].Content
	if strings.Contains(sys, "should not appear") {
		t.Error("NoContextFiles=true should skip all context file loading")
	}
	if strings.Contains(sys, "<project_context>") {
		t.Error("NoContextFiles=true should not emit <project_context>")
	}
}

func TestBuilderGlobalContextFile(t *testing.T) {
	// Simulate a global context file by creating a .codeagent dir under a fake home.
	fakeHome := t.TempDir()
	codeagentDir := filepath.Join(fakeHome, ".codeagent")
	if err := os.MkdirAll(codeagentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codeagentDir, "AGENTS.md"),
		[]byte("global rules"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Override HOME for this test.
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", fakeHome)
	defer os.Setenv("HOME", origHome)

	workspace := t.TempDir() // no local AGENTS.md
	sess, err := NewBuilder(workspace).Build()
	if err != nil {
		t.Fatal(err)
	}
	sys := sess.Messages[0].Content
	if !strings.Contains(sys, "global rules") {
		t.Errorf("should load global AGENTS.md from ~/.codeagent/:\n%s", sys)
	}
}

func TestBuilderGlobalAndLocalDeduplication(t *testing.T) {
	// When a context file in ~/.codeagent/ and one in the workspace are different
	// paths, both should load. If they happen to be the same path (unlikely), only
	// one should appear.
	fakeHome := t.TempDir()
	codeagentDir := filepath.Join(fakeHome, ".codeagent")
	if err := os.MkdirAll(codeagentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codeagentDir, "AGENTS.md"),
		[]byte("global rules"), 0o644); err != nil {
		t.Fatal(err)
	}

	origHome := os.Getenv("HOME")
	os.Setenv("HOME", fakeHome)
	defer os.Setenv("HOME", origHome)

	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"),
		[]byte("local rules"), 0o644); err != nil {
		t.Fatal(err)
	}

	sess, err := NewBuilder(workspace).Build()
	if err != nil {
		t.Fatal(err)
	}
	sys := sess.Messages[0].Content
	if !strings.Contains(sys, "global rules") {
		t.Error("should load global AGENTS.md")
	}
	if !strings.Contains(sys, "local rules") {
		t.Error("should load local AGENTS.md")
	}
	// global comes first (loaded first), local follows (closest ancestor).
	gIdx := strings.Index(sys, "global rules")
	lIdx := strings.Index(sys, "local rules")
	if gIdx < 0 || lIdx < 0 || gIdx > lIdx {
		t.Error("global rules should appear before local rules")
	}
}

func TestBuilderWorktreeShadow_NestedWorktree(t *testing.T) {
	mainRepo := t.TempDir()

	// Main repo's AGENTS.md.
	if err := os.WriteFile(filepath.Join(mainRepo, "AGENTS.md"),
		[]byte("main repo rules"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Main repo's .git directory.
	mainGit := filepath.Join(mainRepo, ".git")
	if err := os.MkdirAll(mainGit, 0o755); err != nil {
		t.Fatal(err)
	}

	// Worktree's gitdir (simulates .git/worktrees/feat).
	worktreeGitDir := filepath.Join(mainGit, "worktrees", "feat")
	if err := os.MkdirAll(worktreeGitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// commondir points two levels up to mainGit.
	commondirRel := filepath.Join("..", "..")
	if err := os.WriteFile(filepath.Join(worktreeGitDir, "commondir"),
		[]byte(commondirRel), 0o644); err != nil {
		t.Fatal(err)
	}

	// The linked worktree, nested under mainRepo.
	worktreeRoot := filepath.Join(mainRepo, "worktrees", "feat")
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// .git is a file → linked worktree marker.
	gitFile := fmt.Sprintf("gitdir: %s", worktreeGitDir)
	if err := os.WriteFile(filepath.Join(worktreeRoot, ".git"),
		[]byte(gitFile), 0o644); err != nil {
		t.Fatal(err)
	}

	sess, err := NewBuilder(worktreeRoot).Build()
	if err != nil {
		t.Fatal(err)
	}
	sys := sess.Messages[0].Content
	if strings.Contains(sys, "main repo rules") {
		t.Errorf("should NOT load shadowed main repo AGENTS.md in nested worktree:\n%s", sys)
	}
}

func TestBuilderWorktreeShadow_SiblingWorktree(t *testing.T) {
	mainRepo := t.TempDir()

	if err := os.WriteFile(filepath.Join(mainRepo, "AGENTS.md"),
		[]byte("main rules"), 0o644); err != nil {
		t.Fatal(err)
	}

	mainGit := filepath.Join(mainRepo, ".git")
	if err := os.MkdirAll(mainGit, 0o755); err != nil {
		t.Fatal(err)
	}

	// Sibling worktree (not nested under mainRepo).
	siblingWorktree := filepath.Join(t.TempDir(), "feat")
	if err := os.MkdirAll(siblingWorktree, 0o755); err != nil {
		t.Fatal(err)
	}
	worktreeGitDir := filepath.Join(mainGit, "worktrees", "feat-sibling")
	if err := os.MkdirAll(worktreeGitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	commondirRel := filepath.Join("..", "..")
	if err := os.WriteFile(filepath.Join(worktreeGitDir, "commondir"),
		[]byte(commondirRel), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(siblingWorktree, ".git"),
		[]byte(fmt.Sprintf("gitdir: %s", worktreeGitDir)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(siblingWorktree, "AGENTS.md"),
		[]byte("sibling rules"), 0o644); err != nil {
		t.Fatal(err)
	}

	sess, err := NewBuilder(siblingWorktree).Build()
	if err != nil {
		t.Fatal(err)
	}
	sys := sess.Messages[0].Content
	if !strings.Contains(sys, "sibling rules") {
		t.Errorf("should load worktree's own AGENTS.md:\n%s", sys)
	}
	if strings.Contains(sys, "main rules") {
		t.Errorf("sibling worktree should NOT find main repo's AGENTS.md (not an ancestor):\n%s", sys)
	}
}

func TestBuilderWorktreeShadow_NormalRepo(t *testing.T) {
	repo := t.TempDir()

	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"),
		[]byte("normal repo rules"), 0o644); err != nil {
		t.Fatal(err)
	}

	sess, err := NewBuilder(repo).Build()
	if err != nil {
		t.Fatal(err)
	}
	sys := sess.Messages[0].Content
	if !strings.Contains(sys, "normal repo rules") {
		t.Errorf("should load AGENTS.md in normal repo:\n%s", sys)
	}
}
