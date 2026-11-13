package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestExplicitOutputBudgetOnChatWire(t *testing.T) {
	for _, stream := range []bool{false, true} {
		p := NewOpenAICompatibleProviderWithKey("https://example.invalid/v1", "offline")
		p.HTTPClient = &http.Client{Transport: modelRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			var b map[string]any
			if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
				t.Fatal(err)
			}
			if b["max_tokens"] != float64(8192) {
				t.Fatalf("missing bound: %v", b["max_tokens"])
			}
			body := `{"choices":[{"message":{"content":"ok"}}]}`
			if stream {
				body = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		req := Request{Model: "test", MaxOutputTokens: 8192}
		var err error
		if stream {
			_, err = p.CompleteStream(context.Background(), req, nil, nil)
		} else {
			_, err = p.Complete(context.Background(), req)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if toResponsesRequest(Request{MaxOutputTokens: 8192}, false).MaxOutputTokens != 8192 {
		t.Fatal("Responses lost budget")
	}
}
