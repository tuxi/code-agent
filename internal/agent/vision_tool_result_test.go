package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/assetref"
	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/tools"
	"code-agent/internal/tools/filesystem"
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
	ref := assets.Ref{ID: "img1", Kind: "image", MIMEType: t.mime, AbsolutePath: t.path}
	return tools.ToolResult{
		Content:      "captured",
		Assets:       []assets.Ref{ref},
		VisionInputs: []assets.Ref{ref},
	}, nil
}

func TestDirectoryImagesAreNavigationOnly(t *testing.T) {
	root := t.TempDir()
	const imageCount = 61
	for i := 0; i < imageCount; i++ {
		writeRuntimeTestPNG(t, filepath.Join(root, fmt.Sprintf("image-%02d.png", i)), 1, 1)
	}
	call := model.ToolCall{ID: "listing", Type: "function", Function: model.FunctionCall{Name: "list_files", Arguments: `{"path":"."}`}}
	provider := &scriptedProvider{responses: []model.Response{{ToolCalls: []model.ToolCall{call}}, {Content: "done"}}}
	reg := tools.NewRegistry()
	if err := reg.Register(filesystem.NewListFilesTool()); err != nil {
		t.Fatal(err)
	}
	var finished Event
	runner := &Runner{
		Model: provider, Tools: reg, MaxSteps: 2, WorkspaceRoot: root,
		VisionSupported: true, ToolImageInputSupported: true,
		Emitter: validationEmitter(func(event Event) {
			if event.Kind == EventToolFinished {
				finished = event
			}
		}),
	}
	sess := newSession()
	if _, err := runner.RunTurn(context.Background(), sess, "list files"); err != nil {
		t.Fatal(err)
	}
	if len(finished.Assets) != imageCount {
		t.Fatalf("navigation assets = %d, want %d", len(finished.Assets), imageCount)
	}
	for _, ref := range finished.Assets {
		if ref.Kind != "image" || ref.MIMEType != "image/png" || ref.URI == "" || ref.WorkspaceRelativePath == "" {
			t.Fatalf("invalid navigation asset: %+v", ref)
		}
	}
	tool := sess.Messages[len(sess.Messages)-2]
	if tool.ToolCallID != call.ID || len(tool.LocalAssets) != 0 {
		t.Fatalf("stored listing carries image attachments: %+v", tool)
	}
	for _, msg := range provider.lastRequest.Messages {
		if len(msg.ContentParts) != 0 {
			t.Fatalf("provider received image parts on %s message %q", msg.Role, msg.ToolCallID)
		}
	}
}

func TestLegacyDirectoryImagesAreOmittedOnReplay(t *testing.T) {
	root := t.TempDir()
	path := "shared.png"
	writeRuntimeTestPNG(t, filepath.Join(root, path), 1, 1)
	ref := model.LocalAssetRef{ID: "legacy", RelativePath: path, Filename: path, MIMEType: "image/png", Kind: "image"}
	// The same directory entry was explicitly viewed in the same tool batch.
	list := model.ToolCall{ID: "list", Type: "function", Function: model.FunctionCall{Name: "list_files", Arguments: "{}"}}
	view := model.ToolCall{ID: "view", Type: "function", Function: model.FunctionCall{Name: "view_image", Arguments: "{}"}}
	recall := model.ToolCall{ID: "recall", Type: "function", Function: model.FunctionCall{Name: "recall_memory", Arguments: "{}"}}
	ctx := context.Background()
	store, sess := runtimeFixture(t)
	sess.Messages = append(sess.Messages,
		model.Message{Role: model.RoleUser, Content: "analyze structure"},
		model.Message{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{list, recall, view}},
		model.Message{Role: model.RoleTool, ToolCallID: list.ID, Content: path, LocalAssets: []model.LocalAssetRef{ref}},
		model.Message{Role: model.RoleTool, ToolCallID: recall.ID, Content: "no memory"},
		model.Message{Role: model.RoleTool, ToolCallID: view.ID, Content: "viewed", LocalAssets: []model.LocalAssetRef{ref}},
		model.Message{Role: model.RoleAssistant, Content: "done"},
	)
	if err := store.Save(ctx, sess); err != nil {
		t.Fatal(err)
	}
	sess, err := store.Load(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []model.Response{{Content: "done"}}}
	runner := &Runner{Model: provider, WorkspaceRoot: root, VisionSupported: true, ToolImageInputSupported: true}
	if _, err := runner.RunTurnWithAllAssets(ctx, sess, "compare with attached image", nil, []model.LocalAssetRef{ref}); err != nil {
		t.Fatal(err)
	}
	var viewed, attached bool
	for _, msg := range provider.lastRequest.Messages {
		switch {
		case msg.ToolCallID == list.ID:
			if len(msg.ContentParts) != 0 || msg.Content != path {
				t.Fatalf("provider listing = %+v, want original text only", msg)
			}
		case msg.ToolCallID == view.ID:
			viewed = len(msg.ContentParts) == 1 && msg.ContentParts[0].Type == "image_url"
		case msg.Role == model.RoleUser && strings.HasPrefix(msg.Content, "compare with attached image"):
			attached = len(msg.ContentParts) == 1 && msg.ContentParts[0].Type == "image_url"
		}
	}
	if !viewed || !attached || len(sess.Messages[3].LocalAssets) != 1 {
		t.Fatalf("view image sent = %t, user image sent = %t, stored listing = %+v", viewed, attached, sess.Messages[3])
	}
}

func TestClientCaptureSendsOnlyDeclaredImages(t *testing.T) {
	root := t.TempDir()
	capture := filepath.Join(root, "capture.png")
	writeRuntimeTestPNG(t, capture, 1, 1)
	other := filepath.Join(root, "other.png")
	writeRuntimeTestPNG(t, other, 2, 2)
	clientResult := ToolCallResult{
		Content: "captured",
		Assets: []assets.Ref{
			{ID: "navigation", Kind: "image", MIMEType: "image/png", AbsolutePath: other},
			{ID: "capture", Kind: "image", MIMEType: "image/png", AbsolutePath: capture},
		},
		VisionAssetIDs: []string{"capture", "missing", "capture"},
	}
	for _, tc := range []struct {
		name               string
		vision, toolImages bool
		want               int
	}{
		{name: "vision", vision: true, toolImages: true, want: 1},
		{name: "user images only", vision: true},
		{name: "text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := model.ToolCall{ID: "capture", Type: "function", Function: model.FunctionCall{Name: "capture_screen", Arguments: "{}"}}
			provider := &scriptedProvider{responses: []model.Response{{ToolCalls: []model.ToolCall{call}}, {Content: "done"}}}
			reg := tools.NewRegistry()
			if err := reg.Register(tools.NewClientProxyTool("capture_screen", "capture on device", json.RawMessage(`{"type":"object"}`))); err != nil {
				t.Fatal(err)
			}
			runner := &Runner{
				Model: provider, Tools: reg, MaxSteps: 2, WorkspaceRoot: root,
				VisionSupported: tc.vision, ToolImageInputSupported: tc.toolImages,
				ClientWaiter: &immediateClientWaiter{result: clientResult},
			}
			sess := newSession()
			if _, err := runner.RunTurn(context.Background(), sess, "capture screen"); err != nil {
				t.Fatal(err)
			}
			req := provider.lastRequest.Messages
			last := req[len(req)-1]
			stored := sess.Messages[len(sess.Messages)-2]
			if last.ToolCallID != call.ID || len(last.ContentParts) != tc.want || len(stored.LocalAssets) != tc.want {
				t.Fatalf("provider parts = %d, stored refs = %+v, want %d", len(last.ContentParts), stored.LocalAssets, tc.want)
			}
			if tc.want > 0 && (last.ContentParts[0].Type != "image_url" || stored.LocalAssets[0].RelativePath != "capture.png") {
				t.Fatalf("provider part = %+v, stored ref = %+v", last.ContentParts[0], stored.LocalAssets[0])
			}
		})
	}
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
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 2, WorkspaceRoot: root, VisionSupported: true, ToolImageInputSupported: true}
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
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 2, WorkspaceRoot: root, VisionSupported: true, ToolImageInputSupported: true}
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
	runner := &Runner{WorkspaceRoot: root, VisionSupported: true, ToolImageInputSupported: true}
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

// TestTextModeMarksToolLocalAssetsOmitted verifies a non-vision runner strips
// tool-message LocalAssets but states that they were not visible.
func TestTextModeMarksToolLocalAssetsOmitted(t *testing.T) {
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
	if !strings.Contains(tool.Content, "cannot process image input") || !strings.Contains(tool.Content, "omitted") {
		t.Errorf("content = %q, want explicit omission", tool.Content)
	}
}

func TestToolImageInputUnsupportedOmitsToolImages(t *testing.T) {
	root := t.TempDir()
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	if err := os.WriteFile(filepath.Join(root, "shot.png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{WorkspaceRoot: root, VisionSupported: true, ToolImageInputSupported: false}
	msgs := []model.Message{
		{Role: model.RoleUser, Content: "go"},
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "c1", Type: "function", Function: model.FunctionCall{Name: "view_image"}}}},
		{Role: model.RoleTool, ToolCallID: "c1", Content: "captured", LocalAssets: []model.LocalAssetRef{{
			ID: "a1", RelativePath: "shot.png", Filename: "shot.png", MIMEType: "image/png", Kind: "image", SizeBytes: int64(len(png)),
		}}},
	}
	out := runner.withLocalAssetManifests(msgs)
	tool := out[2]
	if len(tool.ContentParts) != 0 {
		t.Fatalf("tool ContentParts = %+v, want none", tool.ContentParts)
	}
	if !strings.Contains(tool.Content, "tool results") || !strings.Contains(tool.Content, "omitted") {
		t.Fatalf("tool content = %q, want explicit omission", tool.Content)
	}
	if len(tool.LocalAssets) != 0 {
		t.Fatalf("tool LocalAssets = %+v, want stripped", tool.LocalAssets)
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
	runner := &Runner{WorkspaceRoot: root, VisionSupported: true, ToolImageInputSupported: true}
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
