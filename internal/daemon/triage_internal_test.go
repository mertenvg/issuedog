package daemon

import (
	"errors"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github"
)

func TestTriageAdmissionKey(t *testing.T) {
	t.Parallel()
	for key, want := range map[string]bool{
		"12": true, "r12": true, "9007199254740993": true,
		"": false, "r": false, "0": false, "r0": false, "012": false, "-12": false, "rr12": false, "12a": false,
		"intake-0": false, "intake-0-9f2a61c0": false, "1@2": false,
	} {
		if got := triageAdmissionKey(key); got != want {
			t.Errorf("triageAdmissionKey(%q) = %t, want %t", key, got, want)
		}
	}
}

func TestTriageFenced(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ text, want string }{
		{"plain", "## H\n\n```text\nplain\n```"},
		{"a `b` c", "## H\n\n```text\na `b` c\n```"},
		{"```\nclose\n```", "## H\n\n````text\n```\nclose\n```\n````"},
		{"`` x ````` y `", "## H\n\n``````text\n`` x ````` y `\n``````"},
	} {
		if got := triageFenced("H", tc.text); got != tc.want {
			t.Errorf("triageFenced(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

func TestTriageDecode(t *testing.T) {
	t.Parallel()
	out, err := triageDecode([]byte(`{"verdict":"leave","sure":true,"reason":"r","summary":"s"}`))
	if err != nil || out != (triageOutput{Verdict: "leave", Sure: true, Reason: "r", Summary: "s"}) {
		t.Errorf("triageDecode = %+v, %v; want the output", out, err)
	}
	for _, raw := range []string{
		`{"verdict":"maybe","sure":true,"reason":"r","summary":"s"}`,
		`{"verdict":"handle","sure":true,"reason":"r","summary":"s","extra":1}`,
		`[]`,
	} {
		if _, err := triageDecode([]byte(raw)); err == nil {
			t.Errorf("triageDecode(%s) = nil error, want one", raw)
		}
	}
}

// Check rejects an output whose text would be too long for GitHub, naming
// each text and the length rule, and passes one that fits.
func TestTriageCheck(t *testing.T) {
	t.Parallel()
	tr := &Triage{Daemon: &Daemon{Config: config.Defaults()}}
	ok := []byte(`{"verdict":"handle","sure":false,"reason":"r","summary":"s"}`)
	if err := tr.Check(t.Context(), Result{Output: ok, S: &github.Snapshot{Number: 7}}); err != nil {
		t.Errorf("Check = %v, want nil", err)
	}
	long := strings.Repeat("y", maxText-textRoom)
	err := tr.Check(t.Context(), Result{Output: []byte(`{"verdict":"handle","sure":false,"reason":"` + long + `","summary":"s"}`), S: &github.Snapshot{Number: 7}})
	var re *ruleError
	if !errors.As(err, &re) || re.Rule != ruleLength {
		t.Fatalf("Check = %v, want a length rule error", err)
	}
	for _, what := range []string{"admission", "flag", "left comment"} {
		if !strings.Contains(err.Error(), "the "+what+" is ") {
			t.Errorf("Check = %v; want it to name the %s", err, what)
		}
	}
	if err := tr.Check(t.Context(), Result{Output: []byte(`{"verdict":"x"}`), S: &github.Snapshot{Number: 7}}); err == nil {
		t.Error("Check of an unknown verdict = nil, want an error")
	}
}

// The flag recommends the verdict first, in either order, and has no cc
// line with no one to notify.
func TestTriageFlagText(t *testing.T) {
	t.Parallel()
	got := triageFlagText(triageOutput{Verdict: "leave", Reason: "A question.", Summary: "Asks how."}, 7, nil)
	want := "### Should issuedog handle #7?\n\n**1. Asks how.** Blocks: #7\n   a. Leave it (recommended): A question.\n   b. Handle it\n\n" +
		"React 👍 to take the recommendation, or post `/track` or `/ignore`."
	if got != want {
		t.Errorf("triageFlagText = %q, want %q", got, want)
	}
	got = triageFlagText(triageOutput{Verdict: "handle", Reason: "r", Summary: "s"}, 7, []string{"a", "b"})
	if !strings.Contains(got, "   a. Handle it (recommended): r\n   b. Leave it\n") || !strings.HasSuffix(got, "\n\ncc @a @b") {
		t.Errorf("triageFlagText = %q; want handle first, and the cc line", got)
	}
}
