package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// #614: an issue the identity marker or the title rule holds after a
// restart has no item read back for it from the digest's body, and one let
// go as gone loses its item, as a held issue's items go (SPEC §9.4, A
// failed history read) and a dropped one's do (Keeping the list). The
// issue is numbered below the digest, so its hold comes before the
// digest's history read takes the digest back on the same pass, which is
// when the start reads the items back.

// The issues of digestKeptHeldRepo's fake: the held App task, below the
// digest.
const (
	digestKeptHeldTask   = 1
	digestKeptHeldDigest = 2
)

// digestKeptHeldRepo is a fake whose digest, #2, a first daemon created
// and wrote, lists the state error of #1, an App task. While no daemon
// runs, the owner's edit puts the digest's identity marker in #1's body,
// so a start holds #1 till its edit history shows the App never wrote it
// (#498). A closed issue filed last keeps the poll's cursor past #1, so
// no poll lists #1 again unless it changes, and a gone answer about it
// counts.
func digestKeptHeldRepo(t testing.TB, o *origin) *githubtest.Fake {
	t.Helper()
	f := newFake(t)
	task := f.AddIssue(app, "A task", "Do it.")
	f.AddLabel(task, app, "stage:ready")
	f.AddComment(task, app, "<!-- issuedog:transition from=intake to=ready run=rready -->")
	digestErrorOn(f, task, "1")
	digestKeptSettle(t, f, digestDaemon(t, f, o, nil))
	if !digestKeptHeldLists(f) {
		t.Fatalf("digest %q; want #%d's error listed", digestKeptHeldBody(f), digestKeptHeldTask)
	}
	f.EditBody(task, "owner", "Do it.\n\n<!-- issuedog:digest -->")
	f.Close(f.AddIssue("owner", "Closed", ""))
	return f
}

// digestKeptHeldBody is the digest's body in f.
func digestKeptHeldBody(f *githubtest.Fake) string {
	return f.State().Issues[digestKeptHeldDigest-1].Body
}

// digestKeptHeldLists reports whether the digest's body in f lists an item
// on the held task.
func digestKeptHeldLists(f *githubtest.Fake) bool {
	return slices.ContainsFunc(digestRunItems(digestKeptHeldBody(f)), func(s string) bool {
		return strings.HasPrefix(s, fmt.Sprintf("#%d ", digestKeptHeldTask))
	})
}

// digestKeptHeldRequests is the requests from the from'th on that are
// GraphQL or name the task or the digest.
func digestKeptHeldRequests(f *githubtest.Fake, from int) []githubtest.Request {
	var got []githubtest.Request
	for _, r := range f.Requests()[from:] {
		for _, n := range []int{digestKeptHeldTask, digestKeptHeldDigest} {
			if p := fmt.Sprintf("/issues/%d", n); r.Path == p || strings.HasPrefix(r.Path, p+"/") {
				got = append(got, r)
			}
		}
		if r.Path == "/graphql" {
			got = append(got, r)
		}
	}
	return got
}

// digestKeptHeldArm arms what GitHub answers the history reads of
// TestDigestKeptHeld's restart, by pass, from 0: an outage that fails
// the read, then a read that works for the digest, but answers the task
// NOT_FOUND, though it's there, on two passes. The first NOT_FOUND holds
// the task still, as one answer may be wrong, and the second lets it go
// (#500).
func digestKeptHeldArm(f *githubtest.Fake, pass int) {
	switch pass {
	case 0:
		f.FailGraphQL(digestHistoryFailure)
	case 1, 2:
		f.HistoryNotFoundOnce(digestKeptHeldTask)
	}
}

// The verify's probe of #510: a restart in an outage holds the digest and
// the task. The pass whose read takes the digest back, and reads its items
// back, holds the task still, on its first NOT_FOUND, so its item goes:
// the pass writes the digest without it. The pass that lets the task go,
// on the second NOT_FOUND, writes nothing, as the item stays gone, and so
// does the pass after, which makes no request about the task or the
// digest, and no GraphQL request.
func TestDigestKeptHeld(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	f := digestKeptHeldRepo(t, o)
	d := digestDaemon(t, f, o, nil)
	for i, want := range [][]string{nil, {fmt.Sprintf("PATCH /issues/%d", digestKeptHeldDigest)}, nil} {
		digestKeptHeldArm(f, i)
		from := len(f.Writes())
		iterate(t, d)
		if got := digestWrites(f, from); !slices.Equal(got, want) {
			t.Errorf("pass %d wrote %q; want %q", i+1, got, want)
		}
		if i > 0 && digestKeptHeldLists(f) {
			t.Errorf("pass %d: digest %q; want #%d's error gone, as it's held or let go", i+1, digestKeptHeldBody(f), digestKeptHeldTask)
		}
	}
	if got := daemon.DigestHistoryDigests(d); !slices.Equal(got, []int{digestKeptHeldDigest}) {
		t.Errorf("digests %v; want #%d alone", got, digestKeptHeldDigest)
	}
	from, reqs := len(f.Writes()), len(f.Requests())
	iterate(t, d)
	if w, got := digestWrites(f, from), digestKeptHeldRequests(f, reqs); len(w) != 0 || len(got) != 0 {
		t.Errorf("the unchanged pass wrote %q and sent %+v about the task, the digest or GraphQL; want neither", w, got)
	}
}

// A task let go as gone on the pass that takes the digest back has no item
// read back for it. With loop.max_attempts 1, the restart's first pass, in
// an outage, posts the task's state error, which GitHub answers a false
// 404 that serves nothing: the first answer that finds it deleted. The
// next pass's read works for the digest, and answers the task NOT_FOUND,
// the second answer, so the task is let go before the digest is taken
// back. The pass writes the digest without the task's item.
func TestDigestKeptHeldGone(t *testing.T) {
	t.Parallel()
	o := digestGoneOrigin(t)
	f := digestKeptHeldRepo(t, o)
	d := digestDaemon(t, f, o, nil)
	post := fmt.Sprintf("/issues/%d/comments", digestKeptHeldTask)
	f.FailGraphQL(digestHistoryFailure)
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == post }, http.StatusNotFound)
	iterate(t, d)
	if !digestKeptHeldLists(f) {
		t.Fatalf("digest %q; want #%d's error listed still, as no digest was taken back", digestKeptHeldBody(f), digestKeptHeldTask)
	}
	f.HistoryNotFoundOnce(digestKeptHeldTask)
	from := len(f.Writes())
	iterate(t, d)
	if got, want := digestWrites(f, from), []string{fmt.Sprintf("PATCH /issues/%d", digestKeptHeldDigest)}; !slices.Equal(got, want) {
		t.Errorf("the pass that lets the task go wrote %q; want %q", got, want)
	}
	if digestKeptHeldLists(f) {
		t.Errorf("digest %q; want #%d's error gone, as it's let go", digestKeptHeldBody(f), digestKeptHeldTask)
	}
	if got := daemon.DigestHistoryDigests(d); !slices.Equal(got, []int{digestKeptHeldDigest}) {
		t.Errorf("digests %v; want #%d alone", got, digestKeptHeldDigest)
	}
}

// An issue held at the start, numbered above the digest, that the pass
// which takes the digest back judges no digest keeps its item read back,
// as the items are read back once every held issue is judged. The digest,
// #1, lists the state error of #2, an App task, whose body the owner's
// edit, while no daemon runs, gave the identity marker. The restart's
// first pass reads both histories: #2's shows the App never wrote the
// marker, so it's in the pipeline that pass, and its read fails. The
// digest keeps #2's item, so the pass writes nothing.
func TestDigestKeptHeldJudged(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), newOrigin(t)
	iterate(t, digestDaemon(t, f, o, nil)) // creates the digest, #1
	task := f.AddIssue(app, "A task", "Do it.")
	f.AddLabel(task, app, "stage:ready")
	f.AddComment(task, app, "<!-- issuedog:transition from=intake to=ready run=rready -->")
	digestErrorOn(f, task, "1")
	digestKeptSettle(t, f, digestDaemon(t, f, o, nil))
	lists := func() bool {
		return slices.ContainsFunc(digestRunItems(f.State().Issues[0].Body), func(s string) bool { return strings.HasPrefix(s, fmt.Sprintf("#%d ", task)) })
	}
	if !lists() {
		t.Fatalf("digest %q; want #%d's error listed", f.State().Issues[0].Body, task)
	}
	f.EditBody(task, "owner", "Do it.\n\n<!-- issuedog:digest -->")
	d := digestDaemon(t, f, o, nil)
	digestKeptFail(t, f, task, "read")
	from := len(f.Writes())
	if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("o/r#%d", task)) {
		t.Fatalf("the restart's first pass: Iterate = %v; want #%d's failure", err, task)
	}
	if got := digestWrites(f, from); len(got) != 0 {
		t.Errorf("the restart's first pass wrote %q; want nothing", got)
	}
	if !lists() {
		t.Errorf("digest %q; want #%d's error kept", f.State().Issues[0].Body, task)
	}
	if got := daemon.DigestHistoryDigests(d); !slices.Equal(got, []int{1}) {
		t.Errorf("digests %v; want #1 alone", got)
	}
}

// TestDigestKeptHeld's restart is safe to replay after a crash at its
// write, the digest's edit: each replay is a restart, whose passes hold
// the task again and write the digest without its item, if the edit
// didn't land.
func TestDigestKeptHeldReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := digestKeptHeldRepo(tb, o)
		f.ForgetWrites()
		return f
	}, func(f *githubtest.Fake) error {
		d := digestDaemon(t, f, o, nil)
		var errs []error
		for i := range 3 {
			digestKeptHeldArm(f, i)
			if err := d.Iterate(context.Background()); err != nil {
				errs = append(errs, fmt.Errorf("pass %d: %w", i+1, err))
			}
		}
		return errors.Join(errs...)
	})
}
