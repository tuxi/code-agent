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
