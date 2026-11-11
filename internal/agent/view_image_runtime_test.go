package agent

import (
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"code-agent/internal/model"
	"code-agent/internal/session"
	"code-agent/internal/tools"
	"code-agent/internal/tools/imageview"
	"code-agent/internal/uicontract"
)

func TestViewImageInjectsSourceAndCropAssets(t *testing.T) {
	root := t.TempDir()
	writeRuntimeTestPNG(t, filepath.Join(root, "screen.png"), 80, 40)

	provider := &scriptedProvider{responses: []model.Response{
		{
			ToolCalls: []model.ToolCall{{
				ID:   "view_1",
				Type: "function",
				Function: model.FunctionCall{
					Name:      "view_image",
					Arguments: `{"path":"screen.png","detail":"original","region":{"x":0,"y":0,"width":0.5,"height":0.5}}`,
				},
			}},
			FinishReason: "tool_calls",
		},
		{Content: "done", FinishReason: "stop"},
	}}
	registry := tools.NewRegistry()
	if err := registry.Register(imageview.NewTool()); err != nil {
		t.Fatal(err)
	}
	var finished Event
	runner := &Runner{
		Model:                        provider,
		Tools:                        registry,
		MaxSteps:                     2,
		WorkspaceRoot:                root,
		VisionSupported:              true,
		ToolImageInputSupported:      true,
		ImageDetailOriginalSupported: true,
		Emitter: validationEmitter(func(event Event) {
			if event.Kind == EventToolFinished {
				finished = event
			}
		}),
	}
	sess := &session.Session{
		ID:       "session",
		Messages: []model.Message{{Role: model.RoleSystem, Content: "test"}},
		Metadata: map[string]any{},
	}
	if _, err := runner.RunTurn(context.Background(), sess, "inspect the image"); err != nil {
		t.Fatal(err)
	}

	if len(finished.Assets) != 2 {
		t.Fatalf("tool_finished assets = %+v, want source + crop", finished.Assets)
	}
	if finished.Assets[0].Metadata["asset_role"] != "source" || finished.Assets[1].Metadata["asset_role"] != "crop" {
		t.Fatalf("asset roles = %+v, %+v", finished.Assets[0].Metadata, finished.Assets[1].Metadata)
	}
	if finished.Assets[1].Metadata["source_asset_id"] != finished.Assets[0].ID {
		t.Fatalf("crop parent = %+v, source = %s", finished.Assets[1].Metadata, finished.Assets[0].ID)
	}
	schema, err := os.ReadFile(filepath.Join("..", "..", "schemas", "view_image_result.v1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := uicontract.ValidateAgainstSchema(schema, finished.Output); err != nil {
		t.Fatalf("tool_finished output schema: %v", err)
	}
	if err := uicontract.ValidateViewImageResult(finished.Output); err != nil {
		t.Fatalf("tool_finished output semantics: %v", err)
	}

	var toolMessage *model.Message
	for i := range sess.Messages {
		if sess.Messages[i].Role == model.RoleTool {
			toolMessage = &sess.Messages[i]
		}
	}
	if toolMessage == nil || len(toolMessage.LocalAssets) != 2 {
		t.Fatalf("tool message = %+v, want two LocalAssets", toolMessage)
	}
	if toolMessage.LocalAssets[0].Filename != "screen.png" || toolMessage.LocalAssets[0].Kind != "image" {
		t.Fatalf("source LocalAsset = %+v", toolMessage.LocalAssets[0])
	}

	secondRequestTool := provider.lastMessages[len(provider.lastMessages)-1]
	if secondRequestTool.Role != model.RoleTool || len(secondRequestTool.ContentParts) != 2 {
		t.Fatalf("provider tool message = %+v, want two image content parts", secondRequestTool)
	}
}

func writeRuntimeTestPNG(t *testing.T, path string, width, height int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 90, A: 255})
		}
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := png.Encode(file, img); err != nil {
		t.Fatal(err)
	}
}
