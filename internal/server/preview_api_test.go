package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/preview"
	"code-agent/internal/session"
)

func previewFixtureRoot() string {
	return filepath.Join("..", "..", "docs", "capability-runtime-upgrade", "fixtures", "preview", "v1")
}

func loadPreviewFixture(t *testing.T, name string) preview.Artifact {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(previewFixtureRoot(), name))
	if err != nil {
		t.Fatal(err)
	}
	var artifact preview.Artifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatal(err)
	}
	return artifact
}

func importPreviewFixture(t *testing.T, store *preview.FSStore, artifact preview.Artifact) ([]byte, []byte) {
	t.Helper()
	viewer, err := os.ReadFile(filepath.Join(previewFixtureRoot(), "viewer.html"))
	if err != nil {
		t.Fatal(err)
	}
	fallback, err := os.ReadFile(filepath.Join(previewFixtureRoot(), "assets", "fallback.png"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ImportArtifact(context.Background(), preview.Namespace{SessionID: artifact.SessionID}, artifact, viewer, map[string][]byte{"asset_fixture_fallback": fallback}); err != nil {
		t.Fatal(err)
	}
	return viewer, fallback
}

func TestMuxPreviewViewerAndAssetAreRevisionAddressed(t *testing.T) {
	repo := newFakeConversationRepo()
	if _, err := repo.CreateWithID(context.Background(), "session_fixture", t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	store, err := preview.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	artifact := loadPreviewFixture(t, "ready-interactive.json")
	viewer, fallback := importPreviewFixture(t, store, artifact)
	service := preview.NewService(store)
	service.Now = func() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) }
	srv := httptest.NewServer(NewMux(repo, &fakeEventStore{}, nil, MuxOptions{Preview: service}))
	defer srv.Close()

	viewerURL := srv.URL + preview.ViewerURL(artifact.SessionID, artifact.ArtifactID, artifact.Revision)
	assetURL := srv.URL + preview.AssetURL(artifact.SessionID, artifact.ArtifactID, artifact.Revision, "asset_fixture_fallback")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(viewerURL)
	if err != nil {
		t.Fatal(err)
	}
	viewerBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !bytes.Equal(viewerBody, viewer) {
		t.Fatalf("viewer status/body = %d/%d bytes, want 200/%d", resp.StatusCode, len(viewerBody), len(viewer))
	}
	if resp.Header.Get("Content-Type") != preview.ViewerMIMEType || resp.Header.Get("Content-Security-Policy") != preview.ViewerContentSecurityPolicy || resp.Header.Get("Cache-Control") != "private, max-age=31536000, immutable" {
		t.Fatalf("viewer headers = %+v", resp.Header)
	}

	resp, err = client.Get(assetURL)
	if err != nil {
		t.Fatal(err)
	}
	assetBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !bytes.Equal(assetBody, fallback) || resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("asset response = %d %d bytes %q", resp.StatusCode, len(assetBody), resp.Header.Get("Content-Type"))
	}

	inProcessViewer, err := service.Resolve("session_fixture", artifact.ArtifactID, artifact.Revision)
	if err != nil {
		t.Fatal(err)
	}
	inProcessAsset, err := service.ResolveContent("session_fixture", artifact.ArtifactID, artifact.Revision, "asset_fixture_fallback")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(viewerBody, inProcessViewer.Data) || !bytes.Equal(assetBody, inProcessAsset.Data) {
		t.Fatal("HTTP and in-process preview resolvers returned different bytes")
	}
}

func TestMuxPreviewCapabilitiesReconcileHelloAndREST(t *testing.T) {
	store, err := preview.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler := NewMux(newFakeConversationRepo(), &fakeEventStore{}, nil, MuxOptions{
		Preview:      preview.NewService(store),
		Capabilities: []string{"streaming", "preview_interactive_html_v1", "preview_replay_v1"},
	})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/runtime/capabilities", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("capabilities status = %d", recorder.Code)
	}
	var response struct {
		Capabilities RuntimeCapabilities `json:"capabilities"`
	}
	decodeResponse(t, recorder.Result(), &response)
	caps := response.Capabilities
	if !caps.PreviewContractV1 || !caps.PreviewInteractiveHTMLV1 || !caps.PreviewStaticAssetV1 || !caps.PreviewReplayV1 {
		t.Fatalf("preview REST capabilities = %+v", caps)
	}
	hello, err := Hello("test", preview.FullRuntimeFlags().List())
	if err != nil {
		t.Fatal(err)
	}
	var helloFrame struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(hello, &helloFrame); err != nil {
		t.Fatal(err)
	}
	negotiated := preview.Negotiate(helloFrame.Capabilities, caps.previewList(), false)
	if negotiated.LiveMode != preview.ModeInteractive || !negotiated.Effective.Replay {
		t.Fatalf("hello/REST negotiation = %+v", negotiated)
	}
}

func (c RuntimeCapabilities) previewList() []string {
	return preview.RuntimeFlags{
		Contract: c.PreviewContractV1, Interactive: c.PreviewInteractiveHTMLV1,
		Static: c.PreviewStaticAssetV1, Replay: c.PreviewReplayV1,
	}.List()
}

func TestMuxPreviewExpiredErrorEnvelope(t *testing.T) {
	repo := newFakeConversationRepo()
	if _, err := repo.CreateWithID(context.Background(), "session_fixture", t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	store, err := preview.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	artifact := loadPreviewFixture(t, "expired-replay.json")
	importPreviewFixture(t, store, artifact)
	service := preview.NewService(store)
	service.Now = func() time.Time { return time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC) }
	srv := httptest.NewServer(NewMux(repo, &fakeEventStore{}, nil, MuxOptions{Preview: service}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + preview.ViewerURL(artifact.SessionID, artifact.ArtifactID, artifact.Revision))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var envelope struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			HTTPStatus int `json:"http_status"`
			Error      struct {
				Code      string `json:"code"`
				Message   string `json:"message"`
				Retryable bool   `json:"retryable"`
			} `json:"error"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusGone || envelope.Code != 41000 || envelope.Msg != "preview_expired" || envelope.Data.HTTPStatus != 410 || envelope.Data.Error.Code != "preview_expired" {
		t.Fatalf("expired envelope = status %d %+v", resp.StatusCode, envelope)
	}
}

func TestPreviewEventReplayKeepsCompleteSnapshot(t *testing.T) {
	repo := newFakeConversationRepo()
	if _, err := repo.CreateWithID(context.Background(), "session_fixture", t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	artifact := loadPreviewFixture(t, "ready-interactive.json")
	payload, err := json.Marshal(agent.Event{
		Kind: agent.EventPreviewArtifact, SessionID: artifact.SessionID, TurnID: artifact.TurnID,
		CallID: artifact.CallID, Artifact: &artifact,
	})
	if err != nil {
		t.Fatal(err)
	}
	events := &fakeEventStore{recs: map[string][]session.EventRecord{
		"session_fixture": {{
			Seq: 1, SessionID: artifact.SessionID, TurnID: artifact.TurnID,
			Kind: string(agent.EventPreviewArtifact), At: artifact.UpdatedAt, Payload: payload,
		}},
	}}
	recorder := httptest.NewRecorder()
	NewMux(repo, events, nil, MuxOptions{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/conversations/session_fixture/events?since=0", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("events status = %d", recorder.Code)
	}
	var frames []json.RawMessage
	decodeResponse(t, recorder.Result(), &frames)
	if len(frames) != 1 {
		t.Fatalf("replay frames = %d", len(frames))
	}
	var frame struct {
		Kind     string           `json:"kind"`
		Seq      int64            `json:"seq"`
		Session  string           `json:"session_id"`
		Turn     string           `json:"turn_id"`
		Call     string           `json:"call_id"`
		Artifact preview.Artifact `json:"artifact"`
	}
	if err := json.Unmarshal(frames[0], &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Kind != "preview_artifact" || frame.Seq != 1 || frame.Session != artifact.SessionID || frame.Turn != artifact.TurnID || frame.Call != artifact.CallID {
		t.Fatalf("replay envelope = %+v", frame)
	}
	if frame.Artifact.RevisionSHA256 != artifact.RevisionSHA256 || frame.Artifact.Fallback == nil || len(frame.Artifact.Assets) != 1 || frame.Artifact.Viewer == nil {
		t.Fatalf("replay artifact snapshot = %+v", frame.Artifact)
	}
}

func TestPreviewEncodeRejectsCorrelationMismatch(t *testing.T) {
	artifact := loadPreviewFixture(t, "pending.json")
	artifact.SessionID = "other_session"
	hash, err := preview.RevisionSHA256(artifact)
	if err != nil {
		t.Fatal(err)
	}
	artifact.RevisionSHA256 = hash
	_, err = Encode(agent.Event{
		Kind: agent.EventPreviewArtifact, SessionID: "session_fixture", TurnID: artifact.TurnID,
		CallID: artifact.CallID, Artifact: &artifact,
	}, "evt", "")
	if err == nil {
		t.Fatal("correlation mismatch was encoded")
	}
}

func TestMuxPreviewDigestMismatchUses422Envelope(t *testing.T) {
	repo := newFakeConversationRepo()
	if _, err := repo.CreateWithID(context.Background(), "session_fixture", t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	store, err := preview.NewFSStore(base)
	if err != nil {
		t.Fatal(err)
	}
	artifact := loadPreviewFixture(t, "ready-interactive.json")
	viewer, _ := importPreviewFixture(t, store, artifact)
	var viewerPath string
	if err := filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr == nil && !entry.IsDir() && entry.Name() == "viewer.html" {
			viewerPath = path
		}
		return walkErr
	}); err != nil {
		t.Fatal(err)
	}
	if viewerPath == "" {
		t.Fatal("materialized viewer not found")
	}
	corrupt := bytes.Repeat([]byte("x"), len(viewer))
	if err := os.WriteFile(viewerPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	service := preview.NewService(store)
	srv := httptest.NewServer(NewMux(repo, &fakeEventStore{}, nil, MuxOptions{Preview: service}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + preview.ViewerURL(artifact.SessionID, artifact.ArtifactID, artifact.Revision))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var envelope struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnprocessableEntity || envelope.Code != 42200 || envelope.Msg != "preview_digest_mismatch" || envelope.Data.Error.Code != "preview_digest_mismatch" {
		t.Fatalf("digest mismatch envelope = status %d %+v", resp.StatusCode, envelope)
	}
}

func TestPreviewUnsupportedIsSuccessfulDegradedResponse(t *testing.T) {
	recorder := httptest.NewRecorder()
	writePreviewError(recorder, httptest.NewRequest(http.MethodGet, "/preview", nil), preview.NewError(preview.CodeUnsupported, "Preview unavailable.", false))
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			Presentation string `json:"presentation"`
			Error        struct {
				Code string `json:"code"`
			} `json:"error"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || envelope.Code != 0 || envelope.Data.Presentation != "unavailable" || envelope.Data.Error.Code != "preview_unsupported" {
		t.Fatalf("unsupported response = status %d %+v", recorder.Code, envelope)
	}
}
