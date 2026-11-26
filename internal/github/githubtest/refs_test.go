package githubtest

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/logtest"
	"github.com/mertenvg/logr/v2"
)

// refsStory is a fake of o/r whose git remote has main, the story branch
// issuedog/5-story one commit past it, the task branches issuedog/7-task
// and issuedog/8-task, each one commit past the story branch, and other,
// one past main. It has the App's open pull requests from issuedog/7-task
// into the story branch and from the story branch into main, the owner's
// from other into main, and the App's from issuedog/8-task into the story
// branch, which the owner closed, all their writes forgotten
// (ForgetWrites). It returns the fake, its remote, and the four pull
// requests' numbers in that order.
func refsStory(t testing.TB) (*Fake, *pullRemote, [4]int) {
	t.Helper()
	f, r := pullFake(t, "issuedog/5-story", "other")
	for _, task := range []string{"issuedog/7-task", "issuedog/8-task"} {
		r.git("update-ref", "refs/heads/"+task, r.tip("issuedog/5-story"))
		r.commit(task)
	}
	c, ctx := f.Client(t), context.Background()
	var prs [4]int
	for i, p := range [][2]string{{"issuedog/7-task", "issuedog/5-story"}, {"issuedog/5-story", "main"}, {"other", "main"}, {"issuedog/8-task", "issuedog/5-story"}} {
		if i == 2 {
			prs[i] = f.OpenPRAs("owner", p[0], p[1], "Other", "")
			continue
		}
		pr, err := c.CreatePullRequest(ctx, "o", "r", github.NewPullRequest{Title: "T " + p[0], Head: p[0], Base: p[1], Body: "B"})
		if err != nil {
			t.Fatal(err)
		}
		prs[i] = pr.Number
	}
	f.ClosePR(prs[3], "owner")
	f.ForgetWrites()
	return f, r, prs
}

// Deleting a branch, through DELETE /git/refs as the App or by an admin
// with DeleteBranchAs, closes each open pull request into it and from it,
// unmerged, with a closed event naming the deleter, as GitHub does (SPEC
// §16). Each keeps its head SHA. A closed pull request into it gets no
// event, and one that neither comes from it nor goes into it stays open.
func TestRefsDeleteClosesPulls(t *testing.T) {
	t.Parallel()
	for _, by := range []string{"", "admin"} {
		t.Run(fmt.Sprintf("by %q", by), func(t *testing.T) {
			t.Parallel()
			f, r, prs := refsStory(t)
			before := f.State().PRs
			want := "issuedog[bot]"
			if by == "" {
				if status, msg := pullDo(t, f, "DELETE", "repos/o/r/git/refs/heads/issuedog/5-story", nil, nil); status != http.StatusNoContent {
					t.Fatalf("delete: %d %q; want 204", status, msg)
				}
			} else {
				want = by
				if err := f.DeleteBranchAs(by, "issuedog/5-story"); err != nil {
					t.Fatal(err)
				}
			}
			if got := r.tip("issuedog/5-story"); got != "" {
				t.Errorf("the story branch at %s; want it gone", got)
			}
			after := f.State().PRs
			for i, wantState := range []string{"closed", "closed", "open", "closed"} {
				got, was := after[i], before[i]
				was.State = wantState
				if !reflect.DeepEqual(got, was) {
					t.Errorf("#%d after the delete %+v; want %+v, unmerged", prs[i], got, was)
				}
			}
			for i, wantEvents := range [][]string{{"closed " + want}, {"closed " + want}, nil, {"closed owner"}} {
				if got := pullMergeEvents(t, f, prs[i]); !reflect.DeepEqual(got, wantEvents) {
					t.Errorf("#%d's timeline %q; want %q", prs[i], got, wantEvents)
				}
			}
			if got, wantOpen := pullList(t, f, "state=open"), []string{fmt.Sprintf("#%d other->main open", prs[2])}; !reflect.DeepEqual(got, wantOpen) {
				t.Errorf("open pull requests %q; want %q", got, wantOpen)
			}
		})
	}
}

// A pull request whose head a push brought into its base before its
// branch is deleted reads merged by the pusher, as GitHub marked it at the
// push, not closed by the delete.
func TestRefsDeleteAfterFastForward(t *testing.T) {
	t.Parallel()
	f, r, prs := refsStory(t)
	head := r.tip("issuedog/7-task")
	r.git("update-ref", "refs/heads/issuedog/5-story", head)
	if status, msg := pullDo(t, f, "DELETE", "repos/o/r/git/refs/heads/issuedog/7-task", nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete: %d %q; want 204", status, msg)
	}
	pr, err := f.Client(t).PullRequest(t.Context(), "o", "r", prs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !pr.Merged || pr.MergedBy != "issuedog[bot]" || pr.MergeCommitSHA != head {
		t.Errorf("read after the delete: %+v; want merged by the App at %s", pr, head)
	}
	if got, want := pullMergeEvents(t, f, prs[0]), []string{"merged issuedog[bot]", "closed issuedog[bot]"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the timeline %q; want %q", got, want)
	}
}

// A pull request a delete closed can't be reopened while its base or head
// branch is gone. RestoreBranch puts the branch back at its tip and
// reopens none, and each can then be reopened, by the App through PATCH
// /pulls/{n} and by someone else alike, at the head it had.
func TestRefsRestoreThenReopen(t *testing.T) {
	t.Parallel()
	f, r, prs := refsStory(t)
	tip, task := r.tip("issuedog/5-story"), r.tip("issuedog/7-task")
	if err := f.DeleteBranchAs("admin", "issuedog/5-story"); err != nil {
		t.Fatal(err)
	}
	reopen := func(n int) (int, string) {
		return pullDo(t, f, "PATCH", fmt.Sprintf("repos/o/r/pulls/%d", n), map[string]string{"state": "open"}, nil)
	}
	for _, n := range prs[:2] {
		if status, msg := reopen(n); status != http.StatusUnprocessableEntity || !strings.Contains(msg, "issuedog/5-story branch was deleted") {
			t.Errorf("reopen #%d with the story branch gone: %d %q; want 422, deleted", n, status, msg)
		}
	}
	before := f.State()
	if err := f.RestoreBranch("issuedog/5-story", tip); err != nil {
		t.Fatal(err)
	}
	if got := r.tip("issuedog/5-story"); got != tip {
		t.Errorf("the restored branch at %q; want %s", got, tip)
	}
	if got := f.State(); !reflect.DeepEqual(got.PRs, before.PRs) {
		t.Errorf("pull requests after the restore %+v; want them as they were %+v", got.PRs, before.PRs)
	}
	for _, n := range prs[:2] {
		if got, want := pullMergeEvents(t, f, n), []string{"closed admin"}; !reflect.DeepEqual(got, want) {
			t.Errorf("#%d's timeline after the restore %q; want %q", n, got, want)
		}
	}
	if status, msg := reopen(prs[0]); status != http.StatusOK {
		t.Errorf("reopen #%d by the App: %d %q; want 200", prs[0], status, msg)
	}
	f.ReopenPR(prs[1], "owner")
	if got := pullList(t, f, "state=open"); len(got) != 3 {
		t.Errorf("open pull requests %q; want #%d, #%d and #%d", got, prs[0], prs[1], prs[2])
	}
	if got := f.State().PRs; got[0].HeadSHA != task || got[1].HeadSHA != tip {
		t.Errorf("heads after the reopens %+v; want %s and %s", got, task, tip)
	}
	for i, by := range []string{"issuedog[bot]", "owner"} {
		if got, want := pullMergeEvents(t, f, prs[i]), []string{"closed admin", "reopened " + by}; !reflect.DeepEqual(got, want) {
			t.Errorf("#%d's timeline after the reopen %q; want %q", prs[i], got, want)
		}
	}
}

// DeleteBranchAs a branch the git remote hasn't got, and RestoreBranch of
// a branch it has, or at a commit it hasn't got, are errors naming the
// branch, and change nothing.
func TestRefsDeleteRestoreRefused(t *testing.T) {
	t.Parallel()
	f, r, _ := refsStory(t)
	before, tip := f.State(), r.tip("issuedog/5-story")
	if err := f.DeleteBranchAs("admin", "issuedog/9-never"); err == nil || !strings.Contains(err.Error(), "issuedog/9-never") {
		t.Errorf("delete a branch that isn't there: %v; want an error naming it", err)
	}
	if err := f.RestoreBranch("issuedog/5-story", r.tip("main")); err == nil || !strings.Contains(err.Error(), "issuedog/5-story") {
		t.Errorf("restore a branch that's there: %v; want an error naming it", err)
	}
	if err := f.RestoreBranch("issuedog/9-never", strings.Repeat("1", 40)); err == nil || !strings.Contains(err.Error(), "issuedog/9-never") {
		t.Errorf("restore at a commit the remote hasn't got: %v; want an error naming the branch", err)
	}
	if got := f.State(); !reflect.DeepEqual(got, before) || r.tip("issuedog/5-story") != tip {
		t.Errorf("state after the refusals:\n%+v\nwant it unchanged:\n%+v", got, before)
	}
}

// Deleting a story branch with open pull requests into it and from it is
// safe to replay after a crash at any write: the branch is gone, both pull
// requests are closed, unmerged, and a second delete finds the branch gone
// and closes nothing more.
func TestRefsDeleteClosesReplay(t *testing.T) {
	t.Parallel()
	setup := func(tb testing.TB) *Fake {
		f, _, _ := refsStory(tb)
		return f
	}
	pass := func(f *Fake) error {
		if err := f.Client(t).DeleteBranch(context.Background(), "o", "r", "issuedog/5-story"); err != nil {
			return fmt.Errorf("delete: %w", err)
		}
		return nil
	}
	clean := setup(t)
	if err := pass(clean); err != nil {
		t.Fatal(err)
	}
	if s := clean.State(); len(s.PRs) != 4 || s.PRs[0].State != "closed" || s.PRs[1].State != "closed" || s.PRs[0].Merged || s.PRs[1].Merged ||
		s.PRs[2].State != "open" || len(s.Branches) != 2 {
		t.Fatalf("clean state: PRs %+v, branches %+v; want the story's two closed, unmerged, the other open, and the story branch gone", s.PRs, s.Branches)
	}
	Replay(t, setup, pass)
}

// DELETE /git/refs/heads/{branch} deletes the branch in the git remote,
// with a 204, counted as a write, and leaves the other branches. A branch
// that's gone, deleted already or never there, is GitHub's 422 "Reference
// does not exist", and is a write too. State's Branches are the remote's
// branches under issuedog/, by name, with their tips, before and after,
// and never main or another branch outside issuedog/.
func TestRefsDeleteBranch(t *testing.T) {
	t.Parallel()
	f, r := pullFake(t, "issuedog/7-task", "issuedog/5-story", "other")
	task, story := r.tip("issuedog/7-task"), r.tip("issuedog/5-story")
	if got, want := f.State().Branches, []BranchState{{"issuedog/5-story", story}, {"issuedog/7-task", task}}; !reflect.DeepEqual(got, want) {
		t.Errorf("State().Branches %+v; want %+v", got, want)
	}
	if status, msg := pullDo(t, f, "DELETE", "repos/o/r/git/refs/heads/issuedog/7-task", nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete: %d %q; want 204", status, msg)
	}
	if got := r.tip("issuedog/7-task"); got != "" {
		t.Errorf("the deleted branch's tip %s; want it gone", got)
	}
	if got, want := f.State().Branches, []BranchState{{"issuedog/5-story", story}}; !reflect.DeepEqual(got, want) {
		t.Errorf("State().Branches after the delete %+v; want %+v", got, want)
	}
	for _, branch := range []string{"issuedog/7-task", "issuedog/9-never"} {
		if status, msg := pullDo(t, f, "DELETE", "repos/o/r/git/refs/heads/"+branch, nil, nil); status != http.StatusUnprocessableEntity || msg != "Reference does not exist" {
			t.Errorf("delete %s: %d %q; want 422 Reference does not exist", branch, status, msg)
		}
	}
	w := f.Writes()
	if len(w) != 3 || w[0].Method != "DELETE" || w[0].Path != "/git/refs/heads/issuedog/7-task" || w[2].Path != "/git/refs/heads/issuedog/9-never" {
		t.Errorf("writes %+v; want the three DELETEs", w)
	}
	if r.tip("other") == "" || r.tip("main") == "" {
		t.Error("a branch other than the one named went")
	}
}

// A delete that FailWrite fails changes nothing, and one FailWriteAfter
// fails deletes the branch though it answers 500.
func TestRefsDeleteFailed(t *testing.T) {
	t.Parallel()
	for _, after := range []bool{false, true} {
		f, r := pullFake(t, "issuedog/7-task")
		if after {
			f.FailWriteAfter(1)
		} else {
			f.FailWrite(1)
		}
		if status, _ := pullDo(t, f, "DELETE", "repos/o/r/git/refs/heads/issuedog/7-task", nil, nil); status != http.StatusInternalServerError {
			t.Errorf("after %v: delete %d; want 500", after, status)
		}
		if gone := r.tip("issuedog/7-task") == ""; gone != after {
			t.Errorf("after %v: the branch gone %v; want %v", after, gone, after)
		}
	}
}

// A delete's 204 is sent with no body, as GitHub's is, so the fake logs no
// failed write of one. It reads logr, so it isn't parallel.
func TestRefsDeleteNoBody(t *testing.T) {
	f, _ := pullFake(t, "issuedog/7-task")
	stop := logtest.Capture(t, logr.All)
	status, msg := pullDo(t, f, "DELETE", "repos/o/r/git/refs/heads/issuedog/7-task", nil, nil)
	if logs := stop(); status != http.StatusNoContent || logs != "" {
		t.Errorf("delete: %d %q, logged %q; want 204, nothing logged", status, msg, logs)
	}
}

// With no git remote, a delete is a 500 naming SetGitRemote, and State
// has no branches.
func TestRefsNoRemote(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	if status, msg := pullDo(t, f, "DELETE", "repos/o/r/git/refs/heads/issuedog/7-task", nil, nil); status != http.StatusInternalServerError ||
		!strings.HasSuffix(msg, "githubtest: no git remote to read branches from: call SetGitRemote") {
		t.Errorf("delete: %d %q; want a 500 naming SetGitRemote", status, msg)
	}
	if got := f.State().Branches; got != nil {
		t.Errorf("State().Branches %+v; want none", got)
	}
}
