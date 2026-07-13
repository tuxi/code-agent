package projectgraph

import (
	"code-agent/internal/workspace"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// errEarlyExit is a sentinel error used to stop file iteration early once
// Phase 1 has found at least one matching USR.
var errEarlyExit = errors.New("early exit: USR found")

// swiftAdapter delegates Swift semantic queries to sourcekitten (SourceKit).
//
//	sourcekitten structure --file <f>  -> JSON AST with key.substructure tree
//	sourcekitten index --file <f> -- -sdk <sdk> <f> -> JSON index with USRs
//
// It never parses Swift source itself. The value is in the JSON-tree walker
// (collectSymbols) and kind normalizer (normalizeSwiftKind), which are pure
// functions and carry the test coverage since sourcekitten may not be
// installed in CI.
//
// FindSymbol walks every .swift file under the workspace root, runs
// sourcekitten structure on each, and recurses into the key.substructure tree
// to find declarations whose key.name matches the query. This is O(n_files)
// but sourcekitten has no workspace-wide query primitive.
//
// FindReferences is implemented via a two-pass per-file scan of sourcekitten
// index output. It is O(2 × n_files) without a workspace index, but it is
// precise: matching is based on compiler-resolved USRs, not text.
//
// Phase 1: walk all .swift files, run `sourcekitten index --file`, and
// collect the USR of every declaration whose name matches symbol.
//
// Phase 2: walk all .swift files again, run `sourcekitten index --file`, and
// collect every entity (declaration or reference) whose USR matches one of the
// target USRs.
//
// Unlike the structure command, the index command requires compiler arguments
// (at minimum -sdk <path>). The adapter caches the macOS SDK path on first use
// and falls back gracefully if xcrun is not available.
type swiftAdapter struct {
	sdkPath      string // cached macOS SDK path for index compiler args
	sdkPathReady bool   // true after first attempt to resolve sdkPath

	indexStorePath      string // cached path to the Swift index store
	indexStorePathReady bool   // true after first attempt to resolve index store
	indexStoreAvailable bool   // true if the index store helper works
	helperChecked       bool   // true after first attempt to use the helper
}

func (a *swiftAdapter) Language() string { return "swift" }

func (a *swiftAdapter) Available() bool {
	_, err := exec.LookPath("sourcekitten")
	return err == nil
}

func (a *swiftAdapter) FindSymbol(ctx context.Context, root, query string) ([]Symbol, error) {
	var allSymbols []Symbol

	err := walkSwiftFiles(root, func(absPath string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		out, err := a.run(ctx, absPath, "structure", "--file", absPath)
		if err != nil {
			return nil // best-effort: skip files sourcekitten can't parse
		}
		syms, err := collectSymbols(out, query, absPath, root)
		if err != nil {
			return nil // skip malformed output
		}
		allSymbols = append(allSymbols, syms...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if allSymbols == nil {
		allSymbols = []Symbol{}
	}
	return allSymbols, nil
}

func (a *swiftAdapter) FindReferences(ctx context.Context, root, symbol string) ([]Reference, error) {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return nil, fmt.Errorf("symbol is required for find_references")
	}

	var diag []string // collects diagnostic info across all attempted paths

	// Primary path: use the compiler's index store if available.
	idxPath := a.resolveIndexStore(root)
	if idxPath != "" {
		refs, err := a.findReferencesViaIndexStore(ctx, idxPath, symbol, root)
		if err == nil && len(refs) > 0 {
			return refs, nil
		}
		if err != nil {
			diag = append(diag, fmt.Sprintf("indexstore: %v", err))
		} else {
			diag = append(diag, fmt.Sprintf("indexstore: symbol %q not found in %s", symbol, idxPath))
		}
		// Fall through to sourcekitten fallback.
	} else {
		diag = append(diag, "indexstore: not available (no .build/debug/index or DerivedData found)")
	}

	// Fallback: per-file sourcekitten index.
	sdk := a.resolveSDK()
	if sdk != "" {
		indexArgs := a.resolveBuildFlags(root, sdk)
		targetUSRs := make(map[string]bool)
		var phase1Tried, phase1OK, totalDecls, parseErrors int
		var sampleNames []string // first few decl names for debugging
		if err := walkSwiftFiles(root, func(absPath string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			phase1Tried++
			out, err := a.runIndex(ctx, absPath, indexArgs)
			if err != nil {
				return nil
			}
			phase1OK++
			declCount, names := countDeclarations(out, 50)
			totalDecls += declCount
			sampleNames = append(sampleNames, names...)
			usrs, err := collectMatchingUSRs(out, symbol)
			if err != nil {
				parseErrors++
				return nil
			}
			for u := range usrs {
				targetUSRs[u] = true
			}
			// Stop scanning files early once we have the symbol's USR.
			if len(targetUSRs) > 0 {
				return errEarlyExit
			}
			return nil
		}); err != nil && err != errEarlyExit {
			return nil, err
		}

		diag = append(diag, fmt.Sprintf("sourcekitten: tried %d files, %d ok, %d parse errs, %d total decls, sample: %s",
			phase1Tried, phase1OK, parseErrors, totalDecls, strings.Join(firstN(sampleNames, 15), ", ")))

		if phase1Tried > 0 && phase1OK == 0 {
			diag = append(diag, "sourcekitten: all files failed to index (missing compiler args?)")
		}

		if len(targetUSRs) > 0 {
			var refs []Reference
			if err := walkSwiftFiles(root, func(absPath string) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				out, err := a.runIndex(ctx, absPath, indexArgs)
				if err != nil {
					return nil
				}
				found, err := findReferencesByUSR(out, targetUSRs, absPath, root)
				if err != nil {
					return nil
				}
				refs = append(refs, found...)
				return nil
			}); err != nil {
				return nil, err
			}
			if len(refs) > 0 {
				refs = dedupeRefs(refs)
				return refs, nil
			}
		}
	} else {
		diag = append(diag, "sourcekitten: SDK not available")
	}

	// Nothing worked. Return the diagnostic so the agent can act on it.
	return []Reference{}, fmt.Errorf(
		"find_references %q: all backends failed — %s. Use grep for text-level search.",
		symbol, strings.Join(diag, "; "),
	)
}

// ---------------------------------------------------------------------------
// IndexStore-backed queries (C helper)
// ---------------------------------------------------------------------------

// resolveIndexStore returns the path to the Swift index store for the given
// workspace root, or "" if no index store is found. It searches SPM build
// directories and Xcode DerivedData.
func (a *swiftAdapter) resolveIndexStore(root string) string {
	if a.indexStorePathReady {
		return a.indexStorePath
	}
	a.indexStorePathReady = true

	var candidates []string

	// SPM: .build/<triple>/debug/index. Check root and parent dir.
	for _, r := range []string{root, filepath.Dir(root)} {
		buildDir := filepath.Join(r, ".build")
		entries, err := os.ReadDir(buildDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			indexPath := filepath.Join(buildDir, e.Name(), "debug", "index")
			if s, err := os.Stat(indexPath); err == nil && s.IsDir() {
				candidates = append(candidates, indexPath)
			}
		}
	}

	// Xcode DerivedData (common patterns)
	if home, err := os.UserHomeDir(); err == nil {
		derivedData := filepath.Join(home, "Library", "Developer", "Xcode", "DerivedData")
		if entries, err := os.ReadDir(derivedData); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				indexPath := filepath.Join(derivedData, e.Name(), "Index.noindex")
				if s, err := os.Stat(indexPath); err == nil && s.IsDir() {
					candidates = append(candidates, indexPath)
				}
			}
		}
	}

	if len(candidates) > 0 {
		a.indexStorePath = candidates[0]
	}
	return a.indexStorePath
}

// findReferencesViaIndexStore resolves the symbol name to a USR via the index
// store, then finds all occurrences of that USR (including cross-file refs).
func (a *swiftAdapter) findReferencesViaIndexStore(ctx context.Context, indexStorePath, symbol, root string) ([]Reference, error) {
	helper, err := a.indexStoreHelper()
	if err != nil {
		return nil, err
	}

	// Step 1: find the symbol's USR.
	symOut, err := a.runHelper(ctx, helper, "find-symbol", indexStorePath, symbol)
	if err != nil {
		return nil, fmt.Errorf("indexstore find-symbol: %w", err)
	}
	var symbols []struct {
		Name string `json:"name"`
		USR  string `json:"usr"`
		Kind string `json:"kind"`
		File string `json:"file"`
		Line int    `json:"line"`
	}
	if err := json.Unmarshal([]byte(symOut), &symbols); err != nil {
		return nil, fmt.Errorf("parse find-symbol output: %w", err)
	}
	if len(symbols) == 0 {
		return []Reference{}, nil
	}

	// Step 2: find all occurrences for each matching USR.
	var allRefs []Reference
	seen := make(map[string]bool)
	for _, sym := range symbols {
		if sym.USR == "" || seen[sym.USR] {
			continue
		}
		seen[sym.USR] = true

		occOut, err := a.runHelper(ctx, helper, "occurrences", indexStorePath, sym.USR)
		if err != nil {
			continue // one USR failure shouldn't block others
		}
		var occs []struct {
			USR    string `json:"usr"`
			File   string `json:"file"`
			Line   int    `json:"line"`
			Column int    `json:"column"`
			Name   string `json:"name"`
			Kind   string `json:"kind"`
		}
		if err := json.Unmarshal([]byte(occOut), &occs); err != nil {
			continue
		}
		for _, o := range occs {
			allRefs = append(allRefs, Reference{
				File:    relativize(o.File, root),
				Line:    o.Line,
				Context: readSourceLine(o.File, o.Line),
			})
		}
		// Deduplicate by (file, line).
		allRefs = dedupeRefs(allRefs)
	}
	if allRefs == nil {
		allRefs = []Reference{}
	}
	return allRefs, nil
}

// indexStoreHelper returns the path to the compiled C helper binary, building
// it on first use if necessary.
func (a *swiftAdapter) indexStoreHelper() (string, error) {
	// Locate the helper relative to this source file.
	// The helper lives at internal/tools/project_graph/indexstore-helper/
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot locate indexstore-helper: runtime.Caller failed")
	}
	helperDir := filepath.Join(filepath.Dir(thisFile), "indexstore-helper")
	helperBin := filepath.Join(helperDir, "indexstore-helper")

	if _, err := os.Stat(helperBin); os.IsNotExist(err) {
		// Try to build. Only works on macOS with Xcode CLT.
		cmd := exec.Command("cc",
			"-o", "indexstore-helper", "main.c",
			"-F", "/Applications/Xcode.app/Contents/SharedFrameworks",
			"-framework", "IndexStoreDB_CIndexStoreDB",
			"-framework", "IndexStoreDB_LLVMSupport",
			"-framework", "IndexStoreDB_Support",
			"-framework", "IndexStoreDB_Core",
			"-framework", "IndexStoreDB_Database",
			"-framework", "IndexStoreDB_Index",
			"-Xlinker", "-rpath", "-Xlinker", "/Applications/Xcode.app/Contents/SharedFrameworks",
			"-fblocks",
		)
		cmd.Dir = helperDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("failed to build indexstore-helper: %v\n%s", err, string(out))
		}
	}
	return helperBin, nil
}

// runHelper executes the indexstore-helper binary with the given arguments
// and returns stdout.
func (a *swiftAdapter) runHelper(ctx context.Context, helper string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, helper, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w\n%s", helper, err, string(out))
	}
	return string(out), nil
}

// runtimeCaller returns the file of the caller. This is a thin wrapper so
// tests can override it.
func runtimeCaller() (uintptr, string, int, bool) {
	return caller(0)
}

// caller is a variable so tests can stub it.
var caller = func(skip int) (uintptr, string, int, bool) {
	// Use runtime.Caller but avoid import cycle; we know this file's path.
	return 0, "", 0, false
}

func (a *swiftAdapter) run(ctx context.Context, absPath string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "sourcekitten", args...)
	cmd.Dir = filepath.Dir(absPath)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runIndex runs `sourcekitten index --file <absPath>` with compiler arguments.
// Uses Output() (stdout only) because CombinedOutput() merges stderr warnings
// into the JSON, making it unparseable.
func (a *swiftAdapter) runIndex(ctx context.Context, absPath string, args []string) (string, error) {
	base := []string{"index", "--file", absPath, "--"}
	base = append(base, args...)
	base = append(base, absPath) // file must also appear as a compiler input
	cmd := exec.CommandContext(ctx, "sourcekitten", base...)
	cmd.Dir = filepath.Dir(absPath)
	out, err := cmd.Output()
	return string(out), err
}

// resolveSDK returns the path to the macOS SDK, caching the result after the
// first call. It tries xcrun first, then the SDKROOT environment variable.
func (a *swiftAdapter) resolveSDK() string {
	if a.sdkPathReady {
		return a.sdkPath
	}
	a.sdkPathReady = true

	// Try xcrun first.
	if out, err := exec.Command("xcrun", "--show-sdk-path").Output(); err == nil {
		a.sdkPath = strings.TrimSpace(string(out))
		return a.sdkPath
	}
	// Fall back to SDKROOT.
	if sdk := os.Getenv("SDKROOT"); sdk != "" {
		a.sdkPath = sdk
		return a.sdkPath
	}
	return ""
}

// resolveBuildFlags returns the compiler arguments needed by sourcekitten index.
// It always includes -sdk. When the workspace is an SPM project that has been
// built, it additionally includes -I flags for the build output directories so
// cross-module imports can be resolved.
func (a *swiftAdapter) resolveBuildFlags(root, sdk string) []string {
	args := []string{"-sdk", sdk}

	// SPM project: if Package.swift exists and .build/debug/ has output, add
	// include paths for the compiled modules.
	pkgSwift := filepath.Join(root, "Package.swift")
	if _, err := os.Stat(pkgSwift); err == nil {
		// SPM 5.9+ uses target-triple-specific dirs; also try the legacy flat
		// layout.
		candidates := []string{
			filepath.Join(root, ".build", "debug"),
		}
		// Add target-specific dirs if we can guess the triple (arm64-apple-macos).
		if entries, err := os.ReadDir(filepath.Join(root, ".build")); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				debug := filepath.Join(root, ".build", e.Name(), "debug")
				if s, err := os.Stat(debug); err == nil && s.IsDir() {
					candidates = append(candidates, debug)
				}
			}
		}
		for _, dir := range candidates {
			if s, err := os.Stat(dir); err == nil && s.IsDir() {
				args = append(args, "-I", dir)
				// ModuleCache may not exist; add it if present.
				cache := filepath.Join(dir, "ModuleCache")
				if s, err := os.Stat(cache); err == nil && s.IsDir() {
					args = append(args, "-I", cache)
				}
			}
		}
	}
	return args
}

// ---------------------------------------------------------------------------
// JSON-tree walker — pure functions, testable without sourcekitten
// ---------------------------------------------------------------------------

// swiftNode is a single declaration node in sourcekitten's structure JSON.
// Only the fields we need are decoded; the rest is ignored.
type swiftNode struct {
	Kind         string      `json:"key.kind"`
	Name         string      `json:"key.name"`
	Offset       int         `json:"key.offset"`
	NameOffset   int         `json:"key.nameoffset"`
	NameLength   int         `json:"key.namelength"`
	Substructure []swiftNode `json:"key.substructure"`
}

// swiftStructureRoot is the top-level object sourcekitten returns.
type swiftStructureRoot struct {
	Substructure []swiftNode `json:"key.substructure"`
}

// collectSymbols parses sourcekitten's JSON structure output, recurses into
// the key.substructure tree, and returns every declaration whose (case-folded)
// name exactly matches query. Byte offsets are converted to 1-based line
// numbers using the file content.
func collectSymbols(jsonOut, query, absPath, root string) ([]Symbol, error) {
	dec := json.NewDecoder(strings.NewReader(jsonOut))
	// sourcekitten may emit the JSON as a single-line blob; use buffered decode.
	var rootNode swiftStructureRoot
	if err := dec.Decode(&rootNode); err != nil {
		return nil, fmt.Errorf("decode sourcekitten structure: %w", err)
	}

	lines := buildLineTable(absPath)

	var syms []Symbol
	queryLower := strings.ToLower(query)
	walkSubstructure(rootNode.Substructure, func(n swiftNode) bool {
		// Skip non-declaration nodes (expressions, statements, etc.).
		// sourcekitten's structure tree includes expr.call nodes whose
		// key.name matches a type name (e.g. "VideoFrameProvider" in
		// "let x = VideoFrameProvider()").
		if !isSwiftDeclaration(n.Kind) {
			return true
		}
		if strings.ToLower(n.Name) != queryLower {
			return true // continue walking children
		}
		line := offsetToLine(lines, n.NameOffset)
		if line == 0 {
			line = offsetToLine(lines, n.Offset) // fallback to decl start
		}
		syms = append(syms, Symbol{
			Name: n.Name,
			Kind: normalizeSwiftKind(n.Kind),
			File: relativize(absPath, root),
			Line: line,
		})
		return true
	})
	return syms, nil
}

// walkSubstructure recurses through the node tree, calling fn for each node.
// If fn returns false, the node's children are skipped.
func walkSubstructure(nodes []swiftNode, fn func(swiftNode) bool) {
	for _, n := range nodes {
		if fn(n) {
			walkSubstructure(n.Substructure, fn)
		}
	}
}

// ---------------------------------------------------------------------------
// Kind normalizer — UTI → human-readable
// ---------------------------------------------------------------------------

// normalizeSwiftKind maps sourcekitten's UTI kind strings
// (source.lang.swift.decl.*) to the short, language-agnostic kind labels the
// unified schema uses. Unknown kinds are lowercased and their prefix stripped.
func normalizeSwiftKind(uti string) string {
	if s, ok := swiftKindMap[uti]; ok {
		return s
	}
	// Fallback: strip the UTI prefix and lower-case the remainder.
	const prefix = "source.lang.swift.decl."
	if after, ok := strings.CutPrefix(uti, prefix); ok {
		return strings.ToLower(strings.ReplaceAll(after, ".", " "))
	}
	const syntaxPrefix = "source.lang.swift.syntaxtype."
	if after, ok := strings.CutPrefix(uti, syntaxPrefix); ok {
		return strings.ToLower(after)
	}
	return strings.ToLower(uti)
}

var swiftKindMap = map[string]string{
	"source.lang.swift.decl.struct":                   "struct",
	"source.lang.swift.decl.class":                    "class",
	"source.lang.swift.decl.enum":                     "enum",
	"source.lang.swift.decl.protocol":                 "protocol",
	"source.lang.swift.decl.extension":                "extension",
	"source.lang.swift.decl.function.method.instance": "method",
	"source.lang.swift.decl.function.method.static":   "static method",
	"source.lang.swift.decl.function.method.class":    "class method",
	"source.lang.swift.decl.function.free":            "function",
	"source.lang.swift.decl.function.constructor":     "constructor",
	"source.lang.swift.decl.function.destructor":      "destructor",
	"source.lang.swift.decl.function.subscript":       "subscript",
	"source.lang.swift.decl.function.accessor.getter": "getter",
	"source.lang.swift.decl.function.accessor.setter": "setter",
	"source.lang.swift.decl.var.instance":             "instance variable",
	"source.lang.swift.decl.var.static":               "static variable",
	"source.lang.swift.decl.var.class":                "class variable",
	"source.lang.swift.decl.var.global":               "global variable",
	"source.lang.swift.decl.var.local":                "local variable",
	"source.lang.swift.decl.var.parameter":            "parameter",
	"source.lang.swift.decl.typealias":                "typealias",
	"source.lang.swift.decl.associatedtype":           "associated type",
	"source.lang.swift.decl.enumcase":                 "enum case",
	"source.lang.swift.decl.enumelement":              "enum element",
	"source.lang.swift.decl.generic_type_param":       "generic type param",
	"source.lang.swift.decl.subscript":                "subscript",
	"source.lang.swift.decl.opaque_type":              "opaque type",
}

// ---------------------------------------------------------------------------
// Offset → line conversion
// ---------------------------------------------------------------------------

// lineTable records the byte offset where each 1-based line starts.
type lineTable []int

// buildLineTable reads absPath and returns a table where table[n] is the byte
// offset of the start of line n+1. Returns nil if the file cannot be read.
func buildLineTable(absPath string) lineTable {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil
	}
	var table lineTable
	table = append(table, 0) // line 1 starts at offset 0
	for i, b := range data {
		if b == '\n' {
			table = append(table, i+1) // next line starts after '\n'
		}
	}
	return table
}

// offsetToLine returns the 1-based line number for the given byte offset,
// or 0 if the offset is out of range. It binary-searches the line table.
func offsetToLine(lines lineTable, offset int) int {
	if len(lines) == 0 || offset < 0 {
		return 0
	}
	// The line table is small enough that a linear scan is faster than sort.Search.
	// Walk backwards to find the line whose start ≤ offset.
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i] <= offset {
			return i + 1 // 1-based
		}
	}
	return 1
}

// ---------------------------------------------------------------------------
// File walker
// ---------------------------------------------------------------------------

// walkSwiftFiles finds every .swift file under root (skipping hidden dirs,
// Packages, .build, DerivedData, and similar non-source trees) and calls fn
// with the absolute path of each file.
func walkSwiftFiles(root string, fn func(absPath string) error) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		base := d.Name()
		if workspace.ShouldSkipPath(root, path) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if base == "" {
				return nil
			}
			// Skip directories that are never user source.
			if base[0] == '.' ||
				base == "node_modules" ||
				base == "Pods" ||
				base == "Carthage" ||
				base == ".build" ||
				base == "DerivedData" ||
				base == "Packages" ||
				base == "build" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(base, ".swift") {
			return nil
		}
		absPath, err := filepath.Abs(path)
		if err != nil {
			absPath = path
		}
		return fn(absPath)
	})
}

// ---------------------------------------------------------------------------
// Index-based reference resolution — two-pass per-file USR matching
// ---------------------------------------------------------------------------

// swiftIndexEntity is a single entity in sourcekitten's index JSON. Unlike the
// structure output (which uses byte offsets), the index output reports
// key.line directly and includes USRs on both declarations and references.
// Entities nest recursively: a function declaration contains its parameters,
// local variables, and any symbols it references within its body.
type swiftIndexEntity struct {
	Kind     string             `json:"key.kind"`
	Name     string             `json:"key.name"`
	USR      string             `json:"key.usr"`
	Line     int                `json:"key.line"`
	Entities []swiftIndexEntity `json:"key.entities"`
}

// swiftIndexRoot is the top-level object sourcekitten's index command returns.
type swiftIndexRoot struct {
	Entities []swiftIndexEntity `json:"key.entities"`
}

// collectMatchingUSRs parses sourcekitten's index JSON and returns the set of
// USRs for every declaration whose (case-folded) name matches query.
func collectMatchingUSRs(jsonOut, query string) (map[string]bool, error) {
	var root swiftIndexRoot
	if err := json.Unmarshal([]byte(jsonOut), &root); err != nil {
		return nil, fmt.Errorf("decode sourcekitten index: %w", err)
	}
	usrs := make(map[string]bool)
	queryLower := strings.ToLower(query)
	walkIndexEntities(root.Entities, func(e swiftIndexEntity) bool {
		if e.USR == "" {
			return true
		}
		if strings.ToLower(e.Name) == queryLower && isSwiftDeclaration(e.Kind) {
			usrs[e.USR] = true
		}
		return true
	})
	return usrs, nil
}

// findReferencesByUSR parses sourcekitten's index JSON and returns a Reference
// for every entity whose USR is in targetUSRs. Both declarations and references
// are included, since the definition site is a valid use-site for rename
// analysis.
func findReferencesByUSR(jsonOut string, targetUSRs map[string]bool, absPath, root string) ([]Reference, error) {
	var rootNode swiftIndexRoot
	if err := json.Unmarshal([]byte(jsonOut), &rootNode); err != nil {
		return nil, fmt.Errorf("decode sourcekitten index: %w", err)
	}
	relPath := relativize(absPath, root)
	var refs []Reference
	walkIndexEntities(rootNode.Entities, func(e swiftIndexEntity) bool {
		if e.USR == "" || !targetUSRs[e.USR] {
			return true
		}
		refs = append(refs, Reference{
			File:    relPath,
			Line:    e.Line,
			Context: readSourceLine(absPath, e.Line),
		})
		return true
	})
	return refs, nil
}

// isSwiftDeclaration reports whether kind is a Swift declaration UTI (as
// opposed to a reference, syntax token, or import).
func isSwiftDeclaration(kind string) bool {
	return strings.HasPrefix(kind, "source.lang.swift.decl.")
}

// countDeclarations returns the total number of declaration entities found in
// sourcekitten index JSON and up to `maxNames` sample names for diagnostics.
func countDeclarations(jsonOut string, maxNames int) (int, []string) {
	var root swiftIndexRoot
	if err := json.Unmarshal([]byte(jsonOut), &root); err != nil {
		return 0, nil
	}
	count := 0
	var names []string
	walkIndexEntities(root.Entities, func(e swiftIndexEntity) bool {
		if isSwiftDeclaration(e.Kind) {
			count++
			if len(names) < maxNames && e.Name != "" {
				names = append(names, e.Name)
			}
		}
		return true
	})
	return count, names
}

func dedupeRefs(refs []Reference) []Reference {
	if len(refs) <= 1 {
		return refs
	}
	seen := make(map[string]bool)
	var out []Reference
	for _, r := range refs {
		key := fmt.Sprintf("%s:%d", r.File, r.Line)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// walkIndexEntities recurses through the index entity tree, calling fn for
// each node. If fn returns false, the node's children are skipped.
func walkIndexEntities(entities []swiftIndexEntity, fn func(swiftIndexEntity) bool) {
	for _, e := range entities {
		if fn(e) {
			walkIndexEntities(e.Entities, fn)
		}
	}
}
