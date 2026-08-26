package automation

import "testing"

func TestNormalizePermissionMode(t *testing.T) {
	cases := []struct {
		raw     string
		want    string
		wantOK  bool
	}{
		{"", "", true}, // inherit the workspace tier
		{"ask", "ask", true},
		{"auto", "auto", true},
		{"full", "full", true},
		{"full_access", "full", true}, // legacy alias
		{"bogus", "", false},
		{"FULL", "", false}, // case-sensitive, like the tier vocabulary
	}
	for _, c := range cases {
		got, ok := NormalizePermissionMode(c.raw)
		if got != c.want || ok != c.wantOK {
			t.Errorf("NormalizePermissionMode(%q) = (%q, %v), want (%q, %v)", c.raw, got, ok, c.want, c.wantOK)
		}
	}
}
