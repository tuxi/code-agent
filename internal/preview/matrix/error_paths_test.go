package matrix

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLayoutUnavailableErrorsKeepDiagnosticsInternal verifies that the backend
// never places a filesystem path in the model-safe Error() text, while the
// runtime-only diagnostic still carries the failing path for logs.
func TestLayoutUnavailableErrorsKeepDiagnosticsInternal(t *testing.T) {
	t.Run("nonexistent revision", func(t *testing.T) {
		ws := newWorkspace(t, workspaceOptions{})
		_, builder := newTestStore(t)
		request := buildRequest(ws, "matrix_p1g_path_missing", nil)
		request.Layout = LayoutRef{ArtifactID: "layout_missing", Revision: 1}

		_, err := builder.Build(context.Background(), request)
		matrixErr := requireLayoutUnavailable(t, err, "layout revision metadata is unavailable")
		assertModelSafe(t, matrixErr.Error(), ws.Root)
		if diag := matrixErr.Diagnostic(); !strings.Contains(diag, ws.Root) || !strings.Contains(diag, "metadata.json") {
			t.Fatalf("diagnostic should retain the failing path, got %q", diag)
		}
		if diag := Diagnostic(err); diag != matrixErr.Diagnostic() {
			t.Fatalf("Diagnostic(err) = %q, want %q", diag, matrixErr.Diagnostic())
		}
	})

	t.Run("metadata is not a regular file", func(t *testing.T) {
		ws := newWorkspace(t, workspaceOptions{})
		_, builder := newTestStore(t)
		metadataPath := filepath.Join(ws.Root, ".codeagent", "assets", "ui-layout", "layout_p1g", "revision-000001", "metadata.json")
		if err := os.Remove(metadataPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(metadataPath, 0o700); err != nil {
			t.Fatal(err)
		}

		_, err := builder.Build(context.Background(), buildRequest(ws, "matrix_p1g_path_regular", nil))
		matrixErr := requireLayoutUnavailable(t, err, "layout revision metadata is unavailable")
		assertModelSafe(t, matrixErr.Error(), ws.Root)
		if diag := matrixErr.Diagnostic(); !strings.Contains(diag, "not a regular file") {
			t.Fatalf("diagnostic = %q, want a non-regular-file cause", diag)
		}
	})

	t.Run("metadata permission error", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("permission-denied behaviour is not observable as root")
		}
		ws := newWorkspace(t, workspaceOptions{})
		_, builder := newTestStore(t)
		metadataPath := filepath.Join(ws.Root, ".codeagent", "assets", "ui-layout", "layout_p1g", "revision-000001", "metadata.json")
		if err := os.Chmod(metadataPath, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(metadataPath, 0o600) })

		_, err := builder.Build(context.Background(), buildRequest(ws, "matrix_p1g_path_permission", nil))
		matrixErr := requireLayoutUnavailable(t, err, "layout revision metadata is unavailable")
		assertModelSafe(t, matrixErr.Error(), ws.Root)
		if diag := matrixErr.Diagnostic(); !strings.Contains(strings.ToLower(diag), "permission denied") {
			t.Fatalf("diagnostic = %q, want permission denied", diag)
		}
	})

	t.Run("revision symlink escapes the workspace", func(t *testing.T) {
		ws := newWorkspace(t, workspaceOptions{})
		_, builder := newTestStore(t)
		artifactDir := filepath.Join(ws.Root, ".codeagent", "assets", "ui-layout", "layout_escape")
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
		request := buildRequest(ws, "matrix_p1g_path_symlink", nil)
		request.Layout = LayoutRef{ArtifactID: "layout_escape", Revision: 1}

		_, err := builder.Build(context.Background(), request)
		matrixErr := requireLayoutUnavailable(t, err, "layout revision path is outside the workspace")
		assertModelSafe(t, matrixErr.Error(), ws.Root, outside)
	})
}

func requireLayoutUnavailable(t *testing.T, err error, wantMessage string) *Error {
	t.Helper()
	var matrixErr *Error
	if !errors.As(err, &matrixErr) {
		t.Fatalf("error = %v, want *matrix.Error", err)
	}
	if matrixErr.Code != CodeLayoutUnavailable {
		t.Fatalf("code = %q, want %q", matrixErr.Code, CodeLayoutUnavailable)
	}
	if matrixErr.Error() != wantMessage {
		t.Fatalf("message = %q, want %q", matrixErr.Error(), wantMessage)
	}
	return matrixErr
}

// assertModelSafe rejects absolute paths, workspace asset roots, and raw
// filesystem error text in a model-visible message.
func assertModelSafe(t *testing.T, message string, forbidden ...string) {
	t.Helper()
	needles := append([]string{".codeagent", ".codeagent/assets", "lstat ", "/private/", "/var/folders/"}, forbidden...)
	for _, needle := range needles {
		if needle != "" && strings.Contains(message, needle) {
			t.Errorf("model-visible message leaks %q: %q", needle, message)
		}
	}
	if filepath.IsAbs(message) {
		t.Errorf("model-visible message is an absolute path: %q", message)
	}
}
