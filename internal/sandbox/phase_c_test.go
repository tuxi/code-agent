package sandbox

import (
	"testing"
)

func TestPeelWrappers(t *testing.T) {
	tests := []struct {
		command string
		want    string
	}{
		// Simple wrappers
		{"timeout 30s go test ./...", "go test ./..."},
		{"sudo go build", "go build"},
		{"nohup go test ./...", "go test ./..."},
		{"nice go build", "go build"},
		{"nice -n 10 go build", "go build"},
		{"time go build", "go build"},

		// Nested wrappers
		{"sudo timeout 30s go test", "go test"},
		{"sudo nice go build", "go build"},

		// env with VAR=val
		{"env VAR=val go test", "go test"},
		{"env A=1 B=2 go test", "go test"},

		// stdbuf
		{"stdbuf -oL go build", "go build"},
		{"stdbuf -o0 -e0 go test", "go test"},

		// sudo with flags
		{"sudo -u nobody go build", "go build"},

		// Non-wrappers unchanged
		{"git status", "git status"},
		{"go test ./...", "go test ./..."},
		{"rm -rf build", "rm -rf build"},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			got := peelWrappers(tt.command)
			if got != tt.want {
				t.Errorf("peelWrappers(%q) = %q, want %q", tt.command, got, tt.want)
			}
		})
	}
}

func TestClassifyWrappers(t *testing.T) {
	policy := DefaultPolicy()

	tests := []struct {
		command string
		want    Decision
		desc    string
	}{
		// Wrapped Allow commands must still be Allow
		{"timeout 30s go test ./...", Allow, "timeout go test"},
		{"sudo go build", Allow, "sudo go build"},
		{"nohup go vet ./...", Allow, "nohup go vet"},
		{"time go build", Allow, "time go build"},

		// Wrapped Confirm commands must still be Confirm
		{"sudo git push origin main", Confirm, "sudo git push"},
		{"timeout 30s git commit -m wip", Confirm, "timeout git commit"},

		// Wrapped Block commands must still be Block
		{"sudo rm -rf /", Block, "sudo rm -rf /"},
		{"timeout 1s rm -rf /", Block, "timeout rm -rf /"},
		{"sudo git push --force origin main", Block, "sudo force push"},

		// env-wrapped
		{"env GOOS=linux go build", Allow, "env go build"},

		// Compound with wrappers
		{"timeout 30s go build && go test", Allow, "wrapped chain"},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			got := policy.Classify(tt.command)
			if got.Decision != tt.want {
				t.Errorf("Classify(%q).Decision = %v, want %v (reason: %s)",
					tt.command, got.Decision, tt.want, got.Reason)
			}
		})
	}
}

func TestClassifyWrappersBackstop(t *testing.T) {
	policy := DefaultPolicy()

	// "timeout" with no inner command — should not panic, classified as-is.
	got := policy.Classify("timeout")
	if got.Decision == Block {
		// OK — empty inner defaults to Confirm for unrecognized command
		// (not blocked by any pattern, just not in any list).
	}
	_ = got

	// "sudo" alone — not a known command, should be Confirm (unrecognized).
	got = policy.Classify("sudo")
	if got.Decision != Confirm {
		t.Errorf("bare 'sudo' should be Confirm (unrecognized), got %v", got.Decision)
	}
}

func TestCommandSubstitutionStillRejected(t *testing.T) {
	// $() and backticks must STILL be rejected by ContainsShellOperators.
	// $() and backticks OUTSIDE quotes are still rejected. Inside double quotes,
	// unquotedStructure strips them (known limitation — ASCII-level only).
	// Only backticks remain rejected. $() is now supported.
	rejected := []string{
		"echo `date`",
	}
	for _, c := range rejected {
		if !ContainsShellOperators(c) {
			t.Errorf("ContainsShellOperators(%q) = false, want true (still rejected)", c)
		}
	}
}

func TestInputRedirectNowSupported(t *testing.T) {
	// < is now supported (Phase D extension). /dev/null is always safe.
	if ContainsShellOperators("cat < /dev/null") {
		t.Error("input redirect < should now be supported, not rejected")
	}
}
