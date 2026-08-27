package runtime

import (
	"context"
	"encoding/json"
	"fmt"

	"code-agent/internal/controlplane"
	"code-agent/internal/tools"
	"code-agent/internal/tools/sessions"
)

// ── Headless workflow run (P2: trigger a saved template without a conversation) ──

// HeadlessRunFunc submits a saved workflow template by name and returns the
// task id. The workspace's started runtime (task + await-poll workers, plus
// the injected ExternalResolver) is reused from the pool, so the run executes
// asynchronously: awaits suspend the task and the AwaitPollWorker wakes it on
// child completion (R7).
type HeadlessRunFunc func(ctx context.Context, workspaceRoot, workflowName string, input map[string]any) (int64, error)

// HeadlessRuntime synthesizes the execution context a workflow run needs when
// there is no conversation: session tools are backed by the control plane
// manager, the tool registry carries only the tools templates may use, and
// the NestedExecutor executes tools directly (the run is pre-authorized by
// its trigger — no per-call approval).
type HeadlessRuntime struct {
	control *controlplane.Manager
}

// NewHeadlessRuntime builds a HeadlessRuntime from the control plane manager.
// A nil manager disables session tools (send_to_session etc. return their
// "control plane is not available" errors).
func NewHeadlessRuntime(control *controlplane.Manager) *HeadlessRuntime {
	return &HeadlessRuntime{control: control}
}

// headlessNestedExecutor runs a tool directly. Unlike the agent loop's
// executor there is no approval, inspection or client-dispatch layer — the
// headless run was already authorized by its trigger (App panel / automation
// permission context). Tools that need a turn (ask_user, client tools) are
// not in the headless registry to begin with.
type headlessNestedExecutor struct {
	base tools.ExecutionContext
}

func (e *headlessNestedExecutor) ExecuteNestedTool(ctx context.Context, _, callID string, tool tools.Tool, input json.RawMessage) (tools.ToolResult, error) {
	ec := e.base
	ec.CallID = callID
	ec.NestedExecutor = e
	return tool.Execute(ctx, ec, input)
}

// BuildHeadlessContext assembles the tools.ExecutionContext a headless run's
// tool projection needs. The registry mirrors the conversation profile but
// drops everything that requires an interactive turn: the session tools that
// v1/v2 templates use (send_to_session/read_session/wait_sessions/check_turn)
// are included; conversation-only and workflow meta tools are excluded by
// fluxExcludedTools when projected.
func (r *HeadlessRuntime) BuildHeadlessContext(workspaceRoot, callID string) tools.ExecutionContext {
	reg := tools.NewRegistry()
	for _, tool := range []tools.Tool{
		&sessions.SendToSessionTool{},
		&sessions.ReadSessionTool{},
		&sessions.WaitSessionsTool{},
		&CheckTurnTool{},
	} {
		_ = reg.Register(tool)
	}

	ec := tools.ExecutionContext{
		WorkspaceRoot:  workspaceRoot,
		SessionID:      "headless",
		TurnID:         "headless",
		CallID:         callID,
		SessionControl: r.control,
		SessionIndex:   SessionIndex(),
		ToolRegistry:   reg,
	}
	ne := &headlessNestedExecutor{base: ec}
	ec.NestedExecutor = ne
	return ec
}

// SubmitHeadlessRun registers the template's tool projection into the
// workspace runtime's tool registry, then submits the workflow by name. The
// definition must already be persisted (a saved template); Submit resolves
// the latest version from the DB, so no in-memory registration is needed.
//
// The tool projection step is what plan_workflow.Execute does before Submit
// (flux_tool.go): task workers resolve tool nodes through the runtime's
// registry, so send_to_session/read_session must be registered there or the
// nodes fail with "tool not found".
func (r *HeadlessRuntime) SubmitHeadlessRun(ctx context.Context, workspaceRoot, workflowName string, input map[string]any) (int64, error) {
	if r == nil || r.control == nil {
		return 0, fmt.Errorf("headless run unavailable: control plane not wired")
	}
	if workflowName == "" {
		return 0, fmt.Errorf("workflow name is required")
	}

	rt, err := getOrCreateRuntime(ctx, workspaceRoot)
	if err != nil {
		return 0, fmt.Errorf("headless run: %w", err)
	}

	ec := r.BuildHeadlessContext(workspaceRoot, "headless:"+workflowName)
	fluxReg := projectFluxTools(ctx, ec, &nestedUsageCollector{})
	if len(fluxReg.List()) == 0 {
		return 0, fmt.Errorf("headless run: no eligible tools are available")
	}
	for _, projected := range fluxReg.List() {
		rt.ToolRegistry().Register(projected)
	}

	return rt.Submit(ctx, workflowName, input)
}
