// Package websearch provides the web_search tool — a multi-provider internet
// search capability with automatic fallback and deduplication.
package websearch

import "context"

// Result is a single search result, normalized across providers.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
	Source  string `json:"source"`
}

// SearchProvider abstracts a search backend (SearXNG, Brave, Bing, etc.).
type SearchProvider interface {
	// Search executes a query and returns up to topK results.
	Search(ctx context.Context, query string, topK int) ([]Result, error)

	// Name returns a short provider identifier for diagnostics and the Source
	// field of each result.
	Name() string
}
