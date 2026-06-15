package agent

import "time"

type Step struct {
	Index       int
	Decision    Decision
	Observation string
	Error       string
	StartedAt   time.Time
	FinishedAt  time.Time
}

type State struct {
	Goal      string
	Steps     []Step
	MaxSteps  int
	Completed bool
	Final     string
}
