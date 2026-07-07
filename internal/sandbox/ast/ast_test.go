package ast

import (
	"testing"
)

func TestParseSimpleCommand(t *testing.T) {
	prog := Parse("go build ./...")
	if len(prog.Statements) != 1 {
		t.Fatalf("expected 1 statement, got %d", len(prog.Statements))
	}
	st := prog.Statements[0]
	if st.TooComplex {
		t.Fatal("simple command should not be too complex")
	}
	if st.Cmd.Program != "go" {
		t.Errorf("program = %q, want go", st.Cmd.Program)
	}
	if len(st.Cmd.Args) != 3 {
		t.Errorf("args = %v, want 3 elements", st.Cmd.Args)
	}
}

func TestParseChain(t *testing.T) {
	prog := Parse("go build && go test")
	if len(prog.Statements) != 2 {
		t.Fatalf("expected 2 statements, got %d", len(prog.Statements))
	}
	if prog.Statements[0].Cmd.Program != "go" {
		t.Errorf("stmt[0] program = %q", prog.Statements[0].Cmd.Program)
	}
	if prog.Statements[1].Cmd.Program != "go" {
		t.Errorf("stmt[1] program = %q", prog.Statements[1].Cmd.Program)
	}
	if prog.Statements[0].Op != "&&" {
		t.Errorf("stmt[0] op = %q, want &&", prog.Statements[0].Op)
	}
}

func TestParseSequential(t *testing.T) {
	prog := Parse("git add .; git commit -m wip")
	if len(prog.Statements) != 2 {
		t.Fatalf("expected 2 statements, got %d", len(prog.Statements))
	}
	if prog.Statements[1].Cmd.Args[2] != "-m" {
		t.Errorf("stmt[1] args = %v", prog.Statements[1].Cmd.Args)
	}
}

func TestParsePipeline(t *testing.T) {
	prog := Parse("go test | grep FAIL")
	if len(prog.Statements) != 1 {
		t.Fatalf("pipeline: expected 1 statement, got %d", len(prog.Statements))
	}
	if prog.Statements[0].Cmd.Program != "go" {
		t.Errorf("pipeline program = %q, want go", prog.Statements[0].Cmd.Program)
	}
}

func TestParseRedirect(t *testing.T) {
	prog := Parse("go test > /tmp/out")
	st := prog.Statements[0]
	if len(st.Cmd.Redirects) != 1 {
		t.Fatalf("expected 1 redirect, got %d", len(st.Cmd.Redirects))
	}
	if st.Cmd.Redirects[0].Op != ">" || st.Cmd.Redirects[0].Target != "/tmp/out" {
		t.Errorf("redirect = %v", st.Cmd.Redirects[0])
	}
}

func TestParseAssignments(t *testing.T) {
	prog := Parse("GOOS=linux go build")
	st := prog.Statements[0]
	if len(st.Cmd.Assignments) != 1 || st.Cmd.Assignments[0] != "GOOS=linux" {
		t.Errorf("assignments = %v", st.Cmd.Assignments)
	}
	if st.Cmd.Program != "go" {
		t.Errorf("program after assignment = %q, want go", st.Cmd.Program)
	}
}

func TestParseTooComplex(t *testing.T) {
	tests := []string{
		"if true; then echo yes; fi",
		"for f in *.go; do echo $f; done",
		"while true; do echo loop; done",
		"case $x in a) echo a;; esac",
		"function foo() { echo bar; }",
		"(cd /tmp && ls)",
	}
	for _, cmd := range tests {
		t.Run(cmd, func(t *testing.T) {
			prog := Parse(cmd)
			if len(prog.Statements) != 1 {
				t.Fatalf("too complex: expected 1 statement, got %d", len(prog.Statements))
			}
			if !prog.Statements[0].TooComplex {
				t.Errorf("%q should be marked TooComplex", cmd)
			}
		})
	}
}

func TestParseCompound(t *testing.T) {
	// Complex real-world command
	prog := Parse("timeout 30s go test ./... | grep FAIL && echo failed || echo passed")
	if len(prog.Statements) < 2 {
		t.Fatalf("compound: expected multiple statements, got %d", len(prog.Statements))
	}
}

func TestParseEmpty(t *testing.T) {
	prog := Parse("")
	if len(prog.Statements) != 0 {
		t.Errorf("empty command should have 0 statements")
	}
}

func TestParseSubshell(t *testing.T) {
	prog := Parse("(cd /tmp && ls)")
	if !prog.Statements[0].TooComplex {
		t.Error("subshell should be TooComplex")
	}
}
