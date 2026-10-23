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
	"github.com/mertenvg/issuedog/internal/state"
)

// The digest (SPEC §9.4, #159) is the App's issue that lists what waits on
// the owner. It is the issue the App authored whose body has the identity
// marker (digestRenderIdentity), read even in an edited body (SPEC §5.1),
// since the marker holds no state, or one listed finds by other means if an
// edit took the marker out. If there's more than one, the lowest number wins
// and the rest are logged. Every one of them is kept out of the pipeline: the
// poll never marks one stale, so it has no snapshot, and step 1, CheckLabels,
// intake, stage runs and triage never see it.
//
// The digest's body has no rewrite marker (SPEC §5.1, #169), though it's
// a whole body the App writes: it is never read for state but for its
// identity marker, which is read in an edited body anyway, it has no run
// to name, and the pin note must be its first line (digestPin). It's an
// exception for #162 to write into §5.1, beside the identity
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
	// pinFailed is the digest whose pin check or pin failed on this start,
	// other than by GitHub's refusal, with note naming the failure: it's
	// tried again only on a pass that writes the body anyway (digestPin).
	pinFailed int
	// wrote is the digest's updated_at after the App's last write to it,
	// a body edit or a comment, as GitHub answered it (digestOpen).
	wrote time.Time
	// late is the reopen being waited for (lateReopen), or the zero value.
	late lateReopenSeen
	// origins are the App's comments with an origin marker, edited or not,
	// by ID, as the issue each is on: an issue with one is never the
	// digest by its title (digestTake).
	origins map[int64]int
	// titled are the issues only the title rule would take, as last
	// listed, till digestTake judges them after the poll's comments, or,
	// for one whose edit history failed to read, till a pass reads it
	// (digestHistoryHold).
	titled map[int]*gogithub.Issue
	// firsts records, by issue number, whether the App's first version of
	// the issue's body has an origin marker, from its edit history, which
	// is read once per start for each issue titled holds (digestTake).
	firsts map[int]bool
	// history is what this start knows of the failed reads of the edit
	// histories of issues titled holds, by issue number (digestTake).
	history map[int]digestHistoryFail
	// historyErrs are the App's comments with a digesthistory state error,
	// edited or not, by ID, as the issue and key each is for
	// (digestHistoryReport).
	historyErrs map[int64]digestHistoryPosted
}

// lateReopenSeen is a reopen of the digest being waited for (lateReopen):
// the digest's number, the ID of the newest close or reopen event on its
// timeline then, 0 for none, and GitHub's clock when a pass first found
// it so.
type lateReopenSeen struct {
	number int
	event  int64
	since  time.Time
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
// for nothing (SPEC §9.4, "Announcing new items").
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
//     body again, marker and all (digestRewrite).
//
// One whose title is digestTitle and whose body has no origin marker, which
// every other issue the App files has (SPEC §5.2), may be a digest whose
// marker was taken out while no daemon ran, or just before a restart. But
// it may as well be an App task whose body lost its origin to an edit, or
// had it escaped by MarkDirty, and that was renamed (#87's probe). So it's
// no digest yet: it's held in titled, and digestTake judges it after the
// poll's comments, and records it if it is one.
//
// Gap, for SPEC §16 (#162): a digest whose marker was taken out and
// whose title was changed, both before this daemon listed it, isn't found.
// A new digest is created, and the old one is an issue like any other, so
// triage runs on it.
func (g *digestState) listed(slug string, is *gogithub.Issue) bool {
	n := is.GetNumber()
	if is.GetUser().GetLogin() != slug+"[bot]" {
		return false // only the App's issues, whose author never changes, are ever digests; only a bot's login ends in [bot]
	}
	delete(g.titled, n) // judged on this listing alone
	markers := github.ParseMarkers(is.GetBody())
	if !g.has(n) && !slices.ContainsFunc(markers, digestIdentity) {
		if is.GetTitle() == digestTitle && !slices.ContainsFunc(markers, digestOrigin) {
			if g.titled == nil {
				g.titled = map[int]*gogithub.Issue{}
			}
			g.titled[n] = is
		}
		return false
	}
	g.found(is)
	return true
}

// found records is, a listed issue, as a digest.
func (g *digestState) found(is *gogithub.Issue) {
	n := is.GetNumber()
	if g.issues == nil {
		g.issues = map[int]digestSeen{}
	}
	g.issues[n] = digestSeen{number: n, open: is.GetState() == "open", reason: is.GetStateReason(), closed: is.GetClosedAt().Time,
		updated: is.GetUpdatedAt().Time, body: is.GetBody()}
}

// digestIdentity reports whether m is the digest's identity marker.
func digestIdentity(m github.Marker) bool { return m.Kind == "digest" }

// digestOrigin reports whether m is an origin marker.
func digestOrigin(m github.Marker) bool { return m.Kind == "origin" }

// comment records a listed comment on issue n if it is an App comment with
// a digest-new marker, and forgets it otherwise. The same goes for an App
// comment with an origin marker, edited or not, for digestTake, and one
// with a digesthistory state error (digestHistoryReport).
func (g *digestState) comment(slug string, n int, c *gogithub.IssueComment) {
	author := github.Author{Login: c.GetUser().GetLogin(), Type: c.GetUser().GetType()}
	markers := github.TrustedMarkers(author, c.GetBody(), slug)
	if !g.record(c.GetID(), n, markers, c.GetUpdatedAt().After(c.GetCreatedAt().Time)) {
		delete(g.news, c.GetID())
	}
	g.historyComment(c.GetID(), n, markers)
	if !slices.ContainsFunc(markers, digestOrigin) {
		delete(g.origins, c.GetID())
		return
	}
	if g.origins == nil {
		g.origins = map[int64]int{}
	}
	g.origins[c.GetID()] = n
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

// digestTake judges the issues held because only the title rule would take
// them (digestState.listed), once the poll has listed the comments too: on
// a start, every one. It takes one as a digest, recorded and out of the
// pipeline from then on (Daemon.leave), only if both hold:
//   - No App comment on it has an origin marker, edited or not
//     (digestState.comment), such as a reposted one.
//   - The App's first version of its body had no origin marker. That's its
//     body as listed if it was never edited, and else the original post in
//     its edit history (github's History), read once per start for each
//     issue and kept by number. One whose original text someone deleted
//     from the history can't show that it had none, so it counts as one
//     that had.
//
// So an App task whose body lost its origin and that was renamed is never
// the digest, with or without a reposted origin, while a digest whose
// marker was taken out is still found by its title. One query reads every
// history needed, and an unchanged poll, which lists nothing new, needs
// none while no issue is held (below): one it lists again, at the cursor,
// was judged already. A poll that fails before digestTake keeps what it
// held for the next one.
//
// A failed read is logged, and each issue it was for stays held, unjudged:
// it's kept out of step 1 and Work (digestHistoryHold), and digestPass
// creates no digest while one is held, since it may be the digest, whose
// marker was taken out, and a new one would be a second (#305). Each later
// pass reads again, in one query, the histories of the issues still held,
// and judges each one read, so the first pass whose read works takes the
// old digest back. An issue a poll lists again is judged on that listing,
// as ever: renamed, or with its marker back, it's held no more. But a read
// that keeps failing would cost a request and an error log on every pass,
// and hold back the pipeline's work on the issue for good, so after
// digestHistoryStrikes failures the issue gets a state error (SPEC §5.3,
// digestHistoryReport), and its history isn't read again on this start. It
// stays held, with no digest created, till the owner acts: a restart reads
// it again, a rename lets the pipeline take it as any other issue, and the
// identity marker put back makes it the digest.
func (d *Daemon) digestTake(ctx context.Context) {
	a, g := d.Step.Applier, &d.digest
	// An issue a listing let go of, renamed or a digest by its marker, has
	// its failures forgotten, so a rename back starts with no strike.
	maps.DeleteFunc(g.history, func(n int, _ digestHistoryFail) bool {
		_, held := g.titled[n]
		return !held
	})
	if len(g.titled) == 0 {
		return
	}
	reposted := map[int]bool{}
	for _, n := range g.origins {
		reposted[n] = true
	}
	need := map[string]int{} // by node ID
	for n, is := range g.titled {
		if _, ok := g.firsts[n]; !ok && !reposted[n] && g.history[n].fails < digestHistoryStrikes {
			need[is.GetNodeID()] = n
		}
	}
	if len(need) > 0 {
		read, err := a.Client.History(ctx, slices.Sorted(maps.Keys(need)))
		if err != nil {
			logr.Errorf("the edit histories of %s/%s's issues %v, titled %q, failed to read, so they stay out of the pipeline, and no digest is created, till they're read: %v",
				a.Owner, a.Repo, slices.Sorted(maps.Values(need)), digestTitle, err)
			g.historyFailed(slices.Collect(maps.Values(need)), err)
		} else {
			if g.firsts == nil {
				g.firsts = map[int]bool{}
			}
			for node, n := range need {
				h := read[node]
				// With no edit, the body as listed, which has none, is the first.
				g.firsts[n] = len(h) > 0 && (h[0].Text == "" || slices.ContainsFunc(github.ParseMarkers(h[0].Text), digestOrigin))
			}
		}
	}
	for _, n := range slices.Sorted(maps.Keys(g.titled)) {
		is := g.titled[n]
		if _, ok := g.firsts[n]; !ok && !reposted[n] {
			d.digestHistoryHold(ctx, is)
			continue
		}
		delete(g.titled, n)
		delete(g.history, n)
		if reposted[n] || g.firsts[n] {
			continue
		}
		g.found(is)
		d.leave(n)
		d.closingForget(n)
		logr.Infof("%s/%s#%d is a digest, found by its title: the App filed it with no origin marker, and no comment of the App's on it has one",
			a.Owner, a.Repo, n)
	}
}

// digestPass keeps the digest, if Daemon.Digest is set: after step 1 and
// before Work, since a stage run can take minutes (SPEC §8.1). It ranks
// the items cached for every issue (digestCacheRanked), then:
//   - With no digest, it creates one with them listed (SPEC §9.4).
//     That's in the loop, not at startup, so a failure doesn't stop the
//     daemon (SPEC §3.5); after a crash that follows the create, the next
//     poll finds it. While the title rule holds an issue whose edit
//     history is still to read, which may be the digest, it creates none
//     (digestTake).
//   - It resolves a close or reopen (digestOpen, SPEC §9.4). A digest a
//     trusted user closed is off: nothing more is done to it.
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
	if len(nums) == 0 && len(g.titled) > 0 {
		// One may be the digest, its marker taken out (digestTake).
		logr.Debugf("%s/%s has no digest, and none is created while the edit histories of its issues %v, titled %q, are still to read",
			a.Owner, a.Repo, slices.Sorted(maps.Keys(g.titled)), digestTitle)
		return nil
	}
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
	if g.late.number == n {
		g.late = lateReopenSeen{}
	}
	if g.pinned == n || g.pinFailed == n {
		g.pinned, g.pinFailed, g.note = 0, 0, ""
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
// (SPEC §9.4, "The body" and "Announcing new items"):
//   - The items whose keys no never-edited digest-new comment of the App's
//     on n holds are new (digestRenderNew). If digestRenderNews gives a
//     comment for them, as it does for a kind that doesn't mention the
//     owner already, with notify set, it's posted, one per pass. Its keys
//     are recorded at once (digestState.record), so the next pass doesn't
//     announce them again before the poll lists the comment. A new item
//     with no comment, such as a round, stays new, and never gets one.
//   - Then the body is written, if it isn't already (digestEdit), with
//     the pin note this start's check left (digestPin), a failed check's
//     included (noteOf), or none: when the check didn't run, the body had
//     none.
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
	if err := d.digestEdit(ctx, n, digestBody(g.noteOf(n), items)); err != nil {
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

// digestOpen resolves the digest's close or reopen (SPEC §9.4, "Closing it")
// and reports whether the digest is on. A close by a trusted user stands, and
// turns the digest off until a trusted user reopens it (digestTrusted). Any
// other close or reopen is reverted, as SPEC §6.4 does for any issue. Which
// stands is read from the timeline, so it holds across restarts. The timeline
// is read only for a digest that was ever closed (closed, or open with
// state_reason "reopened"), and only when it changed since the last
// resolution, so an unchanged pass reads nothing.
//
// An event GitHub hasn't written yet leaves the state as it is, with
// nothing reverted, and the next pass looks again. A close's event must
// be from closed_at on, and the digest, listed closed, is off meanwhile.
// For a reopen, see lateReopen: the digest is off while it's waited for,
// at most lateReopenWait, since a write then would make the next listing
// look like the App's own (below), and the reopen would never be looked
// at again.
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
	stands, last, lastAt, lastID := false, "", time.Time{}, int64(0)
	for _, e := range events {
		if e.GetEvent() != "closed" && e.GetEvent() != "reopened" {
			continue
		}
		last, lastAt, lastID = e.GetEvent(), e.GetCreatedAt().Time, e.GetID()
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
	// clears closed_at, so lateReopen judges a reopen.
	if !s.open && (last != "closed" || lastAt.Before(s.closed)) {
		logr.Infof("%s/%s#%d, the digest, has no closed event yet, so it's looked at again next pass", a.Owner, a.Repo, s.number)
		return false, nil
	}
	if s.open && (last != "reopened" || lastAt.Before(s.updated)) {
		switch {
		case d.lateReopen(s, lastID, d.Step.Client.Now()):
			logr.Infof("%s/%s#%d, the digest, has no reopened event yet, so it's looked at again next pass", a.Owner, a.Repo, s.number)
			return false, nil
		case last != "reopened":
			// No resolution stands for the App's writes to match (above),
			// so every pass reads the timeline till the reopen's event
			// shows, even where the last resolution was an earlier reopen
			// (#179's verify).
			g.done = digestSeen{}
			logr.Infof("%s/%s#%d, the digest, still has no reopened event, so it's on, and looked at again next pass", a.Owner, a.Repo, s.number)
			return true, nil
		}
	}
	g.late = lateReopenSeen{}
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

// lateReopenWait is how long, by GitHub's clock, a reopen of the digest is
// waited for at most (lateReopen): as long as an issue with unexplained
// labels is held back, long enough for a late event to show.
const lateReopenWait = github.HoldBound

// lateReopen reports whether digest s, listed open, still waits for the
// reopen that made it open, whose event isn't on the timeline yet, so the
// digest is off for the pass and the timeline is read again next pass
// (digestOpen). event is the ID of the newest close or reopen event, 0
// for none: a close, or a reopen older than updated_at. A reopen clears
// closed_at, so nothing in the listing dates it. The reopen moved
// updated_at, so its event is from updated_at on, unless a later change
// moved updated_at again: an older one may be an earlier reopen's, with a
// later close's and reopen's events still to come, as when a trusted
// close and an untrusted reopen come in GitHub's event lag on a digest
// reopened before.
//
// The wait ends once GitHub's clock is lateReopenWait past the earlier of
// updated_at and the time a pass first found the digest waiting with this
// newest event (lateReopenSeen.since), kept in memory. So a change found
// long after it is hardly waited for, and later changes, such as steady
// comments, which move updated_at, can't stretch the wait (#179's verify):
// the digest is written on the first pass after it. Then an older reopen
// is taken as the newest (digestOpen), and the wait is forgotten, so the
// next change waits again. While a close is the newest event, the digest
// is on again after the wait, as before #179, and still looked at each
// pass until the reopen's event shows: digestOpen forgets the last
// resolution, so no write of the App's ends that. A newer newest event, or
// another digest, starts the wait again. now is GitHub's clock; the wait
// is over at lateReopenWait exactly.
func (d *Daemon) lateReopen(s digestSeen, event int64, now time.Time) bool {
	g := &d.digest
	if l := g.late; l.number != s.number || l.event != event {
		g.late = lateReopenSeen{number: s.number, event: event, since: now}
	}
	start := g.late.since
	if s.updated.Before(start) {
		start = s.updated
	}
	return now.Before(start.Add(lateReopenWait))
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
// stands, and count as no one's. A pipeline issue's close judges its actor
// the same way (closingTrusted), but accepts the App's close too.
func (d *Daemon) digestTrusted(ctx context.Context, actor *gogithub.User) (bool, error) {
	ok, err := closingTrusted(ctx, d.Step.Trust, github.Author{Login: actor.GetLogin(), Type: actor.GetType()})
	if err != nil {
		return false, fmt.Errorf("the digest's closer: %w", err)
	}
	return ok, nil
}

// noteOf is the pin note digest n's body should start with: the one this
// start's check left (digestPin), a failed check's included, or none when
// the check was for another digest or didn't run.
func (g *digestState) noteOf(n int) string {
	if g.pinned == n || g.pinFailed == n {
		return g.note
	}
	return ""
}

// digestPin handles the pin note (SPEC §9.4, "Creating and pinning it").
// While the digest's body starts with it, the first pass of each start that
// finds the digest on checks whether it's pinned, and if not, pins it. Once
// the digest is pinned, the note goes, and it's never pinned again, so an
// unpin by the owner stands. If GitHub refuses the pin, such as for a missing
// permission or three pinned issues already, or answers with no error but no
// pin, that's logged, the note names GitHub's message, and the loop carries
// on. A failed edit of the note is the pass's error, and the next pass checks
// again: the check counts as done only once the body says what it found. The
// body it writes lists items, so the rewrite that follows has nothing to
// change.
//
// Any other failure, the check's included, is logged and named in the note
// (digestPinFailNote), and stops nothing (#226): the rewrite that follows
// announces new items and writes the body with that note. It's tried again
// only on a later pass that writes the body anyway, as the items or the
// body changed, even by an edit that took the note out, which the rewrite
// puts back. So a check that fails on every pass adds no request to an
// unchanged pass, and at most one check and one pin to a changed one, with
// no body edit of its own: a note that changes goes in that pass's edit. The
// next start tries again too, as the note is still there.
//
// A check or pin that finds the digest gone, deleted or transferred, is the
// pass's error, marked gone (digestGone), so the digest is dropped
// (digestPass) and nothing more is written to it. PinIssue pins nothing
// whose read GitHub answered from another repo, so a digest transferred
// mid-pass is never pinned in its new repo (#413).
func (d *Daemon) digestPin(ctx context.Context, n int, items []waitItem) error {
	a, g := d.Step.Applier, &d.digest
	body := g.issues[n].body
	switch {
	case g.pinned == n:
		return nil
	case g.pinFailed == n:
		// The note is this start's own, so it's tried again on any pass
		// that writes the body, even one that puts back a note edited out.
		if digestSameBody(body, digestBody(g.note, items)) {
			return nil
		}
	case !strings.HasPrefix(strings.TrimSpace(body), digestPinPrefix):
		return nil
	}
	pinned, err := a.Client.IsPinned(ctx, a.Owner, a.Repo, n)
	switch {
	case gone(err):
		return digestGone(fmt.Errorf("check the pin of the digest: %w", err))
	case err != nil:
		d.digestPinFailed(n, err, "checking whether it's pinned failed")
		return nil
	}
	note := ""
	if !pinned {
		err := a.Client.PinIssue(ctx, a.Owner, a.Repo, n)
		switch {
		case gone(err):
			return digestGone(fmt.Errorf("pin the digest: %w", err))
		case errors.Is(err, github.ErrGraphQL) || errors.Is(err, github.ErrNotPinned):
			logr.Errorf("pinning %s/%s#%d, the digest, failed, so its body says so, and the next start tries again: %v", a.Owner, a.Repo, n, err)
			note = digestPinNote(err)
		case err != nil:
			d.digestPinFailed(n, err, "pinning it failed")
			return nil
		}
	}
	if err := d.digestEdit(ctx, n, digestBody(note, items)); err != nil {
		return err
	}
	g.pinned, g.pinFailed, g.note = n, 0, note
	return nil
}

// digestPinFailed records that digest n's pin check or pin failed with
// err, other than by GitHub's refusal, as what says: it's logged, and the
// note names it (digestPinFailNote), for the rewrite to write and for
// digestPin to try again on a pass that writes the body anyway. The note
// is digest n's alone, so pinned no longer names another digest, whose
// body noteOf would give it to.
func (d *Daemon) digestPinFailed(n int, err error, what string) {
	a, g := d.Step.Applier, &d.digest
	logr.Errorf("pinning %s/%s#%d, the digest, failed, so its body says so, and a pass that changes it tries again: %s: %v",
		a.Owner, a.Repo, n, what, err)
	g.pinned, g.pinFailed, g.note = 0, n, digestPinFailNote(err, what)
}

// digestPinFailNote is the pin note for a pin check or pin that failed
// with err, other than by GitHub's refusal, as what says. It names the
// HTTP status GitHub answered, if any, but not the rest of err, which may
// name a URL or change from one try to the next: the log has it. So it's
// all the App's text but for a number, and the same failure again leaves
// the body as it is.
func digestPinFailNote(err error, what string) string {
	var er *gogithub.ErrorResponse
	if errors.As(err, &er) && er.Response != nil {
		what += fmt.Sprintf(" (HTTP %d)", er.Response.StatusCode)
	}
	return digestPinPrefix + " yet: " + what + ". I'll try again when the list changes, or pin it by hand."
}

// digestPinNoteMax is the most characters of GitHub's message the pin note
// names, its end cut and marked with "…" when longer (SPEC §9.4), so the
// note stays one short line.
const digestPinNoteMax = 200

// digestPinNote is the pin note naming GitHub's message in err, a pin's
// error that wraps github.ErrGraphQL, or github.ErrNotPinned's text for one
// that wraps that. For an HTTP 401 or 403, which PinIssue takes as a
// refusal too (#226), it's the status and GitHub's message from its
// answer, without the URL go-github's error names. The message is cut to
// digestPinNoteMax characters, on a character's boundary. GitHub wrote it,
// not the App, so it's escaped (SPEC §5.1), and kept to one code span,
// after the cut, so the cut can't leave half an escape.
func digestPinNote(err error) string {
	msg := err.Error()
	if _, after, ok := strings.Cut(msg, github.ErrGraphQL.Error()+": "); ok {
		msg = after
	}
	var er *gogithub.ErrorResponse
	if errors.As(err, &er) && er.Response != nil {
		msg = fmt.Sprintf("HTTP %d", er.Response.StatusCode)
		if er.Message != "" {
			msg += ": " + er.Message
		}
	}
	if errors.Is(err, github.ErrNotPinned) {
		msg = github.ErrNotPinned.Error()
	}
	if r := []rune(msg); len(r) > digestPinNoteMax {
		msg = string(r[:digestPinNoteMax-1]) + "…"
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
// with its dirty content dirt and, if it's held, its open discussion
// triggers holds, and whether an owner hold stays too, ownerHeld
// (holdItemFind), and whether an open issue blocks it, if it's a human
// task, blocked (humanBlocked). Iterate calls it whether or not the
// digest is on, since assignment takes the same items (assign.go). An
// issue with nothing waiting has no entry. If waitFind fails, that's
// logged, and the issue keeps the items it had, as one whose step 1
// failed does.
func (d *Daemon) digestCacheSet(s *github.Snapshot, inputs []github.Comment, dirt Dirt, holds []discTrigger, ownerHeld, blocked bool) {
	st := d.Step
	items, err := waitFind(waitInput{Owner: st.Owner, Repo: st.Repo, AppSlug: st.AppSlug, S: s, Dirt: dirt, Inputs: inputs, CloserErrAnswered: st.closerErrAnswered, Holds: holds,
		OwnerHeld: ownerHeld, Blocked: blocked})
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

// holdItemFind returns the open discussion triggers on s, as its visit
// left it, with its dirty content dirt, for its hold item (waitFind,
// #192), if s is held, and nil otherwise, with no request. Iterate calls
// it at the end of the visit, after step 1 and the other visits, so the
// item shows what's open once step 1 has resolved what it could, and the
// records its hold and notices just wrote (discRecords): #188's
// resolutions in step 1 come before it. It asks the visit's trust
// (trustOnceTrust), which step 1's own look for triggers (Step.discussion)
// asked already, so it makes no permission request of its own. Edit
// histories are kept by the Applier, so it reads none either. The one
// read it may repeat is the reactions of a writer's own edited comment
// that no marker records (discOwnEdits), as for an own edit whose notice
// discLost held back: that read is conditional, so on an unchanged pass
// it costs no rate limit. So are the reactions of the comments a trigger
// may be resolved by (resolver), as step 1 read them already.
//
// With a trigger open, it also reports whether an owner hold on s stays
// once they're resolved (resolveHoldsLeft), as resolveLeave would judge
// it: one a marker records, or a /hold on s no marker records, or on an
// ancestor that the record of a discussion's end names, but not one a
// /resume ended while the issue stayed held (resolveLeft), on s or an
// ancestor. That reads nothing more: an ancestor's /hold that no record
// names yet is read only once s is about to leave held (resolveAbove), so
// until then the item doesn't name it.
func (st *Step) holdItemFind(ctx context.Context, s *github.Snapshot, dirt Dirt) ([]discTrigger, bool, error) {
	is := state.Derive(s, st.AppSlug, dirt.Has)
	if s.Closed || is.Stage != state.Held {
		return nil, false, nil
	}
	found, err := st.discFind(ctx, s, dirt, st.trustOnceTrust())
	if err != nil || len(found) == 0 {
		if err != nil {
			return nil, false, fmt.Errorf("the digest's hold item: %w", err)
		}
		return nil, false, nil
	}
	holds, _, err := st.resolveHoldsLeft(ctx, s, dirt, st.trustOnceTrust(), is)
	if err != nil {
		return nil, false, fmt.Errorf("the digest's hold item: %w", err)
	}
	return found, resolveOwnerHeld(holds), nil
}

// digestCacheRanked returns every issue's cached items that stand
// (digestCacheOf), and the closed issues' closer state errors
// (closerErrDigest), which assignment doesn't take, ranked by SPEC §9.4's
// order against every snapshot held (waitRank).
func (d *Daemon) digestCacheRanked() []waitItem {
	var items []waitItem
	for _, n := range slices.Sorted(maps.Keys(d.digestCache)) {
		items = append(items, d.digestCacheOf(n)...)
	}
	items = append(items, d.closerErrDigest()...)
	waitRank(items, d.snaps)
	return items
}

// digestCacheOf returns issue n's cached items that stand on this pass:
// every one but a finished story's that digestDoneStands doesn't keep.
// The digest and assignment both take them from here, so they never
// disagree.
func (d *Daemon) digestCacheOf(n int) []waitItem {
	return slices.DeleteFunc(slices.Clone(d.digestCache[n]), func(it waitItem) bool {
		return it.Kind == waitDone && !d.digestDoneStands(it)
	})
}

// digestDoneStands reports whether it, a finished story's item (#278),
// stands on this pass: every child its record names is one the poll last
// listed closed (Daemon.closedListed), whose close was judged and
// accepted (closingSeen) with no judging left to do (closingDue), and no
// open snapshot names the story as its parent, such as an issue added
// below it by hand. It's judged only from what the poll listed and the
// snapshots it read, and makes no request: so a child filed this pass,
// which no poll listed yet, or one last listed open, keeps it out, and so
// does one whose close may be untrusted, as while its event is late or
// its closer can't be asked, till that close is judged, accepted or
// reverted (SPEC §6.4). A close that's never judged keeps it out: for
// good once it's forgotten as gone, and for a child with no stage label,
// which closingList doesn't make due unless this daemon held it with one,
// as when an untrusted user took the label off a child no visit has read
// yet and closed it, till a closed stage added to it has its close judged
// (SPEC §16). A child's close changes nothing on the story, so the item
// is found on the story's last visit (waitFind) and judged again here on
// each pass. An epic planned into stories closes by the same judgement
// (epicClosePass, #251).
func (d *Daemon) digestDoneStands(it waitItem) bool {
	for _, c := range it.Children {
		_, judged := d.closingSeen[c]
		if _, due := d.closingDue[c]; !d.closedListed[c] || !judged || due {
			return false
		}
	}
	for _, s := range d.snaps {
		if !s.Closed && s.Parent == it.Issue {
			return false
		}
	}
	return true
}
