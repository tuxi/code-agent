package agent

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/tools"
)

// visionLocalAsset builds a LocalAssetRef pointing at a real workspace file.
func visionLocalAsset(t *testing.T, root, rel, filename, mime, kind string, size int64) model.LocalAssetRef {
	t.Helper()
	return model.LocalAssetRef{
		ID:             filename,
		RelativePath:   rel,
		Filename:       filename,
		MIMEType:       mime,
		Kind:           kind,
		SizeBytes:      size,
		SHA256:         strings.Repeat("a", 64),
		TransferPolicy: "local_only",
	}
}

// TestVisionInjectionInlinesImageParts verifies a vision-capable runner turns a
// local PNG attachment into a data-URL image_url content part on the user
// message, with the text prompt preserved as the leading text part, and that
// persisted history keeps only the reference (no bytes, no parts).
func TestVisionInjectionInlinesImageParts(t *testing.T) {
	root := t.TempDir()
	// Minimal 1x1 PNG: magic bytes sniffed as image/png regardless of the
	// declared MIME type.
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	if err := os.WriteFile(filepath.Join(root, "shot.png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []model.Response{{Content: "done"}}}
	runner := &Runner{
		Model: provider, Tools: tools.NewRegistry(), MaxSteps: 1,
		WorkspaceRoot: root, VisionSupported: true,
	}
	local := visionLocalAsset(t, root, "shot.png", "shot.png", "application/octet-stream", "image", int64(len(png)))
	sess := &session.Session{ID: "session", Messages: []model.Message{{Role: model.RoleSystem, Content: "test"}}, Metadata: map[string]any{}}
	if _, err := runner.RunTurnWithAllAssets(context.Background(), sess, "describe", nil, []model.LocalAssetRef{local}); err != nil {
		t.Fatal(err)
	}
	// History keeps only the reference.
	if len(sess.Messages[1].LocalAssets) != 1 || len(sess.Messages[1].ContentParts) != 0 {
		t.Fatalf("history mutated: %+v", sess.Messages[1])
	}
	// Provider request carries the parts.
	user := provider.lastMessages[1]
	if len(user.ContentParts) != 1 {
		t.Fatalf("ContentParts = %+v, want 1 image part", user.ContentParts)
	}
	part := user.ContentParts[0]
	if part.Type != "image_url" || part.ImageURL == nil {
		t.Fatalf("part = %+v, want image_url", part)
	}
	wantURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	if part.ImageURL.URL != wantURL {
		t.Errorf("url = %q, want %q", part.ImageURL.URL, wantURL)
	}
	// Structured local metadata never reaches the provider.
	if len(user.LocalAssets) != 0 {
		t.Errorf("provider received structured local assets: %+v", user.LocalAssets)
	}
}

// TestVisionInjectionPreservesPromptText verifies the user's text prompt stays
// the leading content part so the model reads the instruction before the image.
func TestVisionInjectionPreservesPromptText(t *testing.T) {
	root := t.TempDir()
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	if err := os.WriteFile(filepath.Join(root, "a.png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []model.Response{{Content: "ok"}}}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 1, WorkspaceRoot: root, VisionSupported: true}
	local := visionLocalAsset(t, root, "a.png", "a.png", "image/png", "image", int64(len(png)))
	sess := &session.Session{ID: "s", Messages: []model.Message{{Role: model.RoleSystem, Content: "sys"}}, Metadata: map[string]any{}}
	if _, err := runner.RunTurnWithAllAssets(context.Background(), sess, "look at it", nil, []model.LocalAssetRef{local}); err != nil {
		t.Fatal(err)
	}
	user := provider.lastMessages[1]
	if !strings.HasPrefix(user.Content, "look at it") {
		t.Errorf("text content = %q, want prompt preserved as prefix", user.Content)
	}
	if !strings.Contains(user.Content, `stored in the workspace at`) {
		t.Errorf("content %q should carry the workspace-path note", user.Content)
	}
	if len(user.ContentParts) != 1 {
		t.Fatalf("ContentParts = %+v, want the image part only (text rides Content)", user.ContentParts)
	}
}

// TestVisionInjectionNonImageFallsBackToManifest verifies PDF-style attachments
// still get the textual manifest note and are not dropped.
func TestVisionInjectionNonImageFallsBackToManifest(t *testing.T) {
	root := t.TempDir()
	pdf := []byte("%PDF-1.4 fake")
	if err := os.WriteFile(filepath.Join(root, "doc.pdf"), pdf, 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []model.Response{{Content: "ok"}}}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 1, WorkspaceRoot: root, VisionSupported: true}
	local := visionLocalAsset(t, root, "doc.pdf", "doc.pdf", "application/pdf", "pdf", int64(len(pdf)))
	sess := &session.Session{ID: "s", Messages: []model.Message{{Role: model.RoleSystem, Content: "sys"}}, Metadata: map[string]any{}}
	if _, err := runner.RunTurnWithAllAssets(context.Background(), sess, "read", nil, []model.LocalAssetRef{local}); err != nil {
		t.Fatal(err)
	}
	user := provider.lastMessages[1]
	if len(user.ContentParts) != 0 {
		t.Fatalf("ContentParts = %+v, want none for non-image", user.ContentParts)
	}
	for _, want := range []string{"doc.pdf", "application/pdf", "do not guess"} {
		if !strings.Contains(user.Content, want) {
			t.Errorf("content %q missing %q", user.Content, want)
		}
	}
}

// TestVisionInjectionPathEscapeDegrades verifies a persisted RelativePath that
// escapes the workspace is refused even when the target file exists and is a
// valid image outside the workspace: it degrades to an unavailable note.
func TestVisionInjectionPathEscapeDegrades(t *testing.T) {
	root := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.png")
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	if err := os.WriteFile(outside, png, 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []model.Response{{Content: "ok"}}}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 1, WorkspaceRoot: root, VisionSupported: true}
	local := model.LocalAssetRef{
		ID: "evil", RelativePath: "../" + filepath.Base(outsideDir) + "/secret.png",
		Filename: "secret.png", MIMEType: "image/png", Kind: "image", SizeBytes: int64(len(png)),
		SHA256: strings.Repeat("a", 64), TransferPolicy: "local_only",
	}
	sess := &session.Session{ID: "s", Messages: []model.Message{{Role: model.RoleSystem, Content: "sys"}}, Metadata: map[string]any{}}
	if _, err := runner.RunTurnWithAllAssets(context.Background(), sess, "read", nil, []model.LocalAssetRef{local}); err != nil {
		t.Fatal(err)
	}
	user := provider.lastMessages[1]
	if len(user.ContentParts) != 0 {
		t.Fatalf("ContentParts = %+v, want none (path escaped)", user.ContentParts)
	}
	if !strings.Contains(user.Content, "unavailable") {
		t.Errorf("content %q should mention unavailability", user.Content)
	}
}

// TestVisionInjectionKeepsGatewayAssetManifest verifies gateway-owned Assets
// still get the textual manifest under a vision runner (the Runtime holds no
// bytes for them, so they are never inlined) and are stripped from the request.
func TestVisionInjectionKeepsGatewayAssetManifest(t *testing.T) {
	provider := &scriptedProvider{responses: []model.Response{{Content: "ok"}}}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 1, VisionSupported: true, UserAssetsSupported: true, RequestID: "req_gw", ReservedTurnID: "turn_gw"}
	asset := model.GatewayAssetRef{AssetID: 9, Kind: "image", MIMEType: "image/png", Filename: "up.png"}
	sess := &session.Session{ID: "s", Messages: []model.Message{{Role: model.RoleSystem, Content: "sys"}}, Metadata: map[string]any{}}
	if _, err := runner.RunTurnWithAssets(context.Background(), sess, "look", []model.GatewayAssetRef{asset}); err != nil {
		t.Fatal(err)
	}
	user := provider.lastMessages[1]
	if len(user.ContentParts) != 0 {
		t.Fatalf("ContentParts = %+v, want none (gateway assets are not inlined)", user.ContentParts)
	}
	if len(user.Assets) != 0 {
		t.Fatalf("gateway assets leaked into provider request: %+v", user.Assets)
	}
	for _, want := range []string{"asset_id=9", "analyze_image", "up.png"} {
		if !strings.Contains(user.Content, want) {
			t.Errorf("content %q missing %q", user.Content, want)
		}
	}
}

// TestVisionInjectionMissingFileDegradesNotFails verifies an attachment whose
// file vanished degrades to a textual note (appended to message content)
// instead of failing the turn.
func TestVisionInjectionMissingFileDegradesNotFails(t *testing.T) {
	root := t.TempDir()
	provider := &scriptedProvider{responses: []model.Response{{Content: "ok"}}}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 1, WorkspaceRoot: root, VisionSupported: true}
	local := visionLocalAsset(t, root, "gone.png", "gone.png", "image/png", "image", 10)
	sess := &session.Session{ID: "s", Messages: []model.Message{{Role: model.RoleSystem, Content: "sys"}}, Metadata: map[string]any{}}
	if _, err := runner.RunTurnWithAllAssets(context.Background(), sess, "read", nil, []model.LocalAssetRef{local}); err != nil {
		t.Fatal(err)
	}
	user := provider.lastMessages[1]
	if len(user.ContentParts) != 0 {
		t.Fatalf("ContentParts = %+v, want none (no image materialized)", user.ContentParts)
	}
	if !strings.Contains(user.Content, "unavailable") {
		t.Errorf("content %q should mention unavailability", user.Content)
	}
}

// TestVisionInjectionOversizedImageDegradesNotFails verifies the per-image
// budget check skips oversized images with a note instead of failing the turn.
func TestVisionInjectionOversizedImageDegradesNotFails(t *testing.T) {
	root := t.TempDir()
	// A byte slice whose first bytes look like a JPEG but is way over budget.
	big := make([]byte, visionMaxImageBytes+1)
	big[0], big[1], big[2] = 0xFF, 0xD8, 0xFF
	if err := os.WriteFile(filepath.Join(root, "huge.jpg"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []model.Response{{Content: "ok"}}}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 1, WorkspaceRoot: root, VisionSupported: true}
	local := visionLocalAsset(t, root, "huge.jpg", "huge.jpg", "image/jpeg", "image", int64(len(big)))
	sess := &session.Session{ID: "s", Messages: []model.Message{{Role: model.RoleSystem, Content: "sys"}}, Metadata: map[string]any{}}
	if _, err := runner.RunTurnWithAllAssets(context.Background(), sess, "read", nil, []model.LocalAssetRef{local}); err != nil {
		t.Fatal(err)
	}
	user := provider.lastMessages[1]
	if len(user.ContentParts) != 0 {
		t.Fatalf("ContentParts = %+v, want none (image skipped)", user.ContentParts)
	}
	if !strings.Contains(user.Content, "omitted") {
		t.Errorf("content %q should mention omission", user.Content)
	}
}

// TestTextOnlyModelStillUsesManifest verifies the fail-safe default: a runner
// without VisionSupported keeps the exact textual manifest behavior for images.
func TestTextOnlyModelStillUsesManifest(t *testing.T) {
	root := t.TempDir()
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	if err := os.WriteFile(filepath.Join(root, "shot.png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []model.Response{{Content: "done"}}}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 1, WorkspaceRoot: root, VisionSupported: false}
	local := visionLocalAsset(t, root, "shot.png", "shot.png", "image/png", "image", int64(len(png)))
	sess := &session.Session{ID: "session", Messages: []model.Message{{Role: model.RoleSystem, Content: "test"}}, Metadata: map[string]any{}}
	if _, err := runner.RunTurnWithAllAssets(context.Background(), sess, "describe", nil, []model.LocalAssetRef{local}); err != nil {
		t.Fatal(err)
	}
	user := provider.lastMessages[1]
	if len(user.ContentParts) != 0 {
		t.Fatalf("text-only model received ContentParts: %+v", user.ContentParts)
	}
	for _, want := range []string{"NOT visible", "analyze_local_image"} {
		if !strings.Contains(user.Content, want) {
			t.Errorf("manifest %q missing %q", user.Content, want)
		}
	}
}

// TestVisionInjectionMixedAssets verifies image + non-image attachments in one
// message: the image becomes a part, the PDF stays a manifest-style note.
func TestVisionInjectionMixedAssets(t *testing.T) {
	root := t.TempDir()
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	if err := os.WriteFile(filepath.Join(root, "a.png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	pdf := []byte("%PDF-1.4")
	if err := os.WriteFile(filepath.Join(root, "b.pdf"), pdf, 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []model.Response{{Content: "ok"}}}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 1, WorkspaceRoot: root, VisionSupported: true}
	sess := &session.Session{ID: "s", Messages: []model.Message{{Role: model.RoleSystem, Content: "sys"}}, Metadata: map[string]any{}}
	_, err := runner.RunTurnWithAllAssets(context.Background(), sess, "both", nil, []model.LocalAssetRef{
		visionLocalAsset(t, root, "a.png", "a.png", "image/png", "image", int64(len(png))),
		visionLocalAsset(t, root, "b.pdf", "b.pdf", "application/pdf", "pdf", int64(len(pdf))),
	})
	if err != nil {
		t.Fatal(err)
	}
	user := provider.lastMessages[1]
	if len(user.ContentParts) != 1 {
		t.Fatalf("ContentParts = %+v, want exactly the image part", user.ContentParts)
	}
	if user.ContentParts[0].Type != "image_url" {
		t.Errorf("part = %+v, want image_url", user.ContentParts[0])
	}
	if !strings.Contains(user.Content, "b.pdf") {
		t.Errorf("content %q should mention the pdf in a note", user.Content)
	}
}
