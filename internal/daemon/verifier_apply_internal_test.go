package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// verifyMarker is a verify marker with fields, as key, value pairs.
func verifyMarker(kv ...string) github.Marker {
	m := github.Marker{Kind: verifyKind, Fields: map[string]string{}}
	for i := 0; i+1 < len(kv); i += 2 {
		m.Fields[kv[i]] = kv[i+1]
	}
	return m
}

// verdictOn finds only a pass, on that head: no other kind of marker or
// verdict counts, and no head has none.
func TestVerdictOn(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		marker github.Marker
		head   string
		want   bool
	}{
		{"a pass on the head", verifyMarker("verdict", "pass", "run", "r", "head", "abc"), "abc", true},
		{"a pass on another head", verifyMarker("verdict", "pass", "run", "r", "head", "def"), "abc", false},
		{"no head", verifyMarker("verdict", "pass", "run", "r"), "", false},
		{"another verdict", verifyMarker("verdict", "request_changes", "run", "r", "head", "abc"), "abc", false},
		{"another kind", github.Marker{Kind: "transition", Fields: map[string]string{"verdict": "pass", "head": "abc"}}, "abc", false},
	} {
		is := state.Issue{Markers: []github.Marker{tt.marker}}
		if got := verdictOn(is, tt.head); got != tt.want {
			t.Errorf("%s: verdictOn = %t, want %t", tt.name, got, tt.want)
		}
	}
}

// verdictRun finds a verify marker by its run=, whatever its verdict, and
// runApplied that, or the run's move.
func TestVerdictRunAndRunApplied(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		marker       github.Marker
		run          string
		verdict, any bool
	}{
		{"the run's pass", verifyMarker("verdict", "pass", "run", "r1"), "r1", true, true},
		{"the run's other verdict", verifyMarker("verdict", "question", "run", "r1"), "r1", true, true},
		{"another run's", verifyMarker("verdict", "pass", "run", "r2"), "r1", false, false},
		{"no run", verifyMarker("verdict", "pass"), "", false, false},
		{"the run's move", github.Marker{Kind: "transition", Fields: map[string]string{"run": "r1"}}, "r1", false, true},
		{"a marker of another kind", github.Marker{Kind: "triage", Fields: map[string]string{"run": "r1"}}, "r1", false, false},
	} {
		is := state.Issue{Markers: []github.Marker{tt.marker}}
		if got := verdictRun(is, tt.run); got != tt.verdict {
			t.Errorf("%s: verdictRun = %t, want %t", tt.name, got, tt.verdict)
		}
		if got := runApplied(is, tt.run); got != tt.any {
			t.Errorf("%s: runApplied = %t, want %t", tt.name, got, tt.any)
		}
	}
}

// The head named by the latest move into verifying stops the verifier's
// Pending once it has a pass. A later move's new head has work again, and
// so does a move that names no head, which verifyTask judges by the task's.
func TestVerifierPendingOnTheHead(t *testing.T) {
	t.Parallel()
	task := state.Item{Type: "task", FromPlan: true}
	move := func(fields ...string) github.Marker {
		m := github.Marker{Kind: "transition", Fields: map[string]string{"to": "verifying"}}
		for i := 0; i+1 < len(fields); i += 2 {
			m.Fields[fields[i]] = fields[i+1]
		}
		return m
	}
	pass := verifyMarker("verdict", "pass", "run", "r", "head", "abc")
	for _, tt := range []struct {
		name    string
		markers []github.Marker
		want    bool
	}{
		{"no pass", []github.Marker{move("head", "abc")}, true},
		{"a pass on the head", []github.Marker{move("head", "abc"), pass}, false},
		{"a pass on an earlier head", []github.Marker{move("head", "abc"), pass, move("head", "def")}, true},
		{"a move that names no head", []github.Marker{move("head", "abc"), pass, move()}, true},
	} {
		in := Input{Issue: state.Issue{Stage: state.Verifying, Item: task, Markers: tt.markers}}
		if got := (&Verifier{}).Pending(in); got != tt.want {
			t.Errorf("%s: Pending = %t, want %t", tt.name, got, tt.want)
		}
	}
}

// A pass's comment names the head and the notes, as block leaves them.
func TestRenderPass(t *testing.T) {
	t.Parallel()
	got := renderPass("abc", "# Heading\n\nFine.")
	want := "The verifier passed `abc`.\n\n" + block("# Heading\n\nFine.")
	if got != want {
		t.Errorf("renderPass =\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(got, "\n# Heading") {
		t.Errorf("renderPass =\n%s\nwant the agent's heading defused by block", got)
	}
}

// decodeVerifier reads every golden field the schema has, and refuses one
// it doesn't, as decodeImplementer does.
func TestDecodeVerifier(t *testing.T) {
	t.Parallel()
	out, err := decodeVerifier(json.RawMessage(`{"verdict":"request_changes","notes":"n",
		"findings":[{"criterion":"c","class":"regression","detail":"d"}],
		"follow_ups":[{"title":"t","scope":"s","scenario":"sc","likelihood":"l","impact":"i"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if out.Verdict != "request_changes" || out.Notes != "n" || len(out.Findings) != 1 || out.Findings[0].Class != "regression" ||
		len(out.FollowUps) != 1 || out.FollowUps[0].Impact != "i" {
		t.Errorf("decodeVerifier = %+v", out)
	}
	if _, err := decodeVerifier(json.RawMessage(`{"verdict":"pass","notes":"n","extra":1}`)); err == nil {
		t.Error("decodeVerifier took a field the types don't have")
	}
}
