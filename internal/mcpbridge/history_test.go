//go:build unix

package mcpbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type historyRuntime struct {
	mu                               sync.Mutex
	readRoot, store                  string
	rows                             []map[string]any
	events                           []map[string]any
	requests, details, pages, writes int
	afterEvents                      func()
	supports                         bool
}

func historyFixture(t *testing.T) (*ManualService, *historyRuntime, string, connectionManifest) {
	t.Helper()
	root := testRepo(t)
	readRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &historyRuntime{readRoot: readRoot, store: "store-1", supports: true}
	f.rows = []map[string]any{{"id": "foreign", "workspace_path": root, "name": "PRIVATE unrelated"}, {"id": "recent", "workspace_path": readRoot, "name": "recent"}, {"id": "older", "workspace_path": readRoot, "name": "older"}}
	f.events = []map[string]any{{"seq": int64(1), "kind": "turn_started", "session_id": "recent", "turn_id": "t1", "text": "user request"}, {"seq": int64(2), "kind": "tool_started", "session_id": "recent", "turn_id": "t1", "call_id": "call1", "tool_name": "read_file", "tool_args": map[string]any{"path": "example.py"}}, {"seq": int64(3), "kind": "tool_finished", "session_id": "recent", "turn_id": "t1", "call_id": "call1", "tool_name": "read_file", "error": "file not found", "output": map[string]any{"error": "file not found"}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests++
		if r.Method != "GET" {
			f.writes++
		}
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("fixture-token", 4) {
			w.WriteHeader(401)
			return
		}
		f.mu.Lock()
		var out any
		switch r.URL.Path {
		case "/v1/runtime/info", "/v1/tasks/capabilities":
			out = map[string]string{"server_id": "server-1", "store_id": f.store}
		case "/v1/runtime/capabilities":
			out = map[string]any{"capabilities": map[string]bool{"conversation_list_pagination_v1": f.supports}}
		case "/v1/conversations":
			f.pages++
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			start, _ := strconv.Atoi(r.URL.Query().Get("before"))
			end := start + limit
			if limit < 1 || start < 0 || start > len(f.rows) {
				f.mu.Unlock()
				w.WriteHeader(400)
				return
			}
			if end > len(f.rows) {
				end = len(f.rows)
			}
			w.Header().Set("X-Codeagent-Has-More", "0")
			if end < len(f.rows) {
				w.Header().Set("X-Codeagent-Has-More", "1")
				w.Header().Set("X-Codeagent-Next-Cursor", strconv.Itoa(end))
			}
			out = f.rows[start:end]
		case "/v1/conversations/recent":
			f.details++
			out = map[string]any{"id": "recent", "workspace_path": f.readRoot}
		case "/v1/conversations/recent/events":
			before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
			rows := []map[string]any{}
			for _, e := range f.events {
				if before == 0 || e["seq"].(int64) < before {
					rows = append(rows, e)
				}
			}
			out = rows
			w.Header().Set("X-Codeagent-Has-More-Older", "0")
			w.Header().Set("X-Codeagent-Turn-Fragment", "0")
		default:
			f.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		// Encode while locked; invoke hooks before net/http flushes the response.
		_ = json.NewEncoder(w).Encode(out)
		hook := f.afterEvents
		if strings.HasSuffix(r.URL.Path, "/events") {
			f.afterEvents = nil
		} else {
			hook = nil
		}
		f.mu.Unlock()
		if hook != nil {
			hook()
		}
	}))
	path, grant := writeGrant(t, srv.URL)
	s, err := NewManualServiceWithReadWorkspaces(context.Background(), path, root, []string{readRoot})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); srv.Close() })
	return s, f, path, grant
}

func TestHistoryDiscoveryFilteredPaginationAndExactRead(t *testing.T) {
	s, f, _, _ := historyFixture(t)
	ctx := context.Background()
	page, err := s.ListSessions(ctx, HistoryListInput{Workspace: f.readRoot, Limit: 1})
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != "recent" || page.NextCursor == "" || page.Order != "updated_at_desc,id_desc" {
		t.Fatal(page, err)
	}
	encoded, _ := json.Marshal(page)
	if strings.Contains(string(encoded), "PRIVATE") {
		t.Fatal("foreign metadata escaped")
	}
	older, err := s.ListSessions(ctx, HistoryListInput{Workspace: f.readRoot, Limit: 1, Cursor: page.NextCursor})
	if err != nil || len(older.Sessions) != 1 || older.Sessions[0].ID != "older" || older.NextCursor != "" {
		t.Fatal(older, err)
	}
	history, err := s.ReadHistory(ctx, HistoryReadInput{Workspace: f.readRoot, SessionID: "recent", Limit: 2})
	if err != nil || len(history.Events) != 2 || !history.HasMore || history.BeforeCursor != 2 {
		t.Fatal(history, err)
	}
	if _, ok := history.Events[0].Fields["tool_args"]; ok {
		t.Fatal("tool details must be opt-in")
	}
	detail, err := s.ReadHistory(ctx, HistoryReadInput{Workspace: f.readRoot, SessionID: "recent", IncludeToolDetails: true})
	if err != nil || !strings.Contains(detail.Events[1].Fields["tool_args"].Text, "example.py") || detail.Events[2].Fields["error"].Text != "file not found" {
		t.Fatal(detail, err)
	}
	earlier, err := s.ReadHistory(ctx, HistoryReadInput{Workspace: f.readRoot, SessionID: "recent", Before: history.BeforeCursor})
	if err != nil || len(earlier.Events) != 1 || earlier.Events[0].Seq != 1 {
		t.Fatal(earlier, err)
	}
	if len(s.bindings) != 0 || len(s.creations) != 0 {
		t.Fatal("history created execution state")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writes != 0 {
		t.Fatal("history wrote to Runtime")
	}
}

func TestHistoryScopesAndExecutionRemainSeparate(t *testing.T) {
	s, f, _, _ := historyFixture(t)
	foreign := t.TempDir()
	for _, root := range []string{foreign, filepath.Join(f.readRoot, "nested"), "../relative"} {
		before := f.requests
		_, err := s.ReadHistory(context.Background(), HistoryReadInput{Workspace: root, SessionID: "recent"})
		if err == nil || f.requests != before {
			t.Fatal("unapproved scope reached Runtime", root, err)
		}
	}
	identity := manualIdentity(t, s)
	_, err := s.Bind(context.Background(), ManualBindInput{ManualIdentity: identity, SessionID: "recent"})
	requireDiagnostic(t, err, "session_mismatch")
	if len(s.bindings) != 0 {
		t.Fatal("read scope became execution binding")
	}
	// Advancing Git HEAD must not prevent baseline-free history access.
	writeFile(t, filepath.Join(s.boundary.root, "delivery.txt"), "delivery")
	gitRun(t, s.boundary.root, "add", "delivery.txt")
	gitRun(t, s.boundary.root, "commit", "-qm", "delivery")
	if _, err := s.ReadHistory(context.Background(), HistoryReadInput{Workspace: f.readRoot, SessionID: "recent"}); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryRejectsCursorScopeAndUnsupportedPagination(t *testing.T) {
	s, f, _, _ := historyFixture(t)
	page, err := s.ListSessions(context.Background(), HistoryListInput{Workspace: f.readRoot, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ListSessions(context.Background(), HistoryListInput{Workspace: s.boundary.root, Cursor: page.NextCursor})
	requireDiagnostic(t, err, "invalid_tool_arguments")
	f.mu.Lock()
	f.supports = false
	before := f.pages
	f.mu.Unlock()
	_, err = s.ListSessions(context.Background(), HistoryListInput{Workspace: f.readRoot})
	requireDiagnostic(t, err, "history_pagination_unsupported")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pages != before {
		t.Fatal("unsupported pagination fell back to global list")
	}
}

func TestHistoryScanLimitIsNotEmptyCompletion(t *testing.T) {
	s, f, _, _ := historyFixture(t)
	f.mu.Lock()
	f.rows = nil
	for i := 0; i < 11; i++ {
		f.rows = append(f.rows, map[string]any{"id": "foreign", "workspace_path": s.boundary.root})
	}
	f.rows = append(f.rows, map[string]any{"id": "recent", "workspace_path": f.readRoot})
	f.mu.Unlock()
	page, err := s.ListSessions(context.Background(), HistoryListInput{Workspace: f.readRoot, Limit: 1})
	if err != nil || !page.ScanLimited || page.NextCursor == "" || len(page.Sessions) != 0 {
		t.Fatal(page, err)
	}
	page, err = s.ListSessions(context.Background(), HistoryListInput{Workspace: f.readRoot, Limit: 1, Cursor: page.NextCursor})
	if err != nil || page.ScanLimited || len(page.Sessions) != 1 || page.Sessions[0].ID != "recent" {
		t.Fatal(page, err)
	}
}

func TestHistoryRefusesLateGrantWorkspaceOrStoreChanges(t *testing.T) {
	for _, kind := range []string{"grant", "workspace", "store", "session-workspace"} {
		t.Run(kind, func(t *testing.T) {
			s, f, path, grant := historyFixture(t)
			if _, err := s.ConnectionInfo(context.Background(), ""); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			f.mu.Lock()
			f.afterEvents = func() {
				var err error
				switch kind {
				case "grant":
					grant.InstanceID = "replacement"
					var raw []byte
					raw, err = json.Marshal(grant)
					if err == nil {
						err = os.WriteFile(path, raw, 0600)
					}
				case "workspace":
					err = os.Rename(f.readRoot, f.readRoot+"-old")
					if err == nil {
						err = os.Mkdir(f.readRoot, 0700)
					}
				case "store":
					f.mu.Lock()
					f.store = "foreign-store"
					f.mu.Unlock()
				case "session-workspace":
					f.mu.Lock()
					f.readRoot = s.boundary.root
					f.mu.Unlock()
				}
				done <- err
			}
			root := f.readRoot
			f.mu.Unlock()
			_, err := s.ReadHistory(context.Background(), HistoryReadInput{Workspace: root, SessionID: "recent"})
			if err == nil {
				t.Fatal("late target change returned history")
			}
			if hookErr := <-done; hookErr != nil {
				t.Fatal(hookErr)
			}
		})
	}
}

func TestHistoryTruncationAndForeignEventsAreExplicit(t *testing.T) {
	s, f, _, _ := historyFixture(t)
	f.mu.Lock()
	f.events[1]["tool_args"] = map[string]string{"path": strings.Repeat("界", 3000)}
	f.mu.Unlock()
	page, err := s.ReadHistory(context.Background(), HistoryReadInput{Workspace: f.readRoot, SessionID: "recent", IncludeToolDetails: true})
	if err != nil || !page.Truncated || !page.Events[1].Fields["tool_args"].Truncated || !utf8.ValidString(page.Events[1].Fields["tool_args"].Text) {
		t.Fatal(page, err)
	}
	if len(page.Events[1].Fields["tool_args"].Text) > 2048 {
		t.Fatal("unbounded preview")
	}
	f.mu.Lock()
	f.events[0]["session_id"] = "foreign"
	f.mu.Unlock()
	_, err = s.ReadHistory(context.Background(), HistoryReadInput{Workspace: f.readRoot, SessionID: "recent"})
	requireDiagnostic(t, err, "evidence_incomplete")
}

func TestHistoryNativeCatalogAndNoExecutionHandle(t *testing.T) {
	s, f, _, _ := historyFixture(t)
	ctx := context.Background()
	a, b := mcp.NewInMemoryTransports()
	server, err := ManualServer(s).Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "history-test"}, nil).Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "codeagent_list_sessions", Arguments: HistoryListInput{Workspace: f.readRoot, Limit: 1}})
	if err != nil || result.IsError {
		t.Fatal(result, err)
	}
	result, err = client.CallTool(ctx, &mcp.CallToolParams{Name: "codeagent_read_history", Arguments: HistoryReadInput{Workspace: f.readRoot, SessionID: "recent"}})
	if err != nil || result.IsError {
		t.Fatal(result, err)
	}
	encoded, _ := json.Marshal(result.StructuredContent)
	if strings.Contains(string(encoded), "binding_id") {
		t.Fatal("history returned executable handle")
	}
	list, err := client.ListTools(ctx, nil)
	if err != nil || len(list.Tools) != 13 {
		t.Fatal(list, err)
	}
}

func TestHistoryLargePageHasRecoverableCursor(t *testing.T) {
	s, f, _, _ := historyFixture(t)
	f.mu.Lock()
	f.events = nil
	for i := int64(1); i <= 30; i++ {
		f.events = append(f.events, map[string]any{"seq": i, "kind": "turn_started", "session_id": "recent", "turn_id": "t1", "text": strings.Repeat("x", 8000)})
	}
	f.mu.Unlock()
	seen := map[int64]bool{}
	var before int64
	for pages := 0; pages < 30; pages++ {
		page, err := s.ReadHistory(context.Background(), HistoryReadInput{Workspace: f.readRoot, SessionID: "recent", Before: before, Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(page)
		if len(raw) > 52<<10 || !page.Truncated {
			t.Fatal("unbounded or unmarked page", len(raw))
		}
		for _, event := range page.Events {
			if seen[event.Seq] {
				t.Fatal("cursor duplicated an event")
			}
			seen[event.Seq] = true
		}
		if !page.HasMore {
			break
		}
		if page.BeforeCursor <= 0 || before > 0 && page.BeforeCursor >= before {
			t.Fatal("cursor did not progress")
		}
		before = page.BeforeCursor
	}
	if len(seen) != 30 {
		t.Fatal("cursor lost history", len(seen))
	}
}

func TestHistoryConfiguredSymlinkCannotRetarget(t *testing.T) {
	s, f, _, _ := historyFixture(t)
	link := filepath.Join(t.TempDir(), "read-alias")
	if err := os.Symlink(f.readRoot, link); err != nil {
		t.Fatal(err)
	}
	s.readScopes = append(s.readScopes, pinHistoryScope(link))
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(s.boundary.root, link); err != nil {
		t.Fatal(err)
	}
	// Validate the pinned alias directly: an independently allowed execution
	// root must not make the changed alias itself valid.
	requireDiagnostic(t, s.readScopes[len(s.readScopes)-1].check(), "workspace_mismatch")
	_, err := s.historyScope(link)
	requireDiagnostic(t, err, "workspace_mismatch")
}
