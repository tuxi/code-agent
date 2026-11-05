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

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func multiFixture(t *testing.T) (*ManualService, *manualRuntime, string, connectionManifest, string) {
	t.Helper()
	f := &manualRuntime{root: testRepo(t), server: "server-1", store: "store-1", events: map[string][]Event{}, inputs: map[string]int{}, finish: false, firstInput: make(chan struct{})}
	roots := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("fixture-token", 4) {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/v1/conversations" && r.Method == "POST" {
			var in SessionRef
			if json.NewDecoder(r.Body).Decode(&in) != nil {
				w.WriteHeader(400)
				return
			}
			f.mu.Lock()
			f.posts++
			id := fmt.Sprintf("s%d", f.posts)
			roots[id] = in.WorkspacePath
			f.events[id] = []Event{}
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(SessionRef{ID: id, WorkspacePath: in.WorkspacePath})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/conversations/") && !strings.HasSuffix(r.URL.Path, "/stream") && !strings.HasSuffix(r.URL.Path, "/events") {
			id := strings.TrimPrefix(r.URL.Path, "/v1/conversations/")
			f.mu.Lock()
			root := roots[id]
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "workspace_path": root, "execution_policy": "shared_workspace"})
			return
		}
		f.serve(w, r)
	}))
	path, grant := writeGrant(t, srv.URL)
	s, err := NewMultiWorkspaceManualService(context.Background(), path, f.root, nil)
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
	return s, f, path, grant, testRepo(t)
}

func multiCreate(t *testing.T, s *ManualService, path, request string) ManualBindingRef {
	t.Helper()
	info, err := s.ConnectionInfoForWorkspace(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	if !info.WorkspaceSelection || info.Workspace != path {
		t.Fatal(info)
	}
	ref, err := s.Create(context.Background(), ManualCreateInput{ManualIdentity: ManualIdentity{ServerID: info.ServerID, StoreID: info.StoreID, Baseline: info.HeadCommit}, Workspace: path, RequestID: request})
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestMultiWorkspaceConcurrentAdmissionAndExactRouting(t *testing.T) {
	s, f, _, _, other := multiFixture(t)
	a := multiCreate(t, s, f.root, "create-a")
	b := multiCreate(t, s, other, "create-b")
	results := make(chan error, 2)
	for _, ref := range []ManualBindingRef{a, b} {
		go func(ref ManualBindingRef) {
			_, _, err := s.send(context.Background(), manualSend(ref, "run-"+ref.ID))
			results <- err
		}(ref)
	}
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	// Both are still running; selecting/creating another binding in the same
	// project must not evade the existing writer check.
	_, err := s.Create(context.Background(), ManualCreateInput{ManualIdentity: a.ManualIdentity, Workspace: f.root, RequestID: "same-project"})
	requireDiagnostic(t, err, "session_busy")
	wrong := manualSend(a, "wrong-session")
	wrong.SessionID = b.ID
	_, _, err = s.send(context.Background(), wrong)
	requireDiagnostic(t, err, "session_mismatch")
	info, err := s.ConnectionInfo(context.Background(), b.BindingID)
	if err != nil || info.Workspace != other {
		t.Fatal(info, err)
	}
	for _, ref := range []ManualBindingRef{a, b} {
		_, value, err := manualCall(context.Background(), s, ref.BindingID, ref.ID, func(c *Client) (ReadResult, error) { return c.Read(context.Background(), ref.ID, ref.ID+"-turn-1", 0) })
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(value)
		if !strings.Contains(string(raw), ref.Workspace) {
			t.Fatal("missing exact workspace", string(raw))
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inputs[a.ID] != 1 || f.inputs[b.ID] != 1 {
		t.Fatal(f.inputs)
	}
}

func TestMultiWorkspaceNoImplicitSelectionOrReadScopeEscalation(t *testing.T) {
	s, f, _, _, other := multiFixture(t)
	identity := manualIdentity(t, s)
	_, err := s.Create(context.Background(), ManualCreateInput{ManualIdentity: identity, RequestID: "missing"})
	requireDiagnostic(t, err, "workspace_selection_required")
	_, err = s.Create(context.Background(), ManualCreateInput{ManualIdentity: identity, Workspace: other, RequestID: "not-open"})
	requireDiagnostic(t, err, "workspace_not_selected")
	_, err = s.ReadHistory(context.Background(), HistoryReadInput{Workspace: other, SessionID: "s1"})
	requireDiagnostic(t, err, "read_workspace_not_allowed")
	s.readScopes = append(s.readScopes, pinHistoryScope(other))
	_, err = s.Bind(context.Background(), ManualBindInput{ManualIdentity: identity, Workspace: other, SessionID: "s1"})
	requireDiagnostic(t, err, "workspace_not_selected")
	a := multiCreate(t, s, f.root, "stable")
	info, err := s.ConnectionInfoForWorkspace(context.Background(), other, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Create(context.Background(), ManualCreateInput{ManualIdentity: ManualIdentity{ServerID: info.ServerID, StoreID: info.StoreID, Baseline: info.HeadCommit}, Workspace: other, RequestID: "stable"})
	requireDiagnostic(t, err, "request_conflict")
	_, err = s.Bind(context.Background(), ManualBindInput{ManualIdentity: a.ManualIdentity, Workspace: other, SessionID: a.ID})
	requireDiagnostic(t, err, "session_mismatch")
	_, err = s.ConnectionInfoForWorkspace(context.Background(), other, a.BindingID)
	requireDiagnostic(t, err, "invalid_tool_arguments")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 1 || len(f.inputs) != 0 {
		t.Fatal("scope check caused side effects", f.posts, f.inputs)
	}
}

func TestMultiWorkspaceSharedIdentityAndExplicitReconnect(t *testing.T) {
	s, f, path, grant, other := multiFixture(t)
	a := multiCreate(t, s, f.root, "a")
	b := multiCreate(t, s, other, "b")
	grant.InstanceID = "rotated"
	saveGrant(t, path, grant)
	_, err := s.ConnectionInfoForWorkspace(context.Background(), other, "")
	requireDiagnostic(t, err, "grant_replaced")
	fresh := testRepo(t)
	_, err = s.ConnectionInfoForWorkspace(context.Background(), fresh, "")
	requireDiagnostic(t, err, "grant_replaced")
	info, err := s.Reconnect(context.Background(), ManualReconnectInput{ServerID: a.ServerID, StoreID: a.StoreID, Workspace: other})
	if err != nil || !info.Reconnected {
		t.Fatal(info, err)
	}
	for _, ref := range []ManualBindingRef{a, b} {
		_, err := s.ConnectionInfo(context.Background(), ref.BindingID)
		requireDiagnostic(t, err, "grant_replaced")
		next, err := s.Bind(context.Background(), ManualBindInput{ManualIdentity: ref.ManualIdentity, Workspace: ref.Workspace, SessionID: ref.ID, ReplaceBindingID: ref.BindingID})
		if err != nil || next.Workspace != ref.Workspace {
			t.Fatal(next, err)
		}
	}
	grant.StoreID = "foreign"
	saveGrant(t, path, grant)
	f.mu.Lock()
	f.store = "foreign"
	f.mu.Unlock()
	_, err = s.ConnectionInfoForWorkspace(context.Background(), fresh, "")
	requireDiagnostic(t, err, "identity_mismatch")
}

func TestMultiWorkspaceRejectsAliasesOverlapAndSharedGit(t *testing.T) {
	s, f, _, _, other := multiFixture(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(other, alias); err != nil {
		t.Fatal(err)
	}
	info, err := s.ConnectionInfoForWorkspace(context.Background(), alias, "")
	if err != nil || info.Workspace != other {
		t.Fatal(info, err)
	}
	secondAlias := filepath.Join(t.TempDir(), "second-alias")
	if err := os.Symlink(other, secondAlias); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConnectionInfoForWorkspace(context.Background(), secondAlias, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(secondAlias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.root, secondAlias); err != nil {
		t.Fatal(err)
	}
	_, err = s.ConnectionInfoForWorkspace(context.Background(), secondAlias, "")
	requireDiagnostic(t, err, "workspace_mismatch")
	_, err = s.ReadHistory(context.Background(), HistoryReadInput{Workspace: secondAlias, SessionID: "s1"})
	requireDiagnostic(t, err, "workspace_mismatch")
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.root, alias); err != nil {
		t.Fatal(err)
	}
	_, err = s.ConnectionInfoForWorkspace(context.Background(), alias, "")
	requireDiagnostic(t, err, "workspace_mismatch")
	nested := filepath.Join(f.root, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, nested, "init", "-q")
	_, err = s.ConnectionInfoForWorkspace(context.Background(), nested, "")
	requireDiagnostic(t, err, "workspace_overlap")
	linked := filepath.Join(t.TempDir(), "linked")
	gitRun(t, f.root, "worktree", "add", "--detach", linked, "HEAD")
	_, err = s.ConnectionInfoForWorkspace(context.Background(), linked, "")
	requireDiagnostic(t, err, "workspace_overlap")
}

func TestMultiWorkspaceNativeSchemaAndConcurrentSelections(t *testing.T) {
	s, f, _, _, other := multiFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, b := mcp.NewInMemoryTransports()
	server, err := ManualServer(s).Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "multi-offline"}, nil).Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "codeagent_connection_info", Arguments: map[string]any{"workspace_path": other}})
			if err == nil && res.IsError {
				err = fmt.Errorf("tool error: %v", res.Content)
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(s.router.snapshot()) != 2 {
		t.Fatal("duplicate project registration")
	}
	info, err := s.ConnectionInfoForWorkspace(ctx, other, "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "codeagent_create_session", Arguments: ManualCreateInput{ManualIdentity: ManualIdentity{ServerID: info.ServerID, StoreID: info.StoreID, Baseline: info.HeadCommit}, Workspace: other, RequestID: "native"}})
	if err != nil || res.IsError {
		t.Fatal(res, err)
	}
	list, err := client.ListTools(ctx, nil)
	if err != nil || len(list.Tools) != 13 {
		t.Fatal(list, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != 1 || len(f.inputs) != 0 {
		t.Fatal("selection dispatched work")
	}
}

func TestMultiWorkspaceUnknownAdmissionIsRetainedPerProject(t *testing.T) {
	s, f, _, _, other := multiFixture(t)
	a := multiCreate(t, s, f.root, "a")
	b := multiCreate(t, s, other, "b")
	f.mu.Lock()
	f.failEvents = true
	f.mu.Unlock()
	_, _, err := s.send(context.Background(), manualSend(a, "uncertain"))
	requireDiagnostic(t, err, "admission_unknown")
	f.mu.Lock()
	f.failEvents = false
	f.events[a.ID] = []Event{}
	f.mu.Unlock()
	if _, _, err := s.send(context.Background(), manualSend(b, "independent")); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.send(context.Background(), manualSend(a, "must-not-replay"))
	requireDiagnostic(t, err, "admission_unknown")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inputs[a.ID] != 1 || f.inputs[b.ID] != 1 {
		t.Fatal(f.inputs)
	}
}

func TestMultiWorkspaceSelectionOutlivesRequestAndUnrelatedProjectFailure(t *testing.T) {
	s, f, _, _, other := multiFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := s.ConnectionInfoForWorkspace(ctx, other, ""); err != nil {
		t.Fatal(err)
	}
	cancel()
	b := multiCreate(t, s, other, "b")
	if err := os.Rename(f.root, f.root+"-removed"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.send(context.Background(), manualSend(b, "independent")); err != nil {
		t.Fatal("unrelated default root failure blocked selected project", err)
	}
}
