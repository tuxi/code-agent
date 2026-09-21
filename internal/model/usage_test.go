package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"code-agent/internal/credential"
)

// TestOpenAICompatibleProviderSendsGatewayAssetRefs verifies the asset-first
// Gateway contract: only a small reference enters the chat request, never image
// bytes, a data URL, an OSS URL, or a local path. The provider is wired with a
// gateway-namespaced credential because that is what enables the correlation
// fields the Gateway needs to resolve the asset reference.
func TestOpenAICompatibleProviderSendsGatewayAssetRefs(t *testing.T) {
	var request map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"I received the screenshot"}}]}`))
	}))
	defer srv.Close()

	target := credential.Target{Namespace: "gateway", Name: "default"}
	p := NewOpenAICompatibleProvider(srv.URL, credential.StaticResolver{
		target: {Type: credential.Bearer, Secret: "jwt"},
	}, target)
	_, err := p.Complete(context.Background(), Request{SessionID: "sess_1", ExecutionID: "exec_1", Model: "vision-test", Messages: []Message{{
		Role:    RoleTool,
		Content: "Screenshot captured.",
		Assets: []GatewayAssetRef{{
			AssetID:  12345,
			SHA256:   "abc123",
			Kind:     "image",
			MIMEType: "image/png",
			Filename: "screenshot.png",
		}},
	}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	messages, ok := request["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages = %#v, want one message", request["messages"])
	}
	message := messages[0].(map[string]any)
	if request["session_id"] != "sess_1" || request["execution_id"] != "exec_1" {
		t.Fatalf("request correlation = session:%#v execution:%#v", request["session_id"], request["execution_id"])
	}
	assets, ok := message["assets"].([]any)
	if !ok || len(assets) != 1 {
		t.Fatalf("assets = %#v, want one asset ref", message["assets"])
	}
	asset := assets[0].(map[string]any)
	if asset["asset_id"] != float64(12345) || asset["mime_type"] != "image/png" {
		t.Fatalf("asset = %#v, want Gateway asset ref", asset)
	}
	encoded, _ := json.Marshal(request)
	if string(encoded) == "" || string(encoded) == "png-test-bytes" {
		t.Fatalf("request unexpectedly contains binary content: %s", encoded)
	}
}

// TestThirdPartyProviderOmitsGatewayCorrelationFields pins the Groq regression:
// a non-Gateway OpenAI-compatible endpoint must not receive the Gateway
// correlation extension, which strict providers reject with
// 400 invalid_request_error ("property 'execution_id' is unsupported").
func TestThirdPartyProviderOmitsGatewayCorrelationFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target credential.Target
	}{
		{"groq", credential.Target{Namespace: "llm", Name: "groq"}},
		{"opencode-go", credential.Target{Namespace: "llm", Name: "opencode-go"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var parsed map[string]any
				if err := json.NewDecoder(r.Body).Decode(&parsed); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				body = parsed
				if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
					return
				}
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
			}))
			defer srv.Close()

			resolver := credential.StaticResolver{tc.target: {Type: credential.Bearer, Secret: "k"}}
			p := NewOpenAICompatibleProvider(srv.URL, resolver, tc.target)
			req := Request{
				SessionID: "sess_1", TurnID: "turn_1", RequestID: "req_1", ExecutionID: "exec_1",
				Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}},
			}
			calls := map[string]func() error{
				"Complete":       func() error { _, err := p.Complete(context.Background(), req); return err },
				"CompleteStream": func() error { _, err := p.CompleteStream(context.Background(), req, nil, nil); return err },
			}
			for _, name := range []string{"Complete", "CompleteStream"} {
				body = nil
				if err := calls[name](); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				for _, key := range []string{"session_id", "turn_id", "request_id", "execution_id"} {
					if _, present := body[key]; present {
						t.Errorf("%s: body carries %q=%v; strict providers reject it", name, key, body[key])
					}
				}
			}
		})
	}
}

// TestGatewayProviderSendsCorrelationFields is the counterpart: the Gateway
// relies on these fields to resolve conversation asset references.
func TestGatewayProviderSendsCorrelationFields(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	target := credential.Target{Namespace: "gateway", Name: "default"}
	p := NewOpenAICompatibleProvider(srv.URL, credential.StaticResolver{
		target: {Type: credential.Bearer, Secret: "jwt"},
	}, target)
	_, err := p.Complete(context.Background(), Request{
		SessionID: "sess_1", TurnID: "turn_1", RequestID: "req_1", ExecutionID: "exec_1",
		Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	for key, want := range map[string]string{
		"session_id": "sess_1", "turn_id": "turn_1",
		"request_id": "req_1", "execution_id": "exec_1",
	} {
		if body[key] != want {
			t.Errorf("body[%q] = %v, want %q", key, body[key], want)
		}
	}
}

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
	p := NewOpenAICompatibleProviderWithKey(srv.URL, "" /* no key */)
	resp, err := p.Complete(context.Background(), Request{Model: "test"})
	if err != nil {
		t.Fatalf("local endpoint should not require an API key: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("Content = %q, want ok", resp.Content)
	}
}

// TestProviderClientHasNoTotalTimeout guards against reintroducing a fixed
// http.Client.Timeout AND a hardcoded ResponseHeaderTimeout. Such ceilings bound
// the exchange independently of the per-attempt context deadline: with a large
// context (hundreds of thousands of tokens), especially through a relay/proxy,
// the upstream can take well over 60 s before the first response header arrives
// (body upload + relay forwarding + prompt processing). A hardcoded header
// timeout fires first and produces false-positive timeouts that exhaust the
// retry budget. Total per-attempt time must come from ResilientProvider's
// context deadline (request_timeout_seconds); the client only bounds
// connect/TLS-handshake, phases that do not scale with generation length.
func TestProviderClientHasNoTotalTimeout(t *testing.T) {
	p := NewOpenAICompatibleProviderWithKey("https://example.test", "key")
	if p.HTTPClient.Timeout != 0 {
		t.Fatalf("http.Client.Timeout = %s, want 0 (no total ceiling — it would cap long/streamed body reads)", p.HTTPClient.Timeout)
	}
	tr, ok := p.HTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport bounding connect/TLS phases", p.HTTPClient.Transport)
	}
	if tr.ResponseHeaderTimeout != 0 {
		t.Fatalf("ResponseHeaderTimeout = %s, want 0 (hardcoded header timeout false-fires on large-context relay calls; the per-attempt context deadline is the correct bound)", tr.ResponseHeaderTimeout)
	}
	if tr.DialContext == nil {
		t.Fatal("DialContext = nil; connect phase should be bounded")
	}
	if tr.TLSHandshakeTimeout == 0 {
		t.Fatal("TLSHandshakeTimeout = 0; handshake phase should be bounded")
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

			p := NewOpenAICompatibleProviderWithKey(srv.URL, "key")
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
