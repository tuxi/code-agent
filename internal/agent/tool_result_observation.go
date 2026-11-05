package agent

import (
	"strings"

	"code-agent/internal/observation"
)

// Resolve the outcome once, before counters, gates, events and history consume it.
func (r *Runner) toolResultObservation(name string, result toolCallResult) observation.Observation {
	obs := observation.Observation{Tool: name, OK: result.succeeded, FailureType: observation.FailureNone}
	if r.Observer != nil {
		obs = r.Observer.Observe(name, result.observation)
	}
	if result.failure != nil {
		kind := result.failure.Kind
		if kind == "canceled" {
			obs.OK, obs.FailureType, obs.Summary = true, observation.FailureType(kind), "tool call interrupted"
			return obs
		}
		if kind == "" {
			kind = "runtime"
		}
		// Preserve a more precise command diagnosis such as compile/test.
		if kind != "runtime" || obs.OK {
			obs.FailureType = observation.FailureType(kind)
		}
		obs.OK = false
	}
	if !obs.OK {
		if obs.FailureType == observation.FailureNone {
			obs.FailureType = observation.FailureRuntime
		}
		if obs.Summary == "" {
			line, _, _ := strings.Cut(result.observation, "\n")
			runes := []rune(line)
			if len(runes) > 300 {
				line = string(runes[:300]) + "…"
			}
			obs.Summary = line
		}
	}
	return obs
}
