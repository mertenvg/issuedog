package daemon

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file is the interviewer at the clarifying after a discussion hold
// (T5 of #87, #189, SPEC §6.4): where the issue came from, the owner input
// the discussion gave, the /revise that waited through it, and what the
// move into that clarifying says of what comes next (Q3b).

// ivHoldOrigin returns, from the issue's state is, the stage the issue
// was at before its first discussion hold, while it's at the clarifying
// after one (state.Issue.AfterHold), or "". Every marker the interviewer
// writes there carries it (SPEC §6.4), and its unchanged outcome goes
// back there.
func ivHoldOrigin(is state.Issue) state.Stage {
	if !is.AfterHold() {
		return ""
	}
	return discOrigin(is, state.Clarifying)
}

// ivHoldLatest returns is's latest transition that bounds a round, as
// ivLatestMove does, but looking through holds (#185's verify note): a
// move into held, a held → held marker, and a move out of held back to the
// stage the transition before the hold put the issue at, carrying the
// round bound from before the hold (seen), keep the round that transition
// began, as state.Issue.Notes looks through them. So after /resume back to
// clarifying, the round asked before the hold is still the one its answers
// answer, and the interviewer waits for one, as it did before the hold.
//
// With entries, a move out of held that is an entry of its own isn't
// looked through, so the interviewer runs on it at once, as on any entry
// (SPEC §9.1): one carrying at, which only the end of a discussion writes
// (resolveFields), as the interviewer reads the discussion next, or by, a
// stage set by hand, an override.
func ivHoldLatest(is state.Issue, entries bool) github.Marker {
	var last github.Marker
	for _, m := range is.Markers {
		if _, ok := state.RoundBound(m, 0); !ok {
			continue
		}
		from, to := state.Stage(m.Fields["from"]), state.Stage(m.Fields["to"])
		out := from == state.Held && to != state.Held
		through := to == state.Held || out && m.Fields[state.FieldSeen] != "" && to == state.Stage(last.Fields["to"])
		if entries && out && (m.Fields["at"] != "" || m.Fields["by"] != "") {
			through = false
		}
		if through && last.Kind != "" {
			continue
		}
		last = m
	}
	return last
}

// ivHoldSpan is what an issue's transition markers say of its holds
// (ivHoldSpanOf), read as state.Derive reads them.
type ivHoldSpan struct {
	// Start and End are the comments holding the move into held that began
	// the latest hold the issue left, and the move out of it, and At is the
	// stage it began at. Both are 0 if it left none.
	Start, End int64
	At         state.Stage
	// Period is set while the issue is at the clarifying after a hold
	// (state.Issue.AfterHold), and From is then the round bound from
	// before the first hold of that period: a /hold from that clarifying,
	// and its /resume back, don't begin another. The discussion is the
	// comments above it.
	Period bool
	From   int64
}

// ivHoldSpanOf returns s's holds, as ivHoldSpan says. A hold begins as
// state.HeldStack begins one, with the bound from before it: its seen, or
// else the bound the transition before it set.
func ivHoldSpanOf(s *github.Snapshot, appSlug string, dirty func(int64) bool) ivHoldSpan {
	var sp ivHoldSpan
	held, start, bound, from := false, int64(0), int64(0), int64(0)
	at := state.Stage("")
	discMarkers(s, appSlug, dirty, func(id int64, _ time.Time, m github.Marker) {
		b, ok := state.RoundBound(m, id)
		if !ok {
			return
		}
		to := state.Stage(m.Fields["to"])
		switch {
		case to == state.Held && (!held || state.Stage(m.Fields["from"]) != state.Held):
			// Its bound is From only if the move out of it begins a period:
			// a hold from inside one doesn't change From.
			held, start, at, from = true, id, state.Stage(m.Fields["at"]), bound
			if v, err := strconv.ParseInt(m.Fields[state.FieldSeen], 10, 64); err == nil && v > 0 {
				from = v
			}
		case to == state.Held:
		default:
			if held {
				held, sp.Start, sp.End, sp.At = false, start, id, at
			}
			o := state.Stage(m.Fields["origin"])
			switch afterHold := to == state.Clarifying && o.Open() && o != state.Held; {
			case afterHold && !sp.Period:
				sp.Period, sp.From = true, from
			case !afterHold:
				sp.Period, sp.From = false, 0
			}
		}
		bound = b
	})
	return sp
}

// ivHoldInput is the owner input a discussion gave (SPEC §6.4): the
// comments by people who aren't trusted that a trusted user approved with
// a 👍, and the trusted comments that counter the untrusted ones before
// them.
type ivHoldInput struct {
	Approved  map[int64]string // the approver's login, by comment
	Countered []int64          // the counter comments, oldest first
}

// ivHoldInputOf returns the owner input of the discussion on s above
// from, the bound before the after-hold period's first hold (ivHoldSpan):
//   - approved: each clean comment by someone who isn't trusted, no bot,
//     with a 👍 by a trusted user, no bot and not the App, who is named:
//     the first such 👍 the reactions list. It reads the reactions of
//     those comments only, so the 👍 is read whenever it came, before the
//     hold ended or after. A 👍 approves only the text it post-dates: an
//     approval approves what the approver read, not what the author
//     writes later, as SPEC §6.2 says of an untrusted author's edits
//     after admission. So a 👍 counts only if it came strictly after the
//     comment's last edit, whoever made that edit, and whatever became of
//     it as a trigger. Times are to the second, so a 👍 in the edit's
//     second came before it, as for the trigger (discOwnEdits). The
//     author's own edit is sanctioned, so it leaves the comment clean,
//     and by someone without write access it's no trigger either (#189's
//     verify). A write collaborator's edit is a trigger, and accepting it,
//     by its notice's 👍 or a /resume, lets the issue leave its hold, but
//     doesn't make the edited text owner input: that takes a code owner's
//     👍 after the edit, such as another code owner's, or the same one's
//     removed and given again. Until then the comment is in untrusted.md
//     only (#189's verify, attempt 3);
//   - countered: each clean comment by a trusted user, other than a
//     /hold or a /resume, which resolve nothing (resolveHoldCommand),
//     after at least one comment by someone who isn't trusted.
//
// Comments by bots other than the App are untrusted, as for
// untrusted.md, but approve nothing and are never approved.
func ivHoldInputOf(ctx context.Context, a *Applier, tc trustCache, s *github.Snapshot, d Dirt, from int64) (ivHoldInput, error) {
	in := ivHoldInput{Approved: map[int64]string{}}
	untrusted := false
	for _, c := range s.Comments {
		if c.ID <= from || d.Has(c.ID) || state.IsApp(c.Author, a.AppSlug) {
			continue
		}
		trusted, err := tc.trusted(ctx, c.Author)
		switch {
		case err != nil:
			return ivHoldInput{}, fmt.Errorf("the discussion's comment %d: %w", c.ID, err)
		case trusted && untrusted && !resolveHoldCommand(c.Body):
			in.Countered = append(in.Countered, c.ID)
		case trusted:
		case c.Author.Type == "Bot":
			untrusted = true
		default:
			untrusted = true
			reactions, err := a.Client.Reactions(ctx, a.Owner, a.Repo, c.ID)
			if err != nil {
				return ivHoldInput{}, fmt.Errorf("the 👍s on the discussion's comment %d: %w", c.ID, err)
			}
			for _, r := range reactions {
				// A 👍 approves only the text it post-dates. An unedited
				// comment's edit time is zero, so any 👍 post-dates it.
				if r.Content != "+1" || !r.CreatedAt.After(c.Edit.At) {
					continue
				}
				ok, err := tc.trusted(ctx, r.User)
				if err != nil {
					return ivHoldInput{}, fmt.Errorf("the 👍s on the discussion's comment %d: %w", c.ID, err)
				}
				if ok {
					in.Approved[c.ID] = r.User.Login
					break
				}
			}
		}
	}
	return in, nil
}

// ivHoldSection is issue.md's "After a hold" section (SPEC §6.4): the stage
// the issue came from, origin; where the hold began and ended, sp, whose
// App comments in conversation.md name what held it and how each trigger
// was resolved; and the owner input in, each comment fenced under a
// heading the daemon writes, its text escaped (ivFenced).
func ivHoldSection(s *github.Snapshot, origin state.Stage, sp ivHoldSpan, in ivHoldInput) string {
	lines := []string{"## After a hold", ""}
	intro := fmt.Sprintf("This issue was on hold for discussion, and the discussion is resolved, so it came back to clarifying. "+
		"Before its first discussion hold it was at `%s`.", origin)
	if sp.Start > 0 {
		intro += fmt.Sprintf(" Its latest hold began at `%s`, in comment %d, and ended in comment %d: "+
			"issuedog's comments from there on, in conversation.md, say what held it and how each was resolved.", sp.At, sp.Start, sp.End)
	}
	lines = append(lines, intro, "",
		"The owner input from the discussion follows: comments a trusted person approved with a 👍 given after the comment's last edit, "+
			"so the approver read the text shown, and trusted comments that answered the untrusted comments before them. "+
			"Treat it as the owner's. A comment edited after its 👍 isn't owner input, so it isn't here.")
	for _, c := range s.Comments {
		at := "### Comment " + strconv.FormatInt(c.ID, 10) + " by " + ivLogin(c.Author.Login)
		switch by, approved := in.Approved[c.ID]; {
		case approved:
			lines = append(lines, "", at+", approved by "+ivLogin(by)+"'s 👍, so it's owner input", "", ivFenced(c.Body))
		case slices.Contains(in.Countered, c.ID):
			lines = append(lines, "", at+", a trusted comment that counters the untrusted comments before it", "", ivFenced(c.Body))
		}
	}
	if len(in.Approved)+len(in.Countered) == 0 {
		lines = append(lines, "", "None: nobody approved or countered a comment.")
	}
	return strings.Join(lines, "\n") + "\n"
}

// ivHoldRevise returns the /revise the interviewer handles at the
// clarifying after a hold from spec-review (#87's assumption 8, SPEC
// §6.4), or nil: the oldest clean comment above from, the bound before the
// after-hold period's first hold (ivHoldSpan), and not above seen, if
// seen is set, by a trusted user, whose /revise passes its own check at
// spec-review, that no marker names, and that the App hasn't reacted to.
// Such a /revise waits (state.Command.Waits), so nothing else handles it.
// The interviewer's move names it with cmd= (Move.check), so it's
// processed, and isn't applied again. Only one is handled at a time, as a
// marker names one cmd: another one waits for the next run, or applies
// once the issue is at spec-review again.
func ivHoldRevise(ctx context.Context, a *Applier, tc trustCache, s *github.Snapshot, d Dirt, is state.Issue, from, seen int64) (*github.Comment, error) {
	for i, c := range s.Comments {
		cmd, ok := state.ParseCommand(c.Body)
		if c.ID <= from || seen > 0 && c.ID > seen || !ok || cmd.Name != "revise" || cmd.Check(state.SpecReview) != nil ||
			d.Has(c.ID) || is.Keys[state.CmdKey(c.ID)] {
			continue
		}
		trusted, err := tc.trusted(ctx, c.Author)
		if err != nil {
			return nil, fmt.Errorf("the /revise in comment %d: %w", c.ID, err)
		}
		if !trusted {
			continue
		}
		mine, err := a.appReactions(ctx, c.ID)
		if err != nil {
			return nil, fmt.Errorf("the /revise in comment %d: %w", c.ID, err)
		}
		if len(mine) == 0 {
			return &s.Comments[i], nil
		}
	}
	return nil, nil
}

// ivHoldMove returns where an interviewer outcome moves the issue, and on
// which trigger (SPEC §7.2), with origin the stage it came from to the
// clarifying after a hold (ivHoldOrigin), or "": a spec to spec-review, on
// spec-rewritten after a hold, else spec-written; unchanged to origin, on
// discussion-no-change; anything else stays at clarifying, on
// more-questions.
func ivHoldMove(outcome string, origin state.Stage) (state.Stage, state.Trigger) {
	switch {
	case outcome == outcomeSpec && origin != "":
		return state.SpecReview, state.TriggerSpecRewritten
	case outcome == outcomeSpec:
		return state.SpecReview, state.TriggerSpecWritten
	case outcome == outcomeUnchanged:
		return origin, state.TriggerDiscussionNoChange
	}
	return state.Clarifying, state.TriggerMoreQuestions
}

// ivHoldFields is what Apply adds to the interviewer's move at the
// clarifying after a hold (#189), and what it does after the move.
type ivHoldFields struct {
	origin state.Stage // the stage the issue came from (ivHoldOrigin), or ""
	revise int64       // the /revise the move names (ivHoldRevise), or 0
}

// holdFields returns what Apply adds to r's move on an issue in state is:
// once the move is written (moved), what its marker says, so a replay
// names the same /revise, and an unchanged outcome goes back to the same
// origin, whatever was read since; else the origin of the clarifying
// after a hold it's at, if it's at one, and from spec-review, the /revise
// that waited through the hold that the run read, bounded by its seen
// (ivHoldRevise).
func (iv *Interviewer) holdFields(ctx context.Context, a *Applier, r Result, is state.Issue, moved bool) (ivHoldFields, error) {
	if moved {
		for _, m := range is.Markers {
			if m.Kind == "transition" && m.Fields["run"] == r.RunID {
				id, _ := state.Key{Field: "cmd", ID: m.Fields["cmd"]}.Comment() // 0 for none
				return ivHoldFields{origin: state.Stage(m.Fields["origin"]), revise: id}, nil
			}
		}
	}
	h := ivHoldFields{origin: ivHoldOrigin(is)}
	if h.origin != state.SpecReview {
		return h, nil
	}
	sp := ivHoldSpanOf(r.S, a.AppSlug, r.Dirt.Has)
	c, err := ivHoldRevise(ctx, a, trustCache{st: iv.Daemon.Step, verdicts: map[string]bool{}}, r.S, r.Dirt, is, sp.From, r.Seen)
	if err != nil {
		return h, fmt.Errorf("the /revise that waited through the hold: %w", err)
	}
	if c != nil {
		h.revise = c.ID
	}
	return h, nil
}

// set returns m with origin, if h has one, and cmd=<the /revise>, if any.
func (h ivHoldFields) set(m Move) Move {
	if h.origin == "" && h.revise == 0 {
		return m
	}
	fields := maps.Clone(m.Fields)
	if fields == nil {
		fields = map[string]string{}
	}
	if h.origin != "" {
		fields["origin"] = string(h.origin)
	}
	if h.revise != 0 {
		fields["cmd"] = strconv.FormatInt(h.revise, 10)
	}
	m.Fields = fields
	return m
}

// ack 👍s the /revise h names, if any, once its move is written, as for
// any command applied (SPEC §9.2). wrap is Apply's.
func (h ivHoldFields) ack(ctx context.Context, a *Applier, wrap func(error) error) error {
	if h.revise == 0 {
		return nil
	}
	if err := a.Ack(ctx, h.revise); err != nil {
		return wrap(fmt.Errorf("the /revise it handled: %w", err))
	}
	return nil
}

// ivHoldStanding is the line of an unchanged outcome's comment on the
// revision that still stands at to, its origin (renderUnchanged): at
// spec-review, the latest revision (state.LatestRevision), while the body
// isn't dirty and still has its hash, and nothing hides it, as only then
// does a 👍 or /approve on it approve it (SPEC §7.4). Otherwise it's "",
// and ReviewVisit posts a new revision once the issue is at spec-review.
func ivHoldStanding(s *github.Snapshot, appSlug string, d Dirt, to state.Stage) string {
	latest, ok := state.LatestRevision(s, appSlug, state.SpecRevision, d.Has)
	if to != state.SpecReview || !ok || latest.Hidden || d.Has(state.BodyID) || latest.Hash != state.BodyHash(s.Body) {
		return ""
	}
	return fmt.Sprintf("Revision %d of the spec, `%s`, still stands: to approve it, react 👍 to it, or post `/approve`.", latest.Rev, latest.Hash)
}

// ivHoldWait is what a move to the clarifying after a hold says of what
// comes next (Q3b): the interviewer reads the discussion on an issue of a
// class it runs on (interviewerClass); any other issue waits there until a
// code owner sets its stage by hand (resolveWait).
func ivHoldWait(it state.Item) string {
	if interviewerClass(it) {
		return "The interviewer reads the discussion next, and picks where it goes from there."
	}
	return resolveWait
}
