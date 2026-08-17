package server

import (
	"sort"

	"code-agent/internal/app"
)

// ProviderTemplateDTO is the wire shape for GET /v1/provider-templates.
// Each template describes a known service that the user can connect to.
type ProviderTemplateDTO struct {
	ID          string                     `json:"id"`
	DisplayName string                     `json:"display_name"`
	Summary     string                     `json:"summary"`
	Kind        string                     `json:"kind"` // "api_key" | "local" | "gateway"
	BaseURL     string                     `json:"base_url,omitempty"`
	API         string                     `json:"api,omitempty"`
	Env         string                     `json:"env,omitempty"`
	Models      []ProviderTemplateModelDTO `json:"models,omitempty"`
}

// ProviderTemplateModelDTO is one suggested model in a provider template.
type ProviderTemplateModelDTO struct {
	ID                string   `json:"id"`
	RuntimeAlias      string   `json:"runtime_alias,omitempty"`
	ContextWindow     int      `json:"context_window,omitempty"`
	Temperature       float64  `json:"temperature,omitempty"`
	SupportsTools     bool     `json:"supports_tools,omitempty"`
	SupportsReasoning bool     `json:"supports_reasoning,omitempty"`
	InputModalities   []string `json:"input_modalities,omitempty"`
	WebSearch         bool     `json:"web_search,omitempty"`
	InputPricePerM    float64  `json:"input_price_per_million,omitempty"`
	OutputPricePerM   float64  `json:"output_price_per_million,omitempty"`
}

// buildProviderTemplates converts the built-in registry to template DTOs.
func buildProviderTemplates() []ProviderTemplateDTO {
	builtins := app.BuiltinProviderTemplates()
	out := make([]ProviderTemplateDTO, 0, len(builtins))
	for _, b := range builtins {
		dto := ProviderTemplateDTO{
			ID:          b.ID,
			DisplayName: coalesce(b.DisplayName, b.ID),
			Summary:     b.Summary,
			Kind:        coalesce(b.Kind, "api_key"),
			BaseURL:     b.BaseURL,
			API:         b.API,
			Env:         b.Env,
		}
		for _, m := range b.Models {
			dto.Models = append(dto.Models, ProviderTemplateModelDTO{
				ID:                m.ID,
				RuntimeAlias:      m.RuntimeAlias,
				ContextWindow:     m.ContextWindow,
				Temperature:       m.Temperature,
				SupportsTools:     m.SupportsTools,
				SupportsReasoning: m.SupportsReasoning,
				InputModalities:   m.InputModalities,
				WebSearch:         m.WebSearch,
				InputPricePerM:    m.InputPricePerM,
				OutputPricePerM:   m.OutputPricePerM,
			})
		}
		out = append(out, dto)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func coalesce(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}
