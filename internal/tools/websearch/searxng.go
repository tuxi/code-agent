package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Default public SearXNG instances. The provider tries them in order until one
// succeeds. Add or override via config: web.search.searxng_instances.
var defaultSearXNGInstances = []string{
	"https://searx.be",
	"https://search.sapti.me",
	"https://searx.tiekoetter.com",
	"https://searx.si",
	"https://searx.baczek.me",
}

// SearXNGProvider queries a pool of SearXNG instances via the JSON API. It tries
// each instance in order; the first to respond successfully wins. SearXNG is a
// free, self-hostable meta-search engine — no API key required.
type SearXNGProvider struct {
	Instances []string
	Client    *http.Client
}

// NewSearXNG returns a SearXNGProvider. instances is a list of base URLs; the
// first is tried first, with others as fallback. timeoutSeconds sets the
// per-instance HTTP client timeout.
func NewSearXNG(instances []string, timeoutSeconds int) *SearXNGProvider {
	if len(instances) == 0 {
		instances = defaultSearXNGInstances
	}
	// Deduplicate and clean.
	seen := make(map[string]bool)
	clean := make([]string, 0, len(instances))
	for _, u := range instances {
		u = strings.TrimRight(strings.TrimSpace(u), "/")
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		clean = append(clean, u)
	}
	return &SearXNGProvider{
		Instances: clean,
		Client: &http.Client{
			Timeout: time.Duration(timeoutSeconds) * time.Second,
		},
	}
}

func (p *SearXNGProvider) Name() string { return "searxng" }

// searxngResponse is the JSON shape returned by SearXNG's search API.
type searxngResponse struct {
	Results []searxngResult `json:"results"`
}

type searxngResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
	Engine  string `json:"engine"`
}

func (p *SearXNGProvider) Search(ctx context.Context, searchReq SearchRequest) (SearchResponse, error) {
	var lastErr error
	for _, baseURL := range p.Instances {
		results, err := p.tryInstance(ctx, baseURL, searchReq.Query, searchReq.TopK)
		if err == nil {
			return SearchResponse{Results: results}, nil
		}
		lastErr = err
		// If context is done, stop retrying.
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr != nil {
		return SearchResponse{}, fmt.Errorf("searxng: all %d instances failed (last: %w)", len(p.Instances), lastErr)
	}
	return SearchResponse{}, fmt.Errorf("searxng: no instances configured")
}

func (p *SearXNGProvider) tryInstance(ctx context.Context, baseURL, query string, topK int) ([]Result, error) {
	params := url.Values{}
	params.Set("q", query)
	params.Set("format", "json")
	params.Set("categories", "general")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		baseURL+"/search?"+params.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", baseURL, err)
	}
	req.Header.Set("User-Agent", "CodeAgent/1.0")
	req.Header.Set("Accept", "application/json")

	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: request failed: %w", baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", baseURL, resp.StatusCode)
	}

	var sr searxngResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, fmt.Errorf("%s: invalid response: %w", baseURL, err)
	}

	results := make([]Result, 0, len(sr.Results))
	for _, r := range sr.Results {
		if r.URL == "" {
			continue
		}
		results = append(results, Result{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: r.Content,
			Source:  r.Engine,
		})
		if len(results) >= topK {
			break
		}
	}
	return results, nil
}
