package agent

import (
	"strings"
	"testing"
	"time"

	"code-agent/internal/model"
)

func TestWithCurrentDate_AppendsToSystemMessage(t *testing.T) {
	now := time.Date(2026, 7, 3, 15, 0, 0, 0, time.UTC) // a Friday
	msgs := []model.Message{
		{Role: model.RoleSystem, Content: "You are CodeAgent."},
		{Role: model.RoleUser, Content: "hi"},
	}

	out := withCurrentDate(msgs, now)

	want := "The current date is 2026-07-03 (Friday)."
	if !strings.Contains(out[0].Content, want) {
		t.Errorf("system message missing date line %q:\n%s", want, out[0].Content)
	}
	// Copy-on-write: the session's persisted messages must be untouched.
	if strings.Contains(msgs[0].Content, "current date") {
		t.Errorf("original system message was mutated: %s", msgs[0].Content)
	}
	if out[1].Content != "hi" {
		t.Errorf("non-system message changed: %q", out[1].Content)
	}
}

func TestWithCurrentDate_StripsStaleBakedDate(t *testing.T) {
	// A session persisted by the older Builder carries a date frozen at creation
	// time; it must be replaced, not joined by a contradicting second date.
	now := time.Date(2026, 7, 3, 15, 0, 0, 0, time.UTC)
	msgs := []model.Message{
		{Role: model.RoleSystem, Content: "You are CodeAgent.\n\nThe current date is 2026-06-28 (Sunday)."},
	}

	out := withCurrentDate(msgs, now)

	if strings.Contains(out[0].Content, "2026-06-28") {
		t.Errorf("stale baked date survived:\n%s", out[0].Content)
	}
	if got := strings.Count(out[0].Content, "The current date is"); got != 1 {
		t.Errorf("want exactly 1 date line, got %d:\n%s", got, out[0].Content)
	}
	if !strings.Contains(out[0].Content, "2026-07-03 (Friday)") {
		t.Errorf("fresh date missing:\n%s", out[0].Content)
	}
}

func TestWithCurrentDate_NoSystemMessage(t *testing.T) {
	now := time.Now()
	msgs := []model.Message{{Role: model.RoleUser, Content: "hi"}}
	out := withCurrentDate(msgs, now)
	if len(out) != 1 || out[0].Content != "hi" {
		t.Errorf("messages without a system head must pass through unchanged: %+v", out)
	}
	if withCurrentDate(nil, now) != nil {
		t.Error("nil messages must pass through as nil")
	}
}
