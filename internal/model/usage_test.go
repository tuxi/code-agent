package model

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
