package agent

import (
	"fmt"
	"regexp"
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
)

var (
	// "...usage limit reached|1760000000": a Unix time in seconds.
	resetUnixRE = regexp.MustCompile(`\|(\d{9,11})\b`)
	// "...resets 3pm", "resets at 3:30 pm (Europe/Amsterdam)", "resets 15:00".
	resetClockRE = regexp.MustCompile(`(?i)\bresets\s+(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?(?:\s*\(([^)]+)\))?`)
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
		t, ok := nextClock(now, m[1], m[2], m[3], m[4])
		if !ok {
			return time.Time{}, false
		}
		reset = t
	} else {
		return time.Time{}, false
	}
	if !reset.After(now) || reset.Sub(now) > maxReset {
		return time.Time{}, false
	}
	return reset, true
}

// nextClock returns the next time after now that the clock reads hour:minute
// (with an optional am/pm) in zone, or in now's location when zone is empty.
func nextClock(now time.Time, hour, minute, ampm, zone string) (time.Time, bool) {
	h, _ := strconv.Atoi(hour)
	m := 0
	if minute != "" {
		m, _ = strconv.Atoi(minute)
	}
	switch strings.ToLower(ampm) {
	case "am", "pm":
		if h < 1 || h > 12 {
			return time.Time{}, false
		}
		h %= 12
		if strings.EqualFold(ampm, "pm") {
			h += 12
		}
	}
	if h > 23 || m > 59 {
		return time.Time{}, false
	}
	loc := now.Location()
	if zone != "" {
		l, err := time.LoadLocation(strings.TrimSpace(zone))
		if err != nil {
			return time.Time{}, false
		}
		loc = l
	}
	local := now.In(loc)
	t := time.Date(local.Year(), local.Month(), local.Day(), h, m, 0, 0, loc)
	if !t.After(now) {
		t = time.Date(local.Year(), local.Month(), local.Day()+1, h, m, 0, 0, loc)
	}
	return t, true
}
