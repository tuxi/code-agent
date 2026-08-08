// Package hooks runs user-configured shell commands around tool execution (8.5).
// It implements the agent.ToolHook interface structurally (no import of agent), so
// it stays a pure, independently testable command runner. Hooks are the user's
// own commands and run with the user's shell — like Claude Code, they are trusted
// configuration, not model-driven input.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Event is when a hook fires.
type Event string

const (
	PreToolUse  Event = "pre_tool_use"  // before a tool runs; may BLOCK
	PostToolUse Event = "post_tool_use" // after a tool succeeds; e.g. format/lint
	// AfterTurn is the finalize stop decision (8.5): runs when the model wants to
	// finish. It is a turn-level policy, not a tool, so Match is ignored. The
	// operator configures a hook that owns the stop decision, REPLACING the
	// harness's built-in default policy.
	AfterTurn Event = "after_turn"
	// ProjectTrust fires during trust resolution, BEFORE project settings are
	// loaded. The hook receives the project path and whether it contains
	// trust-requiring resources. First-responder wins; undecided falls through.
	// Match is ignored (project trust is not tool-scoped).
	ProjectTrust Event = "project_trust"
	// ContextPreRequest fires before every LLM call and can transform the message
	// list. Handlers are CHAINED: each handler sees the previous handler's output.
	// Match is ignored (context transform is not tool-scoped).
	ContextPreRequest Event = "context"
	// PostToolResult fires after a tool executes successfully and its result is
	// ready. Handlers are CHAINED: each handler sees the previous handler's output.
	// Unlike PostToolUse (fire-and-forget), PostToolResult CAN modify the result
	// before it is committed to history. Match is honored per tool-name glob.
	PostToolResult Event = "post_tool_result"
)

// Hook is one configured command. Match is a tool name, or "*"/"" for any tool.
// Command is run via `sh -c`, so it may be a pipeline. The tool's input arrives on
// stdin (JSON) and the tool name in $CODEAGENT_TOOL_NAME.
//
// The struct carries both yaml tags (config.yaml `hooks:`, layer 0) and json tags
// (the project settings layer's `hooks` block, P11.c) so one type serves both.
type Hook struct {
	Event   Event  `yaml:"event" json:"event"`
	Match   string `yaml:"match" json:"match"`
	Command string `yaml:"command" json:"command"`
}

// Runner executes the configured hooks. Construct with New (which returns nil when
// nothing is configured, so the loop's nil-safe path applies).
type Runner struct {
	hooks []Hook
	root  string
	log   io.Writer
}

// New returns a Runner, or nil when no hooks are configured. The caller assigns
// the result to the agent.ToolHook field only when non-nil, to avoid a typed-nil
// interface.
func New(hs []Hook, root string) *Runner {
	if len(hs) == 0 {
		return nil
	}
	return &Runner{hooks: hs, root: root, log: os.Stderr}
}

// PreToolUse runs the matching pre-hooks in order. The first one to fail (non-zero
// exit) blocks the call; its output is the reason the model sees.
func (r *Runner) PreToolUse(ctx context.Context, tool string, input json.RawMessage) error {
	for _, h := range r.matched(PreToolUse, tool) {
		out, err := r.run(ctx, h, tool, input)
		if err != nil {
			reason := strings.TrimSpace(out)
			if reason == "" {
				reason = err.Error()
			}
			return fmt.Errorf("%s", reason)
		}
	}
	return nil
}

// PostToolUse runs the matching post-hooks, best-effort: a failure is logged but
// does not undo the tool or change its result.
func (r *Runner) PostToolUse(ctx context.Context, tool string, input json.RawMessage, _ string) error {
	var firstErr error
	for _, h := range r.matched(PostToolUse, tool) {
		if out, err := r.run(ctx, h, tool, input); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			fmt.Fprintf(r.log, "[hook] post %q after %s failed: %v\n%s\n", h.Command, tool, err, strings.TrimSpace(out))
		}
	}
	return firstErr
}

func (r *Runner) matched(ev Event, tool string) []Hook {
	var out []Hook
	for _, h := range r.hooks {
		if h.Event == ev && (h.Match == "*" || h.Match == "" || h.Match == tool) {
			out = append(out, h)
		}
	}
	return out
}

func (r *Runner) run(ctx context.Context, h Hook, tool string, input json.RawMessage) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", h.Command)
	cmd.Dir = r.root
	cmd.Stdin = bytes.NewReader(input)
	cmd.Env = append(os.Environ(), "CODEAGENT_TOOL_NAME="+tool)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}

// StopHookInput is the JSON snapshot an after_turn hook receives on stdin.
type StopHookInput struct {
	LastText    string         `json:"lastText"`
	Todos       []StopHookTodo `json:"todos"`
	PlanState   string         `json:"planState"`
	CodeMutated bool           `json:"codeMutated"`
	ToolCalls   int            `json:"toolCalls"`
	MaxSteps    int            `json:"maxSteps"`
}

// StopHookTodo mirrors one checklist item for a stop hook.
type StopHookTodo struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// StopHookVerdict is the JSON an after_turn hook writes to stdout. Exit 0 with
// an empty body, or {"continue":false}, accepts the finish; {"continue":true}
// rejects it and Message is injected as a one-shot ephemeral nudge.
type StopHookVerdict struct {
	Continue bool   `json:"continue"`
	Message  string `json:"message"`
}

// afterTurnHooks returns the configured after_turn hooks. Match is ignored: a
// turn-level stop decision is not tool-scoped, so a hook that sets `match`
// still fires.
func (r *Runner) afterTurnHooks() []Hook {
	if r == nil {
		return nil
	}
	var out []Hook
	for _, h := range r.hooks {
		if h.Event == AfterTurn {
			out = append(out, h)
		}
	}
	return out
}

// HasAfterTurn reports whether an after_turn stop hook is configured. The
// runtime uses it to decide whether to install the external stop policy in
// place of the harness's built-in default.
func (r *Runner) HasAfterTurn() bool {
	return len(r.afterTurnHooks()) > 0
}

// StopDecide consults the first after_turn hook and maps its result to a
// verdict. Non-zero exit and malformed output FAIL CLOSED: the finish is
// rejected (a stop policy that errored must not silently accept a premature
// stop). The loop's step budget bounds the resulting delay, so a buggy hook
// can never deadlock a run.
func (r *Runner) StopDecide(ctx context.Context, in StopHookInput) StopHookVerdict {
	hs := r.afterTurnHooks()
	if len(hs) == 0 {
		return StopHookVerdict{} // nothing configured: accept
	}
	// A stop decision is a single point: the first after_turn hook decides.
	h := hs[0]
	payload, _ := json.Marshal(in)
	out, err := r.run(ctx, h, "after_turn", payload)
	if err != nil {
		msg := strings.TrimSpace(out)
		if msg == "" {
			msg = err.Error()
		}
		return StopHookVerdict{Continue: true, Message: msg}
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return StopHookVerdict{} // exit 0 + empty body accepts
	}
	var v StopHookVerdict
	if jerr := json.Unmarshal([]byte(out), &v); jerr != nil {
		return StopHookVerdict{Continue: true, Message: "[after_turn hook] invalid JSON on stdout: " + jerr.Error()}
	}
	return v
}

// ---- project_trust event ------------------------------------------------

// ProjectTrustInput is the JSON a project_trust hook receives on stdin.
type ProjectTrustInput struct {
	CWD          string `json:"cwd"`
	HasResources bool   `json:"hasResources"`
}

// ProjectTrustVerdict is the JSON a project_trust hook writes to stdout.
type ProjectTrustVerdict struct {
	Decided bool   `json:"-"`     // false when hook was undecided (non-zero exit, empty, or missing "trusted")
	Trusted bool   `json:"trusted"`
	Reason  string `json:"reason"`
}

// projectTrustHooks returns the configured project_trust hooks. Match is
// ignored: project trust is not tool-scoped.
func (r *Runner) projectTrustHooks() []Hook {
	if r == nil {
		return nil
	}
	var out []Hook
	for _, h := range r.hooks {
		if h.Event == ProjectTrust {
			out = append(out, h)
		}
	}
	return out
}

// HasProjectTrustHook reports whether a project_trust hook is configured.
func (r *Runner) HasProjectTrustHook() bool {
	if r == nil {
		return false
	}
	return len(r.projectTrustHooks()) > 0
}

// ProjectTrustDecide consults the first project_trust hook. First-responder
// wins: exit 0 with valid JSON → decided; non-zero exit, empty stdout, or
// invalid JSON → undecided (falls through to the next chain link).
func (r *Runner) ProjectTrustDecide(ctx context.Context, cwd string, hasResources bool) (ProjectTrustVerdict, error) {
	hs := r.projectTrustHooks()
	if len(hs) == 0 {
		return ProjectTrustVerdict{}, nil
	}
	h := hs[0]
	payload, _ := json.Marshal(ProjectTrustInput{CWD: cwd, HasResources: hasResources})
	out, err := r.run(ctx, h, "project_trust", payload)
	if err != nil {
		// Non-zero exit or execution error: hook is undecided.
		return ProjectTrustVerdict{}, nil
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return ProjectTrustVerdict{}, nil
	}
	var v ProjectTrustVerdict
	if jerr := json.Unmarshal([]byte(out), &v); jerr != nil {
		fmt.Fprintf(r.log, "[hook] project_trust %q: invalid JSON: %v\n", h.Command, jerr)
		return ProjectTrustVerdict{}, nil
	}
	v.Decided = true
	return v, nil
}

// ---- context event ------------------------------------------------------

// ContextHookMessage mirrors a generic chat message for context hooks.
type ContextHookMessage struct {
	Role    string                `json:"role"`
	Content string                `json:"content,omitempty"`
	// ToolCalls present on assistant messages with pending tool calls.
	ToolCalls []ContextHookToolCall `json:"tool_calls,omitempty"`
	// ToolCallID present on tool result messages.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// ContextHookToolCall mirrors a tool call within a message.
type ContextHookToolCall struct {
	ID       string                  `json:"id"`
	Type     string                  `json:"type"`
	Function ContextHookFunctionCall `json:"function"`
}

// ContextHookFunctionCall mirrors the function call within a tool call.
type ContextHookFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ContextHookInput is the JSON a context hook receives on stdin.
type ContextHookInput struct {
	Messages  []ContextHookMessage `json:"messages"`
	SessionID string               `json:"session_id"`
	TurnID    string               `json:"turn_id"`
}

// ContextHookOutput is the JSON a context hook writes to stdout.
type ContextHookOutput struct {
	Messages []ContextHookMessage `json:"messages"`
}

// contextHooks returns the configured context hooks. Match is ignored.
func (r *Runner) contextHooks() []Hook {
	if r == nil {
		return nil
	}
	var out []Hook
	for _, h := range r.hooks {
		if h.Event == ContextPreRequest {
			out = append(out, h)
		}
	}
	return out
}

// HasContextHook reports whether any context hook is configured.
func (r *Runner) HasContextHook() bool {
	if r == nil {
		return false
	}
	return len(r.contextHooks()) > 0
}

// RunContextHooks fires all context hooks in order, chaining their outputs.
// Each hook receives the previous hook's output. On error, the current state
// is preserved and execution continues. Returns the final message list.
func (r *Runner) RunContextHooks(ctx context.Context, messages []ContextHookMessage, sessionID, turnID string) []ContextHookMessage {
	current := messages
	for _, h := range r.contextHooks() {
		in := ContextHookInput{
			Messages:  current,
			SessionID: sessionID,
			TurnID:    turnID,
		}
		payload, err := json.Marshal(in)
		if err != nil {
			continue
		}
		out, runErr := r.run(ctx, h, "context", payload)
		if runErr != nil {
			fmt.Fprintf(r.log, "[hook] context %q failed: %v\n%s\n", h.Command, runErr, strings.TrimSpace(out))
			continue
		}
		out = strings.TrimSpace(out)
		if out == "" {
			continue
		}
		var result ContextHookOutput
		if jerr := json.Unmarshal([]byte(out), &result); jerr != nil {
			fmt.Fprintf(r.log, "[hook] context %q: invalid JSON: %v\n", h.Command, jerr)
			continue
		}
		if result.Messages != nil {
			current = result.Messages
		}
	}
	return current
}

// ---- post_tool_result event ---------------------------------------------

// ToolResultInput is the JSON a tool_result hook receives on stdin.
type ToolResultInput struct {
	Tool   string          `json:"tool"`
	Input  json.RawMessage `json:"input"`
	Result ToolResultPayload `json:"result"`
}

// ToolResultPayload mirrors the mutable portion of a tool result.
type ToolResultPayload struct {
	Content string          `json:"content"`
	IsError bool            `json:"isError"`
	Output  json.RawMessage `json:"output,omitempty"`
}

// ToolResultOutput is the JSON a tool_result hook writes to stdout.
type ToolResultOutput struct {
	Result ToolResultPayload `json:"result"`
}

// PostToolResult implements agent.ToolHook. It fires post_tool_result hooks in
// order, chaining their outputs. Each hook receives the previous hook's output.
// On error, the current state is preserved and execution continues.
func (r *Runner) PostToolResult(
	ctx context.Context,
	tool string,
	input json.RawMessage,
	content string,
	isError bool,
	output json.RawMessage,
) (string, bool, json.RawMessage, error) {
	current := ToolResultPayload{
		Content: content,
		IsError: isError,
		Output:  output,
	}
	for _, h := range r.matched(PostToolResult, tool) {
		in := ToolResultInput{Tool: tool, Input: input, Result: current}
		payload, err := json.Marshal(in)
		if err != nil {
			continue
		}
		out, runErr := r.run(ctx, h, tool, payload)
		if runErr != nil {
			fmt.Fprintf(r.log, "[hook] post_tool_result %q after %s failed: %v\n%s\n", h.Command, tool, runErr, strings.TrimSpace(out))
			continue
		}
		out = strings.TrimSpace(out)
		if out == "" {
			continue
		}
		var result ToolResultOutput
		if jerr := json.Unmarshal([]byte(out), &result); jerr != nil {
			fmt.Fprintf(r.log, "[hook] post_tool_result %q after %s: invalid JSON: %v\n", h.Command, tool, jerr)
			continue
		}
		current = result.Result
	}
	return current.Content, current.IsError, current.Output, nil
}
