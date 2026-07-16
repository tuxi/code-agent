package sandbox

import (
	"reflect"
	"testing"
)

func TestSplitByOperators(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		wantSubs []string
		wantOps  []string
	}{
		{
			name:     "simple chaining with &&",
			command:  "go build ./... && go test ./...",
			wantSubs: []string{"go build ./...", "go test ./..."},
			wantOps:  []string{"&&"},
		},
		{
			name:     "sequential with ;",
			command:  "git add .; git commit -m wip",
			wantSubs: []string{"git add .", "git commit -m wip"},
			wantOps:  []string{";"},
		},
		{
			name:     "triple chain",
			command:  "go build && go test && go vet",
			wantSubs: []string{"go build", "go test", "go vet"},
			wantOps:  []string{"&&", "&&"},
		},
		{
			name:     "mixed operators",
			command:  "go build ./...; go test ./...",
			wantSubs: []string{"go build ./...", "go test ./..."},
			wantOps:  []string{";"},
		},
		{
			name:     "single command no operators",
			command:  "go build ./...",
			wantSubs: []string{"go build ./..."},
			wantOps:  nil,
		},
		{
			name:     "&& inside quotes not split",
			command:  `echo "hello && world"`,
			wantSubs: []string{`echo "hello && world"`},
			wantOps:  nil,
		},
		{
			name:     "; inside single quotes not split",
			command:  `echo 'hello; world'`,
			wantSubs: []string{`echo 'hello; world'`},
			wantOps:  nil,
		},
		{
			name:     "trailing operator ignored",
			command:  "go build && ",
			wantSubs: []string{"go build"},
			wantOps:  []string{"&&"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subs, ops := splitByOperators(tt.command)
			if !reflect.DeepEqual(subs, tt.wantSubs) {
				t.Errorf("subcommands = %q, want %q", subs, tt.wantSubs)
			}
			if !reflect.DeepEqual(ops, tt.wantOps) {
				t.Errorf("operators = %q, want %q", ops, tt.wantOps)
			}
		})
	}
}

func TestClassifyChain(t *testing.T) {
	policy := DefaultPolicy()

	tests := []struct {
		command string
		want    Decision
		desc    string
	}{
		// All Allow subcommands → Allow
		{"go build ./... && go test ./...", Allow, "all-allow chain"},
		{"go build ./...; go vet ./...", Allow, "all-allow sequential"},
		// Any Confirm → Confirm
		{"go build ./... && git push origin main", Confirm, "chain with push"},
		{"git add .; git commit -m wip", Confirm, "sequential with commit"},
		// Any Block → Block
		{"go build ./... && rm -rf /", Block, "chain with catastrophic rm"},
		{"git add . && git push --force origin main", Block, "chain with force push"},
		// Mixed: Allow + Confirm → Confirm
		{"ls -la && rm build/output", Confirm, "allow + confirm = confirm"},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			got := policy.Classify(tt.command)
			if got.Decision != tt.want {
				t.Errorf("Classify(%q).Decision = %v, want %v (reason: %s)", tt.command, got.Decision, tt.want, got.Reason)
			}
		})
	}
}

func TestClassifyChainNotFalsePositive(t *testing.T) {
	policy := DefaultPolicy()

	// These must NOT be blocked — they're normal compound commands.
	normal := []string{
		"go build ./... && go test ./...",
		"go vet ./...; go build ./...",
		"cargo build && cargo test",
		"git add . && git commit -m 'my changes'",
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
