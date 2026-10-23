package daemon_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// TestPhase1PinUnchangedTriageAsk pins what epic #1's verifier found
// untested (#228): an unchanged pass over a repo with an issue triage
// flagged, at triage:ask. A 👍 changes no updated_at, so every pass runs
// step 1 on the flagged issue to read its flag's reactions. With nothing
// changed, that pass writes nothing, and every read it makes is a
// conditional GET that GitHub answers 304, the flag's reactions among them.
// eve's 👍, untrusted, takes nothing and costs no permission read, in that
// pass or any before it. Once owner 👍s the flag, the next pass takes the
// recommendation.
func TestPhase1PinUnchangedTriageAsk(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	// eve has read access, so a read of her permission would be a 200 that
	// a later pass repeats as a 304, which the unchanged pass's check of
	// its reads alone would let through.
	f.SetPermission("eve", "read")
	n := f.AddIssue("owner", "Odd export", "The export looks odd.")
	flag := f.AddComment(n, app, digestRunFlag(n, "Export format"))
	f.AddLabel(n, app, "triage:ask")
	f.AddReaction(flag, "eve", "+1")
	d := digestRunDaemon(t, f, newOrigin(t), nil, nil)
	// The first pass creates the digest, and the next reads the App's
	// writes once.
	iterate(t, d)
	iterate(t, d)
	from, writes, histories := len(f.Requests()), len(f.Writes()), f.HistoryQueries()

	iterate(t, d)
	reqs := f.Requests()[from:]
	if len(reqs) == 0 {
		t.Fatal("the unchanged pass sent no requests")
	}
	for _, r := range reqs {
		if r.Method != "GET" || !r.Conditional || r.Status != 304 {
			t.Errorf("the unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}
	if w := f.Writes(); len(w) != writes || f.HistoryQueries() != histories {
		t.Errorf("the unchanged pass wrote %+v and sent %d history queries; want none", w[writes:], f.HistoryQueries()-histories)
	}
	reactions := fmt.Sprintf("/issues/comments/%d/reactions", flag)
	if !slices.ContainsFunc(reqs, func(r githubtest.Request) bool { return r.Path == reactions }) {
		t.Errorf("the unchanged pass sent %+v; want a read of %s, as a 👍 changes no updated_at", reqs, reactions)
	}
	if got := ivLabels(f, n); !slices.Equal(got, []string{"triage:ask"}) {
		t.Fatalf("labels %v after eve's 👍; want only triage:ask", got)
	}
	// CODEOWNERS names only owner, so eve is untrusted (SPEC §6.1) with no
	// need to ask GitHub her role: no pass so far, the unchanged one among
	// them, reads any permission.
	if i := slices.IndexFunc(f.Requests(), func(r githubtest.Request) bool {
		return strings.HasPrefix(r.Path, "/collaborators/") && strings.HasSuffix(r.Path, "/permission")
	}); i >= 0 {
		t.Errorf("request %+v before owner's 👍; want no permission read, as eve's 👍 costs none", f.Requests()[i])
	}

	f.AddReaction(flag, "owner", "+1")
	iterate(t, d)
	if got := ivLabels(f, n); !slices.Equal(got, []string{"stage:clarifying"}) {
		t.Errorf("labels %v after owner's 👍; want stage:clarifying, as the flag recommends handling it", got)
	}
}
