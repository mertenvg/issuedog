package daemon

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// approvedLabel is the label by which a trusted user lets an issue into the
// pipeline (SPEC §6.2).
const approvedLabel = "approved"

// IntakeVisit ends a visit to an issue at intake: the loop calls it after
// ReviewVisit, and at any other stage it does nothing. It moves the issue
// to clarifying on §7.2's "issue is trusted" row, by the first two ways in
// of plan #6's assumption 1:
//
//   - The issue has stage:intake and a trusted author. The move is keyed
//     event=<the labeled event of stage:intake that stands>.
//   - A trusted user added approved, and the issue has stage:intake or no
//     stage label. The move is keyed event=<that labeled event>.
//
// Otherwise the issue stays: an untrusted author's issue waits for approved,
// and one with no stage label and no approved is left to triage. Its
// comment says why the issue is in and, under plan #6's Q1a, tells a bug or
// a task that it waits at clarifying until phase 3, and a story with a
// parent that it waits there too, since the interviewer doesn't run on it.
//
// CheckLabels has run on s, so its labels are the ones that stand. The
// events are read from a fresh timeline, and the move waits for the next
// visit if the timeline's stage labels no longer match s's, since an
// override made after CheckLabels must be recorded first (Transition). An
// event a marker already names, as when an owner's override brought the
// issue back to intake, can't key a second move: that move is keyed by the
// comment of the transition that put the issue at intake instead. A move the
// table lacks, for an issue of unknown type, is reported once by Transition,
// and the issue stays.
func (st *Step) IntakeVisit(ctx context.Context, s *github.Snapshot) error {
	wrap := func(err error) error { return fmt.Errorf("intake of %s/%s#%d: %w", st.Owner, st.Repo, s.Number, err) }
	if s.Closed {
		return nil
	}
	d, err := st.Dirty(ctx, s, st.trustOnceTrust())
	if err != nil {
		return wrap(err)
	}
	is := state.Derive(s, st.AppSlug, d.Has)
	if is.Stage != state.Intake {
		return nil
	}
	stages := stageLabels(s.Labels)
	labelled := slices.Equal(stages, []string{"stage:intake"})
	if !labelled && len(stages) > 0 {
		return nil // CheckLabels has put them right, or will next visit
	}
	tc := trustCache{st: st, verdicts: map[string]bool{}}
	byAuthor := false
	if labelled {
		if byAuthor, err = tc.trusted(ctx, s.Author); err != nil {
			return wrap(err)
		}
	}
	approved := slices.ContainsFunc(s.Labels, func(l string) bool { return strings.EqualFold(l, approvedLabel) })
	if !byAuthor && !approved {
		return nil
	}
	timeline, err := st.Client.TimelineSince(ctx, st.Owner, st.Repo, s.Number, time.Time{})
	if err != nil {
		return wrap(err)
	}
	if now := timelineStages(timeline); !slices.Equal(now, stages) {
		logr.Debugf("%s/%s#%d: no move from intake yet: its stage labels changed since the label rules ran, from %v to %v", st.Owner, st.Repo, s.Number, stages, now)
		return nil
	}
	var ev *gogithub.Timeline
	var text string
	if byAuthor {
		if ev, err = tc.admitting(ctx, timeline, "stage:intake", true); err != nil {
			return wrap(err)
		}
		text = fmt.Sprintf("%s opened this issue, and it has `stage:intake`, so it's in the pipeline (SPEC §6.2). I've moved it to `clarifying`.", login(s.Author))
	}
	if ev == nil && approved {
		if ev, err = tc.admitting(ctx, timeline, approvedLabel, false); err != nil {
			return wrap(err)
		}
		if ev != nil {
			text = fmt.Sprintf("%s added `approved`, so this issue is in the pipeline (SPEC §6.2). I've moved it to `clarifying`.", ev.GetActor().GetLogin())
		}
	}
	if ev == nil {
		logr.Debugf("%s/%s#%d: no move from intake yet: the timeline has no labeled event that stands for its labels %v", st.Owner, st.Repo, s.Number, s.Labels)
		return nil
	}
	text += intakeNote(s)
	key := state.Key{Field: "event", ID: strconv.FormatInt(ev.GetID(), 10)}
	if is.Moved(key) {
		id := lastTransition(s, st.AppSlug, d.Has)
		if id == 0 {
			return wrap(fmt.Errorf("event %d keys a transition, but no transition comment counts", ev.GetID()))
		}
		key.ID = strconv.FormatInt(id, 10)
		logr.Infof("%s/%s#%d: event %d already keys a transition, so the move from intake is keyed by the transition comment %s", st.Owner, st.Repo, s.Number, ev.GetID(), key.ID)
	}
	err = st.Transition(ctx, s, d.Has, Move{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: key, Text: text})
	switch {
	case StateError(err):
		return nil // reported once by Transition; a new type reads the issue again
	case err != nil:
		return wrap(err)
	}
	logr.Infof("%s/%s#%d: moved from intake to clarifying, keyed event=%s", st.Owner, st.Repo, s.Number, key.ID)
	return nil
}

// intakeNote is what a move into the pipeline tells an issue that waits at
// clarifying, under plan #6's Q1a, after a space, or "": a bug or a task
// waits until phase 3, and a story with a parent waits too, since the
// interviewer doesn't run on it.
func intakeNote(s *github.Snapshot) string {
	switch {
	case s.Type == "bug" || s.Type == "task":
		return fmt.Sprintf(" It's a %s: bugs and tasks wait at `clarifying` for now, until issuedog can implement them (build phase 3).", s.Type)
	case s.Type == "story" && s.Parent != 0:
		return " It's a story with a parent: those wait at `clarifying` for now, as issuedog only clarifies epics, stories with no parent and untyped issues."
	}
	return ""
}

// trustCache asks st.Trust about each login once. No bot is trusted.
type trustCache struct {
	st       *Step
	verdicts map[string]bool
}

func (tc trustCache) trusted(ctx context.Context, a github.Author) (bool, error) {
	if a.Type == "Bot" || a.Login == "" {
		return false, nil
	}
	login := strings.ToLower(a.Login)
	if v, ok := tc.verdicts[login]; ok {
		return v, nil
	}
	v, err := tc.st.Trust.Trusted(ctx, a.Login)
	if err != nil {
		return false, fmt.Errorf("trust %s: %w", a.Login, err)
	}
	tc.verdicts[login] = v
	return v, nil
}

// admitting returns the change to the label name in timeline that stands,
// if it added the label: the newest change by a trusted user or the App,
// passing over untrusted changes, as CheckLabels reverts them. With app
// unset, an add by the App is a restore, so the trusted add before it is
// returned. It returns nil if the label's newest change of all removed it,
// or the change that stands did.
func (tc trustCache) admitting(ctx context.Context, timeline []*gogithub.Timeline, name string, app bool) (*gogithub.Timeline, error) {
	newest := true
	for _, e := range slices.Backward(timeline) {
		if !isLabelEvent(e) || !strings.EqualFold(e.GetLabel().GetName(), name) {
			continue
		}
		if newest && e.GetEvent() != "labeled" {
			return nil, nil
		}
		newest = false
		added := e.GetEvent() == "labeled"
		actor := github.Author{Login: e.GetActor().GetLogin(), Type: e.GetActor().GetType()}
		if state.IsApp(actor, tc.st.AppSlug) {
			switch {
			case added && app:
				return e, nil
			case !added:
				return nil, nil
			}
			continue // a restore: the trusted add it restored is the key
		}
		ok, err := tc.trusted(ctx, actor)
		if err != nil {
			return nil, fmt.Errorf("event %d: %w", e.GetID(), err)
		}
		switch {
		case ok && added:
			return e, nil
		case ok:
			return nil, nil
		}
	}
	return nil, nil
}

// stageLabels returns the stage:* labels in labels, in lower case and sorted.
func stageLabels(labels []string) []string {
	var stages []string
	for _, l := range labels {
		if isStageLabel(l) {
			stages = append(stages, strings.ToLower(l))
		}
	}
	slices.Sort(stages)
	return slices.Compact(stages)
}

// timelineStages returns the stage:* labels whose newest event in timeline
// added them, in lower case and sorted.
func timelineStages(timeline []*gogithub.Timeline) []string {
	on := map[string]bool{}
	for _, e := range timeline {
		if l := e.GetLabel().GetName(); isLabelEvent(e) && isStageLabel(l) {
			on[strings.ToLower(l)] = e.GetEvent() == "labeled"
		}
	}
	var stages []string
	for l, ok := range on {
		if ok {
			stages = append(stages, l)
		}
	}
	slices.Sort(stages)
	return stages
}

func isLabelEvent(e *gogithub.Timeline) bool {
	return e.GetEvent() == "labeled" || e.GetEvent() == "unlabeled"
}

// lastTransition returns the ID of the comment with the latest transition
// marker that counts, as state.Derive reads them, or 0 if there is none.
func lastTransition(s *github.Snapshot, appSlug string, dirty func(int64) bool) int64 {
	var id int64
	for _, c := range s.Comments {
		if dirty(c.ID) {
			continue
		}
		for _, m := range github.TrustedMarkers(c.Author, c.Body, appSlug) {
			if _, ok := state.RoundBound(m, c.ID); ok {
				id = c.ID
			}
		}
	}
	return id
}
