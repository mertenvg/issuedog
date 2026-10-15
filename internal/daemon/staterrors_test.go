package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// errAckError is the App's disagreement state error keyed 9.
const errAckError = "The stage labels didn't match.\n\n<!-- issuedog:error kind=disagreement key=9 -->"

// errAckIssue files issue 1 at ready, as the App moved it (comment 1001),
// with errAckError as comment 1002, then runs more.
func errAckIssue(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		t.Helper()
		f := newFake(t)
		fileAt(f, "ready", "body")
		flagWant(t, f.AddComment(1, app, errAckError), 1002)
		if more != nil {
			more(t, f)
		}
		return f
	}
}

// errAckTrust trusts owner and owner2, and, to show they're ignored
// whatever trust says, the App and a helper bot.
var errAckTrust = trusted{"owner": true, "owner2": true, app: true, "helper[bot]": true}

// errAckPass runs step 1 on issue 1 with only the state error trigger.
func errAckPass(t testing.TB) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		c := f.Client(t)
		s, err := c.Snapshot(context.Background(), "o", "r", 1)
		if err != nil {
			return err
		}
		st := &daemon.Step{Applier: &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}, Trust: errAckTrust,
			Reactions: daemon.ErrorByReaction(errAckTrust)}
		_, err = st.Run(context.Background(), s)
		return err
	}
}

// errAckReply is the ack of the error kind/key by login's 👍 r.
func errAckReply(login, kind, key string, r int64) string {
	return fmt.Sprintf("Acknowledged, by `%s`'s 👍.\n\n<!-- issuedog:error kind=%s key=%s ack=r%d -->", login, kind, key, r)
}

// errAckReads counts the reads of comment id's reactions among reqs.
func errAckReads(reqs []githubtest.Request, id int64) int {
	path := fmt.Sprintf("/issues/comments/%d/reactions", id)
	n := 0
	for _, r := range reqs {
		if r.Method == "GET" && r.Path == path {
			n++
		}
	}
	return n
}

// errAckRun runs pass on f and fails t on its error.
func errAckRun(t *testing.T, f *githubtest.Fake, pass func(*githubtest.Fake) error) {
	t.Helper()
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
}

// The earliest trusted 👍 on an open state error gets one ack reply that
// names its kind and key, under Replay, and a pass after it writes nothing.
func TestErrAckByReaction(t *testing.T) {
	for _, tc := range []struct {
		name    string
		more    func(t testing.TB, f *githubtest.Fake)
		replies []string
	}{
		{"a trusted 👍", func(t testing.TB, f *githubtest.Fake) {
			flagWant(t, f.AddReaction(1002, "owner", "+1"), 1003)
		}, []string{errAckReply("owner", "disagreement", "9", 1003)}},
		{"the earliest trusted 👍 wins", func(t testing.TB, f *githubtest.Fake) {
			f.AddReaction(1002, app, "+1")
			f.AddReaction(1002, "helper[bot]", "+1")
			f.AddReaction(1002, "mallory", "+1")
			f.AddReaction(1002, "owner", "heart")
			flagWant(t, f.AddReaction(1002, "owner2", "+1"), 1007)
			f.AddReaction(1002, "owner", "+1")
		}, []string{errAckReply("owner2", "disagreement", "9", 1007)}},
		{"a 👍 on a second copy counts", func(t testing.TB, f *githubtest.Fake) {
			flagWant(t, f.AddComment(1, app, errAckError), 1003)
			flagWant(t, f.AddReaction(1003, "owner", "+1"), 1004)
		}, []string{errAckReply("owner", "disagreement", "9", 1004)}},
		{"the earliest 👍 across copies wins", func(t testing.TB, f *githubtest.Fake) {
			flagWant(t, f.AddComment(1, app, errAckError), 1003)
			flagWant(t, f.AddReaction(1003, "owner2", "+1"), 1004)
			f.AddReaction(1002, "owner", "+1")
		}, []string{errAckReply("owner2", "disagreement", "9", 1004)}},
		{"two errors in one comment get an ack each", func(t testing.TB, f *githubtest.Fake) {
			flagWant(t, f.AddComment(1, app, "Two.\n\n<!-- issuedog:error kind=labels key=0 -->\n<!-- issuedog:error kind=frob key=1 -->"), 1003)
			flagWant(t, f.AddReaction(1003, "owner", "+1"), 1004)
		}, []string{errAckReply("owner", "frob", "1", 1004), errAckReply("owner", "labels", "0", 1004)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := errAckIssue(tc.more)
			githubtest.Replay(t, setup, errAckPass(t))
			f := setup(t)
			want := append(commentBodies(f, 1), tc.replies...)
			errAckRun(t, f, errAckPass(t))
			if got := commentBodies(f, 1); !slices.Equal(got, want) {
				t.Errorf("comments %q; want %q", got, want)
			}
			n := len(f.Writes())
			errAckRun(t, f, errAckPass(t))
			if w := f.Writes()[n:]; len(w) != 0 {
				t.Errorf("a second pass wrote %+v; want nothing", w)
			}
		})
	}
}

// No ack for a 👍 that isn't a trusted user's, or one on an error that has
// ended. An ended error's reactions aren't read.
func TestErrAckIgnored(t *testing.T) {
	const moved = "<!-- issuedog:transition from=ready to=ready run=again -->"
	for _, tc := range []struct {
		name  string
		more  func(t testing.TB, f *githubtest.Fake)
		reads int // of comment 1002's reactions
	}{
		{"the App's, a bot's and an untrusted user's 👍", func(t testing.TB, f *githubtest.Fake) {
			f.AddReaction(1002, app, "+1")
			f.AddReaction(1002, "helper[bot]", "+1")
			f.AddReaction(1002, "mallory", "+1")
			f.AddReaction(1002, "owner", "-1")
		}, 1},
		{"a 👍 after a later transition", func(t testing.TB, f *githubtest.Fake) {
			f.AddComment(1, app, moved)
			f.AddReaction(1002, "owner", "+1")
		}, 0},
		{"a 👍 on an acknowledged error", func(t testing.TB, f *githubtest.Fake) {
			f.AddReaction(1002, "owner", "+1")
			f.AddComment(1, app, errAckReply("owner2", "disagreement", "9", 1))
		}, 0},
		{"a 👍 on an error a kindless ack covers", func(t testing.TB, f *githubtest.Fake) {
			f.AddReaction(1002, "owner", "+1")
			f.AddComment(1, app, "<!-- issuedog:error key=9 ack=r1 -->")
		}, 0},
		{"a 👍 on an ack of another kind", func(t testing.TB, f *githubtest.Fake) {
			f.AddComment(1, app, errAckReply("owner2", "transition", "9", 1))
		}, 1},
		{"a 👍 on an error in a user's comment", func(t testing.TB, f *githubtest.Fake) {
			f.AddReaction(f.AddComment(1, "owner2", errAckError), "owner", "+1")
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := errAckIssue(tc.more)(t)
			errAckRun(t, f, errAckPass(t))
			if w := f.Writes(); len(w) != 0 {
				t.Errorf("wrote %+v; want nothing", w)
			}
			if got := errAckReads(f.Requests(), 1002); got != tc.reads {
				t.Errorf("%d reads of comment 1002's reactions, want %d", got, tc.reads)
			}
		})
	}
}

// Removing the 👍 after the ack changes nothing, and a later 👍 adds no
// second ack.
func TestErrAckRemoved(t *testing.T) {
	f := errAckIssue(func(t testing.TB, f *githubtest.Fake) { flagWant(t, f.AddReaction(1002, "owner", "+1"), 1003) })(t)
	errAckRun(t, f, errAckPass(t))
	want := []string{"<!-- issuedog:transition from=intake to=ready run=rready -->", errAckError, errAckReply("owner", "disagreement", "9", 1003)}
	if got := commentBodies(f, 1); !slices.Equal(got, want) {
		t.Fatalf("comments %q; want %q", got, want)
	}
	if !f.RemoveReaction(1002, 1003) {
		t.Fatal("no 👍 to remove")
	}
	errAckRun(t, f, errAckPass(t))
	f.AddReaction(1002, "owner2", "+1")
	from := len(f.Requests())
	errAckRun(t, f, errAckPass(t))
	if got := commentBodies(f, 1); !slices.Equal(got, want) {
		t.Errorf("comments %q; want them unchanged", got)
	}
	if n := errAckReads(f.Requests()[from:], 1002); n != 0 {
		t.Errorf("%d reads of the acknowledged error's reactions, want none", n)
	}
}

// An acknowledged error is never posted again, even once its comment is
// dirty, so only the ack says it was posted. Here a 👍 on a task's revision
// asks for a move the table lacks (SPEC §5.3), and each pass tries it.
func TestErrAckNeverPostedAgain(t *testing.T) {
	f := newFake(t)
	n := f.AddIssue("owner", "Task", "body")
	f.SetType(n, "Task")
	f.AddLabel(n, app, "stage:spec-review")
	flagWant(t, f.AddComment(n, app, revision(1, "body")), 1001)
	flagWant(t, f.AddReaction(1001, "owner", "+1"), 1002)
	pass := func(f *githubtest.Fake) error {
		c := f.Client(t)
		s, err := c.Snapshot(context.Background(), "o", "r", n)
		if err != nil {
			return err
		}
		approve, ack := daemon.ApproveByReaction(errAckTrust), daemon.ErrorByReaction(errAckTrust)
		st := &daemon.Step{Applier: &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}, Trust: errAckTrust,
			Reactions: func(ctx context.Context, a *daemon.Applier, s *github.Snapshot, d daemon.Dirt) error {
				if err := approve(ctx, a, s, d); err != nil {
					return err
				}
				return ack(ctx, a, s, d)
			}}
		_, err = st.Run(context.Background(), s)
		return err
	}
	errAckRun(t, f, pass)
	got := commentBodies(f, n)
	if len(got) != 2 || !strings.Contains(got[1], "kind=transition key=r1002") {
		t.Fatalf("comments %q; want the revision, then the transition error keyed r1002", got)
	}
	errAckRun(t, f, pass)
	if got := commentBodies(f, n); len(got) != 2 {
		t.Fatalf("comments %q; want the error once", got)
	}
	flagWant(t, f.AddReaction(1003, "owner", "+1"), 1004)
	errAckRun(t, f, pass)
	f.EditComment(1003, "mallory", "nothing to see")
	errAckRun(t, f, pass)
	errAckRun(t, f, pass)
	got = commentBodies(f, n)
	if len(got) != 3 || got[2] != errAckReply("owner", "transition", "r1002", 1004) {
		t.Errorf("comments %q; want the revision, the error, now dirty, and its ack, and no second error", got)
	}
	if !strings.Contains(got[1], "issuedog:dirty by=mallory") {
		t.Errorf("the error comment is %q; want it noted dirty", got[1])
	}
}

// The loop runs step 1 on every pass on an issue with an open state error,
// since a 👍 changes no updated_at, and an unchanged pass over it makes
// only conditional reads. An issue whose error has ended, or was
// acknowledged, isn't visited.
func TestErrAckThroughTheLoop(t *testing.T) {
	f := newFake(t)
	fileAt(f, "ready", "body")
	flagWant(t, f.AddComment(1, app, errAckError), 1002)
	fileAt(f, "ready", "body")
	f.AddComment(2, app, errAckError)
	f.AddComment(2, app, "<!-- issuedog:transition from=ready to=ready run=again -->")
	fileAt(f, "ready", "body")
	var visits []int
	d := newDaemon(t, f, newOrigin(t), &visits)
	record, ack := d.Step.Reactions, daemon.ErrorByReaction(d.Step.CurrentTrust())
	d.Step.Reactions = func(ctx context.Context, a *daemon.Applier, s *github.Snapshot, dt daemon.Dirt) error {
		if err := record(ctx, a, s, dt); err != nil {
			return err
		}
		return ack(ctx, a, s, dt)
	}
	pass := func(want ...int) {
		t.Helper()
		visits = nil
		iterate(t, d)
		if !slices.Equal(visits, want) {
			t.Errorf("step 1 ran on %v, want %v", visits, want)
		}
	}
	pass(1, 2, 3)
	if w := f.Writes(); len(w) != 0 {
		t.Fatalf("the first pass wrote %+v; want nothing", w)
	}
	from := len(f.Requests())
	pass(1)
	reqs := f.Requests()[from:]
	for _, r := range reqs {
		if r.Method != "GET" || !r.Conditional || r.Status != 304 {
			t.Errorf("an unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}
	if errAckReads(reqs, 1002) != 1 {
		t.Errorf("an unchanged pass read the error's reactions %d times, want once", errAckReads(reqs, 1002))
	}
	r := f.AddReaction(1002, "owner", "+1")
	pass(1)
	want := []string{"<!-- issuedog:transition from=intake to=ready run=rready -->", errAckError, errAckReply("owner", "disagreement", "9", r)}
	if got := commentBodies(f, 1); !slices.Equal(got, want) {
		t.Fatalf("comments %q; want %q", got, want)
	}
	pass(1) // read again: the ack is a new comment
	pass()
	f.RemoveReaction(1002, r)
	pass()
	if got := commentBodies(f, 1); !slices.Equal(got, want) {
		t.Errorf("comments %q; want them unchanged", got)
	}
}

// An attempts error keys on step 1's inputs, which ErrorByReaction works
// out itself: a trusted user's clean comment, new input, ends it, so its
// 👍 is ignored and not read. Any other comment leaves it open.
func TestErrAckInputs(t *testing.T) {
	const attempts = "Failed.\n\n<!-- issuedog:error kind=attempts key=clarifying-1001 -->"
	for _, tc := range []struct {
		name  string
		more  func(f *githubtest.Fake)
		ended bool
	}{
		{"no input", func(*githubtest.Fake) {}, false},
		{"an untrusted user's comment", func(f *githubtest.Fake) { f.AddComment(1, "mallory", "more") }, false},
		{"a bot's comment", func(f *githubtest.Fake) { f.AddComment(1, "helper[bot]", "more") }, false},
		{"a dirty trusted comment", func(f *githubtest.Fake) { f.EditComment(f.AddComment(1, "owner2", "more"), "mallory", "less") }, false},
		{"a trusted comment", func(f *githubtest.Fake) { f.AddComment(1, "owner2", "more") }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			fileAt(f, "clarifying", "body")
			flagWant(t, f.AddComment(1, app, attempts), 1002)
			tc.more(f)
			r := f.AddReaction(1002, "owner", "+1")
			errAckRun(t, f, errAckPass(t))
			got := commentBodies(f, 1)
			acked := got[len(got)-1] == errAckReply("owner", "attempts", "clarifying-1001", r)
			if acked == tc.ended {
				t.Errorf("comments %q; want an ack: %v", got, !tc.ended)
			}
			if reads := errAckReads(f.Requests(), 1002); (reads == 0) != tc.ended {
				t.Errorf("%d reads of the error's reactions; want some: %v", reads, !tc.ended)
			}
		})
	}
}

// An error reading trust or reactions, or posting the ack, fails the
// trigger, wrapped.
func TestErrAckErrors(t *testing.T) {
	status := func(err error, code int) bool {
		var e *gogithub.ErrorResponse
		return errors.As(err, &e) && e.Response.StatusCode == code
	}
	const attempts = "Failed.\n\n<!-- issuedog:error kind=attempts key=clarifying-1001 -->"
	for _, tc := range []struct {
		name  string
		error string // the error comment
		trust github.Trust
		gone  bool // delete the error after the snapshot
		fail  int  // FailWrite's n
		want  func(error) bool
	}{
		{"trust of a 👍", errAckError, failTrust{}, false, 0, func(err error) bool {
			return errors.Is(err, errTrust) && strings.HasPrefix(err.Error(), "acknowledgments on o/r#1: trust owner: ")
		}},
		{"trust of an input", attempts, loginErrTrust{errAckTrust, "mallory"}, false, 0, func(err error) bool {
			return err != nil && strings.HasPrefix(err.Error(), "acknowledgments on o/r#1: open state errors of #1: inputs of #1: trust mallory: ")
		}},
		{"reactions", errAckError, errAckTrust, true, 0, func(err error) bool {
			return status(err, http.StatusNotFound) && strings.HasPrefix(err.Error(), "acknowledgments on o/r#1: ")
		}},
		{"the ack", errAckError, errAckTrust, false, 1, func(err error) bool {
			return status(err, http.StatusInternalServerError) && strings.HasPrefix(err.Error(), "acknowledgments on o/r#1: comment on o/r#1: ")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			fileAt(f, "clarifying", "body")
			flagWant(t, f.AddComment(1, app, tc.error), 1002)
			f.AddComment(1, "mallory", "more")
			f.AddReaction(1002, "owner", "+1")
			a, s, err := applier(t, f)
			if err != nil {
				t.Fatal(err)
			}
			if tc.gone {
				f.DeleteComment(1002)
			}
			f.FailWrite(tc.fail)
			if err := daemon.ErrorByReaction(tc.trust)(context.Background(), a, s, daemon.Dirt{}); !tc.want(err) {
				t.Errorf("err = %v, want it wrapped", err)
			}
		})
	}
}
