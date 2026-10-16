package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// PostRevision posts the next revision comment for s's spec, its body (SPEC
// §7.4): one more than the latest revision (state.LatestRevision), with the
// body's hash, how to approve it, and a revision marker. m.Text, escaped,
// goes first. dirty is as for state.Derive.
//   - With m.To set, the revision comes with that move, in one comment
//     through Transition, whose marker also gets rev and body. It is posted
//     whatever the hash: a question answered is a new revision even if
//     nothing changed (SPEC §7.4), and the earlier revision's 👍s mustn't
//     approve it. Once the move's marker exists, Transition only finishes it.
//   - With no m.To, it posts the revision on its own, unless the latest
//     revision has the body's hash already and isn't Hidden. Its marker
//     gets seen (newestRead), so a comment posted after s was read can
//     still be a revision request on it (requested). With a move, the
//     transition's seen, if the move has one, does that.
//
// A dirty body (#91) gets no revision, since nothing may approve it. A move
// still happens, saying why there's no revision, so the issue doesn't wait
// on a stage run that can't succeed.
func (a *Applier) PostRevision(ctx context.Context, s *github.Snapshot, dirty func(int64) bool, m Move) error {
	if m.To != "" && state.Derive(s, a.AppSlug, dirty).Moved(m.Key) {
		return a.Transition(ctx, s, dirty, m)
	}
	if dirty(state.BodyID) {
		logr.Debugf("%s/%s#%d: no revision: the body has an unsanctioned edit", a.Owner, a.Repo, s.Number)
		if m.To == "" {
			return nil
		}
		m.Text = join(m.Text, "There's no revision to approve: the issue description was edited without approval (see the note on it).")
		return a.Transition(ctx, s, dirty, m)
	}
	latest, _ := state.LatestRevision(s, a.AppSlug, dirty)
	hash := state.BodyHash(s.Body)
	if m.To == "" && latest.Rev > 0 && latest.Hash == hash && !latest.Hidden {
		return nil
	}
	rev := strconv.Itoa(latest.Rev + 1)
	marker := github.Marker{Kind: "revision", Fields: map[string]string{"rev": rev, "body": hash}}
	text := join(m.Text, fmt.Sprintf("**Revision %s** of the spec, `%s`.\n\n"+
		"- To approve it, react 👍 to this comment, or post `/approve`.\n"+
		"- To send it back, post `/revise <notes>`, or any other comment.", rev, hash))
	logr.Infof("%s/%s#%d: posting revision %s, %s", a.Owner, a.Repo, s.Number, rev, hash)
	if m.To != "" {
		m.Fields = maps.Clone(m.Fields)
		if m.Fields == nil {
			m.Fields = map[string]string{}
		}
		m.Fields["rev"], m.Fields["body"] = rev, hash
		m.Text, m.Markers = text, append(m.Markers[:len(m.Markers):len(m.Markers)], marker)
		return a.Transition(ctx, s, dirty, m)
	}
	floor := max(latest.Comment, state.Derive(s, a.AppSlug, dirty).Round)
	if seen := newestRead(s, a.AppSlug, floor); seen > 0 {
		marker.Fields[state.FieldSeen] = strconv.FormatInt(seen, 10)
	}
	f, err := github.FormatMarker(marker)
	if err != nil {
		return fmt.Errorf("post revision %s on %s/%s#%d: %w", rev, a.Owner, a.Repo, s.Number, err)
	}
	if err := a.comment(ctx, s, join(github.Escape(text), f)); err != nil {
		return fmt.Errorf("post revision %s: %w", rev, err)
	}
	return nil
}

// ApproveCommand is the /approve handler (SPEC §9.2). At spec-review, a
// trusted /approve posted after the latest revision comment approves that
// revision while the body still has its hash and isn't dirty (SPEC §7.4):
// Apply moves the issue to planning with cmd=<comment id>, rev, body and
// seen=<comment id>, so a comment posted after the /approve is planning's
// input (state.Inputs), then the 👍. Check rejects it otherwise, when the
// table has no such move for the issue's class (SPEC §7.2), and at
// plan-review, which isn't handled yet.
func ApproveCommand() Handler {
	return Handler{
		Check: func(_ context.Context, a *Applier, c Cmd) error {
			// Once Apply's marker exists, the stage is planning, so Step
			// rejects or finishes the command before it asks Check.
			is := state.Derive(c.S, a.AppSlug, c.Dirt.Has)
			if is.Stage == state.PlanReview {
				return Rejection{Reason: "approving a plan isn't handled yet"}
			}
			latest, ok := state.LatestRevision(c.S, a.AppSlug, c.Dirt.Has)
			switch {
			case c.Dirt.Has(state.BodyID):
				return Rejection{Reason: fmt.Sprintf("the issue description has an edit by %s that wasn't approved, so I can't approve it", c.Dirt[state.BodyID])}
			case !ok:
				return Rejection{Reason: "there's no revision to approve yet"}
			case latest.Hidden:
				return Rejection{Reason: fmt.Sprintf("a comment of mine after revision %d was edited without approval, "+
					"so it may hide a newer revision. Approve the next revision I post instead", latest.Rev), Final: true}
			case c.Comment.ID < latest.Comment:
				return Rejection{Reason: fmt.Sprintf("it was posted before revision %d, the latest. To approve that, react 👍 to it, or post `/approve` again", latest.Rev), Final: true}
			case state.BodyHash(c.S.Body) != latest.Hash:
				return Rejection{Reason: fmt.Sprintf("the issue description changed after revision %d, so it no longer matches that revision's hash, `%s`", latest.Rev, latest.Hash)}
			}
			return inTable(is, state.Planning, state.TriggerSpecApproved, "on approval")
		},
		Apply: func(ctx context.Context, a *Applier, c Cmd) error {
			return a.approve(ctx, c.S, c.Dirt, state.CmdKey(c.Comment.ID), c.Comment.Author.Login+"'s `/approve`")
		},
	}
}

// ApproveByReaction returns the reaction trigger for Step.Reactions that
// approves a spec by 👍 (SPEC §7.4). At spec-review, the earliest 👍 on the
// latest revision comment from a user trust trusts, not a bot, approves it
// while the body still has its hash: the issue moves to planning with
// cmd=r<reaction id>, rev and body. Other 👍s, the App's included, are
// ignored. At any other stage it reads nothing, so removing the 👍 once the
// approval is applied changes nothing. A 👍 the table can't apply is
// reported once by Transition (SPEC §5.3), then only logged. An issue of a
// type ClassOf doesn't know is only logged: the §4 type error will report it.
func ApproveByReaction(trust github.Trust) func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error {
	return func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error {
		latest, ok := state.LatestRevision(s, a.AppSlug, d.Has)
		is := state.Derive(s, a.AppSlug, d.Has)
		if is.Stage != state.SpecReview || !ok {
			return nil
		}
		if _, err := state.ClassOf(is.Item); err != nil {
			logr.Debugf("%s/%s#%d: no approval by 👍: %v", a.Owner, a.Repo, s.Number, err)
			return nil
		}
		reactions, err := a.Client.Reactions(ctx, a.Owner, a.Repo, latest.Comment)
		if err != nil {
			return fmt.Errorf("approvals on %s/%s#%d: %w", a.Owner, a.Repo, s.Number, err)
		}
		var first *github.Reaction
		for _, r := range reactions {
			if r.Content != "+1" || r.User.Type == "Bot" || (first != nil && first.ID < r.ID) {
				continue
			}
			t, err := trust.Trusted(ctx, r.User.Login)
			if err != nil {
				return fmt.Errorf("approvals on %s/%s#%d: trust %s: %w", a.Owner, a.Repo, s.Number, r.User.Login, err)
			}
			if t {
				first = &r
			}
		}
		if first == nil {
			return nil
		}
		err = a.approve(ctx, s, d, state.ReactionKey(first.ID), first.User.Login+"'s 👍")
		if errors.Is(err, state.ErrNoTransition) {
			logr.Debugf("%s/%s#%d: ignoring the 👍 on revision %d: %v", a.Owner, a.Repo, s.Number, latest.Rev, err)
			return nil
		}
		return err
	}
}

// approve moves s from spec-review to planning on key, approving the latest
// revision, if the body isn't dirty and still has its hash, and the revision
// isn't Hidden. by says whose approval it is. Its seen is a comment key's
// ID, else, for a 👍, the newest comment read (newestRead), so a comment
// posted after s was read is planning's input. That swallows no revision
// request: Step handled every comment in s first, and one it rejected for
// the table or the type would make the approval fail the same way.
func (a *Applier) approve(ctx context.Context, s *github.Snapshot, d Dirt, key state.Key, by string) error {
	if d.Has(state.BodyID) {
		logr.Debugf("%s/%s#%d: not approving by %s: the body has an edit by %s that wasn't approved", a.Owner, a.Repo, s.Number, by, d[state.BodyID])
		return nil
	}
	latest, _ := state.LatestRevision(s, a.AppSlug, d.Has)
	if latest.Hidden {
		logr.Debugf("%s/%s#%d: not approving revision %d by %s: a dirty comment of the App's after it may hide a newer one", a.Owner, a.Repo, s.Number, latest.Rev, by)
		return nil
	}
	if hash := state.BodyHash(s.Body); hash != latest.Hash {
		logr.Debugf("%s/%s#%d: not approving revision %d by %s: the body's hash is %s, not %s", a.Owner, a.Repo, s.Number, latest.Rev, by, hash, latest.Hash)
		return nil
	}
	m := Move{To: state.Planning, Trigger: state.TriggerSpecApproved, Key: key,
		Fields: map[string]string{"rev": strconv.Itoa(latest.Rev), "body": latest.Hash},
		Text:   fmt.Sprintf("Revision %d of the spec is approved, by %s.", latest.Rev, by)}
	m.Fields[state.FieldSeen] = key.ID
	if _, ok := key.Comment(); !ok {
		m.Fields[state.FieldSeen] = strconv.FormatInt(newestRead(s, a.AppSlug, latest.Comment), 10)
	}
	if err := a.Transition(ctx, s, d.Has, m); err != nil {
		return fmt.Errorf("approve: %w", err)
	}
	return nil
}

// inTable returns a Final Rejection unless the table moves an issue like is
// to stage to on trigger (SPEC §7.2). on says when, for the reason.
func inTable(is state.Issue, to state.Stage, trigger state.Trigger, on string) error {
	class, err := state.ClassOf(is.Item)
	if err != nil {
		return Rejection{Reason: fmt.Sprintf("I can't tell what kind of issue this is: %v", err), Final: true}
	}
	if _, err := state.Lookup(is.Stage, to, class, trigger); errors.Is(err, state.ErrNoTransition) {
		return Rejection{Reason: fmt.Sprintf("only an epic, or a story with no parent, goes from `%s` to `%s` %s, and this is a %s issue", is.Stage, to, on, class), Final: true}
	} else if err != nil {
		return fmt.Errorf("move to %s %s: %w", to, on, err)
	}
	return nil
}

// ReviseCommand is the /revise handler (SPEC §9.2), and Step sends it each
// revision request too (requested). At spec-review, Apply sends the spec
// back: it moves the issue to clarifying with cmd=<comment id> and
// seen=<comment id>, then the 👍. The comment is the notes in state.Inputs,
// and a comment posted after it is the next round's answer. Check rejects it
// when the table has no such move for the issue's class (SPEC §7.2), and at
// plan-review, which isn't handled yet.
func ReviseCommand() Handler {
	return Handler{
		Check: func(_ context.Context, a *Applier, c Cmd) error {
			// Once Apply's marker exists, the stage is clarifying, so Step
			// rejects or finishes the command before it asks Check.
			is := state.Derive(c.S, a.AppSlug, c.Dirt.Has)
			if is.Stage == state.PlanReview {
				return Rejection{Reason: "revising a plan isn't handled yet"}
			}
			return inTable(is, state.Clarifying, state.TriggerSpecRevise, "on a revision request")
		},
		Apply: func(ctx context.Context, a *Applier, c Cmd) error {
			m := Move{To: state.Clarifying, Trigger: state.TriggerSpecRevise, Key: state.CmdKey(c.Comment.ID),
				Fields: map[string]string{state.FieldSeen: strconv.FormatInt(c.Comment.ID, 10)},
				Text:   fmt.Sprintf("The spec is sent back for revision, with %s's comment as the notes.", c.Comment.Author.Login)}
			if err := a.Transition(ctx, c.S, c.Dirt.Has, m); err != nil {
				return fmt.Errorf("revise: %w", err)
			}
			return nil
		},
	}
}

// requested returns the /revise that comment c, trusted input, stands for
// if it is a revision request (SPEC §7.4): a comment that isn't a command
// and that no marker names, posted at spec-review after the latest revision,
// whatever the stage is now. The stage c was posted at is its round's
// (postedAt), so a remark left at another stage never counts. After the
// revision is after its Seen, if it has one, as a comment posted after the
// App read the issue may come before the revision comment; else after its
// comment.
//
// So a request rejected at spec-review whose reply was lost still gets it
// after the stage moves on (Step.handle). The latest revision is then still
// the one c was posted after: only the App posts one, and only after a pass
// whose Run succeeded, so had the reply. This reads only the markers in s,
// and Step reads the App's reactions only to a request, not to every plain
// comment. At spec-review it handles a request in the pass that finds it,
// after which a reply or a move names it. Once the stage has moved on, the
// requests left are those posted after the latest revision that the App
// never handled, such as one by a user trusted only since: the leftovers
// of the last spec-review visit, which the next revision ends.
func requested(s *github.Snapshot, appSlug string, d Dirt, is state.Issue, c github.Comment) (state.Command, bool) {
	if _, cmd := state.ParseCommand(c.Body); cmd || is.Keys[state.CmdKey(c.ID)] || postedAt(s, appSlug, d, c.ID) != state.SpecReview {
		return state.Command{}, false
	}
	latest, _ := state.LatestRevision(s, appSlug, d.Has)
	if c.ID <= cmp.Or(latest.Seen, latest.Comment) {
		return state.Command{}, false
	}
	text := strings.TrimSpace(c.Body)
	line, _, _ := strings.Cut(text, "\n")
	return state.Command{Name: "revise", Arg: strings.TrimSpace(line), Text: text}, true
}

// postedAt returns the stage of the round comment id is in (state.RoundBound),
// at which it was posted: the to= of the last transition whose bound is below
// id, or Intake if there is none. A move to held posted after id doesn't
// count, though its bound, the one from before the hold (state.HeldStack),
// is below id: id was posted before the hold. Like state.Derive, it skips
// dirty comments.
func postedAt(s *github.Snapshot, appSlug string, d Dirt, id int64) state.Stage {
	posted := state.Intake
	for _, c := range s.Comments {
		if d.Has(c.ID) {
			continue
		}
		for _, m := range github.TrustedMarkers(c.Author, c.Body, appSlug) {
			if b, bounds := state.RoundBound(m, c.ID); bounds && b < id && (c.ID < id || state.Stage(m.Fields["to"]) != state.Held) {
				posted = state.Stage(m.Fields["to"])
			}
		}
	}
	return posted
}

// newestRead returns the newest ID of s's comments by anyone but the App,
// or floor if it is newer: a seen for what a pass read. The App's own
// comments in s may be this pass's writes, posted after a comment s lacks.
func newestRead(s *github.Snapshot, appSlug string, floor int64) int64 {
	for _, c := range s.Comments {
		if !state.IsApp(c.Author, appSlug) && c.ID > floor {
			floor = c.ID
		}
	}
	return floor
}

// ReviewVisit ends a visit to an issue at spec-review: the loop calls it
// after Run succeeds on the issue, and at any other stage it does nothing.
// While the latest revision is stale, Hidden or missing (SPEC §7.4), it
// posts the next one (PostRevision), as after the owner edits the body,
// which voids any pending approval: that binds to the latest revision. A
// dirty body, or one last edited by a user Trust doesn't trust, gets none,
// so the mismatch keeps approval blocked; the second is logged here.
func (st *Step) ReviewVisit(ctx context.Context, s *github.Snapshot) error {
	wrap := func(err error) error { return fmt.Errorf("revision of %s/%s#%d: %w", st.Owner, st.Repo, s.Number, err) }
	d, err := st.Dirty(ctx, s, st.trustOnceTrust())
	if err != nil {
		return wrap(err)
	}
	if state.Derive(s, st.AppSlug, d.Has).Stage != state.SpecReview {
		return nil
	}
	latest, ok := state.LatestRevision(s, st.AppSlug, d.Has)
	if stale := !ok || latest.Hidden || latest.Hash != state.BodyHash(s.Body); stale && !d.Has(state.BodyID) {
		by := s.Author
		if !s.BodyEdit.At.IsZero() {
			by = s.BodyEdit.By
		}
		t := state.IsApp(by, st.AppSlug)
		if !t && by.Type != "Bot" && by.Login != "" {
			if t, err = st.Trust.Trusted(ctx, by.Login); err != nil {
				return wrap(fmt.Errorf("trust %s: %w", by.Login, err))
			}
		}
		if !t {
			logr.Debugf("%s/%s#%d: no revision: the body was last edited by %s, who isn't trusted", st.Owner, st.Repo, s.Number, login(by))
			return nil
		}
	}
	if err := st.PostRevision(ctx, s, d.Has, Move{}); err != nil {
		return wrap(err)
	}
	return nil
}
