package model

import (
	"bufio"
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
	Model         string           `json:"model"`
	Messages      []Message        `json:"messages"`
	Temperature   float64          `json:"temperature,omitempty"`
	Tools         []ToolDefinition `json:"tools,omitempty"`
	ToolChoice    string           `json:"tool_choice,omitempty"`
	Stream        bool             `json:"stream,omitempty"`
	StreamOptions *streamOptions   `json:"stream_options,omitempty"`
}

// streamOptions asks the provider to include a final usage chunk in the SSE
// stream, so streamed calls still report token usage for cost accounting.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
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

// streamChunk is one SSE delta in an OpenAI-compatible streaming response.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens         int `json:"prompt_tokens"`
		CompletionTokens     int `json:"completion_tokens"`
		TotalTokens          int `json:"total_tokens"`
		PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
		PromptTokensDetails  struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// CompleteStream is the streaming form of Complete (StreamingProvider). It calls
// onText for each text delta as it arrives, accumulates tool-call deltas (the
// loop needs them whole), and returns the same complete Response Complete would —
// so everything downstream is identical; only a renderer saw the text live.
func (p *OpenAICompatibleProvider) CompleteStream(ctx context.Context, req Request, onText func(string)) (Response, error) {
	if p.APIKey == "" {
		return Response{}, fmt.Errorf("missing api key")
	}
	if p.BaseURL == "" || req.Model == "" {
		return Response{}, fmt.Errorf("missing base url or model")
	}

	data, err := json.Marshal(chatCompletionRequest{
		Model: req.Model, Messages: req.Messages, Temperature: req.Temperature,
		Tools: req.Tools, ToolChoice: req.ToolChoice,
		Stream: true, StreamOptions: &streamOptions{IncludeUsage: true},
	})
	if err != nil {
		return Response{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.HTTPClient.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return Response{}, &APIError{StatusCode: resp.StatusCode, Body: string(raw)}
	}

	var content strings.Builder
	calls := map[int]*ToolCall{}
	var order []int
	var finishReason string
	var usage Usage

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // SSE lines can be large
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "[DONE]" {
			break
		}
		var chunk streamChunk
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue // tolerate keep-alives / partial lines
		}
		if u := chunk.Usage; u != nil {
			cached := u.PromptCacheHitTokens
			if cached == 0 {
				cached = u.PromptTokensDetails.CachedTokens
			}
			usage = Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens, CachedPromptTokens: cached}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		ch := chunk.Choices[0]
		if ch.FinishReason != "" {
			finishReason = ch.FinishReason
		}
		if ch.Delta.Content != "" {
			content.WriteString(ch.Delta.Content)
			if onText != nil {
				onText(ch.Delta.Content)
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			acc := calls[tc.Index]
			if acc == nil {
				acc = &ToolCall{Type: "function"}
				calls[tc.Index] = acc
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				acc.ID = tc.ID
			}
			if tc.Function.Name != "" {
				acc.Function.Name = tc.Function.Name
			}
			acc.Function.Arguments += tc.Function.Arguments
		}
	}
	if err := sc.Err(); err != nil {
		return Response{}, err
	}

	toolCalls := make([]ToolCall, 0, len(order))
	for _, idx := range order {
		toolCalls = append(toolCalls, *calls[idx])
	}
	return Response{
		Content:      strings.TrimSpace(content.String()),
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
		Usage:        usage,
	}, nil
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
