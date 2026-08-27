package runtime

import (
	"fmt"
	"strings"

	"github.com/tuxi/flux-workflow/definition"
)

const (
	crossWorkspaceCollaborationV1 = "cross_workspace_collaboration_v1"
	crossWorkspaceCollaborationV2 = "cross_workspace_collaboration_v2"
)

type crossWorkspaceAgentSpec struct {
	Role              string `json:"role"`
	SessionID         string `json:"session_id"`
	ReviewerSessionID string `json:"reviewer_session_id,omitempty"`
	WorkspacePath     string `json:"workspace_path,omitempty"`
	Message           string `json:"message"`
	ReviewerMessage   string `json:"reviewer_message,omitempty"`
	Acceptance        string `json:"acceptance,omitempty"`
	MaxIterations     int    `json:"max_iterations,omitempty"`
	CorrelationID     string `json:"correlation_id"`
	Intent            string `json:"intent,omitempty"`
}

func validateCrossWorkspaceManifest(template string, agents []crossWorkspaceAgentSpec, parallelism int) error {
	switch template {
	case crossWorkspaceCollaborationV1, crossWorkspaceCollaborationV2:
	default:
		return fmt.Errorf("unsupported workflow template %q", template)
	}
	if len(agents) == 0 || len(agents) > 8 {
		return fmt.Errorf("agents must contain 1 to 8 entries")
	}
	if parallelism < 0 || parallelism > 8 {
		return fmt.Errorf("parallelism must be between 1 and 8")
	}
	if template == crossWorkspaceCollaborationV2 {
		for index := range agents {
			if agents[index].ReviewerSessionID == "" {
				return fmt.Errorf("agent %d requires reviewer_session_id for v2 template", index)
			}
			if agents[index].ReviewerMessage == "" {
				agents[index].ReviewerMessage = fmt.Sprintf("Review the implementation by %s. Check against acceptance criteria: %s. Report VERDICT: PASS or VERDICT: REQUEST_CHANGES with specific file:line feedback.", agents[index].Role, agents[index].Acceptance)
			}
			if agents[index].MaxIterations <= 0 {
				agents[index].MaxIterations = 3
			}
		}
	}
	seenSessions := make(map[string]struct{}, len(agents))
	seenCorrelations := make(map[string]struct{}, len(agents))
	for index := range agents {
		agent := &agents[index]
		agent.Role = strings.TrimSpace(agent.Role)
		agent.SessionID = strings.TrimSpace(agent.SessionID)
		agent.ReviewerSessionID = strings.TrimSpace(agent.ReviewerSessionID)
		agent.WorkspacePath = strings.TrimSpace(agent.WorkspacePath)
		agent.Message = strings.TrimSpace(agent.Message)
		agent.CorrelationID = strings.TrimSpace(agent.CorrelationID)
		agent.Intent = strings.TrimSpace(agent.Intent)
		if agent.Role == "" || agent.SessionID == "" || agent.Message == "" || agent.CorrelationID == "" {
			return fmt.Errorf("agent %d requires role, session_id, message, and correlation_id", index)
		}
		if agent.Intent == "" {
			agent.Intent = "request"
		}
		if agent.Intent != "request" && agent.Intent != "notification" {
			return fmt.Errorf("agent %d intent must be request or notification", index)
		}
		if _, exists := seenSessions[agent.SessionID]; exists {
			return fmt.Errorf("duplicate agent session_id %q", agent.SessionID)
		}
		seenSessions[agent.SessionID] = struct{}{}
		if _, exists := seenCorrelations[agent.CorrelationID]; exists {
			return fmt.Errorf("duplicate agent correlation_id %q", agent.CorrelationID)
		}
		seenCorrelations[agent.CorrelationID] = struct{}{}
	}
	return nil
}

func crossWorkspaceWorkflowDefinitions(template, workflowID string, parallelism int) (*definition.WorkflowDefinition, *definition.WorkflowDefinition) {
	if parallelism <= 0 {
		parallelism = 4
	}
	suffix := strings.TrimPrefix(workflowID, "wf_")
	childName := template + "_agent_" + suffix
	parentName := template + "_" + suffix

	switch template {
	case crossWorkspaceCollaborationV2:
		return v2Definitions(childName, parentName, parallelism)
	default:
		return v1Definitions(childName, parentName, parallelism)
	}
}

func v1Definitions(childName, parentName string, parallelism int) (*definition.WorkflowDefinition, *definition.WorkflowDefinition) {
	child := &definition.WorkflowDefinition{
		Name: childName,
		Desc: "Dispatch one cross-workspace Agent turn and collect its terminal report",
		Output: definition.OutputDefinition{
			ResultType: "session_agent_result",
			Extras: map[string]string{
				"role":           "input.agent.role",
				"workspace_path": "input.agent.workspace_path ?? ''",
				"session_id":     "nodes.dispatch.output.session_id",
				"turn_id":        "nodes.dispatch.output.turn_id",
				"cursor":         "nodes.dispatch.output.cursor",
				"status":         "nodes.wait.output.status ?? 'waiting'",
				"summary":        "nodes.read.output.summary ?? ''",
				"last_turn":      "nodes.read.output.last_turn ?? ''",
			},
		},
		Nodes: []definition.NodeDefinition{
			{Name: "start", Label: "Start", Type: definition.NodeStart},
			{
				Name: "dispatch", Label: "Dispatch Agent", Type: definition.NodeTool,
				Config: map[string]any{"tool": "send_to_session"},
				InputMapping: map[string]string{
					"id":             "input.agent.session_id",
					"message":        "input.agent.message",
					"intent":         "input.agent.intent",
					"correlation_id": "input.agent.correlation_id",
				},
			},
			{
				Name: "wait", Label: "Await Agent Turn", Type: definition.NodeAwait,
				InputMapping: map[string]string{
					"session_id": "nodes.dispatch.output.session_id",
					"turn_id":    "nodes.dispatch.output.turn_id",
					"cursor":     "nodes.dispatch.output.cursor",
				},
				Config: map[string]any{
					"await_type":      "external_task",
					"source":          "webhook_or_poll",
					"timeout_seconds": 86400,
					"correlation": map[string]any{
						"session_id": "session_id",
						"turn_id":    "turn_id",
						"cursor":     "cursor",
					},
					"fallback_poll": map[string]any{
						"enabled":      true,
						"tool":         "check_turn",
						"start_after":  10,
						"interval":     15,
						"max_attempts": 5760,
					},
				},
			},
			{
				Name: "read", Label: "Read Agent Report", Type: definition.NodeTool,
				Config:       map[string]any{"tool": "read_session"},
				InputMapping: map[string]string{"id": "input.agent.session_id"},
			},
			{Name: "end", Label: "End", Type: definition.NodeEnd},
		},
		Edges: []definition.EdgeDefinition{
			{From: "start", To: "dispatch", Type: definition.EdgeNormal},
			{From: "dispatch", To: "wait", Type: definition.EdgeNormal},
			{From: "wait", To: "read", Type: definition.EdgeNormal},
			{From: "read", To: "end", Type: definition.EdgeNormal},
		},
	}

	parent := &definition.WorkflowDefinition{
		Name: parentName,
		Desc: "Deterministic Map fan-out/fan-in for cross-workspace Agent collaboration",
		Output: definition.OutputDefinition{
			ResultType: "cross_workspace_collaboration",
			Extras:     map[string]string{"results": "nodes.run_agents.output.results"},
		},
		Nodes: []definition.NodeDefinition{
			{Name: "start", Label: "Start", Type: definition.NodeStart},
			{
				Name: "run_agents", Label: "Run Agents", Type: definition.NodeMap,
				Config: map[string]any{
					"items":          "input.agents",
					"iterator":       "agent",
					"workflow":       childName,
					"parallel":       parallelism,
					"failure_policy": "fail_fast",
				},
			},
			{Name: "end", Label: "End", Type: definition.NodeEnd},
		},
		Edges: []definition.EdgeDefinition{
			{From: "start", To: "run_agents", Type: definition.EdgeNormal},
			{From: "run_agents", To: "end", Type: definition.EdgeNormal},
		},
	}
	return parent, child
}

// v2Definitions builds the Generator-Critic workflow: each agent's implementation
// is independently reviewed by a separate reviewer session. The child workflow is
// implement → await → read_impl → review → await_review → read_review → end.
// The parent Map fans out in parallel as in v1.
func v2Definitions(childName, parentName string, parallelism int) (*definition.WorkflowDefinition, *definition.WorkflowDefinition) {
	child := &definition.WorkflowDefinition{
		Name: childName,
		Desc: "Implement, await, read, then independently review with structured verdict",
		Output: definition.OutputDefinition{
			ResultType: "session_agent_result",
			Extras: map[string]string{
				"role":           "input.agent.role",
				"workspace_path": "input.agent.workspace_path ?? ''",
				"session_id":     "nodes.dispatch.output.session_id",
				"turn_id":        "nodes.dispatch.output.turn_id",
				"impl_status":    "nodes.wait_impl.output.status ?? 'waiting'",
				"impl_summary":   "nodes.read_impl.output.summary ?? ''",
				"impl_last_turn": "nodes.read_impl.output.last_turn ?? ''",
				"review_status":  "nodes.wait_review.output.status ?? 'waiting'",
				"review_verdict": "nodes.read_review.output.last_turn ?? ''",
			},
		},
		Nodes: []definition.NodeDefinition{
			{Name: "start", Label: "Start", Type: definition.NodeStart},
			// --- implement stage ---
			{
				Name: "dispatch", Label: "Dispatch Implementation", Type: definition.NodeTool,
				Config: map[string]any{"tool": "send_to_session"},
				InputMapping: map[string]string{
					"id":             "input.agent.session_id",
					"message":        "input.agent.message",
					"intent":         "input.agent.intent",
					"correlation_id": "input.agent.correlation_id",
				},
			},
			{
				Name: "wait_impl", Label: "Await Implementation", Type: definition.NodeAwait,
				InputMapping: map[string]string{
					"session_id": "nodes.dispatch.output.session_id",
					"turn_id":    "nodes.dispatch.output.turn_id",
					"cursor":     "nodes.dispatch.output.cursor",
				},
				Config: map[string]any{
					"await_type":      "external_task",
					"source":          "webhook_or_poll",
					"timeout_seconds": 86400,
					"correlation":     map[string]any{"session_id": "session_id", "turn_id": "turn_id", "cursor": "cursor"},
					"fallback_poll":   map[string]any{"enabled": true, "tool": "check_turn", "start_after": 10, "interval": 15, "max_attempts": 5760},
				},
			},
			{
				Name: "read_impl", Label: "Read Implementation", Type: definition.NodeTool,
				Config:       map[string]any{"tool": "read_session"},
				InputMapping: map[string]string{"id": "input.agent.session_id"},
			},
			// --- review stage ---
			{
				Name: "dispatch_review", Label: "Dispatch Review", Type: definition.NodeTool,
				Config: map[string]any{"tool": "send_to_session"},
				InputMapping: map[string]string{
					"id":             "input.agent.reviewer_session_id",
					"message":        "input.agent.reviewer_message",
					"intent":         "input.agent.intent",
					"correlation_id": "input.agent.correlation_id",
				},
			},
			{
				Name: "wait_review", Label: "Await Review", Type: definition.NodeAwait,
				InputMapping: map[string]string{
					"session_id": "nodes.dispatch_review.output.session_id",
					"turn_id":    "nodes.dispatch_review.output.turn_id",
					"cursor":     "nodes.dispatch_review.output.cursor",
				},
				Config: map[string]any{
					"await_type":      "external_task",
					"source":          "webhook_or_poll",
					"timeout_seconds": 86400,
					"correlation":     map[string]any{"session_id": "session_id", "turn_id": "turn_id", "cursor": "cursor"},
					"fallback_poll":   map[string]any{"enabled": true, "tool": "check_turn", "start_after": 10, "interval": 15, "max_attempts": 5760},
				},
			},
			{
				Name: "read_review", Label: "Read Review Verdict", Type: definition.NodeTool,
				Config:       map[string]any{"tool": "read_session"},
				InputMapping: map[string]string{"id": "input.agent.reviewer_session_id"},
			},
			{Name: "end", Label: "End", Type: definition.NodeEnd},
		},
		Edges: []definition.EdgeDefinition{
			{From: "start", To: "dispatch", Type: definition.EdgeNormal},
			{From: "dispatch", To: "wait_impl", Type: definition.EdgeNormal},
			{From: "wait_impl", To: "read_impl", Type: definition.EdgeNormal},
			{From: "read_impl", To: "dispatch_review", Type: definition.EdgeNormal},
			{From: "dispatch_review", To: "wait_review", Type: definition.EdgeNormal},
			{From: "wait_review", To: "read_review", Type: definition.EdgeNormal},
			{From: "read_review", To: "end", Type: definition.EdgeNormal},
		},
	}

	parent := &definition.WorkflowDefinition{
		Name: parentName,
		Desc: "Deterministic Map fan-out/fan-in with independent review per agent",
		Output: definition.OutputDefinition{
			ResultType: "cross_workspace_collaboration_v2",
			Extras:     map[string]string{"results": "nodes.run_agents.output.results"},
		},
		Nodes: []definition.NodeDefinition{
			{Name: "start", Label: "Start", Type: definition.NodeStart},
			{
				Name: "run_agents", Label: "Run Agents", Type: definition.NodeMap,
				Config: map[string]any{
					"items": "input.agents", "iterator": "agent",
					"workflow": childName, "parallel": parallelism,
					"failure_policy": "fail_fast",
				},
			},
			{Name: "end", Label: "End", Type: definition.NodeEnd},
		},
		Edges: []definition.EdgeDefinition{
			{From: "start", To: "run_agents", Type: definition.EdgeNormal},
			{From: "run_agents", To: "end", Type: definition.EdgeNormal},
		},
	}
	return parent, child
}
