package daemon_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// This file is #245: on a repo that uses issue types, a revert of the issue
// type restores the type that stands by the name the org gives it now. The
// org renames the owner's Task to Bug, a name the pipeline knows that can
// be at ready, so the issue gets no type error.

// typeRenameOrgReads is the number of reads of the org's issue types in f's
// requests from the from'th on.
func typeRenameOrgReads(f *githubtest.Fake, from int) int {
	return len(slices.DeleteFunc(f.Requests()[from:], func(r githubtest.Request) bool { return r.Path != "/orgs/o/issue-types" }))
}

// After the org renames Task to Bug, an untrusted change of the type is
// reverted to Bug, under Replay: one comment, naming Bug, then the type set
// back, with no 422 for the timeline's Task, and no type error. So is one
// whose event never lands, after github.UnexplainedWait. A pass after it
// writes nothing.
func TestTypeRenameRevertReplay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		lag     time.Duration
		pass    func(testing.TB) func(*githubtest.Fake) error
		comment string
	}{
		{"an untrusted change", 0, func(t testing.TB) func(*githubtest.Fake) error { return typeErrPass(t, true) },
			issueTypeRevertWant("restored", "Bug", 9003)},
		{"a change whose event never lands", time.Hour, issueTypeWaitPass, issueTypeWaitFallback("Epic", "Bug", "issue-type:epic@9001")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			setup := issueTypeRevertReady(func(t testing.TB, f *githubtest.Fake) {
				f.RenameOrgIssueType("Task", "Bug")
				f.LagEvents(tc.lag)
				flagWant(t, f.SetTypeBy(1, "mallory", "Epic"), 9003)
			})
			pass := tc.pass(t)
			githubtest.Replay(t, setup, pass)
			f := setup(t)
			errAckRun(t, f, pass)
			issueTypeRevertCheck(t, f, "Bug", tc.comment)
			if w := digestWrites(f, 0); !slices.Equal(w, []string{"POST /issues/1/comments", "PATCH /issues/1"}) {
				t.Errorf("writes %q; want the comment and one PATCH of the issue", w)
			}
			typeErrQuiet(t, f, pass)
		})
	}
}

// The daemon reads the org's issue types once a pass, and only for a
// revert: an unchanged pass reads nothing, two reverts in one pass read the
// list once, and the next pass reads it again, so it sees the org's later
// rename. Stage work's applies, after the pass's step 1, read it afresh.
func TestTypeRenamePass(t *testing.T) {
	t.Parallel()
	f := issueTypeRevertReady(nil)(t)
	d := issueTypeRevertDaemon(t, f)
	other := f.AddIssue("owner", "Sync more", "Fix the rest of the sync.")
	f.SetTypeBy(other, "owner", "Task")
	f.AddLabel(other, app, "stage:ready")
	f.AddComment(other, app, "<!-- issuedog:transition from=intake to=ready run=rready -->")
	d.pass(t)
	from := len(f.Requests())
	if w := d.pass(t); len(w) != 0 || typeRenameOrgReads(f, from) != 0 {
		t.Errorf("an unchanged pass: writes %q, %d reads of the org's types; want neither", w, typeRenameOrgReads(f, from))
	}
	f.RenameOrgIssueType("Task", "Bug")
	for _, n := range []int{1, other} {
		f.SetTypeBy(n, "mallory", "Epic")
	}
	from = len(f.Requests())
	want := []string{"POST /issues/1/comments", "PATCH /issues/1", "POST /issues/3/comments", "PATCH /issues/3"}
	if w := d.pass(t); !slices.Equal(w, want) || typeRenameOrgReads(f, from) != 1 {
		t.Errorf("two reverts: writes %q, %d reads of the org's types; want %q and 1 read", w, typeRenameOrgReads(f, from), want)
	}
	for _, n := range []int{1, other} {
		if got := f.State().Issues[n-1].Type; got != "Bug" {
			t.Errorf("#%d's type %q; want Bug", n, got)
		}
	}
	from = len(f.Requests())
	if w := d.pass(t); len(w) != 0 || typeRenameOrgReads(f, from) != 0 {
		t.Errorf("a pass after the reverts: writes %q, %d reads of the org's types; want neither", w, typeRenameOrgReads(f, from))
	}
	f.RenameOrgIssueType("Bug", "Task")
	f.SetTypeBy(other, "mallory", "Epic")
	d.pass(t)
	if got := f.State().Issues[other-1].Type; got != "Task" {
		t.Errorf("after the org renamed Bug back to Task: #%d's type %q; want Task", other, got)
	}
	// Step 1 reverts #3 to Bug, reading the list. Then, while stage work
	// runs, the org renames Bug to Task once more, and mallory changes #1:
	// work's own apply restores Task, not the pass's Bug, which GitHub
	// refuses.
	f.RenameOrgIssueType("Task", "Bug")
	f.SetTypeBy(other, "mallory", "Epic")
	worked := false
	d.d.Work = func(ctx context.Context, _ []daemon.Ready) error {
		if worked {
			return nil
		}
		worked = true
		f.RenameOrgIssueType("Bug", "Task")
		f.SetTypeBy(1, "mallory", "Epic")
		s, err := d.d.Step.Client.Snapshot(ctx, "o", "r", 1)
		if err != nil {
			return err
		}
		return d.d.Step.CheckLabels(ctx, s, d.d.Step.CurrentTrust(), func(int64) bool { return false })
	}
	from = len(f.Requests())
	d.pass(t)
	if got := typeRenameOrgReads(f, from); got != 2 {
		t.Errorf("step 1's revert and work's: %d reads of the org's types; want 2", got)
	}
	// #3's Bug, set back in step 1, is the org's Task now too.
	for _, n := range []int{1, other} {
		if got := f.State().Issues[n-1].Type; got != "Task" {
			t.Errorf("#%d's type %q; want Task", n, got)
		}
	}
}
