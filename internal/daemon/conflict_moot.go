package daemon

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file closes a conflict bug made moot (SPEC §7.5, A conflict merged
// by hand): once its story branch's tip holds the main commit the bug
// names, other than through the bug's own head, as when someone merged
// that commit, or a newer main, into the branch by hand, there's nothing
// left for the bug to merge. Step 4 judges each open conflict bug of a
// story before it judges a tip new to the daemon (Runs.tipCheck), and
// closes a moot one's open PR, then the bug, as not planned. Nothing
// judges a closed bug, so the PR goes first: a crash between the two
// leaves the bug open, at worst handed off for its closed PR, and the next
// judging of the tip closes it.

// conflictMoot judges each open conflict bug of story, whose branch's tip
// is tip, new to the daemon (SPEC §7.5, A conflict merged by hand): each
// snapshot the daemon holds, by number, that's open, below story in this
// repo, and a conflict bug of it (conflictMain). One is moot when the main
// commit its origin names is in tip's history (conflictMootIn), and its
// own head isn't (conflictMootHead), read in the bare repo, at no request.
// A moot one is closed, its PR first (conflictMootClose). So a story with
// no open conflict bug, or none moot, costs no request.
//
// A conflict bug that's skip, the issue the pass applied a saved result
// to, whose snapshot is from before that, so its head and its PR may be
// out of date, isn't judged further once tip holds its main commit:
// conflictMoot reports false, and the tip waits for the next pass, so its
// judging finds the bug as the result left it. A tip that lacks that
// commit doesn't wait. Otherwise it reports true, and the tip is judged as
// any push the App didn't make.
func (r *Runs) conflictMoot(ctx context.Context, story *github.Snapshot, tip string, skip int) (bool, error) {
	d := r.Daemon
	a := d.Step.Applier
	for _, n := range slices.Sorted(maps.Keys(d.snaps)) {
		s := d.snaps[n]
		if s == nil || s.Closed || s.Parent != story.Number || s.ParentRepo != "" {
			continue
		}
		main := d.conflictMain(s)
		if main == "" {
			continue
		}
		in, err := d.conflictMootIn(ctx, main, tip)
		switch {
		case err != nil:
			return false, fmt.Errorf("judge its conflict bug #%d on the tip %s: its main commit: %w", n, tip, err)
		case !in:
			continue
		case n == skip:
			logr.Debugf("%s/%s#%d: its branch's tip %s waits for the next pass, as it holds main's %s, which its conflict bug #%d names, and #%d had a result applied this pass",
				a.Owner, a.Repo, story.Number, tip, main, n, n)
			return false, nil
		}
		is := state.Derive(s, a.AppSlug, failureEdited(s, a.AppSlug))
		moot, err := d.conflictMootHead(ctx, n, is, tip)
		if err != nil {
			return false, fmt.Errorf("judge its conflict bug #%d on the tip %s: %w", n, tip, err)
		}
		if !moot {
			continue
		}
		if err := d.conflictMootClose(ctx, s, is, main, tip); err != nil {
			return false, fmt.Errorf("close its conflict bug #%d, made moot by the tip %s: %w", n, tip, err)
		}
	}
	return true, nil
}

// conflictMootHead reports whether tip, which holds the main commit
// conflict bug n names, makes the bug moot, as its own head isn't in tip's
// history (SPEC §7.5, A conflict merged by hand). Its head is the head its
// PR was verified at, in is, its state (prHeadOf), or else its head ref,
// refs/issuedog/tasks/<n>, or none, which makes it moot. It reads the bare
// repo, at no request.
func (d *Daemon) conflictMootHead(ctx context.Context, n int, is state.Issue, tip string) (bool, error) {
	head := prHeadOf(is)
	if head == "" {
		ref, ok, err := d.Clone.ReadRef(ctx, taskRef(n))
		if err != nil {
			return false, fmt.Errorf("read its head: %w", err)
		}
		if ok {
			head = ref
		}
	}
	if head == "" {
		return true, nil
	}
	in, err := d.conflictMootIn(ctx, head, tip)
	if err != nil {
		return false, fmt.Errorf("its head: %w", err)
	}
	return !in, nil
}

// conflictMootIn reports whether the commit sha is in tip's history, read
// in the bare repo: one the bare repo lacks isn't, as the bare repo holds
// tip's history.
func (d *Daemon) conflictMootIn(ctx context.Context, sha, tip string) (bool, error) {
	has, err := d.Clone.HasCommit(ctx, sha)
	if err != nil {
		return false, fmt.Errorf("look for %s: %w", sha, err)
	}
	if !has {
		return false, nil
	}
	in, err := d.Clone.IsAncestor(ctx, sha, tip)
	if err != nil {
		return false, fmt.Errorf("read whether the tip %s holds %s: %w", tip, sha, err)
	}
	return in, nil
}

// conflictMootClose closes s, an open conflict bug whose state is is,
// which names main's commit main, made moot by tip, its story branch's tip
// (SPEC §7.5, A conflict merged by hand). Each write is checked first, so
// a replay, a restart or a crash between the two repeats none and loses
// none (SPEC §5.2):
//
//   - its PR (taskPR), if the poll lists it open and not merged
//     (Daemon.prs), is closed, and kept closed, with the updated_at the
//     poll listed, so the next poll lists it as changed;
//   - then the bug moves to closed, as not planned, on conflict-moot,
//     keyed event=t<SHA> by tip (failureKey), with a comment naming the tip
//     and the main commit (renderConflictMoot), from whatever open stage
//     it's at, held included, which ends its hold. A closed issue starts
//     no run, and a result of its run still waiting is dropped (SPEC §8.2).
//
// A state error is warned of, and the bug left open, as Transition
// reported it. The bug is read again next pass.
func (d *Daemon) conflictMootClose(ctx context.Context, s *github.Snapshot, is state.Issue, main, tip string) error {
	a, n := d.Step.Applier, s.Number
	if p, ok := d.prs[taskPR(is)]; ok && p.Open && !p.Merged {
		closed := "closed"
		pr, err := a.Client.UpdatePullRequest(ctx, a.Owner, a.Repo, p.Number, nil, nil, &closed)
		if err != nil {
			return fmt.Errorf("close its PR #%d: %w", p.Number, err)
		}
		p.Open, p.ClosedAt = false, pr.ClosedAt
		d.prs[p.Number] = p
		logr.Infof("%s/%s#%d: closed its PR #%d, as the story branch's tip %s holds main's %s, which this conflict bug was filed to merge",
			a.Owner, a.Repo, n, p.Number, tip, main)
	}
	dirt, err := a.Dirty(ctx, s, d.Step.Trust)
	if err != nil {
		return fmt.Errorf("read its dirty content: %w", err)
	}
	d.reread(n, false)
	m := Move{To: state.Closed, StateReason: "not_planned", Trigger: state.TriggerConflictMoot, Key: failureKey(tip), Text: renderConflictMoot(tip, main)}
	err = a.Transition(ctx, s, dirt.Has, m)
	if StateError(err) {
		logr.Warnf("%s/%s#%d: left open, though the story branch's tip %s makes this conflict bug moot: %v", a.Owner, a.Repo, n, tip, err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("close it: %w", err)
	}
	logr.Infof("%s/%s#%d: closed as not planned, as the story branch's tip %s holds main's %s, which this conflict bug was filed to merge",
		a.Owner, a.Repo, n, tip, main)
	return nil
}

// renderConflictMoot is the comment of a conflict bug's close once tip,
// its story branch's tip, makes it moot, holding main's commit main, which
// the bug names (SPEC §7.5, A conflict merged by hand), before Transition
// escapes it.
func renderConflictMoot(tip, main string) string {
	return fmt.Sprintf("Closed as not planned: the story branch's tip `%s` holds `main` at `%s`, which this conflict bug was filed to merge, "+
		"though not through this bug's own head, as when someone merged that commit, or a newer `main`, into the branch by hand. "+
		"So this bug is moot, and its PR, if it had an open one, is closed.", tip, main)
}
