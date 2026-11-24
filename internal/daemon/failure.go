package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file files a failure on a story branch (SPEC §7.5, A failure on the
// story branch): step 4's checks on a tip that fails file one human task
// for rule 2, or one bug for any other rule, under the story, before they
// post the tip's check (Runs.tipFile), and a later failing tip comments on
// a failure's bug that's still open instead. Each issue carries the App's
// origin story=#<S> tip=<SHA>, found again by it before any filing (SPEC
// §5, Story branch failures), so a replay files none twice.

// errFailureRefused is GitHub's 422 on a write of a failure's filing, the
// issue's create or its link under the story, as for a story with 100
// sub-issues, GitHub's cap: no later pass gets past it, so it's logged
// once, and the tip's check is posted anyway (Runs.tipFile).
var errFailureRefused = errors.New("GitHub refused to file it")

// failureKind is the kind of the marker on the App's comment on an open
// failure's bug that names a later failing tip of its story branch (SPEC
// §5, failure): failure tip=<SHA>.
const failureKind = "failure"

// failureSpec is one failure on a story branch to file (Daemon.failureFile):
// Story, the story whose branch, Branch, failed at Tip; Human for the human
// task rule 2's failure files, else a bug; Main, for a conflict bug, the
// main commit that didn't merge into Tip, which its origin names, and
// Files, the files that conflicted, which its Body names; and the issue's
// Title and Body, without its origin marker, escaped (failure_render.go).
type failureSpec struct {
	Story       *github.Snapshot
	Branch      string
	Tip, Main   string
	Human       bool
	Title, Body string
	Files       []string
}

// what names f's issue in the log and in its first move's comment.
func (f failureSpec) what() string {
	switch {
	case f.Human:
		return "the human task for its branch's failing tip " + f.Tip
	case f.Main != "":
		return fmt.Sprintf("the conflict bug for its branch's tip %s and main's %s, in %d files", f.Tip, f.Main, len(f.Files))
	}
	return "the bug for its branch's failing tip " + f.Tip
}

// stage is f's issue's first stage (SPEC §7.2): needs-human for a human
// task, ready for a bug.
func (f failureSpec) stage() state.Stage {
	if f.Human {
		return state.NeedsHuman
	}
	return state.Ready
}

// failureKey is the key of the first move of an issue filed for the
// failing tip tip (SPEC §5.2): event=t<SHA>.
func failureKey(tip string) state.Key {
	return state.Key{Field: "event", ID: "t" + tip}
}

// failureNames reports whether body, an issue's body by author, holds the
// App's origin of a failure on story's branch at tip: the first story
// branch failure's origin among its markers the App wrote
// (github.TrustedMarkers, state.FailureOf) names both. So a person's issue
// with the same marker names nothing.
func failureNames(author github.Author, body, appSlug string, story int, tip string) bool {
	for _, m := range github.TrustedMarkers(author, body, appSlug) {
		if s, t, _, ok := state.FailureOf(m); ok {
			return s == story && t == tip
		}
	}
	return false
}

// failureFind finds the issue filed for story's branch's failing tip tip,
// open or closed (SPEC §5, Story branch failures): one whose body, as
// listed, holds the App's origin naming the story and the tip
// (failureNames), among the story's sub-issues, one request, which hold
// every one linked, and the snapshots the daemon holds (Daemon.snaps),
// which hold one filed but not yet linked when a pass stopped. A sub-issue
// in another repo, and a pull request, are skipped. A sub-issue is Linked,
// and so is a held one whose snapshot names the story as its parent.
// Should two issues name the tip, the lower number wins, and that's
// logged. A body someone edited still counts while it keeps the App's
// marker, as reusing such an issue is safer than filing a second one.
func (d *Daemon) failureFind(ctx context.Context, story *github.Snapshot, tip string) (childFound, bool, error) {
	a, n := d.Step.Applier, story.Number
	subs, err := a.Client.SubIssues(ctx, a.Owner, a.Repo, n)
	if err != nil {
		return childFound{}, false, fmt.Errorf("find the issue filed for its branch's tip %s: %w", tip, err)
	}
	var found []childFound
	seen := map[int]bool{}
	for _, is := range subs {
		if !a.inRepo(is.GetRepositoryURL()) || is.IsPullRequest() {
			continue
		}
		seen[is.GetNumber()] = true
		author := github.Author{Login: is.GetUser().GetLogin(), Type: is.GetUser().GetType()}
		if failureNames(author, is.GetBody(), a.AppSlug, n, tip) {
			found = append(found, childFound{Number: is.GetNumber(), ID: is.GetID(), Linked: true, Closed: is.GetState() == "closed"})
		}
	}
	for _, m := range slices.Sorted(maps.Keys(d.snaps)) {
		s := d.snaps[m]
		if s == nil || seen[m] || !failureNames(s.Author, s.Body, a.AppSlug, n, tip) {
			continue
		}
		found = append(found, childFound{Number: m, Linked: s.Parent == n && s.ParentRepo == "", Closed: s.Closed})
	}
	if len(found) == 0 {
		return childFound{}, false, nil
	}
	slices.SortFunc(found, func(x, y childFound) int { return x.Number - y.Number })
	for _, other := range found[1:] {
		logr.Warnf("%s/%s#%d: #%d and #%d were both filed for its branch's failing tip %s, so #%d is the one kept",
			a.Owner, a.Repo, n, found[0].Number, other.Number, tip, found[0].Number)
	}
	return found[0], true, nil
}

// failureFile files f, a failure on its story's branch (SPEC §7.5), and
// returns its issue's number. Each write is checked first, so a replay, a
// restart or a crash between any two repeats none and loses none (SPEC
// §5.2):
//
//   - the issue: one found for the tip already (failureFind) is reused,
//     open or closed. Otherwise the App files it at once with its title,
//     its body, then its origin marker, origin story=#<S> tip=<SHA>, with
//     human=1 on a human task and main=<SHA> on a conflict bug, its type,
//     as type:bug or type:task, or the issue type on a repo that uses
//     them, the story's priority:* label, and its first stage's label, as
//     a plan's child is filed (CreateChild). A closed one found gets
//     nothing more;
//   - its link under the story, unless it's a sub-issue of it already. One
//     the daemon holds isn't listed with its ID, so that's read first;
//   - its first move, intake → ready for a bug, or intake → needs-human
//     for a human task, on TriggerCreated, keyed event=t<SHA> by the
//     failing tip (failureKey), as a plan's child's is written (firstMove).
//
// Each issue it writes to, the story among them, is read again next pass
// (Daemon.reread). A 422 on the create or the link is errFailureRefused,
// wrapped, as is a body over childMaxBody, which GitHub would refuse.
func (d *Daemon) failureFile(ctx context.Context, f failureSpec) (int, error) {
	a, n := d.Step.Applier, f.Story.Number
	wrap := func(err error) error { return fmt.Errorf("file %s: %w", f.what(), err) }
	found, ok, err := d.failureFind(ctx, f.Story, f.Tip)
	if err != nil {
		return 0, wrap(err)
	}
	var child *github.Snapshot // the issue filed in this pass, as the create answered
	if !ok {
		c, err := failureChild(f, a.IssueTypes)
		if err != nil {
			return 0, wrap(err)
		}
		is, err := a.Client.CreateChild(ctx, a.Owner, a.Repo, c)
		if err != nil {
			if childRefused(err) {
				return 0, wrap(fmt.Errorf("%w: %w", errFailureRefused, err))
			}
			return 0, wrap(err)
		}
		found = childFound{Number: is.GetNumber(), ID: is.GetID()}
		d.reread(found.Number, false)
		child = childCreated(is, n)
		logr.Infof("%s/%s#%d: filed #%d, %s", a.Owner, a.Repo, n, found.Number, f.what())
	}
	if found.Closed {
		// Closed, as by its PR's merge or a person, it's done with: it's
		// reused as it is, with no link and no first move.
		logr.Infof("%s/%s#%d: #%d, %s, is closed, so it's left as it is", a.Owner, a.Repo, n, found.Number, f.what())
		return found.Number, nil
	}
	if !found.Linked {
		if found.ID == 0 {
			listed, _, err := a.Client.Issues.Get(ctx, a.Owner, a.Repo, found.Number)
			if err != nil {
				return 0, wrap(fmt.Errorf("read #%d's ID: %w", found.Number, err))
			}
			found.ID = listed.GetID()
		}
		d.reread(n, false)
		d.reread(found.Number, false)
		if err := a.Client.AddSubIssue(ctx, a.Owner, a.Repo, n, found.ID); err != nil {
			err = fmt.Errorf("link #%d: %w", found.Number, err)
			if childRefused(err) {
				return 0, wrap(fmt.Errorf("%w: %w", errFailureRefused, err))
			}
			return 0, wrap(err)
		}
		logr.Infof("%s/%s#%d: linked #%d under it, %s", a.Owner, a.Repo, n, found.Number, f.what())
	}
	d.reread(found.Number, false)
	created := fmt.Sprintf("Filed for story #%d, whose branch %s fails its checks at `%s`.", n, checkCode(f.Branch), f.Tip)
	if err := d.Step.firstMove(ctx, n, found.Number, f.what(), f.stage(), failureKey(f.Tip), created, child); err != nil {
		return 0, wrap(err)
	}
	return found.Number, nil
}

// failureChild is what CreateChild files for f (failureFile): its title
// and body, the body ending in its origin marker, its labels and its issue
// type, on a repo that uses them, where the type label is left off.
func failureChild(f failureSpec, issueTypes bool) (github.NewChild, error) {
	fields := map[string]string{"story": fmt.Sprintf("#%d", f.Story.Number), "tip": f.Tip}
	typ := "bug"
	if f.Human {
		fields["human"], typ = "1", "task"
	}
	if f.Main != "" {
		fields["main"] = f.Main
	}
	origin, err := github.FormatMarker(github.Marker{Kind: "origin", Fields: fields})
	if err != nil {
		return github.NewChild{}, fmt.Errorf("its origin marker: %w", err)
	}
	body := f.Body + "\n\n" + origin
	if n := textLen(body); n > childMaxBody {
		return github.NewChild{}, fmt.Errorf("%w: its body is %d characters, over %d", errFailureRefused, n, childMaxBody)
	}
	c := github.NewChild{Title: f.Title, Body: body}
	if issueTypes {
		c.Type = strings.ToUpper(typ[:1]) + typ[1:]
	} else {
		c.Labels = append(c.Labels, "type:"+typ)
	}
	stage, _ := f.stage().Label()
	c.Labels = append(c.Labels, fmt.Sprintf("priority:p%d", priority(f.Story)), stage)
	return c, nil
}

// failureHeldOrigin is the Origin Derive reads from s, a snapshot the
// daemon holds, at no request: an App comment someone else edited is
// dirty, as only the App may edit one (SPEC §5.1), so a marker in it
// counts for nothing, and Derive reads the body's markers only while the
// App made its last edit, or nobody did.
func failureHeldOrigin(s *github.Snapshot, appSlug string) state.Origin {
	return state.Derive(s, appSlug, failureEdited(s, appSlug)).Origin
}

// failureEdited is the dirty check failureHeldOrigin and failureComment
// read s with: a comment someone other than the App edited.
func failureEdited(s *github.Snapshot, appSlug string) func(int64) bool {
	edited := map[int64]bool{}
	for _, c := range s.Comments {
		if !c.Edit.At.IsZero() && !state.IsApp(c.Edit.By, appSlug) {
			edited[c.ID] = true
		}
	}
	return func(id int64) bool { return edited[id] }
}

// failureOpenBug is an open failure's bug of story (SPEC §7.5, When the
// branch moves under a failure's bug), from the snapshots the daemon holds,
// at no request, or false for none: an open bug held below story, whose
// App origin names a failure on story's branch, a conflict bug's too, at a
// tip other than tip, as one filed for tip itself is its own filing, which
// failureFile finishes. One touched this pass, as by a move or a close in
// step 2, such as its close on its PR's merge, isn't one, as its snapshot
// is out of date. Of several, the lowest number is.
func (d *Daemon) failureOpenBug(story int, tip string) (*github.Snapshot, bool) {
	slug := d.Step.AppSlug
	for _, n := range slices.Sorted(maps.Keys(d.snaps)) {
		s := d.snaps[n]
		if s == nil || s.Closed || d.stale[n] || s.Parent != story || s.ParentRepo != "" || s.Type != "bug" {
			continue
		}
		if o := failureHeldOrigin(s, slug); o.Story == story && o.Tip != tip {
			return s, true
		}
	}
	return nil, false
}

// failureComment comments text, escaped already, on bug, an open failure's
// bug of its story, for tip, a later failing tip of its branch (SPEC §7.5,
// When the branch moves under a failure's bug), then the marker
// failure tip=<SHA>. It's checked first: a clean App comment on bug with
// that marker, as bug holds it, means it's posted, and nothing is. bug is
// read again next pass.
func (d *Daemon) failureComment(ctx context.Context, bug *github.Snapshot, tip, text string) error {
	a := d.Step.Applier
	edited := failureEdited(bug, a.AppSlug)
	for _, c := range bug.Comments {
		if edited(c.ID) {
			continue
		}
		for _, m := range github.TrustedMarkers(c.Author, c.Body, a.AppSlug) {
			if m.Kind == failureKind && m.Fields["tip"] == tip {
				logr.Debugf("%s/%s#%d: comment %d names its story branch's failing tip %s already", a.Owner, a.Repo, bug.Number, c.ID, tip)
				return nil
			}
		}
	}
	marker, err := github.FormatMarker(github.Marker{Kind: failureKind, Fields: map[string]string{"tip": tip}})
	if err != nil {
		return fmt.Errorf("comment the failing tip %s on #%d: %w", tip, bug.Number, err)
	}
	d.reread(bug.Number, false)
	if err := a.comment(ctx, bug, join(text, marker)); err != nil {
		return fmt.Errorf("comment the failing tip %s on #%d: %w", tip, bug.Number, err)
	}
	logr.Infof("%s/%s#%d: commented its story branch's failing tip %s on it, as it's that branch's open failure's bug", a.Owner, a.Repo, bug.Number, tip)
	return nil
}
