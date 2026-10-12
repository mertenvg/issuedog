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
// comment, the new stage label, removing the old ones, then the 👍. Its
// writes are added to s. dirty is as for state.Derive.
//
// The stage labels are set to match the latest marker, so a stage label a
// trusted user set by hand would be overwritten. That's safe only because
// Applier.CheckLabels runs first and records such a change as an override
// (SPEC §5.1): callers run it on a fresh snapshot just before Transition (T11
// will). A change made in between is still caught: the next CheckLabels
// records it as an override, as it isn't in the marker this writes.
//
// A move the table lacks changes no label. It is reported once with an
// error comment keyed by m.Key's ID (SPEC §5.3), logged as an error when
// that comment is posted and at debug level after, and returned as an error
// wrapping state.ErrNoTransition.
func (a *Applier) Transition(ctx context.Context, s *github.Snapshot, dirty func(int64) bool, m Move) error {
	wrap := func(err error) error {
		return fmt.Errorf("move %s/%s#%d to %s on %s: %w", a.Owner, a.Repo, s.Number, m.To, m.Trigger, err)
	}
	if err := m.check(); err != nil {
		return wrap(err)
	}
	is := state.Derive(s, a.AppSlug, dirty)
	if !is.Moved(m.Key) {
		class, err := state.ClassOf(is.Item)
		if err != nil {
			return wrap(err)
		}
		if _, err := state.Lookup(is.Stage, m.To, class, m.Trigger); err != nil {
			if !errors.Is(err, state.ErrNoTransition) {
				return wrap(err)
			}
			if is.Reported("transition", m.Key.ID) {
				logr.Debugf("state error, reported already: %v", wrap(err))
				return wrap(err)
			}
			if rerr := a.report(ctx, s, is, m, class); rerr != nil {
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
		if err := a.comment(ctx, s, join(github.Escape(m.Text), marker)); err != nil {
			return wrap(err)
		}
		is = state.Derive(s, a.AppSlug, dirty)
	}
	if err := a.setStage(ctx, s, is.Stage); err != nil {
		return wrap(err)
	}
	if id, ok := m.Key.Comment(); ok {
		if err := a.Ack(ctx, id); err != nil {
			return wrap(err)
		}
	}
	return nil
}

// Ack adds the App's 👍 to a comment, unless it has one already.
func (a *Applier) Ack(ctx context.Context, commentID int64) error {
	reactions, err := a.Client.Reactions(ctx, a.Owner, a.Repo, commentID)
	if err != nil {
		return fmt.Errorf("ack comment %d: %w", commentID, err)
	}
	app := github.Author{Login: a.AppSlug + "[bot]", Type: "Bot"}
	for _, r := range reactions {
		if r.Content == "+1" && r.User == app {
			return nil
		}
	}
	if _, _, err := a.Client.Client.Reactions.CreateIssueCommentReaction(ctx, a.Owner, a.Repo, commentID, "+1"); err != nil {
		return fmt.Errorf("ack comment %d in %s/%s: %w", commentID, a.Owner, a.Repo, err)
	}
	return nil
}

// report posts the state error for a move the table lacks.
func (a *Applier) report(ctx context.Context, s *github.Snapshot, is state.Issue, m Move, class state.Class) error {
	marker, err := github.FormatMarker(github.Marker{Kind: "error", Fields: map[string]string{"kind": "transition", "key": m.Key.ID}})
	if err != nil {
		return fmt.Errorf("report: %w", err)
	}
	text := fmt.Sprintf("I can't move this issue from `%s` to `%s` on %s: the transition table (SPEC §7.2) has no such row for a %s issue. It stays at `%s`.",
		is.Stage, m.To, m.Trigger, class, is.Stage)
	if err := a.comment(ctx, s, join(text, marker)); err != nil {
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
	if !slices.ContainsFunc(s.Labels, func(l string) bool { return strings.EqualFold(l, want) }) {
		labels, _, err := a.Client.Issues.AddLabelsToIssue(ctx, a.Owner, a.Repo, s.Number, []string{want})
		if err != nil {
			return fmt.Errorf("add %s to %s/%s#%d: %w", want, a.Owner, a.Repo, s.Number, err)
		}
		s.Labels = nil
		for _, l := range labels {
			s.Labels = append(s.Labels, l.GetName())
		}
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
	s.Labels = slices.DeleteFunc(s.Labels, is)
	return nil
}

// join puts text, if any, before a marker.
func join(text, marker string) string {
	if text == "" {
		return marker
	}
	return text + "\n\n" + marker
}
