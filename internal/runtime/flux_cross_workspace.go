package runtime

import (
	"fmt"
	"strings"

	"github.com/tuxi/flux-workflow/definition"
)

const crossWorkspaceCollaborationTemplate = "cross_workspace_collaboration_v1"

type crossWorkspaceAgentSpec struct {
	Role          string `json:"role"`
	SessionID     string `json:"session_id"`
	WorkspacePath string `json:"workspace_path,omitempty"`
	Message       string `json:"message"`
	CorrelationID string `json:"correlation_id"`
	Intent        string `json:"intent,omitempty"`
}

func validateCrossWorkspaceManifest(template string, agents []crossWorkspaceAgentSpec, parallelism int) error {
	if template != crossWorkspaceCollaborationTemplate {
		return fmt.Errorf("unsupported workflow template %q", template)
	}
	if len(agents) == 0 || len(agents) > 8 {
		return fmt.Errorf("agents must contain 1 to 8 entries")
	}
	if parallelism < 0 || parallelism > 8 {
		return fmt.Errorf("parallelism must be between 1 and 8")
	}
	seenSessions := make(map[string]struct{}, len(agents))
	seenCorrelations := make(map[string]struct{}, len(agents))
	for index := range agents {
		agent := &agents[index]
		agent.Role = strings.TrimSpace(agent.Role)
		agent.SessionID = strings.TrimSpace(agent.SessionID)
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

func crossWorkspaceWorkflowDefinitions(workflowID string, parallelism int) (*definition.WorkflowDefinition, *definition.WorkflowDefinition) {
	if parallelism <= 0 {
		parallelism = 4
	}
	suffix := strings.TrimPrefix(workflowID, "wf_")
	childName := crossWorkspaceCollaborationTemplate + "_agent_" + suffix
	parentName := crossWorkspaceCollaborationTemplate + "_" + suffix

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
				"status":         "len(nodes.wait.output.completed ?? []) > 0 ? nodes.wait.output.completed[0].status : (nodes.wait.output.timed_out ? 'timed_out' : 'waiting')",
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
				Name: "wait", Label: "Wait for Agent", Type: definition.NodeTool,
				Config: map[string]any{"tool": "wait_sessions"},
				InputMapping: map[string]string{
					"targets":    `[{"id": nodes.dispatch.output.session_id, "turn_id": nodes.dispatch.output.turn_id, "cursor": nodes.dispatch.output.cursor}]`,
					"timeout_ms": "input.timeout_ms",
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
