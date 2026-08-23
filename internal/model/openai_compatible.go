package model

import (
	"bufio"
	"bytes"
	"code-agent/pkg"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
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

// loadSystemRootCAs returns a CertPool backed by the system CA bundle
// (/etc/ssl/cert.pem). On macOS this bypasses the Security.framework path
// (SecPolicyCreateSSL), which can intermittently return NULL in hardened
// runtime child processes and cause "tls: failed to verify certificate:
// SecPolicyCreateSSL error: 0". Returns nil if the bundle is unavailable,
// in which case Go falls back to its default system root pool.
func loadSystemRootCAs() *x509.CertPool {
	data, err := os.ReadFile("/etc/ssl/cert.pem")
	if err != nil {
		return nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil
	}
	return pool
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
			TLSClientConfig:       &tls.Config{RootCAs: loadSystemRootCAs()},
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

type chatCompletionRequest struct {
	SessionID     string            `json:"session_id,omitempty"`
	TurnID        string            `json:"turn_id,omitempty"`
	RequestID     string            `json:"request_id,omitempty"`
	ExecutionID   string            `json:"execution_id,omitempty"`
	Model         string            `json:"model"`
	Messages      []wireMessage     `json:"messages"`
	Temperature   float64           `json:"temperature,omitempty"`
	Tools         *[]ToolDefinition `json:"tools,omitempty"`
	ToolChoice    string            `json:"tool_choice,omitempty"`
	Stream        bool              `json:"stream,omitempty"`
	StreamOptions *streamOptions    `json:"stream_options,omitempty"`
}

// wireMessage is the on-the-wire form of a Message. Content is a plain string
// for text-only messages (the historical shape every OpenAI-compatible endpoint
// accepts); when the loop assembled multimodal ContentParts, Content becomes a
// content-block array instead. Assets ride along for the Agent Gateway contract
// (which resolves them server-side); LocalAssets/OriginTurnID are runtime and
// persistence state and never serialized here.
type wireMessage struct {
	Role       Role              `json:"role"`
	Content    any               `json:"content"`
	Assets     []GatewayAssetRef `json:"assets,omitempty"`
	ToolCalls  []ToolCall        `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
}

func newWireMessages(messages []Message) []wireMessage {
	out := make([]wireMessage, 0, len(messages)+4)
	// pendingToolImages accumulates image parts from consecutive tool messages:
	// chat completions cannot carry images in tool messages (DeepSeek rejects
	// them), so they are promoted into ONE synthetic user message after the
	// run of tool results, keeping the assistant tool_calls / tool-result
	// pairing intact.
	var pendingToolImages []ContentPart
	flushToolImages := func() {
		if len(pendingToolImages) == 0 {
			return
		}
		// The synthetic message exists only to carry the images; it must not be
		// mistaken for a user-authored turn. Content is a block list: the
		// provenance note leads, images follow.
		blocks := make([]ContentPart, 0, len(pendingToolImages)+1)
		blocks = append(blocks, ContentPart{Type: "text", Text: "[Images returned by the tools above are attached for you to inspect.]"})
		blocks = append(blocks, pendingToolImages...)
		out = append(out, wireMessage{
			Role:    RoleUser,
			Content: blocks,
		})
		pendingToolImages = nil
	}
	for _, m := range messages {
		w := wireMessage{
			Role:       m.Role,
			Assets:     m.Assets,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
		}
		if len(m.ContentParts) > 0 {
			parts := make([]ContentPart, len(m.ContentParts))
			copy(parts, m.ContentParts)
			if m.Role == RoleTool {
				// Tool results stay text-only on this wire; their images ride
				// the next synthetic user message.
				pendingToolImages = append(pendingToolImages, parts...)
				w.Content = m.Content
			} else {
				// The message's text content leads the block array so the model
				// reads the prompt before the images.
				if m.Content != "" {
					parts = append([]ContentPart{{Type: "text", Text: m.Content}}, parts...)
				}
				w.Content = parts
			}
		} else {
			w.Content = m.Content
		}
		out = append(out, w)
		// Flush before any non-tool message so the images land between the tool
		// results and whatever follows (assistant reply or new user turn).
		if m.Role != RoleTool {
			flushToolImages()
		}
	}
	flushToolImages()
	return out
}

// streamOptions asks the provider to include a final usage chunk in the SSE
// stream, so streamed calls still report token usage for cost accounting.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatCompletionResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			// ReasoningContent is the DeepSeek/vLLM-style reasoning channel;
			// OpenRouter delivers the same data as Reasoning /
			// ReasoningDetails (see streamChunk.Delta).
			ReasoningContent string            `json:"reasoning_content"`
			Reasoning        string            `json:"reasoning"`
			ReasoningDetails []reasoningDetail `json:"reasoning_details"`
			ToolCalls        []ToolCall        `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int   `json:"prompt_tokens"`
		CompletionTokens int   `json:"completion_tokens"`
		TotalTokens      int   `json:"total_tokens"`
		BillingUnits     int64 `json:"billing_units"`
		// Cached-input accounting, reported under different keys per provider:
		PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"` // deepseek
		PromptTokensDetails  struct {
			CachedTokens int `json:"cached_tokens"` // openai-style
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *openAIErrorPayload `json:"error,omitempty"`
}

type openAIErrorPayload struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

// reasoningDetail is one entry of OpenRouter's reasoning_details array — its
// normalized shape for reasoning output across providers (DeepSeek-style
// providers use the flat reasoning_content string instead). Only the text
// variants carry displayable reasoning; encrypted/summary-only entries are
// tolerated but contribute nothing.
type reasoningDetail struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// Summary backs the "reasoning.summary" type, whose payload field differs
	// from "reasoning.text".
	Summary string `json:"summary"`
}

// reasoningText flattens one reasoning detail to its displayable text, or ""
// for non-text variants (encrypted blocks, redacted payloads).
func (d reasoningDetail) reasoningText() string {
	switch d.Type {
	case "reasoning.text":
		return d.Text
	case "reasoning.summary":
		return d.Summary
	default:
		return ""
	}
}

// streamChunk is one SSE delta in an OpenAI-compatible streaming response.
type streamChunk struct {
	// Error is emitted by an OpenAI-compatible gateway when the upstream request
	// failed after the HTTP response had already switched to SSE. It must not be
	// ignored as an empty chunk: doing so turns a failed turn into an empty
	// turn_finished event at the runtime layer.
	Error   *openAIErrorPayload `json:"error,omitempty"`
	Choices []struct {
		Delta struct {
			Content string `json:"content"` // final-answer text delta
			// ReasoningContent is the DeepSeek/vLLM-style reasoning channel.
			// OpenRouter normalizes the same data into Reasoning (its wire name,
			// with reasoning_content accepted as an input alias only) and the
			// structured ReasoningDetails array; all three are accumulated.
			ReasoningContent string            `json:"reasoning_content"`
			Reasoning        string            `json:"reasoning"`
			ReasoningDetails []reasoningDetail `json:"reasoning_details"`
			ToolCalls        []struct {
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
		PromptTokens         int   `json:"prompt_tokens"`
		CompletionTokens     int   `json:"completion_tokens"`
		TotalTokens          int   `json:"total_tokens"`
		BillingUnits         int64 `json:"billing_units"`
		PromptCacheHitTokens int   `json:"prompt_cache_hit_tokens"`
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
	if host == "localhost" {
		return true
	}
	return pkg.IsInnerIP(host)
}

// CompleteStream is the streaming form of Complete (StreamingProvider). It calls
// onText/onReasoning for their respective deltas as they arrive, accumulates
// tool-call deltas (the loop needs them whole), and returns the same complete
// Response Complete would.
func (p *OpenAICompatibleProvider) CompleteStream(ctx context.Context, req Request, onText func(string), onReasoning func(string)) (Response, error) {
	// Local endpoints (Ollama etc.) do not require a credential.
	if !p.hasCredential() && !IsLocalBaseURL(p.BaseURL) {
		return Response{}, fmt.Errorf("missing credential")
	}
	if p.BaseURL == "" {
		return Response{}, fmt.Errorf("missing base url")
	}

	data, err := json.Marshal(chatCompletionRequest{
		SessionID: req.SessionID, TurnID: req.TurnID, RequestID: req.RequestID, ExecutionID: req.ExecutionID,
		Model: req.Model, Messages: newWireMessages(req.Messages), Temperature: req.Temperature,
		Tools: toolsForGatewayRequest(req.Messages, req.Tools), ToolChoice: req.ToolChoice,
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
		return Response{}, p.withCredentialContext(apiErrorFromBody(resp.StatusCode, raw))
	}

	var content strings.Builder
	var reasoningContent strings.Builder
	calls := map[int]*ToolCall{}
	var order []int
	var finishReason string
	var usage Usage
	sawDone := false

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // SSE lines can be large
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "[DONE]" {
			sawDone = true
			break
		}
		var chunk streamChunk
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue // tolerate keep-alives / partial lines
		}
		if chunk.Error != nil {
			message := chunk.Error.Message
			if message == "" {
				message = "model stream returned an error: " + payload
			}
			// An SSE response is already HTTP 200, so the original upstream
			// status is unavailable here. Treat a gateway-delivered stream error
			// as a bad gateway; this preserves APIError-based retry policy and,
			// most importantly, prevents an empty successful response.
			return Response{}, &APIError{
				StatusCode: http.StatusBadGateway,
				Type:       chunk.Error.Type,
				Code:       errorCode(chunk.Error.Code),
				Message:    message,
				Body:       payload,
			}
		}
		if u := chunk.Usage; u != nil {
			cached := u.PromptCacheHitTokens
			if cached == 0 {
				cached = u.PromptTokensDetails.CachedTokens
			}
			usage = Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens, CachedPromptTokens: cached, BillingUnits: u.BillingUnits}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		ch := chunk.Choices[0]
		if ch.FinishReason != "" {
			finishReason = ch.FinishReason
		}

		if ch.Delta.ReasoningContent != "" {
			reasoningContent.WriteString(ch.Delta.ReasoningContent)
			if onReasoning != nil {
				onReasoning(ch.Delta.ReasoningContent)
			}
		}
		// OpenRouter-style reasoning channels: the flat `reasoning` string and
		// the structured `reasoning_details` array carry the same data that
		// DeepSeek-style providers put in reasoning_content. Without them a
		// reasoning-heavy turn looks empty to us.
		if ch.Delta.Reasoning != "" {
			reasoningContent.WriteString(ch.Delta.Reasoning)
			if onReasoning != nil {
				onReasoning(ch.Delta.Reasoning)
			}
		}
		for _, d := range ch.Delta.ReasoningDetails {
			if text := d.reasoningText(); text != "" {
				reasoningContent.WriteString(text)
				if onReasoning != nil {
					onReasoning(text)
				}
			}
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
	if !sawDone {
		if err := ctx.Err(); err != nil {
			return Response{}, err
		}
		return Response{}, fmt.Errorf("model stream ended before [DONE]")
	}

	toolCalls := make([]ToolCall, 0, len(order))
	for _, idx := range order {
		toolCalls = append(toolCalls, *calls[idx])
	}
	return Response{
		Content:          strings.TrimSpace(content.String()),
		ReasoningContent: strings.TrimSpace(reasoningContent.String()),
		ToolCalls:        toolCalls,
		FinishReason:     finishReason,
		Usage:            usage,
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
		SessionID:   req.SessionID,
		TurnID:      req.TurnID,
		RequestID:   req.RequestID,
		ExecutionID: req.ExecutionID,
		Model:       req.Model,
		Messages:    newWireMessages(req.Messages),
		Temperature: req.Temperature,
		Tools:       toolsForGatewayRequest(req.Messages, req.Tools),
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
		return Response{}, p.withCredentialContext(apiErrorFromBody(resp.StatusCode, raw))
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
	// OpenRouter-style reasoning channels (see streamChunk.Delta): prefer the
	// DeepSeek-style flat string, then OpenRouter's flat alias, then its
	// structured details array.
	reasoning := choice.Message.ReasoningContent
	if reasoning == "" {
		reasoning = choice.Message.Reasoning
	}
	if reasoning == "" {
		var sb strings.Builder
		for _, d := range choice.Message.ReasoningDetails {
			sb.WriteString(d.reasoningText())
		}
		reasoning = sb.String()
	}
	return Response{
		Content:          strings.TrimSpace(choice.Message.Content),
		ReasoningContent: strings.TrimSpace(reasoning),
		ToolCalls:        choice.Message.ToolCalls,
		FinishReason:     choice.FinishReason,
		Usage: Usage{
			PromptTokens:       decoded.Usage.PromptTokens,
			CompletionTokens:   decoded.Usage.CompletionTokens,
			TotalTokens:        decoded.Usage.TotalTokens,
			BillingUnits:       decoded.Usage.BillingUnits,
			CachedPromptTokens: cached,
		},
		Raw: raw,
	}, nil
}

func toolsForGatewayRequest(messages []Message, tools []ToolDefinition) *[]ToolDefinition {
	if tools != nil {
		return &tools
	}
	for _, message := range messages {
		if message.Role == RoleUser && len(message.Assets) > 0 {
			empty := []ToolDefinition{}
			return &empty
		}
	}
	return nil
}

// apiErrorFromBody preserves OpenAI-compatible structured error fields for the
// retry policy. The raw body is retained for providers that use a different
// error shape or return non-JSON error pages.
func apiErrorFromBody(statusCode int, raw []byte) *APIError {
	apiErr := &APIError{StatusCode: statusCode, Body: string(raw)}
	var decoded struct {
		Error *openAIErrorPayload `json:"error"`
	}
	if json.Unmarshal(raw, &decoded) == nil && decoded.Error != nil {
		apiErr.Type = decoded.Error.Type
		apiErr.Code = errorCode(decoded.Error.Code)
		apiErr.Message = decoded.Error.Message
	}
	return apiErr
}

// withCredentialContext makes 401/403 actionable without exposing provider
// response bodies that may echo sensitive authentication material.
func (p *OpenAICompatibleProvider) withCredentialContext(err *APIError) *APIError {
	if err == nil || (err.StatusCode != http.StatusUnauthorized && err.StatusCode != http.StatusForbidden) {
		return err
	}
	if p.CredentialTarget.Namespace != "" || p.CredentialTarget.Name != "" {
		err.CredentialTarget = p.CredentialTarget.String()
	}
	err.Type = "authentication_error"
	err.Code = "auth_expired"
	err.Message = "provider authentication failed"
	err.Body = ""
	return err
}

func errorCode(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}
