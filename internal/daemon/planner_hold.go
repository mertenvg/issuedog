package daemon

import (
	"context"
	"fmt"
	"slices"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file is what the planner reads of the discussion hold whose
// interviewer run sent its issue to planning, by its plan outcome (#294)
// or, after a hold from planning, its unchanged one: the interviewer's
// notes, only after plan (#340), the discussion's owner input, and the
// /revise that waited through the hold.

// plannerHold is the discussion hold that sent an issue to planning, as
// plannerHoldOf reads it. Its zero value is none.
type plannerHold struct {
	// Move is the clean App comment of the interviewer's clarifying →
	// planning move (ivHoldToPlanning), on plan-changed or, after a hold
	// from planning, discussion-no-change, or nil.
	Move *github.Comment
	// Notes is whether Move is the plan outcome's (ivPlanChanged), so its
	// message is the interviewer's notes on what changes in the plan.
	Notes bool
	// Input is the discussion's owner input (ivHoldInputOf), read as the
	// interviewer read it, up to Move.
	Input ivHoldInput
	// Discussion is the issue's comments from the discussion's start up to
	// Move, which Input names comments of.
	Discussion []github.Comment
	// Revise are the clean /revise comments the interviewer's moves at the
	// clarifying after the hold named, oldest first: each a waiting
	// /revise that a hold from plan-review handed it (ivHoldRevise).
	Revise []github.Comment
}

// ivReplanField is the field the interviewer's plan outcome's move
// carries, as replan=1, so it's told from its unchanged outcome's move
// after a hold from planning, the same clarifying → planning with run= and
// origin= (SPEC §5).
const ivReplanField = "replan"

// ivHoldToPlanning reports whether m is the interviewer's move to planning
// at the clarifying after a discussion hold: a run's clarifying →
// planning, carrying origin. Only its plan and unchanged outcomes make it
// (SPEC §7.2), and unchanged only after a hold from planning, as it goes
// back to origin.
func ivHoldToPlanning(m github.Marker) bool {
	return m.Kind == "transition" && m.Fields["run"] != "" && m.Fields["origin"] != "" &&
		state.Stage(m.Fields["from"]) == state.Clarifying && state.Stage(m.Fields["to"]) == state.Planning
}

// ivPlanChanged reports whether m is the interviewer's plan outcome's
// move: its move to planning after a hold (ivHoldToPlanning) with
// replan=1 or an origin other than planning, as unchanged's move goes to
// planning only after a hold from planning, with no replan. A plan move
// written before the field was added has none either: after a hold from
// plan-review or planned, its origin still shows it's plan, but after a
// hold from planning it reads as unchanged, the only thing a move without
// replan can mean there, so the planner runs without the interviewer's
// notes, with the owner input still (SPEC §12). m is a marker the App
// wrote, as planEntry and TrustedMarkers read them, so nobody else's
// replan counts.
func ivPlanChanged(m github.Marker) bool {
	return ivHoldToPlanning(m) && (m.Fields[ivReplanField] == "1" || state.Stage(m.Fields["origin"]) != state.Planning)
}

// plannerHoldOf returns, for issue s in state is with dirty content d,
// the discussion hold that sent it to planning, while the move that put it
// at planning (planEntry) is the interviewer's, by its plan outcome or
// its unchanged one (ivHoldToPlanning):
//   - Move, the comment that carries that move, and Notes, whether it's
//     plan's (ivPlanChanged), whose message is notes for the planner;
//   - Input, the discussion's owner input, from the bound before the
//     after-hold period's first hold (ivHoldSpan) up to Move, read as the
//     interviewer read it at that clarifying (ivHoldInputOf), so a comment
//     a trusted 👍 approved, or a trusted comment that countered one, such
//     as the owner's "Yes, move T1 after T2", reaches the planner after
//     either outcome: the move's seen covers the hold, so none of it is in
//     the run's inputs (state.Inputs);
//   - Revise, every /revise the interviewer's moves in that period named
//     with cmd=, its own last move included: a /revise that waited through
//     a hold from plan-review was meant for the plan, and a question round
//     before the plan outcome may have named it.
//
// Otherwise it's the zero plannerHold. It reads the reactions of the
// discussion's untrusted comments, as the interviewer's context does, and
// trust through tc.
func plannerHoldOf(ctx context.Context, a *Applier, tc trustCache, s *github.Snapshot, d Dirt, is state.Issue) (plannerHold, error) {
	entry := planEntry(is)
	if !ivHoldToPlanning(entry) {
		return plannerHold{}, nil
	}
	at := slices.IndexFunc(s.Comments, func(c github.Comment) bool {
		return !d.Has(c.ID) && slices.ContainsFunc(github.TrustedMarkers(c.Author, c.Body, a.AppSlug), func(m github.Marker) bool {
			return ivHoldToPlanning(m) && m.Fields["run"] == entry.Fields["run"]
		})
	})
	if at < 0 {
		return plannerHold{}, nil // planEntry reads only clean App comments, so this doesn't happen
	}
	// The issue as it was when the move was written: the after-hold period
	// it ends, and its discussion, are in the comments before it.
	before := *s
	before.Comments = s.Comments[:at]
	sp := ivHoldSpanOf(&before, a.AppSlug, d.Has)
	h := plannerHold{Move: &s.Comments[at], Notes: ivPlanChanged(entry)}
	if !sp.Period {
		return h, nil
	}
	in, err := ivHoldInputOf(ctx, a, tc, &before, d, sp.From)
	if err != nil {
		return plannerHold{}, fmt.Errorf("the discussion's owner input: %w", err)
	}
	h.Input = in
	var named []int64
	for _, c := range s.Comments[:at+1] {
		if c.ID <= sp.From || d.Has(c.ID) {
			continue
		}
		h.Discussion = append(h.Discussion, c)
		for _, m := range github.TrustedMarkers(c.Author, c.Body, a.AppSlug) {
			if m.Kind != "transition" || m.Fields["run"] == "" || m.Fields["origin"] == "" || state.Stage(m.Fields["from"]) != state.Clarifying {
				continue
			}
			if id, ok := (state.Key{Field: "cmd", ID: m.Fields["cmd"]}).Comment(); ok {
				named = append(named, id)
			}
		}
	}
	for _, c := range s.Comments {
		if slices.Contains(named, c.ID) && !d.Has(c.ID) {
			h.Revise = append(h.Revise, c)
		}
	}
	return h, nil
}

// sections are input.md's sections for h, after the inputs (plannerInputs):
// the interviewer's move, whose message is its notes on what changes in the
// plan, only after plan (Notes), then the discussion's owner input, then
// each /revise, as revision notes for the plan. Each is fenced under a
// heading the daemon writes, its text escaped (ivSection). Without a move,
// there are none.
func (h plannerHold) sections() []string {
	if h.Move == nil {
		return nil
	}
	var sections []string
	if h.Notes {
		sections = append(sections, ivSection(contextAt(*h.Move)+": the interviewer's move to planning after a discussion hold, "+
			"whose message is its notes on what the discussion changes in the plan", ivStripMarkers(h.Move.Body)))
	}
	for _, c := range h.Discussion {
		switch by, approved := h.Input.Approved[c.ID]; {
		case approved:
			sections = append(sections, ivSection(contextAt(c)+", during the discussion hold, approved by "+ivLogin(by)+
				"'s 👍, so it's owner input", c.Body))
		case slices.Contains(h.Input.Countered, c.ID) && !slices.ContainsFunc(h.Revise, func(r github.Comment) bool { return r.ID == c.ID }):
			// A /revise that counters is listed once, below, as the notes.
			sections = append(sections, ivSection(contextAt(c)+", during the discussion hold, a trusted comment "+
				"that counters the untrusted comments before it, so it's owner input", c.Body))
		}
	}
	for _, c := range h.Revise {
		sections = append(sections, ivSection(contextAt(c)+": a `/revise` that waited through the hold, "+
			"whose text is revision notes for the plan", c.Body))
	}
	return sections
}
