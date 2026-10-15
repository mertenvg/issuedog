package daemon

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// A code owner answers a flag, the triage run's question (plan #6,
// assumption 3 and Q6a), or skips triage altogether (Q2a):
//   - /track admits the issue: a move to clarifying on §7.2's "issue is
//     trusted" row, keyed cmd=<comment id>.
//   - /ignore records "left": a triage marker, verdict=leave, keyed
//     cmd=<comment id>.
//   - A 👍 on the latest flag takes its recommendation, recorded the same
//     way, keyed cmd=r<reaction id>.
//
// Step counts only trusted, clean comments, and FlagByReaction only
// trusted 👍s, so on an untrusted author's issue each answer is the §6.2
// approval, as approved is. FlagVisit keeps the triage:* labels in line.

// FlagCommands returns the handlers of /track and /ignore, for
// Step.Commands. state.Command.Check has rejected either one anywhere but
// at intake.
func FlagCommands() map[string]Handler {
	return map[string]Handler{
		"track":  {Check: flagTrackCheck, Apply: flagTrack},
		"ignore": {Check: flagIgnoreCheck, Apply: flagIgnore},
	}
}

// flagTrackCheck rejects a /track when the table has no move to
// clarifying for the issue, as for a type the pipeline doesn't know. Once
// Apply's marker exists, the stage is clarifying, so Step finishes the
// command before it asks Check.
func flagTrackCheck(_ context.Context, a *Applier, c Cmd) error {
	why, err := noRow(state.Derive(c.S, a.AppSlug, c.Dirt.Has), flagAdmit(c.S, state.CmdKey(c.Comment.ID), ""))
	switch {
	case StateError(err):
		return Rejection{Reason: "this issue can't move to `clarifying`: " + why}
	case err != nil:
		return fmt.Errorf("check /track: %w", err)
	}
	return nil
}

// flagTrack admits c's issue: the move keyed by c, then the triage:*
// labels off, then the 👍, which ends the command. A crash before the 👍
// leaves the marker, so Step only adds the 👍, and FlagVisit takes the
// labels off.
func flagTrack(ctx context.Context, a *Applier, c Cmd) error {
	m := flagAdmit(c.S, state.CmdKey(c.Comment.ID), login(c.Comment.Author)+" posted `/track`")
	m.NoAck = true
	if err := a.Transition(ctx, c.S, c.Dirt.Has, m); err != nil {
		return fmt.Errorf("track: %w", err)
	}
	if err := triageLabels(ctx, a, c.S, ""); err != nil {
		return fmt.Errorf("track: %w", err)
	}
	if err := a.Ack(ctx, c.Comment.ID); err != nil {
		return fmt.Errorf("track: %w", err)
	}
	logr.Infof("%s/%s#%d: /track in comment %d admitted it to clarifying", a.Owner, a.Repo, c.S.Number, c.Comment.ID)
	return nil
}

// flagAdmit is the move of s into the pipeline by key, a command's or a
// 👍's. by says who let the issue in, and the text then says whether it
// waits at clarifying (intakeNote).
func flagAdmit(s *github.Snapshot, key state.Key, by string) Move {
	return Move{To: state.Clarifying, Trigger: state.TriggerTrusted, Key: key,
		Text: by + ", so this issue is in the pipeline (SPEC §6.2). I've moved it to `clarifying`." + intakeNote(s)}
}

// flagIgnoreCheck rejects an /ignore on an issue in the pipeline (see
// flagInPipeline), or one left to people already.
func flagIgnoreCheck(_ context.Context, a *Applier, c Cmd) error {
	is := state.Derive(c.S, a.AppSlug, c.Dirt.Has)
	if why := flagInPipeline(c.S, is); why != "" {
		return Rejection{Reason: "only an issue with no stage label can be left to people, and " + why}
	}
	if is.Triage.Verdict == triageLeave {
		return Rejection{Reason: "issuedog has left this issue to people already", Final: true}
	}
	return nil
}

// flagIgnore records that issuedog leaves c's issue to people: the triage
// marker keyed by c, then triage:left, then the 👍, which ends the
// command. The marker names c, so once it's there Step only adds the 👍,
// and FlagVisit finishes the labels.
func flagIgnore(ctx context.Context, a *Applier, c Cmd) error {
	key := state.CmdKey(c.Comment.ID)
	if err := flagLeave(ctx, a, c.S, c.Dirt, key, login(c.Comment.Author)+" posted `/ignore`, so issuedog leaves this issue to people."); err != nil {
		return fmt.Errorf("ignore: %w", err)
	}
	if err := a.Ack(ctx, c.Comment.ID); err != nil {
		return fmt.Errorf("ignore: %w", err)
	}
	return nil
}

// flagLeave posts the triage marker verdict=leave keyed by key, after
// text, escaped (SPEC §5.1), unless a clean one with that key is on s
// already, then makes triage:left the issue's triage label.
func flagLeave(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt, key state.Key, text string) error {
	is := state.Derive(s, a.AppSlug, d.Has)
	if !slices.ContainsFunc(is.Markers, func(m github.Marker) bool { return m.Kind == "triage" && m.Fields[key.Field] == key.ID }) {
		marker, err := github.FormatMarker(github.Marker{Kind: "triage", Fields: map[string]string{"verdict": triageLeave, key.Field: key.ID}})
		if err != nil {
			return fmt.Errorf("left marker: %w", err)
		}
		text = github.Escape(text + " A code owner can post `/track` to bring it into the pipeline.")
		if err := a.comment(ctx, s, join(text, marker)); err != nil {
			return fmt.Errorf("left: %w", err)
		}
		logr.Infof("%s/%s#%d: recorded left, keyed %s=%s", a.Owner, a.Repo, s.Number, key.Field, key.ID)
	}
	if err := triageLabels(ctx, a, s, triageLeftLabel); err != nil {
		return fmt.Errorf("left: %w", err)
	}
	return nil
}

// flagInPipeline says why issue s, in state is, is in the pipeline, or
// returns "" if it isn't: it has a stage label, one left out included, or
// a transition marker.
func flagInPipeline(s *github.Snapshot, is state.Issue) string {
	switch labels := stageLabels(s.Labels); {
	case len(labels) > 0:
		return "this one has `" + labels[0] + "`"
	case leftOutStage(s):
		return "this one has a stage label"
	case slices.ContainsFunc(is.Markers, func(m github.Marker) bool { return m.Kind == "transition" }):
		return "this one has been in the pipeline"
	}
	return ""
}

// FlagByReaction returns the reaction trigger for Step.Reactions that
// answers a flag by 👍 (plan #6, Q6a). At intake, while the latest triage
// marker is a flag, the earliest 👍 on the flag from a user trust trusts,
// not a bot, takes the flag's recommendation, from its marker's recommend:
//   - handle admits the issue, an untrusted author's too, keyed
//     cmd=r<reaction id>. A move the table lacks is reported once by
//     Transition (SPEC §5.3), then only logged.
//   - leave records "left", keyed the same way, unless the issue is in the
//     pipeline (flagInPipeline), as /ignore's check says.
//
// 👍s on an older flag, untrusted 👍s and the App's are ignored, and so is
// a flag answered already by a transition after it, as when an override
// brought the issue back to intake. Each answer applies once: its move
// takes the issue past intake, and its "left" comment is the latest triage
// marker, so the flag is no longer the latest. A crash before the labels
// leaves them to FlagVisit, and a crash between a move's marker and its
// stage label to the label rules' unfinished-write repair (SPEC §5.1).
func FlagByReaction(trust github.Trust) func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error {
	return func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error {
		wrap := func(err error) error {
			return fmt.Errorf("answers to the flag on %s/%s#%d: %w", a.Owner, a.Repo, s.Number, err)
		}
		is := state.Derive(s, a.AppSlug, d.Has)
		flag := is.Triage
		if s.Closed || is.Stage != state.Intake || flag.Verdict != triageAsk {
			return nil
		}
		reactions, err := a.Client.Reactions(ctx, a.Owner, a.Repo, flag.Comment)
		if err != nil {
			return wrap(err)
		}
		var first *github.Reaction
		for _, r := range reactions {
			if r.Content != "+1" || r.User.Type == "Bot" || (first != nil && first.ID < r.ID) {
				continue
			}
			t, err := trust.Trusted(ctx, r.User.Login)
			if err != nil {
				return wrap(fmt.Errorf("trust %s: %w", r.User.Login, err))
			}
			if t {
				first = &r
			}
		}
		if first == nil {
			return nil
		}
		key := state.ReactionKey(first.ID)
		if lastTransition(s, a.AppSlug, d.Has) > flag.Comment {
			logr.Debugf("%s/%s#%d: ignoring the 👍 on the flag in comment %d: a move after it answered it", a.Owner, a.Repo, s.Number, flag.Comment)
			return nil
		}
		by := login(first.User) + "'s 👍 on the flag takes its recommendation"
		switch flag.Recommend {
		case triageHandle:
			err := a.Transition(ctx, s, d.Has, flagAdmit(s, key, by))
			if StateError(err) {
				logr.Debugf("%s/%s#%d: ignoring the 👍 on the flag in comment %d: %v", a.Owner, a.Repo, s.Number, flag.Comment, err)
				return nil
			}
			if err != nil {
				return wrap(err)
			}
			if err := triageLabels(ctx, a, s, ""); err != nil {
				return wrap(err)
			}
			logr.Infof("%s/%s#%d: %s's 👍 on the flag admitted it to clarifying", a.Owner, a.Repo, s.Number, login(first.User))
		case triageLeave:
			if why := flagInPipeline(s, is); why != "" {
				logr.Infof("%s/%s#%d: ignoring the 👍 on the flag in comment %d, which recommends leaving it: %s", a.Owner, a.Repo, s.Number, flag.Comment, why)
				return nil
			}
			if err := flagLeave(ctx, a, s, d, key, by+": issuedog leaves this issue to people."); err != nil {
				return wrap(err)
			}
		default:
			logr.Warnf("%s/%s#%d: ignoring the 👍 on the flag in comment %d: its marker recommends %q", a.Owner, a.Repo, s.Number, flag.Comment, flag.Recommend)
		}
		return nil
	}
}

// FlagVisit ends a visit to an open issue: the loop calls it after
// IntakeVisit. It brings the triage:* labels in line with the latest
// triage marker (plan #6, assumption 3), as assignees follow state: none
// once the issue is in the pipeline (flagInPipeline), else triage:ask for
// a flag, triage:left for "left", and none with no marker. So it puts back
// a label someone else removed, and removes one someone else added, with
// no comment: the labels are never read as state.
func (st *Step) FlagVisit(ctx context.Context, s *github.Snapshot) error {
	if s.Closed {
		return nil
	}
	d, err := st.Dirty(ctx, s)
	if err != nil {
		return fmt.Errorf("triage labels of %s/%s#%d: %w", st.Owner, st.Repo, s.Number, err)
	}
	is := state.Derive(s, st.AppSlug, d.Has)
	label := ""
	switch {
	case flagInPipeline(s, is) != "":
	case is.Triage.Verdict == triageAsk:
		label = triageAskLabel
	case is.Triage.Verdict == triageLeave:
		label = triageLeftLabel
	}
	if err := triageLabels(ctx, st.Applier, s, label); err != nil {
		return fmt.Errorf("triage labels of %s/%s#%d: %w", st.Owner, st.Repo, s.Number, err)
	}
	return nil
}

// flagAsked reports whether label is triage:ask, in any case: the issue
// has a flag waiting on a code owner, whose 👍 changes no updated_at, so
// Iterate visits it on every pass. FlagVisit keeps the label in line with
// the flag, and someone else's change to it is an update, which a poll
// finds.
func flagAsked(label string) bool { return strings.EqualFold(label, triageAskLabel) }

// flagAdmittedAt returns when a command admitted s, and whether one did
// (#133's note on #141): the earliest time of a trusted, clean command
// comment, or a trusted 👍 on a clean flag, that a clean App transition
// out of intake into the pipeline names in its cmd. A dirty or deleted
// transition or command, or a 👍 since removed, names no time.
func flagAdmittedAt(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt, tc trustCache) (time.Time, bool, error) {
	var at time.Time
	admitted := false
	earliest := func(t time.Time) {
		if !admitted || t.Before(at) {
			at, admitted = t, true
		}
	}
	var reactions []int64
	var flags []int64
	for _, c := range s.Comments {
		if d.Has(c.ID) {
			continue
		}
		for _, m := range github.TrustedMarkers(c.Author, c.Body, a.AppSlug) {
			if m.Kind == "triage" && m.Fields["verdict"] == triageAsk {
				flags = append(flags, c.ID)
			}
			to := state.Stage(m.Fields["to"])
			if m.Kind != "transition" || state.Stage(m.Fields["from"]) != state.Intake || !to.Open() || to == state.Intake || to == state.Held {
				continue
			}
			key := m.Fields["cmd"]
			if r, ok := strings.CutPrefix(key, "r"); ok {
				if id, err := strconv.ParseInt(r, 10, 64); err == nil { // no reaction has an ID of 0 or less, so none matches one
					reactions = append(reactions, id)
				}
				continue
			}
			id, ok := state.Key{Field: "cmd", ID: key}.Comment()
			i := slices.IndexFunc(s.Comments, func(x github.Comment) bool { return x.ID == id })
			if !ok || i < 0 || d.Has(id) {
				continue
			}
			trusted, err := tc.trusted(ctx, s.Comments[i].Author)
			if err != nil {
				return time.Time{}, false, fmt.Errorf("comment %d: %w", id, err)
			}
			if trusted {
				earliest(s.Comments[i].CreatedAt)
			}
		}
	}
	if len(reactions) == 0 {
		return at, admitted, nil
	}
	for _, f := range flags {
		got, err := a.Client.Reactions(ctx, a.Owner, a.Repo, f)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("the 👍s on the flag in comment %d: %w", f, err)
		}
		for _, r := range got {
			if r.Content != "+1" || !slices.Contains(reactions, r.ID) {
				continue
			}
			trusted, err := tc.trusted(ctx, r.User)
			if err != nil {
				return time.Time{}, false, fmt.Errorf("reaction %d: %w", r.ID, err)
			}
			if trusted {
				earliest(r.CreatedAt)
			}
		}
	}
	return at, admitted, nil
}
