package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/issuedog/internal/state"
)

// pickReady files a task at ready, as its plan's approval moved it there,
// with priority:<p>, and returns its number.
func pickReady(f *githubtest.Fake, p string) int {
	n := fileAt(f, "ready", "Refuse an expired quote.", "priority:"+p)
	f.SetType(n, "Task")
	return n
}

// pickRig is newImplementerRig, with the implementer committing and
// returning the golden commits output, on a fresh root.
func pickRig(t *testing.T, f *githubtest.Fake) *rig {
	t.Helper()
	return newImplementerRig(t, f, newOrigin(t), t.TempDir(), nil, implementerOutput(t, "commits"))
}

// pickReads counts, in the fake's requests from from on, the issue
// searches and the reads of an issue's blocked_by.
func pickReads(f *githubtest.Fake, from int) (searches, blockedBy int) {
	for _, r := range f.Requests()[from:] {
		switch {
		case r.Path == "/search/issues":
			searches++
		case strings.HasSuffix(r.Path, "/dependencies/blocked_by"):
			blockedBy++
		}
	}
	return searches, blockedBy
}

// pickStage is issue n's stage label in the fake.
func pickStage(f *githubtest.Fake, n int) string {
	for _, l := range f.State().Issues[n-1].Labels {
		if strings.HasPrefix(l, "stage:") {
			return l
		}
	}
	return ""
}

// pickMoved is the App's comment moving a task from ready to in-progress,
// keyed by moved, the comment holding its move to ready, with seen.
func pickMoved(moved, seen int64) string {
	return fmt.Sprintf("I've picked this task, so the implementer starts on it now.\n\n"+
		"<!-- issuedog:transition from=ready to=in-progress event=s%d seen=%d -->", moved, seen)
}

// pickPass runs a pass of g and returns the issue searches and blocked_by
// reads it made.
func pickPass(g *rig) (searches, blockedBy int) {
	g.t.Helper()
	from := len(g.f.Requests())
	g.pass()
	return pickReads(g.f, from)
}

// With no task holding the worker, step 4 picks the ready task: it moves
// it to in-progress, keyed by the comment holding its move to ready, with
// seen, and starts its run in the same pass. The run is given the move, so
// its result isn't dropped for it: the task moves on to verifying, with
// seen= the move's comment (SPEC §8.1, §8.2).
func TestPickStartsTheRun(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := pickReady(f, "p1")
	g := pickRig(t, f)
	g.pass()
	if g.runs() != 1 {
		t.Fatalf("claude ran %d times, want once, in the pick's pass", g.runs())
	}
	cs := implementerComments(t, f, n)
	if len(cs) != 3 {
		t.Fatalf("the task has %d comments, want 3: its move to ready, the pick's and the run's", len(cs))
	}
	if want := pickMoved(cs[0].ID, cs[0].ID); cs[1].Author.Login != app || cs[1].Body != want {
		t.Errorf("the pick's comment, by %s, =\n%s\nwant\n%s", cs[1].Author.Login, cs[1].Body, want)
	}
	if want := implementerCommitted("run1", g.trimmed("commit.1"), cs[1].ID); cs[2].Body != want {
		t.Errorf("the run's comment =\n%s\nwant\n%s", cs[2].Body, want)
	}
	if got := pickStage(f, n); got != "stage:verifying" {
		t.Errorf("the task is at %s, want stage:verifying", got)
	}
	if l := g.leaseOf("1"); l.Issue != n || l.Stage != state.InProgress || l.Seen != cs[1].ID {
		t.Errorf("the run's lease = %+v; want it on #%d at in-progress, seen at the pick's comment %d", l, n, cs[1].ID)
	}
}

// The pick takes the ready task first by priority, then issue number,
// whatever order they were filed in, and leaves the others at ready.
func TestPickOrder(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	for _, p := range []string{"p2", "p1", "p0", "p0"} {
		pickReady(f, p)
	}
	g := pickRig(t, f)
	g.pass()
	if got := g.leaseIssues(); !slices.Equal(got, []int{3}) {
		t.Errorf("the runs leased %v, want #3 alone, the first by number at p0", got)
	}
	for n, want := range map[int]string{1: "stage:ready", 2: "stage:ready", 3: "stage:verifying", 4: "stage:ready"} {
		if got := pickStage(f, n); got != want {
			t.Errorf("#%d is at %s, want %s", n, got, want)
		}
	}
}

// A task at in-progress or verifying holds the worker: nothing is picked,
// and the pass makes no search and reads no blocked_by, on a start's
// first pass or after. The task at in-progress here is in a story, so it
// gets no run of its own until #624. A task waiting on the owner, at
// clarifying, needs-human, pr-open or held, releases the worker, and the
// ready task is picked (SPEC §8.1).
func TestPickWorker(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		stage string
		held  bool
	}{
		{"in-progress", true}, {"verifying", true},
		{"clarifying", false}, {"needs-human", false}, {"pr-open", false}, {"held", false},
	} {
		t.Run(tt.stage, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			other := fileAt(f, tt.stage, "Another task.", "priority:p0")
			f.SetType(other, "Task")
			if tt.stage == "in-progress" {
				story := fileAt(f, "planned", "A story.")
				f.SetType(story, "Story")
				f.SetParent(other, story)
			}
			ready := pickReady(f, "p1")
			g := pickRig(t, f)
			searches, blockedBy := pickPass(g)
			if !tt.held {
				if got := g.leaseIssues(); !slices.Equal(got, []int{ready}) || pickStage(f, ready) != "stage:verifying" {
					t.Errorf("with a task at %s, the runs leased %v, and #%d is at %s; want #%d picked, and at verifying",
						tt.stage, got, ready, pickStage(f, ready), ready)
				}
				return
			}
			if g.runs() != 0 || pickStage(f, ready) != "stage:ready" || searches != 0 || blockedBy != 0 {
				t.Errorf("with a task at %s, claude ran %d times, #%d is at %s, and the pass made %d searches and %d blocked_by reads; "+
					"want no run, the task left at ready, and neither request", tt.stage, g.runs(), ready, pickStage(f, ready), searches, blockedBy)
			}
			if searches, blockedBy := pickPass(g); searches != 0 || blockedBy != 0 {
				t.Errorf("an unchanged pass with the worker held made %d searches and %d blocked_by reads, want none", searches, blockedBy)
			}
		})
	}
}

// A task in a story isn't picked, uncounted, until #624 names its story
// branch's tip: with it the only ready task, a pass makes no search and
// writes nothing, and a task with no parent filed after it, at a lower
// priority, is picked.
func TestPickSkipsTaskInStory(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	story := fileAt(f, "planned", "A story.")
	f.SetType(story, "Story")
	inStory := pickReady(f, "p0")
	f.SetParent(inStory, story)
	g := pickRig(t, f)
	writes := len(f.Writes())
	if searches, blockedBy := pickPass(g); searches != 0 || blockedBy != 0 || g.runs() != 0 || len(f.Writes()) != writes {
		t.Errorf("with only a task in a story at ready, the pass made %d searches and %d blocked_by reads, ran claude %d times and wrote %+v; "+
			"want none of them", searches, blockedBy, g.runs(), f.Writes()[writes:])
	}
	if l := g.lease(); l.RunID != "" {
		t.Errorf("the lease = %+v; want none, with no attempt counted", l)
	}
	alone := pickReady(f, "p2")
	g.pass()
	if got := g.leaseIssues(); !slices.Equal(got, []int{alone}) || pickStage(f, inStory) != "stage:ready" {
		t.Errorf("the runs leased %v, with the story's task at %s; want #%d alone, and the story's task left at ready", got, pickStage(f, inStory), alone)
	}
}

// Only a task that gets a PR is picked: a human task at ready, which is
// the owner's to do (SPEC §9.3), is passed over with nothing written, and
// the next task is picked.
func TestPickOnlyTasks(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	human := f.AddIssue(app, "Add the secret", "Add the secret.\n\n<!-- issuedog:origin plan=#9 rev=1 item=T1 human=1 -->")
	f.SetType(human, "Task")
	f.AddLabel(human, app, "stage:ready")
	f.AddLabel(human, app, "priority:p0")
	f.AddComment(human, app, "<!-- issuedog:transition from=intake to=ready run=rready -->")
	next := pickReady(f, "p1")
	g := pickRig(t, f)
	g.pass()
	if got := g.leaseIssues(); !slices.Equal(got, []int{next}) {
		t.Errorf("the runs leased %v, want #%d alone", got, next)
	}
	if cs := implementerComments(t, f, human); pickStage(f, human) != "stage:ready" || len(cs) != 1 {
		t.Errorf("the human task is at %s, with %d comments; want it left at ready, with its move there alone", pickStage(f, human), len(cs))
	}
}

// The issue search leaves out a ready task an open issue blocks, with no
// read of its blocked_by, and the next is picked, its blocked_by read
// first (SPEC §8.1).
func TestPickSearchBlocked(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	blocked := pickReady(f, "p0")
	next := pickReady(f, "p1")
	blocker := f.AddIssue("owner", "Something first", "")
	f.SetBlockedBy(blocked, blocker)
	g := pickRig(t, f)
	if searches, blockedBy := pickPass(g); searches != 1 || blockedBy != 1 {
		t.Errorf("the pick made %d searches and %d blocked_by reads, want 1 of each: the next task's alone", searches, blockedBy)
	}
	if got := g.leaseIssues(); !slices.Equal(got, []int{next}) || pickStage(f, blocked) != "stage:ready" {
		t.Errorf("the runs leased %v, with the blocked task at %s; want #%d alone, and the blocked task left at ready", got, pickStage(f, blocked), next)
	}
}

// While the search lags behind a blocked-by link, every candidate it
// leaves out is confirmed through its blocked_by before it's picked: one
// with an open blocker is passed over for the next, so with both blocked
// none starts. One found blocked isn't read again until the next search,
// which a poll listing the blocker's close makes: then the first is
// picked (SPEC §8.1).
func TestPickConfirmsEachCandidate(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	first := pickReady(f, "p0")
	second := pickReady(f, "p1")
	blocker := f.AddIssue("owner", "Something first", "")
	f.FreezeSearch()
	f.SetBlockedBy(first, blocker)
	f.SetBlockedBy(second, blocker)
	g := pickRig(t, f)
	if searches, blockedBy := pickPass(g); searches != 1 || blockedBy != 2 || g.runs() != 0 {
		t.Errorf("the pick made %d searches and %d blocked_by reads, and claude ran %d times; want 1 search, both tasks' blocked_by read, and no run",
			searches, blockedBy, g.runs())
	}
	for _, n := range []int{first, second} {
		if got := pickStage(f, n); got != "stage:ready" {
			t.Errorf("#%d is at %s, want it left at ready", n, got)
		}
	}
	if searches, blockedBy := pickPass(g); searches != 0 || blockedBy != 0 {
		t.Errorf("an unchanged pass made %d searches and %d blocked_by reads, want none", searches, blockedBy)
	}
	f.Close(blocker)
	if searches, blockedBy := pickPass(g); searches != 1 || blockedBy != 1 {
		t.Errorf("after the blocker's close, the pass made %d searches and %d blocked_by reads, want 1 of each", searches, blockedBy)
	}
	if got := g.leaseIssues(); !slices.Equal(got, []int{first}) {
		t.Errorf("the runs leased %v, want #%d alone", got, first)
	}
}

// A ready task whose model and effort labels give the implementer no
// settings isn't picked: it gets the labels state error a run would, once,
// and stays at ready, with no run, and the next task is picked (SPEC §3.3,
// §5.3).
func TestPickBadLabels(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	bad := pickReady(f, "p0")
	f.AddLabel(bad, "owner", "model:opus")
	f.AddLabel(bad, "owner", "model:sonnet")
	g := pickRig(t, f)
	g.pass()
	want := `I can't start the implementer run, because of this issue's model and effort labels: "model:opus" and "model:sonnet" are both model:* labels.`
	if errs := labelErrors(f, bad); len(errs) != 1 || !strings.HasPrefix(errs[0], want) {
		t.Errorf("the labels errors on #%d are %q; want one, saying %q", bad, errs, want)
	}
	g.pass()
	g.pass()
	if errs := labelErrors(f, bad); len(errs) != 1 || g.runs() != 0 || pickStage(f, bad) != "stage:ready" {
		t.Errorf("two passes after left %d labels errors, with %d runs, and the task at %s; want 1, none, and it left at ready",
			len(errs), g.runs(), pickStage(f, bad))
	}
	next := pickReady(f, "p1")
	g.pass()
	if got := g.leaseIssues(); !slices.Equal(got, []int{next}) || pickStage(f, bad) != "stage:ready" {
		t.Errorf("the runs leased %v, with the task with bad labels at %s; want #%d alone, and it left at ready", got, pickStage(f, bad), next)
	}
}

// A ready task that closes after the poll listed it, so step 1 reads it
// closed, isn't picked, and nothing is written to it.
func TestPickSkipsClosedTask(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := pickReady(f, "p1")
	g := pickRig(t, f)
	f.BeforeRequest(func(r githubtest.Request) bool {
		return r.Method == http.MethodGet && r.Path == fmt.Sprintf("/issues/%d", n)
	}, func() { f.Close(n) })
	writes := len(f.Writes())
	g.pass()
	if g.runs() != 0 || slices.ContainsFunc(f.Writes()[writes:], func(w githubtest.Write) bool { return strings.HasPrefix(w.Path, fmt.Sprintf("/issues/%d/", n)) }) {
		t.Errorf("claude ran %d times, and the pass wrote %+v; want no run, and nothing written to #%d", g.runs(), f.Writes()[writes:], n)
	}
}

// The pick, then its run's move to verifying, a crash at each of their
// writes, then a restart: the replay ends with the same writes (SPEC §5.2).
func TestPickReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	roots, clones := map[*githubtest.Fake]string{}, map[*githubtest.Fake]*repo.Repo{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		pickReady(f, "p1")
		roots[f] = t.TempDir()
		return f
	}, func(f *githubtest.Fake) error {
		g := newImplementerRig(t, f, o, roots[f], clones[f], implementerOutput(t, "commits"))
		// The commit's dates are fixed, so each replay's commit, and the
		// comment naming it, is the clean run's.
		g.r.Env = append(g.r.Env, "GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z")
		if clones[f] == nil {
			clones[f] = g.d.Clone
		}
		return g.d.Iterate(context.Background())
	})
	for f, root := range roots {
		g := &rig{t: t, f: f, root: root}
		cs := implementerComments(t, f, 1)
		if g.runs() != 1 || len(cs) != 3 || cs[1].Body != pickMoved(cs[0].ID, cs[0].ID) || pickStage(f, 1) != "stage:verifying" {
			t.Errorf("after the replay, claude ran %d times, and the task, at %s, has comments %+v; want one run, and the pick's move then the run's, at verifying",
				g.runs(), pickStage(f, 1), cs)
		}
	}
}

// Quiet passes (SPEC §8.1, Polling cost): with a ready task the search
// finds blocked, an unchanged pass makes no search and no blocked_by
// read, and makes only conditional GETs answered 304. The search is made
// again once 10 minutes have passed by GitHub's clock, a 304 then, and
// once a poll lists a change, here a comment on the blocker.
func TestPickQuietPass(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	waiting := pickReady(f, "p1")
	blocker := f.AddIssue("owner", "Something first", "")
	f.SetBlockedBy(waiting, blocker)
	g := pickRig(t, f)
	if searches, blockedBy := pickPass(g); searches != 1 || blockedBy != 0 {
		t.Errorf("the start's first pass made %d searches and %d blocked_by reads, want 1 search and no read", searches, blockedBy)
	}
	g.pass() // the pass after the start's first, whose reads may still miss the ETag cache
	from := len(f.Requests())
	if searches, blockedBy := pickPass(g); searches != 0 || blockedBy != 0 {
		t.Errorf("an unchanged pass made %d searches and %d blocked_by reads, want none", searches, blockedBy)
	}
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("an unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}
	// GitHub's clock is the Date of the latest answer, to the second, moved
	// on by the local clock since, so each side of 10 minutes has a margin.
	f.Advance(10*time.Minute - 2*time.Second)
	if searches, _ := pickPass(g); searches != 0 {
		t.Errorf("a pass short of 10 minutes after the search made %d searches, want none", searches)
	}
	f.Advance(3 * time.Second)
	from = len(f.Requests())
	if searches, blockedBy := pickPass(g); searches != 1 || blockedBy != 0 {
		t.Errorf("a pass 10 minutes after the search made %d searches and %d blocked_by reads, want 1 search and no read", searches, blockedBy)
	}
	for _, r := range f.Requests()[from:] {
		if r.Path == "/search/issues" && (!r.Conditional || r.Status != http.StatusNotModified) {
			t.Errorf("the search 10 minutes on was %+v; want a conditional GET answered 304", r)
		}
	}
	pr := f.AddIssue("owner", "A pull request", "")
	f.SetPullRequest(pr)
	g.pass() // the poll lists the new pull request
	f.AddComment(pr, "owner", "Looks good.")
	if searches, _ := pickPass(g); searches != 0 {
		t.Errorf("a pass whose poll listed only a pull request's change made %d searches, want none", searches)
	}
	f.AddComment(blocker, "owner", "Nearly done.")
	if searches, _ := pickPass(g); searches != 1 {
		t.Errorf("a pass whose poll listed a change made %d searches, want 1", searches)
	}
	if g.runs() != 0 || pickStage(f, waiting) != "stage:ready" {
		t.Errorf("claude ran %d times, and the blocked task is at %s; want no run, and it left at ready", g.runs(), pickStage(f, waiting))
	}
}

// A restart's first pass makes no request it didn't make before the pick
// was built, but for the pick's own search, when a pick can happen: the
// same pass, with no Stage at in-progress, so no pick, makes the same
// requests in the same order, less that search.
func TestPickRestartRequests(t *testing.T) {
	t.Parallel()
	requests := func(pick bool) []string {
		f := newFake(t)
		waiting := pickReady(f, "p1")
		f.SetBlockedBy(waiting, f.AddIssue("owner", "Something first", ""))
		g := pickRig(t, f)
		if !pick {
			delete(g.r.Stages, state.InProgress)
		}
		g.pass()
		var sent []string
		for _, r := range f.Requests() {
			sent = append(sent, r.Method+" "+r.Path+"?"+r.Query)
		}
		return sent
	}
	with, without := requests(true), requests(false)
	i := slices.IndexFunc(with, func(r string) bool { return strings.HasPrefix(r, "GET /search/issues?") })
	if i < 0 || !slices.Equal(slices.Delete(slices.Clone(with), i, i+1), without) {
		t.Errorf("a start's first pass sent\n%s\nwant the one with no pick's,\n%s\nwith one search", strings.Join(with, "\n"), strings.Join(without, "\n"))
	}
}
