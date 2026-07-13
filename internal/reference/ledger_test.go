package reference

import (
	"encoding/json"
	"testing"
	"time"
)

func TestRegisterAndResolveHandleOnlyInput(t *testing.T) {
	now := time.Date(2026, 7, 12, 6, 47, 0, 0, time.UTC)
	raw := json.RawMessage(`{"snapshotID":"snapshot_7c1d","references":[{"pointer":"/snapshotID","kind":"snapshot","expiresAt":"2026-07-12T06:48:00Z"}]}`)
	entries, substitutions := Register(raw, nil, "session-a", "call-a", now)
	if len(entries) != 1 || entries[0].Handle != "ref_0001" || substitutions["snapshot_7c1d"] != "$ref:ref_0001" {
		t.Fatalf("registration = %#v, %#v", entries, substitutions)
	}
	schema := json.RawMessage(`{"x-codeagent-reference-inputs":[{"pointer":"/snapshotID","kind":"snapshot","mode":"handle_only"}]}`)
	got, err := ResolveInput(json.RawMessage(`{"snapshotID":"$ref:ref_0001"}`), schema, entries, "session-a", now)
	if err != nil || string(got) != `{"snapshotID":"snapshot_7c1d"}` {
		t.Fatalf("resolve = %s, %v", got, err)
	}
}

func TestSnapshotScopedElementResolvesWithinItsSession(t *testing.T) {
	now := time.Date(2026, 7, 12, 6, 47, 0, 0, time.UTC)
	raw := json.RawMessage(`{"matches":[{"elementRef":"el_6"}],"references":[{"pointer":"/matches/0/elementRef","kind":"element","scope":"snapshot","expiresAt":"2026-07-12T06:48:00Z"}]}`)
	entries, substitutions := Register(raw, nil, "session-a", "find-call", now)
	if len(entries) != 1 || entries[0].Scope != "snapshot" || substitutions["el_6"] != "$ref:ref_0001" {
		t.Fatalf("registration = %#v, %#v", entries, substitutions)
	}
	schema := json.RawMessage(`{"x-codeagent-reference-inputs":[{"pointer":"/elementRef","kind":"element","mode":"handle_only"}]}`)
	got, err := ResolveInput(json.RawMessage(`{"elementRef":"$ref:ref_0001"}`), schema, entries, "session-a", now)
	if err != nil || string(got) != `{"elementRef":"el_6"}` {
		t.Fatalf("snapshot-scoped element should resolve: %s, %v", got, err)
	}
}

func TestResolveRejectsGuessedExpiredWrongSessionAndWrongKind(t *testing.T) {
	now := time.Date(2026, 7, 12, 6, 47, 0, 0, time.UTC)
	entries := []Entry{{Handle: "ref_0001", RawValue: "snapshot_real", Kind: "snapshot", SessionID: "s1", ExpiresAt: now.Add(time.Minute), Scope: "session"}}
	schema := json.RawMessage(`{"x-codeagent-reference-inputs":[{"pointer":"/snapshotID","kind":"snapshot","mode":"handle_only"}]}`)
	cases := []struct {
		name, input, session, want string
		at                         time.Time
	}{
		{"guessed", `{"snapshotID":"snapshot_guessed"}`, "s1", "reference_handle_required", now},
		{"missing", `{"snapshotID":"$ref:missing"}`, "s1", "reference_not_found", now},
		{"wrong session", `{"snapshotID":"$ref:ref_0001"}`, "s2", "reference_scope_denied", now},
		{"expired", `{"snapshotID":"$ref:ref_0001"}`, "s1", "reference_expired", now.Add(2 * time.Minute)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveInput(json.RawMessage(tc.input), schema, entries, tc.session, tc.at)
			if err == nil || !contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
		})
	}
	_, err := ResolveInput(json.RawMessage(`{"snapshotID":"snapshot_guessed"}`), schema, entries, "s1", now)
	if err == nil || !contains(err.Error(), `"handle":"$ref:ref_0001"`) || contains(err.Error(), "snapshot_real") {
		t.Fatalf("recovery error must expose only a safe handle hint: %v", err)
	}
	wrongKind := json.RawMessage(`{"x-codeagent-reference-inputs":[{"pointer":"/snapshotID","kind":"element","mode":"handle_only"}]}`)
	if _, err := ResolveInput(json.RawMessage(`{"snapshotID":"$ref:ref_0001"}`), wrongKind, entries, "s1", now); err == nil || !contains(err.Error(), "reference_kind_mismatch") {
		t.Fatalf("wrong kind err = %v", err)
	}
}

func contains(s, part string) bool { return len(s) >= len(part) && (s == part || index(s, part) >= 0) }
func index(s, part string) int {
	for i := 0; i+len(part) <= len(s); i++ {
		if s[i:i+len(part)] == part {
			return i
		}
	}
	return -1
}
