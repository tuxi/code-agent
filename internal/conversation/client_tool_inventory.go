package conversation

// RegisteredClientTool is registration evidence, not a permission grant or a
// promise that the tool is included in the current model request.
type RegisteredClientTool struct{ Name, Description string }

func (e *TurnExecutor) RegisteredClientTools(sessionID string) []RegisteredClientTool {
	out := []RegisteredClientTool{}
	if e == nil || e.active == nil {
		return out
	}
	for _, tool := range e.active.ClientTools(sessionID) {
		out = append(out, RegisteredClientTool{Name: tool.Name, Description: tool.Description})
	}
	return out
}
