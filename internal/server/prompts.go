package server

import (
	"context"

	"code-agent/internal/mcp"
)

// PromptService exposes the MCP *prompts* primitive to wire clients: list the
// available prompts (GET /v1/prompts) and invoke one by rendering its template to
// turn text server-side (the invoke_prompt control message). Satisfied by
// *mcp.Manager. Nil disables prompts on the wire.
//
// Prompts are user-invoked, and only the server holds the MCP session, so a
// client cannot render a prompt itself — it sends invoke_prompt with the command
// + args and the server renders, then runs the result as a normal turn.
type PromptService interface {
	Prompts() []mcp.PromptSpec
	RenderPrompt(ctx context.Context, command string, args []string) (string, error)
}

// promptDTO is the stable wire shape for GET /v1/prompts (mcp.PromptSpec has no
// JSON tags, so we project it explicitly).
type promptDTO struct {
	Command     string         `json:"command"` // invoke as {"type":"invoke_prompt","command":...}
	Server      string         `json:"server"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Args        []promptArgDTO `json:"args,omitempty"`
}

type promptArgDTO struct {
	Name        string `json:"name"`
	Required    bool   `json:"required,omitempty"`
	Description string `json:"description,omitempty"`
}

// promptsResponse is the GET /v1/prompts body.
type promptsResponse struct {
	Prompts []promptDTO `json:"prompts"`
}

func toPromptsResponse(specs []mcp.PromptSpec) promptsResponse {
	out := promptsResponse{Prompts: make([]promptDTO, 0, len(specs))}
	for _, s := range specs {
		d := promptDTO{Command: s.Command, Server: s.Server, Name: s.Name, Description: s.Description}
		for _, a := range s.Args {
			d.Args = append(d.Args, promptArgDTO{Name: a.Name, Required: a.Required, Description: a.Description})
		}
		out.Prompts = append(out.Prompts, d)
	}
	return out
}
