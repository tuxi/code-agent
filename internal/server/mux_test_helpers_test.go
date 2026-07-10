package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// decodeResponse decodes a unified-envelope HTTP response body into v.
// It unwraps the {trace_id, code, msg, data} envelope.
func decodeResponse(t *testing.T, resp *http.Response, v interface{}) {
	t.Helper()
	defer resp.Body.Close()
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if v == nil {
		return
	}
	if err := json.Unmarshal(env.Data, v); err != nil {
		t.Fatalf("decode data: %v (raw: %s)", err, string(env.Data))
	}
}
