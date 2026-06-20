package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const tavilyAPIBase = "https://api.tavily.com/search"

// TavilyProvider queries the Tavily Search API — a search backend built for LLM
// agents. Results come back already cleaned for model consumption, and Tavily
// fetches result content on its own servers (relevant when the agent's own
// network can't reach a result URL directly). Requires an API key (free tier at
// https://tavily.com).
type TavilyProvider struct {
	APIKey  string
	BaseURL string // endpoint override; defaults to tavilyAPIBase when empty (tests)
	Client  *http.Client
}

// NewTavily returns a TavilyProvider. timeoutSeconds sets the HTTP client
// timeout; apiKey is the Tavily API key (tvly-...).
func NewTavily(apiKey string, timeoutSeconds int) *TavilyProvider {
	return &TavilyProvider{
		APIKey: apiKey,
		Client: &http.Client{
			Timeout: time.Duration(timeoutSeconds) * time.Second,
		},
	}
}

func (p *TavilyProvider) Name() string { return "tavily" }

// tavilyRequest is the POST body for Tavily's /search endpoint. The API key
// travels in the body (the classic, broadly-compatible form); newer Tavily also
// accepts an Authorization: Bearer header.
type tavilyRequest struct {
	APIKey      string `json:"api_key"`
	Query       string `json:"query"`
	MaxResults  int    `json:"max_results"`
	SearchDepth string `json:"search_depth"`
	Topic       string `json:"topic"`
}

type tavilyResponse struct {
	Query   string         `json:"query"`
	Answer  string         `json:"answer"`
	Results []tavilyResult `json:"results"`
}

type tavilyResult struct {
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	Content string  `json:"content"`
	Score   float64 `json:"score"`
}

func (p *TavilyProvider) Search(ctx context.Context, query string, topK int) ([]Result, error) {
	reqBody, err := json.Marshal(tavilyRequest{
		APIKey:      p.APIKey,
		Query:       query,
		MaxResults:  topK,
		SearchDepth: "basic",
		Topic:       "general",
	})
	if err != nil {
		return nil, fmt.Errorf("tavily: marshal request: %w", err)
	}

	endpoint := p.BaseURL
	if endpoint == "" {
		endpoint = tavilyAPIBase
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("tavily: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tavily: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tavily: HTTP %d", resp.StatusCode)
	}

	var tr tavilyResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, fmt.Errorf("tavily: invalid response: %w", err)
	}

	results := make([]Result, 0, len(tr.Results))
	for _, r := range tr.Results {
		if r.URL == "" {
			continue
		}
		results = append(results, Result{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: r.Content,
			Source:  "tavily",
		})
		if len(results) >= topK {
			break
		}
	}
	return results, nil
}
