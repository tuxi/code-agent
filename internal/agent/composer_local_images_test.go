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

func TestComposerTwoLocalImagesAndPathReferencesReachProvider(t *testing.T) {
	root := t.TempDir()
	images := [][]byte{
		{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 1},
		{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 2},
	}
	refs := make([]model.LocalAssetRef, 0, 2)
	for i, name := range []string{"first.png", "second.png"} {
		if err := os.WriteFile(filepath.Join(root, name), images[i], 0600); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, visionLocalAsset(t, root, name, name, "image/png", "image", int64(len(images[i]))))
	}
	text := "检查更新\n\n引用的本地路径（按需读取）：\n- 目录：\"/outside/ai\"\n- 文件：\"/outside/app.dmg\"\n- 文件：\"/outside/movie.m4v\""
	provider := &scriptedProvider{responses: []model.Response{{Content: "ok"}}}
	runner := &Runner{Model: provider, Tools: tools.NewRegistry(), MaxSteps: 1, WorkspaceRoot: root, VisionSupported: true}
	sess := &session.Session{ID: "s", Messages: []model.Message{{Role: model.RoleSystem, Content: "test"}}, Metadata: map[string]any{}}
	if _, err := runner.RunTurnWithAllAssets(context.Background(), sess, text, nil, refs); err != nil {
		t.Fatal(err)
	}
	user := provider.lastMessages[1]
	if len(user.ContentParts) != 2 {
		t.Fatalf("provider has %d image parts, want 2", len(user.ContentParts))
	}
	for i, part := range user.ContentParts {
		expected := "data:image/png;base64," + base64.StdEncoding.EncodeToString(images[i])
		if part.Type != "image_url" || part.ImageURL == nil || part.ImageURL.URL != expected {
			t.Fatalf("image %d was not delivered as image content", i)
		}
	}
	for _, path := range []string{"/outside/ai", "/outside/app.dmg", "/outside/movie.m4v"} {
		if !strings.Contains(user.Content, path) {
			t.Fatalf("reference %s missing from model text", path)
		}
	}
	if len(user.Assets) != 0 || len(user.LocalAssets) != 0 {
		t.Fatal("provider must receive image content, not transport-only asset references")
	}
	if len(sess.Messages[1].LocalAssets) != 2 || len(sess.Messages[1].ContentParts) != 0 {
		t.Fatal("history must retain local references instead of image bytes")
	}
}
