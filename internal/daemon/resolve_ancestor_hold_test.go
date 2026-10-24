package daemon_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// A parent's /hold while the child is held for discussion writes no
// marker on the child (SPEC §6.4), but keeps it held once its discussion is
// resolved: the record names the parent's /hold, so no later pass reads
// the parent again. The parent's /resume then sends the child to
// clarifying.
func TestResolveParentHoldKeepsChild(t *testing.T) {
	t.Parallel()
	var hold, c int64
	setup := func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		file(f, 1, ready)
		c = f.AddComment(2, "mallory", "Wait.")
		if err := resolvePass(t, 2)(f); err != nil {
			t.Fatal(err)
		}
		hold = f.AddComment(1, "owner", "/hold")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:held"}) || strings.Contains(newestComment(f, 2), fmt.Sprintf("cmd=%d", hold)) {
			t.Fatalf("labels of #1 %q, newest comment on #2 %q; want #1 held, and no marker of the /hold on #2", l, newestComment(f, 2))
		}
		f.ForgetWrites()
		f.AddReaction(c, "owner", "+1")
		return f
	}
	resolveCheck(t, setup, 2, func(f *githubtest.Fake) issueWant {
		return issueWant{[]string{"stage:held"}, []string{fmt.Sprintf("The discussion is resolved, but this issue stays on hold: "+
			"a `/hold` on an issue above it, in comment %d on #1, hasn't ended. `/resume` there, or here, ends that, and then it goes to `clarifying`."+
			"\n\ncc @owner\n\n<!-- issuedog:ended reason=discussion held=%d heldon=1 resolved=%d -->", hold, hold, resolveReactionAt(t, f, c))}}
	})
	f := run(t, setup, resolvePass(t, 2))
	from := len(f.Requests())
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.Requests()[from:] {
		if strings.HasPrefix(r.Path, "/issues/1") {
			t.Errorf("a later pass sent %+v; want the parent not read again", r)
		}
	}
	resume := f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l, body := f.State().Issues[1].Labels, newestComment(f, 2); !slices.Equal(l, []string{"stage:clarifying"}) ||
		!strings.Contains(body, fmt.Sprintf("from=held to=clarifying at=ready origin=ready cmd=%d ", resume)) {
		t.Errorf("#2: labels %q, newest comment %q; want it sent to clarifying by #1's /resume", l, body)
	}
}

// A /resume on the parent posted before its /hold doesn't end that /hold:
// the child stays held once its discussion is resolved.
func TestResolveEarlierResumeAbove(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	file(f, 1, ready)
	c := f.AddComment(2, "mallory", "Wait.")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(1, "owner", "/hold")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.AddReaction(c, "owner", "+1")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	if l, body := f.State().Issues[1].Labels, newestComment(f, 2); !slices.Equal(l, []string{"stage:held"}) || !strings.Contains(body, "held=") {
		t.Errorf("#2: labels %q, newest comment %q; want it held by #1's /hold, with the record", l, body)
	}
}

// A parent's /hold, then its /resume, both while the child is held for
// discussion: once the discussion is resolved, the child goes to
// clarifying, the parent's /hold ended.
func TestResolveParentHoldThenResume(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	file(f, 1, ready)
	c := f.AddComment(2, "mallory", "Wait.")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(1, "owner", "/hold")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.AddReaction(c, "owner", "+1")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	if l := f.State().Issues[1].Labels; !slices.Equal(l, []string{"stage:clarifying"}) {
		t.Errorf("#2: labels %q, newest comment %q; want clarifying", l, newestComment(f, 2))
	}
}

// A /hold on an issue between a grandparent and its grandchild, posted
// while both were held, writes no marker (SPEC §6.4), but still ends only by
// /resume on that issue or below it (#188's decision, round 3): the
// grandparent's /resume, with the grandparent out of held by an override,
// would end only its own /hold below, which leaves both held by the one
// between, so it's rejected, naming that issue. Its /resume there then
// takes both back.
func TestResolveHoldBetween(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	file(f, 1, ready) // #2, under #1
	file(f, 2, ready) // #3, under #2
	f.AddComment(1, "owner", "/hold")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.RemoveLabel(1, "owner", "stage:held")
	f.AddLabel(1, "owner", "stage:ready")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(2, "owner", "/hold")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	st := f.State()
	if body := newestComment(f, 1); !slices.ContainsFunc(st.Issues[0].Comments, func(c githubtest.CommentState) bool {
		return c.Body == "/resume" && slices.Contains(c.Reactions, slug+"[bot] -1")
	}) || !strings.Contains(body, madeOn("#2", 2)) {
		t.Errorf("newest comment on #1 %q; want the /resume rejected, 👎'd, naming #2", body)
	}
	for i := 1; i < 3; i++ {
		if l := st.Issues[i].Labels; !slices.Equal(l, []string{"stage:held"}) {
			t.Errorf("#%d: labels %q; want it still held", i+1, l)
		}
	}
	f.AddComment(2, "owner", "/resume")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 3; i++ {
		if l := f.State().Issues[i].Labels; !slices.Equal(l, []string{"stage:ready"}) {
			t.Errorf("after /resume on #2: #%d: labels %q; want it back at ready", i+1, l)
		}
	}
}

// With the grandparent still held, its /resume applies, but ends neither
// the /hold between nor the grandchild's hold by it (#188's decision,
// round 3): the grandparent goes back, and its move names the issues
// still on hold. /resume on the issue between then takes both back.
func TestResolveHoldBetweenApplied(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	file(f, 1, ready) // #2, under #1
	file(f, 2, ready) // #3, under #2
	f.AddComment(1, "owner", "/hold")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(2, "owner", "/hold")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	st := f.State()
	for i, want := range []string{"stage:ready", "stage:held", "stage:held"} {
		if l := st.Issues[i].Labels; !slices.Equal(l, []string{want}) {
			t.Errorf("#%d: labels %q; want %s", i+1, l, want)
		}
	}
	if body := newestComment(f, 1); !strings.Contains(body, "Still on hold: #2 and #3, by a hold this `/resume` doesn't end") {
		t.Errorf("newest comment on #1 %q; want it to name #2 and #3 as still on hold", body)
	}
	f.AddComment(2, "owner", "/resume")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	st = f.State()
	for i, want := range []string{"stage:ready", "stage:ready", "stage:ready"} {
		if l := st.Issues[i].Labels; !slices.Equal(l, []string{want}) {
			t.Errorf("after /resume on #2: #%d: labels %q; want %s", i+1, l, want)
		}
	}
}

// A /resume on a parent that isn't held, whose child is held only for a
// discussion with an open trigger, is rejected, naming the child: a
// /resume on an ancestor doesn't end a discussion hold.
func TestResolveResumeOnParentOfDiscussion(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	file(f, 1, ready)
	f.AddComment(2, "mallory", "Wait.")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	resume := f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	want := final("/resume", "I didn't apply this command", "#2 is on hold for discussion, which only a code owner's response there, "+
		"or `/resume` there, ends", fmt.Sprint(resume))
	if body := newestComment(f, 1); body != want || !slices.Equal(f.State().Issues[1].Labels, []string{"stage:held"}) {
		t.Errorf("newest comment on #1 %q, #2's labels %q; want %q, and #2 held", body, f.State().Issues[1].Labels, want)
	}
}

// When the last trigger goes with no act to resolve it, as mallory
// deleting her own comment, an ancestor's /hold found keeps the issue
// held, and the record naming it is written all the same (SPEC §6.4), with
// no resolved time, so no later pass reads the ancestors again: an
// unchanged pass costs no rate limit.
func TestResolveAncestorHoldWithNoAct(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	for n := 1; n <= 3; n++ {
		fileAt(f, "ready", "do it")
		f.SetType(n, "task")
		if n > 1 {
			f.SetParent(n, n-1)
		}
	}
	d := resolveDaemon(t, f, newOrigin(t))
	f.AddComment(2, "owner", "/hold")
	iterate(t, d)
	c := f.AddComment(3, "mallory", "Wait.")
	iterate(t, d)
	f.AddComment(2, "owner", "/resume")
	for range 2 {
		iterate(t, d)
	}
	if body := newestComment(f, 3); !strings.Contains(body, "issuedog:ended reason=owner") {
		t.Fatalf("newest comment on #3 %q; want the record that #2's /resume ended its owner hold", body)
	}
	hold := f.AddComment(1, "owner", "/hold")
	iterate(t, d)
	f.DeleteCommentBy("mallory", c)
	for range 3 {
		iterate(t, d)
	}
	want := fmt.Sprintf("No discussion trigger is open on this issue, but it stays on hold: a `/hold` on an issue above it, in comment %d on #1, "+
		"hasn't ended. `/resume` there, or here, ends that.\n\n<!-- issuedog:ended reason=discussion held=%d heldon=1 -->", hold, hold)
	if l, body := f.State().Issues[2].Labels, newestComment(f, 3); !slices.Equal(l, []string{"stage:held"}) || body != want {
		t.Fatalf("#3: labels %q, newest comment %q; want it held, with %q", l, body, want)
	}
	writes, requests := len(f.Writes()), len(f.Requests())
	iterate(t, d)
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("an unchanged pass wrote %+v; want nothing", w)
	}
	for _, r := range f.Requests()[requests:] {
		if r.Status != http.StatusNotModified {
			t.Errorf("an unchanged pass sent %+v; want only requests answered 304", r)
		}
	}
	f.AddComment(1, "owner", "/resume")
	for range 2 {
		iterate(t, d)
	}
	if l, body := f.State().Issues[2].Labels, newestComment(f, 3); !slices.Equal(l, []string{"stage:ready"}) {
		t.Errorf("#3: labels %q, newest comment %q; want it back at ready once #1's /resume ends the /hold, no discussion having ended", l, body)
	}
}

// The walk up for ancestors' /holds stops at a parent in another repo, as
// madeAbove's does: its /hold keeps nothing held here.
func TestResolveAboveStopsAtOtherRepo(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	file(f, 1, ready)
	c := f.AddComment(2, "mallory", "Wait.")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(1, "owner", "/hold")
	f.Transfer(1, "o/other")
	f.AddReaction(c, "owner", "+1")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	if l := f.State().Issues[1].Labels; !slices.Equal(l, []string{"stage:clarifying"}) {
		t.Errorf("#2: labels %q, newest comment %q; want clarifying, the parent in another repo", l, newestComment(f, 2))
	}
}

// Replay over the record that names an ancestor's /hold with no act to
// resolve the last trigger, as mallory deleting her own comment.
func TestResolveHeldRecordReplay(t *testing.T) {
	t.Parallel()
	var hold int64
	setup := func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		for n := 1; n <= 3; n++ {
			fileAt(f, "ready", "do it")
			f.SetType(n, "task")
			if n > 1 {
				f.SetParent(n, n-1)
			}
		}
		step := func(n int) {
			if err := resolvePass(t, n)(f); err != nil {
				t.Fatal(err)
			}
		}
		f.AddComment(2, "owner", "/hold")
		step(2)
		c := f.AddComment(3, "mallory", "Wait.")
		step(3)
		f.AddComment(2, "owner", "/resume")
		step(2)
		step(3)
		hold = f.AddComment(1, "owner", "/hold")
		step(1)
		f.ForgetWrites()
		f.DeleteCommentBy("mallory", c)
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 3))
	f := run(t, setup, resolvePass(t, 3))
	if l, body := f.State().Issues[2].Labels, newestComment(f, 3); !slices.Equal(l, []string{"stage:held"}) ||
		!strings.Contains(body, fmt.Sprintf("<!-- issuedog:ended reason=discussion held=%d heldon=1 -->", hold)) {
		t.Errorf("#3: labels %q, newest comment %q; want it held, with the record naming #1's /hold", l, body)
	}
}

// resolveParentHoldChildResume is #1 at ready, and #2 under it held by
// mallory's comment; the owner's /hold on #1, then, if first, before the
// owner's /resume on #2 with mallory's late comment, which keeps #2 held,
// by passes. It returns the fake, the /hold's ID and the late comment's.
func resolveParentHoldChildResume(t testing.TB, first bool) (*githubtest.Fake, int64, int64) {
	f := discFake(t)
	file(f, 1, ready)
	step := func(n int) {
		if err := resolvePass(t, n)(f); err != nil {
			t.Fatal(err)
		}
	}
	f.AddComment(2, "mallory", "Wait.")
	step(2)
	var hold int64
	if first {
		hold = f.AddComment(1, "owner", "/hold")
		step(1)
	}
	f.AddComment(2, "owner", "/resume")
	late := f.AddComment(2, "mallory", "One more thing.")
	step(2)
	if l, body := f.State().Issues[1].Labels, newestComment(f, 2); !slices.Equal(l, []string{"stage:held"}) || !strings.Contains(body, "cmd=") {
		t.Fatalf("#2: labels %q, newest comment %q; want it held, with its /resume's record", l, body)
	}
	if !first {
		hold = f.AddComment(1, "owner", "/hold")
		step(1)
	}
	return f, hold, late
}

// A /resume on the issue itself ends a /hold on its parent posted before
// it (SPEC §6.4): once the late trigger is resolved, the issue goes to
// clarifying, with no record naming the parent's /hold.
func TestResolveOwnResumeEndsParentHold(t *testing.T) {
	t.Parallel()
	setup := func(t testing.TB) *githubtest.Fake {
		f, _, late := resolveParentHoldChildResume(t, true)
		f.ForgetWrites()
		f.AddReaction(late, "owner", "+1")
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 2))
	f := run(t, setup, resolvePass(t, 2))
	if l, body := f.State().Issues[1].Labels, newestComment(f, 2); !slices.Equal(l, []string{"stage:clarifying"}) ||
		!strings.HasPrefix(body, "The discussion is resolved, by `owner`, so this issue leaves its hold") || strings.Contains(body, "held=") {
		t.Errorf("#2: labels %q, newest comment %q; want it at clarifying, with no record of #1's /hold", l, body)
	}
}

// A /hold on the parent posted after the issue's own /resume still keeps
// it held once the late trigger is resolved, with the record naming it.
func TestResolveParentHoldAfterOwnResume(t *testing.T) {
	t.Parallel()
	f, hold, late := resolveParentHoldChildResume(t, false)
	f.AddReaction(late, "owner", "+1")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	if l, body := f.State().Issues[1].Labels, newestComment(f, 2); !slices.Equal(l, []string{"stage:held"}) ||
		!strings.Contains(body, fmt.Sprintf("held=%d ", hold)) {
		t.Errorf("#2: labels %q, newest comment %q; want it held, with the record naming #1's /hold", l, body)
	}
}

// A /hold on the /resume's own issue, known on the path between by its
// marker there, is the /resume's to end (resolveHeldBy, heldStackEnds):
// #3's /hold holds #3 and #2 with markers, and #1, held for mallory's
// comment, with none. #3's /resume ends it on #1 too, which stays held for
// discussion, with the record that it ended (resolveResumed), so once
// mallory's comment is resolved, #1 goes to clarifying. With #3 and #2
// set to ready by hand first, it's the same: the /resume applies for #1
// alone, as #3, out of held, would have no marker of it.
func TestResolveResumeEndsOwnIssueHoldBelow(t *testing.T) {
	t.Parallel()
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprintf("override=%t", override), func(t *testing.T) {
			var hold, resume int64
			var w int64
			setup := func(t testing.TB) *githubtest.Fake {
				f := discFake(t)
				file(f, 0, ready)
				file(f, 0, ready)
				f.SetParent(1, 2)
				f.SetParent(2, 3)
				w = f.AddComment(1, "mallory", "Wait.")
				resolveSteps(t, f, 1)
				hold = f.AddComment(3, "owner", "/hold")
				resolveSteps(t, f, 3)
				if override {
					for _, n := range []int{3, 2} {
						f.RemoveLabel(n, "owner", "stage:held")
						f.AddLabel(n, "owner", "stage:ready")
						resolveSteps(t, f, n)
					}
				}
				resume = f.AddComment(3, "owner", "/resume")
				f.ForgetWrites()
				return f
			}
			githubtest.Replay(t, setup, resolvePass(t, 3))
			f := run(t, setup, resolvePass(t, 3))
			if l := f.State().Issues[1].Labels; !slices.Equal(l, []string{"stage:ready"}) {
				t.Fatalf("#2: labels %q; want it at ready", l)
			}
			if body := newestComment(f, 1); !strings.Contains(body, fmt.Sprintf("<!-- issuedog:ended reason=owner key=c%d cmd=%d -->", hold, resume)) {
				t.Fatalf("#1: newest comment %q; want the record that #3's /resume ended #3's /hold", body)
			}
			f.AddReaction(w, "owner", "+1")
			resolveSteps(t, f, 1)
			if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:clarifying"}) {
				t.Errorf("#1: labels %q, newest comment %q; want it at clarifying", l, newestComment(f, 1))
			}
		})
	}
}

// A marker in App content a write collaborator edited isn't read (SPEC
// §5.1), on an issue above the /resume's too: one forged on #3 to name
// #1's own /hold doesn't let #2's /resume end that (madeAbove), so the
// /resume, which ends nothing else, is rejected, naming #1, and #1 stays
// held. No pass over #3 has marked the edit yet, so only madeAbove's own
// reading of #3's edits keeps the forged marker out.
func TestResolveForgedMarkerAbove(t *testing.T) {
	t.Parallel()
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
	if !resolveRejectedWith(f, 2, resume, madeOn("#1", 1)) {
		t.Errorf("#2: newest comment %q; want the /resume rejected, naming #1", newestComment(f, 2))
	}
	resolveLabels(t, f, "after #2's /resume", "held", "ready")
}
