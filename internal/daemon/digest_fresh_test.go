package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// #581: a digest item's count of the open issues its issue blocks, and its
// issue's created time, stay fresh. The 10-minute listing of every open
// issue gives each issue's issue_dependencies_summary, so a blocker's
// count catches up by the next listing, though a link's write or a
// blocked issue's close moves no updated_at of the blocker's, with no
// request of its own (SPEC §8.1, §9.4 The order). And a kept item, read
// back from the digest's body after a restart, ranks by what the last
// listing of its issue showed, while its read fails (SPEC §9.4, After a
// restart).

// The issues of digestFreshRepo's repo, by number.
const (
	digestFreshOlder    = 2
	digestFreshNewer    = 3
	digestFreshBlockedA = 4
	digestFreshBlockedB = 5
)

// digestFreshRepo builds a repo whose issue 1 is the pinned digest, never
// edited, as the App filed it, with the body and comments a pass over the
// rest leaves it with, copied from such a pass over the same repo, so a new
// daemon's first pass over it writes nothing. The rest are 2, Older, and
// 3, Newer, each at ready with a state error at p2, so they tie but for
// their age and what they block, and 4 and 5, Blocked A and Blocked B, with
// no items. With linked, 3 blocks 4.
func digestFreshRepo(o *origin, linked bool) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		t.Helper()
		fill := func(f *githubtest.Fake) {
			digestError(f, "Older", "5")
			digestError(f, "Newer", "6")
			f.AddIssue("owner", "Blocked A", "The first one blocked.")
			f.AddIssue("owner", "Blocked B", "The second one blocked.")
			if linked {
				f.SetBlockedBy(digestFreshBlockedA, digestFreshNewer)
			}
		}
		src := digestPinned(t)
		fill(src)
		digestKeptSettle(t, src, digestDaemon(t, src, o, nil))
		digest := src.State().Issues[0]
		f := newFake(t)
		f.SetPinned(f.AddIssue(app, "issuedog digest", digest.Body), true)
		fill(f)
		for _, c := range digest.Comments {
			f.AddComment(1, c.Author, c.Body)
		}
		return f
	}
}

// digestFreshItems is the items in the digest's body in f, as
// digestRunItems gives them.
func digestFreshItems(f *githubtest.Fake) []string { return digestRunItems(f.State().Issues[0].Body) }

// digestFreshReadFails makes the next GET of issue n answer 500, which
// fails its read.
func digestFreshReadFails(f *githubtest.Fake, n int) {
	read := fmt.Sprintf("/issues/%d", n)
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == read }, http.StatusInternalServerError)
}

// digestFreshNoReads fails t if a request from the from'th on is the GET
// of one of the repo's issues but the digest, a snapshot's first read, or
// a dependency listing: the count comes from the listing alone. The
// conditional reads step 1 makes of an issue with an open state error, on
// every pass, are no read of the issue.
func digestFreshNoReads(t *testing.T, f *githubtest.Fake, step string, from int) {
	t.Helper()
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet {
			continue
		}
		for n := digestFreshOlder; n <= digestFreshBlockedB; n++ {
			if r.Path == fmt.Sprintf("/issues/%d", n) {
				t.Errorf("%s: sent GET %s; want no read of #%d", step, r.Path, n)
			}
		}
		if strings.Contains(r.Path, "/dependencies/") {
			t.Errorf("%s: sent GET %s; want no dependency listing", step, r.Path)
		}
	}
}

// A blocker's count catches up by the next 10-minute listing, with no
// request of its own. #3 is linked to block #4 and #5, which moves no
// updated_at of #3's, so no poll lists it, and a pass before the listing
// leaves the digest as it was. The listing's pass ranks #3 first, as it
// blocks two open issues, and writes the digest once, reading no issue and
// no dependency listing. #4's close moves no updated_at of #3's either, so
// the next listing's pass takes #3's count down to one. Once the polls
// and a listing have caught up with that write, a listing's pass with
// nothing changed makes no request but conditional GETs answered 304, and
// writes nothing.
func TestDigestFreshListedBlocking(t *testing.T) {
	t.Parallel()
	o := digestNotifyOrigin(t)
	f := digestFreshRepo(o, false)(t)
	d := digestDaemon(t, f, o, nil)
	iterate(t, d)
	if got := digestWrites(f, 0); len(got) != 0 {
		t.Fatalf("the first pass wrote %q; want nothing, as the digest lists what it would", got)
	}
	step := func(name string, writes []string, items ...string) {
		t.Helper()
		from, written := len(f.Requests()), len(f.Writes())
		iterate(t, d)
		if got := digestWrites(f, written); !slices.Equal(got, writes) {
			t.Errorf("%s: wrote %q; want %q", name, got, writes)
		}
		if got := digestFreshItems(f); !slices.Equal(got, items) {
			t.Errorf("%s: the digest's items %q; want %q", name, got, items)
		}
		digestFreshNoReads(t, f, name, from)
	}
	older := fmt.Sprintf("#%d State error p2 1", digestFreshOlder)
	step("a pass before the links", nil, older, fmt.Sprintf("#%d State error p2 1", digestFreshNewer))
	f.SetBlockedBy(digestFreshBlockedA, digestFreshNewer)
	f.SetBlockedBy(digestFreshBlockedB, digestFreshNewer)
	step("a pass before ten minutes", nil, older, fmt.Sprintf("#%d State error p2 1", digestFreshNewer))
	f.Advance(transferUnlistedWait)
	step("the listing's pass", []string{"PATCH /issues/1"}, fmt.Sprintf("#%d State error p2 3", digestFreshNewer), older)
	f.Close(digestFreshBlockedA)
	step("the pass after #4's close", nil, fmt.Sprintf("#%d State error p2 3", digestFreshNewer), older)
	f.Advance(transferUnlistedWait)
	step("the next listing's pass", []string{"PATCH /issues/1"}, fmt.Sprintf("#%d State error p2 2", digestFreshNewer), older)
	step("the pass after", nil, fmt.Sprintf("#%d State error p2 2", digestFreshNewer), older)
	f.Advance(transferUnlistedWait)
	step("the listing's pass after", nil, fmt.Sprintf("#%d State error p2 2", digestFreshNewer), older)
	f.Advance(transferUnlistedWait)
	from := len(f.Requests())
	step("an unchanged listing's pass", nil, fmt.Sprintf("#%d State error p2 2", digestFreshNewer), older)
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("an unchanged listing's pass: sent %+v; want only conditional GETs answered 304", r)
		}
	}
}

// The listing's pass, whose only write is the digest's, ranking #3 by the
// count the listing gives, is safe to run again after a crash at that
// write: a restart's first pass reads #3, with the same count, and writes
// the digest the same way, or finds it written.
func TestDigestFreshListedReplay(t *testing.T) {
	t.Parallel()
	o := digestNotifyOrigin(t)
	pass := func(f *githubtest.Fake) error {
		d := digestDaemon(t, f, o, nil)
		if err := d.Iterate(context.Background()); err != nil {
			return err
		}
		f.SetBlockedBy(digestFreshBlockedA, digestFreshNewer) // a link it has already changes nothing
		f.Advance(transferUnlistedWait)
		return d.Iterate(context.Background())
	}
	f := digestFreshRepo(o, false)(t)
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	if got := digestWrites(f, 0); !slices.Equal(got, []string{"PATCH /issues/1"}) {
		t.Fatalf("the passes wrote %q; want the digest's rewrite alone", got)
	}
	githubtest.Replay(t, digestFreshRepo(o, false), pass)
}

// A restart over a digest the App never edited, whose first pass fails the
// read of #3, keeps #3's item from the body, ranked where it was, so the
// pass writes nothing: its count of the open issues #3 blocks, and #3's
// created time, come from the start's first poll, which lists every issue.
// With #3 blocking #4, it stays first, unblocking two; with no link, it
// stays after #2, the older. The next pass, whose read of #3 works, writes
// nothing either. While #3's read keeps failing, the 10-minute listing
// brings its count up to date: #5 linked to it too ranks it unblocking
// three, and the read that works then finds the same.
func TestDigestFreshNeverEdited(t *testing.T) {
	t.Parallel()
	o := digestNotifyOrigin(t)
	older := fmt.Sprintf("#%d State error p2 1", digestFreshOlder)
	for _, tc := range []struct {
		name   string
		linked bool
		items  []string
	}{
		{"blocking", true, []string{fmt.Sprintf("#%d State error p2 2", digestFreshNewer), older}},
		{"created", false, []string{older, fmt.Sprintf("#%d State error p2 1", digestFreshNewer)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := digestFreshRepo(o, tc.linked)(t)
			before := f.State().Issues[0].Body
			if got := digestFreshItems(f); !slices.Equal(got, tc.items) {
				t.Fatalf("the digest's items %q; want %q", got, tc.items)
			}
			d := digestDaemon(t, f, o, nil)
			failed := func(name string) {
				t.Helper()
				digestFreshReadFails(f, digestFreshNewer)
				if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("o/r#%d", digestFreshNewer)) {
					t.Fatalf("%s: Iterate = %v; want #%d's failure", name, err, digestFreshNewer)
				}
			}
			failed("the restart's first pass")
			if got := digestWrites(f, 0); len(got) != 0 {
				t.Errorf("the restart's first pass wrote %q; want nothing", got)
			}
			if got := f.State().Issues[0].Body; got != before {
				t.Errorf("digest\n%s\nwant it as before the restart\n%s", got, before)
			}
			if !tc.linked {
				iterate(t, d)
				if got := digestWrites(f, 0); len(got) != 0 {
					t.Errorf("the restart's second pass wrote %q; want nothing", got)
				}
				return
			}
			f.SetBlockedBy(digestFreshBlockedB, digestFreshNewer)
			f.Advance(transferUnlistedWait)
			failed("the listing's pass")
			want := []string{fmt.Sprintf("#%d State error p2 3", digestFreshNewer), older}
			if got, items := digestWrites(f, 0), digestFreshItems(f); !slices.Equal(got, []string{"PATCH /issues/1"}) || !slices.Equal(items, want) {
				t.Errorf("the listing's pass wrote %q, leaving the digest's items %q; want the digest's rewrite, to %q", got, items, want)
			}
			written := len(f.Writes())
			iterate(t, d)
			if got := digestWrites(f, written); len(got) != 0 {
				t.Errorf("the pass whose read of #%d works wrote %q; want nothing", digestFreshNewer, got)
			}
		})
	}
}
