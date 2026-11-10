package r4input

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code-agent/internal/preview"
)

func testPNG(t *testing.T, shade uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 12, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 12; x++ {
			img.Set(x, y, color.RGBA{R: shade, G: uint8(x * 10), B: uint8(y * 20), A: 255})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func defaults(fallbackPath string) Defaults {
	return Defaults{
		ViewerHTML: "<div>default</div>", FallbackPath: fallbackPath,
		Title: "Default title", Summary: "Default summary", ArtifactKey: "default-key",
		FallbackText: "Fallback", Alt: "Alt", PreferredAspectRatio: 1.5, MinHeight: 360,
	}
}

func TestPrepareDefaultsAndCustomFileBytes(t *testing.T) {
	workspace := t.TempDir()
	fallback := filepath.Join(workspace, "assets", "fallback.png")
	writeFile(t, fallback, testPNG(t, 10))
	prepared, err := Prepare(Options{}, defaults("assets/fallback.png"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ViewerHTML != "<div>default</div>" || prepared.Title != "Default title" || prepared.ArtifactKey != "default-key" {
		t.Fatalf("default prepared = %+v", prepared)
	}

	html := []byte("<main>custom preview bytes</main>")
	htmlPath := filepath.Join(t.TempDir(), "custom.html")
	writeFile(t, htmlPath, html)
	fallbackBytes := testPNG(t, 200)
	fallbackPath := filepath.Join(t.TempDir(), "custom.png")
	writeFile(t, fallbackPath, fallbackBytes)
	custom, err := Prepare(Options{
		ViewerHTMLFile: htmlPath, FallbackFile: fallbackPath,
		Title: "Custom title", Summary: "Custom summary", ArtifactKey: "custom-key",
		PreferredAspectRatio: 1.78, MinHeight: 420,
	}, defaults("assets/fallback.png"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if custom.ViewerHTML != string(html) {
		t.Fatalf("viewer bytes changed: %q", custom.ViewerHTML)
	}
	if custom.Title != "Custom title" || custom.Summary != "Custom summary" || custom.ArtifactKey != "custom-key" || custom.PreferredAspectRatio != 1.78 || custom.MinHeight != 420 {
		t.Fatalf("custom metadata = %+v", custom)
	}
	if filepath.IsAbs(custom.FallbackPath) || !strings.HasPrefix(custom.FallbackPath, ".codeagent/assets/client/r4-input/") {
		t.Fatalf("fallback path is not controlled/workspace-relative: %q", custom.FallbackPath)
	}
	copied, err := os.ReadFile(filepath.Join(workspace, filepath.FromSlash(custom.FallbackPath)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(copied, fallbackBytes) || custom.FallbackSHA256 != digestBytes(fallbackBytes) {
		t.Fatal("external fallback bytes were not copied exactly")
	}
}

func TestPrepareRejectsEmptyAndOversizeViewer(t *testing.T) {
	workspace := t.TempDir()
	writeFile(t, filepath.Join(workspace, "assets", "fallback.png"), testPNG(t, 1))
	empty := filepath.Join(t.TempDir(), "empty.html")
	writeFile(t, empty, nil)
	if _, err := Prepare(Options{ViewerHTMLFile: empty}, defaults("assets/fallback.png"), workspace); err == nil {
		t.Fatal("empty viewer HTML was accepted")
	}
	oversize := filepath.Join(t.TempDir(), "oversize.html")
	writeFile(t, oversize, bytes.Repeat([]byte("x"), preview.MaxViewerBytes+1))
	if _, err := Prepare(Options{ViewerHTMLFile: oversize}, defaults("assets/fallback.png"), workspace); err == nil {
		t.Fatal("oversize viewer HTML was accepted")
	}
}

func TestPrepareCopiesValidWebPFallback(t *testing.T) {
	const webpBase64 = "UklGRrIBAABXRUJQVlA4TKUBAAAvSsAYAA8w//M///MfeJAkbXvaSG7m8Q3GfYSBJekwQztm/IcZlgwnmWImn2BK7aFmBtnVir6q//8VOkFE/xm4baTIu8c48ArEo6+B3zFKYln3pqClSCKX0begFTAXFOLXHSyF8cCNcZEG4OywuA4KVVfJCiArU7GAgJI8+lJP/OKMT/fBAjevg1cYB7YVkFuWga2lyPi5I0HFy5YTpWIHg0RZpkniRVW9odHAKOwosWuOGdxIyn2OvaCDvhg/we6TwadPBPbqBV58MsLmMJ8yZnOWk8SRz4N+QoyPL+MnamzMvcE1rHNEr91F9GKZPVUcS9w7PhhH36suB9qPeYb/oLk6cuTiJ0wOK3m5h1cKjW6EVZCYMK7dxcKCBdgP9HkKr9gkAO2P8GKZGWVdIAatQa+1IDpt6qyorVwdy01xdW8Jkfk6xjEXmVQQ+HQdFr6OKhIN34dXWq0+0qr6EJSCeeVLH9+gvGTLyqM65PQ44ihzlTXxQKjKbAvshXgir7Lil9w4L2bvMycmjQcqXaMCO6BlY28i+FOLzbfI1vEqxAhotocAAA=="
	webpData, err := base64.StdEncoding.DecodeString(webpBase64)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	writeFile(t, filepath.Join(workspace, "assets", "fallback.png"), testPNG(t, 1))
	source := filepath.Join(t.TempDir(), "fallback.webp")
	writeFile(t, source, webpData)
	prepared, err := Prepare(Options{FallbackFile: source}, defaults("assets/fallback.png"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(prepared.FallbackPath, ".webp") {
		t.Fatalf("WebP fallback path = %q", prepared.FallbackPath)
	}
	copied, err := os.ReadFile(filepath.Join(workspace, filepath.FromSlash(prepared.FallbackPath)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(copied, webpData) {
		t.Fatal("WebP fallback bytes changed")
	}
}

func TestPrepareRejectsInvalidFallback(t *testing.T) {
	workspace := t.TempDir()
	writeFile(t, filepath.Join(workspace, "assets", "fallback.png"), testPNG(t, 1))
	bad := filepath.Join(t.TempDir(), "bad.png")
	writeFile(t, bad, []byte("not an image"))
	if _, err := Prepare(Options{FallbackFile: bad}, defaults("assets/fallback.png"), workspace); err == nil {
		t.Fatal("invalid fallback image was accepted")
	}
}
