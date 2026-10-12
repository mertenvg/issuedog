package agent

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mertenvg/logr/v2"

	// The runtime image may not ship tzdata, and reset times name a zone.
	_ "time/tzdata"
)

// Backoff for usage limits whose reset time can't be read (SPEC §3.3).
const (
	minBackoff = 15 * time.Minute
	maxBackoff = 2 * time.Hour
	// maxReset bounds how far ahead a reset time may be; further is unusable.
	maxReset = 8 * 24 * time.Hour
	// resetGrace is how long a passed clock-only reset still counts as today's.
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
	// errNoReset means the text names no reset time, so the backoff applies.
	errNoReset = errors.New("no reset time in the text")
)

// Gate holds stage work back after a run hits a Claude usage limit. It never
// blocks: the loop asks Open each pass and keeps doing steps 1 and 2 meanwhile.
type Gate struct {
	pattern *regexp.Regexp
	backoff time.Duration
	openAt  time.Time
}

// NewGate returns an open gate that matches usage limits by usage_limit.pattern.
func NewGate(pattern string) (*Gate, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("usage_limit.pattern: %w", err)
	}
	return &Gate{pattern: re, backoff: minBackoff}, nil
}

// Observe records a finished run and reports whether it hit a usage limit,
// which doesn't count as an attempt (SPEC §8.3). Only a failed run (failed: a
// non-success subtype or is_error) can. The gate then stays closed until the
// reset time in text, or for the backoff, which doubles from 15 minutes to a
// 2-hour cap. Any run that didn't hit a limit resets the backoff.
func (g *Gate) Observe(now time.Time, failed bool, text string) (limited bool) {
	if !failed || !g.pattern.MatchString(text) {
		g.backoff = minBackoff
		return false
	}
	reset, err := parseReset(now, text)
	if err == nil {
		g.openAt = reset
		return true
	}
	if !errors.Is(err, errNoReset) {
		logr.Warnf("usage limit: using the backoff, since the reset time can't be used: %v", err)
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

// parseReset reads a Unix time after "|", or a clock time after "resets" with
// an optional (IANA zone), meaning its next occurrence after now. It returns
// errNoReset for text with neither, and an error for one it can't use.
func parseReset(now time.Time, text string) (time.Time, error) {
	var reset time.Time
	if m := resetUnixRE.FindStringSubmatch(text); m != nil {
		secs, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("reset time %q: %w", m[1], err)
		}
		reset = time.Unix(secs, 0)
	} else if m := resetClockRE.FindStringSubmatch(text); m != nil {
		var err error
		if reset, err = nextClock(now, clock{month: m[1], day: m[2], year: m[3], hour: m[4], minute: m[5], ampm: m[6], zone: m[7]}); err != nil {
			return time.Time{}, fmt.Errorf("reset time %q: %w", m[0], err)
		}
	} else {
		return time.Time{}, errNoReset
	}
	if !reset.After(now) || reset.Sub(now) > maxReset {
		return time.Time{}, fmt.Errorf("reset time %s is not within %v after %s", reset.Format(time.RFC3339), maxReset, now.Format(time.RFC3339))
	}
	return reset, nil
}

// clock is a printed reset time's parts. Every part but hour may be empty.
type clock struct{ month, day, year, hour, minute, ampm, zone string }

// nextClock returns the next time after now that c names, in c's zone or now's:
// today or tomorrow without a date, or the next such date without a year.
func nextClock(now time.Time, c clock) (time.Time, error) {
	h, herr := number("hour", c.hour)
	m, merr := number("minute", c.minute)
	if err := errors.Join(herr, merr); err != nil {
		return time.Time{}, err
	}
	if c.ampm != "" {
		if h < 1 || h > 12 {
			return time.Time{}, fmt.Errorf("hour %d is not a 12-hour clock hour", h)
		}
		h %= 12
		if strings.EqualFold(c.ampm, "pm") {
			h += 12
		}
	}
	if h > 23 || m > 59 {
		return time.Time{}, fmt.Errorf("%02d:%02d is not a time of day", h, m)
	}
	loc := now.Location()
	if c.zone != "" {
		l, err := time.LoadLocation(strings.TrimSpace(c.zone))
		if err != nil {
			return time.Time{}, fmt.Errorf("zone: %w", err)
		}
		loc = l
	}
	local := now.In(loc)

	if c.month == "" {
		// The next instant after now, unless one passed within resetGrace (also
		// yesterday's, near midnight): parseReset then refuses it for the backoff.
		var next time.Time
		for off := -1; off <= 1; off++ {
			d := time.Date(local.Year(), local.Month(), local.Day()+off, 12, 0, 0, 0, loc)
			for _, t := range instants(loc, d.Year(), d.Month(), d.Day(), h, m) {
				if !t.After(now) {
					if now.Sub(t) <= resetGrace {
						return t, nil
					}
					continue
				}
				if next.IsZero() || t.Before(next) {
					next = t
				}
			}
		}
		return next, nil // zero if none, which parseReset refuses
	}

	month := time.Month(slices.Index(months, strings.ToLower(c.month)[:3]) + 1)
	day, derr := number("day", c.day)
	year, yerr := number("year", c.year)
	if err := errors.Join(derr, yerr); err != nil {
		return time.Time{}, err
	}
	years := []int{year}
	if c.year == "" {
		years = []int{local.Year(), local.Year() + 1}
	}
	for _, y := range years {
		for _, t := range instants(loc, y, month, day, h, m) {
			if t.After(now) {
				return t, nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("%s %d %v has no time after now", month, day, years)
}

// number parses one numeric part of a printed reset time. An empty part is 0.
func number(part, s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s %q: %w", part, s, err)
	}
	return n, nil
}

// instants returns, in order, every instant reading this date and time in loc:
// two in a DST fall-back hour, none in a DST gap or for Feb 30. DST shifts are ≤ 2h.
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
