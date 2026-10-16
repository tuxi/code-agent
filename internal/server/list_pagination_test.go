package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"code-agent/internal/conversation"
	"code-agent/internal/session"
	sessionsqlite "code-agent/internal/session/sqlite"
)

func TestConversationListPaginationStableCursorAndCompatibility(t *testing.T) {
	store, err := sessionsqlite.New(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	for _, id := range []string{"a", "b", "c"} {
		if err := store.Save(context.Background(), &session.Session{ID: id, Name: "name " + id, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	repo := conversation.NewSQLiteRepository(store, 1000, 800, "test", "", nil)
	srv := httptest.NewServer(newTestMux(repo, &conversation.StoreEventAdapter{Store: store}))
	defer srv.Close()
	get := func(query string) ([]string, string, string) {
		t.Helper()
		r, err := http.Get(srv.URL + "/v1/conversations" + query)
		if err != nil {
			t.Fatal(err)
		}
		if r.StatusCode != 200 {
			r.Body.Close()
			t.Fatalf("status=%d", r.StatusCode)
		}
		next, more := r.Header.Get("X-Codeagent-Next-Cursor"), r.Header.Get("X-Codeagent-Has-More")
		var refs []ConversationRef
		decodeResponse(t, r, &refs)
		ids := []string{}
		for _, ref := range refs {
			ids = append(ids, ref.ID)
		}
		return ids, next, more
	}
	first, cursor, more := get("?limit=2")
	if !reflect.DeepEqual(first, []string{"c", "b"}) || cursor == "" || more != "1" {
		t.Fatalf("first=%v cursor=%s more=%s", first, cursor, more)
	}
	last, next, more := get("?before=" + cursor + "&limit=2")
	if !reflect.DeepEqual(last, []string{"a"}) || next != "" || more != "0" {
		t.Fatalf("last=%v next=%s more=%s", last, next, more)
	}
	all, next, more := get("")
	if len(all) != 3 || next != "" || more != "" {
		t.Fatalf("legacy=%v next=%s more=%s", all, next, more)
	}
	for _, query := range []string{"?limit=0", "?limit=201", "?before=broken", "?before=" + cursor + "&archived=true"} {
		r, err := http.Get(srv.URL + "/v1/conversations" + query)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != 400 {
			t.Fatalf("%s status=%d", query, r.StatusCode)
		}
	}
}
