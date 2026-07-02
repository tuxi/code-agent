package projectgraph

import (
	"bufio"
	"code-agent/internal/assetref"
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ProjectGraphTool is the model-facing semantic tool. It dispatches on an
// "action" field to one or more LanguageAdapters and normalizes every backend's
// output into the unified Symbol / Reference / RenameCheck schema.
//
// It is read-only: it observes the codebase and never mutates it, so the runtime
// runs it without a confirmation prompt.
type ProjectGraphTool struct {
	Adapters []LanguageAdapter
	Timeout  time.Duration
}

// NewProjectGraphTool wires the MVP language set: Go (gopls, implemented) plus
// Swift, Rust, and Python (stubs that detect their toolchain). Adapters whose
// toolchain is not installed are simply skipped at query time.
func NewProjectGraphTool() *ProjectGraphTool {
	return &ProjectGraphTool{
		Adapters: []LanguageAdapter{
			NewGoAdapter(),
			NewSwiftAdapter(),
			NewRustAdapter(),
			NewPythonAdapter(),
		},
		Timeout: 300 * time.Second,
	}
}

type projectGraphInput struct {
	Action   string `json:"action"`
	Query    string `json:"query"`    // find_symbol
	Symbol   string `json:"symbol"`   // find_references
	From     string `json:"from"`     // rename_check
	To       string `json:"to"`       // rename_check
	Language string `json:"language"` // optional: restrict to one backend
}

type graphOutput struct {
	Kind     string            `json:"kind"`
	Action   string            `json:"action"`
	Query    string            `json:"query,omitempty"`
	Symbol   string            `json:"symbol,omitempty"`
	Language string            `json:"language,omitempty"`
	Items    []graphOutputItem `json:"items"`
}

type graphOutputItem struct {
	AssetID      string `json:"asset_id"`
	Kind         string `json:"kind"`
	Name         string `json:"name,omitempty"`
	SymbolKind   string `json:"symbol_kind,omitempty"`
	Language     string `json:"language,omitempty"`
	Path         string `json:"path"`
	AbsolutePath string `json:"absolute_path,omitempty"`
	Line         int    `json:"line"`
	Column       int    `json:"column,omitempty"`
	Preview      string `json:"preview,omitempty"`
}

func (t *ProjectGraphTool) Name() string { return "project_graph" }

func (t *ProjectGraphTool) Description() string {
	return "Semantic code understanding via language toolchains (gopls, sourcekitten, rust-analyzer, pyright). " +
		"Prefer this over grep whenever you need structure rather than text. Actions: " +
		`"find_symbol" (locate a symbol's definition by name, via "query"), ` +
		`"find_references" (find use-sites of a "symbol"), ` +
		`"rename_check" (safety report for renaming "from" to "to": affected files, collisions, warnings). ` +
		`Optional "language" (go|swift|rust|python) restricts the query to one backend. Returns JSON.`
}

func (t *ProjectGraphTool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"action": {
			Type:        "string",
			Description: "The query to run.",
			Enum:        []string{"find_symbol", "find_references", "rename_check"},
		},
		"query":    {Type: "string", Description: `Symbol name to locate (for action "find_symbol").`},
		"symbol":   {Type: "string", Description: `Symbol name whose references to find (for action "find_references").`},
		"from":     {Type: "string", Description: `Current symbol name (for action "rename_check").`},
		"to":       {Type: "string", Description: `Proposed new symbol name (for action "rename_check").`},
		"language": {Type: "string", Description: "Optional. Restrict to one backend: go, swift, rust, or python."},
	}, "action").JSON()
}

func (t *ProjectGraphTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	var in projectGraphInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("invalid project_graph input: %w", err)
		}
	}

	rootAbs, err := filepath.Abs(ec.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()

	switch strings.TrimSpace(in.Action) {
	case "find_symbol":
		return t.findSymbol(ctx, rootAbs, ec, in)
	case "find_references":
		return t.findReferences(ctx, rootAbs, ec, in)
	case "rename_check":
		return t.renameCheck(ctx, rootAbs, in)
	case "":
		return tools.ToolResult{}, fmt.Errorf("action is required (find_symbol | find_references | rename_check)")
	default:
		return tools.ToolResult{}, fmt.Errorf("unknown action %q (want find_symbol | find_references | rename_check)", in.Action)
	}
}

func (t *ProjectGraphTool) findSymbol(ctx context.Context, root string, ec tools.ExecutionContext, in projectGraphInput) (tools.ToolResult, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return tools.ToolResult{}, fmt.Errorf("query is required for find_symbol")
	}
	adapters, msg := t.selectAdapters(in.Language)
	if len(adapters) == 0 {
		return tools.ToolResult{Content: msg}, nil
	}

	var symbols []Symbol
	var errs []string
	for _, a := range adapters {
		found, err := a.FindSymbol(ctx, root, query)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", a.Language(), err))
			continue
		}
		symbols = append(symbols, found...)
	}
	if symbols == nil {
		symbols = []Symbol{}
	}
	realErrs := filterRealErrors(errs)
	if len(symbols) == 0 && len(realErrs) > 0 {
		return tools.ToolResult{}, fmt.Errorf("find_symbol returned no results, but some backends failed: %s. Specify 'language' to restrict to a single backend.", strings.Join(realErrs, "; "))
	}
	return t.symbolResult(root, ec, in, symbols)
}

func (t *ProjectGraphTool) findReferences(ctx context.Context, root string, ec tools.ExecutionContext, in projectGraphInput) (tools.ToolResult, error) {
	symbol := strings.TrimSpace(in.Symbol)
	if symbol == "" {
		return tools.ToolResult{}, fmt.Errorf("symbol is required for find_references")
	}
	adapters, msg := t.selectAdapters(in.Language)
	if len(adapters) == 0 {
		return tools.ToolResult{Content: msg}, nil
	}

	var refs []Reference
	var errs []string
	for _, a := range adapters {
		found, err := a.FindReferences(ctx, root, symbol)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", a.Language(), err))
			continue
		}
		refs = append(refs, found...)
	}
	if refs == nil {
		refs = []Reference{}
	}
	// When no results were found and at least one backend returned a real
	// (non-stub) error, surface the error so the agent doesn't silently
	// interpret "indexing failed" as "no references exist".
	realErrs := filterRealErrors(errs)
	if len(refs) == 0 && len(realErrs) > 0 {
		return tools.ToolResult{}, fmt.Errorf("find_references returned no results, but some backends failed: %s. Use grep for text-level search, or specify 'language' to restrict to a single backend.", strings.Join(realErrs, "; "))
	}
	return t.referenceResult(root, ec, in, refs)
}

// filterRealErrors excludes "not implemented yet" errors from stub adapters.
// These are expected noise; they don't indicate an operational failure.
func filterRealErrors(errs []string) []string {
	var out []string
	for _, e := range errs {
		if strings.Contains(e, "not implemented yet") {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (t *ProjectGraphTool) renameCheck(ctx context.Context, root string, in projectGraphInput) (tools.ToolResult, error) {
	from := strings.TrimSpace(in.From)
	to := strings.TrimSpace(in.To)
	if from == "" || to == "" {
		return tools.ToolResult{}, fmt.Errorf("both from and to are required for rename_check")
	}
	adapters, msg := t.selectAdapters(in.Language)
	if len(adapters) == 0 {
		return tools.ToolResult{Content: msg}, nil
	}

	var (
		refs       []Reference
		collisions []Symbol
	)
	for _, a := range adapters {
		if r, err := a.FindReferences(ctx, root, from); err == nil {
			refs = append(refs, r...)
		}
		if c, err := a.FindSymbol(ctx, root, to); err == nil {
			collisions = append(collisions, c...)
		}
	}

	files := map[string]struct{}{}
	for _, r := range refs {
		files[r.File] = struct{}{}
	}

	warnings := []string{}
	if len(refs) == 0 {
		warnings = append(warnings, fmt.Sprintf("no references resolved for %q (symbol not found, or its toolchain is unavailable) — rename may be incomplete or unnecessary", from))
	}
	if exact := exactNameMatches(collisions, to); len(exact) > 0 {
		warnings = append(warnings, fmt.Sprintf("%q is already defined (%d symbol(s)); renaming %q to it may cause a collision", to, len(exact), from))
	}

	check := RenameCheck{
		From:          from,
		To:            to,
		AffectedFiles: len(files),
		References:    len(refs),
		Safe:          len(warnings) == 0,
		Warnings:      warnings,
	}
	return jsonResult(check)
}

// selectAdapters resolves which backends to query. With an explicit language it
// returns that one backend (or a message if it is unsupported or unavailable).
// Otherwise it returns every backend whose toolchain is installed. The second
// return value is a human-readable explanation to surface when the slice is
// empty, so the agent learns *why* nothing ran (e.g. "install gopls").
func (t *ProjectGraphTool) selectAdapters(language string) ([]LanguageAdapter, string) {
	language = strings.ToLower(strings.TrimSpace(language))
	if language != "" {
		for _, a := range t.Adapters {
			if a.Language() != language {
				continue
			}
			if !a.Available() {
				return nil, fmt.Sprintf("the %s backend is not available: its toolchain is not installed", language)
			}
			return []LanguageAdapter{a}, ""
		}
		return nil, fmt.Sprintf("unsupported language %q; supported: %s", language, strings.Join(t.supportedLanguages(), ", "))
	}

	var avail []LanguageAdapter
	for _, a := range t.Adapters {
		if a.Available() {
			avail = append(avail, a)
		}
	}
	if len(avail) == 0 {
		return nil, "no language backend is available. Install a toolchain to enable project_graph, e.g. gopls for Go: go install golang.org/x/tools/gopls@latest"
	}
	return avail, ""
}

func (t *ProjectGraphTool) supportedLanguages() []string {
	langs := make([]string, 0, len(t.Adapters))
	for _, a := range t.Adapters {
		langs = append(langs, a.Language())
	}
	sort.Strings(langs)
	return langs
}

func exactNameMatches(symbols []Symbol, name string) []Symbol {
	var out []Symbol
	for _, s := range symbols {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

func jsonResult(v any) (tools.ToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return tools.ToolResult{}, err
	}
	return tools.ToolResult{Content: string(data)}, nil
}

func (t *ProjectGraphTool) symbolResult(root string, ec tools.ExecutionContext, in projectGraphInput, symbols []Symbol) (tools.ToolResult, error) {
	content, err := jsonContent(symbols)
	if err != nil {
		return tools.ToolResult{}, err
	}
	workspaceID := assets.WorkspaceID(root)
	items := make([]graphOutputItem, 0, len(symbols))
	refs := make([]assets.Ref, 0, len(symbols))
	for i, s := range symbols {
		rel := filepath.ToSlash(s.File)
		abs := filepath.Join(root, filepath.FromSlash(rel))
		preview := previewLine(abs, s.Line)
		lang := languageForPath(rel, in.Language)
		rng := &assets.Range{StartLine: s.Line, StartColumn: 1}
		id := assets.StableID(ec.TurnID, ec.CallID, i+1, "project_graph", "symbol", s.Name, s.Kind, rel, fmt.Sprint(s.Line))
		items = append(items, graphOutputItem{
			AssetID:      id,
			Kind:         "symbol",
			Name:         s.Name,
			SymbolKind:   s.Kind,
			Language:     lang,
			Path:         rel,
			AbsolutePath: abs,
			Line:         s.Line,
			Column:       1,
			Preview:      preview,
		})
		refs = append(refs, assets.Ref{
			ID:                    id,
			Kind:                  "symbol",
			URI:                   assets.WorkspaceURI(workspaceID, rel, rng),
			DisplayName:           s.Name,
			WorkspaceID:           workspaceID,
			WorkspaceRelativePath: rel,
			AbsolutePath:          abs,
			Range:                 rng,
			Preview:               preview,
			MIMEType:              assets.MIMEType(rel),
			Metadata: map[string]string{
				"symbol_kind": s.Kind,
				"language":    lang,
			},
			SourceTurnID: ec.TurnID,
			SourceCallID: ec.CallID,
		})
	}
	output, err := tools.JSONOutput(graphOutput{
		Kind:     "symbols",
		Action:   "find_symbol",
		Query:    strings.TrimSpace(in.Query),
		Language: strings.TrimSpace(in.Language),
		Items:    items,
	})
	if err != nil {
		return tools.ToolResult{}, err
	}
	return tools.ToolResult{Content: content, Output: output, Assets: refs}, nil
}

func (t *ProjectGraphTool) referenceResult(root string, ec tools.ExecutionContext, in projectGraphInput, refs []Reference) (tools.ToolResult, error) {
	content, err := jsonContent(refs)
	if err != nil {
		return tools.ToolResult{}, err
	}
	workspaceID := assets.WorkspaceID(root)
	items := make([]graphOutputItem, 0, len(refs))
	assetRefs := make([]assets.Ref, 0, len(refs))
	for i, ref := range refs {
		rel := filepath.ToSlash(ref.File)
		abs := filepath.Join(root, filepath.FromSlash(rel))
		preview := strings.TrimSpace(ref.Context)
		if preview == "" {
			preview = previewLine(abs, ref.Line)
		}
		lang := languageForPath(rel, in.Language)
		rng := &assets.Range{StartLine: ref.Line, StartColumn: 1}
		id := assets.StableID(ec.TurnID, ec.CallID, i+1, "project_graph", "reference", strings.TrimSpace(in.Symbol), rel, fmt.Sprint(ref.Line), preview)
		items = append(items, graphOutputItem{
			AssetID:      id,
			Kind:         "file_location",
			Name:         strings.TrimSpace(in.Symbol),
			Language:     lang,
			Path:         rel,
			AbsolutePath: abs,
			Line:         ref.Line,
			Column:       1,
			Preview:      preview,
		})
		assetRefs = append(assetRefs, assets.Ref{
			ID:                    id,
			Kind:                  "file_location",
			URI:                   assets.WorkspaceURI(workspaceID, rel, rng),
			DisplayName:           assets.DisplayName(rel, ref.Line),
			WorkspaceID:           workspaceID,
			WorkspaceRelativePath: rel,
			AbsolutePath:          abs,
			Range:                 rng,
			Preview:               preview,
			MIMEType:              assets.MIMEType(rel),
			Metadata: map[string]string{
				"language": lang,
			},
			SourceTurnID: ec.TurnID,
			SourceCallID: ec.CallID,
		})
	}
	output, err := tools.JSONOutput(graphOutput{
		Kind:     "references",
		Action:   "find_references",
		Symbol:   strings.TrimSpace(in.Symbol),
		Language: strings.TrimSpace(in.Language),
		Items:    items,
	})
	if err != nil {
		return tools.ToolResult{}, err
	}
	return tools.ToolResult{Content: content, Output: output, Assets: assetRefs}, nil
}

func jsonContent(v any) (string, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func previewLine(path string, line int) string {
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
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		if lineNo == line {
			return strings.TrimSpace(scanner.Text())
		}
	}
	return ""
}

func languageForPath(path, explicit string) string {
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		return strings.ToLower(explicit)
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".swift":
		return "swift"
	case ".rs":
		return "rust"
	case ".py":
		return "python"
	default:
		return ""
	}
}

var _ tools.Tool = (*ProjectGraphTool)(nil)
