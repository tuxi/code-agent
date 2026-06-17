package sandbox

import "testing"

func TestDefaultPolicyClassify(t *testing.T) {
	p := DefaultPolicy()

	cases := []struct {
		command string
		want    Decision
	}{
		// Read-only and build commands run without confirmation.
		{"git status", Allow},
		{"git status --short", Allow},
		{"git diff HEAD~1", Allow},
		{"git log --oneline -n 5", Allow},
		{"ls -la internal", Allow},
		{"cat go.mod", Allow},
		{"grep -rn TODO .", Allow},
		{"go build ./...", Allow},
		{"go test ./internal/...", Allow},
		{"go vet ./...", Allow},
		{"git add internal/sandbox/policy.go", Allow},

		// Mutating / networked commands require confirmation.
		{"git commit -m \"wip\"", Confirm},
		{"git checkout main", Confirm}, // can discard working changes
		{"git reset --hard", Confirm},
		{"git push origin feature", Confirm},
		{"rm build/output", Confirm},
		{"mv a b", Confirm},
		{"curl https://example.com", Confirm},
		{"make all", Confirm},

		// Unrecognized commands default to confirmation, not a hard block.
		{"some-unknown-binary --flag", Confirm},

		// Catastrophic commands are blocked outright.
		{"rm -rf /", Block},
		{"rm -rf / --no-preserve-root", Block},
		{"sudo rm -rf /", Block}, // substring match still catches it
		{":(){ :|:& };:", Block},
		{"dd if=/dev/zero of=/dev/sda", Block},
		{"git push --force origin main", Block},

		// Empty command is refused.
		{"   ", Block},
	}

	for _, tc := range cases {
		got := p.Classify(tc.command).Decision
		if got != tc.want {
			t.Errorf("Classify(%q) = %v, want %v", tc.command, got, tc.want)
		}
	}
}

func TestClassifyPrefixIsWordBounded(t *testing.T) {
	p := DefaultPolicy()

	// "git status" must not match an unrelated command that merely shares a prefix.
	if d := p.Classify("git stat-something").Decision; d == Allow {
		t.Errorf("Classify(%q) unexpectedly Allow; prefix match should be word-bounded", "git stat-something")
	}
	// Exact prefix with arguments is fine.
	if d := p.Classify("git status").Decision; d != Allow {
		t.Errorf("Classify(%q) = %v, want Allow", "git status", d)
	}
}

func TestClassifyLongestPrefixWins(t *testing.T) {
	// A confirm rule that is a longer, more specific prefix than a matching allow
	// rule should win. "go build" is Allow; add a longer confirm rule for a
	// specific variant and verify it takes precedence.
	p := CommandPolicy{
		AllowedCommands: []string{"go build"},
		RequiresConfirm: []string{"go build -o"},
	}
	if d := p.Classify("go build ./cmd/...").Decision; d != Allow {
		t.Errorf("Classify(go build ./cmd/...) = %v, want Allow", d)
	}
	if d := p.Classify("go build -o bin/foo").Decision; d != Confirm {
		t.Errorf("Classify(go build -o bin/foo) = %v, want Confirm", d)
	}
}

func TestSplitArgs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"git status", []string{"git", "status"}},
		{"  go   test ./...  ", []string{"go", "test", "./..."}},
		{`git commit -m "a long message"`, []string{"git", "commit", "-m", "a long message"}},
		{`echo 'single quoted'`, []string{"echo", "single quoted"}},
		{`mixed "a b"c`, []string{"mixed", "a bc"}},
	}
	for _, tc := range cases {
		got, err := SplitArgs(tc.in)
		if err != nil {
			t.Errorf("SplitArgs(%q) error: %v", tc.in, err)
			continue
		}
		if !equalStrings(got, tc.want) {
			t.Errorf("SplitArgs(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}

	if _, err := SplitArgs(`echo "unterminated`); err == nil {
		t.Error("SplitArgs with unterminated quote: expected error, got nil")
	}
}

func TestContainsShellOperators(t *testing.T) {
	with := []string{"a | b", "a && b", "a > out.txt", "cat < in", "echo $(date)", "a; b", "a `b`"}
	for _, c := range with {
		if !ContainsShellOperators(c) {
			t.Errorf("ContainsShellOperators(%q) = false, want true", c)
		}
	}
	without := []string{"go test ./...", "git status --short", "ls -la"}
	for _, c := range without {
		if ContainsShellOperators(c) {
			t.Errorf("ContainsShellOperators(%q) = true, want false", c)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
