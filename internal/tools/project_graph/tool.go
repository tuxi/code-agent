package projectgraph

import (
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"fmt"
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
		Timeout: 30 * time.Second,
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
		return t.findSymbol(ctx, rootAbs, in)
	case "find_references":
		return t.findReferences(ctx, rootAbs, in)
	case "rename_check":
		return t.renameCheck(ctx, rootAbs, in)
	case "":
		return tools.ToolResult{}, fmt.Errorf("action is required (find_symbol | find_references | rename_check)")
	default:
		return tools.ToolResult{}, fmt.Errorf("unknown action %q (want find_symbol | find_references | rename_check)", in.Action)
	}
}

func (t *ProjectGraphTool) findSymbol(ctx context.Context, root string, in projectGraphInput) (tools.ToolResult, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return tools.ToolResult{}, fmt.Errorf("query is required for find_symbol")
	}
	adapters, msg := t.selectAdapters(in.Language)
	if len(adapters) == 0 {
		return tools.ToolResult{Content: msg}, nil
	}

	var symbols []Symbol
	for _, a := range adapters {
		found, err := a.FindSymbol(ctx, root, query)
		if err != nil {
			continue // a backend that errors (e.g. stub) must not fail the whole query
		}
		symbols = append(symbols, found...)
	}
	if symbols == nil {
		symbols = []Symbol{}
	}
	return jsonResult(symbols)
}

func (t *ProjectGraphTool) findReferences(ctx context.Context, root string, in projectGraphInput) (tools.ToolResult, error) {
	symbol := strings.TrimSpace(in.Symbol)
	if symbol == "" {
		return tools.ToolResult{}, fmt.Errorf("symbol is required for find_references")
	}
	adapters, msg := t.selectAdapters(in.Language)
	if len(adapters) == 0 {
		return tools.ToolResult{Content: msg}, nil
	}

	var refs []Reference
	for _, a := range adapters {
		found, err := a.FindReferences(ctx, root, symbol)
		if err != nil {
			continue
		}
		refs = append(refs, found...)
	}
	if refs == nil {
		refs = []Reference{}
	}
	return jsonResult(refs)
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

var _ tools.Tool = (*ProjectGraphTool)(nil)
