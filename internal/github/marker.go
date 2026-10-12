package github

import (
	"regexp"
	"strings"
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
)

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
