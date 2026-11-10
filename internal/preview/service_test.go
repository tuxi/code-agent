package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	assets "code-agent/internal/assetref"
)

type memoryAssetLoader struct {
	data []byte
	mime string
}

func (l memoryAssetLoader) Load(context.Context, string, assets.Ref) ([]byte, string, error) {
	return append([]byte(nil), l.data...), l.mime, nil
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	for y := 0; y < 3; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 40), G: uint8(y * 60), B: 120, A: 255})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func newTestService(t *testing.T) (*Service, *FSStore) {
	t.Helper()
	store, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store)
	service.AssetLoader = memoryAssetLoader{data: testPNG(t), mime: "image/png"}
	service.Now = func() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) }
	return service, store
}

func readyDraft(key string) Draft {
	return Draft{
		ArtifactKey: key, Kind: KindInteractiveHTML, Title: "Preview",
		ViewerHTML: "<div>ready</div>",
		Assets: []AssetDraft{{
			AssetID: "asset_fallback", Role: RoleFallback, MIMEType: "image/png", Alt: "fallback",
		}},
		Fallback: &FallbackDraft{AssetID: "asset_fallback", Text: "fallback"},
	}
}

func TestPendingThenReadyAllocatesNextRevision(t *testing.T) {
	service, store := newTestService(t)
	base := ProduceRequest{
		SessionID: "session_preview", TurnID: "turn_1", CallID: "call_1",
		WorkspaceRoot: t.TempDir(), ProducerID: "ui_preview", Capability: "ui-adaptation",
		Assets: []assets.Ref{{ID: "asset_fallback", MIMEType: "image/png", WorkspaceRelativePath: "fallback.png"}},
	}
	base.Drafts = []Draft{{ArtifactKey: "artifact", Kind: KindInteractiveHTML, Title: "Preview"}}
	first := service.Produce(context.Background(), base)
	if first.Fatal != nil || len(first.Rejected) != 0 || len(first.Artifacts) != 1 {
		t.Fatalf("pending result = %+v", first)
	}
	if first.Artifacts[0].Status != StatusPending || first.Artifacts[0].Revision != 1 {
		t.Fatalf("pending artifact = %+v", first.Artifacts[0])
	}

	base.Drafts = []Draft{readyDraft("artifact")}
	second := service.Produce(context.Background(), base)
	if second.Fatal != nil || len(second.Rejected) != 0 || len(second.Artifacts) != 1 {
		t.Fatalf("ready result = %+v", second)
	}
	ready := second.Artifacts[0]
	if ready.Status != StatusReady || ready.Revision != 2 {
		t.Fatalf("ready artifact = %+v", ready)
	}
	if ready.ArtifactID != first.Artifacts[0].ArtifactID || !ready.CreatedAt.Equal(first.Artifacts[0].CreatedAt) {
		t.Fatalf("identity/timestamp changed across revisions: pending=%+v ready=%+v", first.Artifacts[0], ready)
	}
	if ready.RevisionSHA256 == first.Artifacts[0].RevisionSHA256 {
		t.Fatal("pending and ready must have distinct revision hashes")
	}
	latest, ok, err := store.Latest(context.Background(), Namespace{
		SessionID: base.SessionID, Capability: base.Capability, ProducerID: base.ProducerID, ArtifactKey: "artifact",
	})
	if err != nil || !ok || latest.Revision != 2 {
		t.Fatalf("latest = %+v ok=%v err=%v", latest, ok, err)
	}
}

func TestStoreRejectsMutableRevision(t *testing.T) {
	service, store := newTestService(t)
	request := ProduceRequest{
		SessionID: "session_preview", TurnID: "turn_1", CallID: "call_1",
		WorkspaceRoot: t.TempDir(), ProducerID: "ui_preview", Capability: "ui-adaptation",
		Assets: []assets.Ref{{ID: "asset_fallback", MIMEType: "image/png", WorkspaceRelativePath: "fallback.png"}},
		Drafts: []Draft{readyDraft("artifact")},
	}
	result := service.Produce(context.Background(), request)
	if result.Fatal != nil || len(result.Artifacts) != 1 {
		t.Fatalf("produce result = %+v", result)
	}
	artifact := result.Artifacts[0].Clone()
	viewer, err := store.ReadViewer(context.Background(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	fallback, err := store.ReadAsset(context.Background(), artifact, "asset_fallback")
	if err != nil {
		t.Fatal(err)
	}
	artifact.Title = "Mutated"
	hash, err := RevisionSHA256(artifact)
	if err != nil {
		t.Fatal(err)
	}
	artifact.RevisionSHA256 = hash
	namespace := Namespace{SessionID: artifact.SessionID, Capability: artifact.Capability, ProducerID: artifact.ProducerID, ArtifactKey: "artifact"}
	err = store.ImportArtifact(context.Background(), namespace, artifact, viewer, map[string][]byte{"asset_fallback": fallback})
	var previewErr *Error
	if !errors.As(err, &previewErr) || previewErr.Code != CodeRevisionConflict {
		t.Fatalf("mutable revision error = %v", err)
	}
}

func TestPreviewAssetDTOHasNoAbsolutePath(t *testing.T) {
	service, _ := newTestService(t)
	request := ProduceRequest{
		SessionID: "session_preview", TurnID: "turn_1", CallID: "call_1",
		WorkspaceRoot: t.TempDir(), ProducerID: "ui_preview", Capability: "ui-adaptation",
		Assets: []assets.Ref{{ID: "asset_fallback", MIMEType: "image/png", WorkspaceRelativePath: "nested/fallback.png"}},
		Drafts: []Draft{readyDraft("artifact")},
	}
	result := service.Produce(context.Background(), request)
	if result.Fatal != nil || len(result.Artifacts) != 1 {
		t.Fatalf("produce result = %+v", result)
	}
	data, err := json.Marshal(result.Artifacts[0])
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "absolute_path") || strings.Contains(text, request.WorkspaceRoot) {
		t.Fatalf("preview DTO leaked a filesystem path: %s", text)
	}
	if !strings.Contains(text, `"workspace_relative_path":"nested/fallback.png"`) {
		t.Fatalf("sanitized workspace-relative path missing: %s", text)
	}
}

func TestExpiredPreviewResolverReturnsGone(t *testing.T) {
	root := filepath.Join("..", "..", "docs", "capability-runtime-upgrade", "fixtures", "preview", "v1")
	data, err := os.ReadFile(filepath.Join(root, "expired-replay.json"))
	if err != nil {
		t.Fatal(err)
	}
	var artifact Artifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatal(err)
	}
	viewer, err := os.ReadFile(filepath.Join(root, "viewer.html"))
	if err != nil {
		t.Fatal(err)
	}
	fallback, err := os.ReadFile(filepath.Join(root, "assets", "fallback.png"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ImportArtifact(context.Background(), Namespace{SessionID: artifact.SessionID}, artifact, viewer, map[string][]byte{"asset_fixture_fallback": fallback}); err != nil {
		t.Fatal(err)
	}
	service := NewService(store)
	service.Now = func() time.Time { return time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC) }
	_, err = service.Resolve(artifact.SessionID, artifact.ArtifactID, artifact.Revision)
	var previewErr *Error
	if !errors.As(err, &previewErr) || previewErr.Code != CodeExpired {
		t.Fatalf("expired resolve error = %v", err)
	}
}

func TestFourDraftLimitRejectsOnlyOverflow(t *testing.T) {
	service, _ := newTestService(t)
	drafts := make([]Draft, 5)
	for i := range drafts {
		drafts[i] = Draft{ArtifactKey: "artifact-" + string(rune('a'+i)), Kind: KindInteractiveHTML, Title: "Pending"}
	}
	result := service.Produce(context.Background(), ProduceRequest{
		SessionID: "session_preview", TurnID: "turn_1", CallID: "call_1",
		ProducerID: "ui_preview", Capability: "ui-adaptation", Drafts: drafts,
	})
	if len(result.Artifacts) != 4 || len(result.Rejected) != 1 || result.Rejected[0].Index != 4 {
		t.Fatalf("draft limit result = %+v", result)
	}
}

func TestMaterializePrivateViewerUsesVerifiedBytes(t *testing.T) {
	root := filepath.Join("..", "..", "docs", "capability-runtime-upgrade", "fixtures", "preview", "v1")
	data, err := os.ReadFile(filepath.Join(root, "ready-interactive.json"))
	if err != nil {
		t.Fatal(err)
	}
	var artifact Artifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatal(err)
	}
	viewer, err := os.ReadFile(filepath.Join(root, "viewer.html"))
	if err != nil {
		t.Fatal(err)
	}
	fallback, err := os.ReadFile(filepath.Join(root, "assets", "fallback.png"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ImportArtifact(context.Background(), Namespace{SessionID: artifact.SessionID}, artifact, viewer, map[string][]byte{"asset_fixture_fallback": fallback}); err != nil {
		t.Fatal(err)
	}
	service := NewService(store)
	dir := filepath.Join(t.TempDir(), "private")
	path, err := service.MaterializePrivateViewer(context.Background(), dir, artifact.SessionID, artifact.ArtifactID, artifact.Revision)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, viewer) {
		t.Fatal("private viewer bytes differ from verified fixture bytes")
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0o600 || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("private viewer permissions = file %o dir %o", fileInfo.Mode().Perm(), dirInfo.Mode().Perm())
	}
}

func TestClientPreviewRejectsAbsoluteAssetReference(t *testing.T) {
	service, _ := newTestService(t)
	result := service.Produce(context.Background(), ProduceRequest{
		SessionID: "session_preview", TurnID: "turn_1", CallID: "call_1", ClientOrigin: true,
		WorkspaceRoot: t.TempDir(), ProducerID: "ui_preview", Capability: "ui-adaptation",
		Assets: []assets.Ref{{ID: "asset_fallback", MIMEType: "image/png", AbsolutePath: "/tmp/fallback.png", WorkspaceRelativePath: ".codeagent/assets/client/fallback.png"}},
		Drafts: []Draft{readyDraft("artifact")},
	})
	if len(result.Rejected) != 1 || result.Rejected[0].Err.Code != CodeSecurityViolation {
		t.Fatalf("client absolute asset result = %+v", result)
	}
}

func TestDuplicateArtifactKeysFailThatToolResult(t *testing.T) {
	service, _ := newTestService(t)
	draft := Draft{ArtifactKey: "duplicate", Kind: KindInteractiveHTML, Title: "Pending"}
	result := service.Produce(context.Background(), ProduceRequest{
		SessionID: "session_preview", TurnID: "turn_1", CallID: "call_1",
		ProducerID: "ui_preview", Capability: "ui-adaptation", Drafts: []Draft{draft, draft},
	})
	if len(result.Artifacts) != 0 || len(result.Rejected) != 2 || result.Rejected[0].Err.Code != CodeInvalidArtifact {
		t.Fatalf("duplicate artifact result = %+v", result)
	}
}

func TestMaterializedViewerAllowsDataImagesAndRejectsNetworkSources(t *testing.T) {
	if _, err := MaterializeViewer("ok", `<img src="data:image/png;base64,iVBORw0KGgo="><link rel="icon" href="data:image/svg+xml;base64,PHN2Zz4=">`); err != nil {
		t.Fatalf("self-contained data resources rejected: %+v", err)
	}
	if _, err := MaterializeViewer("bad", `<img src="https://example.com/a.png">`); err == nil || err.Code != CodeSecurityViolation {
		t.Fatalf("network image error = %+v", err)
	}
	if _, err := MaterializeViewer("bad", `<a href="https://example.com">go</a>`); err == nil || err.Code != CodeSecurityViolation {
		t.Fatalf("network href error = %+v", err)
	}
}

func TestClientPreviewAcceptsRuntimeStagedSnapshot(t *testing.T) {
	service, _ := newTestService(t)
	result := service.Produce(context.Background(), ProduceRequest{
		SessionID: "session_preview", TurnID: "turn_1", CallID: "call_1", ClientOrigin: true,
		WorkspaceRoot: t.TempDir(), ProducerID: "ui_preview", Capability: "ui-adaptation",
		Assets: []assets.Ref{{ID: "asset_fallback", MIMEType: "image/png", WorkspaceRelativePath: ".codeagent/assets/client/fallback.png"}},
		Drafts: []Draft{readyDraft("artifact")},
	})
	if len(result.Artifacts) != 1 || len(result.Rejected) != 0 {
		t.Fatalf("client snapshot result = %+v", result)
	}
}

func TestConcurrentReadySnapshotsAllocateDistinctRevisions(t *testing.T) {
	service, _ := newTestService(t)
	request := func(title string) ProduceRequest {
		draft := readyDraft("artifact")
		draft.Title = title
		return ProduceRequest{
			SessionID: "session_preview", TurnID: "turn_1", CallID: "call_1",
			WorkspaceRoot: t.TempDir(), ProducerID: "ui_preview", Capability: "ui-adaptation",
			Assets: []assets.Ref{{ID: "asset_fallback", MIMEType: "image/png", WorkspaceRelativePath: "fallback.png"}},
			Drafts: []Draft{draft},
		}
	}
	type outcome struct {
		artifact Artifact
		result   ProduceResult
	}
	results := make(chan outcome, 2)
	for _, title := range []string{"One", "Two"} {
		go func(title string) {
			result := service.Produce(context.Background(), request(title))
			var artifact Artifact
			if len(result.Artifacts) == 1 {
				artifact = result.Artifacts[0]
			}
			results <- outcome{artifact: artifact, result: result}
		}(title)
	}
	revisions := map[int64]bool{}
	for i := 0; i < 2; i++ {
		got := <-results
		if got.result.Fatal != nil || len(got.result.Rejected) != 0 || len(got.result.Artifacts) != 1 {
			t.Fatalf("concurrent result = %+v", got.result)
		}
		revisions[got.artifact.Revision] = true
	}
	if !revisions[1] || !revisions[2] {
		t.Fatalf("concurrent revisions = %v", revisions)
	}
}

func TestMaterializedViewerAllowsHeaderAndOpenAIWidgetStateAPI(t *testing.T) {
	fragment := `<header><strong>DreamLog</strong></header><script>const saved=window.openai?.widgetState?.modelContent;</script>`
	if _, err := MaterializeViewer("ok", fragment); err != nil {
		t.Fatalf("safe header/openai fragment rejected: %+v", err)
	}
	if _, err := MaterializeViewer("bad", `<script>window.open("https://example.com")</script>`); err == nil || err.Code != CodeSecurityViolation {
		t.Fatalf("window.open navigation error = %+v", err)
	}
}
