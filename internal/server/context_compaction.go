package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"code-agent/internal/conversation"
)

func registerContextCompaction(mux *http.ServeMux, repo conversation.ConversationRepository, executor *conversation.TurnExecutor) {
	mux.HandleFunc("POST /v1/conversations/{id}/compact", func(w http.ResponseWriter, r *http.Request) {
		if executor == nil {
			http.Error(w, "context compaction unavailable", http.StatusNotImplemented)
			return
		}
		id := r.PathValue("id")
		if _, err := repo.Load(r.Context(), id); err != nil {
			http.Error(w, "conversation not found", http.StatusNotFound)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		sess, changed, err := executor.CompactConversation(ctx, id)
		if err != nil {
			if writeConversationOperationError(w, r, id, err) {
				return
			}
			if errors.Is(err, conversation.ErrCompactionUnsupported) {
				http.Error(w, err.Error(), http.StatusNotImplemented)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		status := "no_change"
		if changed {
			status = "compacted_pending_measurement"
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"status": status, "snapshot": contextSnapshotFromSession(sess)})
	})
}
