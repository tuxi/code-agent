package approve

import "testing"

func TestRuleFingerprintTracksEffectivePolicy(t *testing.T) {
	a := &RuleStore{allow: toSet([]string{"one", "two"}), deny: toSet([]string{"blocked"})}
	b := &RuleStore{allow: toSet([]string{"two", "one"}), deny: toSet([]string{"blocked"})}
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("policy identity depends on map order")
	}
	b.allow["new"] = struct{}{}
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatal("new authority did not change policy identity")
	}
}
