package daemon_test

import (
	"context"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// The rest of creating a plan's children (#462, from #334's verify): a
// revision's changed item that can't be written as an issue, a child
// GitHub refuses to file, and a revision's plan comment that names no run
// are each a failure no later pass gets past; a held child's first move
// keeps its hold's seen, and stands for its first move once the plan is
// approved again; and a stage a trusted user set by hand on a held child,
// back to intake, is an override, so the child gets no first move.

// creationRestStuck fails t unless issue 1 of f has one state error, for
// plan revision rev's children, saying why, keyed by p, the comment
// holding its move into planned, and a visit after it makes no request.
func creationRestStuck(t *testing.T, f *githubtest.Fake, k childPlan, rev int, p int64, why string) {
	t.Helper()
	want := "I can't create the children of plan rev " + strconv.Itoa(rev) + ", as " + why +
		". I won't try again until this issue moves, as by `/hold` and `/resume`. The daemon's log has the error.\n\n" + childrenCrashError(p)
	if got := childrenCrashErrors(f.State().Issues[0]); !reflect.DeepEqual(got, []string{want}) {
		t.Errorf("issue 1's state errors are\n%q\nwant one\n%q", got, want)
	}
	if got := records(f.State().Issues[0]); len(got) != 0 {
		t.Errorf("issue 1's records are %q, want none", got)
	}
	visit := childrenStep(t, f, k)
	s, err := visit.Client.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	from := len(f.Requests())
	if err := visit.ChildrenVisit(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if r := f.Requests()[from:]; len(r) != 0 {
		t.Errorf("a visit once the state error stands sent %+v; want nothing", r)
	}
}

// A revision whose changed item can't be written as an issue, here T2
// with no scope, stops with nothing written, as a first plan's item does
// (childSpecsWhere): a state error on the story says so, the visit returns
// no error, and a visit after it makes no request. No child is filed for
// the new T4, and no child gets a write. A crash at the error's post
// replays.
func TestChildrenChangedItemUnwritable(t *testing.T) {
	t.Parallel()
	setup := func(tb testing.TB) *githubtest.Fake {
		out := plGolden(tb, "story-revision")
		delete(plItem(tb, out, 1), "scope")
		return revApprovedWith(tb, changedPlanData(tb, out))
	}
	pass := childrenPass(t, revisionPlan)
	githubtest.Replay(t, setup, pass)

	f := run(t, setup, pass)
	if got := len(f.State().Issues); got != 5 {
		t.Errorf("%d issues, want the story, its three children and the bug: none filed", got)
	}
	for _, n := range []int{2, 3, 4, 5} {
		droppedNoWrite(t, f, n, "a child or the bug, while the revision is stopped")
	}
	creationRestStuck(t, f, revisionPlan, 2, goneMoveID(t, f), "an item of it can't be written as an issue")
}

// A child GitHub refuses to file with a 422, here as the org renamed the
// Task type its items name, is a failure no later pass gets past, as a
// refused link is: a state error on the parent says so, in the daemon's
// words, quoting none of GitHub's, the visit returns no error, and a visit
// after it makes no request. A crash at the error's post replays.
func TestChildrenCreateRefused(t *testing.T) {
	t.Parallel()
	setup := planAtReview(threeTasks, func(tb testing.TB, f *githubtest.Fake) {
		thumbsUp(tb, f)
		f.RenameOrgIssueType("Task", "Chore")
	})
	pass := childrenPass(t, threeTasks)
	githubtest.Replay(t, setup, pass)

	f := run(t, setup, pass)
	if st := f.State(); len(st.Issues) != 1 || len(st.Issues[0].SubIssues) != 0 {
		t.Errorf("%+v; want the parent alone, with no child filed", st.Issues)
	}
	if !slices.Contains(f.State().Issues[0].Labels, "stage:planned") {
		t.Errorf("issue 1 is labelled %v, want stage:planned still", f.State().Issues[0].Labels)
	}
	p, _ := planMoveID(t, f)
	creationRestStuck(t, f, threeTasks, 1, p, "GitHub refused to file the child of item T1")
}

// A child that GitHub fails to file with a 5xx isn't a failure no later
// pass gets past: the visit returns it, so the parent is read again next
// pass, and no state error is posted.
func TestChildrenCreateServerErrorRetried(t *testing.T) {
	t.Parallel()
	setup := planAtReview(threeTasks, thumbsUp)
	pass := childrenPass(t, threeTasks)
	f := setup(t)
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == "POST" && r.Path == "/issues" }, 502)
	if err := pass(f); err == nil {
		t.Fatal("the pass whose create got a 502 returned no error; want it returned, so the parent is read again")
	}
	if got := childrenCrashErrors(f.State().Issues[0]); len(got) != 0 {
		t.Errorf("issue 1's state errors are %q; want none for a 502", got)
	}
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	checkChildren(t, f, threeTasks)
}

// A revision whose plan comment names no run, which the rewrite marker of
// its changed children would name, is a failure no later pass gets past,
// as the comment is the approved revision's: a state error on the story
// says so, the visit returns no error, and a visit after it makes no
// request. The changed T2's child gets no write. Nor does any other: the
// missing run is found before any write, as a gone child is, so no child
// is filed for the new T4 (#544). A crash at any write replays.
func TestChildrenChangedNoRun(t *testing.T) {
	t.Parallel()
	setup := func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		r := revStory(tb, f, revByOwner)
		c := revMove(tb, f, r.n, daemon.Move{To: state.PlanReview, Trigger: state.TriggerPlanValid, Key: state.Key{Field: "event", ID: "77"},
			PlanRev: 2, Text: planText(r.n, 2), Markers: []github.Marker{planData(tb, revisionPlan)}})
		f.AddReaction(c, "owner", "+1")
		f.ForgetWrites()
		return f
	}
	pass := childrenPass(t, revisionPlan)
	githubtest.Replay(t, setup, pass)

	f := run(t, setup, pass)
	droppedNoWrite(t, f, 3, "T2's child, whose rewrite would name the plan comment's run")
	if got := len(f.State().Issues); got != 5 {
		t.Errorf("%d issues, want the story, its three children and the bug: none filed for T4", got)
	}
	for _, n := range []int{2, 4, 5} {
		droppedNoWrite(t, f, n, "a child or the bug, while the revision is stopped")
	}
	creationRestStuck(t, f, revisionPlan, 2, goneMoveID(t, f),
		"its plan comment names no run, which the rewrite of its changed children would name")
}

// creationRestHeld is the setup of a pass that finds T1 linked but not yet
// moved, as a crash at its first move left it, and held by the owner's
// /hold on it, which it gets in that pass.
func creationRestHeld(t *testing.T) func(testing.TB) *githubtest.Fake {
	first := planAtReview(threeTasks, thumbsUp)
	pass := childrenPass(t, threeTasks)
	move := writeAt(t, first, pass, 1, isLink) + 1 // T1's first move
	return func(tb testing.TB) *githubtest.Fake {
		f := childrenCrashStopped(tb, first, pass, move)
		f.AddComment(2, "owner", "/hold")
		f.ForgetWrites()
		return f
	}
}

// A held child's first move carries its hold's seen, the round bound from
// before the hold (SPEC §5, §7.2), as a changed held child's does. Here
// T1, linked but not yet moved, was held and resumed back to intake, which
// sets its bound to the /resume's comment, then held again, so the hold
// that stands carries that seen, and so does the held → held first move.
// A crash at any write replays.
func TestChildrenHeldFirstMoveSeen(t *testing.T) {
	t.Parallel()
	first := planAtReview(threeTasks, thumbsUp)
	pass := childrenPass(t, threeTasks)
	move := writeAt(t, first, pass, 1, isLink) + 1 // T1's first move
	// child runs step 1 on T1 alone.
	child := func(f *githubtest.Fake) error {
		st := childrenStep(t, f, threeTasks)
		s, err := st.Client.Snapshot(context.Background(), "o", "r", 2)
		if err != nil {
			return err
		}
		_, err = st.Run(context.Background(), s)
		return err
	}
	setup := func(tb testing.TB) *githubtest.Fake {
		f := childrenCrashStopped(tb, first, pass, move)
		for _, cmd := range []string{"/hold", "/resume"} {
			f.AddComment(2, "owner", cmd)
			if err := child(f); err != nil {
				tb.Fatal(err)
			}
		}
		f.AddComment(2, "owner", "/hold")
		f.ForgetWrites()
		return f
	}
	both := childrenCrashOn(t, threeTasks, 1, 2)
	githubtest.Replay(t, setup, both)

	f := run(t, setup, both)
	p, _ := planMoveID(t, f)
	t1 := f.State().Issues[1]
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", 2)
	if err != nil {
		t.Fatal(err)
	}
	var resumed int64 // the App's comment that applied the /resume on T1
	for _, c := range s.Comments {
		if strings.Contains(c.Body, "from=held to=intake cmd=") {
			resumed = c.ID
		}
	}
	if resumed == 0 {
		t.Fatalf("T1's comments are %+v; want the /resume's move back to intake", s.Comments)
	}
	moves := childrenCrashMoves(t1)
	if len(moves) != 1 {
		t.Fatalf("T1's first moves are %q, want one", moves)
	}
	want := "Created from #1's plan, rev 1, as T1. It stays on hold, and `/resume` takes it to `stage:ready`.\n\n" +
		"<!-- issuedog:transition from=held to=held at=ready origin=ready event=p" + strconv.FormatInt(p, 10) +
		" seen=" + strconv.FormatInt(resumed, 10) + " -->"
	if moves[0] != want {
		t.Errorf("T1's first move is\n%s\nwant a held → held move with no reason, carrying the hold's seen, the /resume's comment\n%s", moves[0], want)
	}
	if !slices.Contains(t1.Labels, "stage:held") {
		t.Errorf("T1 is labelled %v, want stage:held", t1.Labels)
	}
	if got, want := records(f.State().Issues[0]), []string{wantRecord(threeTasks)}; !reflect.DeepEqual(got, want) {
		t.Errorf("issue 1's records are\n%q\nwant\n%q", got, want)
	}
}

// A held child's first move, a held → held marker with no reason, stands
// for its first move (SPEC §5.2, "Creating the children"), though it isn't
// keyed by the parent's latest move: here a crash stopped the pass after
// T1's held first move, and the owner sent the parent back and approved
// the plan again before the next pass. T1 gets no second move, keyed by
// the new approval, while T2 and T3 get theirs. A crash at any write of
// the pass after the approval replays.
func TestChildrenHeldFirstMoveReapproved(t *testing.T) {
	t.Parallel()
	held := creationRestHeld(t)
	both := childrenCrashOn(t, threeTasks, 1, 2)
	create := writeAt(t, held, both, 1, isCreate) // T2's, after T1's held first move
	setup := func(tb testing.TB) *githubtest.Fake {
		f := childrenCrashStopped(tb, held, both, create)
		f.RemoveLabel(1, "owner", "stage:planned")
		f.AddLabel(1, "owner", "stage:plan-review")
		f.AddComment(1, "owner", "/approve")
		f.ForgetWrites()
		return f
	}
	githubtest.Replay(t, setup, both)

	f := run(t, setup, both)
	st := f.State()
	if len(st.Issues) != 4 || !reflect.DeepEqual(st.Issues[0].SubIssues, []int{2, 3, 4}) {
		t.Fatalf("%+v; want the parent and its three children, linked in plan order", st.Issues)
	}
	ids, _ := childrenCrashPlanMoves(t, f)
	if len(ids) != 2 {
		t.Fatalf("issue 1 has %d moves into planned with plan revision 1, want the approval and the one after the owner sent it back", len(ids))
	}
	moves := childrenCrashMoves(st.Issues[1])
	if len(moves) != 1 || !strings.HasSuffix(moves[0], "<!-- issuedog:transition from=held to=held at=ready origin=ready event=p"+strconv.FormatInt(ids[0], 10)+" -->") {
		t.Errorf("T1's first moves are\n%q\nwant its one held → held move, keyed by the first approval", moves)
	}
	if !slices.Contains(st.Issues[1].Labels, "stage:held") {
		t.Errorf("T1 is labelled %v, want stage:held", st.Issues[1].Labels)
	}
	for i, w := range wantChildren(threeTasks)[1:] {
		if got, want := childrenCrashMoves(st.Issues[i+2]), []string{firstMove(w, ids[1])}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s's first moves are\n%q\nwant one, keyed by the latest approval\n%q", w.item, got, want)
		}
	}
	if got, want := records(st.Issues[0]), []string{wantRecord(threeTasks)}; !reflect.DeepEqual(got, want) {
		t.Errorf("issue 1's records are\n%q\nwant\n%q", got, want)
	}
}

// A held child that a trusted user moved back to intake by hand, with the
// stage label, gets no first move: the label rules record the change as
// an override, with by=, and a found child keeps the stage a trusted user
// set (SPEC §5.2, "A found child keeps its labels"). Only a move back to
// intake that isn't an override, as a /resume's, leaves it a first move to
// get. Here T1 was linked but not yet moved when a pass stopped, then held
// with the parent by its /hold, and the owner set stage:intake on it
// before the parent's /resume. A crash at any write replays.
func TestChildrenHandSetIntake(t *testing.T) {
	t.Parallel()
	first := planAtReview(threeTasks, thumbsUp)
	pass := childrenPass(t, threeTasks)
	move := writeAt(t, first, pass, 1, isLink) + 1 // T1's first move
	setup := func(tb testing.TB) *githubtest.Fake {
		f := childrenCrashStopped(tb, first, pass, move)
		f.AddComment(1, "owner", "/hold")
		if err := pass(f); err != nil {
			tb.Fatal(err)
		}
		f.RemoveLabel(2, "owner", "stage:held")
		f.AddLabel(2, "owner", "stage:intake")
		f.AddComment(1, "owner", "/resume")
		f.ForgetWrites()
		return f
	}
	githubtest.Replay(t, setup, pass)

	f := run(t, setup, pass)
	st := f.State()
	if len(st.Issues) != 4 || !reflect.DeepEqual(st.Issues[0].SubIssues, []int{2, 3, 4}) {
		t.Fatalf("%+v; want the parent and its three children, linked in plan order", st.Issues)
	}
	t1 := st.Issues[1]
	if got := childrenCrashMoves(t1); len(got) != 0 {
		t.Errorf("T1's first moves are %q, want none", got)
	}
	if got := t1.Comments[len(t1.Comments)-1].Body; !strings.Contains(got, "<!-- issuedog:transition from=held to=intake by=owner event=") {
		t.Errorf("T1's newest comment is %q, want the override of its move back to intake by hand", got)
	}
	if !slices.Contains(t1.Labels, "stage:intake") {
		t.Errorf("T1 is labelled %v, want stage:intake, as the owner set it", t1.Labels)
	}
	p, _ := planMoveID(t, f)
	for i, w := range wantChildren(threeTasks)[1:] {
		if got, want := childrenCrashMoves(st.Issues[i+2]), []string{firstMove(w, p)}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s's first moves are\n%q\nwant\n%q", w.item, got, want)
		}
	}
	if got, want := records(st.Issues[0]), []string{wantRecord(threeTasks)}; !reflect.DeepEqual(got, want) {
		t.Errorf("issue 1's records are\n%q\nwant\n%q", got, want)
	}
}
