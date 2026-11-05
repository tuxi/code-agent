package tools

// ToolFailure describes a business failure even when execution itself returned
// no Go error. It does not stop the turn or prescribe the model's next action.
type ToolFailure struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func (f *ToolFailure) Error() string { return f.Message }

func FailureResult(kind, message string) ToolResult {
	return ToolResult{Content: message, Failure: &ToolFailure{Kind: kind, Message: message}}
}

func (r ToolResult) Err() error {
	if r.Failure == nil {
		return nil
	}
	if r.Failure.Message != "" {
		return r.Failure
	}
	return &ToolFailure{Kind: r.Failure.Kind, Message: r.Content}
}
