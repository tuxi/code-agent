package tools

import (
	"context"
	"encoding/json"
)

type ToolResult struct {
	Content string `json:"content"`
}

type Tool interface {
	Name() string
	Description() string
	InputSchema() json.RawMessage
	Execute(ctx context.Context, input json.RawMessage) (ToolResult, error)
}
