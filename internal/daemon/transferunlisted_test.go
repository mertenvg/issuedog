package daemon_test

import (
	"context"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// Held issues no longer in the repo (#237): a settled issue, which no
// pass visits, transferred or deleted, drops out of the repo's listings
// and changes nothing there. Once every ten minutes, by GitHub's clock,
// the daemon lists every open issue and reads again, once, each issue it
// holds that the listing leaves out, which drops it if it's gone.

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
// on: Work gets its issues. The next pass lists again, at once, and drops
// the transferred issue.
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
	from = len(f.Requests())
	assignGonePasses(t, f, d, assignGoneRead["transferred"], nil)
	if n := transferUnlistedListings(f, from); n != 1 {
		t.Errorf("the passes after the failure listed the open issues %d times; want once, at once", n)
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
