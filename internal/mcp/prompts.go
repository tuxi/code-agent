package mcp

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file implements the MCP *prompts* primitive (P1): reusable, parameterized
// instruction templates a server exposes. Unlike tools (model-invoked) and
// resources (model-pulled context), prompts are USER-controlled — the user picks
// one explicitly. We surface them as slash commands `/mcp__<server>__<prompt>`;
// invoking one calls prompts/get on the server and injects the returned messages
// as the turn's input (see the REPL loop). Discovery + rendering live here,
// frontend-agnostic, so the CLI, TUI, and wire server can all reuse them.

// promptCaller is the slice of *mcpsdk.ClientSession the prompt registry needs.
type promptCaller interface {
	ListPrompts(ctx context.Context, params *mcpsdk.ListPromptsParams) (*mcpsdk.ListPromptsResult, error)
	GetPrompt(ctx context.Context, params *mcpsdk.GetPromptParams) (*mcpsdk.GetPromptResult, error)
}

// PromptArg is one declared argument of a prompt.
type PromptArg struct {
	Name        string
	Required    bool
	Description string
}

// PromptSpec describes a discovered prompt for a frontend to list. Command is the
// slash-command name without the leading "/" (mcp__<server>__<prompt>), matching
// the tool wire-name convention so the whole MCP namespace is consistent.
type PromptSpec struct {
	Command     string
	Server      string
	Name        string // the prompt's real name on the server
	Description string
	Args        []PromptArg
}

// promptEntry is the internal record: the spec plus the session to invoke.
type promptEntry struct {
	spec   PromptSpec
	caller promptCaller
	log    io.Writer
}

// discoverPrompts pages prompts/list and builds entries for one server.
func discoverPrompts(ctx context.Context, caller promptCaller, server string, log io.Writer) ([]*promptEntry, error) {
	if log == nil {
		log = io.Discard
	}
	var out []*promptEntry
	params := &mcpsdk.ListPromptsParams{}
	for {
		res, err := caller.ListPrompts(ctx, params)
		if err != nil {
			return nil, err
		}
		for _, p := range res.Prompts {
			out = append(out, &promptEntry{
				caller: caller,
				log:    log,
				spec: PromptSpec{
					Command:     wireName(server, p.Name),
					Server:      server,
					Name:        p.Name,
					Description: promptDescription(p),
					Args:        toPromptArgs(p.Arguments),
				},
			})
		}
		if res.NextCursor == "" {
			break
		}
		params.Cursor = res.NextCursor
	}
	return out, nil
}

func promptDescription(p *mcpsdk.Prompt) string {
	if p.Description != "" {
		return p.Description
	}
	return p.Title
}

func toPromptArgs(args []*mcpsdk.PromptArgument) []PromptArg {
	out := make([]PromptArg, 0, len(args))
	for _, a := range args {
		out = append(out, PromptArg{Name: a.Name, Required: a.Required, Description: a.Description})
	}
	return out
}

// Prompts returns the discovered prompts across all connected servers, sorted by
// command, for a frontend to list (e.g. the REPL's /prompts, a TUI palette).
func (m *Manager) Prompts() []PromptSpec {
	specs := make([]PromptSpec, 0, len(m.prompts))
	for _, e := range m.prompts {
		specs = append(specs, e.spec)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Command < specs[j].Command })
	return specs
}

// RenderPrompt invokes a prompt by its command name (mcp__<server>__<prompt>,
// without the leading "/") with positional arguments mapped onto the prompt's
// declared argument order, and returns the server's rendered messages as text to
// feed into a turn. A missing required argument or unknown command is an error.
func (m *Manager) RenderPrompt(ctx context.Context, command string, args []string) (string, error) {
	var e *promptEntry
	for _, cand := range m.prompts {
		if cand.spec.Command == command {
			e = cand
			break
		}
	}
	if e == nil {
		return "", fmt.Errorf("unknown MCP prompt %q (see /prompts)", command)
	}

	// Positional → named: the i-th typed argument fills the i-th declared argument.
	argMap := make(map[string]string, len(e.spec.Args))
	for i, a := range e.spec.Args {
		if i < len(args) {
			argMap[a.Name] = args[i]
		} else if a.Required {
			return "", fmt.Errorf("prompt %q: missing required argument %q", command, a.Name)
		}
	}
	fmt.Fprintf(e.log, "[mcp] prompt %s args=%v\n", label(e.spec.Server, e.spec.Name), args)

	res, err := e.caller.GetPrompt(ctx, &mcpsdk.GetPromptParams{Name: e.spec.Name, Arguments: argMap})
	if err != nil {
		return "", fmt.Errorf("mcp: protocol error: %s: %w", command, err)
	}
	return renderPromptMessages(res.Messages), nil
}

// renderPromptMessages flattens a prompt result's messages into a single text
// block to seed a turn. Role labels are prefixed only when there is more than one
// message (a single filled-template message is the common case and needs no
// decoration).
func renderPromptMessages(msgs []*mcpsdk.PromptMessage) string {
	multi := len(msgs) > 1
	parts := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		text := promptContentText(msg.Content)
		if multi {
			parts = append(parts, fmt.Sprintf("[%s] %s", msg.Role, text))
		} else {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func promptContentText(c mcpsdk.Content) string {
	if t, ok := c.(*mcpsdk.TextContent); ok {
		return t.Text
	}
	return fmt.Sprintf("[non-text prompt content: %T omitted]", c)
}

// PromptHelp renders the available prompts for the REPL's /prompts command.
func (m *Manager) PromptHelp() string {
	specs := m.Prompts()
	if len(specs) == 0 {
		return "(no MCP prompts available)"
	}
	var b strings.Builder
	b.WriteString("Available MCP prompts (invoke as /<command> [args]):\n")
	for _, s := range specs {
		fmt.Fprintf(&b, "  /%s", s.Command)
		for _, a := range s.Args {
			if a.Required {
				fmt.Fprintf(&b, " <%s>", a.Name)
			} else {
				fmt.Fprintf(&b, " [%s]", a.Name)
			}
		}
		if s.Description != "" {
			fmt.Fprintf(&b, "  — %s", s.Description)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
