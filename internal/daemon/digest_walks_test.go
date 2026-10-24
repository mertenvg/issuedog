package daemon_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// digestWalksFake is o/r with G, #1, and C, #3, the owner's tasks at
// ready, and the pinned digest, #2, which someone made a sub-issue of G,
// and the parent of C.
func digestWalksFake(t testing.TB) *githubtest.Fake {
	t.Helper()
	f := newFake(t)
	for _, title := range []string{"G", "", "C"} {
		if title == "" {
			f.SetPinned(f.AddIssue(app, "issuedog digest", digestList), true)
			continue
		}
		n := f.AddIssue("owner", title, "A part.")
		f.AddLabel(n, "owner", "type:task")
		f.AddLabel(n, app, "stage:ready")
		f.AddComment(n, app, "<!-- issuedog:transition from=intake to=ready run=rready -->")
	}
	f.SetParent(2, 1)
	f.SetParent(3, 2)
	return f
}

// digestWalksStage fails t unless issue n of f has the one stage label
// want, and the digest, #2, has no label and no transition comment, as
// the walk down passes over it (SPEC §5.1, #527).
func digestWalksStage(t *testing.T, f *githubtest.Fake, when string, want string, ns ...int) {
	t.Helper()
	is := f.State().Issues
	for _, n := range ns {
		if got := is[n-1].Labels; !slices.Equal(got, []string{want, "type:task"}) {
			t.Errorf("%s: #%d's labels %q; want %s", when, n, got, want)
		}
	}
	if len(is[1].Labels) != 0 {
		t.Errorf("%s: the digest's labels %q; want none", when, is[1].Labels)
	}
	for _, c := range is[1].Comments {
		if strings.Contains(c.Body, "issuedog:transition") {
			t.Errorf("%s: the digest has the transition comment %q; want none", when, c.Body)
		}
	}
}

// The owner's /hold on G holds C, below the digest, which someone made
// G's sub-issue, and never the digest: the walk down passes over it, as
// the daemon never takes a snapshot of the digest (SPEC §5.1, #527). The
// /resume on G takes both back to ready, the digest untouched again, and
// an unchanged pass after costs no rate limit.
func TestDigestWalksHoldPassesOver(t *testing.T) {
	t.Parallel()
	f := digestWalksFake(t)
	l := newDiscRunLoop(t, f)
	digestMarkerTrustSettle(t, l)
	f.AddComment(1, "owner", "/hold")
	digestMarkerTrustSettle(t, l)
	digestWalksStage(t, f, "after the /hold", "stage:held", 1, 3)
	f.AddComment(1, "owner", "/resume")
	digestMarkerTrustSettle(t, l)
	digestWalksStage(t, f, "after the /resume", "stage:ready", 1, 3)
	for _, c := range f.State().Issues[0].Comments {
		if (c.Body == "/hold" || c.Body == "/resume") && !slices.Equal(c.Reactions, []string{app + " +1"}) {
			t.Errorf("the owner's %q has reactions %q; want the App's 👍", c.Body, c.Reactions)
		}
	}
	l.quiet("after the /resume")
}

// Replay passes for the owner's /hold on G of TestDigestWalksHoldPassesOver:
// the moves of G and C, with the digest passed over, and the digest's edit.
func TestDigestWalksHoldPassesOverReplay(t *testing.T) {
	t.Parallel()
	discRunReplay(t, digestWalksFake, nil, func(l *discRunLoop) {
		digestMarkerTrustSettle(t, l)
		l.f.AddComment(1, "owner", "/hold")
	})
}

// digestWalksMiddleFake is o/r with P, #1, A, #2, and G, #4, the owner's
// tasks at ready, and the pinned digest, #3, between A and G: P is A's
// parent, A the digest's, as someone made it, and the digest G's.
func digestWalksMiddleFake(t testing.TB) *githubtest.Fake {
	t.Helper()
	f := newFake(t)
	for _, title := range []string{"P", "A", "", "G"} {
		if title == "" {
			f.SetPinned(f.AddIssue(app, "issuedog digest", digestList), true)
			continue
		}
		n := f.AddIssue("owner", title, "A part.")
		f.AddLabel(n, "owner", "type:task")
		f.AddLabel(n, app, "stage:ready")
		f.AddComment(n, app, "<!-- issuedog:transition from=intake to=ready run=rready -->")
	}
	f.SetParent(2, 1)
	f.SetParent(3, 2)
	f.SetParent(4, 3)
	return f
}

// The owner's /hold on P, then on A, which writes no marker on G, held
// already, then /resume on P: the /resume never ends A's /hold, posted on
// P's child (SPEC §6.4), so A and G stay held, P alone back at ready. The
// path from G up to P passes over the digest, as the walk down did, so A
// stays on it (#527). The digest is never moved.
func TestDigestWalksResumeKeepsHoldBelow(t *testing.T) {
	t.Parallel()
	f := digestWalksMiddleFake(t)
	l := newDiscRunLoop(t, f)
	digestMarkerTrustSettle(t, l)
	f.AddComment(1, "owner", "/hold")
	digestMarkerTrustSettle(t, l)
	hold := f.AddComment(2, "owner", "/hold")
	digestMarkerTrustSettle(t, l)
	f.AddComment(1, "owner", "/resume")
	digestMarkerTrustSettle(t, l)
	is := f.State().Issues
	if got := is[0].Labels; !slices.Equal(got, []string{"stage:ready", "type:task"}) {
		t.Errorf("P's labels %q; want stage:ready", got)
	}
	for _, n := range []int{2, 4} {
		if got := is[n-1].Labels; !slices.Equal(got, []string{"stage:held", "type:task"}) {
			t.Errorf("#%d's labels %q; want stage:held, as A's /hold keeps it held", n, got)
		}
	}
	if len(is[2].Labels) != 0 {
		t.Errorf("the digest's labels %q; want none", is[2].Labels)
	}
	if c := newestComment(f, 4); !strings.Contains(c, "held="+strconv.FormatInt(hold, 10)+" heldon=2") || strings.Contains(c, "Resumed by") {
		t.Errorf("G's newest comment = %q; want it to name A's /hold, %d, and no resume", c, hold)
	}
	l.quiet("after the /resume")
}

// Mallory, a write collaborator, deletes the owner's comment on A during
// the hold that the owner's /hold on P began. It may have been a /hold on
// A that wrote no marker on G, held already (SPEC §6.4), so P's /resume,
// posted before it, doesn't end it, and G stays held: the path from G up
// to P passes over the digest, so the deletion on A is read (resolveGone,
// #527). A is held for the deletion's discussion too.
func TestDigestWalksResumeKeepsDeletionBelow(t *testing.T) {
	t.Parallel()
	f := digestWalksMiddleFake(t)
	note := f.AddComment(2, "owner", "A note.")
	l := newDiscRunLoop(t, f)
	digestMarkerTrustSettle(t, l)
	f.AddComment(1, "owner", "/hold")
	digestMarkerTrustSettle(t, l)
	f.DeleteCommentBy("mallory", note)
	digestMarkerTrustSettle(t, l)
	f.AddComment(1, "owner", "/resume")
	digestMarkerTrustSettle(t, l)
	is := f.State().Issues
	if got := is[0].Labels; !slices.Equal(got, []string{"stage:ready", "type:task"}) {
		t.Errorf("P's labels %q; want stage:ready", got)
	}
	for _, n := range []int{2, 4} {
		if got := is[n-1].Labels; !slices.Equal(got, []string{"stage:held", "type:task"}) {
			t.Errorf("#%d's labels %q; want stage:held, as the deletion on A may have been a /hold", n, got)
		}
	}
	if c := newestComment(f, 4); strings.Contains(c, "Resumed by") {
		t.Errorf("G's newest comment = %q; want no resume", c)
	}
	if len(is[2].Labels) != 0 {
		t.Errorf("the digest's labels %q; want none", is[2].Labels)
	}
	l.quiet("after the /resume")
}

// Replay passes for the /resume on P of TestDigestWalksResumeKeepsHoldBelow.
func TestDigestWalksResumeKeepsHoldBelowReplay(t *testing.T) {
	t.Parallel()
	discRunReplay(t, digestWalksMiddleFake, nil, func(l *discRunLoop) {
		digestMarkerTrustSettle(t, l)
		l.f.AddComment(1, "owner", "/hold")
		digestMarkerTrustSettle(t, l)
		l.f.AddComment(2, "owner", "/hold")
		digestMarkerTrustSettle(t, l)
		l.f.AddComment(1, "owner", "/resume")
	})
}
