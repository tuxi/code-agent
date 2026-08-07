// Package task provides the `task` tool: the model delegates a self-contained,
// read-only investigation to a subagent that runs in its own isolated context and
// returns only its conclusion. The verbose exploration never enters the parent's
// context — that is the root fix for context hygiene (8.3).
//
// This package depends only on the narrow SubAgent interface and the tools
// package; it does NOT import the agent loop. The concrete SubAgent — which builds
// an isolated session and a read-only sub-runner — lives in the command layer, so
// the loop stays unaware that a subagent exists.
package task

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"code-agent/internal/tools"
)

// SubAgent runs a delegated subtask in an isolated context and returns only its
// final conclusion. The prompt is the ONLY channel into the subagent — it sees
// nothing of the parent conversation. The ExecutionContext carries the calling
// turn's identity (session/turn), which the runtime uses to forward the
// delegation's bracket events into the parent's stream (P8.7 §8.4-2) — routing
// metadata, never subagent input.
type SubAgent interface {
	Run(ctx context.Context, ec tools.ExecutionContext, prompt string) (string, error)
}

// Tool is the model-facing `task` tool. It is read-only (it does not implement a
// side-effect marker): it spends model tokens but never mutates the workspace, so
// it runs without confirmation. The boundary that matters — that the subagent
// cannot write or run commands — lives in the subagent's own toolset, not here.
type Tool struct {
	agent SubAgent
}

// NewTool returns a task tool backed by the given subagent runner.
func NewTool(agent SubAgent) *Tool { return &Tool{agent: agent} }

// Agent returns the immutable subagent backing this tool. Serve-mode runtimes
// use it as a template when cloning the tool registry for a turn, so the cloned
// task tool can be rebound to that turn's provider and credential resolver.
func (t *Tool) Agent() SubAgent { return t.agent }

func (t *Tool) Name() string { return "task" }

func (t *Tool) Description() string {
	return "Delegate ONE broad, self-contained, read-only investigation to a subagent that runs in " +
		"its own isolated context and returns only its conclusion — so the verbose exploration never " +
		"enters this conversation. " +
		"Use it when answering would otherwise mean reading MANY files you won't reference again " +
		"(e.g. 'trace how X flows across the codebase', 'find everywhere Y is configured'). " +
		"Do NOT use it to read a single file or run one search — do that yourself; spinning up a " +
		"subagent per file is pure overhead. And TRUST what it returns: do not re-read or re-investigate " +
		"the files it already covered. " +
		"The subagent sees NOTHING of this conversation — its only input is your prompt, so put every " +
		"file path, error message, and the precise question into that prompt. It is read-only (no edits, " +
		"no commands). For a harness gate, set kind to change_review and require the first " +
		"line to be exactly VERDICT: PASS or VERDICT: REQUEST_CHANGES. Returns the subagent's findings."
}

func (t *Tool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"kind": {
			Type: "string",
			Enum: []string{"investigation", "change_review"},
			Description: "Optional harness role: change_review for the independent review after implementation.",
		},
		"subject_path": {
			Type: "string",
			Description: "For change_review, the workspace-relative path to the primary changed or affected file.",
		},
		"prompt": {
			Type: "string",
			Description: "The self-contained task for the subagent. Include all context it needs " +
				"(file paths, error text, the exact question) — it cannot see this conversation.",
		},
	}, "prompt").JSON()
}

type input struct {
	Kind        string `json:"kind"`
	SubjectPath string `json:"subject_path"`
	Prompt      string `json:"prompt"`
}

func (t *Tool) Execute(ctx context.Context, ec tools.ExecutionContext, raw json.RawMessage) (tools.ToolResult, error) {
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		return tools.ToolResult{}, fmt.Errorf("invalid task input: %w", err)
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return tools.ToolResult{}, fmt.Errorf("task requires a non-empty prompt")
	}
	prompt, err := harnessPrompt(in)
	if err != nil {
		return tools.ToolResult{}, err
	}

	conclusion, err := t.agent.Run(ctx, ec, prompt)
	if err != nil {
		return tools.ToolResult{}, err
	}
	return tools.ToolResult{Content: conclusion}, nil
}

func harnessPrompt(in input) (string, error) {
	switch in.Kind {
	case "change_review":
		return "You are an independent Change Reviewer. Inspect git_diff and the relevant files yourself; " +
			"do not trust the implementing agent's summary. Compare the changes with the stated requirement " +
			"and approved plan. Look for wrong-direction implementation, regressions, missing tests, unsafe " +
			"behavior, and verification gaps. Your first non-empty line MUST be exactly `VERDICT: PASS` or " +
			"`VERDICT: REQUEST_CHANGES`.\n\n" +
			"Categorize every finding as blocking (must fix), advisory (note for follow-up), or nit (style/formatting). " +
			"Return VERDICT: PASS when no blocking finding remains. Advisory and nit findings do not block.\n\n" +
			in.Prompt, nil
	default:
		return in.Prompt, nil
	}
}
