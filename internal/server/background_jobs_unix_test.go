//go:build unix

package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"code-agent/internal/jobs"
)

func TestBackgroundJobStopTerminatesOwnedDescendants(t *testing.T) {
	repo := newFakeConversationRepo()
	sess, _ := repo.Create(context.Background(), t.TempDir(), "")
	registry := jobs.NewRegistry()
	root := t.TempDir()
	heartbeat := filepath.Join(root, "heartbeat")
	job := registry.Start(root, "owned descendant fixture", []string{"sh", "-c",
		`(while :; do printf x >> "$1"; sleep 0.02; done) & wait`, "job-tree", heartbeat},
		jobs.Owner{SessionID: sess.ID, TurnID: "turn", CallID: "call"})
	t.Cleanup(func() { _ = registry.Cancel(job.Snapshot().ID); job.Wait() })
	deadline := time.Now().Add(3 * time.Second)
	for {
		if b, _ := os.ReadFile(heartbeat); len(b) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("descendant did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	h := NewMux(repo, &fakeEventStore{}, nil, MuxOptions{BackgroundJobs: registry})
	w := jobRequest(h, "POST", "/v1/conversations/"+sess.ID+"/jobs/"+job.Snapshot().ID+"/cancel", `{"turn_id":"turn","call_id":"call"}`)
	if w.Code != 200 && w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	select {
	case <-job.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("process cleanup did not finish")
	}
	before, _ := os.ReadFile(heartbeat)
	time.Sleep(120 * time.Millisecond)
	after, _ := os.ReadFile(heartbeat)
	if len(before) != len(after) {
		t.Fatal("descendant survived job cancellation")
	}
	if view := backgroundJobSnapshot(job); view.Status != jobs.Canceled {
		t.Fatalf("not canceled: %+v", view)
	}
}
