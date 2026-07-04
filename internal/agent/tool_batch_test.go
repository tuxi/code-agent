package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"code-agent/internal/model"
	"code-agent/internal/tools"
)

// concTracker records live concurrency across all probe tools in a batch.
type concTracker struct {
	mu            sync.Mutex
	active        int
	maxActive     int
	sideViolation bool     // a side-effecting tool ran while others were active
	order         []string // tool completion order
}

func (tr *concTracker) enter(name string, side bool) {
	tr.mu.Lock()
	tr.active++
	if tr.active > tr.maxActive {
		tr.maxActive = tr.active
	}
	if side && tr.active != 1 {
		tr.sideViolation = true
	}
	tr.mu.Unlock()
}

func (tr *concTracker) leave(name string, side bool) {
	tr.mu.Lock()
	if side && tr.active != 1 {
		tr.sideViolation = true
	}
	tr.active--
	tr.order = append(tr.order, name)
	tr.mu.Unlock()
}

// gate blocks each arriving goroutine until all `n` have arrived, so a batch
// that is NOT actually concurrent cannot get past it (the arrivals time out).
// This makes "did they run in parallel?" a deterministic assertion, not a race.
type gate struct {
	mu    sync.Mutex
	count int
	n     int
	ch    chan struct{}
}

func newGate(n int) *gate { return &gate{n: n, ch: make(chan struct{})} }

func (g *gate) arrive() {
	g.mu.Lock()
	g.count++
	if g.count == g.n {
		close(g.ch)
	}
	g.mu.Unlock()
	select {
	case <-g.ch: // all arrived → truly concurrent
	case <-time.After(2 * time.Second): // sequential: no one else is coming
	}
}

// probeTool records concurrency and (optionally) syncs on a gate. side controls
// whether it declares side effects (→ serial barrier).
type probeTool struct {
	name    string
	side    bool
	tracker *concTracker
	gate    *gate // nil = don't sync
}

func (t *probeTool) Name() string                 { return t.name }
func (t *probeTool) Description() string          { return "probe" }
func (t *probeTool) InputSchema() json.RawMessage { return tools.Object(nil).JSON() }
func (t *probeTool) SideEffects() bool            { return t.side }
func (t *probeTool) Execute(_ context.Context, _ tools.ExecutionContext, _ json.RawMessage) (tools.ToolResult, error) {
	t.tracker.enter(t.name, t.side)
	if t.gate != nil {
		t.gate.arrive()
	}
	t.tracker.leave(t.name, t.side)
	return tools.ToolResult{Content: "done:" + t.name}, nil
}

// runBatch registers the given tools, scripts a single assistant message that
// calls each once (in the given order), and runs the turn to completion.
func runBatch(t *testing.T, maxParallel int, approver Approver, toolset []tools.Tool, callOrder []string) (*Runner, TurnResult) {
	t.Helper()
	reg := tools.NewRegistry()
	for _, tl := range toolset {
		if err := reg.Register(tl); err != nil {
			t.Fatalf("register %s: %v", tl.Name(), err)
		}
	}
	calls := make([]model.ToolCall, len(callOrder))
	for i, name := range callOrder {
		calls[i] = model.ToolCall{
			ID: fmt.Sprintf("call_%d", i), Type: "function",
			Function: model.FunctionCall{Name: name, Arguments: "{}"},
		}
	}
	provider := &scriptedProvider{responses: []model.Response{
		{ToolCalls: calls, FinishReason: "tool_calls"},
		{Content: "all done", FinishReason: "stop"},
	}}
	runner := &Runner{
		Model: provider, Tools: reg, MaxSteps: 4,
		MaxParallelTools: maxParallel, Approver: approver,
	}
	res, err := runner.RunTurn(context.Background(), newSession(), "go")
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	return runner, res
}

// TestParallelReadOnlyRunConcurrently is the map primitive: 5 independent
// read-only calls in one batch all run at once. The gate would deadlock (time
// out) if they ran sequentially — maxActive must reach 5.
func TestParallelReadOnlyRunConcurrently(t *testing.T) {
	tr := &concTracker{}
	g := newGate(5)
	var toolset []tools.Tool
	var order []string
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("read%d", i)
		toolset = append(toolset, &probeTool{name: name, tracker: tr, gate: g})
		order = append(order, name)
	}

	start := time.Now()
	_, res := runBatch(t, 5, nil, toolset, order)

	if tr.maxActive != 5 {
		t.Errorf("maxActive = %d, want 5 (all read-only calls concurrent)", tr.maxActive)
	}
	if len(res.Steps) != 5 {
		t.Errorf("steps = %d, want 5", len(res.Steps))
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("batch took %v — likely serialized (gate timed out)", elapsed)
	}
}

// TestSequentialWhenMaxParallelOne: the same read-only batch with the default
// (max=1) never overlaps — maxActive stays 1.
func TestSequentialWhenMaxParallelOne(t *testing.T) {
	tr := &concTracker{}
	var toolset []tools.Tool
	var order []string
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("read%d", i)
		toolset = append(toolset, &probeTool{name: name, tracker: tr}) // no gate
		order = append(order, name)
	}
	_, res := runBatch(t, 1, nil, toolset, order)

	if tr.maxActive != 1 {
		t.Errorf("maxActive = %d, want 1 (sequential when max_parallel_tools=1)", tr.maxActive)
	}
	if len(res.Steps) != 3 {
		t.Errorf("steps = %d, want 3", len(res.Steps))
	}
}

// TestWriteSerial: a batch mixing reads and a side-effecting write. Reads
// overlap; the write never runs concurrently with anything (barrier), and
// results still commit in model order.
func TestWriteSerial(t *testing.T) {
	tr := &concTracker{}
	// [read0, read1, write, read2, read3]: read0/read1 share a gate so they
	// provably overlap (the first group); write is a barrier; read2/read3 are the
	// next group.
	sharedGate := newGate(2)
	toolset := []tools.Tool{
		&probeTool{name: "read0", tracker: tr, gate: sharedGate},
		&probeTool{name: "read1", tracker: tr, gate: sharedGate},
		&probeTool{name: "write", side: true, tracker: tr},
		&probeTool{name: "read2", tracker: tr},
		&probeTool{name: "read3", tracker: tr},
	}
	order := []string{"read0", "read1", "write", "read2", "read3"}
	_, res := runBatch(t, 4, allowApprover{}, toolset, order)

	if tr.sideViolation {
		t.Error("side-effecting tool overlapped another tool — write-serial violated")
	}
	if tr.maxActive < 2 {
		t.Errorf("maxActive = %d, want >= 2 (reads should overlap)", tr.maxActive)
	}
	// Results commit in MODEL order regardless of completion order.
	gotSteps := make([]string, len(res.Steps))
	for i, s := range res.Steps {
		gotSteps[i] = s.ToolName
	}
	if strings.Join(gotSteps, ",") != strings.Join(order, ",") {
		t.Errorf("step order = %v, want model order %v", gotSteps, order)
	}
}

// TestParallelResultsPreserveModelOrder: even when completion order is shuffled,
// the tool-result messages appear in the model's call order (provider requires
// results to correspond to tool_calls).
func TestParallelResultsPreserveModelOrder(t *testing.T) {
	// Different sleeps so completion order != call order.
	toolset := []tools.Tool{
		&sleepTool{name: "a", d: 60 * time.Millisecond},
		&sleepTool{name: "b", d: 10 * time.Millisecond},
		&sleepTool{name: "c", d: 30 * time.Millisecond},
	}
	order := []string{"a", "b", "c"}
	_, res := runBatch(t, 3, nil, toolset, order)

	got := make([]string, len(res.Steps))
	for i, s := range res.Steps {
		got[i] = s.ToolName
	}
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("step order = %v, want [a b c] (model order, not completion order)", got)
	}
}

// TestParallelCancelLeavesBalancedHistory: cancelling during a concurrent group
// still leaves a balanced, resumable history — the not-yet-run barrier call gets
// an interrupted marker and the turn returns context.Canceled.
func TestParallelCancelLeavesBalancedHistory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reg := tools.NewRegistry()
	// read0/read1 run concurrently (group 1); read0 cancels the context. `write`
	// is a barrier (group 2) that must therefore never run.
	mustReg := func(tl tools.Tool) {
		if err := reg.Register(tl); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	mustReg(&cancelOnRun{name: "read0", cancel: cancel})
	mustReg(&sleepTool{name: "read1", d: 20 * time.Millisecond})
	write := &probeTool{name: "write", side: true, tracker: &concTracker{}}
	mustReg(write)

	calls := []model.ToolCall{
		{ID: "c0", Type: "function", Function: model.FunctionCall{Name: "read0", Arguments: "{}"}},
		{ID: "c1", Type: "function", Function: model.FunctionCall{Name: "read1", Arguments: "{}"}},
		{ID: "c2", Type: "function", Function: model.FunctionCall{Name: "write", Arguments: "{}"}},
	}
	provider := &scriptedProvider{responses: []model.Response{
		{ToolCalls: calls, FinishReason: "tool_calls"},
		{Content: "done", FinishReason: "stop"},
	}}
	runner := &Runner{Model: provider, Tools: reg, MaxSteps: 4, MaxParallelTools: 2, Approver: allowApprover{}}
	sess := newSession()

	_, err := runner.RunTurn(ctx, sess, "go")
	if err == nil {
		t.Fatal("want a cancellation error, got nil")
	}
	// Every tool_call must have exactly one result (balanced), and the barrier
	// carries the interrupted marker.
	results := map[string]string{}
	for _, m := range sess.Messages {
		if m.Role == model.RoleTool {
			results[m.ToolCallID] = m.Content
		}
	}
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3 (balanced history)", len(results))
	}
	if results["c2"] != toolInterruptedObservation {
		t.Errorf("barrier c2 = %q, want interrupted marker", results["c2"])
	}
}

// cancelOnRun is a read-only tool that cancels a context when it executes.
type cancelOnRun struct {
	name   string
	cancel context.CancelFunc
}

func (t *cancelOnRun) Name() string                 { return t.name }
func (t *cancelOnRun) Description() string          { return "cancels on run" }
func (t *cancelOnRun) InputSchema() json.RawMessage { return tools.Object(nil).JSON() }
func (t *cancelOnRun) Execute(_ context.Context, _ tools.ExecutionContext, _ json.RawMessage) (tools.ToolResult, error) {
	t.cancel()
	return tools.ToolResult{Content: "cancelled-here"}, nil
}

type sleepTool struct {
	name string
	d    time.Duration
}

func (t *sleepTool) Name() string                 { return t.name }
func (t *sleepTool) Description() string          { return "sleep" }
func (t *sleepTool) InputSchema() json.RawMessage { return tools.Object(nil).JSON() }
func (t *sleepTool) Execute(_ context.Context, _ tools.ExecutionContext, _ json.RawMessage) (tools.ToolResult, error) {
	time.Sleep(t.d)
	return tools.ToolResult{Content: "done:" + t.name}, nil
}
