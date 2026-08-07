package tui

import (
	"fmt"
	"strings"
	"time"

	"code-agent/internal/session"
)

// formatSessionList is the text output for the /sessions command (printed to
// scrollback), built from the same metas the picker uses.
func formatSessionList(metas []session.Meta) string {
	if len(metas) == 0 {
		return "no saved sessions"
	}
	var b strings.Builder
	for _, m := range metas {
		t := effectiveTitle(m)
		if t == "" {
			t = m.ID
		}
		fmt.Fprintf(&b, "%s — %s · %d msgs · %s\n", t, m.Model, m.MessageCount, humanAgo(m.UpdatedAt))
	}
	return strings.TrimRight(b.String(), "\n")
}

// effectiveTitle returns the best display title for a session, preferring the
// persisted Name (auto-generated or user-set) over the derived Title fallback.
func effectiveTitle(m session.Meta) string {
	if m.Name != "" {
		return m.Name
	}
	return sessionTitle(m.Title)
}

// sessionTitle flattens a first-message into a single clean line.
func sessionTitle(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

// humanAgo renders a coarse relative time ("18 minutes ago").
func humanAgo(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return ago(int(d.Seconds()), "second")
	case d < time.Hour:
		return ago(int(d.Minutes()), "minute")
	case d < 24*time.Hour:
		return ago(int(d.Hours()), "hour")
	default:
		return ago(int(d.Hours()/24), "day")
	}
}

func ago(n int, unit string) string {
	if n <= 1 {
		return "1 " + unit + " ago"
	}
	return fmt.Sprintf("%d %ss ago", n, unit)
}
