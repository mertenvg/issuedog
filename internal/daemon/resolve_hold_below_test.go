package daemon_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// Every owner hold ends by /resume on the held issue, on the issue where
// its /hold was posted, or on any issue between the two, and by no other,
// a child's own /hold that wrote no marker as the child was held already
// included (SPEC §6.4). The /resume path and the leave path judge by the
// same rule (resolveEndsHold), and count a /hold by the App's record that
// it applied it, whatever a write collaborator made of its comment since,
// so they agree in either order, across a restart.

// A /hold that wrote no marker never ends with a /resume above it (SPEC
// §6.4); the /resume path and the leave path agree, whichever /holds
// above the /resume's issue they read.

// #3 > #2 > #1: #3's /hold held all three; #1's own /resume took #1 out;
// then #2's /hold, which writes no marker on #2, held by #3's, but one on
// #1; then mallory's comment on #1. #3's /resume ends #3's hold on #2, but
// not #2's /hold, posted below it, on #2 or on #1 (#188's decision, round
// 3): #2 stays held by its own /hold, with the record that #3's ended,
// and #1 by #2's and for mallory's comment, with no record. Once the owner's 👍
// resolves that, #1 still stays held, until #2's /resume, which takes #2
// back and sends #1 to clarifying.
func TestResolveHoldMarkedBelowNotEndedFromAbove(t *testing.T) {
	t.Parallel()
	var w, between, top int64
	setup := func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		file(f, 0, ready) // #2
		file(f, 0, ready) // #3
		f.SetParent(1, 2)
		f.SetParent(2, 3)
		top = f.AddComment(3, "owner", "/hold")
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
		f.AddComment(3, "owner", "/resume")
		f.ForgetWrites()
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 3))
	f := run(t, setup, resolvePass(t, 3))
	resolveLabels(t, f, "after #3's /resume", "held", "held", "ready")
	// #2 records that #3's /hold ended there, though #2's own stays
	// (SPEC §6.4); #1 was in no hold of #3's.
	if body := newestComment(f, 2); !strings.Contains(body, fmt.Sprintf("<!-- issuedog:ended reason=owner key=c%d cmd=", top)) {
		t.Errorf("#2: newest comment %q; want the record that #3's /hold ended", body)
	}
	if body := newestComment(f, 1); strings.Contains(body, "issuedog:ended") {
		t.Errorf("#1: newest comment %q; want no record that an owner hold ended", body)
	}
	f.AddReaction(w, "owner", "+1")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after the 👍", "held", "held", "ready")
	f.AddComment(2, "owner", "/resume")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after #2's /resume", "clarifying", "ready", "ready")
}

// #1 > #2 > #3 > #4, mallory's comments holding #3 and #4. The owner's
// /hold on #1 writes markers on #1 and #2, none on #3 or #4; the owner's
// /hold on #3 writes none either, as #3 is held already. #2's /resume ends
// #1's hold below it, but not #3's /hold, posted below #2. So once the
// owner's 👍s resolve mallory's comments, #3 and #4 both stay held by #3's
// /hold, whether the 👍s come before #2's /resume or after it, with a
// restart between, until #3's /resume, which sends both to clarifying.
func TestResolveHoldAboveResumeNotABase(t *testing.T) {
	t.Parallel()
	for _, thumbsFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("thumbs first %v", thumbsFirst), func(t *testing.T) {
			l := newResolveLoop(t, newFake(t))
			top := l.task(0)
			mid := l.task(top)
			x := l.task(mid)
			c := l.task(x)
			l.settle("start")
			wx := l.f.AddComment(x, "mallory", "Wait X.")
			wc := l.f.AddComment(c, "mallory", "Wait C.")
			l.settle("comments")
			l.f.AddComment(top, "owner", "/hold")
			l.settle("#1's /hold")
			for _, n := range []int{x, c} {
				if body := newestComment(l.f, n); strings.Contains(body, "reason=owner") {
					t.Fatalf("#%d: newest comment %q; want no marker of #1's /hold", n, body)
				}
			}
			l.f.AddComment(x, "owner", "/hold")
			l.settle("#3's /hold")
			thumbs := func() {
				l.f.AddReaction(wx, "owner", "+1")
				l.f.AddReaction(wc, "owner", "+1")
				l.settle("the 👍s")
			}
			if thumbsFirst {
				thumbs()
				l.restart()
				l.f.AddComment(mid, "owner", "/resume")
				l.settle("#2's /resume")
			} else {
				l.f.AddComment(mid, "owner", "/resume")
				l.settle("#2's /resume")
				l.restart()
				thumbs()
			}
			resolveLabels(t, l.f, "after both", "held", "ready", "held", "held")
			for _, n := range []int{x, c} {
				if !anyComment(l.f, n, "hasn't ended") {
					t.Errorf("#%d: newest comment %q; want the record that a hold on it hasn't ended", n, newestComment(l.f, n))
				}
			}
			l.restart()
			l.settle("a restart")
			resolveLabels(t, l.f, "after a restart", "held", "ready", "held", "held")
			l.f.AddComment(x, "owner", "/resume")
			l.settle("#3's /resume")
			resolveLabels(t, l.f, "after #3's /resume", "held", "ready", "clarifying", "clarifying")
		})
	}
}

// #4 > #3 > #2: the owner's /hold on #4 writes markers on #3 and #2, held
// by none, and on #4 unless the owner held it by hand first; #2's own
// /hold, none, as #2 is held by #4's. #3's /resume ends #4's hold on #3
// and #2, but not #2's /hold, posted below #3, whether #4's /hold's
// comment is there, with a marker on #4 or not, or deleted, known by its
// markers, the farthest on #4. #1 under #2, held by
// #4's /hold, and by #2's, which wrote no marker there, and for mallory's
// comment, agrees once that is resolved: both stay held, until #2's
// /resume.
func TestResolveMarkedHoldAboveResumeNotABase(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		byHand, gone bool
	}{
		{"#4's /hold with its marker", false, false},
		{"#4 held by hand first, so #4's /hold writes no marker there", true, false},
		{"#4's /hold deleted", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := discFake(t)  // #1
			file(f, 0, ready) // #2
			file(f, 0, ready) // #3
			if tt.byHand {
				file(f, 0, "from=ready to=held at=ready reason=owner by=owner event=77") // #4
			} else {
				file(f, 0, ready) // #4
			}
			f.SetParent(1, 2)
			f.SetParent(2, 3)
			f.SetParent(3, 4)
			hold := f.AddComment(4, "owner", "/hold")
			resolveSettle(t, f, 4)
			resolveLabels(t, f, "after #4's /hold", "held", "held", "held", "held")
			if marked := strings.Contains(newestComment(f, 4), "reason=owner cmd="); marked == tt.byHand {
				t.Fatalf("#4: newest comment %q; want a marker of its /hold unless it was held by hand", newestComment(f, 4))
			}
			f.AddComment(2, "owner", "/hold")
			resolveSettle(t, f, 2)
			if tt.gone {
				f.DeleteComment(hold)
			}
			w := f.AddComment(1, "mallory", "Wait.")
			resolveSettle(t, f, 1)
			f.AddComment(3, "owner", "/resume")
			resolveSettle(t, f, 4, 3, 2, 1)
			resolveLabels(t, f, "after #3's /resume", "held", "held", "ready", "held")
			f.AddReaction(w, "owner", "+1")
			resolveSettle(t, f, 4, 3, 2, 1)
			resolveLabels(t, f, "after the 👍", "held", "held", "ready", "held")
			f.AddComment(2, "owner", "/resume")
			resolveSettle(t, f, 4, 3, 2, 1)
			resolveLabels(t, f, "after #2's /resume", "clarifying", "ready", "ready", "held")
		})
	}
}

// #3 > #2 > #1. #3's /hold, which writes no marker on #1, held for
// mallory's comment, ended there with #1's /resume; then mallory's next
// comment held #1 again, and the owner's /hold on #2, which writes no
// marker on #2, held by #3's, nor on #1, and #1's own /hold, which writes
// none either. #3's /resume ends #3's hold on #2 alone: #2's /hold and
// #1's were posted below #3 (#188's decision, round 3), so #2 and #1 stay
// held, #1 still once mallory's comment is resolved. #1's /resume then
// sends #1 to clarifying, and #2 stays held by its own /hold until #2's
// /resume.
func TestResolveHoldsBetweenNotEndedFromAbove(t *testing.T) {
	t.Parallel()
	f := discFake(t)  // #1
	file(f, 0, ready) // #2
	file(f, 0, ready) // #3
	f.SetParent(1, 2)
	f.SetParent(2, 3)
	f.AddComment(1, "mallory", "Wait.")
	resolveSettle(t, f, 1)
	f.AddComment(3, "owner", "/hold")
	resolveSettle(t, f, 3, 2, 1)
	f.AddComment(1, "owner", "/resume")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after #1's /resume", "ready", "held", "held")
	w := f.AddComment(1, "mallory", "Wait again.")
	resolveSettle(t, f, 1)
	f.AddComment(2, "owner", "/hold")
	resolveSettle(t, f, 2, 1)
	f.AddComment(1, "owner", "/hold")
	resolveSettle(t, f, 1)
	f.AddComment(3, "owner", "/resume")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after #3's /resume", "held", "held", "ready")
	f.AddReaction(w, "owner", "+1")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after the 👍", "held", "held", "ready")
	f.AddComment(1, "owner", "/resume")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after #1's /resume", "clarifying", "held", "ready")
	f.AddComment(2, "owner", "/resume")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after #2's /resume", "clarifying", "ready", "ready")
}

// holdBelowCase is a family of tasks, levels deep, issues[0] at the top, each
// the parent of the next, and what happens to it:
//   - mallory comments on each issue in discussed, which holds it;
//   - with byHand, the owner holds issues[holds[0]] by hand first, so its
//     /hold writes no marker there;
//   - the owner posts /hold on each issue in holds, in order;
//   - with touch, mallory edits or deletes the first of those /holds;
//   - the owner posts /resume on issues[resume], and 👍s each of mallory's
//     comments, in either order, with a restart between;
//   - want is each issue's stage then, and records are the issues that
//     then carry the record that an owner hold on them hasn't ended;
//   - last, the owner posts /resume on issues[release], and final is each
//     issue's stage then.
type holdBelowCase struct {
	name            string
	levels          int
	discussed       []int
	byHand          bool
	holds           []int
	touch           string
	resume, release int
	want, final     []string
	records         []int
	unendedText     string
}

// run runs c, the 👍s first or the /resume first.
func (c holdBelowCase) run(t *testing.T, thumbsFirst bool) {
	l := newResolveLoop(t, newFake(t))
	issues := make([]int, c.levels)
	for i := range issues {
		parent := 0
		if i > 0 {
			parent = issues[i-1]
		}
		issues[i] = l.task(parent)
	}
	l.settle("start")
	var ws []int64
	for _, i := range c.discussed {
		ws = append(ws, l.f.AddComment(issues[i], "mallory", fmt.Sprintf("Wait on #%d.", issues[i])))
	}
	l.settle("mallory's comments")
	if c.byHand {
		n := issues[c.holds[0]]
		l.f.RemoveLabel(n, "owner", "stage:ready")
		l.f.AddLabel(n, "owner", "stage:held")
		l.settle("the hold by hand")
	}
	var holds []int64
	for _, i := range c.holds {
		holds = append(holds, l.f.AddComment(issues[i], "owner", "/hold"))
		l.settle(fmt.Sprintf("the /hold on #%d", issues[i]))
	}
	if c.byHand && strings.Contains(newestComment(l.f, issues[c.holds[0]]), fmt.Sprintf("cmd=%d", holds[0])) {
		t.Fatalf("#%d: newest comment %q; want no marker of its /hold, as it was held by hand", issues[c.holds[0]], newestComment(l.f, issues[c.holds[0]]))
	}
	switch c.touch {
	case "edited":
		l.f.EditComment(holds[0], "mallory", "Never mind.")
	case "deleted":
		l.delete(issues[c.holds[0]], holds[0])
	}
	l.settle("mallory's touch")
	thumbs := func() {
		for _, w := range ws {
			l.f.AddReaction(w, "owner", "+1")
		}
		l.settle("the 👍s")
	}
	resume := func() {
		l.f.AddComment(issues[c.resume], "owner", "/resume")
		l.settle(fmt.Sprintf("the /resume on #%d", issues[c.resume]))
	}
	if thumbsFirst {
		thumbs()
		l.restart()
		resume()
	} else {
		resume()
		l.restart()
		thumbs()
	}
	l.stages("after both", issues, c.want...)
	for _, i := range c.records {
		if !anyComment(l.f, issues[i], c.unendedText) {
			t.Errorf("#%d: newest comment %q; want the record that an owner hold on it hasn't ended", issues[i], newestComment(l.f, issues[i]))
		}
	}
	l.restart()
	l.settle("a restart")
	l.stages("after a restart", issues, c.want...)
	l.unchanged("after a restart")
	l.f.AddComment(issues[c.release], "owner", "/resume")
	l.settle(fmt.Sprintf("the /resume on #%d", issues[c.release]))
	l.stages(fmt.Sprintf("after the /resume on #%d", issues[c.release]), issues, c.final...)
	l.restart()
	l.settle("a last restart")
	l.unchanged("at the end")
}

// At three and four levels: the owner's /hold on the issue at the top,
// then one on a child held for discussion, which writes no marker there;
// then mallory edits or deletes the first /hold, or deletes one that
// wrote no marker on its own issue either, as that was held by hand; then
// a /resume on the top issue, or on one between. It ends the top /hold's
// holds, by its record, but not the child's /hold, posted below it, so
// the child and its own child stay held, whether the 👍s come before the
// /resume or after it, until the child's /resume (SPEC §6.4).
func TestResolveOwnHoldOutlivesTouchedAncestorHold(t *testing.T) {
	t.Parallel()
	const unended = "hasn't ended"
	var cases []holdBelowCase
	for _, touch := range []string{"edited", "deleted", "deleted, no marker"} {
		byHand := touch == "deleted, no marker"
		touched := strings.TrimSuffix(touch, ", no marker")
		// At four levels, the top /hold, held by hand, writes its marker on
		// the issue between alone, so once its comment is deleted it counts
		// as posted there, the farthest issue with its marker (SPEC §6.4):
		// the top's /resume, above it, doesn't end it there, and the
		// child's /resume, below it, ends it on the child and below.
		between := "ready"
		if byHand {
			between = "held"
		}
		cases = append(cases,
			// #1 > #2 > #3: the top, its child and grandchild, the child and
			// grandchild held for discussion.
			holdBelowCase{name: "three levels, " + touch, levels: 3, discussed: []int{1, 2}, byHand: byHand, holds: []int{0, 1}, touch: touched,
				resume: 0, want: []string{"ready", "held", "held"}, records: []int{1, 2}, unendedText: unended,
				release: 1, final: []string{"ready", "clarifying", "clarifying"}},
			// Four levels, the /resume on the top issue.
			holdBelowCase{name: "four levels, the /resume on the top, " + touch, levels: 4, discussed: []int{2, 3}, byHand: byHand, holds: []int{0, 2}, touch: touched,
				resume: 0, want: []string{"ready", between, "held", "held"}, records: []int{2, 3}, unendedText: unended,
				release: 2, final: []string{"ready", between, "clarifying", "clarifying"}},
			// Four levels, the /resume on the issue between the top and the
			// child: it ends the top /hold below it, not on the top.
			holdBelowCase{name: "four levels, the /resume between, " + touch, levels: 4, discussed: []int{2, 3}, byHand: byHand, holds: []int{0, 2}, touch: touched,
				resume: 1, want: []string{"held", "ready", "held", "held"}, records: []int{2, 3}, unendedText: unended,
				release: 2, final: []string{"held", "ready", "clarifying", "clarifying"}},
		)
	}
	for _, c := range cases {
		for _, thumbsFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, thumbs first %v", c.name, thumbsFirst), func(t *testing.T) { c.run(t, thumbsFirst) })
		}
	}
}

// At three and four levels: the owner's /hold on the top issue, then one
// on an issue held for discussion below, which writes no marker there;
// then a /resume on an issue between the two. It ends the top /hold's
// holds below it, but not the other /hold, posted below it, so that
// issue, and the one under it, stay held, whether the 👍s come before the
// /resume or after it, until /resume on that issue (SPEC §6.4).
func TestResolveResumeBetweenLeavesOwnHoldBelow(t *testing.T) {
	t.Parallel()
	const unended = "hasn't ended"
	for _, c := range []holdBelowCase{
		{name: "three levels", levels: 3, discussed: []int{2}, holds: []int{0, 2},
			resume: 1, want: []string{"held", "ready", "held"}, records: []int{2}, unendedText: unended,
			release: 2, final: []string{"held", "ready", "clarifying"}},
		{name: "four levels", levels: 4, discussed: []int{2, 3}, holds: []int{0, 2},
			resume: 1, want: []string{"held", "ready", "held", "held"}, records: []int{2, 3}, unendedText: unended,
			release: 2, final: []string{"held", "ready", "clarifying", "clarifying"}},
		{name: "four levels, the /hold edited", levels: 4, discussed: []int{2, 3}, holds: []int{0, 2}, touch: "edited",
			resume: 1, want: []string{"held", "ready", "held", "held"}, records: []int{2, 3}, unendedText: unended,
			release: 2, final: []string{"held", "ready", "clarifying", "clarifying"}},
		{name: "four levels, the /hold deleted", levels: 4, discussed: []int{2, 3}, holds: []int{0, 2}, touch: "deleted",
			resume: 1, want: []string{"held", "ready", "held", "held"}, records: []int{2, 3}, unendedText: unended,
			release: 2, final: []string{"held", "ready", "clarifying", "clarifying"}},
	} {
		for _, thumbsFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, thumbs first %v", c.name, thumbsFirst), func(t *testing.T) { c.run(t, thumbsFirst) })
		}
	}
}

// A /hold posted after a /resume, handled first: #1 > #2 > #3. The
// owner's /resume on #2, then a /hold on #1, posted after it, whose pass
// comes first, so it holds all three with markers. The /resume doesn't
// end that /hold on #3, posted after it (SPEC §6.4), so #3 stays held,
// as #1 does, and nor on #2 (#205), so #2 stays held too.
func TestResolveResumeEndsNoLaterHoldAbove(t *testing.T) {
	t.Parallel()
	f := discFake(t)  // #1
	file(f, 1, ready) // #2, under #1
	file(f, 2, ready) // #3, under #2
	f.AddComment(2, "owner", "/resume")
	f.AddComment(1, "owner", "/hold")
	resolveSettle(t, f, 1)
	resolveLabels(t, f, "after #1's /hold", "held", "held", "held")
	resolveSettle(t, f, 2)
	if got := f.State().Issues[2].Labels; len(got) != 1 || got[0] != "stage:held" {
		t.Errorf("#3: labels %q, newest comment %q; want it still held by #1's /hold", got, newestComment(f, 3))
	}
	if got := f.State().Issues[0].Labels; len(got) != 1 || got[0] != "stage:held" {
		t.Errorf("#1: labels %q; want it still held", got)
	}
	if got := f.State().Issues[1].Labels; len(got) != 1 || got[0] != "stage:held" {
		t.Errorf("#2: labels %q, newest comment %q; want it still held by #1's /hold", got, newestComment(f, 2))
	}
}

// A /hold on the path posted after a /resume, handled first: #3 > #2 >
// #1, #1 held for mallory's comment. The owner's /hold on #3 holds #3 and
// #2. Then the owner's /resume on #3, and
// a /hold on #2 posted after it, whose pass comes first: it writes no
// marker, #2 and #1 held already. The /resume doesn't end it, posted
// below it and after it, so #2 stays held by it, and #1 too once mallory's
// comment is resolved, until #2's /resume.
func TestResolveResumeEndsNoLaterHoldBelow(t *testing.T) {
	t.Parallel()
	f := discFake(t)  // #1
	file(f, 0, ready) // #2
	file(f, 0, ready) // #3
	f.SetParent(1, 2)
	f.SetParent(2, 3)
	w := f.AddComment(1, "mallory", "Wait.")
	resolveSettle(t, f, 1)
	f.AddComment(3, "owner", "/hold")
	resolveSettle(t, f, 3, 2, 1)
	f.AddComment(3, "owner", "/resume")
	f.AddComment(2, "owner", "/hold")
	resolveSettle(t, f, 2)
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after #3's /resume", "held", "held", "ready")
	f.AddReaction(w, "owner", "+1")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after the 👍", "held", "held", "ready")
	f.AddComment(2, "owner", "/resume")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after #2's /resume", "clarifying", "ready", "ready")
}

// The same, with #1 held only by #3's /hold: #3's /resume ends that on
// #1, but not #2's /hold, posted below it and after it, whose pass came
// first, so #1 stays held by it, as #2 does, until #2's /resume.
func TestResolveResumeEndsNoLaterHoldBelowNoDiscussion(t *testing.T) {
	t.Parallel()
	f := discFake(t)  // #1
	file(f, 0, ready) // #2
	file(f, 0, ready) // #3
	f.SetParent(1, 2)
	f.SetParent(2, 3)
	f.AddComment(3, "owner", "/hold")
	resolveSettle(t, f, 3, 2, 1)
	f.AddComment(3, "owner", "/resume")
	f.AddComment(2, "owner", "/hold")
	resolveSettle(t, f, 2)
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after #3's /resume", "held", "held", "ready")
	f.AddComment(2, "owner", "/resume")
	resolveSettle(t, f, 3, 2, 1)
	resolveLabels(t, f, "after #2's /resume", "ready", "ready", "ready")
}
