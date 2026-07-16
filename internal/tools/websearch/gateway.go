package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"code-agent/internal/credential"
	"code-agent/internal/tools"
)

const gatewaySearchPath = "/tools/web-search"

// GatewaySearchProvider calls Agent Gateway's managed, billable Web Search.
// It resolves the JWT for every request so per-session credentials and host
// Reconfigure updates are observed without storing a token in the tool.
type GatewaySearchProvider struct {
	Credential credential.Resolver
	Target     credential.Target
	BaseURL    string
	Client     *http.Client
}

func NewGatewaySearchProvider(resolver credential.Resolver, target credential.Target, baseURL string, timeoutSeconds int) *GatewaySearchProvider {
	if target.Namespace == "" && target.Name == "" {
		target = credential.Target{Namespace: "gateway", Name: "default"}
	}
	if timeoutSeconds <= 0 {
		timeoutSeconds = 120
	}
	return &GatewaySearchProvider{
		Credential: resolver,
		Target:     target,
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Client:     &http.Client{Timeout: time.Duration(timeoutSeconds) * time.Second},
	}
}

func (g *GatewaySearchProvider) Name() string { return "gateway" }

func (g *GatewaySearchProvider) Search(ctx context.Context, searchReq SearchRequest) (SearchResponse, error) {
	if g.BaseURL == "" {
		return SearchResponse{}, errors.New("gateway web search base URL is empty")
	}
	if searchReq.CallID == "" {
		return SearchResponse{}, errors.New("gateway web search requires a tool call_id")
	}

	body, err := json.Marshal(gatewayRequest{
		CallID:      searchReq.CallID,
		SessionID:   searchReq.SessionID,
		ExecutionID: searchReq.ExecutionID,
		TurnID:      searchReq.TurnID,
		Query:       searchReq.Query,
		MaxResults:  searchReq.TopK,
		SearchDepth: "basic",
		Topic:       "general",
	})
	if err != nil {
		return SearchResponse{}, fmt.Errorf("gateway web search: marshal request: %w", err)
	}

	// A 409 can mean the identical call is still being settled, while 502 is a
	// provider failure whose reservation was released. Retrying the exact bytes
	// preserves Gateway's (user_id, call_id) idempotency guarantee.
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if err := waitRetry(ctx, time.Duration(attempt)*200*time.Millisecond); err != nil {
				return SearchResponse{}, err
			}
		}
		resp, err := g.doSearch(ctx, body)
		if err == nil {
			if resp.Usage == nil {
				return SearchResponse{}, &GatewayProtocolError{Message: "gateway web search returned no usage receipt"}
			}
			if resp.Usage.ToolCallID != searchReq.CallID {
				return SearchResponse{}, &GatewayProtocolError{Message: fmt.Sprintf("gateway web search usage call_id mismatch: got %q want %q", resp.Usage.ToolCallID, searchReq.CallID)}
			}
			if resp.Usage.ToolName != "web_search" {
				return SearchResponse{}, &GatewayProtocolError{Message: fmt.Sprintf("gateway web search usage tool_name mismatch: got %q", resp.Usage.ToolName)}
			}
			if resp.Usage.Provider == "" || resp.Usage.Operation == "" ||
				resp.Usage.ProviderCredits <= 0 || resp.Usage.BillingUnits <= 0 ||
				resp.Usage.FundingSource == "" || resp.Usage.ReservationID == "" ||
				resp.Usage.PricingVersion <= 0 {
				return SearchResponse{}, &GatewayProtocolError{Message: "gateway web search returned incomplete usage receipt"}
			}
			resp.Usage.Replayed = resp.Replayed
			return resp, nil
		}
		lastErr = err
		var httpErr *GatewayHTTPError
		if !errors.As(err, &httpErr) || !retryableGatewayHTTPError(httpErr) {
			break
		}
	}
	return SearchResponse{}, lastErr
}

func retryableGatewayHTTPError(err *GatewayHTTPError) bool {
	if err.StatusCode == http.StatusBadGateway {
		return true
	}
	if err.StatusCode != http.StatusConflict {
		return false
	}
	// Gateway currently uses one business code for both "still processing" and
	// "same call_id, different request". Its stable message distinguishes the
	// permanent conflict; all other 409s get the documented short polling retry.
	return !strings.Contains(strings.ToLower(err.Message), "different request")
}

func (g *GatewaySearchProvider) doSearch(ctx context.Context, body []byte) (SearchResponse, error) {
	if g.Credential == nil {
		return SearchResponse{}, errors.New("gateway web search credential resolver is unavailable")
	}
	resolved, err := g.Credential.Resolve(ctx, g.Target)
	if err != nil {
		return SearchResponse{}, fmt.Errorf("resolve gateway web search credential for %s: %w", g.Target.String(), err)
	}
	if resolved.IsZero() || resolved.Type != credential.Bearer || resolved.Secret == "" {
		return SearchResponse{}, fmt.Errorf("gateway bearer credential %s is unavailable", g.Target.String())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.BaseURL+gatewaySearchPath, bytes.NewReader(body))
	if err != nil {
		return SearchResponse{}, fmt.Errorf("gateway web search: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+resolved.Secret)

	resp, err := g.Client.Do(req)
	if err != nil {
		return SearchResponse{}, fmt.Errorf("gateway web search: request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return SearchResponse{}, fmt.Errorf("gateway web search: read response: %w", err)
	}
	var envelope gatewayAPIResponse
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &envelope)
	}
	if resp.StatusCode != http.StatusOK {
		return SearchResponse{}, &GatewayHTTPError{
			StatusCode: resp.StatusCode,
			Code:       envelope.Code,
			Message:    envelope.Message,
			Body:       string(raw),
		}
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return SearchResponse{}, fmt.Errorf("gateway web search: decode response: %w", err)
	}
	if envelope.Code != 0 {
		return SearchResponse{}, &GatewayHTTPError{
			StatusCode: resp.StatusCode,
			Code:       envelope.Code,
			Message:    envelope.Message,
			Body:       string(raw),
		}
	}

	var data gatewayWebSearchResponse
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return SearchResponse{}, errors.New("gateway web search returned empty data")
	}
	if err := json.Unmarshal(envelope.Data, &data); err != nil {
		return SearchResponse{}, fmt.Errorf("gateway web search: decode data: %w", err)
	}

	results := make([]Result, 0, len(data.Results))
	for _, item := range data.Results {
		if item.URL == "" {
			continue
		}
		results = append(results, Result{
			Title:   item.Title,
			URL:     item.URL,
			Snippet: item.Content,
			Source:  "gateway",
		})
	}
	return SearchResponse{Results: results, Usage: data.Usage, Replayed: data.Replayed}, nil
}

func waitRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type gatewayRequest struct {
	CallID      string `json:"call_id"`
	SessionID   string `json:"session_id,omitempty"`
	ExecutionID string `json:"execution_id,omitempty"`
	TurnID      string `json:"turn_id,omitempty"`
	Query       string `json:"query"`
	SearchDepth string `json:"search_depth"`
	Topic       string `json:"topic"`
	MaxResults  int    `json:"max_results"`
}

type gatewayAPIResponse struct {
	TraceID string          `json:"trace_id"`
	Code    int             `json:"code"`
	Message string          `json:"msg"`
	Data    json.RawMessage `json:"data"`
}

type gatewayWebSearchResponse struct {
	Query    string            `json:"query"`
	Results  []webSearchResult `json:"results"`
	Usage    *tools.ToolUsage  `json:"usage"`
	Replayed bool              `json:"replayed"`
}

type webSearchResult struct {
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	Content string  `json:"content"`
	Score   float64 `json:"score,omitempty"`
}

// GatewayHTTPError preserves HTTP/business status for lifecycle handling.
type GatewayHTTPError struct {
	StatusCode int
	Code       int
	Message    string
	Body       string
}

// GatewayProtocolError means Gateway reported success after charging the call
// but omitted or corrupted the authoritative usage receipt. Stop the turn so a
// model cannot hide the accounting fault by issuing a new, separately billable
// call_id.
type GatewayProtocolError struct {
	Message string
}

func (e *GatewayProtocolError) Error() string              { return e.Message }
func (e *GatewayProtocolError) LifecycleErrorCode() string { return "request_failed" }
func (e *GatewayProtocolError) TurnFatalToolError() bool   { return true }

func (e *GatewayHTTPError) Error() string {
	message := strings.TrimSpace(e.Message)
	if message == "" {
		message = strings.TrimSpace(e.Body)
	}
	if len(message) > 500 {
		message = message[:500] + "…"
	}
	return fmt.Sprintf("gateway web search error: status=%d code=%d message=%s", e.StatusCode, e.Code, message)
}

func (e *GatewayHTTPError) LifecycleErrorCode() string {
	switch e.StatusCode {
	case http.StatusUnauthorized:
		return "auth_expired"
	case http.StatusTooManyRequests:
		return "quota_exceeded"
	default:
		return "request_failed"
	}
}

func (e *GatewayHTTPError) TurnFatalToolError() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusTooManyRequests
}

var _ SearchProvider = (*GatewaySearchProvider)(nil)
var _ tools.TurnFatalToolError = (*GatewayHTTPError)(nil)
var _ tools.TurnFatalToolError = (*GatewayProtocolError)(nil)
