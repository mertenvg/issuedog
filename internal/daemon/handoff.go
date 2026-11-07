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
	"github.com/mertenvg/issuedog/internal/verify"
	"github.com/mertenvg/issuedog/internal/workspace"
	"github.com/mertenvg/logr/v2"
)

// taskRef is task n's head in the bare repo (SPEC §3.2).
func taskRef(n int) string { return repo.TasksRef + strconv.Itoa(n) }

// runStart is what start names for a run: the commits of its clone, and
// Dropped, why the task's head wasn't named, rule 2's finding on it (SPEC
// §3.4, §10.1), for the preamble to tell the run (SPEC §12), or "" when
// it was named, or there's none.
type runStart struct {
	workspace.Commits
	Dropped string
}

// start names the commits of st's run on issue n (SPEC §3.2, §3.4). A
// Committer's run starts from the task's head, taskRef(n), once it passes
// rule 2 of SPEC §10.1 (startFrom), or with none yet, from its base:
// main's commit as the pass's sync left it, which is a task with no
// parent's (inStory). Any other Stage's run starts from main's commit.
// Only a Committer's run may name a target, or have its head dropped.
func (r *Runs) start(ctx context.Context, st Stage, n int) (runStart, error) {
	head, hasHead := "", false
	if _, ok := st.(Committer); ok {
		var err error
		if head, hasHead, err = r.Daemon.Clone.ReadRef(ctx, taskRef(n)); err != nil {
			return runStart{}, fmt.Errorf("read the task's head: %w", err)
		}
	}
	main, err := r.Daemon.Clone.Resolve(ctx, "main")
	if err != nil {
		return runStart{}, fmt.Errorf("read main: %w", err)
	}
	if !hasHead {
		return runStart{Commits: workspace.Commits{Start: main}}, nil
	}
	c, err := r.startFrom(ctx, n, head, main)
	if err != nil {
		return runStart{}, fmt.Errorf("name the task's head %s: %w", head, err)
	}
	return c, nil
}

// startFrom names the commits of a run on task n, whose head is head, with
// main's commit main as its rule 2 baseline and its target (SPEC §3.4,
// §10.1). It reads only the bare repo, so it costs no request.
//   - A head that fails rule 2 is never named: the run starts from main,
//     as a first attempt does, and the log says why, as Dropped does for
//     the preamble: rule 2's finding. A head with no merge base with main,
//     or more than one, fails rule 2 too (verify.ErrMergeBases,
//     mergeBasesDetail). The task's head isn't moved here: the new run's
//     head replaces it once its result applies (moveTaskHead).
//   - A head that passes but doesn't merge cleanly with main, by the
//     merge-tree read rule 7 judges by (repo.MergeTree), starts the run
//     with main named as its target, to merge first.
//   - Otherwise the run starts from the head, with no target.
//
// Any other failed read is an error, so no run starts.
func (r *Runs) startFrom(ctx context.Context, n int, head, main string) (runStart, error) {
	f := verify.Facts{Repo: r.Daemon.Clone, Baseline: main, Head: head, Target: main, Config: r.Daemon.Config.Verify}
	base, err := verify.BaseOf(ctx, f)
	if errors.Is(err, verify.ErrMergeBases) {
		logr.Infof("#%d: the task's head %s fails rule 2, so the run starts from main's %s, as a first attempt does: %v", n, head, main, err)
		return runStart{Commits: workspace.Commits{Start: main}, Dropped: mergeBasesDetail(main, err)}, nil
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
		logr.Infof("#%d: the task's head %s fails rule 2, so the run starts from main's %s, as a first attempt does.\n%s", n, head, main, finding.Detail)
		return runStart{Commits: workspace.Commits{Start: main}, Dropped: finding.Detail}, nil
	}
	conflicts, err := r.Daemon.Clone.MergeTree(ctx, main, head)
	if err != nil {
		return runStart{}, fmt.Errorf("merge with main: %w", err)
	}
	if len(conflicts) > 0 {
		logr.Infof("#%d: the task's head %s doesn't merge cleanly with main's %s, so the run starts from the head with main named as its target, to merge first; these files conflict: %q",
			n, head, main, conflicts)
		return runStart{Commits: workspace.Commits{Start: head, Target: main}}, nil
	}
	return runStart{Commits: workspace.Commits{Start: head}}, nil
}

// mergeBasesDetail is rule 2's finding on a head that doesn't have exactly
// one merge base with main's commit main, as err, verify.BaseOf's, says:
// the task's own diff then has no start (SPEC §10.1).
func mergeBasesDetail(main string, err error) string {
	return fmt.Sprintf("The head doesn't have exactly one merge base with main's %s, so the task's own diff has no start: %v", main, err)
}

// inStory reports whether st is a Committer and s has a parent. Such a
// task's base is its story branch's tip, which nothing names yet (#624),
// so it gets no start: no run starts for it, and that isn't counted.
// Step 1's snapshot tells, so the skip costs no request.
func inStory(st Stage, s *github.Snapshot) bool {
	if _, ok := st.(Committer); !ok || s.Parent == 0 {
		return false
	}
	logr.Debugf("#%d: no %s run: it has a parent, #%d, and a task in a story gets no start until its story branch's tip is named",
		s.Number, st.Agent(), s.Parent)
	return true
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
