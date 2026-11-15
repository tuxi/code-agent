package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/assetref"
	"code-agent/internal/conversation"
	"code-agent/internal/model"
	"code-agent/internal/preview"
	runtimepkg "code-agent/internal/runtime"
	"code-agent/internal/session"
	sessionsqlite "code-agent/internal/session/sqlite"
	"code-agent/internal/tools"
)

func evidenceStore() *conversation.StoreEventAdapter {
	return &conversation.StoreEventAdapter{Store: session.NewMemoryStore()}
}
func appendEvidence(t *testing.T, store conversation.ConversationEventStore, id string, event agent.Event) {
	t.Helper()
	if event.SessionID == "" {
		event.SessionID = id
	}
	if event.At.IsZero() {
		event.At = time.Now()
	}
	if _, err := store.Append(context.Background(), storedEvent(event)); err != nil {
		t.Fatal(err)
	}
}

func TestRailOutputsGeneratedEvidenceIdentityVersionsAndLegacy(t *testing.T) {
	store := evidenceStore()
	id := "conversation"
	appendEvidence(t, store, id, agent.Event{Kind: agent.EventToolFinished, OutputEvidenceVersion: 1, CallID: "read", Assets: []assets.Ref{{ID: "input", WorkspaceRelativePath: "read.txt"}}})
	appendEvidence(t, store, id, agent.Event{Kind: agent.EventToolFinished, OutputEvidenceVersion: 1, CallID: "failed", Err: "write failed"})
	for _, item := range []struct{ call, path, status string }{{"a1", "same.txt", tools.WriteAdded}, {"a2", "same.txt", tools.WriteModified}, {"b1", "other/same.txt", tools.WriteAdded}, {"x", "preexisting.txt", tools.WriteDeleted}, {"a3", "same.txt", tools.WriteDeleted}} {
		appendEvidence(t, store, id, agent.Event{Kind: agent.EventToolFinished, OutputEvidenceVersion: 1, TurnID: "turn", CallID: item.call, FileWrites: []tools.FileWrite{{Path: item.path, Status: item.status}}})
	}
	for _, revision := range []int64{2, 1, 2} {
		appendEvidence(t, store, id, agent.Event{Kind: agent.EventPreviewArtifact, Artifact: &preview.Artifact{ArtifactID: "artifact", SessionID: id, Title: fmt.Sprintf("revision %d", revision), Revision: revision}})
	}
	data, err := collectRailOutputs(context.Background(), store, id, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if data.TotalCount != 3 || data.Coverage != "recorded_evidence" {
		t.Fatalf("wrong manifest: %+v", data)
	}
	for _, item := range data.Items {
		if item.Path == "same.txt" && (item.State != "deleted" || item.VersionCount != 3) {
			t.Fatalf("file versions: %+v", item)
		}
		if item.Kind == "preview" && (item.Latest.Artifact.Revision != 2 || item.VersionCount != 2 || item.Title != "revision 2") {
			t.Fatalf("preview replay regressed latest version: %+v", item)
		}
	}
	appendEvidence(t, store, id, agent.Event{Kind: agent.EventToolFinished, Assets: []assets.Ref{{ID: "old", WorkspaceRelativePath: "old.txt"}}})
	legacy, err := collectRailOutputs(context.Background(), store, id, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.TotalCount != 3 || legacy.Coverage != "legacy_incomplete" {
		t.Fatal("legacy read silently became output")
	}
}
func TestRailOutputPaginationAndPreviewBound(t *testing.T) {
	store := evidenceStore()
	repo := newFakeConversationRepo()
	sess, _ := repo.Create(context.Background(), t.TempDir(), "")
	other, _ := repo.Create(context.Background(), t.TempDir(), "")
	for i := 0; i < 120; i++ {
		appendEvidence(t, store, sess.ID, agent.Event{Kind: agent.EventToolFinished, OutputEvidenceVersion: 1, TurnID: "turn", CallID: fmt.Sprint(i), FileWrites: []tools.FileWrite{{Path: fmt.Sprintf("file-%d.txt", i), Status: tools.WriteAdded}}})
	}
	h := NewMux(repo, store, nil, MuxOptions{})
	path := "/v1/conversations/" + sess.ID + "/outputs"
	var page struct {
		Data struct {
			railOutputs
			NextCursor string `json:"next_cursor"`
		} `json:"data"`
	}
	decode := func(w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
	}
	decode(jobRequest(h, "GET", path+"?limit=100", ""))
	cursor := page.Data.NextCursor
	if page.Data.TotalCount != 120 || len(page.Data.Items) != 100 || cursor == "" {
		t.Fatal("first page truncated total")
	}
	decode(jobRequest(h, "GET", path+"?cursor="+cursor, ""))
	if len(page.Data.Items) != 20 {
		t.Fatal("missing tail")
	}
	if w := jobRequest(h, "GET", "/v1/conversations/"+other.ID+"/outputs?cursor="+cursor, ""); w.Code != 400 {
		t.Fatal("foreign cursor accepted")
	}
	snapshot := decodeRail(t, jobRequest(h, "GET", "/v1/conversations/"+sess.ID+"/session-rail", ""))
	outputs := snapshot.Sections["outputs"].Data.(map[string]any)
	if outputs["total_count"] != float64(120) || len(outputs["items"].([]any)) != 5 {
		t.Fatal("root preview changed directory total")
	}
	appendEvidence(t, store, sess.ID, agent.Event{Kind: agent.EventToolFinished, OutputEvidenceVersion: 1, CallID: "new", FileWrites: []tools.FileWrite{{Path: "new.txt", Status: tools.WriteAdded}}})
	if w := jobRequest(h, "GET", path+"?cursor="+cursor, ""); w.Code != 409 {
		t.Fatal("stale cursor accepted", w.Code)
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=1&limit=2", "cursor=", "cursor=bad"} {
		if w := jobRequest(h, "GET", path+"?"+query, ""); w.Code != 400 {
			t.Fatal(query, w.Code)
		}
	}
}
func TestRailContextSourceVersionSurvivesSQLiteReopen(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "source.db")
	store, err := sessionsqlite.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sess := &session.Session{ID: "source-session", WorkspacePath: root, ContextWindow: 1000, Metadata: map[string]any{}}
	source := session.RegisterContextSource(sess, "memory_files", "AGENTS.md", "AGENTS.md", "loaded version")
	sess.Messages = []model.Message{{Role: model.RoleSystem, Content: "loaded version"}}
	if err := store.Save(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	events := &conversation.StoreEventAdapter{Store: store}
	snapshot := &agent.RequestContextSnapshot{SnapshotID: "request-1", Model: "model-A", ContextWindow: 1000, HistoryRevision: session.HistoryRevision(sess.Messages), SourceCoverage: "recorded", Sources: []agent.ContextSourceInfo{{ID: source.ID, Kind: source.Kind, SHA256: source.SHA256}}, Categories: map[string]int{"memory_files": 5}, TotalEstimatedTokens: 5}
	appendEvidence(t, events, sess.ID, agent.Event{Kind: agent.EventModelRequest, InvocationID: "request-1", ContextSnapshot: snapshot})
	succeeded := true
	appendEvidence(t, events, sess.ID, agent.Event{Kind: agent.EventModelFinished, InvocationID: "request-1", ModelSucceeded: &succeeded, PromptTokens: 77})
	appendEvidence(t, events, sess.ID, agent.Event{Kind: agent.EventModelFinished, InvocationID: "foreign-request", ModelSucceeded: &succeeded, PromptTokens: 999})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sessionsqlite.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	loaded, err := reopened.Load(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("new disk version"), 0600); err != nil {
		t.Fatal(err)
	}
	repo := newFakeConversationRepo()
	repo.sessions[sess.ID] = loaded
	h := NewMux(repo, &conversation.StoreEventAdapter{Store: reopened}, nil, MuxOptions{})
	path := "/v1/conversations/" + sess.ID + "/context/sources/" + source.ID + "/content?snapshot_id=request-1"
	w := jobRequest(h, "GET", path, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "loaded version") || strings.Contains(w.Body.String(), "new disk version") {
		t.Fatal("read current disk as model-loaded source", w.Code, w.Body.String())
	}
	loaded.Messages = append(loaded.Messages, model.Message{Role: model.RoleUser, Content: "next input before the failed request"})
	appendEvidence(t, &conversation.StoreEventAdapter{Store: reopened}, sess.ID, agent.Event{Kind: agent.EventModelRequest, InvocationID: "request-2", ContextSnapshot: &agent.RequestContextSnapshot{SnapshotID: "request-2", Model: "model-B", ContextWindow: 2000, HistoryRevision: session.HistoryRevision(loaded.Messages), SourceCoverage: "recorded"}})
	failed := false
	appendEvidence(t, &conversation.StoreEventAdapter{Store: reopened}, sess.ID, agent.Event{Kind: agent.EventModelFinished, InvocationID: "request-2", ModelSucceeded: &failed, PromptTokens: 999})
	ctx := decodeRail(t, jobRequest(h, "GET", "/v1/conversations/"+sess.ID+"/session-rail", "")).Sections["context"].Data.(map[string]any)
	if ctx["prompt_tokens"] != float64(77) || ctx["measurement_snapshot_id"] != "request-1" || ctx["measurement_model"] != "model-A" || ctx["measured_at"] == nil || ctx["history_state"] != "history_changed" || ctx["request"].(map[string]any)["snapshot_id"] != "request-1" || ctx["latest_request_snapshot_id"] != "request-2" {
		t.Fatalf("uncorrelated measurement: %+v", ctx)
	}
	delete(session.ContextCatalog(loaded), source.ID)
	if w = jobRequest(h, "GET", path, ""); w.Code != 410 {
		t.Fatal("evicted source silently replaced", w.Code, w.Body.String())
	}
	if w = jobRequest(h, "GET", "/v1/conversations/"+sess.ID+"/context/sources/not-loaded/content", ""); w.Code != 404 {
		t.Fatal("arbitrary source accepted")
	}
}

func TestRailEvidenceRoutesRequireAuthentication(t *testing.T) {
	repo := newFakeConversationRepo()
	sess, _ := repo.Create(context.Background(), t.TempDir(), "")
	h := NewMux(repo, evidenceStore(), nil, MuxOptions{ServerAuth: ServerAuth{Enabled: true, Token: strings.Repeat("a", 32)}})
	for _, suffix := range []string{"outputs", "outputs/item", "context/request", "context/sources", "context/tools", "context/sources/source/content"} {
		if w := jobRequest(h, "GET", "/v1/conversations/"+sess.ID+"/"+suffix, ""); w.Code != 401 {
			t.Fatalf("unprotected %s: %d", suffix, w.Code)
		}
	}
}

func TestRailRequestExposureUsesExactRequestAndKeepsUnknown(t *testing.T) {
	data := runtimepkg.ConnectorSnapshot{Items: []runtimepkg.Connector{{ID: "mcp:server", Kind: "mcp"}, {ID: "client:capture", Kind: "client_capability"}, {ID: "mcp:not-in-request", Kind: "mcp"}}}
	snapshot := &agent.RequestContextSnapshot{SnapshotID: "request", Tools: []agent.ContextToolInfo{{Name: "lookup", Origin: "mcp", ConnectorID: "mcp:server", Executor: "server"}, {Name: "capture", Origin: "system", Executor: "client"}}}
	applyRailRequestExposure(&data, snapshot)
	if *data.Items[0].ExposedToolCount != 1 || *data.Items[1].ExposedToolCount != 1 || *data.Items[2].ExposedToolCount != 0 || data.Items[0].ExposureSnapshotID != "request" {
		t.Fatal("catalog substituted for request exposure")
	}
	empty := runtimepkg.ConnectorSnapshot{Items: []runtimepkg.Connector{{ID: "mcp:server", Kind: "mcp"}}}
	applyRailRequestExposure(&empty, &agent.RequestContextSnapshot{Tools: []agent.ContextToolInfo{{Name: "unknown", Origin: "unknown"}}})
	if empty.Items[0].ExposedToolCount != nil {
		t.Fatal("unknown origin became exact zero")
	}
}

func TestRailOutputsLegacyDiffstatRetainsOnlyProvenWrites(t *testing.T) {
	store := evidenceStore()
	appendEvidence(t, store, "legacy", agent.Event{Kind: agent.EventTurnFinished, TurnID: "old-turn", DiffStat: &agent.TurnDiffStat{Files: []tools.FileWrite{{Path: "old-output.txt", Status: tools.WriteModified}}, FilesChanged: 900}})
	data, err := collectRailOutputs(context.Background(), store, "legacy", "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if data.TotalCount != 1 || data.Coverage != "legacy_incomplete" || data.Items[0].Latest.Evidence != "turn_diffstat" || data.Items[0].Latest.CallID != "" {
		t.Fatalf("legacy scope fabricated: %+v", data)
	}
}
