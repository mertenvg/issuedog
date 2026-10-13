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

// handle processes c, a comment that counts as trusted input. It is
// processed once a marker names it in cmd, a reply marker included, or the
// App has 👍'd or 👎'd it. Only the App can remove its reaction, so a 👎
// keeps a rejected command rejected after its reply is edited or deleted.
//   - A rejected command whose reply is posted, but not its 👎, gets the 👎.
//   - A comment edited since it was acted on gets one edited reply, and isn't
//     applied again. That is since the first App comment whose marker names
//     it, or whose transition's round covers it, for an answer the App 👍'd.
//     For a command only 👍'd, such as /priority, it is since the 👍. An
//     answer in the current round is the exception: its latest text counts.
//   - One a marker names, but the App hasn't 👍'd, only gets its 👍.
//   - At clarifying, one in the current round (state.Issue.Round) that
//     isn't a command is an answer, and gets a 👍.
//   - A command is checked, then applied by its handler or rejected.
func (st *Step) handle(ctx context.Context, s *github.Snapshot, d Dirt, c github.Comment) error {
	is := state.Derive(s, st.AppSlug, d.Has)
	named := is.Keys[state.CmdKey(c.ID)]
	cmd, isCmd := state.ParseCommand(c.Body)
	answer := !isCmd && is.Stage == state.Clarifying && c.ID > is.Round
	reason := replied(is, c.ID)
	if reason == "edited" || (reason == "" && !isCmd && !answer && !named && c.Edit.At.IsZero()) {
		return nil
	}
	mine, err := st.appReactions(ctx, c.ID)
	if err != nil {
		return fmt.Errorf("read the App's reactions to comment %d: %w", c.ID, err)
	}
	up, acked := mine["+1"]
	_, downed := mine["-1"]
	at, used := st.usedAt(s, d, c.ID, isCmd)
	switch {
	case downed:
		return nil
	case reason == "rejected":
		return st.react(ctx, c.ID, "-1")
	case used && (acked || named) && editedSince(c, at):
		return st.reply(ctx, s, c, "edited", editedText)
	case acked && isCmd && !named && editedSince(c, up.CreatedAt):
		return st.reply(ctx, s, c, "edited", editedText)
	case acked:
		return nil
	case named || answer:
		return st.react(ctx, c.ID, "+1")
	case !isCmd:
		return nil
	}
	return st.command(ctx, s, d, c, cmd, is.Stage)
}

const editedText = "This was edited after I acted on it, so I've ignored the edit. Post a new comment instead."

// editedSince reports whether c was edited at or after t. GitHub's times are
// to the second, so an edit in t's second counts as after it.
func editedSince(c github.Comment, t time.Time) bool {
	return !c.Edit.At.IsZero() && !c.Edit.At.Before(t)
}

// usedAt returns when the App first used comment id: when the first App
// comment was posted whose marker names id in cmd or, unless id is a
// command, whose transition's round bound covers id. Like state.Derive, it
// skips dirty comments.
func (st *Step) usedAt(s *github.Snapshot, d Dirt, id int64, isCmd bool) (time.Time, bool) {
	for _, c := range s.Comments {
		if d.Has(c.ID) {
			continue
		}
		for _, m := range github.TrustedMarkers(c.Author, c.Body, st.AppSlug) {
			b, bounds := state.RoundBound(m, c.ID)
			if m.Fields["cmd"] == state.CmdKey(id).ID || (!isCmd && bounds && c.ID > id && b >= id) {
				return c.CreatedAt, true
			}
		}
	}
	return time.Time{}, false
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
