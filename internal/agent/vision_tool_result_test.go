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

// imageTool returns a tool whose result carries an image asset ref.
type imageTool struct {
	path string
	mime string
}

func (t imageTool) Name() string        { return "snap" }
func (t imageTool) Description() string { return "produces an image" }
func (t imageTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (t imageTool) Execute(context.Context, tools.ExecutionContext, json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{
		Content: "captured",
		Assets: []assets.Ref{{
			ID: "img1", Kind: "image", MIMEType: t.mime, AbsolutePath: t.path,
		}},
	}, nil
}

// runImageToolTurn drives one turn where the model calls the image tool once.
func runImageToolTurn(t *testing.T, runner *Runner) *session.Session {
	t.Helper()
	sess := newSession()
	calls := []model.ToolCall{{ID: "call_img", Type: "function", Function: model.FunctionCall{Name: "snap", Arguments: "{}"}}}
	provider := runner.Model.(*scriptedProvider)
	provider.responses = []model.Response{
		{ToolCalls: calls, FinishReason: "tool_calls"},
		{Content: "done"},
	}
	if _, err := runner.RunTurn(context.Background(), sess, "look"); err != nil {
		t.Fatal(err)
	}
	return sess
}

// TestVisionToolResultStagesLocalAsset verifies a vision runner persists a
// workspace-relative LocalAssetRef on the tool message for an in-workspace
// image, and that history keeps no ContentParts.
func TestVisionToolResultStagesLocalAsset(t *testing.T) {
	root := t.TempDir()
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	imgPath := filepath.Join(root, "shot.png")
	if err := os.WriteFile(imgPath, png, 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 2, WorkspaceRoot: root, VisionSupported: true}
	if err := runner.Tools.Register(imageTool{path: imgPath, mime: "image/png"}); err != nil {
		t.Fatal(err)
	}
	sess := runImageToolTurn(t, runner)

	var toolMsg *model.Message
	for i := range sess.Messages {
		if sess.Messages[i].Role == model.RoleTool {
			toolMsg = &sess.Messages[i]
		}
	}
	if toolMsg == nil {
		t.Fatal("no tool message in history")
	}
	if len(toolMsg.LocalAssets) != 1 {
		t.Fatalf("tool message LocalAssets = %+v, want 1", toolMsg.LocalAssets)
	}
	ref := toolMsg.LocalAssets[0]
	if ref.RelativePath != "shot.png" || ref.MIMEType != "image/png" || ref.Kind != "image" {
		t.Errorf("ref = %+v", ref)
	}
	if len(toolMsg.ContentParts) != 0 {
		t.Errorf("history carries ContentParts: %+v", toolMsg.ContentParts)
	}
}

// TestVisionToolResultStagesOutOfWorkspaceCapture verifies a client capture
// written outside the workspace is staged into .codeagent/assets/client/ and
// referenced from there.
func TestVisionToolResultStagesOutOfWorkspaceCapture(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "photo.png")
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	if err := os.WriteFile(outside, png, 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 2, WorkspaceRoot: root, VisionSupported: true}
	if err := runner.Tools.Register(imageTool{path: outside, mime: "image/png"}); err != nil {
		t.Fatal(err)
	}
	sess := runImageToolTurn(t, runner)

	for i := range sess.Messages {
		if sess.Messages[i].Role != model.RoleTool {
			continue
		}
		refs := sess.Messages[i].LocalAssets
		if len(refs) != 1 {
			t.Fatalf("LocalAssets = %+v, want 1 staged ref", refs)
		}
		if !strings.HasPrefix(refs[0].RelativePath, ".codeagent/assets/client/") {
			t.Errorf("relative path = %q, want staged under .codeagent/assets/client/", refs[0].RelativePath)
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(refs[0].RelativePath))); err != nil {
			t.Errorf("staged file missing: %v", err)
		}
		return
	}
	t.Fatal("no tool message")
}

// TestVisionWireTransformerInlinesToolImages verifies the request-time
// transformer converts a tool message's LocalAssets into image parts on that
// message (the provider decides final placement).
func TestVisionWireTransformerInlinesToolImages(t *testing.T) {
	root := t.TempDir()
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	if err := os.WriteFile(filepath.Join(root, "shot.png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{WorkspaceRoot: root, VisionSupported: true}
	msgs := []model.Message{
		{Role: model.RoleSystem, Content: "sys"},
		{Role: model.RoleUser, Content: "go"},
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "c1", Type: "function", Function: model.FunctionCall{Name: "snap"}}}},
		{Role: model.RoleTool, ToolCallID: "c1", Content: "captured", LocalAssets: []model.LocalAssetRef{{
			ID: "a1", RelativePath: "shot.png", Filename: "shot.png", MIMEType: "image/png", Kind: "image", SizeBytes: int64(len(png)),
		}}},
	}
	out := runner.withLocalAssetManifests(msgs)
	tool := out[3]
	if len(tool.ContentParts) != 1 || tool.ContentParts[0].Type != "image_url" {
		t.Fatalf("tool ContentParts = %+v, want one image part", tool.ContentParts)
	}
	if !strings.Contains(tool.Content, "stored in the workspace at") {
		t.Errorf("tool content %q should carry the path note", tool.Content)
	}
	if len(tool.LocalAssets) != 0 {
		t.Errorf("structured refs leaked to provider message: %+v", tool.LocalAssets)
	}
}

// TestTextModeStripsToolLocalAssetsSilently verifies a non-vision runner strips
// tool-message LocalAssets without adding manifest text — behavior unchanged.
func TestTextModeStripsToolLocalAssetsSilently(t *testing.T) {
	msgs := []model.Message{
		{Role: model.RoleUser, Content: "go"},
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "c1", Type: "function", Function: model.FunctionCall{Name: "snap"}}}},
		{Role: model.RoleTool, ToolCallID: "c1", Content: "captured", LocalAssets: []model.LocalAssetRef{{
			ID: "a1", RelativePath: "shot.png", Filename: "shot.png", MIMEType: "image/png", Kind: "image", SizeBytes: 12,
		}}},
	}
	out := withTextAssetManifests(msgs)
	tool := out[2]
	if len(tool.ContentParts) != 0 || len(tool.LocalAssets) != 0 {
		t.Fatalf("tool message not stripped: %+v", tool)
	}
	if strings.Contains(tool.Content, "manifest") || strings.Contains(tool.Content, "NOT visible") {
		t.Errorf("manifest text leaked into tool observation: %q", tool.Content)
	}
	if tool.Content != "captured" {
		t.Errorf("content = %q, want unchanged observation", tool.Content)
	}
}

// TestSharedBudgetAcrossUserAndToolImages verifies the per-request budget is
// shared in message order: when the user image consumes it, the later tool
// image degrades to a note instead of failing.
func TestSharedBudgetAcrossUserAndToolImages(t *testing.T) {
	root := t.TempDir()
	big := make([]byte, visionMaxTotalImgBytes-10)
	big[0], big[1], big[2] = 0xFF, 0xD8, 0xFF // JPEG magic
	if err := os.WriteFile(filepath.Join(root, "user.jpg"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	small := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	if err := os.WriteFile(filepath.Join(root, "tool.png"), small, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{WorkspaceRoot: root, VisionSupported: true}
	msgs := []model.Message{
		{Role: model.RoleUser, Content: "go", LocalAssets: []model.LocalAssetRef{{
			ID: "u1", RelativePath: "user.jpg", Filename: "user.jpg", MIMEType: "image/jpeg", Kind: "image", SizeBytes: int64(len(big)),
		}}},
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "c1", Type: "function", Function: model.FunctionCall{Name: "snap"}}}},
		{Role: model.RoleTool, ToolCallID: "c1", Content: "captured", LocalAssets: []model.LocalAssetRef{{
			ID: "t1", RelativePath: "tool.png", Filename: "tool.png", MIMEType: "image/png", Kind: "image", SizeBytes: int64(len(small)),
		}}},
	}
	out := runner.withLocalAssetManifests(msgs)
	if len(out[0].ContentParts) != 1 {
		t.Fatalf("user parts = %+v, want the user image inlined", out[0].ContentParts)
	}
	if len(out[2].ContentParts) != 0 {
		t.Fatalf("tool parts = %+v, want none (budget exhausted)", out[2].ContentParts)
	}
	if !strings.Contains(out[2].Content, "omitted") {
		t.Errorf("tool content %q should carry the omission note", out[2].Content)
	}
}
