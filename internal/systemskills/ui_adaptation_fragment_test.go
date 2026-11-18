package systemskills

import (
	"io/fs"
	"os"
	"strings"
	"testing"

	"code-agent/internal/preview"
)

// P1-H follow-up: the skill must teach the model to author a body fragment,
// not a full HTML document. This guards the skill bundle and the real preview
// security contract without changing the backend.
func TestUIAdaptationSkillRequiresViewerFragment(t *testing.T) {
	skill, err := fs.ReadFile(Embedded(), "ui-adaptation/SKILL.md")
	if err != nil {
		t.Fatalf("read ui-adaptation skill: %v", err)
	}
	for _, required := range []string{
		"self-contained HTML fragment",
		"<!doctype html>",
		"<html>",
		"<head>",
		"<body>",
		"restricted inline script",
		"`data:` URL",
		"`href=\"#\"`",
		"<button type=\"button\">",
		"preview_security_violation",
		"Do not change the",
		"bridge",
	} {
		if !strings.Contains(string(skill), required) {
			t.Fatalf("ui-adaptation skill missing fragment requirement %q", required)
		}
	}

	reference, err := fs.ReadFile(
		Embedded(),
		"ui-adaptation/references/docs/preview-matrix-orchestration.md",
	)
	if err != nil {
		t.Fatalf("read matrix orchestration reference: %v", err)
	}
	for _, required := range []string{
		"`viewer_html` is a fragment",
		`"viewer_html": "<style>`,
		"no `<!doctype html>`",
		"no `<html>`",
		"no `<head>`",
		"no `<body>`",
		"`href` and `src`",
		"`href=\"#\"`",
		"preview_security_violation",
		"Do not change",
		"bridge",
	} {
		if !strings.Contains(string(reference), required) {
			t.Fatalf("matrix orchestration reference missing fragment requirement %q", required)
		}
	}
}

func TestViewerFragmentFixtures(t *testing.T) {
	fragment, err := os.ReadFile("testdata/ui-adaptation/fragment.html")
	if err != nil {
		t.Fatalf("read fragment fixture: %v", err)
	}
	if _, previewErr := preview.MaterializeViewer("fragment fixture", string(fragment)); previewErr != nil {
		t.Fatalf("fragment fixture rejected: %v (%s)", previewErr, previewErr.Message)
	}

	fullDocument, err := os.ReadFile("testdata/ui-adaptation/full-document.html")
	if err != nil {
		t.Fatalf("read full-document fixture: %v", err)
	}
	_, previewErr := preview.MaterializeViewer("full document fixture", string(fullDocument))
	if previewErr == nil {
		t.Fatal("full-document fixture was accepted; want preview_security_violation")
	}
	if previewErr.Code != preview.CodeSecurityViolation {
		t.Fatalf("full-document fixture error = %s, want %s (%s)", previewErr.Code, preview.CodeSecurityViolation, previewErr.Message)
	}

	navigation, err := os.ReadFile("testdata/ui-adaptation/navigation-fragment.html")
	if err != nil {
		t.Fatalf("read navigation fixture: %v", err)
	}
	_, previewErr = preview.MaterializeViewer("navigation fixture", string(navigation))
	if previewErr == nil || previewErr.Code != preview.CodeSecurityViolation {
		t.Fatalf("navigation fixture error = %v, want preview_security_violation", previewErr)
	}
}

func TestUIAdaptationFragmentBundleVersion(t *testing.T) {
	manifest, _, err := ReadManifestFS(Embedded())
	if err != nil {
		t.Fatalf("read embedded manifest: %v", err)
	}
	if manifest.BundleVersion != "2026.11.18.1" {
		t.Fatalf("bundle_version = %q, want 2026.11.18.1", manifest.BundleVersion)
	}
}
