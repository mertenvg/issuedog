package daemon

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// Step is loop step 1 (SPEC §8.1) for one issue at a time: it acts on label
// events, commands, answers and reactions.
type Step struct {
	*Applier
	Trust github.Trust
	// Commands are the handlers, by name. A valid command with none waits,
	// except /done, rejected as not handled yet. See Handler for the contract.
	Commands map[string]Handler
	// Reactions, if set, runs after the comments: the reaction triggers.
	Reactions func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error

	trustOnce *trustOnceVisit // the trust of Iterate's visit under way, if any (#174)
}

// Run runs step 1 on s, in this order: the label rules (CheckLabels), dirty
// content (MarkDirty), the type (typeCheck), trusted comments oldest first,
// from the first again each time one takes the issue out of held, then
// Reactions. Dirty
// is read before CheckLabels, which needs it. Writes are added to s, and each
// is checked first, so a pass that crashed part way can run again (SPEC
// §5.2). It returns s's Inputs, which are only right after a Run that
// succeeds, so a caller skips stage work for an issue whose Run failed.
// When CheckLabels holds labels back, Run stops there, and its error wraps
// ErrLabelsHeld.
// After a Run that succeeds, the loop calls ReviewVisit, then IntakeVisit,
// then FlagVisit.
func (st *Step) Run(ctx context.Context, s *github.Snapshot) ([]github.Comment, error) {
	wrap := func(err error) error { return fmt.Errorf("step 1 on %s/%s#%d: %w", st.Owner, st.Repo, s.Number, err) }
	d, err := st.Dirty(ctx, s, st.trustOnceTrust())
	if err != nil {
		return nil, wrap(err)
	}
	if err := st.CheckLabels(ctx, s, st.Trust, d.Has); err != nil {
		return nil, wrap(err)
	}
	if err := st.MarkDirty(ctx, s, d); err != nil {
		return nil, wrap(err)
	}
	if err := st.typeCheck(ctx, s, d); err != nil {
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
	for restarts := 0; ; restarts++ {
		left, err := sweep()
		if err != nil {
			return nil, wrap(err)
		}
		if !left {
			break
		}
		if restarts >= len(read) {
			return nil, wrap(fmt.Errorf("the comments took the issue out of held more than %d times", len(read)))
		}
	}
	if st.Reactions != nil {
		if err := st.Reactions(ctx, st.Applier, s, d); err != nil {
			return nil, wrap(err)
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
			if t, err = st.Trust.Trusted(ctx, c.Author.Login); err != nil {
				return nil, fmt.Errorf("trust %s: %w", c.Author.Login, err)
			}
			trusted[c.Author.Login] = t
		}
		counts[c.ID] = t
	}
	return counts, nil
}

// handle processes c, a comment that counts as trusted input. It is
// processed once a cmd or reply marker names it, or the App 👍'd or 👎'd it.
// Only the App can remove its reaction, so a rejection is 👎, then the reply,
// and a 👎'd command with no clean reply gets it again (reword). A processed
// comment missing its 👍 gets it (SPEC §5.2), and one edited since it was
// used gets one edited reply (SPEC §6.2). At clarifying, a plain comment in
// the round is an answer, and gets a 👍. A revision request, a plain comment
// posted at spec-review, is handled as /revise (requested). Once the stage
// has moved on, one is only a 👎'd request whose reply was lost, which gets
// it (reword). One the App never reacted to is left alone, such as one from
// before #83, one by a user trusted only since, or one posted just before
// the owner set the stage by hand.
func (st *Step) handle(ctx context.Context, s *github.Snapshot, d Dirt, c github.Comment) error {
	is := state.Derive(s, st.AppSlug, d.Has)
	named := is.Keys[state.CmdKey(c.ID)] // any reply marker included
	cmd, isCmd := state.ParseCommand(c.Body)
	late := false // a request at a stage since moved on
	if r, ok := requested(s, st.AppSlug, d, is, c); ok {
		cmd, isCmd, late = r, true, is.Stage != state.SpecReview
	}
	answer := !isCmd && !named && is.Stage == state.Clarifying && c.ID > is.Round
	reason := replied(is, c.ID)
	if !isCmd && !answer && !named && c.Edit.At.IsZero() {
		return nil
	}
	mine, err := st.appReactions(ctx, c.ID)
	if err != nil {
		return fmt.Errorf("read the App's reactions to comment %d: %w", c.ID, err)
	}
	up, acked := mine["+1"]
	if _, downed := mine["-1"]; downed {
		if reason == "rejected" {
			return nil
		}
		return st.reword(ctx, s, d, c, cmd, isCmd, is.Stage)
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
	switch {
	case reason == "edited":
		return nil
	case used && acked && editedSince(c, at):
		return st.reply(ctx, s, c, "edited", editedText)
	case acked && !named && (isCmd || posted != state.Clarifying) && editedSince(c, up.CreatedAt):
		return st.reply(ctx, s, c, "edited", editedText)
	case acked:
		return nil
	case answer:
		return st.react(ctx, c.ID, "+1")
	case !isCmd:
		return nil
	}
	return st.command(ctx, s, d, c, cmd, is.Stage)
}

// reword posts again the reply to c, a 👎'd command with no clean reply. It
// words the reason again through checks that don't write, so after a crash
// the reply is the same, or else says it was lost. It never applies c.
func (st *Step) reword(ctx context.Context, s *github.Snapshot, d Dirt, c github.Comment, cmd state.Command, ok bool, stage state.Stage) error {
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
	_, parsed := state.ParseCommand(c.Body)
	return st.rejected(ctx, s, c, r, ok && !parsed)
}

const editedText = "This was edited after I acted on it, so I've ignored the edit. Post a new comment instead."

// editedSince reports whether c was edited at or after t. GitHub's times are
// to the second, so an edit in t's second counts as after it.
func editedSince(c github.Comment, t time.Time) bool {
	return !c.Edit.At.Before(t) // never edited is the zero time, before any
}

// usedAt returns when the App first used comment id, and the stage of the
// round id is in (state.RoundBound), at which a plain comment was an answer.
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
