package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	agentruntime "code-agent/internal/runtime"
)

func TestR4ARealBackendTurnProducesAndResolvesPreview(t *testing.T) {
	oldState := agentruntime.StoreBaseDir()
	defer agentruntime.SetStoreBaseDir(oldState)
	var output bytes.Buffer
	summary, err := runAcceptance(context.Background(), AcceptanceOptions{
		Workspace: t.TempDir(), StateDir: t.TempDir(), ViewerDir: t.TempDir(), Open: false, Output: &output,
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.SessionID == "" || summary.TurnID == "" || summary.ArtifactID == "" || summary.Revision != 1 {
		t.Fatalf("summary identity = %+v", summary)
	}
	if summary.Capability != "ui-adaptation" || summary.ProducerID != "ui_preview" {
		t.Fatalf("producer identity = %s/%s", summary.Capability, summary.ProducerID)
	}
	if summary.Status != "ready" || summary.ViewerSHA256 == "" || summary.EventSeq == 0 {
		t.Fatalf("preview summary = %+v", summary)
	}
	info, err := os.Stat(summary.ViewerPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("viewer mode = %o", info.Mode().Perm())
	}
	if !bytes.Contains(output.Bytes(), []byte("[preview] R4a Backend Preview")) {
		t.Fatalf("live CLI output missing preview event:\n%s", output.String())
	}
}

func TestR4ACustomFileBackedPreviewBytes(t *testing.T) {
	oldState := agentruntime.StoreBaseDir()
	defer agentruntime.SetStoreBaseDir(oldState)
	workspace := t.TempDir()
	viewerHTML := []byte(`<main id="custom-r4a">Custom viewer bytes 2026-11-10</main>`)
	htmlPath := filepath.Join(t.TempDir(), "viewer.html")
	if err := os.WriteFile(htmlPath, viewerHTML, 0o644); err != nil {
		t.Fatal(err)
	}
	fallback := testR4APNG(t, 180)
	fallbackPath := filepath.Join(t.TempDir(), "fallback.png")
	if err := os.WriteFile(fallbackPath, fallback, 0o644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	summary, err := runAcceptance(context.Background(), AcceptanceOptions{
		Workspace: workspace, StateDir: t.TempDir(), ViewerDir: t.TempDir(), Open: false, Output: &output,
		ViewerHTMLFile: htmlPath, FallbackFile: fallbackPath,
		Title: "Custom R4a", Summary: "Custom summary", ArtifactKey: "ui-adaptation:r4a-custom",
		PreferredAspectRatio: 1.78, MinHeight: 420,
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Title != "Custom R4a" || summary.Summary != "Custom summary" {
		t.Fatalf("custom artifact metadata = %+v", summary)
	}
	if summary.AssetSHA256 != "sha256:"+sha256Hex(fallback) || summary.AssetSizeBytes != int64(len(fallback)) {
		t.Fatalf("custom fallback digest/size = %s/%d", summary.AssetSHA256, summary.AssetSizeBytes)
	}
	materialized, err := os.ReadFile(summary.ViewerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(materialized, viewerHTML) {
		t.Fatal("custom viewer fragment bytes did not enter the trusted viewer")
	}
}

func testR4APNG(t *testing.T, shade uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 32, 24))
	for y := 0; y < 24; y++ {
		for x := 0; x < 32; x++ {
			img.Set(x, y, color.RGBA{R: shade, G: uint8(x * 5), B: uint8(y * 7), A: 255})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
