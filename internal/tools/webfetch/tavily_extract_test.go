package webfetch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"code-agent/internal/app"
)

func TestTavilyExtractor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req tavilyExtractRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.APIKey != "tvly-x" {
			t.Errorf("expected api_key in body, got %q", req.APIKey)
		}
		if len(req.URLs) != 1 || req.URLs[0] != "https://example.com/x" {
			t.Errorf("unexpected urls: %v", req.URLs)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tavilyExtractResponse{
			Results: []tavilyExtractResult{
				{URL: "https://example.com/x", RawContent: "# Page\nhello world"},
			},
		})
	}))
	defer srv.Close()

	e := &tavilyExtractor{apiKey: "tvly-x", baseURL: srv.URL, client: srv.Client()}
	content, err := e.Extract(context.Background(), "https://example.com/x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != "# Page\nhello world" {
		t.Errorf("unexpected content: %q", content)
	}
}

func TestTavilyExtractor_EmptyResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tavilyExtractResponse{Results: nil})
	}))
	defer srv.Close()

	e := &tavilyExtractor{apiKey: "k", baseURL: srv.URL, client: srv.Client()}
	if _, err := e.Extract(context.Background(), "https://example.com"); err == nil {
		t.Fatal("expected error on empty results")
	}
}

// fakeFallback is an injectable contentFallback for testing the fetch fallback
// path without hitting a real extractor.
type fakeFallback struct {
	content string
	calls   int
}

func (f *fakeFallback) Extract(_ context.Context, _ string) (string, error) {
	f.calls++
	return f.content, nil
}

// TestFetch_FallbackOnDirectFailure: a direct fetch to a dead port fails at the
// transport layer, so the configured fallback recovers the content.
func TestFetch_FallbackOnDirectFailure(t *testing.T) {
	tool := newTool(app.WebConfig{}, true) // allowPrivate: dead-port dial isn't SSRF-blocked
	fb := &fakeFallback{content: "## Recovered via fallback\nbody text"}
	tool.fallback = fb

	out, err := tool.fetch(context.Background(), "http://127.0.0.1:1/unreachable")
	if err != nil {
		t.Fatalf("expected fallback to recover, got error: %v", err)
	}
	if fb.calls != 1 {
		t.Errorf("expected fallback called once, got %d", fb.calls)
	}
	if !strings.Contains(out.Markdown, "Recovered via fallback") {
		t.Errorf("expected fallback content in output, got: %q", out.Markdown)
	}
}

// TestFetch_NoFallbackOnSSRFBlock is the security-critical case: an SSRF-blocked
// dial must NOT trigger the fallback, or the internal URL leaks to a third party.
func TestFetch_NoFallbackOnSSRFBlock(t *testing.T) {
	tool := NewTool(app.WebConfig{}) // production: SSRF guard active
	fb := &fakeFallback{content: "should never be used"}
	tool.fallback = fb

	_, err := tool.fetch(context.Background(), "http://127.0.0.1:8080/internal")
	if err == nil {
		t.Fatal("expected an SSRF block error")
	}
	if fb.calls != 0 {
		t.Errorf("fallback must NOT run on an SSRF block (would leak the internal URL), got %d calls", fb.calls)
	}
}
