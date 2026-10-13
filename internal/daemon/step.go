package daemon

import (
	"cmp"
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
	// Commands are the handlers, by command name. A valid command with none
	// stays unprocessed for the task that adds it, except /done, which is
	// rejected as not handled yet. Each must be replay-safe (see Handler).
	Commands map[string]Handler
	// Reactions, if set, runs after the comments: the reaction triggers.
	Reactions func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error
}

// Run runs step 1 on s, in this order: the label rules (CheckLabels), dirty
// content (MarkDirty), trusted comments oldest first, then Reactions. Dirty
// is read before CheckLabels, which needs it. It returns s's Inputs. Writes
// are added to s, and each is checked first, so a pass that crashed part way
// can run again (SPEC §5.2).
func (st *Step) Run(ctx context.Context, s *github.Snapshot) ([]github.Comment, error) {
	wrap := func(err error) error { return fmt.Errorf("step 1 on %s/%s#%d: %w", st.Owner, st.Repo, s.Number, err) }
	d, err := st.Dirty(ctx, s)
	if err != nil {
		return nil, wrap(err)
	}
	if err := st.CheckLabels(ctx, s, st.Trust, d.Has); err != nil {
		return nil, wrap(err)
	}
	if err := st.MarkDirty(ctx, s, d); err != nil {
		return nil, wrap(err)
	}
	counts, err := st.counted(ctx, s, d)
	if err != nil {
		return nil, wrap(err)
	}
	// Replies are added to s.Comments, so range over the comments read.
	for _, c := range slices.Clone(s.Comments) {
		if !counts[c.ID] {
			continue
		}
		if err := st.handle(ctx, s, d, c); err != nil {
			return nil, wrap(fmt.Errorf("comment %d: %w", c.ID, err))
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

// handle processes c, a comment that counts as trusted input. c is processed
// once a cmd or reply marker names it, or the App has 👍'd or 👎'd it: only
// the App can remove its reaction, so a rejection is 👎 first, then the reply.
//   - A processed comment missing its reaction gets it (SPEC §5.2), and a
//     👎'd one with no clean reply gets the reply again (reword), whose marker
//     keeps it out of Inputs.
//   - One edited since it was used (usedAt), or a 👍'd one that wasn't an
//     answer edited since the 👍, gets one edited reply (SPEC §6.2).
//   - At clarifying, a comment in the round that isn't a command is an
//     answer, and gets a 👍. A command is applied or rejected.
func (st *Step) handle(ctx context.Context, s *github.Snapshot, d Dirt, c github.Comment) error {
	is := state.Derive(s, st.AppSlug, d.Has)
	named := is.Keys[state.CmdKey(c.ID)]
	cmd, isCmd := state.ParseCommand(c.Body)
	answer := !isCmd && !named && is.Stage == state.Clarifying && c.ID > is.Round
	reason := replied(is, c.ID)
	if (reason == "edited" && !named) || (reason == "" && !isCmd && !answer && !named && c.Edit.At.IsZero()) {
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
		return st.reword(ctx, s, d, c, is.Stage)
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
	at, used := st.usedAt(s, d, c.ID, isCmd)
	switch {
	case reason == "edited":
		return nil
	case used && acked && editedSince(c, at):
		return st.reply(ctx, s, c, "edited", editedText)
	case acked && !named && (isCmd || st.stageAt(s, d, c.ID) != state.Clarifying) && editedSince(c, up.CreatedAt):
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

// reword posts again the reply to c, a rejected command with no clean reply:
// the App crashed after its 👎, or the reply was deleted or made dirty. It
// gives the reason again, through checks that don't write, so a crash leaves
// the same reply; if the command would apply now, or isn't one any more, it
// says the reply was lost. It never applies the command.
func (st *Step) reword(ctx context.Context, s *github.Snapshot, d Dirt, c github.Comment, stage state.Stage) error {
	reason := lost
	if cmd, ok := state.ParseCommand(c.Body); ok {
		why, _, err := st.why(ctx, s, d, c, cmd, stage)
		if err != nil {
			return fmt.Errorf("reword: %w", err)
		}
		reason = cmp.Or(why, lost)
	}
	return st.rejected(ctx, s, c, reason)
}

const editedText = "This was edited after I acted on it, so I've ignored the edit. Post a new comment instead."

// editedSince reports whether c was edited at or after t. GitHub's times are
// to the second, so an edit in t's second counts as after it.
func editedSince(c github.Comment, t time.Time) bool {
	return !c.Edit.At.Before(t) // never edited is the zero time, before any
}

// usedAt returns when the App first used comment id: when the first App
// comment was posted whose marker names id in cmd or, unless id is a
// command, whose transition's seen covers id. A transition with no seen,
// such as a hold, didn't read the answers. Like state.Derive, it skips dirty
// comments.
func (st *Step) usedAt(s *github.Snapshot, d Dirt, id int64, isCmd bool) (time.Time, bool) {
	for _, c := range s.Comments {
		if d.Has(c.ID) {
			continue
		}
		for _, m := range github.TrustedMarkers(c.Author, c.Body, st.AppSlug) {
			b, bounds := state.RoundBound(m, c.ID)
			if m.Fields["cmd"] == state.CmdKey(id).ID || (!isCmd && bounds && m.Fields[state.FieldSeen] != "" && b >= id) {
				return c.CreatedAt, true
			}
		}
	}
	return time.Time{}, false
}

// stageAt returns the stage the issue was at when comment id was posted: the
// latest transition before it. A plain comment posted at clarifying was an
// answer. Like state.Derive, it skips dirty comments.
func (st *Step) stageAt(s *github.Snapshot, d Dirt, id int64) state.Stage {
	stage := state.Intake
	for _, c := range s.Comments {
		if c.ID >= id {
			break
		}
		for _, m := range github.TrustedMarkers(c.Author, c.Body, st.AppSlug) {
			if _, ok := state.RoundBound(m, c.ID); ok && !d.Has(c.ID) {
				stage = state.Stage(m.Fields["to"])
			}
		}
	}
	return stage
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
