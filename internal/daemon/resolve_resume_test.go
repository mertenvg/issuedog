package daemon_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// resolveSelfResumed is the record /resume writes on its own issue when a
// trigger made after it keeps the issue held, resolved at its time.
func resolveSelfResumed(resolved int64, resume int64) string {
	return fmt.Sprintf("This `/resume` ended the holds on this issue from before it, but it stays on hold for discussion: "+
		"a write collaborator acted after the `/resume` was posted. A code owner's response to that, or another `/resume` here, ends it."+
		"\n\n<!-- issuedog:ended reason=discussion cmd=%d resolved=%d -->", resume, resolved)
}

// A /resume accepts only the triggers made before it (#188's verify,
// attempt 1): a rename, or a deletion or edit of the owner's comment,
// made after the owner's /resume but before the App's pass, gets its
// notice, and keeps the issue held, with the record that the /resume
// ended the rest. The next pass writes nothing, and once a code owner
// accepts the act, the issue leaves the hold.
func TestResolveResumeThenLaterAct(t *testing.T) {
	t.Parallel()
	for name, act := range map[string]func(f *githubtest.Fake, x int64){
		"a rename":                      func(f *githubtest.Fake, _ int64) { f.RenameIssue(1, "mallory", "Renamed") },
		"a deletion of owner's comment": func(f *githubtest.Fake, x int64) { f.DeleteCommentBy("mallory", x) },
		"an edit of owner's comment":    func(f *githubtest.Fake, x int64) { f.EditComment(x, "mallory", "Don't ship it.") },
	} {
		t.Run(name, func(t *testing.T) {
			var resume int64
			setup := func(t testing.TB) *githubtest.Fake {
				f := discFake(t)
				x := f.AddComment(1, "owner", "Ship it.") // 1002
				f.AddComment(1, "mallory", "Wait.")       // 1003
				if err := discPass(t, 1)(f); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(newestComment(f, 1), "event=c1003 ") {
					t.Fatalf("newest comment %q; want the hold for comment 1003", newestComment(f, 1))
				}
				f.ForgetWrites()
				resume = f.AddComment(1, "owner", "/resume")
				act(f, x)
				return f
			}
			githubtest.Replay(t, setup, resolvePass(t, 1))
			f := run(t, setup, resolvePass(t, 1))
			is := f.State().Issues[0]
			var added []string
			for _, c := range is.Comments[len(setup(t).State().Issues[0].Comments):] {
				added = append(added, c.Body)
			}
			record := resolveSelfResumed(resolveCommentAt(t, f, 1, resume), resume)
			if !slices.Equal(is.Labels, []string{"stage:held"}) || len(added) != 2 || !strings.Contains(added[0], "issuedog:notice key=") || added[1] != record {
				t.Fatalf("labels %q, new comments %q; want the issue held, the act's notice, then %q", is.Labels, added, record)
			}
			// The notice accepted, the issue leaves: no owner hold is left.
			// But a deletion during the hold may have hidden a code owner's
			// /hold, so only /resume accepts it (resolveHitsApp, #188's
			// verify, round 2).
			ids := commentIDs(t, f, 1)
			f.AddReaction(ids[len(ids)-2], "owner", "+1")
			if err := resolvePass(t, 1)(f); err != nil {
				t.Fatal(err)
			}
			if name == "a deletion of owner's comment" {
				if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:held"}) {
					t.Fatalf("labels %q, newest comment %q; want the issue held, as only /resume accepts the deletion", l, newestComment(f, 1))
				}
				f.AddComment(1, "owner", "/resume")
				if err := resolvePass(t, 1)(f); err != nil {
					t.Fatal(err)
				}
				if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:ready"}) {
					t.Errorf("labels %q, newest comment %q; want the second /resume to restore ready", l, newestComment(f, 1))
				}
				return
			}
			if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:clarifying"}) {
				t.Errorf("labels %q, newest comment %q; want the issue at clarifying once the act is accepted", l, newestComment(f, 1))
			}
		})
	}
}

// An edit made after the /resume stays open once known from its record
// alone, by the time its key gives: edited again, its notice still needs
// its own 👍, after the newer edit's.
func TestResolveResumeThenEditedTwice(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	x := f.AddComment(1, "owner", "Ship it.") // 1002
	f.AddComment(1, "mallory", "Wait.")       // 1003
	if err := discPass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(1, "owner", "/resume")
	f.EditComment(x, "mallory", "Don't ship it.")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.EditComment(x, "mallory", "Never ship it.")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	ids := commentIDs(t, f, 1)
	f.AddReaction(ids[len(ids)-1], "owner", "+1") // the newer edit's notice
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:held"}) || len(discNoticeKeys(f, 1)) != 2 {
		t.Errorf("labels %q, notices %q; want the issue held for the older edit, with two notices", l, discNoticeKeys(f, 1))
	}
}

// A comment in the same window holds the issue again too, the trigger
// open after the /resume: it isn't countered by the /resume, which
// counters nothing.
func TestResolveResumeThenLaterComment(t *testing.T) {
	t.Parallel()
	f := resolveHeldWith(func(_ testing.TB, f *githubtest.Fake) {})(t)
	resume := f.AddComment(1, "owner", "/resume")
	late := f.AddComment(1, "mallory", "One more thing.")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) ||
		body != resolveSelfResumed(resolveCommentAt(t, f, 1, resume), resume) {
		t.Errorf("labels %q, newest comment %q; want the issue held for comment %d, with the record", l, body, late)
	}
}

// A comment the owner countered, whose author isn't a writer, as trudy,
// who can only read, ended no discussion: a /resume in the same pass ends
// the owner's hold back to at, not to clarifying.
func TestResolveResumeAfterNonWriter(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	f.SetPermission("trudy", "read")
	f.AddComment(1, "owner", "/hold")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(1, "trudy", "Looks risky.")
	f.AddComment(1, "owner", "It's fine.")
	resume := f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:ready"}) ||
		!strings.Contains(body, fmt.Sprintf("from=held to=ready cmd=%d ", resume)) {
		t.Errorf("labels %q, newest comment %q; want the /resume back to ready", l, body)
	}
}

// resolveResumeTwice posts /resume on issue 1 of f, then mallory's comment
// before the pass, which keeps it held with the /resume's record, then a
// second /resume, by passes, and returns that second /resume's ID.
func resolveResumeTwice(t *testing.T, f *githubtest.Fake) int64 {
	t.Helper()
	f.AddComment(1, "owner", "/resume")
	f.AddComment(1, "mallory", "One more thing.")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) || !strings.Contains(body, "cmd=") {
		t.Fatalf("labels %q, newest comment %q; want the issue held, with the /resume's record", l, body)
	}
	resume := f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	return resume
}

// A second /resume, after the first kept the issue held for a later
// trigger, restores at (#188's verify, attempt 2): the first one's record
// says no discussion ended, as that /resume itself went back to at. So it
// is from a discussion hold, and from an owner /hold.
func TestResolveSecondResume(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(t testing.TB) *githubtest.Fake{
		"a discussion hold": resolveHeldWith(func(_ testing.TB, _ *githubtest.Fake) {}),
		"an owner /hold": func(t testing.TB) *githubtest.Fake {
			f := discFake(t)
			f.AddComment(1, "owner", "/hold")
			if err := resolvePass(t, 1)(f); err != nil {
				t.Fatal(err)
			}
			return f
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			resume := resolveResumeTwice(t, f)
			if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:ready"}) ||
				!strings.Contains(body, fmt.Sprintf("Resumed by `/resume`, back to `ready`.\n\n<!-- issuedog:transition from=held to=ready cmd=%d ", resume)) {
				t.Errorf("labels %q, newest comment %q; want the second /resume back to ready", l, body)
			}
		})
	}
}

// A first /resume that would itself have gone to clarifying, a discussion
// resolved while the owner's hold stayed, carries that in its record
// (after=1), so the second /resume goes to clarifying.
func TestResolveSecondResumeAfterDiscussion(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	f.AddComment(1, "owner", "/hold")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	w := f.AddComment(1, "mallory", "Wait.")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.AddReaction(w, "owner", "+1")
	resume := resolveResumeTwice(t, f)
	st := f.State().Issues[0]
	var record string
	for _, c := range st.Comments {
		if strings.Contains(c.Body, "issuedog:ended reason=discussion") {
			record = c.Body
		}
	}
	if !strings.Contains(record, "after=1") || !slices.Equal(st.Labels, []string{"stage:clarifying"}) ||
		!strings.Contains(newestComment(f, 1), fmt.Sprintf("from=held to=clarifying at=ready origin=ready cmd=%d ", resume)) {
		t.Errorf("record %q, labels %q, newest comment %q; want the first /resume's record with after=1, and the second to clarifying",
			record, st.Labels, newestComment(f, 1))
	}
}

// A 👍 given after the /resume, on a trigger made before it, doesn't count
// as a discussion that ended while the owner's hold stayed: the /resume
// accepts the trigger itself, and restores at.
func TestResolveThumbAfterResume(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	f.AddComment(1, "owner", "/hold")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	w := f.AddComment(1, "mallory", "Wait.")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	resume := f.AddComment(1, "owner", "/resume")
	f.AddReaction(w, "owner", "+1")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:ready"}) ||
		!strings.Contains(body, fmt.Sprintf("from=held to=ready cmd=%d ", resume)) {
		t.Errorf("labels %q, newest comment %q; want the /resume back to ready", l, body)
	}
}

// Replay over the record of a /resume that would have gone to clarifying,
// kept held by a later trigger: after=1.
func TestResolveAfterRecordReplay(t *testing.T) {
	t.Parallel()
	setup := func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		f.AddComment(1, "owner", "/hold")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		w := f.AddComment(1, "mallory", "Wait.")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		f.ForgetWrites()
		f.AddReaction(w, "owner", "+1")
		f.AddComment(1, "owner", "/resume")
		f.AddComment(1, "mallory", "One more thing.")
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 1))
	f := run(t, setup, resolvePass(t, 1))
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) || !strings.Contains(body, " after=1 ") {
		t.Errorf("labels %q, newest comment %q; want the issue held, with the record carrying after=1", l, body)
	}
}

// resumeCheck checks the row the /resume will take: for an untyped issue
// held from ready, whose discussion ended while the owner's hold stayed,
// the row to clarifying, which it has, not the one back to ready, which
// it lacks.
func TestResolveResumeRowToClarifying(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	f.AddComment(1, "owner", "/hold")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	w := f.AddComment(1, "mallory", "Wait.")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.AddReaction(w, "owner", "+1")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.SetType(1, "")
	resume := f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:clarifying"}) ||
		!strings.Contains(body, fmt.Sprintf("from=held to=clarifying at=ready origin=ready cmd=%d ", resume)) {
		t.Errorf("labels %q, newest comment %q; want the /resume to clarifying", l, body)
	}
}
