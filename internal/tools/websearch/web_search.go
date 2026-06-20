package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"code-agent/internal/app"
	"code-agent/internal/tools"
)

// Tool implements the web_search tool. It queries the configured search
// provider and returns deduplicated, top-K-capped results. When the primary
// provider fails, it automatically falls back to the fallback provider.
type Tool struct {
	Primary  SearchProvider
	Fallback SearchProvider
	TopK     int
}

// NewTool builds a web_search tool from the web section of config. It returns
// nil if no provider is configured (the tool is simply not registered, and the
// model won't see it).
func NewTool(cfg app.WebConfig) *Tool {
	t := &Tool{
		TopK: cfg.Search.TopK,
	}
	if t.TopK <= 0 {
		t.TopK = 5
	}

	timeout := cfg.Search.TimeoutSeconds
	if timeout <= 0 {
		timeout = 10
	}

	switch cfg.Search.Provider {
	case "brave":
		if key := cfg.Search.BraveAPIKey(); key != "" {
			t.Primary = NewBrave(key, timeout)
		}
	case "searxng":
		t.Primary = NewSearXNG(cfg.Search.SearXNGInstances(), timeout)
	default: // "tavily" or empty — Tavily is the default provider
		if key := cfg.Search.TavilyAPIKey(); key != "" {
			t.Primary = NewTavily(key, timeout)
		}
	}

	switch cfg.Search.FallbackProvider {
	case "brave":
		if key := cfg.Search.BraveAPIKey(); key != "" {
			t.Fallback = NewBrave(key, timeout)
		}
	case "tavily":
		if key := cfg.Search.TavilyAPIKey(); key != "" {
			t.Fallback = NewTavily(key, timeout)
		}
	case "searxng":
		t.Fallback = NewSearXNG(cfg.Search.SearXNGInstances(), timeout)
	}

	return t
}

func (t *Tool) Name() string { return "web_search" }

func (t *Tool) Description() string {
	return "Search the web for real-time information. Returns structured results with titles, URLs, snippets, and source engine. Use this to find current documentation, recent news, or up-to-date technical answers before calling web_fetch on a specific URL."
}

type searchInput struct {
	Query string `json:"query"`
	TopK  int    `json:"top_k,omitempty"`
}

func (t *Tool) InputSchema() json.RawMessage {
	return tools.Object(map[string]tools.Property{
		"query": {Type: "string", Description: "Natural-language search query."},
		"top_k": {Type: "integer", Description: "Maximum results to return (default 5)."},
	}, "query").JSON()
}

func (t *Tool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	var in searchInput
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("invalid web_search input: %w", err)
		}
	}

	in.Query = strings.TrimSpace(in.Query)
	if in.Query == "" {
		return tools.ToolResult{}, fmt.Errorf("query is required")
	}
	if in.TopK <= 0 {
		in.TopK = t.TopK
	}

	results, err := t.search(ctx, in.Query, in.TopK)
	if err != nil {
		return tools.ToolResult{}, err
	}

	b, err := json.MarshalIndent(struct {
		Query   string   `json:"query"`
		Results []Result `json:"results"`
	}{Query: in.Query, Results: results}, "", "  ")
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("marshal results: %w", err)
	}

	return tools.ToolResult{Content: string(b)}, nil
}

func (t *Tool) search(ctx context.Context, query string, topK int) ([]Result, error) {
	if t.Primary == nil {
		return nil, fmt.Errorf(
			"no search provider configured: the default provider is tavily but no API key was found. " +
				"Set web.search.tavily_api_key_env in config.yaml to an env var holding your Tavily key " +
				"(free tier at https://tavily.com), or switch web.search.provider to searxng/brave")
	}

	var results []Result
	var errs []string

	results, err := t.Primary.Search(ctx, query, topK)
	if err != nil {
		errs = append(errs, fmt.Sprintf("%s: %s", t.Primary.Name(), err.Error()))
	}

	// Fallback: only when primary returned no results (error or empty).
	if len(results) == 0 && t.Fallback != nil {
		results, err = t.Fallback.Search(ctx, query, topK)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", t.Fallback.Name(), err.Error()))
		}
	}

	if len(results) == 0 {
		if len(errs) > 0 {
			return nil, fmt.Errorf("search failed: %s", strings.Join(errs, "; "))
		}
		return nil, fmt.Errorf("no results found for %q", query)
	}

	// Deduplicate by URL, then cap. Provider order is preserved — relevance and
	// quality ranking are left to the provider and the calling model.
	results = dedup(results)
	if len(results) > topK {
		results = results[:topK]
	}

	return results, nil
}

func dedup(results []Result) []Result {
	seen := make(map[string]bool, len(results))
	out := make([]Result, 0, len(results))
	for _, r := range results {
		key := strings.ToLower(r.URL)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}
