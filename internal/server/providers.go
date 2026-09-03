package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// ProviderService manages grouped provider configuration (design-providers-
// grouped-config.md §4). The server layer is a dumb pipe: the interface is
// implemented by the assembler (main.go / codeagentd) which owns the settings
// file, the Reconfigure trigger, and the failure-rollback policy. A nil
// Providers disables the endpoints (404), matching the Granter pattern.
type ProviderService interface {
	// List returns all providers (responses strip secrets/headers).
	List() ([]ProviderDTO, error)
	// Get returns one provider, or ErrProviderNotFound.
	Get(id string) (ProviderDTO, error)
	// Upsert creates or replaces provider id with spec. The spec's headers are
	// env references only; secrets never cross the wire. Returns applied=true
	// when the change took effect immediately, false when it is persisted but
	// needs a restart (OQ2).
	Upsert(id string, spec ProviderSpec) (applied bool, err error)
	// Delete removes provider id. Returns ErrProviderInUse if it is referenced
	// by default_model/subagent_model. Returns applied=true when the removal
	// took effect immediately, false when it needs a restart.
	Delete(id string) (applied bool, err error)
}

// ProviderDTO is the wire shape of a provider: the grouped config minus
// headers and credential details (secrets never leave the runtime, §4.3).
type ProviderDTO struct {
	ID         string             `json:"id"`
	Enabled    bool               `json:"enabled"`
	BaseURL    string             `json:"base_url,omitempty"`
	API        string             `json:"api,omitempty"`
	Credential ProviderCred       `json:"credential,omitempty"`
	Models     []ProviderModelDTO `json:"models"`
}

// ProviderCred is the non-secret credential declaration (namespace/name ref).
type ProviderCred struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
}

// ProviderModelDTO mirrors settings.ProviderModel so the full model definition
// can round-trip through the /v1/providers API. Presentation fields (pricing,
// capabilities) may also be available from /v1/runtime/models, but the
// canonical source is the settings file.
type ProviderModelDTO struct {
	ID                  string   `json:"id"`
	RuntimeAlias        string   `json:"runtime_alias,omitempty"`
	API                 string   `json:"api,omitempty"`
	ContextWindow       int      `json:"context_window,omitempty"`
	Temperature         float64  `json:"temperature,omitempty"`
	InputPricePerM      float64  `json:"input_price_per_million,omitempty"`
	OutputPricePerM     float64  `json:"output_price_per_million,omitempty"`
	CacheInputPricePerM float64  `json:"cache_input_price_per_million,omitempty"`
	SupportsTools       *bool    `json:"supports_tools,omitempty"`
	SupportsReasoning   *bool    `json:"supports_reasoning,omitempty"`
	InputModalities     []string `json:"input_modalities,omitempty"`
	WebSearch           bool     `json:"web_search,omitempty"`

	// ReasoningEffort is the model's default thinking budget
	// ("low"|"medium"|"high"|"x-high"|"max"; "" = auto/provider default).
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// SupportedReasoningEfforts lists the effort levels this model accepts
	// (empty = toggle only, no standardized effort control).
	SupportedReasoningEfforts []string `json:"supported_reasoning_efforts,omitempty"`
	// CanDisableReasoning reports whether reasoning may be turned off entirely
	// (false = reasoner-only; nil/true = the off position is allowed).
	CanDisableReasoning *bool `json:"can_disable_reasoning,omitempty"`
}

// ProviderSpec is the write shape accepted by PUT (may carry headers as env
// references).
type ProviderSpec struct {
	// Enabled defaults to true when absent (OQ1).
	Enabled    *bool              `json:"enabled,omitempty"`
	BaseURL    string             `json:"base_url,omitempty"`
	API        string             `json:"api,omitempty"`
	Credential ProviderCred       `json:"credential,omitempty"`
	Headers    map[string]string  `json:"headers,omitempty"`
	Models     []ProviderModelDTO `json:"models"`
}

var ErrProviderNotFound = errNotFound("provider")

// ErrProviderInUse is returned by Delete when the provider is referenced by
// default_model / subagent_model — the delete is refused to avoid a dangling
// default model (design-providers-grouped-config.md §7 ②).
var ErrProviderInUse = errors.New("provider is referenced by default_model or subagent_model")

func errNotFound(kind string) error { return &notFoundError{kind: kind} }

type notFoundError struct{ kind string }

func (e *notFoundError) Error() string { return e.kind + " not found" }

func (e *notFoundError) Is(target error) bool {
	var notFoundError *notFoundError
	ok := errors.As(target, &notFoundError)
	return ok
}

// registerProviderRoutes wires the /v1/providers endpoints onto mux. A nil
// service leaves the endpoints absent (404), matching other optional MuxOptions.
func registerProviderRoutes(mux *http.ServeMux, opts MuxOptions) {
	if opts.Providers == nil {
		return
	}
	svc := opts.Providers

	mux.HandleFunc("GET /v1/providers", func(w http.ResponseWriter, r *http.Request) {
		list, err := svc.List()
		if err != nil {
			writeProviderError(w, err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"providers": list})
	})

	mux.HandleFunc("GET /v1/providers/{id}", func(w http.ResponseWriter, r *http.Request) {
		dto, err := svc.Get(r.PathValue("id"))
		if err != nil {
			writeProviderError(w, err)
			return
		}
		writeJSON(w, r, http.StatusOK, dto)
	})

	mux.HandleFunc("PUT /v1/providers/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			writeJSON(w, r, http.StatusBadRequest, map[string]any{"error": "provider id required"})
			return
		}
		var spec ProviderSpec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			writeJSON(w, r, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
			return
		}
		// NOTE: an empty models list is allowed here — Upsert fills it from the
		// built-in registry for known connections (api-key-only onboarding) and
		// rejects it for custom ids. Per-model id validation happens after the
		// fill so registry-derived ids are covered too.
		for _, m := range spec.Models {
			if strings.TrimSpace(m.ID) == "" {
				writeJSON(w, r, http.StatusBadRequest, map[string]any{"error": "model id must not be empty"})
				return
			}
		}
		applied, uerr := svc.Upsert(id, spec)
		if uerr != nil {
			writeProviderError(w, uerr)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"id": id, "applied": applied})
	})

	mux.HandleFunc("DELETE /v1/providers/{id}", func(w http.ResponseWriter, r *http.Request) {
		applied, err := svc.Delete(r.PathValue("id"))
		if err != nil {
			writeProviderError(w, err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"id": r.PathValue("id"), "applied": applied})
	})
}

// writeProviderError maps a ProviderService error to an HTTP status: not-found
// → 404, in-use → 409, anything else → 400. All responses are plain text so the
// client sees a stable, greppable error.
func writeProviderError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrProviderNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, ErrProviderInUse):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}
