package github

import (
	"reflect"
	"testing"
)

const appSlug = "issuedog-mertenvg"

var app = Author{Login: appSlug + "[bot]", Type: "Bot"}

func TestParseMarkersKindsInUse(t *testing.T) {
	// Bodies copied from comments on #1 to #4, and the SPEC examples.
	for _, tt := range []struct {
		body string
		want Marker
	}{
		{
			"Plan rev 1 approved by @mertenvg's 👍 (reaction 437857492).\n\n<!-- issuedog:transition from=plan-review to=planned rev=1 cmd=r437857492 -->",
			Marker{"transition", map[string]string{"from": "plan-review", "to": "planned", "rev": "1", "cmd": "r437857492"}},
		},
		{
			"<!-- issuedog:transition from=clarifying to=spec-review rev=3 body=9f2a61c0 run=8f2c cmd=2214587 -->",
			Marker{"transition", map[string]string{"from": "clarifying", "to": "spec-review", "rev": "3", "body": "9f2a61c0", "run": "8f2c", "cmd": "2214587"}},
		},
		{
			"## Test plan\nA test in `cmd/issuedog`.\n\n<!-- issuedog:origin plan=#2 rev=1 item=T4 -->",
			Marker{"origin", map[string]string{"plan": "#2", "rev": "1", "item": "T4"}},
		},
		{
			"**Verifier: pass** (round 2, after review; attempt 1 of 3) at 4652781.\n\n<!-- issuedog:verify verdict=pass attempt=1 round=2 sha=4652781 -->",
			Marker{"verify", map[string]string{"verdict": "pass", "attempt": "1", "round": "2", "sha": "4652781"}},
		},
		{
			"<!-- issuedog:depends-on url=https://github.com/dmcnp/open-dmcn/issues/12 -->",
			Marker{"depends-on", map[string]string{"url": "https://github.com/dmcnp/open-dmcn/issues/12"}},
		},
		{
			"<!--issuedog:transition from=intake to=ready-->",
			Marker{"transition", map[string]string{"from": "intake", "to": "ready"}},
		},
	} {
		got := ParseMarkers(tt.body)
		if len(got) != 1 || !reflect.DeepEqual(got[0], tt.want) {
			t.Errorf("ParseMarkers(%q) = %+v, want [%+v]", tt.body, got, tt.want)
		}
	}
}

func TestParseMarkersSeveralInOrder(t *testing.T) {
	body := "<!-- issuedog:verify verdict=findings attempt=3 sha=0e36d82 -->\ntext\n" +
		"<!-- issuedog:transition from=verifying to=needs-human attempt=3 -->"
	got := ParseMarkers(body)
	if len(got) != 2 || got[0].Kind != "verify" || got[1].Kind != "transition" {
		t.Fatalf("got %+v, want verify then transition", got)
	}
	if got[1].Fields["to"] != "needs-human" {
		t.Errorf("second marker fields = %v", got[1].Fields)
	}
}

func TestParseMarkersSkipsMalformed(t *testing.T) {
	for _, body := range []string{
		"<!-- issuedog:verify verdict pass -->",    // field without =
		"<!-- issuedog:verify verdict= -->",        // empty value
		"<!-- issuedog:verify a=1 a=2 -->",         // repeated key
		"<!-- issuedog:verify Verdict=pass -->",    // key not lowercase
		"<!-- issuedog: verify=pass -->",           // no kind
		"<!-- issuedog:verify verdict=pass",        // not closed
		"<!-- issuedog:Verify verdict=pass -->",    // kind not lowercase
		"<!-- notissuedog:verify verdict=pass -->", // other namespace
		"issuedog:verify verdict=pass",             // not a comment
		"<!-- issuedog:verify v=<b>pass</b> -->",   // angle brackets in value
	} {
		if got := ParseMarkers(body); len(got) != 0 {
			t.Errorf("ParseMarkers(%q) = %+v, want none", body, got)
		}
	}
	// A malformed marker doesn't hide a good one after it.
	got := ParseMarkers("<!-- issuedog:verify verdict pass -->\n<!-- issuedog:verify verdict=pass -->")
	if len(got) != 1 || got[0].Fields["verdict"] != "pass" {
		t.Errorf("got %+v, want the one well-formed marker", got)
	}
}

func TestTrustedMarkers(t *testing.T) {
	body := "<!-- issuedog:transition from=plan-review to=planned rev=1 cmd=r1 -->"
	for _, tt := range []struct {
		name   string
		author Author
		want   int
	}{
		{"the App", app, 1},
		{"a trusted user", Author{Login: "mertenvg", Type: "User"}, 0},
		{"a user named like the App", Author{Login: appSlug + "[bot]", Type: "User"}, 0},
		{"a different bot", Author{Login: "dependabot[bot]", Type: "Bot"}, 0},
		{"a bot with the slug but no suffix", Author{Login: appSlug, Type: "Bot"}, 0},
		{"a bot whose slug extends the App's", Author{Login: appSlug + "-x[bot]", Type: "Bot"}, 0},
	} {
		if got := TrustedMarkers(tt.author, body, appSlug); len(got) != tt.want {
			t.Errorf("%s: %d markers, want %d", tt.name, len(got), tt.want)
		}
	}
	if got := TrustedMarkers(Author{Login: "[bot]", Type: "Bot"}, body, ""); got != nil {
		t.Errorf("empty app slug trusted %+v, want nothing", got)
	}
}

func TestFormatMarker(t *testing.T) {
	for _, tt := range []struct {
		m    Marker
		want string
	}{
		{Marker{"transition", map[string]string{"cmd": "2214587", "run": "8f2c", "body": "9f2a61c0", "rev": "3", "to": "spec-review", "from": "clarifying"}},
			"<!-- issuedog:transition from=clarifying to=spec-review rev=3 body=9f2a61c0 run=8f2c cmd=2214587 -->"},
		{Marker{"error", map[string]string{"key": "r12", "kind": "transition"}}, "<!-- issuedog:error kind=transition key=r12 -->"},
		{Marker{"origin", map[string]string{"item": "T4", "rev": "1", "plan": "#2"}}, "<!-- issuedog:origin plan=#2 rev=1 item=T4 -->"},
		{Marker{"x", map[string]string{"zz": "1", "aa": "2", "to": "ready"}}, "<!-- issuedog:x to=ready aa=2 zz=1 -->"},
		{Marker{"revert", nil}, "<!-- issuedog:revert -->"},
	} {
		got, err := FormatMarker(tt.m)
		if err != nil {
			t.Fatalf("FormatMarker(%+v): %v", tt.m, err)
		}
		if got != tt.want {
			t.Errorf("FormatMarker(%+v) = %q, want %q", tt.m, got, tt.want)
		}
		back := ParseMarkers(got)
		if len(back) != 1 || back[0].Kind != tt.m.Kind || len(back[0].Fields) != len(tt.m.Fields) {
			t.Errorf("ParseMarkers(%q) = %+v, want %+v back", got, back, tt.m)
		}
	}
}

func TestFormatMarkerRefusesUnreadable(t *testing.T) {
	for _, m := range []Marker{
		{"Transition", nil},
		{"", nil},
		{"transition", map[string]string{"to": ""}},
		{"transition", map[string]string{"to": "a b"}},
		{"transition", map[string]string{"to": "x-->"}},
		{"transition", map[string]string{"To": "ready"}},
	} {
		if got, err := FormatMarker(m); err == nil {
			t.Errorf("FormatMarker(%+v) = %q, want an error", m, got)
		}
	}
}

func TestEscape(t *testing.T) {
	got := Escape("see <!-- issuedog:transition to=planning --> and <!--issuedog:error key=1-->")
	if want := "see &lt;!-- issuedog:transition to=planning --> and &lt;!--issuedog:error key=1-->"; got != want {
		t.Errorf("Escape = %q, want %q", got, want)
	}
	if m := TrustedMarkers(app, got, appSlug); len(m) != 0 {
		t.Errorf("markers in escaped text = %+v, want none", m)
	}
}
