package github

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// pipelineLabel reports whether name is a label SPEC §5.1 guards, or a model
// or effort override label (#93's plan, T4).
func pipelineLabel(name string) bool {
	n := strings.ToLower(name)
	for _, p := range []string{"stage:", "priority:", "scope:", "type:"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return n == "approved" || OverrideLabel(name)
}

// OverrideLabel reports whether name, ignoring case, is a model or effort
// override label of one of the four kinds.
func OverrideLabel(name string) bool {
	n := strings.ToLower(name)
	for _, p := range []string{ModelPrefix, VerifierModelPrefix, EffortPrefix, VerifierEffortPrefix} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// CheckLabels applies the label rules of SPEC §5.1 to one issue. An untrusted
// change to a pipeline label is reverted, with a comment; changes by the App
// (appSlug) and trusted users stand. It returns labels, the issue's labels
// as the caller last read them, with the pipeline labels brought up to date
// with the timeline and its reverts. It reports an override for a trusted stage change that the latest
// transition marker doesn't record (see overrides), or else a disagreement
// when the stage labels don't match that marker. Issue types on org repos
// aren't checked. A replay is safe: the comment carries a revert marker keyed
// by the event, is posted once, and goes out before the revert.
//
// A pipeline label on the issue that no standing change explains, as from a
// repo label rename, is removed the same way once it has stayed unexplained
// for UnexplainedWait, by seen's memory (see unexplained). Until HoldBound
// after the issue was first found with one, such labels are returned in
// held: the caller must not act on the issue until a later call holds
// nothing back. After that, they're left out of labels, so the caller can
// go ahead without acting on them. A nil seen remembers nothing, so the
// issue is held back for good.
//
// It also returns settings, the ID of the newest change to a model or
// effort label (OverrideLabel) that stands, as standing judges it, or 0 if
// there is none: a trusted user's or the App's, but no untrusted change and
// no App revert of one.
//
// Markers in a comment that dirty reports, by its ID, are skipped, as
// state.Derive skips them. The body isn't on the timeline, so dirty is never
// asked about it.
func (c *Client) CheckLabels(ctx context.Context, owner, repo string, number int, labels []string, appSlug string, trust Trust, dirty func(id int64) bool,
	seen *Unexplained) (_ []string, _ []LabelEvent, settings int64, held []string, _ error) {
	wrap := func(err error) error { return fmt.Errorf("check labels of %s/%s#%d: %w", owner, repo, number, err) }
	timeline, err := c.TimelineSince(ctx, owner, repo, number, time.Time{})
	if err != nil {
		return nil, nil, 0, nil, wrap(err)
	}
	states := map[string]*labelState{}
	var order []*labelState
	eventAt := map[string]int{}                // label events' timeline positions, by ID
	latest := transition{at: -1, recorded: -1} // none yet
	prev := latest
	reverted := map[string]bool{}
	for i, e := range timeline {
		switch e.GetEvent() {
		case "labeled", "unlabeled":
			name := e.GetLabel().GetName()
			if !pipelineLabel(name) {
				continue
			}
			key := strings.ToLower(name)
			if states[key] == nil {
				states[key] = &labelState{name: name}
				order = append(order, states[key])
			}
			states[key].events = append(states[key].events, e)
			states[key].ats = append(states[key].ats, i)
			eventAt[strconv.FormatInt(e.GetID(), 10)] = i
		case "commented":
			if dirty(e.GetID()) {
				continue
			}
			author := Author{Login: e.GetUser().GetLogin(), Type: e.GetUser().GetType()}
			for _, m := range TrustedMarkers(author, e.GetBody(), appSlug) {
				switch {
				case m.Kind == "transition" && m.Fields["to"] != "":
					prev, latest = latest, transition{to: m.Fields["to"], from: m.Fields["from"], at: i, recorded: -1}
					if at, ok := eventAt[m.Fields["event"]]; ok {
						latest.recorded = at
					}
				case m.Kind == "revert":
					reverted[m.Fields["key"]] = true
				}
			}
		}
	}

	r := labelRules{c: c, owner: owner, repo: repo, number: number, appSlug: appSlug, trust: trust, verdicts: map[string]bool{}}
	marker, markerAt := latest.to, latest.at
	given := labels
	labels = slices.Clone(labels)
	// Whether each label, by lowercase name, is on after settle, or was just
	// reverted by it: either way, unexplained leaves it alone.
	settled := map[string]bool{}
	var stages []string
	var override, last *gogithub.Timeline
	overrideAt, lastAt, settingsAt := -1, -1, -1
	for _, l := range order {
		// acc is the last change that stands: the latest if accepted, or
		// else the one a revert goes back to. An untrusted change after a
		// trusted override must not hide that override.
		on, acc, accAt, wrote, err := r.settle(ctx, l, reverted)
		if err != nil {
			return nil, nil, 0, nil, wrap(err)
		}
		settled[strings.ToLower(l.name)] = on || wrote
		if has := slices.ContainsFunc(labels, func(x string) bool { return strings.EqualFold(x, l.name) }); on && !has {
			labels = append(labels, l.name)
		} else if !on && has {
			labels = slices.DeleteFunc(labels, func(x string) bool { return strings.EqualFold(x, l.name) })
		}
		if OverrideLabel(l.name) {
			// Not settle's acc, which falls back to the App's revert.
			e, at, err := r.standing(ctx, l, nil)
			if err != nil {
				return nil, nil, 0, nil, wrap(err)
			}
			if at > settingsAt {
				settings, settingsAt = e.GetID(), at
			}
			continue
		}
		if !strings.HasPrefix(strings.ToLower(l.name), "stage:") {
			continue
		}
		if on {
			stages = append(stages, l.name)
		}
		if acc != nil && acc.GetEvent() == "labeled" && !r.isApp(acc) && accAt > overrideAt && latest.overrides(l.name, accAt, prev.at) {
			override, overrideAt = acc, accAt
		}
		// last is the latest change that stands, but for changes toward the
		// marker made after it, which finish the App's own write, repair a
		// disagreement or agree with it. So a disagreement keeps its key
		// while it is being repaired.
		e, at, err := r.standing(ctx, l, func(i int) bool {
			return l.ats[i] > markerAt && strings.EqualFold(l.name, "stage:"+marker) == (l.events[i].GetEvent() == "labeled")
		})
		if err != nil {
			return nil, nil, 0, nil, wrap(err)
		}
		if at > lastAt {
			last, lastAt = e, at
		}
	}
	if labels, held, err = r.unexplained(ctx, labels, given, states, settled, reverted, marker, seen); err != nil {
		return nil, nil, 0, nil, wrap(err)
	}
	if override != nil {
		return labels, []LabelEvent{{Kind: LabelOverride, Stages: stages, Marker: marker, By: override.GetActor().GetLogin(),
			Label: override.GetLabel().GetName(), Key: override.GetID()}}, settings, held, nil
	}
	if marker != "" && (len(stages) != 1 || !strings.EqualFold(stages[0], "stage:"+marker)) {
		return labels, []LabelEvent{{Kind: LabelDisagreement, Stages: stages, Marker: marker, Key: last.GetID(), Unfinished: lastAt < markerAt}}, settings, held, nil
	}
	return labels, nil, settings, held, nil
}

// transition is a transition marker on the timeline.
type transition struct {
	to, from string
	// at is its timeline position, and recorded that of the label event it
	// names in event=, or -1.
	at, recorded int
}

// overrides reports whether a trusted add of the stage label name, at
// timeline position at, is an override: a stage change that t, the latest
// marker, doesn't record. Made after t, it is one unless it agrees with t.
// Made while t's transition was in flight, after the marker before it (at
// prevAt) and after the change t records, it is one unless it names t's from
// or to stage: then the transition saw it, or agrees with it.
func (t transition) overrides(name string, at, prevAt int) bool {
	is := func(stage string) bool { return stage != "" && strings.EqualFold(name, "stage:"+stage) }
	switch {
	case is(t.to):
		return false
	case at > t.at:
		return true
	}
	return at > max(prevAt, t.recorded) && !is(t.from)
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

// standing returns the latest change to l that stands, and its timeline
// position (nil and -1 if none), passing over the events that skip reports
// by index (skip may be nil). A change stands if the App or a trusted user
// made it, and isn't the App's revert, which just restores the change before
// it: so a trusted override it restored stays an override.
func (r *labelRules) standing(ctx context.Context, l *labelState, skip func(i int) bool) (*gogithub.Timeline, int, error) {
	for i := len(l.events) - 1; i >= 0; i-- {
		if skip != nil && skip(i) {
			continue
		}
		ok, err := r.accepted(ctx, l.events[i])
		if err == nil && ok && r.isApp(l.events[i]) {
			var revert bool
			revert, err = r.restores(ctx, l, i)
			ok = !revert
		}
		if err != nil {
			return nil, -1, fmt.Errorf("change to %s that stands: %w", l.name, err)
		}
		if ok {
			return l.events[i], l.ats[i], nil
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
// change left it, or off. It reports whether l is on, and whether it reverted
// l. The comment, keyed by the latest event, goes first and only once, so a
// crash between the two writes replays cleanly.
func (r *labelRules) settle(ctx context.Context, l *labelState, reverted map[string]bool) (bool, *gogithub.Timeline, int, bool, error) {
	latest := l.latest()
	acc, accAt, err := r.standing(ctx, l, nil)
	if err != nil {
		return false, nil, -1, false, fmt.Errorf("settle %s: %w", l.name, err)
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
		return want, acc, accAt, false, nil
	}
	key := strconv.FormatInt(latest.GetID(), 10)
	who := latest.GetActor().GetLogin()
	if !reverted[key] {
		verb := map[bool]string{true: "restored", false: "removed"}[want]
		// The label name is someone else's text, so a marker in it is escaped (§5.1).
		body := fmt.Sprintf("I %s `%s`: only trusted users can change pipeline labels, and %s isn't one.\n\n<!-- issuedog:revert key=%s -->", verb, Escape(l.name), who, key)
		if _, _, err := r.c.Issues.CreateComment(ctx, r.owner, r.repo, r.number, gogithub.IssueCommentRequest{Body: body}); err != nil {
			return false, nil, -1, false, fmt.Errorf("comment on revert of %s on %s/%s#%d: %w", l.name, r.owner, r.repo, r.number, err)
		}
	}
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
		return false, nil, -1, false, fmt.Errorf("revert %s on %s/%s#%d: %w", l.name, r.owner, r.repo, r.number, err)
	}
	logr.Infof("reverted %s's change to %s on %s/%s#%d", who, l.name, r.owner, r.repo, r.number)
	return want, acc, accAt, true, nil
}

// UnexplainedWait is how long a pipeline label must stay unexplained, with
// no change to its events, before it is removed.
const UnexplainedWait = 5 * time.Minute

// HoldBound is how long an issue with unexplained labels is held back, from
// the first call that found it so, continuously. Its only purpose is to wait
// out GitHub's late label events, seen to take 1 to 2 seconds: 30 seconds is
// 15 times that, and at the default loop.idle_sleep of a minute, it holds an
// issue back for one pass. As a time, not a count of passes, it doesn't
// depend on how often the loop runs. After it, the issue goes ahead with
// the unexplained labels left out, so a label renamed again and again holds
// nothing up for longer.
const HoldBound = 30 * time.Second

// Unexplained remembers, in memory only, when CheckLabels found each
// pipeline label on an issue unexplained, with how many events the label
// had then, and when the issue was first held back for it, all by GitHub's
// clock. A daemon keeps one for as long as it runs. Losing it, as on a
// restart, only delays a removal. The zero value is ready to use, and it is
// safe for concurrent use.
type Unexplained struct {
	mu     sync.Mutex
	issues map[string]heldIssue // by unexplainedKey
}

// heldIssue is what Unexplained keeps of one issue.
type heldIssue struct {
	since  time.Time            // since when it has had unexplained labels, continuously
	labels map[string]firstSeen // by lowercase label
}

// firstSeen is when a label was found unexplained, and its events then.
type firstSeen struct {
	at     time.Time
	events int
}

// get returns a copy of what u keeps of issue.
func (u *Unexplained) get(issue string) heldIssue {
	u.mu.Lock()
	defer u.mu.Unlock()
	h := u.issues[issue]
	h.labels = maps.Clone(h.labels)
	return h
}

// set replaces what u keeps of issue. With no labels, issue is forgotten.
func (u *Unexplained) set(issue string, h heldIssue) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(h.labels) == 0 {
		delete(u.issues, issue)
		return
	}
	if u.issues == nil {
		u.issues = map[string]heldIssue{}
	}
	u.issues[issue] = h
}

// unexplainedKey is how Unexplained names an issue.
func unexplainedKey(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s#%d", owner, repo, number)
}

// ForgetIssue forgets the labels of owner/repo#number, as for an issue that
// is closed or gone, so its times don't outlive it.
func (u *Unexplained) ForgetIssue(owner, repo string, number int) {
	u.set(unexplainedKey(owner, repo, number), heldIssue{})
}

// Has reports whether u keeps unexplained labels of owner/repo#number: ones
// held back or left out, for a later call to check again.
func (u *Unexplained) Has(owner, repo string, number int) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.issues[unexplainedKey(owner, repo, number)].labels) > 0
}

// forget drops label, lowercase, of issue, and the issue once it has none.
func (u *Unexplained) forget(issue, label string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.issues[issue].labels, label)
	if len(u.issues[issue].labels) == 0 {
		delete(u.issues, issue)
	}
}

// unexplained deals with each pipeline label in given that the timeline
// says is off once settle is done (settled), whether no event names it or
// the last change that stands removed it. Such a label is removed, with a
// revert comment, once it has stayed unexplained for UnexplainedWait with
// no change to its events. Until then, for HoldBound from when the issue
// was first found with one, it's returned in held, and the caller holds the
// issue back; after that, it's left out of labels, so the caller goes ahead
// and acts on nothing of it. It returns labels without the labels it
// removed, found gone or left out.
//
// Such a label comes from a repo label rename (#118). A rename puts the new
// name on every issue that has the label and adds nothing to their
// timelines, so a write collaborator who isn't trusted could rename an
// unguarded label to a pipeline one, or one pipeline label to another. What
// GitHub reports, as far as is known:
//
//   - No issue event records a label rename. The timeline's renamed event is
//     a change of the issue's title (GitHub Docs, "Issue event types").
//   - A labeled or unlabeled event's label has only a name and a color, with
//     no ID or URL as a live label has (GitHub Docs, "Issue event types";
//     go-github's Timeline.Label). That suggests the event keeps the name the
//     label had then, and a third-party report says refetched events keep the
//     old name (iFixit/pulldasher#522). GitHub doesn't document it, and it
//     hasn't been checked by hand on a scratch repo.
//
// So both ways are handled, failing closed wherever the timeline can tell:
//
//   - Old events keep the old name (githubtest.RenameLabel): the new name is
//     on the issue with nothing on the timeline to explain it, and is dealt
//     with here.
//   - Old events take the new name (githubtest.RenameLabelInEvents): the
//     rename looks like an add of the new name by whoever added the old one.
//     An untrusted user's add is reverted by settle, and a stage label that
//     disagrees with the latest transition marker is reported and put back.
//     A rename of a label that the App or a trusted user added can't be told
//     from their own add, so it stands: the one gap left.
//
// No rename names who made it, so this fails closed for the owner too: a
// trusted rename of a pipeline label, such as scope:gh to scope:github,
// removes the new name from every issue that has it, each with a comment.
// The owner adds it back by hand after renaming, which makes events that
// explain it.
//
// A stage label that names the stage of the latest transition marker is
// explained by the marker, the App's own record, so it's never held back,
// left out or removed. That covers the App's own stage write whose event is
// late, and leaving a label out can't then make the stage labels look as if
// they disagree with the marker: the disagreement check reads the timeline,
// which never counts an unexplained label, and the one stage label the
// repair would write is never left out.
//
// A label renamed away, which the timeline says is on but the issue doesn't
// have, is left as before: CheckLabels keeps it in labels, as for a read made
// before the change. Putting it on the issue again would also re-create a
// pipeline label the owner deleted, on every issue that ever had it.
//
// Label events are written late: GitHub was seen writing an issue's labeled
// events 1 to 2 seconds after the issue was created with those labels, and
// the App's own events can lag the same way. A read in that gap sees labels
// with no events yet, which is why the issue is held back, briefly, before
// anything is acted on. The hold is timed per issue, so renaming a label
// again, to a new name, doesn't hold the issue back again; the removal is
// timed per label, and starts again when the label's events change, as when
// the owner removes it and adds it back by hand. Times are by GitHub's
// clock, the Date of the response that reads the issue again (see below),
// or the local clock if it has none. A label renamed to a new name every
// few minutes is never removed, but it's left out, so nothing acts on it.
//
// given is the issue's labels as the caller read them, before the timeline,
// so a change made in between can make a label look unexplained. So
// unexplained reads the issue's labels again, then, before it removes any,
// the timeline. A label still on the issue, with no event for it since the
// first timeline read, is on with nothing to explain it. A label with a new
// event isn't removed, its time kept, and one that's gone is dropped from
// labels. Labels settle has just reverted are left out: settle has
// commented on them already, and the reads may not show its write yet.
//
// Labels set by hand before issuedog adopted an issue have their labeled
// events like any other, so this doesn't touch them, with one exception.
// GitHub has recorded labeled events only since about mid-2014, so an issue
// labelled before then has no events for those labels: an issue that old,
// still open, with pipeline labels would have them all removed.
//
// One comment covers every label removed in a call, each with its own revert
// marker. The revert key is the label's lowercase name, URL-escaped, then "@"
// and the ID of its latest event on the timeline, or 0. A replay whose key is
// posted already doesn't comment again.
func (r *labelRules) unexplained(ctx context.Context, labels, given []string, states map[string]*labelState, settled, reverted map[string]bool,
	marker string, seen *Unexplained) ([]string, []string, error) {
	if seen == nil {
		seen = &Unexplained{}
	}
	issueKey := unexplainedKey(r.owner, r.repo, r.number)
	var suspects []string
	for _, x := range given {
		if pipelineLabel(x) && !settled[strings.ToLower(x)] && (marker == "" || !strings.EqualFold(x, "stage:"+marker)) {
			suspects = append(suspects, x)
		}
	}
	if len(suspects) == 0 {
		seen.set(issueKey, heldIssue{})
		return labels, nil, nil
	}
	drop := func(name string) {
		labels = slices.DeleteFunc(labels, func(x string) bool { return strings.EqualFold(x, name) })
	}
	issue, resp, err := r.c.Issues.Get(ctx, r.owner, r.repo, r.number)
	if err != nil {
		return nil, nil, fmt.Errorf("read the labels of %s/%s#%d again: %w", r.owner, r.repo, r.number, err)
	}
	var still []string
	for _, x := range suspects {
		i := slices.IndexFunc(issue.Labels, func(l *gogithub.Label) bool { return strings.EqualFold(l.GetName(), x) })
		if i < 0 {
			drop(x)
			continue
		}
		still = append(still, issue.Labels[i].GetName())
	}
	if len(still) == 0 {
		seen.set(issueKey, heldIssue{})
		return labels, nil, nil
	}
	now, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		logr.Debugf("the read of %s/%s#%d has no Date GitHub's clock can be read from, so the local clock times its unexplained labels: %v", r.owner, r.repo, r.number, err)
		now = time.Now()
	}
	h := seen.get(issueKey)
	next := heldIssue{since: h.since, labels: map[string]firstSeen{}}
	if next.since.IsZero() {
		next.since = now
	}
	var pending []string // unexplained, and not to be removed yet
	var due []removal
	for _, x := range still {
		had, latest := 0, int64(0)
		if l := states[strings.ToLower(x)]; l != nil {
			had, latest = len(l.events), l.latest().GetID()
		}
		fs, ok := h.labels[strings.ToLower(x)]
		if !ok || fs.events != had {
			fs = firstSeen{at: now, events: had} // new, or its events changed: start again
		}
		next.labels[strings.ToLower(x)] = fs
		if waited := now.Sub(fs.at); waited < UnexplainedWait {
			logr.Debugf("%s on %s/%s#%d is unexplained, for %v so far", x, r.owner, r.repo, r.number, waited)
			pending = append(pending, x)
			continue
		}
		due = append(due, removal{x, url.QueryEscape(strings.ToLower(x)) + "@" + strconv.FormatInt(latest, 10), had})
	}
	seen.set(issueKey, next)
	if len(due) > 0 {
		timeline, err := r.c.TimelineSince(ctx, r.owner, r.repo, r.number, time.Time{})
		if err != nil {
			return nil, nil, fmt.Errorf("read the timeline of %s/%s#%d again: %w", r.owner, r.repo, r.number, err)
		}
		due = slices.DeleteFunc(due, func(d removal) bool {
			n := 0
			for _, e := range timeline {
				if (e.GetEvent() == "labeled" || e.GetEvent() == "unlabeled") && strings.EqualFold(e.GetLabel().GetName(), d.name) {
					n++
				}
			}
			if n != d.had {
				logr.Debugf("%s on %s/%s#%d changed while its labels were checked, so it isn't removed yet", d.name, r.owner, r.repo, r.number)
				pending = append(pending, d.name)
			}
			return n != d.had
		})
		if err := r.remove(ctx, issueKey, seen, due, reverted, drop); err != nil {
			return nil, nil, err
		}
	}
	if len(pending) == 0 {
		return labels, nil, nil
	}
	if held := now.Sub(next.since); held < HoldBound {
		logr.Debugf("%v on %s/%s#%d are unexplained, so it's held back, for %v so far", pending, r.owner, r.repo, r.number, held)
		return labels, pending, nil
	}
	for _, x := range pending {
		drop(x)
	}
	logr.Infof("%v on %s/%s#%d are still unexplained after %v, so they're left out until they're explained or removed", pending, r.owner, r.repo, r.number, HoldBound)
	return labels, nil, nil
}

// removal is an unexplained label to remove.
type removal struct {
	name, key string
	had       int // its events on the first timeline read
}

// remove removes the labels due, after one comment for those whose revert
// key isn't posted already, drops each from the caller's labels, and
// forgets it.
func (r *labelRules) remove(ctx context.Context, issueKey string, seen *Unexplained, due []removal, reverted map[string]bool, drop func(string)) error {
	var names, markers []string
	for _, d := range due {
		if reverted[d.key] {
			continue
		}
		marker, err := FormatMarker(Marker{Kind: "revert", Fields: map[string]string{"key": d.key}})
		if err != nil {
			return fmt.Errorf("revert %s on %s/%s#%d: %w", d.name, r.owner, r.repo, r.number, err)
		}
		// The label name is someone else's text, so a marker in it is escaped (§5.1).
		names, markers = append(names, "`"+Escape(d.name)+"`"), append(markers, marker)
	}
	if len(names) > 0 {
		text := "no change by a trusted user or the App explains it, so it may have come from a renamed repo label"
		if len(names) > 1 {
			text = "no change by a trusted user or the App explains them, so they may have come from renamed repo labels"
		}
		body := fmt.Sprintf("I removed %s: %s. Only trusted users can change pipeline labels.\n\n%s", strings.Join(names, ", "), text, strings.Join(markers, "\n"))
		if _, _, err := r.c.Issues.CreateComment(ctx, r.owner, r.repo, r.number, gogithub.IssueCommentRequest{Body: body}); err != nil {
			return fmt.Errorf("comment on removal of %s from %s/%s#%d: %w", strings.Join(names, ", "), r.owner, r.repo, r.number, err)
		}
	}
	for _, d := range due {
		resp, err := r.c.Issues.RemoveLabelForIssue(ctx, r.owner, r.repo, r.number, d.name)
		if err != nil && (resp == nil || resp.StatusCode != http.StatusNotFound) {
			return fmt.Errorf("remove %s from %s/%s#%d: %w", d.name, r.owner, r.repo, r.number, err)
		}
		drop(d.name)
		seen.forget(issueKey, strings.ToLower(d.name))
		logr.Infof("removed %s from %s/%s#%d: no trusted change explains it", d.name, r.owner, r.repo, r.number)
	}
	return nil
}
