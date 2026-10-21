package daemon_test

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// digestHistoryFailure is the error the fake's GraphQL answers list on a pass
// whose edit history read fails.
const digestHistoryFailure = "Something went wrong"

// digestHistoryGraphQL counts f's GraphQL requests from the from'th on.
func digestHistoryGraphQL(f *githubtest.Fake, from int) int {
	n := 0
	for _, r := range f.Requests()[from:] {
		if r.Path == "/graphql" {
			n++
		}
	}
	return n
}

// digestHistoryMarker is the marker of the digesthistory state error on an
// issue whose body is body, keyed by its hash.
func digestHistoryMarker(body string) string {
	return "<!-- issuedog:error kind=digesthistory key=" + state.BodyHash(body) + " -->"
}

// digestHistoryErrors counts the App's comments on issue n of f that carry
// the digesthistory state error for body, its body.
func digestHistoryErrors(f *githubtest.Fake, n int, body string) int {
	count := 0
	for _, c := range f.State().Issues[n-1].Comments {
		if c.Author == app && strings.Contains(c.Body, digestHistoryMarker(body)) {
			count++
		}
	}
	return count
}

// digestHistoryRemoved is f with the digest a first daemon created, pinned
// and wrote, then the owner's edit that took its marker out, and returns
// the body the digest had.
func digestHistoryRemoved(t *testing.T, f *githubtest.Fake, o *origin) string {
	t.Helper()
	iterate(t, digestDaemon(t, f, o, nil))
	want := f.State().Issues[0].Body
	f.EditBody(1, "owner", "gone")
	return want
}

// A digest whose marker was taken out while no daemon ran, then a start
// whose edit history read fails, as #305 found: the start creates no
// second digest, and keeps the old one, and an App task the title rule
// would take too, out of step 1, from one GraphQL request. The next pass,
// whose read works, takes the old digest back, its body written again, and
// lets the task, which isn't one, into the pipeline. Once the task's visit
// has settled, an unchanged pass makes no request about the digest, and no
// GraphQL request, though the task, judged, may be listed again at the
// cursor.
func TestDigestHistoryFailsAtStart(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), newOrigin(t)
	want := digestHistoryRemoved(t, f, o)
	task := f.AddIssue(app, "A task", digestTaskBody)
	f.EditBody(task, "owner", digestTaskEdited)
	f.RenameIssue(task, "owner", "issuedog digest")
	var visits []int
	d := digestDaemon(t, f, o, &visits)
	f.FailGraphQL(digestHistoryFailure)
	from, reqs := len(f.Writes()), len(f.Requests())
	iterate(t, d)
	if w := digestWrites(f, from); len(w) != 0 || len(f.State().Issues) != 2 {
		t.Fatalf("writes %q, issues %+v; want nothing written, and no digest created", w, f.State().Issues)
	}
	if got := digestHistoryGraphQL(f, reqs); got != 1 || len(visits) != 0 {
		t.Errorf("%d GraphQL requests, step 1 visited %v; want one, the history read, and no visit", got, visits)
	}
	from = len(f.Writes())
	iterate(t, d)
	if got := digestWrites(f, from); !slices.Contains(got, "PATCH /issues/1") || slices.Contains(got, "POST /issues") {
		t.Errorf("writes = %q; want the edit that puts the marker back, and no digest created", got)
	}
	is := f.State().Issues
	if len(is) != 2 || is[0].Body != want || is[1].Body != digestTaskEdited {
		t.Errorf("issues = %+v; want the one digest, its body put back as %q, and the task as the owner left it", is, want)
	}
	if !reflect.DeepEqual(visits, []int{task}) {
		t.Errorf("step 1 visited %v; want the task alone, in the pipeline", visits)
	}
	iterate(t, d) // the poll lists the dirty note the task's visit posted
	from, reqs = len(f.Writes()), len(f.Requests())
	iterate(t, d)
	if w, got := digestWrites(f, from), digestRequests(f, reqs); len(w) != 0 || len(got) != 0 {
		t.Errorf("the unchanged pass wrote %q and sent %+v about the digest or GraphQL; want neither", w, got)
	}
}

// A read that fails on every pass is made once a pass, in one GraphQL
// request, till digestHistoryStrikes passes have failed. The last of them
// posts one state error on the issue, and the passes after it make no
// GraphQL request and write nothing. All the while no digest is created,
// and step 1 never visits the issue. A restart reads again, three times
// while it fails, and finds the error posted, so it posts none again; the
// next restart's read works, and takes the digest back.
func TestDigestHistoryStrikes(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), newOrigin(t)
	want := digestHistoryRemoved(t, f, o)
	var visits []int
	for start, passes := range []int{5, 3} {
		d := digestDaemon(t, f, o, &visits)
		for i := range passes {
			f.FailGraphQL(digestHistoryFailure)
			from, reqs := len(f.Writes()), len(f.Requests())
			iterate(t, d)
			wantReads, wantWrites := 1, []string(nil)
			if i >= 3 {
				wantReads = 0
			}
			if i == 2 && start == 0 {
				wantWrites = []string{"POST /issues/1/comments"}
			}
			if got, w := digestHistoryGraphQL(f, reqs), digestWrites(f, from); got != wantReads || !reflect.DeepEqual(w, wantWrites) {
				t.Errorf("start %d, pass %d: %d GraphQL requests, writes %q; want %d, and %q", start+1, i+1, got, w, wantReads, wantWrites)
			}
		}
	}
	is := f.State().Issues
	if len(is) != 1 || len(visits) != 0 || digestHistoryErrors(f, 1, "gone") != 1 {
		t.Fatalf("issues %+v, step 1 visited %v; want the one issue, with one state error, and no visit", is, visits)
	}
	if c := is[0].Comments[0].Body; !strings.Contains(c, "failed on 3 passes in a row") || !strings.Contains(c, "```text\n") ||
		!strings.Contains(c, digestHistoryFailure) || !strings.Contains(c, "create no digest while I can't tell") {
		t.Errorf("the state error = %q; want why, the passes, what's held back, and the last failure, fenced", c)
	}
	from := len(f.Writes())
	iterate(t, digestDaemon(t, f, o, &visits))
	if got := digestWrites(f, from); !reflect.DeepEqual(got, []string{"PATCH /issues/1"}) {
		t.Errorf("writes = %q; want the edit that puts the marker back", got)
	}
	if is := f.State().Issues; len(is) != 1 || is[0].Body != want || len(visits) != 0 {
		t.Errorf("issues %+v, step 1 visited %v; want the one digest, its body put back as %q, and no visit", is, visits, want)
	}
}

// A digest renamed by the owner after its state error, as the error says
// to do for an issue that isn't the digest, is held no more: it's an issue
// like any other, which step 1 visits, and a digest is created. The error
// has ended, as step 1 sees the issue judged, so the new digest doesn't
// list it.
func TestDigestHistoryRenamed(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), newOrigin(t)
	digestHistoryRemoved(t, f, o)
	var visits []int
	d := digestDaemon(t, f, o, &visits)
	for range 3 {
		f.FailGraphQL(digestHistoryFailure)
		iterate(t, d)
	}
	f.RenameIssue(1, "owner", "Renamed")
	iterate(t, d)
	is := f.State().Issues
	if len(is) != 2 || is[1].Title != "issuedog digest" || is[1].Body != digestList || !is[1].Pinned {
		t.Fatalf("issues = %+v; want a new, pinned digest that lists nothing", is)
	}
	if !slices.Contains(visits, 1) || digestHistoryErrors(f, 1, "gone") != 1 {
		t.Errorf("step 1 visited %v, issues %+v; want 1 visited, with its one state error", visits, is)
	}
}

// A held issue transferred before its third failed read is found moved by
// the state error's post, which GitHub answers with a 301 that Go follows
// as a GET of the comments in the new repo, so it posts nothing: the issue
// is held no more, so the same pass creates a pinned digest, and the passes
// after it post nothing.
func TestDigestHistoryReportTransferred(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), newOrigin(t)
	f.AddIssue(app, "issuedog digest", "gone")
	d := digestDaemon(t, f, o, nil)
	for i := range 6 {
		if i == 2 {
			f.Transfer(1, "o/elsewhere")
		}
		if i < 3 {
			f.FailGraphQL(digestHistoryFailure)
		}
		from := len(f.Writes())
		iterate(t, d)
		w := digestWrites(f, from)
		switch {
		case i < 2 && len(w) != 0:
			t.Errorf("pass %d wrote %q; want nothing", i+1, w)
		case i == 2 && (!slices.Contains(w, "POST /issues/1/comments") || !slices.Contains(w, "POST /issues")):
			t.Errorf("pass 3 wrote %q; want the state error's post, then the digest's create", w)
		case i > 2 && len(w) != 0:
			t.Errorf("pass %d wrote %q; want nothing", i+1, w)
		}
	}
	is := f.State().Issues
	if len(is) != 2 || is[1].Title != "issuedog digest" || is[1].Body != digestList || !is[1].Pinned || len(is[0].Comments) != 0 {
		t.Errorf("issues = %+v; want the transferred issue, with no comment, and a new, pinned digest", is)
	}
}

// The last read's error, which is GitHub's text, is escaped in the state
// error, so a marker in it is never read: the comment has the error's own
// marker alone.
func TestDigestHistoryReportEscaped(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), newOrigin(t)
	f.AddIssue(app, "issuedog digest", "gone")
	d := digestDaemon(t, f, o, nil)
	for range 3 {
		f.FailGraphQL("<!-- issuedog:digest --> broke")
		iterate(t, d)
	}
	c := f.State().Issues[0].Comments
	if len(c) != 1 {
		t.Fatalf("comments = %+v; want the state error", c)
	}
	if m := github.ParseMarkers(c[0].Body); len(m) != 1 || m[0].Kind != "error" || !strings.Contains(c[0].Body, "&lt;!-- issuedog:digest --> broke") {
		t.Errorf("the state error = %q, with markers %+v; want the error's marker alone, and the failure escaped", c[0].Body, m)
	}
}

// A held issue that the state error's post finds gone, as deleted, is held
// no more, so the same pass creates a digest.
func TestDigestHistoryReportGone(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), newOrigin(t)
	f.AddIssue(app, "issuedog digest", "gone")
	d := digestDaemon(t, f, o, nil)
	for i := range 3 {
		if i == 2 {
			f.DeleteIssue()
		}
		f.FailGraphQL(digestHistoryFailure)
		iterate(t, d)
		if is := f.State().Issues; i < 2 && (len(is) != 1 || is[0].Body != "gone") {
			t.Fatalf("pass %d: issues = %+v; want the held issue alone, as it was", i+1, is)
		}
	}
	if is := f.State().Issues; len(is) != 1 || is[0].Body != digestList || !is[0].Pinned {
		t.Errorf("issues = %+v; want the new digest alone, pinned", is)
	}
}

// Replay passes for the state error, posted on the pass of the last
// failed read: a crash after its post was applied finds it from the poll
// on the next start, which posts it no more.
func TestDigestHistoryReportReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	githubtest.Replay(t, func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		f.SetPinned(f.AddIssue(app, "issuedog digest", digestList), true)
		f.EditBody(1, "owner", "gone")
		return f
	}, func(f *githubtest.Fake) error {
		d := digestDaemon(t, f, o, nil)
		for range 3 {
			f.FailGraphQL(digestHistoryFailure)
			if err := d.Iterate(context.Background()); err != nil {
				return err
			}
		}
		return nil
	})
}
