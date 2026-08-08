package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestTrustStorePath(t *testing.T) {
	p := TrustStorePath("/home/user")
	if p != filepath.Join("/home/user", ".codeagent", "trust.json") {
		t.Fatalf("TrustStorePath = %q", p)
	}
}

func TestTrustStoreLoadMissingFile(t *testing.T) {
	dir := t.TempDir()
	ts, err := LoadTrustStore(dir)
	if err != nil {
		t.Fatalf("missing trust.json should not error: %v", err)
	}
	if ts == nil {
		t.Fatal("should return an empty store")
	}
	_, found := ts.Lookup(dir)
	if found {
		t.Fatal("empty store should not find anything")
	}
}

func TestTrustStoreLookupExact(t *testing.T) {
	dir := t.TempDir()
	trusted := true
	ts := &TrustStore{path: filepath.Join(dir, "trust.json"), cache: map[string]*bool{
		dir: &trusted,
	}}
	got, found := ts.Lookup(dir)
	if !found || !got {
		t.Fatalf("Lookup(%q) = (%v, %v), want (true, true)", dir, got, found)
	}
}

func TestTrustStoreLookupUntrusted(t *testing.T) {
	dir := t.TempDir()
	untrusted := false
	ts := &TrustStore{path: filepath.Join(dir, "trust.json"), cache: map[string]*bool{
		dir: &untrusted,
	}}
	got, found := ts.Lookup(dir)
	if !found || got {
		t.Fatalf("Lookup(%q) = (%v, %v), want (false, true)", dir, got, found)
	}
}

func TestTrustStoreLookupNullEntry(t *testing.T) {
	dir := t.TempDir()
	ts := &TrustStore{path: filepath.Join(dir, "trust.json"), cache: map[string]*bool{
		dir: nil, // null in JSON = "asked but undecided"
	}}
	_, found := ts.Lookup(dir)
	if found {
		t.Fatal("null entry should not be found (falls through)")
	}
}

func TestTrustStoreLookupParentInheritance(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "a", "b", "c")
	trusted := true
	ts := &TrustStore{path: filepath.Join(parent, "trust.json"), cache: map[string]*bool{
		parent: &trusted,
	}}
	got, found := ts.Lookup(child)
	if !found || !got {
		t.Fatalf("Lookup(%q) = (%v, %v), want (true, true) via parent inheritance", child, got, found)
	}
}

func TestTrustStoreLookupMostSpecificWins(t *testing.T) {
	// Positive inheritance beats negative exact-match-only: when a parent
	// is untrusted, children are NOT blocked — they inherit from the
	// nearest trusted ancestor.
	grandparent := t.TempDir()
	parent := filepath.Join(grandparent, "a")
	child := filepath.Join(parent, "b")
	grandTrue := true
	parentFalse := false
	ts := &TrustStore{path: filepath.Join(grandparent, "trust.json"), cache: map[string]*bool{
		grandparent: &grandTrue,
		parent:      &parentFalse,
	}}
	got, found := ts.Lookup(child)
	if !found || !got {
		t.Fatalf("Lookup(%q) = (%v, %v), want (true, true) — negative parent is exact-match only, grandparent trust inherits to child", child, got, found)
	}
	// The parent itself should still be rejected (exact negative match).
	got2, found2 := ts.Lookup(parent)
	if !found2 || got2 {
		t.Fatalf("Lookup(%q) = (%v, %v), want (false, true) — exact match on parent should be untrusted", parent, got2, found2)
	}
}

func TestTrustStoreStoreAndLookup(t *testing.T) {
	dir := t.TempDir()
	ts, err := LoadTrustStore(dir)
	if err != nil {
		t.Fatalf("LoadTrustStore: %v", err)
	}
	if err := ts.Store(dir, true); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, found := ts.Lookup(dir)
	if !found || !got {
		t.Fatalf("after Store, Lookup = (%v, %v), want (true, true)", got, found)
	}
}

func TestTrustStorePersistsToDisk(t *testing.T) {
	dir := t.TempDir()
	ts, err := LoadTrustStore(dir)
	if err != nil {
		t.Fatalf("LoadTrustStore: %v", err)
	}
	if err := ts.Store(dir, false); err != nil {
		t.Fatalf("Store: %v", err)
	}
	// Reload from disk.
	ts2, err := LoadTrustStore(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, found := ts2.Lookup(dir)
	if !found || got {
		t.Fatalf("after reload, Lookup = (%v, %v), want (false, true)", got, found)
	}
}

func TestTrustStoreCorruptFile(t *testing.T) {
	dir := t.TempDir()
	p := TrustStorePath(dir)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte("not json{{{"), 0o644); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	ts, err := LoadTrustStore(dir)
	if err != nil {
		t.Fatalf("corrupt file should not error, just start fresh: %v", err)
	}
	if ts == nil {
		t.Fatal("should return an empty store")
	}
	_, found := ts.Lookup(dir)
	if found {
		t.Fatal("corrupt file should result in empty store")
	}
}

func TestTrustHasRequiringResources(t *testing.T) {
	dir := t.TempDir()
	if HasTrustRequiringResources(dir) {
		t.Fatal("empty dir should have no trust-requiring resources")
	}
	// Create a .codeagent/settings.json
	settingsDir := filepath.Join(dir, ".codeagent")
	os.MkdirAll(settingsDir, 0o755)
	os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte("{}"), 0o644)
	if !HasTrustRequiringResources(dir) {
		t.Fatal("dir with .codeagent/settings.json should require trust")
	}
}

func TestResolveTrustNoResources(t *testing.T) {
	dir := t.TempDir()
	// Trust is now universal — even without resources, the full chain runs.
	// With no policy, no CLI, no store → fail-closed.
	trusted, reason, err := ResolveTrust(t.Context(), dir, false, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if trusted {
		t.Fatalf("(%v, %q), want fail-closed without any decision source", trusted, reason)
	}
}

func TestResolveTrustCLIOverrideTrust(t *testing.T) {
	dir := t.TempDir()
	override := true
	trusted, reason, err := ResolveTrust(t.Context(), dir, true, &override, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !trusted || reason != "CLI flag --trust" {
		t.Fatalf("(%v, %q), want (true, 'CLI flag --trust')", trusted, reason)
	}
}

func TestResolveTrustCLIOverrideNoTrust(t *testing.T) {
	dir := t.TempDir()
	override := false
	trusted, reason, err := ResolveTrust(t.Context(), dir, true, &override, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if trusted || reason != "CLI flag --no-trust" {
		t.Fatalf("(%v, %q), want (false, 'CLI flag --no-trust')", trusted, reason)
	}
}

func TestResolveTrustPersistedDecision(t *testing.T) {
	dir := t.TempDir()
	ts := &TrustStore{path: filepath.Join(dir, "trust.json"), cache: map[string]*bool{}}
	trusted := true
	ts.cache[dir] = &trusted
	result, reason, err := ResolveTrust(t.Context(), dir, true, nil, nil, ts, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result || reason != "persisted trust decision" {
		t.Fatalf("(%v, %q), want (true, persisted)", result, reason)
	}
}

func TestResolveTrustDefaultNever(t *testing.T) {
	dir := t.TempDir()
	defaultTrust := false
	result, reason, err := ResolveTrust(t.Context(), dir, true, nil, nil, nil, &defaultTrust, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result || reason != "default: never trust" {
		t.Fatalf("(%v, %q), want (false, 'default: never trust')", result, reason)
	}
}

func TestResolveTrustFailClosed(t *testing.T) {
	dir := t.TempDir()
	// No CLI override, no hooks, no store, no default, no policy.
	result, reason, err := ResolveTrust(t.Context(), dir, true, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result {
		t.Fatalf("(%v, %q), want (false, fail-closed)", result, reason)
	}
}

func TestTrustStoreStoreJSONFormat(t *testing.T) {
	dir := t.TempDir()
	ts, _ := LoadTrustStore(dir)
	ts.Store(dir, true)
	// Verify the file has valid JSON with expected structure.
	data, err := os.ReadFile(TrustStorePath(dir))
	if err != nil {
		t.Fatalf("read trust.json: %v", err)
	}
	var m map[string]*bool
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, string(data))
	}
	abs, _ := filepath.Abs(dir)
	v, ok := m[filepath.Clean(abs)]
	if !ok || v == nil || !*v {
		t.Fatalf("stored JSON does not contain trusted entry: %v", m)
	}
}
