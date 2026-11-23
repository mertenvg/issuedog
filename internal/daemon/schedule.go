package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/issuedog/internal/state"
)

// pickSearchEvery is how often, by GitHub's clock, the pick searches for
// the blocked ready tasks again though no poll listed a change since its
// last search (SPEC §8.1): a blocked-by link changes no issue's updated_at.
const pickSearchEvery = 10 * time.Minute

// pickTask is step 4's pick (SPEC §8.1), which Work calls once no task at
// in-progress had a run: it picks the next ready task, moves it to
// in-progress, and starts its run in the same pass, through the Stage at
// in-progress, the implementer. It reports whether it picked one.
//
// Nothing is picked while a task holds the one worker (pickHeld), or with
// no Stage at in-progress. The candidates are the open ready issues with a
// stage:ready label and no parent, or in a story (pickCandidate), in
// pickLess's order. A task with any other parent isn't one, uncounted.
// Only once there is a candidate does the pick ask the issue search for
// the blocked ready tasks (pickSearch), and each candidate it finds
// blocked is passed over with no request. The first of the others that
// pickOne picks is the one: a candidate pickOne passes over leaves the
// next its turn. A candidate whose story's branch pickOne cuts ends the
// pick too, with nothing picked that pass (pickStory).
//
// Nor is anything picked on a pass whose step 1 didn't finish on every
// issue (pickGaps): an issue whose read failed or found it gone is out of
// the pass, so a task the pass can't see may hold the worker.
func (r *Runs) pickTask(ctx context.Context, ready []Ready) (bool, error) {
	st, ok := r.Stages[state.InProgress]
	if !ok {
		return false, nil
	}
	if r.pickGaps() {
		return false, nil
	}
	switch held, err := r.pickHeld(ctx, ready); {
	case err != nil:
		return false, fmt.Errorf("the worker: %w", err)
	case held:
		return false, nil
	}
	var candidates []pickCand
	for _, rd := range ready {
		if rd.S.Closed || stageOf(rd.S) != state.Ready {
			continue
		}
		story, ok, err := r.pickCandidate(ctx, st, rd.S)
		if err != nil {
			return false, fmt.Errorf("a candidate's story: %w", err)
		}
		if ok {
			candidates = append(candidates, pickCand{Ready: rd, story: story})
		}
	}
	if len(candidates) == 0 {
		return false, nil
	}
	less := pickLess(r.Daemon.snaps)
	slices.SortStableFunc(candidates, func(a, b pickCand) int { return less(a.S, b.S) })
	blocked, err := r.pickSearch(ctx)
	if err != nil {
		return false, fmt.Errorf("the search: %w", err)
	}
	for _, c := range candidates {
		if blocked[c.S.Number] {
			logr.Debugf("%s/%s#%d isn't picked: the issue search finds it blocked", r.Daemon.Step.Owner, r.Daemon.Step.Repo, c.S.Number)
			continue
		}
		switch picked, err := r.pickOne(ctx, c.Ready, st, c.story); {
		case err != nil:
			return picked, fmt.Errorf("a candidate: %w", err)
		case picked:
			return true, nil
		}
	}
	return false, nil
}

// pickCand is a candidate of the pick: a ready task, and its story, or nil
// for a task with no parent, or for any task when the Stage at in-progress
// isn't a Committer (pickCandidate).
type pickCand struct {
	Ready
	story *github.Snapshot
}

// pickCandidate reports whether the ready task s is a candidate of the
// pick, for st, the Stage at in-progress, and returns its story when it's
// in one (SPEC §7.5, §8.1). A task with no parent is one, with no story,
// and so is any task when st isn't a Committer, as such a Stage's run
// names no story branch (storyStart). Otherwise it's a candidate only in
// a story: its parent is an open story, or an open epic planned into
// tasks, in this repo, among the snapshots the daemon holds (storyOf).
// A task with any other parent, as an epic planned into stories, isn't
// one, and that's logged at debug. Nor is one whose parent's plan data
// doesn't read back, which is warned of once (pickStoryOnce). It reads
// step 1's snapshots, and an epic's dirty content alone, kept from step 1
// (Applier.Dirty), so it costs no request: a failure to read that is an
// error.
func (r *Runs) pickCandidate(ctx context.Context, st Stage, s *github.Snapshot) (*github.Snapshot, bool, error) {
	if _, ok := st.(Committer); !ok || s.Parent == 0 {
		return nil, true, nil
	}
	d, n := r.Daemon, s.Number
	a := d.Step.Applier
	parent := d.snaps[s.Parent]
	var parentDirt Dirt
	var dirtErr error
	read := false
	dirty := func(id int64) bool {
		if !read {
			read = true
			parentDirt, dirtErr = a.Dirty(ctx, parent, d.Step.Trust)
		}
		return parentDirt.Has(id)
	}
	story, ok, err := storyOf(d.snaps, s, a.AppSlug, dirty)
	switch {
	case dirtErr != nil:
		return nil, false, fmt.Errorf("read #%d's parent #%d: %w", n, s.Parent, dirtErr)
	case err != nil:
		r.pickStoryOnce(s.Parent, "plan:"+err.Error(), logr.Warnf, "%s/%s#%d: no task under it is picked, as it can't tell what its plan is of: %v",
			a.Owner, a.Repo, s.Parent, err)
		return nil, false, nil
	case !ok:
		logr.Debugf("%s/%s#%d isn't picked: its parent, #%d, is no open story or epic planned into tasks in this repo", a.Owner, a.Repo, n, s.Parent)
		return nil, false, nil
	}
	return story, true, nil
}

// pickStoryOnce logs, with logf, why no task in story n is picked, unless
// the last such log for it had the same key, so a wait that lasts, as for
// its branch's tip's check, is logged once, not every pass, and a new
// cause, or a new tip, is logged again. The repeats are logged at debug.
// Its keys are the pick's own (Runs.pickLogged), apart from the ones the
// tip checks and the checks at verifying keep for the same story
// (storyWarnOnce), so neither's log on a wait that lasts makes the other's
// repeat.
func (r *Runs) pickStoryOnce(n int, key string, logf func(string, ...any) string, format string, args ...any) {
	if r.pickLogged[n] == key {
		logr.Debugf(format, args...)
		return
	}
	if r.pickLogged == nil {
		r.pickLogged = map[int]string{}
	}
	r.pickLogged[n] = key
	logf(format, args...)
}

// pickGaps reports whether step 1 didn't finish on some issue this pass
// (Daemon.step1Gaps), its read included, and logs why: then the pass may
// not see the task that holds the worker, such as one at in-progress
// whose read failed, so nothing is picked (SPEC §8.1). It costs no
// request.
func (r *Runs) pickGaps() bool {
	gaps := r.Daemon.step1Gaps
	if len(gaps) == 0 {
		return false
	}
	why := make([]string, 0, len(gaps))
	for _, n := range slices.Sorted(maps.Keys(gaps)) {
		why = append(why, fmt.Sprintf("#%d, as %s", n, gaps[n].why))
	}
	logr.Debugf("no task is picked: step 1 didn't finish on every issue this pass, so a task holding the worker may be out of it: %s",
		strings.Join(why, "; "))
	return true
}

// pickHeld reports whether a task holds the one worker (SPEC §8.1): a
// lease holds a run, or an open issue Work got or the daemon holds a
// snapshot of is at in-progress or verifying, by its stage labels, which
// step 1 keeps in line with its latest marker (SPEC §5.1), so it costs no
// request. A task at clarifying, needs-human, pr-open or held waits on
// the owner, and releases it.
func (r *Runs) pickHeld(ctx context.Context, ready []Ready) (bool, error) {
	l, ok, err := r.readLease(ctx)
	if err != nil {
		return false, fmt.Errorf("the worker's lease: %w", err)
	}
	if ok && !l.Failed {
		logr.Debugf("no task is picked: run %s on #%d holds the worker", l.RunID, l.Issue)
		return true, nil
	}
	snaps := make([]*github.Snapshot, 0, len(ready)+len(r.Daemon.snaps))
	for _, rd := range ready {
		snaps = append(snaps, rd.S)
	}
	for _, s := range r.Daemon.snaps {
		snaps = append(snaps, s)
	}
	for _, s := range snaps {
		if s.Closed {
			continue
		}
		if i := slices.IndexFunc(s.Labels, pickHolds); i >= 0 {
			logr.Debugf("no task is picked: #%d, at %s, holds the worker", s.Number, s.Labels[i])
			return true, nil
		}
	}
	return false, nil
}

// pickHolds reports whether label is the stage label of a task that holds
// the worker: in-progress or verifying.
func pickHolds(label string) bool {
	st, ok := state.ParseLabel(label)
	return ok && (st == state.InProgress || st == state.Verifying)
}

// pickLess orders the pick's candidates (SPEC §8.1): by priority, then the
// parent's priority, from its snapshot in snaps, or the task's own when it
// has no parent or none is held, then by number.
func pickLess(snaps map[int]*github.Snapshot) func(a, b *github.Snapshot) int {
	parent := func(s *github.Snapshot) int {
		if p := snaps[s.Parent]; s.Parent != 0 && p != nil {
			return priority(p)
		}
		return priority(s)
	}
	return func(a, b *github.Snapshot) int {
		return cmp.Or(cmp.Compare(priority(a), priority(b)), cmp.Compare(parent(a), parent(b)), cmp.Compare(a.Number, b.Number))
	}
}

// pickSearch returns the open ready tasks the issue search finds blocked
// (github.Client.ReadyBlocked): its last answer, asked again only when a
// poll listed a change since (Daemon.pickStale), as a start's first poll
// does, or once pickSearchEvery has passed by GitHub's clock. A
// repeat with nothing changed is a 304. The search lags behind dependency
// writes, so pickOne confirms whatever it would start.
func (r *Runs) pickSearch(ctx context.Context) (map[int]bool, error) {
	st := r.Daemon.Step
	now := st.Client.Now()
	if !r.Daemon.pickStale && now.Sub(r.pickSearched) < pickSearchEvery {
		return r.pickBlocked, nil
	}
	blocked, err := st.Client.ReadyBlocked(ctx, st.Owner, st.Repo)
	if err != nil {
		return nil, fmt.Errorf("the blocked ready tasks: %w", err)
	}
	r.pickBlocked, r.pickSearched, r.Daemon.pickStale = blocked, now, false
	return blocked, nil
}

// pickText is the text of the pick's move.
const pickText = "I've picked this task, so the implementer starts on it now."

// pickOne picks the ready task rd, unless it's passed over, and reports
// whether the pick ends this pass: it picked rd, or cut the branch of
// story, rd's story, or nil for none (pickCandidate). It's passed over,
// with nothing written, when its latest marker doesn't put it at ready,
// or it's no task that gets a PR (state.AnyTask), or it has an open
// blocker. Labels that give its run no settings (state.ErrLabels) pass it
// over too, with their state error, as a run would get (reportLabels),
// once for each change to them that stands (labelsKey). A task in a story
// is passed over, too, while its story waits, and with no branch for its
// story, it gets the branch's cut instead of the pick (pickStory).
//
// Its blockers are read through blocked_by (pickOwnBlocked), with a
// conditional request, as the search may lag behind a link: one with an
// open blocker joins the search's answer, so it's read no more until the
// next search. Then it moves to in-progress, keyed by the comment holding
// its move to ready, event=s<comment ID>, with seen= the newest comment,
// and its run starts (RunStage) on the snapshot the move left, so the
// run's seen is the move's comment, and the move doesn't drop its result
// (SPEC §8.2). A task in a story starts from its story branch's tip, which
// pickStory saw checked (storyStart).
func (r *Runs) pickOne(ctx context.Context, rd Ready, st Stage, story *github.Snapshot) (bool, error) {
	a, n := r.Daemon.Step.Applier, rd.S.Number
	wrap := func(err error) error { return fmt.Errorf("%s/%s#%d: %w", a.Owner, a.Repo, n, err) }
	dirt, err := a.Dirty(ctx, rd.S, r.Daemon.Step.Trust)
	if err != nil {
		return false, wrap(err)
	}
	is := state.Derive(rd.S, a.AppSlug, dirt.Has)
	if class, err := state.ClassOf(is.Item); is.Stage != state.Ready || err != nil || class&state.AnyTask == 0 {
		logr.Debugf("%s/%s#%d isn't picked: it's no task at ready, by its markers", a.Owner, a.Repo, n)
		return false, nil
	}
	_, badLabels := state.RunSettings(state.Agent(st.Agent()), rd.S.Labels, r.Daemon.Config, 1)
	switch {
	case errors.Is(badLabels, state.ErrLabels) && is.Reported(kindLabels, labelsKey(rd.S)):
		logr.Debugf("%s/%s#%d isn't picked: its model and effort labels give no settings, as reported: %v", a.Owner, a.Repo, n, badLabels)
		return false, nil
	case errors.Is(badLabels, state.ErrLabels):
		// Step 1 checked the labels as rd.S has them this pass, or the poll
		// found them unchanged since, so rd.S is the issue as read again.
		r.Daemon.reread(n, false)
		if err := r.reportLabels(ctx, rd.S, rd.S, state.Ready, st, badLabels); err != nil {
			return false, wrap(err)
		}
		return false, nil
	case badLabels != nil:
		return false, wrap(badLabels)
	}
	if story != nil {
		cut, wait, err := r.pickStory(ctx, rd, story)
		switch {
		case err != nil:
			return false, wrap(err)
		case cut:
			return true, nil
		case wait:
			return false, nil
		}
	} else {
		switch blocked, err := r.pickOwnBlocked(ctx, n); {
		case err != nil:
			return false, wrap(err)
		case blocked:
			return false, nil
		}
	}
	var seen int64
	for _, c := range rd.S.Comments {
		seen = max(seen, c.ID)
	}
	m := Move{To: state.InProgress, Trigger: state.TriggerScheduled, Key: state.Key{Field: "event", ID: "s" + strconv.FormatInt(is.TransitionComment, 10)},
		Text: pickText}
	if seen > 0 {
		m.Fields = map[string]string{state.FieldSeen: strconv.FormatInt(seen, 10)}
	}
	defer r.Daemon.reread(n, false)
	if err := a.Transition(ctx, rd.S, dirt.Has, m); err != nil {
		return false, wrap(err)
	}
	logr.Infof("%s/%s#%d: picked, at priority p%d, and moved to %s", a.Owner, a.Repo, n, priority(rd.S), state.InProgress)
	if _, err := r.RunStage(ctx, Ready{S: rd.S}, st); err != nil {
		return true, wrap(err)
	}
	return true, nil
}

// pickOwnBlocked reports whether ready task n has an open blocker, read
// through blocked_by (pickOpenBlocker), one conditional request, and logs
// it: the search may lag behind a link (SPEC §8.1).
func (r *Runs) pickOwnBlocked(ctx context.Context, n int) (bool, error) {
	a := r.Daemon.Step.Applier
	b, blocked, err := r.pickOpenBlocker(ctx, n)
	if err != nil {
		return false, fmt.Errorf("its blockers: %w", err)
	}
	if blocked {
		logr.Infof("%s/%s#%d isn't picked: #%d blocks it, though the issue search didn't find it blocked yet", a.Owner, a.Repo, n, b)
	}
	return blocked, nil
}

// pickOpenBlocker returns an open blocker of issue n, and true, or false
// with none, from its blockers, read through blocked_by
// (github.Client.Blockers), with a conditional request. An issue with an
// open blocker joins the search's answer (Runs.pickBlocked), so it's read
// no more until the next search (pickSearch).
func (r *Runs) pickOpenBlocker(ctx context.Context, n int) (int, bool, error) {
	a := r.Daemon.Step.Applier
	blockers, err := a.Client.Blockers(ctx, a.Owner, a.Repo, n)
	if err != nil {
		return 0, false, fmt.Errorf("read #%d's blockers: %w", n, err)
	}
	i := slices.IndexFunc(blockers, func(b github.Blocker) bool { return !b.Closed })
	if i < 0 {
		return 0, false, nil
	}
	if r.pickBlocked == nil {
		r.pickBlocked = map[int]bool{}
	}
	r.pickBlocked[n] = true
	return blockers[i].Number, true, nil
}

// pickStory judges the pick of rd, a ready task in story, that pickOne
// would otherwise take (SPEC §3.4, §7.5, §8.1), and reports whether it
// cut story's branch, which ends the pick for the pass, with nothing
// picked, or whether rd waits, uncounted. With neither, rd is picked, and
// its run starts from the branch's checked tip.
//
// Its story's branch is the one with its prefix in the bare clone, as the
// pass's sync left it, and its tip that branch's (storyBranch), at no
// request. Two branches with its prefix get no cut and no pick, and that's
// warned of once (pickStoryOnce). A tip whose App issuedog/tip naming the
// branch isn't success, as step 4 judged or read it (tipSuccess), as a
// new one before its check, after a restart before step 4 reads it, a
// failure, or one step 2 merged a task's PR onto (tipMergedOnto), has rd
// wait, logged once a tip, still at no request. So a
// task waiting for its tip makes no request.
//
// Otherwise rd's own blockers are read (pickOwnBlocked), then its story's,
// one conditional request each, through blocked_by (pickOpenBlocker), so
// the story's are read only on a pass where rd could be picked. A story
// found blocked joins the search's answer by its number, and isn't read
// again until the next search, as a task isn't: till then its tasks wait
// with no request. With neither blocked, a story with no branch gets its
// branch cut (pickCut), and with a tip that's success, rd is picked.
func (r *Runs) pickStory(ctx context.Context, rd Ready, story *github.Snapshot) (cut, wait bool, err error) {
	d, n, sn := r.Daemon, rd.S.Number, story.Number
	a := d.Step.Applier
	branch, tip, err := d.storyBranch(ctx, story)
	switch {
	case errors.Is(err, errTwoBranches):
		r.pickStoryOnce(sn, "branches:"+err.Error(), logr.Warnf, "%s/%s#%d: no task in it is picked, and no branch is cut for it, as it can't tell which is its branch: %v",
			a.Owner, a.Repo, sn, err)
		return false, true, nil
	case err != nil:
		return false, false, fmt.Errorf("its story's branch: %w", err)
	case tip != "" && !d.tipSuccess(branch, tip):
		why := "has no " + github.TipCheck + " as success yet"
		if j, ok := d.tips[branch]; ok && j.Tip == tip {
			why = "has " + github.TipCheck + " as " + j.Conclusion
		}
		if d.tipMergedOnto(branch, tip) {
			why = "had a task's PR merged onto it, so it's the branch's tip no more"
		}
		r.pickStoryOnce(sn, "tip:"+branch+"@"+tip+":"+why, logr.Infof, "%s/%s#%d: no task in it is picked, as its branch %s's tip %s %s",
			a.Owner, a.Repo, sn, branch, tip, why)
		return false, true, nil
	}
	if r.pickBlocked[sn] {
		logr.Debugf("%s/%s#%d isn't picked: its story #%d has an open blocker, as last read", a.Owner, a.Repo, n, sn)
		return false, true, nil
	}
	switch blocked, err := r.pickOwnBlocked(ctx, n); {
	case err != nil:
		return false, false, fmt.Errorf("in story #%d: %w", sn, err)
	case blocked:
		return false, true, nil
	}
	b, blocked, err := r.pickOpenBlocker(ctx, sn)
	switch {
	case err != nil:
		return false, false, fmt.Errorf("its story's blockers: %w", err)
	case blocked:
		logr.Infof("%s/%s#%d isn't picked: #%d blocks its story #%d", a.Owner, a.Repo, n, b, sn)
		return false, true, nil
	case tip == "":
		if err := r.pickCut(ctx, n, story); err != nil {
			return false, false, fmt.Errorf("cut its story's branch: %w", err)
		}
		return true, false, nil
	}
	return false, false, nil
}

// pickCut cuts story's branch for task n, the pick's candidate in it
// (SPEC §7.5, §8.1): one push of main's commit, as the pass's sync left it
// in the bare clone, to issuedog/<story>-<slug>, its slug from the story's
// title as a task's is (taskSlug), leased at no branch (repo.Repo.Push),
// with the App's installation token, as the sync fetches. It moves no
// issue: n stays at ready until step 4 has checked the new tip. The push
// lands only while the remote has no such branch, so a replay, or a
// restart after a crash, that finds none in the bare clone pushes again
// to that one name, and never cuts a second. A push refused as stale, as
// one an admin cut since the sync, changes nothing, and is logged: the
// next pass's sync shows the branch, and the pick looks again.
func (r *Runs) pickCut(ctx context.Context, n int, story *github.Snapshot) error {
	d := r.Daemon
	a := d.Step.Applier
	main, err := d.Clone.Resolve(ctx, "main")
	if err != nil {
		return fmt.Errorf("read main: %w", err)
	}
	branch := taskBranchPrefix(story.Number) + taskSlug(story.Title)
	token, err := d.Step.Client.Token(ctx)
	if err != nil {
		return fmt.Errorf("cut %s: %w", branch, err)
	}
	err = d.Clone.Push(ctx, token, main, branch, "")
	switch {
	case errors.Is(err, repo.ErrPushStale):
		logr.Infof("%s/%s#%d: its story #%d's branch %s isn't cut from main's %s, as the remote has one since the sync, so the next pass looks again: %v",
			a.Owner, a.Repo, n, story.Number, branch, main, err)
		return nil
	case err != nil:
		return fmt.Errorf("cut %s from main's %s: %w", branch, main, err)
	}
	logr.Infof("%s/%s#%d: cut its story #%d's branch %s from main's %s, so it's picked once the new tip is checked", a.Owner, a.Repo, n, story.Number, branch, main)
	return nil
}
