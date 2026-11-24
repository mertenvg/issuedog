package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/repo"
)

// A conflict bug made moot is closed as not planned (SPEC §7.5's A
// conflict merged by hand), on conflictFFSeeds, with real temporary
// repos: story 1's branch, its conflict bug #2 for the seed's tip and main
// commit, and its PR. Once the story branch's tip holds that main commit
// other than through the bug's own head, as when a person merged it, or a
// newer main, into the branch by hand, step 4 closes the bug's open PR,
// then the bug, keyed event=t<SHA> by the tip, before it judges the tip,
// which it then checks as any push the App didn't make.

// conflictMootMerge merges main, a main commit, into s's story tip in s's
// origin's work tree by hand, as a person does, resolving f.txt, sends the
// merge to the origin outside refs/heads, so a fake can put the story
// branch there, goes back to main, and returns the merge.
func conflictMootMerge(t *testing.T, s *conflictFFSeed, main string) string {
	t.Helper()
	w := s.base.o.work
	gitIn(t, w, "checkout", "--quiet", "-B", "by-hand", s.tip)
	gitIn(t, w, "merge", "--quiet", "--no-edit", "-s", "ours", main)
	if err := os.WriteFile(filepath.Join(w, "f.txt"), []byte("merged by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, w, "add", "f.txt")
	gitIn(t, w, "commit", "--quiet", "--amend", "--no-edit")
	merge := gitIn(t, w, "rev-parse", "HEAD")
	gitIn(t, w, "push", "--quiet", s.base.o.url, merge+":refs/seed/"+merge)
	gitIn(t, w, "checkout", "--quiet", "main")
	return merge
}

// conflictMootText is the comment of conflict bug #2's close once tip
// makes it moot, holding main, up to its marker.
func conflictMootText(tip, main string) string {
	return "Closed as not planned: the story branch's tip `" + tip + "` holds `main` at `" + main + "`, which this conflict bug was filed to merge, " +
		"though not through this bug's own head, as when someone merged that commit, or a newer `main`, into the branch by hand. " +
		"So this bug is moot, and its PR, if it had an open one, is closed.\n\n"
}

// conflictMootMarker is the transition marker of conflict bug #2's close
// from stage from once tip makes it moot.
func conflictMootMarker(from, tip string) string {
	return "<!-- issuedog:transition from=" + from + " to=closed reason=not_planned event=t" + tip + " -->"
}

// conflictMootPRCloses are the App's closes of PR pr among f's writes since
// from, PATCH /pulls/{n} with the state closed, and their indexes.
func conflictMootPRCloses(f *githubtest.Fake, from, pr int) []int {
	var at []int
	for i, w := range f.Writes()[from:] {
		if w.Method == http.MethodPatch && w.Path == "/pulls/"+strconv.Itoa(pr) && strings.Contains(w.Body, `"state":"closed"`) {
			at = append(at, from+i)
		}
	}
	return at
}

// conflictMootBugClose is the index, among f's writes, of the close of
// issue n, PATCH /issues/{n} with the state closed, or -1 for none.
func conflictMootBugClose(f *githubtest.Fake, n int) int {
	return slices.IndexFunc(f.Writes(), func(w githubtest.Write) bool {
		return w.Method == http.MethodPatch && w.Path == "/issues/"+strconv.Itoa(n) && strings.Contains(w.Body, `"state":"closed"`) && !w.Failed
	})
}

// conflictMootClosed fails unless bug #2 on f is closed as not planned,
// by one App move from from on its conflict-moot, keyed event=t<tip>, with
// conflictMootText for tip and main, and its PR closed, unmerged, by one
// write of the App's, before the bug's close.
func conflictMootClosed(t *testing.T, f *githubtest.Fake, from, tip, main string) {
	t.Helper()
	closes := storyFailureComments(f, 2, "<!-- issuedog:transition from="+from+" to=closed ")
	if len(closes) != 1 || closes[0] != conflictMootText(tip, main)+conflictMootMarker(from, tip) {
		t.Fatalf("the bug's closes = %q; want one,\n%s", closes, conflictMootText(tip, main)+conflictMootMarker(from, tip))
	}
	is, _, err := f.Client(t).Issues.Get(context.Background(), "o", "r", 2)
	if err != nil {
		t.Fatal(err)
	}
	if is.GetState() != "closed" || is.GetStateReason() != "not_planned" {
		t.Errorf("the bug is %s, as %s; want closed, as not_planned", is.GetState(), is.GetStateReason())
	}
	pr := f.State().PRs[0]
	prCloses := conflictMootPRCloses(f, 0, pr.Number)
	if pr.State != "closed" || pr.Merged || len(prCloses) != 1 {
		t.Fatalf("the bug's PR = %+v, closed by the writes %v; want it closed, unmerged, by one", pr, prCloses)
	}
	if bug := conflictMootBugClose(f, 2); bug < prCloses[0] {
		t.Errorf("the bug's close is write %d, its PR's write %d; want the PR's close first", bug, prCloses[0])
	}
}

// conflictMootJudged runs the pass that closes bug #2 on v, made moot by
// tip, story 1's branch's tip, holding main, from stage from, and fails
// unless the same pass then checks tip as success, as any push the App
// didn't make, running the commands once and starting no claude run. The
// passes after read what it wrote, then make no request but 304s.
func conflictMootJudged(t *testing.T, v *verdictRig, from, tip, main string) {
	t.Helper()
	ran, runs := v.checksRan(), v.runs()
	v.pass()
	conflictMootClosed(t, v.f, from, tip, main)
	if got := v.f.State().CheckRuns; !slices.Contains(got, tipCheck(tip, tipBranch, "success")) || v.checksRan() != ran+1 || v.runs() != runs {
		t.Errorf("the moot close's pass left the checks %+v, ran the commands %d times and claude %d; want success on %s, once, and none",
			got, v.checksRan()-ran, v.runs()-runs, tip)
	}
	v.passes(2)
	v.quietPass("after the moot close")
}

// A person's merge of the main commit a conflict bug names into the story
// branch makes the bug moot (SPEC §7.5's A conflict merged by hand): the
// pass that finds the new tip closes the bug's open PR, then the bug, as
// not planned, keyed event=t<SHA> by the tip, with a comment naming it and
// the main commit, then checks the tip. Once the passes are quiet, a
// person's later push, to a story with no open conflict bug now, is
// checked with no request for the bug or its PR: only the tip's check
// runs are read, and its check posted.
func TestConflictMootMerged(t *testing.T) {
	t.Parallel()
	s := newConflictFFSeed(t)
	merge := conflictMootMerge(t, s, s.main)
	f := s.fake(t, merge, s.head, nil)
	v := s.rig(t, f, t.TempDir(), nil)
	conflictMootJudged(t, v, "pr-open", merge, s.main)
	later := v.tipPush(tipBranch, map[string]string{"later.txt": "a person's later change\n"})
	from := len(f.Requests())
	v.pass()
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(later, tipBranch, "success")) {
		t.Fatalf("the later push's pass left the checks %+v; want success on %s", got, later)
	}
	for _, r := range f.Requests()[from:] {
		quiet := r.Method == http.MethodGet && r.Conditional && r.Status == http.StatusNotModified
		tip := (r.Method == http.MethodGet && r.Path == "/commits/"+later+"/check-runs") || (r.Method == http.MethodPost && r.Path == "/check-runs")
		if !quiet && !tip {
			t.Errorf("the later push's pass sent %+v; want only the tip's check runs read and its check posted, and 304s", r)
		}
	}
}

// A person's merge of a newer main, past the main commit the bug names,
// makes the bug moot too (SPEC §7.5's A conflict merged by hand): the tip
// holds that commit through main's history.
func TestConflictMootNewerMain(t *testing.T) {
	t.Parallel()
	s := newConflictFFSeed(t)
	newer := s.base.storyMainMoved("g.txt", "a newer main\n")
	merge := conflictMootMerge(t, s, newer)
	f := s.fake(t, merge, s.head, nil)
	v := s.rig(t, f, t.TempDir(), nil)
	conflictMootJudged(t, v, "pr-open", merge, s.main)
}

// A tip that holds the conflict bug's own head isn't moot, though it holds
// the main commit the bug names (SPEC §7.5's A conflict merged by hand):
// the bug's own fast-forward, before GitHub marks its PR merged, and a
// person's push on top of it. And a bug at in-progress with no PR, whose
// head is its head ref alone, refs/issuedog/tasks/<n>, isn't moot on a tip
// that holds that head. None closes the bug or its PR, and once the tip's
// check is read, the passes make no request but 304s.
func TestConflictMootOwnHead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		person bool
		// ref puts the head in the bug's head ref, with the bug at
		// in-progress and no move into pr-open.
		ref bool
	}{
		{name: "its own fast-forward"},
		{name: "a person's push on its fast-forward", person: true},
		{name: "its head ref alone", person: true, ref: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newConflictFFSeed(t)
			at := s.head
			if tc.person {
				at = s.commit(t, "person.txt", "a person's change on the bug's head\n", false)
			}
			var f *githubtest.Fake
			if tc.ref {
				f = conflictMootInProgressFake(t, s, at, false)
			} else {
				f = s.fake(t, at, s.head, nil)
			}
			v := s.rig(t, f, t.TempDir(), nil)
			if tc.ref {
				gitIn(t, v.d.Clone.Dir(), "fetch", "--quiet", "--no-tags", s.base.o.url, "refs/seed/"+s.head+":"+repo.TasksRef+"2")
			}
			v.passes(3)
			if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(at, tipBranch, "success")) {
				t.Fatalf("the checks are %+v; want success on %s as the story branch's tip", got, at)
			}
			v.quietPass("a tip that holds the bug's own head")
			if closes := storyFailureComments(f, 2, " to=closed "); len(closes) != 0 || f.State().Issues[1].Closed {
				t.Errorf("the bug is closed %t, with the closes %q; want it open, with none", f.State().Issues[1].Closed, closes)
			}
			if got := conflictMootPRCloses(f, 0, 3); len(got) != 0 {
				t.Errorf("the App closed the bug's PR by the writes %v; want none", got)
			}
		})
	}
}

// conflictMootInProgressFake is s's fake with the story branch at at, the
// bug #2's branch at s's head, and the bug at in-progress. With pr, it has
// its PR open still, as when its story branch's tip moved since its verify
// sent it back, keyed event=b<SHA>. Without, it has no PR, and no move of
// the bug's into pr-open names a head, as for a bug whose run hasn't
// passed verify yet: its moves are its first move, then the pick's. Its
// writes so far are forgotten.
func conflictMootInProgressFake(t testing.TB, s *conflictFFSeed, at string, pr bool) *githubtest.Fake {
	t.Helper()
	if pr {
		return s.fake(t, at, s.head, func(f *githubtest.Fake, _ int) {
			f.RemoveLabel(2, app, "stage:pr-open")
			f.AddLabel(2, app, "stage:in-progress")
			f.AddComment(2, app, "The story branch moved.\n\n<!-- issuedog:transition from=pr-open to=in-progress event=b"+s.tip+" -->")
		})
	}
	gitIn(t, s.base.o.url, "update-ref", "refs/heads/"+s.branch, s.head)
	gitIn(t, s.base.o.url, "update-ref", "refs/heads/"+tipBranch, at)
	f := newFake(t)
	f.SetPermission(wholeRunOwner, "admin")
	f.SetGitRemote(s.base.o.url)
	tipStory(t, f)
	bug := f.AddIssue(app, "Merge main at "+s.main[:7]+" into story #1's branch", "Resolve the conflicts.\n\n"+storyMainConflictOrigin(s.tip, s.main),
		"type:bug", "priority:p1")
	if bug != 2 {
		t.Fatalf("the bug is #%d; want #2", bug)
	}
	f.SetParent(2, 1)
	f.AddLabel(2, app, "stage:in-progress")
	f.AddComment(2, app, "Filed.\n\n<!-- issuedog:transition from=intake to=ready event=t"+s.tip+" -->")
	f.AddComment(2, app, "Picked.\n\n<!-- issuedog:transition from=ready to=in-progress event=s1 -->")
	if _, err := f.Client(t).PostCheck(context.Background(), "o", "r", github.TipCheck, github.NewCheckRun{HeadSHA: s.tip, Conclusion: "success",
		Title: "t", Summary: "s", ExternalID: "branch=" + tipBranch}); err != nil {
		t.Fatal(err)
	}
	f.ForgetWrites()
	return f
}

// A held conflict bug made moot is closed from held, its PR first, which
// ends its hold (SPEC §6.4, §7.5's A conflict merged by hand).
func TestConflictMootHeld(t *testing.T) {
	t.Parallel()
	s := newConflictFFSeed(t)
	merge := conflictMootMerge(t, s, s.main)
	f := s.fake(t, merge, s.head, func(f *githubtest.Fake, _ int) {
		hold := f.AddComment(2, "owner", "/hold")
		f.RemoveLabel(2, app, "stage:pr-open")
		f.AddLabel(2, app, "stage:held")
		f.AddComment(2, app, fmt.Sprintf("On hold.\n\n<!-- issuedog:transition from=pr-open to=held at=pr-open reason=owner cmd=%d -->", hold))
	})
	v := s.rig(t, f, t.TempDir(), nil)
	conflictMootJudged(t, v, "held", merge, s.main)
}

// A conflict bug at in-progress, sent back with its PR open, made moot,
// is closed from in-progress, its PR first, and no run of its starts, on a
// rig whose in-progress stage would run it (SPEC §7.5's A conflict merged
// by hand, §8.2): its run waits for a tip step 4 judged, and once the bug
// is closed, nothing starts it.
func TestConflictMootInProgress(t *testing.T) {
	t.Parallel()
	s := newConflictFFSeed(t)
	merge := conflictMootMerge(t, s, s.main)
	f := conflictMootInProgressFake(t, s, merge, true)
	v := s.rig(t, f, t.TempDir(), nil)
	v.noPick = false
	v.wire()
	conflictMootJudged(t, v, "in-progress", merge, s.main)
	if v.runs() != 0 {
		t.Errorf("claude ran %d times; want none for a closed bug", v.runs())
	}
}

// A crash just after the PR's close, as GitHub applied it but the answer
// was lost, then a restart (SPEC §5.2, §7.5's A conflict merged by hand):
// the crashed pass leaves the bug open, with its PR closed and the tip
// unjudged, and the fresh daemon's first pass, which finds the PR closed,
// closes the bug from where that left it, as not planned, keyed by the
// tip, and checks the tip, with the PR closed by that one write alone.
// The passes after are quiet.
func TestConflictMootCrash(t *testing.T) {
	t.Parallel()
	s := newConflictFFSeed(t)
	merge := conflictMootMerge(t, s, s.main)
	f := s.fake(t, merge, s.head, nil)
	pr := f.State().PRs[0].Number
	f.BeforeRequest(func(r githubtest.Request) bool {
		return r.Method == http.MethodPatch && r.Path == "/pulls/"+strconv.Itoa(pr)
	}, func() { f.FailWriteAfter(len(f.Writes()) + 1) })
	root := t.TempDir()
	v := s.rig(t, f, root, nil)
	if err := v.d.Iterate(t.Context()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("Iterate = %v; want the PR's close answered with a 500", err)
	}
	if got := f.State(); got.PRs[0].State != "closed" || got.Issues[1].Closed || slices.ContainsFunc(got.CheckRuns, func(r githubtest.CheckRunState) bool {
		return r.SHA == merge
	}) {
		t.Fatalf("the crashed pass left the PR %+v, the bug closed %t, and the checks %+v; want the PR closed, the bug open, and no check on %s",
			got.PRs[0], got.Issues[1].Closed, got.CheckRuns, merge)
	}
	v = s.rig(t, f, root, v.d.Clone)
	v.pass()
	from := "pr-open"
	if moves := storyFailureComments(f, 2, "<!-- issuedog:transition from=pr-open to=needs-human "); len(moves) == 1 {
		// Step 2 hands the bug off for its closed PR before step 4
		// judges the tip, as it would any task at pr-open.
		from = "needs-human"
	}
	conflictMootClosed(t, f, from, merge, s.main)
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(merge, tipBranch, "success")) {
		t.Errorf("the restart's pass left the checks %+v; want success on %s", got, merge)
	}
	v.passes(2)
	v.quietPass("after the restart")
}

// The PR's close, then the bug's, replay safely (SPEC §5.2), for a
// conflict bug at in-progress with its PR open, sent back on a tip that
// moved: every run ends with the PR closed, unmerged, and the bug closed as
// not planned, by one move keyed by the tip, and the tip checked as
// success. Each pass is a fresh daemon on the fake's own bare clone, as
// conflictFFSeed.replay runs it.
func TestConflictMootReplay(t *testing.T) {
	t.Parallel()
	s := newConflictFFSeed(t)
	merge := conflictMootMerge(t, s, s.main)
	roots, clones := map[*githubtest.Fake]string{}, map[*githubtest.Fake]*repo.Repo{}
	var fakes []*githubtest.Fake
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := conflictMootInProgressFake(tb, s, merge, true)
		roots[f] = t.TempDir()
		fakes = append(fakes, f)
		return f
	}, func(f *githubtest.Fake) error {
		v := s.rig(t, f, roots[f], clones[f])
		clones[f] = v.d.Clone
		for range 5 {
			if err := v.d.Iterate(context.Background()); err != nil {
				return err
			}
		}
		return nil
	})
	for _, f := range fakes {
		st := f.State()
		closes := storyFailureComments(f, 2, conflictMootMarker("in-progress", merge))
		if len(st.PRs) != 1 || st.PRs[0].State != "closed" || st.PRs[0].Merged || !st.Issues[1].Closed || len(closes) != 1 {
			t.Fatalf("after the replay, the PRs = %+v, the bug closed %t, by the closes %q; want one, closed unmerged, and the bug closed by one",
				st.PRs, st.Issues[1].Closed, closes)
		}
		if !slices.Contains(st.CheckRuns, tipCheck(merge, tipBranch, "success")) {
			t.Errorf("after the replay, the checks = %+v; want success on %s", st.CheckRuns, merge)
		}
	}
}

// A conflict bug whose head, the one its latest move into pr-open names,
// the bare repo lacks, as one gone with its branch, isn't in the tip's
// history, which the bare repo holds, so a tip holding the main commit it
// names makes it moot, with no error (SPEC §7.5's A conflict merged by
// hand). Here it's at in-progress, sent back again after a second PR step
// that named that head.
func TestConflictMootHeadMissing(t *testing.T) {
	t.Parallel()
	s := newConflictFFSeed(t)
	merge := conflictMootMerge(t, s, s.main)
	gone := strings.Repeat("ab", 20)
	f := conflictMootInProgressFake(t, s, merge, true)
	f.AddComment(2, app, "The implementer committed.\n\n<!-- issuedog:transition from=in-progress to=verifying attempt=2 run=r2 head="+gone+" -->")
	f.AddComment(2, app, "PR updated.\n\n<!-- issuedog:transition from=verifying to=pr-open run=rv2 head="+gone+" pr=3 tip="+s.tip+" -->")
	f.AddComment(2, app, "The story branch moved.\n\n<!-- issuedog:transition from=pr-open to=in-progress event=b"+s.main+" -->")
	f.ForgetWrites()
	v := s.rig(t, f, t.TempDir(), nil)
	conflictMootJudged(t, v, "in-progress", merge, s.main)
}
