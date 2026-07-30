// verify-test
package settings

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveVerifyCommandDefaultsToAuto(t *testing.T) {
	// Create a temp dir with a go.mod so DetectVerify finds "go build ./...".
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test"), 0o644)

	// Empty "" should auto-detect.
	if got := resolveVerifyCommand("", dir); got != "go build ./..." {
		t.Errorf("empty string defaulted to %q, want 'go build ./...'", got)
	}

	// Explicit "auto" should also auto-detect.
	if got := resolveVerifyCommand("auto", dir); got != "go build ./..." {
		t.Errorf("'auto' resolved to %q, want 'go build ./...'", got)
	}

	// "off" should disable.
	if got := resolveVerifyCommand("off", dir); got != "" {
		t.Errorf("'off' resolved to %q, want ''", got)
	}

	// "none" should disable.
	if got := resolveVerifyCommand("none", dir); got != "" {
		t.Errorf("'none' resolved to %q, want ''", got)
	}

	// Explicit command should pass through verbatim.
	if got := resolveVerifyCommand("go test ./...", dir); got != "go test ./..." {
		t.Errorf("explicit command resolved to %q, want 'go test ./...'", got)
	}
}

func TestResolveVerifyCommandNoProjectFiles(t *testing.T) {
	// Empty dir with no project files → DetectVerify returns "".
	dir := t.TempDir()
	if got := resolveVerifyCommand("", dir); got != "" {
		t.Errorf("no project files: got %q, want empty", got)
	}
}
