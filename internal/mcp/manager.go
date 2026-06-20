package mcp

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"code-agent/internal/tools"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// connectTimeout bounds the startup handshake and tool discovery for one server.
// Without it, a server that never speaks MCP over stdio (or an `npx` first-run
// still downloading its package) freezes the whole agent at launch. On timeout
// the server is skipped and flagged in the summary. Generous on purpose: a cold
// `npx` can legitimately take many seconds.
const connectTimeout = 30 * time.Second

// Manager owns the lifecycle of every configured MCP server connection for the
// process: it spawns each server over stdio, discovers its tools, wraps them as
// tools.Tool, and tears the subprocesses down on Close. It is the single owner
// of MCP sessions — nothing else holds one — so shutdown is exactly one call.
type Manager struct {
	trace    io.Writer // per-call raw I/O trace; io.Discard unless debugging
	sessions []*mcpsdk.ClientSession
	tools    []tools.Tool
	report   []ServerStatus
}

// ServerStatus is the connect outcome for one server, for the startup summary.
type ServerStatus struct {
	Name      string
	Connected bool
	ToolCount int
	Err       error
}

// NewManager returns a Manager that writes per-call raw I/O traces to trace
// (pass io.Discard to silence them; the startup summary is separate and always
// available via Summary).
func NewManager(trace io.Writer) *Manager {
	if trace == nil {
		trace = io.Discard
	}
	return &Manager{trace: trace}
}

// Connect starts every configured server and discovers its tools. A server that
// fails to start or list is skipped and recorded in the report (surfaced loudly
// by Summary) rather than aborting the whole agent — one broken MCP server must
// not take the agent down with it. Connect is sequential: the server count is
// small and this keeps startup ordering and the summary legible.
func (m *Manager) Connect(ctx context.Context, servers []ServerConfig) error {
	for _, s := range servers {
		st := ServerStatus{Name: s.Name}
		session, n, err := m.connectOne(ctx, s)
		if err != nil {
			st.Err = err
			m.report = append(m.report, st)
			continue
		}
		m.sessions = append(m.sessions, session)
		st.Connected, st.ToolCount = true, n
		m.report = append(m.report, st)
	}
	return nil
}

func (m *Manager) connectOne(ctx context.Context, s ServerConfig) (*mcpsdk.ClientSession, int, error) {
	if s.Command == "" {
		return nil, 0, fmt.Errorf("empty command")
	}
	cmd := exec.Command(s.Command, s.Args...)
	cmd.Env = os.Environ()
	for k, v := range s.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	// Bound the handshake and discovery. cctx governs only these startup calls;
	// once Connect returns, the SDK drives the session from its own context, so
	// the deferred cancel cannot tear down a session that did come up.
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "code-agent", Version: "0.1.0"}, nil)
	session, err := client.Connect(cctx, &mcpsdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		killProcess(cmd) // best-effort: reap a child the SDK may have left running
		return nil, 0, startupError(cctx, err, s.Command)
	}

	discovered, err := m.discover(cctx, session, s.Name)
	if err != nil {
		_ = session.Close()
		return nil, 0, startupError(cctx, err, s.Command)
	}
	m.tools = append(m.tools, discovered...)
	return session, len(discovered), nil
}

// startupError turns a deadline into an actionable message; other errors pass
// through unchanged.
func startupError(cctx context.Context, err error, command string) error {
	if cctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("timed out after %s (is %q installed and speaking MCP over stdio?)", connectTimeout, command)
	}
	return err
}

// killProcess best-effort terminates a spawned server. Used only on the connect
// error path; a healthy session is closed via ClientSession.Close instead.
func killProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// discover pages through tools/list and wraps each remote tool.
func (m *Manager) discover(ctx context.Context, session *mcpsdk.ClientSession, server string) ([]tools.Tool, error) {
	var out []tools.Tool
	params := &mcpsdk.ListToolsParams{}
	for {
		res, err := session.ListTools(ctx, params)
		if err != nil {
			return nil, err
		}
		for _, rt := range res.Tools {
			out = append(out, &remoteTool{
				caller:     session,
				server:     server,
				remoteName: rt.Name,
				wireName:   wireName(server, rt.Name),
				label:      label(server, rt.Name),
				desc:       rt.Description,
				schema:     marshalSchema(rt.InputSchema),
				log:        m.trace,
			})
		}
		if res.NextCursor == "" {
			break
		}
		params.Cursor = res.NextCursor
	}
	return out, nil
}

// Tools returns the wrapped remote tools discovered across all connected
// servers, in discovery order.
func (m *Manager) Tools() []tools.Tool { return m.tools }

// Report returns per-server connect outcomes.
func (m *Manager) Report() []ServerStatus { return m.report }

// Close terminates every server subprocess. It is safe to call when nothing
// connected, and clears the session list so a second call is a no-op.
func (m *Manager) Close() error {
	var firstErr error
	for _, s := range m.sessions {
		if err := s.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	m.sessions = nil
	return firstErr
}

// Summary renders the startup report: connected servers with tool counts, failed
// servers flagged loudly, and the full list of registered tool names — so a
// missing tool is never a silent mystery. Returns "" when no servers were
// configured.
func (m *Manager) Summary() string {
	if len(m.report) == 0 {
		return ""
	}
	var b strings.Builder
	for _, st := range m.report {
		if st.Connected {
			fmt.Fprintf(&b, "[mcp] %-12s connected — %d tools\n", st.Name, st.ToolCount)
		} else {
			fmt.Fprintf(&b, "[mcp] %-12s FAILED: %v — skipped\n", st.Name, st.Err)
		}
	}
	names := make([]string, 0, len(m.tools))
	for _, t := range m.tools {
		if rt, ok := t.(*remoteTool); ok {
			names = append(names, rt.label)
		}
	}
	sort.Strings(names)
	if len(names) > 0 {
		fmt.Fprintf(&b, "[mcp] tools registered: %s\n", strings.Join(names, ", "))
	}
	return strings.TrimRight(b.String(), "\n")
}
