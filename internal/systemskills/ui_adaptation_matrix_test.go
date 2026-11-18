package systemskills

import (
	"io/fs"
	"strings"
	"testing"
)

// P1-H: the system skill must carry the model-driven matrix orchestration
// contract. This guards the skill bundle, not the frozen backend.
func TestUIAdaptationSkillCoversPreviewMatrixOrchestration(t *testing.T) {
	data, err := fs.ReadFile(Embedded(), "ui-adaptation/SKILL.md")
	if err != nil {
		t.Fatalf("read ui-adaptation skill: %v", err)
	}
	body := string(data)
	for _, required := range []string{
		"Generate the preview matrix",
		"one independent",
		"ui_preview",
		"target × state",
		"completed",
		"failed",
		"missing",
		"Retry one cell at a time",
		"Cancel and resume",
		"coverage",
		"Never reuse another target",
		"ui_preview_matrix",
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("ui-adaptation skill missing matrix orchestration requirement %q", required)
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
		"matrix_dreamlog_duo_iphone",
		"duo_folded",
		"iphone_portrait",
		"loading",
		"Retry one cell",
		"Cancel and resume",
		"coverage",
	} {
		if !strings.Contains(string(reference), required) {
			t.Fatalf("matrix reference missing %q", required)
		}
	}

	manifest, _, err := ReadManifestFS(Embedded())
	if err != nil {
		t.Fatalf("read embedded manifest: %v", err)
	}
	for _, entry := range manifest.Skills {
		if entry.Name == "ui-adaptation" && entry.Version != "8" {
			t.Fatalf("ui-adaptation version = %q, want 8", entry.Version)
		}
	}
}
