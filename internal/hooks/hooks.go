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
