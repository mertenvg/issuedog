package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file merges main into the open story branches in step 2 (SPEC §7.5,
// When main moves; §8.1): a story branch whose tip passed its checks gets
// main's commit, as the pass's sync left it, merged in with a merge commit
// the daemon builds in its bare repo (repo.Repo.MainMerge) and pushes to
// the branch, leased at the tip, only as a fast-forward. The new tip is
// then checked in step 4 as any push the App didn't make, and the open
// task PRs into the branch are checked again on their merge with it
// (Runs.mainMoved), as for any tip that moved. A merge that conflicts
// pushes nothing, and files one conflict bug under the story (failureFile).

// storyMainPass merges main into one open story branch, in step 2 (SPEC
// §7.5, When main moves; §8.1): of the open issues the daemon holds that
// are a story or an epic, by type, not held, by priority then number, the
// first whose branch qualifies (storyMainWant), and only it,
// so one branch a pass (storyMainMerge). An issue to be read again, as
// one a command touched or whose read or step 1 failed, waits for its
// read, so a /hold just posted holds it. What it remembers of an issue the
// daemon no longer holds is dropped. With main unchanged, or no branch
// that qualifies, it reads only memory, the snapshots and the bare repo,
// so it makes no request and pushes nothing.
func (d *Daemon) storyMainPass(ctx context.Context) error {
	held := func(n int) bool { s := d.snaps[n]; return s != nil && !s.Closed || d.stale[n] }
	defer func() {
		maps.DeleteFunc(d.storyMainLogged, func(n int, _ string) bool { return !held(n) })
		maps.DeleteFunc(d.storyMainFiled, func(n int, _ string) bool { return !held(n) })
	}()
	var stories []*github.Snapshot
	for _, n := range slices.Sorted(maps.Keys(d.snaps)) {
		s := d.snaps[n]
		if s == nil || s.Closed || d.stale[n] || stageOf(s) == state.Held {
			continue
		}
		if class, err := state.ClassOf(state.Item{Type: s.Type, HasParent: s.Parent != 0}); err != nil || class&(state.AnyStory|state.ClassEpic) == 0 {
			continue
		}
		stories = append(stories, s)
	}
	slices.SortStableFunc(stories, func(a, b *github.Snapshot) int {
		return cmp.Or(cmp.Compare(priority(a), priority(b)), cmp.Compare(a.Number, b.Number))
	})
	a := d.Step.Applier
	for _, s := range stories {
		branch, tip, main, ok, err := d.storyMainWant(ctx, s)
		if err != nil {
			return fmt.Errorf("merge main into %s/%s#%d's branch: %w", a.Owner, a.Repo, s.Number, err)
		}
		if !ok {
			continue
		}
		if err := d.storyMainMerge(ctx, s, branch, tip, main); err != nil {
			return fmt.Errorf("merge main into %s/%s#%d's branch %s: %w", a.Owner, a.Repo, s.Number, branch, err)
		}
		return nil
	}
	return nil
}

// storyMainWant reports whether s, an open story, or an epic, not held,
// gets main merged into its branch this pass, and names the branch, its
// tip and the main commit to merge (SPEC §7.5, When main moves). It does
// when:
//
//   - s has one branch with its prefix in the bare clone, as the pass's
//     sync left it (storyBranch): two get no main, nor does none. So a
//     branch with a branches state error standing gets none either, as
//     that error stands only on two or more branches, or on none (SPEC
//     §5.3);
//   - the tip's App issuedog/tip naming the branch is success, as step 4
//     judged or read it (tipSuccess), so a tip not checked yet, one that
//     failed, one the App merged a task's PR onto this pass, and after a
//     merge's push, its new tip, get no main till step 4 checks them.
//     Step 4 checks and reads only the tips of stories, and of epics
//     whose approved plan is of tasks (storyIssue), so an epic planned
//     into stories gets none;
//   - no failure on the branch is open (storyFailure). But a conflict bug
//     filed for this tip whose first move a crash cut short, which holds
//     the story as an open failure, gets its main commit merged again, so
//     its filing is found and finished (storyMainUnmoved);
//   - the tip lacks main's commit as the pass's sync left it, so a branch
//     that has it gets no redundant merge commit;
//   - and that pair isn't one whose conflict this daemon filed already
//     (Daemon.storyMainFiled), as a conflict bug someone closed leaves it.
//
// It reads memory, the snapshots and the bare repo, so it costs no
// request.
func (d *Daemon) storyMainWant(ctx context.Context, s *github.Snapshot) (branch, tip, main string, ok bool, err error) {
	a, n := d.Step.Applier, s.Number
	branch, tip, err = d.storyBranch(ctx, s)
	switch {
	case errors.Is(err, errTwoBranches):
		logr.Debugf("%s/%s#%d: main isn't merged into its branch, as it can't tell which is its branch: %v", a.Owner, a.Repo, n, err)
		return "", "", "", false, nil
	case err != nil:
		return "", "", "", false, fmt.Errorf("name its branch: %w", err)
	case tip == "":
		return "", "", "", false, nil
	}
	if !d.tipSuccess(branch, tip) {
		logr.Debugf("%s/%s#%d: main isn't merged into its branch %s yet, as its tip %s %s", a.Owner, a.Repo, n, branch, tip, d.tipWhy(branch, tip))
		return "", "", "", false, nil
	}
	if bug, open := d.storyFailure(s, branch, tip); open {
		if main, ok := d.storyMainUnmoved(bug, n, tip); ok {
			logr.Infof("%s/%s#%d: its conflict bug #%d for main's %s has no first move, so the merge is made again to finish its filing", a.Owner, a.Repo, n, bug, main)
			return branch, tip, main, true, nil
		}
		d.storyMainOnce(n, "failure:"+strconv.Itoa(bug)+"@"+tip, logr.Infof, "%s/%s#%d: main isn't merged into its branch %s, as a failure on it is open, as its bug #%d is",
			a.Owner, a.Repo, n, branch, bug)
		return "", "", "", false, nil
	}
	main, err = d.Clone.Resolve(ctx, "main")
	if err != nil {
		return "", "", "", false, fmt.Errorf("read main: %w", err)
	}
	has, err := d.Clone.IsAncestor(ctx, main, tip)
	switch {
	case err != nil:
		return "", "", "", false, fmt.Errorf("read whether its tip %s has main's %s: %w", tip, main, err)
	case has:
		return "", "", "", false, nil
	case d.storyMainFiled[n] == tip+"@"+main:
		logr.Debugf("%s/%s#%d: main's %s isn't merged into its branch %s, as its conflict with the tip %s is filed already", a.Owner, a.Repo, n, main, branch, tip)
		return "", "", "", false, nil
	}
	return branch, tip, main, true, nil
}

// storyMainUnmoved reports whether bug, an open failure's bug of story n,
// as storyFailure names it, is a conflict bug filed for tip whose first
// move a crash cut short, and returns the main commit its origin names
// (SPEC §5.2, §7.5): its snapshot, read this pass, has no move of the
// App's. Its filing found it open below the story, which holds the
// story's merges of main, so only merging that pair again finds it
// (failureFind) and writes what's left. It reads the snapshot, at no
// request.
func (d *Daemon) storyMainUnmoved(bug, n int, tip string) (string, bool) {
	s := d.snaps[bug]
	if s == nil || d.stale[bug] {
		return "", false
	}
	slug := d.Step.AppSlug
	o := failureHeldOrigin(s, slug)
	if o.Story != n || o.Tip != tip || o.Main == "" || childMoved(s, slug, failureEdited(s, slug)) {
		return "", false
	}
	return o.Main, true
}

// storyMainMerge merges main, a main commit, into story's branch, branch,
// at its tip, tip (SPEC §3.2, §7.5, When main moves). Each write is
// checked first, so a replay, a restart or a crash between any two
// repeats none and loses none (SPEC §5.2):
//
//   - the build: the merge commit, tip its first parent and main its
//     second, by the App's bot identity (appIdentity), dated as main was
//     committed, so the same pair always gives the same commit, and a
//     crash before its push builds it again;
//   - the push, with the App's installation token, leased at tip, and only
//     once the merge commit has tip in its history, so it's a fast-forward,
//     never a force. A lease gone stale, as when a person pushed since the
//     sync, pushes nothing, and is logged once (storyMainOnce): the next
//     pass's sync shows the new tip, which step 4 checks first. Once it
//     lands, the bare clone names the merge commit as the branch's tip,
//     which isn't checked yet, so no second merge comes till step 4 checks
//     it.
//
// A merge that conflicts pushes nothing, and files one conflict bug
// (storyMainConflict).
func (d *Daemon) storyMainMerge(ctx context.Context, story *github.Snapshot, branch, tip, main string) error {
	a, n := d.Step.Applier, story.Number
	name, email := appIdentity(a.AppSlug)
	merge, conflicts, err := d.Clone.MainMerge(ctx, n, branch, tip, main, name, email)
	if err != nil {
		return fmt.Errorf("build the merge of main's %s into %s: %w", main, tip, err)
	}
	if len(conflicts) > 0 {
		return d.storyMainConflict(ctx, story, branch, tip, main, conflicts)
	}
	forward, err := d.Clone.IsAncestor(ctx, tip, merge)
	switch {
	case err != nil:
		return fmt.Errorf("read whether the merge %s has the tip %s: %w", merge, tip, err)
	case !forward:
		return fmt.Errorf("the merge %s doesn't have the tip %s in its history, so it's no fast-forward and isn't pushed", merge, tip)
	}
	token, err := a.Client.Token(ctx)
	if err != nil {
		return fmt.Errorf("push the merge %s: %w", merge, err)
	}
	err = d.Clone.Push(ctx, token, merge, branch, tip)
	switch {
	case errors.Is(err, repo.ErrPushStale):
		d.storyMainOnce(n, "stale:"+tip+"@"+main, logr.Infof,
			"%s/%s#%d: the merge %s of main's %s isn't pushed to its branch %s, as the branch moved from %s since the sync, so the next pass looks again: %v",
			a.Owner, a.Repo, n, merge, main, branch, tip, err)
		return nil
	case err != nil:
		return fmt.Errorf("push the merge %s, leased at %s: %w", merge, tip, err)
	}
	logr.Infof("%s/%s#%d: merged main's %s into its branch %s at %s, as %s, so step 4 checks the new tip", a.Owner, a.Repo, n, main, branch, tip, merge)
	return nil
}

// storyMainConflict files the conflict bug for main, a main commit whose
// merge into story's branch, branch, at its tip, tip, conflicts in files
// (SPEC §7.5, A conflict): a bug under the story, its origin naming the
// story, the tip and main, at ready, its body naming main, the tip and
// each file (renderConflictBug), through failureFile, which finds one
// filed already first. Once it's open, it's a failure on the branch, so no
// more of main is merged in. The pair is then remembered
// (Daemon.storyMainFiled), so a bug someone closed with the tip and main
// unchanged costs no request. A 422 on the filing (errFailureRefused) is
// warned of once (storyMainOnce), and the pair is remembered the same way.
func (d *Daemon) storyMainConflict(ctx context.Context, story *github.Snapshot, branch, tip, main string, files []string) error {
	a, n := d.Step.Applier, story.Number
	pri := "p" + strconv.Itoa(priority(story))
	_, err := d.failureFile(ctx, failureSpec{Story: story, Branch: branch, Tip: tip, Main: main, Files: files,
		Title: conflictBugTitle(n, tip, main), Body: renderConflictBug(n, pri, branch, tip, main, files)})
	switch {
	case errors.Is(err, errFailureRefused):
		d.storyMainOnce(n, "refused:"+tip+"@"+main, logr.Warnf, "%s/%s#%d: main's %s conflicts with its branch %s at %s, in %d files, but no conflict bug is filed: %v",
			a.Owner, a.Repo, n, main, branch, tip, len(files), err)
	case err != nil:
		return fmt.Errorf("main's %s conflicts with %s: %w", main, tip, err)
	}
	if d.storyMainFiled == nil {
		d.storyMainFiled = map[int]string{}
	}
	d.storyMainFiled[n] = tip + "@" + main
	return nil
}

// storyMainOnce logs, with log, why main isn't merged into story n's
// branch, or isn't pushed there, unless the last such log for it had the
// same key, so a wait that lasts is logged once, not every pass, and a new
// cause is logged again (logOnce, Daemon.storyMainLogged).
func (d *Daemon) storyMainOnce(n int, key string, log func(string, ...any) string, format string, args ...any) {
	logOnce(&d.storyMainLogged, n, key, log, format, args...)
}
