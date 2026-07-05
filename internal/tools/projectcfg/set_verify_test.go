package projectcfg

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/settings"
	"code-agent/internal/tools"
)

func run(t *testing.T, root, input string) tools.ToolResult {
	t.Helper()
	res, err := NewSetVerifyCommandTool().Execute(context.Background(),
		tools.ExecutionContext{WorkspaceRoot: root}, json.RawMessage(input))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

// An explicit command is persisted and later resolved by the settings layer.
func TestSetVerifyExplicit(t *testing.T) {
	root := t.TempDir()
	res := run(t, root, `{"command":"go test ./..."}`)
	if !strings.Contains(res.Content, "go test ./...") {
		t.Errorf("result should confirm the command: %q", res.Content)
	}
	if got := settings.ResolveVerify(root, t.TempDir(), ""); got != "go test ./..." {
		t.Errorf("resolved = %q, want %q", got, "go test ./...")
	}
	// The machine-local file is gitignored.
	gi, _ := os.ReadFile(filepath.Join(root, ".gitignore"))
	if !strings.Contains(string(gi), ".codeagent/settings.local.json") {
		t.Errorf(".gitignore missing the settings.local.json entry: %q", string(gi))
	}
}

// Omitted command → detected from the project (go.mod → go build).
func TestSetVerifyAutoDetects(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := run(t, root, `{}`)
	if !strings.Contains(res.Content, "go build ./...") {
		t.Errorf("auto-detect should pick go build: %q", res.Content)
	}
	if got := settings.ResolveVerify(root, t.TempDir(), ""); got != "go build ./..." {
		t.Errorf("resolved = %q, want %q", got, "go build ./...")
	}
}

// No recognizable build system → a helpful message, nothing written.
func TestSetVerifyNoBuildSystem(t *testing.T) {
	root := t.TempDir()
	res := run(t, root, `{"command":"auto"}`)
	if !strings.Contains(res.Content, "No recognizable build system") {
		t.Errorf("expected a not-found message, got %q", res.Content)
	}
	if _, err := os.Stat(settings.ProjectLocalPath(root)); !os.IsNotExist(err) {
		t.Errorf("nothing should have been written, but the settings file exists")
	}
}

// The tool declares side effects (writes a file), so it is gated like other writes.
func TestSetVerifyIsSideEffecting(t *testing.T) {
	if !tools.HasSideEffects(NewSetVerifyCommandTool()) {
		t.Error("set_verify_command should declare side effects")
	}
}
