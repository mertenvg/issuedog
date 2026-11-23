package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/issuedog/internal/verify"
	"github.com/mertenvg/issuedog/internal/workspace"
	"github.com/mertenvg/logr/v2"
)

// taskRef is task n's head in the bare repo (SPEC §3.2).
func taskRef(n int) string { return repo.TasksRef + strconv.Itoa(n) }

// runStart is what start names for a run: the commits of its clone, and
// Dropped, why the task's head wasn't named, rule 2's finding on it (SPEC
// §3.4, §10.1), for the preamble to tell the run (SPEC §12), or "" when
// it was named, or there's none. For an AfterChecks Stage's run, Against
// is what its head passed the checks against, main's commit or a story
// branch's tip (VerifyTarget), and empty for any other's. For a
// Committer's run on a task in a story, StoryTip is its story branch's
// tip, which the run names as its base (storyStart), and "" for any other
// run. Branch is the branch a Committer's run's base is on, main or the
// story branch, which the preamble names with a target or a dropped head
// (SPEC §3.4, §7.5, §12).
type runStart struct {
	workspace.Commits
	Dropped  string
	Against  VerifyTarget
	StoryTip string
	Branch   string
}

// start names the commits of st's run on issue n, whose state is is (SPEC
// §3.2, §3.4), with story, what storyStart named for it. A Committer's run
// starts from the task's head, taskRef(n), once it passes rule 2 of SPEC
// §10.1 (startFrom), or with none yet, from its base: for a task with no
// parent, main's commit as the pass's sync left it, and for a task in a
// story, its story branch's tip, story.StoryTip, as the pass's sync left
// it, which storyStart names only while its issuedog/tip is success. A
// task in a story's head is judged with the story's baseline, the merge
// base of that tip and main's commit (verify.BaseOf), and the tip as its
// target. When the task's latest move names a head its head follows, as a
// review's with the branch's tip does, the task's head moves there first,
// if it's behind (taskHeadFollow). An AfterChecks Stage's run starts from
// the head the checks judge (startAfterChecks). Any other Stage's run
// starts from main's commit. Only a Committer's run may name a target, or
// have its head dropped.
func (r *Runs) start(ctx context.Context, st Stage, is state.Issue, n int, story runStart) (runStart, error) {
	if _, ok := st.(AfterChecks); ok {
		return r.startAfterChecks(ctx, is, n)
	}
	head, hasHead := "", false
	if _, ok := st.(Committer); ok {
		// A review's move that named the branch's tip, and the task's head
		// not moved after it, as by a crash between them, is finished
		// first (SPEC §3.2).
		if err := r.Daemon.taskHeadFollow(ctx, n, is); err != nil {
			return runStart{}, fmt.Errorf("follow the task's head: %w", err)
		}
		var err error
		if head, hasHead, err = r.Daemon.Clone.ReadRef(ctx, taskRef(n)); err != nil {
			return runStart{}, fmt.Errorf("read the task's head: %w", err)
		}
	}
	main, err := r.Daemon.Clone.Resolve(ctx, "main")
	if err != nil {
		return runStart{}, fmt.Errorf("read main: %w", err)
	}
	base, baseline, branch := main, main, "main"
	if story.StoryTip != "" {
		base, branch = story.StoryTip, story.Branch
		if hasHead {
			if baseline, err = verify.BaseOf(ctx, verify.Facts{Repo: r.Daemon.Clone, Head: base, Target: main}); err != nil {
				return runStart{}, fmt.Errorf("read the story's baseline: %w", err)
			}
		}
	}
	if !hasHead {
		return runStart{Commits: workspace.Commits{Start: base}, StoryTip: story.StoryTip, Branch: branch}, nil
	}
	c, err := r.startFrom(ctx, n, head, baseline, base, branch, mainRound(is))
	if err != nil {
		return runStart{}, fmt.Errorf("name the task's head %s: %w", head, err)
	}
	c.StoryTip, c.Branch = story.StoryTip, branch
	return c, nil
}

// startAfterChecks names the commits of a run on task n, whose state is
// is, that only follows the checks: the head they judge, alone (verifyHead,
// SPEC §10.2), with the target its pass names (Runs.passed). It refuses a
// head with no pass for it and that target as the pass's sync left it,
// main's commit or the story branch's tip (Daemon.targetNow), so a run on
// a head the checks haven't passed never starts, a target that moved
// since runs them again first, and so does a restart, which empties
// passed. It reads only the bare repo, so it costs no request.
func (r *Runs) startAfterChecks(ctx context.Context, is state.Issue, n int) (runStart, error) {
	head, err := r.verifyHead(ctx, n, is)
	if err != nil {
		return runStart{}, fmt.Errorf("name the head the checks judge: %w", err)
	}
	p := r.passed[n]
	now, err := r.Daemon.targetNow(ctx, p.Target)
	if err != nil {
		return runStart{}, fmt.Errorf("name the head's target: %w", err)
	}
	if now == "" || !p.holds(head, now) {
		return runStart{}, fmt.Errorf("the head %q has no pass of the checks against %s", head, VerifyTarget{Branch: p.Target.Branch, SHA: now})
	}
	return runStart{Commits: workspace.Commits{Start: head}, Against: p.Target}, nil
}

// startFrom names the commits of a run on task n, whose head is head, with
// baseline, a main commit, as its rule 2 baseline, and target, branch's
// commit, as its target (SPEC §3.4, §10.1), and with merge, in a round
// that main's moving began at pr-open (mainRound). For a task with no
// parent, baseline and target are both main's commit, and branch is main;
// for a task in a story, target is its story branch's tip, and baseline
// the story's. It reads only the bare repo, so it costs no request.
//   - A head that fails rule 2 is never named: the run starts from target,
//     as a first attempt does, and the log says why, as Dropped does for
//     the preamble: rule 2's finding. A head with no merge base with
//     target, or more than one, fails rule 2 too (verify.ErrMergeBases,
//     branchMergeBasesDetail). The task's head isn't moved here: the new run's
//     head replaces it once its result applies (moveTaskHead).
//   - A head that passes but doesn't merge cleanly with target, by the
//     merge-tree read rule 7 judges by (repo.MergeTree), starts the run
//     with target named as its target, to merge first.
//   - So does one that merges cleanly, with merge, while target isn't in
//     its history: its merge with main failed the checks (SPEC §7.2,
//     §10.4), and the fix needs main's changes in the checkout.
//   - Otherwise the run starts from the head, with no target.
//
// The log and Dropped name target with its branch. Any other failed read
// is an error, so no run starts.
func (r *Runs) startFrom(ctx context.Context, n int, head, baseline, target, branch string, merge bool) (runStart, error) {
	f := verify.Facts{Repo: r.Daemon.Clone, Baseline: baseline, Head: head, Target: target, Config: r.Daemon.Config.Verify}
	base, err := verify.BaseOf(ctx, f)
	if errors.Is(err, verify.ErrMergeBases) {
		logr.Infof("#%d: the task's head %s fails rule 2, so the run starts from %s's %s, as a first attempt does: %v", n, head, branch, target, err)
		return runStart{Commits: workspace.Commits{Start: target}, Dropped: branchMergeBasesDetail(branch, target, err)}, nil
	}
	if err != nil {
		return runStart{}, fmt.Errorf("read the head's base: %w", err)
	}
	f.Base = base
	finding, err := verify.Rule2(ctx, f)
	if err != nil {
		return runStart{}, fmt.Errorf("check rule 2: %w", err)
	}
	if finding != nil {
		logr.Infof("#%d: the task's head %s fails rule 2, so the run starts from %s's %s, as a first attempt does.\n%s", n, head, branch, target, finding.Detail)
		return runStart{Commits: workspace.Commits{Start: target}, Dropped: finding.Detail}, nil
	}
	conflicts, err := r.Daemon.Clone.MergeTree(ctx, target, head)
	if err != nil {
		return runStart{}, fmt.Errorf("merge with %s: %w", branch, err)
	}
	if len(conflicts) > 0 {
		logr.Infof("#%d: the task's head %s doesn't merge cleanly with %s's %s, so the run starts from the head with %s named as its target, to merge first; these files conflict: %q",
			n, head, branch, target, branch, conflicts)
		return runStart{Commits: workspace.Commits{Start: head, Target: target}}, nil
	}
	if merge {
		merged, err := r.Daemon.Clone.IsAncestor(ctx, target, head)
		if err != nil {
			return runStart{}, fmt.Errorf("read %s against the head: %w", branch, err)
		}
		if !merged {
			logr.Infof("#%d: main moving sent the task back, and its head %s lacks %s's %s, so the run starts from the head with %s named as its target, to merge first",
				n, head, branch, target, branch)
			return runStart{Commits: workspace.Commits{Start: head, Target: target}}, nil
		}
	}
	return runStart{Commits: workspace.Commits{Start: head}}, nil
}

// mergeBasesDetail is rule 2's finding on a head that doesn't have exactly
// one merge base with main's commit main, as err, verify.BaseOf's, says:
// the task's own diff then has no start (SPEC §10.1).
func mergeBasesDetail(main string, err error) string {
	return branchMergeBasesDetail("main", main, err)
}

// branchMergeBasesDetail is mergeBasesDetail for a head judged against
// target, branch's commit, such as a story branch's tip.
func branchMergeBasesDetail(branch, target string, err error) string {
	return fmt.Sprintf("The head doesn't have exactly one merge base with %s's %s, so the task's own diff has no start: %v", branch, target, err)
}

// storyStart names the base of st's run on task s, for start, and reports
// whether the run may start (SPEC §3.4, §7.5). A Stage that isn't a
// Committer, and a task with no parent, have no story to name: they get
// none, and may start. A Committer's run on a task with a parent names its
// story branch's tip, as the pass's sync left it, in StoryTip, and the
// branch in Branch: the story is s's parent, among the snapshots the
// daemon holds, when it's an open story, or an open epic planned into
// tasks, in this repo (storyOf), and its branch the one with its prefix
// in the bare clone (storyBranch). It's named only while the App's
// issuedog/tip naming the branch is success on that tip, as step 4 judged
// it (tipSuccess).
//
// Otherwise the run isn't ready: no run starts, uncounted, and the log
// says why once a cause, keyed by the tip (startWaitOnce). That's a parent
// that's no such story, as for any other parent the pick passes over, or
// whose plan data doesn't read back, two branches with its story's
// prefix, no branch yet, and a tip that isn't checked yet, after a
// restart too, until step 4 reads its check, or whose check is failure,
// or that step 2 merged a task's PR onto, which isn't the tip any more
// (tipMergedOnto).
// It reads only the snapshots step 1 left, the bare repo and what step 4
// judged, so it costs no request: the parent's dirty content is kept from
// step 1 (Applier.Dirty).
func (r *Runs) storyStart(ctx context.Context, st Stage, s *github.Snapshot) (runStart, bool, error) {
	if _, ok := st.(Committer); !ok || s.Parent == 0 {
		return runStart{}, true, nil
	}
	a, d, n := r.Daemon.Step.Applier, r.Daemon, s.Number
	dirt := Dirt{}
	if p := d.snaps[s.Parent]; p != nil && s.ParentRepo == "" {
		var err error
		if dirt, err = a.Dirty(ctx, p, d.Step.Trust); err != nil {
			return runStart{}, false, fmt.Errorf("#%d's story #%d: %w", n, s.Parent, err)
		}
	}
	story, ok, err := storyOf(d.snaps, s, a.AppSlug, dirt.Has)
	switch {
	case err != nil:
		r.startWaitOnce(n, "plan:"+err.Error(), "#%d: no %s run: it can't tell whether its parent, #%d, is a story: %v", n, st.Agent(), s.Parent, err)
		return runStart{}, false, nil
	case !ok:
		r.startWaitOnce(n, "parent:"+strconv.Itoa(s.Parent), "#%d: no %s run: its parent, #%d, is no open story or epic planned into tasks in this repo, so it has no story branch to start from",
			n, st.Agent(), s.Parent)
		return runStart{}, false, nil
	}
	branch, tip, err := d.storyBranch(ctx, story)
	switch {
	case errors.Is(err, errTwoBranches):
		r.startWaitOnce(n, "branches:"+err.Error(), "#%d: no %s run: it can't tell which is its story #%d's branch: %v", n, st.Agent(), story.Number, err)
		return runStart{}, false, nil
	case err != nil:
		return runStart{}, false, fmt.Errorf("#%d's story branch: %w", n, err)
	case tip == "":
		r.startWaitOnce(n, "cut:"+branch, "#%d: no %s run: its story #%d has no branch yet", n, st.Agent(), story.Number)
		return runStart{}, false, nil
	case !d.tipSuccess(branch, tip):
		why := "has no " + github.TipCheck + " as success yet"
		if j, ok := d.tips[branch]; ok && j.Tip == tip {
			why = "has " + github.TipCheck + " as " + j.Conclusion
		}
		if d.tipMergedOnto(branch, tip) {
			why = "had a task's PR merged onto it, so it's the branch's tip no more"
		}
		r.startWaitOnce(n, "tip:"+branch+"@"+tip+":"+why, "#%d: no %s run: its story branch %s's tip %s %s, so it isn't named as the run's base",
			n, st.Agent(), branch, tip, why)
		return runStart{}, false, nil
	}
	return runStart{StoryTip: tip, Branch: branch}, true, nil
}

// startWaitOnce logs, at info, why task n's run doesn't start, unless the
// last such log for it had the same key, so a wait that lasts, as for its
// story branch's tip's check, is logged once, not every pass, and a new
// cause, or a new tip, is logged again. The repeats are logged at debug.
func (r *Runs) startWaitOnce(n int, key, format string, args ...any) {
	if r.startWaited[n] == key {
		logr.Debugf(format, args...)
		return
	}
	if r.startWaited == nil {
		r.startWaited = map[int]string{}
	}
	r.startWaited[n] = key
	logr.Infof(format, args...)
}

// bundlePath is where the handoff writes run id's bundle: next to its
// result, in a dir only the daemon can write.
func (r *Runs) bundlePath(id string) string {
	return filepath.Join(r.Runner.StateDir, "runs", id+".bundle")
}

// handoff takes the commits of l's run into the bare repo, if st is a
// Committer, and returns the run's head, or "" when it made none past its
// start (SPEC §3.2). The run's ref, repo.RunsRef+<run-id>, holds the head
// once it's fetched, so a run that has it isn't bundled again: the bundle
// is fetched once, across a restart too. With moved, the run's move is
// written, and the result began to apply only after its handoff, so with
// no ref the run made no commits, and its clone isn't read.
//
// Otherwise the workspace bundles the clone's commits past l.Start as
// agent (Workspace.Bundle), into bundlePath, and the bare repo fetches
// that file as the daemon (repo.FetchBundle). Bundle gets a writer the
// daemon copies from, never the *os.File, which exec would hand the agent's
// process as its stdout: so agent never holds the file, and nothing it
// left running can write to it once Bundle returns. On an error the file
// is removed, and the lease and the clone stay, for the next pass to try
// again (applyLease). Runs applies one lease at a time, so no two fetches
// of one run race.
func (r *Runs) handoff(ctx context.Context, l Lease, st Stage, moved bool) (string, error) {
	if _, ok := st.(Committer); !ok {
		return "", nil
	}
	head, fetched, err := r.Daemon.Clone.ReadRef(ctx, repo.RunsRef+l.RunID)
	if err != nil {
		return "", fmt.Errorf("hand off the run's commits: %w", err)
	}
	if fetched || moved {
		return head, nil
	}
	path := r.bundlePath(l.RunID)
	if head, err = BundleAndFetch(ctx, r.Workspace, r.Daemon.Clone, l.RunID, l.RunID, l.Start, path); err != nil {
		return "", errors.Join(fmt.Errorf("hand off the run's commits: %w", err), r.remove(path))
	}
	return head, nil
}

// BundleAndFetch is handoff's bundle and fetch, through the file at path,
// in a dir only the caller can write. ws bundles the commits past start in
// the clone ws.Dir(checkout) as agent, and bare fetches that file as the
// caller, under repo.RunsRef+runID. A stage run's checkout is its run ID.
// It returns the fetched head, or "" when the clone has none past start.
// As handoff says, Bundle gets a writer this copies from, never the
// *os.File. The unlisted handoff command, which sandbox/test.sh runs,
// calls it too, so the sandbox checks the daemon's own path.
func BundleAndFetch(ctx context.Context, ws *workspace.Workspace, bare *repo.Repo, checkout, runID, start, path string) (string, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("create the bundle file: %w", err)
	}
	made, err := ws.Bundle(ctx, checkout, start, struct{ io.Writer }{f})
	if cerr := f.Close(); cerr != nil {
		err = errors.Join(err, fmt.Errorf("close the bundle file: %w", cerr))
	}
	if err != nil {
		return "", fmt.Errorf("bundle the clone: %w", err)
	}
	if !made {
		return "", nil
	}
	head, err := bare.FetchBundle(ctx, path, runID, start)
	if err != nil {
		return "", fmt.Errorf("fetch the bundle: %w", err)
	}
	return head, nil
}

// moveTaskHead moves the head of l's task, taskRef, to head, the commit
// its run left, once the run's result applies: a dropped result moves
// nothing. A run that made no commits, with no head, moves nothing either.
func (r *Runs) moveTaskHead(ctx context.Context, l Lease, head string) error {
	if head == "" {
		return nil
	}
	if err := r.Daemon.Clone.UpdateRef(ctx, taskRef(l.Issue), head); err != nil {
		return fmt.Errorf("move the task's head: %w", err)
	}
	logr.Infof("#%d: the task's head is now run %s's, %s", l.Issue, l.RunID, head)
	return nil
}
