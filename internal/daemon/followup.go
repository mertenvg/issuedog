package daemon

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file approves a follow-up (SPEC §6.2, §10.2): an issue the App
// filed with an origin marker naming the verify that found it waits at
// intake until a trusted 👍 on it moves it to ready (FollowUpByReaction).

// followUpWaits reports whether s may be a follow-up waiting at intake for
// its approval: open, with stage:intake, in any case, and filed by the
// App. It reads only s, so it costs no request: Iterate visits such an
// issue on every pass, as a 👍 on it changes no updated_at (SPEC §8.1).
// Whether it is a follow-up is FollowUpByReaction's to judge, from its
// origin marker, as only Derive reads that, with the issue's dirty content.
func followUpWaits(s *github.Snapshot, appSlug string) bool {
	return !s.Closed && state.IsApp(s.Author, appSlug) && slices.ContainsFunc(s.Labels, func(l string) bool {
		stage, ok := state.ParseLabel(l)
		return ok && stage == state.Intake
	})
}

// firstTrustedThumb returns the 👍 that a reaction command takes from
// reactions (SPEC §6.1, §9.2), or nil if none counts: the earliest, by
// reaction ID, whose user trust trusts and isn't a bot, so neither the App
// nor any other bot counts, whatever trust says, and that named, if set,
// doesn't name, as a 👍 a marker names already has been taken (SPEC §5.2).
// Trust is asked about each 👍 in that order, until one is trusted. A plan
// or spec approval (ApproveByReaction), a hand-off's end
// (HandoffByReaction) and a follow-up's approval (FollowUpByReaction) all
// take their 👍 by it, so the three can't drift apart. A failed trust
// check is an error.
func firstTrustedThumb(ctx context.Context, trust github.Trust, reactions []github.Reaction, named func(id int64) bool) (*github.Reaction, error) {
	// Oldest first, so the first trusted one is the 👍 to take.
	sorted := slices.SortedFunc(slices.Values(reactions), func(x, y github.Reaction) int { return cmp.Compare(x.ID, y.ID) })
	for _, r := range sorted {
		if r.Content != "+1" || r.User.Type == "Bot" || named != nil && named(r.ID) {
			continue
		}
		t, err := trust.Trusted(ctx, r.User.Login)
		if err != nil {
			return nil, fmt.Errorf("trust %s: %w", r.User.Login, err)
		}
		if t {
			return &r, nil
		}
	}
	return nil, nil
}

// FollowUpByReaction returns the reaction trigger for Step.Reactions that
// approves a follow-up by 👍 (SPEC §6.2, §10.2). It acts only on an open
// task at intake that Derive reads as a follow-up (state.Item.FollowUp),
// so a 👍 given while it's held waits until the hold ends back at intake,
// and any other issue costs no read. It reads the issue's own reactions
// (Client.IssueReactions), with a conditional request, so a pass where
// nothing changed costs a 304. It takes the earliest 👍 that no marker
// names (state.ReactionKey) by a user trust trusts, not a bot
// (firstTrustedThumb), so neither the App nor any other bot counts, and
// moves the follow-up intake → ready on follow-up-approved, keyed
// cmd=r<reaction id>, with seen the newest comment read (newestRead), and
// none when no comment was read. Its text names the 👍's user, escaped and
// defused. A read that fails is an error: the pass fails, and the next
// pass tries again.
func FollowUpByReaction(trust github.Trust) func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error {
	return func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error {
		wrap := func(err error) error {
			return fmt.Errorf("follow-up approval by 👍 on %s/%s#%d: %w", a.Owner, a.Repo, s.Number, err)
		}
		is := state.Derive(s, a.AppSlug, d.Has)
		if s.Closed || is.Stage != state.Intake || !is.Item.FollowUp {
			return nil
		}
		if class, err := state.ClassOf(is.Item); err != nil || class != state.ClassTask {
			return nil // a type change; the type check (SPEC §5.3) reports it
		}
		reactions, err := a.Client.IssueReactions(ctx, a.Owner, a.Repo, s.Number)
		if err != nil {
			return wrap(fmt.Errorf("read its reactions: %w", err))
		}
		thumb, err := firstTrustedThumb(ctx, trust, reactions, func(id int64) bool { return is.Keys[state.ReactionKey(id)] })
		if err != nil {
			return wrap(err)
		}
		if thumb == nil {
			return nil
		}
		// The login isn't the App's text: it's escaped, and defused.
		who := "`" + defuse(login(thumb.User)) + "`"
		m := Move{To: state.Ready, Trigger: state.TriggerFollowUpApproved, Key: state.ReactionKey(thumb.ID),
			Text: fmt.Sprintf("%s's 👍 on this follow-up approves it, so it goes to `ready`.", who)}
		if seen := newestRead(s, a.AppSlug, lastTransition(s, a.AppSlug, d.Has)); seen > 0 {
			m.Fields = map[string]string{state.FieldSeen: strconv.FormatInt(seen, 10)}
		}
		if err := a.Transition(ctx, s, d.Has, m); err != nil {
			return wrap(err)
		}
		logr.Infof("%s/%s#%d: %s's 👍 approved the follow-up, so it went to %s", a.Owner, a.Repo, s.Number, login(thumb.User), state.Ready)
		return nil
	}
}
