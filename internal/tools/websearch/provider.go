// Package websearch provides the web_search tool — a multi-provider internet
// search capability with automatic fallback and deduplication.
package websearch

import (
	"context"

	"code-agent/internal/tools"
)

// Result is a single search result, normalized across providers.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
	Source  string `json:"source"`
}

// SearchRequest is the provider-neutral request. Runtime execution identifiers
// are populated for managed providers and harmlessly ignored by direct/BYOK
// providers.
type SearchRequest struct {
	Query       string
	TopK        int
	CallID      string
	SessionID   string
	TurnID      string
	ExecutionID string
}

// SearchResponse carries normalized model-facing results plus an optional
// managed-tool billing receipt. Direct/BYOK providers leave Usage nil.
type SearchResponse struct {
	Results  []Result
	Usage    *tools.ToolUsage
	Replayed bool
}

// SearchProvider abstracts a search backend (SearXNG, Brave, Bing, etc.).
type SearchProvider interface {
	// Search executes a query and returns normalized results.
	Search(ctx context.Context, req SearchRequest) (SearchResponse, error)

	// Name returns a short provider identifier for diagnostics and the Source
	// field of each result.
	Name() string
}
