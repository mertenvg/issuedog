package daemon_test

import (
	"context"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// Rare crashes while creating a plan's children (#334, from #276's
// verify): a child filed but not linked when the owner approves the plan
// again, a child linked but not moved when a hold moves it, and a failure
// no later pass gets past, which is reported once on the parent.

// childrenCrashStopped runs pass once on a fresh fake from setup, stopped
// by a failed write at write n of a clean pass, and fails t unless the
// pass stopped. Then it clears the failure and returns the fake.
func childrenCrashStopped(t testing.TB, setup func(testing.TB) *githubtest.Fake, pass func(*githubtest.Fake) error, n int) *githubtest.Fake {
	t.Helper()
	f := setup(t)
	f.FailWrite(n)
	if err := pass(f); err == nil {
		t.Fatalf("the first pass made every write; want it stopped at write %d", n)
	}
	f.FailWrite(0)
	return f
}

// childrenCrashMoves are the bodies of is's comments that carry a move
// keyed by a parent's move into planned, event=p<id>: a child's first move.
func childrenCrashMoves(is githubtest.IssueState) []string {
	var moves []string
	for _, c := range is.Comments {
		if strings.Contains(c.Body, " event=p") {
			moves = append(moves, c.Body)
		}
	}
	return moves
}

// childrenCrashPlanMoves are the IDs of the App's comments on issue 1 that
// moved it into planned with plan revision 1, oldest first, and when each
// was posted.
func childrenCrashPlanMoves(t *testing.T, f *githubtest.Fake) ([]int64, []time.Time) {
	t.Helper()
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	var at []time.Time
	for _, c := range s.Comments {
		if c.Author.Login == app && strings.Contains(c.Body, "to=planned rev=1 ") {
			ids, at = append(ids, c.ID), append(at, c.CreatedAt)
		}
	}
	return ids, at
}

// A child filed just before a crash, and never linked, is still found
// when the owner sends the parent back to plan-review by hand and
// approves the same plan revision again before the next pass. The new
// approval is later than the child's last update, so the issues are listed
// since the revision's first approval, not its latest (SPEC §5.2, "Finding
// a child, without search"). The child is linked and moved, keyed by the latest
// approval, with no second issue, and a crash at any write replays.
func TestChildrenReapprovedUnlinked(t *testing.T) {
	t.Parallel()
	first := planAtReview(threeTasks, thumbsUp)
	pass := childrenPass(t, threeTasks)
	link := writeAt(t, first, pass, 1, isLink)
	setup := func(tb testing.TB) *githubtest.Fake {
		f := childrenCrashStopped(tb, first, pass, link)
		f.RemoveLabel(1, "owner", "stage:planned")
		f.AddLabel(1, "owner", "stage:plan-review")
		f.AddComment(1, "owner", "/approve")
		f.ForgetWrites()
		return f
	}
	githubtest.Replay(t, setup, pass)

	f := run(t, setup, pass)
	st := f.State()
	if len(st.Issues) != 4 || !reflect.DeepEqual(st.Issues[0].SubIssues, []int{2, 3, 4}) {
		t.Fatalf("%+v; want the parent and its three children, T1 the one filed before the crash, linked in plan order", st.Issues)
	}
	ids, at := childrenCrashPlanMoves(t, f)
	if len(ids) != 2 {
		t.Fatalf("issue 1 has %d moves into planned with plan revision 1, want the approval and the one after the owner sent it back", len(ids))
	}
	since := "since=" + url.QueryEscape(at[0].UTC().Format(time.RFC3339))
	if !slices.ContainsFunc(f.Requests(), func(r githubtest.Request) bool {
		return r.Method == "GET" && r.Path == "/issues" && strings.Contains(r.Query, since)
	}) {
		t.Errorf("no issue list since the first approval, %s, in %+v", at[0], f.Requests())
	}
	for i, w := range wantChildren(threeTasks) {
		if got, want := childrenCrashMoves(st.Issues[i+1]), []string{firstMove(w, ids[1])}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s's first moves are\n%q\nwant one, keyed by the latest approval\n%q", w.item, got, want)
		}
	}
	if got, want := records(st.Issues[0]), []string{wantRecord(threeTasks)}; !reflect.DeepEqual(got, want) {
		t.Errorf("issue 1's records are\n%q\nwant\n%q", got, want)
	}
	noSearch(t, f)
}

// childrenCrashOn reads each of numbers in turn and runs step 1 on it,
// then ChildrenVisit on issue 1, as Iterate's visits in number order do.
func childrenCrashOn(t *testing.T, k childPlan, numbers ...int) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		st := childrenStep(t, f, k)
		var parent *github.Snapshot
		for _, n := range numbers {
			s, err := st.Client.Snapshot(context.Background(), "o", "r", n)
			if err != nil {
				return err
			}
			if _, err := st.Run(context.Background(), s); err != nil {
				return err
			}
			if n == 1 {
				parent = s
			}
		}
		return st.ChildrenVisit(context.Background(), parent)
	}
}

// A child linked just before a crash, but not yet moved, that a /hold on
// the parent then holds with it, gets its first move once a /resume
// brings both back: the hold and the move back from it to intake aren't
// a first move (SPEC §5.2, "Creating the children"). A crash at any write
// of the pass that resumes them replays.
func TestChildrenHeldBeforeFirstMove(t *testing.T) {
	t.Parallel()
	first := planAtReview(threeTasks, thumbsUp)
	pass := childrenPass(t, threeTasks)
	move := writeAt(t, first, pass, 1, isLink) + 1 // T1's first move
	held := func(tb testing.TB) *githubtest.Fake {
		f := childrenCrashStopped(tb, first, pass, move)
		f.AddComment(1, "owner", "/hold")
		if err := pass(f); err != nil {
			tb.Fatal(err)
		}
		return f
	}
	f := held(t)
	st := f.State()
	if len(st.Issues) != 2 || !slices.Contains(st.Issues[0].Labels, "stage:held") || !slices.Contains(st.Issues[1].Labels, "stage:held") {
		t.Fatalf("%+v; want the parent and T1, both held", st.Issues)
	}
	resumed := func(tb testing.TB) *githubtest.Fake {
		f := held(tb)
		f.AddComment(1, "owner", "/resume")
		f.ForgetWrites()
		return f
	}
	githubtest.Replay(t, resumed, pass)

	f = run(t, resumed, pass)
	st = f.State()
	if len(st.Issues) != 4 || !reflect.DeepEqual(st.Issues[0].SubIssues, []int{2, 3, 4}) {
		t.Fatalf("%+v; want the parent and its three children, linked in plan order", st.Issues)
	}
	p, _ := planMoveID(t, f)
	for i, w := range wantChildren(threeTasks) {
		is := st.Issues[i+1]
		if got, want := childrenCrashMoves(is), []string{firstMove(w, p)}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s's first moves are\n%q\nwant\n%q", w.item, got, want)
		}
		if want := "stage:" + string(w.stage); !slices.Contains(is.Labels, want) {
			t.Errorf("%s is labelled %v, want %s", w.item, is.Labels, want)
		}
	}
	if got := len(st.Issues[1].Comments); got != 3 {
		t.Errorf("T1 has %d comments, want its hold, its resume and its first move: %+v", got, st.Issues[1].Comments)
	}
	if got, want := records(st.Issues[0]), []string{wantRecord(threeTasks)}; !reflect.DeepEqual(got, want) {
		t.Errorf("issue 1's records are\n%q\nwant\n%q", got, want)
	}
}

// A child linked just before a crash, but not yet moved, that is still
// held when the parent's visit finds it, here by the owner's /hold on the
// child itself, gets its first move as a held child: a held → held move
// with no reason, keyed as a first move is, that makes its first stage
// the hold's at and origin (SPEC §5, §7.2). So it stays held, the record
// stands, and /resume on the child takes it to its first stage. A crash
// at any write replays.
func TestChildrenHeldChildFirstMove(t *testing.T) {
	t.Parallel()
	first := planAtReview(threeTasks, thumbsUp)
	pass := childrenPass(t, threeTasks)
	move := writeAt(t, first, pass, 1, isLink) + 1 // T1's first move
	setup := func(tb testing.TB) *githubtest.Fake {
		f := childrenCrashStopped(tb, first, pass, move)
		f.AddComment(2, "owner", "/hold")
		f.ForgetWrites()
		return f
	}
	both := childrenCrashOn(t, threeTasks, 1, 2)
	githubtest.Replay(t, setup, both)

	f := run(t, setup, both)
	st := f.State()
	if len(st.Issues) != 4 || !reflect.DeepEqual(st.Issues[0].SubIssues, []int{2, 3, 4}) {
		t.Fatalf("%+v; want the parent and its three children, linked in plan order", st.Issues)
	}
	p, _ := planMoveID(t, f)
	t1 := st.Issues[1]
	moves := childrenCrashMoves(t1)
	if len(moves) != 1 {
		t.Fatalf("T1's first moves are %q, want one", moves)
	}
	want := "Created from #1's plan, rev 1, as T1. It stays on hold, and `/resume` takes it to `stage:ready`."
	// The hold has no seen, as T1 had no round before it, so neither has
	// the move.
	want += "\n\n<!-- issuedog:transition from=held to=held at=ready origin=ready event=p" + strconv.FormatInt(p, 10) + " -->"
	if moves[0] != want {
		t.Errorf("T1's first move is\n%s\nwant a held → held move with no reason, at and origin ready, keyed by the approval\n%s", moves[0], want)
	}
	if !slices.Contains(t1.Labels, "stage:held") {
		t.Errorf("T1 is labelled %v, want stage:held", t1.Labels)
	}
	if got, want := records(st.Issues[0]), []string{wantRecord(threeTasks)}; !reflect.DeepEqual(got, want) {
		t.Errorf("issue 1's records are\n%q\nwant\n%q", got, want)
	}

	f.AddComment(2, "owner", "/resume")
	if err := both(f); err != nil {
		t.Fatal(err)
	}
	if got := f.State().Issues[1].Labels; !slices.Contains(got, "stage:ready") {
		t.Errorf("after /resume on T1, it's labelled %v, want stage:ready", got)
	}
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Derive(s, slug, clean).Stage; got != state.Ready {
		t.Errorf("after /resume on T1, it's at %s, want ready", got)
	}
}

// childrenCrashError is the state error marker for an approved plan's
// children on issue 1, keyed by comment p.
func childrenCrashError(p int64) string {
	return "<!-- issuedog:error kind=children key=" + strconv.FormatInt(p, 10) + " -->"
}

// childrenCrashErrors are the bodies of is's comments that carry a state
// error.
func childrenCrashErrors(is githubtest.IssueState) []string {
	var bodies []string
	for _, c := range is.Comments {
		if strings.Contains(c.Body, "issuedog:error") {
			bodies = append(bodies, c.Body)
		}
	}
	return bodies
}

// childrenCrashOtherParent is the setup of a pass that finds T1 filed but
// not linked, as a crash at its link left it, and linked by hand under
// another issue, #3, so GitHub refuses its link with a 422.
func childrenCrashOtherParent(t *testing.T) func(testing.TB) *githubtest.Fake {
	first := planAtReview(threeTasks, thumbsUp)
	pass := childrenPass(t, threeTasks)
	link := writeAt(t, first, pass, 1, isLink)
	return func(tb testing.TB) *githubtest.Fake {
		f := childrenCrashStopped(tb, first, pass, link)
		f.SetParent(2, f.AddIssue("owner", "Other", "body"))
		f.ForgetWrites()
		return f
	}
}

// A failure creating the children that no later pass gets past, as
// nothing it reads would change, is reported once on the parent, as a
// state error keyed by the comment holding its latest move, the one into
// planned (SPEC §5.3): plan data that won't read back, an item that can't
// be written as an issue, and a link GitHub refuses with a 422, here for
// a child linked under another issue by hand. The error says what failed
// in the daemon's words, quoting neither the planner's text nor GitHub's.
// The visit then returns no error, so the parent isn't read again for it,
// and a visit after that makes no request at all, while the parent stays
// where it is. A crash at the error's post replays.
func TestChildrenStuckReported(t *testing.T) {
	t.Parallel()
	badData := github.Marker{Kind: "plan-data", Fields: map[string]string{"v": "1", "data": "bm90LWRlZmxhdGVk"}}
	noScope := func(tb testing.TB) github.Marker {
		return planDataEdited(tb, threeTasks, func(item map[string]any) {
			if item["id"] == "T2" {
				delete(item, "scope")
			}
		})
	}
	withData := func(data func(testing.TB) github.Marker) func(testing.TB) *githubtest.Fake {
		return atPlanning(threeTasks, func(tb testing.TB, f *githubtest.Fake) {
			moveOn(tb, f, 1, daemon.Move{To: state.PlanReview, Trigger: state.TriggerPlanValid, Key: state.Key{Field: "run", ID: "p1"}, PlanRev: 1,
				Text: planText(1, 1), Markers: []github.Marker{data(tb)}}, 1002)
			thumbsUp(tb, f)
		})
	}
	for _, tt := range []struct {
		name   string
		setup  func(testing.TB) *githubtest.Fake
		why    string
		issues int // the issues but the parent: the children filed, before or by the pass, and any other
	}{
		{"plan data that won't read back", withData(func(testing.TB) github.Marker { return badData }),
			"its data doesn't read back from the plan comment", 0},
		{"an item that can't be written as an issue", withData(noScope),
			"an item of it can't be written as an issue", 0},
		{"a link GitHub refuses with a 422", childrenCrashOtherParent(t),
			"GitHub refused to link #2, the child of item T1, under this issue", 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pass := childrenPass(t, threeTasks)
			githubtest.Replay(t, tt.setup, pass)

			f := run(t, tt.setup, pass)
			st := f.State()
			p, _ := planMoveID(t, f)
			want := "I can't create the children of plan rev 1, as " + tt.why +
				". I won't try again until this issue moves, as by `/hold` and `/resume`. The daemon's log has the error.\n\n" + childrenCrashError(p)
			if got := childrenCrashErrors(st.Issues[0]); !reflect.DeepEqual(got, []string{want}) {
				t.Errorf("issue 1's state errors are\n%q\nwant one\n%q", got, want)
			}
			if got := len(st.Issues) - 1; got != tt.issues || len(records(st.Issues[0])) != 0 || len(st.Issues[0].SubIssues) != 0 {
				t.Errorf("%+v; want %d issues besides the parent, none linked under it, and no record", st.Issues, tt.issues)
			}
			if !slices.Contains(st.Issues[0].Labels, "stage:planned") {
				t.Errorf("issue 1 is labelled %v, want stage:planned still", st.Issues[0].Labels)
			}
			visit := childrenStep(t, f, threeTasks)
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
		})
	}
}

// A link that fails with a 5xx isn't a failure no later pass gets past:
// the visit returns it, so the parent is read again next pass, and no
// state error is posted.
func TestChildrenLinkServerErrorRetried(t *testing.T) {
	t.Parallel()
	setup := planAtReview(threeTasks, thumbsUp)
	pass := childrenPass(t, threeTasks)
	f := setup(t)
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == "POST" && r.Path == "/issues/1/sub_issues" }, 502)
	if err := pass(f); err == nil {
		t.Fatal("the pass whose link got a 502 returned no error; want it returned, so the parent is read again")
	}
	if got := childrenCrashErrors(f.State().Issues[0]); len(got) != 0 {
		t.Errorf("issue 1's state errors are %q; want none for a 502", got)
	}
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	checkChildren(t, f, threeTasks)
}

// Once the parent moves, its children's state error ends, and the visit
// tries again, as the error's key is the comment holding the parent's
// latest move: here the owner unlinks the child from the other issue, then
// holds and resumes the parent, and the children are created.
func TestChildrenStuckRetriedAfterMove(t *testing.T) {
	t.Parallel()
	pass := childrenPass(t, threeTasks)
	f := run(t, childrenCrashOtherParent(t), pass)
	if got := childrenCrashErrors(f.State().Issues[0]); len(got) != 1 {
		t.Fatalf("issue 1's state errors are %q; want one", got)
	}
	f.SetParent(2, 0)
	f.AddComment(1, "owner", "/hold")
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(1, "owner", "/resume")
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	st := f.State()
	if len(st.Issues) != 5 || !reflect.DeepEqual(st.Issues[0].SubIssues, []int{2, 4, 5}) {
		t.Fatalf("%+v; want the parent's three children, T1 the one filed before the crash, linked in plan order", st.Issues)
	}
	if got := childrenCrashErrors(st.Issues[0]); len(got) != 1 {
		t.Errorf("issue 1's state errors are %q; want the one, not posted again", got)
	}
	if got := records(st.Issues[0]); len(got) != 1 || !strings.HasSuffix(got[0], "<!-- issuedog:created rev=1 children=2,4,5 -->") {
		t.Errorf("issue 1's records are %q, want one naming 2, 4 and 5", got)
	}
}
