package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// seen is the memory of unexplained labels: a.Unexplained, or a's own.
func (a *Applier) seen() *github.Unexplained {
	if a.Unexplained != nil {
		return a.Unexplained
	}
	return &a.unexplained
}

// unexplainedDue reports whether one of issue n's unexplained labels, left
// out until then, is due for removal by now, GitHub's clock: then the issue
// must be read again, for CheckLabels to remove it. Nothing acts on such a
// label before then, so the issue needn't be read again for it sooner.
func (a *Applier) unexplainedDue(n int, now time.Time) bool {
	due, ok := a.seen().Due(a.Owner, a.Repo, n)
	return ok && !now.Before(due)
}

// hide leaves out of s, the issue read again by a path that doesn't run
// CheckLabels on it, the labels CheckLabels last held back or left out on
// it, so nothing acts on them there either.
func (a *Applier) hide(s *github.Snapshot) {
	s.LeftOut = a.seen().Labels(a.Owner, a.Repo, s.Number)
	s.SetLabels(s.Labels)
}

// hidden is a copy of s with hide applied, for a check on a snapshot that
// CheckLabels may yet run on, which must keep all its labels.
func (a *Applier) hidden(s *github.Snapshot) *github.Snapshot {
	c := *s
	c.LeftOut = maps.Clone(s.LeftOut)
	for l := range a.seen().Labels(a.Owner, a.Repo, s.Number) {
		if c.LeftOut == nil {
			c.LeftOut = map[string]bool{}
		}
		c.LeftOut[l] = true
	}
	c.SetLabels(s.Labels)
	return &c
}

// forgetLabels forgets issue n's unexplained labels, once it's closed or
// gone, so the memory of them doesn't outlive it.
func (a *Applier) forgetLabels(n int) { a.seen().ForgetIssue(a.Owner, a.Repo, n) }

// ErrLabelsHeld is wrapped by CheckLabels' error for an issue with a label
// held back: one with no event to explain it yet. Iterate skips the rest of
// step 1 and Work for the issue, and reads it again next pass.
var ErrLabelsHeld = errors.New("labels held back until their events explain them, or they are removed")

// CheckLabels applies the label rules (SPEC §5.1) to the issue s and acts on
// what github.CheckLabels finds. It must run before any Transition on s.
//
//   - An override, a trusted user's change of the stage label by hand, gets a
//     transition marker with by=<login> and event=<id>. That includes a change
//     made while a transition was in flight, so the owner's intent wins. It
//     isn't looked up in the table. A move to held is an owner hold (SPEC §6.4), so it also gets
//     at=<previous> and reason=owner. Holds stack, so stage:held removed and
//     added back by hand on a held issue, which github.CheckLabels reports
//     as an override, is an owner hold of its own (plan #87 rev 2, Q2b): a
//     held → held marker with reason=owner that keeps the first hold's at
//     and origin. Each move into, within or out of held also carries the
//     round bound from before the hold in seen. An override to any other
//     stage the issue is at already gets no marker. An issue whose
//     labels were set by hand, with no marker of the App's, is adopted this
//     way (plan #5, assumption 6).
//   - A disagreement gets one error comment keyed by its timeline event
//     (SPEC §5.3), then its stage labels are set to match the latest marker.
//     An override to a label that names no stage is one too. With no marker,
//     only that label is removed.
//   - An Unfinished disagreement, the App's own write cut short, only gets
//     its labels set.
//
// The writes go in plan #5's assumption 4 order, the comment first, so a
// pass that crashed part way can run again: once an override's marker is
// posted, the override is older than the latest marker, and what's left is
// an Unfinished disagreement; an error comment is posted once per key. The
// writes are added to s, and s.SettingsEvent and s.TypeEvent are set, the
// latter from the issue types or the type:* labels, as a.IssueTypes says,
// and so is s.DiscRenames, for the discussion triggers (discussion.go).
// On a repo that uses issue types, the issue type is guarded too
// (issueTypeRevertGuard): an untrusted change of it is reverted, and s.Type
// set back with it, and a type no event explains yet is held back, as such
// a label is. dirty is as for state.Derive.
//
// A pipeline label that nothing on the timeline explains yet, whose events
// may be late or which a repo label rename may have put on, is held back
// (github.CheckLabels). Then nothing else is acted on, and the error wraps
// ErrLabelsHeld: the caller acts on nothing of s, and reads it again later.
// That lasts github.HoldBound at most, for each issue: after it, such a
// label is left out of s.Labels, and s.Type with it, so the caller goes on
// without acting on it, and Iterate reads the issue again when the label
// falls due for removal (unexplainedDue), or sooner if the issue changes.
func (a *Applier) CheckLabels(ctx context.Context, s *github.Snapshot, trust github.Trust, dirty func(int64) bool) error {
	// The labels as read, those left out before included, so a label left
	// out is checked again.
	given := slices.Clone(s.Labels)
	for l := range s.LeftOut {
		if !slices.ContainsFunc(given, func(x string) bool { return strings.EqualFold(x, l) }) {
			given = append(given, l)
		}
	}
	found, err := a.Client.CheckLabels(ctx, a.Owner, a.Repo, s.Number, given, a.AppSlug, trust, dirty, a.seen(), a.issueTypeRevertGuard(s))
	if err != nil {
		return fmt.Errorf("labels of %s/%s#%d: %w", a.Owner, a.Repo, s.Number, err)
	}
	labels, events, settings, held := found.Labels, found.Events, found.Settings, found.Held
	// CheckLabels may have reverted labels since s was read, or left out
	// some it can't explain yet: those stay out of s.Labels, however the
	// labels are read again (SetLabels).
	// Those are the unexplained ones it remembers, held back or not, that
	// it didn't return; not those it or settle removed.
	s.LeftOut = a.seen().Labels(a.Owner, a.Repo, s.Number)
	for l := range s.LeftOut {
		if slices.ContainsFunc(labels, func(x string) bool { return strings.EqualFold(x, l) }) {
			delete(s.LeftOut, l)
		}
	}
	s.SetLabels(labels)
	s.SettingsEvent, s.TypeEvent, s.DiscRenames = settings, found.TypeEvent(a.IssueTypes), found.DiscRenames
	if rv := found.IssueTypeRevert; rv != nil {
		// Nothing after this in the pass acts on the type set back from: not
		// the type check, a type error's end, a move, a stage run or the
		// digest. With no issue type, s.Type comes from the labels, as for
		// an issue read with none.
		s.Type, s.TypeFromLabels = strings.ToLower(rv.To), rv.To == ""
		s.SetLabels(s.Labels)
	}
	if len(held) > 0 {
		return fmt.Errorf("labels of %s/%s#%d: %v: %w", a.Owner, a.Repo, s.Number, held, ErrLabelsHeld)
	}
	for _, ev := range events {
		to, ok := state.ParseLabel(ev.Label)
		if ev.Kind == github.LabelOverride && ok {
			err = a.override(ctx, s, dirty, ev, to)
		} else {
			err = a.disagreement(ctx, s, dirty, ev)
		}
		if err != nil {
			return fmt.Errorf("labels of %s/%s#%d: %s at event %d: %w", a.Owner, a.Repo, s.Number, ev.Kind, ev.Key, err)
		}
	}
	return nil
}

// issueTypeRevertGuard is what asks github.CheckLabels to guard s's issue
// type: on a repo that uses issue types (a.IssueTypes), the issue type as s
// has it, or nil on a repo that uses labels, which leaves issue types
// alone, as before (SPEC §5.1). A failed revert is CheckLabels' error, so
// nothing acts on s in this pass, and the next pass tries again.
func (a *Applier) issueTypeRevertGuard(s *github.Snapshot) *github.IssueTypeGuard {
	if !a.IssueTypes {
		return nil
	}
	guard := &github.IssueTypeGuard{}
	if !s.TypeFromLabels {
		guard.Type = s.Type
	}
	return guard
}

// override records ev, a trusted user's move to stage to, then sets the
// labels. CheckLabels reports no override older than the latest marker, so
// it needs no check of its own that ev is recorded. Every move into, within
// or out of held carries the round bound from before the hold
// (state.HeldStack.Seen), so the round the hold came in goes on after it.
func (a *Applier) override(ctx context.Context, s *github.Snapshot, dirty func(int64) bool, ev github.LabelEvent, to state.Stage) error {
	is := state.Derive(s, a.AppSlug, dirty)
	if to != is.Stage || to == state.Held {
		fields := map[string]string{"from": string(is.Stage), "to": string(to), "by": ev.By, "event": strconv.FormatInt(ev.Key, 10)}
		switch h := is.HeldStack; {
		case to == state.Held && is.Stage == state.Held:
			// A hold by hand on a held issue (plan #87 rev 2, Q2b): the
			// first hold's at and origin stay, and this hold is recorded.
			fields["at"], fields["reason"] = string(h.At), "owner"
			if h.Origin != "" {
				fields["origin"] = string(h.Origin)
			}
			state.HeldStackFields(fields, h.Seen)
		case to == state.Held:
			fields["at"], fields["reason"] = string(is.Stage), "owner"
			state.HeldStackFields(fields, is.Round)
		case is.Stage == state.Held:
			// Every hold ends (SPEC §6.4).
			state.HeldStackFields(fields, h.Seen)
		}
		marker, err := github.FormatMarker(github.Marker{Kind: "transition", Fields: fields})
		if err != nil {
			return fmt.Errorf("override: %w", err)
		}
		text := fmt.Sprintf("%s set `%s` by hand, so I've recorded it as an owner override.", ev.By, ev.Label)
		if to == is.Stage {
			text = fmt.Sprintf("%s set `%s` by hand on this held issue, so I've recorded it as an owner hold of its own. "+
				"It stays held until `/resume` here.", ev.By, ev.Label)
		}
		if err := a.comment(ctx, s, join(github.Escape(text), marker)); err != nil {
			return fmt.Errorf("override: %w", err)
		}
		logr.Infof("recorded %s's override to %s on %s/%s#%d", ev.By, to, a.Owner, a.Repo, s.Number)
	}
	if err := a.setStage(ctx, s, to); err != nil {
		return fmt.Errorf("override: %w", err)
	}
	return nil
}

// disagreement reports ev once, unless it's Unfinished, then sets the stage
// labels to match the latest marker. With no marker, ev is a label that names
// no stage, and only that label is removed.
func (a *Applier) disagreement(ctx context.Context, s *github.Snapshot, dirty func(int64) bool, ev github.LabelEvent) error {
	is := state.Derive(s, a.AppSlug, dirty)
	key := strconv.FormatInt(ev.Key, 10)
	desc := fmt.Sprintf("stage labels %v on %s/%s#%d disagree with the latest marker, to=%s", ev.Stages, a.Owner, a.Repo, s.Number, is.Stage)
	labels := "none"
	if len(ev.Stages) > 0 {
		labels = "`" + strings.Join(ev.Stages, "`, `") + "`"
	}
	text := fmt.Sprintf("The stage labels didn't match this issue's latest transition, to `%s`, so I've set them to match. They were: %s.", is.Stage, labels)
	if ev.Marker == "" {
		desc = fmt.Sprintf("%s on %s/%s#%d names no stage", ev.Label, a.Owner, a.Repo, s.Number)
		text = fmt.Sprintf("`%s` isn't a pipeline stage, so I've removed it.", ev.Label)
	}
	switch {
	case ev.Unfinished:
		logr.Infof("finishing the App's own transition: %s", desc)
	case is.Reported("disagreement", key):
		logr.Debugf("state error, reported already: %s", desc)
	default:
		marker, err := github.FormatMarker(github.Marker{Kind: "error", Fields: map[string]string{"kind": "disagreement", "key": key}})
		if err != nil {
			return fmt.Errorf("report disagreement: %w", err)
		}
		// The labels are someone else's text, so a marker in them is escaped.
		if err := a.comment(ctx, s, join(github.Escape(text), marker)); err != nil {
			return fmt.Errorf("report disagreement: %w", err)
		}
		logr.Errorf("state error: %s", desc)
	}
	var err error
	if ev.Marker == "" {
		err = a.removeLabel(ctx, s, ev.Label)
	} else {
		err = a.setStage(ctx, s, is.Stage)
	}
	if err != nil {
		return fmt.Errorf("repair disagreement: %w", err)
	}
	return nil
}

// isStageLabel reports whether l is a stage:* label, naming a stage or not.
func isStageLabel(l string) bool {
	return strings.HasPrefix(strings.ToLower(l), "stage:")
}
