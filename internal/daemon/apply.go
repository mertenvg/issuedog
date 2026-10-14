// Package daemon runs the pipeline. Every GitHub write that changes an
// issue's state goes through its Applier.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// Applier makes the writes that change an issue's state. Each is checked
// before it's made, so a pass that crashed part way can run again (SPEC §5.2).
type Applier struct {
	Client      *github.Client
	Owner, Repo string
	// AppSlug is the App's slug. Its comments are by "<slug>[bot]".
	AppSlug string
	// Touched, if set, is told of each issue that a command on another
	// issue may write to, such as a descendant of a /hold, before it does.
	Touched func(number int)
	// Unexplained, if set, is the memory of unexplained labels CheckLabels
	// keeps between passes. If nil, the Applier keeps its own.
	Unexplained *github.Unexplained

	histories   histories // see Dirty
	unexplained github.Unexplained
}

// Move is one transition to apply.
type Move struct {
	To      state.Stage
	Trigger state.Trigger
	// Key is what caused the move: a cmd, run or event. It makes the move
	// idempotent, and a command comment's key also gets the App's 👍.
	Key state.Key
	// Fields are further marker fields, such as rev and body. They can't
	// be from, to or a key's field.
	Fields map[string]string
	// Text goes before the marker. It is escaped (SPEC §5.1), so it may
	// quote agent output or comments.
	Text string
	// NoAck leaves out the 👍 for a command comment's key: for a move a
	// command makes before its own marker, such as on a descendant.
	NoAck bool
	// Markers go after the transition marker, in the same comment, such as
	// a revision marker.
	Markers []github.Marker
	// ErrorKey, if set, keys the state error for a move the table lacks,
	// instead of Key's ID.
	ErrorKey string
}

// errorKey keys the state error for m, if the table lacks it.
func (m Move) errorKey() string {
	if m.ErrorKey != "" {
		return m.ErrorKey
	}
	return m.Key.ID
}

// keyFields are the marker fields a Key names.
var keyFields = []string{"cmd", "run", "event"}

func (m Move) check() error {
	if !slices.Contains(keyFields, m.Key.Field) || m.Key.ID == "" {
		return fmt.Errorf("bad key %+v", m.Key)
	}
	for f := range m.Fields {
		if f == "from" || f == "to" || slices.Contains(keyFields, f) {
			return fmt.Errorf("field %s is set by the move", f)
		}
	}
	if !m.To.Open() {
		return fmt.Errorf("can't move to %s: nothing closes issues yet", m.To)
	}
	return nil
}

// Transition moves the issue s to m.To through the §7.2 table. It writes in
// plan #5's assumption 4 order, skipping each step already done: the marker
// comment, the new stage label, removing the old ones, then the 👍, unless
// m.NoAck. Its writes are added to s. dirty is as for state.Derive.
//
// The stage labels are set to match the latest marker, so a stage label a
// trusted user set by hand would be overwritten. That's safe only because
// Applier.CheckLabels runs first and records such a change as an override
// (SPEC §5.1): callers run it on a fresh snapshot just before Transition (T11
// will). A change made after that check, while no other marker lands, is
// still caught: the next CheckLabels records it as an override, as it isn't
// in the marker this writes. A snapshot older than the latest marker isn't
// safe, so a run's result must re-read the issue before its Transition.
//
// A move the table lacks, including one for an issue of unknown type,
// changes no label. It is reported once with an error comment keyed by
// m.ErrorKey or m.Key's ID (SPEC §5.3), logged as an error when that
// comment is posted and at debug level after, and returned as an error
// StateError reports.
func (a *Applier) Transition(ctx context.Context, s *github.Snapshot, dirty func(int64) bool, m Move) error {
	wrap := func(err error) error {
		return fmt.Errorf("move %s/%s#%d to %s on %s: %w", a.Owner, a.Repo, s.Number, m.To, m.Trigger, err)
	}
	if err := m.check(); err != nil {
		return wrap(err)
	}
	is := state.Derive(s, a.AppSlug, dirty)
	if !is.Moved(m.Key) {
		if why, err := noRow(is, m); err != nil {
			if !StateError(err) {
				return wrap(err)
			}
			if is.Reported("transition", m.errorKey()) {
				logr.Debugf("state error, reported already: %v", wrap(err))
				return wrap(err)
			}
			if rerr := a.report(ctx, s, is, m, why); rerr != nil {
				return wrap(rerr)
			}
			logr.Errorf("state error: %v", wrap(err))
			return wrap(err)
		}
		fields := maps.Clone(m.Fields)
		if fields == nil {
			fields = map[string]string{}
		}
		fields["from"], fields["to"], fields[m.Key.Field] = string(is.Stage), string(m.To), m.Key.ID
		marker, err := github.FormatMarker(github.Marker{Kind: "transition", Fields: fields})
		if err != nil {
			return wrap(err)
		}
		for _, x := range m.Markers {
			more, err := github.FormatMarker(x)
			if err != nil {
				return wrap(err)
			}
			marker += "\n" + more
		}
		if err := a.comment(ctx, s, join(github.Escape(m.Text), marker)); err != nil {
			return wrap(err)
		}
		is = state.Derive(s, a.AppSlug, dirty)
	}
	if err := a.setStage(ctx, s, is.Stage); err != nil {
		return wrap(err)
	}
	if id, ok := m.Key.Comment(); ok && !m.NoAck {
		if err := a.Ack(ctx, id); err != nil {
			return wrap(err)
		}
	}
	return nil
}

// Ack adds the App's 👍 to a comment, unless it has one already.
func (a *Applier) Ack(ctx context.Context, commentID int64) error {
	mine, err := a.appReactions(ctx, commentID)
	if err != nil {
		return fmt.Errorf("ack comment %d: %w", commentID, err)
	}
	if _, ok := mine["+1"]; ok {
		return nil
	}
	return a.react(ctx, commentID, "+1")
}

// appReactions returns the App's reactions to a comment, by content. Only
// the App's Bot counts.
func (a *Applier) appReactions(ctx context.Context, commentID int64) (map[string]github.Reaction, error) {
	reactions, err := a.Client.Reactions(ctx, a.Owner, a.Repo, commentID)
	if err != nil {
		return nil, fmt.Errorf("the App's reactions: %w", err)
	}
	mine := map[string]github.Reaction{}
	for _, r := range reactions {
		if state.IsApp(r.User, a.AppSlug) {
			mine[r.Content] = r
		}
	}
	return mine, nil
}

// react adds the App's reaction, such as "+1", to a comment, with no check:
// see Ack.
func (a *Applier) react(ctx context.Context, commentID int64, content string) error {
	if _, _, err := a.Client.Client.Reactions.CreateIssueCommentReaction(ctx, a.Owner, a.Repo, commentID, content); err != nil {
		return fmt.Errorf("react %s to comment %d in %s/%s: %w", content, commentID, a.Owner, a.Repo, err)
	}
	return nil
}

// StateError reports whether err is a state error (SPEC §5.3) from
// Transition: a move the table lacks, or an issue of unknown type.
func StateError(err error) bool {
	return errors.Is(err, state.ErrNoTransition) || errors.Is(err, state.ErrUnknownType)
}

// noRow returns why the table has no row for m on an issue in state is,
// with a StateError, or "" and nil if it has one. Other errors come back
// with no reason.
func noRow(is state.Issue, m Move) (string, error) {
	class, err := state.ClassOf(is.Item)
	if errors.Is(err, state.ErrUnknownType) {
		return fmt.Sprintf("its type `%s` isn't one the pipeline knows (SPEC §4)", is.Item.Type), err
	}
	if err != nil {
		return "", err
	}
	if _, err := state.Lookup(is.Stage, m.To, class, m.Trigger); err != nil {
		if errors.Is(err, state.ErrNoTransition) {
			return fmt.Sprintf("the transition table (SPEC §7.2) has no such row for a %s issue", class), err
		}
		return "", err
	}
	return "", nil
}

// report posts the state error for a move the table lacks, saying why. The
// type in why isn't the App's text, so it's escaped (SPEC §5.1).
func (a *Applier) report(ctx context.Context, s *github.Snapshot, is state.Issue, m Move, why string) error {
	marker, err := github.FormatMarker(github.Marker{Kind: "error", Fields: map[string]string{"kind": "transition", "key": m.errorKey()}})
	if err != nil {
		return fmt.Errorf("report: %w", err)
	}
	text := fmt.Sprintf("I can't move this issue from `%s` to `%s` on %s: %s. It stays at `%s`.", is.Stage, m.To, m.Trigger, why, is.Stage)
	if err := a.comment(ctx, s, join(github.Escape(text), marker)); err != nil {
		return fmt.Errorf("report: %w", err)
	}
	return nil
}

// comment posts body on s as the App and adds it to s.
func (a *Applier) comment(ctx context.Context, s *github.Snapshot, body string) error {
	c, _, err := a.Client.Issues.CreateComment(ctx, a.Owner, a.Repo, s.Number, gogithub.IssueCommentRequest{Body: body})
	if err != nil {
		return fmt.Errorf("comment on %s/%s#%d: %w", a.Owner, a.Repo, s.Number, err)
	}
	s.Comments = append(s.Comments, github.Comment{ID: c.GetID(), Body: c.GetBody(),
		Author:    github.Author{Login: c.GetUser().GetLogin(), Type: c.GetUser().GetType()},
		CreatedAt: c.GetCreatedAt().Time, UpdatedAt: c.GetUpdatedAt().Time})
	return nil
}

// setStage makes stage's label the only stage:* label on s: it adds it, then
// removes the others, even one that names no stage. A stage with no label
// changes nothing.
func (a *Applier) setStage(ctx context.Context, s *github.Snapshot, stage state.Stage) error {
	want, ok := stage.Label()
	if !ok {
		return nil
	}
	if err := a.addLabel(ctx, s, want); err != nil {
		return fmt.Errorf("set stage %s: %w", stage, err)
	}
	for _, l := range slices.Clone(s.Labels) {
		if !isStageLabel(l) || strings.EqualFold(l, want) {
			continue
		}
		if err := a.removeLabel(ctx, s, l); err != nil {
			return fmt.Errorf("set stage %s: %w", stage, err)
		}
	}
	return nil
}

// addLabel adds l to s, unless s has it. Like GitHub, it ignores case. The
// labels GitHub answers with replace s's, but for those left out
// (Snapshot.LeftOut). A label that's on the issue but left out, unexplained,
// is first removed, so the add makes the App's own event, which explains it:
// otherwise GitHub would make none, and the label would be removed later as
// unexplained. A crash between the two leaves the label off, which a replay
// adds, as for any label not yet added.
func (a *Applier) addLabel(ctx context.Context, s *github.Snapshot, l string) error {
	if slices.ContainsFunc(s.Labels, func(x string) bool { return strings.EqualFold(x, l) }) {
		return nil
	}
	if s.LeftOut[strings.ToLower(l)] {
		resp, err := a.Client.Issues.RemoveLabelForIssue(ctx, a.Owner, a.Repo, s.Number, l)
		if err != nil && (resp == nil || resp.StatusCode != http.StatusNotFound) {
			return fmt.Errorf("remove %s, unexplained, from %s/%s#%d to add it: %w", l, a.Owner, a.Repo, s.Number, err)
		}
		delete(s.LeftOut, strings.ToLower(l))
		a.seen().ForgetLabel(a.Owner, a.Repo, s.Number, l)
	}
	labels, _, err := a.Client.Issues.AddLabelsToIssue(ctx, a.Owner, a.Repo, s.Number, []string{l})
	if err != nil {
		return fmt.Errorf("add %s to %s/%s#%d: %w", l, a.Owner, a.Repo, s.Number, err)
	}
	var names []string
	for _, x := range labels {
		names = append(names, x.GetName())
	}
	s.SetLabels(names)
	return nil
}

// removeLabel removes l from s, if s has it. Like GitHub, it ignores case.
// A label gone meanwhile is no error.
func (a *Applier) removeLabel(ctx context.Context, s *github.Snapshot, l string) error {
	is := func(x string) bool { return strings.EqualFold(x, l) }
	if !slices.ContainsFunc(s.Labels, is) {
		return nil
	}
	resp, err := a.Client.Issues.RemoveLabelForIssue(ctx, a.Owner, a.Repo, s.Number, l)
	if err != nil && (resp == nil || resp.StatusCode != http.StatusNotFound) {
		return fmt.Errorf("remove %s from %s/%s#%d: %w", l, a.Owner, a.Repo, s.Number, err)
	}
	s.SetLabels(slices.DeleteFunc(slices.Clone(s.Labels), is))
	return nil
}

// join puts text, if any, before a marker.
func join(text, marker string) string {
	if text == "" {
		return marker
	}
	return text + "\n\n" + marker
}
