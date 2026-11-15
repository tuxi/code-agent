package server

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func changeGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=Rail", "-c", "user.email=rail@example.invalid", "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = root
	out, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("git %v: %v %s", args, e, out)
	}
	return string(out)
}
func changeFile(t *testing.T, root, path, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
func changeRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	changeGit(t, root, "init")
	return root
}

func TestRailChangesNetHeadBinaryUntrackedAndNames(t *testing.T) {
	root := changeRepo(t)
	for path, body := range map[string]string{"text.txt": "a\nb\n", "binary.dat": "\x00abc", "old.txt": "rename\n", "tab\t名\n.txt": "old\n"} {
		changeFile(t, root, path, body)
	}
	changeGit(t, root, "add", ".")
	changeGit(t, root, "commit", "-m", "initial")
	changeFile(t, root, "text.txt", "a\nB\n")
	changeGit(t, root, "add", "text.txt")
	changeFile(t, root, "text.txt", "a\nC\nd\n")
	changeFile(t, root, "binary.dat", "\x00xyz")
	changeFile(t, root, "tab\t名\n.txt", "new\n")
	changeFile(t, root, "input.txt", "not yet tracked\n")
	changeGit(t, root, "mv", "old.txt", "new.txt")
	data, err := readRailChanges(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !data.IsGitRepository || data.Baseline != "HEAD" || data.FileCount != 5 || data.Added != 3 || data.Removed != 2 || data.UncountedFiles != 2 {
		t.Fatalf("net stats: %+v", data)
	}
	byPath := map[string]railChangedFile{}
	for _, f := range data.Files {
		byPath[f.Path] = f
	}
	if f := byPath["text.txt"]; f.Added == nil || *f.Added != 2 || *f.Removed != 1 {
		t.Fatalf("staged/unstaged summed twice: %+v", f)
	}
	if f := byPath["binary.dat"]; !f.Binary || f.Added != nil || f.Removed != nil {
		t.Fatalf("binary fabricated zeros: %+v", f)
	}
	if f := byPath["input.txt"]; f.Status != "untracked" || f.Added != nil {
		t.Fatalf("untracked miscounted: %+v", f)
	}
	if f := byPath["new.txt"]; f.Status != "renamed" || f.PreviousPath != "old.txt" || f.Added == nil || *f.Added != 0 {
		t.Fatalf("rename lost: %+v", f)
	}
	if _, ok := byPath["tab\t名\n.txt"]; !ok {
		t.Fatal("special path corrupted")
	}
}

func TestRailChangesUnbornNonGitAndNestedScope(t *testing.T) {
	root := changeRepo(t)
	changeFile(t, root, "first.txt", "first\n")
	changeGit(t, root, "add", "first.txt")
	data, err := readRailChanges(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if data.Head.Kind != "unborn" || data.FileCount != 1 || data.Added != 1 {
		t.Fatalf("unborn: %+v", data)
	}
	clean := t.TempDir()
	data, err = readRailChanges(context.Background(), clean)
	if err != nil || data.IsGitRepository || data.Head.Kind != "not_git" {
		t.Fatalf("non-git: %+v %v", data, err)
	}
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = readRailChanges(context.Background(), nested); err == nil || err.Error() != "workspace_not_git_root" {
		t.Fatal("nested project exposed parent repository", err)
	}
}

func TestRailChangesImmutableSnapshotScopeExpiryAndAuth(t *testing.T) {
	root := changeRepo(t)
	changeFile(t, root, "a.txt", "old\n")
	changeGit(t, root, "add", ".")
	changeGit(t, root, "commit", "-m", "initial")
	changeFile(t, root, "a.txt", "new\n")
	store := newRailChangesStore()
	data, err := store.capture(context.Background(), "session-a", root)
	if err != nil {
		t.Fatal(err)
	}
	if data.ObservedAt == "" {
		t.Fatal("missing observation time")
	}
	file := data.Files[0]
	changeFile(t, root, "a.txt", "later\n")
	patch, state, ok := store.patch("session-a", root, data.SnapshotID, file.ID)
	if !ok || state != "ready" || !strings.Contains(patch, "+new") || strings.Contains(patch, "+later") {
		t.Fatal("snapshot silently read current file", patch)
	}
	if _, _, ok = store.patch("session-b", root, data.SnapshotID, file.ID); ok {
		t.Fatal("snapshot leaked across conversations")
	}
	if _, _, ok = store.patch("session-a", t.TempDir(), data.SnapshotID, file.ID); ok {
		t.Fatal("snapshot survived workspace rebind")
	}
	store.mu.Lock()
	entry := store.entries["session-a"]
	entry.at = time.Now().Add(-6 * time.Minute)
	store.entries["session-a"] = entry
	store.mu.Unlock()
	if _, _, ok = store.patch("session-a", root, data.SnapshotID, file.ID); ok {
		t.Fatal("expired snapshot returned content")
	}
	repo := newFakeConversationRepo()
	sess, _ := repo.Create(context.Background(), root, "")
	h := NewMux(repo, &fakeEventStore{}, nil, MuxOptions{})
	w := jobRequest(h, "GET", "/v1/conversations/"+sess.ID+"/changes?workspace_path=/", " ")
	var envelope struct {
		Data railChanges `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || envelope.Data.FileCount != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = jobRequest(h, "GET", "/v1/conversations/"+sess.ID+"/changes/missing/unknown", ""); w.Code != 409 {
		t.Fatal(w.Code)
	}
	token := strings.Repeat("a", 32)
	secured := NewMux(repo, &fakeEventStore{}, nil, MuxOptions{ServerAuth: ServerAuth{Enabled: true, Token: token}})
	if w = jobRequest(secured, "GET", "/v1/conversations/"+sess.ID+"/changes", ""); w.Code != 401 {
		t.Fatal("unprotected changes")
	}
}

func TestRailChangesConflictIsNotCountedAsOrdinaryLines(t *testing.T) {
	root := changeRepo(t)
	changeFile(t, root, "a.txt", "base\n")
	changeGit(t, root, "add", ".")
	changeGit(t, root, "commit", "-m", "base")
	main := strings.TrimSpace(changeGit(t, root, "branch", "--show-current"))
	changeGit(t, root, "checkout", "-b", "other")
	changeFile(t, root, "a.txt", "other\n")
	changeGit(t, root, "commit", "-am", "other")
	changeGit(t, root, "checkout", main)
	changeFile(t, root, "a.txt", "main\n")
	changeGit(t, root, "commit", "-am", "main")
	cmd := exec.Command("git", "-c", "user.name=Rail", "-c", "user.email=rail@example.invalid", "-c", "core.hooksPath=/dev/null", "merge", "other")
	cmd.Dir = root
	if err := cmd.Run(); err == nil {
		t.Fatal("fixture did not create a conflict")
	}
	data, err := readRailChanges(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if data.FileCount != 1 || data.UncountedFiles != 1 || !data.Files[0].Conflict || data.Files[0].Added != nil || data.Files[0].PatchState != "conflict" {
		t.Fatalf("conflict miscounted: %+v", data)
	}
}

func TestRailChangesSubmoduleHasUnknownLineCount(t *testing.T) {
	module := changeRepo(t)
	changeFile(t, module, "a.txt", "first\n")
	changeGit(t, module, "add", ".")
	changeGit(t, module, "commit", "-m", "first")
	first := strings.TrimSpace(changeGit(t, module, "rev-parse", "HEAD"))
	changeFile(t, module, "a.txt", "second\n")
	changeGit(t, module, "commit", "-am", "second")
	root := changeRepo(t)
	changeGit(t, root, "-c", "protocol.file.allow=always", "submodule", "add", module, "module")
	changeGit(t, root, "commit", "-am", "module")
	changeGit(t, filepath.Join(root, "module"), "checkout", first)
	data, err := readRailChanges(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if data.FileCount != 1 || data.UncountedFiles != 1 || !data.Files[0].Submodule || data.Files[0].Added != nil || data.Files[0].PatchState != "submodule" {
		t.Fatalf("submodule counted as source lines: %+v", data)
	}
}
