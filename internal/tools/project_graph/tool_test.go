package projectgraph

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fakeAdapter is an in-memory LanguageAdapter for testing the tool's dispatch,
// merging, and rename logic without depending on any installed toolchain.
type fakeAdapter struct {
	lang    string
	avail   bool
	symbols []Symbol
	refs    []Reference
}

func (f fakeAdapter) Language() string { return f.lang }
func (f fakeAdapter) Available() bool  { return f.avail }

func (f fakeAdapter) FindSymbol(_ context.Context, _, query string) ([]Symbol, error) {
	var out []Symbol
	for _, s := range f.symbols {
		if s.Name == query {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f fakeAdapter) FindReferences(_ context.Context, _, symbol string) ([]Reference, error) {
	return f.refs, nil
}

func toolWith(a LanguageAdapter) *ProjectGraphTool {
	return &ProjectGraphTool{WorkspaceRoot: ".", Adapters: []LanguageAdapter{a}, Timeout: time.Second}
}

func run(t *testing.T, tool *ProjectGraphTool, input string) string {
	t.Helper()
	res, err := tool.Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("Execute(%s): %v", input, err)
	}
	return res.Content
}

func TestFindSymbol(t *testing.T) {
	tool := toolWith(fakeAdapter{
		lang:  "go",
		avail: true,
		symbols: []Symbol{
			{Name: "SourceOutput", Kind: "struct", File: "a.go", Line: 32},
			{Name: "Other", Kind: "struct", File: "b.go", Line: 1},
		},
	})
	content := run(t, tool, `{"action":"find_symbol","query":"SourceOutput"}`)

	var syms []Symbol
	if err := json.Unmarshal([]byte(content), &syms); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, content)
	}
	if len(syms) != 1 || syms[0].Name != "SourceOutput" || syms[0].Line != 32 {
		t.Errorf("find_symbol = %+v, want one SourceOutput at line 32", syms)
	}
}

func TestFindReferences(t *testing.T) {
	tool := toolWith(fakeAdapter{
		lang:  "go",
		avail: true,
		refs: []Reference{
			{File: "a.go", Line: 210, Context: "s := SourceOutput{}"},
		},
	})
	content := run(t, tool, `{"action":"find_references","symbol":"SourceOutput"}`)

	var refs []Reference
	if err := json.Unmarshal([]byte(content), &refs); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, content)
	}
	if len(refs) != 1 || refs[0].Line != 210 {
		t.Errorf("find_references = %+v, want one ref at line 210", refs)
	}
}

func TestRenameCheckSafe(t *testing.T) {
	// from has references across two files; to does not yet exist => safe.
	tool := toolWith(fakeAdapter{
		lang:    "go",
		avail:   true,
		symbols: []Symbol{{Name: "SourceOutput", Kind: "struct", File: "a.go", Line: 32}},
		refs: []Reference{
			{File: "a.go", Line: 32},
			{File: "a.go", Line: 210},
			{File: "b.go", Line: 5},
		},
	})
	content := run(t, tool, `{"action":"rename_check","from":"SourceOutput","to":"FrameSource"}`)

	var rc RenameCheck
	if err := json.Unmarshal([]byte(content), &rc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, content)
	}
	if rc.AffectedFiles != 2 {
		t.Errorf("affected_files = %d, want 2 (distinct files)", rc.AffectedFiles)
	}
	if rc.References != 3 {
		t.Errorf("references = %d, want 3", rc.References)
	}
	if !rc.Safe || len(rc.Warnings) != 0 {
		t.Errorf("expected safe with no warnings, got safe=%v warnings=%v", rc.Safe, rc.Warnings)
	}
}

func TestRenameCheckCollision(t *testing.T) {
	// to ("FrameSource") already exists => collision warning, not safe.
	tool := toolWith(fakeAdapter{
		lang:  "go",
		avail: true,
		symbols: []Symbol{
			{Name: "SourceOutput", Kind: "struct", File: "a.go", Line: 32},
			{Name: "FrameSource", Kind: "struct", File: "c.go", Line: 8},
		},
		refs: []Reference{{File: "a.go", Line: 32}},
	})
	content := run(t, tool, `{"action":"rename_check","from":"SourceOutput","to":"FrameSource"}`)

	var rc RenameCheck
	if err := json.Unmarshal([]byte(content), &rc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, content)
	}
	if rc.Safe {
		t.Error("expected unsafe due to name collision, got safe=true")
	}
	if len(rc.Warnings) == 0 {
		t.Error("expected a collision warning")
	}
}

func TestRenameCheckNoReferences(t *testing.T) {
	tool := toolWith(fakeAdapter{lang: "go", avail: true})
	content := run(t, tool, `{"action":"rename_check","from":"Ghost","to":"Phantom"}`)

	var rc RenameCheck
	if err := json.Unmarshal([]byte(content), &rc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, content)
	}
	if rc.Safe {
		t.Error("expected unsafe when no references resolve, got safe=true")
	}
}

func TestNoAdapterAvailable(t *testing.T) {
	tool := toolWith(fakeAdapter{lang: "go", avail: false})
	content := run(t, tool, `{"action":"find_symbol","query":"X"}`)
	if !strings.Contains(content, "no language backend") {
		t.Errorf("expected a helpful 'no backend available' message, got: %s", content)
	}
}

func TestUnsupportedLanguage(t *testing.T) {
	tool := toolWith(fakeAdapter{lang: "go", avail: true})
	content := run(t, tool, `{"action":"find_symbol","query":"X","language":"cobol"}`)
	if !strings.Contains(content, "unsupported language") {
		t.Errorf("expected 'unsupported language' message, got: %s", content)
	}
}

func TestUnknownAction(t *testing.T) {
	tool := toolWith(fakeAdapter{lang: "go", avail: true})
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"frobnicate"}`))
	if err == nil {
		t.Error("expected an error for an unknown action")
	}
}
