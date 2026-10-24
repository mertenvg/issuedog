package daemon_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// heldWordingHere is resumeCheck's reason for a /resume on an issue that
// stays on hold for discussion, posted on that issue (#222).
const heldWordingHere = "this issue is on hold for discussion, which only a code owner's response here, or `/resume` here, ends"

// #222: a /resume on #1, with mallory's comment after it seen in the same
// pass, ends nothing, as #1's discussion hold is for a trigger made after
// it: the rejection, posted on #1, says "here", not "#1 … there". With a
// child #2 held for discussion too, it names #2 first, with "there".
// Replay passes for each, and a second pass writes nothing.
func TestHeldWordingResumeHere(t *testing.T) {
	t.Parallel()
	for _, child := range []bool{false, true} {
		t.Run(fmt.Sprintf("a child held for discussion %t", child), func(t *testing.T) {
			t.Parallel()
			var resume int64
			setup := func(t testing.TB) *githubtest.Fake {
				f := discFake(t)
				if child {
					file(f, 1, ready)
					f.AddComment(2, "mallory", "Wait.")
					if err := resolvePass(t, 2)(f); err != nil {
						t.Fatal(err)
					}
					if l := f.State().Issues[1].Labels; !slices.Equal(l, []string{"stage:held"}) {
						t.Fatalf("#2's labels %q; want it held for discussion", l)
					}
				}
				resume = f.AddComment(1, "owner", "/resume")
				f.AddComment(1, "mallory", "Wait.")
				f.ForgetWrites()
				return f
			}
			githubtest.Replay(t, setup, resolvePass(t, 1))
			f := run(t, setup, resolvePass(t, 1))
			reason := heldWordingHere
			if child {
				reason = "#2 is on hold for discussion, which only a code owner's response there, or `/resume` there, ends; and " + heldWordingHere
			}
			want := final("/resume", "I didn't apply this command", reason, fmt.Sprint(resume))
			if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) || body != want {
				t.Errorf("#1: labels %q, newest comment\n%q\nwant it held, with\n%q", l, body, want)
			}
		})
	}
}

// #222: a held= record posted before the record's text named the issue,
// with heldon= or from before it, is still the record of the discussion's
// end, as it's read by its marker alone: a later pass posts no second
// record, nor reads the parent again, and the parent's /resume then sends
// the child to clarifying, as TestResolveParentHoldKeepsChild's record
// does.
func TestHeldWordingOldRecord(t *testing.T) {
	t.Parallel()
	for _, heldOn := range []bool{true, false} {
		t.Run(fmt.Sprintf("heldon %t", heldOn), func(t *testing.T) {
			t.Parallel()
			f := discFake(t)
			file(f, 1, ready)
			c := f.AddComment(2, "mallory", "Wait.")
			if err := resolvePass(t, 2)(f); err != nil {
				t.Fatal(err)
			}
			hold := f.AddComment(1, "owner", "/hold")
			if err := resolvePass(t, 1)(f); err != nil {
				t.Fatal(err)
			}
			f.AddReaction(c, "owner", "+1")
			fields := fmt.Sprintf("held=%d", hold)
			if heldOn {
				fields += " heldon=1"
			}
			old := fmt.Sprintf("The discussion is resolved, but this issue stays on hold: "+
				"a `/hold` on an issue above it, in comment %d, hasn't ended. `/resume` there, or here, ends that, and then it goes to `clarifying`."+
				"\n\ncc @owner\n\n<!-- issuedog:ended reason=discussion %s resolved=%d -->", hold, fields, resolveReactionAt(t, f, c))
			f.AddComment(2, app, old)
			comments := len(f.State().Issues[1].Comments)
			writes, requests := len(f.Writes()), len(f.Requests())
			for range 2 {
				if err := resolvePass(t, 2)(f); err != nil {
					t.Fatal(err)
				}
			}
			if w := f.Writes()[writes:]; len(w) != 0 {
				t.Errorf("passes over the old record wrote %+v; want nothing", w)
			}
			for _, r := range f.Requests()[requests:] {
				if strings.HasPrefix(r.Path, "/issues/1") {
					t.Errorf("a pass over the old record sent %+v; want the parent not read again", r)
				}
			}
			if l, n := f.State().Issues[1].Labels, len(f.State().Issues[1].Comments); !slices.Equal(l, []string{"stage:held"}) || n != comments {
				t.Errorf("#2: labels %q, %d comments; want it held, with the %d it had", l, n, comments)
			}
			resume := f.AddComment(1, "owner", "/resume")
			if err := resolvePass(t, 1)(f); err != nil {
				t.Fatal(err)
			}
			if l, body := f.State().Issues[1].Labels, newestComment(f, 2); !slices.Equal(l, []string{"stage:clarifying"}) ||
				!strings.Contains(body, fmt.Sprintf("from=held to=clarifying at=ready origin=ready cmd=%d ", resume)) {
				t.Errorf("#2: labels %q, newest comment %q; want it sent to clarifying by #1's /resume", l, body)
			}
		})
	}
}

// heldWordingMadeHere is #1 with no stage label, so not in the pipeline,
// which no trigger holds, and #2 below it held by its own /hold; then the
// owner's /resume on #1, and mallory's deletion of the owner's comment on
// #1 after it, which may have been a /hold that holds #2 (SPEC §6.4). It
// returns the /resume.
func heldWordingMadeHere(t testing.TB, f *githubtest.Fake) int64 {
	t.Helper()
	f.SetPermission("owner", "admin")
	f.SetPermission("mallory", "write")
	n := f.AddIssue("owner", "Issue", "body")
	f.SetType(n, "task")
	file(f, n, ready) // #2
	f.AddComment(2, "owner", "/hold")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	note := f.AddComment(1, "owner", "Note.")
	resume := f.AddComment(1, "owner", "/resume")
	f.DeleteCommentBy("mallory", note)
	return resume
}

// #389: a /resume on #1 that ends no hold, while #2 stays held by its own
// /hold and by the comment deleted on #1 after the /resume, is rejected,
// naming #2 with "there" and #1, where it's posted, as "this issue", with
// "here". Replay passes, and a second pass writes nothing. A /resume on #1
// posted after the deletion does end that hold, as "here" says: its
// rejection names only #2.
func TestHeldWordingMadeHere(t *testing.T) {
	t.Parallel()
	var resume int64
	setup := func(t testing.TB) *githubtest.Fake {
		f := githubtest.New(t, "o/r", slug)
		resume = heldWordingMadeHere(t, f)
		f.ForgetWrites()
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 1))
	f := run(t, setup, resolvePass(t, 1))
	// One hold each, so "one" and "ends it" (#466).
	want := final("/resume", "I didn't apply this command", "one hold left was made on #2, by `/hold` or by hand, so `/resume` there ends it; "+
		"and one was made on this issue, so `/resume` here ends it", fmt.Sprint(resume))
	if l, body := f.State().Issues[1].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) || body != want {
		t.Errorf("#2's labels %q, #1's newest comment\n%q\nwant #2 held, with\n%q", l, body, want)
	}
	again := f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	want = final("/resume", "I didn't apply this command", madeOn("#2", 1), fmt.Sprint(again))
	if l, body := f.State().Issues[1].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) || body != want {
		t.Errorf("after a second /resume on #1: #2's labels %q, #1's newest comment\n%q\nwant #2 held, with\n%q", l, body, want)
	}
}

// heldWordingLeftOnSelf is #1 with #2 below it, and with below, #3 below
// #2, all at ready; then the owner's /resume on #2, and the owner's /hold
// on #1 after it, in one poll, which #1's pass handles first, so it holds
// #2, and #3, and the /resume posted before it doesn't end that (SPEC
// §6.4). It returns the /resume.
func heldWordingLeftOnSelf(t testing.TB, f *githubtest.Fake, below bool) int64 {
	t.Helper()
	f.SetPermission("owner", "admin")
	file(f, 0, ready) // #1
	file(f, 1, ready) // #2
	if below {
		file(f, 2, ready) // #3
	}
	resume := f.AddComment(2, "owner", "/resume")
	f.AddComment(1, "owner", "/hold")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	return resume
}

// #466: a /resume on #2 that ends no hold, as the one left on #2 itself
// is by a /hold on #1 posted after it, is rejected, naming #1 as where the
// hold left was made, not as "below" #2, and with "ends it" for that one
// hold; with #3 below #2, held by it too, the holds left are two, with
// "ends them". Replay passes, and a second pass writes nothing.
func TestHeldWordingLeftOnSelf(t *testing.T) {
	t.Parallel()
	for _, below := range []bool{false, true} {
		t.Run(fmt.Sprintf("an issue below %t", below), func(t *testing.T) {
			t.Parallel()
			var resume int64
			setup := func(t testing.TB) *githubtest.Fake {
				f := githubtest.New(t, "o/r", slug)
				resume = heldWordingLeftOnSelf(t, f, below)
				f.ForgetWrites()
				return f
			}
			githubtest.Replay(t, setup, resolvePass(t, 2))
			f := run(t, setup, resolvePass(t, 2))
			holds := 1
			if below {
				holds = 2
			}
			want := final("/resume", "I didn't apply this command", madeOn("#1", holds), fmt.Sprint(resume))
			if l, body := f.State().Issues[1].Labels, newestComment(f, 2); !slices.Equal(l, []string{"stage:held"}) || body != want {
				t.Errorf("#2: labels %q, newest comment\n%q\nwant it held, with\n%q", l, body, want)
			}
		})
	}
}

// #466: the parent's /resume, which takes it back while the issues below
// stay held by #2's own /hold, names them as still on hold, with "ends it"
// for one issue and "ends them" for two. Replay passes, and a second pass
// writes nothing.
func TestHeldWordingStillOnHold(t *testing.T) {
	t.Parallel()
	for _, two := range []bool{false, true} {
		t.Run(fmt.Sprintf("two issues %t", two), func(t *testing.T) {
			t.Parallel()
			var resume int64
			setup := func(t testing.TB) *githubtest.Fake {
				f := discFake(t)  // #1
				file(f, 1, ready) // #2
				if two {
					file(f, 2, ready) // #3
				}
				f.AddComment(1, "owner", "/hold")
				if err := resolvePass(t, 1)(f); err != nil {
					t.Fatal(err)
				}
				f.AddComment(2, "owner", "/hold")
				if err := resolvePass(t, 2)(f); err != nil {
					t.Fatal(err)
				}
				resume = f.AddComment(1, "owner", "/resume")
				f.ForgetWrites()
				return f
			}
			githubtest.Replay(t, setup, resolvePass(t, 1))
			f := run(t, setup, resolvePass(t, 1))
			issues, it := "#2", "it"
			if two {
				issues, it = "#2 and #3", "them"
			}
			want := moved("Resumed by `/resume`, back to `ready`. Still on hold: "+issues+", by a hold this `/resume` doesn't end: one set by hand, "+
				"a `/hold` below this issue, or a deleted comment that was, or may have been, a `/hold`. `/resume` there ends "+it+".",
				fmt.Sprintf("from=held to=ready cmd=%d seen=1001", resume))
			if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:ready"}) || body != want {
				t.Errorf("#1: labels %q, newest comment\n%q\nwant it back at ready, with\n%q", l, body, want)
			}
		})
	}
}

// #389: a rejection posted in its old words, before it named the issue it's
// posted on as "this issue", or gave "is" to an issue held for discussion,
// is still the reply to its /resume, as a reply is read by its marker
// alone: later passes post no second reply, nor write anything else, and
// the daemon's pass over it then makes no request that isn't answered 304.
// So is one in #389's words, before its holds were "left", not "below", and
// its words followed the number of holds (#466). Each is posted on on.
func TestHeldWordingOldRejection(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		setup func(t testing.TB, f *githubtest.Fake) int64
		on    int
		old   string
	}{
		{"made here", heldWordingMadeHere, 1,
			"the holds below were made on #2, #1, by `/hold` or by hand, so `/resume` there ends them"},
		{"made here, in #389's words", heldWordingMadeHere, 1,
			"the holds below were made on #2, by `/hold` or by hand, so `/resume` there ends them; " +
				"and the rest were made on this issue, so `/resume` here ends them"},
		{"left on its own issue, in #389's words", func(t testing.TB, f *githubtest.Fake) int64 {
			t.Helper()
			return heldWordingLeftOnSelf(t, f, false)
		}, 2, "the holds below were made on #1, by `/hold` or by hand, so `/resume` there ends them"},
		{"a child held for discussion", func(t testing.TB, f *githubtest.Fake) int64 {
			t.Helper()
			f.SetPermission("owner", "admin")
			f.SetPermission("mallory", "write")
			file(f, 0, ready)
			file(f, 1, ready)
			f.AddComment(2, "mallory", "Wait.")
			if err := resolvePass(t, 2)(f); err != nil {
				t.Fatal(err)
			}
			return f.AddComment(1, "owner", "/resume")
		}, 1, "#2 on hold for discussion, which only a code owner's response there, or `/resume` there, ends"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := githubtest.New(t, "o/r", slug)
			resume := c.setup(t, f)
			f.AddReaction(resume, app, "-1")
			f.AddComment(c.on, app, final("/resume", "I didn't apply this command", c.old, fmt.Sprint(resume)))
			comments := len(f.State().Issues[c.on-1].Comments)
			writes := len(f.Writes())
			for range 2 {
				if err := resolvePass(t, c.on)(f); err != nil {
					t.Fatal(err)
				}
			}
			l := newResolveLoop(t, f)
			l.settle("the daemon over the old rejection")
			if w := f.Writes()[writes:]; len(w) != 0 {
				t.Errorf("passes over the old rejection wrote %+v; want nothing", w)
			}
			if n := len(f.State().Issues[c.on-1].Comments); n != comments {
				t.Errorf("#%d has %d comments; want the %d it had", c.on, n, comments)
			}
			l.unchanged("after the old rejection")
		})
	}
}
