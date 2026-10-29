//go:build unix

package processexec

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The child inherits both output descriptors and keeps doing observable work.
// Killing only the parent reproduces the unbounded Cmd.Wait pipe drain.
func TestProcessHelper(t *testing.T) {
	if len(os.Args) < 4 || os.Args[len(os.Args)-3] != "--" {
		return
	}
	mode, path := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	if mode == "child" {
		for {
			f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				os.Exit(2)
			}
			_, _ = f.WriteString("x")
			_ = f.Close()
			_, _ = os.Stdout.WriteString("child output\n")
			_, _ = os.Stderr.WriteString("child error\n")
			time.Sleep(10 * time.Millisecond)
		}
	}
	child := exec.Command(os.Args[0], "-test.run=^TestProcessHelper$", "--", "child", path)
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if child.Start() != nil {
		os.Exit(2)
	}
	if mode == "exit" {
		os.Exit(0)
	}
	for {
		time.Sleep(time.Second)
	}
}

func TestOwnedTreeStopsAndPipeWaitIsBounded(t *testing.T) {
	for _, mode := range []string{"timeout", "cancel", "exit"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			if mode == "timeout" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 400*time.Millisecond)
			}
			defer cancel()
			path := filepath.Join(t.TempDir(), "heartbeat")
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessHelper$", "--", mode, path)
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			cleanup := Configure(cmd)
			defer cleanup()
			started := time.Now()
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			ready := time.Now().Add(2 * time.Second)
			for {
				if info, err := os.Stat(path); err == nil && info.Size() > 0 {
					break
				}
				if time.Now().After(ready) {
					t.Fatal("child never started")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if mode == "cancel" {
				cancel()
			}
			err := cmd.Wait()
			cleanup()
			if err == nil {
				t.Fatal("incomplete/cancelled process reported success")
			}
			if mode == "exit" && !errors.Is(err, exec.ErrWaitDelay) {
				t.Fatalf("exit error = %v", err)
			}
			if time.Since(started) > 3*time.Second {
				t.Fatal("process/pipe wait exceeded bound")
			}
			before, _ := os.ReadFile(path)
			time.Sleep(100 * time.Millisecond)
			after, _ := os.ReadFile(path)
			if len(before) != len(after) {
				t.Fatal("descendant survived tree cleanup")
			}
			if output.Len() == 0 {
				t.Fatal("missing real child output")
			}
		})
	}
}
