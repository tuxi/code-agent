package conversation

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"code-agent/internal/model"
)

// LLMTitleGenerator calls a provider to generate a concise title from the
// conversation's user input. It uses a simple, low-temperature prompt so
// the title is deterministic and cheap (typically < 200 tokens).
type LLMTitleGenerator struct {
	Provider model.Provider
	Model    string
}

// NewLLMTitleGenerator creates a TitleGenerator backed by the given provider.
func NewLLMTitleGenerator(provider model.Provider, modelName string) *LLMTitleGenerator {
	return &LLMTitleGenerator{Provider: provider, Model: modelName}
}

// GenerateTitle summarizes the user's request before an assistant reply exists.
func (g *LLMTitleGenerator) GenerateTitle(ctx context.Context, sessionID, userMessage, _ string) (string, error) {
	userMessage = strings.TrimSpace(userMessage)
	if userMessage == "" {
		return "", fmt.Errorf("no user message to generate title from")
	}
	if utf8.RuneCountInString(userMessage) > 900 {
		userMessage = string([]rune(userMessage)[:900])
	}
	resp, err := g.Provider.Complete(ctx, model.Request{
		SessionID: sessionID,
		Model:     g.Model,
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: fmt.Sprintf("Write a concise conversation title in the user's language, using only their request as context. Return only a single title on one line, at most %d characters. Prefer 6–12 Chinese characters or 3–6 English words. Preserve relevant ticket or error codes. Do not answer the request.", MaxGeneratedTitleChars)},
			{Role: model.RoleUser, Content: userMessage},
		},
		Temperature: 0.3,
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(resp.Content), `"'`)), nil
}
