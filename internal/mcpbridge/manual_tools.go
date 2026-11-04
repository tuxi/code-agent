package mcpbridge

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type manualTarget struct {
	BindingID string `json:"binding_id"`
}
type manualSendInput struct {
	manualTarget
	SendInput
}
type manualReadInput struct {
	manualTarget
	ReadInput
}
type manualUpdateInput struct {
	manualTarget
	UpdateInput
}
type manualHandoffInput struct {
	manualTarget
	HandoffInput
}
type manualArtifactInput struct {
	manualTarget
	ArtifactInput
}
type manualCancelInput struct {
	manualTarget
	CancelInput
}
type manualAdmissionInput struct {
	manualTarget
	AdmissionInput
}

// Enrich new-profile responses without changing the legacy response structs or
// rounding persistent cursor numbers through float64.
func manualOutput(b *manualBinding, value any) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil, err
	}
	for key, value := range map[string]string{"binding_id": b.id, "server_id": b.c.binding.verifiedServerID, "store_id": b.c.binding.verifiedStoreID, "baseline_commit": b.c.baseline} {
		out[key], _ = json.Marshal(value)
	}
	return out, nil
}

func manualCall[T any](ctx context.Context, s *ManualService, binding, session string, fn func(*Client) (T, error)) (*mcp.CallToolResult, any, error) {
	if session == "" {
		return nil, nil, bridgeError("session_mismatch", nil)
	}
	b, release, err := s.acquire(ctx, binding, session)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	value, err := fn(b.c)
	if err != nil {
		return nil, nil, err
	}
	out, err := manualOutput(b, value)
	return nil, out, err
}

func (s *ManualService) send(ctx context.Context, in manualSendInput) (*mcp.CallToolResult, any, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return manualCall(ctx, s, in.BindingID, in.SessionID, func(c *Client) (Receipt, error) {
		c.sendMu.Lock()
		var previous *submission
		if stored := c.submissions[in.RequestID]; stored != nil {
			copy := *stored
			previous = &copy
		}
		c.sendMu.Unlock()
		if previous != nil {
			if previous.receipt == nil {
				return Receipt{}, admissionError("admission_unknown", c.id, in.RequestID, nil)
			}
			return c.sendWithExpectations(ctx, in.SessionID, in.RequestID, in.Message, in.Model, in.Development, true)
		}
		probe, err := s.probe(ctx, false)
		if err != nil {
			return Receipt{}, err
		}
		defer probe.Close()
		if err := s.checkOtherWriters(ctx, probe, c.id); err != nil {
			return Receipt{}, err
		}
		if err := probe.sessionIdle(ctx, c.id); err != nil {
			return Receipt{}, err
		}
		head, cut, err := gitOutput(ctx, c.root, "rev-parse", "HEAD")
		if err != nil || cut || strings.TrimSpace(head) != c.baseline {
			return Receipt{}, bridgeError("baseline_changed", err)
		}
		return c.sendWithExpectations(ctx, in.SessionID, in.RequestID, in.Message, in.Model, in.Development, true)
	})
}

// ManualServer keeps a stable native MCP catalog even when the Runtime is
// unavailable. All task operations require an explicit immutable handle.
func ManualServer(s *ManualService) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "codeagent-manual", Version: "1.0.0-experimental"}, &mcp.ServerOptions{Instructions: "One project workspace; no implicit current Session. Check connection_info, then create_session or bind_session with exact Runtime/Store/baseline. Retain binding_id and send stable request_id. After durable admission END THIS RESPONSE: no polling or wakeups. User observes/approves in Talkify and returns; get_handoff once for the exact binding/Session/Turn. Missing receipt: lookup_admission, never a new request_id. Expired grants require user authorization and explicit binding replacement; never retarget. Manual evidence is mutable, not acceptance. No approval tool, dependency installation, commit or recursive delegation."})
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mcp.AddTool(server, &mcp.Tool{Name: "codeagent_connection_info", Description: "Read service identity without creating a Session. Optional binding_id checks only that immutable binding. After the user explicitly renews local access, reconnect={server_id,store_id,workspace_path} reauthenticates the previously verified service without restarting MCP; retain the original identity values. Cannot combine reconnect with binding_id. Existing Session bindings require bind_session with replace_binding_id; uncertain requests stay retained. Invalid grant/offline Runtime is a tool error; never expands authorization.", Annotations: ro}, func(ctx context.Context, _ *mcp.CallToolRequest, in manualConnectionInput) (*mcp.CallToolResult, any, error) {
		if in.Reconnect != nil {
			if in.BindingID != "" {
				return nil, nil, bridgeError("invalid_tool_arguments", nil)
			}
			v, err := s.Reconnect(ctx, *in.Reconnect)
			return nil, v, err
		}
		v, err := s.ConnectionInfo(ctx, in.BindingID)
		return nil, v, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "codeagent_list_models", Description: "Read configured Runtime model catalog; no model API call or task binding. Omitted send model inherits Runtime defaults.", Annotations: ro}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		c, err := s.probe(ctx, false)
		if err != nil {
			return nil, nil, err
		}
		defer c.Close()
		v, err := c.ListModels(ctx)
		return nil, v, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "codeagent_create_session", Description: "Explicitly create a Session at the verified project HEAD. Requires server_id/store_id/baseline_commit from connection_info. Returns immutable binding_id; no Turn dispatched. Lost creation receipt is unknown, not safe to repeat after restart."}, func(ctx context.Context, _ *mcp.CallToolRequest, in ManualCreateInput) (*mcp.CallToolResult, any, error) {
		v, err := s.Create(ctx, in)
		return nil, v, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "codeagent_bind_session", Description: "Explicitly bind an existing exact Session/baseline/Runtime/Store. Observation only; does not resume execution or approvals. replace_binding_id explicitly revalidates a prior binding after user renews authorization; it cannot migrate to another Store or clear uncertain submissions."}, func(ctx context.Context, _ *mcp.CallToolRequest, in ManualBindInput) (*mcp.CallToolResult, any, error) {
		v, err := s.Bind(ctx, in)
		return nil, v, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "codeagent_send_to_session", Description: "Delegate to exact binding_id/session_id. Optional model; declare development.validation_commands/manual_checks and instruct Executor to run checks. Requires durable_validation_contract_v1 before sending development; never falls back to local metadata. Durable receipt is admission, not completion. END THIS RESPONSE after receipt; user watches Talkify. No wait/read polling. Missing receipt: lookup_admission first."}, s.sendTool)
	mcp.AddTool(server, &mcp.Tool{Name: "codeagent_lookup_admission", Description: "Read durable admission for the original request_id on its original binding/Session. unknown does not mean unsubmitted. Does not send or replay anything.", Annotations: ro}, func(ctx context.Context, _ *mcp.CallToolRequest, in manualAdmissionInput) (*mcp.CallToolResult, any, error) {
		s.opMu.Lock()
		defer s.opMu.Unlock()
		return manualCall(ctx, s, in.BindingID, in.SessionID, func(c *Client) (Admission, error) {
			v, err := c.LookupAdmission(ctx, in.SessionID, in.RequestID)
			if err == nil && v.Receipt != nil {
				c.sendMu.Lock()
				if previous := c.submissions[in.RequestID]; previous != nil {
					previous.receipt = v.Receipt
				}
				c.sendMu.Unlock()
			}
			return v, err
		})
	})
	mcp.AddTool(server, &mcp.Tool{Name: "codeagent_read_session", Description: "Read durable exact-Turn evidence and human attention references, only when needed for review or diagnosis.", Annotations: ro}, func(ctx context.Context, _ *mcp.CallToolRequest, in manualReadInput) (*mcp.CallToolResult, any, error) {
		return manualCall(ctx, s, in.BindingID, in.SessionID, func(c *Client) (ReadResult, error) { return c.Read(ctx, in.SessionID, in.TurnID, in.Cursor) })
	})
	mcp.AddTool(server, &mcp.Tool{Name: "codeagent_wait_for_update", Description: "Only if user explicitly requests waiting. Quiet wait up to 30 seconds; timeout never cancels. Long-task default is send and end response.", Annotations: ro}, func(ctx context.Context, _ *mcp.CallToolRequest, in manualUpdateInput) (*mcp.CallToolResult, any, error) {
		return manualCall(ctx, s, in.BindingID, in.SessionID, func(c *Client) (Update, error) {
			return c.WaitForUpdate(ctx, in.SessionID, in.TurnID, in.AfterCursor, time.Duration(in.TimeoutSeconds)*time.Second)
		})
	})
	mcp.AddTool(server, &mcp.Tool{Name: "codeagent_get_handoff", Description: "After terminal, compact evidence index for exact binding/Session/Turn. Execution, validation and acceptance differ. Missing validation stays partial/unknown. Read detail only for identified risks.", Annotations: ro}, func(ctx context.Context, _ *mcp.CallToolRequest, in manualHandoffInput) (*mcp.CallToolResult, any, error) {
		return manualCall(ctx, s, in.BindingID, in.SessionID, func(c *Client) (Handoff, error) { return c.HandoffForTask(ctx, in.SessionID, in.TurnID, in.TaskID) })
	})
	mcp.AddTool(server, &mcp.Tool{Name: "codeagent_get_artifacts", Description: "Detailed current-workspace Git diff and command evidence for a terminal Turn. Not an immutable per-Turn snapshot.", Annotations: ro}, func(ctx context.Context, _ *mcp.CallToolRequest, in manualArtifactInput) (*mcp.CallToolResult, any, error) {
		return manualCall(ctx, s, in.BindingID, in.SessionID, func(c *Client) (Artifacts, error) { return c.Artifacts(ctx, in.SessionID, in.TurnID) })
	})
	mcp.AddTool(server, &mcp.Tool{Name: "codeagent_cancel_turn", Description: "Request cancellation only for this explicit binding/Session. Not immediate stop confirmation and not approval."}, func(ctx context.Context, _ *mcp.CallToolRequest, in manualCancelInput) (*mcp.CallToolResult, any, error) {
		s.opMu.Lock()
		defer s.opMu.Unlock()
		return manualCall(ctx, s, in.BindingID, in.SessionID, func(c *Client) (map[string]bool, error) {
			err := c.Cancel(ctx, in.SessionID)
			return map[string]bool{"cancel_requested": err == nil}, err
		})
	})
	return server
}

func (s *ManualService) sendTool(ctx context.Context, _ *mcp.CallToolRequest, in manualSendInput) (*mcp.CallToolResult, any, error) {
	return s.send(ctx, in)
}
