package session

import "testing"

// NeedCompaction is now a pure threshold check. The cooldown and cache-affinity
// logic lives in Runner.maybeCompact (see TestRunTurnIneffectiveCompactionCoolsDown
// in the agent package), where runtime telemetry drives the adaptive cooldown ratio.
func TestNeedCompactionReturnsThresholdCheck(t *testing.T) {
	s := &Session{PromptTokens: 95000, CompactThreshold: 90000}
	if !s.NeedCompaction() {
		t.Fatal("over threshold must need compaction")
	}

	// Even after an ineffective compaction, NeedCompaction still says yes —
	// the cooldown gate is in Runner.maybeCompact, not here.
	s.RecordCompaction(95000, 100)
	s.FinalizeCompaction(92000)
	s.PromptTokens = 92000
	if !s.NeedCompaction() {
		t.Fatal("NeedCompaction only checks threshold; cooldown is in the Runner")
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
