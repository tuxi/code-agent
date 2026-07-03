package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseJSONEmpty(t *testing.T) {
	for _, in := range []string{"", "   \n", "{}", `{"mcpServers":{}}`} {
		cfg, err := ParseJSON([]byte(in))
		if err != nil {
			t.Fatalf("ParseJSON(%q): %v", in, err)
		}
		if len(cfg.Servers) != 0 {
			t.Fatalf("ParseJSON(%q): expected no servers, got %d", in, len(cfg.Servers))
		}
	}
}

// A Claude-authored `.mcp.json` with all three transports must bind field-for-
// field, come back sorted by name, and normalize the "streamable-http" alias.
func TestParseJSONAllTransports(t *testing.T) {
	t.Setenv("TOK", "secret")
	data := `{
      "mcpServers": {
        "zed": {"type": "stdio", "command": "npx", "args": ["-y", "srv"], "env": {"K": "v"}},
        "api": {"type": "streamable-http", "url": "https://api.example.com/mcp", "headers": {"Authorization": "Bearer ${TOK}"}},
        "evt": {"type": "sse", "url": "https://evt.example.com/sse"}
      }
    }`
	cfg, err := ParseJSON([]byte(data))
	if err != nil {
		t.Fatalf("ParseJSON: %v", err)
	}
	if len(cfg.Servers) != 3 {
		t.Fatalf("expected 3 servers, got %d", len(cfg.Servers))
	}
	// Sorted by name: api, evt, zed.
	if got := []string{cfg.Servers[0].Name, cfg.Servers[1].Name, cfg.Servers[2].Name}; got[0] != "api" || got[1] != "evt" || got[2] != "zed" {
		t.Fatalf("servers not sorted by name: %v", got)
	}
	api := cfg.Servers[0]
	if api.Type != TransportHTTP {
		t.Fatalf("streamable-http should normalize to %q, got %q", TransportHTTP, api.Type)
	}
	if api.Headers["Authorization"] != "Bearer secret" {
		t.Fatalf("header env not expanded: %q", api.Headers["Authorization"])
	}
	zed := cfg.Servers[2]
	if zed.Type != TransportStdio || zed.Command != "npx" || zed.Env["K"] != "v" {
		t.Fatalf("stdio server not parsed: %+v", zed)
	}
}

// Type defaults to stdio when omitted (matches Claude), and unknown per-server
// fields (Claude's alwaysLoad/timeout/oauth) are ignored, not rejected.
func TestParseJSONDefaultsAndUnknownFields(t *testing.T) {
	cfg, err := ParseJSON([]byte(`{"mcpServers":{"s":{"command":"foo","alwaysLoad":true,"timeout":600000}}}`))
	if err != nil {
		t.Fatalf("ParseJSON: %v", err)
	}
	if len(cfg.Servers) != 1 || cfg.Servers[0].Type != TransportStdio {
		t.Fatalf("expected one stdio server, got %+v", cfg.Servers)
	}
}

func TestParseJSONValidation(t *testing.T) {
	cases := map[string]string{
		"stdio without command": `{"mcpServers":{"s":{"type":"stdio"}}}`,
		"http without url":      `{"mcpServers":{"s":{"type":"http"}}}`,
		"sse without url":       `{"mcpServers":{"s":{"type":"sse"}}}`,
		"unsupported type":      `{"mcpServers":{"s":{"type":"ws","url":"wss://x"}}}`,
	}
	for name, data := range cases {
		if _, err := ParseJSON([]byte(data)); err == nil {
			t.Fatalf("%s: expected error, got nil", name)
		}
	}
}

func TestParseJSONEnvDefaultAndMissing(t *testing.T) {
	// ${VAR:-default} falls back when unset.
	cfg, err := ParseJSON([]byte(`{"mcpServers":{"s":{"type":"http","url":"${BASE:-https://d.example.com}/mcp"}}}`))
	if err != nil {
		t.Fatalf("ParseJSON: %v", err)
	}
	if cfg.Servers[0].URL != "https://d.example.com/mcp" {
		t.Fatalf("default not applied: %q", cfg.Servers[0].URL)
	}

	// ${VAR:-default} also falls back when set-but-empty (POSIX `:-`).
	t.Setenv("BASE", "")
	cfg, err = ParseJSON([]byte(`{"mcpServers":{"s":{"type":"http","url":"${BASE:-https://d.example.com}/mcp"}}}`))
	if err != nil {
		t.Fatalf("ParseJSON with empty env: %v", err)
	}
	if cfg.Servers[0].URL != "https://d.example.com/mcp" {
		t.Fatalf("empty env should use default: %q", cfg.Servers[0].URL)
	}

	// ${VAR} with no default and unset is a hard error — a missing credential must
	// not silently become an empty string.
	_, err = ParseJSON([]byte(`{"mcpServers":{"s":{"type":"http","url":"https://x","headers":{"Authorization":"Bearer ${NOPE_UNSET}"}}}}`))
	if err == nil || !strings.Contains(err.Error(), "NOPE_UNSET") {
		t.Fatalf("expected missing-env error naming NOPE_UNSET, got %v", err)
	}
}

// Higher-precedence (later) layers override lower ones by name, wholesale, and
// the result is sorted by name. Project scope wins over user scope.
// RemoteServers keeps only http/sse (no subprocess) — what a sandboxed iOS host
// can connect to; stdio servers are dropped.
func TestRemoteServersFiltersStdio(t *testing.T) {
	in := []ServerConfig{
		{Name: "local", Type: TransportStdio, Command: "bin"},
		{Name: "api", Type: TransportHTTP, URL: "https://x"},
		{Name: "events", Type: TransportSSE, URL: "https://y"},
	}
	got := RemoteServers(in)
	if len(got) != 2 {
		t.Fatalf("expected 2 remote servers (http, sse), got %d: %+v", len(got), got)
	}
	for _, s := range got {
		if s.Type == TransportStdio {
			t.Fatalf("stdio server should be dropped: %+v", s)
		}
	}
	// A stdio-only set on a sandboxed host yields nothing to connect.
	if n := len(RemoteServers([]ServerConfig{{Name: "a", Type: TransportStdio, Command: "b"}})); n != 0 {
		t.Fatalf("stdio-only should filter to empty, got %d", n)
	}
}

func TestMergePrecedence(t *testing.T) {
	user := Config{Servers: []ServerConfig{
		{Name: "shared", Type: TransportStdio, Command: "user-cmd"},
		{Name: "user-only", Type: TransportStdio, Command: "u"},
	}}
	project := Config{Servers: []ServerConfig{
		{Name: "shared", Type: TransportHTTP, URL: "https://project"},
		{Name: "proj-only", Type: TransportStdio, Command: "p"},
	}}
	got := Merge(user, project) // project is higher precedence

	if len(got.Servers) != 3 {
		t.Fatalf("expected 3 merged servers, got %d: %+v", len(got.Servers), got.Servers)
	}
	names := []string{got.Servers[0].Name, got.Servers[1].Name, got.Servers[2].Name}
	if names[0] != "proj-only" || names[1] != "shared" || names[2] != "user-only" {
		t.Fatalf("merged servers not sorted by name: %v", names)
	}
	// The "shared" entry must come wholesale from project (the http one), not a
	// field-merge of the two.
	var shared ServerConfig
	for _, s := range got.Servers {
		if s.Name == "shared" {
			shared = s
		}
	}
	if shared.Type != TransportHTTP || shared.URL != "https://project" || shared.Command != "" {
		t.Fatalf("project scope should win the whole entry, got %+v", shared)
	}
}

// ResolveDesktop layers the scopes so local (.mcp.local.json) overrides project
// (.mcp.json) on a same-name server, and both unique servers survive.
func TestResolveDesktopLocalOverridesProject(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir()) // hermetic: no real ~/.codeagent/mcp.json

	writeFile(t, filepath.Join(root, ".mcp.json"),
		`{"mcpServers":{"shared":{"type":"http","url":"https://project"},"proj-only":{"command":"p"}}}`)
	writeFile(t, filepath.Join(root, ".mcp.local.json"),
		`{"mcpServers":{"shared":{"command":"local-cmd"},"local-only":{"command":"l"}}}`)

	cfg, err := ResolveDesktop(root, false)
	if err != nil {
		t.Fatalf("ResolveDesktop: %v", err)
	}
	if len(cfg.Servers) != 3 {
		t.Fatalf("expected 3 servers (shared, proj-only, local-only), got %+v", cfg.Servers)
	}
	var shared ServerConfig
	for _, s := range cfg.Servers {
		if s.Name == "shared" {
			shared = s
		}
	}
	// local wins the whole "shared" entry: it is the stdio one, not project's http.
	if shared.Type != TransportStdio || shared.Command != "local-cmd" || shared.URL != "" {
		t.Fatalf("local scope should win over project, got %+v", shared)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Importing ~/.claude.json reads only the top-level user-scope mcpServers, skips
// servers that fail validation instead of failing the whole load, and ignores
// unrelated fields (projects, oauth, …).
func TestImportClaudeFileLenient(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	doc := `{
      "oauthAccount": {"emailAddress": "x@y.z"},
      "projects": {"/some/path": {"mcpServers": {"local-only": {"command": "should-be-ignored"}}}},
      "mcpServers": {
        "good": {"type": "http", "url": "https://good.example.com/mcp"},
        "bad":  {"type": "http"}
      }
    }`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, skipped, err := importClaudeFile(path)
	if err != nil {
		t.Fatalf("importClaudeFile: %v", err)
	}
	if len(cfg.Servers) != 1 || cfg.Servers[0].Name != "good" {
		t.Fatalf("expected only the valid user-scope server, got %+v", cfg.Servers)
	}
	if len(skipped) != 1 || skipped[0] != "bad" {
		t.Fatalf("expected the invalid server reported as skipped, got %v", skipped)
	}
}

// A missing ~/.claude.json is not an error — the import is opt-in and best-effort.
func TestImportClaudeFileMissing(t *testing.T) {
	cfg, skipped, err := importClaudeFile(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil || len(cfg.Servers) != 0 || skipped != nil {
		t.Fatalf("missing file should be a clean no-op, got cfg=%+v skipped=%v err=%v", cfg.Servers, skipped, err)
	}
}
