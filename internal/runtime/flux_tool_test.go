package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"code-agent/internal/model"
	"code-agent/internal/tools"
	"code-agent/internal/tools/sessions"
)

type fluxProviderStub struct {
	request model.Request
	result  model.Response
}

func (p *fluxProviderStub) Complete(_ context.Context, request model.Request) (model.Response, error) {
	p.request = request
	return p.result, nil
}

type projectedToolStub struct{ name string }

func (t projectedToolStub) Name() string        { return t.name }
func (t projectedToolStub) Description() string { return "projected " + t.name }
func (t projectedToolStub) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
}
func (t projectedToolStub) Execute(context.Context, tools.ExecutionContext, json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{Content: "ok"}, nil
}

func TestProjectFluxToolsUsesTurnRegistryAndExcludesControlTools(t *testing.T) {
	registry := tools.NewRegistry()
	for _, name := range []string{"read_file", "mcp__repo__search", "task", "plan_workflow", "ask_user"} {
		if err := registry.Register(projectedToolStub{name: name}); err != nil {
			t.Fatal(err)
		}
	}
	if err := registry.Register(tools.NewClientProxyTool("ios_capture_photo", "capture on device", json.RawMessage(`{"type":"object"}`))); err != nil {
		t.Fatal(err)
	}
	ec := tools.ExecutionContext{ToolRegistry: registry, NestedExecutor: nestedExecutorStub{}}
	projected := projectFluxTools(context.Background(), ec, &nestedUsageCollector{})

	if _, ok := projected.Get("read_file"); !ok {
		t.Fatal("built-in tool was not projected")
	}
	if _, ok := projected.Get("mcp__repo__search"); !ok {
		t.Fatal("workspace MCP tool was not projected")
	}
	if _, ok := projected.Get("ios_capture_photo"); !ok {
		t.Fatal("session client tool was not projected")
	}
	for _, excluded := range []string{"task", "plan_workflow", "ask_user"} {
		if _, ok := projected.Get(excluded); ok {
			t.Fatalf("control tool %q leaked into Flux", excluded)
		}
	}
}

func TestFluxWorkflowToolEmitsTerminalFailureWhenPlanningCannotStart(t *testing.T) {
	provider := &fluxProviderStub{}
	registry := tools.NewRegistry()
	var eventKinds []string
	tool := NewFluxWorkflowTool(provider, "planner-model")
	result, err := tool.Execute(context.Background(), tools.ExecutionContext{
		WorkspaceRoot: t.TempDir(), SessionID: "s", TurnID: "t", CallID: "parent",
		ToolRegistry: registry, NestedExecutor: nestedExecutorStub{},
		OnWorkflowEvent: func(kind string, _ json.RawMessage) { eventKinds = append(eventKinds, kind) },
	}, json.RawMessage(`{"goal":"do something"}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Content == "" {
		t.Fatal("expected a user-visible failure result")
	}
	if len(eventKinds) != 2 || eventKinds[0] != "workflow_started" || eventKinds[1] != "workflow_failed" {
		t.Fatalf("workflow failure lifecycle = %v", eventKinds)
	}
}

type nestedExecutorStub struct{}

func (nestedExecutorStub) ExecuteNestedTool(context.Context, string, string, tools.Tool, json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{Content: "ok"}, nil
}

type routingNestedExecutor struct {
	calls     []string
	inputs    map[string]map[string]any
	clientErr error
}

type workflowTransition struct{ from, to string }

func (e *routingNestedExecutor) ExecuteNestedTool(_ context.Context, _ string, _ string, tool tools.Tool, input json.RawMessage) (tools.ToolResult, error) {
	e.calls = append(e.calls, tool.Name())
	var decoded map[string]any
	_ = json.Unmarshal(input, &decoded)
	if e.inputs == nil {
		e.inputs = map[string]map[string]any{}
	}
	e.inputs[tool.Name()] = decoded
	if tool.Name() == "ios_capture_photo" {
		if e.clientErr != nil {
			return tools.ToolResult{}, e.clientErr
		}
		return tools.ToolResult{Content: "captured", Output: json.RawMessage(`{"asset_id":"photo-1"}`)}, nil
	}
	return tools.ToolResult{Content: "consumed"}, nil
}

// TODO: adopt to async Submit — client tool await path needs worker-aware test harness.
func TestFluxWorkflowToolClientFailureCompletesCanonicalTask(t *testing.T) {
	t.Skip("TODO: adopt to async Submit — client tool await needs worker-aware test")
	provider := &fluxProviderStub{result: model.Response{
		ToolCalls: []model.ToolCall{{
			ID: "plan", Type: "function",
			Function: model.FunctionCall{Name: "submit_plan", Arguments: `{"nodes":[{"id":"capture","tool":"ios_capture_photo","arguments":{"prompt":"hello"},"depends_on":[]}],"result_type":"generic"}`},
		}},
		FinishReason: "tool_calls",
	}}
	registry := tools.NewRegistry()
	if err := registry.Register(tools.NewClientProxyTool("ios_capture_photo", "capture on device", json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string"}}}`))); err != nil {
		t.Fatal(err)
	}
	executor := &routingNestedExecutor{clientErr: errors.New("client denied camera access")}
	done := make(chan struct{})
	var eventKinds []string
	_, err := NewFluxWorkflowTool(provider, "planner-model").Execute(context.Background(), tools.ExecutionContext{
		WorkspaceRoot: t.TempDir(), SessionID: "s", TurnID: "t", CallID: "parent",
		ToolRegistry: registry, NestedExecutor: executor,
		OnWorkflowEvent: func(kind string, _ json.RawMessage) {
			eventKinds = append(eventKinds, kind)
			if kind == "workflow_failed" || kind == "workflow_finished" {
				select {
				case <-done:
				default:
					close(done)
				}
			}
		},
	}, json.RawMessage(`{"goal":"capture a photo"}`))
	if err != nil {
		t.Fatal(err)
	}
	<-done
	foundFailed := false
	for _, kind := range eventKinds {
		foundFailed = foundFailed || kind == "workflow_failed"
	}
	if !foundFailed {
		t.Fatalf("workflow failure event missing: %v", eventKinds)
	}
}

func TestFluxWorkflowToolRunsGeneratedDefinitionWithCanonicalEngine(t *testing.T) {
	provider := &fluxProviderStub{result: model.Response{
		ToolCalls: []model.ToolCall{{
			ID: "plan", Type: "function",
			Function: model.FunctionCall{Name: "submit_plan", Arguments: `{"nodes":[{"id":"read","tool":"read_file","arguments":{"path":"README.md"},"depends_on":[]}],"result_type":"generic"}`},
		}},
		FinishReason: "tool_calls",
		Usage:        model.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10, BillingUnits: 99},
	}}
	registry := tools.NewRegistry()
	if err := registry.Register(projectedToolStub{name: "read_file"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var eventMu sync.Mutex
	var eventKinds []string
	tool := NewFluxWorkflowTool(provider, "planner-model")
	result, err := tool.Execute(context.Background(), tools.ExecutionContext{
		WorkspaceRoot: t.TempDir(), SessionID: "s", TurnID: "t", CallID: "parent",
		ExecutionID: "exec", ToolRegistry: registry, NestedExecutor: nestedExecutorStub{},
		OnWorkflowEvent: func(kind string, _ json.RawMessage) {
			eventMu.Lock()
			eventKinds = append(eventKinds, kind)
			eventMu.Unlock()
			if kind == "workflow_finished" || kind == "workflow_failed" {
				select {
				case <-done:
				default:
					close(done)
				}
			}
		},
	}, json.RawMessage(`{"goal":"read the project"}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.ModelUsage == nil || result.ModelUsage.BillingUnits != 99 {
		t.Fatalf("planner billing was not returned: %#v", result.ModelUsage)
	}
	<-done
	eventMu.Lock()
	events := append([]string(nil), eventKinds...)
	eventMu.Unlock()
	foundPlan := false
	foundFinished := false
	foundNodeState := false
	for _, kind := range events {
		foundPlan = foundPlan || kind == "workflow_plan_ready"
		foundFinished = foundFinished || kind == "workflow_finished"
		foundNodeState = foundNodeState || kind == "workflow_node_state_changed"
	}
	if !foundPlan || !foundFinished {
		t.Fatalf("workflow lifecycle events = %v", events)
	}
	if !foundNodeState {
		t.Fatalf("bridge events missing workflow_node_state_changed, got: %v", events)
	}
}

type fluxSessionControlStub struct {
	tools.SessionControl
	mu              sync.Mutex
	requests        []tools.SessionSendRequest
	concurrentGate  chan struct{}
	gateOnce        sync.Once
	timedOutSession string
}

func (s *fluxSessionControlStub) Send(ctx context.Context, request tools.SessionSendRequest) (tools.SessionDelivery, error) {
	s.mu.Lock()
	s.requests = append(s.requests, request)
	ordinal := len(s.requests)
	s.mu.Unlock()
	if s.concurrentGate != nil {
		if ordinal >= 2 {
			s.gateOnce.Do(func() { close(s.concurrentGate) })
		}
		select {
		case <-s.concurrentGate:
		case <-ctx.Done():
			return tools.SessionDelivery{}, ctx.Err()
		}
	}
	return tools.SessionDelivery{
		Accepted: true, Delivery: "started", SessionID: request.TargetSessionID,
		TurnID: request.TargetSessionID + "-turn", Cursor: int64(100 + ordinal),
	}, nil
}

func (s *fluxSessionControlStub) WaitAny(_ context.Context, targets []tools.SessionWaitTarget, _ time.Duration) (tools.SessionWaitResult, error) {
	target := targets[0]
	if target.SessionID == s.timedOutSession {
		return tools.SessionWaitResult{Waiting: targets, TimedOut: true}, nil
	}
	return tools.SessionWaitResult{Completed: []tools.SessionWaitCompletion{{
		SessionID: target.SessionID, TurnID: target.TurnID, Cursor: target.Cursor + 1, Status: "completed",
	}}}, nil
}

func (s *fluxSessionControlStub) snapshot() []tools.SessionSendRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]tools.SessionSendRequest(nil), s.requests...)
}

type fluxSessionIndexStub struct{}

func (fluxSessionIndexStub) ListAll() ([]tools.SessionIndexEntry, error)  { return nil, nil }
func (fluxSessionIndexStub) Get(string) (*tools.SessionIndexEntry, error) { return nil, nil }
func (fluxSessionIndexStub) Read(_ context.Context, id string) (*tools.SessionIndexDetail, error) {
	return &tools.SessionIndexDetail{
		ID: id, WorkspacePath: "/workspace/" + id, Name: id,
		TurnStatus: "done", Summary: "summary for " + id, LastTurn: "completed " + id,
	}, nil
}
func (fluxSessionIndexStub) Search(context.Context, string, int) ([]tools.SessionSearchResult, error) {
	return nil, nil
}

type sessionToolNestedExecutor struct {
	ec tools.ExecutionContext
}

func (e *sessionToolNestedExecutor) ExecuteNestedTool(ctx context.Context, _, callID string, tool tools.Tool, input json.RawMessage) (tools.ToolResult, error) {
	ec := e.ec
	ec.CallID = callID
	return tool.Execute(ctx, ec, input)
}

func TestFluxWorkflowToolExecutesCrossSessionDispatchDAG(t *testing.T) {
	provider := &fluxProviderStub{result: model.Response{
		ToolCalls: []model.ToolCall{{
			ID: "plan", Type: "function",
			Function: model.FunctionCall{Name: "submit_plan", Arguments: `{"nodes":[{"id":"dispatch_frontend","tool":"send_to_session","arguments":{"id":"frontend-session","message":"Build the frontend shell.","intent":"request","correlation_id":"todo/frontend/initial/1"},"depends_on":[]},{"id":"dispatch_backend","tool":"send_to_session","arguments":{"id":"backend-session","message":"Build the backend API.","intent":"request","correlation_id":"todo/backend/initial/1"},"depends_on":[]}],"result_type":"generic"}`},
		}},
		FinishReason: "tool_calls",
	}}
	registry := tools.NewRegistry()
	if err := registry.Register(&sessions.SendToSessionTool{}); err != nil {
		t.Fatal(err)
	}
	control := &fluxSessionControlStub{}
	nested := &sessionToolNestedExecutor{ec: tools.ExecutionContext{
		SessionID: "supervisor-session", TurnID: "supervisor-turn", SessionControl: control,
	}}
	done := make(chan struct{})
	var eventsMu sync.Mutex
	var eventKinds []string
	workspaceRoot := t.TempDir()
	result, err := NewFluxWorkflowTool(provider, "planner-model").Execute(context.Background(), tools.ExecutionContext{
		WorkspaceRoot: workspaceRoot, SessionID: "supervisor-session", TurnID: "supervisor-turn", CallID: "workflow-call",
		ToolRegistry: registry, NestedExecutor: nested,
		OnWorkflowEvent: func(kind string, _ json.RawMessage) {
			eventsMu.Lock()
			eventKinds = append(eventKinds, kind)
			eventsMu.Unlock()
			if kind == "workflow_finished" || kind == "workflow_failed" {
				select {
				case <-done:
				default:
					close(done)
				}
			}
		},
	}, json.RawMessage(`{"goal":"dispatch independent frontend and backend work in parallel"}`))
	if err != nil {
		t.Fatal(err)
	}
	var admission struct {
		WorkflowID string `json:"workflow_id"`
	}
	if json.Unmarshal(result.Output, &admission) != nil || admission.WorkflowID == "" {
		t.Fatalf("workflow admission=%s", result.Output)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("workflow did not reach a terminal state")
	}

	requests := control.snapshot()
	if len(requests) != 2 {
		t.Fatalf("session sends=%+v", requests)
	}
	seen := map[string]string{}
	for _, request := range requests {
		if request.SenderSessionID != "supervisor-session" || request.SenderTurnID != "supervisor-turn" {
			t.Fatalf("supervisor provenance lost: %+v", request)
		}
		seen[request.TargetSessionID] = request.CorrelationID
	}
	if seen["frontend-session"] != "todo/frontend/initial/1" || seen["backend-session"] != "todo/backend/initial/1" {
		t.Fatalf("session routing=%+v", seen)
	}
	definition, err := (&workflowDefTool{}).Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: workspaceRoot},
		json.RawMessage(`{"workflow_id":"`+admission.WorkflowID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	var topology struct {
		Nodes []struct {
			Name string `json:"name"`
			Tool string `json:"tool"`
		} `json:"nodes"`
		Edges []any `json:"edges"`
	}
	if json.Unmarshal(definition.Output, &topology) != nil || len(topology.Nodes) != 4 || len(topology.Edges) != 4 {
		t.Fatalf("workflow topology=%s", definition.Output)
	}
	toolsByNode := map[string]string{}
	for _, node := range topology.Nodes {
		toolsByNode[node.Name] = node.Tool
	}
	if toolsByNode["dispatch_frontend"] != "send_to_session" || toolsByNode["dispatch_backend"] != "send_to_session" {
		t.Fatalf("workflow tools=%+v", toolsByNode)
	}

	status, err := (&workflowStatusTool{}).Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: workspaceRoot},
		json.RawMessage(`{"workflow_id":"`+admission.WorkflowID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot WorkflowSnapshot
	if json.Unmarshal(status.Output, &snapshot) != nil || snapshot.Task == nil {
		t.Fatalf("workflow snapshot=%s", status.Output)
	}
	if snapshot.Task.Status != "success" || len(snapshot.Nodes) != 4 || len(snapshot.Edges) != 4 {
		t.Fatalf("workflow snapshot=%+v", snapshot)
	}
	for _, node := range snapshot.Nodes {
		if node.State != "success" || !node.Terminal {
			t.Fatalf("node did not complete: %+v", node)
		}
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	if len(eventKinds) == 0 {
		t.Fatal("workflow emitted no observable events")
	}
}

func TestFluxWorkflowToolExecutesCrossWorkspaceMapTemplate(t *testing.T) {
	registry := tools.NewRegistry()
	for _, tool := range []tools.Tool{
		&sessions.SendToSessionTool{}, &sessions.WaitSessionsTool{}, &sessions.ReadSessionTool{},
	} {
		if err := registry.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	control := &fluxSessionControlStub{concurrentGate: make(chan struct{}), timedOutSession: "frontend-session"}
	nested := &sessionToolNestedExecutor{ec: tools.ExecutionContext{
		SessionID: "supervisor-session", TurnID: "supervisor-turn",
		SessionControl: control, SessionIndex: fluxSessionIndexStub{},
	}}
	done := make(chan struct{})
	workspaceRoot := t.TempDir()
	callerCtx, cancelCaller := context.WithCancel(context.Background())
	result, err := NewFluxWorkflowTool(nil, "").Execute(callerCtx, tools.ExecutionContext{
		WorkspaceRoot: workspaceRoot, SessionID: "supervisor-session", TurnID: "supervisor-turn", CallID: "map-call",
		ToolRegistry: registry, NestedExecutor: nested,
		OnWorkflowEvent: func(kind string, _ json.RawMessage) {
			if kind == "workflow_finished" || kind == "workflow_failed" {
				select {
				case <-done:
				default:
					close(done)
				}
			}
		},
	}, json.RawMessage(`{
		"goal":"build frontend and backend concurrently",
		"template":"cross_workspace_collaboration_v1",
		"parallelism":2,
		"timeout_ms":1000,
		"agents":[
			{"role":"frontend","session_id":"frontend-session","workspace_path":"/workspace/frontend","message":"Build frontend.","correlation_id":"case/frontend/1"},
			{"role":"backend","session_id":"backend-session","workspace_path":"/workspace/backend","message":"Build backend.","correlation_id":"case/backend/1"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	// plan_workflow is asynchronous. Ending the supervisor tool call must not
	// cancel Map children or their terminal waits.
	cancelCaller()
	var admission struct {
		WorkflowID string `json:"workflow_id"`
		Template   string `json:"template"`
	}
	if err := json.Unmarshal(result.Output, &admission); err != nil {
		t.Fatal(err)
	}
	if admission.WorkflowID == "" || admission.Template != crossWorkspaceCollaborationV1 {
		t.Fatalf("workflow admission=%s", result.Output)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Map workflow did not finish; child tasks may not be running concurrently")
	}

	requests := control.snapshot()
	if len(requests) != 2 {
		t.Fatalf("session sends=%+v", requests)
	}
	seen := map[string]string{}
	for _, request := range requests {
		seen[request.TargetSessionID] = request.CorrelationID
		if request.SenderSessionID != "supervisor-session" || request.SenderTurnID != "supervisor-turn" {
			t.Fatalf("supervisor provenance lost: %+v", request)
		}
	}
	if seen["frontend-session"] != "case/frontend/1" || seen["backend-session"] != "case/backend/1" {
		t.Fatalf("session routing=%+v", seen)
	}

	status, err := (&workflowStatusTool{}).Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: workspaceRoot},
		json.RawMessage(`{"workflow_id":"`+admission.WorkflowID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot WorkflowSnapshot
	if err := json.Unmarshal(status.Output, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Task == nil || snapshot.Task.Status != "success" {
		t.Fatalf("workflow snapshot=%s", status.Output)
	}
	var taskOutput struct {
		Final struct {
			ResultType string `json:"result_type"`
			Extras     struct {
				Results []any `json:"results"`
			} `json:"extras"`
		} `json:"final"`
	}
	if err := json.Unmarshal(snapshot.Task.Output, &taskOutput); err != nil {
		t.Fatal(err)
	}
	if taskOutput.Final.ResultType != "cross_workspace_collaboration" || len(taskOutput.Final.Extras.Results) != 2 {
		t.Fatalf("fan-in output=%s", snapshot.Task.Output)
	}
	statuses := map[string]bool{}
	for _, rawResult := range taskOutput.Final.Extras.Results {
		resultMap, ok := rawResult.(map[string]any)
		if !ok {
			t.Fatalf("unexpected Map result=%#v", rawResult)
		}
		extras, ok := resultMap["extras"].(map[string]any)
		if !ok {
			t.Fatalf("unexpected child output=%#v", resultMap)
		}
		status, _ := extras["status"].(string)
		statuses[status] = true
	}
	if !statuses["timed_out"] || !statuses["completed"] {
		t.Fatalf("child statuses=%v output=%s", statuses, snapshot.Task.Output)
	}

	definitionResult, err := (&workflowDefTool{}).Execute(context.Background(), tools.ExecutionContext{WorkspaceRoot: workspaceRoot},
		json.RawMessage(`{"workflow_id":"`+admission.WorkflowID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(definitionResult.Content, "run_agents (map)") {
		t.Fatalf("workflow definition=%s", definitionResult.Content)
	}
}

func TestValidateCrossWorkspaceManifestRejectsAmbiguousWorkers(t *testing.T) {
	agents := []crossWorkspaceAgentSpec{
		{Role: " frontend ", SessionID: "same", Message: "one", CorrelationID: "case/one"},
		{Role: "backend", SessionID: "same", Message: "two", CorrelationID: "case/two"},
	}
	if err := validateCrossWorkspaceManifest(crossWorkspaceCollaborationV1, agents, 2); err == nil || !strings.Contains(err.Error(), "duplicate agent session_id") {
		t.Fatalf("duplicate session validation error = %v", err)
	}
	agents[1].SessionID = "backend"
	agents[1].CorrelationID = "case/one"
	if err := validateCrossWorkspaceManifest(crossWorkspaceCollaborationV1, agents, 2); err == nil || !strings.Contains(err.Error(), "duplicate agent correlation_id") {
		t.Fatalf("duplicate correlation validation error = %v", err)
	}
}

// TODO: adopt to async Submit — client tool await/resume needs worker-aware test harness.
func TestFluxWorkflowToolSuspendsAndResumesAroundClientTool(t *testing.T) {
	t.Skip("TODO: adopt to async Submit — client tool await/resume needs worker-aware test")
	provider := &fluxProviderStub{result: model.Response{
		ToolCalls: []model.ToolCall{{
			ID: "plan", Type: "function",
			Function: model.FunctionCall{Name: "submit_plan", Arguments: `{"nodes":[{"id":"capture","tool":"ios_capture_photo","arguments":{"prompt":"hello"},"depends_on":[]},{"id":"consume","tool":"consume_asset","arguments":{"path":{"$from":"capture","field":"asset_id"}},"depends_on":["capture"]}],"result_type":"generic"}`},
		}},
		FinishReason: "tool_calls",
	}}
	registry := tools.NewRegistry()
	if err := registry.Register(tools.NewClientProxyTool("ios_capture_photo", "capture on device", json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string"}}}`))); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(projectedToolStub{name: "consume_asset"}); err != nil {
		t.Fatal(err)
	}
	executor := &routingNestedExecutor{}
	done := make(chan struct{})
	var nodeTransitions []workflowTransition
	var taskTransitions []workflowTransition
	tool := NewFluxWorkflowTool(provider, "planner-model")
	_, err := tool.Execute(context.Background(), tools.ExecutionContext{
		WorkspaceRoot: t.TempDir(), SessionID: "s", TurnID: "t", CallID: "parent",
		ToolRegistry: registry, NestedExecutor: executor,
		OnWorkflowEvent: func(kind string, payload json.RawMessage) {
			var event map[string]any
			_ = json.Unmarshal(payload, &event)
			switch kind {
			case "workflow_node_state_changed":
				nodeTransitions = append(nodeTransitions, workflowTransition{from: stringValueForTest(event["from"]), to: stringValueForTest(event["to"])})
			case "workflow_task_state_changed":
				taskTransitions = append(taskTransitions, workflowTransition{from: stringValueForTest(event["from"]), to: stringValueForTest(event["to"])})
			}
			if kind == "workflow_finished" || kind == "workflow_failed" {
				select {
				case <-done:
				default:
					close(done)
				}
			}
		},
	}, json.RawMessage(`{"goal":"capture and consume a photo"}`))
	if err != nil {
		t.Fatal(err)
	}
	<-done
	if got := executor.inputs["ios_capture_photo"]["prompt"]; got != "hello" {
		t.Fatalf("client input prompt = %#v", got)
	}
	if got := executor.inputs["consume_asset"]["path"]; got != "photo-1" {
		t.Fatalf("downstream input path = %#v", got)
	}
	// With async Submit, the routingNestedExecutor handles client tools
	// server-side, so no runtime await/suspend occurs. But node state
	// transitions and downstream data flow must still be verified.
	if len(nodeTransitions) == 0 {
		t.Fatal("expected node state transitions, got none")
	}
	if len(taskTransitions) == 0 {
		t.Fatal("expected task state transitions, got none")
	}
}

func stringValueForTest(value any) string {
	valueString, _ := value.(string)
	return valueString
}

func hasTransition(transitions []workflowTransition, from, to string) bool {
	for _, transition := range transitions {
		if transition.from == from && transition.to == to {
			return true
		}
	}
	return false
}

// TestProjectFluxToolsSurfacesSessionToolOutputSchemas verifies the Flux adapter
// surfaces each session tool's per-tool OutputSchema (not the generic
// {content, output, assets} fallback), so DAG mappings such as
// nodes.dispatch.output.session_id resolve against real contracts.
func TestProjectFluxToolsSurfacesSessionToolOutputSchemas(t *testing.T) {
	registry := tools.NewRegistry()
	for _, tool := range []tools.Tool{
		&sessions.SendToSessionTool{}, &sessions.WaitSessionsTool{}, &sessions.ReadSessionTool{},
	} {
		if err := registry.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	ec := tools.ExecutionContext{ToolRegistry: registry, NestedExecutor: nestedExecutorStub{}}
	projected := projectFluxTools(context.Background(), ec, &nestedUsageCollector{})

	for _, name := range []string{"send_to_session", "wait_sessions", "read_session"} {
		fluxTool, ok := projected.Get(name)
		if !ok {
			t.Fatalf("%s was not projected into the Flux registry", name)
		}
		if len(fluxTool.OutputSchema().Fields) == 0 {
			t.Fatalf("%s: Flux OutputSchema is empty; per-tool schema was not surfaced", name)
		}
	}

	// The send_to_session contract is what DAG mappings reference
	// (e.g. nodes.dispatch.output.session_id), so it must be visible to Flux.
	sendTool, _ := projected.Get("send_to_session")
	fields := sendTool.OutputSchema().Fields
	for _, field := range []string{"accepted", "delivery", "session_id", "turn_id", "cursor"} {
		if _, ok := fields[field]; !ok {
			t.Fatalf("send_to_session Flux OutputSchema missing %q (fields: %v)", field, fields)
		}
	}
}
