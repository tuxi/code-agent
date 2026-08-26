package sessions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"code-agent/internal/tools"
)

type SendToSessionTool struct{}

func (*SendToSessionTool) Name() string { return "send_to_session" }

// SideEffects marks send_to_session as side-effecting: it dispatches a turn to
// another session (possibly another workspace), triggering execution there. The
// approval chain gates it like any other mutating tool — ask prompts, auto asks
// (cross-workspace dispatch is not an in-workspace op), full auto-runs. The
// target turn's own permission tier is resolved by the target session.
func (*SendToSessionTool) SideEffects() bool { return true }
func (*SendToSessionTool) OutputSchema() json.RawMessage {
	return json.RawMessage(`{
	"type": "object",
	"properties": {
		"accepted":    {"type": "boolean"},
		"delivery":    {"type": "string", "description": "started or queued"},
		"session_id":  {"type": "string", "description": "Target session ID"},
		"turn_id":     {"type": "string", "description": "Admitted turn ID for wait_sessions cursor tracking"},
		"cursor":      {"type": "integer", "description": "Admission event sequence for wait_sessions cursor tracking"}
	}
}`)
}
func (*SendToSessionTool) Description() string {
	return "Send Agent-originated work to another live session through its owning Runtime. " +
		"The target uses its own model, credentials, tools, approval policy, workspace, and scheduler. " +
		"A busy target queues a new turn; this never steers the active turn. " +
		"Intent records request vs notification provenance; both execute a target turn and may reply. " +
		"Returns a turn_id and cursor; use wait_sessions when the terminal outcome matters."
}
func (*SendToSessionTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"message":{"type":"string"},"model":{"type":"string"},"intent":{"type":"string","enum":["request","notification"]},"correlation_id":{"type":"string"}},"required":["id","message"],"additionalProperties":false}`)
}

type sendToSessionInput struct {
	ID            string `json:"id"`
	Message       string `json:"message"`
	Model         string `json:"model"`
	Intent        string `json:"intent"`
	CorrelationID string `json:"correlation_id"`
}

func (*SendToSessionTool) Execute(ctx context.Context, ec tools.ExecutionContext, raw json.RawMessage) (tools.ToolResult, error) {
	var in sendToSessionInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return tools.ToolResult{}, fmt.Errorf("send_to_session: parse input: %w", err)
	}
	if strings.TrimSpace(in.ID) == "" || strings.TrimSpace(in.Message) == "" {
		return tools.ToolResult{}, fmt.Errorf("send_to_session: %w", errors.New("id and message are required"))
	}
	if in.ID == ec.SessionID {
		return tools.ToolResult{}, fmt.Errorf("send_to_session: %w", errors.New("target must be a different session"))
	}
	if ec.SessionControl == nil {
		return tools.ToolResult{}, fmt.Errorf("send_to_session: %w", errors.New("control plane is not available"))
	}
	if in.Intent == "" {
		in.Intent = "request"
	}
	if in.Intent != "request" && in.Intent != "notification" {
		return tools.ToolResult{}, fmt.Errorf("send_to_session: %w", errors.New("intent must be request or notification"))
	}
	messageID, err := randomID()
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("send_to_session: message id: %w", err)
	}
	if in.CorrelationID == "" {
		in.CorrelationID = messageID
	}
	delivery, err := ec.SessionControl.Send(ctx, tools.SessionSendRequest{
		TargetSessionID: in.ID, Message: in.Message, Model: in.Model,
		SenderSessionID: ec.SessionID, SenderTurnID: ec.TurnID,
		MessageID: messageID, CorrelationID: in.CorrelationID, Intent: in.Intent,
	})
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("send_to_session: %w", err)
	}
	out, err := json.Marshal(delivery)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("send_to_session: marshal: %w", err)
	}
	return tools.ToolResult{Content: string(out), Output: out}, nil
}

var _ tools.SideEffecting = (*SendToSessionTool)(nil)

func randomID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "msg_" + hex.EncodeToString(buf), nil
}

func stableToolRequestID(prefix string, ec tools.ExecutionContext) (string, error) {
	if ec.SessionID == "" || ec.TurnID == "" || ec.CallID == "" {
		return randomID()
	}
	digest := sha256.Sum256([]byte(ec.SessionID + "\x00" + ec.TurnID + "\x00" + ec.CallID))
	return prefix + "_" + hex.EncodeToString(digest[:16]), nil
}
