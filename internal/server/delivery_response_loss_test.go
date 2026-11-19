package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Commit the real handler's response, then close the TCP connection before the
// acceptance reaches the client. The database and executor remain unchanged.
func deliveryResponseLossServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	var dropped atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saved := httptest.NewRecorder()
		handler.ServeHTTP(saved, r)
		if r.Method == http.MethodPost && saved.Code == http.StatusAccepted && !dropped.Swap(true) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		for key, values := range saved.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(saved.Code)
		_, _ = w.Write(saved.Body.Bytes())
	}))
	t.Cleanup(srv.Close)
	return srv
}

func recoverLostDeliveryResponse(t *testing.T, ctx context.Context, srv *httptest.Server, path, id, token, body string) []byte {
	t.Helper()
	request := func(method, path, body string) ([]byte, int, error) {
		req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := srv.Client().Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		return data, response.StatusCode, err
	}
	if _, status, err := request("POST", path, body); err == nil {
		t.Fatal("acceptance should be lost on TCP connection", status)
	}
	queried, status, err := request("GET", path+"/"+id, "")
	if err != nil || status != 200 {
		t.Fatal(status, err, string(queried))
	}
	retried, status, err := request("POST", path, body)
	if err != nil || status != 202 || !bytes.Equal(retried, queried) {
		t.Fatal("retry changed original receipt", status, err, string(retried), string(queried))
	}
	return retried
}
