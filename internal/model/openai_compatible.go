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
	// SHOULD have a hard ceiling — connect and TLS handshake — neither of
	// which scale with generation length.
	//
	// ResponseHeaderTimeout is deliberately omitted: with a large context
	// (hundreds of thousands of tokens), the upstream API — especially
	// through a relay/proxy — can take well over 60 s before the first
	// response header arrives (body upload + relay forwarding + prompt
	// processing). A hardcoded header timeout here would fire before the
	// ResilientProvider's context deadline and produce false-positive
	// timeouts that exhaust the retry budget. The context deadline
	// (request_timeout_seconds) already provides the correct overall bound,
	// as the Ollama provider does.
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSClientConfig:       &tls.Config{RootCAs: loadSystemRootCAs()},
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

type chatCompletionRequest struct {
	SessionID       string            `json:"session_id,omitempty"`
	TurnID          string            `json:"turn_id,omitempty"`
	RequestID       string            `json:"request_id,omitempty"`
	ExecutionID     string            `json:"execution_id,omitempty"`
	Model           string            `json:"model"`
	Messages        []wireMessage     `json:"messages"`
	Temperature     float64           `json:"temperature,omitempty"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"`
	Tools           *[]ToolDefinition `json:"tools,omitempty"`
	ToolChoice      string            `json:"tool_choice,omitempty"`
	Stream          bool              `json:"stream,omitempty"`
	StreamOptions   *streamOptions    `json:"stream_options,omitempty"`
}

// attachGatewayCorrelation adds the Gateway's correlation extension
// (session_id / turn_id / request_id / execution_id) to a chat-completions
// body. Those fields are how the Gateway resolves conversation asset references
// and traces a turn, and they are NOT part of the OpenAI schema: strict
// providers validate the request and reject unknown properties with a 400
// ("property 'execution_id' is unsupported" from Groq; "Extra inputs are not
// permitted" from OpenCode Go). They are therefore sent only to the Gateway,
// identified by its gateway-namespaced credential; OpenCode carries its session
// id in the x-opencode-session header instead.
func (p *OpenAICompatibleProvider) attachGatewayCorrelation(body *chatCompletionRequest, req Request) {
	if p.CredentialTarget.Namespace != "gateway" {
		return
	}
	body.SessionID = req.SessionID
	body.TurnID = req.TurnID
	body.RequestID = req.RequestID
	body.ExecutionID = req.ExecutionID
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
	// ReasoningContent is echoed back verbatim for DeepSeek-style thinking
	// models: their API contract requires the assistant's reasoning_content to
	// be passed back on the next request, or the call fails with a 400. A
	// pointer lets us distinguish "no reasoning" from "assistant tool-call
	// message that must still carry the field": some strict gateways (b.ai)
	// reject an assistant message with tool_calls when the reasoning_content
	// key is absent entirely, even if the model produced no reasoning that
	// turn. newWireMessages sets it to "" for those messages.
	ReasoningContent *string `json:"reasoning_content,omitempty"`
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
		// Echo reasoning verbatim when present. Strict thinking-mode gateways
		// also require the reasoning_content key on assistant tool-call
		// messages even when the model produced no reasoning — emit "" so the
		// field is present, not omitted (an absent key is a 400 there).
		if m.ReasoningContent != "" {
			rc := m.ReasoningContent
			w.ReasoningContent = &rc
		} else if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			empty := ""
			w.ReasoningContent = &empty
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

// pickReasoning resolves the three redundant reasoning channels an
// OpenAI-compatible provider may fill for one message/delta. Per OpenRouter's
// docs, `reasoning_content` is an exact alias of `reasoning`, and
// `reasoning_details` is the structured form of the same content — a provider
// (OpenRouter in particular) routinely sends the identical text on two or
// three of them. Accumulating every channel duplicates each delta once per
// channel (the "NowNowNowNow I I I I" TUI symptom); selecting one preserves
// providers that only ever fill a single channel (DeepSeek-style
// reasoning_content, OpenRouter-style reasoning/reasoning_details).
// Fallback order mirrors the non-streaming parse: flat strings first, then
// the structured array flattened.
func pickReasoning(reasoningContent, reasoning string, details []reasoningDetail) string {
	if reasoningContent != "" {
		return reasoningContent
	}
	if reasoning != "" {
		return reasoning
	}
	var sb strings.Builder
	for _, d := range details {
		sb.WriteString(d.reasoningText())
	}
	return sb.String()
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
			// with reasoning_content accepted as an input alias) and the
			// structured ReasoningDetails array. These are redundant views of
			// one delta — pickReasoning selects a single channel per chunk.
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

// reasoningEffortToOpenAI maps the generic effort onto the OpenAI-compatible
// reasoning_effort parameter. The reserved "off" becomes "none" (explicitly
// disable the chain-of-thought pass where the provider supports it); "" stays
// unset (provider default); any other level forwards verbatim.
func reasoningEffortToOpenAI(effort string) string {
	if effort == ReasoningEffortOff {
		return "none"
	}
	return effort
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

	body := chatCompletionRequest{
		Model:           req.Model,
		Messages:        newWireMessages(req.Messages),
		Temperature:     req.Temperature,
		ReasoningEffort: reasoningEffortToOpenAI(req.ReasoningEffort),
		Tools:           toolsForGatewayRequest(req.Messages, req.Tools),
		ToolChoice:      req.ToolChoice,
		Stream:          true,
		StreamOptions:   &streamOptions{IncludeUsage: true},
	}
	p.attachGatewayCorrelation(&body, req)
	data, err := json.Marshal(body)
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
	// https://opencode.ai/docs/go/#where-can-i-use-it
	// 为每段对话在 x-opencode-session 请求头中发送稳定的会话 ID，以便我们优化路由和提示词缓存。
	if strings.Contains(p.BaseURL, "opencode.ai") {
		httpReq.Header.Set("x-opencode-session", req.SessionID)
	}

	resp, err := p.HTTPClient.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return Response{}, p.withCredentialContext(apiErrorFromBody(resp.StatusCode, raw), bearerSecret(httpReq))
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
			// An SSE response is already HTTP 200, so the transport carries no
			// upstream status; apiErrorFromPayload recovers it from the payload's
			// `code` (falling back to 502). Keeping it APIError-based preserves the
			// retry policy and, most importantly, prevents an empty successful
			// response.
			apiErr := apiErrorFromPayload(http.StatusBadGateway, chunk.Error)
			apiErr.Message = message
			apiErr.Body = payload
			return Response{}, apiErr
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

		// The three reasoning channels are redundant representations of the
		// same delta (see pickReasoning); select one per chunk instead of
		// accumulating all of them, which duplicated every word per channel.
		if delta := pickReasoning(ch.Delta.ReasoningContent, ch.Delta.Reasoning, ch.Delta.ReasoningDetails); delta != "" {
			reasoningContent.WriteString(delta)
			if onReasoning != nil {
				onReasoning(delta)
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

	// The session/turn/request/execution correlation fields are a Gateway-only
	// body extension; strict OpenAI-compatible providers reject them.
	body := chatCompletionRequest{
		Model:           req.Model,
		Messages:        newWireMessages(req.Messages),
		Temperature:     req.Temperature,
		ReasoningEffort: reasoningEffortToOpenAI(req.ReasoningEffort),
		Tools:           toolsForGatewayRequest(req.Messages, req.Tools),
		ToolChoice:      req.ToolChoice,
	}
	p.attachGatewayCorrelation(&body, req)
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
	// https://opencode.ai/docs/go/#where-can-i-use-it
	// 为每段对话在 x-opencode-session 请求头中发送稳定的会话 ID，以便我们优化路由和提示词缓存。
	if strings.Contains(p.BaseURL, "opencode.ai") {
		httpReq.Header.Set("x-opencode-session", req.SessionID)
	}

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
		return Response{}, p.withCredentialContext(apiErrorFromBody(resp.StatusCode, raw), bearerSecret(httpReq))
	}

	// A 200 with an empty body is a truncated upstream response (seen from
	// OpenRouter when a provider dies after the headers are sent), not a
	// malformed payload. io.ErrUnexpectedEOF classifies it as transient so the
	// resilience layer retries, instead of surfacing "unexpected end of JSON
	// input; raw=" to the user.
	if len(raw) == 0 {
		return Response{}, fmt.Errorf("model api returned an empty response body: %w", io.ErrUnexpectedEOF)
	}

	var decoded chatCompletionResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Response{}, fmt.Errorf("decode response: %w; raw=%s", err, string(raw))
	}

	// A 200 whose body carries an `error` object and no choices is a gateway
	// reporting an upstream provider failure after the transport already
	// succeeded (OpenRouter: {"error":{"code":503,"message":"Upstream error from
	// Nvidia: Service temporarily overloaded"}}). Classify it as an APIError so
	// the resilience layer's status-based retry policy applies — the old
	// "no choices" decode error was non-retryable, so a transient upstream
	// overload ended the turn and forced the user to resend.
	if decoded.Error != nil {
		return Response{}, apiErrorFromPayload(http.StatusBadGateway, decoded.Error)
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
	// The three reasoning channels are redundant views of the same content
	// (see pickReasoning); select one rather than accumulating duplicates.
	reasoning := pickReasoning(choice.Message.ReasoningContent, choice.Message.Reasoning, choice.Message.ReasoningDetails)
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

// withCredentialContext annotates a 401/403 with the credential target and
// makes it safe to surface: the raw body (which may echo the Authorization
// header) is always dropped. Genuine authentication failures get the actionable
// "provider authentication failed" wording; a structural 403 (region lock,
// entitlement) keeps the provider's own type/message so the real reason reaches
// the user instead of being mislabeled as an auth failure.
func (p *OpenAICompatibleProvider) withCredentialContext(err *APIError, secret string) *APIError {
	if err == nil || (err.StatusCode != http.StatusUnauthorized && err.StatusCode != http.StatusForbidden) {
		return err
	}
	if p.CredentialTarget.Namespace != "" || p.CredentialTarget.Name != "" {
		err.CredentialTarget = p.CredentialTarget.String()
	}
	err.Body = ""
	if IsAuthFailure(err) {
		err.Type = "authentication_error"
		err.Code = "auth_expired"
		err.Message = "provider authentication failed"
		return err
	}
	err.Message = redactSecret(err.Message, secret)
	return err
}

// bearerSecret extracts the resolved credential from the request's
// Authorization header, or "" when no bearer credential was applied. Used to
// scrub an upstream error message that may have echoed the secret.
func bearerSecret(req *http.Request) string {
	return strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
}

// apiErrorFromPayload builds an APIError from an embedded provider error
// payload. When the payload carries an HTTP status in its `code` field (some
// gateways forward the upstream status this way — OpenRouter sends the Nvidia
// 503 inline in an HTTP-200 body), that status overrides defaultStatus so the
// resilience layer retries transient upstream failures instead of treating them
// as permanent decode errors.
func apiErrorFromPayload(defaultStatus int, payload *openAIErrorPayload) *APIError {
	if payload == nil {
		return &APIError{StatusCode: defaultStatus}
	}
	status := defaultStatus
	if code, ok := payload.Code.(float64); ok && code >= 400 && code < 600 {
		status = int(code)
	}
	return &APIError{
		StatusCode: status,
		Type:       payload.Type,
		Code:       errorCode(payload.Code),
		Message:    payload.Message,
	}
}

func errorCode(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}
