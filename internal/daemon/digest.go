package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
)

// The digest (SPEC §9.4, plan #7 T7) is the App's issue that lists what
// waits on the owner. It is the issue the App authored whose body has the
// identity marker, read even in an edited body (the plan's assumption 1),
// since the marker holds no state, or one listed finds by other means if
// an edit took the marker out. If there's more than one, the lowest
// number wins and the rest are logged. Every one of them is kept out of the
// pipeline: the poll never marks one stale, so it has no snapshot, and step
// 1, CheckLabels, intake, stage runs and triage never see it.
const (
	digestTitle  = "issuedog digest"
	digestMarker = "<!-- issuedog:digest -->"
	// digestPlaceholder stands in for the list until T8 renders it.
	digestPlaceholder = "This issue will list what waits on you: open questions, pending approvals and state errors."
	// digestPinPrefix starts the pin note, the body's first line while the
	// digest may not be pinned (digestPin).
	digestPinPrefix = "I couldn't pin this issue"
	// digestPinPending is the pin note a new digest is created with, so a
	// crash before its pin leaves the note that makes the next start pin it.
	digestPinPending = digestPinPrefix + " yet. If this line stays, pin it by hand."
)

// digestState is what the daemon knows of the digest, from the poll.
type digestState struct {
	issues map[int]digestSeen   // the digests, by number, as last listed or written
	news   map[int64]digestNews // the App's digest-new comments, by ID
	done   digestSeen           // the close or reopen last resolved (digestOpen), without its body
	pinned int                  // the digest whose pin note this start has handled
	note   string               // the pin note the body should start with, "" for none
	others string               // the other digests last logged
}

// digestSeen is a digest as listed: its number, state, state_reason,
// closed_at, updated_at and body.
type digestSeen struct {
	number          int
	open            bool
	reason          string
	closed, updated time.Time
	body            string
}

// digestNews is a digest-new comment of the App's: the issue it's on, the
// item keys it announced, and whether it was edited, which makes it count
// for nothing (the plan's assumption 11).
type digestNews struct {
	issue  int
	items  []string
	edited bool
}

// listed records a listed issue, never a pull request, which the poll
// passes over: as a digest if it is one, and reports whether it is. An
// issue of the App's is a digest if:
//   - its body has the identity marker, or
//   - this daemon has found it a digest before, so an edit that takes the
//     marker out, by anyone, changes nothing, and the next pass puts the
//     marker back (digestMark), or
//   - its title is digestTitle and its body has no origin marker, which every
//     other issue the App files has (SPEC §5.2). That finds a digest whose
//     marker was taken out while no daemon ran, or just before a restart.
//
// Gap, for SPEC §16 (T10, #162): a digest whose marker was taken out and
// whose title was changed, both before this daemon listed it, isn't found.
// A new digest is created, and the old one is an issue like any other, so
// triage runs on it.
func (g *digestState) listed(slug string, is *gogithub.Issue) bool {
	n := is.GetNumber()
	if is.GetUser().GetLogin() != slug+"[bot]" {
		return false // only the App's issues, whose author never changes, are ever digests; only a bot's login ends in [bot]
	}
	markers := github.ParseMarkers(is.GetBody())
	titled := is.GetTitle() == digestTitle && !slices.ContainsFunc(markers, func(m github.Marker) bool { return m.Kind == "origin" })
	if !g.has(n) && !titled && !slices.ContainsFunc(markers, digestIdentity) {
		return false
	}
	if g.issues == nil {
		g.issues = map[int]digestSeen{}
	}
	g.issues[n] = digestSeen{number: n, open: is.GetState() == "open", reason: is.GetStateReason(), closed: is.GetClosedAt().Time,
		updated: is.GetUpdatedAt().Time, body: is.GetBody()}
	return true
}

// digestIdentity reports whether m is the digest's identity marker.
func digestIdentity(m github.Marker) bool { return m.Kind == "digest" }

// comment records a listed comment on issue n if it is an App comment with
// a digest-new marker, and forgets it otherwise.
func (g *digestState) comment(slug string, n int, c *gogithub.IssueComment) {
	author := github.Author{Login: c.GetUser().GetLogin(), Type: c.GetUser().GetType()}
	for _, m := range github.TrustedMarkers(author, c.GetBody(), slug) {
		if m.Kind == "digest-new" {
			if g.news == nil {
				g.news = map[int64]digestNews{}
			}
			g.news[c.GetID()] = digestNews{issue: n, items: strings.Split(m.Fields["items"], ","),
				edited: c.GetUpdatedAt().After(c.GetCreatedAt().Time)}
			return
		}
	}
	delete(g.news, c.GetID())
}

// announced returns the item keys the App's digest-new comments on issue n
// announced, but for edited ones: the items not new any more (T8).
func (g *digestState) announced(n int) map[string]bool {
	keys := map[string]bool{}
	for _, c := range g.news {
		if c.issue != n || c.edited {
			continue
		}
		for _, k := range c.items {
			keys[k] = true
		}
	}
	return keys
}

// has reports whether issue n is a digest.
func (g *digestState) has(n int) bool {
	_, ok := g.issues[n]
	return ok
}

// digestPass keeps the digest, if Daemon.Digest is set: after step 1 and
// before Work, since a stage run can take minutes (assumption 13).
//   - With no digest, it creates one (assumption 2). That's in the loop, not
//     at startup, so a failure doesn't stop the daemon (SPEC §3.5); after a
//     crash that follows the create, the next poll finds it.
//   - It resolves a close or reopen (digestOpen, Q4a). A digest a trusted
//     user closed is off: nothing more is done to it.
//   - It handles the pin note (digestPin).
//   - It puts back the identity marker if an edit took it out (digestMark).
//
// An unchanged pass makes no request about the digest: the poll has already
// listed it.
func (d *Daemon) digestPass(ctx context.Context) error {
	if !d.Digest {
		return nil
	}
	a, g := d.Step.Applier, &d.digest
	nums := slices.Sorted(maps.Keys(g.issues))
	if len(nums) == 0 {
		body := digestBody(digestPinPending)
		n, err := a.Client.CreateIssue(ctx, a.Owner, a.Repo, digestTitle, body)
		if err != nil {
			return fmt.Errorf("create the digest: %w", err)
		}
		logr.Infof("created the digest, %s/%s#%d", a.Owner, a.Repo, n)
		if g.issues == nil {
			g.issues = map[int]digestSeen{}
		}
		g.issues[n] = digestSeen{number: n, open: true, body: body}
		nums = []int{n}
	}
	n := nums[0]
	if others := fmt.Sprint(nums[1:]); others != g.others {
		g.others = others
		if len(nums) > 1 {
			logr.Warnf("%s/%s has more than one digest: #%d is the digest, and the App's issues %v, which are digests too, are left alone",
				a.Owner, a.Repo, n, nums[1:])
		}
	}
	on, err := d.digestOpen(ctx, g.issues[n])
	if err != nil || !on {
		return err
	}
	if err := d.digestPin(ctx, n); err != nil {
		return err
	}
	// T8 replaces digestMark: it renders the list with g.note at its top,
	// which has the marker, writes it with digestEdit, and announces the
	// keys g.announced(n) doesn't have.
	return d.digestMark(ctx, n)
}

// digestMark puts back the digest's identity marker if an edit took it
// out, rewriting the body with the pin note the body should have: the one
// this start's pin check found, if it ran (digestPin), and none otherwise,
// since then the body had none. The body is the App's own, so whatever the
// edit wrote goes, rather than being posted again as the App's.
func (d *Daemon) digestMark(ctx context.Context, n int) error {
	a, g := d.Step.Applier, &d.digest
	if slices.ContainsFunc(github.ParseMarkers(g.issues[n].body), digestIdentity) {
		return nil
	}
	logr.Infof("%s/%s#%d, the digest, lost its marker to an edit, so it's put back", a.Owner, a.Repo, n)
	note := ""
	if g.pinned == n {
		note = g.note
	}
	return d.digestEdit(ctx, n, digestBody(note))
}

// digestOpen resolves the digest's close or reopen (Q4a) and reports whether
// the digest is on. A close by a trusted user stands, and turns the digest
// off until a trusted user reopens it (digestTrusted). Any other close or
// reopen is reverted, as SPEC §6.4 does for any issue. Which stands is
// read from the timeline, so it holds across restarts. The timeline is read
// only for a digest that was ever closed (closed, or open with state_reason
// "reopened"), and only when it changed since the last resolution, so an
// unchanged pass reads nothing. An event GitHub hasn't written yet leaves
// the state as it is, until the next pass.
func (d *Daemon) digestOpen(ctx context.Context, s digestSeen) (bool, error) {
	a, g := d.Step.Applier, &d.digest
	s.body = ""
	if s == g.done || s.open && s.reason != "reopened" {
		return s.open, nil
	}
	wrap := func(err error) error {
		return fmt.Errorf("the close of the digest, %s/%s#%d: %w", a.Owner, a.Repo, s.number, err)
	}
	events, err := a.Client.TimelineSince(ctx, a.Owner, a.Repo, s.number, time.Time{})
	if err != nil {
		return false, wrap(err)
	}
	stands, last, lastAt := false, "", time.Time{}
	for _, e := range events {
		if e.GetEvent() != "closed" && e.GetEvent() != "reopened" {
			continue
		}
		last, lastAt = e.GetEvent(), e.GetCreatedAt().Time
		ok, err := d.digestTrusted(ctx, e.GetActor())
		if err != nil {
			return false, wrap(err)
		}
		if ok {
			stands = e.GetEvent() == "closed"
		}
	}
	// The newest event must be the one that made the state listed: for a
	// close, one from closed_at on, as an older close may be newest while
	// a later reopen's and close's events are still to come.
	want, written := "reopened", last == "reopened"
	if !s.open {
		want, written = "closed", last == "closed" && !lastAt.Before(s.closed)
	}
	if !written {
		logr.Infof("%s/%s#%d, the digest, has no %s event yet, so it's looked at again next pass", a.Owner, a.Repo, s.number, want)
		return s.open, nil
	}
	switch {
	case stands == !s.open:
		g.done = s
		return s.open, nil
	case stands:
		logr.Infof("%s/%s#%d, the digest, was reopened by someone untrusted after a trusted close, so it's closed again", a.Owner, a.Repo, s.number)
		if _, _, err := a.Client.Issues.Update(ctx, a.Owner, a.Repo, s.number, gogithub.UpdateIssueRequest{State: new("closed")}); err != nil {
			return false, wrap(fmt.Errorf("close it again: %w", err))
		}
		return false, nil
	default:
		logr.Infof("%s/%s#%d, the digest, was closed by someone untrusted, so it's reopened", a.Owner, a.Repo, s.number)
		if err := a.Client.ReopenIssue(ctx, a.Owner, a.Repo, s.number); err != nil {
			return false, wrap(err)
		}
		return true, nil
	}
}

// digestTrusted reports whether a close or reopen by actor stands: a
// trusted user's (SPEC §6.1), not a bot's or a ghost's. The App's own
// closes and reopens only revert someone else's, so they never change what
// stands, and count as no one's.
func (d *Daemon) digestTrusted(ctx context.Context, actor *gogithub.User) (bool, error) {
	if actor.GetLogin() == "" || actor.GetType() == "Bot" {
		return false, nil
	}
	ok, err := d.Step.Trust.Trusted(ctx, actor.GetLogin())
	if err != nil {
		return false, fmt.Errorf("trust %s: %w", actor.GetLogin(), err)
	}
	return ok, nil
}

// digestPin handles the pin note (assumption 3). While the digest's body
// starts with it, the first pass of each start that finds the digest on
// checks whether it's pinned, and if not, pins it. Once the digest is
// pinned, the note goes, and it's never pinned again, so an unpin by the
// owner stands. If GitHub refuses the pin, such as for a missing permission
// or three pinned issues already, or answers with no error but no pin,
// that's logged, the note names GitHub's message, and the loop carries on.
// Any other failure is the pass's error, which Run logs, and the next pass
// tries again. So does a failed edit of the note: the check counts as done
// only once the body says what it found.
func (d *Daemon) digestPin(ctx context.Context, n int) error {
	a, g := d.Step.Applier, &d.digest
	if g.pinned == n || !strings.HasPrefix(strings.TrimSpace(g.issues[n].body), digestPinPrefix) {
		return nil
	}
	wrap := func(err error) error {
		return fmt.Errorf("the pin of the digest, %s/%s#%d: %w", a.Owner, a.Repo, n, err)
	}
	pinned, err := a.Client.IsPinned(ctx, a.Owner, a.Repo, n)
	if err != nil {
		return wrap(err)
	}
	note := ""
	if !pinned {
		err := a.Client.PinIssue(ctx, a.Owner, a.Repo, n)
		switch {
		case errors.Is(err, github.ErrGraphQL) || errors.Is(err, github.ErrNotPinned):
			logr.Errorf("pinning %s/%s#%d, the digest, failed, so its body says so, and the next start tries again: %v", a.Owner, a.Repo, n, err)
			note = digestPinNote(err)
		case err != nil:
			return wrap(err)
		}
	}
	if err := d.digestEdit(ctx, n, digestBody(note)); err != nil {
		return err
	}
	g.pinned, g.note = n, note
	return nil
}

// digestPinNote is the pin note naming GitHub's message in err, a pin's
// error that wraps github.ErrGraphQL, or github.ErrNotPinned's text for one
// that wraps that. GitHub wrote the message, not the App, so it's escaped
// (SPEC §5.1), and kept to one code span.
func digestPinNote(err error) string {
	msg := err.Error()
	if _, after, ok := strings.Cut(msg, github.ErrGraphQL.Error()+": "); ok {
		msg = after
	}
	if errors.Is(err, github.ErrNotPinned) {
		msg = github.ErrNotPinned.Error()
	}
	msg = strings.ReplaceAll(github.Escape(text(msg)), "`", "'")
	return digestPinPrefix + ": `" + msg + "`. Pin it by hand."
}

// digestBody is the digest's body: note, the pin note, at its top if it
// isn't "", then the list, then the identity marker. The list is a
// placeholder until T8 renders it here (plan #7, T6 and T8).
func digestBody(note string) string {
	body := digestPlaceholder + "\n\n" + digestMarker
	if note != "" {
		body = note + "\n\n" + body
	}
	return body
}

// digestEdit writes body as the digest's, unless it is already, as last
// listed or written. It's the one way the digest's body is written: T8's
// rewrite of the list calls it too.
func (d *Daemon) digestEdit(ctx context.Context, n int, body string) error {
	a, g := d.Step.Applier, &d.digest
	s := g.issues[n]
	if s.body == body {
		return nil
	}
	if _, _, err := a.Client.Issues.Update(ctx, a.Owner, a.Repo, n, gogithub.UpdateIssueRequest{Body: &body}); err != nil {
		return fmt.Errorf("edit the body of the digest, %s/%s#%d: %w", a.Owner, a.Repo, n, err)
	}
	s.body = body
	g.issues[n] = s
	return nil
}
