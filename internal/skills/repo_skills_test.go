package skills

import "testing"

// TestRepoSeedSkillsAreValid loads the repo's own skills/ directory so a
// malformed seed SKILL.md is caught here rather than at runtime. Skips cleanly
// if no skills are committed.
func TestRepoSeedSkillsAreValid(t *testing.T) {
	reg, err := Load("../../skills")
	if err != nil {
		t.Fatalf("loading repo skills: %v", err)
	}
	if len(reg.Skipped) != 0 {
		t.Errorf("repo seed skills failed to parse: %v", reg.Skipped)
	}
	if reg.Len() == 0 {
		t.Skip("no seed skills committed")
	}
	for _, m := range reg.Index() {
		if len(m.Description) < 20 {
			t.Errorf("skill %q description is too short to be a good trigger: %q", m.Name, m.Description)
		}
		if m.Version == "" {
			t.Errorf("skill %q is missing a version", m.Name)
		}
	}
}
