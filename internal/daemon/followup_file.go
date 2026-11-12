package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file files a pass's follow-up candidates as follow-ups (SPEC §10.2):
// each one a task the App files at intake, which waits there for a trusted
// 👍 to approve it (followup.go), then the filed record on the task.

// filedKind is the kind of the record that a pass's follow-ups are filed
// (SPEC §5, filed), as created is a plan's children's.
const filedKind = "filed"

// followUpsErrKind is the kind of the state error for a pass whose
// follow-ups a failure no later pass gets past keeps from being filed
// (followUpsStuck, SPEC §5.3).
const followUpsErrKind = "followups"

// FollowUpsVisit files the follow-up candidates of a task's passes (SPEC
// §10.2): the loop calls it after ChildrenVisit, so in step 1, after the
// task's commands and reactions. The pass comment is the input, not the
// run's result, so a replay, a restart or a crash between the comment and
// the filing changes nothing: the next visit files what the comment lists.
// A closed issue gets nothing. A held one files as any other does, as its
// passes came before the hold: a follow-up waits at intake for its
// approval all the same, and in a story it's linked and its addendum
// posted too, so the record can stand (SPEC §10.2). A task with a parent
// files its follow-ups under its story, the parent (followUpLink,
// followUpAddendum). One whose parent is in another repo files none, as
// cross-repo work isn't built (SPEC §14): this repo's issue of that number
// isn't its story.
//
// It acts on each clean verify marker of a pass, in a comment
// (markerCommentOf): one in a comment someone else edited isn't read
// (Derive), so a dirty pass comment files nothing. Once a clean filed
// record with the pass's run stands (state.FiledRecord), or a followups
// state error was reported, or acknowledged, under the pass's key
// (followUpsErrKey), it skips the pass before any read or write, so an
// unchanged visit costs nothing. Otherwise it reads the pass's candidates
// back (readVerifyData), which costs no request: a pass with none, as one
// with no verify-data marker of the App's, files nothing, and needs no
// record. Then it describes each follow-up before any write
// (followUpsDescribe), lists the ones filed already (followUpsFind), files
// each one not found, in the pass's order (followUpWrite), and last posts
// the record, which names each, found or filed (followUpsRecord). In a
// story, each follow-up, found or filed, is linked under the story unless
// it's a sub-issue already (followUpLink), then gets its addendum on the
// story unless a clean one of the App's names it (followUpAddenda,
// followUpAddendum), before the next follow-up, so the order is the issue,
// its link, its addendum, and last the record. So each write is checked
// first, and the record stands only once every follow-up does (SPEC §5.2).
// The task is reported to Touched before its first write, the record's or
// the state error's, so it skips the rest of the pass and is read again on
// the next, and so are the story and a follow-up found unlinked, before
// the writes to them. A follow-up filed in this visit isn't one the pass
// has read, so it isn't reported.
//
// A failed read or write is returned, wrapped: Iterate reads the task again
// next pass, and the visit starts again from the reads. A failure no later
// pass gets past, as nothing it reads would change (followUpsStuck), isn't
// returned: data that won't read back, a candidate that can't be written as
// an issue, or a follow-up GitHub refuses to file, or to link under the
// story, with a 422, as for a story with 100 sub-issues or a follow-up
// someone linked under another parent. It files nothing more of that pass,
// and is reported once on the task instead, as a state error keyed by the
// pass comment and the task's latest move (followUpsStuckReport, SPEC §5.3).
// While that error is reported, the pass is skipped, at no cost, until the
// task moves, as by /hold and /resume, which gives the key a new value, and
// the visit tries again.
func (st *Step) FollowUpsVisit(ctx context.Context, s *github.Snapshot) error {
	wrap := func(err error) error {
		return fmt.Errorf("follow-ups of %s/%s#%d: %w", st.Owner, st.Repo, s.Number, err)
	}
	if s.Closed {
		return nil
	}
	d, err := st.Dirty(ctx, s, st.trustOnceTrust())
	if err != nil {
		return wrap(err)
	}
	is := state.Derive(s, st.AppSlug, d.Has)
	for i, m := range is.Markers {
		run := m.Fields["run"]
		if m.Kind != verifyKind || m.Fields["verdict"] != verdictPass || run == "" {
			continue
		}
		pass, ok := markerCommentOf(s, is, i)
		if !ok {
			continue
		}
		if _, done := state.FiledRecord(is, run); done {
			continue
		}
		key := followUpsErrKey(pass.ID, is.TransitionComment)
		if is.Reported(followUpsErrKind, key) {
			logr.Debugf("%s/%s#%d: the follow-ups of pass comment %d wait for it to move, as the state error keyed %s reported why they can't be filed",
				st.Owner, st.Repo, s.Number, pass.ID, key)
			continue
		}
		if s.Parent != 0 && s.ParentRepo != "" {
			logr.Debugf("%s/%s#%d: the follow-ups of pass comment %d wait, as its parent is in %s, and filing them under a story in another repo isn't built",
				st.Owner, st.Repo, s.Number, pass.ID, s.ParentRepo)
			continue
		}
		if err := st.followUpsFile(ctx, s, run, pass, key); err != nil {
			return wrap(err)
		}
	}
	return nil
}

// followUpsErrKey is the key of the followups state error for the pass in
// comment pass, while the task's latest move is in comment move
// (state.Issue.TransitionComment): both IDs, joined by "-". So each pass
// reports it once per stay at a stage, and a move, as by /hold and
// /resume, gives the key a new value (SPEC §5.3).
func followUpsErrKey(pass, move int64) string {
	return strconv.FormatInt(pass, 10) + "-" + strconv.FormatInt(move, 10)
}

// followUpsFile files the follow-ups of run's pass on task s, whose comment
// is pass, then posts its record (FollowUpsVisit). A failure no later pass
// gets past is reported under key (followUpsStuckReport), and isn't
// returned.
func (st *Step) followUpsFile(ctx context.Context, s *github.Snapshot, run string, pass github.Comment, key string) error {
	link := waitLink(st.Owner, st.Repo, s.Number, pass.ID)
	fail := func(err error) error {
		var stuck *followUpsStuck
		if !errors.As(err, &stuck) {
			return fmt.Errorf("pass comment %d: %w", pass.ID, err)
		}
		cause := fmt.Errorf("follow-ups of %s/%s#%d, pass comment %d: %w", st.Owner, st.Repo, s.Number, pass.ID, err)
		if err := st.followUpsStuckReport(ctx, s, link, key, stuck, cause); err != nil {
			return fmt.Errorf("pass comment %d: %w", pass.ID, err)
		}
		return nil
	}
	fs, ok, err := readVerifyData(pass, st.AppSlug)
	if err == nil && (!ok || len(fs) == 0) {
		return nil
	}
	st.touch(s.Number) // the visit writes to it: the record, or the state error
	if err != nil {
		return fail(&followUpsStuck{why: "its candidates don't read back from the pass comment", err: err})
	}
	children, err := followUpsDescribe(s.Number, link, run, fs, st.IssueTypes, st.Scopes, st.Notify)
	if err != nil {
		return fail(err)
	}
	found, err := st.followUpsFind(ctx, s, run, pass.CreatedAt)
	if err != nil {
		return fmt.Errorf("pass comment %d: %w", pass.ID, err)
	}
	var addenda map[int]bool // in a story, the follow-ups with an addendum
	if s.Parent != 0 {
		if addenda, err = st.followUpAddenda(ctx, s.Parent, pass.CreatedAt); err != nil {
			return fmt.Errorf("pass comment %d: %w", pass.ID, err)
		}
	}
	filed := make([]filedChild, len(children))
	for k, c := range children {
		f, ok := found[c.Item]
		if !ok {
			if f, err = st.followUpWrite(ctx, s.Number, run, c); err != nil {
				return fail(err)
			}
		}
		if s.Parent != 0 {
			if err := st.followUpLink(ctx, s, c.Item, f); err != nil {
				return fail(err)
			}
			if !addenda[f.Number] {
				if err := st.followUpAddendum(ctx, s, link, c, f.Number); err != nil {
					return fail(err)
				}
			}
		}
		filed[k] = filedChild{Item: c.Item, Number: f.Number, Title: c.Title}
	}
	if err := st.followUpsRecord(ctx, s, run, link, filed); err != nil {
		return fmt.Errorf("pass comment %d: %w", pass.ID, err)
	}
	return nil
}

// followUpChild is the follow-up of one candidate, as the App files it:
// its place in the pass, F1, F2 and so on, and what CreateChild takes.
// scopeLeftOff is true when the candidate's scope is left off its labels,
// as no longer on config's list, so followUpWrite logs it.
type followUpChild struct {
	Item         string
	Title, Body  string
	Labels       []string
	IssueType    string
	scopeLeftOff bool
	scope        string
}

// followUpsDescribe describes the follow-up of each of fs, run's pass on
// task, whose comment is at link, before any is filed, so a candidate that
// can't be written as an issue stops the pass with nothing filed: one whose
// title is blank once it's on one line (followUpTitle), or whose body is
// over childMaxBody, as a plan's child's may not be. Each follow-up's
// origin marker names task, run and its item, as
// origin verify=#<task> run=<run> item=F<k> (SPEC §5).
func followUpsDescribe(task int, link, run string, fs []verifierFollowUp, issueTypes bool, scopes, notify []string) ([]followUpChild, error) {
	children := make([]followUpChild, 0, len(fs))
	for k, c := range fs {
		item := "F" + strconv.Itoa(k+1)
		title := followUpTitle(c)
		if title == "" {
			return nil, &followUpsStuck{why: fmt.Sprintf("candidate %s has no title an issue can have", item),
				err: fmt.Errorf("candidate %s's title is blank once it's on one line", item)}
		}
		origin, err := github.FormatMarker(github.Marker{Kind: "origin", Fields: map[string]string{
			"verify": "#" + strconv.Itoa(task), "run": run, "item": item}})
		if err != nil {
			return nil, fmt.Errorf("candidate %s's origin marker: %w", item, err)
		}
		body := followUpBody(task, link, item, c, notify, origin)
		if n := textLen(body); n > childMaxBody {
			return nil, &followUpsStuck{why: fmt.Sprintf("the issue of candidate %s would be too long to file", item),
				err: fmt.Errorf("candidate %s's body is %d characters, over %d", item, n, childMaxBody)}
		}
		labels, typ, leftOff := followUpLabels(c, issueTypes, scopes)
		children = append(children, followUpChild{Item: item, Title: title, Body: body, Labels: labels, IssueType: typ,
			scopeLeftOff: leftOff, scope: c.Scope})
	}
	return children, nil
}

// followUpsFind lists the follow-ups of run's pass on task s filed
// already, by item: the repo's issues updated since the pass comment was
// posted, at since, as no follow-up of it can come before it, and for a
// task with a parent, its story, the story's sub-issues too, which say
// whether each is linked under it. A sub-issue in another repo, as inRepo
// tells by its repository_url, is skipped, as its number isn't this
// repo's. Both lists are consistent, unlike search, which lags behind
// writes, so they also hold a follow-up filed just before a crash (SPEC
// §5.2). A follow-up is an issue, open or closed, whose origin the App
// wrote, read from its listed author and body (github.TrustedMarkers,
// state.FollowUpOf), that names the task and run: the first such marker in
// the body counts. So a person's issue with the same marker names nothing,
// and a pull request in the listing is skipped. Should two issues name one
// item, the lower number wins, and the other is logged. A follow-up found
// among the story's sub-issues is Linked.
func (st *Step) followUpsFind(ctx context.Context, s *github.Snapshot, run string, since time.Time) (map[string]childFound, error) {
	task := s.Number
	listed, err := st.Client.IssuesSince(ctx, st.Owner, st.Repo, since)
	if err != nil {
		return nil, fmt.Errorf("find the follow-ups filed: %w", err)
	}
	listed = slices.Clone(listed)
	linked := map[int]bool{}
	if s.Parent != 0 {
		subs, err := st.Client.SubIssues(ctx, st.Owner, st.Repo, s.Parent)
		if err != nil {
			return nil, fmt.Errorf("find the follow-ups filed under #%d: %w", s.Parent, err)
		}
		for _, is := range subs {
			if st.inRepo(is.GetRepositoryURL()) {
				linked[is.GetNumber()] = true
				listed = append(listed, is)
			}
		}
	}
	slices.SortStableFunc(listed, func(a, b *gogithub.Issue) int { return cmp.Compare(a.GetNumber(), b.GetNumber()) })
	found := map[string]childFound{}
	seen := map[int]bool{} // one both lists hold is read once
	for _, l := range listed {
		n := l.GetNumber()
		if seen[n] || l.IsPullRequest() {
			continue
		}
		seen[n] = true
		author := github.Author{Login: l.GetUser().GetLogin(), Type: l.GetUser().GetType()}
		for _, m := range github.TrustedMarkers(author, l.GetBody(), st.AppSlug) {
			verify, r, item, ok := state.FollowUpOf(m)
			if !ok {
				continue
			}
			if verify == task && r == run {
				if had, ok := found[item]; ok {
					logr.Warnf("%s/%s: #%d and #%d are both the follow-up of candidate %s of #%d's pass in run %s, so #%d is the one kept",
						st.Owner, st.Repo, had.Number, n, item, task, run, had.Number)
				} else {
					found[item] = childFound{Number: n, ID: l.GetID(), Linked: linked[n], Closed: l.GetState() == "closed"}
				}
			}
			break
		}
	}
	return found, nil
}

// followUpWrite files c, the follow-up of a candidate of run's pass on task,
// as the App, with its title, body, labels and issue type at once
// (CreateChild), and returns it, by its number and ID, linked under no parent
// yet. A scope left off its labels is logged as a warning. A 422 is a failure
// no later pass gets past (childRefused), as for an issue type the org
// doesn't have.
func (st *Step) followUpWrite(ctx context.Context, task int, run string, c followUpChild) (childFound, error) {
	is, err := st.Client.CreateChild(ctx, st.Owner, st.Repo, github.NewChild{Title: c.Title, Body: c.Body, Labels: c.Labels, Type: c.IssueType})
	if err != nil {
		if childRefused(err) {
			return childFound{}, &followUpsStuck{why: fmt.Sprintf("GitHub refused to file the issue of candidate %s", c.Item),
				err: fmt.Errorf("file candidate %s: %w", c.Item, err)}
		}
		return childFound{}, fmt.Errorf("file candidate %s: %w", c.Item, err)
	}
	n := is.GetNumber()
	logr.Infof("%s/%s#%d: filed #%d for candidate %s of run %s's pass", st.Owner, st.Repo, task, n, c.Item, run)
	if c.scopeLeftOff {
		logr.Warnf("%s/%s#%d: filed #%d for candidate %s of run %s's pass without a scope label, as its scope %q isn't on config's list any more",
			st.Owner, st.Repo, task, n, c.Item, run, c.scope)
	}
	return childFound{Number: n, ID: is.GetID()}, nil
}

// followUpLink links f, the follow-up of candidate item of a pass on task
// s, under s's parent, its story (SPEC §10.2), unless it's a sub-issue of
// the story already (followUpsFind). The story and f are reported to
// Touched first, as the link moves their updated_at. A 422 is a failure no
// later pass gets past (childRefused), as for a story with 100 sub-issues,
// GitHub's cap, or a follow-up someone linked under another parent.
func (st *Step) followUpLink(ctx context.Context, s *github.Snapshot, item string, f childFound) error {
	if f.Linked {
		return nil
	}
	st.touch(s.Parent)
	st.touch(f.Number)
	if err := st.Client.AddSubIssue(ctx, st.Owner, st.Repo, s.Parent, f.ID); err != nil {
		if childRefused(err) {
			return &followUpsStuck{why: fmt.Sprintf("GitHub refused to link #%d, the follow-up of candidate %s, under #%d, its story", f.Number, item, s.Parent),
				err: fmt.Errorf("link #%d, the follow-up of candidate %s, under #%d: %w", f.Number, item, s.Parent, err)}
		}
		return fmt.Errorf("link #%d, the follow-up of candidate %s, under #%d: %w", f.Number, item, s.Parent, err)
	}
	logr.Infof("%s/%s#%d: linked #%d, the follow-up of candidate %s, under its story #%d", st.Owner, st.Repo, s.Number, f.Number, item, s.Parent)
	return nil
}

// addendumKind is the kind of the marker on the App's addendum comment on
// a story, which names a follow-up filed under it (SPEC §5, addendum).
const addendumKind = "addendum"

// addendumOf is the follow-up an addendum marker names, by its child=, or
// false for another marker, or a child= that isn't a number above 0.
func addendumOf(m github.Marker) (int, bool) {
	n, err := strconv.Atoi(m.Fields["child"])
	if m.Kind != addendumKind || err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// followUpAddenda reads story's comments updated since the pass comment
// was posted, at since, as no addendum of the pass's follow-ups can come
// before it (Client.CommentsSince), and returns the follow-ups a clean
// addendum names, by number: a comment the App wrote, with an addendum
// marker, never edited. The App never edits an addendum, so an edit is
// someone else's, or the App's note on content someone else edited (SPEC
// §5.1): that addendum counts for nothing, so another is posted. A listing
// names no editor, but an edit moves a comment's updated_at past its
// created_at, so one whose two times differ is edited.
func (st *Step) followUpAddenda(ctx context.Context, story int, since time.Time) (map[int]bool, error) {
	comments, err := st.Client.CommentsSince(ctx, st.Owner, st.Repo, story, since)
	if err != nil {
		return nil, fmt.Errorf("find the addenda on #%d: %w", story, err)
	}
	has := map[int]bool{}
	for _, c := range comments {
		if !c.GetUpdatedAt().Equal(c.GetCreatedAt()) {
			continue
		}
		author := github.Author{Login: c.GetUser().GetLogin(), Type: c.GetUser().GetType()}
		for _, m := range github.TrustedMarkers(author, c.GetBody(), st.AppSlug) {
			if n, ok := addendumOf(m); ok {
				has[n] = true
			}
		}
	}
	return has, nil
}

// followUpAddendum posts the addendum of child, the follow-up of c, a
// candidate of the pass on task s whose comment is at link, on s's parent,
// its story (SPEC §10.2): addendumText, then the marker
// addendum child=<child>, which followUpAddendumOf reads back. The story is
// reported to Touched first, as the comment moves its updated_at.
func (st *Step) followUpAddendum(ctx context.Context, s *github.Snapshot, link string, c followUpChild, child int) error {
	marker, err := github.FormatMarker(github.Marker{Kind: addendumKind, Fields: map[string]string{"child": strconv.Itoa(child)}})
	if err != nil {
		return fmt.Errorf("post #%d's addendum: %w", child, err)
	}
	st.touch(s.Parent)
	if err := st.comment(ctx, &github.Snapshot{Number: s.Parent}, join(addendumText(child, s.Number, link, c.Item, c.Title), marker)); err != nil {
		return fmt.Errorf("post #%d's addendum: %w", child, err)
	}
	logr.Infof("%s/%s#%d: posted the addendum of #%d, the follow-up of candidate %s, on its story #%d", st.Owner, st.Repo, s.Number, child, c.Item, s.Parent)
	return nil
}

// followUpsRecord posts the filed record of run's pass on task s, whose
// comment is at link (SPEC §5, filed): one App comment listing each of
// filed, in the pass's order (filedText), then the marker
// filed run=<run> children=<n>,<n>, which state.FiledRecord reads back.
func (st *Step) followUpsRecord(ctx context.Context, s *github.Snapshot, run, link string, filed []filedChild) error {
	marker, err := github.FormatMarker(github.Marker{Kind: filedKind, Fields: map[string]string{"run": run, "children": filedChildren(filed)}})
	if err != nil {
		return fmt.Errorf("record the follow-ups: %w", err)
	}
	if err := st.comment(ctx, s, join(filedText(link, filed), marker)); err != nil {
		return fmt.Errorf("record the follow-ups: %w", err)
	}
	logr.Infof("%s/%s#%d: recorded the follow-ups of run %s's pass: %s", st.Owner, st.Repo, s.Number, run, filedChildren(filed))
	return nil
}

// followUpsStuck is a failure filing a pass's follow-ups that no later pass
// gets past, as nothing it reads would change: why says what failed, in the
// daemon's own words, for the state error, and err is the cause, which may
// quote the verifier's text or GitHub's, so it's only logged (SPEC §5.3,
// What it quotes), as childrenStuck's is.
type followUpsStuck struct {
	why string
	err error
}

func (e *followUpsStuck) Error() string { return e.why + ": " + e.err.Error() }

func (e *followUpsStuck) Unwrap() error { return e.err }

// followUpsStuckReport posts the followups state error for stuck on task s,
// keyed by key, the pass comment's and the task's latest move's
// (followUpsErrKey). FollowUpsVisit calls it only while none was posted or
// acknowledged with that key (state.Issue.Reported). Its why is the
// daemon's text alone, which names a candidate by its place in the pass,
// never by its text, and link is the pass comment's, so it quotes nothing
// of the pass's data or of an error, which may carry it. cause, the full
// error, is logged as an error as the comment is posted, and the comment
// says the log has it, as childrenStuckReport's does.
func (st *Step) followUpsStuckReport(ctx context.Context, s *github.Snapshot, link, key string, stuck *followUpsStuck, cause error) error {
	marker, err := github.FormatMarker(github.Marker{Kind: "error", Fields: map[string]string{"kind": followUpsErrKind, "key": key}})
	if err != nil {
		return fmt.Errorf("report why the follow-ups can't be filed: %w", err)
	}
	text := fmt.Sprintf("I can't file the follow-ups of [this pass](%s), as %s, so I've stopped filing them. "+
		"I won't try again until this issue moves, as by `/hold` and `/resume`. The daemon's log has the error.", link, stuck.why)
	if err := st.comment(ctx, s, join(github.Escape(text), marker)); err != nil {
		return fmt.Errorf("report why the follow-ups can't be filed: %w", err)
	}
	logr.Errorf("state error: %v", cause)
	return nil
}
