// Package mcp consumes external Model Context Protocol servers and exposes each
// of their tools as an ordinary tools.Tool, so remote tools live in the same
// Registry as built-in ones and are gated by the same policy layer. The agent
// loop never learns that a tool is remote — that is the whole point.
//
// Config format: the on-disk shape is Claude Code's `.mcp.json` verbatim — a
// `mcpServers` object keyed by server name — so a config authored for Claude (or
// copied from the Anthropic connector directory) is consumed without edits. See
// ParseJSON / LoadFile. The tool wire name (mcp__<server>__<tool>) also matches
// Claude, so the whole surface is drop-in compatible.
//
// v1 scope: stdio, streamable-http, and sse transports; tools/list + tools/call;
// text content (other content kinds become a one-line placeholder); every remote
// tool is treated as side-effecting (so each call is confirmed). Resources,
// prompts, sampling, OAuth, tool search, and exposing our own tools as a server
// are deliberately out of scope for now.
package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Transport types accepted in `.mcp.json`. "streamable-http" is accepted as an
// alias for "http" (the MCP spec's name for the transport), matching Claude, and
// is normalized to TransportHTTP at parse time.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
	TransportSSE   = "sse"
)

// ServerConfig describes one external MCP server. Its JSON tags mirror the
// per-server object in Claude Code's `.mcp.json`, so an entry copied from a
// Claude config binds field-for-field. Name is the key in the `mcpServers`
// object, not a JSON field of the object itself; ParseJSON fills it in.
//
// Unknown fields (Claude's `alwaysLoad`, `timeout`, `oauth`, `headersHelper`, …)
// are ignored rather than rejected, so a richer Claude config still loads — we
// simply don't act on the parts we don't yet support.
type ServerConfig struct {
	Name    string            `json:"-"`       // map key in `mcpServers`
	Type    string            `json:"type"`    // stdio (default) | http | sse; "streamable-http" == http
	Command string            `json:"command"` // stdio: executable to launch
	Args    []string          `json:"args"`    // stdio: command arguments
	Env     map[string]string `json:"env"`     // stdio: extra environment, appended to os.Environ
	URL     string            `json:"url"`     // http | sse: endpoint
	Headers map[string]string `json:"headers"` // http | sse: request headers (e.g. Authorization)
}

// Config is the resolved set of MCP servers for a process. Servers is normalized
// (sorted by name) so tool registration order is deterministic. An empty Servers
// list makes the whole feature a no-op, so MCP is fully opt-in.
type Config struct {
	Servers []ServerConfig
}

// file is the on-disk `.mcp.json` document: {"mcpServers": {"<name>": {…}}}.
type file struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`
}

// LoadFile reads and parses a `.mcp.json` file. A missing file is not an error —
// it yields an empty Config, since MCP is opt-in. Any other read or parse error
// is returned so a malformed config fails loudly at startup rather than silently
// dropping servers.
func LoadFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, err
	}
	cfg, err := ParseJSON(data)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// LoadProject reads the project-scope `<root>/.mcp.json`, the file Claude checks
// into version control to share MCP servers with a team. Missing => empty Config.
func LoadProject(root string) (Config, error) {
	return LoadFile(filepath.Join(root, ".mcp.json"))
}

// LoadLocal reads the local-scope `<root>/.mcp.local.json` — private to this
// machine + project (gitignore it), the analog of Claude's local scope. It sits
// next to the shared `.mcp.json` and takes precedence over it, so a developer can
// override or add servers (e.g. a personal token-bearing HTTP server) without
// touching the team file. Missing => empty Config.
func LoadLocal(root string) (Config, error) {
	return LoadFile(filepath.Join(root, ".mcp.local.json"))
}

// LoadUser reads the user-scope `~/.codeagent/mcp.json` — our own home namespace
// (alongside ~/.codeagent/skills), the analog of Claude's user scope. It uses the
// same strict parsing as project scope: a malformed user file fails loudly.
// A missing home directory or file yields an empty Config, since MCP is opt-in.
func LoadUser() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, nil
	}
	return LoadFile(filepath.Join(home, ".codeagent", "mcp.json"))
}

// ResolveDesktop layers the desktop MCP scopes into one config, applying Claude's
// scope precedence (the whole server entry comes from the highest-precedence
// source; fields never merge across scopes):
//
//	local (<root>/.mcp.local.json)  >  project (<root>/.mcp.json)  >  user (~/.codeagent/mcp.json)
//
// When inheritClaude is true it additionally imports the user's existing Claude
// user-scope servers (~/.claude.json) at the LOWEST precedence, so a Claude user
// inherits their servers with zero setup but can still shadow any of them from
// our own files. Claude servers that fail validation are skipped and logged
// rather than failing the whole load (it is a foreign file we don't control).
func ResolveDesktop(root string, inheritClaude bool) (Config, error) {
	var layers []Config // lowest precedence first
	if inheritClaude {
		imported, skipped, err := ImportClaudeUser()
		if err != nil {
			return Config{}, err
		}
		for _, name := range skipped {
			fmt.Fprintf(os.Stderr, "[mcp] skipped inherited Claude server %q (failed validation)\n", name)
		}
		layers = append(layers, imported)
	}
	user, err := LoadUser()
	if err != nil {
		return Config{}, err
	}
	proj, err := LoadProject(root)
	if err != nil {
		return Config{}, err
	}
	local, err := LoadLocal(root)
	if err != nil {
		return Config{}, err
	}
	// Precedence (highest wins on same name): local > project > user (> Claude
	// import), matching Claude. Merge is lowest-first, so append in that order.
	layers = append(layers, user, proj, local)
	return Merge(layers...), nil
}

// Merge combines scope layers, lowest precedence first, returning servers sorted
// by name. On a name collision the higher-precedence (later) layer's entry wins
// wholesale, matching Claude's cross-scope semantics.
func Merge(layers ...Config) Config {
	byName := make(map[string]ServerConfig)
	for _, layer := range layers {
		for _, s := range layer.Servers {
			byName[s.Name] = s
		}
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	servers := make([]ServerConfig, 0, len(names))
	for _, n := range names {
		servers = append(servers, byName[n])
	}
	return Config{Servers: servers}
}

// claudeGlobal is the sliver of Claude Code's ~/.claude.json we read: the
// top-level user-scope mcpServers map. Everything else in that file (per-project
// entries, OAuth tokens, history) is deliberately ignored.
type claudeGlobal struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`
}

// ImportClaudeUser best-effort reads the user's Claude Code user-scope MCP
// servers from ~/.claude.json. It is opt-in (the caller gates it) — we never
// touch Claude's private state file unless asked — and lenient: a server that
// fails validation is skipped (its name returned in skipped) rather than failing
// the load. A missing home or file yields an empty Config.
func ImportClaudeUser() (cfg Config, skipped []string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, nil, nil
	}
	return importClaudeFile(filepath.Join(home, ".claude.json"))
}

func importClaudeFile(path string) (Config, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil, nil
		}
		return Config{}, nil, err
	}
	var g claudeGlobal
	if err := json.Unmarshal(data, &g); err != nil {
		return Config{}, nil, fmt.Errorf("%s: %w", path, err)
	}
	names := make([]string, 0, len(g.MCPServers))
	for name := range g.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)

	var servers []ServerConfig
	var skipped []string
	for _, name := range names {
		s := g.MCPServers[name]
		s.Name = name
		if err := normalize(&s); err != nil {
			skipped = append(skipped, name)
			continue
		}
		servers = append(servers, s)
	}
	return Config{Servers: servers}, skipped, nil
}

// ParseJSON parses a `.mcp.json` document, expands environment-variable
// references, normalizes and validates each server, and returns them sorted by
// name. Empty/whitespace input yields an empty Config.
func ParseJSON(data []byte) (Config, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return Config{}, nil
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return Config{}, fmt.Errorf("parse mcpServers: %w", err)
	}

	names := make([]string, 0, len(f.MCPServers))
	for name := range f.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)

	servers := make([]ServerConfig, 0, len(names))
	for _, name := range names {
		s := f.MCPServers[name]
		s.Name = name
		if err := normalize(&s); err != nil {
			return Config{}, err
		}
		servers = append(servers, s)
	}
	return Config{Servers: servers}, nil
}

// normalize expands env references in place, canonicalizes the transport type,
// and enforces the per-transport required fields. It mutates s.
func normalize(s *ServerConfig) error {
	if s.Name == "" {
		return errors.New("mcp: server name is required")
	}
	if err := expandServer(s); err != nil {
		return fmt.Errorf("mcp server %q: %w", s.Name, err)
	}

	switch s.Type {
	case "", TransportStdio:
		s.Type = TransportStdio
		if s.Command == "" {
			return fmt.Errorf("mcp server %q: stdio transport requires a command", s.Name)
		}
	case TransportHTTP, "streamable-http":
		s.Type = TransportHTTP
		if s.URL == "" {
			return fmt.Errorf("mcp server %q: http transport requires a url", s.Name)
		}
	case TransportSSE:
		if s.URL == "" {
			return fmt.Errorf("mcp server %q: sse transport requires a url", s.Name)
		}
	default:
		return fmt.Errorf("mcp server %q: unsupported transport type %q (want stdio, http, or sse)", s.Name, s.Type)
	}
	return nil
}

// expandServer applies environment-variable expansion to every field that
// Claude expands (command, args, env, url, headers).
func expandServer(s *ServerConfig) error {
	var err error
	set := func(dst *string) {
		if err != nil {
			return
		}
		*dst, err = expand(*dst)
	}
	set(&s.Command)
	for i := range s.Args {
		set(&s.Args[i])
	}
	set(&s.URL)
	expandMap(s.Env, set)
	expandMap(s.Headers, set)
	return err
}

func expandMap(m map[string]string, set func(*string)) {
	// Sort keys so a failure reports the same variable deterministically.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := m[k]
		set(&v)
		m[k] = v
	}
}

// envRef matches ${VAR} and ${VAR:-default}, the two forms Claude supports.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// expand replaces ${VAR} / ${VAR:-default} references, matching Claude's
// semantics: ${VAR} fails if VAR is unset (a missing credential must not become
// a silent empty string); ${VAR:-default} falls back to default when VAR is
// unset or empty (POSIX `:-`).
func expand(s string) (string, error) {
	if !strings.Contains(s, "${") {
		return s, nil
	}
	var missing string
	out := envRef.ReplaceAllStringFunc(s, func(m string) string {
		sub := envRef.FindStringSubmatch(m)
		name, hasDefault, def := sub[1], sub[2] != "", sub[3]
		val, ok := os.LookupEnv(name)
		if hasDefault {
			if ok && val != "" {
				return val
			}
			return def
		}
		if ok {
			return val
		}
		if missing == "" {
			missing = name
		}
		return ""
	})
	if missing != "" {
		return "", fmt.Errorf("environment variable %q is not set and has no default", missing)
	}
	return out, nil
}
