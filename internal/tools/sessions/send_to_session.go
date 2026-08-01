package sessions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"code-agent/internal/tools"
)

type SendToSessionTool struct{}

func (*SendToSessionTool) Name() string { return "send_to_session" }
func (*SendToSessionTool) Description() string {
	return "Send Agent-originated work to another live session through its owning Runtime. " +
		"The target uses its own model, credentials, tools, approval policy, workspace, and scheduler. " +
		"A busy target queues a new turn; this never steers the active turn. Returns a turn_id and cursor for wait_sessions."
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

func randomID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "msg_" + hex.EncodeToString(buf), nil
}
