// Package taskreplay serves immutable offline test output, without a Runtime or Store.
package taskreplay

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"path"
	"reflect"
	"strconv"
	"strings"

	"code-agent/internal/collab"
	"code-agent/internal/server"
)

const MaxFixtureBytes = 24 << 20

type fixture struct {
	FixtureOnly bool                           `json:"fixture_only"`
	Snapshot    collab.Snapshot                `json:"snapshot"`
	Page        collab.EventPage               `json:"event_page"`
	Bundles     map[string]collab.ReviewBundle `json:"bundles"`
	Blobs       map[string][]byte              `json:"bundle_blobs"`
}

type Host struct {
	data  fixture
	token string
}

// New copies and validates the complete export before making any data available.
// Require the original blob bytes/hash, rather than a re-encoded API DTO hash.
func New(raw []byte, token string) (*Host, error) {
	if len(raw) > MaxFixtureBytes || len(token) < 32 || strings.ContainsAny(token, " \r\n\t") {
		return nil, errors.New("invalid replay input or credential")
	}
	var f fixture
	if json.Unmarshal(raw, &f) != nil || !f.FixtureOnly || f.Snapshot.ManagedRun == nil {
		return nil, errors.New("expected offline managed export")
	}
	if err := validate(f); err != nil {
		return nil, err
	}
	return &Host{data: f, token: token}, nil
}

func decimal(s string) (*big.Int, bool) {
	if s == "" {
		return nil, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return nil, false
		}
	}
	return new(big.Int).SetString(s, 10)
}

func validate(f fixture) error {
	bad := errors.New("inconsistent offline Task export")
	s, p := f.Snapshot, f.Page
	r := s.ManagedRun
	watermark, ok := decimal(s.SnapshotTaskSeq)
	if !ok || s.Schema != "task-snapshot/v1" || s.TaskID == "" || r.TaskID != s.TaskID || r.ServerID == "" || r.StoreID == "" ||
		p.Schema != "task-event-page/v1" || p.TaskID != s.TaskID || p.After != "0" || p.HasMore || p.ResyncRequired || p.NextCursor != s.SnapshotTaskSeq {
		return bad
	}
	if _, ok := decimal(s.Revision); !ok {
		return bad
	}
	previous := big.NewInt(0)
	ids := map[string]bool{}
	for _, e := range p.Events {
		n, ok := decimal(e.TaskSeq)
		if !ok || n.Cmp(previous) <= 0 || n.Cmp(watermark) > 0 || e.TaskID != s.TaskID || e.EventID == "" || ids[e.EventID] {
			return bad
		}
		ids[e.EventID] = true
		previous = n
	}
	if previous.Cmp(watermark) != 0 {
		return bad
	}
	refs := map[string]collab.TaskArtifactRef{}
	for _, a := range s.ArtifactRefs {
		if a.Kind != "review_bundle" {
			continue
		}
		b, ok := f.Bundles[a.ArtifactID]
		raw := f.Blobs[a.ArtifactID]
		if !ok || a.ContentHash == nil || a.Truncated || a.CaptureMode != "immutable" {
			return errors.New("invalid immutable bundle reference")
		}
		if *a.ContentHash != collab.PayloadHash(raw) {
			return errors.New("missing or corrupt original bundle bytes")
		}
		api, err := json.Marshal(b)
		var stored, exported any
		if err != nil || json.Unmarshal(raw, &stored) != nil || json.Unmarshal(api, &exported) != nil || !reflect.DeepEqual(stored, exported) {
			return errors.New("bundle DTO differs from original blob")
		}
		if b.Schema != "coding_review_bundle_v1" || b.AttemptID != a.AttemptID || b.TurnID != a.SourceRef.TurnID || b.EventCursor != a.SourceRef.Seq ||
			b.Source.TaskID != s.TaskID || b.Source.RunID != r.RunID || b.Source.ServerID != r.ServerID || b.Source.StoreID != r.StoreID ||
			b.Plan.ArtifactID != r.PlanArtifactID || b.Plan.Hash != r.PlanHash || b.Plan.Version != r.PlanVersion || a.Version != strconv.Itoa(b.Iteration) {
			return bad
		}
		bound := false
		for _, attempt := range s.Attempts {
			if attempt.AttemptID == b.AttemptID && attempt.TaskID == s.TaskID && attempt.SourceRef.TurnID == b.TurnID && attempt.Iteration == b.Iteration {
				bound = true
			}
		}
		if !bound {
			return bad
		}
		if _, duplicate := refs[a.ArtifactID]; duplicate {
			return bad
		}
		refs[a.ArtifactID] = a
	}
	if len(refs) == 0 || len(refs) != len(f.Bundles) || len(refs) != len(f.Blobs) {
		return bad
	}
	for _, e := range p.Events {
		if e.Kind != "review.recorded" {
			continue
		}
		var d collab.GateDecision
		if json.Unmarshal(e.Payload, &d) != nil {
			return bad
		}
		ref, ok := refs[d.Review.Artifact.ArtifactID]
		b := f.Bundles[d.Review.Artifact.ArtifactID]
		if !ok || d.Review.Artifact.Hash != *ref.ContentHash || d.Review.Artifact.Version != ref.Version || d.Review.AttemptID != b.AttemptID ||
			d.Review.RunID != b.Source.RunID || d.Review.Epoch != b.Source.Epoch || d.Review.Plan != b.Plan {
			return bad
		}
	}
	return nil
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data any    `json:"data"`
	}{0, "success", value})
}

func reject(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": status * 100, "msg": http.StatusText(status)})
}

func (h *Host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-CodeAgent-Replay", "offline-read-only")
	// AgentKit probes health without a credential; disclose no identity or data.
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" && r.URL.RawPath == "" && r.Header.Get("Upgrade") == "" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+h.token)) != 1 {
		reject(w, http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet || r.Header.Get("Upgrade") != "" {
		reject(w, http.StatusForbidden)
		return
	}
	if path.Clean(r.URL.Path) != r.URL.Path || r.URL.RawPath != "" {
		reject(w, http.StatusBadRequest)
		return
	}
	f, s := h.data, h.data.Snapshot
	identity := collab.Identity{ServerID: s.ManagedRun.ServerID, StoreID: s.ManagedRun.StoreID}
	switch r.URL.Path {
	case "/healthz":
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	case "/v1/runtime/info":
		reply(w, 200, server.RuntimeInfo{Schema: "runtime-info/v1", ServerID: identity.ServerID, DisplayName: "P0.5 Offline Replay (read-only)", Product: "codeagent", RuntimeVersion: "offline-replay/v1", AgentWireProtocol: server.AgentWireProtocolInfo{Major: 1, Revision: "offline-replay/v1"}, RuntimeProfile: server.RuntimeProfileHeadless})
	case "/v1/runtime/capabilities":
		reply(w, 200, struct {
			Capabilities server.RuntimeCapabilities `json:"capabilities"`
		}{server.RuntimeCapabilities{TaskTimeline: true}})
	case "/v1/runtime/models":
		reply(w, 200, server.RuntimeModelCatalog{Schema: "runtime-model-catalog/v2", Revision: 1, VerificationScope: "offline_replay_no_execution", Connections: []server.RuntimeModelConnection{}})
	case "/v1/tasks/capabilities":
		reply(w, 200, struct {
			collab.Identity
			Capabilities []string `json:"capabilities"`
			Projection   string   `json:"projection"`
		}{identity, []string{"task_timeline_v1"}, "offline_replay"})
	case "/v1/tasks":
		if !validLimit(r) {
			reject(w, 400)
			return
		}
		tasks := []collab.CodingTask{}
		if filter := r.URL.Query().Get("workspace_ref"); filter == "" || filter == s.WorkspaceRef {
			tasks = append(tasks, s.CodingTask)
		}
		reply(w, 200, tasks)
	case "/v1/tasks/" + s.TaskID:
		if scope := r.URL.Query().Get("scope_workspace_ref"); scope != "" && scope != s.WorkspaceRef {
			reject(w, 403)
			return
		}
		if scope := r.URL.Query().Get("scope_session_id"); scope != "" {
			for _, p := range s.Participations {
				if p.SessionRef.Kind == "local" && p.SessionRef.SessionID != scope {
					reject(w, 403)
					return
				}
			}
		}
		reply(w, 200, s)
	case "/v1/tasks/" + s.TaskID + "/events":
		q := r.URL.Query()
		after := q.Get("after")
		if _, present := q["after"]; !present {
			after = "0"
		}
		n, ok := decimal(after)
		end, _ := decimal(s.SnapshotTaskSeq)
		if !ok || n.Cmp(end) > 0 || len(q["after"]) > 1 || !validLimit(r) {
			reject(w, 400)
			return
		}
		limit := 200
		if q.Get("limit") != "" {
			limit, _ = strconv.Atoi(q.Get("limit"))
		}
		p := collab.EventPage{Schema: f.Page.Schema, TaskID: s.TaskID, After: after, NextCursor: after, Events: []collab.TaskEvent{}}
		for _, e := range f.Page.Events {
			seq, _ := decimal(e.TaskSeq)
			if seq.Cmp(n) <= 0 {
				continue
			}
			if len(p.Events) == limit {
				p.HasMore = true
				break
			}
			p.Events = append(p.Events, e)
			p.NextCursor = e.TaskSeq
		}
		reply(w, 200, p)
	default:
		prefix := "/v1/tasks/" + s.TaskID + "/immutable-artifacts/"
		if strings.HasPrefix(r.URL.Path, prefix) {
			if raw, ok := f.Blobs[strings.TrimPrefix(r.URL.Path, prefix)]; ok {
				reply(w, 200, json.RawMessage(raw))
				return
			}
		}
		reject(w, 404)
	}
}

func validLimit(r *http.Request) bool {
	v, ok := r.URL.Query()["limit"]
	if !ok {
		return true
	}
	if len(v) != 1 {
		return false
	}
	n, err := strconv.Atoi(v[0])
	return err == nil && n >= 1 && n <= 200
}
