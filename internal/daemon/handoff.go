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
	"github.com/mertenvg/issuedog/internal/workspace"
	"github.com/mertenvg/logr/v2"
)

// taskRef is task n's head in the bare repo (SPEC §3.2).
func taskRef(n int) string { return repo.TasksRef + strconv.Itoa(n) }

// start names the start of st's run on issue n (SPEC §3.2, §3.4). A
// Committer's run starts from the task's head, taskRef(n), or with none
// yet, from its base: main's commit as the pass's sync left it, which is a
// task with no parent's (inStory). Any other Stage's run starts from
// main's commit.
func (r *Runs) start(ctx context.Context, st Stage, n int) (string, error) {
	if _, ok := st.(Committer); ok {
		head, ok, err := r.Daemon.Clone.ReadRef(ctx, taskRef(n))
		if err != nil {
			return "", fmt.Errorf("read the task's head: %w", err)
		}
		if ok {
			return head, nil
		}
	}
	main, err := r.Daemon.Clone.Resolve(ctx, "main")
	if err != nil {
		return "", fmt.Errorf("read main: %w", err)
	}
	return main, nil
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
