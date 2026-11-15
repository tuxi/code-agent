package server

import (
	"code-agent/internal/agent"
	"net/http"

	"code-agent/internal/conversation"
	runtimepkg "code-agent/internal/runtime"
)

// Both daemon and embedded hosts already install the workspace registry as the
// prompt owner. The optional read port observes that same owner, never a second
// manager with its own subprocesses or configuration policy.
func registerRailConnectorRoutes(mux *http.ServeMux, repo conversation.ConversationRepository, opts MuxOptions, executor *conversation.TurnExecutor, events conversation.ConversationEventStore) {
	reader, ok := opts.Prompts.(interface {
		ConnectorSnapshot(string) runtimepkg.ConnectorSnapshot
	})
	if !ok {
		return
	}
	load := func(w http.ResponseWriter, r *http.Request) (runtimepkg.ConnectorSnapshot, bool) {
		sess, err := loadConversationHeader(r.Context(), repo, r.PathValue("id"))
		if err != nil {
			ErrorSimple(w, r, 404, "conversation not found")
			return runtimepkg.ConnectorSnapshot{}, false
		}
		if rebind, e := repo.NeedsRebind(r.Context(), sess.ID); e != nil {
			ErrorSimple(w, r, 503, "workspace unavailable")
			return runtimepkg.ConnectorSnapshot{}, false
		} else if rebind {
			ErrorSimple(w, r, 409, "workspace needs rebind")
			return runtimepkg.ConnectorSnapshot{}, false
		}
		w.Header().Set("Cache-Control", "no-store")
		data := collectRailConnectors(reader, executor, sess.ID, sess.WorkspacePath)
		if requests, e := readRailContextRequests(r.Context(), events, sess.ID); e == nil {
			applyRailRequestExposure(&data, requests.accepted)
		}
		return data, true
	}
	mux.HandleFunc("GET /v1/conversations/{id}/connectors", func(w http.ResponseWriter, r *http.Request) {
		if data, ok := load(w, r); ok {
			writeJSON(w, r, 200, data)
		}
	})
	mux.HandleFunc("GET /v1/conversations/{id}/connectors/{connector_id}", func(w http.ResponseWriter, r *http.Request) {
		data, ok := load(w, r)
		if !ok {
			return
		}
		for _, row := range data.Items {
			if row.ID == r.PathValue("connector_id") {
				writeJSON(w, r, 200, row)
				return
			}
		}
		if data.State == "loading" || data.State == "unavailable" {
			ErrorSimple(w, r, 503, "connector inventory unavailable")
			return
		}
		ErrorSimple(w, r, 404, "connector not found")
	})
}

func collectRailConnectors(reader interface {
	ConnectorSnapshot(string) runtimepkg.ConnectorSnapshot
}, executor *conversation.TurnExecutor, sessionID, root string) runtimepkg.ConnectorSnapshot {
	data := reader.ConnectorSnapshot(root)
	if executor == nil {
		return data
	}
	data.ClientCapabilitiesState = "registered_tools_only"
	for _, tool := range executor.RegisteredClientTools(sessionID) {
		one := 1
		data.Items = append(data.Items, runtimepkg.Connector{ID: "client:" + tool.Name, Kind: "client_capability", Name: tool.Name, Source: "session_client", Status: "unknown", Reason: "registration_does_not_imply_permission", Health: "not_probed", RegisteredToolCount: &one, Tools: []runtimepkg.ConnectorTool{{Name: tool.Name, Description: tool.Description, Registered: true}}})
	}
	if data.State == "unavailable" && len(data.Items) > 0 {
		data.State = "stale"
	}
	if data.State == "empty" && len(data.Items) > 0 {
		data.State = "ready"
	}
	return data
}

func applyRailRequestExposure(data *runtimepkg.ConnectorSnapshot, snapshot *agent.RequestContextSnapshot) {
	if snapshot == nil {
		return
	}
	for _, tool := range snapshot.Tools {
		if tool.Origin == "" || tool.Origin == "unknown" {
			return
		}
	}
	for i := range data.Items {
		row := &data.Items[i]
		count := 0
		for _, tool := range snapshot.Tools {
			if row.Kind == "mcp" && tool.Origin == "mcp" && tool.ConnectorID == row.ID {
				count++
			}
			if row.Kind == "client_capability" && row.ID == "client:"+tool.Name && tool.Executor == "client" {
				count++
			}
		}
		row.ExposedToolCount = &count
		row.ExposureSnapshotID = snapshot.SnapshotID
	}
}
