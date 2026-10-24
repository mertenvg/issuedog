package daemon_test

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// These tests pin what epic #1's verifier found untested (#228): the
// disagreement and missing-transition digest items, through the whole
// loop. TestDigestRunWhole lists both kinds' items, but from error comments
// its fixture plants. Here the loop finds the cause itself, posts the state
// error, then lists it in the digest and announces it, once, with one
// mention, as daemon.NewStep wires step 1 (digestRunDaemon). A 👍 on the
// error acknowledges it, and its item goes.

// phase1PinFake is o/r with mertenvg, digestRunOwner, an admin.
func phase1PinFake(t testing.TB) *githubtest.Fake {
	f := newFake(t)
	f.SetPermission(digestRunOwner, "admin")
	return f
}

// phase1PinReady files an issue by mertenvg with labels, at ready, as the App
// moved it there, and returns its number.
func phase1PinReady(f *githubtest.Fake, title string, labels ...string) int {
	n := f.AddIssue(digestRunOwner, title, "Do it.", labels...)
	f.AddLabel(n, app, "stage:ready")
	f.AddComment(n, app, "<!-- issuedog:transition from=intake to=ready run=rready -->")
	return n
}

// phase1PinAnnounced checks that the digest, issue digest, has exactly one
// announcement, which names key and mentions mertenvg once, and that its
// items are want, as digestRunItems gives them.
func phase1PinAnnounced(t *testing.T, f *githubtest.Fake, digest int, key string, want ...string) {
	t.Helper()
	news := digestComments(f, digest)
	if len(news) != 1 || strings.Count(news[0], "@"+digestRunOwner) != 1 ||
		!strings.HasSuffix(news[0], "\n\ncc @"+digestRunOwner+"\n\n<!-- issuedog:digest-new items="+key+" -->") {
		t.Errorf("the digest's comments = %q; want one announcement of %s, with one mention", news, key)
	}
	if got := digestRunItems(f.State().Issues[digest-1].Body); !slices.Equal(got, want) {
		t.Errorf("items = %q; want %q", got, want)
	}
}

// phase1PinAck 👍s, as mertenvg, the state error kind with key on issue n,
// and checks that the next pass posts the ack, then edits the digest,
// whose item for it goes, and that the pass after writes nothing.
func phase1PinAck(t *testing.T, f *githubtest.Fake, d *daemon.Daemon, n, digest int, kind, key string) {
	t.Helper()
	id := digestRunCommentID(t, f, n, fmt.Sprintf("<!-- issuedog:error kind=%s key=%s -->", kind, key))
	r := f.AddReaction(id, digestRunOwner, "+1")
	digestRunStep(t, f, d, "the 👍 on the error", fmt.Sprintf("POST /issues/%d/comments", n), fmt.Sprintf("PATCH /issues/%d", digest))
	if got, want := newestComment(f, n), errAckReply(digestRunOwner, kind, key, r); got != want {
		t.Errorf("newest comment on #%d = %q; want the ack, %q", n, got, want)
	}
	if got := digestRunItems(f.State().Issues[digest-1].Body); len(got) != 0 {
		t.Errorf("items after the ack = %q; want none", got)
	}
	digestRunStep(t, f, d, "an extra pass after the ack")
	if n := digestRunNews(f, digest); n != 1 {
		t.Errorf("%d announcements; want still one", n)
	}
}

// TestPhase1PinDigestDisagreement: mertenvg removes an issue's stage label
// by hand. The next pass finds the disagreement, posts its state error
// keyed by the removal's event, puts the label back, lists the error in
// the digest and announces it.
func TestPhase1PinDigestDisagreement(t *testing.T) {
	t.Parallel()
	f := phase1PinFake(t)
	n := phase1PinReady(f, "Broken sync")
	d := digestRunDaemon(t, f, digestRunOrigin(t), nil, nil)
	digest := n + 1
	digestRunStep(t, f, d, "the first pass", "POST /issues", "POST /graphql", fmt.Sprintf("PATCH /issues/%d", digest))
	digestRunStep(t, f, d, "an extra pass")

	ev := f.RemoveLabel(n, digestRunOwner, "stage:ready")
	key := strconv.FormatInt(ev, 10)
	digestRunStep(t, f, d, "the removal", fmt.Sprintf("POST /issues/%d/comments", n), fmt.Sprintf("POST /issues/%d/labels", n),
		fmt.Sprintf("POST /issues/%d/comments", digest), fmt.Sprintf("PATCH /issues/%d", digest))
	if got, want := newestComment(f, n), "The stage labels didn't match this issue's latest transition, to `ready`, so I've set them to match. "+
		"They were: none.\n\n<!-- issuedog:error kind=disagreement key="+key+" -->"; got != want {
		t.Errorf("newest comment on #%d = %q; want the disagreement, %q", n, got, want)
	}
	if got := ivLabels(f, n); !slices.Equal(got, []string{"stage:ready"}) {
		t.Errorf("labels %v; want stage:ready put back", got)
	}
	phase1PinAnnounced(t, f, digest, fmt.Sprintf("err-%d-disagreement-%s", n, key), fmt.Sprintf("#%d State error p2 1", n))
	digestRunStep(t, f, d, "an extra pass after the removal")
	phase1PinAck(t, f, d, n, digest, "disagreement", key)
}

// TestPhase1PinDigestTransition: mertenvg holds a parent and its child,
// then removes the child's type, and /resumes the parent. The table has no
// row back for an untyped issue, so the /resume posts the child's state
// error, keyed by the command, and the child stays held. The next pass
// lists the error in the digest and announces it. That pass reads the
// child again both because the command touched it and because its new
// comment puts it in the poll, so this test doesn't show which. The
// touch's own re-read is pinned by tests whose touched issue the poll
// doesn't list.
func TestPhase1PinDigestTransition(t *testing.T) {
	t.Parallel()
	f := phase1PinFake(t)
	parent := phase1PinReady(f, "Quotes", "type:task")
	child := phase1PinReady(f, "Part", "type:task")
	f.SetParent(child, parent)
	d := digestRunDaemon(t, f, digestRunOrigin(t), nil, nil)
	digest := child + 1
	digestRunStep(t, f, d, "the first pass", "POST /issues", "POST /graphql", fmt.Sprintf("PATCH /issues/%d", digest))
	f.AddComment(parent, digestRunOwner, "/hold")
	iterate(t, d)
	digestRunStep(t, f, d, "an extra pass after the /hold")
	if got := ivLabels(f, child); !slices.Equal(got, []string{"stage:held", "type:task"}) {
		t.Fatalf("the child's labels %v after the /hold; want stage:held", got)
	}
	f.RemoveLabel(child, digestRunOwner, "type:task")
	digestRunStep(t, f, d, "the removal of the child's type")

	resume := f.AddComment(parent, digestRunOwner, "/resume")
	key := strconv.FormatInt(resume, 10)
	digestRunStep(t, f, d, "the /resume", fmt.Sprintf("POST /issues/%d/comments", child),
		fmt.Sprintf("POST /issues/%d/comments", parent), fmt.Sprintf("POST /issues/%d/labels", parent),
		fmt.Sprintf("DELETE /issues/%d/labels/stage:held", parent), fmt.Sprintf("POST /issues/comments/%d/reactions", resume))
	if got, want := newestComment(f, child), "I can't move this issue from `held` to `ready` on resume: the transition table (SPEC §7.2) "+
		"has no such row for a untyped issue. It stays at `held`.\n\n<!-- issuedog:error kind=transition key="+key+" -->"; got != want {
		t.Errorf("newest comment on #%d = %q; want the transition error, %q", child, got, want)
	}
	if got := ivLabels(f, child); !slices.Equal(got, []string{"stage:held"}) {
		t.Errorf("the child's labels %v; want it still held", got)
	}
	digestRunStep(t, f, d, "the pass after the /resume", fmt.Sprintf("POST /issues/%d/comments", digest), fmt.Sprintf("PATCH /issues/%d", digest))
	phase1PinAnnounced(t, f, digest, fmt.Sprintf("err-%d-transition-%s", child, key), fmt.Sprintf("#%d State error p2 1", child))
	digestRunStep(t, f, d, "an extra pass after the error")
	phase1PinAck(t, f, d, child, digest, "transition", key)
}

// phase1PinPass is a pass of a new daemon on f, as after a restart: two
// Iterates, so an issue the first one's command wrote to is read again,
// and its items listed, in the second.
func phase1PinPass(t *testing.T) func(f *githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		d := digestRunDaemon(t, f, digestRunOrigin(t), nil, nil)
		for range 2 {
			if err := d.Iterate(context.Background()); err != nil {
				return err
			}
		}
		return nil
	}
}

// phase1PinDisagreed files issue 1 at ready, whose stage label mertenvg
// has removed by hand, and returns the removal's event ID.
func phase1PinDisagreed(t testing.TB) (*githubtest.Fake, int64) {
	f := phase1PinFake(t)
	n := phase1PinReady(f, "Broken sync")
	return f, f.RemoveLabel(n, digestRunOwner, "stage:ready")
}

// phase1PinResumed files task 1 and its untyped child, 2, both held by
// mertenvg's /hold on 1, as the loop held them, then mertenvg's /resume on
// 1, which it returns the ID of.
func phase1PinResumed(t testing.TB) (*githubtest.Fake, int64) {
	f := phase1PinFake(t)
	parent := phase1PinReady(f, "Quotes", "type:task")
	child := phase1PinReady(f, "Part")
	f.SetParent(child, parent)
	hold := f.AddComment(parent, digestRunOwner, "/hold")
	f.AddReaction(hold, app, "+1")
	// The /hold's writes, as the loop made them, the child's first; seen
	// is each issue's newest comment before the hold.
	for _, h := range []struct {
		n        int
		on, seen string
	}{{child, fmt.Sprintf(" on #%d", parent), "1002"}, {parent, "", "1001"}} {
		f.AddComment(h.n, app, fmt.Sprintf("On hold by `/hold`%s, from `ready`.\n\n"+
			"<!-- issuedog:transition from=ready to=held at=ready reason=owner cmd=%d seen=%s -->", h.on, hold, h.seen))
		f.AddLabel(h.n, app, "stage:held")
		f.RemoveLabel(h.n, app, "stage:ready")
	}
	return f, f.AddComment(parent, digestRunOwner, "/resume")
}

// TestPhase1PinDigestDisagreementReplay: a new daemon's pass that finds
// the disagreement, repairs it, and creates the digest that lists and
// announces it is safe to replay after a crash at any write.
func TestPhase1PinDigestDisagreementReplay(t *testing.T) {
	t.Parallel()
	f, ev := phase1PinDisagreed(t)
	if err := phase1PinPass(t)(f); err != nil {
		t.Fatal(err)
	}
	key := strconv.FormatInt(ev, 10)
	if got := ivLabels(f, 1); !slices.Equal(got, []string{"stage:ready"}) ||
		!strings.HasSuffix(newestComment(f, 1), "<!-- issuedog:error kind=disagreement key="+key+" -->") {
		t.Fatalf("labels %v, newest comment %q; want the disagreement reported and repaired", got, newestComment(f, 1))
	}
	phase1PinAnnounced(t, f, 2, "err-1-disagreement-"+key, "#1 State error p2 1")
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f, _ := phase1PinDisagreed(tb)
		return f
	}, phase1PinPass(t))
}

// TestPhase1PinDigestTransitionReplay: a new daemon's pass, in which the
// /resume posts the child's transition error, then the digest lists and
// announces it, is safe to replay after a crash at any write.
func TestPhase1PinDigestTransitionReplay(t *testing.T) {
	t.Parallel()
	f, resume := phase1PinResumed(t)
	if err := phase1PinPass(t)(f); err != nil {
		t.Fatal(err)
	}
	key := strconv.FormatInt(resume, 10)
	if got := ivLabels(f, 2); !slices.Equal(got, []string{"stage:held"}) ||
		!strings.HasSuffix(newestComment(f, 2), "<!-- issuedog:error kind=transition key="+key+" -->") {
		t.Fatalf("the child's labels %v, newest comment %q; want it held, with the /resume's transition error", got, newestComment(f, 2))
	}
	phase1PinAnnounced(t, f, 3, "err-2-transition-"+key, "#2 State error p2 1")
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f, _ := phase1PinResumed(tb)
		return f
	}, phase1PinPass(t))
}
