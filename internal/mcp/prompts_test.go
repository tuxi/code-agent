package mcp

import (
	"context"
	"io"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type fakePromptCaller struct {
	pages    [][]*mcpsdk.Prompt
	listCall int
	gotArgs  map[string]string // captured from the last GetPrompt
	messages []*mcpsdk.PromptMessage
	getErr   error
}

func (f *fakePromptCaller) ListPrompts(_ context.Context, _ *mcpsdk.ListPromptsParams) (*mcpsdk.ListPromptsResult, error) {
	i := f.listCall
	f.listCall++
	res := &mcpsdk.ListPromptsResult{Prompts: f.pages[i]}
	if i < len(f.pages)-1 {
		res.NextCursor = "more"
	}
	return res, nil
}

func (f *fakePromptCaller) GetPrompt(_ context.Context, p *mcpsdk.GetPromptParams) (*mcpsdk.GetPromptResult, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	f.gotArgs = p.Arguments
	return &mcpsdk.GetPromptResult{Messages: f.messages}, nil
}

// managerWith builds a Manager whose prompt registry is discovered from f (server "gh").
func managerWith(t *testing.T, f *fakePromptCaller) *Manager {
	t.Helper()
	entries, err := discoverPrompts(context.Background(), f, "gh", io.Discard)
	if err != nil {
		t.Fatalf("discoverPrompts: %v", err)
	}
	return &Manager{trace: io.Discard, prompts: entries}
}

func prReviewCaller() *fakePromptCaller {
	return &fakePromptCaller{
		pages: [][]*mcpsdk.Prompt{
			{{Name: "pr_review", Description: "Review a PR", Arguments: []*mcpsdk.PromptArgument{
				{Name: "pr", Required: true}, {Name: "depth"},
			}}},
			{{Name: "changelog"}}, // second page, exercises pagination
		},
		messages: []*mcpsdk.PromptMessage{
			{Role: "user", Content: &mcpsdk.TextContent{Text: "Review PR"}},
		},
	}
}

func TestDiscoverPromptsPaginates(t *testing.T) {
	f := prReviewCaller()
	m := managerWith(t, f)
	if f.listCall != 2 {
		t.Fatalf("expected 2 ListPrompts calls, got %d", f.listCall)
	}
	specs := m.Prompts()
	if len(specs) != 2 {
		t.Fatalf("expected 2 prompts, got %d", len(specs))
	}
	// Sorted by command; changelog < pr_review.
	if specs[0].Command != "mcp__gh__changelog" || specs[1].Command != "mcp__gh__pr_review" {
		t.Fatalf("commands/order wrong: %q, %q", specs[0].Command, specs[1].Command)
	}
	if specs[1].Args[0].Name != "pr" || !specs[1].Args[0].Required {
		t.Fatalf("pr_review args not carried: %+v", specs[1].Args)
	}
}

// Positional args map onto declared argument order; result messages render to text.
func TestRenderPromptMapsPositionalArgs(t *testing.T) {
	f := prReviewCaller()
	m := managerWith(t, f)

	text, err := m.RenderPrompt(context.Background(), "mcp__gh__pr_review", []string{"456", "deep"})
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	if f.gotArgs["pr"] != "456" || f.gotArgs["depth"] != "deep" {
		t.Fatalf("positional args not mapped: %v", f.gotArgs)
	}
	if text != "Review PR" {
		t.Fatalf("single-message render should be bare text, got %q", text)
	}
}

func TestRenderPromptMissingRequired(t *testing.T) {
	m := managerWith(t, prReviewCaller())
	if _, err := m.RenderPrompt(context.Background(), "mcp__gh__pr_review", nil); err == nil {
		t.Fatal("missing required 'pr' should error")
	}
}

func TestRenderPromptUnknown(t *testing.T) {
	m := managerWith(t, prReviewCaller())
	if _, err := m.RenderPrompt(context.Background(), "mcp__gh__nope", nil); err == nil {
		t.Fatal("unknown prompt command should error")
	}
}

// Multi-message results get role prefixes; single messages do not.
func TestRenderPromptMessagesRoles(t *testing.T) {
	single := renderPromptMessages([]*mcpsdk.PromptMessage{
		{Role: "user", Content: &mcpsdk.TextContent{Text: "hello"}},
	})
	if single != "hello" {
		t.Fatalf("single message should be bare, got %q", single)
	}
	multi := renderPromptMessages([]*mcpsdk.PromptMessage{
		{Role: "user", Content: &mcpsdk.TextContent{Text: "q"}},
		{Role: "assistant", Content: &mcpsdk.TextContent{Text: "a"}},
	})
	if !strings.Contains(multi, "[user] q") || !strings.Contains(multi, "[assistant] a") {
		t.Fatalf("multi message should prefix roles, got %q", multi)
	}
}

func TestPromptHelp(t *testing.T) {
	m := managerWith(t, prReviewCaller())
	help := m.PromptHelp()
	if !strings.Contains(help, "/mcp__gh__pr_review <pr> [depth]") {
		t.Fatalf("help should show required <pr> and optional [depth]:\n%s", help)
	}
	// Empty manager reports none.
	if got := (&Manager{}).PromptHelp(); got != "(no MCP prompts available)" {
		t.Fatalf("empty PromptHelp = %q", got)
	}
}
