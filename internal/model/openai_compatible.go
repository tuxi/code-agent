package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type OpenAICompatibleProvider struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

func NewOpenAICompatibleProvider(baseURL, apiKey string) *OpenAICompatibleProvider {
	return &OpenAICompatibleProvider{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		// No client-level timeout: timeout policy lives in one place, the
		// ResilientProvider, which sets a per-attempt deadline via context. The
		// request already threads ctx (NewRequestWithContext), so cancellation
		// and deadlines are honored. Callers that use this provider unwrapped
		// must pass a context with a deadline.
		HTTPClient: &http.Client{},
	}
}

type chatCompletionRequest struct {
	Model       string           `json:"model"`
	Messages    []Message        `json:"messages"`
	Temperature float64          `json:"temperature,omitempty"`
	Tools       []ToolDefinition `json:"tools,omitempty"`
	ToolChoice  string           `json:"tool_choice,omitempty"`
}

type chatCompletionResponse struct {
	Choices []struct {
		Message struct {
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		// Cached-input accounting, reported under different keys per provider:
		PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"` // deepseek
		PromptTokensDetails  struct {
			CachedTokens int `json:"cached_tokens"` // openai-style
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error,omitempty"`
}

func (p *OpenAICompatibleProvider) Complete(ctx context.Context, req Request) (Response, error) {
	if p.APIKey == "" {
		return Response{}, fmt.Errorf("missing api key")
	}
	if p.BaseURL == "" {
		return Response{}, fmt.Errorf("missing base url")
	}
	if req.Model == "" {
		return Response{}, fmt.Errorf("missing model")
	}

	body := chatCompletionRequest{
		Model:       req.Model,
		Messages:    req.Messages,
		Temperature: req.Temperature,
		Tools:       req.Tools,
		ToolChoice:  req.ToolChoice,
	}

	data, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}

	httpReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		p.BaseURL+"/chat/completions",
		bytes.NewReader(data),
	)
	if err != nil {
		return Response{}, err
	}

	httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.HTTPClient.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, err
	}

	// Classify by status BEFORE decoding: a 5xx often returns a non-JSON body
	// (proxy/HTML error page), and we must not mask a retryable status as a
	// "decode response" failure. Parse the structured error best-effort for a
	// better message.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &APIError{StatusCode: resp.StatusCode, Body: string(raw)}
		var decoded chatCompletionResponse
		if json.Unmarshal(raw, &decoded) == nil && decoded.Error != nil {
			apiErr.Type = decoded.Error.Type
			apiErr.Message = decoded.Error.Message
		}
		return Response{}, apiErr
	}

	var decoded chatCompletionResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Response{}, fmt.Errorf("decode response: %w; raw=%s", err, string(raw))
	}

	if len(decoded.Choices) == 0 {
		return Response{}, fmt.Errorf("model api returned no choices: raw=%s", string(raw))
	}

	// Cached-prompt tokens: prefer deepseek's explicit field, fall back to the
	// OpenAI-style nested detail. Either way it is a portion of PromptTokens.
	cached := decoded.Usage.PromptCacheHitTokens
	if cached == 0 {
		cached = decoded.Usage.PromptTokensDetails.CachedTokens
	}

	choice := decoded.Choices[0]
	return Response{
		Content:      strings.TrimSpace(choice.Message.Content),
		ToolCalls:    choice.Message.ToolCalls,
		FinishReason: choice.FinishReason,
		Usage: Usage{
			PromptTokens:       decoded.Usage.PromptTokens,
			CompletionTokens:   decoded.Usage.CompletionTokens,
			TotalTokens:        decoded.Usage.TotalTokens,
			CachedPromptTokens: cached,
		},
		Raw: raw,
	}, nil
}
