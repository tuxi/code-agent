package model

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsLocalBaseURL(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		// Local endpoints — skip key requirement.
		{"http://localhost:11434/v1", true},
		{"http://127.0.0.1:8080", true},
		{"http://0.0.0.0:8000", true},
		{"http://[::1]:11434", true},
		{"https://localhost:443/v1", true},
		// Remote endpoints — key required.
		{"https://api.deepseek.com", false},
		{"https://api.openai.com/v1", false},
		{"https://dashscope.aliyuncs.com/compatible-mode/v1", false},
		// Edge cases.
		{"", false},
		{"not-a-url:://", false},
	}
	for _, tt := range tests {
		got := IsLocalBaseURL(tt.url)
		if got != tt.want {
			t.Errorf("IsLocalBaseURL(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
}

// TestLocalEndpointSkipsAPIKey verifies that a local base URL (no API key) is
// accepted by the provider, not rejected with "missing api key".
func TestLocalEndpointSkipsAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer srv.Close()

	// Strip the scheme/host from the test server URL to get a local-ish endpoint.
	// httptest.Server listens on 127.0.0.1, so it IS local.
	p := NewOpenAICompatibleProvider(srv.URL, "" /* no key */)
	resp, err := p.Complete(context.Background(), Request{Model: "test"})
	if err != nil {
		t.Fatalf("local endpoint should not require an API key: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("Content = %q, want ok", resp.Content)
	}
}

// TestProviderClientHasNoTotalTimeout guards against reintroducing a fixed
// http.Client.Timeout. Such a ceiling bounds the WHOLE exchange — including the
// response body — so it kills any streamed or long generation that runs past
// it ("context deadline exceeded ... while reading body" on long tasks). Total
// per-attempt time must come from ResilientProvider's context deadline; the
// client should only bound connect/TLS/time-to-first-byte via its Transport.
func TestProviderClientHasNoTotalTimeout(t *testing.T) {
	p := NewOpenAICompatibleProvider("https://example.test", "key")
	if p.HTTPClient.Timeout != 0 {
		t.Fatalf("http.Client.Timeout = %s, want 0 (no total ceiling — it would cap long/streamed body reads)", p.HTTPClient.Timeout)
	}
	tr, ok := p.HTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport bounding connect/TLS/header phases", p.HTTPClient.Transport)
	}
	if tr.ResponseHeaderTimeout == 0 {
		t.Fatal("ResponseHeaderTimeout = 0; time-to-first-byte should still be bounded")
	}
}

// TestParsesCachedPromptTokens verifies the cached-input portion is read from
// either provider's reporting shape (deepseek's flat field or OpenAI's nested
// detail), and is 0 when neither is present.
func TestParsesCachedPromptTokens(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"deepseek", `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_cache_hit_tokens":80}}`, 80},
		{"openai", `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":60}}}`, 60},
		{"none", `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":100,"completion_tokens":5}}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			p := NewOpenAICompatibleProvider(srv.URL, "key")
			resp, err := p.Complete(context.Background(), Request{Model: "m"})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if resp.Usage.CachedPromptTokens != tc.want {
				t.Fatalf("CachedPromptTokens = %d, want %d", resp.Usage.CachedPromptTokens, tc.want)
			}
			if resp.Usage.PromptTokens != 100 {
				t.Fatalf("PromptTokens = %d, want 100 (cached is a breakdown, not extra)", resp.Usage.PromptTokens)
			}
		})
	}
}
