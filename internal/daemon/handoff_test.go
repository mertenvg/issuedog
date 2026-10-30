package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/issuedog/internal/workspace"
)

// commitStage is a fakeStage whose runs make commits (daemon.Committer).
// It keeps the Result.Head each Check and Apply got. At each Check it also
// keeps the run's ref in the bare repo, so a test sees the bundle was
// fetched before the result was checked.
type commitStage struct {
	*fakeStage
	t        testing.TB
	bare     string // the daemon's clone
	checkErr error  // Check's error
	checked  []string
	applied  []string
}

func (*commitStage) MakesCommits() {}

// Check keeps "<Result.Head> <the run's ref's object>", with "" for none.
func (cs *commitStage) Check(_ context.Context, r daemon.Result) error {
	ref := gitIn(cs.t, cs.bare, "for-each-ref", "--format=%(objectname)", repo.RunsRef+r.RunID)
	cs.checked = append(cs.checked, r.Head+" "+ref)
	return cs.checkErr
}

func (cs *commitStage) Apply(ctx context.Context, a *daemon.Applier, r daemon.Result) error {
	cs.applied = append(cs.applied, r.Head)
	return cs.fakeStage.Apply(ctx, a, r)
}

// newCommitRig is newRig with a commitStage at clarifying in place of the
// fakeStage, and okOut for claude to print. With clone set, the daemon's
// clone, and so the workspace's bare repo, is clone, as when the state
// volume and the bare repo outlive a crash.
func newCommitRig(t *testing.T, f *githubtest.Fake, o *origin, root string, clone *repo.Repo) (*rig, *commitStage) {
	t.Helper()
	g := newRig(t, f, o, root)
	if clone != nil {
		g.d.Clone, g.r.Workspace.Bare = clone, clone.Dir()
	}
	cs := &commitStage{fakeStage: g.stage, t: t, bare: g.d.Clone.Dir()}
	g.r.Stages[state.Clarifying] = cs
	g.claude(okOut, 0)
	return g, cs
}

// commits makes the fake claude commit in its clone, or not.
func (g *rig) commits(on bool) {
	g.t.Helper()
	path := g.path("claude", "commit")
	if !on {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			g.t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		g.t.Fatal(err)
	}
}

// trimmed is the fake claude's file name, such as commit.1, trimmed.
func (g *rig) trimmed(name string) string {
	g.t.Helper()
	return strings.TrimSpace(g.file(name))
}

// leaseOf is the lease the fake claude's run n ran under.
func (g *rig) leaseOf(n string) daemon.Lease {
	g.t.Helper()
	var l daemon.Lease
	if err := json.Unmarshal([]byte(g.file("lease."+n)), &l); err != nil {
		g.t.Fatal(err)
	}
	return l
}

// bareRefs lists the refs of g's daemon clone, with the objects they name.
func (g *rig) bareRefs() string {
	g.t.Helper()
	return gitIn(g.t, g.d.Clone.Dir(), "for-each-ref", "--format=%(refname) %(objectname)")
}

// wantRefs is what bareRefs lists for main at main and, if head is set,
// issue 1's task head at head.
func wantRefs(main, head string) string {
	want := "refs/heads/main " + main
	if head != "" {
		want += "\n" + repo.TasksRef + "1 " + head
	}
	return want
}

// A committing stage's first run starts at main, and its commit lands in
// the bare repo: fetched as the run's ref before the stage's check, then,
// once the result applies, as the task's head, with the run's ref removed.
// The next run starts from that head, and adds to it. After each run, the
// bare repo has only main and the task's head (SPEC §3.2). An unchanged
// pass after makes no request but conditional GETs answered 304.
func TestCommitterRunsHandOffTheirCommits(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	interview(f)
	g, cs := newCommitRig(t, f, newOrigin(t), t.TempDir(), nil)
	cs.moreQuestions = true // the issue stays at clarifying, for the next run
	g.commits(true)
	main := gitIn(t, g.d.Clone.Dir(), "rev-parse", "main")

	g.pass()
	first := g.trimmed("commit.1")
	if l := g.leaseOf("1"); l.Start != main || g.trimmed("head.1") != main {
		t.Errorf("the first run's lease names start %q, and its clone was at %q; want main's %s", l.Start, g.trimmed("head.1"), main)
	}
	if parent := gitIn(t, g.d.Clone.Dir(), "rev-parse", first+"^"); parent != main {
		t.Errorf("the first run's commit %s has parent %s in the bare repo, want main's %s", first, parent, main)
	}
	if got := g.bareRefs(); got != wantRefs(main, first) {
		t.Errorf("the bare repo's refs after the first run =\n%s\nwant\n%s", got, wantRefs(main, first))
	}
	if want := []string{first + " " + first}; !slices.Equal(cs.checked, want) || !slices.Equal(cs.applied, []string{first}) {
		t.Errorf("the stage checked %q and applied %q; want the head checked once it's fetched as the run's ref, %q, and applied, %q",
			cs.checked, cs.applied, want, []string{first})
	}
	if left := g.leftovers(); len(left) != 0 {
		t.Errorf("left %v after the first run; want no lease, result, bundle or clone", left)
	}

	f.AddComment(1, "owner", "more")
	g.pass()
	if g.runs() != 2 {
		t.Fatalf("claude ran %d times, want twice", g.runs())
	}
	second := g.trimmed("commit.2")
	if l := g.leaseOf("2"); l.Start != first || g.trimmed("head.2") != first {
		t.Errorf("the next run's lease names start %q, and its clone was at %q; want the task's head %s", l.Start, g.trimmed("head.2"), first)
	}
	if parent := gitIn(t, g.d.Clone.Dir(), "rev-parse", second+"^"); parent != first {
		t.Errorf("the next run's commit %s has parent %s, want the first run's %s", second, parent, first)
	}
	if got := g.bareRefs(); got != wantRefs(main, second) {
		t.Errorf("the bare repo's refs after the next run =\n%s\nwant\n%s", got, wantRefs(main, second))
	}
	if !slices.Equal(cs.applied, []string{first, second}) {
		t.Errorf("the stage applied %q, want %q", cs.applied, []string{first, second})
	}
	if left := g.leftovers(); len(left) != 0 {
		t.Errorf("left %v after the next run; want no lease, result, bundle or clone", left)
	}

	writes := len(f.Writes())
	g.pass()
	if w := f.Writes(); len(w) != writes || g.runs() != 2 {
		t.Errorf("the pass after wrote %+v, with %d runs in all; want nothing, and 2", w[writes:], g.runs())
	}
	from, notModified := len(f.Requests()), f.NotModified()
	g.pass()
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("an unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}
	if f.NotModified() == notModified {
		t.Error("an unchanged pass got no 304")
	}
	if got := g.bareRefs(); got != wantRefs(main, second) {
		t.Errorf("the bare repo's refs after the syncs of the later passes =\n%s\nwant\n%s", got, wantRefs(main, second))
	}
}

// A committing run that makes no commits applies with no head, and leaves
// the bare repo's refs as they were, so the next run starts at main again.
func TestCommitterRunWithNoCommits(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	interview(f)
	g, cs := newCommitRig(t, f, newOrigin(t), t.TempDir(), nil)
	cs.moreQuestions = true
	main := gitIn(t, g.d.Clone.Dir(), "rev-parse", "main")
	for i, n := range []string{"1", "2"} {
		if i > 0 {
			f.AddComment(1, "owner", "more")
		}
		if err := g.work(1); err != nil {
			t.Fatal(err)
		}
		if l := g.leaseOf(n); l.Start != main {
			t.Errorf("run %s's lease names start %q, want main's %s", n, l.Start, main)
		}
		if got := g.bareRefs(); got != wantRefs(main, "") {
			t.Errorf("the bare repo's refs after run %s =\n%s\nwant\n%s", n, got, wantRefs(main, ""))
		}
		if left := g.leftovers(); len(left) != 0 {
			t.Errorf("left %v after run %s; want no lease, result, bundle or clone", left, n)
		}
	}
	if want := []string{" ", " "}; !slices.Equal(cs.checked, want) || !slices.Equal(cs.applied, []string{"", ""}) {
		t.Errorf("the stage checked %q and applied %q; want no head and no run's ref, twice", cs.checked, cs.applied)
	}
}

// A fetch that fails, here at its ref's write, as a lock is in the way,
// is a failure to apply: the lease, the result and the clone stay, the
// bundle file goes, and nothing is written. The next pass bundles the
// clone again and applies the result, with no other run.
func TestCommitterFetchFailsThenApplies(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	g, cs := newCommitRig(t, f, newOrigin(t), t.TempDir(), nil)
	g.commits(true)
	main := gitIn(t, g.d.Clone.Dir(), "rev-parse", "main")
	lock := filepath.Join(g.d.Clone.Dir(), "refs", "issuedog", "runs", "run1.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	writes := len(f.Writes())
	if err := g.work(1); err == nil || !strings.Contains(err.Error(), "fetch the bundle") {
		t.Fatalf("work = %v, want the fetch's error", err)
	}
	if l := g.lease(); l.RunID != "run1" || l.ApplyFailures != 1 || l.Failed {
		t.Errorf("the lease after the failed fetch = %+v; want run1's, with 1 failure to apply", l)
	}
	for _, path := range []string{g.path("runs", "run1"), g.path("state", "runs", "run1.json")} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("after the failed fetch: %v; want the clone and the result kept", err)
		}
	}
	if _, err := os.Stat(g.path("state", "runs", "run1.bundle")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the bundle file after the failed fetch: %v; want it removed", err)
	}
	if w := f.Writes(); len(w) != writes || len(cs.checked) != 0 {
		t.Errorf("the failed fetch wrote %+v, and the stage checked %q; want nothing", w[writes:], cs.checked)
	}
	if got := g.bareRefs(); got != wantRefs(main, "") {
		t.Errorf("the bare repo's refs after the failed fetch =\n%s\nwant\n%s", got, wantRefs(main, ""))
	}

	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	head := g.trimmed("commit.1")
	if got := g.bareRefs(); got != wantRefs(main, head) || g.runs() != 1 || !specApplied(f, 1, "run1", answer) || !slices.Equal(cs.applied, []string{head}) {
		t.Errorf("after the next pass, the bare repo's refs =\n%s\nwith %d runs, and the stage applied %q; want\n%s\nwith 1 run, and %s applied",
			got, g.runs(), cs.applied, wantRefs(main, head), head)
	}
	if left := g.leftovers(); len(left) != 0 {
		t.Errorf("left %v; want no lease, result, bundle or clone", left)
	}
}

// After a restart, a saved result whose bundle wasn't fetched gets it
// from the clone, which a daemon that stopped before its apply left, and
// applies with no new run. The new daemon's clone of the origin has the
// run's start, as every sync of it does.
func TestCommitterResumeFetchesFromTheClone(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	o, root := newOrigin(t), t.TempDir()
	stopped, _ := newCommitRig(t, f, o, root, nil)
	ctx := context.Background()
	main := gitIn(t, stopped.d.Clone.Dir(), "rev-parse", "main")
	if err := stopped.r.Workspace.Prepare(ctx, "saved", workspace.Commits{Start: main}, nil); err != nil {
		t.Fatal(err)
	}
	clone := stopped.r.Workspace.Dir("saved")
	if err := os.WriteFile(filepath.Join(clone, "saved.txt"), []byte("saved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, clone, "add", "saved.txt")
	gitIn(t, clone, "commit", "--quiet", "-m", "saved")
	head := gitIn(t, clone, "rev-parse", "HEAD")
	l, err := json.Marshal(daemon.Lease{Issue: 1, Stage: state.Clarifying, RunID: "saved", Clone: clone, Seen: answer,
		AttemptKey: "clarifying-" + strconv.FormatInt(answer, 10), Attempt: 1, Start: main})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "state", "runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"lease.json": string(l), "runs/saved.json": okOut} {
		if err := os.WriteFile(filepath.Join(root, "state", name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	g, cs := newCommitRig(t, f, o, root, nil)
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	if !specApplied(f, 1, "saved", answer) || g.runs() != 0 {
		t.Errorf("after the restart, the saved result applied: %v, with %d runs; want it applied with no run", specApplied(f, 1, "saved", answer), g.runs())
	}
	if want := []string{head + " " + head}; !slices.Equal(cs.checked, want) || !slices.Equal(cs.applied, []string{head}) {
		t.Errorf("the stage checked %q and applied %q; want the clone's head fetched, %q, and applied", cs.checked, cs.applied, want)
	}
	if got := g.bareRefs(); got != wantRefs(main, head) {
		t.Errorf("the bare repo's refs after the restart =\n%s\nwant\n%s", got, wantRefs(main, head))
	}
	if left := g.leftovers(); len(left) != 0 {
		t.Errorf("left %v; want no lease, result, bundle or clone", left)
	}
}

// A run whose bundle was fetched before its apply failed isn't bundled
// again: the next pass takes the head from the run's ref, without reading
// the clone, here removed to show it. The failed apply moves no task head.
func TestCommitterFetchedRunIsNotBundledAgain(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	g, cs := newCommitRig(t, f, newOrigin(t), t.TempDir(), nil)
	g.commits(true)
	cs.applyErr = errors.New("not now")
	if err := g.work(1); err == nil {
		t.Fatal("work = nil, want the apply's error")
	}
	head := g.trimmed("commit.1")
	if got := gitIn(t, g.d.Clone.Dir(), "for-each-ref", "--format=%(objectname)", repo.RunsRef+"run1"); got != head {
		t.Fatalf("the run's ref after the failed apply = %q, want the fetched %s", got, head)
	}
	if got := gitIn(t, g.d.Clone.Dir(), "for-each-ref", "--format=%(objectname)", repo.TasksRef+"1"); got != "" {
		t.Errorf("the task's head after the failed apply = %q, want none: it moves only once the result applies", got)
	}
	if err := g.r.Workspace.Cleanup(context.Background(), "run1"); err != nil {
		t.Fatal(err)
	}
	cs.applyErr = nil
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	main := gitIn(t, g.d.Clone.Dir(), "rev-parse", "main")
	if got := g.bareRefs(); got != wantRefs(main, head) || !specApplied(f, 1, "run1", answer) || g.runs() != 1 {
		t.Errorf("the bare repo's refs =\n%s\nwith %d runs, applied: %v; want\n%s\nwith the result applied, with no other run",
			got, g.runs(), specApplied(f, 1, "run1", answer), wantRefs(main, head))
	}
}

// A run with no commits whose files a crash cut short of removing, after
// its clone went but before its result did, is only finished: its move is
// written, so with no ref of its own it made no commits, and its clone,
// which is gone, isn't read. The task's head stays where it was.
func TestCommitterMovedRunWithNoCommits(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	root := t.TempDir()
	g, cs := newCommitRig(t, f, newOrigin(t), root, nil)
	cs.moreQuestions = true
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	if len(cs.applied) != 1 || g.runs() != 1 {
		t.Fatalf("the stage applied %q after %d runs; want one run applied", cs.applied, g.runs())
	}
	writes := len(f.Writes())
	l, err := json.Marshal(daemon.Lease{Issue: 1, Stage: state.Clarifying, RunID: "run1", Clone: g.path("runs", "run1"), Seen: answer,
		AttemptKey: "clarifying-" + strconv.FormatInt(answer, 10), Attempt: 1, Start: gitIn(t, g.d.Clone.Dir(), "rev-parse", "main")})
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"lease.json": string(l), "runs/run1.json": okOut} {
		if err := os.WriteFile(filepath.Join(root, "state", name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	if w := f.Writes(); len(w) != writes || g.runs() != 1 {
		t.Errorf("finishing the run wrote %+v, with %d runs; want nothing, and no other run", w[writes:], g.runs())
	}
	main := gitIn(t, g.d.Clone.Dir(), "rev-parse", "main")
	if got := g.bareRefs(); got != wantRefs(main, "") {
		t.Errorf("the bare repo's refs =\n%s\nwant\n%s", got, wantRefs(main, ""))
	}
	if left := g.leftovers(); len(left) != 0 {
		t.Errorf("left %v; want no lease, result, bundle or clone", left)
	}
}

// A dropped result moves no task head. One dropped as the owner moved
// the issue during the run isn't fetched at all. One that fails the
// stage's check was fetched, and its run's ref goes with its other files.
func TestCommitterDroppedResultMovesNothing(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"moved", "check failed"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			interview(f)
			g, cs := newCommitRig(t, f, newOrigin(t), t.TempDir(), nil)
			g.commits(true)
			main := gitIn(t, g.d.Clone.Dir(), "rev-parse", "main")
			if name == "moved" {
				cs.during = func() { f.AddLabel(1, "owner", "stage:held") }
			} else {
				cs.checkErr = errors.New("not good enough")
			}
			packs := len(packFiles(t, g.d.Clone.Dir()))
			if err := g.work(1); err != nil {
				t.Fatal(err)
			}
			// A fetch stores the bundle's pack, which outlives the run's
			// ref, so the pack count tells whether one was fetched.
			fetched := len(packFiles(t, g.d.Clone.Dir())) - packs
			if want := map[bool]int{true: 0, false: 1}[name == "moved"]; fetched != want {
				t.Errorf("the bare repo got %d packs from the run, want %d: a dropped result isn't fetched, one that fails its check was", fetched, want)
			}
			if got := g.bareRefs(); got != wantRefs(main, "") {
				t.Errorf("the bare repo's refs =\n%s\nwant\n%s", got, wantRefs(main, ""))
			}
			if len(cs.applied) != 0 || name == "moved" && len(cs.checked) != 0 {
				t.Errorf("the stage checked %q and applied %q; want nothing applied, and for a move, nothing checked", cs.checked, cs.applied)
			}
			if head := g.trimmed("commit.1"); name != "moved" && !slices.Equal(cs.checked, []string{head + " " + head}) {
				t.Errorf("the stage checked %q, want the fetched head %s", cs.checked, head)
			}
			if left := g.leftovers(); len(left) != 0 {
				t.Errorf("left %v; want no result, bundle or clone", left)
			}
		})
	}
}

// A committing stage starts no run on an issue with a parent, as nothing
// names a story task's base yet, and that's no attempt. Step 1's read
// tells, so Work makes no request; a parent the issue gets after step 1
// shows in the read before the run, which starts none either. A stage
// that makes no commits runs on it as before.
func TestCommitterTaskWithParent(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"at step 1", "after step 1", "no commits"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			interview(f)
			story := f.AddIssue("owner", "Story", "the story")
			g, _ := newCommitRig(t, f, newOrigin(t), t.TempDir(), nil)
			if name == "no commits" {
				g.r.Stages[state.Clarifying] = g.stage
			}
			if name != "after step 1" {
				f.SetParent(1, story)
			}
			ctx := context.Background()
			s, err := g.d.Step.Client.Snapshot(ctx, "o", "r", 1)
			if err != nil {
				t.Fatal(err)
			}
			in, err := g.d.Step.Run(ctx, s)
			if err != nil {
				t.Fatal(err)
			}
			if name == "after step 1" {
				f.SetParent(1, story)
			}
			from := len(f.Requests())
			if err := g.r.Work(ctx, []daemon.Ready{{S: s, Inputs: in}}); err != nil {
				t.Fatal(err)
			}
			if name == "no commits" {
				if g.runs() != 1 {
					t.Errorf("claude ran %d times, want once: a stage that makes no commits runs on an issue with a parent", g.runs())
				}
				return
			}
			if _, err := os.Stat(g.path("state", "lease.json")); g.runs() != 0 || !errors.Is(err, os.ErrNotExist) || len(g.leftovers()) != 0 {
				t.Errorf("claude ran %d times, leaving the lease %+v and %v; want no run, no lease and nothing left", g.runs(), g.lease(), g.leftovers())
			}
			if r := f.Requests()[from:]; name == "at step 1" && len(r) != 0 {
				t.Errorf("Work sent %+v; want no request", r)
			}
		})
	}
}

// A stage that makes no commits starts from main's SHA, even on an issue
// that has a task head in the bare repo: only a Committer's runs start
// there (SPEC §3.2). Its run moves nothing.
func TestNonCommitterIgnoresTheTaskHead(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.claude(okOut, 0)
	main := gitIn(t, g.d.Clone.Dir(), "rev-parse", "main")
	head := gitIn(t, g.d.Clone.Dir(), "rev-parse", "main^")
	gitIn(t, g.d.Clone.Dir(), "update-ref", repo.TasksRef+"1", head)
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	if l := g.leaseOf("1"); g.runs() != 1 || l.Start != main || g.trimmed("head.1") != main {
		t.Errorf("claude ran %d times, the lease names start %q, and the clone was at %q; want one run from main's %s, not the task's head %s",
			g.runs(), l.Start, g.trimmed("head.1"), main, head)
	}
	if got := g.bareRefs(); got != wantRefs(main, head) {
		t.Errorf("the bare repo's refs =\n%s\nwant\n%s", got, wantRefs(main, head))
	}
}

// githubtest.Replay passes for a committing run's apply, which posts a
// follow-up after its move, whatever write a crash comes at. Each fake
// keeps its state dir and its bare repo for its passes, as both outlive a
// crash. The bundle is fetched once in each: the bare repo gets one pack
// past its sync's, and the task's head is the run's one commit.
func TestCommitterApplyReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	roots, clones, packs := map[*githubtest.Fake]string{}, map[*githubtest.Fake]*repo.Repo{}, map[*githubtest.Fake]int{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		interview(f)
		roots[f] = t.TempDir()
		return f
	}, func(f *githubtest.Fake) error {
		g, cs := newCommitRig(t, f, o, roots[f], clones[f])
		if clones[f] == nil {
			clones[f], packs[f] = g.d.Clone, len(packFiles(t, g.d.Clone.Dir()))
		}
		cs.followUp = true // a write after the move, which a replay must finish
		g.commits(true)
		return g.work(1)
	})
	for f, clone := range clones {
		g := &rig{t: t, f: f, root: roots[f], d: &daemon.Daemon{Clone: clone}}
		head := g.trimmed("commit.1")
		main := gitIn(t, clone.Dir(), "rev-parse", "main")
		if got := g.bareRefs(); got != wantRefs(main, head) || g.runs() != 1 {
			t.Errorf("the bare repo's refs after the replay =\n%s\nwith %d runs; want\n%s\nwith 1 run", got, g.runs(), wantRefs(main, head))
		}
		if got := len(packFiles(t, clone.Dir())); got != packs[f]+1 {
			t.Errorf("the bare repo has %d packs after the replay, want %d: one fetch past the sync's", got, packs[f]+1)
		}
	}
}

// packFiles lists the .pack files of the bare repo at dir.
func packFiles(t testing.TB, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "objects", "pack", "*.pack"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// The agent's git writes the bundle into a pipe the daemon copies from,
// never into the bundle file itself, which would hand agent its
// descriptor (SPEC §3.2). A git on the PATH notes whether each bundle's
// stdout is a pipe, then runs the real git. Not parallel, as it sets PATH.
func TestCommitterBundleThroughAPipe(t *testing.T) {
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin, notes := t.TempDir(), filepath.Join(t.TempDir(), "notes")
	script := "#!/bin/sh\ncase \" $* \" in\n*\" bundle create \"*)\n" +
		"\tkind=other\n\t[ -p /dev/stdout ] && kind=pipe\n\techo \"$kind\" >>" + notes + "\n\t;;\nesac\n" +
		"exec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	f := newFake(t)
	interview(f)
	g, _ := newCommitRig(t, f, newOrigin(t), t.TempDir(), nil)
	g.commits(true)
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(notes)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "pipe\n" {
		t.Errorf("the bundles' stdouts were %q, want one pipe", data)
	}
	main := gitIn(t, g.d.Clone.Dir(), "rev-parse", "main")
	if got := g.bareRefs(); got != wantRefs(main, g.trimmed("commit.1")) {
		t.Errorf("the bare repo's refs =\n%s\nwant\n%s", got, wantRefs(main, g.trimmed("commit.1")))
	}
}

// A run with no agent environment is a setup failure, so it's no attempt,
// for a Committer as for a stage that isn't one: the App's git identity
// isn't added to an empty environment, which Runner.Run refuses before
// claude starts. The run counts no attempt, writes nothing to GitHub, and
// leaves no lease or run files.
func TestCommitterNoEnvironmentIsNoAttempt(t *testing.T) {
	t.Parallel()
	for name, committer := range map[string]bool{"a stage that isn't a Committer": false, "a Committer": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			interview(f)
			g, cs := newCommitRig(t, f, newOrigin(t), t.TempDir(), nil)
			if !committer {
				g.r.Stages[state.Clarifying] = cs.fakeStage
			}
			g.d.Config.Loop.MaxAttempts = 1
			g.claude(failOut, 1)
			g.commits(true)
			g.r.Env = nil
			writes := len(f.Writes())
			if err := g.work(1); err == nil || !strings.Contains(err.Error(), "has no agent environment") {
				t.Errorf("Work = %v, want the setup error for no agent environment", err)
			}
			if w := f.Writes(); len(w) != writes {
				t.Errorf("the run wrote %+v; want nothing written to GitHub", w[writes:])
			}
			if errs, l := errorMarkers(f, 1), g.lease(); len(errs) != 0 || g.runs() != 0 || l.RunID != "" || l.Failed || l.Attempt != 0 {
				t.Errorf("errors %q, %d runs, lease %+v; want no attempt counted, and claude never started", errs, g.runs(), l)
			}
			if left := g.leftovers(); len(left) != 0 {
				t.Errorf("left %v; want no lease, result, bundle or clone", left)
			}
			if len(cs.checked) != 0 || len(cs.applied) != 0 {
				t.Errorf("the stage checked %q and applied %q; want neither", cs.checked, cs.applied)
			}
		})
	}
}
