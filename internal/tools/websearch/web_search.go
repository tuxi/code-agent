package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"code-agent/internal/app"
	"code-agent/internal/credential"
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
func NewTool(cfg app.WebConfig, resolver credential.Resolver) *Tool {
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
	gatewayTimeout := cfg.Search.GatewayTimeoutSeconds
	if gatewayTimeout <= 0 {
		gatewayTimeout = 120
	}

	switch cfg.Search.Provider {
	case "gateway":
		t.Primary = NewGatewaySearchProvider(
			resolver,
			cfg.Search.Credential.Target(),
			cfg.Search.GatewayBaseURL,
			gatewayTimeout,
		)
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
	// Managed search owns provider selection and billing at Gateway. Falling back
	// to a Runtime provider would bypass its financial ledger.
	if cfg.Search.Provider == "gateway" {
		return t
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

func (t *Tool) Execute(ctx context.Context, ec tools.ExecutionContext, input json.RawMessage) (tools.ToolResult, error) {
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

	results, usage, err := t.search(ctx, ec, in.Query, in.TopK)
	if err != nil {
		return tools.ToolResult{}, err
	}

	b, err := marshalBudgeted(in.Query, results)
	if err != nil {
		return tools.ToolResult{}, err
	}

	return tools.ToolResult{Content: string(b), Usage: usage}, nil
}

// Output-budget constants. The tool keeps its own JSON under the agent loop's
// per-observation cap so the generic truncation never fires on it: a byte-level
// cut through the middle of a results array leaves half a URL or half a snippet
// for the model to complete by guessing. Here excess is shed structurally —
// whole results dropped, long snippets clipped at a rune boundary — so the
// model always receives valid, complete JSON, with the omissions declared.
const (
	// maxSnippetBytes bounds one result's snippet; providers occasionally return
	// article-length content.
	maxSnippetBytes = 2000
	// maxOutputBytes bounds the marshaled JSON. Must stay comfortably below the
	// agent loop's observation cap (30000).
	maxOutputBytes = 20000
)

type searchOutput struct {
	Query   string   `json:"query"`
	Results []Result `json:"results"`
	// ResultsOmitted tells the model explicitly that trailing results were
	// dropped for budget, so a short list reads as "capped", not "that's all
	// there is".
	ResultsOmitted int `json:"results_omitted,omitempty"`
}

// marshalBudgeted renders results as JSON within maxOutputBytes, dropping whole
// trailing results (never cutting one) until it fits.
func marshalBudgeted(query string, results []Result) ([]byte, error) {
	for i := range results {
		results[i].Snippet = clipRunes(results[i].Snippet, maxSnippetBytes)
	}
	out := searchOutput{Query: query, Results: results}
	for {
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("marshal results: %w", err)
		}
		if len(b) <= maxOutputBytes || len(out.Results) <= 1 {
			return b, nil
		}
		out.Results = out.Results[:len(out.Results)-1]
		out.ResultsOmitted++
	}
}

// clipRunes caps s at max bytes without splitting a UTF-8 rune, marking the cut
// with an ellipsis.
func clipRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func (t *Tool) search(ctx context.Context, ec tools.ExecutionContext, query string, topK int) ([]Result, *tools.ToolUsage, error) {
	if t.Primary == nil {
		return nil, nil, fmt.Errorf(
			"no search provider configured: the default provider is tavily but no API key was found. " +
				"Set web.search.tavily_api_key_env in config.yaml to an env var holding your Tavily key " +
				"(free tier at https://tavily.com), or switch web.search.provider to searxng/brave")
	}

	request := SearchRequest{
		Query: query, TopK: topK,
		CallID: ec.CallID, SessionID: ec.SessionID, TurnID: ec.TurnID,
		ExecutionID: ec.ExecutionID,
	}
	var response SearchResponse
	var errs []string
	var err error

	response, err = t.Primary.Search(ctx, request)
	if err != nil {
		var fatal tools.TurnFatalToolError
		if errors.As(err, &fatal) && fatal.TurnFatalToolError() {
			return nil, nil, err
		}
		errs = append(errs, fmt.Sprintf("%s: %s", t.Primary.Name(), err.Error()))
	}

	// Fallback: only when primary returned no results (error or empty).
	if len(response.Results) == 0 && t.Fallback != nil {
		response, err = t.Fallback.Search(ctx, request)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", t.Fallback.Name(), err.Error()))
		}
	}

	if len(response.Results) == 0 {
		if len(errs) > 0 {
			return nil, nil, fmt.Errorf("search failed: %s", strings.Join(errs, "; "))
		}
		// A managed call can legitimately return no matches and still consume
		// provider credits. Treat that as a successful empty result so its receipt
		// reaches tool_finished and the model cannot retry behind the ledger's back.
		if response.Usage != nil {
			return []Result{}, response.Usage, nil
		}
		return nil, nil, fmt.Errorf("no results found for %q", query)
	}

	// Deduplicate by URL, then cap. Provider order is preserved — relevance and
	// quality ranking are left to the provider and the calling model.
	results := dedup(response.Results)
	if len(results) > topK {
		results = results[:topK]
	}

	return results, response.Usage, nil
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
