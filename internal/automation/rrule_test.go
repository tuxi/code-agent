package automation

import (
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load location %q: %v", name, err)
	}
	return loc
}

func TestRRuleDaily(t *testing.T) {
	loc := mustLoc(t, "America/Los_Angeles")
	// after = 2026-08-25 10:00 PDT, rule = every day at 16:00
	r, err := parseRRule("FREQ=DAILY;BYHOUR=16;BYMINUTE=0")
	if err != nil {
		t.Fatal(err)
	}
	after := time.Date(2026, 8, 25, 10, 0, 0, 0, loc)
	next, err := r.Next(after, loc)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 8, 25, 16, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Fatalf("daily next = %v, want %v", next, want)
	}
}

func TestRRuleDailyAfterTarget(t *testing.T) {
	loc := mustLoc(t, "America/Los_Angeles")
	r, _ := parseRRule("FREQ=DAILY;BYHOUR=9")
	// after is past 9:00 today, so next is tomorrow 9:00
	after := time.Date(2026, 8, 25, 12, 0, 0, 0, loc)
	next, _ := r.Next(after, loc)
	want := time.Date(2026, 8, 26, 9, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Fatalf("daily-after next = %v, want %v", next, want)
	}
}

func TestRRuleMinutelyInterval(t *testing.T) {
	loc := mustLoc(t, "UTC")
	r, _ := parseRRule("FREQ=MINUTELY;INTERVAL=30")
	after := time.Date(2026, 8, 25, 10, 7, 0, 0, loc)
	next, _ := r.Next(after, loc)
	want := time.Date(2026, 8, 25, 10, 30, 0, 0, loc)
	if !next.Equal(want) {
		t.Fatalf("minutely next = %v, want %v", next, want)
	}
}

func TestRRuleWeeklyByDay(t *testing.T) {
	loc := mustLoc(t, "UTC")
	// every Monday at 9:00 (FREQ=WEEKLY;BYDAY=MO;BYHOUR=9)
	r, _ := parseRRule("FREQ=WEEKLY;BYDAY=MO;BYHOUR=9")
	// 2026-08-25 is a Tuesday. Next Monday = 2026-08-31.
	after := time.Date(2026, 8, 25, 0, 0, 0, 0, loc)
	next, _ := r.Next(after, loc)
	want := time.Date(2026, 8, 31, 9, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Fatalf("weekly next = %v, want %v", next, want)
	}
}

func TestRRuleMonthly(t *testing.T) {
	loc := mustLoc(t, "UTC")
	// monthly on the reference day (creation day = 25th) at 9:00
	r, _ := parseRRule("FREQ=MONTHLY;BYHOUR=9;BYMINUTE=0")
	// after is past 9:00 on the 25th, so next is the 25th of next month.
	after := time.Date(2026, 8, 25, 10, 0, 0, 0, loc)
	next, _ := r.Next(after, loc)
	want := time.Date(2026, 9, 25, 9, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Fatalf("monthly next = %v, want %v", next, want)
	}
}

func TestRRuleUnsupportedPart(t *testing.T) {
	if _, err := parseRRule("FREQ=DAILY;BYMONTH=5"); err == nil {
		t.Fatal("expected error for unsupported BYMONTH")
	}
}

// TestRRuleDST verifies wall-clock behavior across a DST boundary. On 2026-03-08
// (US spring-forward at 2:00), a 9:00 daily firing stays at 9:00 local.
func TestRRuleDST(t *testing.T) {
	loc := mustLoc(t, "America/Los_Angeles")
	r, _ := parseRRule("FREQ=DAILY;BYHOUR=9")
	// The day before DST change, after 9:00 already passed.
	after := time.Date(2026, 3, 7, 10, 0, 0, 0, loc)
	next, _ := r.Next(after, loc)
	if next.Hour() != 9 {
		t.Fatalf("DST daily next hour = %d, want 9", next.Hour())
	}
	// After DST change day, still 9:00 local.
	after2 := time.Date(2026, 3, 8, 10, 0, 0, 0, loc)
	next2, _ := r.Next(after2, loc)
	if next2.Hour() != 9 {
		t.Fatalf("DST-post daily next hour = %d, want 9", next2.Hour())
	}
}
