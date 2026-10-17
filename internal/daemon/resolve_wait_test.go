package daemon_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// resolveReview is review's story at spec-review, revision 1 of "body" in
// 1001, with mallory a writer, held by her comment 1002, the hold in
// 1003, by a pass, then do.
func resolveReview(do func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return review(func(t testing.TB, f *githubtest.Fake) {
		f.SetPermission("owner", "admin")
		f.SetPermission("mallory", "write")
		post(t, f, 1002, "mallory", "Is this right?")
		if err := discPass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(newestComment(f, 1), "to=held at=spec-review") {
			t.Fatalf("newest comment %q; want the hold", newestComment(f, 1))
		}
		f.ForgetWrites()
		do(t, f)
	})
}

// An /approve posted while held waits (#87's assumption 8), and once
// /resume takes the issue back to spec-review, in the same pass, it's
// applied while the body still has revision 1's hash, and rejected with
// the reason once it doesn't.
func TestResolveApproveAfterResume(t *testing.T) {
	resumed := moved("Resumed by `/resume`, back to `spec-review`.", "from=held to=spec-review cmd=1005 seen=1001")
	h := state.BodyHash("body")
	t.Run("the hash matches", func(t *testing.T) {
		setup := resolveReview(func(t testing.TB, f *githubtest.Fake) {
			post(t, f, 1004, "owner", "/approve")
			post(t, f, 1005, "owner", "/resume")
		})
		resolveCheck(t, setup, 1, func(*githubtest.Fake) issueWant {
			return issueWant{[]string{"stage:planning"}, []string{resumed, moved("Revision 1 of the spec is approved, by owner's `/approve`.",
				"from=spec-review to=planning rev=1 body="+h+" cmd=1004 seen=1004")}}
		})
	})
	t.Run("the body changed", func(t *testing.T) {
		setup := resolveReview(func(t testing.TB, f *githubtest.Fake) {
			post(t, f, 1004, "owner", "/approve")
			f.EditBody(1, "owner", "body, changed")
			post(t, f, 1005, "owner", "/resume")
		})
		resolveCheck(t, setup, 1, func(*githubtest.Fake) issueWant {
			return issueWant{[]string{"stage:spec-review"}, []string{resumed, rejected("/approve",
				"the issue description changed after revision 1, so it no longer matches that revision's hash, `"+h+"`", "1004")}}
		})
	})
}

// An /approve posted while held counters mallory's comment, so the issue
// leaves the hold, to clarifying, where the /approve still waits, with no
// reaction and no reply. Once the owner sets spec-review by hand, it's
// applied.
func TestResolveApproveWaitsAtClarifying(t *testing.T) {
	setup := resolveReview(func(t testing.TB, f *githubtest.Fake) { post(t, f, 1004, "owner", "/approve") })
	resolveCheck(t, setup, 1, func(f *githubtest.Fake) issueWant {
		return issueWant{[]string{"stage:clarifying"}, []string{resolveLeaveText("`owner`", "spec-review",
			fmt.Sprintf("from=held to=clarifying at=spec-review origin=spec-review event=h1003 resolved=%d seen=1001", resolveCommentAt(t, f, 1, 1004)))}}
	})
	f := run(t, setup, resolvePass(t, 1))
	if r := f.State().Issues[0].Comments[3].Reactions; len(r) != 0 {
		t.Fatalf("reactions on the /approve %q; want none while it waits", r)
	}
	f.RemoveLabel(1, "owner", "stage:clarifying")
	f.AddLabel(1, "owner", "stage:spec-review")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	want := moved("Revision 1 of the spec is approved, by owner's `/approve`.", "from=spec-review to=planning rev=1 body="+state.BodyHash("body")+" cmd=1004 seen=1004")
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:planning"}) || newestComment(f, 1) != want {
		t.Errorf("labels %q, newest comment %q; want the waiting /approve applied: %q", l, newestComment(f, 1), want)
	}
}

// resolveFamily files #1, a task at ready, and #2, a task under it at
// ready, holds both by /hold on #1, and posts mallory's comment on #2,
// which has no record (Q2b), by passes. It returns that comment's ID.
func resolveFamily(t testing.TB) (*githubtest.Fake, int64) {
	f := discFake(t)
	file(f, 1, ready)
	f.AddComment(1, "owner", "/hold")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	c := f.AddComment(2, "mallory", "Wait.")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	st := f.State()
	if !slices.Equal(st.Issues[0].Labels, []string{"stage:held"}) || !slices.Equal(st.Issues[1].Labels, []string{"stage:held"}) {
		t.Fatalf("labels %q and %q; want both held", st.Issues[0].Labels, st.Issues[1].Labels)
	}
	f.ForgetWrites()
	return f, c
}

// A /resume on the parent ends the child's owner hold while its
// discussion stays, and records that on the child. Once the discussion is
// resolved, no owner hold is left, so the child goes to clarifying, with
// no other /resume (#187's verify note).
func TestResolveParentResumeThenResolved(t *testing.T) {
	var resume, c int64
	resumed := func(t testing.TB) *githubtest.Fake {
		var f *githubtest.Fake
		f, c = resolveFamily(t)
		resume = f.AddComment(1, "owner", "/resume")
		return f
	}
	githubtest.Replay(t, resumed, resolvePass(t, 1))
	f := run(t, resumed, resolvePass(t, 1))
	st := f.State()
	if !slices.Equal(st.Issues[0].Labels, []string{"stage:ready"}) || !slices.Equal(st.Issues[1].Labels, []string{"stage:held"}) {
		t.Fatalf("labels %q and %q; want #1 back at ready, #2 still held for discussion", st.Issues[0].Labels, st.Issues[1].Labels)
	}
	if body := newestComment(f, 2); !strings.HasPrefix(body, "`/resume` on #1 ended the owner hold on this issue") ||
		!strings.Contains(body, fmt.Sprintf("<!-- issuedog:ended reason=owner key=c1003 cmd=%d -->", resume)) {
		t.Errorf("newest comment on #2 %q; want the record that #1's /resume ended its /hold, made by comment 1003", body)
	}
	resolved := func(t testing.TB) *githubtest.Fake {
		f := resumed(t)
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		f.ForgetWrites()
		f.AddReaction(c, "owner", "+1")
		return f
	}
	resolveCheck(t, resolved, 2, func(f *githubtest.Fake) issueWant {
		hold := commentIDs(t, f, 2)[1]
		return issueWant{[]string{"stage:clarifying"}, []string{resolveLeaveText("`owner`", "ready",
			fmt.Sprintf("from=held to=clarifying at=ready origin=ready event=h%d resolved=%d seen=1002", hold, resolveReactionAt(t, f, c)))}}
	})
}

// A /resume on a parent that isn't held, which ends only the owner hold of
// a child that stays held for discussion, applies: it gets its 👍, and the
// child the record.
func TestResolveParentResumeEndsOnlyARecord(t *testing.T) {
	f, _ := resolveFamily(t)
	f.RemoveLabel(1, "owner", "stage:held")
	f.AddLabel(1, "owner", "stage:ready")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	resume := f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	st := f.State()
	cs := st.Issues[0].Comments
	if r := cs[len(cs)-1].Reactions; cs[len(cs)-1].Body != "/resume" || !slices.Equal(r, []string{app + " +1"}) {
		t.Errorf("newest comment on #1 %q with %q; want the /resume, 👍'd", cs[len(cs)-1].Body, r)
	}
	if body := newestComment(f, 2); !strings.Contains(body, fmt.Sprintf("<!-- issuedog:ended reason=owner key=c1003 cmd=%d -->", resume)) ||
		!slices.Equal(st.Issues[1].Labels, []string{"stage:held"}) {
		t.Errorf("#2: labels %q, newest comment %q; want it still held, with the record", st.Issues[1].Labels, body)
	}
}

// An issue the owner held at intake, whose discussion is resolved in the
// same pass as the owner's /resume, goes back to intake, not to
// clarifying.
func TestResolveResumeFromIntake(t *testing.T) {
	f := githubtest.New(t, "o/r", slug)
	f.SetPermission("owner", "admin")
	f.SetPermission("mallory", "write")
	file(f, 0, "from=clarifying to=intake event=9 by=owner") // 1001
	f.AddComment(1, "owner", "/hold")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.RenameIssue(1, "mallory", "Renamed")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	ids := commentIDs(t, f, 1)
	f.AddReaction(ids[len(ids)-1], "owner", "+1") // on the rename's notice
	resume := f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:intake"}) ||
		!strings.Contains(body, fmt.Sprintf("from=held to=intake cmd=%d ", resume)) {
		t.Errorf("labels %q, newest comment %q; want the /resume back to intake", l, body)
	}
}

// A discussion resolved before an owner hold began didn't end during it:
// the owner's /resume restores at.
func TestResolveBeforeTheHold(t *testing.T) {
	f := discFake(t)
	f.AddComment(1, "mallory", "Wait.")
	f.AddComment(1, "owner", "Noted.")
	f.AddComment(1, "owner", "/hold")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	resume := f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:ready"}) ||
		!strings.Contains(body, fmt.Sprintf("from=held to=ready cmd=%d ", resume)) {
		t.Errorf("labels %q, newest comment %q; want the /resume back to ready", l, body)
	}
}

// A comment the owner countered resolves nothing when its author isn't a
// writer, as trudy, who can only read: while the owner's hold stays, no
// record is written that a discussion ended.
func TestResolveNoRecordForNonWriter(t *testing.T) {
	setup := func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		f.SetPermission("trudy", "read")
		f.AddComment(1, "owner", "/hold")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		f.ForgetWrites()
		f.AddComment(1, "trudy", "Looks risky.")
		f.AddComment(1, "owner", "It's fine.")
		return f
	}
	if w := run(t, setup, resolvePass(t, 1)).Writes(); len(w) != 0 {
		t.Errorf("writes %+v; want none", w)
	}
}

// The record that a discussion ended while an owner hold stayed closes the
// triggers it saw resolved: once the owner takes back the 👍 that approved
// mallory's comment, the comment holds nothing, and a /resume on the parent
// sends the child to clarifying.
func TestResolveRecordCloses(t *testing.T) {
	f, c := resolveFamily(t)
	r := f.AddReaction(c, "owner", "+1")
	if err := resolvePass(t, 2)(f); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(newestComment(f, 2), "issuedog:ended reason=discussion") {
		t.Fatalf("newest comment on #2 %q; want the record that the discussion ended", newestComment(f, 2))
	}
	if !f.RemoveReaction(c, r) {
		t.Fatal("no 👍 to remove")
	}
	resume := f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l, body := f.State().Issues[1].Labels, newestComment(f, 2); !slices.Equal(l, []string{"stage:clarifying"}) ||
		!strings.Contains(body, fmt.Sprintf("from=held to=clarifying at=ready origin=ready cmd=%d ", resume)) {
		t.Errorf("#2: labels %q, newest comment %q; want it sent to clarifying by #1's /resume", l, body)
	}
}

// The move out of held is bounded by when the act that resolved the last
// trigger happened, not by when the App recorded it: a comment posted
// between the 👍, or the /resume, and the App's move holds the issue
// again, from clarifying or ready, keeping the hold's origin.
func TestResolveBoundByAct(t *testing.T) {
	for name, do := range map[string]func(f *githubtest.Fake){
		"a 👍":       func(f *githubtest.Fake) { f.AddReaction(1002, "owner", "+1") },
		"a /resume": func(f *githubtest.Fake) { f.AddComment(1, "owner", "/resume") },
	} {
		t.Run(name, func(t *testing.T) {
			f := resolveHeldWith(func(_ testing.TB, f *githubtest.Fake) { do(f) })(t)
			var sneaky int64
			f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == "POST" && r.Path == "/issues/1/comments" }, func() {
				sneaky = f.AddComment(1, "mallory", "One more thing.")
			})
			if err := resolvePass(t, 1)(f); err != nil {
				t.Fatal(err)
			}
			if l := f.State().Issues[0].Labels; slices.Equal(l, []string{"stage:held"}) || sneaky == 0 {
				t.Fatalf("labels %q, mallory's comment %d; want the issue out of held, after her comment", l, sneaky)
			}
			if err := resolvePass(t, 1)(f); err != nil {
				t.Fatal(err)
			}
			if l, body := f.State().Issues[0].Labels, newestComment(f, 1); !slices.Equal(l, []string{"stage:held"}) ||
				!strings.Contains(body, fmt.Sprintf("origin=ready reason=discussion event=c%d ", sneaky)) {
				t.Errorf("labels %q, newest comment %q; want the issue held again for comment %d", l, body, sneaky)
			}
		})
	}
}

// At the clarifying after a hold, until T5, the issue waits: more passes
// write nothing, and the interviewer isn't pending (Pending).
func TestResolveWaitsAtClarifying(t *testing.T) {
	f := run(t, resolveHeldWith(func(_ testing.TB, f *githubtest.Fake) { f.AddReaction(1002, "owner", "+1") }), resolvePass(t, 1))
	writes := len(f.Writes())
	for range 2 {
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
	}
	if w := f.Writes()[writes:]; len(w) != 0 || !slices.Equal(f.State().Issues[0].Labels, []string{"stage:clarifying"}) {
		t.Errorf("writes %+v, labels %q; want the issue left at clarifying, with nothing more written", w, f.State().Issues[0].Labels)
	}
}

// An issue held from intake isn't in the pipeline yet, so the end of its
// discussion doesn't send it to clarifying, which would admit it: it
// stays held, with the record saying so, and /resume takes it back to
// intake.
func TestResolveFromIntake(t *testing.T) {
	var c int64
	held := func(t testing.TB) *githubtest.Fake {
		f := githubtest.New(t, "o/r", slug)
		f.SetPermission("owner", "admin")
		f.SetPermission("mallory", "write")
		file(f, 0, "from=clarifying to=intake event=9 by=owner") // 1001
		c = f.AddComment(1, "mallory", "Wait.")
		if err := discPass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(newestComment(f, 1), "to=held at=intake") {
			t.Fatalf("newest comment %q; want the hold from intake", newestComment(f, 1))
		}
		f.ForgetWrites()
		f.AddReaction(c, "owner", "+1")
		return f
	}
	resolveCheck(t, held, 1, func(f *githubtest.Fake) issueWant {
		return issueWant{[]string{"stage:held"}, []string{fmt.Sprintf("The discussion is resolved, by `owner`, but this issue stays on hold: "+
			"it was at `intake` before the hold, so `clarifying` would bring it into the pipeline. `/resume` here takes it back to `intake`."+
			"\n\ncc @owner\n\n<!-- issuedog:ended reason=discussion resolved=%d -->", resolveReactionAt(t, f, c))}}
	})
	var resume int64
	resumed := func(t testing.TB) *githubtest.Fake {
		f := held(t)
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		f.ForgetWrites()
		resume = f.AddComment(1, "owner", "/resume")
		return f
	}
	resolveCheck(t, resumed, 1, func(*githubtest.Fake) issueWant {
		return issueWant{[]string{"stage:intake"}, []string{moved("Resumed by `/resume`, back to `intake`.", fmt.Sprintf("from=held to=intake cmd=%d seen=1001", resume))}}
	})
}
