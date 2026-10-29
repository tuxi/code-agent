//go:build unix

package collabflux_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/model"
	"code-agent/internal/settings"
	"code-agent/internal/tools"
	"code-agent/internal/tools/filesystem"
	"code-agent/internal/tools/shell"
)

type scriptedLoopProvider struct {
	mu    sync.Mutex
	steps map[string]int
	root  string
}

func (p *scriptedLoopProvider) Complete(_ context.Context, req model.Request) (model.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.steps[req.TurnID]++
	step := p.steps[req.TurnID]
	name, args := "", ""
	switch step {
	case 1:
		name, args = "read_file", `{"path":"main.txt"}`
	case 2:
		old, err := os.ReadFile(filepath.Join(p.root, "main.txt"))
		if err != nil {
			return model.Response{}, err
		}
		b, _ := json.Marshal(map[string]string{"path": "main.txt", "old": string(old), "new": "candidate " + req.TurnID + "\n"})
		name, args = "edit_file", string(b)
	case 3:
		name, args = "run_command", `{"command":"git diff --check"}`
	}
	if name == "" {
		return model.Response{Content: "Local script completed with actual tool evidence.", ActualModel: "offline-script", FinishReason: "stop"}, nil
	}
	return model.Response{ActualModel: "offline-script", FinishReason: "tool_calls", ToolCalls: []model.ToolCall{{ID: fmt.Sprintf("actual-%s-%d", req.TurnID, step), Type: "function", Function: model.FunctionCall{Name: name, Arguments: args}}}}, nil
}

type fixtureAllow struct{}

func (fixtureAllow) Approve(string, json.RawMessage) agent.Verdict { return agent.VerdictAllow }

type actualLoopBuilder struct {
	provider  *scriptedLoopProvider
	dir, root string
}

func (*actualLoopBuilder) ResolveModel(string) (*settings.ModelConfig, error) {
	return &settings.ModelConfig{Name: "default", Model: "default"}, nil
}
func (b *actualLoopBuilder) Build(c conversation.RuntimeContext) conversation.TurnRunner {
	reg := tools.NewRegistry()
	_ = reg.Register(filesystem.NewReadFileTool())
	_ = reg.Register(filesystem.NewEditFileTool())
	command := shell.NewRunCommandTool()
	command.ForegroundOnly = true
	_ = reg.Register(command)
	return &agent.Runner{Model: b.provider, ModelName: c.ResolvedModel, Tools: reg, Approver: fixtureAllow{}, Emitter: &effectCounterEmitter{next: c.Publisher, dir: b.dir}, WorkspaceRoot: b.root, ReservedTurnID: c.TurnID, RequestID: c.RequestID, MaxSteps: 8}
}

type effectCounterEmitter struct {
	next agent.Emitter
	dir  string
}

func (e *effectCounterEmitter) Emit(event agent.Event) {
	if event.Kind == agent.EventToolFinished && event.ToolName == "edit_file" && event.Err == "" {
		b, _ := os.ReadFile(filepath.Join(e.dir, "effects"))
		n, _ := strconv.Atoi(string(b))
		_ = durableFile(filepath.Join(e.dir, "effects"), []byte(strconv.Itoa(n+1)))
	}
	e.next.Emit(event)
}
