package tui

import (
	"code-agent/internal/session"
	"path/filepath"
	"testing"
)

func TestFilterSessionsByWorkspaceKeepsOnlyCurrentWorkspace(t *testing.T) {
	root := filepath.Clean("/home/user/work/repo-a")
	metas := []session.Meta{
		{ID: "own", WorkspacePath: "/home/user/work/repo-a"},
		{ID: "own-trailing-slash", WorkspacePath: "/home/user/work/repo-a/"},
		{ID: "other", WorkspacePath: "/home/user/work/repo-b"},
		{ID: "legacy-no-path", WorkspacePath: ""},
	}
	got := filterSessionsByWorkspace(metas, root)
	if len(got) != 2 {
		t.Fatalf("filtered to %d sessions, want 2 (own + own-trailing-slash): %+v", len(got), got)
	}
	for _, m := range got {
		if m.ID == "other" || m.ID == "legacy-no-path" {
			t.Fatalf("session %q must be excluded, got %+v", m.ID, got)
		}
	}
}

func TestFilterSessionsByWorkspaceEmptyRootIsNoop(t *testing.T) {
	metas := []session.Meta{{ID: "a", WorkspacePath: "/x"}}
	if got := filterSessionsByWorkspace(metas, ""); len(got) != 1 {
		t.Fatalf("empty root must not filter, got %d sessions", len(got))
	}
}
