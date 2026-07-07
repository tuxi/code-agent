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
		{"git reset --hard", Block}, // dangerous: hard reset discards all working-tree changes
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

// TestBlocksInterpreterCForms covers the PRD requirement that bash -c / sh -c /
// zsh -c are refused: they would run an arbitrary nested script past
// per-command classification. A bare interactive shell still only needs
// confirmation, and a command that merely mentions "bash -c" in a quoted
// message is not blocked.
func TestBlocksInterpreterCForms(t *testing.T) {
	p := DefaultPolicy()

	blocked := []string{
		`bash -c "rm -rf /tmp/x"`,
		`sh -c "echo hi"`,
		`zsh -c "ls"`,
		`/bin/sh -c "id"`,
	}
	for _, c := range blocked {
		if d := p.Classify(c).Decision; d != Block {
			t.Errorf("Classify(%q) = %v, want Block", c, d)
		}
	}

	// A bare shell (no -c) is gated, not blocked.
	if d := p.Classify("bash").Decision; d != Confirm {
		t.Errorf("Classify(bash) = %v, want Confirm", d)
	}
	// "bash -c" inside a quoted commit message is data, not an invocation.
	if d := p.Classify(`git commit -m "document bash -c usage"`).Decision; d == Block {
		t.Error(`Classify(commit message mentioning "bash -c") = Block; quoted text must not block`)
	}
}

// TestPackageLevelClassify checks the convenience entry point delegates to the
// default policy.
func TestPackageLevelClassify(t *testing.T) {
	if d := Classify("git status").Decision; d != Allow {
		t.Errorf("Classify(git status) = %v, want Allow", d)
	}
	if d := Classify("rm -rf /").Decision; d != Block {
		t.Errorf("Classify(rm -rf /) = %v, want Block", d)
	}
	// Decision is a string-valued enum: it must equal its wire form.
	if string(Classify("git status").Decision) != "allow" {
		t.Errorf("Decision wire value = %q, want %q", Classify("git status").Decision, "allow")
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

// TestClassifyIgnoresQuotedContent guards the dogfooding bug: a commit message
// that merely mentions a blocked pattern (or contains shell-operator-looking
// text) must not be blocked — the dangerous text is data inside quotes, not a
// command. Only an actual unquoted invocation is blocked.
func TestClassifyIgnoresQuotedContent(t *testing.T) {
	p := DefaultPolicy()

	// "rm -rf /" inside a commit message: must classify as Confirm (git commit),
	// not Block.
	msg := `git commit -m "docs: explain that rm -rf / is hard-blocked"`
	if d := p.Classify(msg).Decision; d != Confirm {
		t.Errorf("Classify(commit message mentioning rm -rf /) = %v, want Confirm", d)
	}

	// A bare, unquoted rm -rf / is still blocked.
	if d := p.Classify("rm -rf /").Decision; d != Block {
		t.Errorf("Classify(rm -rf /) = %v, want Block", d)
	}

	// A double-quoted single arg containing the pattern is data, not a command.
	if d := p.Classify(`echo "rm -rf /"`).Decision; d == Block {
		t.Errorf(`Classify(echo "rm -rf /") = Block; quoted text must not block`)
	}
}

func TestContainsShellOperatorsIgnoresQuotes(t *testing.T) {
	// Operators / newlines inside quotes are data (a multi-line commit message),
	// not shell syntax — they must not trip the guard.
	quoted := []string{
		`git commit -m "a | b"`,
		`git commit -m "line one
line two"`,
		`git commit -m "uses > and && in prose"`,
		`echo "$(date)"`,
	}
	for _, c := range quoted {
		if ContainsShellOperators(c) {
			t.Errorf("ContainsShellOperators(%q) = true, want false (operators are inside quotes)", c)
		}
	}

	// Real, unquoted operators are still detected.
	if !ContainsShellOperators("cat a.txt | grep x") {
		t.Error("ContainsShellOperators(cat a.txt | grep x) = false, want true")
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
