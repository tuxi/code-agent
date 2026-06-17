package projectgraph

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// goAdapter delegates Go semantic queries to gopls' CLI surface:
//
//	gopls workspace_symbol <query>   -> symbol search
//	gopls references <file:line:col> -> use-sites of the symbol at a position
//
// It never parses Go source itself. The exec wrappers are thin; the value is in
// the parsers (parseWorkspaceSymbols / parseReferenceLocations), which are pure
// functions and carry the test coverage since gopls may not be installed in CI.
type goAdapter struct {
	timeout int // seconds; 0 uses caller's context deadline only
}

// NewGoAdapter returns the Go backend, delegating to gopls.
func NewGoAdapter() LanguageAdapter { return &goAdapter{} }

func (a *goAdapter) Language() string { return "go" }

func (a *goAdapter) Available() bool {
	_, err := exec.LookPath("gopls")
	return err == nil
}

func (a *goAdapter) FindSymbol(ctx context.Context, root, query string) ([]Symbol, error) {
	out, err := a.run(ctx, root, "workspace_symbol", query)
	if err != nil {
		return nil, err
	}
	return parseWorkspaceSymbols(out, root), nil
}

func (a *goAdapter) FindReferences(ctx context.Context, root, symbol string) ([]Reference, error) {
	// gopls references needs a position, not a name. Resolve the symbol to its
	// definition position via workspace_symbol, then query references there.
	symOut, err := a.run(ctx, root, "workspace_symbol", symbol)
	if err != nil {
		return nil, err
	}
	pos := firstDefinitionPosition(symOut, symbol)
	if pos == "" {
		return nil, nil // symbol not resolved; caller treats as zero references
	}

	refOut, err := a.run(ctx, root, "references", pos)
	if err != nil {
		return nil, err
	}

	locs := parseReferenceLocations(refOut, root)
	refs := make([]Reference, 0, len(locs))
	for _, l := range locs {
		refs = append(refs, Reference{
			File:    l.file,
			Line:    l.line,
			Context: readSourceLine(filepath.Join(root, filepath.FromSlash(l.file)), l.line),
		})
	}
	return refs, nil
}

func (a *goAdapter) run(ctx context.Context, root string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gopls", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// location is an internal (file, line, col) triple parsed from a gopls position.
type location struct {
	file string // workspace-relative, slash-separated
	line int
	col  int
}

// parseWorkspaceSymbols parses `gopls workspace_symbol` output. Each line is:
//
//	<file>:<startLine>:<startCol>-<endLine>:<endCol> <Name> <Kind>
//
// e.g. "/abs/path/VideoFrameProvider.swift.go:32:6-32:18 SourceOutput Struct".
// Paths are normalized to workspace-relative; kinds are lowercased to match the
// unified schema's convention.
func parseWorkspaceSymbols(out, root string) []Symbol {
	var syms []Symbol
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		loc := parseLocation(fields[0])
		syms = append(syms, Symbol{
			Name: fields[len(fields)-2],
			Kind: strings.ToLower(fields[len(fields)-1]),
			File: relativize(loc.file, root),
			Line: loc.line,
		})
	}
	return syms
}

// parseReferenceLocations parses `gopls references` output, one location per
// line ("<file>:<line>:<col>-..." or "<file>:<line>:<col>"), into relative
// (file, line) pairs.
func parseReferenceLocations(out, root string) []location {
	var locs []location
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		loc := parseLocation(strings.Fields(line)[0])
		if loc.file == "" || loc.line == 0 {
			continue
		}
		loc.file = relativize(loc.file, root)
		locs = append(locs, loc)
	}
	return locs
}

// firstDefinitionPosition returns the "file:line:col" start position of the
// first workspace_symbol entry whose name exactly matches symbol, suitable to
// pass to `gopls references`. It returns "" when there is no exact-name match.
func firstDefinitionPosition(symOut, symbol string) string {
	for _, raw := range strings.Split(symOut, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if fields[len(fields)-2] != symbol {
			continue
		}
		// fields[0] is "file:startLine:startCol-endLine:endCol"; the start
		// position is everything before the range dash.
		locStr := fields[0]
		if dash := strings.Index(locStr, "-"); dash >= 0 {
			locStr = locStr[:dash]
		}
		return locStr
	}
	return ""
}

// parseLocation extracts the file and 1-based start line (and col) from a gopls
// position string like "path/file.go:32:6-32:18" or "path/file.go:32:6". The
// file part is taken up to the first colon, which is correct for POSIX paths
// (Windows drive letters are a non-goal).
func parseLocation(loc string) location {
	i := strings.Index(loc, ":")
	if i < 0 {
		return location{file: loc}
	}
	file := loc[:i]
	rest := loc[i+1:]

	line := leadingInt(rest)
	col := 0
	if j := strings.Index(rest, ":"); j >= 0 {
		col = leadingInt(rest[j+1:])
	}
	return location{file: file, line: line, col: col}
}

// leadingInt parses the leading run of digits of s (stopping at the first
// non-digit such as ':' or '-'), returning 0 if there is none.
func leadingInt(s string) int {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	n, _ := strconv.Atoi(s[:end])
	return n
}

// relativize converts an absolute path under root to a workspace-relative,
// slash-separated path. Paths already relative, or outside root, are returned
// slash-normalized as-is.
func relativize(path, root string) string {
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) && root != "" {
		if rootAbs, err := filepath.Abs(root); err == nil {
			if rel, err := filepath.Rel(rootAbs, path); err == nil && !strings.HasPrefix(rel, "..") {
				return filepath.ToSlash(rel)
			}
		}
	}
	return filepath.ToSlash(path)
}

// readSourceLine returns the trimmed content of the given 1-based line of a
// file, or "" if it cannot be read. Best-effort: context is a convenience, not
// load-bearing.
func readSourceLine(path string, line int) string {
	if line <= 0 {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	n := 0
	for scanner.Scan() {
		n++
		if n == line {
			return strings.TrimSpace(scanner.Text())
		}
	}
	return ""
}
