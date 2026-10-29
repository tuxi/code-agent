//go:build unix

package mcpbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAttachEmptySessionRejectsUnsafeOrAmbiguousState(t *testing.T) {
	root := testRepo(t)
	tests := []struct {
		name   string
		change func(map[string]any, map[string]any, *map[string]any, *[]Event)
	}{
		{"wrong_id", func(d, _ map[string]any, _ *map[string]any, _ *[]Event) { d["id"] = "other" }},
		{"wrong_workspace", func(d, _ map[string]any, _ *map[string]any, _ *[]Event) { d["workspace_path"] = root + "/child" }},
		{"historical_turn", func(d, _ map[string]any, _ *map[string]any, _ *[]Event) { d["turn_count"] = 1 }},
		{"historical_message", func(d, _ map[string]any, _ *map[string]any, _ *[]Event) { d["message_count"] = 1 }},
		{"missing_count", func(d, _ map[string]any, _ *map[string]any, _ *[]Event) { delete(d, "turn_count") }},
		{"persisted_running", func(d, _ map[string]any, _ *map[string]any, _ *[]Event) { d["turn_status"] = "running" }},
		{"archived", func(d, _ map[string]any, _ *map[string]any, _ *[]Event) { d["archived_at"] = "2026-10-28T00:00:00Z" }},
		{"needs_rebind", func(d, _ map[string]any, _ *map[string]any, _ *[]Event) { d["needs_rebind"] = true }},
		{"fork", func(d, _ map[string]any, _ *map[string]any, _ *[]Event) { d["fork_source_session_id"] = "source" }},
		{"other_policy", func(d, _ map[string]any, _ *map[string]any, _ *[]Event) { d["execution_policy"] = "read_only" }},
		{"stale_header", func(_, _ map[string]any, _ *map[string]any, e *[]Event) {
			*e = []Event{{SessionID: "s1", TurnID: "old", Kind: "turn_accepted", Seq: 1}}
		}},
		{"scheduler_running", func(_, a map[string]any, _ *map[string]any, _ *[]Event) { a["state"] = "running" }},
		{"queued", func(_, a map[string]any, _ *map[string]any, _ *[]Event) { a["queue_position"] = 1 }},
		{"pending_approval", func(_, a map[string]any, _ *map[string]any, _ *[]Event) { a["pending_approval_count"] = 1 }},
		{"pending_client_tool", func(_, a map[string]any, _ *map[string]any, _ *[]Event) { a["pending_client_tool_count"] = 1 }},
		{"active_turn", func(_, a map[string]any, _ *map[string]any, _ *[]Event) { a["active_turn_id"] = "active" }},
		{"missing_live_count", func(_, a map[string]any, _ *map[string]any, _ *[]Event) { delete(a, "pending_approval_count") }},
		{"history_watermark", func(_, a map[string]any, _ *map[string]any, _ *[]Event) { a["last_sequence"] = 5 }},
		{"previous_terminal", func(_, a map[string]any, _ *map[string]any, _ *[]Event) {
			a["latest_terminal"] = map[string]any{"turn_id": "old"}
		}},
		{"delta", func(_, _ map[string]any, a *map[string]any, _ *[]Event) { (*a)["is_delta"] = true }},
		{"missing_snapshot_kind", func(_, _ map[string]any, a *map[string]any, _ *[]Event) { delete(*a, "is_delta") }},
		{"missing_activity", func(_, _ map[string]any, a *map[string]any, _ *[]Event) { delete(*a, "sessions") }},
		{"null_activity", func(_, _ map[string]any, a *map[string]any, _ *[]Event) { (*a)["sessions"] = nil }},
		{"duplicate_activity", func(_, row map[string]any, a *map[string]any, _ *[]Event) { (*a)["sessions"] = []any{row, row} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			detail := map[string]any{"id": "s1", "workspace_path": root, "turn_count": 0, "message_count": 0, "execution_policy": "shared_workspace"}
			row := map[string]any{"session_id": "s1", "state": "idle", "queue_position": 0, "pending_approval_count": 0, "pending_client_tool_count": 0, "last_sequence": 0}
			activity := map[string]any{"is_delta": false, "sessions": []any{row}}
			events := []Event{}
			test.change(detail, row, &activity, &events)
			var writes atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes.Add(1)
					w.WriteHeader(500)
					return
				}
				var data any
				switch r.URL.Path {
				case "/v1/runtime/info", "/v1/tasks/capabilities":
					data = map[string]string{"server_id": "server-1", "store_id": "store-1"}
				case "/v1/conversations/s1":
					data = detail
				case "/v1/conversations/s1/events":
					data = events
				case "/v1/activity":
					data = activity
				default:
					w.WriteHeader(404)
					return
				}
				_ = json.NewEncoder(w).Encode(data)
			}))
			defer srv.Close()
			path, _ := writeGrant(t, srv.URL)
			c, _, err := NewFromManifest(context.Background(), path, root)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if _, err = c.AttachEmptySession(context.Background(), "s1"); err == nil {
				t.Fatal("unsafe session attached")
			}
			if c.id != "" || writes.Load() != 0 {
				t.Fatal("rejection changed binding or sent a mutation")
			}
		})
	}
}

func TestAttachEmptySessionThenDispatchNewTurnWithoutCreatingSession(t *testing.T) {
	root := testRepo(t)
	f := &fakeRuntime{root: root, events: []Event{}}
	var creates, streams atomic.Int32
	var sparse atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("fixture-token", 4) {
			w.WriteHeader(401)
			return
		}
		var data any
		switch r.URL.Path {
		case "/v1/runtime/info", "/v1/tasks/capabilities":
			data = map[string]string{"server_id": "server-1", "store_id": "store-1"}
		case "/v1/conversations/s1":
			data = map[string]any{"id": "s1", "workspace_path": root, "turn_count": 0, "message_count": 0, "execution_policy": "shared_workspace"}
		case "/v1/activity":
			data = map[string]any{"is_delta": false, "sessions": []any{map[string]any{"session_id": "s1", "state": "idle", "queue_position": 0, "pending_approval_count": 0, "pending_client_tool_count": 0, "last_sequence": 0}}}
			if sparse.Load() {
				data = map[string]any{"is_delta": false, "sessions": []any{}}
			}
		default:
			if r.Method == "POST" {
				creates.Add(1)
			}
			if strings.HasSuffix(r.URL.Path, "/stream") {
				streams.Add(1)
			}
			// The existing fake uses a different credential; translate only after
			// validating the new manifest credential at this test boundary.
			r.Header.Set("Authorization", "Bearer test-token")
			f.serve(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(data)
	}))
	defer srv.Close()
	path, m := writeGrant(t, srv.URL)
	// A raw endpoint/token does not prove runtime/store identity.
	legacy, err := New(context.Background(), srv.URL, m.AccessToken, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = legacy.AttachEmptySession(context.Background(), "s1"); err == nil {
		t.Fatal("unverified client attached")
	}
	legacy.Close()
	withoutStore := m
	withoutStore.StoreID = ""
	saveGrant(t, path, withoutStore)
	limited, _, err := NewFromManifest(context.Background(), path, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = limited.AttachEmptySession(context.Background(), "s1"); err == nil {
		t.Fatal("missing store identity accepted")
	}
	limited.Close()
	saveGrant(t, path, m)
	c, _, err := NewFromManifest(context.Background(), path, root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.AttachEmptySession(context.Background(), "../s1"); err == nil {
		t.Fatal("invalid ID accepted")
	}
	if _, err = c.AttachEmptySession(context.Background(), "missing"); err == nil {
		t.Fatal("missing session accepted")
	}
	ref, err := c.AttachEmptySession(context.Background(), "s1")
	if err != nil || ref.ID != "s1" || ref.WorkspacePath != root {
		t.Fatalf("attach failed: %v", err)
	}
	if creates.Load() != 0 || streams.Load() != 0 || f.inputs != 0 {
		t.Fatal("attachment started work")
	}
	// Unused idle sessions are absent from the real server's sparse projection.
	sparse.Store(true)
	other, _, err := NewFromManifest(context.Background(), path, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.AttachEmptySession(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	other.Close()
	if _, err = c.AttachEmptySession(context.Background(), "s1"); err == nil {
		t.Fatal("second attachment accepted")
	}
	if _, err = c.Create(context.Background(), "new"); err == nil {
		t.Fatal("attached client created another session")
	}
	receipt, err := c.Send(context.Background(), "s1", "new-turn", "fixture only", "")
	if err != nil || !receipt.Accepted || receipt.SessionID != "s1" {
		t.Fatalf("dispatch failed: %v", err)
	}
	if creates.Load() != 0 || streams.Load() != 1 {
		t.Fatal("unexpected create or stream count")
	}
}
