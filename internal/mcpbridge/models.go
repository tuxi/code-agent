package mcpbridge

import (
	"code-agent/internal/model"
	"context"
	"errors"
)

// Safe projection of the daemon catalog, not another model registry.
type ModelDescriptor struct {
	legacyCatalog             bool
	ID                        string   `json:"id"`
	RuntimeAlias              string   `json:"runtime_alias"`
	WireModelID               string   `json:"wire_model_id"`
	Provider                  string   `json:"provider"`
	ProviderID                string   `json:"provider_id"`
	Configured                bool     `json:"configured"`
	Available                 bool     `json:"available"`
	UnavailableReason         string   `json:"unavailable_reason,omitempty"`
	Verified                  bool     `json:"verified"`
	VerifiedAt                string   `json:"verified_at,omitempty"`
	ActualModel               string   `json:"actual_model,omitempty"`
	ConfigurationID           string   `json:"configuration_id,omitempty"`
	DisplayName               string   `json:"display_name"`
	ContextWindow             int      `json:"context_window,omitempty"`
	SupportsTools             bool     `json:"supports_tools"`
	SupportsReasoning         bool     `json:"supports_reasoning"`
	InputModalities           []string `json:"input_modalities"`
	ReasoningEffort           string   `json:"reasoning_effort,omitempty"`
	SupportedReasoningEfforts []string `json:"supported_reasoning_efforts,omitempty"`
	CanDisableReasoning       *bool    `json:"can_disable_reasoning,omitempty"`
}

type ModelCatalog struct {
	DefaultModel              string            `json:"default_model"`
	Models                    []ModelDescriptor `json:"models"`
	VerificationScope         string            `json:"verification_scope,omitempty"`
	VerificationMaxAgeSeconds int64             `json:"verification_max_age_seconds,omitempty"`
}

func (c *Client) ListModels(ctx context.Context) (ModelCatalog, error) {
	var remote struct {
		DefaultModel              string `json:"default_model"`
		DefaultAlias              string `json:"default_runtime_alias"`
		VerificationScope         string `json:"verification_scope"`
		VerificationMaxAgeSeconds int64  `json:"verification_max_age_seconds"`
		Connections               []struct {
			ID         string            `json:"id"`
			ProviderID string            `json:"provider_id"`
			Models     []ModelDescriptor `json:"models"`
		} `json:"connections"`
	}
	if err := c.request(ctx, "GET", "/v1/runtime/models", nil, &remote); err != nil {
		return ModelCatalog{}, err
	}
	out := ModelCatalog{DefaultModel: remote.DefaultModel, Models: []ModelDescriptor{}, VerificationScope: remote.VerificationScope, VerificationMaxAgeSeconds: remote.VerificationMaxAgeSeconds}
	for _, connection := range remote.Connections {
		for _, entry := range connection.Models {
			entry.Provider, entry.ProviderID = connection.ID, connection.ProviderID
			// Older v2 catalogs list only configured models, without an ID field.
			if entry.ID == "" {
				entry.ID = connection.ID + "/" + entry.WireModelID
				entry.Configured = true
				entry.legacyCatalog = true
			}
			if out.DefaultModel == "" && entry.RuntimeAlias == remote.DefaultAlias {
				out.DefaultModel = entry.ID
			}
			out.Models = append(out.Models, entry)
		}
	}
	return out, nil
}

func (c *Client) resolveModel(ctx context.Context, id string) (string, error) {
	catalog, err := c.ListModels(ctx)
	if err != nil {
		return "", err
	}
	for _, entry := range catalog.Models {
		if entry.ID == id || entry.RuntimeAlias == id {
			if !entry.Configured || !entry.Available {
				return "", errors.New("selected model is unavailable: " + entry.UnavailableReason)
			}
			if entry.legacyCatalog && entry.RuntimeAlias != "" {
				return entry.RuntimeAlias, nil
			}
			return id, nil
		}
	}
	return "", errors.New("selected model is not in the runtime model catalog")
}

// A successful model response never proves a tool, test or task passed.
type ModelCallEvidence struct {
	InvocationID       string           `json:"invocation_id"`
	Selection          *model.Selection `json:"model_selection,omitempty"`
	Succeeded          *bool            `json:"model_succeeded,omitempty"`
	PromptTokens       int              `json:"prompt_tokens"`
	CompletionTokens   int              `json:"completion_tokens"`
	TotalTokens        int              `json:"total_tokens"`
	CachedPromptTokens int              `json:"cached_prompt_tokens"`
	ElapsedMS          int64            `json:"elapsed_ms"`
}
