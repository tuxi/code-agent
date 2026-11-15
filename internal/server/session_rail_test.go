package server

import (
	"code-agent/internal/conversation"
	runtimepkg "code-agent/internal/runtime"
	sessionsqlite "code-agent/internal/session/sqlite"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/jobs"
	"code-agent/internal/session"
)

func decodeRail(t *testing.T, w *httptest.ResponseRecorder) railSnapshot {
	t.Helper()
	var envelope struct {
		Data railSnapshot `json:"data"`
	}
	if w.Code != 200 {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data
}

func TestSessionRailContractScopeAndMissingData(t *testing.T) {
	repo := newFakeConversationRepo()
	sess, _ := repo.Create(context.Background(), t.TempDir(), "")
	sess.Metadata = map[string]any{session.MetaWorkspaceID: "workspace-original"}
	sess.PromptTokens = 120
	h := NewMux(repo, &fakeEventStore{}, nil, MuxOptions{RuntimeInfo: RuntimeInfo{ServerID: "source-A", DisplayName: "Runtime A"}, Capabilities: []string{sessionRailCapability}})
	path := "/v1/conversations/" + sess.ID + "/session-rail"
	snap := decodeRail(t, jobRequest(h, "GET", path+"?workspace_path=/another/workspace", ""))
	if snap.Schema != "session-rail/v1" || snap.ServerID != "source-A" || snap.ConversationID != sess.ID || snap.WorkspaceID != "workspace-original" || snap.RuntimeInstanceID == "" || snap.SnapshotID == "" {
		t.Fatalf("scope: %+v", snap)
	}
	if len(snap.Sections) != 6 {
		t.Fatalf("section count: %d", len(snap.Sections))
	}
	for _, name := range []string{"connectors", "outputs"} {
		section := snap.Sections[name]
		if section.State != "unavailable" || section.Code == "" || section.Data != nil {
			t.Fatalf("%s pretends to be empty/available: %+v", name, section)
		}
	}
	env := snap.Sections["environment"].Data.(map[string]any)
	if env["workspace"].(map[string]any)["root_path"] != sess.WorkspacePath {
		t.Fatal("query escaped session workspace")
	}
	ctx := snap.Sections["context"].Data.(map[string]any)
	if ctx["measurement_state"] != "legacy_without_provenance" || ctx["measured_at"] != nil || ctx["window_source"] != "fallback" || ctx["breakdown_state"] != "unavailable" {
		t.Fatalf("invented provenance: %+v", ctx)
	}
	if ctx["prompt_tokens"] != float64(120) {
		t.Fatal("lost legacy measurement")
	}
	sess.PromptTokens = 0
	ctx = decodeRail(t, jobRequest(h, "GET", path, "")).Sections["context"].Data.(map[string]any)
	if ctx["prompt_tokens"] != nil || ctx["measurement_state"] != "unavailable" {
		t.Fatal("unknown shown as measured zero")
	}
	if w := jobRequest(h, "GET", "/v1/conversations/missing/session-rail", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if w := jobRequest(h, "POST", path, ""); w.Code != 405 {
		t.Fatal("read route accepted mutation", w.Code)
	}
	if w := jobRequest(h, "GET", "/v1/runtime/capabilities", ""); strings.Contains(w.Body.String(), `"session_rail_v1":true`) {
		t.Fatal("premature full capability")
	}
}

func TestSessionRailAuthenticationAndRuntimeIdentity(t *testing.T) {
	repo := newFakeConversationRepo()
	sess, _ := repo.Create(context.Background(), t.TempDir(), "")
	token := strings.Repeat("a", 32)
	h := NewMux(repo, &fakeEventStore{}, nil, MuxOptions{ServerAuth: ServerAuth{Enabled: true, Token: token}})
	for _, suffix := range []string{"session-rail", "jobs/history"} {
		path := "/v1/conversations/" + sess.ID + "/" + suffix
		if w := jobRequest(h, "GET", path, ""); w.Code != 401 {
			t.Fatalf("unprotected %s: %d", suffix, w.Code)
		}
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	first := NewMux(repo, &fakeEventStore{}, nil, MuxOptions{})
	second := NewMux(repo, &fakeEventStore{}, nil, MuxOptions{})
	a := decodeRail(t, jobRequest(first, "GET", "/v1/conversations/"+sess.ID+"/session-rail", ""))
	b := decodeRail(t, jobRequest(second, "GET", "/v1/conversations/"+sess.ID+"/session-rail", ""))
	if a.RuntimeInstanceID != b.RuntimeInstanceID {
		t.Fatal("mux recreation changed process identity")
	}
}

func appendRailJob(t *testing.T, store *fakeEventStore, parent, id string, at time.Time, finished bool) {
	t.Helper()
	ev := agent.Event{Kind: agent.EventJobStarted, SessionID: id, TurnID: "turn-" + id, CallID: "call-" + id, Text: "echo " + id, At: at}
	if finished {
		ev.Kind = agent.EventJobFinished
		ev.Text = string(jobs.Exited)
		ev.Elapsed = time.Second
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(context.Background(), session.EventRecord{SessionID: parent, TurnID: ev.TurnID, Kind: string(ev.Kind), At: at, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func TestRailJobsHistoryAcrossRestartAndOverTwenty(t *testing.T) {
	repo := newFakeConversationRepo()
	sess, _ := repo.Create(context.Background(), t.TempDir(), "")
	other, _ := repo.Create(context.Background(), t.TempDir(), "")
	events := &fakeEventStore{}
	now := time.Now().UTC()
	for i := 0; i < 25; i++ {
		id := fmt.Sprintf("job-%02d", i)
		at := now.Add(time.Duration(i) * time.Second)
		appendRailJob(t, events, sess.ID, id, at, false)
		appendRailJob(t, events, sess.ID, id, at.Add(time.Second), true)
	}
	appendRailJob(t, events, sess.ID, "old-unfinished", now.Add(time.Minute), false)
	appendRailJob(t, events, other.ID, "other-job", now, false)
	data, err := collectRailJobs(context.Background(), events, jobs.NewRegistry(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if data.TotalCount != 26 || data.TerminalCount != 25 || data.UnknownCount != 1 || *data.ActiveCount != 0 {
		t.Fatalf("wrong accounting: %+v", data)
	}
	if j := data.Items[0]; j.JobID != "old-unfinished" || j.Status != "unknown" || j.CanControl || j.ExitCode != nil {
		t.Fatalf("restart fabricated completion: %+v", j)
	}
	h := NewMux(repo, events, nil, MuxOptions{BackgroundJobs: jobs.NewRegistry()})
	path := "/v1/conversations/" + sess.ID + "/jobs/history"
	var page struct {
		Data struct {
			railJobs
			NextCursor string `json:"next_cursor"`
		} `json:"data"`
	}
	w := jobRequest(h, "GET", path+"?limit=10", "")
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(page.Data.Items) != 10 || page.Data.TotalCount != 26 || page.Data.NextCursor == "" {
		t.Fatalf("first page: %d %s", w.Code, w.Body.String())
	}
	cursor := page.Data.NextCursor
	w = jobRequest(h, "GET", path+"?limit=100&cursor="+cursor, "")
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(page.Data.Items) != 16 {
		t.Fatalf("second page: %d %s", w.Code, w.Body.String())
	}
	if w = jobRequest(h, "GET", "/v1/conversations/"+other.ID+"/jobs/history?cursor="+cursor, ""); w.Code != 400 {
		t.Fatal("cross-session cursor accepted", w.Code)
	}
	appendRailJob(t, events, sess.ID, "new-job", now.Add(2*time.Minute), false)
	if w = jobRequest(h, "GET", path+"?cursor="+cursor, ""); w.Code != 409 {
		t.Fatal("stale cursor accepted", w.Code)
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=bad", "limit=", "limit=1&limit=2", "cursor=bad", "cursor="} {
		if w = jobRequest(h, "GET", path+"?"+query, ""); w.Code != 400 {
			t.Fatalf("invalid query %s: %d", query, w.Code)
		}
	}
}

type failingRailEvents struct{ fakeEventStore }

func (*failingRailEvents) Replay(context.Context, string) ([]session.EventRecord, error) {
	return nil, errors.New("private storage error")
}

func TestRailJobsPartialFailureAndUnknownRegistry(t *testing.T) {
	repo := newFakeConversationRepo()
	sess, _ := repo.Create(context.Background(), t.TempDir(), "")
	h := NewMux(repo, &failingRailEvents{}, nil, MuxOptions{})
	snap := decodeRail(t, jobRequest(h, "GET", "/v1/conversations/"+sess.ID+"/session-rail", ""))
	if snap.Sections["background_tasks"].State != "unavailable" || snap.Sections["environment"].State != "ready" {
		t.Fatal("partial failure damaged other sections")
	}
	if w := jobRequest(h, "GET", "/v1/conversations/"+sess.ID+"/jobs/history", ""); w.Code != 503 || strings.Contains(w.Body.String(), "private storage error") {
		t.Fatal(w.Code, w.Body.String())
	}
	data, err := collectRailJobs(context.Background(), &fakeEventStore{}, nil, sess.ID)
	if err != nil || data.ActiveCount != nil || data.StoppingCount != nil {
		t.Fatal("missing registry claims zero live jobs")
	}
}

func TestRailJobsStopRequestDoesNotBecomeTerminal(t *testing.T) {
	registry := jobs.NewRegistry()
	sink := &delayedJobFinish{entered: make(chan struct{}), release: make(chan struct{})}
	registry.Sink = sink
	job := registry.Start(t.TempDir(), "sleep 30", []string{"sleep", "30"}, jobs.Owner{SessionID: "parent", TurnID: "turn", CallID: "call"})
	t.Cleanup(func() {
		_ = registry.Cancel(job.Snapshot().ID)
		select {
		case <-sink.release:
		default:
			close(sink.release)
		}
		job.Wait()
	})
	if err := registry.Cancel(job.Snapshot().ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sink.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("job did not stop")
	}
	data, err := collectRailJobs(context.Background(), &fakeEventStore{}, registry, "parent")
	if err != nil {
		t.Fatal(err)
	}
	j := data.Items[0]
	if j.Status != "running" || !j.CancelRequested || !j.CanControl || j.ExitCode != nil || j.FinishedAt != "" || j.StartedAt == "" || *data.StoppingCount != 1 {
		t.Fatalf("premature terminal: %+v", j)
	}
	close(sink.release)
	job.Wait()
	data, err = collectRailJobs(context.Background(), &fakeEventStore{}, registry, "parent")
	if err != nil {
		t.Fatal(err)
	}
	if j = data.Items[0]; j.Status != "canceled" || j.CanControl || j.FinishedAt == "" || j.ExitCode == nil {
		t.Fatalf("missing confirmation: %+v", j)
	}
}

func TestRailJobHistoryUsesDurableParentBracketsAfterReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "history.db")
	store, err := sessionsqlite.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	registry := jobs.NewRegistry()
	registry.Sink = runtimepkg.NewJobEventSink(ctx, store)
	repo := newFakeConversationRepo()
	sess, _ := repo.Create(ctx, t.TempDir(), "")
	for i := 0; i < 23; i++ {
		job := registry.Start(sess.WorkspacePath, "echo recorded", []string{"echo", "recorded"}, jobs.Owner{SessionID: sess.ID, TurnID: fmt.Sprintf("turn-%d", i), CallID: fmt.Sprintf("call-%d", i)})
		job.Wait()
	}
	registry.Sink.JobStarted(jobs.Snapshot{ID: "interrupted-job", Command: "long build", Owner: jobs.Owner{SessionID: sess.ID, TurnID: "old-turn", CallID: "old-call"}})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sessionsqlite.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	events := &conversation.StoreEventAdapter{Store: reopened}
	data, err := collectRailJobs(ctx, events, jobs.NewRegistry(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if data.TotalCount != 24 || data.TerminalCount != 23 || data.UnknownCount != 1 || *data.ActiveCount != 0 {
		t.Fatalf("lost persisted history: %+v", data)
	}
	for _, j := range data.Items {
		if j.CanControl {
			t.Fatal("historical job acquired live control")
		}
		if j.Status == "exited" && (j.StartedAt == "" || j.FinishedAt == "" || j.ExitCode == nil || *j.ExitCode != 0) {
			t.Fatalf("lost lifecycle: %+v", j)
		}
	}
	h := NewMux(repo, events, nil, MuxOptions{BackgroundJobs: jobs.NewRegistry()})
	snap := decodeRail(t, jobRequest(h, "GET", "/v1/conversations/"+sess.ID+"/session-rail", ""))
	summary := snap.Sections["background_tasks"].Data.(map[string]any)
	if summary["total_count"] != float64(24) || len(summary["items"].([]any)) != 3 {
		t.Fatal("summary preview changed complete count", summary)
	}
}
