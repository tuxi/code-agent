package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/assetref"
	"code-agent/internal/conversation"
	"code-agent/internal/session"
)

// ---- test adapters for ConversationRepository ----

// fakeConversationRepo is an in-memory ConversationRepository for mux tests.
type fakeConversationRepo struct {
	mu       sync.Mutex
	sessions map[string]*session.Session
	rebinds  map[string]string
}

func newFakeConversationRepo() *fakeConversationRepo {
	return &fakeConversationRepo{sessions: make(map[string]*session.Session), rebinds: map[string]string{}}
}

func (r *fakeConversationRepo) Create(ctx context.Context, workspacePath, workspaceExtID string) (*session.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := &session.Session{ID: fmt.Sprintf("sess_%d", len(r.sessions)+1), WorkspacePath: workspacePath}
	if workspaceExtID != "" {
		s.Workspace = session.WorkspaceRef{Root: session.RootExternal, ExtID: workspaceExtID}
	}
	r.sessions[s.ID] = s
	return s, nil
}

func (r *fakeConversationRepo) Rebind(ctx context.Context, id, absPath string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sessions[id]; !ok {
		return fmt.Errorf("session %q not found", id)
	}
	r.rebinds[id] = absPath
	return nil
}

func (r *fakeConversationRepo) NeedsRebind(ctx context.Context, id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return false, fmt.Errorf("session %q not found", id)
	}
	if s.Workspace.Root != session.RootExternal {
		return false, nil
	}
	_, rebound := r.rebinds[id]
	return !rebound, nil
}

func (r *fakeConversationRepo) Load(ctx context.Context, id string) (*session.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %q not found", id)
	}
	return s, nil
}

func (r *fakeConversationRepo) Save(ctx context.Context, s *session.Session) error { return nil }
func (r *fakeConversationRepo) List(ctx context.Context) ([]session.Meta, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []session.Meta
	for _, s := range r.sessions {
		out = append(out, session.Meta{
			ID: s.ID, WorkspacePath: s.WorkspacePath, TurnStatus: s.TurnStatus(), UpdatedAt: s.UpdatedAt,
		})
	}
	return out, nil
}
func (r *fakeConversationRepo) Delete(ctx context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, id)
	return nil
}
func (r *fakeConversationRepo) UpdateName(ctx context.Context, id string, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return fmt.Errorf("session %q not found", id)
	}
	s.Name = name
	return nil
}
func (r *fakeConversationRepo) Close() error { return nil }

// ---- test adapters for ConversationEventStore ----

// fakeEventStore implements ConversationEventStore with canned data.
type fakeEventStore struct {
	mu   sync.Mutex
	recs map[string][]session.EventRecord
	seq  int64
}

func (s *fakeEventStore) Append(ctx context.Context, e session.EventRecord) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recs == nil {
		s.recs = make(map[string][]session.EventRecord)
	}
	s.seq++
	e.Seq = s.seq
	s.recs[e.SessionID] = append(s.recs[e.SessionID], e)
	return s.seq, nil
}

func (s *fakeEventStore) Replay(ctx context.Context, sessionID string) ([]session.EventRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recs[sessionID], nil
}

func (s *fakeEventStore) ReplaySince(ctx context.Context, sessionID string, sinceSeq int64) ([]session.EventRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []session.EventRecord
	for _, r := range s.recs[sessionID] {
		if r.Seq > sinceSeq {
			out = append(out, r)
		}
	}
	return out, nil
}

func storedEvent(ev agent.Event) session.EventRecord {
	p, _ := json.Marshal(ev)
	return session.EventRecord{SessionID: ev.SessionID, TurnID: ev.TurnID, Kind: string(ev.Kind), At: ev.At, Payload: p}
}

// newTestMux returns an mux wired with test fakes. executor is nil → WS endpoint
// is skipped.
func newTestMux(repo conversation.ConversationRepository, events conversation.ConversationEventStore) http.Handler {
	return NewMux(repo, events, nil, MuxOptions{})
}

func TestMuxRuntimeCapabilitiesAndActivity(t *testing.T) {
	repo := newFakeConversationRepo()
	running := &session.Session{ID: "running", WorkspacePath: "/tmp/a", UpdatedAt: time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC)}
	running.SetTurnStatus(session.TurnStatusRunning)
	done := &session.Session{ID: "done", WorkspacePath: "/tmp/b"}
	done.SetTurnStatus(session.TurnStatusDone)
	repo.sessions[running.ID] = running
	repo.sessions[done.ID] = done

	h := NewMux(repo, &fakeEventStore{}, nil, MuxOptions{RuntimeCapabilities: RuntimeCapabilities{
		SessionScopedClientTools: true,
		ActivitySnapshot:         true,
		MaxConcurrentTurns:       1,
	}})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/runtime/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	var caps runtimeCapabilitiesResponse
	decodeResponse(t, resp, &caps)
	if caps.Capabilities.MultiSessionExecution || !caps.Capabilities.SessionScopedClientTools || caps.Capabilities.MaxConcurrentTurns != 1 {
		t.Fatalf("capabilities = %+v", caps.Capabilities)
	}

	resp, err = http.Get(srv.URL + "/v1/activity")
	if err != nil {
		t.Fatal(err)
	}
	var activity activityResponse
	decodeResponse(t, resp, &activity)
	if len(activity.Sessions) != 1 || activity.Sessions[0].SessionID != "running" || activity.Sessions[0].State != session.TurnStatusRunning {
		t.Fatalf("activity = %+v", activity.Sessions)
	}
}

func TestConfiguredRuntimeCapabilitiesEnableParallelOnlyAboveOne(t *testing.T) {
	serial := ConfiguredRuntimeCapabilities(1)
	if serial.MultiSessionExecution || serial.MaxConcurrentTurns != 1 {
		t.Fatalf("serial capabilities=%+v", serial)
	}
	parallel := ConfiguredRuntimeCapabilities(5)
	if !parallel.MultiSessionExecution || !parallel.SessionScopedClientTools || !parallel.WorkspaceExecutionPolicy || !parallel.ActivitySnapshot || parallel.ManagedWorktree || parallel.MaxConcurrentTurns != 5 {
		t.Fatalf("parallel capabilities=%+v", parallel)
	}
}

// TestMuxRebindFlow walks the host's Phase-1 attach contract: create an external
// conversation with a bookmark ext_id, see needs_rebind=true + workspace_ref in
// detail, POST the fresh path to /rebind, then see needs_rebind=false.
func TestMuxRebindFlow(t *testing.T) {
	repo := newFakeConversationRepo()
	srv := httptest.NewServer(newTestMux(repo, &fakeEventStore{}))
	defer srv.Close()

	// Create with an external bookmark id.
	resp, err := http.Post(srv.URL+"/v1/conversations", "application/json",
		strings.NewReader(`{"workspace_path":"/var/old/MyProj","workspace_ext_id":"BKMK-7f3a"}`))
	if err != nil {
		t.Fatal(err)
	}
	var ref ConversationRef
	decodeResponse(t, resp, &ref)
	if ref.ID == "" {
		t.Fatal("no id from create")
	}

	getDetail := func() ConversationDetail {
		r, err := http.Get(srv.URL + "/v1/conversations/" + ref.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var d ConversationDetail
		decodeResponse(t, r, &d)
		return d
	}

	d := getDetail()
	if !d.NeedsRebind {
		t.Error("needs_rebind = false, want true before rebind")
	}
	if d.WorkspaceRef == nil || d.WorkspaceRef.Root != "external" || d.WorkspaceRef.ExtID != "BKMK-7f3a" {
		t.Errorf("workspace_ref = %+v, want external/BKMK-7f3a", d.WorkspaceRef)
	}

	// Rebind to a fresh absolute path.
	rb, err := http.Post(srv.URL+"/v1/conversations/"+ref.ID+"/rebind", "application/json",
		strings.NewReader(`{"workspace_path":"/var/new/MyProj"}`))
	if err != nil {
		t.Fatal(err)
	}
	rb.Body.Close()
	if rb.StatusCode != http.StatusNoContent {
		t.Fatalf("rebind status = %d, want 204", rb.StatusCode)
	}

	if getDetail().NeedsRebind {
		t.Error("needs_rebind = true, want false after rebind")
	}

	// Missing workspace_path → 400.
	bad, _ := http.Post(srv.URL+"/v1/conversations/"+ref.ID+"/rebind", "application/json",
		strings.NewReader(`{}`))
	if bad.StatusCode != http.StatusBadRequest {
		t.Errorf("rebind without path status = %d, want 400", bad.StatusCode)
	}
	bad.Body.Close()
}

func TestMuxHealthz(t *testing.T) {
	srv := httptest.NewServer(newTestMux(newFakeConversationRepo(), &fakeEventStore{}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestMuxCreateThenList(t *testing.T) {
	repo := newFakeConversationRepo()
	srv := httptest.NewServer(newTestMux(repo, &fakeEventStore{}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/conversations", "application/json",
		strings.NewReader(`{"workspace_path":"/Users/x/proj"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var ref ConversationRef
	decodeResponse(t, resp, &ref)
	if ref.ID == "" {
		t.Fatal("create did not return an id")
	}
	if ref.WorkspacePath != "/Users/x/proj" {
		t.Errorf("WorkspacePath = %q", ref.WorkspacePath)
	}
	if ref.Workspace == nil {
		t.Fatal("create did not return workspace anchor")
	}
	if ref.Workspace.ID != "proj-local" || ref.Workspace.RootPath != "/Users/x/proj" || ref.Workspace.Name != "proj" {
		t.Fatalf("workspace anchor = %+v", ref.Workspace)
	}

	// List should include the created conversation.
	resp2, err := http.Get(srv.URL + "/v1/conversations")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var refs []ConversationRef
	decodeResponse(t, resp2, &refs)
	found := false
	for _, r := range refs {
		if r.ID == ref.ID {
			found = true
			if r.Workspace == nil || r.Workspace.ID != "proj-local" {
				t.Fatalf("listed workspace anchor = %+v", r.Workspace)
			}
		}
	}
	if !found {
		t.Errorf("created conversation %q not in list %+v", ref.ID, refs)
	}
}

func TestMuxCreateAcceptsEmptyBody(t *testing.T) {
	repo := newFakeConversationRepo()
	srv := httptest.NewServer(newTestMux(repo, &fakeEventStore{}))
	defer srv.Close()

	// workspace_path is required (Phase 3). An empty body → 400.
	resp, err := http.Post(srv.URL+"/v1/conversations", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("create with empty body status = %d, want 400 (workspace_path required)", resp.StatusCode)
	}
}

func TestMuxDelete(t *testing.T) {
	repo := newFakeConversationRepo()
	srv := httptest.NewServer(newTestMux(repo, &fakeEventStore{}))
	defer srv.Close()

	// Create then delete — must include workspace_path.
	resp, _ := http.Post(srv.URL+"/v1/conversations", "application/json",
		strings.NewReader(`{"workspace_path":"/tmp/test"}`))
	var ref ConversationRef
	decodeResponse(t, resp, &ref)
	resp.Body.Close()

	req, _ := http.NewRequest("DELETE", srv.URL+"/v1/conversations/"+ref.ID, nil)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp2.StatusCode != http.StatusNoContent {
		t.Errorf("delete status = %d, want 204", resp2.StatusCode)
	}
	resp2.Body.Close()

	// List should be empty.
	resp3, _ := http.Get(srv.URL + "/v1/conversations")
	var refs []ConversationRef
	decodeResponse(t, resp3, &refs)
	resp3.Body.Close()
	if len(refs) != 0 {
		t.Errorf("list after delete = %d, want 0", len(refs))
	}
}

// ---- conversation read API (P1-A.5) ----

func readMuxWithHistory() (http.Handler, string) {
	at := time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC)
	id := "sess_hist"
	recs := []session.EventRecord{
		storedEvent(agent.Event{Kind: agent.EventTurnStarted, SessionID: id, TurnID: "t1", At: at, Text: "分析项目"}),
		storedEvent(agent.Event{Kind: agent.EventToolStarted, SessionID: id, TurnID: "t1", At: at.Add(time.Second), Step: 1, ToolName: "grep", ToolArgs: `{"q":"x"}`}),
		storedEvent(agent.Event{Kind: agent.EventModelFinished, SessionID: id, TurnID: "t1", At: at.Add(2 * time.Second), PromptTokens: 100, Elapsed: 731 * time.Millisecond}),
		storedEvent(agent.Event{
			Kind:      agent.EventTurnFinished,
			SessionID: id,
			TurnID:    "t1",
			At:        at.Add(3 * time.Second),
			Text:      "项目结构见 App.swift:5",
			TextAnnotations: []assets.TextAnnotation{{
				AssetID:    "asset_t1_call_1_001_file",
				Kind:       "file_location",
				Text:       "App.swift:5",
				StartByte:  len("项目结构见 "),
				EndByte:    len("项目结构见 App.swift:5"),
				StartUTF16: len([]rune("项目结构见 ")),
				EndUTF16:   len([]rune("项目结构见 App.swift:5")),
			}},
		}),
	}
	events := &fakeEventStore{recs: map[string][]session.EventRecord{id: recs}}

	repo := newFakeConversationRepo()
	// Pre-populate the session so Load succeeds.
	repo.sessions[id] = &session.Session{ID: id, WorkspacePath: "/tmp/test"}

	mux := newTestMux(repo, events)
	return mux, id
}

func TestMuxGetEventsReEncodesToWire(t *testing.T) {
	mux, id := readMuxWithHistory()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/conversations/" + id + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var frames []map[string]any
	decodeResponse(t, resp, &frames)
	if len(frames) != 4 {
		t.Fatalf("want 4 event frames, got %d", len(frames))
	}
	tool := frames[1]
	if tool["kind"] != "tool_started" {
		t.Fatalf("frame[1] kind = %v", tool["kind"])
	}
	if args, _ := tool["tool_args"].(map[string]any); args["q"] != "x" {
		t.Errorf("tool_args not structured JSON in history: %v", tool["tool_args"])
	}
	if frames[2]["elapsed_ms"].(float64) != 731 {
		t.Errorf("elapsed_ms = %v, want 731", frames[2]["elapsed_ms"])
	}
	anns, ok := frames[3]["text_annotations"].([]any)
	if !ok || len(anns) != 1 {
		t.Fatalf("text_annotations = %#v, want one", frames[3]["text_annotations"])
	}
	if ann, _ := anns[0].(map[string]any); ann["asset_id"] != "asset_t1_call_1_001_file" {
		t.Fatalf("annotation = %#v", anns[0])
	}
}

func TestMuxGetEventsReplaysToolAssets(t *testing.T) {
	at := time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC)
	id := "sess_assets"
	output := json.RawMessage(`{"kind":"search_results","items":[{"asset_id":"asset_t1_call_1_001_abc","kind":"file_location","path":"Sources/App.swift","line":42}]}`)
	ref := assets.Ref{
		ID:                    "asset_t1_call_1_001_abc",
		Kind:                  "file_location",
		URI:                   "workspace://test-local/Sources/App.swift#L42",
		DisplayName:           "App.swift:42",
		WorkspaceID:           "test-local",
		WorkspaceRelativePath: "Sources/App.swift",
		Range:                 &assets.Range{StartLine: 42},
		SourceTurnID:          "t1",
		SourceCallID:          "call_1",
	}
	events := &fakeEventStore{recs: map[string][]session.EventRecord{id: {
		storedEvent(agent.Event{
			Kind:        agent.EventToolFinished,
			SessionID:   id,
			TurnID:      "t1",
			At:          at,
			Step:        1,
			ToolName:    "grep",
			CallID:      "call_1",
			Observation: "Sources/App.swift:42: let value = 1",
			Output:      output,
			Assets:      []assets.Ref{ref},
		}),
	}}}
	repo := newFakeConversationRepo()
	repo.sessions[id] = &session.Session{ID: id, WorkspacePath: "/tmp/test"}
	srv := httptest.NewServer(newTestMux(repo, events))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/conversations/" + id + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var frames []map[string]any
	decodeResponse(t, resp, &frames)
	if len(frames) != 1 {
		t.Fatalf("want 1 event frame, got %d", len(frames))
	}
	frame := frames[0]
	if frame["kind"] != "tool_finished" {
		t.Fatalf("kind = %v, want tool_finished", frame["kind"])
	}
	if out, ok := frame["output"].(map[string]any); !ok || out["kind"] != "search_results" {
		t.Fatalf("output = %#v, want search_results", frame["output"])
	}
	gotAssets, ok := frame["assets"].([]any)
	if !ok || len(gotAssets) != 1 {
		t.Fatalf("assets = %#v, want one asset", frame["assets"])
	}
	gotAsset, ok := gotAssets[0].(map[string]any)
	if !ok || gotAsset["id"] != ref.ID || gotAsset["uri"] != ref.URI {
		t.Fatalf("asset = %#v, want id/uri preserved", gotAssets[0])
	}
}

func TestMuxAssetPreviewAndContent(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "Sources")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := strings.Join([]string{
		"import Foundation",
		"",
		"struct App {",
		"    let name = \"AgentKit\"",
		"    let value = 42",
		"}",
	}, "\n")
	if err := os.WriteFile(filepath.Join(srcDir, "App.swift"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	id := "sess_asset_api"
	assetID := "asset_t1_call_1_001_file"
	ref := assets.Ref{
		ID:                    assetID,
		Kind:                  "file_location",
		URI:                   "workspace://project-local/Sources/App.swift#L5",
		DisplayName:           "App.swift:5",
		WorkspaceID:           "project-local",
		WorkspaceRelativePath: "Sources/App.swift",
		Range:                 &assets.Range{StartLine: 5},
		MIMEType:              "text/x-swift",
		SourceTurnID:          "t1",
		SourceCallID:          "call_1",
	}
	events := &fakeEventStore{recs: map[string][]session.EventRecord{id: {
		storedEvent(agent.Event{
			Kind:      agent.EventToolFinished,
			SessionID: id,
			TurnID:    "t1",
			CallID:    "call_1",
			ToolName:  "grep",
			Assets:    []assets.Ref{ref},
		}),
	}}}
	repo := newFakeConversationRepo()
	repo.sessions[id] = &session.Session{ID: id, WorkspacePath: root}
	srv := httptest.NewServer(newTestMux(repo, events))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/conversations/" + id + "/assets/" + assetID + "/preview")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preview status = %d, want 200", resp.StatusCode)
	}
	var preview AssetPreviewResponse
	decodeResponse(t, resp, &preview)
	if preview.Source != "file_window" || !strings.Contains(preview.Content, "5:     let value = 42") {
		t.Fatalf("preview = %+v", preview)
	}

	resp2, err := http.Get(srv.URL + "/v1/conversations/" + id + "/assets/" + assetID + "/content")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("content status = %d, want 200", resp2.StatusCode)
	}
	var body AssetContentResponse
	decodeResponse(t, resp2, &body)
	if body.Content != content || body.MIMEType != "text/x-swift" {
		t.Fatalf("content response = %+v", body)
	}
}

func TestMuxAssetPreviewFallsBackToMetadata(t *testing.T) {
	id := "sess_asset_meta"
	assetID := "asset_t1_call_1_001_mcp"
	ref := assets.Ref{
		ID:       assetID,
		Kind:     "image",
		URI:      "mcp://fs/read_file/call_1/001",
		Preview:  "[non-text content: image (image/png, 5 bytes) omitted]",
		MIMEType: "image/png",
		Metadata: map[string]string{
			"source":   "mcp",
			"mcp_type": "image",
		},
		SourceTurnID: "t1",
		SourceCallID: "call_1",
	}
	events := &fakeEventStore{recs: map[string][]session.EventRecord{id: {
		storedEvent(agent.Event{Kind: agent.EventToolFinished, SessionID: id, TurnID: "t1", Assets: []assets.Ref{ref}}),
	}}}
	repo := newFakeConversationRepo()
	repo.sessions[id] = &session.Session{ID: id, WorkspacePath: t.TempDir()}
	srv := httptest.NewServer(newTestMux(repo, events))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/conversations/" + id + "/assets/" + assetID + "/preview")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preview status = %d, want 200", resp.StatusCode)
	}
	var preview AssetPreviewResponse
	decodeResponse(t, resp, &preview)
	if preview.Source != "asset_preview" || preview.Content != ref.Preview || preview.Asset.Metadata["mcp_type"] != "image" {
		t.Fatalf("preview = %+v", preview)
	}
}

func TestMuxAssetPreviewAndBlobForBinaryFile(t *testing.T) {
	root := t.TempDir()
	data := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 1, 2, 3, 4}
	if err := os.WriteFile(filepath.Join(root, "cat_sunset.png"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	id := "sess_asset_blob"
	assetID := "asset_t1_call_1_001_image"
	ref := assets.Ref{
		ID:                    assetID,
		Kind:                  "image",
		URI:                   "workspace://ai-local/cat_sunset.png",
		DisplayName:           "cat_sunset.png",
		WorkspaceID:           "ai-local",
		WorkspaceRelativePath: "cat_sunset.png",
		MIMEType:              "image/png",
		SourceTurnID:          "t1",
		SourceCallID:          "call_1",
	}
	events := &fakeEventStore{recs: map[string][]session.EventRecord{id: {
		storedEvent(agent.Event{Kind: agent.EventToolFinished, SessionID: id, TurnID: "t1", Assets: []assets.Ref{ref}}),
	}}}
	repo := newFakeConversationRepo()
	repo.sessions[id] = &session.Session{ID: id, WorkspacePath: root}
	srv := httptest.NewServer(newTestMux(repo, events))
	defer srv.Close()

	previewResp, err := http.Get(srv.URL + "/v1/conversations/" + id + "/assets/" + assetID + "/preview")
	if err != nil {
		t.Fatal(err)
	}
	defer previewResp.Body.Close()
	if previewResp.StatusCode != http.StatusOK {
		t.Fatalf("preview status = %d, want 200", previewResp.StatusCode)
	}
	var preview AssetPreviewResponse
	decodeResponse(t, previewResp, &preview)
	if preview.Source != "metadata" || preview.MIMEType != "image/png" || preview.SizeBytes != int64(len(data)) {
		t.Fatalf("preview = %+v", preview)
	}
	if preview.Metadata["media_url"] != "/v1/conversations/"+id+"/assets/"+assetID+"/blob" {
		t.Fatalf("preview metadata = %+v", preview.Metadata)
	}

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/conversations/"+id+"/assets/"+assetID+"/blob", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-3")
	blobResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer blobResp.Body.Close()
	if blobResp.StatusCode != http.StatusPartialContent {
		t.Fatalf("blob status = %d, want 206", blobResp.StatusCode)
	}
	if got := blobResp.Header.Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", got)
	}
	if got := blobResp.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q, want bytes", got)
	}
	if got := blobResp.Header.Get("Content-Range"); got != "bytes 0-3/12" {
		t.Fatalf("Content-Range = %q, want bytes 0-3/12", got)
	}
	body, err := io.ReadAll(blobResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(data[:4]) {
		t.Fatalf("blob body = %v, want %v", body, data[:4])
	}
}

func TestMuxAssetBlobRejectsMetadataOnlyAsset(t *testing.T) {
	id := "sess_asset_blob_meta"
	assetID := "asset_t1_call_1_001_mcp"
	ref := assets.Ref{
		ID:       assetID,
		Kind:     "image",
		URI:      "mcp://fs/read_file/call_1/001",
		MIMEType: "image/png",
		Metadata: map[string]string{"source": "mcp"},
	}
	events := &fakeEventStore{recs: map[string][]session.EventRecord{id: {
		storedEvent(agent.Event{Kind: agent.EventToolFinished, SessionID: id, TurnID: "t1", Assets: []assets.Ref{ref}}),
	}}}
	repo := newFakeConversationRepo()
	repo.sessions[id] = &session.Session{ID: id, WorkspacePath: t.TempDir()}
	srv := httptest.NewServer(newTestMux(repo, events))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/conversations/" + id + "/assets/" + assetID + "/blob")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("blob status = %d, want 415", resp.StatusCode)
	}
}

func TestMuxAssetContentRejectsPathEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	id := "sess_asset_escape"
	assetID := "asset_t1_call_1_001_escape"
	ref := assets.Ref{
		ID:           assetID,
		Kind:         "file",
		AbsolutePath: outside,
		MIMEType:     "text/plain",
		SourceTurnID: "t1",
		SourceCallID: "call_1",
	}
	events := &fakeEventStore{recs: map[string][]session.EventRecord{id: {
		storedEvent(agent.Event{Kind: agent.EventToolFinished, SessionID: id, TurnID: "t1", Assets: []assets.Ref{ref}}),
	}}}
	repo := newFakeConversationRepo()
	repo.sessions[id] = &session.Session{ID: id, WorkspacePath: root}
	srv := httptest.NewServer(newTestMux(repo, events))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/conversations/" + id + "/assets/" + assetID + "/content")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("content status = %d, want 403", resp.StatusCode)
	}
}

// TestMuxGetEventsSince covers the v1.2 §4 incremental replay: every frame carries
// its seq, and ?since=<seq> returns only the tail after that seq.
func TestMuxGetEventsSince(t *testing.T) {
	id := "sess_seq"
	events := &fakeEventStore{}
	at := time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		_, _ = events.Append(context.Background(), storedEvent(agent.Event{
			Kind: agent.EventTurnStarted, SessionID: id, TurnID: "t1", At: at.Add(time.Duration(i) * time.Second),
		}))
	}
	repo := newFakeConversationRepo()
	repo.sessions[id] = &session.Session{ID: id, WorkspacePath: "/tmp/test"}
	srv := httptest.NewServer(newTestMux(repo, events))
	defer srv.Close()

	seqsOf := func(url string) []float64 {
		t.Helper()
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var frames []map[string]any
		decodeResponse(t, resp, &frames)
		var out []float64
		for _, f := range frames {
			out = append(out, f["seq"].(float64))
		}
		return out
	}

	if got := seqsOf(srv.URL + "/v1/conversations/" + id + "/events"); len(got) != 4 || got[0] != 1 || got[3] != 4 {
		t.Fatalf("full replay seqs = %v, want [1 2 3 4]", got)
	}
	if got := seqsOf(srv.URL + "/v1/conversations/" + id + "/events?since=2"); len(got) != 2 || got[0] != 3 || got[1] != 4 {
		t.Fatalf("since=2 seqs = %v, want [3 4]", got)
	}
}

func TestMuxGetMessagesDerivesFromEvents(t *testing.T) {
	mux, id := readMuxWithHistory()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/conversations/" + id + "/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var msgs []MessageView
	decodeResponse(t, resp, &msgs)
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages (user+assistant), got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[0].Content != "分析项目" {
		t.Errorf("msg[0] = %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Content != "项目结构见 App.swift:5" {
		t.Errorf("msg[1] = %+v", msgs[1])
	}
}

func TestMuxGetDetail(t *testing.T) {
	mux, id := readMuxWithHistory()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/conversations/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var d ConversationDetail
	decodeResponse(t, resp, &d)
	if d.ID != id || d.TurnCount != 1 || d.MessageCount != 2 {
		t.Errorf("detail = %+v", d)
	}
	if d.CreatedAt == "" || d.UpdatedAt == "" {
		t.Errorf("detail missing timestamps: %+v", d)
	}
}

func TestMuxReadUnknownConversation404(t *testing.T) {
	srv := httptest.NewServer(newTestMux(newFakeConversationRepo(), &fakeEventStore{}))
	defer srv.Close()

	for _, path := range []string{"/v1/conversations/nope", "/v1/conversations/nope/messages", "/v1/conversations/nope/events"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", path, resp.StatusCode)
		}
	}
}

// TestMuxJobEventsEndpoint verifies the P8.7 Phase C job backlog endpoint:
// a job id (which has events but NO sessions row) replays its own stream as
// wire frames, and an unknown id 404s.
func TestMuxJobEventsEndpoint(t *testing.T) {
	jobID := "job_1"
	events := &fakeEventStore{}
	at := time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC)
	_, _ = events.Append(context.Background(), storedEvent(agent.Event{
		Kind: agent.EventJobStarted, SessionID: jobID, At: at, Text: "npm install",
	}))
	_, _ = events.Append(context.Background(), storedEvent(agent.Event{
		Kind: agent.EventJobOutput, SessionID: jobID, At: at.Add(time.Second), Chunk: "cloning...\n",
	}))
	_, _ = events.Append(context.Background(), storedEvent(agent.Event{
		Kind: agent.EventJobFinished, SessionID: jobID, At: at.Add(2 * time.Second), Text: "exited",
	}))

	// A job has NO sessions row — the repo doesn't know it.
	repo := newFakeConversationRepo()
	srv := httptest.NewServer(newTestMux(repo, events))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/jobs/" + jobID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var frames []map[string]any
	decodeResponse(t, resp, &frames)
	if len(frames) != 3 {
		t.Fatalf("want 3 frames, got %d", len(frames))
	}
	if frames[0]["kind"] != "job_started" || frames[0]["session_id"] != jobID {
		t.Errorf("frame[0] = %v", frames[0])
	}
	if frames[0]["seq"].(float64) != 1 || frames[2]["seq"].(float64) != 3 {
		t.Errorf("seqs = %v..%v, want 1..3", frames[0]["seq"], frames[2]["seq"])
	}

	// Unknown job id → 404 (no events, no sessions row).
	resp2, err := http.Get(srv.URL + "/v1/jobs/job_nope/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("unknown job status = %d, want 404", resp2.StatusCode)
	}
}
