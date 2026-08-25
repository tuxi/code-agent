package automation

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// parseRRule parses a minimal RFC5545-style recurrence rule (the subset the PRD
// scenarios need) in a fixed timezone and returns the next firing time strictly
// after `after`. It materializes the result so the scheduler can do plain integer
// comparisons on next_run_at instead of re-parsing the RRULE on every tick.
//
// Supported: FREQ in {SECONDLY, MINUTELY, HOURLY, DAILY, WEEKLY, MONTHLY, YEARLY},
// INTERVAL, BYDAY (for WEEKLY and MONTHLY), BYHOUR, BYMINUTE. A rule using an
// unsupported part returns an explicit error rather than silently mis-firing.
//
// `loc` must be non-nil. It is the automation's persisted creation timezone.
type rrule struct {
	freq     string
	interval int
	byDay    map[string]bool // weekday abbrev (MO..SU) -> present
	byHour   int             // -1 = unspecified
	byMinute int             // -1 = unspecified
}

func parseRRule(expr string) (*rrule, error) {
	r := &rrule{interval: 1, byHour: -1, byMinute: -1, byDay: map[string]bool{}}
	parts := strings.Split(expr, ";")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			return nil, fmt.Errorf("rrule: malformed part %q", p)
		}
		switch strings.ToUpper(strings.TrimSpace(k)) {
		case "FREQ":
			r.freq = strings.ToUpper(strings.TrimSpace(v))
		case "INTERVAL":
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 1 {
				return nil, fmt.Errorf("rrule: invalid INTERVAL %q", v)
			}
			r.interval = n
		case "BYDAY":
			days := strings.Split(v, ",")
			for _, d := range days {
				d = strings.ToUpper(strings.TrimSpace(d))
				if !validWeekday(d) {
					return nil, fmt.Errorf("rrule: unsupported BYDAY %q", d)
				}
				r.byDay[strings.TrimLeft(d, "+-")] = true
			}
		case "BYHOUR":
			h, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || h < 0 || h > 23 {
				return nil, fmt.Errorf("rrule: invalid BYHOUR %q", v)
			}
			r.byHour = h
		case "BYMINUTE":
			m, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || m < 0 || m > 59 {
				return nil, fmt.Errorf("rrule: invalid BYMINUTE %q", v)
			}
			r.byMinute = m
		default:
			// Unknown parts (BYMONTH, BYSETPOS, COUNT, UNTIL, ...) are not in the
			// supported subset. Fail loudly rather than mis-fire.
			return nil, fmt.Errorf("rrule: unsupported part %q", k)
		}
	}
	if r.freq == "" {
		return nil, fmt.Errorf("rrule: missing FREQ")
	}
	switch r.freq {
	case "SECONDLY", "MINUTELY", "HOURLY", "DAILY", "WEEKLY", "MONTHLY", "YEARLY":
	default:
		return nil, fmt.Errorf("rrule: unsupported FREQ %q", r.freq)
	}
	return r, nil
}

func validWeekday(d string) bool {
	switch strings.TrimLeft(d, "+-") {
	case "MO", "TU", "WE", "TH", "FR", "SA", "SU":
		return true
	}
	return false
}

// Next returns the next firing time strictly after `after`, in loc. It walks
// forward in steps of interval * freq from the candidate rounded up to the rule's
// granularity, so it always lands on the next legitimate occurrence. The walk may
// advance multiple base units (e.g. DAILY with BYHOUR) before finding a match.
func (r *rrule) Next(after time.Time, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	a := after.In(loc)

	switch r.freq {
	case "SECONDLY":
		return r.nextSecondly(a, loc)
	case "MINUTELY":
		return r.nextMinutely(a, loc)
	case "HOURLY":
		return r.nextHourly(a, loc)
	case "DAILY":
		return r.nextDaily(a, loc)
	case "WEEKLY":
		return r.nextWeekly(a, loc)
	case "MONTHLY":
		return r.nextMonthly(a, loc)
	case "YEARLY":
		return r.nextYearly(a, loc)
	}
	return time.Time{}, fmt.Errorf("rrule: unsupported freq %q", r.freq)
}

// Sub-daily frequencies (SECONDLY/MINUTELY/HOURLY) are fixed-duration units, but
// the interval is aligned to the clock grid, not to the last firing: INTERVAL=30
// MINUTELY fires at :00 and :30 of each hour, INTERVAL=2 HOURLY fires at even
// hours. Stepping from the grid floor keeps the wall-clock alignment stable.
func (r *rrule) nextSecondly(after time.Time, loc *time.Location) (time.Time, error) {
	c := after.Truncate(time.Second)
	for {
		if c.Second()%r.interval == 0 && c.After(after) {
			return c, nil
		}
		c = c.Add(time.Second)
	}
}

func (r *rrule) nextMinutely(after time.Time, loc *time.Location) (time.Time, error) {
	c := after.Truncate(time.Minute)
	for {
		if c.Minute()%r.interval == 0 && c.After(after) {
			return c, nil
		}
		c = c.Add(time.Minute)
	}
}

// nextHourly aligns to the hour grid (hour % interval == 0) and, when BYMINUTE is
// present, fixes the minute-of-hour.
func (r *rrule) nextHourly(after time.Time, loc *time.Location) (time.Time, error) {
	c := after.Truncate(time.Hour)
	for {
		if c.Hour()%r.interval == 0 {
			target := c
			if r.byMinute >= 0 {
				target = time.Date(c.Year(), c.Month(), c.Day(), c.Hour(), r.byMinute, 0, 0, c.Location())
			}
			if target.After(after) {
				return target, nil
			}
		}
		c = c.Add(time.Hour)
	}
}

// buildTarget constructs the wall-clock firing time on a given day from the
// rule's optional BYHOUR/BYMINUTE, in loc. When neither is set, it returns the
// start of the day (midnight) — a DAILY/WEEKLY rule with no BYHOUR fires at 00:00.
func (r *rrule) buildTarget(day time.Time) time.Time {
	hour := 0
	if r.byHour >= 0 {
		hour = r.byHour
	}
	minute := 0
	if r.byMinute >= 0 {
		minute = r.byMinute
	}
	// day carries the correct location; preserve it via the Date fields.
	return time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, day.Location())
}

// nextDaily handles DAILY with an optional BYHOUR/BYMINUTE target. It advances
// day-by-day by interval, and for each candidate day builds the target time.
func (r *rrule) nextDaily(after time.Time, loc *time.Location) (time.Time, error) {
	dayStart := time.Date(after.Year(), after.Month(), after.Day(), 0, 0, 0, 0, loc)
	for {
		target := r.buildTarget(dayStart)
		if target.After(after) {
			return target, nil
		}
		dayStart = dayStart.AddDate(0, 0, r.interval)
		if dayStart.Year() > after.Year()+100 {
			return time.Time{}, fmt.Errorf("rrule: no next occurrence found")
		}
	}
}

// nextWeekly finds the next occurrence on the BYDAY set within the week, then
// advances by interval weeks. Without BYDAY, it uses the weekday of `after`.
func (r *rrule) nextWeekly(after time.Time, loc *time.Location) (time.Time, error) {
	weekStart := startOfWeek(after.In(loc))
	for {
		for d := 0; d < 7; d++ {
			day := weekStart.AddDate(0, 0, d)
			if len(r.byDay) > 0 && !r.byDay[weekdayAbbr(day.Weekday())] {
				continue
			}
			target := r.buildTarget(day)
			if target.After(after) {
				return target, nil
			}
		}
		weekStart = weekStart.AddDate(0, 0, 7*r.interval)
		if weekStart.Year() > after.Year()+100 {
			return time.Time{}, fmt.Errorf("rrule: no next occurrence found")
		}
	}
}

func startOfWeek(t time.Time) time.Time {
	wd := int(t.Weekday()) // Sunday=0
	// Treat Monday as week start (ISO). Shift back to Monday.
	offset := (int(time.Monday) - wd + 7) % 7
	return time.Date(t.Year(), t.Month(), t.Day()-offset, 0, 0, 0, 0, t.Location())
}

func weekdayAbbr(wd time.Weekday) string {
	switch wd {
	case time.Monday:
		return "MO"
	case time.Tuesday:
		return "TU"
	case time.Wednesday:
		return "WE"
	case time.Thursday:
		return "TH"
	case time.Friday:
		return "FR"
	case time.Saturday:
		return "SA"
	case time.Sunday:
		return "SU"
	}
	return "MO"
}

// nextMonthly handles MONTHLY: a day-of-month (default = after's day, clamped) or
// BYDAY (e.g. 1MO = first Monday). Advances by interval months.
func (r *rrule) nextMonthly(after time.Time, loc *time.Location) (time.Time, error) {
	y, m := after.Year(), after.Month()
	for {
		var day time.Time
		if len(r.byDay) > 0 {
			monthDays := daysInMonth(y, m)
			found := false
			for d := 1; d <= monthDays; d++ {
				cand := time.Date(y, m, d, 0, 0, 0, 0, loc)
				if r.byDay[weekdayAbbr(cand.Weekday())] {
					day = cand
					found = true
					break
				}
			}
			if !found {
				y, m = addMonths(y, m, r.interval)
				continue
			}
		} else {
			dom := after.Day()
			maxDay := daysInMonth(y, m)
			if dom > maxDay {
				dom = maxDay
			}
			day = time.Date(y, m, dom, 0, 0, 0, 0, loc)
		}
		target := r.buildTarget(day)
		if target.After(after) && target.Month() == m && target.Year() == y {
			return target, nil
		}
		y, m = addMonths(y, m, r.interval)
		if y > after.Year()+100 {
			return time.Time{}, fmt.Errorf("rrule: no next occurrence found")
		}
	}
}

// nextYearly handles YEARLY: a month+day (default = after's) or BYDAY. Advances
// by interval years.
func (r *rrule) nextYearly(after time.Time, loc *time.Location) (time.Time, error) {
	y := after.Year()
	for {
		day := time.Date(y, after.Month(), after.Day(), 0, 0, 0, 0, loc)
		target := r.buildTarget(day)
		if target.After(after) {
			return target, nil
		}
		y += r.interval
		if y > after.Year()+100 {
			return time.Time{}, fmt.Errorf("rrule: no next occurrence found")
		}
	}
}

func addMonths(y int, m time.Month, n int) (int, time.Month) {
	total := int(m) - 1 + n
	return y + total/12, time.Month(total%12 + 1)
}

func daysInMonth(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
