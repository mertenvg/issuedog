package state

import (
	"maps"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
)

// CheckFields writes check=<kind> and the percent-encoded value, and
// contains only for a file check that has it. A plain-text Check writes
// nothing.
func TestCheckFields(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		c    Check
		want map[string]string
	}{
		{"plain text", Check{}, nil},
		{"a label", Check{Kind: CheckLabel, Value: "needs design"}, map[string]string{"check": "label", "value": "needs%20design"}},
		{"an issue type", Check{Kind: CheckIssueType, Value: "Bug"}, map[string]string{"check": "issue-type", "value": "Bug"}},
		{"a file", Check{Kind: CheckFile, Value: "docs/a.md"}, map[string]string{"check": "file", "value": "docs/a.md"}},
		{"a file with contains", Check{Kind: CheckFile, Value: "go.mod", Contains: "go 1.25"},
			map[string]string{"check": "file", "value": "go.mod", "contains": "go%201.25"}},
		{"a ruleset", Check{Kind: CheckRuleset, Value: "ci / test (1, 2)"}, map[string]string{"check": "ruleset", "value": "ci%20/%20test%20(1%2C%202)"}},
		{"an installation", Check{Kind: CheckInstallation, Value: "acme"}, map[string]string{"check": "installation", "value": "acme"}},
		{"contains on another kind", Check{Kind: CheckLabel, Value: "x", Contains: "y"}, map[string]string{"check": "label", "value": "x"}},
	} {
		if got := CheckFields(tt.c); !maps.Equal(got, tt.want) {
			t.Errorf("%s: CheckFields = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// A Check of each kind round-trips through the origin marker: CheckFields,
// FormatMarker, then Derive reads it back as Origin.Check exactly, with
// spaces, "%", "<", ">", "," and non-ASCII in its values. A plain-text
// Check leaves the marker as it was.
func TestCheckRoundTrip(t *testing.T) {
	t.Parallel()
	odd := "a b%20<c>,d é\t--> <!-- 100%"
	for _, c := range []Check{
		{},
		{Kind: CheckLabel, Value: odd},
		{Kind: CheckIssueType, Value: odd},
		{Kind: CheckFile, Value: odd},
		{Kind: CheckFile, Value: "docs/setup guide.md", Contains: odd},
		{Kind: CheckRuleset, Value: odd},
		{Kind: CheckInstallation, Value: "Acme-Corp"},
	} {
		fields := map[string]string{"plan": "#250", "rev": "1", "item": "T3", "human": "1"}
		maps.Copy(fields, CheckFields(c))
		marker, err := github.FormatMarker(github.Marker{Kind: "origin", Fields: fields})
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		is := Derive(snap("The task.\n\n"+marker, nil), slug, clean)
		want := Origin{Plan: 250, Rev: 1, Item: "T3", Comment: BodyID, Human: true, Check: c}
		if is.Origin != want {
			t.Errorf("%+v: marker %s reads back as %+v, want %+v", c, marker, is.Origin, want)
		}
		if c == (Check{}) && marker != "<!-- issuedog:origin plan=#250 rev=1 item=T3 human=1 -->" {
			t.Errorf("plain text: marker = %s, want no check fields", marker)
		}
	}
}
