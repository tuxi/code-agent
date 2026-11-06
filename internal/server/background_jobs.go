package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"code-agent/internal/conversation"
	"code-agent/internal/jobs"
	"code-agent/internal/truncate"
)

const backgroundJobControlCapability = "background_job_control_v1"

// A control snapshot is terminal only after process cleanup and the terminal
// event have completed. Registry.Cancel's immediate marker is only a request.
type backgroundJobView struct {
	JobID           string      `json:"job_id"`
	SessionID       string      `json:"session_id"`
	TurnID          string      `json:"turn_id"`
	CallID          string      `json:"call_id"`
	Command         string      `json:"command"`
	Status          jobs.Status `json:"status"`
	ExitCode        *int        `json:"exit_code,omitempty"`
	DurationMS      int64       `json:"duration_ms"`
	CancelRequested bool        `json:"cancel_requested"`
	OutputTail      string      `json:"output_tail"`
}

func backgroundJobSnapshot(job *jobs.Job) backgroundJobView {
	finished := false
	select {
	case <-job.Done():
		finished = true
	default:
	}
	snap := job.Snapshot()
	view := backgroundJobView{JobID: snap.ID, SessionID: snap.Owner.SessionID,
		TurnID: snap.Owner.TurnID, CallID: snap.Owner.CallID,
		Command: snap.Command, Status: snap.Status, DurationMS: snap.DurationMS,
		CancelRequested: snap.Status == jobs.Canceled, OutputTail: truncate.Tail(job.Logs(), 2000)}
	if finished {
		view.ExitCode = &snap.ExitCode
	} else {
		view.Status = jobs.Running
	}
	return view
}

func registerBackgroundJobRoutes(mux *http.ServeMux, repo conversation.ConversationRepository, registry *jobs.Registry) {
	if registry == nil {
		return
	}
	knownConversation := func(w http.ResponseWriter, r *http.Request) bool {
		if repo == nil {
			ErrorSimple(w, r, http.StatusNotFound, "conversation not found")
			return false
		}
		if _, err := repo.Load(r.Context(), r.PathValue("id")); err != nil {
			ErrorSimple(w, r, http.StatusNotFound, "conversation not found")
			return false
		}
		return true
	}
	mux.HandleFunc("GET /v1/conversations/{id}/jobs", func(w http.ResponseWriter, r *http.Request) {
		if !knownConversation(w, r) {
			return
		}
		active, recent := []backgroundJobView{}, []backgroundJobView{}
		for _, snap := range registry.List() {
			if snap.Owner.SessionID != r.PathValue("id") {
				continue
			}
			job, ok := registry.Get(snap.ID)
			if !ok {
				continue
			}
			view := backgroundJobSnapshot(job)
			if view.Status == jobs.Running {
				active = append(active, view)
			} else {
				recent = append(recent, view)
			}
		}
		// Keep every active job, but do not resend an unbounded finished history.
		if len(recent) > 20 {
			recent = recent[len(recent)-20:]
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"jobs": append(active, recent...)})
	})
	mux.HandleFunc("POST /v1/conversations/{id}/jobs/{jobID}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if !knownConversation(w, r) {
			return
		}
		var in struct {
			TurnID string `json:"turn_id"`
			CallID string `json:"call_id"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&in); err != nil {
			ErrorSimple(w, r, http.StatusBadRequest, "invalid cancellation identity")
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF || strings.TrimSpace(in.TurnID) == "" || strings.TrimSpace(in.CallID) == "" {
			ErrorSimple(w, r, http.StatusBadRequest, "turn_id and call_id are required")
			return
		}
		job, ok := registry.Get(r.PathValue("jobID"))
		if !ok {
			ErrorSimple(w, r, http.StatusNotFound, "job not found")
			return
		}
		snap := job.Snapshot()
		if snap.Owner.SessionID != r.PathValue("id") || snap.Owner.TurnID != in.TurnID || snap.Owner.CallID != in.CallID {
			ErrorSimple(w, r, http.StatusNotFound, "job not found")
			return
		}
		// Cancellation and natural completion can race. Both are safe to repeat;
		// a terminal snapshot is not evidence that this request stopped the job.
		if snap.Status == jobs.Running {
			_ = registry.Cancel(snap.ID)
		}
		view := backgroundJobSnapshot(job)
		status := http.StatusOK
		if view.Status == jobs.Running {
			status = http.StatusAccepted
		}
		writeJSON(w, r, status, map[string]any{"job": view})
	})
}
