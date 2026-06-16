package agent

import (
	"encoding/json"
	"time"
)

// State is the full record of one agent run: the goal, the step budget, and
// every tool step taken. It is the basis for tracing and persistence later.
type State struct {
	Goal      string
	MaxSteps  int
	Completed bool
	Final     string
	Steps     []Step
}

// Step records a single tool execution within a turn. The cumulative
// conversation lives on the Session; a Step is just per-turn observability.
type Step struct {
	Index       int
	ToolName    string
	ToolInput   json.RawMessage
	Observation string
	Error       string
	StartedAt   time.Time
	FinishedAt  time.Time
}
