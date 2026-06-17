package observation

import "testing"

func TestClassifyRunCommand(t *testing.T) {
	cases := []struct {
		name string
		res  commandResult
		want FailureType
	}{
		{
			name: "success",
			res:  commandResult{Command: "go test ./...", ExitCode: 0, Decision: "allow"},
			want: FailureNone,
		},
		{
			name: "blocked",
			res:  commandResult{Command: "rm -rf /", ExitCode: -1, Decision: "block", Note: "refused by policy"},
			want: FailureBlocked,
		},
		{
			name: "timeout",
			res:  commandResult{Command: "go test ./...", ExitCode: -1, Decision: "allow", Note: "timed out after 120s"},
			want: FailureTimeout,
		},
		{
			name: "compile error",
			res: commandResult{
				Command:  "go build ./...",
				Stderr:   "# code-agent/internal/foo\ninternal/foo/service.go:42:13: undefined: Bar",
				ExitCode: 1,
				Decision: "allow",
			},
			want: FailureCompile,
		},
		{
			name: "test failure",
			res: commandResult{
				Command:  "go test ./...",
				Stdout:   "--- FAIL: TestParse (0.00s)\n    parse_test.go:12: want 3 got 4\nFAIL\tcode-agent/internal/foo\t0.20s",
				ExitCode: 1,
				Decision: "allow",
			},
			want: FailureTest,
		},
		{
			name: "test that fails to compile is a compile failure",
			res: commandResult{
				Command:  "go test ./...",
				Stdout:   "# code-agent/internal/foo [build failed]\nfoo_test.go:5:2: undefined: Bar\nFAIL\tcode-agent/internal/foo [build failed]",
				ExitCode: 2,
				Decision: "allow",
			},
			want: FailureCompile,
		},
		{
			name: "go vet finding is lint, not compile, despite the package header",
			res: commandResult{
				Command:  "go vet ./...",
				Stderr:   "# code-agent/internal/foo\n./service.go:42:2: Printf format %d has arg s of wrong type string",
				ExitCode: 1,
				Decision: "allow",
			},
			want: FailureLint,
		},
		{
			name: "vet that fails to compile is still compile",
			res: commandResult{
				Command:  "go vet ./...",
				Stderr:   "# code-agent/internal/foo\n./service.go:42:2: undefined: Bar",
				ExitCode: 1,
				Decision: "allow",
			},
			want: FailureCompile,
		},
		{
			name: "generic nonzero exit is runtime",
			res: commandResult{
				Command:  "git fetch origin",
				Stderr:   "fatal: unable to access remote",
				ExitCode: 128,
				Decision: "allow",
			},
			want: FailureRuntime,
		},
		{
			name: "panic is a test failure",
			res: commandResult{
				Command:  "go test ./...",
				Stdout:   "panic: runtime error: index out of range [3]\n\ngoroutine 1 [running]:",
				ExitCode: 2,
				Decision: "allow",
			},
			want: FailureTest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyRunCommand(tc.res); got != tc.want {
				t.Errorf("classifyRunCommand = %q, want %q", got, tc.want)
			}
		})
	}
}
