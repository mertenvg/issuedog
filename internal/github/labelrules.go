package github

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/logr/v2"
)

// Trust answers whether a login is trusted (SPEC §6.1). *trust.Set satisfies it.
type Trust interface {
	Trusted(ctx context.Context, login string) (bool, error)
}

// LabelEventKind is what CheckLabels found that S4 has to act on.
type LabelEventKind string

const (
	// LabelOverride is a trusted user's manual stage change, made after the
	// latest transition marker. S4 records it with by=<login> (SPEC §5.1).
	LabelOverride LabelEventKind = "override"
	// LabelDisagreement is a stage label that disagrees with the latest
	// transition marker. S4 puts the label back and, unless it's Unfinished,
	// reports it (SPEC §5.3).
	LabelDisagreement LabelEventKind = "disagreement"
)

// LabelEvent is one finding of CheckLabels.
type LabelEvent struct {
	Kind LabelEventKind
	// Stages are the stage labels now on the issue, after any reverts.
	Stages []string
	// Marker is the to= stage of the latest transition marker, if any.
	Marker string
	// By is the trusted login behind an override.
	By string
	// Label is the stage label an override added.
	Label string
	// Key is the ID of the timeline event behind it: the state-error key.
	Key int64
	// Unfinished marks a disagreement that is the App's own write cut short
	// (plan #5, assumption 4): no stage label has changed since the latest
	// marker, except by the App bringing the labels toward it. It is to be
	// finished, not reported.
	Unfinished bool
}

// labelState is the history of one pipeline label on the timeline.
type labelState struct {
	name   string
	events []*gogithub.Timeline // labeled and unlabeled, oldest first
	ats    []int                // each event's timeline position
}

// latest returns the newest change to l, or nil if l is nil.
func (l *labelState) latest() *gogithub.Timeline {
	if l == nil {
		return nil
	}
	return l.events[len(l.events)-1]
}

// pipelineLabel reports whether name is a label SPEC §5.1 guards.
func pipelineLabel(name string) bool {
	n := strings.ToLower(name)
	for _, p := range []string{"stage:", "priority:", "scope:", "type:"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return n == "approved"
}

// CheckLabels applies the label rules of SPEC §5.1 to one issue. An untrusted
// change to a pipeline label is reverted, with a comment; changes by the App
// (appSlug) and trusted users stand. It returns an override for a trusted
// stage change newer than the latest transition marker, or else a
// disagreement when the stage labels don't match that marker. Issue types on
// org repos aren't checked. A replay is safe: the comment carries a revert
// marker keyed by the event, is posted once, and goes out before the revert.
//
// Markers in a comment that dirty reports, by its ID, are skipped, as
// state.Derive skips them. The body isn't on the timeline, so dirty is never
// asked about it.
func (c *Client) CheckLabels(ctx context.Context, owner, repo string, number int, appSlug string, trust Trust, dirty func(id int64) bool) ([]LabelEvent, error) {
	timeline, err := c.TimelineSince(ctx, owner, repo, number, time.Time{})
	if err != nil {
		return nil, fmt.Errorf("check labels of %s/%s#%d: %w", owner, repo, number, err)
	}
	labels := map[string]*labelState{}
	var order []*labelState
	marker, markerAt := "", -1
	reverted := map[string]bool{}
	for i, e := range timeline {
		switch e.GetEvent() {
		case "labeled", "unlabeled":
			name := e.GetLabel().GetName()
			if !pipelineLabel(name) {
				continue
			}
			key := strings.ToLower(name)
			if labels[key] == nil {
				labels[key] = &labelState{name: name}
				order = append(order, labels[key])
			}
			labels[key].events = append(labels[key].events, e)
			labels[key].ats = append(labels[key].ats, i)
		case "commented":
			if dirty(e.GetID()) {
				continue
			}
			author := Author{Login: e.GetUser().GetLogin(), Type: e.GetUser().GetType()}
			for _, m := range TrustedMarkers(author, e.GetBody(), appSlug) {
				switch {
				case m.Kind == "transition" && m.Fields["to"] != "":
					marker, markerAt = m.Fields["to"], i
				case m.Kind == "revert":
					reverted[m.Fields["key"]] = true
				}
			}
		}
	}

	r := labelRules{c: c, owner: owner, repo: repo, number: number, appSlug: appSlug, trust: trust, verdicts: map[string]bool{}}
	var stages []string
	var override, last *gogithub.Timeline
	overrideAt, lastAt := -1, -1
	for _, l := range order {
		// acc is the last change that stands: the latest if accepted, or
		// else the one a revert goes back to. An untrusted change after a
		// trusted override must not hide that override.
		on, acc, accAt, err := r.settle(ctx, l, reverted)
		if err != nil {
			return nil, fmt.Errorf("check labels of %s/%s#%d: %w", owner, repo, number, err)
		}
		if !strings.HasPrefix(strings.ToLower(l.name), "stage:") {
			continue
		}
		if on {
			stages = append(stages, l.name)
		}
		if acc != nil && acc.GetEvent() == "labeled" && accAt > markerAt && !r.isApp(acc) && accAt > overrideAt {
			override, overrideAt = acc, accAt
		}
		e, at, err := r.cause(ctx, l, marker, markerAt)
		if err != nil {
			return nil, fmt.Errorf("check labels of %s/%s#%d: %w", owner, repo, number, err)
		}
		if at > lastAt {
			last, lastAt = e, at
		}
	}
	if override != nil {
		return []LabelEvent{{Kind: LabelOverride, Stages: stages, Marker: marker, By: override.GetActor().GetLogin(),
			Label: override.GetLabel().GetName(), Key: override.GetID()}}, nil
	}
	if marker != "" && (len(stages) != 1 || !strings.EqualFold(stages[0], "stage:"+marker)) {
		return []LabelEvent{{Kind: LabelDisagreement, Stages: stages, Marker: marker, Key: last.GetID(), Unfinished: lastAt < markerAt}}, nil
	}
	return nil, nil
}

// labelRules carries what CheckLabels needs to judge and revert one change.
type labelRules struct {
	c           *Client
	owner, repo string
	number      int
	appSlug     string
	trust       Trust
	verdicts    map[string]bool // trust by lowercase login, asked once per call
}

func (r *labelRules) isApp(e *gogithub.Timeline) bool {
	a := e.GetActor()
	return r.appSlug != "" && a.GetType() == "Bot" && a.GetLogin() == r.appSlug+"[bot]"
}

// restores reports whether event i, by the App, leaves l as the accepted change
// before it left it (off if there was none), as a revert does.
func (r *labelRules) restores(ctx context.Context, l *labelState, i int) (bool, error) {
	before := false
	for j := i - 1; j >= 0; j-- {
		ok, err := r.accepted(ctx, l.events[j])
		if err != nil {
			return false, fmt.Errorf("restores %s at %d: %w", l.name, i, err)
		}
		if ok {
			before = l.events[j].GetEvent() == "labeled"
			break
		}
	}
	return before == (l.events[i].GetEvent() == "labeled"), nil
}

// cause returns the latest change to the stage label l that a disagreement
// can be keyed by, and its timeline position (nil and -1 if none). It leaves
// out untrusted changes and the App's reverts of them, and the App's changes
// toward the marker made after it, which finish its own write or repair a
// disagreement. So a disagreement keeps its key while it is being repaired.
func (r *labelRules) cause(ctx context.Context, l *labelState, marker string, markerAt int) (*gogithub.Timeline, int, error) {
	for i := len(l.events) - 1; i >= 0; i-- {
		e := l.events[i]
		if l.ats[i] > markerAt && r.isApp(e) && strings.EqualFold(l.name, "stage:"+marker) == (e.GetEvent() == "labeled") {
			continue
		}
		ok, err := r.accepted(ctx, e)
		if err == nil && ok && r.isApp(e) {
			var revert bool
			revert, err = r.restores(ctx, l, i)
			ok = !revert
		}
		if err != nil {
			return nil, -1, fmt.Errorf("cause of %s: %w", l.name, err)
		}
		if ok {
			return e, l.ats[i], nil
		}
	}
	return nil, -1, nil
}

// accepted reports whether e came from the App or a trusted user.
func (r *labelRules) accepted(ctx context.Context, e *gogithub.Timeline) (bool, error) {
	if r.isApp(e) {
		return true, nil
	}
	login := strings.ToLower(e.GetActor().GetLogin())
	if v, ok := r.verdicts[login]; ok {
		return v, nil
	}
	v, err := r.trust.Trusted(ctx, login)
	if err != nil {
		return false, fmt.Errorf("trust of %s on %s/%s#%d: %w", login, r.owner, r.repo, r.number, err)
	}
	r.verdicts[login] = v
	return v, nil
}

// settle finds the last accepted change to l, from the App or a trusted user,
// and its timeline position (nil and -1 if none), and reverts l to how that
// change left it, or off. It reports whether l is on. The comment, keyed by
// the latest event, goes first and only once, so a crash between the two
// writes replays cleanly.
func (r *labelRules) settle(ctx context.Context, l *labelState, reverted map[string]bool) (bool, *gogithub.Timeline, int, error) {
	latest := l.latest()
	var acc *gogithub.Timeline
	accAt := -1
	for i := len(l.events) - 1; i >= 0; i-- {
		ok, err := r.accepted(ctx, l.events[i])
		if err == nil && ok && r.isApp(l.events[i]) {
			// The App's own revert just restores the change before it, so
			// look past it: a trusted override it restored stays an override.
			var revert bool
			revert, err = r.restores(ctx, l, i)
			ok = !revert
		}
		if err != nil {
			return false, nil, -1, fmt.Errorf("settle %s: %w", l.name, err)
		}
		if ok {
			acc, accAt = l.events[i], l.ats[i]
			break
		}
	}
	if acc == nil {
		// Only reverts and untrusted changes: the App's last word stands.
		for i := len(l.events) - 1; i >= 0; i-- {
			if r.isApp(l.events[i]) {
				acc, accAt = l.events[i], l.ats[i]
				break
			}
		}
	}
	want := acc != nil && acc.GetEvent() == "labeled"
	if acc == latest || want == (latest.GetEvent() == "labeled") {
		return want, acc, accAt, nil
	}
	key := strconv.FormatInt(latest.GetID(), 10)
	who := latest.GetActor().GetLogin()
	if !reverted[key] {
		verb := map[bool]string{true: "restored", false: "removed"}[want]
		// The label name is someone else's text, so a marker in it is escaped (§5.1).
		body := fmt.Sprintf("I %s `%s`: only trusted users can change pipeline labels, and %s isn't one.\n\n<!-- issuedog:revert key=%s -->", verb, Escape(l.name), who, key)
		if _, _, err := r.c.Issues.CreateComment(ctx, r.owner, r.repo, r.number, gogithub.IssueCommentRequest{Body: body}); err != nil {
			return false, nil, -1, fmt.Errorf("comment on revert of %s on %s/%s#%d: %w", l.name, r.owner, r.repo, r.number, err)
		}
	}
	var err error
	if want {
		_, _, err = r.c.Issues.AddLabelsToIssue(ctx, r.owner, r.repo, r.number, []string{l.name})
	} else {
		var resp *gogithub.Response
		resp, err = r.c.Issues.RemoveLabelForIssue(ctx, r.owner, r.repo, r.number, l.name)
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			err = nil // already gone
		}
	}
	if err != nil {
		return false, nil, -1, fmt.Errorf("revert %s on %s/%s#%d: %w", l.name, r.owner, r.repo, r.number, err)
	}
	logr.Infof("reverted %s's change to %s on %s/%s#%d", who, l.name, r.owner, r.repo, r.number)
	return want, acc, accAt, nil
}
