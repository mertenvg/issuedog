package daemon

import (
	"context"
	"fmt"
	"slices"
	"strings"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file is the text in the planner's trusted context files that
// isn't the issue's own conversation (#312): what's shown of the issue's
// and its parent's titles and bodies, past the admission check
// (contextAdmission), and the parent's type and the stages and types of
// the sub-issues children.md and parent.md list (#445). The sub-issues'
// titles are data, as the preamble says (plannerPreamble): anyone who can
// write to the repo can rename one. The interviewer's issue.md shares the
// issue's title as of its last trusted change (plannerTrustedTitleNote).

// plannerTrustedAppBody reports whether s's body is the App's text, so its
// markers are left out rather than escaped: the App made its last edit, as
// when it rewrote the body as a spec, whoever filed the issue, or nobody
// edited it and the App filed it. A trusted user's edit leaves the body
// clean, but it isn't the App's text (SPEC §5.1, "Body markers").
func plannerTrustedAppBody(s *github.Snapshot, appSlug string) bool {
	if e := s.BodyEdit; !e.At.IsZero() {
		return state.IsApp(e.By, appSlug)
	}
	return state.IsApp(s.Author, appSlug)
}

// plannerTrustedTitle returns the title of issue s as of its last trusted
// change, and who renamed it since, if anyone, for an issue the App or a
// trusted author filed, whose renames §6.2's admission check doesn't cover
// (#312): a write collaborator who isn't trusted can rename it. That holds
// the issue for discussion (SPEC §6.4), but a /resume doesn't make the
// title trusted. renames are its renamed events, oldest first:
//   - the latest stands if the App or a trusted user made it, and then the
//     title is s's own, as with no rename;
//   - else the title is the one the newest rename by the App or a trusted
//     user set, or with none, the one the oldest rename replaced: the title
//     the issue was filed with.
//
// An untrusted author's title is contextAdmission's to judge, so it's s's
// own here. by is nil when nobody untrusted renamed it since, and else the
// renamer, whose login is "" for a ghost, which ivLogin names "someone
// unnamed", never a login like "unknown". It reads trust only, through tc.
func plannerTrustedTitle(ctx context.Context, appSlug string, tc trustCache, s *github.Snapshot, renames []github.DiscRename) (title string, by *github.Author, err error) {
	trusted := func(a github.Author) (bool, error) {
		if state.IsApp(a, appSlug) {
			return true, nil
		}
		return tc.trusted(ctx, a)
	}
	ok, err := trusted(s.Author)
	if err != nil {
		return "", nil, fmt.Errorf("the issue's author: %w", err)
	}
	if !ok || len(renames) == 0 {
		return s.Title, nil, nil
	}
	last := renames[len(renames)-1]
	if ok, err = trusted(last.By); err != nil {
		return "", nil, fmt.Errorf("who renamed the issue: %w", err)
	}
	if ok {
		return s.Title, nil, nil
	}
	for _, r := range slices.Backward(renames[:len(renames)-1]) {
		if ok, err = trusted(r.By); err != nil {
			return "", nil, fmt.Errorf("who renamed the issue: %w", err)
		}
		if ok {
			return r.Title, &last.By, nil
		}
	}
	return renames[0].From, &last.By, nil
}

// plannerTrustedTitleNote sets t's title note for issue s, as step 1 read
// it, if someone untrusted renamed it since its last trusted change
// (plannerTrustedTitle): issue.md shows that title instead, and the new
// one goes first in untrusted.md if untrusted is set, as the stage
// collects untrusted input (SPEC §6.2). The planner's issue.md and the
// interviewer's share it. s's renames are the ones CheckLabels set from
// the timeline it reads anyway (Snapshot.DiscRenames), so it costs no
// request. It reads trust only, through tc.
func plannerTrustedTitleNote(ctx context.Context, appSlug string, tc trustCache, s *github.Snapshot, untrusted bool, t *contextText) error {
	title, by, err := plannerTrustedTitle(ctx, appSlug, tc, s, s.DiscRenames)
	if err != nil {
		return fmt.Errorf("its last trusted title: %w", err)
	}
	if by == nil {
		return nil
	}
	t.TitleNote = plannerTrustedRenamed(title, by.Login, untrusted)
	if untrusted {
		t.Untrusted = append([]string{ivUntrusted("the issue's title as renamed by "+ivLogin(by.Login)+", who isn't trusted", s.Title)}, t.Untrusted...)
	}
	return nil
}

// plannerTrustedRenames are timeline's renamed events, oldest first, as
// CheckLabels gives a snapshot's (Snapshot.DiscRenames).
func plannerTrustedRenames(timeline []*gogithub.Timeline) []github.DiscRename {
	var renames []github.DiscRename
	for _, e := range timeline {
		if e.GetEvent() != "renamed" {
			continue
		}
		renames = append(renames, github.DiscRename{ID: e.GetID(), By: github.Author{Login: e.GetActor().GetLogin(), Type: e.GetActor().GetType()},
			At: e.GetCreatedAt().Time, Title: e.GetRename().GetTo(), From: e.GetRename().GetFrom()})
	}
	return renames
}

// plannerTrustedRenamed is the title line of an issue whose latest rename
// was by someone untrusted, by: the title as of its last trusted change
// (plannerTrustedTitle), escaped, on one line, with a note saying where
// the new title is: in untrusted.md if untrusted, else left out.
func plannerTrustedRenamed(title, by string, untrusted bool) string {
	where := "so the new title is left out"
	if untrusted {
		where = "so the new title is in untrusted.md, as untrusted data"
	}
	return fmt.Sprintf("%s (%s, who isn't trusted, renamed it since, %s)", github.Escape(strings.Join(strings.Fields(title), " ")), ivLogin(by), where)
}

// plannerTrustedParent is what parent.md shows of the parent's title and
// body, as for the issue's own (SPEC §6.2, #312).
type plannerTrustedParent struct {
	// TitleBy and BodyBy are who changed the title and body of an untrusted
	// author's parent after it was admitted, or "", and Never is set when it
	// never was, which leaves both out (contextAdmission).
	TitleBy, BodyBy string
	Never           bool
	// Title is the title as of its last trusted change, and RenamedBy who
	// renamed it since, if anyone, of a parent the App or a trusted author
	// filed (plannerTrustedTitle).
	Title     string
	RenamedBy *github.Author
}

// plannerTrustedType returns the type of an issue typed by label, with
// labels, as the label rules judge it (SPEC §5.1): the first of its type:*
// labels whose latest change that stands is an add, the App's or a
// trusted user's (github.StandingLabelChange), or "" if none is, as for a
// type:* label someone untrusted added, which step 1 hasn't reverted yet,
// or never visits, on a closed issue (#445). So an untrusted type never
// shows as trusted text. It reads the issue's timeline, through tl, only
// if labels has a type:* label.
func plannerTrustedType(ctx context.Context, a *Applier, trust github.Trust, labels []string, tl *ivTimeline) (string, error) {
	for _, l := range labels {
		name, ok := strings.CutPrefix(strings.ToLower(l), "type:")
		if !ok {
			continue
		}
		timeline, err := tl.get(ctx)
		if err != nil {
			return "", fmt.Errorf("the type of #%d: %w", tl.number, err)
		}
		e, err := github.StandingLabelChange(ctx, timeline, l, a.AppSlug, trust)
		if err != nil {
			return "", fmt.Errorf("the type of #%d: %w", tl.number, err)
		}
		if e.GetEvent() == "labeled" {
			return name, nil
		}
	}
	return "", nil
}

// plannerListedType is the type of issue is as a listing gives it, for
// plannerSub and planChild: its issue type, in lower case, or with none,
// "" and its labels, whose type plannerTrustedType judges, as any of them
// may be an untrusted type:* label (#445).
func plannerListedType(is *gogithub.Issue) (string, []string) {
	if t := is.GetType().GetName(); t != "" {
		return strings.ToLower(t), nil
	}
	labels := make([]string, 0, len(is.Labels))
	for _, l := range is.Labels {
		labels = append(labels, l.GetName())
	}
	return "", labels
}

// plannerTrustedOpen returns open issue n's stage as state.Derive reads it
// from its markers (SPEC §5.1), not from its stage label, which may be an
// untrusted one step 1 hasn't reverted yet (#312), or "" if no transition
// moved it, as for an issue that never entered the pipeline, and its type.
// It takes the snapshot the daemon holds of n, if step 1 succeeded on it
// and nothing had it read again since (Daemon.inputs), which costs no
// request but for its edits' history (Dirty), and whose type is the one
// step 1 left it, with an untrusted type:* label reverted or left out.
// Else it reads n, as in a test that calls Context on its own, and judges
// a type by label from n's timeline (plannerTrustedType, #445).
func plannerTrustedOpen(ctx context.Context, d *Daemon, n int) (state.Stage, string, error) {
	a := d.Step.Applier
	s := d.snaps[n]
	_, held := d.inputs[n]
	if !held || s == nil {
		held = false
		var err error
		if s, err = a.Client.Snapshot(ctx, a.Owner, a.Repo, n); err != nil {
			return "", "", fmt.Errorf("the stage of #%d: %w", n, err)
		}
	}
	dirt, err := a.Dirty(ctx, s, d.Step.Trust)
	if err != nil {
		return "", "", fmt.Errorf("the stage of #%d: %w", n, err)
	}
	typ := s.Type
	if !held && s.TypeFromLabels {
		if typ, err = plannerTrustedType(ctx, a, d.Step.Trust, s.Labels, &ivTimeline{a: a, number: n}); err != nil {
			return "", "", fmt.Errorf("a snapshot step 1 doesn't hold: %w", err)
		}
	}
	is := state.Derive(s, a.AppSlug, dirt.Has)
	moved := slices.ContainsFunc(is.Markers, func(m github.Marker) bool {
		to := state.Stage(m.Fields["to"])
		return m.Kind == "transition" && (to.Open() || to == state.Closed)
	})
	if !moved {
		return "", typ, nil
	}
	return is.Stage, typ, nil
}

// plannerTrustedSub returns the stage and type of sub-issue n of this repo:
// an open one's from plannerTrustedOpen, and a closed one's type, from what
// the listing gave, typ and labels (plannerListedType), judged as
// plannerTrustedType does, with no stage, as its line gives its state.
func plannerTrustedSub(ctx context.Context, d *Daemon, n int, open bool, typ string, labels []string) (state.Stage, string, error) {
	if open {
		stage, typ, err := plannerTrustedOpen(ctx, d, n)
		if err != nil {
			return "", "", fmt.Errorf("an open sub-issue: %w", err)
		}
		return stage, typ, nil
	}
	if labels == nil {
		return "", typ, nil
	}
	a := d.Step.Applier
	typ, err := plannerTrustedType(ctx, a, d.Step.Trust, labels, &ivTimeline{a: a, number: n})
	if err != nil {
		return "", "", fmt.Errorf("a closed sub-issue's labels: %w", err)
	}
	return "", typ, nil
}

// plannerTrustedSubs sets the Stage and Type of each of subs in this repo,
// but issue skip, which no list shows (plannerList), as plannerTrustedSub
// reads them. One in another repo gets neither: its labels aren't this
// repo's to judge, so its line gives its state, and says its type isn't
// read.
func plannerTrustedSubs(ctx context.Context, d *Daemon, subs []plannerSub, skip int) error {
	for i, sub := range subs {
		if sub.Repo != "" || sub.Number == skip {
			continue
		}
		var err error
		if subs[i].Stage, subs[i].Type, err = plannerTrustedSub(ctx, d, sub.Number, sub.State == "open", sub.Type, sub.Labels); err != nil {
			return err
		}
	}
	return nil
}
