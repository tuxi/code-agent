package websearch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTavilyProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		var req tavilyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.APIKey != "tvly-test" {
			t.Errorf("expected api_key in body, got %q", req.APIKey)
		}
		if req.Query == "" {
			t.Error("missing query in body")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tavilyResponse{
			Query: req.Query,
			Results: []tavilyResult{
				{Title: "Result 1", URL: "https://a.com", Content: "Snippet 1", Score: 0.9},
				{Title: "Result 2", URL: "https://b.com", Content: "Snippet 2", Score: 0.8},
				{Title: "No URL", URL: "", Content: "dropped", Score: 0.1},
			},
		})
	}))
	defer srv.Close()

	p := &TavilyProvider{APIKey: "tvly-test", BaseURL: srv.URL, Client: srv.Client()}
	response, err := p.Search(context.Background(), SearchRequest{Query: "test query", TopK: 5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(response.Results) != 2 {
		t.Fatalf("expected 2 results (empty-URL dropped), got %d", len(response.Results))
	}
	if response.Results[0].Title != "Result 1" || response.Results[0].URL != "https://a.com" {
		t.Errorf("unexpected first result: %+v", response.Results[0])
	}
	if response.Results[0].Snippet != "Snippet 1" {
		t.Errorf("expected content mapped to snippet, got %q", response.Results[0].Snippet)
	}
	if response.Results[0].Source != "tavily" {
		t.Errorf("expected source 'tavily', got %q", response.Results[0].Source)
	}
}

func TestTavilyTopKCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tavilyResponse{
			Results: []tavilyResult{
				{Title: "1", URL: "https://a.com", Content: "..."},
				{Title: "2", URL: "https://b.com", Content: "..."},
				{Title: "3", URL: "https://c.com", Content: "..."},
			},
		})
	}))
	defer srv.Close()

	p := &TavilyProvider{APIKey: "k", BaseURL: srv.URL, Client: srv.Client()}
	response, err := p.Search(context.Background(), SearchRequest{Query: "q", TopK: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(response.Results) != 2 {
		t.Fatalf("expected topK cap of 2, got %d", len(response.Results))
	}
}

func TestTavilyHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	p := &TavilyProvider{APIKey: "bad", BaseURL: srv.URL, Client: srv.Client()}
	_, err := p.Search(context.Background(), SearchRequest{Query: "q", TopK: 5})
	if err == nil {
		t.Fatal("expected error on HTTP 401")
	}
}

func TestTavilyName(t *testing.T) {
	if (&TavilyProvider{}).Name() != "tavily" {
		t.Error("expected name 'tavily'")
	}
}
