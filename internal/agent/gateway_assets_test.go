package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/assetref"
	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/tools"
)

type fakeGatewayUploader struct {
	calls    int
	upload   model.AssetUpload
	assetRef model.GatewayAssetRef
}

func (u *fakeGatewayUploader) UploadAsset(_ context.Context, upload model.AssetUpload) (model.GatewayAssetRef, error) {
	u.calls++
	u.upload = upload
	return u.assetRef, nil
}

type fakeScreenshotTool struct{ path string }

func (t fakeScreenshotTool) Name() string        { return "mcp__desktop_control__screenshot_capture" }
func (t fakeScreenshotTool) Description() string { return "capture screenshot" }
func (t fakeScreenshotTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (t fakeScreenshotTool) Execute(context.Context, tools.ExecutionContext, json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{Content: "Screenshot captured.", Assets: []assets.Ref{
		{ID: "local_screenshot_image", Kind: "image", MIMEType: "image/png", AbsolutePath: t.path},
		// desktop-control returns an ImageContent plus an aliased ResourceLink.
		{ID: "local_screenshot_resource", Kind: "image", MIMEType: "image/png", AbsolutePath: t.path},
	}}, nil
}

func TestScreenshotDoesNotUploadMerelyBecauseUploaderIsConfigured(t *testing.T) {
	path := filepath.Join(t.TempDir(), "screenshot.png")
	if err := os.WriteFile(path, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	if err := registry.Register(fakeScreenshotTool{path: path}); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []model.Response{
		{ToolCalls: []model.ToolCall{{ID: "call_screenshot", Type: "function", Function: model.FunctionCall{Name: "mcp__desktop_control__screenshot_capture", Arguments: `{}`}}}},
		{Content: "done"},
	}}
	uploader := &fakeGatewayUploader{assetRef: model.GatewayAssetRef{AssetID: 42, SHA256: "sha", Kind: "image", MIMEType: "image/png", Filename: "screenshot.png"}}
	runner := &Runner{Model: provider, Tools: registry, WorkspaceRoot: filepath.Dir(path), AssetUploader: uploader, MaxSteps: 2}
	if _, err := runner.RunTurn(context.Background(), &session.Session{ID: "session", Messages: []model.Message{{Role: model.RoleSystem, Content: "test"}}, Metadata: map[string]any{}}, "inspect"); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if uploader.calls != 0 {
		t.Fatalf("screenshot was uploaded without explicit authorization: upload=%+v calls=%d", uploader.upload, uploader.calls)
	}
	for _, message := range provider.lastMessages {
		if message.Role == model.RoleTool && message.ToolCallID == "call_screenshot" {
			if len(message.Assets) != 0 {
				t.Fatalf("tool result unexpectedly contains Gateway assets: %+v", message.Assets)
			}
			if provider.lastRequest.SessionID != "session" || provider.lastRequest.ExecutionID == "" {
				t.Fatalf("gateway correlation missing: %+v", provider.lastRequest)
			}
			return
		}
	}
	t.Fatal("next model request did not include local screenshot tool result")
}

func TestRunTurnWithAssetsPreservesUserGatewayAsset(t *testing.T) {
	provider := &scriptedProvider{responses: []model.Response{{Content: "done"}}}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 1, UserAssetsSupported: true, RequestID: "req_7", ReservedTurnID: "turn_7"}
	asset := model.GatewayAssetRef{AssetID: 7, Kind: "image", MIMEType: "image/jpeg", Filename: "error.jpg", SHA256: "sha"}
	if _, err := runner.RunTurnWithAssets(context.Background(), &session.Session{ID: "session", Messages: []model.Message{{Role: model.RoleSystem, Content: "test"}}, Metadata: map[string]any{}}, "analyze", []model.GatewayAssetRef{asset}); err != nil {
		t.Fatal(err)
	}
	if provider.lastRequest.TurnID != "turn_7" || provider.lastRequest.RequestID != "req_7" || provider.lastRequest.ExecutionID == "" {
		t.Fatalf("gateway identities missing: %+v", provider.lastRequest)
	}
	if len(provider.lastMessages) < 2 || len(provider.lastMessages[1].Assets) != 1 || provider.lastMessages[1].Assets[0].AssetID != 7 {
		t.Fatalf("user assets lost: %+v", provider.lastMessages)
	}
}

func TestRunTurnWithLocalAssetsSendsOnlySafeManifestToProvider(t *testing.T) {
	provider := &scriptedProvider{responses: []model.Response{{Content: "done"}}}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 1, RequestID: "req_local", ReservedTurnID: "turn_local"}
	local := model.LocalAssetRef{
		ID: "doc", RelativePath: "user-assets/doc/report.pdf", Filename: "report.pdf",
		MIMEType: "application/pdf", Kind: "pdf", SizeBytes: 42,
		SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TransferPolicy: "local_only",
	}
	sess := &session.Session{ID: "session", Messages: []model.Message{{Role: model.RoleSystem, Content: "test"}}, Metadata: map[string]any{}}
	if _, err := runner.RunTurnWithAllAssets(context.Background(), sess, "", nil, []model.LocalAssetRef{local}); err != nil {
		t.Fatal(err)
	}
	if len(sess.Messages[1].LocalAssets) != 1 || sess.Messages[1].Content != "" {
		t.Fatalf("runtime session did not preserve local attachment: %+v", sess.Messages[1])
	}
	if len(provider.lastMessages) < 2 || len(provider.lastMessages[1].LocalAssets) != 0 {
		t.Fatalf("provider received structured local assets: %+v", provider.lastMessages)
	}
	content := provider.lastMessages[1].Content
	for _, want := range []string{"user-assets/doc/report.pdf", "report.pdf", "application/pdf", "size_bytes=42", "NOT visible", "Do not guess", "read_pdf"} {
		if !strings.Contains(content, want) {
			t.Fatalf("manifest %q does not contain %q", content, want)
		}
	}
	if strings.Contains(content, local.SHA256) {
		t.Fatalf("manifest leaked sha256: %q", content)
	}
}
