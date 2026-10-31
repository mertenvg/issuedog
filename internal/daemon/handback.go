package daemon

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file ends a hand-off (SPEC §9.3, Blocked work): a task at
// needs-human that waits on an owner action goes back to ready on a trusted
// /done (DoneCommand) or a trusted 👍 on its latest hand-off comment
// (HandoffByReaction).

// latestHandoff returns the ID of the clean App comment holding s's latest
// hand-off, and true, or false if s has none waiting: the move into
// needs-human that put a task there, one that isn't a human task (SPEC
// §9.3), as Derive reads the markers (discMarkers). s must be open, at
// needs-human, and of a class that gets a PR (state.AnyTask). Of its
// transitions, oldest first:
//   - a move into needs-human from a stage a hand-off leaves
//     (handoffFrom), and not one a person set by hand (by=), is a
//     hand-off, the implementer's attempt cap's (capHandoff) and the
//     verify cap's (verifyFindings) included;
//   - a move into held, and one from held back to needs-human, as /resume
//     makes, are looked through: the hand-off from before the hold still
//     waits;
//   - any other move, a hand-set one into needs-human among them, leaves
//     none waiting, so a hand-off from before the task last left
//     needs-human never counts again.
//
// It reads only s, so it costs no request.
func latestHandoff(s *github.Snapshot, appSlug string, dirty func(int64) bool) (int64, bool) {
	is := state.Derive(s, appSlug, dirty)
	if s.Closed || is.Stage != state.NeedsHuman {
		return 0, false
	}
	if class, err := state.ClassOf(is.Item); err != nil || class&state.AnyTask == 0 {
		return 0, false
	}
	var id int64
	discMarkers(s, appSlug, dirty, func(c int64, _ time.Time, m github.Marker) {
		from, to := state.Stage(m.Fields["from"]), state.Stage(m.Fields["to"])
		if m.Kind != "transition" || !to.Open() && to != state.Closed {
			return
		}
		switch {
		case to == state.NeedsHuman && slices.Contains(handoffFrom, from) && m.Fields["by"] == "" && c != state.BodyID:
			id = c
		case to == state.Held, from == state.Held && to == state.NeedsHuman:
		default:
			id = 0
		}
	})
	return id, id != 0
}

// handoffFrom are the stages SPEC §7.2's table moves a task from into
// needs-human with a hand-off (SPEC §9.3, Blocked work): in-progress, as
// the implementer hands off, or its attempt cap does (capHandoff),
// verifying, as the verify cap does (verifyFindings), pr-open, for a
// closed PR, and needs-human
// itself, for an untrusted merge.
var handoffFrom = []state.Stage{state.InProgress, state.Verifying, state.PROpen, state.NeedsHuman}

// HandoffByReaction returns the reaction trigger for Step.Reactions that
// does what /done does on a trusted 👍 on a task's latest hand-off comment
// (SPEC §9.2, §9.3). It acts only on an open task at needs-human with a
// hand-off waiting (latestHandoff), so a 👍 given while the task is held
// waits until the hold ends, a human task's 👍s are DoneByReaction's, and
// any other issue costs no read. It reads that comment's reactions
// (Client.Reactions), with a conditional request, so a pass where nothing
// changed costs a 304: never an earlier hand-off's, nor the issue's own.
// It takes the earliest 👍 that no marker names (state.ReactionKey) by a
// user trust trusts, not a bot, so neither the App nor any other bot
// counts, and moves the task needs-human → ready on handoff-done, keyed
// cmd=r<reaction id>, with seen the newest comment read (newestRead). Its
// text names the 👍's user, escaped and defused. A read that fails is an
// error: the pass fails, and the next pass tries again.
func HandoffByReaction(trust github.Trust) func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error {
	return func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error {
		wrap := func(err error) error {
			return fmt.Errorf("hand-off done by 👍 on %s/%s#%d: %w", a.Owner, a.Repo, s.Number, err)
		}
		id, ok := latestHandoff(s, a.AppSlug, d.Has)
		if !ok {
			return nil
		}
		reactions, err := a.Client.Reactions(ctx, a.Owner, a.Repo, id)
		if err != nil {
			return wrap(fmt.Errorf("read the hand-off's reactions: %w", err))
		}
		is := state.Derive(s, a.AppSlug, d.Has)
		// Oldest first, so the first trusted one is the 👍 to take.
		slices.SortFunc(reactions, func(x, y github.Reaction) int { return cmp.Compare(x.ID, y.ID) })
		var thumb *github.Reaction
		for _, r := range reactions {
			if r.Content != "+1" || r.User.Type == "Bot" || is.Keys[state.ReactionKey(r.ID)] {
				continue
			}
			t, err := trust.Trusted(ctx, r.User.Login)
			if err != nil {
				return wrap(fmt.Errorf("trust %s: %w", r.User.Login, err))
			}
			if t {
				thumb = &r
				break
			}
		}
		if thumb == nil {
			return nil
		}
		// The login isn't the App's text: it's escaped, and defused.
		who := "`" + defuse(login(thumb.User)) + "`"
		seen := newestRead(s, a.AppSlug, lastTransition(s, a.AppSlug, d.Has))
		m := Move{To: state.Ready, Trigger: state.TriggerHandoffDone, Key: state.ReactionKey(thumb.ID),
			Fields: map[string]string{state.FieldSeen: strconv.FormatInt(seen, 10)},
			Text:   fmt.Sprintf("%s's 👍 on the hand-off says it's done, so the task goes back to `ready`.", who)}
		if err := a.Transition(ctx, s, d.Has, m); err != nil {
			return wrap(err)
		}
		logr.Infof("%s/%s#%d: %s's 👍 on hand-off comment %d sent the task back to %s", a.Owner, a.Repo, s.Number, login(thumb.User), id, state.Ready)
		return nil
	}
}
