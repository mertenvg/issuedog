package agent

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	// The runtime image may not ship tzdata, and reset times name a zone.
	_ "time/tzdata"
)

// Backoff for usage limits whose reset time can't be read (SPEC §3.3).
const (
	minBackoff = 15 * time.Minute
	maxBackoff = 2 * time.Hour
	// maxReset bounds how far ahead a parsed reset time may be. Anything
	// further is treated as unreadable.
	maxReset = 8 * 24 * time.Hour
	// resetGrace is how long after a clock-only reset time it still counts
	// as today's.
	resetGrace = 5 * time.Minute
)

var (
	// "...usage limit reached|1760000000": a Unix time in seconds.
	resetUnixRE = regexp.MustCompile(`\|(\d{9,11})\b`)
	// "...resets 3pm", "resets at 3:30 pm (Europe/Amsterdam)", "resets 15:00",
	// and for resets more than a day away "resets Oct 14, 3pm (UTC)".
	resetClockRE = regexp.MustCompile(`(?i)\bresets\s+(?:at\s+)?` +
		`(?:(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+(\d{1,2})(?:,?\s+(\d{4}))?,?\s+(?:at\s+)?)?` +
		`(\d{1,2})(?::(\d{2}))?\s*(am|pm)?(?:\s*\(([^)]+)\))?`)

	months = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
)

// Gate holds stage work back after a run hits a Claude usage limit. It never
// blocks: the loop asks Open on each pass and keeps doing its other steps
// (loop steps 1 and 2) while the gate is closed.
type Gate struct {
	pattern *regexp.Regexp
	backoff time.Duration
	openAt  time.Time
}

// NewGate returns an open gate that recognizes usage limits by pattern
// (usage_limit.pattern in config).
func NewGate(pattern string) (*Gate, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("usage_limit.pattern: %w", err)
	}
	return &Gate{pattern: re, backoff: minBackoff}, nil
}

// Observe records a finished run. failed is whether the run failed (a
// non-success subtype or is_error), and text is its result text. Only a
// failed run can hit a usage limit, so a successful run that merely mentions
// one doesn't close the gate.
//
// It reports whether the run hit a usage limit. Such a run doesn't count as
// an attempt toward the retry cap (SPEC §8.3). The gate stays closed until the
// reset time in the text, or for the current backoff, which doubles from 15
// minutes to a 2-hour cap. Any run that didn't hit a limit resets the backoff.
func (g *Gate) Observe(now time.Time, failed bool, text string) (limited bool) {
	if !failed || !g.pattern.MatchString(text) {
		g.backoff = minBackoff
		return false
	}
	if reset, ok := parseReset(now, text); ok {
		g.openAt = reset
		return true
	}
	g.openAt = now.Add(g.backoff)
	g.backoff = min(2*g.backoff, maxBackoff)
	return true
}

// Open reports whether stage work may run at now.
func (g *Gate) Open(now time.Time) bool {
	return !now.Before(g.openAt)
}

// OpenAt is when stage work may run next.
func (g *Gate) OpenAt() time.Time {
	return g.openAt
}

// parseReset reads a reset time from usage-limit text. It accepts a Unix time
// after a "|", or a clock time after "resets", optionally followed by an IANA
// zone in parentheses. A clock time means its next occurrence after now, in
// that zone or else in now's location. It returns false unless the time is
// after now and within maxReset.
func parseReset(now time.Time, text string) (time.Time, bool) {
	var reset time.Time
	if m := resetUnixRE.FindStringSubmatch(text); m != nil {
		secs, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		reset = time.Unix(secs, 0)
	} else if m := resetClockRE.FindStringSubmatch(text); m != nil {
		var ok bool
		if reset, ok = nextClock(now, clock{month: m[1], day: m[2], year: m[3], hour: m[4], minute: m[5], ampm: m[6], zone: m[7]}); !ok {
			return time.Time{}, false
		}
	} else {
		return time.Time{}, false
	}
	if !reset.After(now) || reset.Sub(now) > maxReset {
		return time.Time{}, false
	}
	return reset, true
}

// clock is a reset time as printed, split into its parts. Every part but
// hour may be empty.
type clock struct {
	month, day, year         string
	hour, minute, ampm, zone string
}

// nextClock returns the next time after now that c names, in c's zone or else
// in now's location. Without a date that's today or tomorrow. With a month
// and day but no year, it's the next such date.
func nextClock(now time.Time, c clock) (time.Time, bool) {
	h, _ := strconv.Atoi(c.hour)
	m, _ := strconv.Atoi(c.minute) // 0 when empty
	if c.ampm != "" {
		if h < 1 || h > 12 {
			return time.Time{}, false
		}
		h %= 12
		if strings.EqualFold(c.ampm, "pm") {
			h += 12
		}
	}
	if h > 23 || m > 59 {
		return time.Time{}, false
	}
	loc := now.Location()
	if c.zone != "" {
		l, err := time.LoadLocation(strings.TrimSpace(c.zone))
		if err != nil {
			return time.Time{}, false
		}
		loc = l
	}
	local := now.In(loc)

	if c.month == "" {
		// The next instant after now, unless one passed within resetGrace
		// (yesterday too, for one just before midnight). That one is returned
		// as is, so parseReset uses the backoff rather than waiting a day.
		var next time.Time
		for off := -1; off <= 1; off++ {
			d := time.Date(local.Year(), local.Month(), local.Day()+off, 12, 0, 0, 0, loc)
			for _, t := range instants(loc, d.Year(), d.Month(), d.Day(), h, m) {
				if !t.After(now) {
					if now.Sub(t) <= resetGrace {
						return t, true
					}
					continue
				}
				if next.IsZero() || t.Before(next) {
					next = t
				}
			}
		}
		return next, !next.IsZero()
	}

	month := time.Month(slices.Index(months, strings.ToLower(c.month)[:3]) + 1)
	day, _ := strconv.Atoi(c.day)
	first := func(year int) (time.Time, bool) {
		for _, t := range instants(loc, year, month, day, h, m) {
			if t.After(now) {
				return t, true
			}
		}
		return time.Time{}, false
	}
	if c.year != "" {
		year, _ := strconv.Atoi(c.year)
		return first(year)
	}
	if t, ok := first(local.Year()); ok {
		return t, true
	}
	return first(local.Year() + 1)
}

// instants returns, in order, every instant whose wall clock in loc reads the
// given date and time. That's usually one, two during a DST fall-back hour,
// and none for a time skipped by DST or a date that doesn't exist (such as
// Feb 30). DST shifts are at most 2 hours, in steps of 30 minutes.
func instants(loc *time.Location, year int, month time.Month, day, hour, minute int) []time.Time {
	base := time.Date(year, month, day, hour, minute, 0, 0, loc)
	var out []time.Time
	for shift := -2 * time.Hour; shift <= 2*time.Hour; shift += 30 * time.Minute {
		c := base.Add(shift).In(loc)
		if c.Year() == year && c.Month() == month && c.Day() == day && c.Hour() == hour && c.Minute() == minute &&
			!slices.ContainsFunc(out, c.Equal) {
			out = append(out, c)
		}
	}
	return out
}
