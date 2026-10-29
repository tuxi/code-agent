//go:build unix

package shell

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"code-agent/internal/tools"
)

func TestForegroundCancellationOutcomes(t *testing.T) {
	for _, command := range []string{"sleep 20", "sleep 20 && echo should-not-run"} {
		for _, cause := range []string{"timeout", "cancel"} {
			t.Run(command+"/"+cause, func(t *testing.T) {
				tool := NewRunCommandTool()
				tool.Timeout = 100 * time.Millisecond
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				want := "timed_out"
				if cause == "cancel" {
					tool.Timeout = 10 * time.Second
					timer := time.AfterFunc(100*time.Millisecond, cancel)
					defer timer.Stop()
					want = "cancelled"
				}
				input, _ := json.Marshal(runCommandInput{Command: command})
				start := time.Now()
				res, err := tool.Execute(ctx, tools.ExecutionContext{WorkspaceRoot: t.TempDir()}, input)
				if err != nil {
					t.Fatal(err)
				}
				got := decodeResult(t, res.Content)
				if got.Status != want || got.ExitCode != -1 || got.Note == "" {
					t.Fatalf("outcome = %+v", got)
				}
				if time.Since(start) > 3*time.Second {
					t.Fatal("cancellation did not bound tool return")
				}
			})
		}
	}
}
