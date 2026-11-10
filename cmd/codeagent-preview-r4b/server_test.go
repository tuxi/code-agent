package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-agent/internal/preview"

	"github.com/coder/websocket"
)

func TestR4BReadinessSupportsNormalTurnLivePreviewAndReplay(t *testing.T) {
	srv, ready, err := Start(context.Background(), ServerOptions{
		Workspace: t.TempDir(), StateDir: t.TempDir(), TurnPrompt: DefaultTurnPrompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if ready.BaseURL == "" || ready.BearerToken == "" || ready.SessionID == "" || ready.TurnPrompt != DefaultTurnPrompt {
		t.Fatalf("readiness = %+v", ready)
	}
	initial := getEventFrames(t, ready, false)
	if len(initial) != 0 {
		t.Fatalf("pre-trigger replay returned %d events", len(initial))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wsURL := strings.Replace(ready.BaseURL, "http://", "ws://", 1) + "/v1/conversations/" + ready.SessionID + "/stream?since=0"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + ready.BearerToken}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatalf("read hello: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"client_capabilities","capabilities":["preview_contract_v1","preview_interactive_html_v1","preview_static_asset_v1","preview_replay_v1"]}`)); err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]any{
		"type": "agent_input", "kind": "text", "request_id": "r4b-trigger-1", "text": ready.TurnPrompt,
	})
	if err := conn.Write(ctx, websocket.MessageText, input); err != nil {
		t.Fatal(err)
	}

	var live preview.Event
	for live.Artifact == nil {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var frame struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(data, &frame) != nil || frame.Kind != "preview_artifact" {
			continue
		}
		live, err = preview.DecodeEvent(data)
		if err != nil {
			t.Fatal(err)
		}
	}
	if live.Seq <= 0 || live.Artifact == nil || live.Artifact.SessionID != ready.SessionID || live.Artifact.Status != preview.StatusReady {
		t.Fatalf("live preview = %+v", live)
	}
	if err := preview.ValidateEventCorrelation(live); err != nil {
		t.Fatal(err)
	}
	t.Logf("session=%s turn=%s artifact=%s revision=%d seq=%d viewer=%s", ready.SessionID, live.TurnID, live.Artifact.ArtifactID, live.Artifact.Revision, live.Seq, live.Artifact.Viewer.SHA256)

	replay := getEventFrames(t, ready, true)
	var replayed *preview.Event
	for _, frame := range replay {
		event, err := preview.DecodeEvent(frame)
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind == "preview_artifact" && event.Artifact != nil && event.Artifact.ArtifactID == live.Artifact.ArtifactID && event.Artifact.Revision == live.Artifact.Revision {
			copyEvent := event
			replayed = &copyEvent
		}
	}
	if replayed == nil || replayed.Seq != live.Seq || replayed.Artifact.RevisionSHA256 != live.Artifact.RevisionSHA256 {
		t.Fatalf("replayed preview = %+v; live = %+v", replayed, live)
	}

	viewerURL := ready.BaseURL + preview.ViewerURL(ready.SessionID, live.Artifact.ArtifactID, live.Artifact.Revision)
	assetURL := ready.BaseURL + preview.AssetURL(ready.SessionID, live.Artifact.ArtifactID, live.Artifact.Revision, live.Artifact.Fallback.AssetID)
	viewer, viewerResp := getAuthorized(t, viewerURL, ready.BearerToken, nil)
	if viewerResp.StatusCode != http.StatusOK || viewerResp.Header.Get("Content-Type") != preview.ViewerMIMEType || int64(len(viewer)) != live.Artifact.Viewer.SizeBytes || !preview.DigestMatches(live.Artifact.Viewer.SHA256, viewer) {
		t.Fatalf("viewer response = %d %q %d", viewerResp.StatusCode, viewerResp.Header.Get("Content-Type"), len(viewer))
	}
	asset, assetResp := getAuthorized(t, assetURL, ready.BearerToken, nil)
	if assetResp.StatusCode != http.StatusOK || assetResp.Header.Get("Content-Type") != "image/png" || !preview.DigestMatches(fallbackAsset(*live.Artifact).SHA256, asset) {
		t.Fatalf("asset response = %d %q %d", assetResp.StatusCode, assetResp.Header.Get("Content-Type"), len(asset))
	}
	if _, _, err := image.DecodeConfig(bytes.NewReader(asset)); err != nil {
		t.Fatalf("fallback PNG decode: %v", err)
	}

	_, unauthorized := getAuthorized(t, viewerURL, "", nil)
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized viewer status = %d", unauthorized.StatusCode)
	}
	if _, _, err := websocket.Dial(ctx, wsURL, nil); err == nil {
		t.Fatal("unauthenticated WebSocket dial succeeded")
	}
}

func fallbackAsset(a preview.Artifact) preview.Asset {
	for _, asset := range a.Assets {
		if a.Fallback != nil && asset.AssetID == a.Fallback.AssetID {
			return asset
		}
	}
	return preview.Asset{}
}

func getEventFrames(t *testing.T, ready Readiness, authorized bool) []json.RawMessage {
	t.Helper()
	var headers http.Header
	if authorized {
		headers = http.Header{"Authorization": []string{"Bearer " + ready.BearerToken}}
	}
	token := ""
	if authorized {
		token = ready.BearerToken
	}
	body, resp := getAuthorized(t, ready.BaseURL+"/v1/conversations/"+ready.SessionID+"/events?since=0", token, headers)
	if authorized && resp.StatusCode != http.StatusOK {
		t.Fatalf("events status = %d %s", resp.StatusCode, body)
	}
	if !authorized && resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized events status = %d", resp.StatusCode)
	}
	if !authorized {
		return nil
	}
	var envelope struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data
}

func getAuthorized(t *testing.T, url, token string, headers http.Header) ([]byte, *http.Response) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body, resp
}

func TestR4BPublicHealthAndModelCatalog(t *testing.T) {
	srv, ready, err := Start(context.Background(), ServerOptions{
		Workspace: t.TempDir(), StateDir: t.TempDir(), TurnPrompt: DefaultTurnPrompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	health, healthResp := getAuthorized(t, ready.BaseURL+"/healthz", "", nil)
	if healthResp.StatusCode != http.StatusOK || string(health) != "ok" {
		t.Fatalf("health = %d %q", healthResp.StatusCode, health)
	}

	body, modelsResp := getAuthorized(t, ready.BaseURL+"/v1/runtime/models", ready.BearerToken, nil)
	if modelsResp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d %s", modelsResp.StatusCode, body)
	}
	var envelope struct {
		Data struct {
			DefaultModel        string `json:"default_model"`
			DefaultRuntimeAlias string `json:"default_runtime_alias"`
			Connections         []struct {
				ID          string `json:"id"`
				ProviderID  string `json:"provider_id"`
				DisplayName string `json:"display_name"`
				Models      []struct {
					ID           string `json:"id"`
					RuntimeAlias string `json:"runtime_alias"`
					Available    bool   `json:"available"`
				} `json:"models"`
			} `json:"connections"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data.Connections) == 0 {
		t.Fatal("runtime models returned no connections")
	}
	if envelope.Data.DefaultRuntimeAlias != "r4b-scripted" || envelope.Data.DefaultModel != "r4b-scripted/r4b-scripted" {
		t.Fatalf("default model = %q alias = %q", envelope.Data.DefaultModel, envelope.Data.DefaultRuntimeAlias)
	}
	found := false
	for _, connection := range envelope.Data.Connections {
		if connection.ID != "r4b-scripted" || connection.ProviderID != "r4b-scripted" || connection.DisplayName != "r4b-scripted" {
			continue
		}
		for _, model := range connection.Models {
			if model.RuntimeAlias == "r4b-scripted" && model.ID == "r4b-scripted/r4b-scripted" && model.Available {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("r4b-scripted available model not found: %s", body)
	}
}

func TestR4BCustomFileBackedPreviewWithNormalTurn(t *testing.T) {
	customHTML := []byte(`<main id="r4b-custom">Custom R4b HTML from file.</main>`)
	htmlPath := filepath.Join(t.TempDir(), "viewer.html")
	if err := os.WriteFile(htmlPath, customHTML, 0o644); err != nil {
		t.Fatal(err)
	}
	customFallback := r4bTestPNG(t, 140)
	fallbackPath := filepath.Join(t.TempDir(), "fallback.png")
	if err := os.WriteFile(fallbackPath, customFallback, 0o644); err != nil {
		t.Fatal(err)
	}
	srv, ready, err := Start(context.Background(), ServerOptions{
		Workspace: t.TempDir(), StateDir: t.TempDir(), TurnPrompt: DefaultTurnPrompt,
		ViewerHTMLFile: htmlPath, FallbackFile: fallbackPath,
		Title: "Custom R4b Title", Summary: "Custom R4b Summary", ArtifactKey: "ui-adaptation:r4b-custom",
		PreferredAspectRatio: 1.78, MinHeight: 420,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	live := triggerR4BPreview(t, ready)
	if live.Artifact.Title != "Custom R4b Title" || live.Artifact.Summary != "Custom R4b Summary" || live.Artifact.Presentation.PreferredAspectRatio != 1.78 || live.Artifact.Presentation.MinHeight != 420 {
		t.Fatalf("custom artifact metadata = %+v", live.Artifact)
	}
	viewerURL := ready.BaseURL + preview.ViewerURL(ready.SessionID, live.Artifact.ArtifactID, live.Artifact.Revision)
	assetURL := ready.BaseURL + preview.AssetURL(ready.SessionID, live.Artifact.ArtifactID, live.Artifact.Revision, live.Artifact.Fallback.AssetID)
	viewer, viewerResp := getAuthorized(t, viewerURL, ready.BearerToken, nil)
	if viewerResp.StatusCode != http.StatusOK || !bytes.Contains(viewer, customHTML) {
		t.Fatalf("custom viewer response = %d %d bytes", viewerResp.StatusCode, len(viewer))
	}
	asset, assetResp := getAuthorized(t, assetURL, ready.BearerToken, nil)
	if assetResp.StatusCode != http.StatusOK || !bytes.Equal(asset, customFallback) {
		t.Fatalf("custom fallback response = %d %d bytes", assetResp.StatusCode, len(asset))
	}
}

func TestR4BRejectsEmptyAndOversizeViewerFiles(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty.html")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Start(context.Background(), ServerOptions{Workspace: t.TempDir(), StateDir: t.TempDir(), ViewerHTMLFile: empty}); err == nil {
		t.Fatal("empty viewer file was accepted")
	}
	oversize := filepath.Join(t.TempDir(), "oversize.html")
	if err := os.WriteFile(oversize, bytes.Repeat([]byte("x"), preview.MaxViewerBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Start(context.Background(), ServerOptions{Workspace: t.TempDir(), StateDir: t.TempDir(), ViewerHTMLFile: oversize}); err == nil {
		t.Fatal("oversize viewer file was accepted")
	}
}

func triggerR4BPreview(t *testing.T, ready Readiness) preview.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wsURL := strings.Replace(ready.BaseURL, "http://", "ws://", 1) + "/v1/conversations/" + ready.SessionID + "/stream?since=0"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + ready.BearerToken}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]any{"type": "agent_input", "kind": "text", "request_id": "r4b-custom-trigger", "text": ready.TurnPrompt})
	if err := conn.Write(ctx, websocket.MessageText, input); err != nil {
		t.Fatal(err)
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var frame struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(data, &frame) != nil || frame.Kind != "preview_artifact" {
			continue
		}
		event, err := preview.DecodeEvent(data)
		if err != nil {
			t.Fatal(err)
		}
		if event.Artifact == nil || event.Artifact.SessionID != ready.SessionID {
			t.Fatalf("invalid preview event = %+v", event)
		}
		return event
	}
}

func r4bTestPNG(t *testing.T, shade uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 48, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 48; x++ {
			img.Set(x, y, color.RGBA{R: shade, G: uint8(x * 4), B: uint8(y * 7), A: 255})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
