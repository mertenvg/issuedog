package daemon_test

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// Plan review across a hold (#271, SPEC §6.4): /approve, /revise and a 👍
// on a plan revision, given while the issue is held from plan-review,
// wait (state.Command.Waits), and go through their normal checks once the
// issue is back at plan-review, by /resume or by the interviewer's
// unchanged move. After a discussion hold, a /revise that waited is the
// interviewer's revision notes instead (#294), as after a hold from
// spec-review: its move names it, whatever the outcome.

// planHoldMallory is mallory's comment that holds the issue for
// discussion.
const planHoldMallory = "Should T1 come later?"

// planHoldAt files issue 1 on f, the owner's epic "Quotes" with body
// "body", typed by the owner's label, at plan-review: revision 1 of its
// spec in comment 1001, its approval in 1002, and plan revision 1 in 1003,
// posted with the planner's move. Then the owner's /hold, if owner, or
// else mallory's comment, is comment 1004, and a pass holds the issue from
// plan-review (resolvePass), in comment 1005.
func planHoldAt(t testing.TB, f *githubtest.Fake, owner bool) {
	t.Helper()
	n := f.AddIssue("owner", "Quotes", "body")
	f.AddLabel(n, "owner", "type:epic")
	f.AddLabel(n, app, "stage:planning")
	post(t, f, 1001, app, revision(1, "body"))
	post(t, f, 1002, app, approvedRev(1, "owner's 👍", "r9", "1001"))
	postPlan(t, f, n, 1, 1003)
	if owner {
		post(t, f, 1004, "owner", "/hold")
	} else {
		post(t, f, 1004, "mallory", planHoldMallory)
	}
	if err := resolvePass(t, n)(f); err != nil {
		t.Fatal(err)
	}
	cs := ivHoldComments(t, f)
	if hold := cs[len(cs)-1]; hold.ID != 1005 || !strings.Contains(hold.Body, "to=held at=plan-review") {
		t.Fatalf("newest comment %d %q; want the hold at plan-review in 1005", hold.ID, hold.Body)
	}
}

// planHeld is planHoldAt on a new fake, then do, with the writes
// forgotten.
func planHeld(owner bool, do func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		planHoldAt(t, f, owner)
		do(t, f)
		f.ForgetWrites()
		return f
	}
}

// planHoldResumed is the move out of a hold from plan-review back to it by
// the /resume in comment cmd.
func planHoldResumed(cmd int64) string {
	return moved("Resumed by `/resume`, back to `plan-review`.", fmt.Sprintf("from=held to=plan-review cmd=%d seen=1003", cmd))
}

// planHoldChanged edits plan revision 1's comment as the App, so it no
// longer matches its hash, but isn't dirty.
func planHoldChanged(f *githubtest.Fake) {
	f.EditComment(1003, app, strings.Replace(planComment(1, 1), "Do it.", "Do it now.", 1))
}

// planHoldReason is why an /approve of plan revision 1 is rejected once its
// comment no longer matches its hash.
var planHoldReason = "the comment of plan revision 1 was edited after I posted it, so it no longer matches that revision's hash, `" +
	planHash(1, 1) + "`. " + replanHint

// planHoldWritesNothing runs resolvePass on f, failing t unless it writes
// nothing and leaves the issue held.
func planHoldWritesNothing(t *testing.T, f *githubtest.Fake) {
	t.Helper()
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("a pass while held wrote %+v; want nothing", w)
	}
	if l := f.State().Issues[0].Labels; !slices.Contains(l, "stage:held") {
		t.Errorf("labels %q; want the issue still held", l)
	}
}

// While the issue is held from plan-review by /hold, an /approve, a
// /revise and a 👍 on the plan revision wait: a pass neither applies nor
// rejects them, and writes nothing, even once the plan comment no longer
// matches its hash. So does a 👍 while it's held for discussion: a
// trusted comment there counters mallory's, which ends the hold, so the
// commands wait at the clarifying after it (TestPlanHoldWaitsAtClarifying).
func TestPlanHoldWaits(t *testing.T) {
	t.Parallel()
	thumb := func(t testing.TB, f *githubtest.Fake) { f.AddReaction(1003, "owner", "+1") }
	for _, tt := range []struct {
		name  string
		owner bool
		do    func(t testing.TB, f *githubtest.Fake)
	}{
		{"/approve", true, func(t testing.TB, f *githubtest.Fake) { f.AddComment(1, "owner", "/approve") }},
		{"/approve on a changed plan", true, func(t testing.TB, f *githubtest.Fake) {
			planHoldChanged(f)
			f.AddComment(1, "owner", "/approve")
		}},
		{"/revise", true, func(t testing.TB, f *githubtest.Fake) { f.AddComment(1, "owner", "/revise split T1") }},
		{"a 👍", true, thumb},
		{"a 👍 during a discussion hold", false, thumb},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := planHeld(tt.owner, tt.do)(t)
			planHoldWritesNothing(t, f)
			for _, c := range f.State().Issues[0].Comments[5:] {
				if len(c.Reactions) != 0 {
					t.Errorf("reactions on %q: %q; want none while it waits", c.Body, c.Reactions)
				}
			}
		})
	}
}

// An /approve posted while held from plan-review waits, and once /resume
// takes the issue back to plan-review, in the same pass, it's applied
// while plan revision 1 is still the latest and matches its hash, and
// rejected with the reason once it doesn't. Under /hold, the /approve
// comes first, to show it waits; under a discussion hold, the /resume
// does, as an /approve before it would counter mallory's comment
// (TestResolveCounterThenResume).
func TestPlanHoldApproveAfterResume(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		owner   bool
		changed bool
	}{
		{"/hold, the hash matches", true, false},
		{"/hold, the plan changed", true, true},
		{"a discussion hold, the hash matches", false, false},
		{"a discussion hold, the plan changed", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var approve, resume int64
			setup := planHeld(tt.owner, func(t testing.TB, f *githubtest.Fake) {
				if tt.changed {
					planHoldChanged(f)
				}
				if tt.owner {
					approve = f.AddComment(1, "owner", "/approve")
					resume = f.AddComment(1, "owner", "/resume")
				} else {
					resume = f.AddComment(1, "owner", "/resume")
					approve = f.AddComment(1, "owner", "/approve")
				}
			})
			resolveCheck(t, setup, 1, func(*githubtest.Fake) issueWant {
				a := strconv.FormatInt(approve, 10)
				if tt.changed {
					return issueWant{[]string{"stage:plan-review", "type:epic"}, []string{planHoldResumed(resume),
						final("/approve", "I didn't apply this command", planHoldReason, a)}}
				}
				return issueWant{[]string{"stage:planned", "type:epic"}, []string{planHoldResumed(resume),
					planApproved(1, "owner's `/approve`", a, a)}}
			})
		})
	}
}

// A trusted 👍 on plan revision 1, given while held from plan-review,
// approves it once /resume takes the issue back there, in the same pass,
// while it still matches its hash. Once it doesn't, it approves nothing,
// and the issue stays at plan-review.
func TestPlanHoldThumbAfterResume(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		owner   bool
		changed bool
	}{
		{"/hold, the hash matches", true, false},
		{"/hold, the plan changed", true, true},
		{"a discussion hold, the hash matches", false, false},
		{"a discussion hold, the plan changed", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var thumb, resume int64
			setup := planHeld(tt.owner, func(t testing.TB, f *githubtest.Fake) {
				if tt.changed {
					planHoldChanged(f)
				}
				thumb = f.AddReaction(1003, "owner", "+1")
				resume = f.AddComment(1, "owner", "/resume")
			})
			resolveCheck(t, setup, 1, func(*githubtest.Fake) issueWant {
				if tt.changed {
					return issueWant{[]string{"stage:plan-review", "type:epic"}, []string{planHoldResumed(resume)}}
				}
				return issueWant{[]string{"stage:planned", "type:epic"}, []string{planHoldResumed(resume),
					planApproved(1, "owner's 👍", fmt.Sprintf("r%d", thumb), strconv.FormatInt(resume, 10))}}
			})
		})
	}
}

// A /revise posted while held from plan-review waits, and once /resume
// takes the issue back there, in the same pass, it sends the plan back to
// planning, with its notes (TestRevisePlan).
func TestPlanHoldReviseAfterResume(t *testing.T) {
	t.Parallel()
	for name, owner := range map[string]bool{"/hold": true, "a discussion hold": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var revise, resume int64
			setup := planHeld(owner, func(t testing.TB, f *githubtest.Fake) {
				if owner {
					revise = f.AddComment(1, "owner", "/revise split T1")
					resume = f.AddComment(1, "owner", "/resume")
				} else {
					resume = f.AddComment(1, "owner", "/resume")
					revise = f.AddComment(1, "owner", "/revise split T1")
				}
			})
			resolveCheck(t, setup, 1, func(*githubtest.Fake) issueWant {
				return issueWant{[]string{"stage:planning", "type:epic"}, []string{planHoldResumed(resume), planSentBack(strconv.FormatInt(revise, 10))}}
			})
			// The /revise is the notes: the first of planning's Inputs.
			var in []string
			if err := visit(t, owners, &in)(run(t, setup, resolvePass(t, 1))); err != nil {
				t.Fatal(err)
			}
			if len(in) == 0 || in[0] != "/revise split T1" {
				t.Errorf("planning's inputs %q; want the /revise first", in)
			}
		})
	}
}

// planHoldDisc files issue 1 on f held for discussion from plan-review
// (planHoldAt), and resolves the discussion by the owner's 👍 on
// mallory's comment, then runs more, with the writes forgotten.
func planHoldDisc(t testing.TB, f *githubtest.Fake, more func(f *githubtest.Fake)) {
	t.Helper()
	planHoldAt(t, f, false)
	f.AddReaction(1004, "owner", "+1")
	if more != nil {
		more(f)
	}
	f.ForgetWrites()
}

// planHoldUnchanged is the start of the interviewer's unchanged move back
// to plan-review, with standing, the line on the plan revision that still
// stands, if any, and the cc line.
func planHoldUnchanged(standing string) string {
	text := "Nothing in the discussion during the hold changes this issue, so it goes back to `plan-review`.\n\n"
	if standing != "" {
		text += standing + "\n\n"
	}
	return text + "cc @owner\n\n<!-- issuedog:transition "
}

// planHoldStands is the line of the unchanged move on plan revision 1,
// which still stands.
var planHoldStands = "Revision 1 of the plan, `" + planHash(1, 1) + "`, still stands: to approve it, react 👍 to it, or post `/approve`."

// planHoldNew is a rig on f with ivHoldRig, whose fake claude returns
// unchanged.
func planHoldNew(t *testing.T, f *githubtest.Fake) *rig {
	t.Helper()
	return ivHoldNew(t, f, map[string]any{"outcome": "unchanged"})
}

// After a discussion hold from plan-review where nothing changed, the
// interviewer's unchanged move takes the issue back to plan-review, in the
// same pass the discussion ends, with origin and no revision. Its comment
// names plan revision 1, which still stands, and ends in the cc line. The
// pass after makes no write and no run, and a 👍 on plan revision 1 then
// approves it. The unchanged pass is safe to replay.
func TestPlanHoldUnchanged(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	planHoldDisc(t, f, nil)
	g := planHoldNew(t, f)
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	body := newestComment(f, 1)
	if want := planHoldUnchanged(planHoldStands); !strings.HasPrefix(body, want) {
		t.Errorf("the unchanged move =\n%s\nwant it to start\n%s", body, want)
	}
	ivHoldFields(t, ivHoldMove(t, body), map[string]string{"from": "clarifying", "to": "plan-review", "run": "run1", "origin": "plan-review",
		"rev": "", "body": "", "cmd": ""})
	if l := ivLabels(f, 1); !slices.Equal(l, []string{"stage:plan-review", "type:epic"}) || g.runs() != 1 {
		t.Errorf("labels %q after %d runs; want plan-review after one", l, g.runs())
	}
	writes := len(f.Writes())
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	if w := f.Writes(); len(w) != writes || g.runs() != 1 {
		t.Errorf("a second pass wrote %+v with %d runs; want nothing, after one run", w[writes:], g.runs())
	}
	thumb := f.AddReaction(1003, "owner", "+1")
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	if l, c := ivLabels(f, 1), newestComment(f, 1); !slices.Equal(l, []string{"stage:planned", "type:epic"}) ||
		!strings.Contains(c, fmt.Sprintf("from=plan-review to=planned rev=1 body=%s cmd=r%d", planHash(1, 1), thumb)) {
		t.Errorf("labels %q, newest comment %q; want plan revision 1 approved by the 👍", l, c)
	}
	replayRuns(t, func(tb testing.TB, f *githubtest.Fake, _ string) { planHoldDisc(tb, f, nil) }, func(g *rig) {
		ivHoldRig(g)
		g.claude(ivResult(t, map[string]any{"outcome": "unchanged"}), 0)
	})
}

// Once plan revision 1 no longer matches its hash, nothing approves it, so
// the unchanged move names no revision that stands, and still ends in the
// cc line.
func TestPlanHoldUnchangedNoneStands(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	planHoldDisc(t, f, planHoldChanged)
	g := planHoldNew(t, f)
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	body := newestComment(f, 1)
	if want := planHoldUnchanged(""); !strings.HasPrefix(body, want) || strings.Contains(body, "still stands") {
		t.Errorf("the unchanged move =\n%s\nwant it to start\n%s", body, want)
	}
	if l := ivLabels(f, 1); !slices.Equal(l, []string{"stage:plan-review", "type:epic"}) {
		t.Errorf("labels %q; want plan-review", l)
	}
}

// planHoldReacted fails t if issue 1's comment with body has any reaction:
// a waiting command has neither the App's 👍 nor its 👎.
func planHoldReacted(t testing.TB, f *githubtest.Fake, body string) {
	t.Helper()
	found := false
	for _, c := range f.State().Issues[0].Comments {
		if c.Body == body {
			found = true
			if len(c.Reactions) != 0 {
				t.Errorf("reactions on %q: %q; want none while it waits", body, c.Reactions)
			}
		}
	}
	if !found {
		t.Errorf("no comment on #1 is %q", body)
	}
}

// planHoldCommands are the commands posted while held for discussion from
// plan-review, by the owner: each counters mallory's comment, so the
// issue leaves its hold for clarifying in the next pass.
var planHoldCommands = []struct {
	name, body string
	changed    bool // the plan comment no longer matches its hash
}{
	{"/approve", "/approve", false},
	{"/approve on a changed plan", "/approve", true},
	{"/revise", "/revise split T1", false},
}

// At the clarifying after a discussion hold from plan-review, an /approve
// or a /revise posted during the hold still waits: the pass that ends the
// hold neither applies nor rejects it, and gives it no reaction, and the
// pass after writes nothing (resolveCheck).
func TestPlanHoldWaitsAtClarifying(t *testing.T) {
	t.Parallel()
	for _, tt := range planHoldCommands {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var cmd int64
			setup := planHeld(false, func(t testing.TB, f *githubtest.Fake) {
				if tt.changed {
					planHoldChanged(f)
				}
				cmd = f.AddComment(1, "owner", tt.body)
			})
			resolveCheck(t, setup, 1, func(f *githubtest.Fake) issueWant {
				return issueWant{[]string{"stage:clarifying", "type:epic"}, []string{ivHoldLeaveText("`owner`", "plan-review",
					fmt.Sprintf("from=held to=clarifying at=plan-review origin=plan-review event=h1005 resolved=%d seen=1003", resolveCommentAt(t, f, 1, cmd)))}}
			})
			planHoldReacted(t, run(t, setup, resolvePass(t, 1)), tt.body)
		})
	}
}

// planHoldBack is a setup of issue 1 held for discussion from plan-review
// (planHoldDisc), with more during the hold, then a pass of the rig on t,
// whose interviewer returns unchanged, back to plan-review, with the
// writes forgotten. It fails tb unless more's command, if body is set,
// still waits, with no reaction, and the interviewer's move names none.
func planHoldBack(t *testing.T, body string, more func(f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		planHoldDisc(tb, f, more)
		if err := planHoldNew(t, f).work(1); err != nil {
			tb.Fatal(err)
		}
		move := newestComment(f, 1)
		if !strings.HasPrefix(move, "Nothing in the discussion during the hold changes this issue") {
			tb.Fatalf("newest comment %q; want the interviewer's unchanged move", move)
		}
		ivHoldFields(tb, ivHoldMove(tb, move), map[string]string{"to": "plan-review", "origin": "plan-review", "cmd": ""})
		if body != "" {
			planHoldReacted(tb, f, body)
		}
		f.ForgetWrites()
		return f
	}
}

// After the interviewer's unchanged move back to plan-review, the next
// pass applies what waited through the hold, with its normal checks
// (resolveCheck: under Replay, and a second pass writes nothing):
//   - an /approve approves plan revision 1 while it matches its hash, and
//     is rejected with the reason once it doesn't;
//   - a trusted 👍 on plan revision 1 approves it while it matches its
//     hash, and approves nothing once it doesn't.
//
// A /revise isn't among them: the interviewer's move names it, so it never
// applies back at plan-review (TestPlanHoldReviseUnchanged, #294).
func TestPlanHoldAfterUnchanged(t *testing.T) {
	t.Parallel()
	atReview, planned := []string{"stage:plan-review", "type:epic"}, []string{"stage:planned", "type:epic"}
	for _, tt := range []struct {
		name    string
		body    string // the command posted during the hold, if any
		changed bool
		want    func(cmd, thumb int64) issueWant
	}{
		{"/approve", "/approve", false, func(cmd, _ int64) issueWant {
			c := strconv.FormatInt(cmd, 10)
			return issueWant{planned, []string{planApproved(1, "owner's `/approve`", c, c)}}
		}},
		{"/approve on a changed plan", "/approve", true, func(cmd, _ int64) issueWant {
			return issueWant{atReview, []string{final("/approve", "I didn't apply this command", planHoldReason, strconv.FormatInt(cmd, 10))}}
		}},
		// Its seen is the newest comment read that isn't the App's,
		// mallory's.
		{"a 👍", "", false, func(_, thumb int64) issueWant {
			return issueWant{planned, []string{planApproved(1, "owner's 👍", fmt.Sprintf("r%d", thumb), "1004")}}
		}},
		{"a 👍 on a changed plan", "", true, func(int64, int64) issueWant { return issueWant{labels: atReview} }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var cmd, thumb int64
			setup := planHoldBack(t, tt.body, func(f *githubtest.Fake) {
				if tt.changed {
					planHoldChanged(f)
				}
				if tt.body != "" {
					cmd = f.AddComment(1, "owner", tt.body)
				} else {
					thumb = f.AddReaction(1003, "owner", "+1")
				}
			})
			resolveCheck(t, setup, 1, func(*githubtest.Fake) issueWant { return tt.want(cmd, thumb) })
		})
	}
}

// planHoldRevise is the owner's /revise posted during the discussion hold
// from plan-review.
const planHoldRevise = "/revise split T1"

// planHoldRevised is planHoldDisc with the owner's /revise during the
// hold.
func planHoldRevised(t testing.TB, f *githubtest.Fake) {
	t.Helper()
	planHoldDisc(t, f, func(f *githubtest.Fake) { f.AddComment(1, "owner", planHoldRevise) })
}

// planHoldAcked fails t unless the /revise has the App's 👍 alone.
func planHoldAcked(t testing.TB, f *githubtest.Fake) {
	t.Helper()
	for _, c := range f.State().Issues[0].Comments {
		if c.Body == planHoldRevise && !slices.Equal(c.Reactions, []string{app + " +1"}) {
			t.Errorf("reactions on the /revise %q; want the App's 👍", c.Reactions)
		}
	}
}

// #294, replacing #271's interim rule: the interviewer at the clarifying
// after a hold from plan-review reads a /revise that waited through the
// hold as its revision notes, as after a hold from spec-review
// (TestIvHoldReviseContext).
func TestPlanHoldReviseNotes(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	planHoldRevised(t, f)
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l := ivLabels(f, 1); !slices.Equal(l, []string{"stage:clarifying", "type:epic"}) {
		t.Fatalf("labels %q; want clarifying after the hold", l)
	}
	id := ivHoldFind(t, f, planHoldRevise)
	files := ivContext(t, ivDaemon(t, f, newOrigin(t)), 1)
	if w := fmt.Sprintf("## Comment %d by owner: a `/revise` that waited through the hold, whose text is the revision notes\n\n```text\n%s\n```", id, planHoldRevise); !strings.Contains(files["input.md"], w) {
		t.Errorf("input.md lacks\n%s\nin\n%s", w, files["input.md"])
	}
}

// After unchanged, the /revise that waited through the hold from
// plan-review is used up (#294): the move names it with cmd= beside run=,
// and 👍s it, so it isn't applied again at plan-review, and plan revision
// 1, which still stands, approves by a 👍 while its hash matches. The
// unchanged pass is safe to replay.
func TestPlanHoldReviseUnchanged(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	planHoldRevised(t, f)
	g := ivHoldNew(t, f, map[string]any{"outcome": "unchanged", "message": "T1 is as small as it gets."})
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	id := ivHoldFind(t, f, planHoldRevise)
	body := newestComment(f, 1)
	if want := "Nothing in the discussion during the hold changes this issue, so it goes back to `plan-review`.\n\n" + planHoldStands +
		"\n\nT1 is as small as it gets.\n\ncc @owner\n\n<!-- issuedog:transition "; !strings.HasPrefix(body, want) {
		t.Errorf("the unchanged move =\n%s\nwant it to start\n%s", body, want)
	}
	ivHoldFields(t, ivHoldMove(t, body), map[string]string{"from": "clarifying", "to": "plan-review", "run": "run1", "origin": "plan-review",
		"cmd": strconv.FormatInt(id, 10)})
	planHoldAcked(t, f)
	writes := len(f.Writes())
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	if w := f.Writes(); len(w) != writes || g.runs() != 1 {
		t.Errorf("a pass at plan-review wrote %+v with %d runs; want nothing: the /revise isn't applied again", w[writes:], g.runs())
	}
	if l := ivLabels(f, 1); !slices.Equal(l, []string{"stage:plan-review", "type:epic"}) {
		t.Errorf("labels %q; want plan-review", l)
	}
	thumb := f.AddReaction(1003, "owner", "+1")
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	if l, c := ivLabels(f, 1), newestComment(f, 1); !slices.Equal(l, []string{"stage:planned", "type:epic"}) ||
		!strings.Contains(c, fmt.Sprintf("from=plan-review to=planned rev=1 body=%s cmd=r%d", planHash(1, 1), thumb)) {
		t.Errorf("labels %q, newest comment %q; want plan revision 1 approved by the 👍", l, c)
	}
	replayRuns(t, func(tb testing.TB, f *githubtest.Fake, _ string) { planHoldRevised(tb, f) }, func(g *rig) {
		ivHoldRig(g)
		g.claude(ivResult(t, map[string]any{"outcome": "unchanged"}), 0)
	})
}

// A /revise that waited through a hold from plan-review was meant for the
// plan, but when the interviewer rewrites the spec instead, its spec move
// names the /revise and 👍s it, so it's used up there, answered in the
// spec's message, and never lands at spec-review as a revision request
// for the new spec (#294).
func TestPlanHoldReviseSpec(t *testing.T) {
	t.Parallel()
	out := ivSpec()
	out["message"] = "The split you asked for needs a spec change first: quotes expire now."
	f := newFake(t)
	planHoldRevised(t, f)
	g := ivHoldNew(t, f, out)
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	id := ivHoldFind(t, f, planHoldRevise)
	ivHoldFields(t, ivHoldMove(t, newestComment(f, 1)), map[string]string{"from": "clarifying", "to": "spec-review", "run": "run1",
		"origin": "plan-review", "cmd": strconv.FormatInt(id, 10)})
	planHoldAcked(t, f)
	writes := len(f.Writes())
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	if w := f.Writes(); len(w) != writes || g.runs() != 1 {
		t.Errorf("a pass at spec-review wrote %+v with %d runs; want nothing: the /revise doesn't send the new spec back", w[writes:], g.runs())
	}
	if l := ivLabels(f, 1); !slices.Equal(l, []string{"stage:spec-review", "type:epic"}) {
		t.Errorf("labels %q; want spec-review", l)
	}
	replayRuns(t, func(tb testing.TB, f *githubtest.Fake, _ string) { planHoldRevised(tb, f) }, func(g *rig) {
		ivHoldRig(g)
		g.claude(ivResult(t, out), 0)
	})
}
