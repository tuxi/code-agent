package previewmatrix

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/preview/matrix"
	"code-agent/internal/tools"
)

// TestExecuteSanitizesLayoutUnavailableErrors covers the model-callable surface:
// every layout-unavailable condition collapses to the stable public message,
// with no filesystem detail or absolute path in Content or Failure.
func TestExecuteSanitizesLayoutUnavailableErrors(t *testing.T) {
	t.Run("nonexistent revision", func(t *testing.T) {
		fx := newFixture(t, nil, nil)
		input := fx.baseInput("matrix_p1g_tool_missing")
		input["layout"].(map[string]any)["artifact_id"] = "layout_missing"

		result := execute(t, fx, input)
		requireSanitizedLayoutFailure(t, fx, result, matrix.PublicLayoutUnavailableMessage)
	})

	t.Run("metadata path error", func(t *testing.T) {
		fx := newFixture(t, nil, nil)
		metadataPath := filepath.Join(fx.Root, ".codeagent", "assets", "ui-layout", fx.LayoutID, "revision-000001", "metadata.json")
		if err := os.Remove(metadataPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(metadataPath, 0o700); err != nil {
			t.Fatal(err)
		}

		result := execute(t, fx, fx.baseInput("matrix_p1g_tool_regular"))
		requireSanitizedLayoutFailure(t, fx, result, matrix.PublicLayoutUnavailableMessage)
	})

	t.Run("revision symlink escapes the workspace", func(t *testing.T) {
		fx := newFixture(t, nil, nil)
		artifactDir := filepath.Join(fx.Root, ".codeagent", "assets", "ui-layout", "layout_tool_escape")
		if err := os.MkdirAll(artifactDir, 0o700); err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "metadata.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(artifactDir, "revision-000001")); err != nil {
			t.Fatal(err)
		}
		input := fx.baseInput("matrix_p1g_tool_escape")
		input["layout"].(map[string]any)["artifact_id"] = "layout_tool_escape"

		result := execute(t, fx, input)
		requireSanitizedLayoutFailure(t, fx, result, matrix.PublicLayoutUnavailableMessage)
	})
}

func requireSanitizedLayoutFailure(t *testing.T, fx fixture, result tools.ToolResult, wantMessage string) {
	t.Helper()
	if result.Failure == nil {
		t.Fatal("expected a typed tool failure")
	}
	if result.Failure.Kind != "not_found" {
		t.Fatalf("failure kind = %q, want not_found", result.Failure.Kind)
	}
	if result.Failure.Message != wantMessage || result.Content != wantMessage {
		t.Fatalf("message = %q / content = %q, want %q", result.Failure.Message, result.Content, wantMessage)
	}
	for _, payload := range []string{result.Content, result.Failure.Message, string(result.Output)} {
		for _, needle := range []string{fx.Root, fx.StoreBase, ".codeagent", ".codeagent/assets", "lstat ", "revision-000001", "/var/folders/", "/private/"} {
			if needle != "" && strings.Contains(payload, needle) {
				t.Fatalf("model-visible payload leaks %q: %q", needle, payload)
			}
		}
	}
	if len(result.Output) != 0 {
		t.Fatalf("request error must not carry structured output: %s", result.Output)
	}
}
