package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"code-agent/internal/credential"
)

// OpenAICompatibleProvider speaks the OpenAI-compatible /v1/chat/completions
// protocol. It supports both static API keys (backward compatible) and dynamic
// credentials via credential.Resolver (for Gateway JWT, MCP OAuth, etc.).
type OpenAICompatibleProvider struct {
	BaseURL    string
	HTTPClient *http.Client

	// Credential, when non-nil, resolves the credential dynamically on each
	// request. CredentialTarget is passed to Credential.Resolve() to identify
	// which service this provider is calling.
	//
	// When Credential is nil, the provider falls back to the static APIKey
	// field (backward compatible path).
	Credential       credential.Resolver
	CredentialTarget credential.Target

	// APIKey is the static API key, used when Credential is nil.
	// Deprecated: set Credential + CredentialTarget instead.
	APIKey string

	// ObjectUploader is optional test wiring for Gateway asset uploads. Nil uses
	// an Aliyun STS direct uploader; it is never used for ordinary chat calls.
	ObjectUploader GatewayObjectUploader
}

// NewOpenAICompatibleProvider creates a provider that resolves credentials
// dynamically via cred. The target identifies which service this provider calls.
//
// When cred is nil, the provider assumes no authentication is needed (local
// models, or HTTPClient.Transport handles it).
func NewOpenAICompatibleProvider(baseURL string, cred credential.Resolver, target credential.Target) *OpenAICompatibleProvider {
	return &OpenAICompatibleProvider{
		BaseURL:          strings.TrimRight(baseURL, "/"),
		Credential:       cred,
		CredentialTarget: target,
		HTTPClient:       defaultHTTPClient(),
	}
}

// NewOpenAICompatibleProviderWithKey creates a provider with a static API key.
// Internally it wraps the key in a StaticResolver so the credential path is
// identical — only the source differs.
//
// Deprecated: use NewOpenAICompatibleProvider with a credential.Resolver.
// This constructor is kept for backward compatibility and will be removed
// in a future major version.
func NewOpenAICompatibleProviderWithKey(baseURL, apiKey string) *OpenAICompatibleProvider {
	p := &OpenAICompatibleProvider{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		APIKey:     apiKey,
		HTTPClient: defaultHTTPClient(),
	}
	// If an API key is provided, also wire it through the credential path so
	// applyAuth has a single code path.
	if apiKey != "" {
		p.Credential = credential.StaticResolver{
			{Namespace: "llm", Name: "default"}: {Type: credential.Bearer, Secret: apiKey},
		}
		p.CredentialTarget = credential.Target{Namespace: "llm", Name: "default"}
	}
	return p
}

// defaultHTTPClient returns the standard HTTP client used by providers.
func defaultHTTPClient() *http.Client {
	// No total Timeout: it is a hard ceiling on the WHOLE exchange including
	// the response body, so a fixed value silently kills any streamed or long
	// generation that runs past it (the classic "context deadline exceeded
	// ... while reading body" on long tasks). Per-attempt total time is
	// governed by ResilientProvider's context deadline
	// (request_timeout_seconds) instead. Here we only bound the phases that
	// SHOULD have a hard ceiling — connect, TLS, and time to first response
	// byte — none of which scale with generation length.
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
		},
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

// hasCredential reports whether this provider has any means of authentication
// (either a dynamic credential resolver or a static API key).
func (p *OpenAICompatibleProvider) hasCredential() bool {
	return p.Credential != nil || p.APIKey != ""
}

// applyAuth resolves the credential for this provider and sets the Authorization
// header on req. When Credential is set, it resolves dynamically; otherwise it
// falls back to the static APIKey field.
func (p *OpenAICompatibleProvider) applyAuth(ctx context.Context, req *http.Request) error {
	if p.Credential != nil {
		c, err := p.Credential.Resolve(ctx, p.CredentialTarget)
		if err != nil {
			return fmt.Errorf("resolve credential for %v: %w", p.CredentialTarget, err)
		}
		if !c.IsZero() {
			switch c.Type {
			case credential.Bearer:
				req.Header.Set("Authorization", "Bearer "+c.Secret)
			case credential.Secret:
				// Non-Bearer — HTTPClient.Transport handles the details.
			case credential.None:
				// No auth needed.
			}
			return nil
		}
	}
	// Fallback to static API key (backward compatible path).
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	return nil
}

// IsLocalBaseURL reports whether urlStr points to a loopback address. Local model
// servers (Ollama, vLLM, llama.cpp, LM Studio) run on localhost and do not require
// an API key, so both the config layer and the provider layer skip the key check
// for these endpoints.
func IsLocalBaseURL(urlStr string) bool {
	if urlStr == "" {
		return false
	}
	u, err := url.Parse(urlStr)
	if err != nil {
		return false
	}
	host := u.Hostname()
	switch host {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	return false
}

// CompleteStream is the streaming form of Complete (StreamingProvider). It calls
// onText for each text delta as it arrives, accumulates tool-call deltas (the
// loop needs them whole), and returns the same complete Response Complete would —
// so everything downstream is identical; only a renderer saw the text live.
func (p *OpenAICompatibleProvider) CompleteStream(ctx context.Context, req Request, onText func(string)) (Response, error) {
	// Local endpoints (Ollama etc.) do not require a credential.
	if !p.hasCredential() && !IsLocalBaseURL(p.BaseURL) {
		return Response{}, fmt.Errorf("missing credential")
	}
	if p.BaseURL == "" {
		return Response{}, fmt.Errorf("missing base url")
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
	if err := p.applyAuth(ctx, httpReq); err != nil {
		return Response{}, err
	}
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
	// Local endpoints (Ollama etc.) do not require a credential.
	if !p.hasCredential() && !IsLocalBaseURL(p.BaseURL) {
		return Response{}, fmt.Errorf("missing credential")
	}
	if p.BaseURL == "" {
		return Response{}, fmt.Errorf("missing base url")
	}
	// Model may be empty for Gateway — the Gateway server selects the model.
	// Non-Gateway providers reject empty models at the API level.

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

	if err := p.applyAuth(ctx, httpReq); err != nil {
		return Response{}, err
	}
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
