package shell

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"code-agent/internal/tools"
)

func TestBuildExitStatusForegroundAndBackground(t *testing.T) {
	for _, background := range []bool{false, true} {
		for _, tc := range []struct {
			name, command string
			want          int
		}{
			{"direct", "sh build.sh", 65},
			{"redirected", "sh build.sh > build.log 2>&1", 65},
			{"saved status", `sh build.sh > build.log 2>&1; build_rc=$?; echo "build_exit=$build_rc"; exit "$build_rc"`, 65},
			// Keep normal shell semantics: the runtime must not silently rewrite
			// a caller's command or guess which subcommand determines its result.
			{"caller masked status", `sh build.sh; echo "exit=$?"`, 0},
		} {
			t.Run(fmt.Sprintf("%s/background=%v", tc.name, background), func(t *testing.T) {
				root := t.TempDir()
				if err := os.WriteFile(filepath.Join(root, "build.sh"), []byte("#!/bin/sh\nprintf 'build failed\\n'\nexit 65\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				tool := NewRunCommandTool()
				input, _ := json.Marshal(runCommandInput{Command: tc.command, Background: background})
				result, err := tool.Execute(ctx, tools.ExecutionContext{WorkspaceRoot: root}, input)
				if err != nil {
					t.Fatal(err)
				}
				if background {
					var started backgroundResult
					if err := json.Unmarshal([]byte(result.Content), &started); err != nil || started.JobID == "" {
						t.Fatalf("background admission=%s %v", result.Content, err)
					}
					defer tool.Jobs.Cancel(started.JobID)
					waitInput, _ := json.Marshal(jobWaitInput{JobID: started.JobID, TimeoutSeconds: 3})
					result, err = (&JobWaitTool{Jobs: tool.Jobs}).Execute(ctx, tools.ExecutionContext{}, waitInput)
					if err != nil {
						t.Fatal(err)
					}
				}
				var out struct {
					ExitCode *int `json:"exit_code"`
				}
				if err := json.Unmarshal([]byte(result.Content), &out); err != nil || out.ExitCode == nil || *out.ExitCode != tc.want {
					t.Fatalf("exit status lost: %s %v", result.Content, err)
				}
				if (result.Failure != nil) != (tc.want != 0) {
					t.Fatalf("failure disagrees with exit code: %+v", result)
				}
			})
		}
	}
}
