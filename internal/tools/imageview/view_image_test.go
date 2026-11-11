package imageview

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/tools"
	"code-agent/internal/uicontract"
)

func TestViewImageSourceOnly(t *testing.T) {
	root := t.TempDir()
	writeTestPNG(t, filepath.Join(root, "screen.png"), 100, 50)
	result := executeViewImage(t, root, tools.ImageCapabilities{Input: true, ToolInput: true, OriginalDetail: true}, `{
		"path":"screen.png","detail":"original"
	}`)
	validateViewImageOutput(t, result)
	if len(result.Assets) != 1 || len(result.Assets[0].Metadata) == 0 {
		t.Fatalf("assets = %+v, want one source asset", result.Assets)
	}
	if result.Assets[0].Metadata["asset_role"] != "source" {
		t.Fatalf("source metadata = %+v", result.Assets[0].Metadata)
	}
	var output viewResult
	mustJSON(t, result.Output, &output)
	if output.CropAssetID != nil || output.SourceRegion != nil || len(output.Assets) != 1 {
		t.Fatalf("output = %+v", output)
	}
}

func TestViewImageCropKeepsSourceFirst(t *testing.T) {
	root := t.TempDir()
	writeTestPNG(t, filepath.Join(root, "screen.png"), 100, 50)
	result := executeViewImage(t, root, tools.ImageCapabilities{Input: true, ToolInput: true, OriginalDetail: true}, `{
		"path":"screen.png","detail":"original","region":{"x":0.2,"y":0.2,"width":0.5,"height":0.4}
	}`)
	validateViewImageOutput(t, result)
	if len(result.Assets) != 2 {
		t.Fatalf("assets = %+v, want source + crop", result.Assets)
	}
	if result.Assets[0].Metadata["asset_role"] != "source" || result.Assets[1].Metadata["asset_role"] != "crop" {
		t.Fatalf("asset roles = %+v, %+v", result.Assets[0].Metadata, result.Assets[1].Metadata)
	}
	if result.Assets[1].Metadata["source_asset_id"] != result.Assets[0].ID {
		t.Fatalf("crop parent = %+v, source = %s", result.Assets[1].Metadata, result.Assets[0].ID)
	}
	crop, err := os.ReadFile(result.Assets[1].AbsolutePath)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(crop))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 50 || cfg.Height != 20 {
		t.Fatalf("crop dimensions = %dx%d, want 50x20", cfg.Width, cfg.Height)
	}
}

func TestViewImageJPEGCrop(t *testing.T) {
	root := t.TempDir()
	writeTestJPEG(t, filepath.Join(root, "photo.jpg"), 80, 40)
	result := executeViewImage(t, root, tools.ImageCapabilities{Input: true, ToolInput: true}, `{
		"path":"photo.jpg","region":{"x":0,"y":0,"width":0.5,"height":0.5}
	}`)
	validateViewImageOutput(t, result)
	if len(result.Assets) != 2 || result.Assets[0].MIMEType != "image/jpeg" || result.Assets[1].MIMEType != "image/png" {
		t.Fatalf("JPEG crop assets = %+v", result.Assets)
	}
}

func TestViewImageWebPSourceOnly(t *testing.T) {
	root := t.TempDir()
	webp := decodeTestWebP(t)
	if err := os.WriteFile(filepath.Join(root, "image.webp"), webp, 0o600); err != nil {
		t.Fatal(err)
	}
	result := executeViewImage(t, root, tools.ImageCapabilities{Input: true, ToolInput: true}, `{"path":"image.webp"}`)
	validateViewImageOutput(t, result)
	if len(result.Assets) != 1 || result.Assets[0].MIMEType != "image/webp" {
		t.Fatalf("WebP assets = %+v", result.Assets)
	}
}

func TestViewImageCapabilitiesFailClosed(t *testing.T) {
	root := t.TempDir()
	writeTestPNG(t, filepath.Join(root, "screen.png"), 10, 10)
	tests := []struct {
		name string
		caps tools.ImageCapabilities
		args string
		want string
	}{
		{"image input", tools.ImageCapabilities{ToolInput: true}, `{"path":"screen.png"}`, "image_input_unsupported"},
		{"tool image input", tools.ImageCapabilities{Input: true}, `{"path":"screen.png"}`, "tool_image_input_unsupported"},
		{"original detail", tools.ImageCapabilities{Input: true, ToolInput: true}, `{"path":"screen.png","detail":"original"}`, "image_detail_original_unsupported"},
		{"bridge is not implicit", tools.ImageCapabilities{Input: true, VisionBridge: true}, `{"path":"screen.png"}`, "tool_image_input_unsupported"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := executeViewImage(t, root, tc.caps, tc.args)
			if result.Failure == nil || result.Failure.Kind != tc.want {
				t.Fatalf("failure = %+v, want %s", result.Failure, tc.want)
			}
		})
	}
}

func TestViewImageRejectsUnsafeAndInvalidInputs(t *testing.T) {
	root := t.TempDir()
	writeTestPNG(t, filepath.Join(root, "screen.png"), 100, 50)
	outside := filepath.Join(t.TempDir(), "outside.png")
	writeTestPNG(t, outside, 10, 10)
	if err := os.Symlink(outside, filepath.Join(root, "escape.png")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "not-image.png"), []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	caps := tools.ImageCapabilities{Input: true, ToolInput: true}
	tests := []struct {
		name string
		args string
		want string
	}{
		{"absolute", `{"path":"/etc/passwd"}`, "blocked"},
		{"traversal", `{"path":"../outside.png"}`, "blocked"},
		{"symlink escape", `{"path":"escape.png"}`, "blocked"},
		{"not image", `{"path":"not-image.png"}`, "unsupported_format"},
		{"bad region", `{"path":"screen.png","region":{"x":0.8,"y":0,"width":0.5,"height":0.5}}`, "invalid_arguments"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := executeViewImage(t, root, caps, tc.args)
			if result.Failure == nil || result.Failure.Kind != tc.want {
				t.Fatalf("failure = %+v, want %s", result.Failure, tc.want)
			}
		})
	}

	t.Run("pixel budget", func(t *testing.T) {
		tool := NewTool()
		tool.MaxImagePixels = 100
		result, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root, ImageCapabilities: caps}, json.RawMessage(`{"path":"screen.png"}`))
		if err != nil || result.Failure == nil || result.Failure.Kind != "image_too_large" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})
	t.Run("file budget", func(t *testing.T) {
		tool := NewTool()
		tool.MaxImageBytes = 10
		result, err := tool.Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: root, ImageCapabilities: caps}, json.RawMessage(`{"path":"screen.png"}`))
		if err != nil || result.Failure == nil || result.Failure.Kind != "image_too_large" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})
	t.Run("webp crop unsupported", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(root, "image.webp"), decodeTestWebP(t), 0o600); err != nil {
			t.Fatal(err)
		}
		result := executeViewImage(t, root, caps, `{"path":"image.webp","region":{"x":0,"y":0,"width":0.2,"height":0.2}}`)
		if result.Failure == nil || result.Failure.Kind != "crop_unsupported" {
			t.Fatalf("failure = %+v", result.Failure)
		}
	})
	t.Run("crop directory symlink escape", func(t *testing.T) {
		symlinkRoot := t.TempDir()
		writeTestPNG(t, filepath.Join(symlinkRoot, "screen.png"), 20, 20)
		outsideDir := t.TempDir()
		if err := os.Symlink(outsideDir, filepath.Join(symlinkRoot, ".codeagent")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		_, err := NewTool().Execute(context.Background(), tools.ExecutionContext{
			WorkspaceRoot:     symlinkRoot,
			ImageCapabilities: caps,
		}, json.RawMessage(`{"path":"screen.png","region":{"x":0,"y":0,"width":0.5,"height":0.5}}`))
		if err == nil {
			t.Fatal("crop directory symlink escape was accepted")
		}
	})
}

func TestViewImageDescriptionAndOutputContainNoDesignDirectives(t *testing.T) {
	tool := NewTool()
	text := strings.ToLower(tool.Description())
	for _, forbidden := range []string{"recommended_component", "layout_should_be", "use_this_view", "recommended for ui", "call me first"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("description contains forbidden workflow/design text %q", forbidden)
		}
	}
	root := t.TempDir()
	writeTestPNG(t, filepath.Join(root, "screen.png"), 10, 10)
	result := executeViewImage(t, root, tools.ImageCapabilities{Input: true, ToolInput: true}, `{"path":"screen.png"}`)
	output := strings.ToLower(string(result.Output))
	for _, forbidden := range []string{"recommended_component", "layout_should_be", "use_this_view", "next_call", "selected_design"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("output contains forbidden design directive %q", forbidden)
		}
	}
}

func TestViewImageInputSchema(t *testing.T) {
	tool := NewTool()
	if err := tools.ValidateInput(tool, json.RawMessage(`{"path":"screen.png"}`)); err != nil {
		t.Fatal(err)
	}
	if err := tools.ValidateInput(tool, json.RawMessage(`{"path":"screen.png","unexpected":true}`)); err == nil {
		t.Fatal("unknown root field accepted")
	}
	if err := tools.ValidateInput(tool, json.RawMessage(`{"path":"screen.png","region":{"x":0}}`)); err == nil {
		t.Fatal("incomplete region accepted")
	}
}

func executeViewImage(t *testing.T, root string, caps tools.ImageCapabilities, args string) tools.ToolResult {
	t.Helper()
	result, err := NewTool().Execute(context.Background(), tools.ExecutionContext{
		WorkspaceRoot:     root,
		SessionID:         "session",
		TurnID:            "turn",
		CallID:            "call",
		ImageCapabilities: caps,
	}, json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func validateViewImageOutput(t *testing.T, result tools.ToolResult) {
	t.Helper()
	if result.Failure != nil {
		t.Fatalf("unexpected failure: %+v", result.Failure)
	}
	schema, err := os.ReadFile(filepath.Join("..", "..", "..", "schemas", "view_image_result.v1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := uicontract.ValidateAgainstSchema(schema, result.Output); err != nil {
		t.Fatal(err)
	}
	if err := uicontract.ValidateViewImageResult(result.Output); err != nil {
		t.Fatal(err)
	}
}

func writeTestPNG(t *testing.T, path string, width, height int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 80, A: 255})
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

func writeTestJPEG(t *testing.T, path string, width, height int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 120, A: 255})
		}
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := jpeg.Encode(file, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
}

func decodeTestWebP(t *testing.T) []byte {
	t.Helper()
	const data = "UklGRrIBAABXRUJQVlA4TKUBAAAvSsAYAA8w//M///eJAkbXva3m8Q3GfYSBJak8Qztm/IcZlgniWImn2BK7aFmBtnVir6q//8VOkFE/xm4baTIu8c48ArEo6+B3zFKYln3pqClSCeeVLH9+gvGTLyqM65PQ44ihzlTXxQKjKbAvshXgir7Lil8BL1F8GKZGVdIAatQa+1IDpt6qyorVwdy01xdW8Jkfk6xjEXmVQQ+HQdFr6OKhIN34dXWq0+0qr6EJSCeeVLH9+gvGTLyqM65PQ44ihzlTXxQKjKbAvshXgir7Lil8BL1F8GKZGVdIAatQa+1IDpt6qyorVwdy01xdW8Jkfk6xjEXmVQQ+HQdFr6OKhIN34dXWq0+0qr6EJSCeeVLH9+gvGTLyqM65PQ44ihzlTXxQKjKbAvshXgir7Lil8BL1F8GKZGVdIAatQa+1IDpt6qyorVwdy01xdW8Jkfk6xjEXmVQQ+HQdFr6OKhIN34dXWq0+0qr6EJSCeeVLH9+gvGTLyqM65PQ44ihzlTXxQKjKbAvshXgir7Lil8BL1F8GKZGVdIAatQa+1IDpt6qyorVwdy01xdW8Jkfk6xjEXmVQQ+HQdFr6OKhIN34dXWq0+0qr6EJSCeeVLH9+gvGTLyqM65PQ44ihzlTXxQKjKbAvshXgir7Lil8BL1F8GKZGVdIAatQa+1IDpt6qyorVwdy01xdW8Jkfk6xjEXmVQQ+HQdFr6OKhIN34dXWq0+0qr6EJSCeeVLH9+gvGTLyqM65PQ44ihzlTXxQKjKbAvshXgir7Lil8BL1F8GKZGVdIAatQa+1IDpt6qyorVwdy01xdW8Jkfk6xjEXmVQQ+HQdFr6OKhIN34dXWq0+0qr6EJSCeeVLH9+gvGTLyqM65PQ44ihzlTXxQKjKbAvshXgir7Lil8BL1F8GKZGVdIAatQa+1IDpt6qyorVwdy01xdW8Jkfk6xjEXmVQQ+HQdFr6OKhIN34dXWq0+0qr6EJSCeeVLH9+gvGTLyqM65PQ44ihzlTXxQKjKbAvshXgir7Lil8BL1F8G"
	bytesValue, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatal(err)
	}
	return bytesValue
}

func mustJSON(t *testing.T, data []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}
