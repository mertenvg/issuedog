package daemon_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// #188's decision, round 3: every owner hold ends by /resume on the held
// issue, on the issue where its /hold was posted, or on any issue between
// the two, and by no other, a child's own /hold that wrote no marker as
// the child was held already (Q2b) included (SPEC §6.4). The /resume path
// and the leave path judge by the same rule (resolveEndsHold), and count a
// /hold by the App's record that it applied it, whatever a write
// collaborator made of its comment since, so they agree in either order,
// across a restart, as round 2's attempts 4 and 5 found they didn't.

// unchanged checks that one more pass of l writes nothing and costs no
// rate limit: every request is answered 304 (#87's plan).
func (l *resolveLoop) unchanged(when string) {
	l.t.Helper()
	w, r := len(l.f.Writes()), len(l.f.Requests())
	iterate(l.t, l.d)
	if x := l.f.Writes()[w:]; len(x) != 0 {
		l.t.Errorf("%s: an unchanged pass wrote %+v", when, x)
	}
	for _, q := range l.f.Requests()[r:] {
		if q.Status != http.StatusNotModified {
			l.t.Errorf("%s: an unchanged pass sent %+v", when, q)
		}
	}
}

// delete has mallory delete comment id on issue n, then eve, who can only
// read, comment there, which holds nothing but makes the poll list the
// issue again, as the deletion alone doesn't in the fake.
func (l *resolveLoop) delete(n int, id int64) {
	l.f.DeleteCommentBy("mallory", id)
	l.f.SetPermission("eve", "read")
	l.f.AddComment(n, "eve", "Bump.")
}

// stages checks each issue's stage, want[i] for issues[i].
func (l *resolveLoop) stages(when string, issues []int, want ...string) {
	l.t.Helper()
	for i, stage := range want {
		if got := l.f.State().Issues[issues[i]-1].Labels; !slices.Equal(got, []string{"stage:" + stage}) {
			l.t.Errorf("%s: #%d: labels %q, newest comment %q; want %s", when, issues[i], got, newestComment(l.f, issues[i]), stage)
		}
	}
}

// round3Case is a family of tasks, levels deep, issues[0] at the top, each
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
type round3Case struct {
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
func (c round3Case) run(t *testing.T, thumbsFirst bool) {
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
		if body := newestComment(l.f, issues[i]); !strings.Contains(body, c.unendedText) {
			t.Errorf("#%d: newest comment %q; want the record that an owner hold on it hasn't ended", issues[i], body)
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

// Round 2's attempt 5, at three and four levels: the owner's /hold on the
// issue at the top, then one on a child held for discussion, which writes
// no marker there; then mallory edits or deletes the first /hold, or
// deletes one that wrote no marker on its own issue either, as that was
// held by hand; then a /resume on the top issue, or on one between. It
// ends the top /hold's holds, by its record, but not the child's /hold,
// posted below it, so the child and its own child stay held, whether the
// 👍s come before the /resume or after it, until the child's /resume.
func TestResolveRound3OwnHoldUnderEditedHold(t *testing.T) {
	const unended = "hasn't ended"
	var cases []round3Case
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
			// #3 > #2 > #1 in the verifier's words: the top, its child and
			// grandchild, the child and grandchild held for discussion.
			round3Case{name: "three levels, " + touch, levels: 3, discussed: []int{1, 2}, byHand: byHand, holds: []int{0, 1}, touch: touched,
				resume: 0, want: []string{"ready", "held", "held"}, records: []int{1, 2}, unendedText: unended,
				release: 1, final: []string{"ready", "clarifying", "clarifying"}},
			// Four levels, the /resume on the top issue.
			round3Case{name: "four levels, the /resume on the top, " + touch, levels: 4, discussed: []int{2, 3}, byHand: byHand, holds: []int{0, 2}, touch: touched,
				resume: 0, want: []string{"ready", between, "held", "held"}, records: []int{2, 3}, unendedText: unended,
				release: 2, final: []string{"ready", between, "clarifying", "clarifying"}},
			// Four levels, the /resume on the issue between the top and the
			// child: it ends the top /hold below it, not on the top.
			round3Case{name: "four levels, the /resume between, " + touch, levels: 4, discussed: []int{2, 3}, byHand: byHand, holds: []int{0, 2}, touch: touched,
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

// Round 2's attempt 4, at three and four levels: the owner's /hold on the
// top issue, then one on an issue held for discussion below, which writes
// no marker there; then a /resume on an issue between the two. It ends
// the top /hold's holds below it, but not the other /hold, posted below
// it, so that issue, and the one under it, stay held, whether the 👍s come
// before the /resume or after it, until /resume on that issue.
func TestResolveRound3OwnHoldBelowResume(t *testing.T) {
	const unended = "hasn't ended"
	for _, c := range []round3Case{
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

// The verifier's M26b gap (round 2): a comment that mallory deletes on an
// ancestor during a hold may have been a /hold that wrote no marker, so it
// counts as one posted on that ancestor, and a /resume posted above it
// doesn't end it, as for any /hold: the issue below stays held, in either
// order, until a /resume on that ancestor, or below it, does.
func TestResolveRound3GoneNotEndedFromAbove(t *testing.T) {
	for _, thumbsFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("thumbs first %v", thumbsFirst), func(t *testing.T) {
			l := newResolveLoop(t, newFake(t))
			top := l.task(0)
			mid := l.task(top)
			low := l.task(mid)
			issues := []int{top, mid, low}
			l.settle("start")
			w := l.f.AddComment(low, "mallory", "Wait.")
			l.settle("mallory's comment")
			l.f.AddComment(top, "owner", "/hold")
			l.settle("the /hold on the top")
			// The owner's comment on the middle issue, which mallory deletes
			// during the hold: GitHub doesn't say what it was.
			gone := l.f.AddComment(mid, "owner", "Looking into it.")
			l.settle("the owner's comment")
			l.delete(mid, gone)
			l.settle("mallory's deletion")
			thumbs := func() {
				l.f.AddReaction(w, "owner", "+1")
				l.settle("the 👍")
			}
			resume := func() {
				l.f.AddComment(top, "owner", "/resume")
				l.settle("the /resume on the top")
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
			l.stages("after both", issues, "ready", "held", "held")
			if body := newestComment(l.f, low); !strings.Contains(body, fmt.Sprintf("a write collaborator deleted a comment on #%d during this hold", mid)) {
				t.Errorf("#%d: newest comment %q; want the record that the comment deleted on #%d keeps it held", low, body, mid)
			}
			l.restart()
			l.settle("a restart")
			l.stages("after a restart", issues, "ready", "held", "held")
			l.unchanged("after a restart")
			l.f.AddComment(mid, "owner", "/resume")
			l.settle("the /resume on the middle")
			l.stages("after the /resume on the middle", issues, "ready", "ready", "clarifying")
			l.unchanged("at the end")
		})
	}
}
