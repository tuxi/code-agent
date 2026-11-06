package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"code-agent/internal/jobs"
)

func jobRequest(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

func jobResponse(t *testing.T, w *httptest.ResponseRecorder) struct {
	Jobs []backgroundJobView `json:"jobs"`
	Job  backgroundJobView   `json:"job"`
} {
	t.Helper()
	var out struct {
		Data struct {
			Jobs []backgroundJobView `json:"jobs"`
			Job  backgroundJobView   `json:"job"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err, w.Body.String())
	}
	return out.Data
}

// Hold terminal publishing so the test can distinguish a cancel request from
// confirmed completion without racing process scheduling.
type delayedJobFinish struct {
	entered chan struct{}
	release chan struct{}
}

func (*delayedJobFinish) JobStarted(jobs.Snapshot)    {}
func (*delayedJobFinish) JobOutput(string, []byte)    {}
func (s *delayedJobFinish) JobFinished(jobs.Snapshot) { close(s.entered); <-s.release }

func TestBackgroundJobControlOwnershipCancellationAndConfirmation(t *testing.T) {
	repo := newFakeConversationRepo()
	a, _ := repo.Create(context.Background(), t.TempDir(), "")
	b, _ := repo.Create(context.Background(), t.TempDir(), "")
	registry := jobs.NewRegistry()
	sink := &delayedJobFinish{entered: make(chan struct{}), release: make(chan struct{})}
	registry.Sink = sink
	owner := jobs.Owner{SessionID: a.ID, TurnID: "turn-original", CallID: "call-original"}
	job := registry.Start(t.TempDir(), "sleep 30", []string{"sleep", "30"}, owner)
	t.Cleanup(func() {
		_ = registry.Cancel(job.Snapshot().ID)
		select {
		case <-sink.release:
		default:
			close(sink.release)
		}
		job.Wait()
	})
	h := NewMux(repo, &fakeEventStore{}, nil, MuxOptions{BackgroundJobs: registry})
	path := "/v1/conversations/" + a.ID + "/jobs"
	id := job.Snapshot().ID
	body := `{"turn_id":"turn-original","call_id":"call-original"}`
	if got := jobResponse(t, jobRequest(h, "GET", path, "")); len(got.Jobs) != 1 || got.Jobs[0].Status != jobs.Running || got.Jobs[0].ExitCode != nil {
		t.Fatalf("running snapshot: %+v", got)
	}
	if got := jobResponse(t, jobRequest(h, "GET", "/v1/conversations/"+b.ID+"/jobs", "")); len(got.Jobs) != 0 {
		t.Fatal("jobs leaked across sessions")
	}
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"/v1/conversations/" + b.ID + "/jobs/" + id + "/cancel", body, 404},
		{path + "/" + id + "/cancel", `{"turn_id":"stale","call_id":"call-original"}`, 404},
		{path + "/" + id + "/cancel", `{"turn_id":"turn-original","call_id":"stale"}`, 404},
		{path + "/missing/cancel", body, 404},
		{path + "/" + id + "/cancel", `{}`, 400},
		{path + "/" + id + "/cancel", body + ` {}`, 400},
		{path + "/" + id + "/cancel", `{"turn_id":"turn-original","call_id":"call-original","command":"bad"}`, 400},
	} {
		if w := jobRequest(h, "POST", tc.path, tc.body); w.Code != tc.status {
			t.Fatalf("%s %s: %d %s", tc.path, tc.body, w.Code, w.Body.String())
		}
	}
	w := jobRequest(h, "POST", path+"/"+id+"/cancel", body)
	view := jobResponse(t, w).Job
	if w.Code != 202 || view.Status != jobs.Running || !view.CancelRequested || view.ExitCode != nil {
		t.Fatalf("premature terminal: %d %+v", w.Code, view)
	}
	select {
	case <-sink.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not stop process")
	}
	if w := jobRequest(h, "POST", path+"/"+id+"/cancel", body); w.Code != 202 {
		t.Fatal("pending cancellation is not idempotent")
	}
	close(sink.release)
	job.Wait()
	w = jobRequest(h, "POST", path+"/"+id+"/cancel", body)
	view = jobResponse(t, w).Job
	if w.Code != 200 || view.Status != jobs.Canceled || view.ExitCode == nil {
		t.Fatalf("terminal: %d %+v", w.Code, view)
	}
}

func TestBackgroundJobControlAuthCapabilityAndUnsupported(t *testing.T) {
	repo := newFakeConversationRepo()
	a, _ := repo.Create(context.Background(), t.TempDir(), "")
	registry := jobs.NewRegistry()
	path := "/v1/conversations/" + a.ID + "/jobs"
	token := strings.Repeat("a", 32)
	h := NewMux(repo, &fakeEventStore{}, nil, MuxOptions{BackgroundJobs: registry, ServerAuth: ServerAuth{Enabled: true, Token: token}})
	for _, method := range []string{"GET", "POST"} {
		url := path
		if method == "POST" {
			url += "/any/cancel"
		}
		if w := jobRequest(h, method, url, `{}`); w.Code != 401 {
			t.Fatalf("unauthenticated %s: %d", method, w.Code)
		}
	}
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || len(jobResponse(t, w).Jobs) != 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, enabled := range []bool{false, true} {
		opts := MuxOptions{Capabilities: []string{backgroundJobControlCapability}}
		if enabled {
			opts.BackgroundJobs = registry
		}
		h := NewMux(repo, &fakeEventStore{}, nil, opts)
		w := jobRequest(h, "GET", "/v1/runtime/capabilities", "")
		var out struct {
			Data struct {
				Capabilities RuntimeCapabilities `json:"capabilities"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Data.Capabilities.BackgroundJobControl != enabled {
			t.Fatal("false capability advertisement")
		}
		if !enabled && jobRequest(h, "GET", path, "").Code != 404 {
			t.Fatal("unsupported registry accepted requests")
		}
	}
}

func TestBackgroundJobHistoryBoundAndNaturalCompletion(t *testing.T) {
	repo := newFakeConversationRepo()
	a, _ := repo.Create(context.Background(), t.TempDir(), "")
	registry := jobs.NewRegistry()
	var last *jobs.Job
	for i := 0; i < 23; i++ {
		last = registry.Start(t.TempDir(), "echo recent", []string{"echo", "recent"}, jobs.Owner{SessionID: a.ID, TurnID: "turn", CallID: "call"})
		last.Wait()
	}
	h := NewMux(repo, &fakeEventStore{}, nil, MuxOptions{BackgroundJobs: registry})
	path := "/v1/conversations/" + a.ID + "/jobs"
	views := jobResponse(t, jobRequest(h, "GET", path, "")).Jobs
	if len(views) != 20 || views[19].OutputTail != "recent\n" {
		t.Fatalf("recent history: %+v", views)
	}
	w := jobRequest(h, "POST", path+"/"+last.Snapshot().ID+"/cancel", `{"turn_id":"turn","call_id":"call"}`)
	view := jobResponse(t, w).Job
	if w.Code != 200 || view.Status != jobs.Exited || view.CancelRequested {
		t.Fatalf("natural completion mislabeled: %+v", view)
	}
	other := jobs.NewRegistry().Start(t.TempDir(), "echo other", []string{"echo", "other"}, jobs.Owner{})
	other.Wait()
	if other.Snapshot().ID == last.Snapshot().ID {
		t.Fatal("registry reused a persisted job identity")
	}
}
