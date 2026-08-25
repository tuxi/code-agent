package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"code-agent/internal/tools"
)

// GetCurrentTimeTool returns the current wall-clock time, the local IANA
// timezone, and the UTC offset. The automation skill instructs the model to call
// it before creating an automation so the schedule's timezone is unambiguous
// (the same rrule means different wall-clock times in UTC vs PDT).
type GetCurrentTimeTool struct{}

func (*GetCurrentTimeTool) Name() string { return "get_current_time" }

func (*GetCurrentTimeTool) Description() string {
	return "Return the current time, the local IANA timezone, and the UTC offset. " +
		"Call this before creating an automation to confirm the timezone for the schedule."
}

func (*GetCurrentTimeTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}

func (*GetCurrentTimeTool) Execute(ctx context.Context, ec tools.ExecutionContext, _ json.RawMessage) (tools.ToolResult, error) {
	now := time.Now()
	_, offset := now.Zone()
	out := map[string]any{
		"now":        now.Format(time.RFC3339),
		"timezone":   now.Location().String(),
		"utc_offset": offset,
	}
	b, err := json.Marshal(out)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("get_current_time: marshal: %w", err)
	}
	return tools.ToolResult{Content: string(b), Output: b}, nil
}

var _ tools.Tool = (*GetCurrentTimeTool)(nil)
