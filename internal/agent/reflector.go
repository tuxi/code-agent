package agent

import "code-agent/internal/reflection"

// Reflector inspects the steps a turn has taken and returns a factual
// ReflectionContext from which a self-check nudge can be built (P4.3). Like
// Observer, it is a *data* hook, never control: it states what happened, the
// loop asks the model about it, and the model decides. Defined here so the loop
// depends only on the interface; the analysis lives in the reflection package.
//
// Nil-safe: when a Runner has no Reflector, the finalize boundary behaves exactly
// as before.
type Reflector interface {
	Reflect(steps []Step) reflection.ReflectionContext
}

// DefaultReflector is the standard Reflector. It adapts the agent's richer Step
// into the reflection package's neutral StepView and delegates the analysis —
// keeping the dependency one-way (agent → reflection) so reflection stays pure
// and independently testable.
type DefaultReflector struct{}

func (DefaultReflector) Reflect(steps []Step) reflection.ReflectionContext {
	views := make([]reflection.StepView, len(steps))
	for i, s := range steps {
		views[i] = reflection.StepView{
			Tool:        s.ToolName,
			Input:       string(s.ToolInput),
			Observation: s.Observation,
		}
	}
	return reflection.Reflect(views)
}
