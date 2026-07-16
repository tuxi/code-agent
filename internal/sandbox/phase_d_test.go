package sandbox

import (
	"testing"
)

func TestASTClassification(t *testing.T) {
	policy := DefaultPolicy()

	tests := []struct {
		command string
		want    Decision
		desc    string
	}{
		// Simple commands unchanged
		{"go build ./...", Allow, "simple go build"},
		{"git status", Allow, "git status"},
		{"rm build/output", Confirm, "rm file"},

		// Chain via AST
		{"go build && go test", Allow, "AST chain all-allow"},
		{"go build && git push", Confirm, "AST chain with push"},
		{"go build && rm -rf /", Block, "AST chain with catastrophic"},

		// Pipeline via AST
		{"go test | grep FAIL", Allow, "AST pipe all-allow"},

		// Too complex → Confirm
		{"if true; then echo yes; fi", Confirm, "if statement"},
		{"for f in *.go; do echo $f; done", Confirm, "for loop"},
		{"(cd /tmp && ls)", Confirm, "subshell"},

		// Wrapper + AST
		{"timeout 30s go build && go test", Allow, "wrapped AST chain"},
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

func TestASTClassificationNotFalsePositive(t *testing.T) {
	policy := DefaultPolicy()

	normal := []string{
		"go build ./... && go test ./...",
		"go vet ./...; go build ./...",
		"go test ./... | grep -E 'FAIL|PASS'",
		"timeout 30s go test ./...",
		"sudo go build ./...",
		"GOOS=linux go build ./...",
		"go build && go test | grep FAIL && echo done",
	}
	for _, cmd := range normal {
		t.Run(cmd, func(t *testing.T) {
			got := policy.Classify(cmd)
			if got.Decision == Block {
				t.Errorf("%q should not be blocked, got: %s", cmd, got.Reason)
			}
		})
	}
}
