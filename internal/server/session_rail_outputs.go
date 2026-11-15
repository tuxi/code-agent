package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"code-agent/internal/agent"
	"code-agent/internal/assetref"
	"code-agent/internal/conversation"
	"code-agent/internal/preview"
	"code-agent/internal/session"
	"code-agent/internal/tools"
)

type railOutputVersion struct {
	Evidence  string            `json:"evidence"`
	TurnID    string            `json:"turn_id"`
	CallID    string            `json:"call_id"`
	Sequence  int64             `json:"sequence"`
	UpdatedAt string            `json:"updated_at"`
	Operation string            `json:"operation"`
	Asset     *assets.Ref       `json:"asset,omitempty"`
	Artifact  *preview.Artifact `json:"artifact,omitempty"`
}
type railOutput struct {
	Availability string            `json:"availability"`
	ID           string            `json:"output_id"`
	Title        string            `json:"title"`
	Kind         string            `json:"kind"`
	Path         string            `json:"relative_path,omitempty"`
	ContentMode  string            `json:"content_mode"`
	State        string            `json:"state"`
	Latest       railOutputVersion `json:"latest"`
	VersionCount int               `json:"version_count"`
	versions     []railOutputVersion
}
type railOutputs struct {
	Scope      string       `json:"scope"`
	Coverage   string       `json:"coverage"`
	Revision   string       `json:"revision"`
	TotalCount int          `json:"total_count"`
	Items      []railOutput `json:"items"`
}

func railEventsByKind(ctx context.Context, events conversation.ConversationEventStore, id string, kinds []string) ([]session.EventRecord, error) {
	if events == nil {
		return nil, errors.New("event_store_unavailable")
	}
	if indexed, ok := events.(session.EventKindStore); ok {
		return indexed.SessionEventsByKind(ctx, id, kinds)
	}
	return nil, errors.New("event_kind_query_required")
}
func collectRailOutputs(ctx context.Context, events conversation.ConversationEventStore, id, root string) (railOutputs, error) {
	out := railOutputs{Scope: "conversation_recorded_outputs", Coverage: "recorded_evidence", Items: []railOutput{}}
	records, err := railEventsByKind(ctx, events, id, []string{string(agent.EventToolFinished), string(agent.EventPreviewArtifact), string(agent.EventTurnFinished), string(agent.EventTurnFailed), string(agent.EventTurnCancelled)})
	if err != nil {
		return out, err
	}
	byID := map[string]railOutput{}
	seen := map[string]bool{}
	knownWrites := map[string]bool{}
	for _, rec := range records {
		if rec.Kind == string(agent.EventToolFinished) {
			if ev, ok := decodeStoredEvent(rec); ok {
				for _, write := range ev.FileWrites {
					knownWrites[ev.TurnID+"\x00"+write.Path] = true
				}
			}
		}
	}

	for _, rec := range records {
		ev, ok := decodeStoredEvent(rec)
		if !ok {
			return out, errors.New("output_evidence_unreadable")
		}
		evidence := "tool_write_report"
		kind := rec.Kind
		if (kind == string(agent.EventTurnFinished) || kind == string(agent.EventTurnFailed) || kind == string(agent.EventTurnCancelled)) && ev.DiffStat != nil {
			evidence = "turn_diffstat"
			kind = string(agent.EventToolFinished)
			ev.OutputEvidenceVersion = 0
			ev.CallID = ""
			ev.FileWrites = nil
			for _, write := range ev.DiffStat.Files {
				if !knownWrites[ev.TurnID+"\x00"+write.Path] {
					ev.FileWrites = append(ev.FileWrites, write)
				}
			}
			if len(ev.FileWrites) == 0 {
				continue
			}
		}
		if kind == string(agent.EventToolFinished) {
			if ev.OutputEvidenceVersion < 1 {
				out.Coverage = "legacy_incomplete"
			}
			for _, write := range ev.FileWrites {
				path := filepath.ToSlash(filepath.Clean(write.Path))
				if path == "." || filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, "../") {
					continue
				}
				switch write.Status {
				case tools.WriteAdded, tools.WriteModified, tools.WriteDeleted:
				default:
					continue
				}
				key := railRevision("file:" + id + ":" + path)
				unique := key + "\x00" + ev.TurnID + "\x00" + ev.CallID + "\x00" + write.Status
				if seen[unique] {
					continue
				}
				seen[unique] = true
				row, exists := byID[key]
				// A deletion alone does not turn a pre-existing input into an output.
				if !exists && write.Status == tools.WriteDeleted {
					continue
				}
				if !exists {
					row = railOutput{ID: key, Title: filepath.Base(path), Kind: assets.KindForMIME(assets.MIMEType(path)), Path: path, ContentMode: "live_workspace"}
				}
				version := railOutputVersion{Evidence: evidence, TurnID: ev.TurnID, CallID: ev.CallID, Sequence: rec.Seq, Operation: write.Status}
				if !rec.At.IsZero() {
					version.UpdatedAt = rec.At.UTC().Format(rfc3339Millis)
				}
				for i := range ev.Assets {
					if ev.Assets[i].WorkspaceRelativePath == path {
						ref := ev.Assets[i]
						version.Asset = &ref
						break
					}
				}
				row.State = "recorded"
				row.Availability = "not_checked"
				if write.Status == tools.WriteDeleted {
					row.State = "deleted"
				}
				row.Latest = version
				row.versions = append(row.versions, version)
				row.VersionCount = len(row.versions)
				byID[key] = row
			}
		} else if rec.Kind == string(agent.EventPreviewArtifact) && ev.Artifact != nil {
			artifact := ev.Artifact
			if artifact.SessionID != id || artifact.ArtifactID == "" || artifact.Revision < 1 {
				continue
			}
			key := railRevision("preview:" + artifact.ArtifactID)
			unique := key + ":" + strconv.FormatInt(artifact.Revision, 10)
			if seen[unique] {
				continue
			}
			seen[unique] = true
			row := byID[key]
			row.ID = key
			row.Kind = "preview"
			row.ContentMode = "immutable"
			row.Availability = "not_checked"
			version := railOutputVersion{Evidence: "preview_artifact", TurnID: artifact.TurnID, CallID: artifact.CallID, Sequence: rec.Seq, Operation: "generated", Artifact: artifact}
			if !rec.At.IsZero() {
				version.UpdatedAt = rec.At.UTC().Format(rfc3339Millis)
			}
			row.versions = append(row.versions, version)
			row.VersionCount = len(row.versions)
			if row.Latest.Artifact == nil || row.Latest.Artifact.Revision < artifact.Revision {
				row.Latest = version
				row.State = string(artifact.Status)
				row.Title = artifact.Title
			}
			byID[key] = row
		}
	}
	for _, row := range byID {
		out.Items = append(out.Items, row)
	}
	sort.Slice(out.Items, func(i, j int) bool {
		if out.Items[i].Latest.Sequence != out.Items[j].Latest.Sequence {
			return out.Items[i].Latest.Sequence > out.Items[j].Latest.Sequence
		}
		return out.Items[i].ID < out.Items[j].ID
	})
	out.TotalCount = len(out.Items)
	out.Revision = railRevision(out.Items)
	return out, nil
}

type railOutputCursor struct {
	Conversation, Workspace, Revision string
	Offset                            int
}

func registerRailOutputRoutes(mux *http.ServeMux, repo conversation.ConversationRepository, events conversation.ConversationEventStore) {
	load := func(w http.ResponseWriter, r *http.Request) (railOutputs, string, bool) {
		sess, err := loadConversationHeader(r.Context(), repo, r.PathValue("id"))
		if err != nil {
			ErrorSimple(w, r, 404, "conversation not found")
			return railOutputs{}, "", false
		}
		if rebind, e := repo.NeedsRebind(r.Context(), sess.ID); e != nil || rebind {
			ErrorSimple(w, r, 409, "workspace unavailable")
			return railOutputs{}, "", false
		}
		data, err := collectRailOutputs(r.Context(), events, sess.ID, sess.WorkspacePath)
		if err != nil {
			ErrorSimple(w, r, 503, "output evidence unavailable")
			return data, "", false
		}
		w.Header().Set("Cache-Control", "no-store")
		return data, railRevision(sess.WorkspacePath), true
	}
	mux.HandleFunc("GET /v1/conversations/{id}/outputs", func(w http.ResponseWriter, r *http.Request) {
		limit := 50
		if values, exists := r.URL.Query()["limit"]; exists {
			n, e := strconv.Atoi(r.URL.Query().Get("limit"))
			if len(values) != 1 || e != nil || n < 1 || n > 100 {
				ErrorSimple(w, r, 400, "invalid limit")
				return
			}
			limit = n
		}
		data, workspace, ok := load(w, r)
		if !ok {
			return
		}
		offset := 0
		if values, exists := r.URL.Query()["cursor"]; exists {
			raw := r.URL.Query().Get("cursor")
			b, e := base64.RawURLEncoding.DecodeString(raw)
			var c railOutputCursor
			if len(values) != 1 || raw == "" || len(raw) > 4096 || e != nil || json.Unmarshal(b, &c) != nil || c.Conversation != r.PathValue("id") || c.Workspace != workspace || c.Offset < 0 || c.Offset > len(data.Items) {
				ErrorSimple(w, r, 400, "invalid cursor")
				return
			}
			if c.Revision != data.Revision {
				ErrorSimple(w, r, 409, "output directory changed")
				return
			}
			offset = c.Offset
		}
		end := min(offset+limit, len(data.Items))
		next := ""
		if end < len(data.Items) {
			b, _ := json.Marshal(railOutputCursor{r.PathValue("id"), workspace, data.Revision, end})
			next = base64.RawURLEncoding.EncodeToString(b)
		}
		data.Items = data.Items[offset:end]
		writeJSON(w, r, 200, struct {
			railOutputs
			NextCursor string `json:"next_cursor,omitempty"`
		}{data, next})
	})
	mux.HandleFunc("GET /v1/conversations/{id}/outputs/{output_id}", func(w http.ResponseWriter, r *http.Request) {
		data, _, ok := load(w, r)
		if !ok {
			return
		}
		for _, row := range data.Items {
			if row.ID == r.PathValue("output_id") {
				writeJSON(w, r, 200, struct {
					railOutput
					Versions []railOutputVersion `json:"versions"`
				}{row, row.versions})
				return
			}
		}
		ErrorSimple(w, r, 404, "output not found")
	})
}
