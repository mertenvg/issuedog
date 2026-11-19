package daemon_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// A task's PR at pr-open in the digest and in "assigned to me" (SPEC §9.4,
// §9.6), against the fakes, with step 1 wired as main wires it, the digest
// and assignment on: task #1 is at pr-open, its move naming PR #2, which
// the App opened from prBranch to main, and the digest, which the first
// pass creates, is #3. mertenvg is notify and the code owner.

// prItemTitle is the PR's title, which the item names.
const prItemTitle = "feat: refuse booking an expired quote"

// prItemOrigin is digestRunOrigin with prBranch one commit past main, for
// the task's PR.
func prItemOrigin(t testing.TB) *origin {
	t.Helper()
	o := digestRunOrigin(t)
	gitIn(t, o.work, "checkout", "--quiet", "-b", "task")
	if err := os.WriteFile(filepath.Join(o.work, "quote.txt"), []byte("expired\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, o.work, "add", "quote.txt")
	gitIn(t, o.work, "commit", "--quiet", "-m", "refuse an expired quote")
	gitIn(t, o.work, "push", "--quiet", o.url, "task:refs/heads/"+prBranch)
	gitIn(t, o.work, "checkout", "--quiet", "main")
	return o
}

// prItemFake is o/r with task #1 at pr-open, priority:p1, its move from
// verifying naming PR #2, the App's, open from prBranch on o to main, with
// priority:p1, and no write logged.
func prItemFake(t testing.TB, o *origin) *githubtest.Fake {
	t.Helper()
	f := newFake(t)
	f.SetPermission(digestRunOwner, "admin")
	f.SetGitRemote(o.url)
	n := f.AddIssue(digestRunOwner, "Refuse an expired quote", "Booking an expired quote returns 410.", "type:task", "priority:p1")
	f.AddLabel(n, app, "stage:pr-open")
	pr, err := f.Client(t).CreatePullRequest(context.Background(), "o", "r",
		github.NewPullRequest{Title: prItemTitle, Head: prBranch, Base: "main", Body: "Closes #1\n\nWhat changed."})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 2 {
		t.Fatalf("the PR is #%d, want #2", pr.Number)
	}
	f.AddLabel(pr.Number, app, "priority:p1")
	head := gitIn(t, o.url, "rev-parse", "refs/heads/"+prBranch)
	f.AddComment(n, app, prMoveText(pr.Number, head)+"<!-- issuedog:transition from=verifying to=pr-open run=rv head="+head+" pr=2 -->")
	f.ForgetWrites()
	return f
}

// prItemDaemon is digestRunDaemon with assignment on.
func prItemDaemon(t testing.TB, f *githubtest.Fake, o *origin) *daemon.Daemon {
	t.Helper()
	d := digestRunDaemon(t, f, o, nil, nil)
	d.Assign = true
	return d
}

// prItemBody is the digest's item for PR #2 at rank 1.
const prItemBody = "### 1. #1: Refuse an expired quote\n\n**Pull request**, priority p1, unblocks 1 open issue (itself included)\n\n" +
	"- **PR #2**: " + prItemTitle + "\n" +
	"- It waits for your review. Merging it closes this task, and closing it without merging hands the task back to you.\n" +
	"- Answer here: https://github.com/o/r/pull/2"

// prItemQuiet runs a pass of d that must write nothing and send no request
// about the PR, #2.
func prItemQuiet(t *testing.T, f *githubtest.Fake, d *daemon.Daemon, step string) {
	t.Helper()
	from, writes := len(f.Requests()), len(f.Writes())
	iterate(t, d)
	if got := writesOf(f)[writes:]; len(got) != 0 {
		t.Errorf("%s: the pass wrote %q; want nothing", step, got)
	}
	for _, r := range f.Requests()[from:] {
		if r.Path == "/issues/2" || strings.HasPrefix(r.Path, "/issues/2/") || strings.HasPrefix(r.Path, "/pulls") {
			t.Errorf("%s: the pass sent %+v; want nothing about the PR", step, r)
		}
	}
}

// A task at pr-open whose PR is open has the digest's pull request item,
// pr-1-2: it names the PR and its title, says merging closes the task and
// closing it hands the task back, links the PR and ranks as an approval.
// The digest announces it, as the move has no cc line, and notify is
// assigned to the PR, never the task. A pass after writes nothing, and
// makes no request about the PR. Then the PR merges, which closes the
// task, or closes: the item ends, and notify is unassigned from the PR,
// once. An assign by hand after that is left alone.
func TestPRItemThroughMergeAndClose(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		end    func(f *githubtest.Fake)
		closed bool // the task closes
	}{
		{"merge", func(f *githubtest.Fake) { f.MergePR(2, digestRunOwner) }, true},
		{"close", func(f *githubtest.Fake) { f.ClosePR(2, digestRunOwner) }, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := prItemOrigin(t)
			f := prItemFake(t, o)
			d := prItemDaemon(t, f, o)
			iterate(t, d)
			digest := f.State().Issues[2]
			if !strings.Contains(digest.Body, "1 item waits on you.\n\n"+prItemBody+"\n\n<!-- issuedog:digest -->") {
				t.Errorf("the digest =\n%s\nwant it to list\n%s", digest.Body, prItemBody)
			}
			news := "- **Pull request** on #1: Refuse an expired quote. Answer here: https://github.com/o/r/pull/2\n\ncc @" + digestRunOwner +
				"\n\n<!-- issuedog:digest-new items=pr-1-2 -->"
			if len(digest.Comments) != 1 || !strings.Contains(digest.Comments[0].Body, news) {
				t.Errorf("the digest's comments = %+v; want one announcing the PR, ending\n%s", digest.Comments, news)
			}
			if got, task := assignees(f, 2), assignees(f, 1); !slices.Equal(got, []string{digestRunOwner}) || len(task) != 0 {
				t.Errorf("the PR's assignees = %q, the task's %q; want %s on the PR, and nobody on the task", got, task, digestRunOwner)
			}
			iterate(t, d)
			prItemQuiet(t, f, d, "at pr-open")

			tt.end(f)
			f.SetAssignees(2, digestRunOwner, "bob")
			from := len(f.Writes())
			iterate(t, d)
			if got, want := assignWrites(f, from), []string{assignRemove(2, digestRunOwner)}; !slices.Equal(got, want) || !slices.Equal(assignees(f, 2), []string{"bob"}) {
				t.Errorf("the pass after the %s wrote assignees %q, leaving %q; want %q, leaving bob, who was assigned by hand", tt.name, got, assignees(f, 2), want)
			}
			if body := f.State().Issues[2].Body; strings.Contains(body, "Pull request") || !strings.Contains(body, "Nothing is waiting on you.") {
				t.Errorf("after the %s, the digest =\n%s\nwant nothing waiting", tt.name, body)
			}
			if task := f.State().Issues[0]; task.Closed != tt.closed || len(task.Assignees) != 0 {
				t.Errorf("after the %s, the task is closed %t, assigned %q; want closed %t, and nobody", tt.name, task.Closed, task.Assignees, tt.closed)
			}
			from = len(f.Writes())
			iterate(t, d)
			f.SetAssignees(2, digestRunOwner)
			iterate(t, d)
			if got := assignWrites(f, from); len(got) != 0 || !slices.Equal(assignees(f, 2), []string{digestRunOwner}) {
				t.Errorf("after the %s, the passes wrote assignees %q, the PR has %q; want nothing written: notify goes once, and an assign by hand stays",
					tt.name, got, assignees(f, 2))
			}
		})
	}
}

// A task held at pr-open has no pull request item, as a /hold is the
// owner's own, so notify is unassigned from the PR, and the task gets
// nobody. /resume brings the task back to pr-open, and the item back with
// its key, so it isn't announced again, and notify is assigned to the PR
// again.
func TestPRItemHeld(t *testing.T) {
	t.Parallel()
	o := prItemOrigin(t)
	f := prItemFake(t, o)
	d := prItemDaemon(t, f, o)
	iterate(t, d)
	iterate(t, d)

	f.AddComment(1, digestRunOwner, "/hold")
	from := len(f.Writes())
	iterate(t, d)
	if pickStage(f, 1) != "stage:held" {
		t.Fatalf("after /hold, the task is at %s; want held", pickStage(f, 1))
	}
	if got, want := assignWrites(f, from), []string{assignRemove(2, digestRunOwner)}; !slices.Equal(got, want) {
		t.Errorf("the /hold's pass wrote assignees %q; want %q", got, want)
	}
	if body := f.State().Issues[2].Body; strings.Contains(body, "Pull request") {
		t.Errorf("with the task held, the digest =\n%s\nwant no pull request", body)
	}

	f.AddComment(1, digestRunOwner, "/resume")
	iterate(t, d)
	from = len(f.Writes())
	iterate(t, d)
	if pickStage(f, 1) != "stage:pr-open" {
		t.Fatalf("after /resume, the task is at %s; want pr-open", pickStage(f, 1))
	}
	if got := assignWrites(f, from); len(got) != 0 {
		t.Errorf("the pass after the /resume's wrote assignees %q; want nothing more", got)
	}
	if got, task := assignees(f, 2), assignees(f, 1); !slices.Equal(got, []string{digestRunOwner}) || len(task) != 0 {
		t.Errorf("after /resume, the PR's assignees = %q, the task's %q; want %s on the PR, and nobody on the task", got, task, digestRunOwner)
	}
	digest := f.State().Issues[2]
	if !strings.Contains(digest.Body, prItemBody) || len(digest.Comments) != 1 {
		t.Errorf("after /resume, the digest =\n%s\nwith comments %+v; want it to list\n%s\nwith the one announcement", digest.Body, digest.Comments, prItemBody)
	}
	prItemQuiet(t, f, d, "after /resume")
}

// A restart reads the item back from the digest's body: with the task's
// read failing on the restart's first pass, the digest lists what its
// body showed, so it isn't edited, and the PR keeps notify, with no write,
// as the task keeps its last items. Once the read works, nothing is
// written either.
func TestPRItemRestart(t *testing.T) {
	t.Parallel()
	o := prItemOrigin(t)
	f := prItemFake(t, o)
	d := prItemDaemon(t, f, o)
	iterate(t, d)
	iterate(t, d)
	body := f.State().Issues[2].Body

	d = prItemDaemon(t, f, o)
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues/1" }, http.StatusInternalServerError)
	from := len(f.Writes())
	if err := d.Iterate(context.Background()); err == nil {
		t.Fatal("the restart's pass whose read of the task failed gave no error")
	}
	if got := writesOf(f)[from:]; len(got) != 0 || f.State().Issues[2].Body != body {
		t.Errorf("the restart's first pass wrote %q, the digest\n%s\nwant nothing written, and the digest as it was\n%s", got, f.State().Issues[2].Body, body)
	}
	if !strings.Contains(body, prItemBody) || !slices.Equal(assignees(f, 2), []string{digestRunOwner}) {
		t.Errorf("the digest =\n%s\nthe PR's assignees %q; want the item, and %s", body, assignees(f, 2), digestRunOwner)
	}
	prItemQuiet(t, f, d, "once the task's read works")
}

// A PR someone else opened is never a task's (SPEC §7.5): with the task's
// move naming one, there's no item, its assignees and labels are left
// alone, and the task is assigned nobody.
func TestPRItemSomeoneElsesPR(t *testing.T) {
	t.Parallel()
	o := digestRunOrigin(t)
	f := newFake(t)
	f.SetPermission(digestRunOwner, "admin")
	n := f.AddIssue(digestRunOwner, "Refuse an expired quote", "Booking an expired quote returns 410.", "type:task", "priority:p1")
	f.AddLabel(n, app, "stage:pr-open")
	pr := f.AddIssue("mallory", prItemTitle, "Closes #1", "priority:p3")
	f.SetPullRequest(pr)
	f.SetAssignees(pr, "bob")
	f.AddComment(n, app, "<!-- issuedog:transition from=verifying to=pr-open run=rv head="+strings.Repeat("ab", 20)+" pr="+strconv.Itoa(pr)+" -->")
	d := prItemDaemon(t, f, o)
	iterate(t, d)
	iterate(t, d)
	for _, w := range writesOf(f) {
		if strings.HasPrefix(w, "POST /issues/"+strconv.Itoa(pr)+"/") || strings.HasPrefix(w, "DELETE /issues/"+strconv.Itoa(pr)+"/") {
			t.Errorf("the passes wrote %s; want nothing on someone else's PR", w)
		}
	}
	if body := f.State().Issues[2].Body; !strings.Contains(body, "Nothing is waiting on you.") {
		t.Errorf("the digest =\n%s\nwant nothing waiting", body)
	}
	if got, task := assignees(f, pr), assignees(f, n); !slices.Equal(got, []string{"bob"}) || len(task) != 0 {
		t.Errorf("the PR's assignees = %q, the task's %q; want bob's left, and nobody on the task", got, task)
	}
}
