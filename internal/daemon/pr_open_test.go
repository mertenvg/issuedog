package daemon_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/logtest"
	"github.com/mertenvg/issuedog/internal/repo"
)

// A pass opens or updates the task's PR (SPEC §7.5, §8.1 step 4, §10.2,
// §11.5), on a verdictRig wired as main's, whose fake reads PRs' heads from
// its real origin, where the daemon pushes.

// prBranch is the branch of task 1, titled "Refuse an expired quote", as
// verifierTask files it (SPEC §7.5).
const prBranch = "issuedog/1-refuse-an-expired-quote"

// prPassComment is the App's pass comment of run on head, with notes, as
// Verifier.Apply posts one with no candidates (SPEC §10.2).
func prPassComment(t testing.TB, run, head, notes string) string {
	t.Helper()
	return "The verifier passed `" + head + "`.\n\n" + notes + "\n\n" +
		"<!-- issuedog:verify verdict=pass attempt=1 run=" + run + " head=" + head + " -->\n" + verifyNotesLine(t, notes)
}

// prTask files task 1 at verifying on a new head, as verifierTask does,
// with a clean pass of the verifier's on it, and returns the head and the
// pass comment's ID.
func (v *verdictRig) prTask() (string, int64) {
	v.t.Helper()
	head := v.head()
	n, _ := verifierTask(v.f, head)
	if n != 1 {
		v.t.Fatalf("the task is #%d, want #1", n)
	}
	return head, v.f.AddComment(n, app, prPassComment(v.t, "rv", head, "Fine."))
}

// originTip is branch's tip in v's origin, or "" with no such branch.
func (v *verdictRig) originTip(branch string) string {
	v.t.Helper()
	return gitIn(v.t, v.o.url, "for-each-ref", "--format=%(objectname)", "refs/heads/"+branch)
}

// mainSHA is main's tip in v's origin.
func (v *verdictRig) mainSHA() string {
	v.t.Helper()
	return gitIn(v.t, v.o.url, "rev-parse", "main")
}

// otherHead commits files on a new branch, name, from main in v's origin's
// work tree, and returns the commit, which isn't pushed anywhere.
func (v *verdictRig) otherHead(name string, files map[string]string) string {
	v.t.Helper()
	gitIn(v.t, v.o.work, "checkout", "--quiet", "-b", name, "main")
	for path, content := range files {
		if err := os.WriteFile(filepath.Join(v.o.work, path), []byte(content), 0o644); err != nil {
			v.t.Fatal(err)
		}
		gitIn(v.t, v.o.work, "add", path)
	}
	gitIn(v.t, v.o.work, "commit", "--quiet", "--allow-empty", "-m", "the commit of "+name)
	sha := gitIn(v.t, v.o.work, "rev-parse", "HEAD")
	gitIn(v.t, v.o.work, "checkout", "--quiet", "main")
	return sha
}

// prVerified is the PR body's last line for task 1 at head, verified
// against main by the pass in comment pass (SPEC §11.5).
func prVerified(head, main string, pass int64) string {
	return "Verified at `" + head + "`, against `main` at `" + main + "`: https://github.com/o/r/issues/1#issuecomment-" + strconv.FormatInt(pass, 10)
}

// prResults is the PR body's Results for v's two commands, all passed.
func (v *verdictRig) prResults() string {
	return "## Results\n\n| Check | Result |\n|---|---|\n" +
		"| `echo ran >> '" + v.ran + "'` | pass |\n" +
		"| `echo " + verifierRunPrinted + "` | pass |\n" +
		"| Rules 2 to 7 | pass |"
}

// prFallbackBody is the body of task 1's PR whose move to verifying keeps
// no pr-text, with the notes "Fine.", as prTask files it.
func (v *verdictRig) prFallbackBody(head, main string, pass int64) string {
	return "Closes #1\n\n## What changed\n\n" +
		"The implementer's summary isn't on #1: the task's move to `verifying` keeps no `pr-text` data, as a move made by hand doesn't.\n\n" +
		v.prResults() + "\n\n## Verifier notes\n\nFine.\n\n" + prVerified(head, main, pass)
}

// prRequests counts the requests to /pulls since from, and the writes.
func prRequests(f *githubtest.Fake, from int) (reads, writes int) {
	for _, r := range f.Requests()[from:] {
		if !strings.HasPrefix(r.Path, "/pulls") {
			continue
		}
		if r.Method == http.MethodGet {
			reads++
		} else {
			writes++
		}
	}
	return reads, writes
}

// A whole run against the fakes, from the implementer's commits to the PR:
// the implementer commits and moves the task to verifying, the next pass
// runs the checks, the pass after runs the verifier, which passes with no
// candidates, and the pass after that pushes the head to the task's branch,
// and opens one PR from it to main, in the SPEC §11.5 format, from the
// implementer's pr-text and the verifier's notes, with the task's priority
// label. The task stays at verifying, holding the worker, and the pass
// after only finds the PR, and each unchanged pass after makes no request
// but 304s: no second push, PR or label.
func TestPROpenWholeRun(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	out := implementerOutput(t, "commits")
	v.out(1, ivResult(t, out))
	notes := "The expiry check runs before the write, as the criteria ask."
	v.out(2, verifierPass(t, notes))
	n := f.AddIssue(wholeRunOwner, "Refuse an expired quote", "Booking an expired quote returns 410.", "type:task", "priority:p1", "scope:booking")
	f.AddLabel(n, wholeRunOwner, "stage:in-progress")
	ready := fileAt(f, "ready", "The next task.", "priority:p1", "scope:booking", "type:task")
	main := v.mainSHA()

	v.passes(3)
	head := v.trimmed("commit.1")
	passes := passComments(f, n)
	if v.runs() != 2 || len(passes) != 1 || v.originTip(prBranch) != "" || len(f.State().PRs) != 0 {
		t.Fatalf("after the verifier's pass: %d runs, verdicts %q, the branch at %q, PRs %+v; want two, one, no branch and no PR yet",
			v.runs(), passes, v.originTip(prBranch), f.State().PRs)
	}
	pass := filingPassID(t, f, n, v.leaseOf("2").RunID)

	writes := len(f.Writes())
	v.pass()
	if got := v.originTip(prBranch); got != head {
		t.Errorf("the branch %s is at %q, want the head %s", prBranch, got, head)
	}
	prs := f.State().PRs
	if len(prs) != 1 {
		t.Fatalf("the PRs = %+v; want one", prs)
	}
	pr := prs[0]
	sum, _ := out["summary"].(map[string]any)
	var tests []string
	for _, kind := range []string{"added", "updated"} {
		list, _ := sum["tests_"+kind].([]any)
		for _, test := range list {
			s, _ := test.(string)
			tests = append(tests, "- "+kind+": "+s)
		}
	}
	whatChanged, _ := sum["what_changed"].(string)
	why, _ := sum["why"].(string)
	body := "Closes #1\n\n## What changed\n\n" + whatChanged + "\n\n## Why\n\n" + why + "\n\n## Tests\n\n" + strings.Join(tests, "\n") + "\n\n" +
		v.prResults() + "\n\n## Verifier notes\n\n" + notes + "\n\n" + prVerified(head, main, pass)
	want := githubtest.PRState{Number: pr.Number, Head: prBranch, Base: "main", HeadSHA: head,
		Title: "feat: refuse booking an expired quote, with a link to a new one", Body: body, State: "open"}
	if pr != want {
		t.Errorf("the PR =\n%+v\nwant\n%+v", pr, want)
	}
	if got := f.State().Issues[pr.Number-1]; !slices.Equal(got.Labels, []string{"priority:p1"}) || got.Author != app {
		t.Errorf("the PR, by %s, has labels %q; want the App's, with priority:p1", got.Author, got.Labels)
	}
	if got, want := writesOf(f)[writes:], []string{"POST /pulls", "POST /issues/" + strconv.Itoa(pr.Number) + "/labels"}; !slices.Equal(got, want) {
		t.Errorf("the PR step wrote %q, want %q", got, want)
	}
	if pickStage(f, n) != "stage:verifying" || len(passComments(f, n)) != 1 {
		t.Errorf("the task is at %s with verdicts %q; want still at verifying, the one pass", pickStage(f, n), passComments(f, n))
	}

	from, writes := len(f.Requests()), len(f.Writes())
	v.pass()
	if reads, prWrites := prRequests(f, from); reads != 1 || prWrites != 0 || len(f.Writes()) != writes {
		t.Errorf("the pass after the PR read /pulls %d times, and wrote %+v; want one find, and nothing", reads, f.Writes()[writes:])
	}
	for _, step := range []string{"first", "second"} {
		from := len(f.Requests())
		v.quietPass(step)
		if reads, _ := prRequests(f, from); reads != 1 {
			t.Errorf("%s: the unchanged pass read /pulls %d times, want once: the find", step, reads)
		}
	}
	if len(f.State().PRs) != 1 || v.originTip(prBranch) != head || v.runs() != 2 {
		t.Errorf("the PRs = %+v, the branch at %s, %d runs; want the one PR, the head, two runs", f.State().PRs, v.originTip(prBranch), v.runs())
	}
	if pickStage(f, ready) != "stage:ready" {
		t.Errorf("the next task is at %s, want ready: the task waiting at verifying holds the worker", pickStage(f, ready))
	}
}

// The PR step comes only once the checks pass on the head and main as they
// are: after a restart, and after main moves, the checks run again first,
// on a pass that opens no PR, and the PR opens on the next, against the new
// main.
func TestPROpenAfterTheChecks(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		act  func(v *verdictRig) *verdictRig
	}{
		{"a restart", func(v *verdictRig) *verdictRig { return v.restart() }},
		{"a move of main", func(v *verdictRig) *verdictRig {
			v.o.commit("docs.md", "Docs.\n")
			return v
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			v := newVerdictRig(t, f, 3)
			head, pass := v.prTask()
			v.pass()
			if v.checksRan() != 1 || len(f.State().PRs) != 0 {
				t.Fatalf("after the checks: the commands at %d, PRs %+v; want one, and none", v.checksRan(), f.State().PRs)
			}
			v = tt.act(v)
			v.pass()
			if v.checksRan() != 2 || len(f.State().PRs) != 0 || v.originTip(prBranch) != "" {
				t.Fatalf("the pass after %s: the commands at %d, PRs %+v, the branch at %q; want two, none, no branch: the checks come first",
					tt.name, v.checksRan(), f.State().PRs, v.originTip(prBranch))
			}
			v.pass()
			prs := f.State().PRs
			if len(prs) != 1 || prs[0].HeadSHA != head || prs[0].Body != v.prFallbackBody(head, v.mainSHA(), pass) ||
				prs[0].Title != "feat: Refuse an expired quote" {
				t.Errorf("the PRs = %+v; want one, at %s, titled from the task, with the fallback body against main at %s", prs, head, v.mainSHA())
			}
		})
	}
}

// main moving under an open PR at verifying runs the checks again, then
// updates the PR's body, which names main's new SHA, alone: no push, no
// second PR, no state and no title sent.
func TestPROpenUpdatesTheBody(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head, pass := v.prTask()
	v.prOpens(head)
	v.o.commit("docs.md", "Docs.\n")
	v.pass()
	writes := len(f.Writes())
	v.pass()
	prs := f.State().PRs
	if len(prs) != 1 || prs[0].Body != v.prFallbackBody(head, v.mainSHA(), pass) || prs[0].State != "open" {
		t.Errorf("the PRs = %+v; want the one, open, against main at %s", prs, v.mainSHA())
	}
	w := f.Writes()[writes:]
	if len(w) != 1 || w[0].Method != http.MethodPatch || w[0].Path != "/pulls/"+strconv.Itoa(prs[0].Number) ||
		!strings.HasPrefix(w[0].Body, `{"body":`) || strings.Contains(w[0].Body, `"state"`) || strings.Contains(w[0].Body, `"title"`) {
		t.Errorf("the update wrote %+v; want one PATCH of the PR's body alone", w)
	}
}

// A PR closed at verifying is reopened, in a request that sets the state
// alone, then updated, by the task's next pass through verify (SPEC §7.5's
// A closed PR). The close here comes with a move of main, so the body
// differs too.
func TestPROpenReopensAClosedPR(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head, pass := v.prTask()
	v.prOpens(head)
	pr := f.State().PRs[0].Number
	f.ClosePR(pr, "owner")
	v.o.commit("docs.md", "Docs.\n")
	v.pass()
	writes := len(f.Writes())
	v.pass()
	prs := f.State().PRs
	if len(prs) != 1 || prs[0].State != "open" || prs[0].Body != v.prFallbackBody(head, v.mainSHA(), pass) {
		t.Errorf("the PRs = %+v; want the one, reopened, against main at %s", prs, v.mainSHA())
	}
	w := f.Writes()[writes:]
	if len(w) != 2 || w[0].Path != "/pulls/"+strconv.Itoa(pr) || w[0].Body != `{"state":"open"}`+"\n" ||
		w[1].Path != "/pulls/"+strconv.Itoa(pr) || !strings.HasPrefix(w[1].Body, `{"body":`) {
		t.Errorf("the PR step wrote %+v; want the reopen, its state alone, then the body", w)
	}
	v.passes(2)
	if len(f.State().PRs) != 1 {
		t.Errorf("the PRs = %+v; want the one", f.State().PRs)
	}
}

// A PR GitHub won't reopen, as once the App pushed a new head over its own
// dropped one since the close, gets a new PR (SPEC §7.5's A closed PR). The
// new head, from main, isn't in the old one's history, but a clean App
// marker names the old one as a head, so the push goes over it.
func TestPROpenRefusedReopen(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	first, _ := v.prTask()
	v.prOpens(first)
	old := f.State().PRs[0].Number
	f.ClosePR(old, "owner")

	second := v.otherHead("second", map[string]string{"quote.txt": "refused\n"})
	gitIn(t, v.d.Clone.Dir(), "fetch", "--quiet", "--no-tags", v.o.work, "+refs/heads/second:"+repo.TasksRef+"1")
	f.AddComment(1, app, "Verify failed.\n\n<!-- issuedog:transition from=verifying to=in-progress attempt=1 event=v1 head="+first+" -->")
	f.AddComment(1, app, "The implementer committed.\n\n<!-- issuedog:transition from=in-progress to=verifying attempt=2 run=r2 head="+second+" -->")
	pass := f.AddComment(1, app, prPassComment(t, "rv2", second, "Fine."))
	v.passes(2)
	if got := v.originTip(prBranch); got != second {
		t.Errorf("the branch is at %s, want the new head %s, forced over the old", got, second)
	}
	prs := f.State().PRs
	if len(prs) != 2 || prs[0].Number != old || prs[0].State != "closed" || prs[1].State != "open" || prs[1].HeadSHA != second ||
		prs[1].Body != v.prFallbackBody(second, v.mainSHA(), pass) {
		t.Errorf("the PRs = %+v; want #%d still closed, and a new one, open at %s", prs, old, second)
	}
	if got := f.State().Issues[prs[1].Number-1].Labels; !slices.Equal(got, []string{"priority:p1"}) {
		t.Errorf("the new PR's labels = %q; want priority:p1", got)
	}
	v.passes(2)
	if len(f.State().PRs) != 2 {
		t.Errorf("the PRs = %+v; want the two", f.State().PRs)
	}
}

// A pass from before the verify-notes marker, with none, gives the PR no
// notes: its Verifier notes say "None.", and the rest is as any PR's.
func TestPROpenPassWithNoNotes(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.head()
	n, _ := verifierTask(f, head)
	pass := f.AddComment(n, app, "The verifier passed `"+head+"`.\n\n<!-- issuedog:verify verdict=pass attempt=1 run=rv head="+head+" -->")
	v.passes(2)
	want := strings.Replace(v.prFallbackBody(head, v.mainSHA(), pass), "## Verifier notes\n\nFine.", "## Verifier notes\n\nNone.", 1)
	if prs := f.State().PRs; len(prs) != 1 || prs[0].Body != want {
		t.Errorf("the PRs = %+v; want one, with the body\n%s", prs, want)
	}
}

// A pass with candidates moves on only once its filed record stands
// (SPEC §10.2): the pass that files them opens no PR, and the next does.
func TestPROpenWaitsForTheFiledRecord(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.head()
	n, _ := verifierTask(f, head)
	v.out(1, ivResult(t, verifierCandidateOutput("A quote that expires mid-booking")))
	v.passes(2)
	if len(passComments(f, n)) != 1 || len(filingFollowUps(f)) != 0 {
		t.Fatalf("after the run: verdicts %q, follow-ups %+v; want one, and none filed yet", passComments(f, n), filingFollowUps(f))
	}
	v.pass()
	if len(filingFollowUps(f)) != 1 || len(f.State().PRs) != 0 || v.originTip(prBranch) != "" {
		t.Fatalf("after the filing: follow-ups %+v, PRs %+v, the branch at %q; want one, and no PR or push yet",
			filingFollowUps(f), f.State().PRs, v.originTip(prBranch))
	}
	v.pass()
	if prs := f.State().PRs; len(prs) != 1 || prs[0].HeadSHA != head {
		t.Errorf("the PRs = %+v; want one, at %s, once the record stands", prs, head)
	}
}

// A pass whose filing hit the followups state error never gets its filed
// record, whether its candidates don't read back, which step 1 reports
// once itself, or GitHub refused to file one, whose error stands here: the
// task waits at verifying, with no PR, and each unchanged pass makes no
// request but 304s, none of them for a PR, and runs no command (SPEC
// §10.2).
func TestPROpenWaitsOnStuckFollowUps(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		data string // the pass comment's verify-data marker's data
		// refused, if set, posts the followups error for the pass, as a
		// filing GitHub refused leaves it.
		refused bool
	}{
		{"data that won't read back", "AAAA", false},
		{"a filing GitHub refused", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			v := newVerdictRig(t, f, 3)
			head := v.head()
			n, move := verifierTask(f, head)
			data := tt.data
			if data == "" {
				data = filingData(t, `[{"title":"A refund after expiry","scope":"booking","scenario":"S","likelihood":"Rare","impact":"A refund"}]`)
			}
			pass := f.AddComment(n, app, prPassComment(t, "rv", head, "Fine.")+"\n<!-- issuedog:verify-data v=1 data="+data+" -->")
			if tt.refused {
				f.AddComment(n, app, "The follow-ups can't be filed.\n\n<!-- issuedog:error kind=followups key="+
					strconv.FormatInt(pass, 10)+"-"+strconv.FormatInt(move, 10)+" -->")
			}
			v.passes(4)
			if len(errorMarkers(f, n)) != 1 || len(filingFollowUps(f)) != 0 || len(f.State().PRs) != 0 || v.originTip(prBranch) != "" {
				t.Fatalf("errors %q, follow-ups %+v, PRs %+v, the branch at %q; want the one followups error, nothing filed, and no PR or push",
					errorMarkers(f, n), filingFollowUps(f), f.State().PRs, v.originTip(prBranch))
			}
			for _, step := range []string{"first", "second"} {
				from := len(f.Requests())
				v.quietPass(step)
				if reads, writes := prRequests(f, from); reads != 0 || writes != 0 {
					t.Errorf("%s: the waiting task's pass made %d reads and %d writes of /pulls; want none", step, reads, writes)
				}
			}
		})
	}
}

// The task's PR is found among the App's PRs from its branch to main that
// didn't merge: one merged, as by hand with no closing keyword, and one to
// another base are never reused, so a new PR opens.
func TestPROpenSkipsOtherPRs(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head, _ := v.prTask()
	gitIn(t, v.o.work, "push", "--quiet", v.o.url, "task:refs/heads/"+prBranch, "main:refs/heads/other")
	c, ctx := f.Client(t), context.Background()
	merged, err := c.CreatePullRequest(ctx, "o", "r", github.NewPullRequest{Title: "Merged", Head: prBranch, Base: "main", Body: "Merged by hand."})
	if err != nil {
		t.Fatal(err)
	}
	f.MergePR(merged.Number, "owner")
	other, err := c.CreatePullRequest(ctx, "o", "r", github.NewPullRequest{Title: "Other", Head: prBranch, Base: "other", Body: "Another base."})
	if err != nil {
		t.Fatal(err)
	}
	writes := len(f.Writes())
	v.passes(2)
	for _, w := range f.Writes()[writes:] {
		if w.Path == "/pulls/"+strconv.Itoa(merged.Number) || w.Path == "/pulls/"+strconv.Itoa(other.Number) {
			t.Errorf("the PR step wrote %+v; want nothing to the merged PR or the one to another base", w)
		}
	}
	prs := f.State().PRs
	if len(prs) != 3 || prs[2].Base != "main" || prs[2].State != "open" || prs[2].HeadSHA != head || !strings.HasPrefix(prs[2].Body, "Closes #1\n\n") {
		t.Fatalf("the PRs = %+v; want a new one to main, open at %s", prs, head)
	}
	if prs[0].Number != merged.Number || !prs[0].Merged || prs[0].Title != "Merged" || prs[1].Number != other.Number || prs[1].Title != "Other" || prs[1].Body != "Another base." {
		t.Errorf("the PRs = %+v; want the merged one and the one to another base as they were", prs)
	}
}

// The open PR is the task's, even when a closed one from its branch is
// newer, as when the owner reopened an older one: a move of main updates
// the open one's body, and the closed one stays as it is.
func TestPROpenFindsTheOpenPR(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head, pass := v.prTask()
	v.prOpens(head)
	older := f.State().PRs[0].Number
	f.ClosePR(older, "owner")
	newer, err := f.Client(t).CreatePullRequest(context.Background(), "o", "r", github.NewPullRequest{Title: "Newer", Head: prBranch, Base: "main", Body: "A newer one."})
	if err != nil {
		t.Fatal(err)
	}
	f.ClosePR(newer.Number, "owner")
	f.ReopenPR(older, "owner")
	v.o.commit("docs.md", "Docs.\n")
	v.passes(2)
	prs := f.State().PRs
	if len(prs) != 2 || prs[0].State != "open" || prs[0].Body != v.prFallbackBody(head, v.mainSHA(), pass) ||
		prs[1].State != "closed" || prs[1].Body != "A newer one." {
		t.Errorf("the PRs = %+v; want #%d open, against main at %s, and #%d closed as it was", prs, older, v.mainSHA(), newer.Number)
	}
}

// A body over GitHub's limit is cut, and the line that stands in for what
// it leaves out links the comment that has it: here the implementer's
// summary, so the line links the move to verifying (SPEC §11.5).
func TestPROpenCutsTheSummary(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.head()
	out := implementerOutput(t, "commits")
	sum, _ := out["summary"].(map[string]any)
	sum["what_changed"] = strings.Repeat("A long change. ", 5000)
	n := f.AddIssue("owner", "Refuse an expired quote", "Booking an expired quote returns 410.", "type:task", "priority:p1", "scope:booking")
	f.AddLabel(n, app, "stage:verifying")
	move := f.AddComment(n, app, "The implementer committed.\n\n<!-- issuedog:transition from=in-progress to=verifying attempt=1 run=r1 head="+head+" -->\n"+prTextLine(t, out))
	f.AddComment(n, app, prPassComment(t, "rv", head, "Fine."))
	v.passes(2)
	prs := f.State().PRs
	want := "## What changed\n\nThe implementer's summary is too long for this description, so it's left out. It's in [its comment](https://github.com/o/r/issues/1#issuecomment-" +
		strconv.FormatInt(move, 10) + ").\n\n## Results"
	if len(prs) != 1 || !strings.Contains(prs[0].Body, want) || prs[0].Title != "feat: refuse booking an expired quote, with a link to a new one" {
		t.Errorf("the PRs = %+v; want one, the implementer's title, its body holding\n%s", prs, want)
	}
}

// A head whose own diff from main is empty, as when an answer's only
// commit is an empty one, gets no push and no PR, on any pass, and the
// next task isn't picked, as it holds the worker.
func TestPROpenEmptyHead(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.otherHead("empty", nil)
	gitIn(t, v.d.Clone.Dir(), "fetch", "--quiet", "--no-tags", v.o.work, "refs/heads/empty:"+repo.TasksRef+"1")
	n, _ := verifierTask(f, head)
	f.AddComment(n, app, prPassComment(t, "rv", head, "Nothing needed to change."))
	v.passes(3)
	if v.originTip(prBranch) != "" || len(f.State().PRs) != 0 || pickStage(f, n) != "stage:verifying" {
		t.Errorf("the branch at %q, PRs %+v, at %s; want no push, no PR, still at verifying", v.originTip(prBranch), f.State().PRs, pickStage(f, n))
	}
	from := len(f.Requests())
	v.quietPass("an empty head")
	if reads, writes := prRequests(f, from); reads != 0 || writes != 0 {
		t.Errorf("the empty head's pass made %d reads and %d writes of /pulls; want none", reads, writes)
	}
}

// Before any write, the issue is read again: a task closed, held by a
// discussion trigger, or moved by hand between step 1 and the PR step gets
// no push and no PR.
func TestPROpenMovedBeforeTheStep(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		during func(f *githubtest.Fake, n int)
		label  string // the stage label the task ends with
	}{
		{"closed", func(f *githubtest.Fake, n int) { f.CloseBy(n, "owner") }, "stage:verifying"},
		{"held", func(f *githubtest.Fake, n int) { f.AddComment(n, "mallory", "Wait.") }, "stage:held"},
		{"moved by hand", func(f *githubtest.Fake, n int) {
			f.RemoveLabel(n, "owner", "stage:verifying")
			f.AddLabel(n, "owner", "stage:ready")
		}, "stage:ready"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			v := newVerdictRig(t, f, 3)
			v.prTask()
			v.pass()
			// The find is the PR step's first request, and the read again
			// comes after it.
			f.BeforeRequest(func(r githubtest.Request) bool {
				return r.Method == http.MethodGet && r.Path == "/pulls"
			}, func() { tt.during(f, 1) })
			v.pass()
			if v.originTip(prBranch) != "" || len(f.State().PRs) != 0 {
				t.Errorf("the branch at %q, PRs %+v; want no push and no PR", v.originTip(prBranch), f.State().PRs)
			}
			if labels := ivLabels(f, 1); !slices.Contains(labels, tt.label) {
				t.Errorf("the task's labels are %v, want %s", labels, tt.label)
			}
		})
	}
}

// Two branches with the task's prefix: the daemon can't tell which is the
// task's, so it pushes to neither and opens no PR, on any pass (SPEC §7.5).
func TestPROpenTwoBranches(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	v.prTask()
	main := v.mainSHA()
	gitIn(t, v.o.work, "push", "--quiet", v.o.url, "main:refs/heads/issuedog/1-a", "main:refs/heads/issuedog/1-b")
	v.passes(3)
	if a, b := v.originTip("issuedog/1-a"), v.originTip("issuedog/1-b"); a != main || b != main || v.originTip(prBranch) != "" || len(f.State().PRs) != 0 {
		t.Errorf("the branches at %s and %s, %s at %q, PRs %+v; want both left at main's %s, no other, and no PR",
			a, b, prBranch, v.originTip(prBranch), f.State().PRs, main)
	}
}

// A tip of the task's branch that isn't in the head's history, and that no
// App marker names, is someone else's commits: nothing is pushed over it,
// no PR opens, and the warning is logged once, not on every pass. It reads
// logr, so it isn't parallel.
func TestPROpenSomeoneElsesTip(t *testing.T) {
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head, _ := v.prTask()
	theirs := v.otherHead("theirs", map[string]string{"theirs.txt": "Theirs.\n"})
	gitIn(t, v.o.work, "push", "--quiet", v.o.url, "theirs:refs/heads/"+prBranch)
	stop := logtest.Capture(t, logr.W)
	v.passes(4)
	logs := stop()
	if got := v.originTip(prBranch); got != theirs || len(f.State().PRs) != 0 {
		t.Errorf("the branch at %s, PRs %+v; want their %s left as it is, and no PR", got, f.State().PRs, theirs)
	}
	want := "#1: its branch " + prBranch + " is at " + theirs + ", which isn't in its head " + head + "'s history"
	if got := strings.Count(logs, want); got != 1 {
		t.Errorf("the warnings =\n%s\nwant %q once", logs, want)
	}
}

// A push the remote refuses for any reason but a stale lease, here a
// pre-receive hook on the origin, is the pass's error, not a wait for the
// next sync, and its stderr, which the App didn't write, reaches no
// comment. A lease gone stale, as when the branch moved after the sync, is
// no error: nothing is pushed or opened, and the next pass, synced, pushes
// and opens the PR.
func TestPROpenPushRefused(t *testing.T) {
	t.Parallel()
	t.Run("a hook", func(t *testing.T) {
		t.Parallel()
		f := newFake(t)
		v := newVerdictRig(t, f, 3)
		head, _ := v.prTask()
		hook := filepath.Join(v.o.url, "hooks", "pre-receive")
		if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'refused by @mallory #12' >&2\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		v.pass()
		err := v.d.Iterate(context.Background())
		if err == nil || !strings.Contains(err.Error(), "refused by @mallory #12") || errors.Is(err, repo.ErrPushStale) {
			t.Errorf("the pass's error = %v; want the hook's refusal, not a stale lease", err)
		}
		if v.originTip(prBranch) != "" || len(f.State().PRs) != 0 {
			t.Errorf("the branch at %q, PRs %+v; want no push and no PR", v.originTip(prBranch), f.State().PRs)
		}
		for _, c := range f.State().Issues[0].Comments {
			if strings.Contains(c.Body, "refused by") {
				t.Errorf("a comment quotes the push's stderr:\n%s", c.Body)
			}
		}
		if err := os.Remove(hook); err != nil {
			t.Fatal(err)
		}
		v.pass()
		if prs := f.State().PRs; len(prs) != 1 || prs[0].HeadSHA != head {
			t.Errorf("the PRs = %+v; want one at %s once the hook is gone", prs, head)
		}
	})
	t.Run("a stale lease", func(t *testing.T) {
		t.Parallel()
		f := newFake(t)
		v := newVerdictRig(t, f, 3)
		head, _ := v.prTask()
		base := gitIn(t, v.o.url, "rev-parse", "main~1")
		gitIn(t, v.o.url, "update-ref", "refs/heads/"+prBranch, base)
		v.pass()
		// After the sync, the branch moves on to main, still in the head's
		// history, so the lease the push names is stale.
		f.BeforeRequest(func(r githubtest.Request) bool {
			return r.Method == http.MethodGet && r.Path == "/pulls"
		}, func() { gitIn(t, v.o.url, "update-ref", "refs/heads/"+prBranch, v.mainSHA()) })
		v.pass()
		if got := v.originTip(prBranch); got != v.mainSHA() || len(f.State().PRs) != 0 {
			t.Errorf("the branch at %s, PRs %+v; want it left at main's %s, and no PR", got, f.State().PRs, v.mainSHA())
		}
		v.pass()
		if prs := f.State().PRs; v.originTip(prBranch) != head || len(prs) != 1 || prs[0].HeadSHA != head {
			t.Errorf("the branch at %s, PRs %+v; want the head %s, and one PR at it", v.originTip(prBranch), prs, head)
		}
	})
}

// The PR step, a crash at each of its writes, then a restart: the replay
// ends with the same push, PR and label (SPEC §5.2), whether the step
// opens the PR, or reopens a closed one and updates its body. Each pass is a
// fresh daemon on the fake's own bare clone, so it runs the checks again
// before the PR step. The origin is shared, so every run's commits, and so
// its SHAs, are the clean run's, and each setup sets the task's branch
// there as its case has it.
func TestPROpenReplay(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		// closed, if set, has the task's branch at the head, and the App's
		// PR from it closed, with another body and no label.
		closed bool
	}{
		{"opened", false},
		{"reopened", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			base := newVerdictRig(t, newFake(t), 3)
			head := base.head()
			roots, clones := map[*githubtest.Fake]string{}, map[*githubtest.Fake]*repo.Repo{}
			githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
				if gitIn(t, base.o.url, "for-each-ref", "refs/heads/"+prBranch) != "" {
					gitIn(t, base.o.url, "update-ref", "-d", "refs/heads/"+prBranch)
				}
				f := newFake(tb)
				f.SetPermission(wholeRunOwner, "admin")
				f.SetGitRemote(base.o.url)
				n, _ := verifierTask(f, head)
				f.AddComment(n, app, prPassComment(t, "rv", head, "Fine."))
				if tt.closed {
					gitIn(t, base.o.work, "push", "--quiet", base.o.url, "task:refs/heads/"+prBranch)
					pr, err := f.Client(tb).CreatePullRequest(context.Background(), "o", "r",
						github.NewPullRequest{Title: "feat: Refuse an expired quote", Head: prBranch, Base: "main", Body: "An older body."})
					if err != nil {
						tb.Fatal(err)
					}
					f.ClosePR(pr.Number, "owner")
					f.ForgetWrites()
				}
				roots[f] = t.TempDir()
				return f
			}, func(f *githubtest.Fake) error {
				g := newRig(t, f, base.o, roots[f])
				if clones[f] == nil {
					clones[f] = g.d.Clone
					fetchStartHead(t, g, base.o, head)
				}
				g.d.Clone, g.r.Workspace.Bare = clones[f], clones[f].Dir()
				v := &verdictRig{rig: g, o: base.o, cfg: base.cfg, ran: base.ran}
				v.wire()
				for range 2 {
					if err := v.d.Iterate(context.Background()); err != nil {
						return err
					}
				}
				return nil
			})
			for f := range roots {
				prs := f.State().PRs
				if len(prs) != 1 || prs[0].HeadSHA != head || prs[0].Head != prBranch || prs[0].State != "open" || !strings.HasPrefix(prs[0].Body, "Closes #1\n\n") {
					t.Errorf("after the replay, the PRs = %+v; want one, open from %s at %s, with the PR step's body", prs, prBranch, head)
				}
				if got := f.State().Issues[prs[0].Number-1].Labels; !slices.Equal(got, []string{"priority:p1"}) {
					t.Errorf("after the replay, the PR's labels = %q; want priority:p1", got)
				}
			}
		})
	}
}
