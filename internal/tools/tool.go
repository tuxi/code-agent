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
	InputSchema() string
	Execute(ctx context.Context, input json.RawMessage) (ToolResult, error)
}
