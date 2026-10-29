package collab

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func snapshotFixture(t *testing.T) (WorkspaceAnchor, ReviewPolicy) {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatal(string(out), err)
		}
	}
	git("-c", "init.defaultBranch=main", "init", "--template=")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "--", "a.txt")
	git("-c", "user.name=Offline", "-c", "user.email=offline@example.invalid", "-c", "core.hooksPath=/dev/null", "commit", "-m", "baseline")
	a, err := AnchorWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	return a, ReviewPolicy{Files: []string{"a.txt", "b.txt", "new.bin"}, ValidationCommands: []string{"check"}}
}

func TestReviewPolicyRejectsProtectedAndEscapingPaths(t *testing.T) {
	for _, path := range []string{"../outside", "/absolute", "a/../../escape", ".env.test", "sub/secrets", "private.key", ".git/config", ".GIT/config", ".codeagent/config", "user/private.txt"} {
		if err := ValidateReviewPolicy(ReviewPolicy{Files: []string{path}, ValidationCommands: []string{"check"}, ProtectedPaths: []string{"private.txt"}}); err == nil {
			t.Fatal("allowed", path)
		}
	}
	if err := ValidateReviewPolicy(ReviewPolicy{Files: []string{"safe", "safe"}, ValidationCommands: []string{"check"}}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestReviewSnapshotRetainsRenameDeletionModeAndBinary(t *testing.T) {
	a, p := snapshotFixture(t)
	before, err := CaptureWorkspace(context.Background(), a, p)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(filepath.Join(a.Root, "a.txt"), filepath.Join(a.Root, "b.txt")); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(filepath.Join(a.Root, "b.txt"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(a.Root, "new.bin"), []byte{0, 1, 2, 255}, 0600); err != nil {
		t.Fatal(err)
	}
	after, err := CaptureWorkspace(context.Background(), a, p)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := ImagePatch(before.Files, after.Files)
	if err != nil || !strings.Contains(patch, "/dev/null") || !strings.Contains(patch, "Binary content differs") {
		t.Fatal(patch, err)
	}
	if after.Files[0].Exists || !after.Files[1].Exists || after.Files[1].Mode != 0700 || string(after.Files[2].Data) != string([]byte{0, 1, 2, 255}) || string(before.Index[0].Data) != string(after.Index[0].Data) {
		t.Fatal(after)
	}
}

func TestReviewSnapshotFailsClosedOnSymlinkOutsideScopeAndOversize(t *testing.T) {
	for _, tc := range []string{"symlink", "outside", "oversize"} {
		t.Run(tc, func(t *testing.T) {
			a, p := snapshotFixture(t)
			switch tc {
			case "symlink":
				if err := os.Symlink(filepath.Join(t.TempDir(), "secret"), filepath.Join(a.Root, "b.txt")); err != nil {
					t.Fatal(err)
				}
			case "outside":
				if err := os.WriteFile(filepath.Join(a.Root, "outside.txt"), []byte("not declared"), 0600); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if err := os.WriteFile(filepath.Join(a.Root, "a.txt"), make([]byte, maxFileBytes+1), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := CaptureWorkspace(context.Background(), a, p); err == nil {
				t.Fatal("unsafe candidate captured")
			}
		})
	}
}
