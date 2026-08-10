package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"code-agent/internal/settings"
)

// ProviderStore is the default ProviderService implementation: it reads and
// writes the providers section of the settings.json files at user/project
// scope, and calls reconfigure (if set) after a successful write so the change
// lands at the next turn boundary. Secrets never appear in the wire DTOs or in
// what is written (headers carry env references only).
//
// The store is created by the assembler (main.go / codeagentd), which supplies
// the settings paths and the Reconfigure trigger — the server layer stays a
// dumb pipe over the ProviderService interface.
type ProviderStore struct {
	// settingsPath is the settings.json file that owns the providers section.
	// For a desktop/server deployment this is <root>/.codeagent/settings.json.
	settingsPath string
	// reconfigure, when set, hot-applies the change (embed.Handle.Reconfigure).
	// The caller decides rollback policy on failure. When nil the change only
	// lands on the next restart.
	reconfigure func() error
}

// NewProviderStore builds the store for the given settings file.
func NewProviderStore(settingsPath string, reconfigure func() error) *ProviderStore {
	return &ProviderStore{settingsPath: settingsPath, reconfigure: reconfigure}
}

// LoadProviders reads the providers section from disk (empty map when absent).
func LoadProviders(settingsPath string) (map[string]settings.ServiceConfig, error) {
	f, err := loadSettingsFile(settingsPath)
	if err != nil {
		return nil, err
	}
	if f.Providers == nil {
		f.Providers = map[string]settings.ServiceConfig{}
	}
	return f.Providers, nil
}

// List implements ProviderService.
func (s *ProviderStore) List() ([]ProviderDTO, error) {
	providers, err := LoadProviders(s.settingsPath)
	if err != nil {
		return nil, err
	}
	out := make([]ProviderDTO, 0, len(providers))
	for id, pc := range providers {
		out = append(out, toProviderDTO(id, pc))
	}
	return out, nil
}

// Get implements ProviderService.
func (s *ProviderStore) Get(id string) (ProviderDTO, error) {
	providers, err := LoadProviders(s.settingsPath)
	if err != nil {
		return ProviderDTO{}, err
	}
	pc, ok := providers[id]
	if !ok {
		return ProviderDTO{}, fmt.Errorf("%w %q", ErrProviderNotFound, id)
	}
	return toProviderDTO(id, pc), nil
}

// Upsert implements ProviderService. It writes the providers section via
// settings.Persist (cross-process file lock), then triggers reconfigure. On
// reconfigure failure the change is rolled back (the section is restored) so
// disk and runtime do not silently diverge (design §4.2). Returns applied=true
// when the change is hot-effective, false when it needs a restart (OQ2).
func (s *ProviderStore) Upsert(id string, spec ProviderSpec) (bool, error) {
	if id == "" {
		return false, errors.New("provider id required")
	}
	// Read the current section for rollback.
	before, err := LoadProviders(s.settingsPath)
	if err != nil {
		return false, err
	}
	pc := settings.ServiceConfig{
		Enabled:    spec.Enabled,
		BaseURL:    spec.BaseURL,
		API:        spec.API,
		Credential: settings.CredentialRef{Namespace: spec.Credential.Namespace, Name: spec.Credential.Name},
		Headers:    spec.Headers,
	}
	for _, m := range spec.Models {
		pc.Models = append(pc.Models, settings.ProviderModel{
			ID:                  m.ID,
			RuntimeAlias:        m.RuntimeAlias,
			API:                 m.API,
			ContextWindow:       m.ContextWindow,
			Temperature:         m.Temperature,
			InputPricePerM:      m.InputPricePerM,
			OutputPricePerM:     m.OutputPricePerM,
			CacheInputPricePerM: m.CacheInputPricePerM,
			SupportsTools:       m.SupportsTools,
			SupportsReasoning:   m.SupportsReasoning,
			InputModalities:     m.InputModalities,
			WebSearch:           m.WebSearch,
		})
	}

	if err := s.writeProviders(func(doc map[string]any, providers map[string]settings.ServiceConfig) {
		providers[id] = pc
	}); err != nil {
		return false, err
	}
	if s.reconfigure == nil {
		return false, nil // persisted, restart required
	}
	if rerr := s.reconfigure(); rerr != nil {
		// Roll back the disk change so "stored but not effective" cannot
		// silently persist.
		if berr := s.writeProviders(func(doc map[string]any, providers map[string]settings.ServiceConfig) {
			if _, existed := before[id]; existed {
				providers[id] = before[id]
			} else {
				delete(providers, id)
			}
		}); berr != nil {
			return false, fmt.Errorf("apply change failed (%v) and rollback failed (%v)", rerr, berr)
		}
		return false, fmt.Errorf("apply change failed; rolled back: %w", rerr)
	}
	return true, nil
}

// Delete implements ProviderService. Refuses when the provider is referenced
// by default_model/subagent_model (dangling default). Returns applied=true
// when the removal is hot-effective, false when it needs a restart (OQ2).
func (s *ProviderStore) Delete(id string) (bool, error) {
	providers, err := LoadProviders(s.settingsPath)
	if err != nil {
		return false, err
	}
	if _, ok := providers[id]; !ok {
		return false, fmt.Errorf("%w %q", ErrProviderNotFound, id)
	}
	// Reference check: if the current default/subagent model belongs to this
	// provider, refuse.
	if s.referencedByDefault(id) {
		return false, ErrProviderInUse
	}
	before := providers
	if err := s.writeProviders(func(doc map[string]any, providers map[string]settings.ServiceConfig) {
		delete(providers, id)
	}); err != nil {
		return false, err
	}
	if s.reconfigure == nil {
		return false, nil // persisted, restart required
	}
	if rerr := s.reconfigure(); rerr != nil {
		_ = s.writeProviders(func(doc map[string]any, providers map[string]settings.ServiceConfig) {
			for k, v := range before {
				providers[k] = v
			}
		})
		return false, fmt.Errorf("apply delete failed; rolled back: %w", rerr)
	}
	return true, nil
}

// writeProviders applies mutate under the settings cross-process lock and
// persists the providers section (preserving all other keys).
func (s *ProviderStore) writeProviders(mutate func(doc map[string]any, providers map[string]settings.ServiceConfig)) error {
	return settings.Persist(s.settingsPath, func(doc map[string]any) {
		providers := map[string]settings.ServiceConfig{}
		if raw, ok := doc["providers"].(map[string]any); ok {
			buf, _ := json.Marshal(raw)
			_ = json.Unmarshal(buf, &providers)
		}
		mutate(doc, providers)
		doc["providers"] = providers
	})
}

// referencedByDefault reports whether provider id is referenced by
// default_model/subagent_model. The default model key is either a friendly
// name (flat form, e.g. "deepseek") or an alias key
// (provider.<b64>.model.<b64>); a default in this provider's namespace counts
// as a reference, as does a friendly name that matches the provider id.
func (s *ProviderStore) referencedByDefault(id string) bool {
	f, err := loadSettingsFile(s.settingsPath)
	if err != nil {
		return false // fail-open on read error; the delete proceeds (documented)
	}
	refs := []string{f.DefaultModel, f.SubagentModel, f.Agent.SubagentModel}
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		if ref == id {
			return true // friendly name == provider id (flat "deepseek" default)
		}
		// alias key: provider.<b64>.model.<b64> — decode the provider segment.
		if pid, ok := aliasProviderID(ref); ok && pid == id {
			return true
		}
		// user-readable "<provider>/<model>" form (e.g. "dashscope/qwen3-coder-plus"):
		// the leftmost segment is the provider id.
		if slash := strings.Index(ref, "/"); slash > 0 && ref[:slash] == id {
			return true
		}
	}
	return false
}

// aliasProviderID decodes the provider segment of an alias key
// (provider.<b64>.model.<b64>). Returns ok=false for non-alias keys.
func aliasProviderID(key string) (string, bool) {
	const p = "provider."
	if !strings.HasPrefix(key, p) {
		return "", false
	}
	rest := key[len(p):]
	dot := strings.Index(rest, ".model.")
	if dot <= 0 {
		return "", false
	}
	enc := rest[:dot]
	buf, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return "", false
	}
	return string(buf), true
}

func toProviderDTO(id string, pc settings.ServiceConfig) ProviderDTO {
	dto := ProviderDTO{
		ID:         id,
		Enabled:    pc.Enabled == nil || *pc.Enabled,
		BaseURL:    pc.BaseURL,
		API:        pc.API,
		Credential: ProviderCred{Namespace: pc.Credential.Namespace, Name: pc.Credential.Name},
	}
	for _, m := range pc.Models {
		dto.Models = append(dto.Models, ProviderModelDTO{
			ID:                  m.ID,
			RuntimeAlias:        m.RuntimeAlias,
			API:                 m.API,
			ContextWindow:       m.ContextWindow,
			Temperature:         m.Temperature,
			InputPricePerM:      m.InputPricePerM,
			OutputPricePerM:     m.OutputPricePerM,
			CacheInputPricePerM: m.CacheInputPricePerM,
			SupportsTools:       m.SupportsTools,
			SupportsReasoning:   m.SupportsReasoning,
			InputModalities:     m.InputModalities,
			WebSearch:           m.WebSearch,
		})
	}
	return dto
}

func loadSettingsFile(path string) (settings.File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return settings.File{}, nil
		}
		return settings.File{}, err
	}
	return settings.ParseJSON(data)
}
