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

	"github.com/tuxi/flux-workflow/domain"
	workflowruntime "github.com/tuxi/flux-workflow/runtime"
	fluxtool "github.com/tuxi/flux-workflow/tool"
	fluxmodel "github.com/tuxi/flux/model"
	"github.com/tuxi/flux/planner"

	"code-agent/internal/model"
	"code-agent/internal/tools"
)

var fluxExcludedTools = map[string]bool{
	"plan_workflow":   true,
	"task":            true,
	"enter_plan_mode": true,
	"propose_plan":    true,
	"ask_user":        true,
	"todo":            true,
	"workflow_status": true,
}

// runtimePool holds one started Runtime per workspace db. After the first
// plan_workflow call starts workers, subsequent calls in the same workspace
// reuse the Runtime so tasks queue up and execute in FIFO order.
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
		workflowruntime.WithTaskWorkers(1),
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
		"For simple single-step changes, skip this and act directly."
}

func (t *FluxWorkflowTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"goal":{"type":"string","description":"A complete description of the multi-step task to execute. Describe every step, their dependencies, and what success looks like. Example: 'Initialize a React+TS project, add ESLint and Prettier configs, create a Button component with tests, then run the test suite.'"},"action":{"type":"string","enum":["new","retry"],"description":"new=create and execute; retry=recover a failed workflow from a specific node"},"workflow_id":{"type":"string","description":"Required for retry: the workflow ID to recover"},"task_id":{"type":"integer","description":"Required for retry: the task ID to retry"},"resume_from":{"type":"string","description":"Optional for retry: node name to resume from; empty auto-collects failed roots"}},"required":["goal"],"additionalProperties":false}`)
}

func (t *FluxWorkflowTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	var args struct {
		Goal       string `json:"goal"`
		Action     string `json:"action"`
		WorkflowID string `json:"workflow_id"`
		TaskID     int64  `json:"task_id"`
		ResumeFrom string `json:"resume_from"`
	}
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
	if t.provider == nil {
		return tools.ToolResult{Content: "flux workflow failed: model provider is unavailable"}, nil
	}
	if ec.ToolRegistry == nil || ec.NestedExecutor == nil {
		return tools.ToolResult{Content: "flux workflow failed: controlled tool execution is unavailable"}, nil
	}

	workflowID := fluxWorkflowID(ec, args.Goal)
	emitWorkflow(ec, "workflow_started", map[string]any{
		"workflow_id": workflowID, "parent_call_id": ec.CallID, "stage": "planning", "goal": args.Goal,
	})
	usage := &fluxUsageCollector{}
	provider := &fluxCompleterAdapter{provider: t.provider, ec: ec, usage: usage}
	nestedUsage := &nestedUsageCollector{}
	fluxReg := projectFluxTools(ctx, ec, nestedUsage)
	if len(fluxReg.List()) == 0 {
		emitWorkflowFailure(ec, workflowID, "planning", "no eligible tools are available")
		return tools.ToolResult{Content: "flux workflow failed: no eligible tools are available"}, nil
	}

	dagPlanner := planner.NewDAGPlanner(provider, t.modelName, args.Goal, fluxReg)
	// Planner LLM calls can easily exceed the HTTP request timeout (large system
	// prompt + complex DAG generation). Use a detached context with a generous
	// deadline while still respecting the tool's own cancellation (e.g. Ctrl-C).
	planCtx, planCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer planCancel()
	def, err := dagPlanner.GenerateWorkflow(planCtx, nil)
	if err != nil {
		emitWorkflowFailure(ec, workflowID, "planning", err.Error())
		return tools.ToolResult{Content: "flux workflow planning failed: " + err.Error(), ModelUsage: usage.snapshot()}, nil
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
	if err := rt.RegisterWorkflow(ctx, def); err != nil {
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
	bridgeFluxEventsAsync(bridgeCtx, rt, ec, workflowID)

	taskID, err := rt.Submit(ctx, def.Name, map[string]any{"goal": args.Goal, "workflow_id": workflowID})
	if err != nil {
		bridgeCancel() // task never enqueued → goroutine would leak
		emitWorkflowFailure(ec, workflowID, "executing", err.Error())
		return tools.ToolResult{Content: "flux workflow submission failed: " + err.Error(), ModelUsage: usage.snapshot()}, nil
	}
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
	out, _ := json.Marshal(payload)
	return tools.ToolResult{
		Content:      string(out),
		Output:       out,
		ModelUsage:   usage.snapshot(),
		NestedUsages: nestedUsage.snapshot(),
	}, nil
}

func RegisterFluxTool(registry *tools.Registry, provider model.Provider, modelName string) {
	if registry == nil {
		return
	}
	_ = registry.Register(NewFluxWorkflowTool(provider, modelName))
	_ = registry.Register(&workflowStatusTool{})
}

// workflowStatusTool lets the model query a previously-submitted workflow's
// current state. After plan_workflow returns {"status":"queued"}, the model
// can call this to check progress before deciding its next action.
type workflowStatusTool struct{}

func (t *workflowStatusTool) Name() string        { return "workflow_status" }
func (t *workflowStatusTool) Description() string { return "Query the current state of a previously-submitted plan_workflow. Returns the task status, each node's state and error, and any output produced so far." }
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
		reg.Register(&codeAgentFluxTool{tool: candidate, ec: ec, parentCtx: ctx, usage: usage, attempts: map[string]int{}})
	}
	return reg
}

type codeAgentFluxTool struct {
	tool      tools.Tool
	ec        tools.ExecutionContext
	parentCtx context.Context
	usage     *nestedUsageCollector
	mu        sync.Mutex
	attempts  map[string]int
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
	result, err := t.invoke(t.parentCtx, meta.NodeName, input)
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
func bridgeFluxEventsAsync(ctx context.Context, rt *workflowruntime.Runtime, ec tools.ExecutionContext, workflowID string) {
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

	go func() {
		defer func() {
			termCancel()
			failCancel()
			for _, s := range subs {
				s.cancel()
			}
		}()
		for _, s := range subs {
			go func(ch <-chan *domain.TaskEvent) {
				for event := range ch {
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
		select {
		case <-ctx.Done():
			return
		case ev := <-termCh:
			terminalEv = ev
		case ev := <-failCh:
			terminalEv = ev
		}
		if terminalEv != nil {
			emitWorkflowTerminalWithTask(ctx, rt, ec, workflowID, terminalEv)
		}
	}()
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
	args struct {
		Goal       string `json:"goal"`
		Action     string `json:"action"`
		WorkflowID string `json:"workflow_id"`
		TaskID     int64  `json:"task_id"`
		ResumeFrom string `json:"resume_from"`
	},
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
