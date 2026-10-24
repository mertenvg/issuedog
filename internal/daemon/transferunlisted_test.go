package daemon_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/logr/v2"
)

// Held issues no longer in the repo (#237): a settled issue, which no
// pass visits, transferred or deleted, drops out of the repo's listings
// and changes nothing there. Once every ten minutes, by GitHub's clock,
// the daemon lists every open issue and reads again, once, each issue it
// holds that the listing leaves out, which drops it if it's gone. The
// listing brings back, too, a live issue it has that the daemon holds
// nothing of, as when a gap in a listing and a wrong 404 dropped it
// (#426).

// transferUnlistedWait is the daemon's transferUnlistedEvery, the ten
// minutes from one listing of every open issue to the next, and a second
// more: GitHub's clock, which the fake's answers give, is to the second,
// and the daemon's moves on between answers.
const transferUnlistedWait = 10*time.Minute + time.Second

// transferUnlistedListing reports whether r is that listing of every open
// issue, with no since=.
func transferUnlistedListing(r githubtest.Request) bool {
	return r.Method == http.MethodGet && r.Path == "/issues" && strings.Contains(r.Query, "state=open") && !strings.Contains(r.Query, "since=")
}

// transferUnlistedRead matches the path of an issue's own GET, the
// snapshot's first read.
var transferUnlistedRead = regexp.MustCompile(`^/issues/[0-9]+$`)

// transferUnlistedListings counts the listings of every open issue from
// the from'th request on.
func transferUnlistedListings(f *githubtest.Fake, from int) int {
	n := 0
	for _, r := range f.Requests()[from:] {
		if transferUnlistedListing(r) {
			n++
		}
	}
	return n
}

// transferUnlistedSettled is assignGoneFake once issue 12, at ready, has
// lost bob and been read again since, so it's settled: no pass reads it,
// and Work gets it on each one.
func transferUnlistedSettled(t *testing.T) (*githubtest.Fake, *daemon.Daemon) {
	t.Helper()
	f, d := assignGoneFake(t)
	assignStep(t, f, d, "the pass that unassigns bob", assignRemove(12, "bob"))
	assignStep(t, f, d, "the pass that reads 12 again")
	from := len(f.Requests())
	assignStep(t, f, d, "a settled pass")
	if got := assignAbout(f, from, 12); len(got) != 0 {
		t.Fatalf("a settled pass's requests about 12 = %q; want none", got)
	}
	return f, d
}

// A settled issue, transferred or deleted, is read once, by the pass that
// lists every open issue, ten minutes after the last listing, and that
// read drops it: Work never gets it from then on, nothing is written to
// it, and no later pass reads it. Until that pass, nothing reads it, and
// Work still gets it, as nothing in the repo shows the change.
func TestTransferUnlistedDrop(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"deleted", "transferred"} {
		t.Run(name, func(t *testing.T) {
			f, d := transferUnlistedSettled(t)
			assignGoneHow[name](f)
			var worked []int
			assignWorked(d, &worked)
			from := len(f.Requests())
			iterate(t, d)
			if got := assignAbout(f, from, 12); len(got) != 0 || transferUnlistedListings(f, from) != 0 {
				t.Errorf("the pass before the listing is due: requests about 12 = %q, %d listings of the open issues; want none of either",
					got, transferUnlistedListings(f, from))
			}
			if !slices.Contains(worked, 12) {
				t.Errorf("the pass before the listing is due: Work got %v; want 12 still, as nothing shows it gone", worked)
			}
			f.Advance(transferUnlistedWait)
			from = len(f.Requests())
			assignGonePasses(t, f, d, assignGoneRead[name], nil, nil)
			if n := transferUnlistedListings(f, from); n != 1 {
				t.Errorf("the open issues were listed %d times in the three passes from ten minutes on; want once", n)
			}
		})
	}
}

// The listing's pass drops a transferred issue from the digest and from
// assignment too: issue 1, whose question round waits on mertenvg, and
// who is assigned to it, loses its item in the digest's rewrite, and
// nothing is written to it, or read of it, after that pass.
func TestTransferUnlistedDigest(t *testing.T) {
	t.Parallel()
	f, o := digestRunFake(t), digestRunOrigin(t)
	d := digestRunDaemon(t, f, o, nil, nil)
	d.Assign = true
	iterate(t, d)
	if got := assignees(f, 1); !slices.Equal(got, []string{digestRunOwner}) {
		t.Fatalf("issue 1's assignees after the first pass = %q; want mertenvg", got)
	}
	digestRunStep(t, f, d, "a settled pass")
	f.Transfer(1, "elsewhere/r")
	digestRunStep(t, f, d, "the pass before the listing is due")
	if items := digestRunItems(f.State().Issues[11].Body); !slices.Contains(items, "#1 Question round p2 1") {
		t.Fatalf("the digest's items before the listing = %q; want 1's round still", items)
	}
	f.Advance(transferUnlistedWait)
	from, writes := len(f.Requests()), len(f.Writes())
	iterate(t, d)
	if got, want := assignAbout(f, from, 1), []string{"GET /issues/1 301", "GET /repositories/2/issues/1 200"}; !slices.Equal(got, want) {
		t.Errorf("the listing's pass: requests about 1 = %q; want %q", got, want)
	}
	want := slices.DeleteFunc(slices.Clone(digestRunRanked), func(item string) bool { return strings.HasPrefix(item, "#1 ") })
	if got := digestRunItems(f.State().Issues[11].Body); !slices.Equal(got, want) {
		t.Errorf("the digest's items after the listing's pass = %q; want %q, 1's round gone", got, want)
	}
	if got := digestWrites(f, writes); !slices.Equal(got, []string{"PATCH /issues/12"}) {
		t.Errorf("the listing's pass wrote %q; want the digest's rewrite alone", got)
	}
	from = len(f.Requests())
	digestRunStep(t, f, d, "the pass after the listing's")
	digestRunStep(t, f, d, "a later pass")
	if got := assignAbout(f, from, 1); len(got) != 0 {
		t.Errorf("the passes after the listing's: requests about 1 = %q; want none", got)
	}
}

// transferUnlistedUnchanged is planRun's unchanged for a pass that lists
// every open issue too, as one does ten minutes after the last: that
// listing is the one request it may send that GitHub answers with
// anything but a 304, as an issue may have changed since the last one.
func (p *planRun) transferUnlistedUnchanged(step string) {
	p.t.Helper()
	from := len(p.f.Requests())
	p.quiet(step)
	for _, r := range p.f.Requests()[from:] {
		if !transferUnlistedListing(r) && (r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified) {
			p.t.Errorf("%s: an unchanged pass sent %+v; want only the listing of the open issues, and conditional GETs answered 304", step, r)
		}
	}
	if n := transferUnlistedListings(p.f, from); n != 1 {
		p.t.Errorf("%s: an unchanged pass listed the open issues %d times; want once", step, n)
	}
}

// An unchanged pass makes no request beyond the poll's, all answered
// 304, until ten minutes after the start: then it lists every open
// issue, which reads no issue, as each is listed. Ten minutes later, the
// listing is a 304 as well, and the passes between list nothing.
func TestTransferUnlistedUnchanged(t *testing.T) {
	t.Parallel()
	f := quiet(t)
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	iterate(t, d)
	unchanged := func(step string, listings int) {
		t.Helper()
		from := len(f.Requests())
		iterate(t, d)
		for _, r := range f.Requests()[from:] {
			if !transferUnlistedListing(r) && (r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified) {
				t.Errorf("%s: sent %+v; want only conditional GETs answered 304, beside the listing", step, r)
			}
			if transferUnlistedRead.MatchString(r.Path) || r.Path == "/graphql" {
				t.Errorf("%s: read %s %s; want no issue read", step, r.Method, r.Path)
			}
		}
		if n := transferUnlistedListings(f, from); n != listings {
			t.Errorf("%s: listed the open issues %d times; want %d", step, n, listings)
		}
	}
	unchanged("a pass before ten minutes", 0)
	f.Advance(transferUnlistedWait)
	from := len(f.Requests())
	unchanged("the pass ten minutes on", 1)
	if i := slices.IndexFunc(f.Requests()[from:], transferUnlistedListing); i < 0 || f.Requests()[from+i].Status != http.StatusOK {
		t.Errorf("the first listing of the open issues: %+v; want a 200, as nothing is cached", f.Requests()[from:])
	}
	unchanged("the pass after", 0)
	f.Advance(transferUnlistedWait)
	from = len(f.Requests())
	unchanged("the pass ten minutes after the listing", 1)
	if i := slices.IndexFunc(f.Requests()[from:], transferUnlistedListing); i < 0 || f.Requests()[from+i].Status != http.StatusNotModified {
		t.Errorf("the second listing of the open issues: %+v; want a 304, as nothing changed", f.Requests()[from:])
	}
}

// A failed listing is the pass's error, and the rest of the pass carries
// on: Work gets its issues. The next listing waits ten minutes from the
// failed one, as from a good one, so a pass between makes no request for
// it, and Work still gets the transferred issue. Ten minutes on, the
// listing drops it.
func TestTransferUnlistedFailed(t *testing.T) {
	t.Parallel()
	f, d := transferUnlistedSettled(t)
	f.Transfer(12, "o/elsewhere")
	var worked []int
	assignWorked(d, &worked)
	f.Advance(transferUnlistedWait)
	f.AnswerOnce(transferUnlistedListing, http.StatusInternalServerError)
	from := len(f.Requests())
	if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "the check for held issues no longer listed") {
		t.Errorf("Iterate with a failed listing = %v; want its error", err)
	}
	if got := assignAbout(f, from, 12); len(got) != 0 || !slices.Contains(worked, 7) {
		t.Errorf("the failed listing's pass: requests about 12 = %q, Work got %v; want no request, and Work its issues, 7 among them", got, worked)
	}
	worked = nil
	from = len(f.Requests())
	iterate(t, d)
	if got, n := assignAbout(f, from, 12), transferUnlistedListings(f, from); len(got) != 0 || n != 0 || !slices.Contains(worked, 12) {
		t.Errorf("the pass after the failed listing: requests about 12 = %q, %d listings of the open issues, Work got %v; want none of either, and 12 given to Work, as the listing waits",
			got, n, worked)
	}
	f.Advance(transferUnlistedWait)
	from = len(f.Requests())
	assignGonePasses(t, f, d, assignGoneRead["transferred"], nil)
	if n := transferUnlistedListings(f, from); n != 1 {
		t.Errorf("the passes ten minutes after the failure listed the open issues %d times; want once", n)
	}
}

// An issue the listing leaves out that is there after all, here one
// transferred away and back while the listing pages, is read once, kept
// and given to Work, and not read again after.
func TestTransferUnlistedBack(t *testing.T) {
	t.Parallel()
	f, d := transferUnlistedSettled(t)
	f.Advance(transferUnlistedWait)
	f.BeforeRequest(transferUnlistedListing, func() {
		f.Transfer(12, "o/elsewhere")
		f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues/12" },
			func() { f.Transfer(12, "o/r") })
	})
	var worked []int
	assignWorked(d, &worked)
	from := len(f.Requests())
	iterate(t, d)
	reads := 0
	for _, r := range assignAbout(f, from, 12) {
		if strings.HasPrefix(r, "GET /issues/12 ") {
			reads++
		}
	}
	if reads != 1 || !slices.Contains(worked, 12) {
		t.Errorf("the listing's pass: requests about 12 = %q, Work got %v; want one read, and 12 given to Work", assignAbout(f, from, 12), worked)
	}
	worked = nil
	from = len(f.Requests())
	iterate(t, d)
	if got := assignAbout(f, from, 12); len(got) != 0 || !slices.Contains(worked, 12) {
		t.Errorf("the pass after: requests about 12 = %q, Work got %v; want none, and 12 given to Work", got, worked)
	}
}

// transferUnlistedReplayFake is digestPinned with issue 2 at clarifying
// with an unanswered round, and the digest as a pass over them leaves it:
// listing the round, with its announcement. It's copied from such a pass
// over the same repo, so a new daemon's first pass over it writes nothing.
func transferUnlistedReplayFake(o *origin) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		t.Helper()
		src := digestPinned(t)
		digestRound(src)
		if err := digestDaemon(t, src, o, nil).Iterate(context.Background()); err != nil {
			t.Fatal(err)
		}
		digest := src.State().Issues[0]
		f := newFake(t)
		f.SetPinned(f.AddIssue(app, "issuedog digest", digest.Body), true)
		digestRound(f)
		for _, c := range digest.Comments {
			f.AddComment(1, c.Author, c.Body)
		}
		return f
	}
}

// transferUnlistedReplayPass is a new daemon's first pass over f, then a
// transfer of issue 2, and ten minutes on, the pass that lists every open
// issue and drops 2. It returns the passes' errors.
func transferUnlistedReplayPass(t *testing.T, o *origin) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		d := digestDaemon(t, f, o, nil)
		if err := d.Iterate(context.Background()); err != nil {
			return err
		}
		f.Transfer(2, "elsewhere/r")
		f.Advance(transferUnlistedWait)
		return d.Iterate(context.Background())
	}
}

// The listing's pass, which drops a transferred issue and rewrites the
// digest without its item, is safe to run again after a crash at that
// write, the pass's only one: a restart's first pass, which never holds
// the transferred issue, rewrites the digest the same way, or finds it
// rewritten.
func TestTransferUnlistedReplay(t *testing.T) {
	t.Parallel()
	o := digestNotifyOrigin(t)
	f := transferUnlistedReplayFake(o)(t)
	if items := digestRunItems(f.State().Issues[0].Body); !slices.Equal(items, []string{"#2 Question round p2 1"}) {
		t.Fatalf("the digest's items before the passes = %q; want 2's round", items)
	}
	if err := transferUnlistedReplayPass(t, o)(f); err != nil {
		t.Fatal(err)
	}
	if got := digestWrites(f, 0); !slices.Equal(got, []string{"PATCH /issues/1"}) {
		t.Fatalf("the passes wrote %q; want the digest's rewrite alone", got)
	}
	if items := digestRunItems(f.State().Issues[0].Body); len(items) != 0 {
		t.Errorf("the digest's items after the drop = %q; want none", items)
	}
	githubtest.Replay(t, transferUnlistedReplayFake(o), transferUnlistedReplayPass(t, o))
}

// No listing is made before ten minutes have passed since the last one,
// by GitHub's clock: nine minutes after a start's first pass, a pass
// lists nothing, and a minute later, one does.
func TestTransferUnlistedNotDue(t *testing.T) {
	t.Parallel()
	f := quiet(t)
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	f.Advance(9 * time.Minute)
	from := len(f.Requests())
	iterate(t, d)
	if n := transferUnlistedListings(f, from); n != 0 {
		t.Errorf("the pass nine minutes after the start listed the open issues %d times; want none, as ten minutes haven't passed", n)
	}
	f.Advance(transferUnlistedWait - 9*time.Minute)
	from = len(f.Requests())
	iterate(t, d)
	if n := transferUnlistedListings(f, from); n != 1 {
		t.Errorf("the pass ten minutes after the start listed the open issues %d times; want once", n)
	}
}

// A pass that holds no issue, and has dropped none since the last
// listing, lists nothing, ten minutes on or not: here the repo's only
// open one is a pull request, beside a closed issue.
func TestTransferUnlistedNoneHeld(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.SetPullRequest(f.AddIssue("owner", "PR", ""))
	f.Close(f.AddIssue("owner", "Closed", ""))
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	f.Advance(transferUnlistedWait)
	from := len(f.Requests())
	iterate(t, d)
	iterate(t, d)
	if n := transferUnlistedListings(f, from); n != 0 {
		t.Errorf("the passes ten minutes on, holding no issue, listed the open issues %d times; want none", n)
	}
}

// transferUnlistedIssueRead reports whether r is the GET of issue n's own
// path, a snapshot's first read.
func transferUnlistedIssueRead(n int) func(githubtest.Request) bool {
	path := fmt.Sprintf("/issues/%d", n)
	return func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == path }
}

// transferUnlistedGap makes the next listing of every open issue leave
// out issue n, live all along, as a gap in GitHub's listing might, and
// the next read of n answer a wrong 404.
func transferUnlistedGap(f *githubtest.Fake, n int) {
	f.AnswerOnce(transferUnlistedIssueRead(n), http.StatusNotFound)
	f.BeforeRequest(transferUnlistedListing, func() {
		f.Transfer(n, "o/elsewhere")
		f.BeforeRequest(transferUnlistedIssueRead(n), func() { f.Transfer(n, "o/r") })
	})
}

// transferUnlistedPast edits issue 7's body, and runs the pass that reads
// it, which writes nothing, so the poll's cursor is past issue 12's last
// change, and no poll lists 12 again.
func transferUnlistedPast(t *testing.T, f *githubtest.Fake, d *daemon.Daemon) {
	t.Helper()
	f.EditBody(7, digestRunOwner, "A part, edited.")
	writes := len(f.Writes())
	iterate(t, d)
	if got := digestWrites(f, writes); len(got) != 0 {
		t.Fatalf("the pass that reads 7's edit wrote %q; want nothing", got)
	}
}

// A live issue wrongly dropped, by a gap in the listing and a wrong 404
// on its read, is held again by the next listing: the pass ten minutes
// on reads it, and Work gets it from then on, with no request about it
// after. Until then, as nothing about it changes, no pass reads it, and
// Work doesn't get it.
func TestTransferUnlistedRestore(t *testing.T) {
	t.Parallel()
	f, d := transferUnlistedSettled(t)
	transferUnlistedPast(t, f, d)
	var worked []int
	assignWorked(d, &worked)
	f.Advance(transferUnlistedWait)
	transferUnlistedGap(f, 12)
	from := len(f.Requests())
	iterate(t, d)
	if got := assignAbout(f, from, 12); !slices.Equal(got, []string{"GET /issues/12 404"}) || slices.Contains(worked, 12) {
		t.Fatalf("the listing's pass that leaves 12 out: requests about 12 = %q, Work got %v; want the read's 404, which drops 12", got, worked)
	}
	worked, from = nil, len(f.Requests())
	iterate(t, d)
	if got := assignAbout(f, from, 12); len(got) != 0 || slices.Contains(worked, 12) {
		t.Errorf("the pass after the drop: requests about 12 = %q, Work got %v; want none, and 12 left out", got, worked)
	}
	f.Advance(transferUnlistedWait)
	worked, from = nil, len(f.Requests())
	iterate(t, d)
	if got := assignAbout(f, from, 12); !slices.Contains(got, "GET /issues/12 200") || !slices.Contains(worked, 12) {
		t.Errorf("the next listing's pass: requests about 12 = %q, Work got %v; want 12 read, and given to Work", got, worked)
	}
	if n := transferUnlistedListings(f, from); n != 1 {
		t.Errorf("the next listing's pass listed the open issues %d times; want once", n)
	}
	worked, from = nil, len(f.Requests())
	iterate(t, d)
	if got := assignAbout(f, from, 12); len(got) != 0 || !slices.Contains(worked, 12) {
		t.Errorf("the pass after it's back: requests about 12 = %q, Work got %v; want none, and 12 given to Work", got, worked)
	}
}

// An issue this pass's poll listed, as one that changed, is there as far
// as that poll shows, so its absence from the listing in the same pass
// isn't counted as a read that found it gone: a wrong 404 on its read is
// the first of the two, and the next pass's read, which finds it, keeps
// it, though the next poll lists it no more.
func TestTransferUnlistedPolled(t *testing.T) {
	t.Parallel()
	f, d := transferUnlistedSettled(t)
	f.Advance(transferUnlistedWait)
	f.EditBody(12, digestRunOwner, "do it, edited")
	f.EditBody(7, digestRunOwner, "A part, edited.") // the poll's cursor, past 12
	transferUnlistedGap(f, 12)
	var worked []int
	assignWorked(d, &worked)
	from := len(f.Requests())
	iterate(t, d)
	if got := assignAbout(f, from, 12); !slices.Equal(got, []string{"GET /issues/12 404"}) || slices.Contains(worked, 12) {
		t.Fatalf("the listing's pass that leaves 12 out: requests about 12 = %q, Work got %v; want the read's 404, and 12 left out", got, worked)
	}
	worked, from = nil, len(f.Requests())
	iterate(t, d)
	if got := assignAbout(f, from, 12); !slices.Contains(got, "GET /issues/12 200") || !slices.Contains(worked, 12) {
		t.Errorf("the pass after: requests about 12 = %q, Work got %v; want 12 read again, kept, and given to Work", got, worked)
	}
}

// An issue the listing has that changed since the poll, here one opened
// and one reopened while the listing pages, is the next poll's to list
// and judge: the listing's pass reads neither, and the next pass reads
// both.
func TestTransferUnlistedLeftToPoll(t *testing.T) {
	t.Parallel()
	f := quiet(t)
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	f.Advance(transferUnlistedWait)
	opened := 0
	f.BeforeRequest(transferUnlistedListing, func() {
		f.Reopen(4)
		opened = f.AddIssue("owner", "New", "")
	})
	from := len(f.Requests())
	iterate(t, d)
	if n := transferUnlistedListings(f, from); n != 1 || opened == 0 {
		t.Fatalf("the pass ten minutes on listed the open issues %d times, and opened issue %d; want once, and an issue opened", n, opened)
	}
	for _, n := range []int{4, opened} {
		if got := assignAbout(f, from, n); len(got) != 0 {
			t.Errorf("the listing's pass: requests about %d = %q; want none, as it's the next poll's", n, got)
		}
	}
	from = len(f.Requests())
	iterate(t, d)
	for _, n := range []int{4, opened} {
		if !slices.ContainsFunc(f.Requests()[from:], transferUnlistedIssueRead(n)) {
			t.Errorf("the pass after the listing's: requests %+v; want issue %d read, as the poll lists it", f.Requests()[from:], n)
		}
	}
}

// transferUnlistedRestoreFake is transferUnlistedReplayFake with issue
// 3, filed after issue 2's round, so the poll's cursor is past 2's last
// change, and no poll lists 2 again, and the daemon still holds an issue
// once 2 is dropped, so the listing is made.
func transferUnlistedRestoreFake(o *origin) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		t.Helper()
		f := transferUnlistedReplayFake(o)(t)
		f.AddIssue("owner", "Other", "something else")
		return f
	}
}

// transferUnlistedRestorePass is a new daemon's first pass over f, then,
// ten minutes on, the listing's pass that leaves issue 2 out, with a
// wrong 404 on its read, which drops it, and ten minutes after that, the
// listing's pass that brings it back. It returns the passes' errors.
func transferUnlistedRestorePass(t *testing.T, o *origin) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		d := digestDaemon(t, f, o, nil)
		if err := d.Iterate(context.Background()); err != nil {
			return err
		}
		f.Advance(transferUnlistedWait)
		transferUnlistedGap(f, 2)
		if err := d.Iterate(context.Background()); err != nil {
			return err
		}
		f.Advance(transferUnlistedWait)
		return d.Iterate(context.Background())
	}
}

// The listing's pass that brings back a wrongly dropped issue rewrites
// the digest with its item again, as the drop's pass rewrote it without:
// those two writes are the passes' only ones, and both are safe to run
// again after a crash, as a restart's first pass, which reads the issue
// as any other, rewrites the digest with its item, or finds it so.
func TestTransferUnlistedRestoreReplay(t *testing.T) {
	t.Parallel()
	o := digestNotifyOrigin(t)
	f := transferUnlistedRestoreFake(o)(t)
	want := []string{"#2 Question round p2 1"}
	if items := digestRunItems(f.State().Issues[0].Body); !slices.Equal(items, want) {
		t.Fatalf("the digest's items before the passes = %q; want %q", items, want)
	}
	if err := transferUnlistedRestorePass(t, o)(f); err != nil {
		t.Fatal(err)
	}
	if got := digestWrites(f, 0); !slices.Equal(got, []string{"PATCH /issues/1", "PATCH /issues/1"}) {
		t.Fatalf("the passes wrote %q; want the digest's two rewrites, without 2's item and with it", got)
	}
	if items := digestRunItems(f.State().Issues[0].Body); !slices.Equal(items, want) {
		t.Errorf("the digest's items once 2 is back = %q; want %q", items, want)
	}
	githubtest.Replay(t, transferUnlistedRestoreFake(o), transferUnlistedRestorePass(t, o))
}

// The listing's pass that brings back a wrongly dropped issue logs that
// issue alone as one it holds nothing of: not the digest, which the
// listing has too, nor issue 3, which it holds. An edit of 3 after the
// drop takes the poll's cursor past the digest's rewrite.
func TestTransferUnlistedRestoreLogs(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logr.AddWriter).
	o := digestNotifyOrigin(t)
	f := transferUnlistedRestoreFake(o)(t)
	d := digestDaemon(t, f, o, nil)
	iterate(t, d)
	f.Advance(transferUnlistedWait)
	transferUnlistedGap(f, 2)
	iterate(t, d)
	f.EditBody(3, "owner", "something else, edited")
	iterate(t, d)
	f.Advance(transferUnlistedWait)
	var logs bytes.Buffer
	stop := logr.AddWriter(&logs, logr.WithFilter(logr.I))
	defer stop()
	iterate(t, d)
	logr.Wait()
	var got []string
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, "is listed open, but the daemon holds nothing of it") {
			got = append(got, line)
		}
	}
	if len(got) != 1 || !strings.Contains(got[0], "o/r#2 ") {
		t.Errorf("the listing's pass logged %q as held nothing of; want o/r#2 alone", got)
	}
}

// transferUnlistedOnly is a repo whose only open issue, 1, is at ready,
// beside a closed issue filed after it, so the poll's cursor is past 1's
// last change, and no poll lists 1 again. A start's first pass, and the
// pass after it, have settled it: no pass reads it, and Work gets it on
// each one.
func transferUnlistedOnly(t *testing.T) (*githubtest.Fake, *daemon.Daemon) {
	t.Helper()
	f := newFake(t)
	if n := fileAt(f, "ready", "do it", "priority:p2"); n != 1 {
		t.Fatalf("the issue at ready is %d; want 1", n)
	}
	f.Close(f.AddIssue("owner", "Closed", ""))
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	iterate(t, d)
	from := len(f.Requests())
	iterate(t, d)
	if got := assignAbout(f, from, 1); len(got) != 0 {
		t.Fatalf("a settled pass's requests about 1 = %q; want none", got)
	}
	return f, d
}

// #507: a repo whose only held issue is wrongly dropped, by a gap in the
// listing and a wrong 404 on its read, gets it back from the next
// listing, ten minutes on, though the daemon holds no issue then: Work
// gets it from then on.
func TestTransferUnlistedOnlyHeld(t *testing.T) {
	t.Parallel()
	f, d := transferUnlistedOnly(t)
	var worked []int
	assignWorked(d, &worked)
	f.Advance(transferUnlistedWait)
	transferUnlistedGap(f, 1)
	from := len(f.Requests())
	iterate(t, d)
	if got := assignAbout(f, from, 1); !slices.Equal(got, []string{"GET /issues/1 404"}) || slices.Contains(worked, 1) {
		t.Fatalf("the listing's pass that leaves 1 out: requests about 1 = %q, Work got %v; want the read's 404, which drops 1", got, worked)
	}
	worked, from = nil, len(f.Requests())
	iterate(t, d)
	if got, n := assignAbout(f, from, 1), transferUnlistedListings(f, from); len(got) != 0 || n != 0 || len(worked) != 0 {
		t.Errorf("the pass after the drop: requests about 1 = %q, %d listings, Work got %v; want none of them", got, n, worked)
	}
	f.Advance(transferUnlistedWait)
	worked, from = nil, len(f.Requests())
	iterate(t, d)
	if got, n := assignAbout(f, from, 1), transferUnlistedListings(f, from); !slices.Contains(got, "GET /issues/1 200") || n != 1 || !slices.Contains(worked, 1) {
		t.Errorf("the next listing's pass: requests about 1 = %q, %d listings, Work got %v; want one listing, and 1 read, and given to Work", got, n, worked)
	}
}

// #579: a failed listing keeps the issues dropped since the last listing
// that worked. A repo whose only held issue is wrongly dropped, as in
// TestTransferUnlistedOnlyHeld, has its next listing, ten minutes on,
// fail: it holds no issue still, but the listing after, ten minutes from
// the failed one, is made all the same, and brings the issue back. Had
// the failed listing forgotten the drop, no listing would be made again,
// and the issue would never come back.
func TestTransferUnlistedOnlyHeldFailed(t *testing.T) {
	t.Parallel()
	f, d := transferUnlistedOnly(t)
	var worked []int
	assignWorked(d, &worked)
	f.Advance(transferUnlistedWait)
	transferUnlistedGap(f, 1)
	from := len(f.Requests())
	iterate(t, d)
	if got := assignAbout(f, from, 1); !slices.Equal(got, []string{"GET /issues/1 404"}) || slices.Contains(worked, 1) {
		t.Fatalf("the listing's pass that leaves 1 out: requests about 1 = %q, Work got %v; want the read's 404, which drops 1", got, worked)
	}
	f.Advance(transferUnlistedWait)
	f.AnswerOnce(transferUnlistedListing, http.StatusInternalServerError)
	worked, from = nil, len(f.Requests())
	if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "the check for held issues no longer listed") {
		t.Errorf("Iterate with a failed listing = %v; want its error", err)
	}
	if got, n := assignAbout(f, from, 1), transferUnlistedListings(f, from); len(got) != 0 || n != 1 || len(worked) != 0 {
		t.Errorf("the failed listing's pass: requests about 1 = %q, %d listings, Work got %v; want no request about 1, one listing, and nothing for Work", got, n, worked)
	}
	f.Advance(transferUnlistedWait)
	worked, from = nil, len(f.Requests())
	iterate(t, d)
	if got, n := assignAbout(f, from, 1), transferUnlistedListings(f, from); !slices.Contains(got, "GET /issues/1 200") || n != 1 || !slices.Contains(worked, 1) {
		t.Errorf("the listing's pass after the failed one: requests about 1 = %q, %d listings, Work got %v; want one listing, and 1 read, and given to Work",
			got, n, worked)
	}
}

// A repo whose only held issue is transferred away lists the open issues
// once more after the pass that drops it, ten minutes on, though it holds
// no issue then, as a live one wrongly dropped would come back from that
// listing (TestTransferUnlistedOnlyHeld). That listing finds nothing to
// bring back, so the passes after it, ten minutes on and more, list
// nothing, and make no request about the issue.
func TestTransferUnlistedOnlyHeldGone(t *testing.T) {
	t.Parallel()
	f, d := transferUnlistedOnly(t)
	f.Transfer(1, "o/elsewhere")
	f.Advance(transferUnlistedWait)
	from := len(f.Requests())
	iterate(t, d)
	if got := assignAbout(f, from, 1); !slices.Equal(got, []string{"GET /issues/1 301", "GET /repositories/2/issues/1 200"}) {
		t.Fatalf("the listing's pass that leaves 1 out: requests about 1 = %q; want the read that finds it transferred", got)
	}
	for i, listings := range []int{1, 0, 0} {
		f.Advance(transferUnlistedWait)
		from = len(f.Requests())
		iterate(t, d)
		if got, n := assignAbout(f, from, 1), transferUnlistedListings(f, from); len(got) != 0 || n != listings {
			t.Errorf("the pass %d0 minutes after the drop: requests about 1 = %q, %d listings; want none, and %d listings", i+1, got, n, listings)
		}
	}
}

// transferUnlistedOnlyHeldFake is transferUnlistedReplayFake with a
// closed issue, 3, filed after issue 2's round, so the poll's cursor is
// past 2's last change, and no poll lists 2 again. Issue 2 is the only
// one the daemon holds: it holds no snapshot of the digest, nor of a
// closed issue.
func transferUnlistedOnlyHeldFake(o *origin) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		t.Helper()
		f := transferUnlistedReplayFake(o)(t)
		f.Close(f.AddIssue("owner", "Closed", ""))
		return f
	}
}

// The pass that brings back a wrongly dropped issue in a repo whose only
// held issue it is rewrites the digest with its item again, as the drop's
// pass rewrote it without. Both writes are safe to run again after a
// crash, as a restart's first pass reads the issue as any other.
func TestTransferUnlistedOnlyHeldReplay(t *testing.T) {
	t.Parallel()
	o := digestNotifyOrigin(t)
	f := transferUnlistedOnlyHeldFake(o)(t)
	if err := transferUnlistedRestorePass(t, o)(f); err != nil {
		t.Fatal(err)
	}
	if got := digestWrites(f, 0); !slices.Equal(got, []string{"PATCH /issues/1", "PATCH /issues/1"}) {
		t.Fatalf("the passes wrote %q; want the digest's two rewrites, without 2's item and with it", got)
	}
	if want, items := []string{"#2 Question round p2 1"}, digestRunItems(f.State().Issues[0].Body); !slices.Equal(items, want) {
		t.Errorf("the digest's items once 2 is back = %q; want %q", items, want)
	}
	githubtest.Replay(t, transferUnlistedOnlyHeldFake(o), transferUnlistedRestorePass(t, o))
}

// #507: an issue the pass reads again anyway, though its poll didn't list
// it, here one whose comment was edited, which moves no updated_at of the
// issue's, keeps the two reads that drop it: a wrong 404 on its read, on
// the pass whose listing leaves it out, is the first of them, and the
// next pass's read, which finds it, keeps it.
func TestTransferUnlistedStale(t *testing.T) {
	t.Parallel()
	f, d := transferUnlistedSettled(t)
	note := f.AddComment(12, digestRunOwner, "a note")
	transferUnlistedPast(t, f, d)
	f.Advance(transferUnlistedWait)
	f.EditComment(note, digestRunOwner, "a note, edited")
	transferUnlistedGap(f, 12)
	var worked []int
	assignWorked(d, &worked)
	from := len(f.Requests())
	iterate(t, d)
	if got := assignAbout(f, from, 12); !slices.Equal(got, []string{"GET /issues/12 404"}) || slices.Contains(worked, 12) {
		t.Fatalf("the listing's pass that leaves 12 out: requests about 12 = %q, Work got %v; want the read's 404, and 12 left out", got, worked)
	}
	worked, from = nil, len(f.Requests())
	iterate(t, d)
	if got := assignAbout(f, from, 12); !slices.Contains(got, "GET /issues/12 200") || !slices.Contains(worked, 12) {
		t.Errorf("the pass after: requests about 12 = %q, Work got %v; want 12 read again, kept, and given to Work", got, worked)
	}
}

// #507: an issue the pass reads again as a label left out of it fell due
// for removal (SPEC §5.1) keeps the two reads that drop it too, though
// its poll didn't list it: the listing is judged after the issues so
// marked, so a wrong 404 on its read is the first of the two, and the
// next pass's read, which finds it, keeps it, and removes the label.
func TestTransferUnlistedLeftOutDue(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	fileAt(f, "ready", "other")
	fileAt(f, "ready", "do it")
	f.AddLabel(2, "mallory", "foo")
	f.Advance(time.Hour)
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	f.RenameLabel("foo", "scope:x")
	f.AddComment(2, "owner", "poke")
	iterate(t, d) // held back, and first seen
	f.Advance(time.Minute)
	iterate(t, d) // left out
	f.EditBody(1, "owner", "other, edited")
	iterate(t, d) // the poll's cursor, past 2
	f.Advance(transferUnlistedWait)
	transferUnlistedGap(f, 2)
	var worked []int
	assignWorked(d, &worked)
	from := len(f.Requests())
	iterate(t, d)
	if got := assignAbout(f, from, 2); !slices.Equal(got, []string{"GET /issues/2 404"}) || slices.Contains(worked, 2) {
		t.Fatalf("the listing's pass that leaves 2 out, as its label falls due: requests about 2 = %q, Work got %v; want the read's 404, and 2 left out",
			got, worked)
	}
	worked, from = nil, len(f.Requests())
	iterate(t, d)
	if got := assignAbout(f, from, 2); !slices.Contains(got, "GET /issues/2 200") || slices.Contains(f.State().Issues[1].Labels, "scope:x") {
		t.Errorf("the pass after: requests about 2 = %q, labels %q; want 2 read again, kept, and scope:x removed", got, f.State().Issues[1].Labels)
	}
}

// #579: an issue the pass reads again as a login GitHub had no
// permission record of is asked about again once its wait is up
// (discNoRecord) keeps the two reads that drop it too, though its poll
// didn't list it: the listing is judged after the issues so marked, so
// the wrong 404 on its read, on the pass whose listing leaves it out, is
// the first of the two, and the next pass's read, which finds it, keeps
// it. Judged before them, the listing's gap would count as the first, and
// that one 404 would drop it.
func TestTransferUnlistedNoRecordDue(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	fileAt(f, "ready", "other")
	fileAt(f, "ready", "do it")
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	f.RenameIssue(2, "trent", "Renamed") // trent has no permission record
	iterate(t, d)
	f.EditBody(1, "owner", "other, edited")
	iterate(t, d) // the poll's cursor, past 2
	// The wait for trent is up, and the listing due.
	f.Advance(time.Hour + time.Minute)
	transferUnlistedGap(f, 2)
	var worked []int
	assignWorked(d, &worked)
	from := len(f.Requests())
	iterate(t, d)
	if got := assignAbout(f, from, 2); !slices.Equal(got, []string{"GET /issues/2 404"}) || slices.Contains(worked, 2) {
		t.Fatalf("the listing's pass that leaves 2 out, as trent's wait is up: requests about 2 = %q, Work got %v; want the read's 404, and 2 left out",
			got, worked)
	}
	worked, from = nil, len(f.Requests())
	iterate(t, d)
	if got := assignAbout(f, from, 2); !slices.Contains(got, "GET /issues/2 200") || !slices.Contains(worked, 2) {
		t.Errorf("the pass after: requests about 2 = %q, Work got %v; want 2 read again, kept, and given to Work", got, worked)
	}
}

// transferUnlistedTitledFake is an App task, 1, whose body lost its
// origin to the owner's edit and that was renamed issuedog digest, so the
// digest's title rule holds it till its edit history is read, then the
// digest, 2, filed after it, so the poll's cursor is past 1's last change,
// and no poll lists 1 again. The next history query answers 1 NOT_FOUND,
// though it's there.
func transferUnlistedTitledFake(t testing.TB) *githubtest.Fake {
	t.Helper()
	f := newFake(t)
	task := f.AddIssue(app, "A task", digestTaskBody)
	f.EditBody(task, "owner", digestTaskEdited)
	f.RenameIssue(task, "owner", "issuedog digest")
	f.SetPinned(f.AddIssue(app, "issuedog digest", digestList), true)
	f.HistoryNotFoundOnce(task)
	return f
}

// transferUnlistedTitledSteps are the passes of transferUnlistedTitledPass,
// by what each does to issue 1.
var transferUnlistedTitledSteps = []string{
	"the start's pass, whose history query finds 1 deleted, wrongly",
	"the pass whose history query finds 1 deleted again, which lets it go",
	"the listing's pass, ten minutes on, which lists 1 open",
	"the pass after the listing's",
}

// transferUnlistedTitledPass runs transferUnlistedTitledSteps on f with a
// new daemon, each visit step 1 makes added to *visits, if it's set, and
// after each pass, after(i), if it's set. It returns the passes' errors.
func transferUnlistedTitledPass(t *testing.T, o *origin, visits *[]int, after func(i int)) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		d := digestDaemon(t, f, o, visits)
		for i := range transferUnlistedTitledSteps {
			switch i {
			case 1:
				f.HistoryNotFoundOnce(1)
			case 2:
				f.Advance(transferUnlistedWait)
			}
			if err := d.Iterate(context.Background()); err != nil {
				return err
			}
			if after != nil {
				after(i)
			}
		}
		return nil
	}
}

// #507: an issue the digest's title rule held, let go as two answers
// found it deleted, though it's there, is held for the title rule again
// when the next listing lists it open, ten minutes on, as it may be the
// digest: that pass neither reads nor visits it, and the next one reads
// its history, judges it, an App task that lost its origin, no digest,
// and only then reads it and runs step 1 on it. The daemon holds no other
// issue, so the listing is made for the one let go alone.
func TestTransferUnlistedTitled(t *testing.T) {
	t.Parallel()
	f := transferUnlistedTitledFake(t)
	var visits []int
	from, queries := len(f.Requests()), f.HistoryQueries()
	after := func(i int) {
		step := transferUnlistedTitledSteps[i]
		wantQueries, wantVisits, wantListings := 1, []int(nil), 0
		switch i {
		case 2:
			wantQueries, wantListings = 0, 1
		case 3: // the title rule's query, then the visit's, as the owner edited 1's body (Dirty)
			wantQueries, wantVisits = 2, []int{1}
		}
		reads := 0
		for _, r := range f.Requests()[from:] {
			if transferUnlistedIssueRead(1)(r) {
				reads++
			}
		}
		if got := f.HistoryQueries() - queries; got != wantQueries || !slices.Equal(visits, wantVisits) || transferUnlistedListings(f, from) != wantListings ||
			(reads != 0) != (i == 3) {
			t.Errorf("%s: %d history queries, step 1 visited %v, %d listings, 1 read %d times; want %d, %v, %d, and 1 read only once judged",
				step, got, visits, transferUnlistedListings(f, from), reads, wantQueries, wantVisits, wantListings)
		}
		from, queries = len(f.Requests()), f.HistoryQueries()
	}
	if err := transferUnlistedTitledPass(t, newOrigin(t), &visits, after)(f); err != nil {
		t.Fatal(err)
	}
	if is := f.State().Issues; len(is) != 2 || is[0].Title != "issuedog digest" || is[1].Body != digestList {
		t.Errorf("issues = %+v; want the task and the one digest, as they were", is)
	}
}

// The passes of TestTransferUnlistedTitled are safe to run again after a
// crash at any write: those of the task's visit, once it's judged no
// digest. A restart reads the task's history at once, and judges it so.
func TestTransferUnlistedTitledReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	githubtest.Replay(t, transferUnlistedTitledFake, transferUnlistedTitledPass(t, o, nil, nil))
}
