package daemon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/logtest"
	"github.com/mertenvg/issuedog/internal/repo"
)

// The App's merge of a task's PR into its story branch (SPEC §6.4, §7.5,
// §8.1), on a newStoryVerifyRig, with real temporary repos: task #1 is in
// story #2, and storyMergeReady runs the PR step's pass, so the task is at
// pr-open with its PR into the story branch open at its verified head,
// verified against the branch's tip, and the next pass is the one that
// merges it. The fake's merge writes the squash commit onto the story
// branch in the origin, as GitHub does.

// storyMergeReady makes task #1's PR into story #2's branch, as
// storyPRReady does, and runs the PR step's pass, so the task is at
// pr-open. It returns the fake, the rig, the story branch's tip the head
// was verified against, the head, and the PR's number.
func storyMergeReady(t *testing.T) (*githubtest.Fake, *verdictRig, string, string, int) {
	t.Helper()
	f, v, _, tip, head, pass := storyPRReady(t)
	v.pass()
	return f, v, tip, head, storyPROpened(t, f, v, head, tip, pass)
}

// storyMergePath is the path of PR pr's merge, PUT /pulls/{n}/merge.
func storyMergePath(pr int) string {
	return "/pulls/" + strconv.Itoa(pr) + "/merge"
}

// storyMerges are the writes since from that merge PR pr, failed ones
// included.
func storyMerges(f *githubtest.Fake, from, pr int) []githubtest.Write {
	var merges []githubtest.Write
	for _, w := range f.Writes()[from:] {
		if w.Method == http.MethodPut && w.Path == storyMergePath(pr) {
			merges = append(merges, w)
		}
	}
	return merges
}

// storyPRMatch matches the read of PR pr, GET /pulls/{n}, the one just
// before the App's merge.
func storyPRMatch(pr int) func(githubtest.Request) bool {
	return func(r githubtest.Request) bool {
		return r.Method == http.MethodGet && r.Path == "/pulls/"+strconv.Itoa(pr)
	}
}

// storyMergeBody is what a merge's request sent, as GitHub's merge API
// takes it.
type storyMergeBody struct {
	SHA           string `json:"sha"`
	MergeMethod   string `json:"merge_method"`
	CommitTitle   string `json:"commit_title"`
	CommitMessage string `json:"commit_message"`
}

// storyMerged fails unless PR pr is merged by the App, by one merge in all
// of f's writes, and story #2's branch is at a squash commit whose only
// parent is tip, the tip head was verified against, and which changes only
// head's quote.txt there. It returns the squash commit.
func (v *verdictRig) storyMerged(pr int, tip string) string {
	v.t.Helper()
	i := slices.IndexFunc(v.f.State().PRs, func(p githubtest.PRState) bool { return p.Number == pr })
	if i < 0 {
		v.t.Fatalf("PR #%d isn't among %+v", pr, v.f.State().PRs)
	}
	if got := v.f.State().PRs[i]; got.State != "closed" || !got.Merged || got.MergedBy != app {
		v.t.Errorf("PR #%d = %+v; want merged and closed by %s", pr, got, app)
	}
	if got := storyMerges(v.f, 0, pr); len(got) != 1 {
		v.t.Errorf("PR #%d's merges = %+v; want one", pr, got)
	}
	squash := v.originTip(storyVerifyBranch)
	if got := gitIn(v.t, v.o.url, "rev-list", "--parents", "-n", "1", squash); got != squash+" "+tip {
		v.t.Errorf("the story branch's tip and its parents are %q; want %s with the one parent %s", got, squash, tip)
	}
	if got := gitIn(v.t, v.o.url, "diff", "--name-only", tip, squash); got != "quote.txt" {
		v.t.Errorf("the squash commit changes %q; want quote.txt alone", got)
	}
	return squash
}

// storyMergeCarried runs the pass after the App's merge of PR pr, task 1's,
// as squash, and fails unless it carried the tip's check over to squash,
// the story branch's new tip, as success, with the commands run no more,
// deleted the task's branch, and closed the task as completed, from
// pr-open, keyed event=g<pr> (SPEC §7.5's Carrying the check over). Then
// it runs the two passes that read the close and judge it, after which
// the task is at completed.
func (v *verdictRig) storyMergeCarried(pr int, squash string) {
	v.t.Helper()
	ran := v.checksRan()
	v.pass()
	if got := v.f.State().CheckRuns; !slices.Contains(got, tipCheck(squash, storyVerifyBranch, "success")) || v.checksRan() != ran {
		v.t.Errorf("the pass after the merge left the checks %+v, and ran the commands %d times; want the carried success on %s, and never",
			got, v.checksRan()-ran, squash)
	}
	if got := v.originTip(prBranch); got != "" {
		v.t.Errorf("the task's branch is at %q; want it deleted", got)
	}
	mergedClosed(v.t, v.f, pr, "pr-open", "The App merged PR #"+strconv.Itoa(pr)+" into the story branch as `"+squash+"`, so this task is complete.\n\n")
	v.passes(2)
	if got := pickStage(v.f, 1); got != "stage:completed" {
		v.t.Errorf("the task is at %s; want stage:completed", got)
	}
}

// storyMergeSettles runs the pass after task 1's PR step, which merges its
// PR, pr, into story #2's branch (prMerge), then the pass that carries the
// tip's check over to the squash commit and closes the task, with no
// command run, and the two that read the close (storyMergeCarried), so the
// next unchanged pass is quiet.
func (v *verdictRig) storyMergeSettles(pr int) {
	v.t.Helper()
	v.pass()
	i := slices.IndexFunc(v.f.State().PRs, func(p githubtest.PRState) bool { return p.Number == pr })
	if i < 0 || !v.f.State().PRs[i].Merged || pickStage(v.f, 1) != "stage:pr-open" {
		v.t.Fatalf("the pass after the PR step left the PRs %+v, the task at %s; want #%d merged, and the task at pr-open till the next pass",
			v.f.State().PRs, pickStage(v.f, 1), pr)
	}
	v.storyMergeCarried(pr, v.originTip(storyVerifyBranch))
}

// A task in a story at pr-open, whose PR step's pass is read on the next
// pass, is merged there: the PR is read again, one conditional request,
// and squash-merged at the verified head, with its title and number as the
// squash commit's title and its body as its message, onto the tip the head
// was verified against, the PR's one write. The task stays at pr-open on
// that pass. The pass after carries the tip's check over to the squash
// commit, with no command run, and closes the task (storyMergeCarried),
// and once the close is read, the passes after make no request but 304s,
// and never merge again.
func TestPRMergeMerges(t *testing.T) {
	t.Parallel()
	f, v, tip, head, pr := storyMergeReady(t)
	body := f.State().PRs[0].Body
	from, writes := len(f.Requests()), len(f.Writes())
	v.pass()
	if got, want := writesOf(f)[writes:], []string{"PUT " + storyMergePath(pr)}; !slices.Equal(got, want) {
		t.Fatalf("the merge's pass wrote %q; want %q", got, want)
	}
	var sent storyMergeBody
	if err := json.Unmarshal([]byte(f.Writes()[writes].Body), &sent); err != nil {
		t.Fatal(err)
	}
	want := storyMergeBody{SHA: head, MergeMethod: "squash", CommitTitle: storyPRTitle + " (#" + strconv.Itoa(pr) + ")", CommitMessage: body}
	if sent != want {
		t.Errorf("the merge sent %+v; want %+v", sent, want)
	}
	var reads []githubtest.Request
	for _, r := range f.Requests()[from:] {
		if storyPRMatch(pr)(r) {
			reads = append(reads, r)
		}
	}
	if len(reads) != 1 || reads[0].Status != http.StatusOK {
		t.Errorf("the PR's reads in the merge's pass = %+v; want one", reads)
	}
	squash := v.storyMerged(pr, tip)
	if pickStage(f, 1) != "stage:pr-open" {
		t.Errorf("the merge's pass left the task at %s; want pr-open, as the next pass reads the merge", pickStage(f, 1))
	}
	v.storyMergeCarried(pr, squash)
	from = len(f.Requests())
	v.quietPass("after the merge")
	v.quietPass("again")
	if reads, writes := prRequests(f, from); reads != 0 || writes != 0 {
		t.Errorf("the quiet passes read /pulls %d times, and wrote it %d times; want neither", reads, writes)
	}
	v.storyMerged(pr, tip)
}

// A push to the task's branch before the merge is a new head: the pass
// whose sync brings it sends the task back to verifying on it (SPEC §7.2),
// and merges nothing. One that lands between the poll's sync and the read
// just before the merge, so GitHub's PR has a head the bare clone hasn't,
// merges nothing either, and the next pass sends the task back to
// verifying the same way.
func TestPRMergeNewHead(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		during bool
	}{
		{"before the pass", false},
		{"during the pass", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, v, _, head, pr := storyMergeReady(t)
			writes := len(f.Writes())
			var person string
			if tt.during {
				person = storyMergePersonCommit(t, v, head)
				f.BeforeRequest(storyPRMatch(pr), func() {
					cmd := exec.Command("git", "push", "--quiet", v.o.url, person+":refs/heads/"+prBranch)
					cmd.Dir = v.o.work
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Errorf("push %s: %v: %s", person, err, out)
					}
				})
				v.pass()
				if pickStage(f, 1) != "stage:pr-open" || len(storyMerges(f, writes, pr)) != 0 {
					t.Fatalf("the pass whose read found a new head left the task at %s, with merges %+v; want pr-open, and none",
						pickStage(f, 1), storyMerges(f, writes, pr))
				}
			} else {
				person = prHeadPush(t, v.o, head, "person.txt")
			}
			v.pass()
			if moves := personComments(t, f, "from=pr-open to=verifying event=n"+person+" "); len(moves) != 1 || pickStage(f, 1) != "stage:verifying" {
				t.Errorf("the task at %s, its moves to verifying on %s %+v; want verifying, by one", pickStage(f, 1), person, moves)
			}
			if got := storyMerges(f, writes, pr); len(got) != 0 || f.State().PRs[0].State != "open" {
				t.Errorf("the PR is %s, its merges %+v; want it open, and none", f.State().PRs[0].State, got)
			}
		})
	}
}

// storyMergePersonCommit commits a person's change on head in v's origin's
// work tree, as prHeadPush does, but doesn't push it, and returns it.
func storyMergePersonCommit(t *testing.T, v *verdictRig, head string) string {
	t.Helper()
	w := v.o.work
	gitIn(t, w, "checkout", "--quiet", "-B", "person", head)
	if err := os.WriteFile(filepath.Join(w, "person.txt"), []byte("a person's change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, w, "add", "person.txt")
	gitIn(t, w, "commit", "--quiet", "-m", "a person's change")
	tip := gitIn(t, w, "rev-parse", "HEAD")
	gitIn(t, w, "checkout", "--quiet", "main")
	return tip
}

// A close and a reopen of the PR before the merge: one the poll lists is
// judged first, as any PR's is (SPEC §6.4), and one made after the poll,
// just before the read, makes that read show the PR changed, so it's
// judged again first (prJudgeOpen). The owner's close and reopen moves
// nothing, so the PR merges. The owner's close and mallory's reopen hands
// the task off, and nothing merges. So does one whose reopened event
// GitHub writes late, though the read just before the merge judges
// nothing, as its newest close or reopen is the close: nothing merges, and
// once the event is written, the next pass hands the task off. A close
// alone leaves the PR closed, so nothing merges, and the next pass hands
// the task off.
func TestPRMergeCloseReopen(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		during   bool
		reopener string
		late     bool
		merged   bool
		stage    string
	}{
		{"the owner's, before the pass", false, wholeRunOwner, false, true, "stage:pr-open"},
		{"the owner's, during the pass", true, wholeRunOwner, false, true, "stage:pr-open"},
		{"mallory's, before the pass", false, "mallory", false, false, "stage:needs-human"},
		{"mallory's, during the pass", true, "mallory", false, false, "stage:needs-human"},
		{"mallory's, its event late, during the pass", true, "mallory", true, false, "stage:pr-open"},
		{"a close alone, during the pass", true, "", false, false, "stage:pr-open"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, v, tip, _, pr := storyMergeReady(t)
			change := func() {
				if tt.late {
					f.LagEvents(30 * time.Second)
				}
				f.ClosePR(pr, wholeRunOwner)
				if tt.reopener != "" {
					if tt.late {
						f.Advance(time.Minute) // the closed event is written
					}
					f.ReopenPR(pr, tt.reopener)
				}
				f.LagEvents(0)
			}
			if tt.during {
				f.BeforeRequest(storyPRMatch(pr), change)
			} else {
				change()
			}
			from, writes := len(f.Requests()), len(f.Writes())
			v.pass()
			timelines := 0
			for _, r := range f.Requests()[from:] {
				if r.Path == "/issues/"+strconv.Itoa(pr)+"/timeline" {
					timelines++
				}
			}
			// The poll lists the PR as changed, by the PR step's writes, or
			// by the change, so step 2 reads its timeline first. A change
			// during the pass has it read again, unless the PR is closed by
			// then, as GitHub won't merge it.
			want := 1
			if tt.during && tt.reopener != "" {
				want = 2
			}
			if timelines != want {
				t.Errorf("the pass read PR #%d's timeline %d times; want %d", pr, timelines, want)
			}
			if got := storyMerges(f, writes, pr); len(got) != 0 && !tt.merged || len(got) != 1 && tt.merged {
				t.Fatalf("the pass merged %+v; want a merge %t", got, tt.merged)
			}
			if tt.merged {
				v.storyMerged(pr, tip)
				return
			}
			if pickStage(f, 1) != tt.stage {
				t.Errorf("the task is at %s; want %s", pickStage(f, 1), tt.stage)
			}
			if tt.late {
				f.Advance(time.Minute) // the reopened event is written
			}
			v.pass()
			if handoffs := personComments(t, f, "<!-- issuedog:transition from=pr-open to=needs-human "); len(handoffs) != 1 || pickStage(f, 1) != "stage:needs-human" {
				t.Errorf("the task at %s, its hand-offs %+v; want needs-human, by one", pickStage(f, 1), handoffs)
			}
			if got := storyMerges(f, 0, pr); len(got) != 0 || v.originTip(storyVerifyBranch) != tip {
				t.Errorf("the merges = %+v, the story branch at %s; want none, at %s", got, v.originTip(storyVerifyBranch), tip)
			}
		})
	}
}

// The owner's "request changes" review before the merge sends the task
// back to in-progress, whether the poll lists it or it lands just before
// the read, which judges the PR again: nothing merges (SPEC §6.4, §7.2).
func TestPRMergeReview(t *testing.T) {
	t.Parallel()
	for _, during := range []bool{false, true} {
		t.Run("during the pass "+strconv.FormatBool(during), func(t *testing.T) {
			t.Parallel()
			f, v, tip, _, pr := storyMergeReady(t)
			review := func() { f.ReviewPR(pr, wholeRunOwner, github.ReviewChangesRequested, "Rename it.", nil) }
			if during {
				f.BeforeRequest(storyPRMatch(pr), review)
			} else {
				review()
			}
			v.pass()
			if moves := personComments(t, f, "<!-- issuedog:transition from=pr-open to=in-progress "); len(moves) != 1 || pickStage(f, 1) != "stage:in-progress" {
				t.Errorf("the task at %s, its moves to in-progress %+v; want in-progress, by one", pickStage(f, 1), moves)
			}
			if got := storyMerges(f, 0, pr); len(got) != 0 || f.State().PRs[0].State != "open" || v.originTip(storyVerifyBranch) != tip {
				t.Errorf("the merges = %+v, the PR %s, the story branch at %s; want none, open, at %s",
					got, f.State().PRs[0].State, v.originTip(storyVerifyBranch), tip)
			}
		})
	}
}

// A story branch that moved since the head was verified against it, as
// by a push, isn't merged onto till the head's merge with it passes (SPEC
// §7.5's The merge's guards): the pass whose sync brings the new tip,
// unchecked yet, merges nothing, and checks it, with no request for the
// task's PR or its head, as the task waits for that check. Once it's
// success, the next pass checks the head's merge with the new tip, once,
// and posts issuedog/verify as success on the head, naming the tip (SPEC
// §10.4). The pass after merges the PR onto that tip, and the one after
// carries no check over to the squash commit: it closes the task, and step
// 4 checks the squash commit afresh, running the commands once. Once that's
// read, the passes are quiet.
func TestPRMergeTipMoved(t *testing.T) {
	t.Parallel()
	f, v, _, head, pr := storyMergeReady(t)
	moved := v.tipPush(storyVerifyBranch, map[string]string{"more.txt": "more of the story\n"})
	from, ran, writes := len(f.Requests()), v.checksRan(), len(f.Writes())
	v.pass()
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(moved, storyVerifyBranch, "success")) || v.checksRan() != ran+1 {
		t.Fatalf("the pass on the moved tip left the checks %+v, and ran the commands %d times; want its success, once", got, v.checksRan()-ran)
	}
	if reads, writes := prRequests(f, from); reads != 0 || writes != 0 || prMainReads(f, from, head) != 0 {
		t.Errorf("the pass waiting on the moved tip's check read /pulls %d times, wrote it %d times, and read the head's checks %d times; want none",
			reads, writes, prMainReads(f, from, head))
	}
	if got := storyMerges(f, writes, pr); len(got) != 0 || f.State().PRs[0].State != "open" || pickStage(f, 1) != "stage:pr-open" ||
		v.originTip(storyVerifyBranch) != moved {
		t.Errorf("the merges = %+v, the PR %s, the task at %s, the story branch at %s; want none, open, pr-open, and %s",
			got, f.State().PRs[0].State, pickStage(f, 1), v.originTip(storyVerifyBranch), moved)
	}
	v.storyTipPassed(1, pr, head, moved)
	squash := v.storyTipMergedOnto(pr, head, moved)
	v.storyTipMergedChecked(1, pr, squash, "success")
	v.passes(2)
	v.quietPass("after the merge")
	v.quietPass("again")
}

// GitHub's refusal of the merge, a 409 or a 405, closes nothing and writes
// nothing else, and the task stays at pr-open. The passes after make no
// request for it, till the PR changes, as by the owner's approving review,
// which moves nothing: then the merge is tried again, its read of the PR a
// conditional one. A second refusal of the head isn't warned of again, and
// once GitHub takes it, the PR merges. It reads logr, so it isn't
// parallel.
func TestPRMergeRefused(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusMethodNotAllowed} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			f, v, tip, head, pr := storyMergeReady(t)
			match := func(r githubtest.Request) bool { return r.Method == http.MethodPut && r.Path == storyMergePath(pr) }
			from, writes := len(f.Requests()), len(f.Writes())
			stop := logtest.Capture(t, logr.W)
			for i := range 2 {
				if i > 0 {
					f.ReviewPR(pr, wholeRunOwner, github.ReviewApproved, "", nil)
				}
				f.AnswerOnce(match, status)
				v.pass()
				v.quietPass("after a refusal")
			}
			logs := stop()
			want := "o/r#1: GitHub refused to merge its PR #" + strconv.Itoa(pr) + " at " + head + " into " + storyVerifyBranch
			if got := strings.Count(logs, want); got != 1 {
				t.Errorf("the refusal was warned of %d times; want once, in\n%s", got, logs)
			}
			if got := writesOf(f)[writes:]; len(got) != 0 {
				t.Errorf("the refused passes wrote %q; want nothing", got)
			}
			var tries, reads []githubtest.Request
			for _, r := range f.Requests()[from:] {
				switch {
				case match(r):
					tries = append(tries, r)
				case storyPRMatch(pr)(r):
					reads = append(reads, r)
				}
			}
			if len(tries) != 2 || tries[0].Status != status || tries[1].Status != status {
				t.Errorf("the merges tried = %+v; want two, each answered %d", tries, status)
			}
			if len(reads) != 2 || !reads[1].Conditional {
				t.Errorf("the PR's reads = %+v; want two, the second a conditional one", reads)
			}
			if f.State().PRs[0].State != "open" || pickStage(f, 1) != "stage:pr-open" || v.originTip(storyVerifyBranch) != tip {
				t.Fatalf("after the refusals, the PR is %s, the task at %s, the story branch at %s; want open, pr-open, and %s",
					f.State().PRs[0].State, pickStage(f, 1), v.originTip(storyVerifyBranch), tip)
			}
			f.ReviewPR(pr, wholeRunOwner, github.ReviewApproved, "", nil)
			v.pass()
			if got := storyMerges(f, writes, pr); len(got) != 1 {
				t.Errorf("the merges = %+v; want one", got)
			}
			if i := slices.IndexFunc(f.State().PRs, func(p githubtest.PRState) bool { return p.Number == pr }); i < 0 || !f.State().PRs[i].Merged {
				t.Errorf("the PRs = %+v; want #%d merged", f.State().PRs, pr)
			}
		})
	}
}

// A crash after the merge, whose answer is lost, leaves the PR merged:
// the pass fails, and a fresh daemon finds the PR merged by the poll, so
// it never merges it again (SPEC §5.2). It holds no tip's judging after
// the restart, so it carries no check over to the squash commit: it
// closes the task, and step 4 checks the new tip as any push (SPEC §7.5's
// Carrying the check over). Its passes are quiet once that's read.
func TestPRMergeCrash(t *testing.T) {
	t.Parallel()
	f, v, tip, _, pr := storyMergeReady(t)
	writes := len(f.Writes())
	f.FailWriteAfter(writes + 1)
	if err := v.d.Iterate(context.Background()); err == nil {
		t.Fatal("the pass whose merge's answer was lost gave no error")
	}
	f.FailWriteAfter(0)
	if w := f.Writes()[writes]; w.Method != http.MethodPut || w.Path != storyMergePath(pr) || !w.Failed {
		t.Fatalf("the failed write was %+v; want the merge", w)
	}
	v = v.restart()
	ran := v.checksRan()
	v.passes(3)
	squash := v.storyMerged(pr, tip)
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(squash, storyVerifyBranch, "success")) || v.checksRan() != ran+1 {
		t.Errorf("after the restart, the checks are %+v, and the commands ran %d times; want the new tip's success, checked once", got, v.checksRan()-ran)
	}
	if pickStage(f, 1) != "stage:completed" || !f.State().Issues[0].Closed {
		t.Errorf("the task is at %s, closed %t; want completed, and closed", pickStage(f, 1), f.State().Issues[0].Closed)
	}
	v.quietPass("after the restart")
}

// A fresh daemon after the PR step's pass doesn't merge on its first pass,
// whose step 2 comes before step 4 reads the story branch's tip's check:
// a tip not judged yet isn't success (SPEC §7.5). The pass after merges,
// once.
func TestPRMergeRestart(t *testing.T) {
	t.Parallel()
	f, v, tip, _, pr := storyMergeReady(t)
	v = v.restart()
	writes := len(f.Writes())
	v.pass()
	if got := storyMerges(f, writes, pr); len(got) != 0 || f.State().PRs[0].State != "open" {
		t.Fatalf("the restart's first pass merged %+v, leaving the PR %s; want none, and open", got, f.State().PRs[0].State)
	}
	v.pass()
	v.storyMerged(pr, tip)
}

// A second branch with the task's prefix, as an admin's, leaves the daemon
// unable to tell which is the task's, so the PR isn't merged (SPEC §7.5's
// Two branches with one prefix), and the task gets one branches state
// error (SPEC §5.3). Once the passes after have read that back, listed it
// and assigned it, they're quiet.
func TestPRMergeTwoBranches(t *testing.T) {
	t.Parallel()
	f, v, _, head, pr := storyMergeReady(t)
	gitIn(t, v.o.url, "update-ref", "refs/heads/issuedog/1-a-copy", head)
	v.passes(3)
	v.quietPass("two branches with the task's prefix")
	if got := branchesErrs(f, 1); len(got) != 1 {
		t.Errorf("the task's branches errors are %q; want one", got)
	}
	if got := storyMerges(f, 0, pr); len(got) != 0 || f.State().PRs[0].State != "open" || pickStage(f, 1) != "stage:pr-open" {
		t.Errorf("the merges = %+v, the PR %s, the task at %s; want none, open, and pr-open", got, f.State().PRs[0].State, pickStage(f, 1))
	}
}

// A person's head that failed verify is back at pr-open with no tip=, so
// it's never merged, though its branch is at its head= and its story
// branch's tip is the one its PR's earlier head was verified against, and
// the passes that wait make no request (SPEC §7.2, §7.5).
func TestPRMergePersonFailedHead(t *testing.T) {
	t.Parallel()
	f, v, tip, head, pr := storyMergeReady(t)
	person, _ := v.personPush(head, "broken.txt")
	v.pass()
	if _, fields := personBack(t, f, person); fields["head"] != person || fields["tip"] != "" {
		t.Fatalf("the move back has %v; want head=%s, and no tip", fields, person)
	}
	if v.originTip(prBranch) != person || v.originTip(storyVerifyBranch) != tip {
		t.Fatalf("the task's branch is at %s, the story branch at %s; want %s, and %s", v.originTip(prBranch), v.originTip(storyVerifyBranch), person, tip)
	}
	v.pass()
	v.quietPass("a failed person's head at pr-open")
	v.quietPass("again")
	if got := storyMerges(f, 0, pr); len(got) != 0 || f.State().PRs[0].State != "open" || pickStage(f, 1) != "stage:pr-open" {
		t.Errorf("the merges = %+v, the PR %s, the task at %s; want none, open, and pr-open", got, f.State().PRs[0].State, pickStage(f, 1))
	}
}

// A person's head that passed verify, back at pr-open with tip=, whose
// App issuedog/verify is a failure since, isn't merged: the merge's pass
// reads the head's checks once, and the passes after make no request.
func TestPRMergePersonCheckFailed(t *testing.T) {
	t.Parallel()
	f, v, tip, head, pr := storyMergeReady(t)
	person, _ := v.personPush(head, "person.txt")
	v.pass()
	v.out(1, verifierPass(t, "Fine."))
	v.pass()
	v.pass()
	if _, fields := personBack(t, f, person); fields["tip"] != tip {
		t.Fatalf("the move back has %v; want tip=%s", fields, tip)
	}
	if _, err := f.Client(t).PostVerifyCheck(context.Background(), "o", "r", github.NewCheckRun{HeadSHA: person, Conclusion: "failure",
		Title: "t", Summary: "s"}); err != nil {
		t.Fatal(err)
	}
	from := len(f.Requests())
	v.pass()
	reads := 0
	for _, r := range f.Requests()[from:] {
		if r.Path == "/commits/"+person+"/check-runs" {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("the pass read the head's checks %d times; want once", reads)
	}
	from = len(f.Requests())
	v.quietPass("a person's head with a failure at pr-open")
	v.quietPass("again")
	for _, r := range f.Requests()[from:] {
		if strings.HasPrefix(r.Path, "/commits/") || strings.HasPrefix(r.Path, "/pulls") {
			t.Errorf("a waiting pass sent %+v; want nothing for the head's checks or the PR", r)
		}
	}
	if got := storyMerges(f, 0, pr); len(got) != 0 || f.State().PRs[0].State != "open" || pickStage(f, 1) != "stage:pr-open" {
		t.Errorf("the merges = %+v, the PR %s, the task at %s; want none, open, and pr-open", got, f.State().PRs[0].State, pickStage(f, 1))
	}
}

// A review's move of a task in a story to in-progress waits at pr-open
// while its story branch's tip has no success, as its run couldn't start
// there (SPEC §3.4, §7.2), with no write, and no request on the passes that
// wait. A push to the task's branch meanwhile is no new head's move: the
// waiting review's move carries it. Once a new tip passes its check, and
// the bug the broken tip filed is closed, as the owner closes it with the
// fix, so no failure on the branch is open (story_failure_test.go), the
// next pass makes the move, with head= the pushed head. The rig picks
// nothing (pickOff), so that bug stays at ready till then.
func TestPRMergeReviewWaitsForTip(t *testing.T) {
	t.Parallel()
	f, v, _, head, pr := storyMergeReady(t)
	v.pickOff()
	broken := v.tipPush(storyVerifyBranch, map[string]string{"broken.txt": "the story broke it\n"})
	v.pass()
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(broken, storyVerifyBranch, "failure")) {
		t.Fatalf("the pass on the broken tip left the checks %+v; want its failure", got)
	}
	bug := len(f.State().Issues)
	if is := f.State().Issues[bug-1]; !slices.Contains(is.Labels, "type:bug") || is.Parent != 2 {
		t.Fatalf("#%d = %+v; want the bug the broken tip filed under the story", bug, is)
	}
	f.ReviewPR(pr, wholeRunOwner, github.ReviewChangesRequested, "Rename it.", nil)
	writes := len(f.Writes())
	v.pass()
	if pickStage(f, 1) != "stage:pr-open" || len(f.Writes()) != writes {
		t.Fatalf("the review's pass left the task at %s, and wrote %+v; want pr-open, and nothing", pickStage(f, 1), f.Writes()[writes:])
	}
	v.quietPass("a review waiting for the story tip")
	v.quietPass("again")
	person := prHeadPush(t, v.o, head, "person.txt")
	v.pass()
	if moves := personComments(t, f, "from=pr-open to=verifying "); len(moves) != 0 || pickStage(f, 1) != "stage:pr-open" {
		t.Fatalf("the push's pass left the task at %s, with moves to verifying %+v; want pr-open, and none", pickStage(f, 1), moves)
	}
	fixed := v.tipPush(storyVerifyBranch, map[string]string{"broken.txt": ""})
	f.CloseBy(bug, "owner")
	v.pass()
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(fixed, storyVerifyBranch, "success")) || pickStage(f, 1) != "stage:pr-open" {
		t.Fatalf("the pass on the fixed tip left the checks %+v, the task at %s; want its success, and pr-open", got, pickStage(f, 1))
	}
	v.pass()
	moves := personComments(t, f, "<!-- issuedog:transition from=pr-open to=in-progress ")
	if len(moves) != 1 || pickStage(f, 1) != "stage:in-progress" {
		t.Fatalf("the task at %s, its moves to in-progress %+v; want in-progress, by one", pickStage(f, 1), moves)
	}
	if got := prMoveFields(t, moves[0].Body)["head"]; got != person {
		t.Errorf("the review's move has head=%q; want %s, the pushed head", got, person)
	}
	if got := storyMerges(f, 0, pr); len(got) != 0 {
		t.Errorf("the merges = %+v; want none", got)
	}
}

// The App's merge, a crash at each of its writes, then a restart: the
// replay ends with the PR merged once, its squash commit on the story
// branch, that new tip checked, and the task closed (SPEC §5.2). Each pass
// is a fresh daemon on the fake's own bare clone, so its first iteration
// reads the tip's check, its second merges, its third carries the check
// over to the new tip and closes the task, or after a crash, checks the
// new tip as any push, and the two after read the close. The origin is
// shared, so each setup puts the task's branch and the story branch back.
func TestPRMergeReplay(t *testing.T) {
	t.Parallel()
	_, base := newStoryVerifyRig(t)
	_, tip := base.storyCut()
	head := base.storyHead(tip, map[string]string{"quote.txt": "expired\n"})
	gitIn(t, base.o.work, "push", "--quiet", base.o.url, head+":refs/heads/"+prBranch)
	roots, clones := map[*githubtest.Fake]string{}, map[*githubtest.Fake]*repo.Repo{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		gitIn(tb, base.o.url, "update-ref", "refs/heads/"+prBranch, head)
		gitIn(tb, base.o.url, "update-ref", "refs/heads/"+storyVerifyBranch, tip)
		f := newFake(tb)
		f.SetPermission(wholeRunOwner, "admin")
		f.SetGitRemote(base.o.url)
		task := f.AddIssue("owner", "Refuse an expired quote", "Booking an expired quote returns 410.", "type:task", "priority:p1", "scope:booking")
		if story := tipStoryAt(f, tipStoryTitle, "planned", "type:story"); task != 1 || story != 2 {
			tb.Fatalf("the task is #%d, the story #%d; want #1 and #2", task, story)
		}
		f.SetParent(1, 2)
		c := f.Client(tb)
		pr, err := c.CreatePullRequest(context.Background(), "o", "r", github.NewPullRequest{Title: storyPRTitle, Head: prBranch, Base: storyVerifyBranch,
			Body: "Task #1, in story #2\n\nThe change."})
		if err != nil {
			tb.Fatal(err)
		}
		f.AddLabel(pr.Number, app, "priority:p1")
		f.AddLabel(1, app, "stage:pr-open")
		f.AddComment(1, app, "The implementer committed.\n\n<!-- issuedog:transition from=in-progress to=verifying attempt=1 run=r1 head="+head+" -->")
		f.AddComment(1, app, "PR open.\n\n<!-- issuedog:transition from=verifying to=pr-open run=rv head="+head+" pr="+strconv.Itoa(pr.Number)+" tip="+tip+" -->")
		for _, run := range []struct{ name, sha, id string }{{github.TipCheck, tip, "branch=" + storyVerifyBranch}, {github.VerifyCheck, head, "main=" + base.mainSHA()}} {
			if _, err := c.PostCheck(context.Background(), "o", "r", run.name, github.NewCheckRun{HeadSHA: run.sha, Conclusion: "success",
				Title: "t", Summary: "s", ExternalID: run.id}); err != nil {
				tb.Fatal(err)
			}
		}
		f.ForgetWrites()
		roots[f] = t.TempDir()
		return f
	}, func(f *githubtest.Fake) error {
		g := newRig(t, f, base.o, roots[f])
		if clones[f] == nil {
			clones[f] = g.d.Clone
		}
		g.d.Clone, g.r.Workspace.Bare = clones[f], clones[f].Dir()
		v := &verdictRig{rig: g, o: base.o, cfg: base.cfg, ran: base.ran}
		v.wire()
		v.storyFetch(head)
		for range 5 {
			if err := v.d.Iterate(context.Background()); err != nil {
				return err
			}
		}
		return nil
	})
	for f := range roots {
		prs := f.State().PRs
		if len(prs) != 1 || !prs[0].Merged || prs[0].MergedBy != app || !f.State().Issues[0].Closed {
			t.Fatalf("after the replay, the PRs = %+v, the task closed %t; want one, merged by %s, and the task closed", prs, f.State().Issues[0].Closed, app)
		}
	}
}
