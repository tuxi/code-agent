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

	"code-agent/internal/credential"
)

// ResponsesProvider speaks OpenAI's Responses API (/v1/responses) protocol.
// It implements the same Provider and StreamingProvider interfaces as
// OpenAICompatibleProvider and OllamaProvider, so the rest of the stack
// (ResilientProvider, agent loop, cost telemetry) works unchanged.
//
// Compared to routing through the OpenAI-compatible /v1/chat/completions
// endpoint, the Responses protocol is the newer OpenAI wire format (items +
// function_call/function_call_output instead of roles + tool_calls). It is
// natively supported by OpenAI and, since 2026-07, by DeepSeek on the same
// host as their chat endpoint, so switching a model to provider:"responses"
// is a config-only change.
type ResponsesProvider struct {
	BaseURL    string
	HTTPClient *http.Client

	// Credential, when non-nil, resolves the credential dynamically on each
	// request. When nil, the provider falls back to the static APIKey field
	// (backward compatible path). Same shape as OpenAICompatibleProvider.
	Credential       credential.Resolver
	CredentialTarget credential.Target

	// APIKey is the static API key, used when Credential is nil.
	APIKey string
}

// NewResponsesProvider creates a provider that resolves credentials
// dynamically via cred. The target identifies which service this provider calls.
func NewResponsesProvider(baseURL string, cred credential.Resolver, target credential.Target) *ResponsesProvider {
	return &ResponsesProvider{
		BaseURL:          strings.TrimRight(baseURL, "/"),
		Credential:       cred,
		CredentialTarget: target,
		HTTPClient:       defaultHTTPClient(),
	}
}

// NewResponsesProviderWithKey creates a provider with a static API key,
// wiring it through the same credential path as the openai adapter.
func NewResponsesProviderWithKey(baseURL, apiKey string) *ResponsesProvider {
	p := &ResponsesProvider{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		APIKey:     apiKey,
		HTTPClient: defaultHTTPClient(),
	}
	if apiKey != "" {
		p.Credential = credential.StaticResolver{
			{Namespace: "llm", Name: "default"}: {Type: credential.Bearer, Secret: apiKey},
		}
		p.CredentialTarget = credential.Target{Namespace: "llm", Name: "default"}
	}
	return p
}

// ── Requests API wire schemas ───────────────────────────────────────────

// responsesRequest is the body for POST {base}/responses. Unsupported top-level
// parameters are silently ignored by compliant providers (OpenAI and DeepSeek
// both), so this universal subset works against either.
type responsesRequest struct {
	Model        string           `json:"model"`
	Instructions string           `json:"instructions,omitempty"`
	Input        []responseItem   `json:"input"`
	Temperature  float64          `json:"temperature,omitempty"`
	Tools        *[]responsesTool `json:"tools,omitempty"`
	ToolChoice   string           `json:"tool_choice,omitempty"`
	Stream       bool             `json:"stream,omitempty"`
}

// responsesTool is the FLAT function-tool shape the Responses API expects.
// Unlike Chat Completions ({"type":"function","function":{...}}), Responses
// puts name/description/parameters at the top level of the tool object —
// DeepSeek's /responses endpoint rejects the nested form with
// "tools[0]: missing field `name`".
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// toResponsesTools flattens the internal ToolDefinition (Chat Completions
// shape) into the Responses tool shape. Nil tools stay nil so the field is
// omitted from the body.
func toResponsesTools(tools []ToolDefinition) *[]responsesTool {
	if tools == nil {
		return nil
	}
	out := make([]responsesTool, len(tools))
	for i, t := range tools {
		toolType := t.Type
		if toolType == "" {
			toolType = "function"
		}
		out[i] = responsesTool{
			Type:        toolType,
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		}
	}
	return &out
}

// responseItem is one input item: a message, a function call to replay, or a
// function call output. Only one of the field groups is set per item.
type responseItem struct {
	Type      string            `json:"type"`
	Role      string            `json:"role,omitempty"`      // message items
	Content   []responseContent `json:"content,omitempty"`   // message items
	CallID    string            `json:"call_id,omitempty"`   // function_call / function_call_output
	Name      string            `json:"name,omitempty"`      // function_call
	Arguments string            `json:"arguments,omitempty"` // function_call (JSON string)
	Output    string            `json:"output,omitempty"`    // function_call_output
}

// responseContent is one content block inside a message item. input_text marks
// user-side text; output_text marks assistant-side text (the OpenAI convention
// for replaying prior assistant messages).
type responseContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// toResponsesRequest maps the canonical Request into the Responses body:
// system messages fold into the top-level instructions field (there is no
// system role on the wire), everything else becomes input items.
func toResponsesRequest(req Request) responsesRequest {
	var instructions []string
	var items []responseItem
	for _, m := range req.Messages {
		switch m.Role {
		case RoleSystem:
			if m.Content != "" {
				instructions = append(instructions, m.Content)
			}
		case RoleUser:
			items = append(items, responseItem{
				Type:    "message",
				Role:    "user",
				Content: []responseContent{{Type: "input_text", Text: m.Content}},
			})
		case RoleAssistant:
			if m.Content != "" {
				items = append(items, responseItem{
					Type:    "message",
					Role:    "assistant",
					Content: []responseContent{{Type: "output_text", Text: m.Content}},
				})
			}
			for _, tc := range m.ToolCalls {
				items = append(items, responseItem{
					Type:      "function_call",
					CallID:    tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}
		case RoleTool:
			items = append(items, responseItem{
				Type:   "function_call_output",
				CallID: m.ToolCallID,
				Output: m.Content,
			})
		}
	}
	return responsesRequest{
		Model:        req.Model,
		Instructions: strings.Join(instructions, "\n\n"),
		Input:        items,
		Temperature:  req.Temperature,
		Tools:        toResponsesTools(req.Tools),
		ToolChoice:   req.ToolChoice,
	}
}

// ── Responses wire schemas ──────────────────────────────────────────────

// responsesResponse is the response object. In non-streaming mode it is the
// whole HTTP body; in streaming mode the terminal event (response.completed /
// response.incomplete / response.failed) embeds one as its "response" field.
type responsesResponse struct {
	ID     string              `json:"id"`
	Status string              `json:"status"` // completed | incomplete | failed | in_progress
	Output []responsesOutput   `json:"output"`
	Usage  *responsesUsage     `json:"usage"`
	Error  *openAIErrorPayload `json:"error,omitempty"`
}

// responsesOutput is one output item. message items carry output_text content
// blocks; reasoning items carry reasoning_text blocks; function_call items
// carry the requested tool.
type responsesOutput struct {
	Type      string            `json:"type"` // message | reasoning | function_call | ...
	ID        string            `json:"id"`
	CallID    string            `json:"call_id,omitempty"`
	Name      string            `json:"name,omitempty"`
	Arguments string            `json:"arguments,omitempty"`
	Content   []responseContent `json:"content,omitempty"`
}

// responsesUsage reports token accounting. Cached-prompt tokens sit under
// input_tokens_details; reasoning tokens under output_tokens_details.
type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokens        int `json:"output_tokens"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	TotalTokens int `json:"total_tokens"`
}

func (u *responsesUsage) toUsage() Usage {
	if u == nil {
		return Usage{}
	}
	return Usage{
		PromptTokens:       u.InputTokens,
		CompletionTokens:   u.OutputTokens,
		TotalTokens:        u.TotalTokens,
		CachedPromptTokens: u.InputTokensDetails.CachedTokens,
	}
}

// responsesFromOutput converts a Responses response object into the canonical
// Response the agent loop consumes. Reasoning text is provider-visible and
// flows into ReasoningContent; function_call items become ToolCalls.
func responsesFromOutput(r responsesResponse) Response {
	var content, reasoning strings.Builder
	var calls []ToolCall
	for _, out := range r.Output {
		switch out.Type {
		case "message":
			for _, c := range out.Content {
				if c.Type == "output_text" {
					content.WriteString(c.Text)
				}
			}
		case "reasoning":
			for _, c := range out.Content {
				if c.Type == "reasoning_text" {
					reasoning.WriteString(c.Text)
				}
			}
		case "function_call":
			// The replay-binding identity is call_id when the provider emits it;
			// fall back to the item id. Both empty is repaired by the loop.
			id := out.CallID
			if id == "" {
				id = out.ID
			}
			calls = append(calls, ToolCall{
				ID:   id,
				Type: "function",
				Function: FunctionCall{
					Name:      out.Name,
					Arguments: out.Arguments,
				},
			})
		}
	}
	finish := "stop"
	switch {
	case len(calls) > 0:
		finish = "tool_calls"
	case r.Status == "incomplete":
		finish = "length"
	}
	return Response{
		Content:          strings.TrimSpace(content.String()),
		ReasoningContent: strings.TrimSpace(reasoning.String()),
		ToolCalls:        calls,
		FinishReason:     finish,
		Usage:            r.Usage.toUsage(),
	}
}

// ── Provider interface ──────────────────────────────────────────────────

// Complete sends a non-streaming request to {base}/responses and returns the
// assembled Response.
func (p *ResponsesProvider) Complete(ctx context.Context, req Request) (Response, error) {
	if !p.hasCredential() && !IsLocalBaseURL(p.BaseURL) {
		return Response{}, fmt.Errorf("missing credential")
	}
	if p.BaseURL == "" {
		return Response{}, fmt.Errorf("missing base url")
	}

	data, err := json.Marshal(toResponsesRequest(req))
	if err != nil {
		return Response{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/responses", bytes.NewReader(data))
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
	// "decode response" failure.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Response{}, p.withCredentialContext(apiErrorFromBody(resp.StatusCode, raw))
	}

	var decoded responsesResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Response{}, fmt.Errorf("decode response: %w; raw=%s", err, string(raw))
	}

	// A status of "failed" carries a structured error in the response object
	// even though the HTTP exchange succeeded. Surface it as a 502 so the
	// resilience layer can retry, mirroring the SSE-error path in streaming.
	if decoded.Status == "failed" || decoded.Error != nil {
		apiErr := &APIError{StatusCode: http.StatusBadGateway, Body: string(raw)}
		if decoded.Error != nil {
			apiErr.Type = decoded.Error.Type
			apiErr.Code = errorCode(decoded.Error.Code)
			apiErr.Message = decoded.Error.Message
		}
		return Response{}, apiErr
	}

	return responsesFromOutput(decoded), nil
}

// ── StreamingProvider interface ─────────────────────────────────────────

// responsesStreamEvent is one SSE event in a Responses stream. Terminal events
// embed the full response object; delta events carry incremental text.
type responsesStreamEvent struct {
	Type     string             `json:"type"`
	Delta    string             `json:"delta"`
	Response *responsesResponse `json:"response"`
}

// CompleteStream streams text/reasoning deltas via the callbacks and returns
// the SAME complete Response Complete would. Unlike the OpenAI-compatible SSE
// format there is no "data: [DONE]" marker: the stream ends with a terminal
// event (response.completed / response.incomplete / response.failed) whose
// embedded response object is authoritative for usage, tool calls, and text.
func (p *ResponsesProvider) CompleteStream(ctx context.Context, req Request, onText, onReasoning func(string)) (Response, error) {
	if !p.hasCredential() && !IsLocalBaseURL(p.BaseURL) {
		return Response{}, fmt.Errorf("missing credential")
	}
	if p.BaseURL == "" {
		return Response{}, fmt.Errorf("missing base url")
	}

	rreq := toResponsesRequest(req)
	rreq.Stream = true
	data, err := json.Marshal(rreq)
	if err != nil {
		return Response{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/responses", bytes.NewReader(data))
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

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // SSE lines can be large
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		// Tolerate a proxied "data: [DONE]" line (some gateways add it even
		// though the Responses protocol does not use it).
		if payload == "[DONE]" {
			continue
		}
		var ev responsesStreamEvent
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue // tolerate keep-alives / partial lines
		}
		switch ev.Type {
		case "response.output_text.delta":
			if onText != nil && ev.Delta != "" {
				onText(ev.Delta)
			}
		case "response.reasoning_text.delta":
			if onReasoning != nil && ev.Delta != "" {
				onReasoning(ev.Delta)
			}
		case "response.completed", "response.incomplete":
			// Terminal success event: the embedded response object is the
			// authoritative snapshot — rebuild the Response from it so a lost
			// delta never corrupts tool calls or text.
			if ev.Response == nil {
				return Response{}, fmt.Errorf("responses stream: %s event without a response object", ev.Type)
			}
			return responsesFromOutput(*ev.Response), nil
		case "response.failed":
			// A failed stream is already HTTP 200, so the original upstream
			// status is unavailable here. Treat it as a bad gateway; this
			// preserves APIError-based retry policy and, most importantly,
			// prevents an empty successful response.
			apiErr := &APIError{StatusCode: http.StatusBadGateway, Body: payload}
			if ev.Response != nil && ev.Response.Error != nil {
				apiErr.Type = ev.Response.Error.Type
				apiErr.Code = errorCode(ev.Response.Error.Code)
				apiErr.Message = ev.Response.Error.Message
			}
			if apiErr.Message == "" {
				apiErr.Message = "model stream returned an error: " + payload
			}
			return Response{}, apiErr
		}
	}
	if err := sc.Err(); err != nil {
		return Response{}, err
	}
	if ctx.Err() != nil {
		return Response{}, ctx.Err()
	}
	return Response{}, fmt.Errorf("responses stream ended before a terminal event (response.completed/incomplete/failed)")
}

// ── Credential helpers (mirror of OpenAICompatibleProvider) ─────────────

// hasCredential reports whether this provider has any means of authentication.
func (p *ResponsesProvider) hasCredential() bool {
	return p.Credential != nil || p.APIKey != ""
}

// applyAuth resolves the credential for this provider and sets the
// Authorization header on req.
func (p *ResponsesProvider) applyAuth(ctx context.Context, req *http.Request) error {
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
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	return nil
}

// withCredentialContext makes 401/403 actionable without exposing provider
// response bodies that may echo sensitive authentication material.
func (p *ResponsesProvider) withCredentialContext(err *APIError) *APIError {
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
