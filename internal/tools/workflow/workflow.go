// Package workflow implements the model-facing `workflow` tool: list/view/save/
// run/delete of named workflow templates. It is the conversation counterpart of
// the workflow panel — the agent can save the current successful run as a
// reusable template, inspect it, trigger it, or remove it, all in natural
// language. Registered into the base registry like the automation tool.
package workflow

import (
	"context"
	"encoding/json"
	"fmt"

	"code-agent/internal/tools"
)

// WorkflowTool is the single-tool, multi-mode entry point (R8), mirroring the
// automation tool's mode dispatch. mode selects list|view|save|run|delete.
type WorkflowTool struct{}

func (*WorkflowTool) Name() string { return "workflow" }

func (*WorkflowTool) Description() string {
	return "Create, list, view, trigger, or delete named workflow templates, and compile a successful conversation into a template. " +
		"A template is a saved multi-step workflow that can be re-run by name. " +
		"mode is required: list (all templates/runs), view (detail: versions + run history + manifest), " +
		"save (persist a finished run as a reusable template, requires name and source_task_id), " +
		"run (trigger a saved template by name, returns task_id), delete (soft-delete a template), " +
		"extract (return this session's recent tool-call sequence so you can compile it into a template), " +
		"save_tool_sequence (compile a tool_sequence manifest {goal, inputs, steps:[{tool,args}]} into a named template, requires name and manifest). " +
		"workspace_path defaults to the current workspace."
}

func (*WorkflowTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"mode": {"type": "string", "enum": ["list", "view", "save", "run", "delete", "extract", "save_tool_sequence"]},
			"name": {"type": "string", "description": "template name; required for view/run/delete/save_tool_sequence, for save it is the new template name"},
			"description": {"type": "string", "description": "save: human-readable template description"},
			"source_task_id": {"type": "integer", "description": "save: the run (task id) to persist as a template"},
			"workspace_path": {"type": "string", "description": "optional workspace; defaults to the current one"},
			"input": {"type": "object", "description": "run: the run input manifest {goal, template, agents:[{role, session_id, message, intent, correlation_id}], parallelism, timeout_ms}"},
			"limit": {"type": "integer", "description": "extract: max number of recent tool calls to return (default 50)"},
			"manifest": {"type": "object", "description": "save_tool_sequence: the tool_sequence manifest {type:\"tool_sequence\", goal, description, inputs:[{name,type,required,description}], steps:[{tool,args}]}"}
		},
		"required": ["mode"],
		"additionalProperties": false
	}`)
}

type workflowInput struct {
	Mode          string          `json:"mode"`
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	SourceTaskID  int64           `json:"source_task_id"`
	WorkspacePath string          `json:"workspace_path"`
	Input         map[string]any  `json:"input"`
	Limit         int             `json:"limit"`
	Manifest      json.RawMessage `json:"manifest"`
}

func (*WorkflowTool) Execute(ctx context.Context, ec tools.ExecutionContext, raw json.RawMessage) (tools.ToolResult, error) {
	var in workflowInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return tools.ToolResult{}, fmt.Errorf("workflow: parse input: %w", err)
	}
	if ec.WorkflowRunner == nil {
		return tools.ToolResult{}, fmt.Errorf("workflow: workflow runner is not available in this host")
	}
	runner := ec.WorkflowRunner
	root := in.WorkspacePath
	if root == "" {
		root = ec.WorkspaceRoot
	}
	if root == "" {
		return tools.ToolResult{}, fmt.Errorf("workflow: workspace_path is required when no workspace is set")
	}

	switch in.Mode {
	case "list":
		out, err := runner.List(ctx, root)
		if err != nil {
			return tools.ToolResult{}, fmt.Errorf("workflow: list: %w", err)
		}
		return tools.ToolResult{Content: string(out), Output: out}, nil

	case "view":
		if in.Name == "" {
			return tools.ToolResult{}, fmt.Errorf("workflow: view requires name")
		}
		out, err := runner.Detail(ctx, root, in.Name)
		if err != nil {
			return tools.ToolResult{}, fmt.Errorf("workflow: view: %w", err)
		}
		return tools.ToolResult{Content: string(out), Output: out}, nil

	case "save":
		if in.Name == "" || in.SourceTaskID <= 0 {
			return tools.ToolResult{}, fmt.Errorf("workflow: save requires name and source_task_id")
		}
		if err := runner.SaveTemplate(ctx, root, in.Name, in.Description, in.SourceTaskID); err != nil {
			return tools.ToolResult{}, fmt.Errorf("workflow: save: %w", err)
		}
		out, _ := json.Marshal(map[string]string{"name": in.Name, "status": "saved"})
		return tools.ToolResult{Content: string(out), Output: out}, nil

	case "run":
		if in.Name == "" {
			return tools.ToolResult{}, fmt.Errorf("workflow: run requires name")
		}
		if in.Input == nil {
			in.Input = map[string]any{}
		}
		taskID, err := runner.Run(ctx, root, in.Name, in.Input)
		if err != nil {
			return tools.ToolResult{}, fmt.Errorf("workflow: run: %w", err)
		}
		out, _ := json.Marshal(map[string]any{"task_id": taskID, "name": in.Name})
		return tools.ToolResult{Content: string(out), Output: out}, nil

	case "delete":
		if in.Name == "" {
			return tools.ToolResult{}, fmt.Errorf("workflow: delete requires name")
		}
		if err := runner.Delete(ctx, root, in.Name); err != nil {
			return tools.ToolResult{}, fmt.Errorf("workflow: delete: %w", err)
		}
		out, _ := json.Marshal(map[string]string{"name": in.Name, "status": "deleted"})
		return tools.ToolResult{Content: string(out), Output: out}, nil

	case "extract":
		if ec.SessionTrace == nil {
			return tools.ToolResult{}, fmt.Errorf("workflow: extract requires a session trace reader (unavailable in this host)")
		}
		if ec.SessionID == "" {
			return tools.ToolResult{}, fmt.Errorf("workflow: extract requires a session")
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 50
		}
		steps, err := ec.SessionTrace(ctx, ec.SessionID, limit)
		if err != nil {
			return tools.ToolResult{}, fmt.Errorf("workflow: extract: %w", err)
		}
		out, _ := json.Marshal(steps)
		return tools.ToolResult{Content: string(out), Output: out}, nil

	case "save_tool_sequence":
		if in.Name == "" {
			return tools.ToolResult{}, fmt.Errorf("workflow: save_tool_sequence requires name")
		}
		if len(in.Manifest) == 0 {
			return tools.ToolResult{}, fmt.Errorf("workflow: save_tool_sequence requires manifest (tool_sequence definition)")
		}
		savedName, err := runner.SaveToolSequence(ctx, root, in.Name, in.Manifest)
		if err != nil {
			return tools.ToolResult{}, fmt.Errorf("workflow: save_tool_sequence: %w", err)
		}
		out, _ := json.Marshal(map[string]string{"name": savedName, "status": "saved"})
		return tools.ToolResult{Content: string(out), Output: out}, nil

	default:
		return tools.ToolResult{}, fmt.Errorf("workflow: unsupported mode %q", in.Mode)
	}
}
