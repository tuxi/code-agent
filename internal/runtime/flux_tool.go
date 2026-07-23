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

	"github.com/tuxi/flux-workflow/definition"
	"github.com/tuxi/flux-workflow/domain"
	"github.com/tuxi/flux-workflow/repository/query"
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
	return "把复杂目标生成并校验为确定性 DAG，再由 Flux Workflow Engine 执行；适合有依赖、并行和可恢复节点的多步任务。"
}

func (t *FluxWorkflowTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"goal":{"type":"string","description":"要完成的复杂目标"}},"required":["goal"],"additionalProperties":false}`)
}

func (t *FluxWorkflowTool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
	var args struct {
		Goal string `json:"goal"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return tools.ToolResult{Content: "invalid input: " + err.Error()}, nil
	}
	args.Goal = strings.TrimSpace(args.Goal)
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
	def, err := dagPlanner.GenerateWorkflow(ctx, nil)
	if err != nil {
		emitWorkflowFailure(ec, workflowID, "planning", err.Error())
		return tools.ToolResult{Content: "flux workflow planning failed: " + err.Error(), ModelUsage: usage.snapshot()}, nil
	}
	rewriteClientToolNodes(def, fluxReg)
	rt, err := newFluxRuntime(ec.WorkspaceRoot, workflowID)
	if err != nil {
		emitWorkflowFailure(ec, workflowID, "initializing", err.Error())
		return tools.ToolResult{Content: "flux workflow runtime failed: " + err.Error(), ModelUsage: usage.snapshot()}, nil
	}
	defer rt.Shutdown()
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

	cancelEvents := bridgeFluxEvents(rt, ec, workflowID)
	result, runErr := rt.Run(ctx, def, map[string]any{"goal": args.Goal})
	for runErr == nil && result != nil && result.Status == "suspended" {
		emitWorkflow(ec, "workflow_suspended", map[string]any{
			"workflow_id": workflowID, "parent_call_id": ec.CallID,
			"task_id": result.TaskID, "status": result.Status,
			"node_name": result.SuspendNode, "reason": result.SuspendReason,
			"resumable": true,
		})
		if !isClientAwaitNode(def, result.SuspendNode) {
			break
		}
		result, runErr = resumeClientAwait(ctx, rt, def, fluxReg, result, nestedUsage)
	}
	cancelEvents()
	if runErr != nil {
		emitWorkflowFailure(ec, workflowID, "executing", runErr.Error())
		return tools.ToolResult{Content: "flux workflow execution failed: " + runErr.Error(), ModelUsage: usage.snapshot(), NestedUsages: nestedUsage.snapshot()}, nil
	}
	if result == nil {
		emitWorkflowFailure(ec, workflowID, "executing", "workflow engine returned no result")
		return tools.ToolResult{Content: "flux workflow execution failed: workflow engine returned no result", ModelUsage: usage.snapshot(), NestedUsages: nestedUsage.snapshot()}, nil
	}

	payload := map[string]any{
		"workflow_id": workflowID,
		"task_id":     result.TaskID,
		"status":      result.Status,
	}
	if result.Task != nil && len(result.Task.OutputJSON) > 0 {
		var output any
		if json.Unmarshal(result.Task.OutputJSON, &output) == nil {
			payload["output"] = output
		}
	}
	if result.Err != nil {
		payload["error"] = result.Err.Error()
	}
	terminalKind := "workflow_finished"
	if result.Status == "failed" || result.Err != nil {
		terminalKind = "workflow_failed"
	} else if result.Status == "suspended" {
		terminalKind = "workflow_suspended"
	}
	// Every suspended result was already emitted inside the resume loop with
	// suspend node/reason. Do not append a duplicate suspension event here.
	if terminalKind != "workflow_suspended" {
		emitWorkflow(ec, terminalKind, payload)
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
}

func fluxWorkflowID(ec tools.ExecutionContext, goal string) string {
	h := sha256.Sum256([]byte(ec.SessionID + "\x00" + ec.TurnID + "\x00" + ec.CallID + "\x00" + goal))
	return "wf_" + hex.EncodeToString(h[:8])
}

func newFluxRuntime(workspaceRoot, workflowID string) (*workflowruntime.Runtime, error) {
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
	return workflowruntime.NewLocal(filepath.Join(dir, workflowID+".db"))
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

func rewriteClientToolNodes(def *definition.WorkflowDefinition, registry *fluxtool.Registry) {
	if def == nil || registry == nil {
		return
	}
	for i := range def.Nodes {
		node := &def.Nodes[i]
		if node.Type != definition.NodeTool || node.Config == nil {
			continue
		}
		toolName, _ := node.Config["tool"].(string)
		projected, ok := registry.Get(toolName)
		if !ok {
			continue
		}
		adapter, ok := projected.(*codeAgentFluxTool)
		if !ok || !adapter.isClientTool() {
			continue
		}
		correlation := make(map[string]any, len(node.InputMapping))
		for inputName := range node.InputMapping {
			correlation[inputName] = inputName
		}
		node.Type = definition.NodeAwait
		node.Config = map[string]any{
			"await_type":  "client_tool",
			"source":      "client",
			"client_tool": toolName,
			"correlation": correlation,
		}
	}
}

func isClientAwaitNode(def *definition.WorkflowDefinition, nodeName string) bool {
	if def == nil || nodeName == "" {
		return false
	}
	for i := range def.Nodes {
		node := &def.Nodes[i]
		if node.Name == nodeName && node.Type == definition.NodeAwait && node.Config != nil {
			_, ok := node.Config["client_tool"].(string)
			return ok
		}
	}
	return false
}

func resumeClientAwait(
	ctx context.Context,
	rt *workflowruntime.Runtime,
	def *definition.WorkflowDefinition,
	registry *fluxtool.Registry,
	suspended *workflowruntime.RunResult,
	usage *nestedUsageCollector,
) (*workflowruntime.RunResult, error) {
	if suspended == nil || rt == nil {
		return nil, fmt.Errorf("client await: workflow result or runtime is nil")
	}
	toolName := ""
	for i := range def.Nodes {
		if def.Nodes[i].Name == suspended.SuspendNode {
			toolName, _ = def.Nodes[i].Config["client_tool"].(string)
			break
		}
	}
	projected, ok := registry.Get(toolName)
	if !ok {
		return completeFluxAwait(ctx, rt, suspended.TaskID, suspended.SuspendNode, nil, "client tool not found: "+toolName)
	}
	adapter, ok := projected.(*codeAgentFluxTool)
	if !ok {
		return completeFluxAwait(ctx, rt, suspended.TaskID, suspended.SuspendNode, nil, "client tool adapter is unavailable: "+toolName)
	}
	binding, err := query.NewAwaitBindingRepository(rt.DB()).GetByTaskAndNode(ctx, suspended.TaskID, suspended.SuspendNode)
	if err != nil {
		return nil, fmt.Errorf("load client await binding: %w", err)
	}
	input := map[string]any{}
	if binding != nil && binding.Correlation != nil {
		input = binding.Correlation
	}
	result, invokeErr := adapter.invoke(ctx, suspended.SuspendNode, input)
	if result.Usage != nil {
		usage.add(result.Usage)
	}
	if invokeErr != nil {
		return completeFluxAwait(ctx, rt, suspended.TaskID, suspended.SuspendNode, nil, invokeErr.Error())
	}
	return completeFluxAwait(ctx, rt, suspended.TaskID, suspended.SuspendNode, fluxToolResultData(result), "")
}

// completeFluxAwait intentionally uses the public Engine/DB surface available
// since flux-workflow v1.0.3. Newer flux-workflow releases expose equivalent
// Runtime.AwaitBinding/CompleteAwait helpers, but code-agent remains independently
// buildable while the three repositories are released in sequence.
func completeFluxAwait(
	ctx context.Context,
	rt *workflowruntime.Runtime,
	taskID int64,
	nodeName string,
	meta map[string]any,
	errMsg string,
) (*workflowruntime.RunResult, error) {
	engineResult := rt.Engine().CompleteNodeAndResume(taskID, nodeName, meta, errMsg)
	task, statusErr := rt.Status(ctx, taskID)
	if statusErr != nil {
		return nil, statusErr
	}
	status := string(engineResult.Status)
	if task != nil {
		status = string(task.Status)
	}
	return &workflowruntime.RunResult{
		TaskID: taskID, Status: status, Err: engineResult.Err, Task: task,
		SuspendReason: engineResult.SuspendReason, SuspendNode: engineResult.SuspendNode,
	}, nil
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

func bridgeFluxEvents(rt *workflowruntime.Runtime, ec tools.ExecutionContext, workflowID string) func() {
	types := []string{
		domain.TaskEventStarted, domain.TaskEventSucceeded, domain.TaskEventFailed, domain.TaskEventSuspended,
		"task_state_changed", "node_state_changed", "task_progress",
		"node_progress", "node_debug", "tool_progress", "tool_log", "tool_stream", "tool_stream_end", "fanout_progress",
	}
	var cancels []func()
	var wg sync.WaitGroup
	for _, eventType := range types {
		ch, cancel := rt.Subscribe(eventType)
		cancels = append(cancels, cancel)
		wg.Add(1)
		go func(ch <-chan *domain.TaskEvent) {
			defer wg.Done()
			for event := range ch {
				payload := flattenFluxTaskEvent(event, workflowID, ec.CallID)
				kind := "workflow_" + event.Type
				emitWorkflow(ec, kind, payload)
			}
		}(ch)
	}
	return func() {
		for _, cancel := range cancels {
			cancel()
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

func emitWorkflowFailure(ec tools.ExecutionContext, workflowID, stage, message string) {
	emitWorkflow(ec, "workflow_failed", map[string]any{
		"workflow_id":    workflowID,
		"parent_call_id": ec.CallID,
		"stage":          stage,
		"error":          message,
	})
}

var _ tools.Tool = (*FluxWorkflowTool)(nil)
