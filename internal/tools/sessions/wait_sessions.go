package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"code-agent/internal/tools"
)

type WaitSessionsTool struct{}

func (*WaitSessionsTool) Name() string { return "wait_sessions" }
func (*WaitSessionsTool) Description() string {
	return "Wait for the first target turn to finish, fail, or be cancelled. This is wait-any, not wait-all. " +
		"Targets must use the session id, turn_id, and cursor returned by send_to_session, so older terminal events cannot satisfy the wait."
}
func (*WaitSessionsTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"targets":{"type":"array","minItems":1,"maxItems":8,"items":{"type":"object","properties":{"id":{"type":"string"},"turn_id":{"type":"string"},"cursor":{"type":"integer","minimum":0}},"required":["id","turn_id","cursor"],"additionalProperties":false}},"timeout_ms":{"type":"integer","minimum":0,"maximum":3600000}},"required":["targets"],"additionalProperties":false}`)
}

type waitSessionsInput struct {
	Targets   []tools.SessionWaitTarget `json:"targets"`
	TimeoutMS *int64                    `json:"timeout_ms"`
}

func (*WaitSessionsTool) Execute(ctx context.Context, ec tools.ExecutionContext, raw json.RawMessage) (tools.ToolResult, error) {
	var in waitSessionsInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return tools.ToolResult{}, fmt.Errorf("wait_sessions: parse input: %w", err)
	}
	if len(in.Targets) == 0 || len(in.Targets) > 8 {
		return tools.ToolResult{}, fmt.Errorf("wait_sessions: targets must contain 1 to 8 entries")
	}
	seen := make(map[string]struct{}, len(in.Targets))
	for _, target := range in.Targets {
		if target.SessionID == "" || target.TurnID == "" || target.Cursor < 0 {
			return tools.ToolResult{}, fmt.Errorf("wait_sessions: every target requires id, turn_id, and non-negative cursor")
		}
		key := target.SessionID + "\x00" + target.TurnID
		if _, exists := seen[key]; exists {
			return tools.ToolResult{}, fmt.Errorf("wait_sessions: duplicate target %s/%s", target.SessionID, target.TurnID)
		}
		seen[key] = struct{}{}
	}
	if ec.SessionControl == nil {
		return tools.ToolResult{}, fmt.Errorf("wait_sessions: control plane is not available")
	}
	timeout := 5 * time.Minute
	if in.TimeoutMS != nil {
		timeout = time.Duration(*in.TimeoutMS) * time.Millisecond
	}
	result, err := ec.SessionControl.WaitAny(ctx, in.Targets, timeout)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("wait_sessions: %w", err)
	}
	out, err := json.Marshal(result)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("wait_sessions: marshal: %w", err)
	}
	return tools.ToolResult{Content: string(out), Output: out}, nil
}
