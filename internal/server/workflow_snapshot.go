package server

import (
	"net/http"

	"code-agent/internal/conversation"
	runtime "code-agent/internal/runtime"
)

// workflowSnapshotHandler returns an HTTP handler that loads the conversation's
// workspace root and delegates to the snapshot func.
func workflowSnapshotHandler(repo conversation.ConversationRepository, snapshotFunc runtime.WorkflowSnapshotFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		convID := r.PathValue("id")
		workflowID := r.PathValue("workflow_id")
		if convID == "" || workflowID == "" {
			http.Error(w, "missing id or workflow_id", http.StatusBadRequest)
			return
		}

		sess, err := repo.Load(r.Context(), convID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}

		snapshot, err := snapshotFunc(r.Context(), sess.WorkspacePath, workflowID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if snapshot == nil {
			http.Error(w, "workflow not found", http.StatusNotFound)
			return
		}

		writeJSON(w, r, http.StatusOK, snapshot)
	}
}
