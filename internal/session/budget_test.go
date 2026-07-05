package session

import "testing"

// The convergence guard (P12.b): a measured compaction that stayed over the
// threshold puts the session in cooldown — no re-compaction until the prompt
// grows meaningfully past the measured floor. This is what turns a pathological
// configuration (threshold below what compaction can reach) into a warning
// instead of an every-iteration summarize loop.
func TestNeedCompactionCoolsDownAfterIneffectiveCompaction(t *testing.T) {
	s := &Session{PromptTokens: 95000, CompactThreshold: 90000}
	if !s.NeedCompaction() {
		t.Fatal("over threshold with no history must need compaction")
	}

	s.RecordCompaction(95000, 100)
	// Pending stat (not yet measured): the guard must not block — the loop
	// sequence guarantees a measurement before the next check anyway.
	if !s.NeedCompaction() {
		t.Fatal("a pending (unmeasured) stat must not cool the session down")
	}

	// Measured still over the threshold → ineffective → cooldown.
	s.FinalizeCompaction(92000)
	s.PromptTokens = 92000
	if s.NeedCompaction() {
		t.Fatal("ineffective compaction must cool the session down")
	}
	// Slight growth stays inside the cooldown margin (threshold/10).
	s.PromptTokens = 92000 + 8999
	if s.NeedCompaction() {
		t.Fatal("growth inside the margin must stay cooled down")
	}
	// Growth past the margin re-arms compaction.
	s.PromptTokens = 92000 + 9000
	if !s.NeedCompaction() {
		t.Fatal("growth past the margin must re-arm compaction")
	}
}

// An effective past compaction must not block a future one: only the last
// stat's ineffectiveness cools the session down.
func TestNeedCompactionUnaffectedByEffectiveCompaction(t *testing.T) {
	s := &Session{PromptTokens: 95000, CompactThreshold: 90000}
	s.RecordCompaction(95000, 100)
	s.FinalizeCompaction(30000) // effective: well under the threshold

	s.PromptTokens = 91000 // grew back over the threshold later
	if !s.NeedCompaction() {
		t.Fatal("an effective past compaction must not block a new one")
	}
}

func TestNeedCompactionDisabledWithoutThreshold(t *testing.T) {
	s := &Session{PromptTokens: 95000}
	if s.NeedCompaction() {
		t.Fatal("no threshold means compaction is disabled")
	}
}
