package projectgraph

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeSwiftKind(t *testing.T) {
	cases := []struct {
		uti  string
		want string
	}{
		{"source.lang.swift.decl.struct", "struct"},
		{"source.lang.swift.decl.class", "class"},
		{"source.lang.swift.decl.enum", "enum"},
		{"source.lang.swift.decl.protocol", "protocol"},
		{"source.lang.swift.decl.extension", "extension"},
		{"source.lang.swift.decl.function.method.instance", "method"},
		{"source.lang.swift.decl.function.method.static", "static method"},
		{"source.lang.swift.decl.function.method.class", "class method"},
		{"source.lang.swift.decl.function.free", "function"},
		{"source.lang.swift.decl.function.constructor", "constructor"},
		{"source.lang.swift.decl.function.destructor", "destructor"},
		{"source.lang.swift.decl.var.instance", "instance variable"},
		{"source.lang.swift.decl.var.static", "static variable"},
		{"source.lang.swift.decl.var.global", "global variable"},
		{"source.lang.swift.decl.var.local", "local variable"},
		{"source.lang.swift.decl.var.parameter", "parameter"},
		{"source.lang.swift.decl.typealias", "typealias"},
		{"source.lang.swift.decl.associatedtype", "associated type"},
		{"source.lang.swift.decl.enumcase", "enum case"},
		{"source.lang.swift.decl.enumelement", "enum element"},
		{"source.lang.swift.decl.generic_type_param", "generic type param"},
		{"source.lang.swift.decl.subscript", "subscript"},
		// Unknown UTI: fallback strips prefix and lowercases.
		{"source.lang.swift.decl.unknown.thing", "unknown thing"},
		{"source.lang.swift.syntaxtype.comment", "comment"},
		{"completely.random.string", "completely.random.string"},
	}
	for _, tc := range cases {
		got := normalizeSwiftKind(tc.uti)
		if got != tc.want {
			t.Errorf("normalizeSwiftKind(%q) = %q, want %q", tc.uti, got, tc.want)
		}
	}
}

func TestOffsetToLine(t *testing.T) {
	// Build a temp file with known content and verify offset→line mapping.
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.swift")
	content := "line1\nline2\nline3\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	lines := buildLineTable(path)
	// "line1\nline2\nline3\n" → 4 table entries (trailing \n starts an empty 4th line)
	if len(lines) != 4 {
		t.Fatalf("expected 4 line-table entries, got %d (table=%v)", len(lines), lines)
	}

	// line1 starts at offset 0
	if l := offsetToLine(lines, 0); l != 1 {
		t.Errorf("offset 0 → line %d, want 1", l)
	}
	// line2 starts at offset 6 ("line1\n" = 6 bytes)
	if l := offsetToLine(lines, 6); l != 2 {
		t.Errorf("offset 6 → line %d, want 2", l)
	}
	// 'n' in line2 at offset 8
	if l := offsetToLine(lines, 8); l != 2 {
		t.Errorf("offset 8 → line %d, want 2", l)
	}
	// line3 starts at offset 12 ("line1\nline2\n" = 12 bytes)
	if l := offsetToLine(lines, 12); l != 3 {
		t.Errorf("offset 12 → line %d, want 3", l)
	}
	// out of range → clamps to last line
	if l := offsetToLine(lines, 999); l != 4 {
		t.Errorf("offset 999 → line %d, want 4 (last line, trailing)", l)
	}
	if l := offsetToLine(nil, 0); l != 0 {
		t.Errorf("nil table → line %d, want 0", l)
	}
}

func TestCollectSymbols(t *testing.T) {
	// Simulated sourcekitten structure output for:
	//   struct VideoFrameProvider {
	//       func renderFrame() {}
	//       var isReady: Bool
	//   }
	//   class FrameManager {
	//       func renderFrame() {}   // same method name, different type
	//   }
	jsonOut := `{
  "key.substructure": [
    {
      "key.kind": "source.lang.swift.decl.struct",
      "key.name": "VideoFrameProvider",
      "key.offset": 0,
      "key.nameoffset": 7,
      "key.namelength": 18,
      "key.substructure": [
        {
          "key.kind": "source.lang.swift.decl.function.method.instance",
          "key.name": "renderFrame()",
          "key.offset": 27,
          "key.nameoffset": 33,
          "key.namelength": 13
        },
        {
          "key.kind": "source.lang.swift.decl.var.instance",
          "key.name": "isReady",
          "key.offset": 54,
          "key.nameoffset": 58,
          "key.namelength": 7
        }
      ]
    },
    {
      "key.kind": "source.lang.swift.decl.class",
      "key.name": "FrameManager",
      "key.offset": 72,
      "key.nameoffset": 78,
      "key.namelength": 12,
      "key.substructure": [
        {
          "key.kind": "source.lang.swift.decl.function.method.instance",
          "key.name": "renderFrame()",
          "key.offset": 98,
          "key.nameoffset": 104,
          "key.namelength": 13
        }
      ]
    }
  ]
}`

	// Write a temp file with content that matches the offsets above.
	tmp := t.TempDir()
	absPath := filepath.Join(tmp, "VideoFrameProvider.swift")
	// Build content to match offsets:
	// offset 0: "struct"  (keyword before name)
	// offset 7: "VideoFrameProvider"  (nameoffset)
	// offset 27: "    func renderFrame() {}"
	// offset 33: "renderFrame()"  (nameoffset of method)
	// etc.
	content := "struct VideoFrameProvider {\n    func renderFrame() {}\n    var isReady: Bool\n}\n\nclass FrameManager {\n    func renderFrame() {}\n}\n"
	if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	syms, err := collectSymbols(jsonOut, "VideoFrameProvider", absPath, tmp)
	if err != nil {
		t.Fatalf("collectSymbols: %v", err)
	}
	if len(syms) != 1 {
		t.Fatalf("got %d symbols, want 1: %+v", len(syms), syms)
	}
	s := syms[0]
	if s.Name != "VideoFrameProvider" {
		t.Errorf("Name = %q, want VideoFrameProvider", s.Name)
	}
	if s.Kind != "struct" {
		t.Errorf("Kind = %q, want struct", s.Kind)
	}
	if s.Line != 1 {
		t.Errorf("Line = %d, want 1 (name at offset 7 on first line)", s.Line)
	}
	rel, _ := filepath.Rel(tmp, absPath)
	if s.File != filepath.ToSlash(rel) {
		t.Errorf("File = %q, want %q", s.File, filepath.ToSlash(rel))
	}

	// Query for "renderFrame()" — should find both (struct method and class method).
	syms2, err := collectSymbols(jsonOut, "renderFrame()", absPath, tmp)
	if err != nil {
		t.Fatalf("collectSymbols renderFrame: %v", err)
	}
	if len(syms2) != 2 {
		t.Fatalf("got %d symbols for renderFrame(), want 2: %+v", len(syms2), syms2)
	}
	if syms2[0].Kind != "method" || syms2[1].Kind != "method" {
		t.Errorf("both renderFrame symbols should be 'method', got %q and %q", syms2[0].Kind, syms2[1].Kind)
	}
	// Should be on lines 2 and 6 respectively (1-based, renderFrame() calls).
	if syms2[0].Line != 2 {
		t.Errorf("first renderFrame line = %d, want 2", syms2[0].Line)
	}

	// Case-insensitive match.
	syms3, err := collectSymbols(jsonOut, "videoframeprovider", absPath, tmp)
	if err != nil {
		t.Fatalf("collectSymbols case-insensitive: %v", err)
	}
	if len(syms3) != 1 || syms3[0].Name != "VideoFrameProvider" {
		t.Errorf("case-insensitive lookup failed: %+v", syms3)
	}
}

func TestCollectSymbolsEmpty(t *testing.T) {
	jsonOut := `{"key.substructure":[]}`
	tmp := t.TempDir()
	absPath := filepath.Join(tmp, "empty.swift")
	os.WriteFile(absPath, []byte("\n"), 0644)

	syms, err := collectSymbols(jsonOut, "Anything", absPath, tmp)
	if err != nil {
		t.Fatalf("collectSymbols empty: %v", err)
	}
	if len(syms) != 0 {
		t.Errorf("expected 0 symbols, got %d", len(syms))
	}
}

func TestCollectSymbolsMalformedJSON(t *testing.T) {
	tmp := t.TempDir()
	absPath := filepath.Join(tmp, "bad.swift")
	os.WriteFile(absPath, []byte("struct"), 0644)

	_, err := collectSymbols(`{garbage}`, "X", absPath, tmp)
	if err == nil {
		t.Error("expected error for malformed JSON")
	}
}

func TestSwiftAdapterAvailable(t *testing.T) {
	a := NewSwiftAdapter()
	if a.Language() != "swift" {
		t.Errorf("Language = %q, want swift", a.Language())
	}
	// Available() just checks PATH; we can't assert true/false in CI, but it
	// must not panic.
	_ = a.Available()
}

func TestFindReferencesEmptySymbol(t *testing.T) {
	// Empty symbol is rejected before the SDK check, so no xcrun needed.
	a := NewSwiftAdapter()
	_, err := a.FindReferences(nil, "", "")
	if err == nil {
		t.Error("expected error for empty symbol")
	}
}

func TestResolveSDK(t *testing.T) {
	a := &swiftAdapter{}
	sdk := a.resolveSDK()
	// On macOS with Xcode CLT, this should be non-empty.
	if sdk == "" {
		t.Log("SDK not found — xcrun may not be installed (expected on CI without Xcode)")
	} else {
		t.Logf("SDK: %s", sdk)
		// Verify caching: second call should return the same value.
		if a.resolveSDK() != sdk {
			t.Error("resolveSDK should cache result")
		}
	}
}

// ---------------------------------------------------------------------------
// Index-based reference resolution tests
// ---------------------------------------------------------------------------

func TestCollectMatchingUSRs(t *testing.T) {
	// Simulated sourcekitten index output for:
	//   struct VideoFrameProvider {
	//       func renderFrame() { process() }  // process is a ref
	//       var isReady: Bool
	//   }
	jsonOut := `{
  "key.entities": [
    {
      "key.kind": "source.lang.swift.decl.struct",
      "key.name": "VideoFrameProvider",
      "key.usr": "s:4main18VideoFrameProviderV",
      "key.line": 1,
      "key.entities": [
        {
          "key.kind": "source.lang.swift.decl.function.method.instance",
          "key.name": "renderFrame()",
          "key.usr": "s:4main18VideoFrameProviderV11renderFrameyyF",
          "key.line": 2,
          "key.entities": [
            {
              "key.kind": "source.lang.swift.ref.function.free",
              "key.name": "process()",
              "key.usr": "s:4main7processyyF",
              "key.line": 2
            }
          ]
        },
        {
          "key.kind": "source.lang.swift.decl.var.instance",
          "key.name": "isReady",
          "key.usr": "s:4main18VideoFrameProviderV7isReadySbvp",
          "key.line": 3
        }
      ]
    }
  ]
}`

	usrs, err := collectMatchingUSRs(jsonOut, "VideoFrameProvider")
	if err != nil {
		t.Fatalf("collectMatchingUSRs: %v", err)
	}
	if len(usrs) != 1 {
		t.Fatalf("expected 1 USR, got %d: %v", len(usrs), usrs)
	}
	if !usrs["s:4main18VideoFrameProviderV"] {
		t.Errorf("expected USR for VideoFrameProvider struct, got set: %v", usrs)
	}

	// "renderFrame()" matches the method — should find exactly the method USR, not the struct USR.
	usrs2, err := collectMatchingUSRs(jsonOut, "renderFrame()")
	if err != nil {
		t.Fatalf("collectMatchingUSRs renderFrame: %v", err)
	}
	if len(usrs2) != 1 || !usrs2["s:4main18VideoFrameProviderV11renderFrameyyF"] {
		t.Errorf("expected exactly the renderFrame method USR, got: %v", usrs2)
	}

	// "process()" is only a reference, not a declaration — should be empty.
	usrs3, err := collectMatchingUSRs(jsonOut, "process()")
	if err != nil {
		t.Fatalf("collectMatchingUSRs process: %v", err)
	}
	if len(usrs3) != 0 {
		t.Errorf("process() is only a ref, expected 0 USRs, got: %v", usrs3)
	}

	// Case-insensitive match.
	usrs4, err := collectMatchingUSRs(jsonOut, "videoframeprovider")
	if err != nil {
		t.Fatalf("collectMatchingUSRs case-insensitive: %v", err)
	}
	if len(usrs4) != 1 || !usrs4["s:4main18VideoFrameProviderV"] {
		t.Errorf("case-insensitive lookup failed: %v", usrs4)
	}
}

func TestFindReferencesByUSR(t *testing.T) {
	// Simulated index output for a file that uses VideoFrameProvider.
	jsonOut := `{
  "key.entities": [
    {
      "key.kind": "source.lang.swift.decl.function.free",
      "key.name": "setup()",
      "key.usr": "s:4main5setupyyF",
      "key.line": 1,
      "key.entities": [
        {
          "key.kind": "source.lang.swift.ref.struct",
          "key.name": "VideoFrameProvider",
          "key.usr": "s:4main18VideoFrameProviderV",
          "key.line": 2
        },
        {
          "key.kind": "source.lang.swift.ref.function.method.instance",
          "key.name": "renderFrame()",
          "key.usr": "s:4main18VideoFrameProviderV11renderFrameyyF",
          "key.line": 4
        }
      ]
    },
    {
      "key.kind": "source.lang.swift.decl.function.free",
      "key.name": "teardown()",
      "key.usr": "s:4main8teardownyyF",
      "key.line": 7,
      "key.entities": [
        {
          "key.kind": "source.lang.swift.ref.struct",
          "key.name": "VideoFrameProvider",
          "key.usr": "s:4main18VideoFrameProviderV",
          "key.line": 8
        }
      ]
    }
  ]
}`

	// Write a temp file with lines matching the line numbers.
	tmp := t.TempDir()
	absPath := filepath.Join(tmp, "Main.swift")
	content := "func setup() {\n    let p = VideoFrameProvider()\n    p.isReady = true\n    p.renderFrame()\n}\n\nfunc teardown() {\n    let p = VideoFrameProvider()\n}\n"
	if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	targetUSRs := map[string]bool{
		"s:4main18VideoFrameProviderV": true,
	}

	refs, err := findReferencesByUSR(jsonOut, targetUSRs, absPath, tmp)
	if err != nil {
		t.Fatalf("findReferencesByUSR: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 references to VideoFrameProvider, got %d: %+v", len(refs), refs)
	}
	if refs[0].Line != 2 || refs[1].Line != 8 {
		t.Errorf("ref lines = %d, %d; want 2, 8", refs[0].Line, refs[1].Line)
	}
	if refs[0].Context == "" {
		t.Error("expected non-empty context line")
	}
}

func TestFindReferencesByUSR_IncludesDeclaration(t *testing.T) {
	// The declaration entity itself should also match (its USR is in targetUSRs).
	jsonOut := `{
  "key.entities": [
    {
      "key.kind": "source.lang.swift.decl.struct",
      "key.name": "VideoFrameProvider",
      "key.usr": "s:4main18VideoFrameProviderV",
      "key.line": 1
    }
  ]
}`
	tmp := t.TempDir()
	absPath := filepath.Join(tmp, "Provider.swift")
	os.WriteFile(absPath, []byte("struct VideoFrameProvider {}\n"), 0644)

	refs, err := findReferencesByUSR(jsonOut, map[string]bool{"s:4main18VideoFrameProviderV": true}, absPath, tmp)
	if err != nil {
		t.Fatalf("findReferencesByUSR: %v", err)
	}
	if len(refs) != 1 || refs[0].Line != 1 {
		t.Errorf("expected 1 ref (the declaration itself) at line 1, got: %+v", refs)
	}
}

func TestFindReferencesByUSR_Empty(t *testing.T) {
	tmp := t.TempDir()
	absPath := filepath.Join(tmp, "empty.swift")
	os.WriteFile(absPath, []byte("\n"), 0644)

	refs, err := findReferencesByUSR(`{"key.entities":[]}`, map[string]bool{"s:fake": true}, absPath, tmp)
	if err != nil {
		t.Fatalf("findReferencesByUSR empty: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("expected 0 references, got %d", len(refs))
	}
}

func TestIsSwiftDeclaration(t *testing.T) {
	if !isSwiftDeclaration("source.lang.swift.decl.struct") {
		t.Error("decl.struct should be a declaration")
	}
	if !isSwiftDeclaration("source.lang.swift.decl.function.method.instance") {
		t.Error("decl.function.method.instance should be a declaration")
	}
	if isSwiftDeclaration("source.lang.swift.ref.struct") {
		t.Error("ref.struct should NOT be a declaration")
	}
	if isSwiftDeclaration("source.lang.swift.syntaxtype.keyword") {
		t.Error("syntaxtype should NOT be a declaration")
	}
}
