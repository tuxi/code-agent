package conversation

import (
	"context"
	"fmt"
	"strings"

	"code-agent/internal/model"
)

// LLMTitleGenerator calls a provider to generate a concise title from the
// conversation's first exchange. It uses a simple, low-temperature prompt so
// the title is deterministic and cheap (typically < 200 tokens).
type LLMTitleGenerator struct {
	Provider model.Provider
	Model    string
}

// NewLLMTitleGenerator creates a TitleGenerator backed by the given provider.
func NewLLMTitleGenerator(provider model.Provider, modelName string) *LLMTitleGenerator {
	return &LLMTitleGenerator{Provider: provider, Model: modelName}
}

// GenerateTitle produces a 3-6 word title by prompting the model with the
// first user message and assistant response. Messages longer than 800 chars are
// truncated to keep the prompt small.
func (g *LLMTitleGenerator) GenerateTitle(ctx context.Context, userMessage, assistantResponse string) (string, error) {
	userMsg := strings.TrimSpace(userMessage)
	assistantMsg := strings.TrimSpace(assistantResponse)

	if userMsg == "" {
		return "", fmt.Errorf("no user message to generate title from")
	}

	// Truncate each to keep the prompt small.
	const maxLen = 800
	if len(userMsg) > maxLen {
		userMsg = userMsg[:maxLen]
	}
	if len(assistantMsg) > maxLen {
		assistantMsg = assistantMsg[:maxLen]
	}

	prompt := fmt.Sprintf(
		"Generate a short, concise title (3-6 words) for a conversation that starts with:\n\n"+
			"User: %s\n\nAssistant: %s\n\nTitle:",
		userMsg, assistantMsg,
	)

	resp, err := g.Provider.Complete(ctx, model.Request{
		Model:       g.Model,
		Messages:    []model.Message{{Role: model.RoleUser, Content: prompt}},
		Temperature: 0.3,
	})
	if err != nil {
		return "", err
	}

	title := strings.TrimSpace(resp.Content)
	// Strip surrounding quotes.
	title = strings.Trim(title, `"'`)
	// Limit to 80 chars (reasonable display width).
	if len(title) > 80 {
		title = title[:80]
	}
	return title, nil
}
