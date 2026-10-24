package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// How a held issue the digest's title rule can't judge recovers (#437):
// the read once every loop.max_attempts passes after the strikes, a node
// GitHub resolves to none, and the items a held issue had.

// digestHistoryOrigin is newOrigin with loop.max_attempts 2.
func digestHistoryOrigin(t testing.TB) *origin {
	t.Helper()
	o := newOrigin(t)
	o.commit(config.Path, "types: labels\nloop:\n  idle_sleep: 7s\n  max_attempts: 2\n")
	return o
}

// An outage at a start longer than the strikes, here loop.max_attempts 2:
// the reads fail on passes 1 and 2, the second posting the state error,
// then the read is made once every 2 passes, failing on pass 4, so the
// outage costs one GraphQL request on each of the first two passes and on
// every second pass after, and the one comment. Pass 6's read works, and
// takes the old digest back, its body written again, without a restart,
// and no digest is ever created. The passes after it read nothing.
func TestDigestHistoryOutageRecovers(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), digestHistoryOrigin(t)
	want := digestHistoryRemoved(t, f, o)
	var visits []int
	d := digestDaemon(t, f, o, &visits)
	for i, read := range []int{1, 1, 0, 1, 0, 1, 0, 0} {
		if read == 1 && i < 5 {
			f.FailGraphQL(digestHistoryFailure)
		}
		from, queries := len(f.Writes()), f.HistoryQueries()
		iterate(t, d)
		w := digestWrites(f, from)
		if got := f.HistoryQueries() - queries; got != read || slices.Contains(w, "POST /issues") {
			t.Errorf("pass %d: %d history queries, writes %q; want %d, and no digest created", i+1, got, w, read)
		}
		switch {
		case i == 1 && !reflect.DeepEqual(w, []string{"POST /issues/1/comments"}):
			t.Errorf("pass 2 wrote %q; want the state error alone", w)
		case i == 5 && !slices.Contains(w, "PATCH /issues/1"):
			t.Errorf("pass 6 wrote %q; want the edit that puts the marker back", w)
		case i != 1 && i != 5 && len(w) != 0:
			t.Errorf("pass %d wrote %q; want nothing", i+1, w)
		}
	}
	is := f.State().Issues
	if len(is) != 1 || is[0].Body != want || !is[0].Pinned || len(visits) != 0 || digestHistoryErrors(f, 1, "gone") != 1 {
		t.Fatalf("issues %+v, step 1 visited %v; want the one digest, pinned, its body put back as %q, with one state error, and no visit",
			is, visits, want)
	}
	if c := is[0].Comments[0].Body; !strings.Contains(c, "failed on 2 passes in a row") || !strings.Contains(c, "this ends on its own once a read works") {
		t.Errorf("the state error = %q; want the 2 passes, and that it ends on its own", c)
	}
}

// Replay passes for the outage of TestDigestHistoryOutageRecovers: the
// state error, posted on pass 2, and the edit that takes the digest back
// on pass 6. A crash at either is followed by a restart, which finds the
// error from the poll, and whose reads take the digest back. The outage
// lasts till a read works, which the App's edit of the body shows, as only
// a read that worked lets it write the body back: a restart after that
// edit meets no outage, and its read takes the digest back at once, as
// the identity marker the edit put back counts only once its history
// shows the App wrote it (SPEC §5.1, #498).
func TestDigestHistoryOutageReplay(t *testing.T) {
	t.Parallel()
	o := digestHistoryOrigin(t)
	githubtest.Replay(t, func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		f.SetPinned(f.AddIssue(app, "issuedog digest", digestList), true)
		f.EditBody(1, "owner", "gone")
		return f
	}, func(f *githubtest.Fake) error {
		d := digestDaemon(t, f, o, nil)
		over := f.State().Issues[0].BodyEditedBy == app
		for i := range 6 {
			if !over && i < 4 && i != 2 {
				f.FailGraphQL(digestHistoryFailure)
			}
			if err := d.Iterate(context.Background()); err != nil {
				return err
			}
		}
		return nil
	})
}

// digestHistoryGoneTask files the App task of #87's probe, its origin
// edited out by the owner and renamed as the digest, as
// TestDigestHistoryFailsAtStart does, and returns its number: only its
// history, whose first version has the origin, shows it isn't the digest.
func digestHistoryGoneTask(f *githubtest.Fake) int {
	task := f.AddIssue(app, "A task", digestTaskBody)
	f.EditBody(task, "owner", digestTaskEdited)
	f.RenameIssue(task, "owner", "issuedog digest")
	return task
}

// Three issues the title rule holds: the digest whose marker was taken
// out, an App task that lost its origin and was renamed as the digest, and
// an App issue titled as the digest, the last deleted between the start's
// poll and its history read. GitHub resolves that one's node to none, so
// it's let go, while the other two's histories, read in the same query,
// are judged on that pass: the digest's takes it back, and the task's,
// whose first version had an origin, lets it into the pipeline, visited,
// so there's the one digest. No pass after reads a history, and no digest
// is created.
func TestDigestHistoryGoneCandidate(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), newOrigin(t)
	want := digestHistoryRemoved(t, f, o)
	task := digestHistoryGoneTask(f)
	f.AddIssue(app, "issuedog digest", "also gone")
	f.BeforeGraphQL(f.DeleteIssue)
	var visits []int
	d := digestDaemon(t, f, o, &visits)
	from, queries := len(f.Writes()), f.HistoryQueries()
	iterate(t, d)
	if w := digestWrites(f, from); !slices.Contains(w, "PATCH /issues/1") || slices.Contains(w, "POST /issues") {
		t.Errorf("writes = %q; want the edit that puts the marker back, and no digest created", w)
	}
	if is := f.State().Issues; len(is) != 2 || is[0].Body != want || is[1].Body != digestTaskEdited {
		t.Errorf("issues = %+v; want the one digest, its body put back as %q, and the task as the owner left it", is, want)
	}
	if got := daemon.DigestHistoryDigests(d); !reflect.DeepEqual(visits, []int{task}) || !reflect.DeepEqual(got, []int{1}) {
		t.Errorf("step 1 visited %v, digests %v; want the task visited, in the pipeline, and 1 the one digest", visits, got)
	}
	// The pass's other history query is the task's visit's, for its dirty body.
	if got := f.HistoryQueries() - queries; got != 2 {
		t.Errorf("%d history queries; want 2, the title rule's and the task's visit's", got)
	}
	queries = f.HistoryQueries()
	for range 2 {
		iterate(t, d)
	}
	if got := f.HistoryQueries() - queries; got != 0 || len(f.State().Issues) != 2 || !reflect.DeepEqual(daemon.DigestHistoryDigests(d), []int{1}) {
		t.Errorf("%d history queries, issues %+v, digests %v; want none, and 1 the one digest",
			got, f.State().Issues, daemon.DigestHistoryDigests(d))
	}
}

// Replay passes for TestDigestHistoryGoneCandidate's pass: the edit that
// takes the digest back, on the pass whose read found the other issue
// gone, and the task's visit.
func TestDigestHistoryGoneCandidateReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	githubtest.Replay(t, func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		f.SetPinned(f.AddIssue(app, "issuedog digest", digestList), true)
		f.EditBody(1, "owner", "gone")
		digestHistoryGoneTask(f)
		f.AddIssue(app, "issuedog digest", "also gone")
		f.BeforeGraphQL(f.DeleteIssue)
		return f
	}, func(f *githubtest.Fake) error { return digestDaemon(t, f, o, nil).Iterate(context.Background()) })
}

// A held issue deleted after its state error was posted is found gone by
// the next read, loop.max_attempts passes after the last, so on the third
// pass after the error: it's let go, and that pass creates a pinned digest.
func TestDigestHistoryGoneAfterError(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), newOrigin(t)
	f.AddIssue(app, "issuedog digest", "gone")
	d := digestDaemon(t, f, o, nil)
	for range 3 {
		f.FailGraphQL(digestHistoryFailure)
		iterate(t, d)
	}
	if digestHistoryErrors(f, 1, "gone") != 1 {
		t.Fatalf("issues = %+v; want the state error posted", f.State().Issues)
	}
	f.DeleteIssue()
	for i := range 3 {
		from := len(f.Writes())
		iterate(t, d)
		if w := digestWrites(f, from); slices.Contains(w, "POST /issues") != (i == 2) {
			t.Errorf("pass %d after the error wrote %q; want the digest created on the third alone", i+1, w)
		}
	}
	if is := f.State().Issues; len(is) != 1 || is[0].Body != digestList || !is[0].Pinned {
		t.Errorf("issues = %+v; want the new digest alone, pinned", is)
	}
}

// The state error the third failed read posts is recorded at once, so the
// next pass, whose comment listing doesn't have it yet, as GitHub's may
// lag, finds it posted and posts none again. The lag is a 304 to the
// listing, whose URL is the last pass's, so the client's cached answer,
// with no comment, stands.
func TestDigestHistoryReportRecorded(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), newOrigin(t)
	f.AddIssue(app, "issuedog digest", "gone")
	d := digestDaemon(t, f, o, nil)
	for range 3 {
		f.FailGraphQL(digestHistoryFailure)
		iterate(t, d)
	}
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues/comments" }, http.StatusNotModified)
	from := len(f.Writes())
	iterate(t, d)
	if w := digestWrites(f, from); len(w) != 0 || digestHistoryErrors(f, 1, "gone") != 1 {
		t.Errorf("writes %q, issues %+v; want nothing written, and the one state error", w, f.State().Issues)
	}
	if !slices.ContainsFunc(f.Requests()[from:], func(r githubtest.Request) bool { return r.Path == "/issues/comments" }) {
		t.Errorf("requests = %+v; want the comment listing, answered with none", f.Requests()[from:])
	}
}

// A task in the pipeline with an open state error, listed in the digest,
// that the owner then edits out of its origin and renames as the digest,
// and whose history read fails: it's held, so its items drop out of the
// digest, whose body is written with nothing listed, rather than listing
// what its last visit cached. Once a read works, it's let go, visited, and
// listed again.
func TestDigestHistoryHeldItems(t *testing.T) {
	t.Parallel()
	f, o := digestPinned(t), digestNotifyOrigin(t)
	task := f.AddIssue(app, "A task", digestTaskBody)
	f.AddLabel(task, app, "stage:ready")
	f.AddComment(task, app, "<!-- issuedog:transition from=intake to=ready run=rready -->")
	digestErrorOn(f, task, "77")
	d := digestDaemon(t, f, o, nil)
	digestPass(t, f, d)
	if body := f.State().Issues[0].Body; !strings.Contains(body, fmt.Sprintf("### 1. #%d: A task\n", task)) {
		t.Fatalf("body = %q; want the task's error listed", body)
	}
	f.EditBody(task, "owner", digestTaskEdited)
	f.RenameIssue(task, "owner", "issuedog digest")
	f.FailGraphQL(digestHistoryFailure)
	if got := digestPass(t, f, d); !reflect.DeepEqual(got, []string{"PATCH /issues/1"}) || f.State().Issues[0].Body != digestList {
		t.Errorf("writes %q, body %q; want the body edit alone, to list nothing", got, f.State().Issues[0].Body)
	}
	digestPass(t, f, d)
	if body := f.State().Issues[0].Body; !strings.Contains(body, fmt.Sprintf("### 1. #%d: issuedog digest\n", task)) {
		t.Errorf("body = %q; want the task's error listed again, once it's let go", body)
	}
}
