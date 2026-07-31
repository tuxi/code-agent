package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"code-agent/internal/lsp"
)

// ── find_symbol ───────────────────────────────────────────────────────

type findSymbolTool struct{ client *lsp.Client }

func (t *findSymbolTool) Name() string        { return "find_symbol" }
func (t *findSymbolTool) Description() string { return findSymbolDesc }
func (t *findSymbolTool) InputSchema() json.RawMessage {
	return Object(map[string]Property{
		"query": {Type: "string", Description: "Symbol name or partial name to search for"},
	}).JSON()
}

func (t *findSymbolTool) Execute(ctx context.Context, _ ExecutionContext, raw json.RawMessage) (ToolResult, error) {
	var in struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ToolResult{}, fmt.Errorf("find_symbol: %w", err)
	}
	if t.client == nil {
		return ToolResult{Content: "(LSP not available for this project)"}, nil
	}
	if !t.client.Ready() {
		return ToolResult{Content: "(LSP server is still warming up — use grep or read_file for now)"}, nil
	}
	syms, err := t.client.FindSymbol(ctx, strings.TrimSpace(in.Query))
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("find_symbol error: %v", err)}, nil
	}
	if len(syms) == 0 {
		return ToolResult{Content: "(no symbols found)"}, nil
	}
	return ToolResult{Content: formatSymbols(syms)}, nil
}

const findSymbolDesc = "Search for symbols (functions, types, classes) by name using the language server. More precise than grep — returns definitions with exact file:line locations."

// ── find_references ───────────────────────────────────────────────────

type findReferencesTool struct{ client *lsp.Client }

func (t *findReferencesTool) Name() string        { return "find_references" }
func (t *findReferencesTool) Description() string { return findReferencesDesc }
func (t *findReferencesTool) InputSchema() json.RawMessage {
	return Object(map[string]Property{
		"file": {Type: "string", Description: "File path relative to workspace"},
		"line": {Type: "integer", Description: "Line number (1-indexed)"},
		"col":  {Type: "integer", Description: "Column number (1-indexed)"},
	}).JSON()
}

func (t *findReferencesTool) Execute(ctx context.Context, _ ExecutionContext, raw json.RawMessage) (ToolResult, error) {
	var in struct {
		File string `json:"file"`
		Line int    `json:"line"`
		Col  int    `json:"col"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ToolResult{}, fmt.Errorf("find_references: %w", err)
	}
	if t.client == nil {
		return ToolResult{Content: "(LSP not available for this project)"}, nil
	}
	if !t.client.Ready() {
		return ToolResult{Content: "(LSP server is still warming up — use grep or read_file for now)"}, nil
	}
	refs, err := t.client.FindReferences(ctx, in.File, in.Line, in.Col)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("find_references error: %v", err)}, nil
	}
	if len(refs) == 0 {
		return ToolResult{Content: "(no references found)"}, nil
	}
	return ToolResult{Content: formatReferences(refs)}, nil
}

const findReferencesDesc = "Find all references to a symbol at a specific file:line:col. Uses the language server for precise results (not text matching)."

// ── hover ─────────────────────────────────────────────────────────────

type hoverTool struct{ client *lsp.Client }

func (t *hoverTool) Name() string        { return "hover" }
func (t *hoverTool) Description() string { return hoverDesc }
func (t *hoverTool) InputSchema() json.RawMessage {
	return Object(map[string]Property{
		"file": {Type: "string", Description: "File path relative to workspace"},
		"line": {Type: "integer", Description: "Line number (1-indexed)"},
		"col":  {Type: "integer", Description: "Column number (1-indexed)"},
	}).JSON()
}

func (t *hoverTool) Execute(ctx context.Context, _ ExecutionContext, raw json.RawMessage) (ToolResult, error) {
	var in struct {
		File string `json:"file"`
		Line int    `json:"line"`
		Col  int    `json:"col"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return ToolResult{}, fmt.Errorf("hover: %w", err)
	}
	if t.client == nil {
		return ToolResult{Content: "(LSP not available for this project)"}, nil
	}
	if !t.client.Ready() {
		return ToolResult{Content: "(LSP server is still warming up — use grep or read_file for now)"}, nil
	}
	result, err := t.client.Hover(ctx, in.File, in.Line, in.Col)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("hover error: %v", err)}, nil
	}
	if result == nil {
		return ToolResult{Content: "(no type information available)"}, nil
	}
	return ToolResult{Content: result.Content}, nil
}

const hoverDesc = "Get type information and documentation for the symbol at file:line:col. Uses the language server."

// ── Factory ───────────────────────────────────────────────────────────

// NewLSPTools returns the LSP-backed tools for the given workspace.
// When client is nil (LSP not available), the tools return descriptive
// "not available" messages rather than errors.
func NewLSPTools(client *lsp.Client) []Tool {
	return []Tool{
		&findSymbolTool{client: client},
		&findReferencesTool{client: client},
		&hoverTool{client: client},
	}
}

// ── Formatters ────────────────────────────────────────────────────────

func formatSymbols(syms []lsp.Symbol) string {
	var b strings.Builder
	for _, s := range syms {
		fmt.Fprintf(&b, "%s:%d:%d — %s %s\n", s.File, s.Line, s.Col, s.Kind, s.Name)
	}
	return b.String()
}

func formatReferences(refs []lsp.Reference) string {
	var b strings.Builder
	for _, r := range refs {
		fmt.Fprintf(&b, "%s:%d:%d\n", r.File, r.Line, r.Col)
	}
	return b.String()
}
