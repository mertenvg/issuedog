package daemon_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// This file is #244: a comment deleted around a /resume on J, by an
// untrusted write collaborator, that may have been a /hold posted after the
// /resume, mustn't release J early. Only a /resume posted after the
// deletion ends the hold it may have been (SPEC §6.4).

// heldDeletionFamily is raceFamily with own, J and K held by J's own
// /hold, then A held by hand, settled, so a /hold on A writes no marker
// anywhere. It returns J's /hold.
func heldDeletionFamily(t testing.TB, order [3]int) (f *githubtest.Fake, a, j, k int, own int64) {
	t.Helper()
	f, a, j, k, own = raceFamily(t, order, true)
	f.RemoveLabel(a, "owner", "stage:ready")
	f.AddLabel(a, "owner", "stage:held")
	resolveSettle(t, f, 1, 2, 3)
	raceStages(t, f, "A held by hand", map[int]string{a: "held", j: "held", k: "held"})
	return f, a, j, k, own
}

// #205's verifier probe: the owner's /resume on J, then the owner's /hold
// on A, and A's pass handles the /hold, which writes no marker, as all
// three are held already. Mallory then deletes A's /hold before J's pass.
// The deletion, after J's /resume, may have been a /hold posted after it,
// so J stays held, as K does: J's /resume applies, ending J's own /hold on
// both, which it records, but not the hold the deletion may have been. A's
// /resume, posted after the deletion, ends that, so all three leave held.
func TestHeldDeletionLaterHold(t *testing.T) {
	t.Parallel()
	for _, order := range raceOrders {
		t.Run(fmt.Sprintf("A J K %v", order), func(t *testing.T) {
			t.Parallel()
			f, a, j, k, own := heldDeletionFamily(t, order)
			resume := f.AddComment(j, "owner", "/resume")
			hold := f.AddComment(a, "owner", "/hold")
			resolveSteps(t, f, a)
			f.DeleteCommentBy("mallory", hold)
			resolveSettle(t, f, 1, 2, 3)
			raceStages(t, f, "after the deletion", map[int]string{a: "held", j: "held", k: "held"})
			if anyComment(f, j, "rejected") {
				t.Errorf("#%d: newest comment %q; want J's /resume applied", j, newestComment(f, j))
			}
			for _, n := range []int{j, k} {
				if !anyComment(f, n, fmt.Sprintf("<!-- issuedog:ended reason=owner key=c%d cmd=%d -->", own, resume)) {
					t.Errorf("#%d: newest comment %q; want the record that J's /resume ended J's /hold", n, newestComment(f, n))
				}
			}
			want := "This `/resume` ended the holds on this issue from before it, but it stays on hold: a comment deleted after it, " +
				"on an issue above, may have been a `/hold`, which it doesn't end. `/resume` where that comment was deleted, or here, ends that."
			if !anyComment(f, j, want) {
				t.Errorf("#%d: newest comment %q; want %q", j, newestComment(f, j), want)
			}
			f.AddComment(a, "owner", "/resume")
			resolveSettle(t, f, 1, 2, 3)
			raceStages(t, f, "after A's /resume", map[int]string{a: "ready", j: "ready", k: "ready"})
		})
	}
}

// The probe with two /holds on A after J's /resume, which A's pass both
// handles, and mallory deletes one of them: J's record names both what
// keeps it held, the /hold left and the deletion, and A's /resume, after
// both, takes all three back to ready.
func TestHeldDeletionLaterHoldAndDeletion(t *testing.T) {
	t.Parallel()
	f, a, j, k, _ := heldDeletionFamily(t, raceOrders[0])
	f.AddComment(j, "owner", "/resume")
	f.AddComment(a, "owner", "/hold")
	gone := f.AddComment(a, "owner", "/hold")
	resolveSteps(t, f, a)
	f.DeleteCommentBy("mallory", gone)
	resolveSettle(t, f, 1, 2, 3)
	raceStages(t, f, "after the deletion", map[int]string{a: "held", j: "held", k: "held"})
	want := "This `/resume` ended the holds on this issue from before it, but it stays on hold by a `/hold` posted after it, " +
		"which it doesn't end, and a comment deleted after it, on an issue above, may have been another. " +
		"`/resume` where each was posted or deleted, or here, ends that."
	if !anyComment(f, j, want) {
		t.Errorf("#%d: newest comment %q; want %q", j, newestComment(f, j), want)
	}
	f.AddComment(a, "owner", "/resume")
	resolveSettle(t, f, 1, 2, 3)
	raceStages(t, f, "after A's /resume", map[int]string{a: "ready", j: "ready", k: "ready"})
}

// heldDeletionStalePass is racePass on issue n, with trust discTrust, but
// from n's snapshot read before between runs: as when a comment is posted
// on n during its pass, after its snapshot was read, so the pass doesn't
// see it, though what it reads of the other issues later does.
func heldDeletionStalePass(t testing.TB, f *githubtest.Fake, n int, between func()) {
	t.Helper()
	c := f.Client(t)
	s, err := c.Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	between()
	tr := discTrust{}
	st := &daemon.Step{Applier: &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug, Notify: []string{"owner"}}, Trust: tr,
		Commands: daemon.Commands(tr), Reactions: daemon.ResolveByReaction(tr)}
	if _, err := st.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

// heldDeletionRecorded is #1, A, at ready, and #2, J, under it, held for
// mallory's comment, which the owner's 👍 resolves. J's pass reads J
// before the owner's /resume on J is posted, then mallory deletes the
// owner's note on A: the pass reads A after that, so it finds the
// deletion, which may have been a /hold on A, and records it on J, which
// stays held (resolveAbove). It returns J's /resume.
func heldDeletionRecorded(t testing.TB) (*githubtest.Fake, int64) {
	t.Helper()
	f := discFake(t)  // #1, A
	file(f, 1, ready) // #2, J
	wait := f.AddComment(2, "mallory", "Wait.")
	resolveSettle(t, f, 1, 2)
	raceStages(t, f, "J held for discussion", map[int]string{1: "ready", 2: "held"})
	note := f.AddComment(1, "owner", "A note.")
	f.AddReaction(wait, "owner", "+1")
	var resume int64
	heldDeletionStalePass(t, f, 2, func() {
		resume = f.AddComment(2, "owner", "/resume")
		f.DeleteCommentBy("mallory", note)
	})
	raceStages(t, f, "after the record", map[int]string{2: "held"})
	if !anyComment(f, 2, " gone=1-") {
		t.Fatalf("#2: newest comment %q; want the record of the comment deleted on #1", newestComment(f, 2))
	}
	return f, resume
}

// J's own record, from before its /resume was handled, names a comment
// deleted on A after the /resume was posted: it may have been a /hold
// posted after the /resume, so the /resume doesn't end it on J itself,
// which stays held (SPEC §6.4). It still applies, ending the discussion
// hold before it, and says what keeps J held. A is held for discussion by
// the deletion too. A's /resume, posted after the deletion, ends both, so
// A goes back to ready, and J to clarifying, as a code owner resolved its
// discussion during the hold.
func TestHeldDeletionRecordedAfter(t *testing.T) {
	t.Parallel()
	f, resume := heldDeletionRecorded(t)
	resolveSettle(t, f, 1, 2)
	raceStages(t, f, "after J's /resume", map[int]string{1: "held", 2: "held"})
	if anyComment(f, 2, "rejected") {
		t.Errorf("#2: newest comment %q; want J's /resume applied", newestComment(f, 2))
	}
	want := "This `/resume` ended the holds on this issue from before it, but it stays on hold: a comment deleted after it, " +
		"on an issue above, may have been a `/hold`, which it doesn't end. `/resume` where that comment was deleted, or here, ends that."
	if !anyComment(f, 2, want) || !anyComment(f, 2, fmt.Sprintf("<!-- issuedog:ended reason=discussion cmd=%d ", resume)) {
		t.Errorf("#2: newest comment %q; want J's /resume's record, saying what keeps it held", newestComment(f, 2))
	}
	f.AddComment(1, "owner", "/resume")
	resolveSettle(t, f, 1, 2)
	raceStages(t, f, "after A's /resume", map[int]string{1: "ready", 2: "clarifying"})
}

// heldDeletionProbe is #205's verifier probe up to J's pass, A #1, J #2
// and K #3 (TestHeldDeletionLaterHold), with the writes forgotten.
func heldDeletionProbe(t testing.TB) *githubtest.Fake {
	t.Helper()
	f, a, j, _, _ := heldDeletionFamily(t, raceOrders[0])
	f.AddComment(j, "owner", "/resume")
	hold := f.AddComment(a, "owner", "/hold")
	resolveSteps(t, f, a)
	f.DeleteCommentBy("mallory", hold)
	f.ForgetWrites()
	return f
}

// J's pass on its /resume is safe to replay after a crash at any write,
// in #205's verifier probe, with its records on K and J and its 👍, and
// with J's own record of the comment deleted after the /resume, with its
// record on J and its 👍: J stays held either way.
func TestHeldDeletionReplay(t *testing.T) {
	t.Parallel()
	recorded := func(t testing.TB) *githubtest.Fake {
		f, _ := heldDeletionRecorded(t)
		f.ForgetWrites()
		return f
	}
	for name, setup := range map[string]func(testing.TB) *githubtest.Fake{"the probe": heldDeletionProbe, "J's own record": recorded} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			githubtest.Replay(t, setup, resolvePass(t, 2))
			f := run(t, setup, resolvePass(t, 2))
			raceStages(t, f, "after J's pass", map[int]string{2: "held"})
			if !anyComment(f, 2, "but it stays on hold: a comment deleted after it, on an issue above, may have been a `/hold`") {
				t.Errorf("#2: newest comment %q; want J's /resume's record", newestComment(f, 2))
			}
		})
	}
}

// J's pass on its /resume asks about mallory, who deleted A's /hold after
// it. Once the leave path's record names that deletion (resolveAbove), as
// the pass after writes it, J's pass neither asks about her nor reads
// anything of A, J's parent: the /resume's reads of J's ancestors come
// only on a pass with a /resume on J to handle.
func TestHeldDeletionReadsOnlyOnResume(t *testing.T) {
	t.Parallel()
	f := heldDeletionProbe(t)
	tr := &raceCounting{asked: map[string]int{}}
	racePass(t, f, 2, tr)
	raceStages(t, f, "after J's /resume", map[int]string{1: "held", 2: "held", 3: "held"})
	if tr.asked["mallory"] == 0 {
		t.Errorf("J's pass on its /resume asked about mallory never; want her deletion on A read")
	}
	resolveSettle(t, f, 1, 2, 3)
	from := len(f.Requests())
	tr = &raceCounting{asked: map[string]int{}}
	racePass(t, f, 2, tr)
	if n := tr.asked["mallory"]; n != 0 {
		t.Errorf("J's pass with no /resume asked about mallory %d times; want none", n)
	}
	for _, r := range f.Requests()[from:] {
		if strings.HasPrefix(r.Path, "/issues/1/") || r.Path == "/issues/1" {
			t.Errorf("J's pass with no /resume sent %+v; want nothing of A read", r)
		}
	}
}

// heldDeletionSettle runs l's Iterates until one writes nothing.
func heldDeletionSettle(t *testing.T, l *discRunLoop, name string) {
	t.Helper()
	for i := 0; ; i++ {
		from := len(l.f.Writes())
		if err := l.g.d.Iterate(context.Background()); err != nil {
			t.Fatalf("%s: Iterate: %v", name, err)
		}
		if len(l.f.Writes()) == from {
			return
		}
		if i >= discRunSettle {
			t.Fatalf("%s: still writing after %d Iterates", name, i+1)
		}
	}
}

// #205's verifier probe as a whole run, with J, #2, numbered below A, #3,
// and K #4. J's /resume waits for the owner's /hold on A posted before it
// (#243), which A's pass handles, then the owner's /hold on A posted after
// it, writing no marker, as all three are held already. Mallory deletes
// that one before the next pass, which handles J's /resume: J and K stay
// held, as the deletion may have been a /hold posted after it. Every pass
// after is unchanged, a restart's too, and A's /resume, posted after the
// deletion, takes all three back to ready.
func TestHeldDeletionRun(t *testing.T) {
	t.Parallel()
	f := discRunRaceFamily(false)(t)
	a, j, k := discRunRaceIssues(false)
	l := newDiscRunLoop(t, f)
	discRunRaceHeld(t, l, false)
	l.quiet("J and K held")
	f.RemoveLabel(a, "owner", "stage:ready")
	f.AddLabel(a, "owner", "stage:held")
	heldDeletionSettle(t, l, "A held by hand")
	l.quiet("A held by hand")
	early := f.AddComment(a, "owner", "/hold")
	resume := f.AddComment(j, "owner", "/resume")
	hold := f.AddComment(a, "owner", "/hold")
	l.step("J's /resume waits, and A's /holds", discRunReact(early), discRunReact(hold))
	f.DeleteCommentBy("mallory", hold)
	heldDeletionSettle(t, l, "the deletion")
	for _, n := range []int{a, j, k} {
		if got := ivLabels(f, n); !slices.Equal(got, []string{"stage:held", "type:task"}) {
			t.Errorf("#%d: labels %q, newest comment %q; want it held", n, got, newestComment(f, n))
		}
	}
	if !anyComment(f, j, fmt.Sprintf(" cmd=%d ", resume)) || anyComment(f, j, "rejected") {
		t.Errorf("#%d: newest comment %q; want J's /resume applied", j, newestComment(f, j))
	}
	l.quiet("after the deletion")
	l.restart("after the deletion")
	f.AddComment(a, "owner", "/resume")
	heldDeletionSettle(t, l, "A's /resume")
	for _, n := range []int{a, j, k} {
		if got := ivLabels(f, n); !slices.Equal(got, []string{"stage:ready", "type:task"}) {
			t.Errorf("#%d: labels %q, newest comment %q; want it back at ready", n, got, newestComment(f, n))
		}
	}
	l.quiet("resumed")
}

// A comment deleted on A after J's /resume counts as a /hold J's /resume
// may not end only when an untrusted write collaborator deleted a comment
// by someone else who is trusted, or whom GitHub doesn't name
// (resolveGoneMay): not when a trusted user deletes it, nor when its
// author does, on J's own read as on the issues below it. So in every
// order of their numbers, J's /resume ends J's own /hold on J and K, which
// go back to ready, and A stays held by hand. A ghost's note, whose author
// GitHub doesn't name, would count if mallory deleted it, so the owner's
// deletion of it turns on her trust alone. Mallory's deletion of it, the
// control, keeps J and K held, as it may have been a /hold posted after
// J's /resume, which still applies.
func TestHeldDeletionTrustedOnOwnRead(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		del  func(f *githubtest.Fake, a int)
		held bool
	}{
		"the owner deletes her own note":   {func(f *githubtest.Fake, a int) { f.DeleteCommentBy("owner", f.AddComment(a, "owner", "A note.")) }, false},
		"the owner deletes a ghost's note": {func(f *githubtest.Fake, a int) { f.DeleteCommentBy("owner", f.AddComment(a, "", "A ghost's note.")) }, false},
		"the owner deletes mallory's":      {func(f *githubtest.Fake, a int) { f.DeleteCommentBy("owner", f.AddComment(a, "mallory", "A note.")) }, false},
		"mallory deletes her own":          {func(f *githubtest.Fake, a int) { f.DeleteCommentBy("mallory", f.AddComment(a, "mallory", "A note.")) }, false},
		"mallory deletes a ghost's note":   {func(f *githubtest.Fake, a int) { f.DeleteCommentBy("mallory", f.AddComment(a, "", "A ghost's note.")) }, true},
	} {
		for _, order := range raceOrders {
			t.Run(fmt.Sprintf("%s, A J K %v", name, order), func(t *testing.T) {
				t.Parallel()
				f, a, j, k, _ := heldDeletionFamily(t, order)
				resume := f.AddComment(j, "owner", "/resume")
				tt.del(f, a)
				resolveSettle(t, f, 1, 2, 3)
				want := "ready"
				if tt.held {
					want = "held"
				}
				raceStages(t, f, "after J's /resume", map[int]string{a: "held", j: want, k: want})
				if anyComment(f, j, "rejected") || !anyComment(f, j, fmt.Sprintf(" cmd=%d ", resume)) {
					t.Errorf("#%d: newest comment %q; want J's /resume applied", j, newestComment(f, j))
				}
			})
		}
	}
}
