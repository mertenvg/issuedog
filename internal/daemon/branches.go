package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/issuedog/internal/state"
)

// An issue's branches are the bare clone's branches with its prefix,
// issuedog/<n>- (SPEC §7.5). Two or more with one prefix, or none for a
// story whose branch was deleted once a task or bug in it had been
// in-progress, are a state error, kind=branches (SPEC §5.3): the daemon
// can't tell which is the issue's branch, or can't cut the story's again
// without dropping its merged work, so it reports it instead of acting.
// Step 2 judges it on every open issue the daemon holds, from the bare
// clone as the pass's sync left it, at no request (branchesPass), and the
// pick judges a story's before it would cut its branch (pickStory). While
// it stands, acknowledged or not, the issue's runs wait, and so do those
// of every issue in a story it stands on (Runs.branchesWait), and the
// pick cuts no branch for the story and picks no task in it.

// branchesErrKind is the kind of the state error for an issue's branches
// with its prefix: two or more, or none for a story whose work started.
const branchesErrKind = "branches"

// branchesNone is the set of no branches, in a branches error's key.
const branchesNone = "none"

// branchesKey is the key of a branches error for names, the issue's
// branches with its prefix (SPEC §5.3): the names, sorted, each
// percent-encoded as a digest-new marker's keys are (github.EncodeValue),
// joined by commas, or "none" for none. With after above 0, it's a new
// occurrence of that set: then ":", which no branch name can hold, and
// after, the ID of the newest comment for the set before it changed.
func branchesKey(names []string, after int64) string {
	set := branchesNone
	if len(names) > 0 {
		enc := make([]string, 0, len(names))
		for _, b := range slices.Sorted(slices.Values(names)) {
			enc = append(enc, github.EncodeValue(b))
		}
		set = strings.Join(enc, ",")
	}
	if after > 0 {
		set += ":" + strconv.FormatInt(after, 10)
	}
	return set
}

// branchesSetOf is the set that key, a branches error's, names: key up to
// its ":", if it has one.
func branchesSetOf(key string) string {
	set, _, _ := strings.Cut(key, ":")
	return set
}

// branchesSeen is what the daemon remembers of an issue's set of branches
// while it runs (Daemon.branchSets): Set, the set it saw last, as
// branchesKey names it with no after, and Since, the ID of the newest
// comment on the issue when it saw the set change to Set, or 0 when it
// has seen no change to it since it started, as for the first set it saw.
// A comment for Set whose ID is Since or below is from before that
// change, so the set has changed since it (branchesKeyNow).
type branchesSeen struct {
	Set   string
	Since int64
}

// branchesStand is the branches error that stands on an issue, as step 2
// or the pick last judged it (Daemon.branchErrs): Key, its key, or "" for
// none, and Story, whether the issue is a story (storyIssue), so the
// issues in it wait too (Runs.branchesWait).
type branchesStand struct {
	Key   string
	Story bool
}

// branchesIssue returns the issue whose prefix branch has, issuedog/<n>-,
// and true, or false for a branch with no such prefix.
func branchesIssue(branch string) (int, bool) {
	rest, ok := strings.CutPrefix(branch, repo.BranchPrefix)
	if !ok {
		return 0, false
	}
	num, _, ok := strings.Cut(rest, "-")
	n, err := strconv.Atoi(num)
	if !ok || err != nil || n <= 0 || strconv.Itoa(n) != num {
		return 0, false
	}
	return n, true
}

// branchesNow returns the key of the branches error that stands on issue
// n, or "" for none, as step 2 or the pick last judged it, and true, or
// false when neither has judged n since the daemon started, so a branches
// error on it is still open (openErrors).
func (d *Daemon) branchesNow(n int) (string, bool) {
	e, ok := d.branchErrs[n]
	return e.Key, ok
}

// branchesPass judges the branches error of each open issue the daemon
// holds, in step 2 (SPEC §5.3, §7.5, §8.1), from the branches of the bare
// clone as the pass's sync left it, grouped by their issuedog/<n>- prefix
// (branchesJudge). It reads no story's sub-issues: only the pick does,
// before a cut. An issue to be read again, as one a command touched or
// whose read or step 1 failed, is judged once it's read, so a post never
// goes on a snapshot that may lack a comment. What it remembers of an
// issue the daemon no longer holds is dropped. A failure on one issue
// stops none of the others, and is the pass's error. An unchanged pass
// makes no request: it reads the bare clone, the snapshots, and an
// issue's dirty content only for an error that stands.
func (d *Daemon) branchesPass(ctx context.Context) error {
	a := d.Step.Applier
	all, err := d.Clone.Branches(ctx, repo.BranchPrefix)
	if err != nil {
		return fmt.Errorf("the branches state errors of %s/%s: %w", a.Owner, a.Repo, err)
	}
	byIssue := map[int][]string{}
	for b := range all {
		if n, ok := branchesIssue(b); ok {
			byIssue[n] = append(byIssue[n], b)
		}
	}
	var errs []error
	for _, n := range slices.Sorted(maps.Keys(d.snaps)) {
		s := d.snaps[n]
		if s.Closed || d.stale[n] || d.digest.has(n) {
			continue
		}
		if err := d.branchesJudge(ctx, s, byIssue[n], false); err != nil {
			errs = append(errs, err)
		}
	}
	held := func(n int) bool { s := d.snaps[n]; return s != nil && !s.Closed || d.stale[n] }
	maps.DeleteFunc(d.branchSets, func(n int, _ branchesSeen) bool { return !held(n) })
	maps.DeleteFunc(d.branchErrs, func(n int, _ branchesStand) bool { return !held(n) })
	return errors.Join(errs...)
}

// branchesJudge judges the branches error of s, an open issue, with
// names, its branches with its prefix, and posts it (SPEC §5.3). It keeps
// what it judged in Daemon.branchErrs, by s's number.
//
// It first remembers the set (Daemon.branchSets): a set other than the
// one it saw last is a change, made since the newest comment s has. The
// error stands for two or more names, and for none on a story, or an epic
// planned into tasks (storyIssue), once a task or bug in it has been
// in-progress (storyWorked, which reads the story's sub-issues only with
// read). Its key is branchesKeyNow's. Before posting, it looks for an
// error with that kind and key, posted or acknowledged
// (state.Issue.Reported), so a replay or a restart posts none twice: a set
// that comes back once it has changed is a new occurrence, with a new
// key, and a 👍 on its earlier comment doesn't cover it. The post is
// logged as an error; once it stands, it's logged at debug. The next
// poll lists the issue the post changed, so step 1 finds the error open
// on the next pass (Daemon.openErrs).
func (d *Daemon) branchesJudge(ctx context.Context, s *github.Snapshot, names []string, read bool) error {
	a, n := d.Step.Applier, s.Number
	wrap := func(err error) error {
		return fmt.Errorf("the branches state error of %s/%s#%d: %w", a.Owner, a.Repo, n, err)
	}
	set := branchesKey(names, 0)
	seen, ok := d.branchSets[n]
	switch {
	case !ok:
		seen = branchesSeen{Set: set}
	case seen.Set != set:
		seen = branchesSeen{Set: set, Since: newestComment(s)}
	}
	if d.branchSets == nil {
		d.branchSets = map[int]branchesSeen{}
	}
	d.branchSets[n] = seen
	if d.branchErrs == nil {
		d.branchErrs = map[int]branchesStand{}
	}
	if len(names) == 1 {
		d.branchErrs[n] = branchesStand{}
		return nil
	}
	// s's dirty content is read once, and only when it's needed: for an
	// epic's plan, or for an error that stands.
	var dirt Dirt
	var dirtErr error
	dirtRead := false
	dirtOf := func() (Dirt, error) {
		if !dirtRead {
			dirtRead = true
			dirt, dirtErr = a.Dirty(ctx, s, d.Step.Trust)
		}
		return dirt, dirtErr
	}
	story, err := storyIssue(s, a.AppSlug, func(id int64) bool {
		dd, err := dirtOf()
		return err == nil && dd.Has(id)
	})
	switch {
	case dirtErr != nil:
		return wrap(dirtErr)
	case err != nil && len(names) == 0:
		// An epic whose plan data doesn't read back gets no cut either
		// (pickCandidate), so nothing is lost by waiting.
		logr.Debugf("%s/%s#%d: no branches state error is judged for it, as it can't tell what its plan is of: %v", a.Owner, a.Repo, n, err)
		d.branchErrs[n] = branchesStand{}
		return nil
	case err != nil:
		// Its tasks wait all the same, as no tip is named (storyStart).
		story = false
	}
	if len(names) == 0 {
		worked := false
		if story {
			if worked, err = d.storyWorked(ctx, s, read); err != nil {
				return wrap(err)
			}
		}
		if !worked {
			d.branchErrs[n] = branchesStand{}
			return nil
		}
	}
	if _, err := dirtOf(); err != nil {
		return wrap(err)
	}
	key := branchesKeyNow(s, a.AppSlug, dirt.Has, set, seen)
	d.branchErrs[n] = branchesStand{Key: key, Story: story}
	desc := fmt.Sprintf("%s/%s#%d has %s with its prefix %s, keyed %s", a.Owner, a.Repo, n, branchesNamed(names), taskBranchPrefix(n), key)
	switch {
	case state.Derive(s, a.AppSlug, dirt.Has).Reported(branchesErrKind, key):
		logr.Debugf("state error, reported already: %s", desc)
		return nil
	case d.stale[n]:
		// Its snapshot may lack a comment, as one a command wrote, so the
		// post waits for its next read.
		logr.Debugf("state error, posted once it's read again: %s", desc)
		return nil
	}
	marker, err := github.FormatMarker(github.Marker{Kind: "error", Fields: map[string]string{"kind": branchesErrKind, "key": key}})
	if err != nil {
		return wrap(err)
	}
	if err := a.comment(ctx, s, join(github.Escape(branchesText(n, names, story)), marker)); err != nil {
		return wrap(err)
	}
	logr.Errorf("state error: %s", desc)
	return nil
}

// branchesNamed names names, an issue's branches, for the log.
func branchesNamed(names []string) string {
	if len(names) == 0 {
		return "no branch"
	}
	return fmt.Sprintf("the branches %q", slices.Sorted(slices.Values(names)))
}

// newestComment is the ID of the newest comment on s, or 0 with none.
func newestComment(s *github.Snapshot) int64 {
	var id int64
	for _, c := range s.Comments {
		id = max(id, c.ID)
	}
	return id
}

// branchesKeyNow is the key of the branches error of s for set, its
// branches with its prefix as branchesKey names them, with seen, what the
// daemon remembers of the set (SPEC §5.3), and dirty, s's dirty content. A
// comment for a set is a clean App comment carrying a branches error, not
// an ack reply, whose key names that set, with or without ":" and an ID.
//   - With no comment for set, the key is set.
//   - With one, the newest, once the set has changed since it, the key is
//     set, ":" and its ID: a new occurrence. The set has changed once the
//     daemon has seen it change since that comment (seen.Since), which it
//     remembers only while it runs, or once s has a newer comment for
//     another set, which a restart reads back.
//   - Otherwise it's that comment's key, so a set that comes back after a
//     change no comment records, which the daemon didn't see, reuses it.
//
// A post of the new occurrence is the newest comment for set then, from
// after the change, so the key stays put: a replay, or a post that failed
// after it landed, finds it.
func branchesKeyNow(s *github.Snapshot, appSlug string, dirty func(int64) bool, set string, seen branchesSeen) string {
	var newest, other int64
	newestKey := ""
	for _, c := range s.Comments {
		if dirty(c.ID) {
			continue
		}
		for _, m := range github.TrustedMarkers(c.Author, c.Body, appSlug) {
			if m.Kind != "error" || m.Fields["kind"] != branchesErrKind || m.Fields["ack"] != "" {
				continue
			}
			key := m.Fields["key"]
			switch {
			case branchesSetOf(key) != set:
				other = max(other, c.ID)
			case c.ID > newest:
				newest, newestKey = c.ID, key
			}
		}
	}
	switch {
	case newest == 0:
		return set
	case seen.Set == set && newest <= seen.Since, other > newest:
		return set + ":" + strconv.FormatInt(newest, 10)
	}
	return newestKey
}

// branchesText is the branches error's comment on issue n, with names,
// its branches with its prefix, and story, whether n is a story, whose
// tasks wait too. The names aren't the App's text, as an admin may name a
// branch anything git allows, so each is a code span (checkCode).
func branchesText(n int, names []string, story bool) string {
	prefix := "`" + taskBranchPrefix(n) + "`"
	if len(names) == 0 {
		return fmt.Sprintf("This story's branch, with the prefix %s, is gone, though a task or bug in it has been in progress, so the story's merged work may be on that branch alone. "+
			"A new branch cut from `main` would drop that work, so I don't cut one, and no task or bug in the story is picked or runs until a branch with its prefix is back. "+
			"Restore the branch, or create one. A 👍 on this comment acknowledges it, but the story's work still waits for its branch.", prefix)
	}
	named := make([]string, 0, len(names))
	for _, b := range slices.Sorted(slices.Values(names)) {
		named = append(named, checkCode(b))
	}
	wait := "start none of this issue's runs"
	if story {
		wait += ", and no task or bug in the story is picked or runs"
	}
	return fmt.Sprintf("This issue has %d branches with its prefix %s: %s. I can't tell which is its branch, so I act on none of them: I push nothing to them, open no pull request from them, and %s, until one is left. "+
		"Delete the others. A 👍 on this comment acknowledges it, but the wait holds until one is left.", len(names), prefix, strings.Join(named, ", "), wait)
}

// storyWorked reports whether a task or bug in story, a story or an epic
// planned into tasks, has been in-progress (SPEC §7.5), so its branch, once
// gone, isn't cut again: its own branches error for no branch is posted
// only then, and work that started never stops having started; or an open
// child the daemon holds has a transition into or out of in-progress among
// its markers; or, with read, a child among the story's sub-issues, read
// once, one request, is closed with stage:completed and isn't a human task
// (storyWorkedClosed), as the daemon holds no closed issue. Without read,
// it reads only the snapshots, and their dirty content only for a marker it
// would find, so it costs no request.
func (d *Daemon) storyWorked(ctx context.Context, story *github.Snapshot, read bool) (bool, error) {
	sn := story.Number
	worked, err := d.storyWorkedBy(ctx, story, func(is state.Issue) bool {
		return slices.ContainsFunc(slices.Collect(maps.Keys(is.Errors)), func(k state.ErrorKey) bool {
			return k.Kind == branchesErrKind && branchesSetOf(k.Key) == branchesNone
		})
	})
	switch {
	case err != nil:
		return false, fmt.Errorf("story #%d's own error: %w", sn, err)
	case worked:
		return true, nil
	}
	for _, c := range slices.Sorted(maps.Keys(d.snaps)) {
		s := d.snaps[c]
		if s.Closed || s.Parent != sn || s.ParentRepo != "" {
			continue
		}
		worked, err := d.storyWorkedBy(ctx, s, func(is state.Issue) bool {
			return slices.ContainsFunc(is.Markers, func(m github.Marker) bool {
				return m.Kind == "transition" && (m.Fields["to"] == string(state.InProgress) || m.Fields["from"] == string(state.InProgress))
			})
		})
		switch {
		case err != nil:
			return false, fmt.Errorf("story #%d's child: %w", sn, err)
		case worked:
			return true, nil
		}
	}
	if !read {
		return false, nil
	}
	worked, err = d.storyWorkedClosed(ctx, sn)
	if err != nil {
		return false, fmt.Errorf("story #%d's closed children: %w", sn, err)
	}
	return worked, nil
}

// storyWorkedBy reports whether test holds of s's state, read with its
// dirty content (state.Derive). It's judged first with no content dirty,
// as dirt only takes markers away, so s's dirty content is read only for
// a marker test would find.
func (d *Daemon) storyWorkedBy(ctx context.Context, s *github.Snapshot, test func(state.Issue) bool) (bool, error) {
	a := d.Step.Applier
	if !test(state.Derive(s, a.AppSlug, func(int64) bool { return false })) {
		return false, nil
	}
	dirt, err := a.Dirty(ctx, s, d.Step.Trust)
	if err != nil {
		return false, fmt.Errorf("whether #%d was in progress: %w", s.Number, err)
	}
	return test(state.Derive(s, a.AppSlug, dirt.Has)), nil
}

// storyWorkedClosed reports whether a sub-issue of story sn, in this repo,
// is closed with stage:completed and isn't a human task, which never goes
// to in-progress: its App origin marker, in its body as listed, has no
// human=1 (state.OriginMarker). It reads the sub-issues, one request.
func (d *Daemon) storyWorkedClosed(ctx context.Context, sn int) (bool, error) {
	a := d.Step.Applier
	subs, err := a.Client.SubIssues(ctx, a.Owner, a.Repo, sn)
	if err != nil {
		return false, fmt.Errorf("read story #%d's sub-issues: %w", sn, err)
	}
	completed, _ := state.Completed.Label()
	for _, is := range subs {
		if !a.inRepo(is.GetRepositoryURL()) || is.GetState() != "closed" || !slices.ContainsFunc(is.Labels, func(l *gogithub.Label) bool {
			return strings.EqualFold(l.GetName(), completed)
		}) {
			continue
		}
		author := github.Author{Login: is.GetUser().GetLogin(), Type: is.GetUser().GetType()}
		if m, ok := state.OriginMarker(is.GetBody()); ok && state.IsApp(author, a.AppSlug) && m.Fields["human"] == "1" {
			continue
		}
		logr.Infof("%s/%s#%d: its sub-issue #%d closed as completed, so work in it started", a.Owner, a.Repo, sn, is.GetNumber())
		return true, nil
	}
	return false, nil
}

// branchesWait reports whether st's run on s waits on a branches state
// error (SPEC §5.3, §7.5), uncounted, and logs why once a cause
// (startWaitOnce): s has two or more branches with its prefix in the bare
// clone as the pass's sync left it (taskBranch), or a branches error
// stands on s, as on a story whose branch is gone once work in it started,
// or on its story, s's parent, as step 2 or the pick last judged it
// (Daemon.branchErrs), acknowledged or not. It reads only the bare clone
// and what was judged, so it costs no request.
func (r *Runs) branchesWait(ctx context.Context, st Stage, s *github.Snapshot) (bool, error) {
	d, n := r.Daemon, s.Number
	branches, err := d.Clone.Branches(ctx, taskBranchPrefix(n))
	if err != nil {
		return false, fmt.Errorf("read #%d's branches: %w", n, err)
	}
	_, _, err = taskBranch(n, s.Title, branches)
	switch {
	case errors.Is(err, errTwoBranches):
		r.startWaitOnce(n, "own:"+err.Error(), "#%d: no %s run: it can't tell which is its branch, so its %s state error stands: %v", n, st.Agent(), branchesErrKind, err)
		return true, nil
	case err != nil:
		return false, fmt.Errorf("#%d's branch: %w", n, err)
	}
	if e := d.branchErrs[n]; e.Key != "" {
		r.startWaitOnce(n, "stands:"+e.Key, "#%d: no %s run: its %s state error stands, keyed %s", n, st.Agent(), branchesErrKind, e.Key)
		return true, nil
	}
	if e := d.branchErrs[s.Parent]; s.Parent != 0 && s.ParentRepo == "" && e.Story && e.Key != "" {
		r.startWaitOnce(n, "story:"+e.Key, "#%d: no %s run: its story #%d's %s state error stands, keyed %s", n, st.Agent(), s.Parent, branchesErrKind, e.Key)
		return true, nil
	}
	return false, nil
}
