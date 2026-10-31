package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/issuedog/internal/workspace"
)

// Restarts, usage limits and failed runs of the implementer (SPEC §8.2,
// §8.3): a restart applies a valid saved result or resets the task, a usage
// limit isn't counted, and after loop.max_attempts failed runs in a row the
// task is handed off to the owner, which releases the worker.

// restartRig is newImplementerRig, on o, whose config digestNotifyOrigin
// gives, notify: [owner], with step 1 as NewStep builds it, trusting
// owner, so /done, a 👍 on a hand-off, /hold and /resume act as they do in
// the daemon.
func restartRig(t *testing.T, f *githubtest.Fake, o *origin, root string, clone *repo.Repo, out map[string]any) *rig {
	t.Helper()
	g := newImplementerRig(t, f, o, root, clone, out)
	g.d.Step = daemon.NewStep(g.d.Step.Applier, trusted{"owner": true}, func() config.Models { return g.d.Config.Models })
	// Work doesn't load the config from main, as a pass does.
	g.d.Config.Notify = []string{"owner"}
	return g
}

// restartKey is the attempt key of task n's round since its move to
// in-progress, the App's comment that implementerTask filed it with.
func restartKey(t testing.TB, f *githubtest.Fake, n int) string {
	t.Helper()
	return "in-progress-" + strconv.FormatInt(implementerComments(t, f, n)[0].ID, 10)
}

// restartLease leaves the lease of run id on task 1, attempt n of its
// round, started at start, under root, with its clone and, unless out is
// empty, its saved result, as a daemon that stopped during the run would.
func restartLease(t testing.TB, f *githubtest.Fake, root, id, start, out string, n int) {
	t.Helper()
	cs := implementerComments(t, f, 1)
	l, err := json.Marshal(daemon.Lease{Issue: 1, Stage: state.InProgress, RunID: id, Clone: filepath.Join(root, "runs", id),
		Seen: cs[len(cs)-1].ID, AttemptKey: restartKey(t, f, 1), Attempt: n, Start: start})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"state/lease.json": string(l), "runs/" + id + "/f.txt": "x"}
	if out != "" {
		files["state/runs/"+id+".json"] = out
	}
	for name, data := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// restartIssueReads counts the reads of issue n itself, a snapshot's first
// request, in the fake's requests from from on.
func restartIssueReads(f *githubtest.Fake, from, n int) int {
	reads := 0
	for _, r := range f.Requests()[from:] {
		if r.Method == http.MethodGet && r.Path == "/issues/"+strconv.Itoa(n) {
			reads++
		}
	}
	return reads
}

// The lease of an implementer run records its named start (SPEC §8.2): the
// first run's is main's SHA, as the task has no head. Its hand-off comes
// with a commit, which becomes the task's head (SPEC §3.2). The owner's
// /done sends the task back to ready, and the pick takes it again in the
// same pass, as the worker is free: the next run's lease names the task's
// head as its start, and its commit is made on it.
func TestImplementerRestartLeaseStart(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := implementerTask(f)
	g := restartRig(t, f, digestNotifyOrigin(t), t.TempDir(), nil, implementerOutput(t, "handoff"))
	main := gitIn(t, g.d.Clone.Dir(), "rev-parse", "main")

	g.pass()
	head := g.trimmed("commit.1")
	if l := g.leaseOf("1"); l.Start != main || pickStage(f, n) != "stage:needs-human" {
		t.Fatalf("run 1's lease names start %q, and the task is at %s; want main's %s, and needs-human", l.Start, pickStage(f, n), main)
	}
	if got := g.bareRefs(); got != wantRefs(main, head) {
		t.Fatalf("the bare repo's refs after run 1 =\n%s\nwant its commit as the task's head:\n%s", got, wantRefs(main, head))
	}

	g.claude(ivResult(t, implementerOutput(t, "commits")), 0)
	f.AddComment(n, "owner", "/done")
	g.pass()
	if g.runs() != 2 {
		t.Fatalf("claude ran %d times once /done sent the task back to ready; want twice, the pick starting run 2 in the same pass", g.runs())
	}
	if l := g.leaseOf("2"); l.Start != head || l.Attempt != 1 {
		t.Errorf("run 2's lease names start %q, attempt %d; want the task's head %s, attempt 1 of a new round", l.Start, l.Attempt, head)
	}
	next := g.trimmed("commit.2")
	if parent := gitIn(t, g.d.Clone.Dir(), "rev-parse", next+"^"); parent != head {
		t.Errorf("run 2's commit %s has parent %s; want the task's head %s", next, parent, head)
	}
	if got := g.bareRefs(); got != wantRefs(main, next) || pickStage(f, n) != "stage:verifying" {
		t.Errorf("the bare repo's refs after run 2 =\n%s\nwith the task at %s; want\n%s\nat verifying", got, pickStage(f, n), wantRefs(main, next))
	}
}

// A restart with a valid saved result whose bundle wasn't fetched yet
// fetches it from the clone the stopped daemon left, and applies it once,
// with no run: the task moves to verifying with the clone's commit as its
// head (SPEC §8.2). The same pass gives the task no stage work, as its
// result was just applied: Work reads the task once, for the apply, and
// never again for a run its step 1 snapshot, still at in-progress, would
// start.
func TestImplementerRestartFetchesSavedResult(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := implementerTask(f)
	o, root := digestNotifyOrigin(t), t.TempDir()
	stopped := newImplementerRig(t, f, o, root, nil, implementerOutput(t, "commits"))
	main := gitIn(t, stopped.d.Clone.Dir(), "rev-parse", "main")
	if err := stopped.r.Workspace.Prepare(context.Background(), "saved", workspace.Commits{Start: main}, nil); err != nil {
		t.Fatal(err)
	}
	clone := stopped.r.Workspace.Dir("saved")
	if err := os.WriteFile(filepath.Join(clone, "saved.txt"), []byte("saved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, clone, "add", "saved.txt")
	gitIn(t, clone, "-c", "user.name=issuedog[bot]", "-c", "user.email=issuedog[bot]@users.noreply.github.com", "commit", "--quiet", "-m", "saved")
	head := gitIn(t, clone, "rev-parse", "HEAD")
	restartLease(t, f, root, "saved", main, ivResult(t, implementerOutput(t, "commits")), 1)

	g := restartRig(t, f, o, root, nil, implementerOutput(t, "commits"))
	ready := g.ready(n)
	from := len(f.Requests())
	if err := g.r.Work(context.Background(), ready); err != nil {
		t.Fatal(err)
	}
	if reads := restartIssueReads(f, from, n); reads != 1 || g.runs() != 0 {
		t.Errorf("the restart's Work read #%d %d times, with %d runs; want once, for the apply, and no run", n, reads, g.runs())
	}
	cs := implementerComments(t, f, n)
	if want := implementerCommitted("saved", head, 1, cs[0].ID); len(cs) != 2 || cs[1].Body != want || pickStage(f, n) != "stage:verifying" {
		t.Errorf("after the restart, %d comments, the newest\n%s\nat %s; want the saved result's move,\n%s\nat verifying", len(cs), cs[len(cs)-1].Body, pickStage(f, n), want)
	}
	if got := g.bareRefs(); got != wantRefs(main, head) {
		t.Errorf("the bare repo's refs after the restart =\n%s\nwant the clone's commit as the task's head:\n%s", got, wantRefs(main, head))
	}
	if left := g.leftovers(); len(left) != 0 {
		t.Errorf("left %v; want no lease, result, bundle or clone", left)
	}
	writes := len(f.Writes())
	g.pass()
	if w := f.Writes(); len(w) != writes || g.runs() != 0 {
		t.Errorf("the pass after wrote %+v, with %d runs; want nothing", w[writes:], g.runs())
	}
}

// A restart with a valid saved result whose bundle was fetched before its
// apply failed applies it once from the run's ref in the bare repo, with
// no other run and no bundle made again: its clone, removed here to show
// it, isn't read.
func TestImplementerRestartAppliesFetchedResult(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := implementerTask(f)
	o, root := digestNotifyOrigin(t), t.TempDir()
	stopped := restartRig(t, f, o, root, nil, implementerOutput(t, "commits"))
	f.FailWrite(len(f.Writes()) + 1) // the move's comment
	if err := stopped.work(n); err == nil {
		t.Fatal("the run's apply wrote its move; want it to fail")
	}
	f.FailWrite(0)
	head := stopped.trimmed("commit.1")
	if got := gitIn(t, stopped.d.Clone.Dir(), "for-each-ref", "--format=%(objectname)", repo.RunsRef+"run1"); got != head {
		t.Fatalf("the run's ref after the failed apply = %q, want the fetched %s", got, head)
	}
	if err := stopped.r.Workspace.Cleanup(context.Background(), "run1"); err != nil {
		t.Fatal(err)
	}

	g := restartRig(t, f, o, root, stopped.d.Clone, implementerOutput(t, "commits"))
	if err := g.r.Work(context.Background(), g.ready(n)); err != nil {
		t.Fatal(err)
	}
	main := gitIn(t, g.d.Clone.Dir(), "rev-parse", "main")
	cs := implementerComments(t, f, n)
	if want := implementerCommitted("run1", head, 1, cs[0].ID); len(cs) != 2 || cs[1].Body != want || g.runs() != 1 {
		t.Errorf("after the restart, %d comments, the newest\n%s\nafter %d runs; want run 1's move,\n%s\nafter 1 run", len(cs), cs[len(cs)-1].Body, g.runs(), want)
	}
	if got := g.bareRefs(); got != wantRefs(main, head) || pickStage(f, n) != "stage:verifying" {
		t.Errorf("the bare repo's refs =\n%s\nwith the task at %s; want\n%s\nat verifying", got, pickStage(f, n), wantRefs(main, head))
	}
}

// A restart without a valid result counts the run as a failed attempt and
// deletes its clone (SPEC §8.2). The task stays at in-progress, as the
// table has no move back to ready for it, so the same pass runs it again,
// as attempt 2 of its round, from the same commit: the task's head, here
// one in the bare repo already.
func TestImplementerRestartWithoutResult(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := implementerTask(f)
	o, root := digestNotifyOrigin(t), t.TempDir()
	stopped := newImplementerRig(t, f, o, root, nil, implementerOutput(t, "commits"))
	bare := stopped.d.Clone.Dir()
	head := gitIn(t, bare, "commit-tree", "-m", "an earlier run", "-p", "main", "main^{tree}")
	gitIn(t, bare, "update-ref", repo.TasksRef+"1", head)
	restartLease(t, f, root, "saved", head, "", 1)

	g := restartRig(t, f, o, root, stopped.d.Clone, implementerOutput(t, "commits"))
	g.claude(failOut, 1)
	writes := len(f.Writes())
	if err := g.r.Work(context.Background(), g.ready(n)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(g.path("runs", "saved")); !os.IsNotExist(err) {
		t.Errorf("the saved run's clone: %v; want it deleted", err)
	}
	if l := g.leaseOf("1"); g.runs() != 1 || l.Start != head || l.Attempt != 2 || l.AttemptKey != restartKey(t, f, n) {
		t.Errorf("%d runs after the restart, the next leased %+v; want one, attempt 2 of round %s, from the task's head %s",
			g.runs(), l, restartKey(t, f, n), head)
	}
	if w := f.Writes(); len(w) != writes || pickStage(f, n) != "stage:in-progress" {
		t.Errorf("the restart wrote %+v, and left the task at %s; want nothing, and in-progress", w[writes:], pickStage(f, n))
	}
	if l := g.lease(); !l.Failed || l.Attempt != 2 {
		t.Errorf("the lease after run 1 failed = %+v; want 2 failed attempts in a row", l)
	}
	if got := gitIn(t, bare, "for-each-ref", "--format=%(objectname)", repo.TasksRef+"1"); got != head {
		t.Errorf("the task's head = %q, want it still %s", got, head)
	}
}

// A usage limit isn't counted, and the task waits at in-progress for the
// gate (SPEC §8.3): a run that hits it, or the saved result of one a
// restart finds, closes the gate, with nothing written and no attempt
// counted. No run starts until the gate opens, and then the next one is
// attempt 1 of the round.
func TestImplementerUsageLimit(t *testing.T) {
	t.Parallel()
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart %t", restart), func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			n := implementerTask(f)
			root := t.TempDir()
			g := restartRig(t, f, digestNotifyOrigin(t), root, nil, implementerOutput(t, "commits"))
			reset := g.now.Add(time.Hour)
			if restart {
				restartLease(t, f, root, "saved", gitIn(t, g.d.Clone.Dir(), "rev-parse", "main"), limitOut("error_during_execution", reset), 1)
			} else {
				g.claude(limitOut("error_during_execution", reset), 1)
			}
			writes := len(f.Writes())
			g.pass()
			if want := map[bool]int{false: 1, true: 0}[restart]; g.runs() != want || g.r.Gate.Open(g.now) {
				t.Fatalf("%d runs, the gate open %t; want %d, and the gate closed", g.runs(), g.r.Gate.Open(g.now), want)
			}
			if l := g.lease(); l.RunID != "" || len(g.leftovers()) != 0 {
				t.Errorf("the lease %+v, left %v; want none, as no attempt is counted", l, g.leftovers())
			}
			g.claude(ivResult(t, implementerOutput(t, "commits")), 0)
			runs := g.runs()
			g.now = reset.Add(-time.Minute)
			g.pass()
			if w := f.Writes(); len(w) != writes || g.runs() != runs || pickStage(f, n) != "stage:in-progress" {
				t.Errorf("before the gate opens: writes %+v, %d runs, the task at %s; want nothing, no run, and in-progress",
					w[writes:], g.runs()-runs, pickStage(f, n))
			}
			g.now = reset
			g.pass()
			if l := g.leaseOf(strconv.Itoa(runs + 1)); g.runs() != runs+1 || l.Attempt != 1 || pickStage(f, n) != "stage:verifying" {
				t.Errorf("once the gate opens, %d runs, the next leased %+v, the task at %s; want one, attempt 1, at verifying",
					g.runs()-runs, l, pickStage(f, n))
			}
		})
	}
}

// restartCapRig is restartRig on o and root, with claude failing every
// run, as failOut, and committing nothing.
func restartCapRig(t *testing.T, f *githubtest.Fake, o *origin, root string) *rig {
	t.Helper()
	g := restartRig(t, f, o, root, nil, implementerOutput(t, "commits"))
	g.claude(failOut, 1)
	g.commits(false)
	return g
}

// restartCapComment is the attempt cap's hand-off on task 1, for its round
// key, after 3 failed runs, the last as failOut fails, with the cc line for
// owner, and seen, the newest comment when the last run started.
func restartCapComment(key string, seen int64) string {
	return "This task's 3 attempts in this round failed, so I've stopped running the implementer here.\n\n" +
		"The last attempt, a run, failed:\n\n" +
		"```text\nrun failed with subtype \"error_max_turns\"\n```\n\n" +
		"**Goal:** Find out why the implementer's runs on this task fail, and fix that, so it can go on with the task.\n" +
		"**Why it blocks:** #1 can't proceed until what made its implementer runs fail is fixed\n" +
		"**Steps:**\n" +
		"1. Read why the last run failed, above, or in the daemon's log.\n" +
		"2. Fix what made it fail, such as an unclear criterion in this task's body, a failure on `main`, or a setting in `.issuedog/config.yaml`.\n\n" +
		"**Links:**\nNone.\n\n" +
		"**Done when:** What made the implementer's runs fail is fixed.\n" +
		"**Estimated time:** It depends on why the runs failed.\n\n" +
		"When it's done, reply `/done` on this issue or react 👍 to this comment, and the task goes back to `ready`.\n\n" +
		"cc @owner\n\n" +
		"<!-- issuedog:transition from=in-progress to=needs-human event=a" + key + " seen=" + strconv.FormatInt(seen, 10) + " -->"
}

// After loop.max_attempts failed implementer runs in a row, the task gets
// a hand-off at needs-human, keyed by its round, that names why the last
// run failed, as the attempt cap's state error would, and no kind=attempts
// state error (SPEC §8.3, §9.3). That releases the worker, so the next
// pass picks the next ready task. The task at needs-human gets no run.
func TestImplementerAttemptCapHandoff(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := implementerTask(f)
	next := pickReady(f, "p2")
	g := restartCapRig(t, f, digestNotifyOrigin(t), t.TempDir())
	key := restartKey(t, f, n)

	for i := 1; i <= 3; i++ {
		g.pass()
		if l := g.lease(); g.runs() != i || !l.Failed || l.Attempt != i || l.AttemptKey != key {
			t.Fatalf("after pass %d: %d runs, lease %+v; want %d, attempt %d of round %s failed", i, g.runs(), l, i, i, key)
		}
		if i < 3 && pickStage(f, n) != "stage:in-progress" {
			t.Fatalf("after %d failed runs, the task is at %s; want in-progress", i, pickStage(f, n))
		}
	}
	cs := implementerComments(t, f, n)
	last := cs[len(cs)-1]
	if want := restartCapComment(key, cs[len(cs)-2].ID); last.Author.Login != app || last.Body != want {
		t.Errorf("the task's newest comment, by %s, =\n%s\nwant the App's\n%s", last.Author.Login, last.Body, want)
	}
	if pickStage(f, n) != "stage:needs-human" || len(errorMarkers(f, n)) != 0 {
		t.Errorf("after the cap, the task is at %s, with errors %q; want needs-human, and none", pickStage(f, n), errorMarkers(f, n))
	}
	if got := pickStage(f, next); got != "stage:ready" {
		t.Errorf("#%d is at %s in the cap's pass; want ready, as the pass ended with the failed run", next, got)
	}

	g.pass()
	if got := g.leaseIssues(); !slices.Equal(got, []int{n, n, n, next}) || pickStage(f, next) != "stage:in-progress" {
		t.Errorf("the runs leased %v, with #%d at %s; want #%d three times, then #%d picked", got, next, pickStage(f, next), n, next)
	}
	if got := pickStage(f, n); got != "stage:needs-human" {
		t.Errorf("the capped task is at %s; want needs-human still", got)
	}
}

// The cap's hand-off ends as any hand-off does (SPEC §9.3): a trusted
// /done, or a trusted 👍 on the hand-off comment, sends the task back to
// ready, and the pick takes it again in the same pass, as the worker is
// free. Its run is attempt 1 of a new round.
func TestImplementerAttemptCapHandoffEnds(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"/done", "👍"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			n := implementerTask(f)
			g := restartCapRig(t, f, digestNotifyOrigin(t), t.TempDir())
			key := restartKey(t, f, n)
			for range 3 {
				g.pass()
			}
			cs := implementerComments(t, f, n)
			if pickStage(f, n) != "stage:needs-human" {
				t.Fatalf("after 3 failed runs, the task is at %s; want needs-human", pickStage(f, n))
			}
			if how == "/done" {
				f.AddComment(n, "owner", "/done")
			} else {
				f.AddReaction(cs[len(cs)-1].ID, "owner", "+1")
			}
			g.pass()
			if l := g.leaseOf("4"); g.runs() != 4 || l.Issue != n || l.Attempt != 1 || l.AttemptKey == key {
				t.Fatalf("after the %s, %d runs, the 4th leased %+v; want run 4 on #%d, attempt 1 of a new round, not %s", how, g.runs(), l, n, key)
			}
			var moves []string
			for _, c := range implementerComments(t, f, n)[len(cs):] {
				if i := strings.Index(c.Body, "<!-- issuedog:transition from="); i >= 0 {
					moves = append(moves, strings.Join(strings.Fields(c.Body[i:])[2:4], " "))
				}
			}
			if want := []string{"from=needs-human to=ready", "from=ready to=in-progress"}; !slices.Equal(moves, want) {
				t.Errorf("after the %s, the moves are %q; want %q", how, moves, want)
			}
		})
	}
}

// restartEnded leaves under root the lease of run id on task 1, ended with
// n failed attempts in its round, as end writes it.
func restartEnded(t testing.TB, f *githubtest.Fake, root, id string, n int) {
	t.Helper()
	l, err := json.Marshal(daemon.Lease{Issue: 1, Stage: state.InProgress, RunID: id, Clone: filepath.Join(root, "runs", id),
		AttemptKey: restartKey(t, f, 1), Attempt: n, Failed: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "state", "lease.json"), l, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The cap's hand-off, a crash at each of its writes, then a restart: the
// replay ends with the same writes, the hand-off posted once, and the task
// at needs-human, with no other run (SPEC §5.2). The restart's count of
// failed attempts is the ended lease's, so it makes the hand-off in place
// of a run, as one whose post failed.
func TestImplementerAttemptCapReplay(t *testing.T) {
	t.Parallel()
	o := digestNotifyOrigin(t)
	roots, rigs := map[*githubtest.Fake]string{}, map[*githubtest.Fake]*rig{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		implementerTask(f)
		roots[f] = t.TempDir()
		restartEnded(tb, f, roots[f], "ended", 2)
		return f
	}, func(f *githubtest.Fake) error {
		g := restartCapRig(t, f, o, roots[f])
		rigs[f] = g
		return g.work(1)
	})
	for f, g := range rigs {
		cs := implementerComments(t, f, 1)
		if want := restartCapComment(restartKey(t, f, 1), cs[0].ID); len(cs) != 2 || cs[1].Body != want || pickStage(f, 1) != "stage:needs-human" || g.runs() != 1 {
			t.Errorf("after the replay, %d comments, the newest\n%s\nat %s, with %d runs; want 2, the newest\n%s\nat needs-human, with 1 run",
				len(cs), cs[len(cs)-1].Body, pickStage(f, 1), g.runs(), want)
		}
	}
}

// duringImplementer is the implementer, but its Context calls during
// first, if set, which happens after the pre-run read: during the run.
type duringImplementer struct {
	*daemon.Implementer
	during func()
}

func (d duringImplementer) Context(ctx context.Context, in daemon.Input) (map[string][]byte, error) {
	if d.during != nil {
		d.during()
	}
	files, err := d.Implementer.Context(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("the implementer's context: %w", err)
	}
	return files, nil
}

// A task the owner moved during the last run allowed gets no hand-off when
// the run fails, as reportCap posts no state error on an issue that moved
// on: the owner's move wins, and is recorded, and the task waits at ready.
func TestImplementerAttemptCapMovedOn(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := implementerTask(f)
	g := restartCapRig(t, f, digestNotifyOrigin(t), t.TempDir())
	im := duringImplementer{Implementer: &daemon.Implementer{Daemon: g.d}}
	g.r.Stages[state.InProgress] = im
	for range 2 {
		g.pass()
	}
	im.during = func() {
		f.RemoveLabel(n, "owner", "stage:in-progress")
		f.AddLabel(n, "owner", "stage:ready")
	}
	g.r.Stages[state.InProgress] = im
	if err := g.work(n); err != nil {
		t.Fatal(err)
	}
	cs := implementerComments(t, f, n)
	last := cs[len(cs)-1]
	if l := g.lease(); g.runs() != 3 || !l.Failed || l.Attempt != 3 || !strings.Contains(last.Body, "from=in-progress to=ready by=owner") {
		t.Errorf("after %d runs, the lease %+v, the newest comment\n%s\nwant 3 failed, and the owner's move recorded", g.runs(), l, last.Body)
	}
	if pickStage(f, n) != "stage:ready" || len(errorMarkers(f, n)) != 0 || strings.Contains(last.Body, "to=needs-human") {
		t.Errorf("the task is at %s, with errors %q; want ready, no error, and no hand-off", pickStage(f, n), errorMarkers(f, n))
	}
}

// The cap's hand-off is for a task only: an issue at in-progress that the
// table moves to needs-human on attempt-cap no more, here a human task the
// App filed, whose ended lease holds loop.max_attempts failed runs, as
// after a retype, gets the kind=attempts state error instead, on the
// restart's first pass, in place of a run, and no move.
func TestImplementerAttemptCapNotATask(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := f.AddIssue(app, "Add the secret", "Add the secret.\n\n<!-- issuedog:origin plan=#9 rev=1 item=T1 human=1 -->")
	f.SetType(n, "Task")
	f.AddLabel(n, app, "stage:in-progress")
	f.AddComment(n, app, "<!-- issuedog:transition from=intake to=in-progress run=rin-progress -->")
	root := t.TempDir()
	restartEnded(t, f, root, "ended", 3)
	g := restartCapRig(t, f, digestNotifyOrigin(t), root)
	if err := g.work(n); err != nil {
		t.Fatal(err)
	}
	want := []string{"<!-- issuedog:error kind=attempts key=" + restartKey(t, f, n) + " -->"}
	if got := errorMarkers(f, n); !slices.Equal(got, want) || pickStage(f, n) != "stage:in-progress" || g.runs() != 0 {
		t.Errorf("errors %q, the issue at %s, %d runs; want %q, at in-progress, and no run", got, pickStage(f, n), g.runs(), want)
	}
}

// A task held while at in-progress goes to ready when /resume ends the
// hold, as its run was dropped (SPEC §6.4), not back to in-progress: the
// pick takes it again in the same pass, as the worker is free, and its
// run's commits move it to verifying.
func TestImplementerResumeHeldTask(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := implementerTask(f)
	g := restartRig(t, f, digestNotifyOrigin(t), t.TempDir(), nil, implementerOutput(t, "commits"))
	f.AddComment(n, "owner", "/hold")
	g.pass()
	if g.runs() != 0 || pickStage(f, n) != "stage:held" {
		t.Fatalf("after /hold, %d runs, the task at %s; want none, and held", g.runs(), pickStage(f, n))
	}
	resume := f.AddComment(n, "owner", "/resume")
	g.pass()
	cs := implementerComments(t, f, n)
	at := slices.IndexFunc(cs, func(c github.Comment) bool { return c.ID == resume })
	var bodies []string
	for _, c := range cs[at+1:] {
		bodies = append(bodies, c.Body)
	}
	// The move to ready carries the round's bound from before the hold,
	// the move to in-progress. The pick is keyed by the move to ready's
	// comment, the newest then, so its seen is that comment too.
	back := moved(resumedToReady, fmt.Sprintf("from=held to=ready cmd=%d seen=%d", resume, cs[0].ID))
	if len(bodies) != 3 || bodies[0] != back || bodies[1] != pickMoved(cs[at+1].ID, cs[at+1].ID) ||
		bodies[2] != implementerCommitted("run1", g.trimmed("commit.1"), 1, cs[at+2].ID) {
		t.Errorf("after /resume, the comments are\n%q\nwant its move to ready,\n%q\nthen the pick's, and run 1's to verifying", bodies, back)
	}
	if g.runs() != 1 || pickStage(f, n) != "stage:verifying" {
		t.Errorf("after /resume, %d runs, the task at %s; want one, and verifying", g.runs(), pickStage(f, n))
	}
}
