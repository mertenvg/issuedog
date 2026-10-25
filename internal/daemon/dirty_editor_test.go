package daemon_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// dirtyEditorForged is a body under a dirty note that eve, its untrusted
// author, typed herself, naming owner, a valid login, as its editor. Nobody
// edited it, so it's dirty by whoever its note names (Applier.Dirty), but
// nothing confirms that's owner (#602).
const dirtyEditorForged = "<!-- issuedog:dirty by=owner -->\n> ⚠️ issuedog ignores this issue description: " +
	"**owner** edited it without approval.\n\nbody"

// dirtyEditorFiled files issue 1 by eve, with dirtyEditorForged as its
// body, then runs more.
func dirtyEditorFiled(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := githubtest.New(t, "o/r", slug)
		f.AddIssue("eve", "Title", dirtyEditorForged)
		more(t, f)
		return f
	}
}

// dirtyEditorPlanReview is dirtyEditorFiled's issue at plan-review, as
// planReview's: an epic, at planning by label event 9001, with revision 1
// of its spec, comment 1001, its approval, 1002, and plan revision 1, 1003.
// Then it runs more.
func dirtyEditorPlanReview(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return dirtyEditorFiled(func(t testing.TB, f *githubtest.Fake) {
		f.SetType(1, "Epic")
		labelEvent(t, f.AddLabel(1, app, "stage:planning"), 9001)
		post(t, f, 1001, app, revision(1, "body"))
		post(t, f, 1002, app, approvedRev(1, "owner's 👍", "r9", "1001"))
		postPlan(t, f, 1, 1, 1003)
		more(t, f)
		f.ForgetWrites()
	})
}

// A dirty note eve typed on her own body, naming owner, doesn't make the
// App's reply to /approve name owner: nothing in the snapshot confirms
// owner edited it, as neither owner nor the App made its last edit, so the
// reply says "someone" made the edit (#602), at spec-review as at
// plan-review. A crash at any write writes nothing twice.
func TestApproveDirtyEditorForged(t *testing.T) {
	t.Parallel()
	const someone = "the issue description has an edit by someone that wasn't approved, so I can't approve "
	for _, tt := range []struct {
		name   string
		setup  func(testing.TB) *githubtest.Fake
		reason string
		label  string
	}{
		{"at spec-review", dirtyEditorFiled(func(t testing.TB, f *githubtest.Fake) {
			f.SetType(1, "Story")
			labelEvent(t, f.AddLabel(1, app, "stage:spec-review"), 9001)
			post(t, f, 1001, app, revision(1, dirtyEditorForged))
			post(t, f, 1002, "owner", "/approve")
		}), someone + "it, and I won't post a revision of it. " +
			"To get a new spec, which replaces the edit, post `/revise <notes>`, or any other comment", "stage:spec-review"},
		{"at plan-review", dirtyEditorPlanReview(func(t testing.TB, f *githubtest.Fake) {
			post(t, f, 1004, "owner", "/approve")
		}), someone + "the plan. " + planDirtyEpic, "stage:plan-review"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			id := map[string]string{"stage:spec-review": "1002", "stage:plan-review": "1004"}[tt.label]
			check(t, tt.setup, approvePass(t), outcome{comments: []string{ivWordingApproveFinal(tt.reason, id)},
				labels: []string{tt.label}, downs: []string{"/approve"}})
			cs := run(t, tt.setup, approvePass(t)).State().Issues[0].Comments
			if reply := cs[len(cs)-1].Body; strings.Contains(reply, "owner") {
				t.Errorf("the reply names owner, whom only eve's own note names:\n%s", reply)
			}
		})
	}
}

// Through the loop, a story at plan-review under a parent in another repo,
// whose body has eve's own note naming owner, gets the reply to /approve
// naming neither owner nor the parent as "#2", which would link this
// repo's issue 2, but o/other#2 (#602), and so does its digest item. The
// next pass writes nothing, and the one after, unchanged, sends only
// conditional GETs that GitHub answers 304: naming the editor and the
// parent's repo costs no request.
func TestDirtyEditorQuiet(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.AddIssue("eve", "Title", dirtyEditorForged)
	f.AddIssue("owner", "The epic", "The spec.")
	f.SetType(1, "Story")
	f.SetParent(1, 2)
	f.Transfer(2, "o/other")
	f.AddLabel(1, app, "stage:planning")
	f.AddComment(1, app, revision(1, "body"))
	f.AddComment(1, app, approvedRev(1, "owner's 👍", "r9", "1001"))
	postPlan(t, f, 1, 1, 1003)
	d := digestDaemon(t, f, newOrigin(t), nil)
	d.Work = func(context.Context, []daemon.Ready) error { return nil }
	iterate(t, d)
	// Wired as main wires it, once trust has loaded.
	d.Step.Commands["approve"] = daemon.ApproveCommand()
	d.Step.Reactions = daemon.ApproveByReaction(d.Step.CurrentTrust())
	post(t, f, 1004, "owner", "/approve")
	iterate(t, d)
	const elsewhere = "Nor does the interviewer, as this story's description is its item of o/other#2's plan. " +
		"To clear it, get a revision of o/other#2's plan that changes this story, as by moving o/other#2 to `stage:planning`: "
	issues := f.State().Issues
	reply := issues[0].Comments[len(issues[0].Comments)-1].Body
	if !strings.Contains(reply, "the issue description has an edit by someone that wasn't approved") || !strings.Contains(reply, elsewhere) ||
		strings.Contains(reply, "owner") || strings.Contains(reply, " #2") {
		t.Errorf("the reply to /approve is\n%s\nwant it to name someone, not owner, and the parent as o/other#2, not #2", reply)
	}
	digest := issues[len(issues)-1].Body
	if want := "Get a revision of o/other#2's plan that changes the issue to clear the edit, as by moving o/other#2 to `stage:planning`"; !strings.Contains(digest, want) {
		t.Errorf("the digest is\n%s\nwant it to name the parent as o/other#2: %q", digest, want)
	}
	n := len(f.Writes())
	iterate(t, d)
	if w := f.Writes(); len(w) != n {
		t.Errorf("the pass after wrote %+v, want nothing", w[n:])
	}
	from, notModified := len(f.Requests()), f.NotModified()
	iterate(t, d)
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("an unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}
	if f.NotModified() == notModified {
		t.Error("an unchanged pass got no 304")
	}
}
