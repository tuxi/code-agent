package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"code-agent/internal/app"
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
	applyMu sync.Mutex
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

// SetReconfigure installs the runtime apply hook after the store has been
// constructed. Embedded runtimes need this because the HTTP store is wired
// into the mux before the Handle has been fully assembled.
func (s *ProviderStore) SetReconfigure(reconfigure func() error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	s.reconfigure = reconfigure
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
//
// Credential auto-declaration: when the provider id is a known built-in
// connection with an api_key kind and a declared env var (e.g. opencode-go →
// OPENCODE_GO_API_KEY), the matching credentials entry {source:"env", env:...}
// is created so the runtime resolver chain (config.CredentialResolver step 2)
// can resolve it. An existing entry is never overwritten — the user may have
// pointed it at a keychain/injected source instead. Custom (non-registry)
// providers get no entry: their env var name is unknown, so they configure
// credentials manually.
func (s *ProviderStore) Upsert(id string, spec ProviderSpec) (bool, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()

	if id == "" {
		return false, errors.New("provider id required")
	}
	// Read the current section for rollback.
	before, err := LoadProviders(s.settingsPath)
	if err != nil {
		return false, err
	}
	// Resolve the credential entry to auto-declare, if any. The credential ref
	// name defaults to the provider id when the spec leaves it empty.
	credNS, credName, credCfg, declare := credentialFor(id, spec)
	// Capture whether the credential entry already existed, so a reconfigure
	// rollback removes only what this upsert created (never a user's pre-existing
	// keychain/injected entry).
	credExisted := false
	if declare {
		_, credExisted, err = loadCredential(s.settingsPath, credNS, credName)
		if err != nil {
			return false, err
		}
	}
	api := spec.API
	if api == "gateway" {
		// Gateway is a connection kind, not a provider wire protocol. Accept
		// legacy clients but persist the OpenAI-compatible protocol value.
		api = "openai"
	}
	pc := settings.ServiceConfig{
		Enabled:    spec.Enabled,
		BaseURL:    spec.BaseURL,
		API:        api,
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
		if declare {
			ensureCredential(doc, credNS, credName, credCfg)
		}
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
			// Remove only a credential entry this upsert created; a pre-existing
			// entry is left untouched.
			if declare && !credExisted {
				removeCredential(doc, credNS, credName)
			}
		}); berr != nil {
			return false, fmt.Errorf("apply change failed (%v) and rollback failed (%v)", rerr, berr)
		}
		return false, fmt.Errorf("apply change failed; rolled back: %w", rerr)
	}
	return true, nil
}

// credentialFor computes the credentials entry to auto-declare for a provider
// upsert. It returns declare=false unless the provider is a known built-in
// api_key connection with a declared env var. The credential name mirrors the
// ref the provider will carry: spec.Credential.Name when set, else the
// provider id (matching the ServiceConfig default of llm/<id>).
func credentialFor(id string, spec ProviderSpec) (ns, name string, cfg settings.CredentialConfig, declare bool) {
	conn, known := app.BuiltinConnection(id)
	if !known || conn.Kind != "api_key" || conn.Env == "" {
		return "", "", settings.CredentialConfig{}, false
	}
	name = spec.Credential.Name
	if name == "" {
		name = id
	}
	ns = spec.Credential.Namespace
	if ns == "" {
		ns = "llm"
	}
	return ns, name, settings.CredentialConfig{Source: "env", Env: conn.Env}, true
}

// Delete implements ProviderService. Refuses when the provider is referenced
// by default_model/subagent_model (dangling default). Returns applied=true
// when the removal is hot-effective, false when it needs a restart (OQ2).
func (s *ProviderStore) Delete(id string) (bool, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()

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

// ensureCredential writes credentials[ns][name] = cfg when the entry is
// absent, preserving an existing entry verbatim (it may point at a keychain or
// injected source the user configured). Operates on the decoded settings
// document inside a writeProviders Persist, so the change is atomic with the
// provider upsert.
func ensureCredential(doc map[string]any, ns, name string, cfg settings.CredentialConfig) {
	creds := decodeCredentials(doc)
	if creds[ns] == nil {
		creds[ns] = map[string]settings.CredentialConfig{}
	}
	if _, exists := creds[ns][name]; exists {
		return // never clobber a user-configured entry
	}
	creds[ns][name] = cfg
	doc["credentials"] = creds
}

// removeCredential deletes credentials[ns][name], pruning the now-empty
// namespace map (and the credentials key) so rollback leaves no residue.
func removeCredential(doc map[string]any, ns, name string) {
	creds := decodeCredentials(doc)
	entries, ok := creds[ns]
	if !ok {
		return
	}
	delete(entries, name)
	if len(entries) == 0 {
		delete(creds, ns)
	}
	if len(creds) == 0 {
		delete(doc, "credentials")
		return
	}
	doc["credentials"] = creds
}

// decodeCredentials reads the credentials section of a decoded settings
// document (empty map when absent).
func decodeCredentials(doc map[string]any) map[string]map[string]settings.CredentialConfig {
	creds := map[string]map[string]settings.CredentialConfig{}
	if raw, ok := doc["credentials"].(map[string]any); ok {
		buf, _ := json.Marshal(raw)
		_ = json.Unmarshal(buf, &creds)
	}
	return creds
}

// loadCredential reads a single credentials entry from the settings file. The
// second return reports whether the entry exists.
func loadCredential(settingsPath, ns, name string) (settings.CredentialConfig, bool, error) {
	f, err := loadSettingsFile(settingsPath)
	if err != nil {
		return settings.CredentialConfig{}, false, err
	}
	if f.Credentials == nil {
		return settings.CredentialConfig{}, false, nil
	}
	entries, ok := f.Credentials[ns]
	if !ok {
		return settings.CredentialConfig{}, false, nil
	}
	cfg, ok := entries[name]
	return cfg, ok, nil
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
