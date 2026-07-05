package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// writeMu serializes all settings-file writes so concurrent persists — e.g. a
// permission grant and an agent-written verify command targeting the same
// settings.local.json — cannot clobber each other.
var writeMu sync.Mutex

// ParseJSON parses an in-memory settings document — the injection path for
// embedded hosts (iOS) that have no fixed settings.json on disk, parallel to how
// .mcp.json is injected. Empty input is an empty File; malformed input errors.
func ParseJSON(data []byte) (File, error) {
	var f File
	if len(data) == 0 {
		return f, nil
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return f, fmt.Errorf("parse settings json: %w", err)
	}
	return f, nil
}

// Persist atomically updates the settings file at path by applying mutate to its
// decoded document, PRESERVING every key mutate does not touch (a corrupt file is
// replaced with a valid one). Parent dirs are created; the write is temp+rename so
// a crash can't leave a partial file. Serialized process-wide. This is the single
// canonical settings writer (P11.d) — permission grants and verify both use it.
func Persist(path string, mutate func(doc map[string]any)) error {
	if path == "" {
		return errors.New("settings: empty path")
	}
	writeMu.Lock()
	defer writeMu.Unlock()

	doc := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &doc) // a corrupt file is overwritten with a valid one
	}
	mutate(doc)

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(append(out, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// AddAllowRule adds a permission allow pattern to the settings file (idempotent,
// sorted), preserving other keys. The canonical writer behind RuleStore grants.
func AddAllowRule(path, pattern string) error {
	return Persist(path, func(doc map[string]any) {
		perms, _ := doc["permissions"].(map[string]any)
		if perms == nil {
			perms = map[string]any{}
		}
		allow := stringsOf(perms["allow"])
		for _, p := range allow {
			if p == pattern {
				return // already present
			}
		}
		allow = append(allow, pattern)
		sort.Strings(allow)
		perms["allow"] = allow
		doc["permissions"] = perms
	})
}

// SetVerifyCommand writes the finalize-verify command into the settings file,
// preserving other keys (P11.d agent self-write — the agent configures the
// project's verification for itself).
func SetVerifyCommand(path, command string) error {
	return Persist(path, func(doc map[string]any) {
		v, _ := doc["verify"].(map[string]any)
		if v == nil {
			v = map[string]any{}
		}
		v["command"] = command
		doc["verify"] = v
	})
}

// EnsureGitignored best-effort appends entry to <root>/.gitignore when it is not
// already listed, so machine-local settings the agent writes are never committed.
func EnsureGitignored(root, entry string) error {
	path := filepath.Join(root, ".gitignore")
	data, _ := os.ReadFile(path)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == entry {
			return nil // already ignored
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	prefix := ""
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		prefix = "\n" // don't glue onto a final line with no trailing newline
	}
	_, err = f.WriteString(prefix + entry + "\n")
	return err
}

func stringsOf(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
