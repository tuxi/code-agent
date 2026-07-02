package agent

import (
	"code-agent/internal/assetref"
	"context"
	"encoding/json"
	"time"
)

// ToolCallResult is the value carried from a client that executed a tool back
// into the agent loop through ClientToolWaiter.
type ToolCallResult struct {
	Subtype string // "result" (v1.1); "progress"|"error"|"cancel" (v1.2+)
	Content string
	Output  json.RawMessage
	Assets  []assets.Ref
	IsError bool
}

// ClientToolDef is one client-side tool declaration from a register_tools
// message. Stored per-session and merged into the tool registry at turn-build
// time. Lives in agent (not server) to avoid the conversation→server import
// cycle.
type ClientToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ClientToolWaiter manages the lifecycle of a tool call delegated to a client.
// Wait blocks the turn goroutine until the client delivers a result via Deliver,
// the lease times out, or the context is cancelled. The shape is identical to
// Approver: Wait runs on the turn goroutine, Deliver runs on the WS read-loop
// goroutine, and a per-request channel bridges them.
//
// See docs/protocols/agent-wire-v1.1-client-tool-execution.md §4.
type ClientToolWaiter interface {
	// Wait blocks until the client delivers a result for callID, the lease
	// expires, or ctx is done.
	Wait(ctx context.Context, callID string, leaseTimeout time.Duration) (ToolCallResult, error)

	// Deliver injects a client result into the blocked Wait call. Unknown callID
	// is silently dropped (already completed / timed out / never registered).
	Deliver(callID string, result ToolCallResult)

	// CancelAll wakes every pending Wait call on disconnect so the agent loop is
	// never left permanently blocked.
	CancelAll()
}
