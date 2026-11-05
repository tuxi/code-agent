//go:build unix

package mcpbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type manualRuntime struct {
	mu                                      sync.Mutex
	root, server, store                     string
	events                                  map[string][]Event
	inputs                                  map[string]int
	conns                                   []*websocket.Conn
	posts, requests                         int
	sessionReads                            int
	afterCapabilities                       func()
	finish, failEvents, loseCreate, pending bool
	firstInput                              chan struct{}
	once                                    sync.Once
	eventGate, eventEntered                 chan struct{}
	gateOnce                                sync.Once
}

func manualFixture(t *testing.T) (*ManualService, *manualRuntime, string, connectionManifest) {
	t.Helper()
	f := &manualRuntime{root: testRepo(t), server: "server-1", store: "store-1", events: map[string][]Event{}, inputs: map[string]int{}, finish: true, firstInput: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.serve(w, r)
		if r.URL.Path == "/v1/tasks/capabilities" {
			f.mu.Lock()
			hook := f.afterCapabilities
			f.afterCapabilities = nil
			f.mu.Unlock()
			if hook != nil {
				hook()
			}
		}
	}))
	path, grant := writeGrant(t, srv.URL)
	s, err := NewManualService(context.Background(), path, f.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Close()
		f.mu.Lock()
		for _, c := range f.conns {
			_ = c.CloseNow()
		}
		f.mu.Unlock()
		srv.Close()
	})
	return s, f, path, grant
}

func (f *manualRuntime) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests++
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("fixture-token", 4) {
		w.WriteHeader(401)
		return
	}
	path := r.URL.Path
	if strings.HasSuffix(path, "/stream") {
		parts := strings.Split(path, "/")
		id := parts[len(parts)-2]
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		f.mu.Lock()
		f.conns = append(f.conns, conn)
		f.mu.Unlock()
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"hello"}`))
		for {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var in map[string]string
			_ = json.Unmarshal(raw, &in)
			f.mu.Lock()
			f.inputs[id]++
			seq := int64(len(f.events[id]) + 1)
			turn := fmt.Sprintf("%s-turn-%d", id, f.inputs[id])
			f.events[id] = append(f.events[id], Event{Kind: "turn_accepted", SessionID: id, TurnID: turn, RequestID: in["request_id"], Seq: seq})
			if f.finish {
				f.events[id] = append(f.events[id], Event{Kind: "turn_finished", SessionID: id, TurnID: turn, Seq: seq + 1})
			}
			f.mu.Unlock()
			f.once.Do(func() { close(f.firstInput) })
		}
	}
	if strings.HasSuffix(path, "/events") {
		f.mu.Lock()
		fail := f.failEvents
		gate, entered := f.eventGate, f.eventEntered
		f.mu.Unlock()
		if gate != nil {
			f.gateOnce.Do(func() { close(entered) })
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		if fail {
			select {
			case <-f.firstInput:
			case <-r.Context().Done():
				return
			}
			http.Error(w, "PRIVATE", 503)
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out any
	switch {
	case path == "/v1/runtime/info" || path == "/v1/tasks/capabilities":
		out = map[string]string{"server_id": f.server, "store_id": f.store}
	case path == "/v1/conversations" && r.Method == "POST":
		f.posts++
		id := fmt.Sprintf("s%d", f.posts)
		f.events[id] = []Event{}
		if f.loseCreate {
			http.Error(w, "PRIVATE", 503)
			return
		}
		out = SessionRef{ID: id, WorkspacePath: f.root}
	case path == "/v1/activity":
		rows := []any{}
		for id, events := range f.events {
			state, pending := "idle", 0
			if len(events) > 0 {
				state = "running"
				if events[len(events)-1].Kind == "turn_finished" {
					state = "done"
				}
			}
			if f.pending {
				state, pending = "waiting_approval", 1
			}
			rows = append(rows, map[string]any{"session_id": id, "state": state, "pending_approval_count": pending, "pending_client_tool_count": 0})
		}
		out = map[string]any{"is_delta": false, "sessions": rows}
	case strings.HasPrefix(path, "/v1/conversations/"):
		f.sessionReads++
		parts := strings.Split(path, "/")
		id := parts[3]
		events, ok := f.events[id]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if strings.HasSuffix(path, "/events") {
			out = events
		} else {
			out = map[string]any{"id": id, "workspace_path": f.root, "execution_policy": "shared_workspace"}
		}
	default:
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(out)
}

func manualIdentity(t *testing.T, s *ManualService) ManualIdentity {
	t.Helper()
	info, err := s.ConnectionInfo(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return ManualIdentity{ServerID: info.ServerID, StoreID: info.StoreID, Baseline: info.HeadCommit}
}

func manualCreate(t *testing.T, s *ManualService, request string) ManualBindingRef {
	t.Helper()
	ref, err := s.Create(context.Background(), ManualCreateInput{ManualIdentity: manualIdentity(t, s), RequestID: request})
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func manualSend(ref ManualBindingRef, request string) manualSendInput {
	return manualSendInput{manualTarget: manualTarget{BindingID: ref.BindingID}, SendInput: SendInput{SessionID: ref.SessionID, RequestID: request, Message: "offline fixture only"}}
}

func TestManualInitializeWithoutGrantAndStableCatalog(t *testing.T) {
	s, f, path, grant := manualFixture(t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, b := mcp.NewInMemoryTransports()
	server, err := ManualServer(s).Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "manual-offline"}, nil).Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	list, err := client.ListTools(ctx, nil)
	if err != nil || len(list.Tools) != 13 {
		t.Fatalf("catalog: %+v %v", list, err)
	}
	f.mu.Lock()
	requests := f.requests
	f.mu.Unlock()
	if requests != 0 {
		t.Fatal("initialize/list accessed Runtime")
	}
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "codeagent_connection_info", Arguments: map[string]any{}})
	if err != nil || !result.IsError {
		t.Fatal(result, err)
	}
	for _, tool := range list.Tools {
		if tool.Name == "codeagent_send_to_session" {
			b, _ := json.Marshal(tool.InputSchema)
			var schema struct{ Required []string }
			if json.Unmarshal(b, &schema) != nil || !contains(schema.Required, "binding_id") || !contains(schema.Required, "session_id") {
				t.Fatalf("missing explicit target: %s", b)
			}
		}
	}
	for _, name := range []string{"codeagent_start_task_run", "codeagent_report_task", "codeagent_wait_sessions"} {
		res, callErr := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		if callErr == nil && (res == nil || !res.IsError) {
			t.Fatalf("hidden tool executed: %s", name)
		}
	}
	saveGrant(t, path, grant)
	info, err := s.ConnectionInfo(ctx, "")
	if err != nil || !info.IdentityVerified || info.ExecutionBound {
		t.Fatal(info, err)
	}
	expired := grant
	expired.ExpiresAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	saveGrant(t, path, expired)
	_, err = s.ConnectionInfo(ctx, "")
	requireDiagnostic(t, err, "grant_expired")
	if _, err := client.ListTools(ctx, nil); err != nil {
		t.Fatal("expiry broke protocol connection", err)
	}
	offline := grant
	offline.Endpoint = "http://127.0.0.1:9"
	saveGrant(t, path, offline)
	_, err = s.ConnectionInfo(ctx, "")
	requireDiagnostic(t, err, "runtime_unreachable")
	if _, err := client.ListTools(ctx, nil); err != nil {
		t.Fatal("offline Runtime broke protocol connection", err)
	}
	saveGrant(t, path, grant)
	list, err = client.ListTools(ctx, nil)
	if err != nil || len(list.Tools) != 13 {
		t.Fatal("catalog changed after authorization", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 0 || len(f.inputs) != 0 {
		t.Fatal("diagnostics had side effects")
	}
}

func TestManualImmutableTargetsAndNativeDispatch(t *testing.T) {
	s, f, _, _ := manualFixture(t)
	a := manualCreate(t, s, "create-a")
	b := manualCreate(t, s, "create-b")
	if a.BindingID == b.BindingID || a.SessionID == b.SessionID {
		t.Fatal("aliased bindings")
	}
	for _, ref := range []ManualBindingRef{a, b} {
		if _, _, err := s.send(context.Background(), manualSend(ref, "work-"+ref.ID)); err != nil {
			t.Fatal(err)
		}
	}
	wrong := manualSend(a, "foreign")
	wrong.SessionID = b.SessionID
	_, _, err := s.send(context.Background(), wrong)
	requireDiagnostic(t, err, "session_mismatch")
	_, _, err = s.send(context.Background(), manualSend(ManualBindingRef{BindingID: "missing", SessionID: a.SessionID}, "missing"))
	requireDiagnostic(t, err, "binding_not_found")
	reused, err := s.Bind(context.Background(), ManualBindInput{ManualIdentity: a.ManualIdentity, SessionID: a.SessionID})
	if err != nil || reused.BindingID != a.BindingID {
		t.Fatal("duplicate bind lost original Client", reused, err)
	}
	bad := a.ManualIdentity
	bad.Baseline = strings.Repeat("a", 40)
	_, err = s.Bind(context.Background(), ManualBindInput{ManualIdentity: bad, SessionID: a.SessionID})
	requireDiagnostic(t, err, "baseline_changed")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 2 || f.inputs[a.ID] != 1 || f.inputs[b.ID] != 1 {
		t.Fatalf("wrong routing: %+v posts=%d", f.inputs, f.posts)
	}
}

func TestManualConcurrentBindingsDoNotShareCurrentSession(t *testing.T) {
	s, f, _, _ := manualFixture(t)
	a := manualCreate(t, s, "a")
	b := manualCreate(t, s, "b")
	var wg sync.WaitGroup
	errors := make(chan error, 12)
	for i := 0; i < 12; i++ {
		ref := a
		if i%2 == 1 {
			ref = b
		}
		wg.Add(1)
		go func(ref ManualBindingRef) {
			defer wg.Done()
			got, err := s.Bind(context.Background(), ManualBindInput{ManualIdentity: ref.ManualIdentity, SessionID: ref.ID})
			if err == nil && got.BindingID != ref.BindingID {
				err = fmt.Errorf("binding changed")
			}
			errors <- err
		}(ref)
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	sends := make(chan error, 2)
	for _, ref := range []ManualBindingRef{a, b} {
		go func(ref ManualBindingRef) {
			_, _, err := s.send(context.Background(), manualSend(ref, "concurrent-"+ref.ID))
			sends <- err
		}(ref)
	}
	for i := 0; i < 2; i++ {
		if err := <-sends; err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inputs[a.ID] != 1 || f.inputs[b.ID] != 1 {
		t.Fatal("concurrent sends crossed targets", f.inputs)
	}
}

func TestManualGrantReplacementRetainsUncertainSubmission(t *testing.T) {
	s, f, path, grant := manualFixture(t)
	a := manualCreate(t, s, "a")
	f.mu.Lock()
	f.failEvents = true
	f.mu.Unlock()
	_, _, err := s.send(context.Background(), manualSend(a, "uncertain"))
	requireDiagnostic(t, err, "admission_unknown")
	f.mu.Lock()
	f.failEvents = false
	f.events[a.ID] = []Event{}
	f.mu.Unlock()
	grant.InstanceID = "renewed"
	saveGrant(t, path, grant)
	_, err = s.Bind(context.Background(), ManualBindInput{ManualIdentity: a.ManualIdentity, SessionID: a.ID})
	requireDiagnostic(t, err, "grant_replaced")
	renewed, err := s.Bind(context.Background(), ManualBindInput{ManualIdentity: a.ManualIdentity, SessionID: a.ID, ReplaceBindingID: a.BindingID})
	if err != nil || renewed.BindingID == a.BindingID {
		t.Fatal(renewed, err)
	}
	_, _, err = s.send(context.Background(), manualSend(renewed, "new-request"))
	requireDiagnostic(t, err, "admission_unknown")
	_, err = s.ConnectionInfo(context.Background(), a.BindingID)
	requireDiagnostic(t, err, "binding_not_found")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inputs[a.ID] != 1 {
		t.Fatal("replacement replayed uncertain execution")
	}
}

func TestManualBlocksSecondWriterAndPendingApprovals(t *testing.T) {
	s, f, _, _ := manualFixture(t)
	a := manualCreate(t, s, "a")
	b := manualCreate(t, s, "b")
	f.mu.Lock()
	f.finish = false
	f.mu.Unlock()
	if _, _, err := s.send(context.Background(), manualSend(a, "running")); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.send(context.Background(), manualSend(b, "parallel"))
	requireDiagnostic(t, err, "session_busy")
	f.mu.Lock()
	turn := f.events[a.ID][0].TurnID
	f.events[a.ID] = append(f.events[a.ID], Event{Kind: "turn_finished", SessionID: a.ID, TurnID: turn, Seq: 2})
	f.pending = true
	f.mu.Unlock()
	_, _, err = s.send(context.Background(), manualSend(b, "approval-bypass"))
	requireDiagnostic(t, err, "session_busy")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inputs[b.ID] != 0 {
		t.Fatal("second writer or approval bypass")
	}
}

func TestManualCreateUnknownCannotBeClearedByNewRequest(t *testing.T) {
	s, f, _, _ := manualFixture(t)
	in := ManualCreateInput{ManualIdentity: manualIdentity(t, s), RequestID: "first"}
	f.mu.Lock()
	f.loseCreate = true
	f.mu.Unlock()
	_, err := s.Create(context.Background(), in)
	requireDiagnostic(t, err, "create_unknown")
	f.mu.Lock()
	f.loseCreate = false
	f.mu.Unlock()
	in.RequestID = "second"
	_, err = s.Create(context.Background(), in)
	requireDiagnostic(t, err, "create_unknown")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 1 {
		t.Fatal("unknown create was repeated")
	}
}

func TestManualNewTaskAfterDeliveryKeepsOldBaseline(t *testing.T) {
	s, f, _, _ := manualFixture(t)
	a := manualCreate(t, s, "a")
	writeFile(t, filepath.Join(f.root, "delivered.txt"), "delivery\n")
	gitRun(t, f.root, "add", "delivered.txt")
	gitRun(t, f.root, "commit", "-qm", "delivery")
	_, _, err := s.send(context.Background(), manualSend(a, "old-baseline"))
	requireDiagnostic(t, err, "baseline_changed")
	b := manualCreate(t, s, "b")
	if a.Baseline == b.Baseline || a.BindingID == b.BindingID {
		t.Fatal("old baseline overwritten")
	}
	if _, _, err := s.send(context.Background(), manualSend(b, "new-work")); err != nil {
		t.Fatal(err)
	}
}

func TestManualBoundaryCannotAdoptAppearingWorkspace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-yet-created")
	s, err := NewManualService(context.Background(), "/private/missing-grant", path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, path, "init", "-q")
	_, err = s.ConnectionInfo(context.Background(), "")
	requireDiagnostic(t, err, "workspace_mismatch")
}

func TestManualNativeRoundTripAndRestartObservation(t *testing.T) {
	s, f, path, _ := manualFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, b := mcp.NewInMemoryTransports()
	server, err := ManualServer(s).Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "manual-roundtrip"}, nil).Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	call := func(name string, in, out any) {
		t.Helper()
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: in})
		if err != nil || result.IsError {
			t.Fatalf("%s: %+v %v", name, result, err)
		}
		encoded, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, out); err != nil {
			t.Fatal(err)
		}
	}
	var ref ManualBindingRef
	call("codeagent_create_session", ManualCreateInput{ManualIdentity: manualIdentity(t, s), RequestID: "native-create"}, &ref)
	var receipt struct {
		Receipt
		BindingID string `json:"binding_id"`
	}
	call("codeagent_send_to_session", manualSend(ref, "native-send"), &receipt)
	if !receipt.Accepted || receipt.BindingID != ref.BindingID {
		t.Fatal(receipt)
	}
	var handoff Handoff
	call("codeagent_get_handoff", manualHandoffInput{manualTarget: manualTarget{BindingID: ref.BindingID}, HandoffInput: HandoffInput{SessionID: ref.ID, TurnID: receipt.TurnID}}, &handoff)
	if handoff.ExecutionStatus != "completed" || handoff.AcceptanceStatus != "not_reviewed" {
		t.Fatal(handoff)
	}
	s.Close()
	restarted, err := NewManualService(context.Background(), path, f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	_, err = restarted.ConnectionInfo(ctx, ref.BindingID)
	requireDiagnostic(t, err, "binding_not_found")
	rebound, err := restarted.Bind(ctx, ManualBindInput{ManualIdentity: ref.ManualIdentity, SessionID: ref.ID})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = restarted.send(ctx, manualSend(rebound, "native-send"))
	requireDiagnostic(t, err, "already_admitted")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inputs[ref.ID] != 1 {
		t.Fatal("restart replayed execution")
	}
}

func TestManualBindingLifetimeOutlivesCreateRequest(t *testing.T) {
	s, _, _, _ := manualFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	ref, err := s.Create(ctx, ManualCreateInput{ManualIdentity: manualIdentity(t, s), RequestID: "short-request"})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := s.ConnectionInfo(context.Background(), ref.BindingID); err != nil {
		t.Fatal("request cancellation closed published binding", err)
	}
}

func TestManualIdentityAndWorkspaceDriftRefuseWrites(t *testing.T) {
	s, f, path, grant := manualFixture(t)
	ref := manualCreate(t, s, "original")
	grant.StoreID = "foreign-store"
	saveGrant(t, path, grant)
	f.mu.Lock()
	f.store = grant.StoreID
	f.mu.Unlock()
	_, err := s.Bind(context.Background(), ManualBindInput{ManualIdentity: ref.ManualIdentity, SessionID: ref.ID, ReplaceBindingID: ref.BindingID})
	requireDiagnostic(t, err, "identity_mismatch")
	grant.StoreID = ref.StoreID
	saveGrant(t, path, grant)
	f.mu.Lock()
	f.store = grant.StoreID
	f.mu.Unlock()
	if err := os.Rename(f.root, f.root+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.root, 0700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, f.root, "init", "-q")
	_, _, err = s.send(context.Background(), manualSend(ref, "must-not-write"))
	requireDiagnostic(t, err, "workspace_mismatch")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inputs[ref.ID] != 0 {
		t.Fatal("identity drift wrote to Runtime")
	}
}

func TestManualLateReadRetainsOldBindingIdentity(t *testing.T) {
	s, f, path, grant := manualFixture(t)
	old := manualCreate(t, s, "a")
	gate, entered := make(chan struct{}), make(chan struct{})
	f.mu.Lock()
	f.eventGate, f.eventEntered = gate, entered
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type response struct {
		value any
		err   error
	}
	readDone := make(chan response, 1)
	go func() {
		_, v, err := manualCall(ctx, s, old.BindingID, old.ID, func(c *Client) (ReadResult, error) { return c.Read(ctx, old.ID, "unstarted", 0) })
		readDone <- response{v, err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("read did not reach delay gate")
	}
	grant.InstanceID = "renewed"
	saveGrant(t, path, grant)
	type bindingResponse struct {
		ref ManualBindingRef
		err error
	}
	bindDone := make(chan bindingResponse, 1)
	go func() {
		ref, err := s.Bind(ctx, ManualBindInput{ManualIdentity: old.ManualIdentity, SessionID: old.ID, ReplaceBindingID: old.BindingID})
		bindDone <- bindingResponse{ref, err}
	}()
	select {
	case result := <-bindDone:
		t.Fatalf("replacement bypassed in-flight read: %+v", result)
	case <-time.After(50 * time.Millisecond):
	}
	close(gate)
	read := <-readDone
	if read.err != nil {
		t.Fatal(read.err)
	}
	encoded, _ := json.Marshal(read.value)
	var identity struct {
		BindingID string `json:"binding_id"`
	}
	if json.Unmarshal(encoded, &identity) != nil || identity.BindingID != old.BindingID {
		t.Fatal("late read lost original target", string(encoded))
	}
	renewed := <-bindDone
	if renewed.err != nil || renewed.ref.BindingID == old.BindingID {
		t.Fatal(renewed)
	}
	if _, err := s.ConnectionInfo(ctx, renewed.ref.BindingID); err != nil {
		t.Fatal(err)
	}
}

func TestManualGrantChangeDuringProbeCannotRetargetSessionReads(t *testing.T) {
	s, f, path, grant := manualFixture(t)
	identity := manualIdentity(t, s)
	changed := grant
	changed.InstanceID = "replacement"
	changed.StoreID = "different-store"
	data, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	hookDone := make(chan error, 1)
	f.mu.Lock()
	f.afterCapabilities = func() {
		f.mu.Lock()
		f.store = changed.StoreID
		f.mu.Unlock()
		hookDone <- os.WriteFile(path, data, 0600)
	}
	f.mu.Unlock()
	_, err = s.Bind(context.Background(), ManualBindInput{ManualIdentity: identity, SessionID: "s1"})
	requireDiagnostic(t, err, "grant_replaced")
	if err := <-hookDone; err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sessionReads != 0 || f.posts != 0 {
		t.Fatal("grant rotation retargeted Session IO")
	}
}

func TestManualEvidenceReplayAndOtherWriterAreRaceSafe(t *testing.T) {
	original, f, path, _ := manualFixture(t)
	a := manualCreate(t, original, "a")
	b := manualCreate(t, original, "b")
	if _, _, err := original.send(context.Background(), manualSend(a, "first")); err != nil {
		t.Fatal(err)
	}
	original.Close()
	s, err := NewManualService(context.Background(), path, f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err = s.Bind(context.Background(), ManualBindInput{ManualIdentity: a.ManualIdentity, SessionID: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	b, err = s.Bind(context.Background(), ManualBindInput{ManualIdentity: b.ManualIdentity, SessionID: b.ID})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	turn := f.events[a.ID][0].TurnID
	for i := 0; i < 300; i++ {
		seq := int64(len(f.events[a.ID]) + 1)
		id := fmt.Sprintf("external-%d", i)
		f.events[a.ID] = append(f.events[a.ID], Event{Kind: "turn_accepted", SessionID: a.ID, TurnID: id, RequestID: id, Seq: seq}, Event{Kind: "turn_finished", SessionID: a.ID, TurnID: id, Seq: seq + 1})
	}
	f.mu.Unlock()
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, _, err := manualCall(context.Background(), s, a.BindingID, a.ID, func(c *Client) (Artifacts, error) { return c.Artifacts(context.Background(), a.ID, turn) })
		results <- err
	}()
	go func() {
		<-start
		_, _, err := s.send(context.Background(), manualSend(b, "other-writer"))
		results <- err
	}()
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inputs[a.ID] != 1 || f.inputs[b.ID] != 1 {
		t.Fatal("replay dispatched work or crossed targets", f.inputs)
	}
}
