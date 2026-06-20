// Package mcp consumes external Model Context Protocol servers and exposes each
// of their tools as an ordinary tools.Tool, so remote tools live in the same
// Registry as built-in ones and are gated by the same policy layer. The agent
// loop never learns that a tool is remote — that is the whole point.
//
// v1 scope: stdio transport only; tools/list + tools/call; text content (other
// content kinds become a one-line placeholder); every remote tool is treated as
// side-effecting (so each call is confirmed). Resources, prompts, sampling, and
// exposing our own tools as a server are deliberately out of scope.
package mcp

// ServerConfig describes one external MCP server launched over stdio. Name
// namespaces the server's tools (see wireName/label) and must be unique across
// configured servers.
type ServerConfig struct {
	Name    string            `yaml:"name"`
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
}

// Config is the `mcp:` block of the application config. An empty Servers list
// makes the whole feature a no-op (the Manager connects nothing), so MCP is
// fully opt-in.
type Config struct {
	Servers []ServerConfig `yaml:"servers"`
}
