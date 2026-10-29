package model

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProviderReportedModelIsNotInferred(t *testing.T) {
	for _, actual := range []string{"served-revision", ""} {
		t.Run("actual="+actual, func(t *testing.T) {
			for _, stream := range []bool{false, true} {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if stream {
						fmt.Fprintf(w, "data: {\"model\":%q,\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", actual)
					} else {
						fmt.Fprintf(w, `{"model":%q,"choices":[{"message":{"content":"ok"}}]}`, actual)
					}
				}))
				p := NewOpenAICompatibleProviderWithKey(server.URL, "")
				var resp Response
				var err error
				if stream {
					resp, err = p.CompleteStream(context.Background(), Request{Model: "requested-alias"}, nil, nil)
				} else {
					resp, err = p.Complete(context.Background(), Request{Model: "requested-alias"})
				}
				server.Close()
				if err != nil || resp.ActualModel != actual {
					t.Fatalf("actual=%q error=%v", resp.ActualModel, err)
				}
			}
		})
	}
}
