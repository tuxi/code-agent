package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tuxi/flux-workflow/definition"
	"github.com/tuxi/flux-workflow/domain"
	workflowruntime "github.com/tuxi/flux-workflow/runtime"
	fluxtool "github.com/tuxi/flux-workflow/tool"
	fluxmodel "github.com/tuxi/flux/model"
	"github.com/tuxi/flux/planner"
	"gorm.io/gorm"

	"code-agent/internal/model"
	"code-agent/internal/tools"
)

var fluxExcludedTools = map[string]bool{
	"plan_workflow":       true,
	"task":                true,
	"enter_plan_mode":     true,
	"propose_plan":        true,
	"ask_user":            true,
	"todo":                true,
	"workflow_status":     true,
	"workflow_list":       true,
	"workflow_definition": true,
	"workflow_events":     true,
}

// runtimePool holds one started Runtime per workspace db. After the first
// plan_workflow call starts workers, subsequent calls in the same workspace
// reuse the Runtime. Multiple task workers are required for NodeMap children
// to honor their parallelism instead of merely queueing fan-out tasks.
var runtimePool sync.Map // key: dbPath (string), value: *workflowruntime.Runtime

func getOrCreateRuntime(ctx context.Context, workspaceRoot string) (*workflowruntime.Runtime, error) {
	dbPath := fluxDBPath(workspaceRoot)
	if v, ok := runtimePool.Load(dbPath); ok {
		return v.(*workflowruntime.Runtime), nil
	}
	rt, err := newFluxRuntime(workspaceRoot)
	if err != nil {
		return nil, err
	}
	// Embedded/mobile mode: only the task worker is needed (consumes queue).
	// AsyncWorker, AwaitPollWorker, and RecoveryScanner are server-side
	// machinery — pointless full-table scans on a single-workflow device.
	if err := rt.Start(context.Background(),
		workflowruntime.WithTaskWorkers(4),
		workflowruntime.WithAsyncWorkers(0),
		workflowruntime.WithAwaitPollWorkers(0),
		workflowruntime.WithRecoveryScanner(false),
	); err != nil {
		rt.Shutdown()
		return nil, fmt.Errorf("start runtime: %w", err)
	}
	actual, loaded := runtimePool.LoadOrStore(dbPath, rt)
	if loaded {
		// Another goroutine raced us and won — shut down our duplicate.
		rt.Shutdown()
	}
	return actual.(*workflowruntime.Runtime), nil
}

func fluxDBPath(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".codeagent", "flux-workflows", "flux-workflows.db")
}

// FluxWorkflowTool is a turn-aware code-agent tool. Flux owns planning and
// compilation; flux-workflow owns execution; code-agent owns provider, tools,
// policy, events and billing.
type FluxWorkflowTool struct {
	provider  model.Provider
	modelName string
}

type fluxWorkflowInput struct {
	Goal        string                    `json:"goal"`
	Action      string                    `json:"action"`
	WorkflowID  string                    `json:"workflow_id"`
	TaskID      int64                     `json:"task_id"`
	ResumeFrom  string                    `json:"resume_from"`
	Template    string                    `json:"template"`
	Agents      []crossWorkspaceAgentSpec `json:"agents"`
	Parallelism int                       `json:"parallelism"`
	TimeoutMS   *int64                    `json:"timeout_ms"`
}

func NewFluxWorkflowTool(provider model.Provider, modelName string) *FluxWorkflowTool {
	return &FluxWorkflowTool{provider: provider, modelName: modelName}
}

func (t *FluxWorkflowTool) Name() string { return "plan_workflow" }

func (t *FluxWorkflowTool) Description() string {
	return "Execute a known, multi-step task as a pre-planned DAG — NO reflection, NO self-correction. " +
		"Every step must be known in advance because the DAG is frozen after user approval. " +
		"Use this ONLY when the steps and their dependencies are already clear: " +
		"parallel work across many files or modules, " +
		"sequential pipelines with well-defined inputs/outputs (A → B → C), " +
		"or long-running operations that may need to resume after failure. " +
		"Do NOT use this to explore or figure out an approach — " +
		"if you need to research, read code, or decide what to do as you go, use enter_plan_mode instead. " +
		"For simple single-step changes, skip this and act directly. " +
		"Use template=cross_workspace_collaboration_v1 with typed agents when a supervisor has already resolved cross-workspace worker sessions."
}

func (t *FluxWorkflowTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"goal":{"type":"string","description":"A complete description of the known workflow and its success conditions."},"action":{"type":"string","enum":["new","retry"],"description":"new=create and execute; retry=recover a failed workflow from a specific node"},"workflow_id":{"type":"string","description":"Required for retry: the workflow ID to recover"},"task_id":{"type":"integer","description":"Required for retry: the task ID to retry"},"resume_from":{"type":"string","description":"Optional for retry: node name to resume from; empty auto-collects failed roots"},"template":{"type":"string","enum":["cross_workspace_collaboration_v1"],"description":"Optional deterministic workflow template. Omit to use the LLM DAG planner."},"agents":{"type":"array","minItems":1,"maxItems":8,"description":"Resolved worker assignments for the cross-workspace template.","items":{"type":"object","properties":{"role":{"type":"string"},"session_id":{"type":"string"},"workspace_path":{"type":"string"},"message":{"type":"string"},"correlation_id":{"type":"string"},"intent":{"type":"string","enum":["request","notification"]}},"required":["role","session_id","message","correlation_id"],"additionalProperties":false}},"parallelism":{"type":"integer","minimum":1,"maximum":8},"timeout_ms":{"type":"integer","minimum":0,"maximum":300000}},"required":["goal"],"additionalProperties":false}`)
}

func (t *FluxWorkflowTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	var args fluxWorkflowInput
	if err := json.Unmarshal(input, &args); err != nil {
		return tools.ToolResult{Content: "invalid input: " + err.Error()}, nil
	}
	args.Goal = strings.TrimSpace(args.Goal)

	// Retry path: recover existing workflow from its durable .db and re-run.
	if args.Action == "retry" {
		return t.executeRetry(ctx, ec, args)
	}

	if args.Goal == "" {
		return tools.ToolResult{Content: "goal is required"}, nil
	}
	if args.Template != "" {
		if err := validateCrossWorkspaceManifest(args.Template, args.Agents, args.Parallelism); err != nil {
			return tools.ToolResult{Content: "flux workflow manifest failed: " + err.Error()}, nil
		}
	} else if t.provider == nil {
		return tools.ToolResult{Content: "flux workflow failed: model provider is unavailable"}, nil
	}
	if ec.ToolRegistry == nil || ec.NestedExecutor == nil {
		return tools.ToolResult{Content: "flux workflow failed: controlled tool execution is unavailable"}, nil
	}

	workflowIdentity := args.Goal
	if args.Template != "" {
		manifestIdentity, _ := json.Marshal(struct {
			Goal        string                    `json:"goal"`
			Template    string                    `json:"template"`
			Agents      []crossWorkspaceAgentSpec `json:"agents"`
			Parallelism int                       `json:"parallelism"`
		}{args.Goal, args.Template, args.Agents, args.Parallelism})
		workflowIdentity = string(manifestIdentity)
	}
	workflowID := fluxWorkflowID(ec, workflowIdentity)
	emitWorkflow(ec, "workflow_started", map[string]any{
		"workflow_id": workflowID, "parent_call_id": ec.CallID, "stage": "planning", "goal": args.Goal,
	})
	usage := &fluxUsageCollector{}
	nestedUsage := &nestedUsageCollector{}
	fluxReg := projectFluxTools(ctx, ec, nestedUsage)
	if len(fluxReg.List()) == 0 {
		emitWorkflowFailure(ec, workflowID, "planning", "no eligible tools are available")
		return tools.ToolResult{Content: "flux workflow failed: no eligible tools are available"}, nil
	}

	var def, childDef *definition.WorkflowDefinition
	if args.Template != "" {
		for _, requiredTool := range []string{"send_to_session", "wait_sessions", "read_session"} {
			if _, ok := fluxReg.Get(requiredTool); !ok {
				err := fmt.Errorf("template requires tool %q", requiredTool)
				emitWorkflowFailure(ec, workflowID, "compiling", err.Error())
				return tools.ToolResult{Content: "flux workflow template failed: " + err.Error()}, nil
			}
		}
		def, childDef = crossWorkspaceWorkflowDefinitions(workflowID, args.Parallelism)
	} else {
		provider := &fluxCompleterAdapter{provider: t.provider, ec: ec, usage: usage}
		dagPlanner := planner.NewDAGPlanner(provider, t.modelName, args.Goal, fluxReg)
		// Planner LLM calls can easily exceed the HTTP request timeout (large system
		// prompt + complex DAG generation). Use a detached context with a generous
		// deadline while still respecting the tool's own cancellation (e.g. Ctrl-C).
		planCtx, planCancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer planCancel()
		var err error
		def, err = dagPlanner.GenerateWorkflow(planCtx, nil)
		if err != nil {
			emitWorkflowFailure(ec, workflowID, "planning", err.Error())
			return tools.ToolResult{Content: "flux workflow planning failed: " + err.Error(), ModelUsage: usage.snapshot()}, nil
		}
	}
	rt, err := getOrCreateRuntime(ctx, ec.WorkspaceRoot)
	if err != nil {
		emitWorkflowFailure(ec, workflowID, "initializing", err.Error())
		return tools.ToolResult{Content: "flux workflow runtime failed: " + err.Error(), ModelUsage: usage.snapshot()}, nil
	}
	// Tools registered before Submit so workers resolve tool nodes correctly.
	// Re-register each time in case the turn's registry has changed.
	for _, projected := range fluxReg.List() {
		rt.ToolRegistry().Register(projected)
	}
	if childDef != nil {
		if err := registerFluxWorkflow(ctx, rt, childDef); err != nil {
			emitWorkflowFailure(ec, workflowID, "registering", err.Error())
			return tools.ToolResult{Content: "flux child workflow registration failed: " + err.Error(), ModelUsage: usage.snapshot()}, nil
		}
	}
	if err := registerFluxWorkflow(ctx, rt, def); err != nil {
		emitWorkflowFailure(ec, workflowID, "registering", err.Error())
		return tools.ToolResult{Content: "flux workflow registration failed: " + err.Error(), ModelUsage: usage.snapshot()}, nil
	}
	emitWorkflow(ec, "workflow_plan_ready", map[string]any{
		"workflow_id": workflowID, "parent_call_id": ec.CallID, "stage": "executing", "goal": args.Goal,
		"nodes": def.Nodes, "edges": def.Edges, "output": def.Output,
	})

	// Block for user approval before enqueuing the task.
	if ec.WorkflowPlanApproval != nil {
		dagJSON, _ := json.Marshal(map[string]any{"nodes": def.Nodes, "edges": def.Edges})
		if !ec.WorkflowPlanApproval(workflowID, args.Goal, string(dagJSON)) {
			emitWorkflow(ec, "workflow_rejected", map[string]any{
				"workflow_id": workflowID, "parent_call_id": ec.CallID, "reason": "user_rejected",
			})
			return tools.ToolResult{Content: "workflow cancelled by user"}, nil
		}
	}

	// Subscribe to events before Submit to avoid missing terminal events.
	bridgeCtx, bridgeCancel := context.WithCancel(context.Background())
	bindBridgeTask := bridgeFluxEventsAsync(bridgeCtx, rt, ec, workflowID)

	taskInput := map[string]any{"goal": args.Goal, "workflow_id": workflowID}
	if args.Template != "" {
		timeoutMS := int64(300000)
		if args.TimeoutMS != nil {
			timeoutMS = *args.TimeoutMS
		}
		taskInput["agents"] = args.Agents
		taskInput["timeout_ms"] = timeoutMS
		taskInput["template"] = args.Template
	}
	taskID, err := rt.Submit(ctx, def.Name, taskInput)
	if err != nil {
		bridgeCancel() // task never enqueued → goroutine would leak
		emitWorkflowFailure(ec, workflowID, "executing", err.Error())
		return tools.ToolResult{Content: "flux workflow submission failed: " + err.Error(), ModelUsage: usage.snapshot()}, nil
	}
	bindBridgeTask(taskID)
	// Submit succeeded — goroutine self-cleans on terminal; don't cancel.
	_ = bridgeCancel

	// Always async — same model as subagent, job, and wait. Events flow via
	// WS; client observes progress in real-time or queries snapshot at any time.
	if ec.OnWorkflowEvent != nil {
		emitWorkflow(ec, "workflow_task_started", map[string]any{
			"workflow_id":    workflowID,
			"parent_call_id": ec.CallID,
			"task_id":        taskID,
			"status":         "pending",
		})
	}

	payload := map[string]any{
		"workflow_id": workflowID,
		"task_id":     taskID,
		"status":      "queued",
	}
	if args.Template != "" {
		payload["template"] = args.Template
	}
	out, _ := json.Marshal(payload)
	return tools.ToolResult{
		Content:      string(out),
		Output:       out,
		ModelUsage:   usage.snapshot(),
		NestedUsages: nestedUsage.snapshot(),
	}, nil
}

func registerFluxWorkflow(ctx context.Context, rt *workflowruntime.Runtime, def *definition.WorkflowDefinition) error {
	err := rt.RegisterWorkflow(ctx, def)
	if err != nil && strings.Contains(err.Error(), "already registered") {
		return nil
	}
	return err
}

func RegisterFluxTool(registry *tools.Registry, provider model.Provider, modelName string) {
	if registry == nil {
		return
	}
	_ = registry.Register(NewFluxWorkflowTool(provider, modelName))
	_ = registry.Register(&workflowStatusTool{})
	_ = registry.Register(&workflowListTool{})
	_ = registry.Register(&workflowDefTool{})
	_ = registry.Register(&workflowEventsTool{})
}

// workflowListTool lists all workflows ever submitted in the workspace, with
// their latest task status. Like `ls` for workflows.
type workflowListTool struct{}

func (t *workflowListTool) Name() string { return "workflow_list" }
func (t *workflowListTool) Description() string {
	return "List all plan_workflow runs in this workspace. Returns each workflow's ID, goal, latest task ID, status, progress, and error."
}
func (t *workflowListTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}

func (t *workflowListTool) Execute(ctx context.Context, ec tools.ExecutionContext, _ json.RawMessage) (tools.ToolResult, error) {
	db, err := openWorkflowDB(ec.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{Content: fmt.Sprintf("workflow_list: %v", err)}, nil
	}
	gdb := db.WithContext(ctx)

	type row struct {
		ID           int64  `gorm:"column:id"`
		Status       string `gorm:"column:status"`
		Progress     float64
		ErrorMessage *string `gorm:"column:error_message"`
		InputJSON    []byte  `gorm:"column:input_json"`
		CreatedAt    string  `gorm:"column:created_at"`
	}
	var rows []row
	if err := gdb.Table("tasks").
		Select("id, status, progress, error_message, input_json, created_at").
		Order("id DESC").
		Find(&rows).Error; err != nil {
		return tools.ToolResult{Content: fmt.Sprintf("workflow_list: query tasks: %v", err)}, nil
	}

	type item struct {
		WorkflowID string  `json:"workflow_id"`
		Goal       string  `json:"goal,omitempty"`
		TaskID     int64   `json:"task_id"`
		Status     string  `json:"status"`
		Progress   float64 `json:"progress"`
		Error      string  `json:"error,omitempty"`
		CreatedAt  string  `json:"created_at,omitempty"`
	}
	var items []item
	for _, r := range rows {
		var input struct {
			WorkflowID string `json:"workflow_id"`
			Goal       string `json:"goal"`
		}
		json.Unmarshal(r.InputJSON, &input)
		if input.WorkflowID == "" {
			continue
		}
		errStr := ""
		if r.ErrorMessage != nil {
			errStr = *r.ErrorMessage
		}
		items = append(items, item{
			WorkflowID: input.WorkflowID,
			Goal:       input.Goal,
			TaskID:     r.ID,
			Status:     r.Status,
			Progress:   r.Progress,
			Error:      errStr,
			CreatedAt:  r.CreatedAt,
		})
	}
	out, _ := json.Marshal(items)
	var lines []string
	for _, it := range items {
		errPart := ""
		if it.Error != "" {
			errPart = " error=" + it.Error
		}
		goal := it.Goal
		if len(goal) > 80 {
			goal = goal[:80] + "..."
		}
		lines = append(lines, fmt.Sprintf("%s | %s | %s | progress=%.1f%s | task=%d",
			it.WorkflowID, it.Status, goal, it.Progress, errPart, it.TaskID))
	}
	content := fmt.Sprintf("%d workflows:\n%s", len(items), strings.Join(lines, "\n"))
	return tools.ToolResult{Content: content, Output: out}, nil
}

// workflowDefTool returns the DAG topology (nodes + edges) for a workflow.
type workflowDefTool struct{}

func (t *workflowDefTool) Name() string { return "workflow_definition" }
func (t *workflowDefTool) Description() string {
	return "Get the DAG topology (nodes and edges) for a workflow. The client can render this as a graph."
}
func (t *workflowDefTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"workflow_id":{"type":"string","description":"The workflow ID to get the DAG for"}},"required":["workflow_id"],"additionalProperties":false}`)
}

func (t *workflowDefTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	var args struct {
		WorkflowID string `json:"workflow_id"`
	}
	if err := json.Unmarshal(input, &args); err != nil || args.WorkflowID == "" {
		return tools.ToolResult{Content: "workflow_id is required"}, nil
	}
	db, err := openWorkflowDB(ec.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{Content: fmt.Sprintf("workflow_definition: %v", err)}, nil
	}
	gdb := db.WithContext(ctx)

	// Find latest task for this workflow_id, then its version definition.
	type taskRow struct {
		ID                int64  `gorm:"column:id"`
		WorkflowVersionID int64  `gorm:"column:workflow_version_id"`
		InputJSON         []byte `gorm:"column:input_json"`
	}
	var tasks []taskRow
	if err := gdb.Table("tasks").
		Select("id, workflow_version_id, input_json").
		Order("id DESC").
		Find(&tasks).Error; err != nil {
		return tools.ToolResult{Content: fmt.Sprintf("workflow_definition: query tasks: %v", err)}, nil
	}
	var versionID int64
	var goal string
	for _, tsk := range tasks {
		var input struct {
			WorkflowID string `json:"workflow_id"`
			Goal       string `json:"goal"`
		}
		json.Unmarshal(tsk.InputJSON, &input)
		if input.WorkflowID == args.WorkflowID {
			versionID = tsk.WorkflowVersionID
			goal = input.Goal
			break
		}
	}
	if versionID == 0 {
		return tools.ToolResult{Content: fmt.Sprintf("workflow not found: %s", args.WorkflowID)}, nil
	}

	var defJSON string
	if err := gdb.Table("workflow_versions").
		Select("definition_json").
		Where("id = ?", versionID).
		Scan(&defJSON).Error; err != nil || defJSON == "" {
		return tools.ToolResult{Content: fmt.Sprintf("workflow_definition: version not found: %v", err)}, nil
	}

	var storedDef struct {
		Nodes []struct {
			Name      string         `json:"name"`
			Type      string         `json:"type"`
			Tool      string         `json:"tool"`
			DependsOn []string       `json:"depends_on"`
			Config    map[string]any `json:"config"`
		} `json:"nodes"`
		Edges []struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"edges,omitempty"`
	}
	if err := json.Unmarshal([]byte(defJSON), &storedDef); err != nil {
		return tools.ToolResult{Content: fmt.Sprintf("workflow_definition: decode version: %v", err)}, nil
	}
	type nodeItem struct {
		Name      string   `json:"name"`
		Type      string   `json:"type"`
		Tool      string   `json:"tool,omitempty"`
		DependsOn []string `json:"depends_on,omitempty"`
	}
	nodes := make([]nodeItem, 0, len(storedDef.Nodes))
	byName := make(map[string]int, len(storedDef.Nodes))
	for _, node := range storedDef.Nodes {
		toolName, _ := node.Config["tool"].(string)
		if toolName == "" {
			toolName = node.Tool
		}
		byName[node.Name] = len(nodes)
		nodes = append(nodes, nodeItem{Name: node.Name, Type: node.Type, Tool: toolName, DependsOn: node.DependsOn})
	}
	if len(storedDef.Edges) == 0 {
		for _, node := range nodes {
			for _, dependency := range node.DependsOn {
				storedDef.Edges = append(storedDef.Edges, struct {
					From string `json:"from"`
					To   string `json:"to"`
				}{From: dependency, To: node.Name})
			}
		}
	}
	for _, edge := range storedDef.Edges {
		if index, ok := byName[edge.To]; ok {
			if !containsString(nodes[index].DependsOn, edge.From) {
				nodes[index].DependsOn = append(nodes[index].DependsOn, edge.From)
			}
		}
	}

	result := map[string]any{
		"workflow_id": args.WorkflowID,
		"goal":        goal,
		"nodes":       nodes,
		"edges":       storedDef.Edges,
	}
	out, _ := json.Marshal(result)
	var nodeDescs []string
	for _, n := range nodes {
		deps := ""
		if len(n.DependsOn) > 0 {
			deps = " ← " + strings.Join(n.DependsOn, ", ")
		}
		kind := n.Type
		if n.Tool != "" {
			kind = n.Tool
		}
		nodeDescs = append(nodeDescs, fmt.Sprintf("  %s (%s)%s", n.Name, kind, deps))
	}
	content := fmt.Sprintf("workflow %s: %d nodes, %d edges\n%s",
		args.WorkflowID, len(nodes), len(storedDef.Edges), strings.Join(nodeDescs, "\n"))
	return tools.ToolResult{Content: content, Output: out}, nil
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

// workflowEventsTool returns event history for a workflow task.
type workflowEventsTool struct{}

func (t *workflowEventsTool) Name() string { return "workflow_events" }
func (t *workflowEventsTool) Description() string {
	return "Get the event history for a workflow task. Returns node state changes, task transitions, and any errors. Use limit and after_id for pagination."
}
func (t *workflowEventsTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"workflow_id":{"type":"string","description":"The workflow ID to get events for"},"limit":{"type":"integer","description":"Max events to return (default 20)"},"after_id":{"type":"integer","description":"Return only events with id > after_id. Use the last event's id from the previous page as the cursor."}},"required":["workflow_id"],"additionalProperties":false}`)
}

func (t *workflowEventsTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	var args struct {
		WorkflowID string `json:"workflow_id"`
		Limit      int    `json:"limit"`
		AfterID    int64  `json:"after_id"`
	}
	if err := json.Unmarshal(input, &args); err != nil || args.WorkflowID == "" {
		return tools.ToolResult{Content: "workflow_id is required"}, nil
	}
	if args.Limit <= 0 || args.Limit > 50 {
		args.Limit = 20
	}
	db, err := openWorkflowDB(ec.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{Content: fmt.Sprintf("workflow_events: %v", err)}, nil
	}
	gdb := db.WithContext(ctx)

	// Find the latest task for this workflow_id.
	type taskRow struct {
		ID        int64  `gorm:"column:id"`
		InputJSON []byte `gorm:"column:input_json"`
	}
	var tasks []taskRow
	if err := gdb.Table("tasks").Select("id, input_json").Order("id DESC").Find(&tasks).Error; err != nil {
		return tools.ToolResult{Content: fmt.Sprintf("workflow_events: query tasks: %v", err)}, nil
	}
	var taskID int64
	for _, tsk := range tasks {
		var inp struct {
			WorkflowID string `json:"workflow_id"`
		}
		json.Unmarshal(tsk.InputJSON, &inp)
		if inp.WorkflowID == args.WorkflowID {
			taskID = tsk.ID
			break
		}
	}
	if taskID == 0 {
		return tools.ToolResult{Content: fmt.Sprintf("workflow not found: %s", args.WorkflowID)}, nil
	}

	type evtRow struct {
		ID        int64  `gorm:"column:id"`
		Type      string `gorm:"column:type"`
		Message   string `gorm:"column:message"`
		Meta      []byte `gorm:"column:meta"`
		CreatedAt string `gorm:"column:created_at"`
	}
	var evts []evtRow
	query := gdb.Table("task_events").
		Select("id, type, message, meta, created_at").
		Where("task_id = ?", taskID)
	if args.AfterID > 0 {
		query = query.Where("id > ?", args.AfterID)
	}
	if err := query.Order("id ASC").Limit(args.Limit).Find(&evts).Error; err != nil {
		return tools.ToolResult{Content: fmt.Sprintf("workflow_events: query events: %v", err)}, nil
	}

	type evt struct {
		ID        int64          `json:"id"`
		Type      string         `json:"type"`
		Message   string         `json:"message"`
		Meta      map[string]any `json:"meta,omitempty"`
		CreatedAt string         `json:"created_at,omitempty"`
	}
	var result []evt
	for _, e := range evts {
		item := evt{ID: e.ID, Type: e.Type, Message: e.Message, CreatedAt: e.CreatedAt}
		if len(e.Meta) > 0 {
			json.Unmarshal(e.Meta, &item.Meta)
		}
		result = append(result, item)
	}
	out, _ := json.Marshal(result)
	var lines []string
	var lastID int64
	for _, e := range result {
		metaStr := ""
		if from, ok := e.Meta["from"].(string); ok {
			if to, ok2 := e.Meta["to"].(string); ok2 {
				metaStr = fmt.Sprintf(" %s→%s", from, to)
			}
		}
		if e.Message != "" && metaStr == "" {
			metaStr = " " + e.Message
		}
		lines = append(lines, fmt.Sprintf("  [%d] %s%s", e.ID, e.Type, metaStr))
		lastID = e.ID
	}
	header := fmt.Sprintf("%d events for task %d", len(result), taskID)
	if args.AfterID > 0 {
		header += fmt.Sprintf(" (after id %d)", args.AfterID)
	}
	if len(result) > 0 {
		header += fmt.Sprintf(", next cursor: after_id=%d", lastID)
	}
	content := header + ":\n" + strings.Join(lines, "\n")
	return tools.ToolResult{Content: content, Output: out}, nil
}

// openWorkflowDB opens the workspace-shared flux-workflow database.
func openWorkflowDB(workspaceRoot string) (*gorm.DB, error) {
	rt, err := getOrCreateRuntime(context.Background(), workspaceRoot)
	if err != nil {
		return nil, err
	}
	return rt.DB(), nil
}

// workflowStatusTool lets the model query a previously-submitted workflow's
// current state.
type workflowStatusTool struct{}

func (t *workflowStatusTool) Name() string { return "workflow_status" }
func (t *workflowStatusTool) Description() string {
	return "Query the current state of a previously-submitted plan_workflow. Returns the task status, each node's state and error, and any output produced so far."
}
func (t *workflowStatusTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"workflow_id":{"type":"string","description":"The workflow ID returned by plan_workflow"}},"required":["workflow_id"],"additionalProperties":false}`)
}

func (t *workflowStatusTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	var args struct {
		WorkflowID string `json:"workflow_id"`
	}
	if err := json.Unmarshal(input, &args); err != nil || args.WorkflowID == "" {
		return tools.ToolResult{Content: "workflow_id is required"}, nil
	}
	snapFunc := NewWorkflowSnapshotFunc()
	snap, err := snapFunc(ctx, ec.WorkspaceRoot, args.WorkflowID)
	if err != nil {
		return tools.ToolResult{Content: fmt.Sprintf("workflow_status: %v", err)}, nil
	}
	out, _ := json.Marshal(snap)
	return tools.ToolResult{Content: string(out), Output: out}, nil
}

func fluxWorkflowID(ec tools.ExecutionContext, goal string) string {
	h := sha256.Sum256([]byte(ec.SessionID + "\x00" + goal))
	return "wf_" + hex.EncodeToString(h[:8])
}

func newFluxRuntime(workspaceRoot string) (*workflowruntime.Runtime, error) {
	root := workspaceRoot
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	dir := filepath.Join(root, ".codeagent", "flux-workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return workflowruntime.NewLocal(filepath.Join(dir, "flux-workflows.db"))
}

func projectFluxTools(ctx context.Context, ec tools.ExecutionContext, usage *nestedUsageCollector) *fluxtool.Registry {
	reg := fluxtool.NewRegistry()
	visible := ec.ToolRegistry.Visible()
	sort.SliceStable(visible, func(i, j int) bool { return visible[i].Name() < visible[j].Name() })
	for _, candidate := range visible {
		if candidate == nil || fluxExcludedTools[candidate.Name()] {
			continue
		}
		reg.Register(&codeAgentFluxTool{tool: candidate, ec: ec, usage: usage, attempts: map[string]int{}})
	}
	return reg
}

type codeAgentFluxTool struct {
	tool     tools.Tool
	ec       tools.ExecutionContext
	usage    *nestedUsageCollector
	mu       sync.Mutex
	attempts map[string]int
}

func (t *codeAgentFluxTool) Name() string                 { return t.tool.Name() }
func (t *codeAgentFluxTool) Description() string          { return t.tool.Description() }
func (t *codeAgentFluxTool) Mode() fluxtool.ExecutionMode { return fluxtool.SyncExecution }
func (t *codeAgentFluxTool) InputSchema() fluxtool.DataSchema {
	return fluxDataSchema(t.tool.InputSchema())
}
func (t *codeAgentFluxTool) OutputSchema() fluxtool.DataSchema {
	return fluxtool.DataSchema{Fields: map[string]fluxtool.FieldSchema{
		"content": {Type: "string", Desc: "工具的文本结果"},
		"output":  {Type: "object", Desc: "工具的结构化结果"},
		"assets":  {Type: "array", Desc: "工具产生的资源"},
	}}
}

func (t *codeAgentFluxTool) isClientTool() bool {
	clientTool, ok := t.tool.(tools.ClientTool)
	return ok && clientTool.ExecutionMode() == tools.ExecStrictClient
}

func (t *codeAgentFluxTool) invoke(ctx context.Context, nodeName string, input map[string]any) (tools.ToolResult, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return tools.ToolResult{}, err
	}
	if nodeName == "" {
		nodeName = t.tool.Name()
	}
	t.mu.Lock()
	t.attempts[nodeName]++
	attempt := t.attempts[nodeName]
	t.mu.Unlock()
	callID := fmt.Sprintf("%s:%s:attempt-%d", t.ec.CallID, nodeName, attempt)
	return t.ec.NestedExecutor.ExecuteNestedTool(ctx, t.ec.CallID, callID, t.tool, raw)
}

func (t *codeAgentFluxTool) Execute(execCtx context.Context, input map[string]any, _ fluxtool.ToolEmitter) (*fluxtool.Result, error) {
	meta, _ := fluxtool.ExecutionMetaFromContext(execCtx)
	result, err := t.invoke(execCtx, meta.NodeName, input)
	if err != nil {
		return fluxtool.Fail(err), nil
	}
	if result.Usage != nil {
		t.usage.add(result.Usage)
	}
	return fluxtool.Success(fluxToolResultData(result)), nil
}

func fluxToolResultData(result tools.ToolResult) map[string]any {
	data := map[string]any{"content": result.Content}
	if len(result.Output) > 0 {
		var output any
		if json.Unmarshal(result.Output, &output) == nil {
			data["output"] = output
			// Preserve the structured envelope for clients, while also exposing
			// object fields directly so downstream DAG input mappings can refer to
			// node.field just like native Flux tools do.
			if fields, ok := output.(map[string]any); ok {
				for key, value := range fields {
					if _, reserved := data[key]; !reserved {
						data[key] = value
					}
				}
			}
		}
	}
	if len(result.Assets) > 0 {
		data["assets"] = result.Assets
	}
	return data
}

func fluxDataSchema(raw json.RawMessage) fluxtool.DataSchema {
	var schema struct {
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	_ = json.Unmarshal(raw, &schema)
	required := map[string]bool{}
	for _, name := range schema.Required {
		required[name] = true
	}
	fields := make(map[string]fluxtool.FieldSchema, len(schema.Properties))
	for name, property := range schema.Properties {
		fieldType := property.Type
		if fieldType == "" {
			fieldType = "object"
		}
		fields[name] = fluxtool.FieldSchema{Type: fieldType, Required: required[name], Desc: property.Description}
	}
	return fluxtool.DataSchema{Fields: fields}
}

type fluxCompleterAdapter struct {
	provider model.Provider
	ec       tools.ExecutionContext
	usage    *fluxUsageCollector
}

func (a *fluxCompleterAdapter) Complete(ctx context.Context, req fluxmodel.Request) (fluxmodel.Response, error) {
	converted := model.Request{
		SessionID: a.ec.SessionID, TurnID: a.ec.TurnID, RequestID: a.ec.RequestID, ExecutionID: a.ec.ExecutionID,
		Model: req.Model, Temperature: req.Temperature, ToolChoice: stringToolChoice(req.ToolChoice),
	}
	for _, message := range req.Messages {
		converted.Messages = append(converted.Messages, model.Message{
			Role: model.Role(message.Role), Content: message.Content, ToolCallID: message.ToolCallID,
			ToolCalls: toCodeAgentToolCalls(message.ToolCalls),
		})
	}
	for _, def := range req.Tools {
		params, _ := json.Marshal(def.Function.Parameters)
		converted.Tools = append(converted.Tools, model.ToolDefinition{Type: def.Type, Function: model.ToolFunction{
			Name: def.Function.Name, Description: def.Function.Description, Parameters: params,
		}})
	}
	response, err := a.provider.Complete(ctx, converted)
	if err != nil {
		return fluxmodel.Response{}, err
	}
	a.usage.add(response.Usage)
	return fluxmodel.Response{
		Content: response.Content, ToolCalls: toFluxToolCalls(response.ToolCalls), FinishReason: response.FinishReason,
		Usage: fluxmodel.Usage{PromptTokens: response.Usage.PromptTokens, CompletionTokens: response.Usage.CompletionTokens,
			TotalTokens: response.Usage.TotalTokens, CachedPromptTokens: response.Usage.CachedPromptTokens}, Raw: response.Raw,
	}, nil
}

func stringToolChoice(choice any) string { value, _ := choice.(string); return value }

func toCodeAgentToolCalls(calls []fluxmodel.ToolCall) []model.ToolCall {
	out := make([]model.ToolCall, 0, len(calls))
	for _, call := range calls {
		out = append(out, model.ToolCall{ID: call.ID, Type: call.Type, Function: model.FunctionCall{Name: call.Function.Name, Arguments: call.Function.Arguments}})
	}
	return out
}

func toFluxToolCalls(calls []model.ToolCall) []fluxmodel.ToolCall {
	out := make([]fluxmodel.ToolCall, 0, len(calls))
	for _, call := range calls {
		out = append(out, fluxmodel.ToolCall{ID: call.ID, Type: call.Type, Function: fluxmodel.FunctionCall{Name: call.Function.Name, Arguments: call.Function.Arguments}})
	}
	return out
}

type fluxUsageCollector struct {
	mu    sync.Mutex
	usage tools.ModelUsage
}

func (c *fluxUsageCollector) add(u model.Usage) {
	c.mu.Lock()
	c.usage.PromptTokens += u.PromptTokens
	c.usage.CompletionTokens += u.CompletionTokens
	c.usage.TotalTokens += u.TotalTokens
	c.usage.BillingUnits += u.BillingUnits
	c.usage.CachedPromptTokens += u.CachedPromptTokens
	c.mu.Unlock()
}
func (c *fluxUsageCollector) snapshot() *tools.ModelUsage {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := c.usage
	return &u
}

type nestedUsageCollector struct {
	mu     sync.Mutex
	usages []*tools.ToolUsage
}

func (c *nestedUsageCollector) add(u *tools.ToolUsage) {
	if u == nil {
		return
	}
	c.mu.Lock()
	copy := *u
	c.usages = append(c.usages, &copy)
	c.mu.Unlock()
}
func (c *nestedUsageCollector) snapshot() []*tools.ToolUsage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*tools.ToolUsage, len(c.usages))
	copy(out, c.usages)
	return out
}

// bridgeFluxEventsAsync subscribes to the Engine event bus before Submit and
// returns a channel that closes when the task reaches a terminal state. Events
// are forwarded to the code-agent stream via emitWorkflow.
func bridgeFluxEventsAsync(ctx context.Context, rt *workflowruntime.Runtime, ec tools.ExecutionContext, workflowID string) func(int64) {
	types := []string{
		domain.TaskEventStarted, domain.TaskEventSucceeded, domain.TaskEventFailed, domain.TaskEventSuspended,
		"task_state_changed", "node_state_changed", "task_progress",
		"node_progress", "node_debug", "tool_progress", "tool_log", "tool_stream", "tool_stream_end", "fanout_progress",
	}

	type sub struct {
		ch     <-chan *domain.TaskEvent
		cancel func()
	}
	var subs []sub
	for _, eventType := range types {
		ch, c := rt.Subscribe(eventType)
		subs = append(subs, sub{ch, c})
	}

	termCh, termCancel := rt.Subscribe(domain.TaskEventSucceeded)
	failCh, failCancel := rt.Subscribe(domain.TaskEventFailed)
	taskIDCh := make(chan int64, 1)

	go func() {
		defer func() {
			termCancel()
			failCancel()
			for _, s := range subs {
				s.cancel()
			}
		}()
		var rootTaskID int64
		select {
		case <-ctx.Done():
			return
		case rootTaskID = <-taskIDCh:
		}
		for _, s := range subs {
			go func(ch <-chan *domain.TaskEvent) {
				for event := range ch {
					if event.TaskID != rootTaskID && event.RootTaskID != rootTaskID {
						continue
					}
					payload := flattenFluxTaskEvent(event, workflowID, ec.CallID)
					kind := "workflow_" + event.Type
					emitWorkflow(ec, kind, payload)
				}
			}(s.ch)
		}
		// Only terminal events end the bridge. Suspension is transient —
		// the forwarder goroutines above emit workflow_task_suspended, and
		// the worker will resume the task when the client returns its result.
		var terminalEv *domain.TaskEvent
		for terminalEv == nil {
			select {
			case <-ctx.Done():
				return
			case ev := <-termCh:
				if ev != nil && ev.TaskID == rootTaskID {
					terminalEv = ev
				}
			case ev := <-failCh:
				if ev != nil && ev.TaskID == rootTaskID {
					terminalEv = ev
				}
			}
		}
		if terminalEv != nil {
			emitWorkflowTerminalWithTask(ctx, rt, ec, workflowID, terminalEv)
		}
	}()
	return func(taskID int64) {
		select {
		case taskIDCh <- taskID:
		case <-ctx.Done():
		}
	}
}

// bridgeFluxEvents subscribes to the Engine event bus and forwards events into
// the code-agent event stream. Used for the synchronous retry path.
func bridgeFluxEvents(rt *workflowruntime.Runtime, ec tools.ExecutionContext, workflowID string, taskID int64) (cancel func()) {
	_ = taskID // reserved for sync retry filtering
	types := []string{
		domain.TaskEventStarted, domain.TaskEventSucceeded, domain.TaskEventFailed, domain.TaskEventSuspended,
		"task_state_changed", "node_state_changed", "task_progress",
		"node_progress", "node_debug", "tool_progress", "tool_log", "tool_stream", "tool_stream_end", "fanout_progress",
	}

	type sub struct {
		ch     <-chan *domain.TaskEvent
		cancel func()
	}
	var subs []sub
	for _, eventType := range types {
		ch, c := rt.Subscribe(eventType)
		subs = append(subs, sub{ch, c})
	}

	var wg sync.WaitGroup
	for _, s := range subs {
		wg.Add(1)
		go func(ch <-chan *domain.TaskEvent) {
			defer wg.Done()
			for event := range ch {
				payload := flattenFluxTaskEvent(event, workflowID, ec.CallID)
				kind := "workflow_" + event.Type
				emitWorkflow(ec, kind, payload)
			}
		}(s.ch)
	}
	return func() {
		for _, s := range subs {
			s.cancel()
		}
		wg.Wait()
	}
}

func flattenFluxTaskEvent(event *domain.TaskEvent, workflowID, parentCallID string) map[string]any {
	payload := map[string]any{
		"workflow_id": workflowID, "parent_call_id": parentCallID,
		"task_id": event.TaskID, "root_task_id": event.RootTaskID,
		"message": event.Message, "error": event.Error, "progress": event.Progress,
		"sequence": event.Sequence, "created_at": event.CreatedAt,
	}
	if event.Step != "" && event.Step != "task" {
		payload["node_name"] = event.Step
	}
	for key, value := range event.Meta {
		payload[key] = value
	}
	return payload
}

func emitWorkflow(ec tools.ExecutionContext, kind string, payload map[string]any) {
	if ec.OnWorkflowEvent == nil {
		return
	}
	raw, err := json.Marshal(payload)
	if err == nil {
		ec.OnWorkflowEvent(kind, raw)
	}
}

func emitWorkflowTerminal(ec tools.ExecutionContext, workflowID, status, errMsg string, ev *domain.TaskEvent) {
	payload := map[string]any{
		"workflow_id": workflowID,
		"status":      status,
	}
	if ev != nil {
		payload["task_id"] = ev.TaskID
	}
	if errMsg != "" {
		payload["error"] = errMsg
	}
	kind := "workflow_finished"
	if status == "failed" {
		kind = "workflow_failed"
	}
	emitWorkflow(ec, kind, payload)
}

// emitWorkflowTerminalWithTask queries the task output from the Runtime and
// includes it in the terminal event. Used by the async bridge goroutine which
// has access to the Runtime but not the synchronous Run result.
func emitWorkflowTerminalWithTask(ctx context.Context, rt *workflowruntime.Runtime, ec tools.ExecutionContext, workflowID string, ev *domain.TaskEvent) {
	status := "success"
	errMsg := ""
	if ev != nil && ev.Type == domain.TaskEventFailed {
		status = "failed"
		if ev.Error != "" {
			errMsg = ev.Error
		}
	}
	payload := map[string]any{
		"workflow_id": workflowID,
		"status":      status,
	}
	if ev != nil {
		payload["task_id"] = ev.TaskID
		task, err := rt.Status(ctx, ev.TaskID)
		if err == nil && task != nil {
			if task.ErrorMessage != "" {
				payload["error"] = task.ErrorMessage
			}
			if len(task.OutputJSON) > 0 {
				var output any
				if json.Unmarshal(task.OutputJSON, &output) == nil {
					payload["output"] = output
				}
			}
		}
	}
	if errMsg != "" && payload["error"] == nil {
		payload["error"] = errMsg
	}
	kind := "workflow_finished"
	if status == "failed" {
		kind = "workflow_failed"
	}
	emitWorkflow(ec, kind, payload)
}

func emitWorkflowFailure(ec tools.ExecutionContext, workflowID, stage, message string) {
	emitWorkflow(ec, "workflow_failed", map[string]any{
		"workflow_id":    workflowID,
		"parent_call_id": ec.CallID,
		"stage":          stage,
		"error":          message,
	})
}

// executeRetry recovers a previously persisted Runtime from its durable .db file,
// re-attaches tools and event bridging, and calls Retry on the target task.
func (t *FluxWorkflowTool) executeRetry(
	ctx context.Context,
	ec tools.ExecutionContext,
	args fluxWorkflowInput,
) (tools.ToolResult, error) {
	if args.WorkflowID == "" || args.TaskID == 0 {
		return tools.ToolResult{Content: "retry requires workflow_id and task_id"}, nil
	}

	rt, err := newFluxRuntime(ec.WorkspaceRoot)
	if err != nil {
		return tools.ToolResult{Content: "failed to recover workflow runtime: " + err.Error()}, nil
	}
	defer rt.Shutdown()

	// Re-register tools — the in-memory tool registry is empty after recovery.
	nestedUsage := &nestedUsageCollector{}
	fluxReg := projectFluxTools(ctx, ec, nestedUsage)
	for _, projected := range fluxReg.List() {
		rt.ToolRegistry().Register(projected)
	}

	if err := rt.Start(ctx); err != nil {
		return tools.ToolResult{Content: "failed to start workflow runtime: " + err.Error()}, nil
	}

	emitWorkflow(ec, "workflow_started", map[string]any{
		"workflow_id": args.WorkflowID, "parent_call_id": ec.CallID,
		"stage": "retrying", "task_id": args.TaskID, "resume_from": args.ResumeFrom,
	})

	cancelEvents := bridgeFluxEvents(rt, ec, args.WorkflowID, 0)
	defer cancelEvents()

	// Subscribe to terminal events on a dedicated channel so we can wait.
	terminalCh, terminalCancel := rt.Subscribe(domain.TaskEventSucceeded)
	defer terminalCancel()
	failedCh, failedCancel := rt.Subscribe(domain.TaskEventFailed)
	defer failedCancel()
	suspendedCh, suspendedCancel := rt.Subscribe(domain.TaskEventSuspended)
	defer suspendedCancel()

	if err := rt.Retry(ctx, args.TaskID, args.ResumeFrom, nil); err != nil {
		return tools.ToolResult{Content: "retry failed: " + err.Error()}, nil
	}

	// Wait for the retried task to reach a terminal state.
	// Workers consume the enqueued task and drive the DAG; we block until
	// the task finishes, fails, or is suspended again (e.g. awaiting a client tool).
	for {
		select {
		case <-ctx.Done():
			return tools.ToolResult{Content: "retry cancelled: " + ctx.Err().Error()}, nil
		case evt := <-terminalCh:
			if evt != nil && evt.TaskID == args.TaskID {
				return retryResult(args.WorkflowID, args.TaskID, "success", "", rt)
			}
		case evt := <-failedCh:
			if evt != nil && evt.TaskID == args.TaskID {
				return retryResult(args.WorkflowID, args.TaskID, "failed", evt.Error, rt)
			}
		case evt := <-suspendedCh:
			if evt != nil && evt.TaskID == args.TaskID {
				return retryResult(args.WorkflowID, args.TaskID, "suspended", "", rt)
			}
		}
	}
}

func retryResult(workflowID string, taskID int64, status string, errMsg string, rt *workflowruntime.Runtime) (tools.ToolResult, error) {
	// Best-effort fetch of the updated task to include any partial output.
	task, _ := rt.Status(context.Background(), taskID)
	payload := map[string]any{"workflow_id": workflowID, "task_id": taskID, "status": status}
	if errMsg != "" {
		payload["error"] = errMsg
	}
	if task != nil && len(task.OutputJSON) > 0 {
		var output any
		if json.Unmarshal(task.OutputJSON, &output) == nil {
			payload["output"] = output
		}
	}
	out, _ := json.Marshal(payload)
	content := fmt.Sprintf("retry workflow %s task %d: %s", workflowID, taskID, status)
	if errMsg != "" {
		content += " (" + errMsg + ")"
	}
	return tools.ToolResult{Content: content, Output: out}, nil
}

var _ tools.Tool = (*FluxWorkflowTool)(nil)
