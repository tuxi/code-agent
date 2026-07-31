// Package lsp is the Language Server Protocol client for code-agent.
//
// Two modes:
//   - CLI mode (gopls): spawn per-call subprocess. Used when the server
//     supports single-shot CLI subcommands (gopls workspace_symbol, etc.).
//   - Server mode (sourcekit-lsp, ts-server, rust-analyzer, pyright):
//     persistent JSON-RPC over stdin/stdout. Used when the server only
//     speaks the full LSP protocol.
//
// Both modes implement the same Client API — the agent tools don't care.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// ── Types ─────────────────────────────────────────────────────────────

type Symbol struct {
	Name string
	Kind string
	File string
	Line int // 1-indexed
	Col  int // 1-indexed
}

type Reference struct {
	File string
	Line int
	Col  int
}

type HoverResult struct {
	Content string
	File    string
	Line    int
	Col     int
}

type clientMode int

const (
	modeCLI    clientMode = iota // per-call subprocess
	modeServer                   // persistent JSON-RPC
)

// ── Client ────────────────────────────────────────────────────────────

type Client struct {
	command string
	args    []string
	mode    clientMode
	root    string
	ready   bool

	// Server-mode state.
	mu   sync.Mutex
	cmd  *exec.Cmd
	r    *bufio.Reader // stdout
	w    io.Writer     // stdin
	seq  int
	done chan struct{} // closed when the server process exits
}

// DetectLanguage guesses the primary language from marker files at root.
func DetectLanguage(root string) (*Client, error) {
	check := func(rel string) bool {
		_, err := os.Stat(filepath.Join(root, rel))
		return err == nil
	}
	hasXcode := func() bool {
		m, _ := filepath.Glob(filepath.Join(root, "*.xcodeproj"))
		return len(m) > 0
	}
	switch {
	case check("go.mod"):
		// gopls supports CLI subcommands — use CLI mode.
		return &Client{command: "gopls", mode: modeCLI}, nil
	case check("Package.swift") || hasXcode():
		// sourcekit-lsp only speaks JSON-RPC — use server mode.
		return &Client{command: "sourcekit-lsp", mode: modeServer}, nil
	case check("package.json"):
		return &Client{command: "npx", args: []string{"-y", "typescript-language-server", "--stdio"}, mode: modeServer}, nil
	case check("Cargo.toml"):
		return &Client{command: "rust-analyzer", mode: modeServer}, nil
	case check("pyproject.toml") || check("setup.py") || check("requirements.txt"):
		return &Client{command: "pyright", args: []string{"--stdio"}, mode: modeServer}, nil
	}
	return nil, fmt.Errorf("lsp: no recognised language found in %s", root)
}

// Language returns the server command name for diagnostics.
func (c *Client) Language() string { return c.command }

// Ready reports whether the client has completed initialization and can
// serve queries. Before ready, FindSymbol/FindReferences/Hover return
// descriptive "not ready" messages rather than errors.
func (c *Client) Ready() bool { return c.ready }

// Initialize checks the binary is installed. For CLI mode it's a no-op
// otherwise. For server mode it starts the persistent process and runs the
// LSP initialize handshake.
//
// Initialize is safe to call synchronously (blocks until the handshake
// completes or times out) or asynchronously (call it in a goroutine;
// Ready() becomes true once the handshake succeeds).
func (c *Client) Initialize(ctx context.Context, root string) error {
	c.root = root
	if _, err := exec.LookPath(c.command); err != nil {
		return fmt.Errorf("lsp: %s not installed (%w)", c.command, err)
	}
	if c.mode == modeCLI {
		c.ready = true
		return nil
	}
	return c.startServer(ctx)
}

func (c *Client) startServer(ctx context.Context) error {
	args := append([]string{}, c.args...)
	c.cmd = exec.CommandContext(ctx, c.command, args...)
	c.cmd.Dir = c.root

	stdin, err := c.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("lsp: stdin pipe: %w", err)
	}
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("lsp: stdout pipe: %w", err)
	}
	stderr, err := c.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("lsp: stderr pipe: %w", err)
	}
	// Drain stderr so the server never blocks on a full pipe buffer.
	go func() { io.ReadAll(stderr) }()

	if err := c.cmd.Start(); err != nil {
		return fmt.Errorf("lsp: start %s: %w", c.command, err)
	}
	c.w = stdin
	c.r = bufio.NewReader(stdout)
	c.done = make(chan struct{})

	// Monitor exit.
	go func() {
		c.cmd.Wait()
		close(c.done)
	}()

	// Initialize handshake.
	rootURI := "file://" + c.root
	if !strings.HasSuffix(rootURI, "/") {
		rootURI += "/"
	}

	_, err = c.call(ctx, "initialize", map[string]any{
		"processId": nil,
		"rootUri":   rootURI,
		"capabilities": map[string]any{
			"workspace": map[string]any{
				"symbol": map[string]any{"dynamicRegistration": false},
			},
			"textDocument": map[string]any{
				"references": map[string]any{"dynamicRegistration": false},
				"hover":      map[string]any{"dynamicRegistration": false},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("lsp: initialize: %w", err)
	}

	// Send initialized notification (no response expected).
	c.notify(ctx, "initialized", map[string]any{})
	c.ready = true
	return nil
}

// Close sends shutdown + exit for server mode, or no-op for CLI mode.
func (c *Client) Close() error {
	if c.mode == modeCLI || c.cmd == nil {
		return nil
	}
	// Best-effort shutdown.
	c.mu.Lock()
	defer c.mu.Unlock()
	c.call(context.Background(), "shutdown", nil)
	c.notify(context.Background(), "exit", nil)
	// Give the process a moment, then kill.
	select {
	case <-c.done:
	default:
		c.cmd.Process.Kill()
	}
	return nil
}

// ── Query methods ─────────────────────────────────────────────────────

func (c *Client) FindSymbol(ctx context.Context, query string) ([]Symbol, error) {
	if !c.ready {
		return nil, fmt.Errorf("lsp: client not initialized")
	}
	if c.mode == modeCLI {
		return c.findSymbolCLI(ctx, query)
	}
	return c.findSymbolServer(ctx, query)
}

func (c *Client) FindReferences(ctx context.Context, file string, line, col int) ([]Reference, error) {
	if !c.ready {
		return nil, fmt.Errorf("lsp: client not initialized")
	}
	if c.mode == modeCLI {
		return c.findReferencesCLI(ctx, file, line, col)
	}
	return c.findReferencesServer(ctx, file, line, col)
}

func (c *Client) Hover(ctx context.Context, file string, line, col int) (*HoverResult, error) {
	if !c.ready {
		return nil, fmt.Errorf("lsp: client not initialized")
	}
	if c.mode == modeCLI {
		return c.hoverCLI(ctx, file, line, col)
	}
	return c.hoverServer(ctx, file, line, col)
}

// ── CLI mode (gopls) ─────────────────────────────────────────────────

func (c *Client) run(ctx context.Context, subcommand string, args ...string) ([]byte, error) {
	allArgs := append(append([]string{}, c.args...), subcommand)
	allArgs = append(allArgs, args...)
	cmd := exec.CommandContext(ctx, c.command, allArgs...)
	cmd.Dir = c.root
	return cmd.Output()
}

func (c *Client) findSymbolCLI(ctx context.Context, query string) ([]Symbol, error) {
	out, err := c.run(ctx, "workspace_symbol", query)
	if err != nil {
		return nil, err
	}
	return parseTextSymbols(string(out), c.root), nil
}

func (c *Client) findReferencesCLI(ctx context.Context, file string, line, col int) ([]Reference, error) {
	pos := fmt.Sprintf("%s:%d:%d", filepath.Join(c.root, file), line, col)
	out, err := c.run(ctx, "references", pos)
	if err != nil {
		return nil, err
	}
	return parseTextReferences(string(out), c.root), nil
}

func (c *Client) hoverCLI(ctx context.Context, file string, line, col int) (*HoverResult, error) {
	// gopls v0.23 removed 'hover' CLI subcommand. Use 'signature' for call-site
	// type info, fall back to the position itself for definition sites.
	pos := fmt.Sprintf("%s:%d:%d", filepath.Join(c.root, file), line, col)
	out, err := c.run(ctx, "signature", pos)
	if err != nil {
		// signature fails at definition sites ("not a function"). Return the
		// position — the agent can read the file for context.
		return &HoverResult{
			Content: fmt.Sprintf("%s:%d:%d (read the file for full context)", file, line, col),
			File:    file, Line: line, Col: col,
		}, nil
	}
	return &HoverResult{
		Content: strings.TrimSpace(string(out)),
		File:    file, Line: line, Col: col,
	}, nil
}

// ── Server mode (JSON-RPC) ───────────────────────────────────────────

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Check server health before writing.
	select {
	case <-c.done:
		return nil, fmt.Errorf("lsp: %s server has exited", c.command)
	default:
	}

	c.seq++
	id := c.seq

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')
	if _, err := c.w.Write(body); err != nil {
		return nil, fmt.Errorf("lsp: write %s: %w", method, err)
	}

	// Read until we get a response with the matching id.
	for {
		line, err := c.r.ReadBytes('\n')
		if err != nil {
			// Check if the server crashed.
			select {
			case <-c.done:
				return nil, fmt.Errorf("lsp: %s server exited during request (method=%s)", c.command, method)
			default:
			}
			return nil, fmt.Errorf("lsp: read %s: %w", method, err)
		}
		var resp struct {
			ID     int              `json:"id"`
			Result json.RawMessage  `json:"result"`
			Error  *json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(line, &resp); err != nil {
			continue
		}
		if resp.ID != id {
			continue // skip notifications or responses for other requests
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("lsp: %s error: %s", method, string(*resp.Error))
		}
		return resp.Result, nil
	}
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return err
	}
	body = append(body, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.w.Write(body)
	return err
}

func (c *Client) findSymbolServer(ctx context.Context, query string) ([]Symbol, error) {
	// Check if the server process has exited (e.g. sourcekit-lsp crashing on
	// an incomplete workspace).
	select {
	case <-c.done:
		return nil, fmt.Errorf("lsp: %s server exited unexpectedly — the workspace may not have enough structure for indexing", c.command)
	default:
	}
	result, err := c.call(ctx, "workspace/symbol", map[string]any{"query": query})
	if err != nil {
		return nil, err
	}
	if result == nil || string(result) == "null" {
		return nil, nil
	}
	var raw []wsSymbolResult
	if err := json.Unmarshal(result, &raw); err != nil {
		return nil, fmt.Errorf("lsp: parse workspace/symbol: %w", err)
	}
	var syms []Symbol
	for _, r := range raw {
		rel := filepathRel(c.root, uriToPath(r.Location.URI))
		syms = append(syms, Symbol{
			Name: r.Name,
			Kind: symbolKind(r.Kind),
			File: rel,
			Line: r.Location.Range.Start.Line + 1,
			Col:  r.Location.Range.Start.Character + 1,
		})
	}
	return syms, nil
}

func (c *Client) findReferencesServer(ctx context.Context, file string, line, col int) ([]Reference, error) {
	uri := "file://" + filepath.Join(c.root, file)
	result, err := c.call(ctx, "textDocument/references", map[string]any{
		"textDocument": map[string]string{"uri": uri},
		"position":     map[string]int{"line": line - 1, "character": col - 1},
		"context":      map[string]bool{"includeDeclaration": false},
	})
	if err != nil {
		return nil, err
	}
	if result == nil || string(result) == "null" {
		return nil, nil
	}
	var raw []refResult
	if err := json.Unmarshal(result, &raw); err != nil {
		return nil, fmt.Errorf("lsp: parse textDocument/references: %w", err)
	}
	var refs []Reference
	for _, r := range raw {
		rel := filepathRel(c.root, uriToPath(r.URI))
		refs = append(refs, Reference{
			File: rel,
			Line: r.Range.Start.Line + 1,
			Col:  r.Range.Start.Character + 1,
		})
	}
	return refs, nil
}

func (c *Client) hoverServer(ctx context.Context, file string, line, col int) (*HoverResult, error) {
	uri := "file://" + filepath.Join(c.root, file)
	result, err := c.call(ctx, "textDocument/hover", map[string]any{
		"textDocument": map[string]string{"uri": uri},
		"position":     map[string]int{"line": line - 1, "character": col - 1},
	})
	if err != nil {
		return nil, err
	}
	if result == nil || string(result) == "null" {
		return nil, nil
	}
	var hover struct {
		Contents any `json:"contents"`
	}
	if err := json.Unmarshal(result, &hover); err != nil {
		return nil, fmt.Errorf("lsp: parse textDocument/hover: %w", err)
	}
	content := formatHoverContent(hover.Contents)
	if content == "" {
		return nil, nil
	}
	return &HoverResult{Content: content, File: file, Line: line, Col: col}, nil
}

func formatHoverContent(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case map[string]any:
		if s, ok := v["value"].(string); ok {
			return s
		}
	case []any:
		var parts []string
		for _, item := range v {
			switch it := item.(type) {
			case string:
				parts = append(parts, it)
			case map[string]any:
				if s, ok := it["value"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return fmt.Sprintf("%v", c)
}

// ── JSON-RPC wire types (server mode only) ───────────────────────────

type wsSymbolResult struct {
	Name     string       `json:"name"`
	Kind     int          `json:"kind"`
	Location jsonLocation `json:"location"`
}

type jsonLocation struct {
	URI   string    `json:"uri"`
	Range jsonRange `json:"range"`
}

type jsonRange struct {
	Start jsonPosition `json:"start"`
	End   jsonPosition `json:"end"`
}

type jsonPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type refResult struct {
	URI   string    `json:"uri"`
	Range jsonRange `json:"range"`
}

// ── CLI text parsers (gopls v0.23+ plain-text format) ─────────────────
//
// gopls workspace_symbol output (one symbol per line):
//
//	/path/file.go:34:6-19 WirePlanTools Function
//	/path/file.go:36:6-10 build Function
//
// gopls references output (one reference per line):
//
//	/path/file.go:136:175-180

func parseTextSymbols(out string, root string) []Symbol {
	var syms []Symbol
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		file, lineNum, col, name, kind := parseTextLine(line)
		if name == "" {
			continue
		}
		syms = append(syms, Symbol{
			Name: name,
			Kind: kind,
			File: filepathRel(root, file),
			Line: lineNum,
			Col:  col,
		})
	}
	return syms
}

func parseTextReferences(out string, root string) []Reference {
	var refs []Reference
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		file, lineNum, col, _, _ := parseTextLine(line)
		if file == "" {
			continue
		}
		refs = append(refs, Reference{
			File: filepathRel(root, file),
			Line: lineNum,
			Col:  col,
		})
	}
	return refs
}

// parseTextLine parses gopls CLI text format: "file:line:col-endcol Name Kind"
func parseTextLine(line string) (file string, lineNum, col int, name, kind string) {
	// Split "file:line:col-endcol" from "Name Kind"
	space := strings.IndexByte(line, ' ')
	if space < 0 {
		// Just "file:line:col-endcol" (references format, no name/kind)
		file, lineNum, col = parseTextPos(line)
		return file, lineNum, col, "", ""
	}
	posPart := line[:space]
	rest := strings.TrimSpace(line[space+1:])

	file, lineNum, col = parseTextPos(posPart)

	// Rest is "Name Kind" or just "Name"
	parts := strings.Fields(rest)
	if len(parts) >= 1 {
		name = parts[0]
	}
	if len(parts) >= 2 {
		kind = strings.ToLower(parts[1])
	}
	return
}

// parseTextPos parses "file:line:col-endcol" or "file:line:col"
func parseTextPos(s string) (file string, line, col int) {
	// Find the last ':' — the line:col-endcol part.
	lastColon := strings.LastIndexByte(s, ':')
	if lastColon < 0 {
		return s, 0, 0
	}
	file = s[:lastColon]
	rest := s[lastColon+1:]

	// "line:col-endcol" or "line:col"
	hyphen := strings.IndexByte(rest, '-')
	if hyphen >= 0 {
		rest = rest[:hyphen]
	}
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) >= 1 {
		line, _ = strconv.Atoi(parts[0])
	}
	if len(parts) >= 2 {
		col, _ = strconv.Atoi(parts[1])
	}
	return
}

// ── Utilities ────────────────────────────────────────────────────────

func filepathRel(root, absPath string) string {
	rel, err := filepath.Rel(root, absPath)
	if err != nil {
		return absPath
	}
	return filepath.ToSlash(rel)
}

func uriToPath(uri string) string {
	return strings.TrimPrefix(strings.TrimPrefix(uri, "file://"), "file:")
}

func symbolKind(k int) string {
	kinds := map[int]string{
		1: "file", 2: "module", 3: "namespace", 4: "package", 5: "class",
		6: "method", 7: "property", 8: "field", 9: "constructor", 10: "enum",
		11: "interface", 12: "function", 13: "variable", 14: "constant",
		15: "string", 16: "number", 17: "boolean", 18: "array",
	}
	if s, ok := kinds[k]; ok {
		return s
	}
	return "symbol"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
