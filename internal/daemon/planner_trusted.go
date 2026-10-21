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
// (contextAdmission), and the stages of the sub-issues children.md and
// parent.md list. The sub-issues' titles are data, as the preamble says
// (plannerPreamble): anyone who can write to the repo can rename one.

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
// own here. It reads trust only, through tc.
func plannerTrustedTitle(ctx context.Context, appSlug string, tc trustCache, s *github.Snapshot, renames []github.DiscRename) (title, by string, err error) {
	trusted := func(a github.Author) (bool, error) {
		if state.IsApp(a, appSlug) {
			return true, nil
		}
		return tc.trusted(ctx, a)
	}
	ok, err := trusted(s.Author)
	if err != nil {
		return "", "", fmt.Errorf("the issue's author: %w", err)
	}
	if !ok || len(renames) == 0 {
		return s.Title, "", nil
	}
	last := renames[len(renames)-1]
	if ok, err = trusted(last.By); err != nil {
		return "", "", fmt.Errorf("who renamed the issue: %w", err)
	}
	if ok {
		return s.Title, "", nil
	}
	for _, r := range slices.Backward(renames[:len(renames)-1]) {
		if ok, err = trusted(r.By); err != nil {
			return "", "", fmt.Errorf("who renamed the issue: %w", err)
		}
		if ok {
			return r.Title, login(last.By), nil
		}
	}
	return renames[0].From, login(last.By), nil
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
	Title, RenamedBy string
}

// plannerTrustedStage returns issue n's stage as state.Derive reads it from
// its markers (SPEC §5.1), not from its stage label, which may be an
// untrusted one step 1 hasn't reverted yet (#312), or "" if no transition
// moved it, as for an issue that never entered the pipeline. It takes the
// snapshot the daemon holds of n, if step 1 succeeded on it and nothing
// had it read again since (Daemon.inputs), which costs no request but for
// its edits' history (Dirty), or else reads n, as in a test that calls
// Context on its own.
func plannerTrustedStage(ctx context.Context, d *Daemon, n int) (state.Stage, error) {
	a := d.Step.Applier
	s := d.snaps[n]
	if _, ok := d.inputs[n]; !ok || s == nil {
		var err error
		if s, err = a.Client.Snapshot(ctx, a.Owner, a.Repo, n); err != nil {
			return "", fmt.Errorf("the stage of #%d: %w", n, err)
		}
	}
	dirt, err := a.Dirty(ctx, s, d.Step.Trust)
	if err != nil {
		return "", fmt.Errorf("the stage of #%d: %w", n, err)
	}
	is := state.Derive(s, a.AppSlug, dirt.Has)
	moved := slices.ContainsFunc(is.Markers, func(m github.Marker) bool {
		to := state.Stage(m.Fields["to"])
		return m.Kind == "transition" && (to.Open() || to == state.Closed)
	})
	if !moved {
		return "", nil
	}
	return is.Stage, nil
}

// plannerTrustedStages sets the Stage of each of subs that's open and in
// this repo, but issue skip, which no list shows (plannerList), from its
// markers (plannerTrustedStage). A closed one, or one in another repo,
// gets none: its line gives its state instead.
func plannerTrustedStages(ctx context.Context, d *Daemon, subs []plannerSub, skip int) error {
	for i, sub := range subs {
		if sub.Repo != "" || sub.State != "open" || sub.Number == skip {
			continue
		}
		stage, err := plannerTrustedStage(ctx, d, sub.Number)
		if err != nil {
			return err
		}
		subs[i].Stage = stage
	}
	return nil
}
