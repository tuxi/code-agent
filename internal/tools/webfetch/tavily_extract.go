package webfetch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const tavilyExtractAPIBase = "https://api.tavily.com/extract"

// tavilyExtractor fetches a URL's content via Tavily's /extract endpoint, which
// runs on Tavily's own servers. web_fetch uses it as a fallback when a direct
// fetch can't reach the host (Tavily reaches sites this network can't, e.g.
// behind the GFW). Its HTTP client deliberately has no SSRF dial guard: it only
// ever contacts the fixed, public api.tavily.com — the arbitrary URL travels in
// the request body and is dialed by Tavily, not by us.
type tavilyExtractor struct {
	apiKey  string
	baseURL string // endpoint override; defaults to tavilyExtractAPIBase (tests)
	client  *http.Client
}

func newTavilyExtractor(apiKey string, timeoutSeconds int) *tavilyExtractor {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 30
	}
	return &tavilyExtractor{
		apiKey: apiKey,
		client: &http.Client{Timeout: time.Duration(timeoutSeconds) * time.Second},
	}
}

type tavilyExtractRequest struct {
	APIKey       string   `json:"api_key"`
	URLs         []string `json:"urls"`
	ExtractDepth string   `json:"extract_depth"`
	Format       string   `json:"format"`
}

type tavilyExtractResult struct {
	URL        string `json:"url"`
	RawContent string `json:"raw_content"`
}

type tavilyExtractResponse struct {
	Results []tavilyExtractResult `json:"results"`
}

// Extract returns the cleaned content of urlStr as fetched by Tavily.
func (e *tavilyExtractor) Extract(ctx context.Context, urlStr string) (string, error) {
	body, err := json.Marshal(tavilyExtractRequest{
		APIKey:       e.apiKey,
		URLs:         []string{urlStr},
		ExtractDepth: "basic",
		Format:       "markdown",
	})
	if err != nil {
		return "", err
	}

	endpoint := e.baseURL
	if endpoint == "" {
		endpoint = tavilyExtractAPIBase
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("tavily extract: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tavily extract: HTTP %d", resp.StatusCode)
	}

	var er tavilyExtractResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return "", fmt.Errorf("tavily extract: invalid response: %w", err)
	}
	if len(er.Results) == 0 || er.Results[0].RawContent == "" {
		return "", fmt.Errorf("tavily extract: no content for %s", urlStr)
	}
	return er.Results[0].RawContent, nil
}
