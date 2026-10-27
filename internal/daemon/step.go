package daemon

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// Step is loop step 1 (SPEC §8.1) for one issue at a time: it acts on label
// events, commands, answers and reactions.
type Step struct {
	*Applier
	Trust github.Trust
	// Commands are the handlers, by name. A valid command with none waits.
	// See Handler for the contract.
	Commands map[string]Handler
	// Reactions, if set, runs after the comments: the reaction triggers.
	Reactions func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error
	// Models, if set, returns models.allowed as of the call, which a plan
	// item's model is resolved against when ChildrenVisit files its child.
	// The loop's reloads replace the daemon's Config, so it asks each time,
	// as SettingCommands does. Unset, no model resolves.
	Models func() config.Models

	trustOnce *trustOnceVisit // the trust of Iterate's visit under way, if any (#174)
	// holdOrderReread, if set, is told of each ancestor whose /hold or
	// /resume another waits for (holdOrderWaits), so Iterate reads it again
	// next pass, and visits it, without skipping its visit in this one.
	holdOrderReread func(number int)
}

// Run runs step 1 on s, in this order: the label rules (CheckLabels), dirty
// content (MarkDirty), a lost origin marker (originRepost), an adopted
// child's (adoptedRepost), the type (typeCheck), the discussion triggers
// (discussion), trusted comments oldest first,
// from the first again each time one takes the issue out of held, then
// Reactions, and the comments again if a reaction trigger took the issue
// out of held, as when a discussion hold ends (ResolveByReaction). Dirty
// is read before CheckLabels, which needs it. Writes are added to s, and each
// is checked first, so a pass that crashed part way can run again (SPEC
// §5.2). It returns s's Inputs, which are only right after a Run that
// succeeds, so a caller skips stage work for an issue whose Run failed.
// When CheckLabels holds labels back, Run stops there, and its error wraps
// ErrLabelsHeld.
// After a Run that succeeds, the loop calls ReviewVisit, then IntakeVisit,
// then FlagVisit, then ChildrenVisit.
func (st *Step) Run(ctx context.Context, s *github.Snapshot) ([]github.Comment, error) {
	wrap := func(err error) error { return fmt.Errorf("step 1 on %s/%s#%d: %w", st.Owner, st.Repo, s.Number, err) }
	d, err := st.Dirty(ctx, s, st.trustOnceTrust())
	if err != nil {
		return nil, wrap(err)
	}
	if err := st.discCheckLabels(ctx, s, st.Trust, d); err != nil {
		return nil, wrap(err)
	}
	if err := st.MarkDirty(ctx, s, d); err != nil {
		return nil, wrap(err)
	}
	if err := st.originRepost(ctx, s, d); err != nil {
		return nil, wrap(err)
	}
	if err := st.adoptedRepost(ctx, s, d); err != nil {
		return nil, wrap(err)
	}
	if err := st.typeCheck(ctx, s, d); err != nil {
		return nil, wrap(err)
	}
	// Triggers first, so a command can't move an issue a discussion holds
	// (SPEC §6.4).
	if _, err := st.discussion(ctx, s, d); err != nil {
		return nil, wrap(err)
	}
	counts, err := st.counted(ctx, s, d)
	if err != nil {
		return nil, wrap(err)
	}
	// Replies are added to s.Comments, so range over the comments read.
	read := slices.Clone(s.Comments)
	// sweep handles the comments read, oldest first, until one takes the
	// issue out of held, and reports whether one did.
	sweep := func() (bool, error) {
		held := state.Derive(s, st.AppSlug, d.Has).Stage == state.Held
		for _, c := range read {
			if !counts[c.ID] {
				continue
			}
			if err := st.handle(ctx, s, d, c); err != nil {
				return false, fmt.Errorf("comment %d: %w", c.ID, err)
			}
			now := state.Derive(s, st.AppSlug, d.Has).Stage == state.Held
			if held && !now {
				return true, nil
			}
			held = now
		}
		return false, nil
	}
	// A hold keeps the round it came in (state.HeldStack.Seen), so once a
	// command takes the issue out of held, the comments are handled again
	// from the first, each in the stage the issue had right after the last
	// move out of held before it, as the next pass would, and as a replay of
	// this one after a crash does. A processed command is never applied
	// again, so each restart needs a move out of held by a command not
	// applied before, and there are no more of those than comments read.
	sweeps := func() error {
		for restarts := 0; ; restarts++ {
			left, err := sweep()
			if err != nil {
				return err
			}
			if !left {
				return nil
			}
			if restarts >= len(read) {
				return fmt.Errorf("the comments took the issue out of held more than %d times", len(read))
			}
		}
	}
	if err := sweeps(); err != nil {
		return nil, wrap(err)
	}
	if st.Reactions != nil {
		held := state.Derive(s, st.AppSlug, d.Has).Stage == state.Held
		if err := st.Reactions(ctx, st.Applier, s, d); err != nil {
			return nil, wrap(err)
		}
		// A reaction trigger that took the issue out of held is a move out
		// of held too, so the comments are handled again, as the next pass
		// would, so it has nothing left to write.
		if held && state.Derive(s, st.AppSlug, d.Has).Stage != state.Held {
			if err := sweeps(); err != nil {
				return nil, wrap(err)
			}
		}
	}
	return state.Derive(s, st.AppSlug, d.Has).Inputs(s, func(c github.Comment) bool { return counts[c.ID] }), nil
}

// counted returns which of s's comments count as trusted input (SPEC §6.2,
// §6.4): those by a trusted user that aren't dirty. No bot is trusted.
func (st *Step) counted(ctx context.Context, s *github.Snapshot, d Dirt) (map[int64]bool, error) {
	counts, trusted := map[int64]bool{}, map[string]bool{}
	for _, c := range s.Comments {
		if c.Author.Type == "Bot" || d.Has(c.ID) {
			continue
		}
		t, seen := trusted[c.Author.Login]
		if !seen {
			var err error
			if t, err = st.trustOnceTrust().Trusted(ctx, c.Author.Login); err != nil {
				return nil, fmt.Errorf("trust %s: %w", c.Author.Login, err)
			}
			trusted[c.Author.Login] = t
		}
		counts[c.ID] = t
	}
	return counts, nil
}

// handle processes c, a comment that counts as trusted input. It is
// processed once a cmd or reply marker names it, or the App 👍'd or 👎'd it,
// but for a revision request the App 👍'd as an answer before the move to
// the review stage, whose seen it comes after: no run read it, so it's
// handled as a request (#318). Unless that move is one a dirty comment hid,
// recorded again with a seen that may be below the run's, and the App 👍'd
// c before the move was posted: then the run read it, so it's no request,
// and an edit after the record gets the edited reply (reviewDirtyRead,
// #447, #485).
// Only the App can remove its reaction, so a rejection is 👎, then the reply,
// and a 👎'd command with no clean reply gets it again (reword). A processed
// comment missing its 👍 gets it (SPEC §5.2), and one edited since it was
// used gets one edited reply (SPEC §6.2). At clarifying, and at planning
// (#253), a plain comment in the round is an answer, and gets a 👍
// (roundStage). A revision request, a plain comment posted at spec-review
// or plan-review, is handled as /revise (requested). Once the stage has
// moved on from the one it was posted at, one is only a 👎'd request whose
// reply was lost, which gets it (reword). One the App never reacted to is
// left alone, such as one from before #83, one by a user trusted only
// since, or one posted just before the owner set the stage by hand. A command that would move the issue
// waits while it's held or at the clarifying after a hold (state.Command.Waits), and a /hold or
// /resume while one posted before it on an ancestor isn't processed (holdOrderWaits).
func (st *Step) handle(ctx context.Context, s *github.Snapshot, d Dirt, c github.Comment) error {
	is := state.Derive(s, st.AppSlug, d.Has)
	named := is.Keys[state.CmdKey(c.ID)] // any reply marker included
	cmd, isCmd := state.ParseCommand(c.Body)
	request, late := false, false // a revision request, and one at a stage since moved on
	if r, posted, ok := requested(s, st.AppSlug, d, is, c); ok {
		cmd, isCmd, request, late = r, true, true, is.Stage != posted
	}
	answer := !isCmd && !named && roundStage(is.Stage) && c.ID > is.Round
	reason := replied(is, c.ID)
	if !isCmd && !answer && !named && c.Edit.At.IsZero() {
		return nil
	}
	mine, err := st.appReactions(ctx, c.ID)
	if err != nil {
		return fmt.Errorf("read the App's reactions to comment %d: %w", c.ID, err)
	}
	up, acked := mine["+1"]
	if down, downed := mine["-1"]; downed {
		if reason == "rejected" {
			return nil
		}
		return st.reword(ctx, s, d, c, cmd, isCmd, is.Stage, editedSince(c, down.CreatedAt))
	}
	if late && !acked {
		return nil
	}
	if reason == "rejected" { // not the App's own writes, which 👎 first
		return st.react(ctx, c.ID, "-1")
	}
	if named && !acked {
		if err := st.react(ctx, c.ID, "+1"); err != nil {
			return fmt.Errorf("finish: %w", err)
		}
		acked = true
	}
	at, used, posted := st.usedAt(s, d, c.ID, isCmd)
	var recorded time.Time // when the record of the hidden move that read c was posted
	read := false
	if acked && request && !late {
		recorded, read = reviewDirtyRead(s, st.AppSlug, d, c, up.CreatedAt)
	}
	switch {
	case reason == "edited":
		return nil
	case read && editedSince(c, recorded):
		// The 👍 is the answer's at clarifying or planning, read by the run
		// whose move a dirty comment hid, and the record stands for that
		// move, which used it: an edit since is ignored (SPEC §6.2).
		return st.reply(ctx, s, c, "edited", editedText)
	case read:
		// The record of that move can only bound what came before the run
		// (reviewDirtyRestore), so the 👍 processed it (SPEC §7.4). The 👍
		// is no use of the answer, so an edit between it and the record,
		// which the run may have read, gets no reply.
		return nil
	case used && acked && editedSince(c, at):
		return st.reply(ctx, s, c, "edited", editedText)
	case acked && !named && (isCmd || !roundStage(posted)) && editedSince(c, up.CreatedAt):
		return st.reply(ctx, s, c, "edited", editedText)
	case acked && (!request || late):
		// The 👍 processed it, but for a request at the stage it was posted
		// at. No marker names that one, so the App 👍'd it as an answer at
		// clarifying or planning, before the run's move came to the review
		// stage with a seen below it. That 👍 wasn't its use (roundStage),
		// and no run read it, so it goes on to be handled as a request, or
		// it would be neither (#318).
		return nil
	case answer:
		return st.react(ctx, c.ID, "+1")
	case !isCmd:
		return nil
	case cmd.Waits(is):
		// A command that would move the issue waits while it's held, or at
		// the clarifying after a hold (SPEC §6.4): it's checked
		// once the issue is at another stage, on a later pass, or in this
		// one once a command takes the issue out of held (Run).
		logr.Debugf("%s/%s#%d: /%s in comment %d waits: the issue is held, or at the clarifying after a hold", st.Owner, st.Repo, s.Number, cmd.Name, c.ID)
		return nil
	}
	// A /hold or /resume waits for one posted before it on an ancestor, so
	// they apply in the order posted (#243).
	wait, err := st.holdOrderWaits(ctx, s, d, c, cmd)
	if err != nil {
		return fmt.Errorf("/%s: %w", cmd.Name, err)
	}
	if wait {
		return nil
	}
	return st.command(ctx, s, d, c, cmd, is.Stage)
}

// roundStage reports whether a plain trusted comment posted in the round
// at stage is an answer: at clarifying, to the interviewer, and at
// planning, to the planner (#253). An answer's 👍 isn't its use, so an
// edit after the 👍 gets no reply until a move's seen covers it (usedAt).
func roundStage(stage state.Stage) bool {
	return stage == state.Clarifying || stage == state.Planning
}

// reword posts again the reply to c, a 👎'd command with no clean reply. It
// words the reason again through checks that don't write, so after a crash
// the reply is the same, or else says it was lost. It never applies c.
// Once c was edited after its 👎 (edited), its text isn't what the 👎
// rejected, and a rejected command's edit is ignored (SPEC §9.2), so it
// isn't worded again from that text: the reply says the reply was lost,
// and that the edit is ignored (#229). It asks for a new comment only when
// one could apply (SPEC §9.2), as rejected does: when the edited text, a
// command or a revision request (ok), would get a Final Rejection, a new
// comment with it would be rejected the same way, so the reply gives that
// reason instead, which says what to do (#399).
func (st *Step) reword(ctx context.Context, s *github.Snapshot, d Dirt, c github.Comment, cmd state.Command, ok bool, stage state.Stage, edited bool) error {
	if edited {
		text := ivWordingLostEdited + " Post a new comment to try again."
		if ok {
			why, _, err := st.why(ctx, s, d, c, cmd, stage)
			if err != nil {
				return fmt.Errorf("reword: %w", err)
			}
			if why.Final {
				text = ivWordingLostEdited + " Its new text wouldn't apply either: " + why.Reason + "."
			}
		}
		logr.Infof("%s/%s#%d: rejected comment %d: %s", st.Owner, st.Repo, s.Number, c.ID, text)
		if err := st.reply(ctx, s, c, "rejected", text); err != nil {
			return fmt.Errorf("reword: %w", err)
		}
		return nil
	}
	r := Rejection{Reason: lost}
	if ok {
		why, _, err := st.why(ctx, s, d, c, cmd, stage)
		if err != nil {
			return fmt.Errorf("reword: %w", err)
		}
		if why.Reason != "" {
			r = why
		}
	}
	var request state.Stage
	if ok {
		request = st.requestAt(s, d, c)
	}
	return st.rejected(ctx, s, c, r, request)
}

const editedText = "This was edited after I acted on it, so I've ignored the edit. Post a new comment instead."

// ivWordingLostEdited is the reply posted again to a 👎'd comment, edited
// after the 👎, whose reply was lost (reword). It may have been a command
// or a revision request, so it says neither. reword ends it with what the
// owner can do.
const ivWordingLostEdited = "I didn't act on this comment, and my reply saying why was lost. " +
	"It was edited after I rejected it, so I've ignored the edit."

// editedSince reports whether c was edited at or after t. GitHub's times are
// to the second, so an edit in t's second counts as after it.
func editedSince(c github.Comment, t time.Time) bool {
	return !c.Edit.At.Before(t) // never edited is the zero time, before any
}

// usedAt returns when the App first used comment id, and the stage of the
// round id is in (state.RoundBound), at which a plain comment was an
// answer if it's a round stage (roundStage).
// A marker uses id if it names id in cmd or, unless id is a command, if it's
// a transition whose seen covers id: one with no seen read no answers. A
// move into, within or out of held carries the bound from before the hold
// (state.HeldStack.Seen), so its seen covers only what an earlier marker's
// did. Like state.Derive, it skips dirty comments.
func (st *Step) usedAt(s *github.Snapshot, d Dirt, id int64, isCmd bool) (at time.Time, used bool, posted state.Stage) {
	posted = postedAt(s, st.AppSlug, d, id)
	for _, c := range s.Comments {
		if d.Has(c.ID) {
			continue
		}
		for _, m := range github.TrustedMarkers(c.Author, c.Body, st.AppSlug) {
			b, bounds := state.RoundBound(m, c.ID)
			if !used && (m.Fields["cmd"] == state.CmdKey(id).ID || (!isCmd && bounds && m.Fields[state.FieldSeen] != "" && b >= id)) {
				at, used = c.CreatedAt, true
			}
		}
	}
	return at, used, posted
}

// replied returns the reason of the first reply marker naming comment id,
// or "".
func replied(is state.Issue, id int64) string {
	for _, m := range is.Markers {
		if m.Kind == "reply" && m.Fields["cmd"] == state.CmdKey(id).ID {
			return m.Fields["reason"]
		}
	}
	return ""
}
