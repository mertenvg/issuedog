package daemon_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// #188's verify, round 2, attempt 3: a comment a write collaborator
// deletes on an ancestor during a hold may have been a /hold that wrote no
// marker, so it keeps the issue held until a /resume there, between or on
// the issue ends it; and a /resume above ends an issue's own /hold that
// wrote no marker only when that /hold is stacked on a hold the /resume
// ends, not one on a discussion hold alone (#87's Q2b, SPEC §6.4).

// resolveSettle runs passes over ns, in order, until one writes nothing.
func resolveSettle(t testing.TB, f *githubtest.Fake, ns ...int) {
	t.Helper()
	for i := 0; ; i++ {
		w := len(f.Writes())
		resolveSteps(t, f, ns...)
		if len(f.Writes()) == w {
			return
		}
		if i > 6 {
			t.Fatalf("still writing after %d rounds: %+v", i, f.Writes()[w:])
		}
	}
}

// resolveLabels checks each issue's labels: want[i] is #(i+1)'s stage.
func resolveLabels(t *testing.T, f *githubtest.Fake, when string, want ...string) {
	t.Helper()
	for i, stage := range want {
		if l := f.State().Issues[i].Labels; !slices.Equal(l, []string{"stage:" + stage}) {
			t.Errorf("%s: #%d: labels %q, newest comment %q; want %s", when, i+1, l, newestComment(f, i+1), stage)
		}
	}
}

// resolveRejectedWith reports whether the newest comment on issue n is the
// App's rejection of the command in comment id, for reason.
func resolveRejectedWith(f *githubtest.Fake, n int, id int64, reason string) bool {
	body := newestComment(f, n)
	return strings.Contains(body, "I didn't apply this command: "+reason+".") &&
		strings.HasSuffix(body, fmt.Sprintf("<!-- issuedog:reply reason=rejected cmd=%d -->", id))
}

// resolveGoneRecord is the start of the record that a comment deleted on
// #2 during the hold keeps #1 held, and its marker's start.
const (
	resolveGoneText = "The discussion is resolved, but this issue stays on hold: a write collaborator deleted a comment on #2 during this hold, " +
		"which may have been a `/hold`, so it counts as one that hasn't ended. `/resume` there, or here, ends that, and then it goes to `clarifying`."
	resolveGoneMarker = "<!-- issuedog:ended reason=discussion gone=2-"
)

// resolveGoneSetup is #2, held by the owner's hand or for mallory's
// comment, and #1 under it, held for mallory's comment w; then the owner's
// /hold on #2, which writes no marker on either, as both are held already
// (Q2b), and mallory's deletion of it, by passes; then the owner's 👍 on
// w, and, with same, the deletion only then, with no pass between. It
// returns the fake and the /hold's ID.
func resolveGoneSetup(byHand, same bool) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		if byHand {
			file(f, 0, "from=ready to=held at=ready reason=owner by=owner event=77") // #2
		} else {
			file(f, 0, ready) // #2
			f.AddComment(2, "mallory", "Wait here too.")
		}
		f.SetParent(1, 2)
		w := f.AddComment(1, "mallory", "Wait.")
		resolveSteps(t, f, 1, 2)
		hold := f.AddComment(2, "owner", "/hold")
		resolveSteps(t, f, 2, 1)
		for n := 1; n <= 2; n++ {
			if body := newestComment(f, n); strings.Contains(body, fmt.Sprintf("cmd=%d", hold)) {
				t.Fatalf("#%d: newest comment %q; want no marker of the /hold, as it was held already", n, body)
			}
		}
		f.DeleteCommentBy("mallory", hold)
		if !same {
			resolveSteps(t, f, 2, 1)
		}
		f.AddReaction(w, "owner", "+1")
		f.ForgetWrites()
		return f
	}
}

// A 👍 on mallory's comment on #1 resolves its discussion, but #1 stays
// held: the comment mallory deleted on #2 may have been the owner's /hold,
// which no marker records and no /resume ended, so the App records that
// and, from then on, writes nothing more. The owner's /resume on #2 ends
// it, and #1 goes to clarifying.
func TestResolveGoneAncestorHold(t *testing.T) {
	for _, tt := range []struct {
		name         string
		byHand, same bool
	}{
		{"a parent held by hand", true, false},
		{"a parent held by hand, the deletion and the 👍 in one window", true, true},
		{"a parent held for discussion too", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setup := resolveGoneSetup(tt.byHand, tt.same)
			githubtest.Replay(t, setup, resolvePass(t, 1))
			f := run(t, setup, resolvePass(t, 1))
			body := newestComment(f, 1)
			if !strings.HasPrefix(body, resolveGoneText+"\n\ncc @owner\n\n"+resolveGoneMarker) {
				t.Errorf("#1: newest comment %q; want the record that the deleted comment on #2 keeps it held", body)
			}
			// The record names what holds it, so a pass over #1 reads #2 no more.
			n := len(f.Requests())
			resolveSteps(t, f, 1)
			for _, r := range f.Requests()[n:] {
				if r.Path == "/issues/2" || strings.HasPrefix(r.Path, "/issues/2/") {
					t.Errorf("a pass over #1 after the record read #2: %+v", r)
				}
			}
			resolveSettle(t, f, 2, 1)
			resolveLabels(t, f, "after the 👍", "held", "held")
			f.AddComment(2, "owner", "/resume")
			resolveSettle(t, f, 2, 1)
			resolveLabels(t, f, "after #2's /resume", "clarifying", "ready")
		})
	}
}

// resolveGoneBelow is #3 at ready, #2 under it held by the owner's hand,
// and #1 under #2 held for mallory's comment, which the owner's 👍
// resolves after mallory deleted the owner's /hold on #2, which wrote no
// marker, by passes: #1 stays held by the comment deleted on #2.
func resolveGoneBelow(t testing.TB) *githubtest.Fake {
	t.Helper()
	f := discFake(t)
	file(f, 0, "from=ready to=held at=ready reason=owner by=owner event=77") // #2
	file(f, 0, ready)                                                        // #3
	f.SetParent(1, 2)
	f.SetParent(2, 3)
	w := f.AddComment(1, "mallory", "Wait.")
	resolveSteps(t, f, 1)
	hold := f.AddComment(2, "owner", "/hold")
	resolveSteps(t, f, 2, 1)
	f.DeleteCommentBy("mallory", hold)
	f.AddReaction(w, "owner", "+1")
	resolveSettle(t, f, 3, 2, 1)
	if body := newestComment(f, 1); !strings.Contains(body, resolveGoneMarker) {
		t.Fatalf("#1: newest comment %q; want the record of the comment deleted on #2", body)
	}
	return f
}

// A /resume above the issue where the comment was deleted doesn't end what
// it may have been, so #3's /resume, which ends nothing else, is rejected,
// naming #2, and #1 stays held.
func TestResolveGoneNotEndedFromAbove(t *testing.T) {
	setup := func(t testing.TB) *githubtest.Fake {
		f := resolveGoneBelow(t)
		f.AddComment(3, "owner", "/resume")
		f.ForgetWrites()
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 3))
	f := run(t, setup, resolvePass(t, 3))
	resume := commentIDs(t, f, 3)[len(commentIDs(t, f, 3))-2]
	if !resolveRejectedWith(f, 3, resume, madeOn("#2")) {
		t.Errorf("#3: newest comment %q; want the /resume rejected, naming #2", newestComment(f, 3))
	}
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after #3's /resume", "held", "held", "ready")
}

// While no record names the deleted comment yet, the same holds: #3's
// /resume, posted before the 👍's pass, leaves #1 held by it, and the
// record follows.
func TestResolveGoneNotEndedFromAboveUnrecorded(t *testing.T) {
	f := discFake(t)
	file(f, 0, "from=ready to=held at=ready reason=owner by=owner event=77") // #2
	file(f, 0, ready)                                                        // #3
	f.SetParent(1, 2)
	f.SetParent(2, 3)
	w := f.AddComment(1, "mallory", "Wait.")
	resolveSteps(t, f, 1)
	hold := f.AddComment(2, "owner", "/hold")
	resolveSteps(t, f, 2, 1)
	f.DeleteCommentBy("mallory", hold)
	f.AddReaction(w, "owner", "+1")
	resume := f.AddComment(3, "owner", "/resume")
	resolveSettle(t, f, 3, 2, 1)
	if !slices.ContainsFunc(f.State().Issues[2].Comments, func(c githubtest.CommentState) bool {
		return strings.HasSuffix(c.Body, fmt.Sprintf("<!-- issuedog:reply reason=rejected cmd=%d -->", resume))
	}) {
		t.Errorf("#3: comments %q; want the /resume rejected", closingComments(f, 3, 0))
	}
	resolveLabels(t, f, "after the passes", "held", "held", "ready")
	if body := newestComment(f, 1); !strings.Contains(body, resolveGoneMarker) {
		t.Errorf("#1: newest comment %q; want the record of the comment deleted on #2", body)
	}
}

// A /resume that ends a hold on a descendant left held by a comment deleted
// below it says so truly: the descendant is held by a deleted comment that
// may have been a /hold, which /resume there ends (#188's verify, attempt
// 3). #3's /hold held all three; the owner's /hold on #2, which wrote no
// marker, mallory deleted while #1 was held for discussion; #3's /resume
// ends #3's hold everywhere, leaves #2 held for its own deletion, and #1
// held by the deleted comment on #2.
func TestResolveGoneStillOnHoldText(t *testing.T) {
	var resume int64
	setup := func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		file(f, 0, ready) // #2
		file(f, 0, ready) // #3
		f.SetParent(1, 2)
		f.SetParent(2, 3)
		f.AddComment(3, "owner", "/hold")
		resolveSteps(t, f, 3)
		w := f.AddComment(1, "mallory", "Wait.")
		resolveSteps(t, f, 1)
		hold := f.AddComment(2, "owner", "/hold")
		resolveSteps(t, f, 2, 1)
		f.DeleteCommentBy("mallory", hold)
		f.AddReaction(w, "owner", "+1")
		resolveSteps(t, f, 2, 1)
		resume = f.AddComment(3, "owner", "/resume")
		f.ForgetWrites()
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 3))
	f := run(t, setup, resolvePass(t, 3))
	want := moved("Resumed by `/resume`, back to `ready`. Still on hold: #1, by a hold this `/resume` doesn't end: one set by hand, "+
		"a `/hold` below this issue, or a deleted comment that was, or may have been, a `/hold`. `/resume` there ends it. "+
		"Still on hold for discussion: #2.", fmt.Sprintf("from=held to=ready cmd=%d seen=1003", resume))
	if body := newestComment(f, 3); body != want {
		t.Errorf("#3: newest comment %q; want %q", body, want)
	}
	resolveLabels(t, f, "after #3's /resume", "held", "held", "ready")
	f.AddComment(2, "owner", "/resume")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after #2's /resume", "clarifying", "ready", "ready")
}

// resolveOwnOnDiscussion is #2 at ready and #1 under it, held for
// mallory's comment w, then the owner's /hold on #1, which writes no
// marker (Q2b), by passes. It returns the fake, w and the /hold.
func resolveOwnOnDiscussion(t testing.TB) (*githubtest.Fake, int64, int64) {
	t.Helper()
	f := discFake(t)
	file(f, 0, ready) // #2
	f.SetParent(1, 2)
	w := f.AddComment(1, "mallory", "Wait.")
	resolveSteps(t, f, 1)
	hold := f.AddComment(1, "owner", "/hold")
	resolveSteps(t, f, 1)
	if body := newestComment(f, 1); strings.Contains(body, fmt.Sprintf("cmd=%d", hold)) {
		t.Fatalf("#1: newest comment %q; want no marker of the /hold", body)
	}
	return f, w, hold
}

// #1's own /hold sits on its discussion hold alone, so #2's /resume, on
// an issue that isn't held, ends nothing and is rejected, naming #1. Once
// the owner's 👍 resolves mallory's comment, #1 stays held for its /hold,
// until its own /resume, which sends it to clarifying.
func TestResolveOwnHoldOnDiscussionStays(t *testing.T) {
	var w, resume int64
	setup := func(t testing.TB) *githubtest.Fake {
		var f *githubtest.Fake
		f, w, _ = resolveOwnOnDiscussion(t)
		resume = f.AddComment(2, "owner", "/resume")
		f.ForgetWrites()
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 2))
	f := run(t, setup, resolvePass(t, 2))
	if !resolveRejectedWith(f, 2, resume, madeOn("#1")) {
		t.Errorf("#2: newest comment %q; want the /resume rejected, naming #1", newestComment(f, 2))
	}
	if body := newestComment(f, 1); strings.Contains(body, "issuedog:ended") {
		t.Errorf("#1: newest comment %q; want no record that a hold ended", body)
	}
	f.AddReaction(w, "owner", "+1")
	resolveSettle(t, f, 2, 1)
	resolveLabels(t, f, "after the 👍", "held", "ready")
	if body := newestComment(f, 1); !strings.Contains(body, "an owner hold on it hasn't ended") {
		t.Errorf("#1: newest comment %q; want the record that its owner hold stays", body)
	}
	f.AddComment(1, "owner", "/resume")
	resolveSettle(t, f, 2, 1)
	resolveLabels(t, f, "after #1's /resume", "clarifying", "ready")
}

// Three levels: #2's own /hold sits on its discussion hold alone, and
// writes a marker on #3, which wasn't held. #1's /resume ends it on
// neither: it's rejected, naming #2, and once mallory's comment is
// resolved, #2 and #3 both stay held, as the /hold on #2 holds them.
func TestResolveOwnHoldOnDiscussionThreeLevels(t *testing.T) {
	var w, resume int64
	setup := func(t testing.TB) *githubtest.Fake {
		f := discFake(t)  // #1, at the top
		file(f, 1, ready) // #2, under #1
		file(f, 2, ready) // #3, under #2
		w = f.AddComment(2, "mallory", "Wait.")
		resolveSteps(t, f, 2)
		f.AddComment(2, "owner", "/hold")
		resolveSteps(t, f, 2)
		resume = f.AddComment(1, "owner", "/resume")
		f.ForgetWrites()
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 1))
	f := run(t, setup, resolvePass(t, 1))
	if !resolveRejectedWith(f, 1, resume, madeOn("#2")) {
		t.Errorf("#1: newest comment %q; want the /resume rejected, naming #2", newestComment(f, 1))
	}
	f.AddReaction(w, "owner", "+1")
	resolveSettle(t, f, 1, 2, 3)
	resolveLabels(t, f, "after the 👍", "ready", "held", "held")
}

// #1, held for mallory's comment, then by the owner's /hold on #2, which
// writes a marker on #2 but none on #1, then by its own /hold, which
// writes none either: it's stacked on #2's /hold, so #2's /resume ends
// both on #1 (Q2b), which stays held for mallory's comment only, with the
// record that its own /hold ended (#2's is #2's /resume's own to end, so
// #1 never counts it); once the owner's 👍 resolves that, it goes to
// clarifying.
func TestResolveOwnHoldStackedOnParentHoldWithoutMarker(t *testing.T) {
	var w, own int64
	setup := func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		file(f, 0, ready) // #2
		f.SetParent(1, 2)
		w = f.AddComment(1, "mallory", "Wait.")
		resolveSteps(t, f, 1)
		f.AddComment(2, "owner", "/hold")
		resolveSteps(t, f, 2, 1)
		own = f.AddComment(1, "owner", "/hold")
		resolveSteps(t, f, 1)
		f.AddComment(2, "owner", "/resume")
		f.ForgetWrites()
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 2))
	f := run(t, setup, resolvePass(t, 2))
	resolveLabels(t, f, "after #2's /resume", "held", "ready")
	if body := newestComment(f, 1); !strings.Contains(body, fmt.Sprintf("<!-- issuedog:ended reason=owner key=c%d cmd=", own)) {
		t.Errorf("#1: newest comment %q; want the record that its own /hold, in comment %d, ended", body, own)
	}
	f.AddReaction(w, "owner", "+1")
	resolveSettle(t, f, 2, 1)
	resolveLabels(t, f, "after the 👍", "clarifying", "ready")
}

// #3 > #2 > #1: #3's /hold held all three; #1's own /resume took #1 out;
// then #2's /hold, which writes no marker on #2, held by #3's, but one on
// #1; then mallory's comment on #1. #3's /resume ends #2's /hold, stacked
// on its own, on #2 and on #1 alike (Q2b): #2 goes back, and #1 stays held
// for mallory's comment only, with one record that the /hold ended.
func TestResolveStackedHoldMarkedBelow(t *testing.T) {
	var w, between, resume int64
	setup := func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		file(f, 0, ready) // #2
		file(f, 0, ready) // #3
		f.SetParent(1, 2)
		f.SetParent(2, 3)
		f.AddComment(3, "owner", "/hold")
		resolveSteps(t, f, 3)
		f.AddComment(1, "owner", "/resume")
		resolveSteps(t, f, 1)
		between = f.AddComment(2, "owner", "/hold")
		resolveSteps(t, f, 2)
		if body := newestComment(f, 1); !strings.Contains(body, fmt.Sprintf("reason=owner cmd=%d", between)) {
			t.Fatalf("#1: newest comment %q; want the marker of #2's /hold", body)
		}
		w = f.AddComment(1, "mallory", "Wait.")
		resolveSteps(t, f, 1)
		resume = f.AddComment(3, "owner", "/resume")
		f.ForgetWrites()
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 3))
	f := run(t, setup, resolvePass(t, 3))
	resolveLabels(t, f, "after #3's /resume", "held", "ready", "ready")
	want := fmt.Sprintf("`/resume` on #3 ended the owner hold on this issue, but it stays on hold for discussion until a code owner resolves that, "+
		"or posts `/resume` here.\n\n<!-- issuedog:ended reason=owner key=c%d cmd=%d -->", between, resume)
	if body := newestComment(f, 1); body != want {
		t.Errorf("#1: newest comment %q; want %q", body, want)
	}
	f.AddReaction(w, "owner", "+1")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after the 👍", "clarifying", "ready", "ready")
}

// resolveDeletedAbove is #4 > #3 > #2 > #1, mallory's comments holding
// #1 and #2, then the owner's /hold on #4, with markers on #4 and #3, and
// none on #2 or #1, held already, by passes; then mallory's deletion of
// it, and the owner's 👍s on her two comments; before them, with resume
// first, #3's /resume. #3 is between #4 and the others, so its /resume
// ends what the /hold, or the deleted comment, held below it, and #1 and
// #2 go to clarifying, whichever came first (#188's verify, attempt 3);
// #4 stays held.
func TestResolveDeletedAboveEitherOrder(t *testing.T) {
	for _, resumeFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("resume first %v", resumeFirst), func(t *testing.T) {
			f := discFake(t)
			file(f, 0, ready) // #2
			file(f, 0, ready) // #3
			file(f, 0, ready) // #4
			f.SetParent(1, 2)
			f.SetParent(2, 3)
			f.SetParent(3, 4)
			w1 := f.AddComment(1, "mallory", "Wait 1.")
			w2 := f.AddComment(2, "mallory", "Wait 2.")
			resolveSettle(t, f, 1, 2)
			hold := f.AddComment(4, "owner", "/hold")
			resolveSettle(t, f, 4, 3, 2, 1)
			resolveLabels(t, f, "after the /hold", "held", "held", "held", "held")
			f.DeleteCommentBy("mallory", hold)
			resolveSettle(t, f, 4, 3, 2, 1)
			thumbs := func() {
				f.AddReaction(w1, "owner", "+1")
				f.AddReaction(w2, "owner", "+1")
				resolveSettle(t, f, 4, 3, 2, 1)
			}
			if resumeFirst {
				f.AddComment(3, "owner", "/resume")
				resolveSettle(t, f, 4, 3, 2, 1)
				thumbs()
			} else {
				thumbs()
				resolveLabels(t, f, "after the 👍s", "held", "held", "held", "held")
				f.AddComment(3, "owner", "/resume")
				resolveSettle(t, f, 4, 3, 2, 1)
			}
			resolveLabels(t, f, "at the end", "clarifying", "clarifying", "ready", "held")
		})
	}
}

// An edited comment before the transition that came before the hold is no
// /hold of this hold, though the App 👍'd it during the hold: once
// mallory's comment is resolved, #1 goes to clarifying (resolveOwnHolds'
// bound).
func TestResolveEditedBeforeHoldIsNoHold(t *testing.T) {
	f := githubtest.New(t, "o/r", slug)
	f.SetPermission("owner", "admin")
	f.SetPermission("mallory", "write")
	f.SetPermission("eve", "read")
	n := f.AddIssue("owner", "Issue", "body")
	f.SetType(n, "task")
	early := f.AddComment(n, "owner", "Noted.")
	f.AddLabel(n, app, "stage:ready")
	f.AddComment(n, app, "<!-- issuedog:transition "+ready+" -->")
	f.EditComment(early, "eve", "Noted, eve says.")
	w := f.AddComment(n, "mallory", "Wait.")
	resolveSteps(t, f, n)
	resolveLabels(t, f, "after mallory's comment", "held")
	f.AddReaction(early, app, "+1")
	f.AddReaction(w, "owner", "+1")
	resolveSettle(t, f, n)
	resolveLabels(t, f, "after the 👍", "clarifying")
}

// A marker in App content a write collaborator edited isn't read (SPEC
// §5.1), on an issue above the /resume's too: one forged on #3 to name
// #1's own /hold doesn't let #2's /resume end that (madeAbove), so the
// /resume, which ends nothing else, is rejected, naming #1, and #1 stays
// held. No pass over #3 has marked the edit yet, so only madeAbove's own
// reading of #3's edits keeps the forged marker out.
func TestResolveForgedMarkerAbove(t *testing.T) {
	f := discFake(t)  // #1
	file(f, 0, ready) // #2
	file(f, 0, ready) // #3
	f.SetParent(1, 2)
	f.SetParent(2, 3)
	hold := f.AddComment(1, "owner", "/hold")
	resolveSteps(t, f, 1)
	resolveLabels(t, f, "after #1's /hold", "held", "ready", "ready")
	f.EditComment(commentIDs(t, f, 3)[0], "mallory", fmt.Sprintf("<!-- issuedog:transition from=ready to=held at=ready reason=owner cmd=%d -->", hold))
	resume := f.AddComment(2, "owner", "/resume")
	resolveSettle(t, f, 2, 1)
	if !resolveRejectedWith(f, 2, resume, madeOn("#1")) {
		t.Errorf("#2: newest comment %q; want the /resume rejected, naming #1", newestComment(f, 2))
	}
	resolveLabels(t, f, "after #2's /resume", "held", "ready")
}

// resolveGoneParent is #2, held by the owner's hand, and #1 under it, held
// for mallory's comment w, by a pass; then the owner's /hold on #2, which
// writes no marker on either, by passes. It returns the fake, w and the
// /hold.
func resolveGoneParent(t testing.TB) (*githubtest.Fake, int64, int64) {
	t.Helper()
	f := discFake(t)
	file(f, 0, "from=ready to=held at=ready reason=owner by=owner event=77") // #2
	f.SetParent(1, 2)
	w := f.AddComment(1, "mallory", "Wait.")
	resolveSteps(t, f, 1)
	hold := f.AddComment(2, "owner", "/hold")
	resolveSteps(t, f, 2, 1)
	return f, w, hold
}

// Which /resume ends what a comment deleted on #2 may have been: one the
// App applied after the deletion, on #2 or on #1 itself, does; one posted
// before the deletion doesn't, as the deleted comment may have been a
// later /hold, failing closed.
func TestResolveGoneEndedBy(t *testing.T) {
	t.Run("a /resume on #2 after the deletion, before the 👍", func(t *testing.T) {
		f, w, hold := resolveGoneParent(t)
		f.DeleteCommentBy("mallory", hold)
		f.AddComment(2, "owner", "/resume")
		resolveSettle(t, f, 2, 1)
		resolveLabels(t, f, "after #2's /resume", "held", "ready")
		f.AddReaction(w, "owner", "+1")
		resolveSettle(t, f, 2, 1)
		resolveLabels(t, f, "after the 👍", "clarifying", "ready")
	})
	t.Run("a /resume on #1 after the deletion, which a later comment kept held", func(t *testing.T) {
		f, _, hold := resolveGoneParent(t)
		f.DeleteCommentBy("mallory", hold)
		f.AddComment(1, "owner", "/resume")
		later := f.AddComment(1, "mallory", "And another thing.")
		resolveSettle(t, f, 2, 1)
		resolveLabels(t, f, "after #1's /resume", "held", "held")
		f.AddReaction(later, "owner", "+1")
		resolveSettle(t, f, 2, 1)
		resolveLabels(t, f, "after the 👍", "clarifying", "held")
	})
	t.Run("a /resume on #2 posted before the deletion, applied after the record", func(t *testing.T) {
		f, w, hold := resolveGoneParent(t)
		f.AddComment(2, "owner", "/resume")
		f.DeleteCommentBy("mallory", hold)
		f.AddReaction(w, "owner", "+1")
		resolveSteps(t, f, 1)
		if body := newestComment(f, 1); !strings.Contains(body, resolveGoneMarker) {
			t.Fatalf("#1: newest comment %q; want the record of the comment deleted on #2", body)
		}
		resolveSettle(t, f, 2, 1)
		resolveLabels(t, f, "after #2's /resume", "held", "held")
	})
	t.Run("a /resume on #2 before the deletion", func(t *testing.T) {
		f, w, hold := resolveGoneParent(t)
		f.AddComment(2, "owner", "/resume")
		resolveSettle(t, f, 2, 1)
		resolveLabels(t, f, "after #2's /resume", "held", "ready")
		f.DeleteCommentBy("mallory", hold)
		f.AddReaction(w, "owner", "+1")
		resolveSettle(t, f, 2, 1)
		resolveLabels(t, f, "after the 👍", "held", "held")
		if body := newestComment(f, 1); !strings.Contains(body, resolveGoneMarker) {
			t.Errorf("#1: newest comment %q; want the record of the comment deleted on #2", body)
		}
	})
}

// Only a deletion that may have hidden a code owner's /hold during the
// hold counts: not one before the hold began, nor one of a comment by a
// user who isn't trusted. A comment by someone GitHub doesn't name, and
// two deleted on the same issue, do; the record names that issue once.
func TestResolveGoneWhich(t *testing.T) {
	t.Run("deleted before the hold", func(t *testing.T) {
		f := discFake(t)
		file(f, 0, ready) // #2
		f.SetParent(1, 2)
		note := f.AddComment(2, "owner", "Note.")
		f.DeleteCommentBy("mallory", note)
		resolveSettle(t, f, 2)
		f.AddReaction(commentIDs(t, f, 2)[len(commentIDs(t, f, 2))-1], "owner", "+1") // the hold's notice
		resolveSettle(t, f, 2)
		resolveLabels(t, f, "after the notice's 👍", "ready", "clarifying")
		w := f.AddComment(1, "mallory", "Wait.")
		resolveSettle(t, f, 1)
		f.AddReaction(w, "owner", "+1")
		resolveSettle(t, f, 2, 1)
		resolveLabels(t, f, "after the 👍", "clarifying", "clarifying")
	})
	t.Run("an untrusted user's comment", func(t *testing.T) {
		f, w, _ := resolveGoneParent(t)
		f.SetPermission("eve", "read")
		aside := f.AddComment(2, "eve", "An aside.")
		f.DeleteCommentBy("mallory", aside)
		f.AddReaction(w, "owner", "+1")
		resolveSettle(t, f, 2, 1)
		resolveLabels(t, f, "after the 👍", "held", "held")
		if body := newestComment(f, 1); strings.Contains(body, resolveGoneMarker) || !strings.Contains(body, "a `/hold` on an issue above it") {
			t.Errorf("#1: newest comment %q; want it held by the /hold on #2 alone", body)
		}
	})
	t.Run("deleted by a user without write access", func(t *testing.T) {
		f, w, _ := resolveGoneParent(t)
		f.SetPermission("eve", "read")
		note := f.AddComment(2, "owner", "Note.")
		f.DeleteCommentBy("eve", note)
		f.AddReaction(w, "owner", "+1")
		resolveSettle(t, f, 2, 1)
		if body := newestComment(f, 1); strings.Contains(body, resolveGoneMarker) || !strings.Contains(body, "a `/hold` on an issue above it") {
			t.Errorf("#1: newest comment %q; want it held by the /hold on #2 alone", body)
		}
	})
	t.Run("a comment by someone GitHub doesn't name", func(t *testing.T) {
		f, w, _ := resolveGoneParent(t)
		ghost := f.AddComment(2, "", "Gone.")
		f.DeleteCommentBy("mallory", ghost)
		f.AddReaction(w, "owner", "+1")
		resolveSettle(t, f, 2, 1)
		if body := newestComment(f, 1); !strings.Contains(body, resolveGoneMarker) {
			t.Errorf("#1: newest comment %q; want the record of the comment deleted on #2", body)
		}
	})
	t.Run("two deleted on #2", func(t *testing.T) {
		f, w, hold := resolveGoneParent(t)
		note := f.AddComment(2, "owner", "Note.")
		f.DeleteCommentBy("mallory", hold)
		f.DeleteCommentBy("mallory", note)
		f.AddReaction(w, "owner", "+1")
		resolveSettle(t, f, 2, 1)
		body := newestComment(f, 1)
		if !strings.HasPrefix(body, resolveGoneText+"\n\ncc @owner\n\n"+resolveGoneMarker) || strings.Count(body, ",2-") != 1 {
			t.Errorf("#1: newest comment %q; want the record naming #2 once, and both deletions", body)
		}
	})
}

// #3 > #2 > #1. The owner's /hold on #3 is posted before the one on #2,
// but #2's pass comes first, so #2's /hold moves #2 and #1 into held, its
// marker on both, and #3's then writes none on them. #2's /hold isn't
// stacked on #3's, so #3's /resume ends #3's hold alone, and #2 and #1
// stay held by #2's.
func TestResolveMarkedHoldNotStacked(t *testing.T) {
	f := discFake(t)
	file(f, 0, ready) // #2
	file(f, 0, ready) // #3
	f.SetParent(1, 2)
	f.SetParent(2, 3)
	f.AddComment(3, "owner", "/hold")
	f.AddComment(2, "owner", "/hold")
	resolveSettle(t, f, 2)
	resolveSettle(t, f, 3)
	resolveLabels(t, f, "after the /holds", "held", "held", "held")
	f.AddComment(3, "owner", "/resume")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after #3's /resume", "held", "held", "ready")
}

// #1's own /hold, posted while it was held for mallory's comment, comes
// before the owner's /hold on #2, which writes no marker on #1, so it
// isn't stacked on that: #2's /resume ends #2's hold, and #1 stays held by
// its own /hold once mallory's comment is resolved.
func TestResolveOwnHoldBeforeParentHold(t *testing.T) {
	f, w, _ := resolveOwnOnDiscussion(t)
	f.AddComment(2, "owner", "/hold")
	resolveSettle(t, f, 2, 1)
	resolveLabels(t, f, "after #2's /hold", "held", "held")
	f.AddComment(2, "owner", "/resume")
	resolveSettle(t, f, 2, 1)
	f.AddReaction(w, "owner", "+1")
	resolveSettle(t, f, 2, 1)
	resolveLabels(t, f, "after the 👍", "held", "ready")
}

// #3 > #2 > #1. An earlier hold on #2, by #3's /hold, ended with #3's
// /resume; then mallory's comment held #2, and #2's own /hold, which
// writes no marker on #2, sits on that discussion hold alone, not on the
// earlier one, and writes a marker on #1. A later /hold and /resume on #3
// end neither, on #2 or on #1.
func TestResolveOwnHoldNotStackedOnEarlierHold(t *testing.T) {
	f := discFake(t)
	file(f, 0, ready) // #2
	file(f, 0, ready) // #3
	f.SetParent(1, 2)
	f.SetParent(2, 3)
	f.AddComment(3, "owner", "/hold")
	resolveSettle(t, f, 3)
	f.AddComment(3, "owner", "/resume")
	resolveSettle(t, f, 3)
	resolveLabels(t, f, "after #3's first /resume", "ready", "ready", "ready")
	w := f.AddComment(2, "mallory", "Wait.")
	resolveSettle(t, f, 2)
	f.AddComment(2, "owner", "/hold")
	resolveSettle(t, f, 2)
	f.AddComment(3, "owner", "/hold")
	resolveSettle(t, f, 3)
	f.AddComment(3, "owner", "/resume")
	resolveSettle(t, f, 3, 2, 1)
	f.AddReaction(w, "owner", "+1")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "at the end", "held", "held", "ready")
}

// #3 > #2 > #1, #1 held for mallory's comment throughout. #3's /hold,
// which writes no marker on #2, held for discussion then, nor on #1, held
// #2 in a hold that #2's /resume ended. Then mallory's comment held #2
// again, and #2's own /hold, which writes no marker, sits on that alone,
// not on #3's earlier /hold, so #3's /resume doesn't end it, on #2 or on
// #1, and both stay held by it once the discussions are resolved.
func TestResolveOwnHoldNotStackedOnEndedHoldAbove(t *testing.T) {
	f := discFake(t)
	file(f, 0, ready) // #2
	file(f, 0, ready) // #3
	f.SetParent(1, 2)
	f.SetParent(2, 3)
	w1 := f.AddComment(1, "mallory", "Wait 1.")
	f.AddComment(2, "mallory", "Wait 2.")
	resolveSettle(t, f, 1, 2)
	f.AddComment(3, "owner", "/hold")
	resolveSettle(t, f, 3, 2, 1)
	f.AddComment(2, "owner", "/resume")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after #2's /resume", "held", "ready", "held")
	w3 := f.AddComment(2, "mallory", "Wait 3.")
	resolveSettle(t, f, 2)
	f.AddComment(2, "owner", "/hold")
	resolveSettle(t, f, 2, 1)
	f.AddComment(3, "owner", "/resume")
	resolveSettle(t, f, 3, 2, 1)
	f.AddReaction(w1, "owner", "+1")
	f.AddReaction(w3, "owner", "+1")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "at the end", "held", "held", "ready")
}
