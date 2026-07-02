package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
)

// initRemoteRepo creates a git repository with an initial commit and returns
// its path. Use file:// URLs to reference it as a remote in tests.
func initRemoteRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# remote\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("initial commit", &gogit.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// commitFile creates or overwrites a file and commits it in the repo at dir.
func commitFile(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add(name); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit(msg, &gogit.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestGitPull_NotARepo(t *testing.T) {
	dir := t.TempDir()
	out := runTool(t, NewGitPullTool(), dir, gitPullInput{})
	if !strings.Contains(out, "Not a git repository") {
		t.Errorf("expected 'Not a git repository', got %q", out)
	}
}

func TestGitPull_AlreadyUpToDate(t *testing.T) {
	remoteDir := initRemoteRepo(t)
	workspace := t.TempDir()

	// Clone the remote into the workspace so it has the remote configured.
	_, err := gogit.PlainClone(workspace, false, &gogit.CloneOptions{URL: "file://" + remoteDir})
	if err != nil {
		t.Fatal(err)
	}

	// Pull immediately — should be already up-to-date.
	out := runTool(t, NewGitPullTool(), workspace, gitPullInput{})
	if !strings.Contains(out, "Already up-to-date") {
		t.Errorf("expected 'Already up-to-date', got %q", out)
	}
}

func TestGitPull_PullsNewCommits(t *testing.T) {
	remoteDir := initRemoteRepo(t)
	workspace := t.TempDir()

	// Clone the remote.
	_, err := gogit.PlainClone(workspace, false, &gogit.CloneOptions{URL: "file://" + remoteDir})
	if err != nil {
		t.Fatal(err)
	}

	// Add a new commit to the remote.
	commitFile(t, remoteDir, "new.txt", "hello\n", "add new.txt")

	// Pull into the workspace.
	out := runTool(t, NewGitPullTool(), workspace, gitPullInput{})
	if !strings.Contains(out, "Pulled ") {
		t.Errorf("expected 'Pulled successfully', got %q", out)
	}

	// The new file should now exist in the workspace.
	if _, err := os.Stat(filepath.Join(workspace, "new.txt")); err != nil {
		t.Errorf("new.txt not found after pull: %v", err)
	}
}

func TestGitPull_SpecificBranch(t *testing.T) {
	remoteDir := initRemoteRepo(t)
	workspace := t.TempDir()

	// Clone the remote.
	_, err := gogit.PlainClone(workspace, false, &gogit.CloneOptions{URL: "file://" + remoteDir})
	if err != nil {
		t.Fatal(err)
	}

	// Create a branch in the remote with a unique file.
	remoteRepo, err := gogit.PlainOpen(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	remoteWt, err := remoteRepo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	// Create and switch to a new branch.
	headRef, err := remoteRepo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if err := remoteWt.Checkout(&gogit.CheckoutOptions{
		Create: true,
		Branch: plumbing.NewBranchReferenceName("feature"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remoteDir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := remoteWt.Add("feature.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := remoteWt.Commit("add feature.txt", &gogit.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	// Switch remote back to main so clone's default branch stays consistent.
	if err := remoteWt.Checkout(&gogit.CheckoutOptions{
		Branch: headRef.Name(),
	}); err != nil {
		t.Fatal(err)
	}

	// Pull the feature branch.
	out := runTool(t, NewGitPullTool(), workspace, gitPullInput{Branch: "feature"})
	if !strings.Contains(out, "Pulled ") {
		t.Errorf("expected 'Pulled successfully', got %q", out)
	}

	// The feature file should now exist in the workspace.
	if _, err := os.Stat(filepath.Join(workspace, "feature.txt")); err != nil {
		t.Errorf("feature.txt not found after pulling feature branch: %v", err)
	}
}

func TestGitPull_CustomRemote(t *testing.T) {
	remoteDir := initRemoteRepo(t)
	workspace := t.TempDir()

	// Clone the remote.
	_, err := gogit.PlainClone(workspace, false, &gogit.CloneOptions{URL: "file://" + remoteDir})
	if err != nil {
		t.Fatal(err)
	}

	// Create a second remote repo with different content.
	remote2 := initRemoteRepo(t)
	commitFile(t, remote2, "from-upstream.txt", "upstream\n", "add from-upstream.txt")

	// Add it as a second remote.
	repo, err := gogit.PlainOpen(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{
		Name: "upstream",
		URLs: []string{"file://" + remote2},
	}); err != nil {
		t.Fatal(err)
	}

	// Pull from the custom remote.
	out := runTool(t, NewGitPullTool(), workspace, gitPullInput{Remote: "upstream"})
	if !strings.Contains(out, "Pulled ") {
		t.Errorf("expected 'Pulled successfully', got %q", out)
	}

	// The file from the second remote should now exist.
	if _, err := os.Stat(filepath.Join(workspace, "from-upstream.txt")); err != nil {
		t.Errorf("from-upstream.txt not found after pulling from upstream: %v", err)
	}
}
