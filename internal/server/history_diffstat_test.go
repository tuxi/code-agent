package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/session"
	sessionsqlite "code-agent/internal/session/sqlite"
	"code-agent/internal/tools"
)

// replayFrames seeds a session with the given events through the real sqlite
// event store and reads them back over the history endpoint, i.e. the path a
// client takes when it reopens an old conversation.
func replayFrames(t *testing.T, id string, events ...agent.Event) []map[string]any {
	t.Helper()
	store, err := sessionsqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	for _, ev := range events {
		ev.SessionID = id
		if _, err := store.RecordEvent(ctx, storedEvent(ev)); err != nil {
			t.Fatal(err)
		}
	}
	repo := newFakeConversationRepo()
	repo.sessions[id] = &session.Session{ID: id, WorkspacePath: "/tmp/replay"}
	srv := httptest.NewServer(newTestMux(repo, &conversation.StoreEventAdapter{Store: store}))
	defer srv.Close()

	url := srv.URL + "/v1/conversations/" + id + "/events?tail=" + strconv.Itoa(len(events))
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("replay status=%d", resp.StatusCode)
	}
	var frames []map[string]any
	decodeResponse(t, resp, &frames)
	if len(frames) != len(events) {
		t.Fatalf("replayed %d frames, want %d", len(frames), len(events))
	}
	return frames
}

// The live tests pin toWire's encoding, but a reopened conversation goes through
// a different path: the terminal event is persisted as raw agent.Event JSON
// (PascalCase keys — agent.Event has no struct tags), then decoded back by
// decodeStoredEvent before toWire runs. Nothing else covers that round trip, so
// a field rename or a dropped tag in TurnDiffStat would silently make
// "edited N files" vanish from history.
func TestReplayedTurnKeepsDiffStat(t *testing.T) {
	at := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	stat := &agent.TurnDiffStat{}
	stat.Add(tools.FileWrite{Path: "internal/agent/loop.go", Added: 12, Removed: 3, Status: tools.WriteModified})
	stat.Add(tools.FileWrite{Path: "docs/turn-diffstat-v1.md", Added: 96, Status: tools.WriteAdded})

	frames := replayFrames(t, "replay-diffstat",
		agent.Event{Kind: agent.EventTurnStarted, TurnID: "t1", At: at},
		agent.Event{Kind: agent.EventTurnFinished, TurnID: "t1", At: at, DiffStat: stat},
	)

	ds, ok := frames[1]["diffstat"].(map[string]any)
	if !ok {
		raw, _ := json.Marshal(frames[1])
		t.Fatalf("history dropped diffstat: %s", raw)
	}
	if ds["files_changed"] != float64(2) || ds["added_lines"] != float64(108) || ds["removed_lines"] != float64(3) {
		t.Fatalf("summary = %v, want 2 / 108 / 3", ds)
	}
	files, ok := ds["files"].([]any)
	if !ok || len(files) != 2 {
		t.Fatalf("files = %v", ds["files"])
	}
	first, _ := files[0].(map[string]any)
	if first["path"] != "internal/agent/loop.go" || first["added"] != float64(12) ||
		first["removed"] != float64(3) || first["status"] != tools.WriteModified {
		t.Fatalf("files[0] = %v", first)
	}
	// The wire contract is snake_case; a persisted PascalCase key leaking into
	// the replayed frame would break every client decoder at once.
	for _, key := range []string{"Path", "Added", "Removed", "Status"} {
		if _, ok := first[key]; ok {
			t.Errorf("replayed files[0] leaked Go field name %q: %v", key, first)
		}
	}
}

// Opening a turn that touched more than maxWireDiffFiles files must still report
// the full files_changed from history: the file list clips, the summary does not.
// The client shows that number in both the folded header and the card title, so a
// clipped summary would understate the turn as soon as it is reopened.
func TestReplayedTurnKeepsFullFileCountWhenClipped(t *testing.T) {
	at := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	stat := &agent.TurnDiffStat{}
	for i := 0; i < 250; i++ {
		stat.Add(tools.FileWrite{Path: "pkg/file_" + strconv.Itoa(i) + strings.Repeat("_", i%3) + ".go", Added: 1, Status: tools.WriteModified})
	}

	frames := replayFrames(t, "replay-clipped",
		agent.Event{Kind: agent.EventTurnStarted, TurnID: "t1", At: at},
		agent.Event{Kind: agent.EventTurnFinished, TurnID: "t1", At: at, DiffStat: stat},
	)

	ds, ok := frames[1]["diffstat"].(map[string]any)
	if !ok {
		t.Fatal("history dropped diffstat")
	}
	if got := len(ds["files"].([]any)); got != maxWireDiffFiles {
		t.Fatalf("files = %d, want %d", got, maxWireDiffFiles)
	}
	if ds["files_changed"] != float64(len(stat.Files)) {
		t.Fatalf("files_changed = %v, want the full %d", ds["files_changed"], len(stat.Files))
	}
}

// A failed turn still reports what it wrote before it stopped — on replay too.
func TestReplayedFailedTurnKeepsDiffStat(t *testing.T) {
	at := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	stat := &agent.TurnDiffStat{}
	stat.Add(tools.FileWrite{Path: "a.go", Added: 1, Status: tools.WriteModified})

	frames := replayFrames(t, "replay-failed",
		agent.Event{Kind: agent.EventTurnStarted, TurnID: "t1", At: at},
		agent.Event{Kind: agent.EventTurnFailed, TurnID: "t1", Err: "boom", At: at, DiffStat: stat},
	)

	ds, ok := frames[1]["diffstat"].(map[string]any)
	if !ok {
		t.Fatal("replayed turn_failed dropped its diffstat")
	}
	if ds["files_changed"] != float64(1) {
		t.Fatalf("files_changed = %v", ds["files_changed"])
	}
}
