package session

import "code-agent/internal/model"

// OutputTokenBudget prevents an upstream default from reserving most of the
// context window for generation. Small windows retain room for the prompt.
func OutputTokenBudget(window int) int {
	if window <= 0 {
		return 16384
	}
	return max(1, min(16384, window/8))
}

func InputTokenBudget(window int) int {
	if window <= 0 {
		return 0
	}
	return max(1, window-OutputTokenBudget(window)-max(1, window/20))
}

// EstimateContextTokens includes echoed reasoning, which can dominate long
// tool conversations. This is an estimate, never a replacement for usage.
func EstimateContextTokens(messages []model.Message) int {
	n := 0
	for _, m := range messages {
		n += 8 + (len(m.Content)+len(m.ReasoningContent)+2)/3
		for _, c := range m.ToolCalls {
			n += (len(c.Function.Name) + len(c.Function.Arguments) + 2) / 3
		}
		for _, p := range m.ContentParts {
			n += (len(p.Text) + 2) / 3
		}
	}
	return n
}
