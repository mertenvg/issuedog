package daemon_test

import (
	"context"
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// How an issue the digest's title rule holds is found gone (#500): only
// by two answers that found it deleted, on two passes, with none between
// that found it there, as one answer may be wrong (SPEC §8.1).

// digestGoneOrigin is newOrigin with loop.max_attempts 1, so the first
// failed read posts the state error.
func digestGoneOrigin(t testing.TB) *origin {
	t.Helper()
	o := newOrigin(t)
	o.commit(config.Path, "types: labels\nloop:\n  idle_sleep: 7s\n  max_attempts: 1\n")
	return o
}

// A start whose read of the history of the digest, its marker taken out,
// GitHub answers NOT_FOUND, though the digest is there: the digest is held
// still, so that pass creates none, and its history is read again the next
// pass. That read works, and takes the old digest back, its body written
// again, so there's never a second digest. Once that settles, an unchanged
// pass makes no request about the digest, and no GraphQL request.
func TestDigestHistoryFalseNotFound(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), newOrigin(t)
	want := digestHistoryRemoved(t, f, o)
	var visits []int
	d := digestDaemon(t, f, o, &visits)
	f.HistoryNotFoundOnce(1)
	for i, back := range []bool{false, true} {
		from, queries := len(f.Writes()), f.HistoryQueries()
		iterate(t, d)
		w := digestWrites(f, from)
		if got := f.HistoryQueries() - queries; got != 1 || slices.Contains(w, "POST /issues") || slices.Contains(w, "PATCH /issues/1") != back ||
			(!back && len(w) != 0) {
			t.Errorf("pass %d: %d history queries, writes %q; want 1, no digest created, and the edit that puts the marker back %v",
				i+1, got, w, back)
		}
	}
	if is := f.State().Issues; len(is) != 1 || is[0].Body != want || !is[0].Pinned || len(visits) != 0 {
		t.Fatalf("issues %+v, step 1 visited %v; want the one digest, pinned, its body put back as %q, and no visit", is, visits, want)
	}
	iterate(t, d) // the poll lists the body the last pass wrote
	from, reqs := len(f.Writes()), len(f.Requests())
	iterate(t, d)
	if w, got := digestWrites(f, from), digestRequests(f, reqs); len(w) != 0 || len(got) != 0 {
		t.Errorf("the unchanged pass wrote %q and sent %+v about the digest or GraphQL; want neither", w, got)
	}
}

// Replay passes for TestDigestHistoryFalseNotFound's two: the edit that
// takes the digest back, on the pass after the read GitHub answered
// NOT_FOUND. A restart after a crash at it reads the history again, which
// works, and takes the digest back at once.
func TestDigestHistoryFalseNotFoundReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	githubtest.Replay(t, func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		f.SetPinned(f.AddIssue(app, "issuedog digest", digestList), true)
		f.EditBody(1, "owner", "gone")
		f.HistoryNotFoundOnce(1)
		return f
	}, func(f *githubtest.Fake) error {
		d := digestDaemon(t, f, o, nil)
		for range 2 {
			if err := d.Iterate(context.Background()); err != nil {
				return err
			}
		}
		return nil
	})
}

// An issue the title rule holds, deleted just before the start's history
// read, which finds it gone: it's held still. The next pass's read fails,
// which shows nothing, so the read after it, which finds it gone again, is
// the second answer, and lets it go, so that pass creates a digest.
func TestDigestHistoryGoneFailedReadBetween(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), newOrigin(t)
	f.AddIssue(app, "issuedog digest", "gone")
	f.BeforeGraphQL(f.DeleteIssue)
	d := digestDaemon(t, f, o, nil)
	for i := range 3 {
		if i == 1 {
			f.FailGraphQL(digestHistoryFailure)
		}
		from, queries := len(f.Writes()), f.HistoryQueries()
		iterate(t, d)
		w := digestWrites(f, from)
		if got := f.HistoryQueries() - queries; got != 1 || slices.Contains(w, "POST /issues") != (i == 2) {
			t.Errorf("pass %d: %d history queries, writes %q; want 1, and the digest created on the third pass alone", i+1, got, w)
		}
	}
	if is := f.State().Issues; len(is) != 1 || is[0].Body != digestList || !is[0].Pinned {
		t.Errorf("issues = %+v; want the new digest alone, pinned", is)
	}
}

// digestGoneArm arms what GitHub answers the passes of
// TestDigestHistoryGoneAnswers about held digest 1 of f, by pass, from 0:
// a failed read and a post of the state error answered 500; a read
// answered NOT_FOUND and a post answered 404, on the same pass; a failed
// read, and a post that works; a read answered NOT_FOUND; and a read that
// works.
func digestGoneArm(f *githubtest.Fake, pass int) {
	post := func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/issues/1/comments" }
	switch pass {
	case 0:
		f.FailGraphQL(digestHistoryFailure)
		f.AnswerOnce(post, http.StatusInternalServerError)
	case 1:
		f.HistoryNotFoundOnce(1)
		f.AnswerOnce(post, http.StatusNotFound)
	case 2:
		f.FailGraphQL(digestHistoryFailure)
	case 3:
		f.HistoryNotFoundOnce(1)
	}
}

// The digest, its marker taken out, held as its read fails, with
// loop.max_attempts 1, so each pass reads its history and, till it's
// posted, posts the state error. The read GitHub answers NOT_FOUND and the
// post it answers 404, on one pass, are one answer, as they're a moment
// apart, so the digest is held still. The next pass's read fails, which
// shows nothing, and its post works, which shows the digest is there, so
// the read answered NOT_FOUND on the pass after counts as the first answer
// again, and the digest is held still. The read after that works, and
// takes the digest back. No pass creates a digest, so there's never a
// second one (#500). Its writes, the state error's post and the edit, are
// replayed by TestDigestHistoryReportReplay and
// TestDigestHistoryOutageReplay: a crash at the post here is a post that
// failed, which shows nothing, so the two NOT_FOUNDs let the digest go, as
// they should.
func TestDigestHistoryGoneAnswers(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), digestGoneOrigin(t)
	want := digestHistoryRemoved(t, f, o)
	d := digestDaemon(t, f, o, nil)
	for i := range 5 {
		digestGoneArm(f, i)
		from, queries := len(f.Writes()), f.HistoryQueries()
		iterate(t, d)
		w := digestWrites(f, from)
		if got := f.HistoryQueries() - queries; got != 1 || slices.Contains(w, "POST /issues") {
			t.Errorf("pass %d: %d history queries, writes %q; want 1, and no digest created", i+1, got, w)
		}
		switch {
		case i == 2 && !reflect.DeepEqual(w, []string{"POST /issues/1/comments"}):
			t.Errorf("pass 3 wrote %q; want the state error alone", w)
		case i == 4 && !slices.Contains(w, "PATCH /issues/1"):
			t.Errorf("pass 5 wrote %q; want the edit that puts the marker back", w)
		case i != 2 && i != 4 && len(w) != 0:
			t.Errorf("pass %d wrote %q; want nothing", i+1, w)
		}
	}
	if is := f.State().Issues; len(is) != 1 || is[0].Body != want || !is[0].Pinned || digestHistoryErrors(f, 1, "gone") != 1 {
		t.Errorf("issues = %+v; want the one digest, pinned, its body put back as %q, with one state error", is, want)
	}
}
