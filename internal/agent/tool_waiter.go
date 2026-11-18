package agent

import (
	"bytes"

	"code-agent/internal/assetref"
	"code-agent/internal/preview"
	"code-agent/internal/tools"
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ToolCallResult is the value carried from a client that executed a tool back
// into the agent loop through ClientToolWaiter.
type ToolCallResult struct {
	Subtype string // "result" (v1.1); "progress"|"error"|"cancel" (v1.2+)
	Content string
	Output  json.RawMessage
	Assets  []assets.Ref
	// VisionAssetIDs selects local images from Assets for the model.
	VisionAssetIDs []string
	// Previews is the typed preview producer side-channel delivered by a client.
	Previews []preview.Draft
	IsError  bool
	// Failure is supplied by the runtime broker for lease/connection failures.
	Failure *tools.ToolFailure
}

func (r ToolCallResult) visionInputs() []assets.Ref {
	if len(r.VisionAssetIDs) == 0 {
		return nil
	}
	byID := make(map[string]assets.Ref, len(r.Assets))
	for _, ref := range r.Assets {
		if ref.ID == "" {
			continue
		}
		if _, exists := byID[ref.ID]; exists {
			byID[ref.ID] = assets.Ref{} // ambiguous reference
		} else {
			byID[ref.ID] = ref
		}
	}
	var refs []assets.Ref
	for _, id := range r.VisionAssetIDs {
		if ref := byID[id]; ref.ID != "" {
			refs = append(refs, ref)
			delete(byID, id)
		}
	}
	return refs
}

// ClientToolDef is one client-side tool declaration from a register_tools
// message. Stored per-session and merged into the tool registry at turn-build
// time. Lives in agent (not server) to avoid the conversation→server import
// cycle.
type ClientToolDef struct {
	Name            string          `json:"name"`
	Description     string          `json:"description"`
	InputSchema     json.RawMessage `json:"input_schema"`
	ModelVisibility string          `json:"model_visibility,omitempty"`
	Executor        string          `json:"executor,omitempty"`
}

func (d *ClientToolDef) UnmarshalJSON(data []byte) error {
	type alias ClientToolDef
	var out alias
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return err
	}
	*d = ClientToolDef(out)
	return nil
}

func (d ClientToolDef) Normalized() ClientToolDef {
	if d.ModelVisibility == "" {
		d.ModelVisibility = string(tools.VisibilityDirect)
	}
	if d.Executor == "" {
		d.Executor = string(tools.ExecutorClient)
	}
	return d
}

func ValidateClientToolDefs(defs []ClientToolDef) error {
	seen := make(map[string]bool, len(defs))
	for _, raw := range defs {
		def := raw.Normalized()
		if def.Name == "" {
			return fmt.Errorf("client tool name is required")
		}
		if seen[def.Name] {
			return fmt.Errorf("duplicate client tool name: %s", def.Name)
		}
		seen[def.Name] = true
		if def.ModelVisibility != string(tools.VisibilityDirect) && def.ModelVisibility != string(tools.VisibilityHidden) {
			return fmt.Errorf("client tool %s has unsupported model_visibility %q", def.Name, def.ModelVisibility)
		}
		if def.Executor != string(tools.ExecutorClient) && def.Executor != string(tools.ExecutorFlex) {
			return fmt.Errorf("client tool %s has unsupported executor %q", def.Name, def.Executor)
		}
	}
	return nil
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
