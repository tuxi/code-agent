package projectgraph

import "testing"

func TestParseWorkspaceSymbols(t *testing.T) {
	out := `/abs/root/internal/foo/bar.go:32:6-32:18 SourceOutput Struct
/abs/root/internal/foo/bar.go:88:6-88:20 SourceOutputView Struct
/abs/root/cmd/main.go:10:6-10:14 newSource Function
`
	syms := parseWorkspaceSymbols(out, "/abs/root")
	if len(syms) != 3 {
		t.Fatalf("got %d symbols, want 3: %+v", len(syms), syms)
	}

	want := Symbol{Name: "SourceOutput", Kind: "struct", File: "internal/foo/bar.go", Line: 32}
	if syms[0] != want {
		t.Errorf("syms[0] = %+v, want %+v", syms[0], want)
	}
	if syms[2].Kind != "function" || syms[2].Line != 10 {
		t.Errorf("syms[2] = %+v, want kind=function line=10", syms[2])
	}
}

func TestParseReferenceLocations(t *testing.T) {
	out := `/abs/root/internal/foo/bar.go:32:6-32:18
/abs/root/internal/foo/use.go:210:14-210:26
/abs/root/cmd/main.go:5:2
`
	locs := parseReferenceLocations(out, "/abs/root")
	if len(locs) != 3 {
		t.Fatalf("got %d locations, want 3: %+v", len(locs), locs)
	}
	if locs[1].file != "internal/foo/use.go" || locs[1].line != 210 {
		t.Errorf("locs[1] = %+v, want file=internal/foo/use.go line=210", locs[1])
	}
	if locs[2].line != 5 {
		t.Errorf("locs[2].line = %d, want 5", locs[2].line)
	}
}

func TestFirstDefinitionPosition(t *testing.T) {
	out := `/abs/root/a.go:32:6-32:18 SourceOutputView Struct
/abs/root/b.go:88:6-88:20 SourceOutput Struct
/abs/root/c.go:10:6-10:14 SourceOutput Method
`
	// Must match the *exact* name, skipping SourceOutputView, and strip the range.
	got := firstDefinitionPosition(out, "SourceOutput")
	if got != "/abs/root/b.go:88:6" {
		t.Errorf("firstDefinitionPosition = %q, want %q", got, "/abs/root/b.go:88:6")
	}

	if got := firstDefinitionPosition(out, "Missing"); got != "" {
		t.Errorf("firstDefinitionPosition(Missing) = %q, want empty", got)
	}
}

func TestParseLocation(t *testing.T) {
	cases := []struct {
		in       string
		wantFile string
		wantLine int
		wantCol  int
	}{
		{"path/file.go:32:6-32:18", "path/file.go", 32, 6},
		{"path/file.go:5:2", "path/file.go", 5, 2},
		{"path/file.go:7", "path/file.go", 7, 0},
		{"noposition", "noposition", 0, 0},
	}
	for _, tc := range cases {
		loc := parseLocation(tc.in)
		if loc.file != tc.wantFile || loc.line != tc.wantLine || loc.col != tc.wantCol {
			t.Errorf("parseLocation(%q) = %+v, want file=%q line=%d col=%d",
				tc.in, loc, tc.wantFile, tc.wantLine, tc.wantCol)
		}
	}
}
