package server

import (
	"encoding/json"
	"net/http"
	"time"

	"code-agent/internal/automation"
)

// registerAutomationRoutes serves the automation control-plane endpoints
// (/v1/automations). They are the client-facing counterpart to the `automation`
// model tool: the client control panel lists/creates/pauses/enables automations
// and inspects their run history. When opts.AutomationStore is nil the endpoints
// are not registered (404), matching the Providers/Granter pattern.
func registerAutomationRoutes(mux *http.ServeMux, opts MuxOptions) {
	if opts.AutomationStore == nil {
		return
	}
	store := opts.AutomationStore

	mux.HandleFunc("GET /v1/automations", func(w http.ResponseWriter, r *http.Request) {
		items, err := store.List(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, r, http.StatusOK, toAutomationListDTO(items))
	})

	mux.HandleFunc("POST /v1/automations", func(w http.ResponseWriter, r *http.Request) {
		var req automationCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		a, err := req.toAutomation()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		created, err := store.Create(r.Context(), a)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, r, http.StatusCreated, toAutomationDTO(created))
	})

	mux.HandleFunc("GET /v1/automations/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		a, err := store.Get(r.Context(), id)
		if err != nil {
			http.Error(w, "automation not found", http.StatusNotFound)
			return
		}
		if !a.DeletedAt.IsZero() {
			http.Error(w, "automation not found", http.StatusNotFound)
			return
		}
		writeJSON(w, r, http.StatusOK, toAutomationDTO(a))
	})

	mux.HandleFunc("PATCH /v1/automations/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var req automationPatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		patch, err := req.toPatch()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		updated, err := store.Update(r.Context(), id, patch)
		if err != nil {
			http.Error(w, "automation not found", http.StatusNotFound)
			return
		}
		writeJSON(w, r, http.StatusOK, toAutomationDTO(updated))
	})

	mux.HandleFunc("DELETE /v1/automations/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := store.Delete(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /v1/automations/{id}/runs", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		runs, err := store.ListRuns(r.Context(), id, 50)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, r, http.StatusOK, runs)
	})
}

// --- DTOs ---

type automationDTO struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	Prompt               string   `json:"prompt"`
	Status               string   `json:"status"`
	ScheduleType         string   `json:"schedule_type"`
	RRule                string   `json:"rrule,omitempty"`
	ScheduledAt          string   `json:"scheduled_at,omitempty"`
	Timezone             string   `json:"timezone"`
	ModeExec             string   `json:"mode_exec"`
	SessionID            string   `json:"session_id,omitempty"`
	CWDs                 []string `json:"cwds,omitempty"`
	ModelID              string   `json:"model_id,omitempty"`
	Skills               []string `json:"skills,omitempty"`
	Connectors           []string `json:"connectors,omitempty"`
	PermissionMode       string   `json:"permission_mode,omitempty"`
	CreatedFromWorkspace string   `json:"created_from_workspace,omitempty"`
	LastRunAt            string   `json:"last_run_at,omitempty"`
	NextRunAt            string   `json:"next_run_at,omitempty"`
	RunCount             int64    `json:"run_count"`
	LastStatus           string   `json:"last_status,omitempty"`
	RetryCount           int      `json:"retry_count"`
	WorkflowRef          string   `json:"workflow_ref,omitempty"`
	WorkflowInput        string   `json:"workflow_input,omitempty"`
	OverlapPolicy        string   `json:"overlap_policy,omitempty"`
	CreatedAt            string   `json:"created_at"`
	UpdatedAt            string   `json:"updated_at"`
}

func toAutomationDTO(a automation.Automation) automationDTO {
	return automationDTO{
		ID:                   a.ID,
		Name:                 a.Name,
		Prompt:               a.Prompt,
		Status:               string(a.Status),
		ScheduleType:         string(a.ScheduleType),
		RRule:                a.RRule,
		ScheduledAt:          fmtTimeOrEmpty(a.ScheduledAt),
		Timezone:             a.Timezone,
		ModeExec:             string(a.ModeExec),
		SessionID:            a.SessionID,
		CWDs:                 a.CWDs,
		ModelID:              a.ModelID,
		Skills:               a.Skills,
		Connectors:           a.Connectors,
		PermissionMode:       a.PermissionMode,
		CreatedFromWorkspace: a.CreatedFromWorkspace,
		LastRunAt:            fmtTimeOrEmpty(a.LastRunAt),
		NextRunAt:            fmtTimeOrEmpty(a.NextRunAt),
		RunCount:             a.RunCount,
		LastStatus:           a.LastStatus,
		RetryCount:           a.RetryCount,
		WorkflowRef:          a.WorkflowRef,
		WorkflowInput:        a.WorkflowInput,
		OverlapPolicy:        a.OverlapPolicy,
		CreatedAt:            a.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:            a.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func toAutomationListDTO(items []automation.Automation) []automationDTO {
	out := make([]automationDTO, 0, len(items))
	for _, a := range items {
		out = append(out, toAutomationDTO(a))
	}
	return out
}

func fmtTimeOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// --- request bodies ---

type automationCreateRequest struct {
	Name           string         `json:"name"`
	Prompt         string         `json:"prompt"`
	ScheduleType   string         `json:"schedule_type"`
	RRule          string         `json:"rrule"`
	ScheduledAt    string         `json:"scheduled_at"`
	Timezone       string         `json:"timezone"`
	ModeExec       string         `json:"mode_exec"`
	SessionID      string         `json:"session_id"`
	CWDs           []string       `json:"cwds"`
	ModelID        string         `json:"model_id"`
	Skills         []string       `json:"skills"`
	Connectors     []string       `json:"connectors"`
	PermissionMode string         `json:"permission_mode"`
	WorkflowRef    string         `json:"workflow_ref"`
	WorkflowInput  map[string]any `json:"workflow_input"`
	OverlapPolicy  string         `json:"overlap_policy"`
	Enabled        *bool          `json:"enabled"`
}

func (r automationCreateRequest) toAutomation() (automation.Automation, error) {
	if r.Name == "" || r.Timezone == "" {
		return automation.Automation{}, errBadRequest("name, timezone, and (prompt or workflow_ref) are required")
	}
	if r.Prompt == "" && r.WorkflowRef == "" {
		return automation.Automation{}, errBadRequest("prompt or workflow_ref is required")
	}
	if r.Prompt != "" && r.WorkflowRef != "" {
		return automation.Automation{}, errBadRequest("prompt and workflow_ref are mutually exclusive — set only one")
	}
	st := automation.ScheduleRecurring
	if r.ScheduleType == "once" {
		st = automation.ScheduleOnce
	} else if r.ScheduleType != "" && r.ScheduleType != "recurring" {
		return automation.Automation{}, errBadRequest("schedule_type must be once or recurring")
	}
	if st == automation.ScheduleOnce && r.ScheduledAt == "" {
		return automation.Automation{}, errBadRequest("once requires scheduled_at")
	}
	if st == automation.ScheduleRecurring && r.RRule == "" {
		return automation.Automation{}, errBadRequest("recurring requires rrule")
	}
	mode := automation.ModeStandalone
	if r.ModeExec == "chat" {
		mode = automation.ModeChat
	} else if r.ModeExec != "" && r.ModeExec != "standalone" {
		return automation.Automation{}, errBadRequest("mode_exec must be standalone or chat")
	}
	if mode == automation.ModeChat && r.SessionID == "" {
		return automation.Automation{}, errBadRequest("chat mode requires session_id")
	}
	permissionMode, ok := automation.NormalizePermissionMode(r.PermissionMode)
	if !ok {
		return automation.Automation{}, errBadRequest("permission_mode must be one of ask, auto, full")
	}
	if permissionMode == "" {
		permissionMode = "full" // unattended default, same as the tool
	}
	status := automation.StatusActive
	if r.Enabled != nil && !*r.Enabled {
		status = automation.StatusPaused
	}
	var scheduledAt time.Time
	if r.ScheduledAt != "" {
		t, err := time.Parse(time.RFC3339, r.ScheduledAt)
		if err != nil {
			return automation.Automation{}, errBadRequest("scheduled_at must be RFC3339")
		}
		scheduledAt = t
	}
	return automation.Automation{
		Name:           r.Name,
		Prompt:         r.Prompt,
		Status:         status,
		ScheduleType:   st,
		RRule:          r.RRule,
		ScheduledAt:    scheduledAt,
		Timezone:       r.Timezone,
		ModeExec:       mode,
		SessionID:      r.SessionID,
		CWDs:           r.CWDs,
		ModelID:        r.ModelID,
		Skills:         r.Skills,
		Connectors:     r.Connectors,
		PermissionMode: permissionMode,
		WorkflowRef:    r.WorkflowRef,
		WorkflowInput:  workflowInputJSON(r.WorkflowInput),
		OverlapPolicy:  r.OverlapPolicy,
	}, nil
}

// workflowInputJSON marshals a workflow-mode trigger input to its persisted
// JSON string; empty map yields "". (Same helper as the automation tool.)
func workflowInputJSON(input map[string]any) string {
	if len(input) == 0 {
		return ""
	}
	b, err := json.Marshal(input)
	if err != nil {
		return ""
	}
	return string(b)
}

type automationPatchRequest struct {
	Name           *string         `json:"name"`
	Prompt         *string         `json:"prompt"`
	ScheduleType   *string         `json:"schedule_type"`
	RRule          *string         `json:"rrule"`
	ScheduledAt    *string         `json:"scheduled_at"`
	Timezone       *string         `json:"timezone"`
	ModeExec       *string         `json:"mode_exec"`
	SessionID      *string         `json:"session_id"`
	CWDs           *[]string       `json:"cwds"`
	ModelID        *string         `json:"model_id"`
	Skills         *[]string       `json:"skills"`
	Connectors     *[]string       `json:"connectors"`
	PermissionMode *string         `json:"permission_mode"`
	WorkflowRef    *string         `json:"workflow_ref"`
	WorkflowInput  *map[string]any `json:"workflow_input"`
	OverlapPolicy  *string         `json:"overlap_policy"`
	Enabled        *bool           `json:"enabled"`
}

func (r automationPatchRequest) toPatch() (automation.AutomationPatch, error) {
	var p automation.AutomationPatch
	p.Name = r.Name
	p.Prompt = r.Prompt
	if r.ScheduleType != nil {
		st := automation.ScheduleType(*r.ScheduleType)
		p.ScheduleType = &st
	}
	p.RRule = r.RRule
	if r.ScheduledAt != nil {
		t, err := time.Parse(time.RFC3339, *r.ScheduledAt)
		if err != nil {
			return p, errBadRequest("scheduled_at must be RFC3339")
		}
		p.ScheduledAt = &t
	}
	p.Timezone = r.Timezone
	if r.ModeExec != nil {
		m := automation.RunMode(*r.ModeExec)
		p.ModeExec = &m
	}
	p.SessionID = r.SessionID
	p.CWDs = r.CWDs
	p.ModelID = r.ModelID
	p.Skills = r.Skills
	p.Connectors = r.Connectors
	if r.PermissionMode != nil {
		m, ok := automation.NormalizePermissionMode(*r.PermissionMode)
		if !ok {
			return automation.AutomationPatch{}, errBadRequest("permission_mode must be one of ask, auto, full")
		}
		p.PermissionMode = &m
	}
	if r.Enabled != nil {
		st := automation.StatusActive
		if !*r.Enabled {
			st = automation.StatusPaused
		}
		p.Status = &st
	}
	p.WorkflowRef = r.WorkflowRef
	if r.WorkflowInput != nil {
		wi := workflowInputJSON(*r.WorkflowInput)
		p.WorkflowInput = &wi
	}
	p.OverlapPolicy = r.OverlapPolicy
	return p, nil
}

type badRequestError struct{ msg string }

func (e *badRequestError) Error() string { return e.msg }

func errBadRequest(msg string) error { return &badRequestError{msg: msg} }
