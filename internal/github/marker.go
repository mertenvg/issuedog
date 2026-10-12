package github

import (
	"cmp"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Marker is a hidden issuedog marker in a comment or issue body (SPEC §5):
//
//	<!-- issuedog:<kind> key=value ... -->
type Marker struct {
	Kind   string
	Fields map[string]string
}

// Author is who wrote a comment or issue body, as GitHub reports it.
type Author struct {
	Login string
	// Type is the account type: "User", "Bot" or "Organization".
	Type string
}

var (
	markerRE = regexp.MustCompile(`<!--\s*issuedog:([a-z][a-z0-9-]*)((?:\s+[^\s<>]*)*?)\s*-->`)
	fieldRE  = regexp.MustCompile(`^([a-z][a-z0-9_]*)=([^\s<>]+)$`)
	kindRE   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
)

// fieldOrder is FormatMarker's order for the fields SPEC names, as in its
// examples. Any other field follows them, sorted.
var fieldOrder = []string{"from", "to", "at", "origin", "reason", "by", "kind", "key", "ack",
	"plan", "rev", "body", "item", "verdict", "attempt", "round", "sha", "url", "run", "event", "cmd"}

// FormatMarker writes m as a hidden marker, its fields in a fixed order
// (fieldOrder). A kind, key or value ParseMarkers wouldn't read back is an
// error, so a marker never goes out without its idempotency key. That
// includes any Unicode space, which splits a field when it's parsed.
func FormatMarker(m Marker) (string, error) {
	if !kindRE.MatchString(m.Kind) {
		return "", fmt.Errorf("format marker: bad kind %q", m.Kind)
	}
	rank := func(k string) int {
		if i := slices.Index(fieldOrder, k); i >= 0 {
			return i
		}
		return len(fieldOrder)
	}
	keys := slices.Sorted(maps.Keys(m.Fields))
	slices.SortStableFunc(keys, func(a, b string) int { return cmp.Compare(rank(a), rank(b)) })
	var b strings.Builder
	b.WriteString("<!-- issuedog:" + m.Kind)
	for _, k := range keys {
		f := k + "=" + m.Fields[k]
		if !fieldRE.MatchString(f) || strings.IndexFunc(f, unicode.IsSpace) >= 0 {
			return "", fmt.Errorf("format %s marker: bad field %q", m.Kind, f)
		}
		b.WriteString(" " + f)
	}
	b.WriteString(" -->")
	return b.String(), nil
}

// Escape escapes "<!--" as "&lt;!--", for text the App posts but didn't
// write, so no marker in it is ever read (SPEC §5.1).
func Escape(text string) string {
	return strings.ReplaceAll(text, "<!--", "&lt;!--")
}

// ParseMarkers returns the well-formed markers in body, in order. It doesn't
// check who wrote body; see TrustedMarkers. A marker with a field that isn't
// key=value, an empty value or a repeated key is skipped.
func ParseMarkers(body string) []Marker {
	var markers []Marker
	for _, m := range markerRE.FindAllStringSubmatch(body, -1) {
		fields, ok := parseFields(m[2])
		if !ok {
			continue
		}
		markers = append(markers, Marker{Kind: m[1], Fields: fields})
	}
	return markers
}

func parseFields(s string) (map[string]string, bool) {
	fields := map[string]string{}
	for _, f := range strings.Fields(s) {
		kv := fieldRE.FindStringSubmatch(f)
		if kv == nil {
			return nil, false
		}
		if _, dup := fields[kv[1]]; dup {
			return nil, false
		}
		fields[kv[1]] = kv[2]
	}
	return fields, true
}

// TrustedMarkers returns the markers in body only when author is the App:
// a bot account whose login is "<appSlug>[bot]". Markers anyone else wrote,
// including trusted users and other bots, are ignored (SPEC §5, §6.2).
func TrustedMarkers(author Author, body, appSlug string) []Marker {
	if appSlug == "" || author.Type != "Bot" || author.Login != appSlug+"[bot]" {
		return nil
	}
	return ParseMarkers(body)
}
