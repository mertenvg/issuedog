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
	// transition marker. S4 puts the label back and reports it (SPEC §5.3).
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
	// Key is the ID of the timeline event behind it: the state-error key.
	Key int64
}

// labelState is the history of one pipeline label on the timeline.
type labelState struct {
	name   string
	events []*gogithub.Timeline // labeled and unlabeled, oldest first
	at     int                  // timeline position of the latest event
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
func (c *Client) CheckLabels(ctx context.Context, owner, repo string, number int, appSlug string, trust Trust) ([]LabelEvent, error) {
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
			labels[key].at = i
		case "commented":
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
	var override, lastStage *labelState
	for _, l := range order {
		latest := l.latest()
		ok, err := r.accepted(ctx, latest)
		if err != nil {
			return nil, err
		}
		on := latest.GetEvent() == "labeled"
		if !ok {
			if on, err = r.revert(ctx, l, reverted); err != nil {
				return nil, err
			}
		}
		if !strings.HasPrefix(strings.ToLower(l.name), "stage:") {
			continue
		}
		if on {
			stages = append(stages, l.name)
		}
		if ok && latest.GetEvent() == "labeled" && l.at > markerAt && !r.isApp(latest) && (override == nil || l.at > override.at) {
			override = l
		}
		if lastStage == nil || l.at > lastStage.at {
			lastStage = l
		}
	}
	if override != nil {
		e := override.latest()
		return []LabelEvent{{Kind: LabelOverride, Stages: stages, Marker: marker, By: e.GetActor().GetLogin(), Key: e.GetID()}}, nil
	}
	if marker != "" && (len(stages) != 1 || !strings.EqualFold(stages[0], "stage:"+marker)) {
		return []LabelEvent{{Kind: LabelDisagreement, Stages: stages, Marker: marker, Key: lastStage.latest().GetID()}}, nil
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

// revert puts l back as the last accepted change left it, or off if none did,
// and reports whether it is on. The comment, keyed by the latest event, goes
// first and only once, so a crash between the two writes replays cleanly.
func (r *labelRules) revert(ctx context.Context, l *labelState, reverted map[string]bool) (bool, error) {
	latest := l.latest()
	want := false
	for i := len(l.events) - 2; i >= 0; i-- {
		ok, err := r.accepted(ctx, l.events[i])
		if err != nil {
			return false, err
		}
		if ok {
			want = l.events[i].GetEvent() == "labeled"
			break
		}
	}
	if want == (latest.GetEvent() == "labeled") {
		return want, nil
	}
	key := strconv.FormatInt(latest.GetID(), 10)
	who := latest.GetActor().GetLogin()
	if !reverted[key] {
		verb := map[bool]string{true: "restored", false: "removed"}[want]
		body := fmt.Sprintf("I %s `%s`: only trusted users can change pipeline labels, and %s isn't one.\n\n<!-- issuedog:revert key=%s -->", verb, l.name, who, key)
		if _, _, err := r.c.Issues.CreateComment(ctx, r.owner, r.repo, r.number, gogithub.IssueCommentRequest{Body: body}); err != nil {
			return false, fmt.Errorf("comment on revert of %s on %s/%s#%d: %w", l.name, r.owner, r.repo, r.number, err)
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
		return false, fmt.Errorf("revert %s on %s/%s#%d: %w", l.name, r.owner, r.repo, r.number, err)
	}
	logr.Infof("reverted %s's change to %s on %s/%s#%d", who, l.name, r.owner, r.repo, r.number)
	return want, nil
}
