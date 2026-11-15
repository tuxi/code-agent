package server

import (
	"context"
	"net/http"
	"strings"

	"code-agent/internal/agent"
	"code-agent/internal/conversation"
	"code-agent/internal/session"
)

type railContextRequests struct {
	latest      *agent.RequestContextSnapshot
	accepted    *agent.RequestContextSnapshot
	snapshots   map[string]*agent.RequestContextSnapshot
	measurement *agent.Event
	measuredAt  string
}

func readRailContextRequests(ctx context.Context, events conversation.ConversationEventStore, id string) (railContextRequests, error) {
	out := railContextRequests{snapshots: map[string]*agent.RequestContextSnapshot{}}
	records, err := railEventsByKind(ctx, events, id, []string{string(agent.EventModelRequest), string(agent.EventModelFinished)})
	if err != nil {
		return out, err
	}
	for _, rec := range records {
		ev, ok := decodeStoredEvent(rec)
		if !ok {
			continue
		}
		if rec.Kind == string(agent.EventModelRequest) && ev.ContextSnapshot != nil && ev.InvocationID != "" && ev.ContextSnapshot.SnapshotID == ev.InvocationID {
			out.snapshots[ev.InvocationID] = ev.ContextSnapshot
			out.latest = ev.ContextSnapshot
		}
		if rec.Kind == string(agent.EventModelFinished) && ev.ModelSucceeded != nil && *ev.ModelSucceeded && out.snapshots[ev.InvocationID] != nil {
			out.accepted = out.snapshots[ev.InvocationID]
			if ev.PromptTokens <= 0 {
				continue
			}
			copy := ev
			out.measurement = &copy
			if !rec.At.IsZero() {
				out.measuredAt = rec.At.UTC().Format(rfc3339Millis)
			}
		}
	}
	return out, nil
}
func extendRailContext(ctx context.Context, base railContext, sess *session.Session, events conversation.ConversationEventStore) railContext {
	requests, err := readRailContextRequests(ctx, events, sess.ID)
	if err != nil {
		return base
	}
	base.exposureRequest = requests.accepted
	base.Request = requests.latest
	if requests.latest != nil {
		base.LatestRequestSnapshotID = requests.latest.SnapshotID
	}

	if ev := requests.measurement; ev != nil {
		n := ev.PromptTokens
		base.PromptTokens = &n
		base.MeasurementState = "provider_reported"
		base.MeasurementSnapshotID = ev.InvocationID
		base.MeasurementModel = requests.snapshots[ev.InvocationID].Model
		base.MeasurementWindow = requests.snapshots[ev.InvocationID].ContextWindow
		base.Request = requests.snapshots[ev.InvocationID]
		if requests.measuredAt != "" {
			at := requests.measuredAt
			base.MeasuredAt = &at
		}
	}
	if base.Request != nil {
		base.BreakdownState = "estimated"
		if base.Request.SourceCoverage != "recorded" {
			base.BreakdownState = "partially_attributed"
		}
		base.HistoryState = "unchanged_history"
		if base.Request.HistoryRevision != session.HistoryRevision(sess.Messages) {
			base.HistoryState = "history_changed"
		}
	}
	return base
}
func registerRailContextRoutes(mux *http.ServeMux, repo conversation.ConversationRepository, events conversation.ConversationEventStore) {
	load := func(w http.ResponseWriter, r *http.Request) (*session.Session, *agent.RequestContextSnapshot, bool) {
		sess, err := repo.Load(r.Context(), r.PathValue("id"))
		if err != nil {
			ErrorSimple(w, r, 404, "conversation not found")
			return nil, nil, false
		}
		requests, err := readRailContextRequests(r.Context(), events, sess.ID)
		if err != nil {
			ErrorSimple(w, r, 503, "context provenance unavailable")
			return nil, nil, false
		}
		selected := requests.latest
		if id := r.URL.Query().Get("snapshot_id"); id != "" {
			selected = requests.snapshots[id]
		}
		if selected == nil {
			ErrorSimple(w, r, 404, "request snapshot not found")
			return nil, nil, false
		}
		w.Header().Set("Cache-Control", "no-store")
		return sess, selected, true
	}
	mux.HandleFunc("GET /v1/conversations/{id}/context/request", func(w http.ResponseWriter, r *http.Request) {
		if _, snapshot, ok := load(w, r); ok {
			writeJSON(w, r, 200, snapshot)
		}
	})
	mux.HandleFunc("GET /v1/conversations/{id}/context/sources", func(w http.ResponseWriter, r *http.Request) {
		_, snapshot, ok := load(w, r)
		if !ok {
			return
		}
		items := []agent.ContextSourceInfo{}
		kind := r.URL.Query().Get("kind")
		if kind != "" && kind != "memory" && kind != "skill" && kind != "system" {
			ErrorSimple(w, r, 400, "invalid source kind")
			return
		}
		for _, source := range snapshot.Sources {
			if kind == "" || (kind == "memory" && source.Kind == "memory_files") || (kind == "skill" && strings.HasPrefix(source.Kind, "skills")) || (kind == "system" && source.Kind == "system_prompt") {
				items = append(items, source)
			}
		}
		writeJSON(w, r, 200, map[string]any{"snapshot_id": snapshot.SnapshotID, "sources": items})
	})
	mux.HandleFunc("GET /v1/conversations/{id}/context/tools", func(w http.ResponseWriter, r *http.Request) {
		_, snapshot, ok := load(w, r)
		if !ok {
			return
		}
		origin := r.URL.Query().Get("origin")
		if origin != "" && origin != "mcp" && origin != "system" && origin != "unknown" {
			ErrorSimple(w, r, 400, "invalid origin")
			return
		}
		items := []agent.ContextToolInfo{}
		for _, tool := range snapshot.Tools {
			if origin == "" || tool.Origin == origin {
				items = append(items, tool)
			}
		}
		writeJSON(w, r, 200, map[string]any{"snapshot_id": snapshot.SnapshotID, "tools": items})
	})
	mux.HandleFunc("GET /v1/conversations/{id}/context/sources/{source_id}/content", func(w http.ResponseWriter, r *http.Request) {
		sess, snapshot, ok := load(w, r)
		if !ok {
			return
		}
		id := r.PathValue("source_id")
		allowed := false
		for _, source := range snapshot.Sources {
			if source.ID == id {
				allowed = true
			}
		}
		for _, tool := range snapshot.Tools {
			if tool.SourceID == id {
				allowed = true
			}
		}
		if !allowed {
			ErrorSimple(w, r, 404, "source not in request snapshot")
			return
		}
		source, exists := session.ContextCatalog(sess)[id]
		if !exists {
			ErrorSimple(w, r, 410, "source version unavailable")
			return
		}
		if session.ContentHash(source.Content) != source.SHA256 {
			ErrorSimple(w, r, 503, "source version integrity error")
			return
		}
		writeJSON(w, r, 200, map[string]any{"source_id": source.ID, "scope": source.Scope, "version": source.Version, "kind": source.Kind, "label": source.Label, "display_path": source.DisplayPath, "sha256": source.SHA256, "content": source.Content})
	})
}
