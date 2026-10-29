package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"code-agent/internal/collab"
	"code-agent/internal/tools"
)

type nestedClaimGuard struct {
	denied             bool
	acquired, released atomic.Int32
}

func (g *nestedClaimGuard) AcquireTaskExecution(context.Context, string, string, string, *collab.ManagedTurnRef) (func(), error) {
	g.acquired.Add(1)
	if g.denied {
		return nil, collab.ErrManagedWorkspace
	}
	return func() { g.released.Add(1) }, nil
}
func (*nestedClaimGuard) RequireTaskRecovery(context.Context, string, string) error { return nil }

type nestedMutation struct{ calls atomic.Int32 }

func (*nestedMutation) Name() string                 { return "fixture_write" }
func (*nestedMutation) Description() string          { return "fixture" }
func (*nestedMutation) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *nestedMutation) Execute(context.Context, tools.ExecutionContext, json.RawMessage) (tools.ToolResult, error) {
	t.calls.Add(1)
	return tools.ToolResult{Content: "written"}, nil
}

func TestManagedClaimFencesHeadlessAndGenericNestedExecutors(t *testing.T) {
	for _, denied := range []bool{false, true} {
		guard := &nestedClaimGuard{denied: denied}
		tool := &nestedMutation{}
		ec := tools.ExecutionContext{SessionID: "source", WorkspaceRoot: t.TempDir()}
		for _, executor := range []tools.NestedToolExecutor{&headlessNestedExecutor{base: ec, guard: guard}, &leasedNestedExecutor{next: &headlessNestedExecutor{base: ec}, guard: guard, root: ec.WorkspaceRoot, sessionID: ec.SessionID}} {
			_, err := executor.ExecuteNestedTool(context.Background(), "parent", "call", tool, json.RawMessage(`{}`))
			if denied {
				if !errors.Is(err, collab.ErrManagedWorkspace) {
					t.Fatal("managed root entered", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		}
		if denied && tool.calls.Load() != 0 {
			t.Fatal("side effect before workspace admission")
		}
		if !denied && (tool.calls.Load() != 2 || guard.released.Load() != 2) {
			t.Fatal("ordinary execution changed or lease leaked")
		}
	}
}
