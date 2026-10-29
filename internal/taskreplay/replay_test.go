package taskreplay

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"code-agent/internal/collab"
)

const testToken = "test-replay-credential-0123456789abcdef"

func load(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../docs/contracts/p05-replay-export.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func host(t *testing.T) *Host {
	t.Helper()
	h, err := New(load(t), testToken)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func get(h *Host, method, path, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder, dest any) {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(envelope.Data, dest); err != nil {
		t.Fatal(err)
	}
}

func TestRealExportIdentityHistoryAndFrozenVersions(t *testing.T) {
	h := host(t)
	var s collab.Snapshot
	decode(t, get(h, "GET", "/v1/tasks/"+h.data.Snapshot.TaskID, testToken), &s)
	if s.TaskAcceptance != "accepted" || s.ManagedRun.StoreID != h.data.Snapshot.ManagedRun.StoreID {
		t.Fatal("snapshot changed")
	}
	base := "/v1/tasks/" + s.TaskID
	cursor := "0"
	count := 0
	reviews := map[string]bool{}
	for {
		var p collab.EventPage
		decode(t, get(h, "GET", base+"/events?after="+cursor+"&limit=2", testToken), &p)
		if p.After != cursor {
			t.Fatal("cursor reused across pages")
		}
		count += len(p.Events)
		for _, e := range p.Events {
			if e.Kind == "review.recorded" {
				var d collab.GateDecision
				_ = json.Unmarshal(e.Payload, &d)
				reviews[d.Review.Verdict] = true
			}
		}
		cursor = p.NextCursor
		if !p.HasMore {
			break
		}
	}
	if count != len(h.data.Page.Events) || cursor != s.SnapshotTaskSeq || !reviews["pass"] || !reviews["request_changes"] {
		t.Fatal("lost history")
	}
	var again collab.EventPage
	decode(t, get(h, "GET", base+"/events?after="+cursor, testToken), &again)
	if len(again.Events) != 0 || again.NextCursor != cursor {
		t.Fatal("duplicate history")
	}
	candidates := map[int]string{}
	for id, want := range h.data.Bundles {
		var b collab.ReviewBundle
		decode(t, get(h, "GET", base+"/immutable-artifacts/"+id, testToken), &b)
		if b.AttemptID != want.AttemptID || b.Candidate.Hash() != want.Candidate.Hash() {
			t.Fatal("wrong round")
		}
		candidates[b.Iteration] = b.Candidate.Hash()
	}
	if len(candidates) != 2 || candidates[1] == candidates[2] {
		t.Fatal("historical candidates collapsed")
	}
}

func TestAllWritesAndExecutionRoutesDenied(t *testing.T) {
	h := host(t)
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "CONNECT"} {
		for _, p := range []string{"/v1/tasks", "/v1/tasks/x/runs", "/v1/tasks/x/runs/r/reviews", "/v1/sessions/x/send", "/v1/approvals/x", "/v1/recovery", "/v1/secrets", "/healthz"} {
			if w := get(h, method, p, testToken); w.Code != 403 {
				t.Fatalf("%s %s: %d", method, p, w.Code)
			}
		}
	}
	for _, p := range []string{"/v1/sessions/x", "/v1/ws", "/v1/tasks/not-this-task", "/v1/tasks/x/immutable-artifacts/y"} {
		if get(h, "GET", p, testToken).Code != 404 {
			t.Fatal("read escaped allowlist", p)
		}
	}
	r := httptest.NewRequest("GET", "/v1/tasks", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("Upgrade", "websocket")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("websocket allowed")
	}
}

func TestAuthenticationCapabilitiesAndPreflight(t *testing.T) {
	h := host(t)
	if w := get(h, "GET", "/healthz", ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "ok" {
		t.Fatal("anonymous health preflight")
	}
	for _, token := range []string{"", "wrong-token"} {
		if get(h, "GET", "/v1/tasks", token).Code != 401 {
			t.Fatal("unauthorized read")
		}
	}
	for _, p := range []string{"/healthz", "/v1/runtime/info", "/v1/runtime/capabilities", "/v1/runtime/models", "/v1/tasks/capabilities", "/v1/tasks"} {
		if get(h, "GET", p, testToken).Code != 200 {
			t.Fatal("preflight unavailable", p)
		}
	}
	var c struct {
		Capabilities []string `json:"capabilities"`
	}
	decode(t, get(h, "GET", "/v1/tasks/capabilities", testToken), &c)
	if strings.Join(c.Capabilities, ",") != "task_timeline_v1" {
		t.Fatal("advertised writes", c)
	}
	if get(h, "GET", "/v1/tasks/"+h.data.Snapshot.TaskID+"?scope_workspace_ref=other", testToken).Code != 403 {
		t.Fatal("scope bypass")
	}
}

func TestRejectedCorruptionAndNoAcceptanceInference(t *testing.T) {
	mutations := map[string]func(*fixture){
		"not offline": func(f *fixture) { f.FixtureOnly = false },
		"cursor":      func(f *fixture) { f.Page.NextCursor = "999" },
		"event task":  func(f *fixture) { f.Page.Events[0].TaskID = "other" },
		"event order": func(f *fixture) { f.Page.Events[1], f.Page.Events[0] = f.Page.Events[0], f.Page.Events[1] },
		"candidate": func(f *fixture) {
			for id, b := range f.Bundles {
				b.Candidate.Files[0].Data = []byte("tampered")
				f.Bundles[id] = b
				break
			}
		},
		"binding": func(f *fixture) {
			for i := range f.Snapshot.ArtifactRefs {
				if f.Snapshot.ArtifactRefs[i].Kind == "review_bundle" {
					f.Snapshot.ArtifactRefs[i].Version = "999"
					break
				}
			}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			var f fixture
			_ = json.Unmarshal(load(t), &f)
			mutate(&f)
			b, _ := json.Marshal(f)
			if _, err := New(b, testToken); err == nil {
				t.Fatal("accepted corrupt export")
			}
		})
	}
	var f fixture
	_ = json.Unmarshal(load(t), &f)
	f.Snapshot.TaskAcceptance = "not_reviewed"
	b, _ := json.Marshal(f)
	h, err := New(b, testToken)
	if err != nil {
		t.Fatal(err)
	}
	var s collab.Snapshot
	decode(t, get(h, "GET", "/v1/tasks/"+f.Snapshot.TaskID, testToken), &s)
	if s.TaskAcceptance != "not_reviewed" {
		t.Fatal("derived acceptance from PASS")
	}
}

func TestInvalidCursorsAndReadScope(t *testing.T) {
	h := host(t)
	base := "/v1/tasks/" + h.data.Snapshot.TaskID + "/events?"
	for _, query := range []string{"after=", "after=-1", "after=abc", "after=99999999999999999999", "after=1&after=2", "limit=0", "limit=201", "limit=1&limit=2"} {
		if get(h, "GET", base+query, testToken).Code != 400 {
			t.Fatal("invalid query", query)
		}
	}
	var tasks []collab.CodingTask
	decode(t, get(h, "GET", "/v1/tasks?workspace_ref=other", testToken), &tasks)
	if len(tasks) != 0 {
		t.Fatal("cross workspace")
	}
	for i := 0; i < 3; i++ {
		if get(h, "GET", base+"limit="+strconv.Itoa(i+1), testToken).Code != 200 {
			t.Fatal("valid limit")
		}
	}
}
