package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// The digest's rewrite and its announcements (plan #7, T8, #160).

// digestNotifyOrigin is newOrigin with notify set to owner, on main.
func digestNotifyOrigin(t testing.TB) *origin {
	t.Helper()
	o := newOrigin(t)
	o.commit(config.Path, "types: labels\nnotify: [owner]\nloop:\n  idle_sleep: 7s\n")
	return o
}

// digestPinned is a fake whose issue 1 is the pinned digest, with nothing
// listed.
func digestPinned(t testing.TB) *githubtest.Fake {
	f := newFake(t)
	f.SetPinned(f.AddIssue(app, "issuedog digest", digestList), true)
	return f
}

// digestError files an issue at ready, as the App set it, with an open
// state error keyed key, a disagreement, which only a later transition or
// an ack ends, and returns its number.
func digestError(f *githubtest.Fake, title, key string) int {
	n := f.AddIssue("owner", title, "do it")
	f.AddLabel(n, app, "stage:ready")
	f.AddComment(n, app, "<!-- issuedog:transition from=intake to=ready run=rready -->")
	digestErrorOn(f, n, key)
	return n
}

// digestErrorOn posts an open disagreement keyed key on issue n.
func digestErrorOn(f *githubtest.Fake, n int, key string) {
	f.AddComment(n, app, "The stage label disagreed with the marker, so I put it back.\n\n<!-- issuedog:error kind=disagreement key="+key+" -->")
}

// digestRound files an issue at clarifying with the App's round 1 of one
// question, unanswered, and returns its number.
func digestRound(f *githubtest.Fake) int {
	n := f.AddIssue("owner", "Rounds", "an idea")
	f.AddLabel(n, app, "stage:clarifying")
	f.AddComment(n, app, "<!-- issuedog:transition from=intake to=clarifying run=rc -->")
	f.AddComment(n, app, fmt.Sprintf("### Questions for #%d (round 1)\n\n**1. Where?** Blocks: #%d\n   a. Here (recommended): close\n   b. There\n\n"+
		"Reply in one comment, e.g. `1a 2c 3: free text`.\n\ncc @owner\n\n"+
		"<!-- issuedog:transition from=clarifying to=clarifying run=r1 round=1 questions=1 -->", n, n))
	return n
}

// digestComments returns the bodies of the comments on issue n.
func digestComments(f *githubtest.Fake, n int) []string {
	var bodies []string
	for _, c := range f.State().Issues[n-1].Comments {
		bodies = append(bodies, c.Body)
	}
	return bodies
}

// digestPass iterates d and returns the writes it made, as "METHOD /path".
func digestPass(t *testing.T, f *githubtest.Fake, d *daemon.Daemon) []string {
	t.Helper()
	from := len(f.Writes())
	iterate(t, d)
	return digestWrites(f, from)
}

// A new state error gives one comment that announces it, with the cc line
// and its key, and a body edit that lists it. Repeating the pass, or a new
// daemon's first pass, writes nothing.
func TestDigestRewriteNewError(t *testing.T) {
	f, o := digestPinned(t), digestNotifyOrigin(t)
	n := digestError(f, "Broken [x](https://evil.example)", "77")
	d := digestDaemon(t, f, o, nil)
	if got, want := digestPass(t, f, d), []string{"POST /issues/1/comments", "PATCH /issues/1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("writes = %q, want %q", got, want)
	}
	comments := digestComments(f, 1)
	key := fmt.Sprintf("err-%d-disagreement-77", n)
	if len(comments) != 1 || !strings.Contains(comments[0], "\n\ncc @owner\n\n") || !strings.HasSuffix(comments[0], "<!-- issuedog:digest-new items="+key+" -->") {
		t.Errorf("comments = %q; want one announcement of %s, with the cc line", comments, key)
	}
	body := f.State().Issues[0].Body
	if !strings.Contains(body, fmt.Sprintf("### 1. #%d: Broken \\[x\\](https://evil.example)\n", n)) || strings.Contains(body, "@owner") ||
		!strings.HasSuffix(body, "<!-- issuedog:digest -->") || !f.State().Issues[0].Pinned {
		t.Errorf("body = %q; want the error listed, its title escaped, no mention, and the marker", body)
	}
	if got := digestPass(t, f, d); len(got) != 0 {
		t.Errorf("the next pass wrote %q; want nothing", got)
	}
	if got := digestPass(t, f, digestDaemon(t, f, o, nil)); len(got) != 0 {
		t.Errorf("a new daemon wrote %q; want nothing", got)
	}
}

// An answered round gives a body edit and no comment. So does a new round,
// as the round's own comment mentions the owner already (Q2a), and it's
// never announced however many passes it stays new.
func TestDigestRewriteRound(t *testing.T) {
	f := digestPinned(t)
	n := digestRound(f)
	d := digestDaemon(t, f, digestNotifyOrigin(t), nil)
	if got := digestPass(t, f, d); !reflect.DeepEqual(got, []string{"PATCH /issues/1"}) {
		t.Fatalf("writes = %q; want the body edit alone", got)
	}
	if body := f.State().Issues[0].Body; !strings.Contains(body, "**Question round**") || !strings.Contains(body, "**Question 1:** Where?") {
		t.Errorf("body = %q; want the round listed", body)
	}
	for range 2 {
		if got := digestPass(t, f, d); len(got) != 0 {
			t.Errorf("a pass with the round still new wrote %q; want nothing", got)
		}
	}
	f.AddComment(n, "owner", "1a")
	// Step 1 takes the answer, with its own 👍; the digest only edits.
	if got := digestPass(t, f, d); !slices.Contains(got, "PATCH /issues/1") || slices.Contains(got, "POST /issues/1/comments") {
		t.Errorf("writes on the answer = %q; want the body edit, and no comment", got)
	}
	if body := f.State().Issues[0].Body; body != digestList || len(f.State().Issues[0].Comments) != 0 {
		t.Errorf("body %q, comments %+v; want nothing listed, and no comment", body, f.State().Issues[0].Comments)
	}
}

// Two new items in one pass give one comment that announces both (Q1a).
// A round new in the same pass is listed, and not announced (Q2a).
func TestDigestRewriteTwoNew(t *testing.T) {
	f := digestPinned(t)
	a, b := digestError(f, "A", "5"), digestError(f, "B", "6")
	digestRound(f)
	if got := digestPass(t, f, digestDaemon(t, f, digestNotifyOrigin(t), nil)); !reflect.DeepEqual(got, []string{"POST /issues/1/comments", "PATCH /issues/1"}) {
		t.Fatalf("writes = %q; want one comment and one edit", got)
	}
	comments := digestComments(f, 1)
	want := fmt.Sprintf("<!-- issuedog:digest-new items=err-%d-disagreement-5,err-%d-disagreement-6 -->", a, b)
	if len(comments) != 1 || !strings.HasSuffix(comments[0], want) || strings.Contains(comments[0], "Question round") || strings.Count(comments[0], "@owner") != 1 {
		t.Errorf("comments = %q; want one, ending %q, with one mention and no round", comments, want)
	}
	if body := f.State().Issues[0].Body; !strings.HasPrefix(body, "3 items wait on you") {
		t.Errorf("body = %q; want all three listed", body)
	}
}

// An edited digest-new comment counts for nothing, whoever edited it, so
// its items are announced again, once. A later item gets a comment of its
// own, which names it alone.
func TestDigestRewriteEditedAnnouncement(t *testing.T) {
	f, o := digestPinned(t), digestNotifyOrigin(t)
	n := digestError(f, "A", "5")
	d := digestDaemon(t, f, o, nil)
	iterate(t, d)
	comments, err := f.Client(t).CommentsSince(context.Background(), "o", "r", 1, time.Time{})
	if err != nil || len(comments) != 1 {
		t.Fatalf("the digest's comments = %+v, %v; want the announcement", comments, err)
	}
	f.EditComment(comments[0].GetID(), "mallory", "Gone quiet. <!-- issuedog:digest-new items=err-2-disagreement-5,err-9-x-1 -->")
	if got := digestPass(t, f, d); !reflect.DeepEqual(got, []string{"POST /issues/1/comments"}) {
		t.Fatalf("writes after the edit = %q; want a new announcement, and no edit", got)
	}
	if got := digestPass(t, f, d); len(got) != 0 {
		t.Errorf("the next pass wrote %q; want nothing", got)
	}
	digestErrorOn(f, n, "6")
	if got := digestPass(t, f, d); !reflect.DeepEqual(got, []string{"POST /issues/1/comments", "PATCH /issues/1"}) {
		t.Fatalf("writes on the second error = %q; want a comment and an edit", got)
	}
	bodies := digestComments(f, 1)
	if want := fmt.Sprintf("items=err-%d-disagreement-6 -->", n); len(bodies) != 3 || !strings.HasSuffix(bodies[2], want) {
		t.Errorf("comments = %q; want the last to announce the new error alone", bodies)
	}
}

// With notify empty, no comment is posted, and the body is edited.
func TestDigestRewriteNoNotify(t *testing.T) {
	f := digestPinned(t)
	digestError(f, "A", "5")
	d := digestDaemon(t, f, newOrigin(t), nil)
	if got := digestPass(t, f, d); !reflect.DeepEqual(got, []string{"PATCH /issues/1"}) {
		t.Errorf("writes = %q; want the body edit alone", got)
	}
	if got := digestPass(t, f, d); len(got) != 0 {
		t.Errorf("the next pass wrote %q; want nothing", got)
	}
}

// A failed comment or body edit is the pass's error, and Work runs anyway.
// The body is edited after a failed comment, and the next pass posts it.
func TestDigestRewriteWriteFails(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fail      int
		err       string
		next      []string
		writesNow []string
	}{
		{"the comment", 1, "announce new items on the digest, o/r#1: ", []string{"POST /issues/1/comments"},
			[]string{"POST /issues/1/comments", "PATCH /issues/1"}},
		{"the edit", 2, "edit the body of the digest, o/r#1: ", []string{"PATCH /issues/1"},
			[]string{"POST /issues/1/comments", "PATCH /issues/1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := digestPinned(t)
			digestError(f, "A", "5")
			d := digestDaemon(t, f, digestNotifyOrigin(t), nil)
			worked := 0
			d.Work = func(context.Context, []daemon.Ready) error { worked++; return nil }
			f.FailWrite(tc.fail)
			if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), tc.err) || worked != 1 {
				t.Fatalf("Iterate = %v, Work ran %d times; want %q, and Work run", err, worked, tc.err)
			}
			if got := digestWrites(f, 0); !reflect.DeepEqual(got, tc.writesNow) {
				t.Errorf("writes = %q; want %q", got, tc.writesNow)
			}
			f.FailWrite(0)
			if got := digestPass(t, f, d); !reflect.DeepEqual(got, tc.next) {
				t.Errorf("the next pass wrote %q; want %q", got, tc.next)
			}
			if comments := digestComments(f, 1); len(comments) != 1 || worked != 2 {
				t.Errorf("comments = %q, Work ran %d times; want one announcement, and Work run each pass", comments, worked)
			}
		})
	}
}

// Replay passes with one new item: its comment and the body edit, a crash
// after the comment and before the edit included, which the next pass
// finds without announcing it again.
func TestDigestRewriteReplay(t *testing.T) {
	o := digestNotifyOrigin(t)
	githubtest.Replay(t, func(t testing.TB) *githubtest.Fake {
		f := digestPinned(t)
		digestError(f, "A", "5")
		return f
	}, func(f *githubtest.Fake) error { return digestDaemon(t, f, o, nil).Iterate(context.Background()) })
}

// Replay passes for a digest created, pinned and announcing at once.
func TestDigestRewriteCreateReplay(t *testing.T) {
	o := digestNotifyOrigin(t)
	githubtest.Replay(t, func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		digestError(f, "A", "5")
		return f
	}, func(f *githubtest.Fake) error { return digestDaemon(t, f, o, nil).Iterate(context.Background()) })
}

// A new digest is created with its items listed, and the pin's edit keeps
// them: no edit follows to put the list in.
func TestDigestRewriteCreate(t *testing.T) {
	f := newFake(t)
	digestError(f, "A", "5")
	if got, want := digestPass(t, f, digestDaemon(t, f, digestNotifyOrigin(t), nil)),
		[]string{"POST /issues", "POST /graphql", "PATCH /issues/2", "POST /issues/2/comments"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("writes = %q; want %q", got, want)
	}
	if created := f.Writes()[0].Body; !strings.Contains(created, "1 item waits on you.") {
		t.Errorf("the create's body = %s; want the item listed", created)
	}
	if body := f.State().Issues[1].Body; !strings.HasPrefix(body, "1 item waits on you.") {
		t.Errorf("body = %q; want the item listed, with no pin note", body)
	}
}

// digestRequestSet is the requests from the from'th on, as
// "METHOD path conditional", sorted. The query is left out: the cursors
// in it differ with the digest's writes.
func digestRequestSet(f *githubtest.Fake, from int) []string {
	var got []string
	for _, r := range f.Requests()[from:] {
		got = append(got, fmt.Sprintf("%s %s %t", r.Method, r.Path, r.Conditional))
	}
	slices.Sort(got)
	return got
}

// An unchanged pass with items listed makes no digest write, and no request
// a daemon without the digest doesn't make too: the digest adds no reads.
func TestDigestRewriteUnchangedPass(t *testing.T) {
	o := digestNotifyOrigin(t)
	var passes [2][]string
	for i, keep := range []bool{false, true} {
		f := digestPinned(t)
		digestError(f, "A", "5")
		digestRound(f)
		d := digestDaemon(t, f, o, nil)
		d.Digest = keep
		iterate(t, d)
		iterate(t, d)
		from, reqs := len(f.Writes()), len(f.Requests())
		iterate(t, d)
		if w := digestWrites(f, from); len(w) != 0 {
			t.Errorf("digest %t: the unchanged pass wrote %q; want nothing", keep, w)
		}
		if got := digestRequests(f, reqs); len(got) != 0 {
			t.Errorf("digest %t: the unchanged pass sent %+v about the digest; want none", keep, got)
		}
		passes[i] = digestRequestSet(f, reqs)
	}
	if !slices.Equal(passes[0], passes[1]) {
		t.Errorf("the unchanged pass sent\n%q\nwith the digest, and\n%q\nwithout; want the same", passes[1], passes[0])
	}
}

// Once the digest was closed and reopened, the App's own writes to it don't
// make the next pass read its timeline again (#159's verify); someone
// else's close still does, and is reverted.
func TestDigestRewriteNoTimelineAfterOwnWrites(t *testing.T) {
	f := digestPinned(t)
	n := digestError(f, "A", "5")
	d := digestDaemon(t, f, digestNotifyOrigin(t), nil)
	f.CloseBy(1, "owner")
	iterate(t, d)
	f.ReopenBy(1, "owner")
	if got := digestPass(t, f, d); !reflect.DeepEqual(got, []string{"POST /issues/1/comments", "PATCH /issues/1"}) {
		t.Fatalf("writes after the owner's reopen = %q; want the announcement and the edit", got)
	}
	timeline := func(from int) int {
		reads := 0
		for _, r := range f.Requests()[from:] {
			if r.Path == "/issues/1/timeline" {
				reads++
			}
		}
		return reads
	}
	for range 2 {
		reqs := len(f.Requests())
		digestErrorOn(f, n, fmt.Sprint(reqs)) // a new item each pass, so the digest is written each pass
		if got := digestPass(t, f, d); !reflect.DeepEqual(got, []string{"POST /issues/1/comments", "PATCH /issues/1"}) {
			t.Errorf("writes = %q; want an announcement and an edit", got)
		}
		if got := timeline(reqs); got != 0 {
			t.Errorf("the pass after the App's writes read the digest's timeline %d times; want none", got)
		}
	}
	reqs := len(f.Requests())
	iterate(t, d)
	if got := digestRequests(f, reqs); len(got) != 0 {
		t.Errorf("the pass after the last writes sent %+v about the digest; want none", got)
	}
	// A pass that only comments, as when an edit undid an announcement,
	// needs no read after it either.
	comments, err := f.Client(t).CommentsSince(context.Background(), "o", "r", 1, time.Time{})
	if err != nil || len(comments) == 0 {
		t.Fatalf("the digest's comments = %+v, %v; want the announcements", comments, err)
	}
	f.EditComment(comments[0].GetID(), "mallory", "Gone quiet.")
	if got := digestPass(t, f, d); !reflect.DeepEqual(got, []string{"POST /issues/1/comments"}) {
		t.Errorf("writes after the edit = %q; want the announcement alone", got)
	}
	reqs = len(f.Requests())
	iterate(t, d)
	if got := timeline(reqs); got != 0 {
		t.Errorf("the pass after the App's comment read the digest's timeline %d times; want none", got)
	}
	f.CloseBy(1, "mallory")
	if got := digestPass(t, f, d); !reflect.DeepEqual(got, []string{"PATCH /issues/1"}) || f.State().Issues[0].Closed {
		t.Errorf("writes on mallory's close = %q; want the reopen", got)
	}
}

// A digest deleted while the daemon runs fails the write with a 404, and is
// dropped. The next pass lists every issue again and, finding no digest,
// creates one, which lists and announces the items.
func TestDigestRewriteDeleted(t *testing.T) {
	f := newFake(t)
	n := digestError(f, "A", "5")
	f.AddIssue(app, "issuedog digest", digestPending) // its pin note makes this start check its pin
	d := digestDaemon(t, f, digestNotifyOrigin(t), nil)
	iterate(t, d)
	if !f.State().Issues[1].Pinned {
		t.Fatalf("issue %+v; want the digest pinned", f.State().Issues[1])
	}
	f.DeleteIssue() // the digest, issue 2, the last
	digestErrorOn(f, n, "6")
	err := d.Iterate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "the digest is gone, deleted or transferred") {
		t.Fatalf("Iterate = %v; want the digest gone", err)
	}
	// The fake numbers an issue by count, so this takes 2: the new digest
	// gets 3, as GitHub, which never reuses a number, would give it a new one.
	f.AddIssue("owner", "Filler", "")
	from, reqs := len(f.Writes()), len(f.Requests())
	iterate(t, d)
	if got, want := digestWrites(f, from), []string{"POST /issues", "POST /graphql", "PATCH /issues/3", "POST /issues/3/comments"}; !reflect.DeepEqual(got, want) {
		t.Errorf("writes after the drop = %q; want %q", got, want)
	}
	listed := false
	for _, r := range f.Requests()[reqs:] {
		listed = listed || r.Path == "/issues" && r.Method == http.MethodGet && !strings.Contains(r.Query, "since=")
	}
	if !listed {
		t.Errorf("the pass after the drop didn't list every issue again: %+v", f.Requests()[reqs:])
	}
	is := f.State().Issues
	if len(is) != 3 || is[2].Title != "issuedog digest" || !is[2].Pinned || !strings.HasPrefix(is[2].Body, "2 items wait on you") || len(is[2].Comments) != 1 {
		t.Fatalf("issues = %+v; want a new pinned digest, 3, listing both errors, with one announcement", is)
	}
	// The old digest's announcements are on no other number: the new one
	// announces both.
	if want := fmt.Sprintf("items=err-%d-disagreement-5,err-%d-disagreement-6 -->", n, n); !strings.HasSuffix(is[2].Comments[0].Body, want) {
		t.Errorf("the announcement = %q; want it to end %q", is[2].Comments[0].Body, want)
	}
	if got := digestPass(t, f, d); len(got) != 0 {
		t.Errorf("the next pass wrote %q; want nothing", got)
	}
}

// A digest a transient 404 dropped, on its comment, its body edit or the
// read of its timeline, is found again by the full listing, and no second
// digest is created.
func TestDigestRewriteDroppedFoundAgain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		notify bool
		before func(t *testing.T, f *githubtest.Fake, d *daemon.Daemon)
		method string
		path   string
		next   []string
	}{
		{"the comment", true, nil, http.MethodPost, "/issues/1/comments", []string{"POST /issues/1/comments", "PATCH /issues/1"}},
		{"the edit", false, nil, http.MethodPatch, "/issues/1", []string{"PATCH /issues/1"}},
		{"the timeline", false, func(t *testing.T, f *githubtest.Fake, d *daemon.Daemon) {
			iterate(t, d)
			f.CloseBy(1, "owner")
			iterate(t, d)
			f.ReopenBy(1, "owner")
		}, http.MethodGet, "/issues/1/timeline", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := digestPinned(t)
			digestError(f, "A", "5")
			o := newOrigin(t)
			if tc.notify {
				o = digestNotifyOrigin(t)
			}
			d := digestDaemon(t, f, o, nil)
			if tc.before != nil {
				tc.before(t, f, d)
			}
			f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == tc.method && r.Path == tc.path }, http.StatusNotFound)
			if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "the digest is gone") {
				t.Fatalf("Iterate = %v; want the digest gone", err)
			}
			if got := digestPass(t, f, d); !reflect.DeepEqual(got, tc.next) || len(f.State().Issues) != 2 {
				t.Errorf("writes = %q, issues %+v; want %q on digest 1, found again, and nothing created", got, f.State().Issues, tc.next)
			}
		})
	}
}

// A digest a one-off 404 dropped, after it announced an item, keeps its
// announcement once it's found again, though the poll never lists that
// comment again: the item isn't announced twice (#160's verify).
func TestDigestRewriteRefoundKeepsAnnouncements(t *testing.T) {
	f := digestPinned(t)
	digestError(f, "A", "5")
	d := digestDaemon(t, f, digestNotifyOrigin(t), nil)
	if got := digestPass(t, f, d); !reflect.DeepEqual(got, []string{"POST /issues/1/comments", "PATCH /issues/1"}) {
		t.Fatalf("writes = %q; want the announcement and the edit", got)
	}
	digestRound(f) // the body changes, with no announcement
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodPatch && r.Path == "/issues/1" }, http.StatusNotFound)
	if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "the digest is gone") {
		t.Fatalf("Iterate = %v; want the digest gone", err)
	}
	if got := digestPass(t, f, d); !reflect.DeepEqual(got, []string{"PATCH /issues/1"}) {
		t.Errorf("writes once found again = %q; want the edit alone", got)
	}
	news := 0
	for _, c := range digestComments(f, 1) {
		if strings.Contains(c, "<!-- issuedog:digest-new ") {
			news++
		}
	}
	if is := f.State().Issues; news != 1 || len(is) != 3 || !strings.HasPrefix(is[0].Body, "2 items wait on you") {
		t.Errorf("%d digest-new comments, issues %+v; want one, no new digest, and both items listed", news, is)
	}
}

// A transferred digest, whose edit GitHub answers from the other repo, is
// gone, not written: the edit's error says so (#160's verify).
func TestDigestRewriteTransferred(t *testing.T) {
	f := digestPinned(t)
	d := digestDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	f.Transfer(1, "elsewhere/r")
	digestError(f, "A", "5")
	err := d.Iterate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "the digest is gone") || !strings.Contains(err.Error(), "transferred to ") {
		t.Errorf("Iterate = %v; want the digest gone, transferred", err)
	}
}

// The digest lists items in Q3a's order, which waitRank gives: an issue
// with an open sub-issue first, though it's newer, with its count.
func TestDigestRewriteRanked(t *testing.T) {
	f := digestPinned(t)
	older := digestError(f, "Older", "5")
	newer := digestError(f, "Newer", "6")
	child := f.AddIssue("owner", "Child", "")
	f.SetParent(child, newer)
	iterate(t, digestDaemon(t, f, newOrigin(t), nil))
	body := f.State().Issues[0].Body
	first := fmt.Sprintf("### 1. #%d: Newer\n\n**State error**, priority p2, unblocks 2 open issues (itself included)", newer)
	second := fmt.Sprintf("### 2. #%d: Older\n", older)
	if !strings.Contains(body, first) || !strings.Contains(body, second) {
		t.Errorf("body = %q; want #%d first, unblocking 2, then #%d", body, newer, older)
	}
}

// A failed check of the pin is the pass's error: the digest isn't pinned on
// a read that failed, nor its note dropped, and Work runs. The next pass
// checks again and pins it (#159's verify, survivor N11).
func TestDigestRewritePinCheckFails(t *testing.T) {
	f := newFake(t)
	f.AddIssue(app, "issuedog digest", digestPending)
	d := digestDaemon(t, f, newOrigin(t), nil)
	worked := 0
	d.Work = func(context.Context, []daemon.Ready) error { worked++; return nil }
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Path == "/graphql" }, http.StatusInternalServerError)
	if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "the pin of the digest, o/r#1: ") || worked != 1 {
		t.Fatalf("Iterate = %v, Work ran %d times; want the check's error, and Work run", err, worked)
	}
	if w := f.Writes(); len(w) != 0 || f.State().Issues[0].Pinned || f.State().Issues[0].Body != digestPending {
		t.Fatalf("writes %+v, issue %+v; want nothing pinned or edited", w, f.State().Issues[0])
	}
	if got := digestPass(t, f, d); !reflect.DeepEqual(got, []string{"POST /graphql", "PATCH /issues/1"}) || !f.State().Issues[0].Pinned ||
		f.State().Issues[0].Body != digestList {
		t.Errorf("writes %q, issue %+v; want it pinned, and the note gone", got, f.State().Issues[0])
	}
}

// An issue whose read fails keeps its items, so the digest isn't edited;
// once it closes, its items go.
func TestDigestRewriteCache(t *testing.T) {
	f := digestPinned(t)
	n := digestError(f, "A", "5")
	d := digestDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	listed := f.State().Issues[0].Body
	f.AddComment(n, "owner", "hello") // it's read again
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Path == fmt.Sprintf("/issues/%d", n) }, http.StatusInternalServerError)
	from := len(f.Writes())
	if err := d.Iterate(context.Background()); err == nil {
		t.Fatal("Iterate = nil; want the failed read's error")
	}
	if got := digestWrites(f, from); len(got) != 0 || f.State().Issues[0].Body != listed {
		t.Errorf("writes %q after the failed read; want none, and the item kept", got)
	}
	f.Close(n)
	if got := digestPass(t, f, d); !reflect.DeepEqual(got, []string{"PATCH /issues/1"}) || f.State().Issues[0].Body != digestList {
		t.Errorf("writes %q, body %q once it closed; want the edit that lists nothing", got, f.State().Issues[0].Body)
	}
}

// An error whose comment eve edited isn't open, once step 1 has noted
// the dirty content: its issue isn't visited on every pass, and the digest
// doesn't list it.
func TestDigestRewriteDirtyError(t *testing.T) {
	f := digestPinned(t)
	n := f.AddIssue("owner", "A", "do it")
	f.AddLabel(n, app, "stage:ready")
	f.AddComment(n, app, "<!-- issuedog:transition from=intake to=ready run=rready -->")
	id := f.AddComment(n, app, "Put back.\n\n<!-- issuedog:error kind=disagreement key=5 -->")
	f.EditComment(id, "eve", "Put back.\n\n<!-- issuedog:error kind=disagreement key=5 -->") // no write access, so no hold
	var visits []int
	d := digestDaemon(t, f, newOrigin(t), &visits)
	var passes [][]int
	for range 4 {
		visits = nil
		iterate(t, d)
		passes = append(passes, visits)
	}
	if w := digestWrites(f, 0); slices.Contains(w, "PATCH /issues/1") || len(passes[3]) != 0 {
		t.Errorf("writes %q, visits by pass %v; want the digest never edited, and no visit once settled", w, passes)
	}
}

// The digest's items are found with the issue's dirty content: on the
// first pass, a spec whose body eve edited is listed as one that can't
// be approved yet, for that edit.
func TestDigestRewriteDirtyBody(t *testing.T) {
	f := digestPinned(t)
	n := f.AddIssue("owner", "Spec", "the spec")
	f.AddLabel(n, app, "stage:spec-review")
	f.AddComment(n, app, revision(1, "the spec"))
	f.EditBody(n, "eve", "the spec, edited") // no write access, so no hold
	iterate(t, digestDaemon(t, f, newOrigin(t), nil))
	if body := f.State().Issues[0].Body; !strings.Contains(body, "It can't be approved yet: the issue description has an edit that wasn't approved") {
		t.Errorf("body = %q; want the approval held back for the dirty description", body)
	}
}
