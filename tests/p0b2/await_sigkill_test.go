//go:build unix

package p0b2_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tuxi/flux-workflow/domain"
)

// This dependency-level probe isolates the commit/queue boundary: no worker
// can consume the committed pending edges before the parent sends SIGKILL.
func TestAwaitCommitProcess(t *testing.T) {
	if os.Getenv("CODEAGENT_B2C_AWAIT_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	dir := os.Getenv("B2C_AWAIT_DIR")
	r := openFlux(t, filepath.Join(dir, "flux.db"))
	recorder := &recordingTool{}
	r.ToolRegistry().Register(recorder)
	ctx := context.Background()
	d := reviewDefinition()
	if err := r.RegisterWorkflow(ctx, d); err != nil {
		t.Fatal(err)
	}
	result, err := r.Run(ctx, d, map[string]any{"task_id": "coding-task"})
	if err != nil || result.Err != nil || result.Status != "suspended" {
		t.Fatal(result, err)
	}
	completion, err := r.CompleteAwait(ctx, result.TaskID, "review1", map[string]any{"verdict": "pass"}, "")
	if err != nil || completion.Err != nil {
		t.Fatal(completion, err)
	}
	binding, err := r.AwaitBinding(ctx, result.TaskID, "review1")
	if err != nil || binding.Status != domain.AwaitBindingCompleted || !reflect.DeepEqual(recorder.snapshot(), []string{"implement1"}) {
		t.Fatal("did not stop at committed continuation", binding, err, recorder.snapshot())
	}
	raw, _ := json.Marshal(result.TaskID)
	f, err := os.Create(filepath.Join(dir, "committed.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err = f.Sync(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	select {}
}

func TestAwaitCommittedPendingEdgesSurviveSIGKILL(t *testing.T) {
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(dir, "child.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd := exec.Command(exe, "-test.run=^TestAwaitCommitProcess$")
	cmd.Env = append(os.Environ(), "CODEAGENT_B2C_AWAIT_HELPER=1", "B2C_AWAIT_DIR="+dir)
	cmd.Stdout, cmd.Stderr = log, log
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	var id int64
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, e := os.ReadFile(filepath.Join(dir, "committed.json"))
		if e == nil && json.Unmarshal(b, &id) == nil && id > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if id == 0 {
		t.Fatal("child did not commit Await")
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatal("child was not killed with SIGKILL")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "child.log"))
	if strings.Contains(string(b), "WARNING: DATA RACE") {
		t.Fatal("child race detector warning")
	}
	r := openFlux(t, filepath.Join(dir, "flux.db"))
	recorder := &recordingTool{}
	r.ToolRegistry().Register(recorder)
	startWorker(t, r)
	if err = r.Engine().TaskRepo().Enqueue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, r, id, domain.TaskSuccess)
	if !reflect.DeepEqual(recorder.snapshot(), []string{"accepted1"}) {
		t.Fatal("completed implementation replayed", recorder.snapshot())
	}
}
