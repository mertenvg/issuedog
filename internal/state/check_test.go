package state

import (
	"maps"
	"strings"
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

// A story branch failure's human task has a tip check, read from its
// origin's story and tip, not from check= (SPEC §5, Story branch
// failures): its Value is the tip, and its story is Origin.Story. Its
// check, value and contains fields count for nothing, whole or not. A
// failure's bug has no check, whatever fields it has, and check=tip on a
// plan's human task is no tip check, but one that couldn't be read, as
// the App never writes it there.
func TestCheckTip(t *testing.T) {
	t.Parallel()
	const tip = "4b1d0e2c9a8f7e6d5c4b3a291807f6e5d4c3b2a1"
	for _, tt := range []struct {
		name   string
		fields string
		want   Check
	}{
		{"a human task", "human=1 story=#625 tip=" + tip, Check{Kind: CheckTip, Value: tip}},
		{"a human task with a whole check", "check=label human=1 story=#625 tip=" + tip + " value=x", Check{Kind: CheckTip, Value: tip}},
		{"a human task with check=tip", "check=tip human=1 story=#625 tip=" + tip + " value=" + strings.Repeat("0", 40), Check{Kind: CheckTip, Value: tip}},
		{"a human task with fields that aren't a whole check", "contains=x human=1 story=#625 tip=" + tip, Check{Kind: CheckTip, Value: tip}},
		{"a bug", "story=#625 tip=" + tip, Check{}},
		{"a bug with a whole check", "check=label story=#625 tip=" + tip + " value=x", Check{}},
		{"a conflict bug", "main=" + strings.Repeat("5", 40) + " story=#625 tip=" + tip, Check{}},
		{"a plan's human task with check=tip", "plan=#250 rev=1 item=T3 human=1 check=tip value=" + tip, Check{Kind: CheckUnreadable}},
	} {
		is := Derive(snap("<!-- issuedog:origin "+tt.fields+" -->", nil), slug, clean)
		if is.Origin.Check != tt.want {
			t.Errorf("%s: Origin.Check = %+v, want %+v", tt.name, is.Origin.Check, tt.want)
		}
		if tt.want.Kind == CheckTip && is.Origin.Story != 625 {
			t.Errorf("%s: Origin.Story = %d, want 625, the tip check's story", tt.name, is.Origin.Story)
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
