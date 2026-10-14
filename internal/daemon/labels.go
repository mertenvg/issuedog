package daemon

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

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
//     at=<previous> and reason=owner. An override to the issue's current
//     stage, held included, since holds stack, gets no marker. An issue whose
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
// writes are added to s, and s.SettingsEvent is set. dirty is as for
// state.Derive.
//
// A pipeline label that nothing on the timeline explains yet, whose events
// may be late or which a repo label rename may have put on, is held back
// (github.CheckLabels). Then nothing else is acted on, and the error wraps
// ErrLabelsHeld: the caller acts on nothing of s, and reads it again later.
func (a *Applier) CheckLabels(ctx context.Context, s *github.Snapshot, trust github.Trust, dirty func(int64) bool) error {
	labels, events, settings, held, err := a.Client.CheckLabels(ctx, a.Owner, a.Repo, s.Number, s.Labels, a.AppSlug, trust, dirty, a.seen())
	if err != nil {
		return fmt.Errorf("labels of %s/%s#%d: %w", a.Owner, a.Repo, s.Number, err)
	}
	// CheckLabels may have reverted labels since s was read.
	s.Labels, s.SettingsEvent = labels, settings
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

// override records ev, a trusted user's move to stage to, then sets the
// labels. CheckLabels reports no override older than the latest marker, so
// it needs no check of its own that ev is recorded.
func (a *Applier) override(ctx context.Context, s *github.Snapshot, dirty func(int64) bool, ev github.LabelEvent, to state.Stage) error {
	is := state.Derive(s, a.AppSlug, dirty)
	if to != is.Stage {
		fields := map[string]string{"from": string(is.Stage), "to": string(to), "by": ev.By, "event": strconv.FormatInt(ev.Key, 10)}
		if to == state.Held {
			fields["at"], fields["reason"] = string(is.Stage), "owner"
		}
		marker, err := github.FormatMarker(github.Marker{Kind: "transition", Fields: fields})
		if err != nil {
			return fmt.Errorf("override: %w", err)
		}
		text := fmt.Sprintf("%s set `%s` by hand, so I've recorded it as an owner override.", ev.By, ev.Label)
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
