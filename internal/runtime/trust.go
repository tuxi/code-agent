package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"code-agent/internal/agent"
	"code-agent/internal/hooks"
	"code-agent/internal/settings"
)

// TrustStore persists user-level trust decisions per project directory.
// Format: ~/.codeagent/trust.json → {"/abs/path": true | false | null}
// null is the "ask again" sentinel — treated as not found, falls through.
//
// Path inheritance: checking /a/b/c first looks for "/a/b/c", then "/a/b",
// then "/a", then "/". The most specific match wins. Exact match takes
// precedence over inherited.
type TrustStore struct {
	path  string
	mu    sync.RWMutex
	cache map[string]*bool // abs path → trusted: true, untrusted: false, nil: undecided (null in JSON)
}

// TrustStorePath returns the path to the trust store file.
func TrustStorePath(home string) string {
	return filepath.Join(home, ".codeagent", "trust.json")
}

// LoadTrustStore reads trust.json from disk, or returns an empty store on
// missing/corrupt file. A missing file is not an error — it means no trust
// decisions have been made yet.
func LoadTrustStore(home string) (*TrustStore, error) {
	ts := &TrustStore{
		path:  TrustStorePath(home),
		cache: make(map[string]*bool),
	}
	data, err := os.ReadFile(ts.path)
	if err != nil {
		if os.IsNotExist(err) {
			return ts, nil // empty store
		}
		return nil, fmt.Errorf("read trust store: %w", err)
	}
	if err := json.Unmarshal(data, &ts.cache); err != nil {
		// Corrupt file: start fresh, next Store will overwrite.
		return &TrustStore{path: ts.path, cache: make(map[string]*bool)}, nil
	}
	return ts, nil
}

// Lookup checks the store for a trust decision on cwd, walking up to the root
// (parent directory inheritance). Returns (trusted, found). When found is
// false, no entry (or only null entries) were found in the ancestor chain.
func (s *TrustStore) Lookup(cwd string) (bool, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	abs, err := filepath.Abs(cwd)
	if err != nil {
		return false, false
	}
	abs = filepath.Clean(abs)

	for {
		v, ok := s.cache[abs]
		if ok && v != nil {
			return *v, true
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			break // reached root
		}
		abs = parent
	}
	return false, false
}

// Store writes a trust decision for cwd. Uses atomic temp+rename so a crash
// cannot leave a partial file. Cross-process coordination via flock on a
// sibling .lock file.
func (s *TrustStore) Store(cwd string, trusted bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	abs, err := filepath.Abs(cwd)
	if err != nil {
		return fmt.Errorf("trust store: abs path: %w", err)
	}
	abs = filepath.Clean(abs)

	// Load current state from disk to merge with in-memory cache.
	if data, err := os.ReadFile(s.path); err == nil {
		var disk map[string]*bool
		if json.Unmarshal(data, &disk) == nil {
			for k, v := range disk {
				if _, ok := s.cache[k]; !ok {
					s.cache[k] = v
				}
			}
		}
	}

	s.cache[abs] = &trusted

	// Write atomically: temp file + rename.
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("trust store: mkdir: %w", err)
	}

	// Cross-process lock.
	lockPath := s.path + ".lock"
	lockF, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("trust store: open lock: %w", err)
	}
	if err := syscall.Flock(int(lockF.Fd()), syscall.LOCK_EX); err != nil {
		lockF.Close()
		return fmt.Errorf("trust store: lock: %w", err)
	}
	defer func() {
		syscall.Flock(int(lockF.Fd()), syscall.LOCK_UN)
		lockF.Close()
	}()

	tmp, err := os.CreateTemp(dir, ".trust-*.json")
	if err != nil {
		return fmt.Errorf("trust store: temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	// Sort keys for stable output.
	keys := make([]string, 0, len(s.cache))
	for k := range s.cache {
		keys = append(keys, k)
	}
	// Simple sort by path.
	sortStrings(keys)

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	out := make(map[string]*bool, len(s.cache))
	for _, k := range keys {
		out[k] = s.cache[k]
	}
	if err := enc.Encode(out); err != nil {
		tmp.Close()
		return fmt.Errorf("trust store: encode: %w", err)
	}
	tmp.Close()

	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("trust store: rename: %w", err)
	}
	return nil
}

// sortStrings sorts a slice of strings in place.
func sortStrings(s []string) {
	for i := 0; i < len(s); i++ {
		for j := i + 1; j < len(s); j++ {
			if s[i] > s[j] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

// HasTrustRequiringResources reports whether a project directory contains
// resources that require a trust decision. If there are none, the project is
// implicitly trusted without prompting the user.
func HasTrustRequiringResources(root string) bool {
	paths := []string{
		settings.ProjectSharedPath(root),
		settings.ProjectLocalPath(root),
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// ResolveTrust runs the trust resolution chain for a project directory:
//  1. CLI flag (--trust/--no-trust) → passed as cliOverride
//  2. project_trust hooks from user settings → hookRunner
//  3. trust.json store (parent dir inheritance) → store
//  4. defaultProjectTrust setting → defaultTrust
//  5. Interactive UI (TrustPolicy interface) → policy
//  6. None of the above → fail-closed: deny
//
// Returns (trusted bool, reason string, error).
func ResolveTrust(
	ctx context.Context,
	cwd string,
	hasResources bool,
	cliOverride *bool,
	hookRunner *hooks.Runner,
	store *TrustStore,
	defaultTrust *bool,
	policy agent.TrustPolicy,
) (bool, string, error) {
	// If there are no trust-requiring resources, implicitly trust.
	if !hasResources {
		return true, "no project resources", nil
	}

	// 1. CLI flag overrides everything.
	if cliOverride != nil {
		if *cliOverride {
			return true, "CLI flag --trust", nil
		}
		return false, "CLI flag --no-trust", nil
	}

	// 2. project_trust hook (from user settings).
	if hookRunner != nil && hookRunner.HasProjectTrustHook() {
		verdict, err := hookRunner.ProjectTrustDecide(ctx, cwd, hasResources)
		if err == nil && verdict.Decided {
			return verdict.Trusted, "project_trust hook: " + verdict.Reason, nil
		}
	}

	// 3. trust.json store with parent dir inheritance.
	if store != nil {
		if trusted, found := store.Lookup(cwd); found {
			reason := "persisted trust decision"
			if !trusted {
				reason = "persisted: not trusted"
			}
			return trusted, reason, nil
		}
	}

	// 4. defaultProjectTrust setting.
	if defaultTrust != nil {
		if *defaultTrust {
			return true, "default: always trust", nil
		}
		return false, "default: never trust", nil
	}

	// 5. Interactive UI via TrustPolicy.
	if policy != nil {
		decision, err := policy.ResolveTrust(ctx, cwd, hasResources)
		if err != nil {
			return false, fmt.Sprintf("trust policy error: %v", err), nil
		}
		switch decision {
		case agent.TrustAllowed:
			return true, "user confirmed", nil
		case agent.TrustDenied:
			return false, "user declined", nil
		default:
			// TrustUndecided: fall through to fail-closed.
		}
	}

	// 6. Fail-closed: unattended/headless mode without a policy.
	return false, "no trust policy available (headless mode)", nil
}

// LoadSettingsWithTrust performs staged settings loading with trust gating.
//
//  1. Load user-level settings only.
//  2. Check if project has trust-requiring resources.
//  3. Resolve trust.
//  4. If trusted, merge project settings; otherwise use user settings only.
func LoadSettingsWithTrust(
	root, home string,
	cliOverride *bool,
	policy agent.TrustPolicy,
	warnWriter interface{ Write([]byte) (int, error) },
) settings.Settings {
	// We need io.Writer for settings.Load* functions.
	type writer interface{ Write([]byte) (int, error) }
	var warn writer = os.Stderr
	if ww, ok := warnWriter.(writer); ok && warnWriter != nil {
		warn = ww
	}

	// Step 1: Load user settings.
	base := settings.LoadUserOnly(home, warn)

	// Step 2: Check for project resources.
	hasResources := HasTrustRequiringResources(root)

	// Step 3: Set up resolution components from user settings.
	var hookRunner *hooks.Runner
	projectTrustHooks := filterHooksByEvent(base.Hooks, hooks.ProjectTrust)
	if len(projectTrustHooks) > 0 {
		hookRunner = hooks.New(projectTrustHooks, root)
	}

	store, _ := LoadTrustStore(home)

	// Step 4: Resolve trust.
	trusted, _, _ := ResolveTrust(context.Background(), root, hasResources, cliOverride, hookRunner, store, nil, policy)

	// Step 5: Conditionally load project settings.
	if trusted {
		overlay := settings.LoadProjectSettings(root, warn)
		settings.MergeSettings(&base, overlay)
	}

	return base
}

// filterHooksByEvent returns hooks matching the given event type.
func filterHooksByEvent(hs []hooks.Hook, ev hooks.Event) []hooks.Hook {
	var out []hooks.Hook
	for _, h := range hs {
		if h.Event == ev {
			out = append(out, h)
		}
	}
	return out
}

// Ensure strings import is used.
var _ = strings.TrimSpace
