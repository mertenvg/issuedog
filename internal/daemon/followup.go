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
// issue on every pass, as a 👍 on it, or on its addendum on its story,
// changes no updated_at (SPEC §8.1).
// Whether it is a follow-up is FollowUpByReaction's to judge, from its
// origin marker, as only Derive reads that, with the issue's dirty content.
func followUpWaits(s *github.Snapshot, appSlug string) bool {
	return !s.Closed && state.IsApp(s.Author, appSlug) && slices.ContainsFunc(s.Labels, func(l string) bool {
		stage, ok := state.ParseLabel(l)
		return ok && stage == state.Intake
	})
}

// followUpApprovable reports whether s, whose state is is, is a follow-up
// waiting at intake that a 👍 would approve (SPEC §10.2): open, at intake,
// with a follow-up's origin marker (state.Item.FollowUp), and still a task.
// One retyped is left to the type check (SPEC §5.3), which reports it.
// FollowUpByReaction acts only on such an issue, and the digest lists it
// as a follow-up (waitFind), so the two agree.
func followUpApprovable(s *github.Snapshot, is state.Issue) bool {
	if s.Closed || is.Stage != state.Intake || !is.Item.FollowUp {
		return false
	}
	class, err := state.ClassOf(is.Item)
	return err == nil && class == state.ClassTask
}

// firstTrustedThumb returns the 👍 that a reaction command takes from
// reactions (SPEC §6.1, §9.2), or nil if none counts: the earliest, by
// reaction ID, whose user trust trusts and isn't a bot, so neither the App
// nor any other bot counts, whatever trust says, and that named, if set,
// doesn't name, as a 👍 a marker names already has been taken (SPEC §5.2).
// Trust is asked about each 👍 in that order, until one is trusted, so
// reactions may come in any order, as a follow-up's two lists do. A plan
// or spec approval (ApproveByReaction), a hand-off's end
// (HandoffByReaction), a follow-up's approval (FollowUpByReaction) and a
// flag's answer (FlagByReaction) all take their 👍 by it, so the four
// can't drift apart. A failed trust check is an error.
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

// followUpAddendumOf is the ID of the addendum comment on story that names
// child, a follow-up filed under it (SPEC §5, addendum), or false if none
// does: the first comment the App wrote with an addendum marker naming
// child, never edited. It reads only story, the snapshot the daemon holds,
// so it costs no request. The App never edits an addendum, so an edit is
// someone else's, or the App's note on content someone else edited (SPEC
// §5.1): that addendum counts for nothing, nor does a 👍 on it.
func followUpAddendumOf(story *github.Snapshot, child int, appSlug string) (int64, bool) {
	for _, c := range story.Comments {
		if !c.Edit.At.IsZero() {
			continue
		}
		for _, m := range github.TrustedMarkers(c.Author, c.Body, appSlug) {
			if n, ok := addendumOf(m); ok && n == child {
				return c.ID, true
			}
		}
	}
	return 0, false
}

// FollowUpByReaction returns the reaction trigger for Step.Reactions that
// approves a follow-up by 👍 (SPEC §6.2, §10.2). It acts only on an open
// task at intake that Derive reads as a follow-up (state.Item.FollowUp),
// so a 👍 given while it's held waits until the hold ends back at intake,
// and any other issue costs no read. It reads the issue's own reactions
// (Client.IssueReactions), with a conditional request, so a pass where
// nothing changed costs a 304. For a follow-up in a story, a parent in
// this repo, it then reads the reactions of its clean addendum comment on
// the story, with a conditional request too. The addendum is found in the
// story's snapshot that held gives, the one the daemon holds, so at no
// request (followUpAddendumOf). held may be nil, or give nil, as for a
// story the daemon doesn't hold, and then only the issue's reactions are
// read. It takes the earliest 👍 of the two lists, by reaction ID, that no
// marker on the follow-up names (state.ReactionKey), by a user trust
// trusts, not a bot (firstTrustedThumb), so neither the App nor any other
// bot counts, and moves the follow-up intake → ready on
// follow-up-approved, keyed cmd=r<reaction id>, with seen the newest
// comment read (newestRead), and none when no comment was read. Its text
// names the 👍's user, escaped and defused, and for a 👍 on the addendum,
// links it. A read that fails is an error: the pass fails, and the next
// pass tries again.
func FollowUpByReaction(trust github.Trust, held func(n int) *github.Snapshot) func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error {
	return func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error {
		wrap := func(err error) error {
			return fmt.Errorf("follow-up approval by 👍 on %s/%s#%d: %w", a.Owner, a.Repo, s.Number, err)
		}
		is := state.Derive(s, a.AppSlug, d.Has)
		if !followUpApprovable(s, is) {
			return nil
		}
		reactions, err := a.Client.IssueReactions(ctx, a.Owner, a.Repo, s.Number)
		if err != nil {
			return wrap(fmt.Errorf("read its reactions: %w", err))
		}
		var addendum int64 // the addendum's comment ID, once its reactions are read
		onAddendum := map[int64]bool{}
		if s.Parent != 0 && s.ParentRepo == "" && held != nil {
			if story := held(s.Parent); story != nil {
				if id, ok := followUpAddendumOf(story, s.Number, a.AppSlug); ok {
					more, err := a.Client.Reactions(ctx, a.Owner, a.Repo, id)
					if err != nil {
						return wrap(fmt.Errorf("read the reactions of its addendum on #%d: %w", s.Parent, err))
					}
					addendum = id
					for _, r := range more {
						onAddendum[r.ID] = true
					}
					reactions = append(slices.Clone(reactions), more...)
				}
			}
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
		text := fmt.Sprintf("%s's 👍 on this follow-up approves it, so it goes to `ready`.", who)
		if onAddendum[thumb.ID] {
			text = fmt.Sprintf("%s's 👍 on [its addendum](%s) on #%d approves this follow-up, so it goes to `ready`.",
				who, waitLink(a.Owner, a.Repo, s.Parent, addendum), s.Parent)
		}
		m := Move{To: state.Ready, Trigger: state.TriggerFollowUpApproved, Key: state.ReactionKey(thumb.ID), Text: text}
		if seen := newestRead(s, a.AppSlug, lastTransition(s, a.AppSlug, d.Has)); seen > 0 {
			m.Fields = map[string]string{state.FieldSeen: strconv.FormatInt(seen, 10)}
		}
		if err := a.Transition(ctx, s, d.Has, m); err != nil {
			return wrap(err)
		}
		where := "it"
		if onAddendum[thumb.ID] {
			where = fmt.Sprintf("its addendum on #%d", s.Parent)
		}
		logr.Infof("%s/%s#%d: %s's 👍 on %s approved the follow-up, so it went to %s", a.Owner, a.Repo, s.Number, login(thumb.User), where, state.Ready)
		return nil
	}
}
