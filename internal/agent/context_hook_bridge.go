package agent

import (
	"code-agent/internal/hooks"
	"code-agent/internal/model"
)

// ctxHookFromModel converts a model.Message slice into the hooks package
// representation consumed by context shell hooks. This is a lossless shape
// conversion — the two types are structurally identical but live in different
// packages so the hooks package stays independently testable.
func ctxHookFromModel(msgs []model.Message) []hooks.ContextHookMessage {
	out := make([]hooks.ContextHookMessage, 0, len(msgs))
	for _, m := range msgs {
		chm := hooks.ContextHookMessage{
			Role:       string(m.Role),
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		if len(m.ToolCalls) > 0 {
			chm.ToolCalls = make([]hooks.ContextHookToolCall, len(m.ToolCalls))
			for i, tc := range m.ToolCalls {
				chm.ToolCalls[i] = hooks.ContextHookToolCall{
					ID:   tc.ID,
					Type: tc.Type,
					Function: hooks.ContextHookFunctionCall{
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				}
			}
		}
		out = append(out, chm)
	}
	return out
}

// ctxHookToModel converts the hooks package representation back into
// model.Message after context hooks have transformed the message list.
func ctxHookToModel(msgs []hooks.ContextHookMessage) []model.Message {
	out := make([]model.Message, 0, len(msgs))
	for _, m := range msgs {
		mm := model.Message{
			Role:       model.Role(m.Role),
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		if len(m.ToolCalls) > 0 {
			mm.ToolCalls = make([]model.ToolCall, len(m.ToolCalls))
			for i, tc := range m.ToolCalls {
				mm.ToolCalls[i] = model.ToolCall{
					ID:   tc.ID,
					Type: tc.Type,
					Function: model.FunctionCall{
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				}
			}
		}
		out = append(out, mm)
	}
	return out
}
