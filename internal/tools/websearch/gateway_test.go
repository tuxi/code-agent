package websearch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"code-agent/internal/credential"
	"code-agent/internal/tools"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestGatewaySearchProviderUsesResolverAndExecutionIdentity(t *testing.T) {
	target := credential.Target{Namespace: "gateway", Name: "default"}
	resolver := credential.StaticResolver{
		target: {Type: credential.Bearer, Secret: "jwt-session-token"},
	}
	provider := NewGatewaySearchProvider(resolver, target, "https://gateway.example/api/v1/agent/", 10)
	provider.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.URL.String(); got != "https://gateway.example/api/v1/agent/tools/web-search" {
			t.Fatalf("URL = %q", got)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer jwt-session-token" {
			t.Fatalf("Authorization = %q", got)
		}
		var body gatewayRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.CallID != "call_web_1" || body.SessionID != "session_1" || body.TurnID != "turn_1" || body.ExecutionID != "inv_1" {
			t.Fatalf("execution identity = %+v", body)
		}
		if body.Query != "golang" || body.MaxResults != 3 || body.SearchDepth != "basic" || body.Topic != "general" {
			t.Fatalf("search request = %+v", body)
		}
		return jsonResponse(http.StatusOK, `{
          "trace_id":"trace-1","code":0,"msg":"success","data":{
            "query":"golang","results":[{"title":"Go","url":"https://go.dev","content":"The Go language"}],
            "usage":{"tool_call_id":"call_web_1","tool_name":"web_search","provider":"tavily","operation":"basic","provider_credits":1,"billing_units":8000,"funding_source":"subscription","reservation_id":"res-1","pricing_version":2},
            "replayed":false
          }
        }`), nil
	})}

	result, err := provider.Search(context.Background(), SearchRequest{
		SessionID: "session_1", TurnID: "turn_1", ExecutionID: "inv_1", CallID: "call_web_1", Query: "golang", TopK: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].URL != "https://go.dev" {
		t.Fatalf("results = %+v", result.Results)
	}
	if result.Usage == nil || result.Usage.BillingUnits != 8000 || result.Usage.ProviderCredits != 1 {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

func TestGatewaySearchProviderClassifiesAuthAndQuota(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{status: http.StatusUnauthorized, code: "auth_expired"},
		{status: http.StatusTooManyRequests, code: "quota_exceeded"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			target := credential.Target{Namespace: "gateway", Name: "default"}
			provider := NewGatewaySearchProvider(credential.StaticResolver{
				target: {Type: credential.Bearer, Secret: "jwt"},
			}, target, "https://gateway.example/api/v1/agent", 10)
			provider.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return jsonResponse(tc.status, `{"code":401,"msg":"denied","data":null}`), nil
			})}
			_, err := provider.Search(context.Background(), SearchRequest{CallID: "call_1", Query: "q", TopK: 1})
			if err == nil {
				t.Fatal("expected error")
			}
			fatal, ok := err.(tools.TurnFatalToolError)
			if !ok || !fatal.TurnFatalToolError() {
				t.Fatalf("error is not turn-fatal: %T %v", err, err)
			}
			coded, ok := err.(interface{ LifecycleErrorCode() string })
			if !ok || coded.LifecycleErrorCode() != tc.code {
				t.Fatalf("lifecycle code = %v", coded)
			}
		})
	}
}

func TestGatewaySearchProviderRequiresCallID(t *testing.T) {
	provider := NewGatewaySearchProvider(nil, credential.Target{}, "https://gateway.example/api/v1/agent", 10)
	_, err := provider.Search(context.Background(), SearchRequest{Query: "q", TopK: 1})
	if err == nil || !strings.Contains(err.Error(), "call_id") {
		t.Fatalf("error = %v", err)
	}
}

func TestGatewaySearchProviderRejectsMissingUsageReceipt(t *testing.T) {
	target := credential.Target{Namespace: "gateway", Name: "default"}
	provider := NewGatewaySearchProvider(credential.StaticResolver{
		target: {Type: credential.Bearer, Secret: "jwt"},
	}, target, "https://gateway.example/api/v1/agent", 10)
	provider.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"code":0,"msg":"success","data":{"results":[]}}`), nil
	})}

	_, err := provider.Search(context.Background(), SearchRequest{CallID: "call_1", Query: "q", TopK: 1})
	if err == nil || !strings.Contains(err.Error(), "usage receipt") {
		t.Fatalf("error = %v, want missing usage receipt", err)
	}
	fatal, ok := err.(tools.TurnFatalToolError)
	if !ok || !fatal.TurnFatalToolError() {
		t.Fatalf("protocol error is not turn-fatal: %T %v", err, err)
	}
}

func TestGatewaySearchProviderRejectsMismatchedUsageCallID(t *testing.T) {
	target := credential.Target{Namespace: "gateway", Name: "default"}
	provider := NewGatewaySearchProvider(credential.StaticResolver{
		target: {Type: credential.Bearer, Secret: "jwt"},
	}, target, "https://gateway.example/api/v1/agent", 10)
	provider.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"code":0,"msg":"success","data":{"results":[],"usage":{"tool_call_id":"another_call","tool_name":"web_search","provider":"tavily","operation":"basic","provider_credits":1,"billing_units":8000,"funding_source":"subscription","reservation_id":"res-1","pricing_version":2}}}`), nil
	})}

	_, err := provider.Search(context.Background(), SearchRequest{CallID: "call_1", Query: "q", TopK: 1})
	if err == nil || !strings.Contains(err.Error(), "call_id mismatch") {
		t.Fatalf("error = %v, want call_id mismatch", err)
	}
}

func TestGatewaySearchProviderDoesNotRetryPermanentCallIDConflict(t *testing.T) {
	target := credential.Target{Namespace: "gateway", Name: "default"}
	provider := NewGatewaySearchProvider(credential.StaticResolver{
		target: {Type: credential.Bearer, Secret: "jwt"},
	}, target, "https://gateway.example/api/v1/agent", 10)
	attempts := 0
	provider.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return jsonResponse(http.StatusConflict, `{"code":123,"msg":"tool call id was already used with a different request"}`), nil
	})}

	_, err := provider.Search(context.Background(), SearchRequest{CallID: "call_1", Query: "q", TopK: 1})
	if err == nil {
		t.Fatal("expected conflict")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want no retry for permanent conflict", attempts)
	}
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
