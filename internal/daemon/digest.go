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
// identity marker (digestRenderIdentity), read even in an edited body (the
// plan's assumption 1), since the marker holds no state, or one listed
// finds by other means if an edit took the marker out. If there's more
// than one, the lowest number wins and the rest are logged. Every one of
// them is kept out of the pipeline: the poll never marks one stale, so it
// has no snapshot, and step 1, CheckLabels, intake, stage runs and triage
// never see it.
//
// The digest's body has no rewrite marker (SPEC §5.1, #169), though it's
// a whole body the App writes: it is never read for state but for its
// identity marker, which is read in an edited body anyway, it has no run
// to name, and the pin note must be its first line (digestPin). It's an
// exception for #162 (T10) to write into §5.1, beside the identity
// marker's.
const (
	digestTitle = "issuedog digest"
	// digestPinPrefix starts the pin note, the body's first line while the
	// digest may not be pinned (digestPin).
	digestPinPrefix = "I couldn't pin this issue"
	// digestPinPending is the pin note a new digest is created with, so a
	// crash before its pin leaves the note that makes the next start pin it.
	digestPinPending = digestPinPrefix + " yet. If this line stays, pin it by hand."
)

// digestErrGone marks the error of a request about the digest that found
// it gone, deleted (404 or 410) or transferred (gone): digestPass drops it.
var digestErrGone = errors.New("the digest is gone, deleted or transferred")

// digestState is what the daemon knows of the digest, from the poll and
// its own writes.
type digestState struct {
	issues map[int]digestSeen   // the digests, by number, as last listed or written
	news   map[int64]digestNews // the App's digest-new comments, by ID
	done   digestSeen           // the close or reopen last resolved (digestOpen), without its body
	pinned int                  // the digest whose pin note this start has handled
	note   string               // the pin note the body should start with, "" for none
	others string               // the other digests last logged
	// wrote is the digest's updated_at after the App's last write to it,
	// a body edit or a comment, as GitHub answered it (digestOpen).
	wrote time.Time
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
//     marker out, by anyone, changes nothing, and the next pass writes the
//     body again, marker and all (digestRewrite), or
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
	if !g.record(c.GetID(), n, github.TrustedMarkers(author, c.GetBody(), slug), c.GetUpdatedAt().After(c.GetCreatedAt().Time)) {
		delete(g.news, c.GetID())
	}
}

// record records comment id on issue n as a digest-new comment, edited or
// not, if markers, the App's in it, have a digest-new marker, and reports
// whether they do.
func (g *digestState) record(id int64, n int, markers []github.Marker, edited bool) bool {
	for _, m := range markers {
		if m.Kind == "digest-new" {
			if g.news == nil {
				g.news = map[int64]digestNews{}
			}
			g.news[id] = digestNews{issue: n, items: strings.Split(m.Fields["items"], ","), edited: edited}
			return true
		}
	}
	return false
}

// announced returns the item keys the App's digest-new comments on issue n
// announced, but for edited ones: the items not new any more. A deleted
// comment still counts until a restart, as the poll lists no deletion:
// its items aren't announced again till then (#159's verify). A reaction
// on a comment doesn't move its updated_at (checked on real GitHub in
// #160's verify), so a 👍 on an announcement isn't an edit. Whether hiding
// a comment moves it isn't known (SPEC §16): if it does, a hidden comment
// counts as edited, and its items are announced once more.
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
// before Work, since a stage run can take minutes (assumption 13). It ranks
// the items cached for every issue (digestCacheRanked), then:
//   - With no digest, it creates one with them listed (assumption 2).
//     That's in the loop, not at startup, so a failure doesn't stop the
//     daemon (SPEC §3.5); after a crash that follows the create, the next
//     poll finds it.
//   - It resolves a close or reopen (digestOpen, Q4a). A digest a trusted
//     user closed is off: nothing more is done to it.
//   - It handles the pin note (digestPin).
//   - It announces new items, and writes the body if it changed
//     (digestRewrite).
//
// A digest a request finds gone, deleted or transferred, is dropped
// (digestDrop), so the next pass finds or creates one. Iterate takes the
// pass's error, and Work runs anyway. An unchanged pass makes no request
// about the digest: the poll has already listed it.
func (d *Daemon) digestPass(ctx context.Context) error {
	if !d.Digest {
		return nil
	}
	a, g := d.Step.Applier, &d.digest
	items := d.digestCacheRanked()
	nums := slices.Sorted(maps.Keys(g.issues))
	if len(nums) == 0 {
		body := digestBody(digestPinPending, items)
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
	err := d.digestKeep(ctx, n, items)
	if errors.Is(err, digestErrGone) {
		d.digestDrop(n, err)
	}
	return err
}

// digestKeep keeps digest n, which lists items: it resolves a close or
// reopen, and on a digest that is on, handles the pin note, then
// announces new items and writes the body.
func (d *Daemon) digestKeep(ctx context.Context, n int, items []waitItem) error {
	on, err := d.digestOpen(ctx, d.digest.issues[n])
	if err != nil || !on {
		return err
	}
	if err := d.digestPin(ctx, n, items); err != nil {
		return err
	}
	return d.digestRewrite(ctx, n, items)
}

// digestDrop forgets digest n, which err, a request's, found gone: deleted
// (404 or 410) or transferred. It stays in no listing, so it would fail on
// every pass until a restart (#159's verify). The next poll lists every
// issue again, as a start's does, so the next pass finds the digest if it
// is there after all, or another, and creates one only if there's none.
//
// Its digest-new comments are kept: a digest found again, after a 404 that
// was only a glitch, still has them, and the poll's comment cursor has
// gone past them, so they'd never be listed again and its items would be
// announced twice (#160's verify). A digest that is really gone can't
// clash with them, as a new one gets a new number.
func (d *Daemon) digestDrop(n int, err error) {
	a, g := d.Step.Applier, &d.digest
	logr.Warnf("%s/%s#%d, the digest, is gone, so it's dropped, and every issue is listed again to find or create one: %v", a.Owner, a.Repo, n, err)
	delete(g.issues, n)
	if g.done.number == n {
		g.done = digestSeen{}
	}
	if g.pinned == n {
		g.pinned, g.note = 0, ""
	}
	g.wrote = time.Time{}
	d.issuesSince = time.Time{}
}

// digestGone returns err, from a request about the digest, marked with
// digestErrGone too if it says the digest is gone (gone).
func digestGone(err error) error {
	if gone(err) {
		return fmt.Errorf("%w: %w", digestErrGone, err)
	}
	return err
}

// digestMoved returns an error that wraps github.ErrTransferred, and so is
// gone, if resp, GitHub's answer to a write to the digest, or is, the
// issue it returned, if any, shows the digest is no longer in a.Owner's
// a.Repo, and nil otherwise. GitHub answers a request about a transferred
// issue with a 301, which the client follows (github's sameHost), and Go
// follows one for a PATCH or a POST with a GET, which may well succeed:
// so a write that's answered from anywhere but this repo's path wrote
// nothing (#160's verify). Like Snapshot, it also takes an issue whose
// repository_url names another repo as moved.
func digestMoved(a *Applier, resp *gogithub.Response, is *gogithub.Issue) error {
	if resp != nil && resp.Response != nil && resp.Request != nil && resp.Request.URL != nil &&
		!strings.Contains(strings.ToLower(resp.Request.URL.Path), strings.ToLower("/repos/"+a.Owner+"/"+a.Repo+"/")) {
		return fmt.Errorf("%w: answered by %s %s", github.ErrTransferred, resp.Request.Method, resp.Request.URL.Path)
	}
	if u := is.GetRepositoryURL(); u != "" && !strings.HasSuffix(strings.ToLower(u), strings.ToLower("/repos/"+a.Owner+"/"+a.Repo)) {
		return fmt.Errorf("%w to %s", github.ErrTransferred, u)
	}
	return nil
}

// digestRewrite brings digest n up to date with items, ranked
// (assumptions 10 and 11, Q1a and Q2a):
//   - The items whose keys no never-edited digest-new comment of the App's
//     on n holds are new (digestRenderNew). If digestRenderNews gives a
//     comment for them, as it does for a kind that doesn't mention the
//     owner already, with notify set, it's posted, one per pass. Its keys
//     are recorded at once (digestState.record), so the next pass doesn't
//     announce them again before the poll lists the comment. A new item
//     with no comment, such as a round, stays new, and never gets one.
//   - Then the body is written, if it isn't already (digestEdit), with
//     the pin note this start's check left (digestPin), or none: when the
//     check didn't run, the body had none.
//
// The comment comes first, so a crash between the two leaves it, and the
// next pass finds its keys. A failed announcement, or a comment that
// can't be rendered, which is logged, still leaves the body written, and
// the next pass announces the items. The edit's error and the comment's
// are the pass's, but for a digest that's gone, which needs no edit.
func (d *Daemon) digestRewrite(ctx context.Context, n int, items []waitItem) error {
	a, g := d.Step.Applier, &d.digest
	var notify []string
	if d.Config != nil {
		notify = d.Config.Notify
	}
	var errs []error
	news, err := digestRenderNews(digestRenderNew(items, g.announced(n)), notify)
	if err != nil {
		logr.Errorf("the new items on %s/%s#%d, the digest, can't be announced, so only its body is written: %v", a.Owner, a.Repo, n, err)
	}
	if news != "" {
		if err := d.digestAnnounce(ctx, n, news); errors.Is(err, digestErrGone) {
			return err
		} else if err != nil {
			errs = append(errs, err)
		}
	}
	note := ""
	if g.pinned == n {
		note = g.note
	}
	if err := d.digestEdit(ctx, n, digestBody(note, items)); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// digestAnnounce posts text, a digest-new comment, on digest n, and
// records it at once, as the poll would.
func (d *Daemon) digestAnnounce(ctx context.Context, n int, text string) error {
	a, g := d.Step.Applier, &d.digest
	c, resp, err := a.Client.Issues.CreateComment(ctx, a.Owner, a.Repo, n, gogithub.IssueCommentRequest{Body: text})
	if moved := digestMoved(a, resp, nil); moved != nil {
		err = moved // a redirected POST, a GET of the comments, posted nothing, whether that decoded or not
	}
	if err != nil {
		return digestGone(fmt.Errorf("announce new items on the digest, %s/%s#%d: %w", a.Owner, a.Repo, n, err))
	}
	g.record(c.GetID(), n, github.ParseMarkers(text), false)
	g.wrote = latest(g.wrote, c.GetCreatedAt().Time)
	logr.Infof("announced new items on %s/%s#%d, the digest, in comment %d", a.Owner, a.Repo, n, c.GetID())
	return nil
}

// digestOpen resolves the digest's close or reopen (Q4a) and reports whether
// the digest is on. A close by a trusted user stands, and turns the digest
// off until a trusted user reopens it (digestTrusted). Any other close or
// reopen is reverted, as SPEC §6.4 does for any issue. Which stands is
// read from the timeline, so it holds across restarts. The timeline is read
// only for a digest that was ever closed (closed, or open with state_reason
// "reopened"), and only when it changed since the last resolution, so an
// unchanged pass reads nothing.
//
// An event GitHub hasn't written yet leaves the state as it is, and the
// digest off for the pass, with nothing written to it, until the next pass
// looks again: a close whose event isn't on the timeline yet, from
// closed_at on, or a reopen whose event isn't (lateReopen). A write while
// a reopen is looked for would make the next listing look like the App's
// own (below), so the reopen would never be looked at again.
//
// The App's own write, a body edit or a comment, changes the digest's
// updated_at alone. A listing that differs from the last resolution in
// nothing else, and is at the updated_at that write gave (digestState.wrote),
// is that write's, and needs no read: so a digest that was reopened once
// doesn't have its whole timeline read after every write (#159's verify).
// Gap: a close and a reopen, of which only the close stands, both after
// the poll that listed the digest and before the App's write, or in the
// same second as the write, look like that write's. They're found at the
// next close or reopen, or the next start.
func (d *Daemon) digestOpen(ctx context.Context, s digestSeen) (bool, error) {
	a, g := d.Step.Applier, &d.digest
	s.body = ""
	if s == g.done || s.open && s.reason != "reopened" {
		return s.open, nil
	}
	if own := g.done; !g.wrote.IsZero() && s.updated.Equal(g.wrote) {
		if own.updated = s.updated; own == s {
			g.done = s
			return s.open, nil
		}
	}
	wrap := func(err error) error {
		return digestGone(fmt.Errorf("the close of the digest, %s/%s#%d: %w", a.Owner, a.Repo, s.number, err))
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
			return false, fmt.Errorf("the close of the digest, %s/%s#%d: %w", a.Owner, a.Repo, s.number, err)
		}
		if ok {
			stands = e.GetEvent() == "closed"
		}
	}
	// The newest event must be the one that made the state listed: for a
	// close, one from closed_at on, as an older close may be newest while
	// a later reopen's and close's events are still to come. A reopen
	// clears closed_at, so lateReopen judges a reopen event.
	want, written := "reopened", last == "reopened" && !d.lateReopen(s, lastAt)
	if !s.open {
		want, written = "closed", last == "closed" && !lastAt.Before(s.closed)
	}
	if !written {
		logr.Infof("%s/%s#%d, the digest, has no %s event yet, so it's looked at again next pass", a.Owner, a.Repo, s.number, want)
		return false, nil
	}
	switch {
	case stands == !s.open:
		g.done = s
		return s.open, nil
	case stands:
		logr.Infof("%s/%s#%d, the digest, was reopened by someone untrusted after a trusted close, so it's closed again", a.Owner, a.Repo, s.number)
		if err := d.digestSetState(ctx, s.number, "closed"); err != nil {
			return false, wrap(fmt.Errorf("close it again: %w", err))
		}
		return false, nil
	default:
		logr.Infof("%s/%s#%d, the digest, was closed by someone untrusted, so it's reopened", a.Owner, a.Repo, s.number)
		if err := d.digestSetState(ctx, s.number, "open"); err != nil {
			return false, wrap(fmt.Errorf("reopen it: %w", err))
		}
		return true, nil
	}
}

// lateReopenWait is how long, by GitHub's clock, after the digest's
// updated_at a reopen event older than it is waited for (lateReopen): as
// long as an issue with unexplained labels is held back, long enough for
// a late event to show.
const lateReopenWait = github.HoldBound

// lateReopen reports whether the reopen event at at, the newest close or
// reopen on the timeline of digest s, listed open, may not be the reopen
// that made it open, so the timeline is read again next pass (digestOpen).
// A reopen clears closed_at, so nothing in the listing dates it. The
// reopen that made s open moved its updated_at, so its event is from
// updated_at on, unless a later change moved updated_at again. An event
// older than updated_at may be an earlier reopen's, with a later close's
// and reopen's events still to come, as when a trusted close and an
// untrusted reopen come in GitHub's event lag on a digest reopened before.
//
// So such an event is waited for, until GitHub's clock is lateReopenWait
// past updated_at. After that, the events of every change up to updated_at
// have had time to show, and the event is taken as the newest. That bounds
// the wait when a later change, such as a comment, a label or someone
// else's edit, moved updated_at past a reopen that is the newest. While
// the digest is looked at again, the App writes nothing to it, so only
// someone else's change moves updated_at, and starts the wait again.
func (d *Daemon) lateReopen(s digestSeen, at time.Time) bool {
	return at.Before(s.updated) && d.Step.Client.Now().Before(s.updated.Add(lateReopenWait))
}

// digestSetState closes or reopens digest n, state "closed" or "open",
// with a PATCH of its state, which a replay may repeat safely. A PATCH
// that was redirected wrote nothing (digestMoved). The timeline read
// before it is a GET, which a redirect only answers with the moved
// issue's own events, so it's not checked: the write after it is, and so
// is any later edit, which every change to the list makes.
func (d *Daemon) digestSetState(ctx context.Context, n int, state string) error {
	a := d.Step.Applier
	is, resp, err := a.Client.Issues.Update(ctx, a.Owner, a.Repo, n, gogithub.UpdateIssueRequest{State: &state})
	if moved := digestMoved(a, resp, is); moved != nil {
		err = moved
	}
	if err != nil {
		return fmt.Errorf("set the state of %s/%s#%d to %s: %w", a.Owner, a.Repo, n, state, err)
	}
	return nil
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
// Any other failure, the check's included, is the pass's error, which Run
// logs, and the next pass tries again. So does a failed edit of the note:
// the check counts as done only once the body says what it found. The body
// it writes lists items, so the rewrite that follows has nothing to change.
func (d *Daemon) digestPin(ctx context.Context, n int, items []waitItem) error {
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
	if err := d.digestEdit(ctx, n, digestBody(note, items)); err != nil {
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

// digestBody is the digest's body (digestRenderBody): note, the pin note,
// at its top if it isn't "", then items, ranked, then the identity marker.
func digestBody(note string, items []waitItem) string {
	return digestRenderBody(note, items)
}

// digestEdit writes body as the digest's, unless it is already, as last
// listed or written (digestSameBody). It's the one way the digest's body
// is written. An edit that was redirected, as for a transferred digest,
// wrote nothing, and the digest is gone (digestMoved).
func (d *Daemon) digestEdit(ctx context.Context, n int, body string) error {
	a, g := d.Step.Applier, &d.digest
	s := g.issues[n]
	if digestSameBody(s.body, body) {
		return nil
	}
	is, resp, err := a.Client.Issues.Update(ctx, a.Owner, a.Repo, n, gogithub.UpdateIssueRequest{Body: &body})
	if moved := digestMoved(a, resp, is); moved != nil {
		err = moved
	}
	if err != nil {
		return digestGone(fmt.Errorf("edit the body of the digest, %s/%s#%d: %w", a.Owner, a.Repo, n, err))
	}
	s.body = body
	g.issues[n] = s
	g.wrote = latest(g.wrote, is.GetUpdatedAt().Time)
	return nil
}

// digestSameBody reports whether bodies a and b are the same as GitHub
// keeps them: it may store a body's line breaks as "\r\n", as a web edit
// does, and trim the space around it. Neither changes what a body shows,
// so a pass never writes again for them alone.
func digestSameBody(a, b string) bool {
	norm := func(s string) string { return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n")) }
	return norm(a) == norm(b)
}

// digestCacheSet caches what waits on the owner on s (waitFind), which
// step 1 just succeeded on and returned inputs for, as step 1 left it,
// with its dirty content dirt. An issue with nothing waiting has no
// entry. If waitFind fails, that's logged, and the issue keeps the items
// it had, as one whose step 1 failed does.
func (d *Daemon) digestCacheSet(s *github.Snapshot, inputs []github.Comment, dirt Dirt) {
	st := d.Step
	items, err := waitFind(waitInput{Owner: st.Owner, Repo: st.Repo, AppSlug: st.AppSlug, S: s, Dirt: dirt, Inputs: inputs})
	if err != nil {
		logr.Errorf("%s/%s#%d keeps the items it had on the digest: %v", st.Owner, st.Repo, s.Number, err)
		return
	}
	if len(items) == 0 {
		delete(d.digestCache, s.Number)
		return
	}
	if d.digestCache == nil {
		d.digestCache = map[int][]waitItem{}
	}
	d.digestCache[s.Number] = items
}

// digestCacheRanked returns every issue's cached items, ranked by Q3a
// against every snapshot held (waitRank).
func (d *Daemon) digestCacheRanked() []waitItem {
	var items []waitItem
	for _, n := range slices.Sorted(maps.Keys(d.digestCache)) {
		items = append(items, d.digestCache[n]...)
	}
	waitRank(items, d.snaps)
	return items
}
