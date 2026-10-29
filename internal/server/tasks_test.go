package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"code-agent/internal/collab"
	"code-agent/internal/conversation"
	"code-agent/internal/session"
	"code-agent/internal/session/sqlite"
)

func TestTaskHTTPAuthProtocolAndDefaultSessionIsolation(t *testing.T) {
	store, err := sqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Save(context.Background(), &session.Session{ID: "s", WorkspacePath: t.TempDir(), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	repo := conversation.NewSQLiteRepository(store, 32000, 24000, "scripted", "", nil)
	const token = "local-task-owner-token-01234567890123456789"
	h := NewMux(repo, &conversation.StoreEventAdapter{Store: store}, nil, MuxOptions{RuntimeInfo: RuntimeInfo{ServerID: "server"}, ServerAuth: ServerAuth{Enabled: true, Token: token}})
	call := func(method, path string, input any, authorization string, shared bool) (int, json.RawMessage) {
		t.Helper()
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		if shared {
			req = withSharedDeviceAuthContext(req)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		var envelope struct {
			Code int             `json:"code"`
			Data json.RawMessage `json:"data"`
		}
		if err = json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("response %d %s", w.Code, w.Body.String())
		}
		return w.Code, envelope.Data
	}
	auth := "Bearer " + token
	if status, _ := call("GET", "/v1/tasks/capabilities", nil, "", false); status != 401 {
		t.Fatal(status)
	}
	if status, _ := call("GET", "/v1/tasks/capabilities", nil, "Bearer bad", false); status != 401 {
		t.Fatal(status)
	}
	if status, _ := call("GET", "/v1/tasks/capabilities", nil, "", true); status != 403 {
		t.Fatalf("shared client acquired owner authority: %d", status)
	}
	status, data := call("GET", "/v1/tasks/capabilities", nil, auth, false)
	var identity collab.Identity
	if status != 200 || json.Unmarshal(data, &identity) != nil || identity.StoreID == "" {
		t.Fatalf("%d %s", status, data)
	}
	status, data = call("POST", "/v1/tasks", collab.CreateInput{RequestID: "task", WorkspaceSessionID: "s", Goal: "goal"}, auth, false)
	var created collab.Receipt
	if status != 200 || json.Unmarshal(data, &created) != nil {
		t.Fatalf("%d %s", status, data)
	}
	if status, _ = call("POST", "/v1/tasks", map[string]any{"request_id": "x", "workspace_session_id": "s", "goal": "g", "actor": "someone"}, auth, false); status != 400 {
		t.Fatal("body actor accepted")
	}
	ref := collab.SessionRef{Kind: "local", ServerID: identity.ServerID, StoreID: identity.StoreID, SessionID: "s"}
	for _, field := range []string{"observation_level", "capabilities", "agent_id"} {
		in := map[string]any{"request_id": "forge", "role": "executor", "session_ref": ref, field: "managed"}
		if status, _ := call("POST", "/v1/tasks/"+created.ID+"/participations", in, auth, false); status != 400 {
			t.Fatalf("caller-controlled %s accepted", field)
		}
	}
	status, data = call("POST", "/v1/tasks/"+created.ID+"/participations", collab.ParticipationInput{RequestID: "part", Role: "executor", SessionRef: ref}, auth, false)
	if status != 200 {
		t.Fatalf("%d %s", status, data)
	}
	status, data = call("GET", "/v1/tasks/"+created.ID, nil, auth, false)
	var snap collab.Snapshot
	if status != 200 || json.Unmarshal(data, &snap) != nil || len(snap.Participations) != 1 {
		t.Fatalf("%d %s", status, data)
	}
	status, data = call("GET", "/v1/tasks/"+created.ID+"/events?after=0&limit=1", nil, auth, false)
	var page collab.EventPage
	if status != 200 || json.Unmarshal(data, &page) != nil || !page.HasMore || len(page.Events) != 1 {
		t.Fatalf("%d %s", status, data)
	}
	for _, suffix := range []string{"?after=-1", "?after=broken", "?after=999999", "?after=", "?limit=0", "?limit=201", "?limit=2&limit=3"} {
		if status, _ = call("GET", "/v1/tasks/"+created.ID+"/events"+suffix, nil, auth, false); status != 400 {
			t.Fatalf("accepted invalid cursor %s", suffix)
		}
	}
	status, data = call("GET", "/v1/conversations", nil, auth, false)
	var sessions []ConversationRef
	if status != 200 || json.Unmarshal(data, &sessions) != nil || len(sessions) != 1 || sessions[0].ID != "s" {
		t.Fatalf("Task contaminated Session list: %d %s", status, data)
	}
	status, data = call("GET", "/v1/runtime/capabilities", nil, auth, false)
	if status != 200 || !strings.Contains(string(data), `"task_links_v1":true`) {
		t.Fatalf("missing optional capability: %s", data)
	}
	// Exercise the complete metadata/report HTTP path. No executor/model runs.
	status, data = call("POST", "/v1/task-agent-sessions", collab.ExternalInput{RequestID: "external", RuntimeKind: "codex"}, auth, false)
	var external collab.SessionRef
	if status != 200 || json.Unmarshal(data, &external) != nil {
		t.Fatalf("external %d %s", status, data)
	}
	roles := map[string]string{}
	for _, role := range []string{"planner", "reviewer"} {
		status, data = call("POST", "/v1/tasks/"+created.ID+"/participations", collab.ParticipationInput{RequestID: role, Role: role, SessionRef: external}, auth, false)
		var receipt collab.Receipt
		if status != 200 || json.Unmarshal(data, &receipt) != nil {
			t.Fatalf("%s %d %s", role, status, data)
		}
		roles[role] = receipt.ID
	}
	status, data = call("POST", "/v1/tasks/"+created.ID+"/plans", collab.ReportInput{RequestID: "plan", ParticipationID: roles["planner"], SubmissionID: "plan-1", Iteration: 1, Content: "plan"}, auth, false)
	if status != 200 {
		t.Fatalf("plan %d %s", status, data)
	}
	_, _, _, err = store.ReserveTurnInput(context.Background(), session.TurnInput{SessionID: "s", RequestID: "execution", TurnID: "t", PayloadHash: "hash", Text: "work"}, session.EventRecord{SessionID: "s", TurnID: "t", Kind: "turn_accepted", Payload: json.RawMessage(`{}`), At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	status, data = call("POST", "/v1/tasks/"+created.ID+"/attempts", collab.AttemptInput{RequestID: "bind", ParticipationID: snap.Participations[0].ParticipationID, TurnID: "t", TurnRequestID: "execution", Iteration: 1}, auth, false)
	var attempt collab.Receipt
	if status != 200 || json.Unmarshal(data, &attempt) != nil {
		t.Fatalf("attempt %d %s", status, data)
	}
	seq, err := store.RecordEvent(context.Background(), session.EventRecord{SessionID: "s", TurnID: "t", Kind: "tool_finished", Payload: json.RawMessage(`{}`), At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	status, data = call("POST", "/v1/tasks/"+created.ID+"/artifacts", collab.ArtifactInput{RequestID: "artifact", AttemptID: attempt.ID, Kind: "patch", SourceSeq: strconv.FormatInt(seq, 10)}, auth, false)
	var artifact collab.Receipt
	if status != 200 || json.Unmarshal(data, &artifact) != nil {
		t.Fatalf("artifact %d %s", status, data)
	}
	status, data = call("POST", "/v1/tasks/"+created.ID+"/reviews", collab.ReportInput{RequestID: "review", ParticipationID: roles["reviewer"], SubmissionID: "review-1", Iteration: 1, Content: "pass", Verdict: "pass", ReviewedArtifactID: artifact.ID}, auth, false)
	var reviewed collab.Receipt
	if status != 200 || json.Unmarshal(data, &reviewed) != nil || reviewed.Status != "recorded" {
		t.Fatalf("review %d %s", status, data)
	}
}

func TestTaskOptionalPortAndAnonymousOwnerFailClosed(t *testing.T) {
	store := session.NewMemoryStore()
	repo := conversation.NewSQLiteRepository(store, 32000, 24000, "m", "", nil)
	h := NewMux(repo, &conversation.StoreEventAdapter{Store: store}, nil, MuxOptions{RuntimeInfo: RuntimeInfo{ServerID: "server"}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/tasks", nil))
	if w.Code != 404 {
		t.Fatalf("unsupported store advertises task surface: %d", w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/runtime/capabilities", nil))
	if strings.Contains(w.Body.String(), `"task_links_v1":true`) {
		t.Fatal("unsupported store advertised tasks")
	}
}
