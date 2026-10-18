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
	"plan", "rev", "body", "item", "verdict", "attempt", "round", "sha", "url", "run", "event", "cmd",
	"v", "data"}

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

// EncodeValue is s as a marker value holds it, one that ParseMarkers
// reads back and DecodeValue undoes: every byte but a printable ASCII one
// other than "%", ",", "<" and ">" is percent-encoded as %XX. That keeps
// every space, "<" and ">", which a value can't hold, and every "," out of
// it, so a value can also be one of a comma-separated list, as a
// digest-new marker's keys are. Any other text is left as it is. An empty
// s is an empty value, which FormatMarker refuses.
func EncodeValue(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c >= 0x7f || strings.IndexByte("%,<>", c) >= 0 {
			fmt.Fprintf(&b, "%%%02X", c)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// DecodeValue undoes EncodeValue: each %XX, with two hex digits in either
// case, is the byte XX, and any other byte is itself. A "%" without two
// hex digits after it is an error.
func DecodeValue(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		hi, lo := -1, -1
		if i+2 < len(s) {
			hi, lo = unhex(s[i+1]), unhex(s[i+2])
		}
		if hi < 0 || lo < 0 {
			return "", fmt.Errorf("decode marker value %q: %% at %d without two hex digits", s, i)
		}
		b.WriteByte(byte(hi<<4 | lo))
		i += 2
	}
	return b.String(), nil
}

// unhex is hex digit c's value, or -1 if it isn't one.
func unhex(c byte) int {
	switch {
	case '0' <= c && c <= '9':
		return int(c - '0')
	case 'a' <= c && c <= 'f':
		return int(c-'a') + 10
	case 'A' <= c && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
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
