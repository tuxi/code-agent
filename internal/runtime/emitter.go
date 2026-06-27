package runtime

import (
	"code-agent/internal/agent"
	"io"
	"os"
)

// MultiEmitter fans one event out to several emitters — e.g. persist the
// subagent's transcript AND show a live heartbeat from the same stream. Nil
// entries are skipped, so callers need not special-case an absent sink.
type MultiEmitter []agent.Emitter

func (m MultiEmitter) Emit(e agent.Event) {
	for _, em := range m {
		if em != nil {
			em.Emit(e)
		}
	}
}

// McpTraceWriter is where the MCP adapter writes its per-call raw I/O trace.
// Off by default (it would spam normal runs); set CODEAGENT_MCP_DEBUG to enable.
// The startup summary is separate from this and always shown.
func McpTraceWriter() io.Writer {
	if os.Getenv("CODEAGENT_MCP_DEBUG") != "" {
		return os.Stderr
	}
	return io.Discard
}
