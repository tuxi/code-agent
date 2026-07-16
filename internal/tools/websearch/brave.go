package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

const braveAPIBase = "https://api.search.brave.com/res/v1/web/search"

// BraveProvider queries the Brave Search API.
// Requires an API key (free tier available at https://brave.com/search/api/).
type BraveProvider struct {
	APIKey string
	Client *http.Client
}

// NewBrave returns a BraveProvider. timeoutSeconds sets the HTTP client timeout;
// apiKey is the Brave Search API key.
func NewBrave(apiKey string, timeoutSeconds int) *BraveProvider {
	return &BraveProvider{
		APIKey: apiKey,
		Client: &http.Client{
			Timeout: time.Duration(timeoutSeconds) * time.Second,
		},
	}
}

func (p *BraveProvider) Name() string { return "brave" }

type braveResponse struct {
	Web *braveWeb `json:"web"`
}

type braveWeb struct {
	Results []braveResult `json:"results"`
}

type braveResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
}

func (p *BraveProvider) Search(ctx context.Context, searchReq SearchRequest) (SearchResponse, error) {
	params := url.Values{}
	params.Set("q", searchReq.Query)
	params.Set("count", fmt.Sprintf("%d", searchReq.TopK))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		braveAPIBase+"?"+params.Encode(), nil)
	if err != nil {
		return SearchResponse{}, fmt.Errorf("brave: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Subscription-Token", p.APIKey)

	resp, err := p.Client.Do(req)
	if err != nil {
		return SearchResponse{}, fmt.Errorf("brave: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return SearchResponse{}, fmt.Errorf("brave: HTTP %d", resp.StatusCode)
	}

	var br braveResponse
	if err := json.NewDecoder(resp.Body).Decode(&br); err != nil {
		return SearchResponse{}, fmt.Errorf("brave: invalid response: %w", err)
	}

	results := make([]Result, 0, len(br.Web.Results))
	for _, r := range br.Web.Results {
		if r.URL == "" {
			continue
		}
		results = append(results, Result{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: r.Description,
			Source:  "web",
		})
	}
	return SearchResponse{Results: results}, nil
}
