package daemon

import (
	"context"
	"fmt"
	"maps"
	"strconv"

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
//     revision has the body's hash already.
func (a *Applier) PostRevision(ctx context.Context, s *github.Snapshot, dirty func(int64) bool, m Move) error {
	if m.To != "" && state.Derive(s, a.AppSlug, dirty).Moved(m.Key) {
		return a.Transition(ctx, s, dirty, m)
	}
	latest, _ := state.LatestRevision(s, a.AppSlug, dirty)
	hash := state.BodyHash(s.Body)
	if m.To == "" && latest.Rev > 0 && latest.Hash == hash {
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
// revision while the body still has its hash (SPEC §7.4): Apply moves the
// issue to planning with cmd=<comment id>, rev and body, then the 👍. Check
// rejects it otherwise, and at plan-review, which isn't handled yet.
func ApproveCommand() Handler {
	return Handler{
		Check: func(_ context.Context, a *Applier, c Cmd) error {
			// Once Apply's marker exists, the stage is planning, so Step
			// rejects or finishes the command before it asks Check.
			if state.Derive(c.S, a.AppSlug, c.Dirt.Has).Stage == state.PlanReview {
				return Rejection{Reason: "approving a plan isn't handled yet"}
			}
			latest, ok := state.LatestRevision(c.S, a.AppSlug, c.Dirt.Has)
			switch {
			case !ok:
				return Rejection{Reason: "there's no revision to approve yet"}
			case c.Comment.ID < latest.Comment:
				return Rejection{Reason: fmt.Sprintf("it was posted before revision %d, the latest. To approve that, react 👍 to it, or post `/approve` again", latest.Rev)}
			case state.BodyHash(c.S.Body) != latest.Hash:
				return Rejection{Reason: fmt.Sprintf("the issue description changed after revision %d, so it no longer matches that revision's hash, `%s`", latest.Rev, latest.Hash)}
			}
			return nil
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
// approval is applied changes nothing.
func ApproveByReaction(trust github.Trust) func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error {
	return func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error {
		latest, ok := state.LatestRevision(s, a.AppSlug, d.Has)
		if state.Derive(s, a.AppSlug, d.Has).Stage != state.SpecReview || !ok {
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
		return a.approve(ctx, s, d, state.ReactionKey(first.ID), first.User.Login+"'s 👍")
	}
}

// approve moves s from spec-review to planning on key, approving the latest
// revision, if the body still has its hash. by says whose approval it is.
func (a *Applier) approve(ctx context.Context, s *github.Snapshot, d Dirt, key state.Key, by string) error {
	latest, _ := state.LatestRevision(s, a.AppSlug, d.Has)
	if hash := state.BodyHash(s.Body); hash != latest.Hash {
		logr.Debugf("%s/%s#%d: not approving revision %d by %s: the body's hash is %s, not %s", a.Owner, a.Repo, s.Number, latest.Rev, by, hash, latest.Hash)
		return nil
	}
	m := Move{To: state.Planning, Trigger: state.TriggerSpecApproved, Key: key,
		Fields: map[string]string{"rev": strconv.Itoa(latest.Rev), "body": latest.Hash},
		Text:   fmt.Sprintf("Revision %d of the spec is approved, by %s.", latest.Rev, by)}
	if err := a.Transition(ctx, s, d.Has, m); err != nil {
		return fmt.Errorf("approve: %w", err)
	}
	return nil
}
