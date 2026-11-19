package daemon_test

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/repo"
)

// The PR step's last writes (SPEC §7.2, §10.2, §10.4): the issuedog/verify
// check on the verified head, then the move verifying → pr-open, or for a
// head that changes nothing on main, the hand-off at needs-human, on a
// verdictRig wired as main's.

// prMoveCheck is the issuedog/verify check the App posted, as the POST
// sent it.
type prMoveCheck struct {
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	DetailsURL string `json:"details_url"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Output     struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	} `json:"output"`
}

// prMoveChecks are the check runs the App posted on f, as the POSTs sent
// them, in order.
func prMoveChecks(t *testing.T, f *githubtest.Fake) []prMoveCheck {
	t.Helper()
	var checks []prMoveCheck
	for _, w := range f.Writes() {
		if w.Method != http.MethodPost || w.Path != "/check-runs" || w.Failed {
			continue
		}
		var c prMoveCheck
		if err := json.Unmarshal([]byte(w.Body), &c); err != nil {
			t.Fatal(err)
		}
		checks = append(checks, c)
	}
	return checks
}

// prMoveComments are the App's comments on task 1 that hold a transition
// marker out of verifying to to.
func prMoveComments(f *githubtest.Fake, to string) []string {
	var bodies []string
	for _, c := range f.State().Issues[0].Comments {
		if c.Author == app && strings.Contains(c.Body, "<!-- issuedog:transition from=verifying to="+to+" ") {
			bodies = append(bodies, c.Body)
		}
	}
	return bodies
}

// prMoveFields are the fields of the transition marker in body, a comment
// prMoveComments gives.
func prMoveFields(t *testing.T, body string) map[string]string {
	t.Helper()
	for _, m := range github.ParseMarkers(body) {
		if m.Kind == "transition" {
			return m.Fields
		}
	}
	t.Fatalf("no transition marker in\n%s", body)
	return nil
}

// prMoveText is the text of the move to pr-open for PR pr at head.
func prMoveText(pr int, head string) string {
	return "PR #" + strconv.Itoa(pr) + " to `main` is open for this task, verified at `" + head +
		"`. It waits for your review and merge, and merging it closes this task.\n\n"
}

// prMoveCheckRuns are the fake's check runs, in State's form.
func prMoveCheckRuns(f *githubtest.Fake) []githubtest.CheckRunState { return f.State().CheckRuns }

// prMoveVerified is the one success of the App's on head, as State has it.
func prMoveVerified(head string) githubtest.CheckRunState {
	return githubtest.CheckRunState{SHA: head, Name: github.VerifyCheck, Conclusion: "success", AppID: githubtest.AppID}
}

// prMoveQuiet runs quietPass, and fails if the pass sent anything about a
// PR or a commit's check runs, or with task, anything about task 1: a task
// at pr-open costs no request of its own (SPEC §8.1). One waiting on its
// hand-off is visited on every pass, at conditional GETs, which quietPass
// allows.
func (v *verdictRig) prMoveQuiet(step string, task bool) {
	v.t.Helper()
	from := len(v.f.Requests())
	v.quietPass(step)
	for _, r := range v.f.Requests()[from:] {
		if task && (r.Path == "/issues/1" || strings.HasPrefix(r.Path, "/issues/1/")) || strings.HasPrefix(r.Path, "/pulls") || strings.HasPrefix(r.Path, "/commits/") {
			v.t.Errorf("%s: the unchanged pass sent %+v; want nothing for the task, its PR or its head", step, r)
		}
	}
}

// A whole run, with a second task waiting at ready: the implementer
// commits, the next pass runs the checks, the one after the verifier, which
// passes, and the one after that pushes the head and opens the PR, then
// posts issuedog/verify, success, on the verified head, with its title, a
// summary naming the task, the head and main's SHA, with the commands'
// results from the checks' pass, which run no more, and a link to the pass
// comment, and last moves the task verifying → pr-open, keyed by the
// pass's run, with head=, pr= and seen=, in a comment naming the PR and
// the head. That pass picks nothing, as the move was its step 4, and the
// next picks the second task, as the task at pr-open holds no worker, and
// reads the move, so the digest lists the PR, and notify is assigned to
// the PR. Once that's read, each unchanged pass makes no request for the
// task, its PR or its head.
func TestPRMoveWholeRun(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	v.out(2, verifierPass(t, "Fine."))
	v.out(3, ivResult(t, implementerOutput(t, "question")))
	n := f.AddIssue(wholeRunOwner, "Refuse an expired quote", "Booking an expired quote returns 410.", "type:task", "priority:p1", "scope:booking")
	f.AddLabel(n, wholeRunOwner, "stage:in-progress")
	next := fileAt(f, "ready", "The next task.", "priority:p1", "scope:booking", "type:task")
	main := v.mainSHA()

	v.passes(3)
	head := v.trimmed("commit.1")
	run := v.leaseOf("2").RunID
	pass := filingPassID(t, f, n, run)
	if len(passComments(f, n)) != 1 || len(prMoveCheckRuns(f)) != 0 || pickStage(f, n) != "stage:verifying" {
		t.Fatalf("after the verifier's pass: verdicts %q, checks %+v, at %s; want one, none, still at verifying",
			passComments(f, n), prMoveCheckRuns(f), pickStage(f, n))
	}

	v.pass()
	prs := f.State().PRs
	if len(prs) != 1 || prs[0].HeadSHA != head {
		t.Fatalf("the PRs = %+v; want one, at %s", prs, head)
	}
	pr := prs[0].Number
	link := "https://github.com/o/r/issues/1#issuecomment-" + strconv.FormatInt(pass, 10)
	if got := prMoveCheckRuns(f); !slices.Equal(got, []githubtest.CheckRunState{prMoveVerified(head)}) {
		t.Errorf("the check runs = %+v; want the App's success on the head %s", got, head)
	}
	checks := prMoveChecks(t, f)
	summary := "issuedog verified `" + head + "` for #1: the checks against `main` at `" + main + "` passed, and so did the verifier, in [its comment](" +
		link + ").\n\n" + strings.TrimPrefix(v.prResults(), "## Results\n\n")
	if len(checks) != 1 || checks[0].Name != github.VerifyCheck || checks[0].HeadSHA != head || checks[0].Status != "completed" ||
		checks[0].Conclusion != "success" || checks[0].DetailsURL != link || checks[0].Output.Title != "issuedog verified this head" ||
		checks[0].Output.Summary != summary {
		t.Errorf("the check posted = %+v; want issuedog/verify, completed, success, on %s, titled \"issuedog verified this head\", linking %s, its summary\n%s",
			checks, head, link, summary)
	}
	if v.checksRan() != 1 {
		t.Errorf("the commands ran %d times, want once: the check's results are the checks' pass's", v.checksRan())
	}
	moves := prMoveComments(f, "pr-open")
	if len(moves) != 1 || !strings.HasPrefix(moves[0], prMoveText(pr, head)) || strings.Contains(moves[0], "cc @") {
		t.Fatalf("the moves to pr-open = %q; want one, starting\n%s\nwith no cc line", moves, prMoveText(pr, head))
	}
	want := map[string]string{"from": "verifying", "to": "pr-open", "run": run, "head": head, "pr": strconv.Itoa(pr), "seen": strconv.FormatInt(pass, 10)}
	if got := prMoveFields(t, moves[0]); !maps.Equal(got, want) {
		t.Errorf("the move's marker has %v; want %v", got, want)
	}
	if pickStage(f, n) != "stage:pr-open" || pickStage(f, next) != "stage:ready" || v.runs() != 2 {
		t.Errorf("the task at %s, the next at %s, after %d runs; want pr-open, and the next still at ready, as the move was the pass's step 4",
			pickStage(f, n), pickStage(f, next), v.runs())
	}

	v.pass()
	if pickStage(f, next) != "stage:clarifying" || v.runs() != 3 {
		t.Errorf("the next task is at %s, after %d runs; want it picked on this pass, its run's question at clarifying", pickStage(f, next), v.runs())
	}
	// That pass reads the move too: the digest lists the PR, and notify is
	// assigned to the PR, not the task (SPEC §9.4, §9.6).
	if digest := prMoveDigest(f); !strings.Contains(digest, "**Pull request**, priority p1") ||
		!strings.Contains(digest, "- Answer here: https://github.com/o/r/pull/"+strconv.Itoa(pr)) {
		t.Errorf("the digest =\n%s\nwant it to list PR #%d", digest, pr)
	}
	if got, task := assignees(f, pr), assignees(f, n); !slices.Equal(got, []string{wholeRunOwner}) || len(task) != 0 {
		t.Errorf("the PR's assignees = %q, the task's %q; want %s on the PR, and nobody on the task", got, task, wholeRunOwner)
	}
	// The next pass reads the question, lists it on the digest and assigns
	// notify to it, and the one after reads those writes.
	v.passes(2)
	for _, step := range []string{"first", "second"} {
		v.prMoveQuiet(step, true)
	}
	if len(f.State().PRs) != 1 || len(prMoveCheckRuns(f)) != 1 || len(prMoveComments(f, "pr-open")) != 1 {
		t.Errorf("the PRs = %+v, the checks %+v, the moves %q; want one each", f.State().PRs, prMoveCheckRuns(f), prMoveComments(f, "pr-open"))
	}
}

// A crash just after the check, and just after the move's comment, each a
// write GitHub made whose answer was lost, then a restart: the replay
// posts no second check or comment. After the check, the restart runs the
// checks again, then the PR step finds the PR as it stands, and the App's
// success on the head, and writes only the move. After the comment, the
// restart's step 1 finishes the move's labels, and nothing at verifying
// is left, so the checks don't run again.
func TestPRMoveCrash(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		path   string // the write whose answer is lost
		checks int    // how many times the commands run in all
	}{
		{"after the check", "/check-runs", 2},
		{"after the comment", "/issues/1/comments", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			v := newVerdictRig(t, f, 3)
			head, _ := v.prTask()
			v.pass()
			f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == tt.path },
				func() { f.FailWriteAfter(len(f.Writes()) + 1) })
			if err := v.d.Iterate(context.Background()); err == nil {
				t.Fatal("the pass whose answer was lost gave no error")
			}
			if got := prMoveCheckRuns(f); !slices.Equal(got, []githubtest.CheckRunState{prMoveVerified(head)}) {
				t.Fatalf("after the lost answer, the check runs = %+v; want the App's success on %s", got, head)
			}

			v = v.restart()
			// The checks, the move, then the pass that lists the PR on
			// the digest and assigns notify to it, and the one that reads
			// those writes.
			v.passes(4)
			if got := prMoveCheckRuns(f); !slices.Equal(got, []githubtest.CheckRunState{prMoveVerified(head)}) {
				t.Errorf("after the restart, the check runs = %+v; want the one", got)
			}
			if moves := prMoveComments(f, "pr-open"); len(moves) != 1 || pickStage(f, 1) != "stage:pr-open" {
				t.Errorf("after the restart, the moves = %q, at %s; want one, at pr-open", moves, pickStage(f, 1))
			}
			if prs := f.State().PRs; len(prs) != 1 || prs[0].HeadSHA != head {
				t.Errorf("after the restart, the PRs = %+v; want the one, at %s", prs, head)
			}
			if v.checksRan() != tt.checks {
				t.Errorf("the commands ran %d times, want %d", v.checksRan(), tt.checks)
			}
			v.prMoveQuiet("after the restart", true)
		})
	}
}

// The App's newest issuedog/verify on the head decides the check (SPEC
// §10.4): a success of its own stands, so none is posted, but another
// App's success counts for nothing, nor does the App's success with a
// failure of its own since, so one is posted. The move comes either way.
func TestPRMoveCheckStands(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		before []githubtest.CheckRunState // the check runs on the head before the PR step, oldest first
		posted bool
	}{
		{"the App's success", []githubtest.CheckRunState{{Conclusion: "success", AppID: githubtest.AppID}}, false},
		{"another App's success", []githubtest.CheckRunState{{Conclusion: "success", AppID: 99}}, true},
		{"the App's failure after its success", []githubtest.CheckRunState{
			{Conclusion: "success", AppID: githubtest.AppID}, {Conclusion: "failure", AppID: githubtest.AppID}}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			v := newVerdictRig(t, f, 3)
			head, _ := v.prTask()
			var want []githubtest.CheckRunState
			for _, c := range tt.before {
				f.SetCheckRun(head, github.VerifyCheck, c.Conclusion, c.AppID)
				want = append(want, githubtest.CheckRunState{SHA: head, Name: github.VerifyCheck, Conclusion: c.Conclusion, AppID: c.AppID})
			}
			if tt.posted {
				want = append(want, prMoveVerified(head))
			}
			v.pass() // the checks
			v.prOpens(head)
			if got := prMoveCheckRuns(f); !slices.Equal(got, want) {
				t.Errorf("the check runs = %+v; want %+v", got, want)
			}
			if moves := prMoveComments(f, "pr-open"); len(moves) != 1 {
				t.Errorf("the moves = %q; want one", moves)
			}
			v.prMoveQuiet(tt.name, true)
		})
	}
}

// Someone pushes to the task's branch between the App's push and the PR's
// open, so the PR shows their head. The check goes on the verified head
// alone, never theirs, so the main ruleset blocks the PR's merge, and the
// move names the verified head (SPEC §10.4).
func TestPRMovePushBetween(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head, _ := v.prTask()
	gitIn(t, v.o.work, "checkout", "--quiet", "-b", "theirs", "task")
	if err := os.WriteFile(filepath.Join(v.o.work, "theirs.txt"), []byte("Theirs.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, v.o.work, "add", "theirs.txt")
	gitIn(t, v.o.work, "commit", "--quiet", "-m", "theirs")
	theirs := gitIn(t, v.o.work, "rev-parse", "HEAD")
	gitIn(t, v.o.work, "checkout", "--quiet", "main")
	v.pass()
	f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/pulls" },
		func() { gitIn(t, v.o.work, "push", "--quiet", v.o.url, "theirs:refs/heads/"+prBranch) })
	v.pass()
	prs := f.State().PRs
	if len(prs) != 1 || prs[0].HeadSHA != theirs {
		t.Fatalf("the PRs = %+v; want one, showing their head %s", prs, theirs)
	}
	if got := prMoveCheckRuns(f); !slices.Equal(got, []githubtest.CheckRunState{prMoveVerified(head)}) {
		t.Errorf("the check runs = %+v; want the App's success on the verified head %s alone, none on %s", got, head, theirs)
	}
	moves := prMoveComments(f, "pr-open")
	if len(moves) != 1 || prMoveFields(t, moves[0])["head"] != head || !strings.HasPrefix(moves[0], prMoveText(prs[0].Number, head)) {
		t.Errorf("the moves = %q; want one naming the verified head %s", moves, head)
	}
}

// prMoveEmptyTask files task 1 at verifying on a head whose own diff from
// main is empty, with a clean pass of the verifier's on it, run rv, and
// returns the head and the pass comment's ID.
func (v *verdictRig) prMoveEmptyTask() (string, int64) {
	v.t.Helper()
	head := v.otherHead("empty", nil)
	gitIn(v.t, v.d.Clone.Dir(), "fetch", "--quiet", "--no-tags", v.o.work, "refs/heads/empty:"+repo.TasksRef+"1")
	n, _ := verifierTask(v.f, head)
	return head, v.f.AddComment(n, app, prPassComment(v.t, "rv", head, "Nothing needed to change."))
}

// A head whose own diff is empty gets no push, no PR and no check: the
// task moves verifying → needs-human on nothing-to-merge, keyed by the
// pass's run, with head= and seen=, in the SPEC §9.3 hand-off, ending in
// the cc line, which the digest lists. A trusted /done, or a trusted 👍
// on the hand-off, sends it back to ready, a new round (SPEC §9.3).
func TestPRMoveNothingToMerge(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		end  func(f *githubtest.Fake, handoff int64) string // ends the hand-off, and returns the key of the move it makes
	}{
		{"/done", func(f *githubtest.Fake, _ int64) string {
			return "cmd=" + strconv.FormatInt(f.AddComment(1, "owner", "/done"), 10)
		}},
		{"👍", func(f *githubtest.Fake, handoff int64) string {
			return "cmd=r" + strconv.FormatInt(f.AddReaction(handoff, "owner", "+1"), 10)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			v := newVerdictRig(t, f, 3)
			v.out(1, ivResult(t, implementerOutput(t, "question")))
			head, pass := v.prMoveEmptyTask()
			from := len(f.Requests())
			v.passes(2)
			reads, _ := prRequests(f, from)
			if v.originTip(prBranch) != "" || len(f.State().PRs) != 0 || len(prMoveCheckRuns(f)) != 0 || reads != 0 {
				t.Errorf("the branch at %q, PRs %+v, checks %+v, %d reads of /pulls; want no push, no PR, no check and no read",
					v.originTip(prBranch), f.State().PRs, prMoveCheckRuns(f), reads)
			}
			moves := prMoveComments(f, "needs-human")
			if len(moves) != 1 || pickStage(f, 1) != "stage:needs-human" {
				t.Fatalf("the hand-offs = %q, at %s; want one, at needs-human", moves, pickStage(f, 1))
			}
			text := "The verifier passed `" + head + "`, but it changes nothing on `main`: its own diff from its merge base with `main` is empty, so it gets no PR.\n\n"
			if !strings.HasPrefix(moves[0], text) || !strings.Contains(moves[0], "\n\ncc @"+wholeRunOwner+"\n\n<!-- issuedog:transition ") {
				t.Errorf("the hand-off =\n%s\nwant it to start\n%s\nand end in the cc line", moves[0], text)
			}
			want := map[string]string{"from": "verifying", "to": "needs-human", "run": "rv", "head": head, "seen": strconv.FormatInt(pass, 10)}
			if got := prMoveFields(t, moves[0]); !maps.Equal(got, want) {
				t.Errorf("the hand-off's marker has %v; want %v", got, want)
			}
			s, err := f.Client(t).Snapshot(context.Background(), "o", "r", 1)
			if err != nil {
				t.Fatal(err)
			}
			var handoff int64
			for _, c := range s.Comments {
				if c.Body == moves[0] {
					handoff = c.ID
				}
			}
			v.passes(2)
			if !slices.ContainsFunc(f.State().Issues, func(is githubtest.IssueState) bool {
				return strings.Contains(is.Body, "A hand-off waits for you: the verifier passed a head that changes nothing on `main`, so it gets no PR.") &&
					strings.Contains(is.Body, "- If something needs to change, reply `/done` on the issue or react 👍 to the hand-off, and the task goes back to `ready`. If not, close the issue.\n")
			}) {
				t.Errorf("no digest lists the hand-off for a head that changes nothing, ending with /done or a 👍 for a change, and a close for none")
			}
			if !strings.Contains(moves[0], "\n\nIf something needs to change, reply `/done` on this issue or react 👍 to this comment, and the task goes back to `ready`. If not, close this issue.\n\ncc @") {
				t.Errorf("the hand-off =\n%s\nwant it to end with /done or a 👍 for a change, and a close for none, before the cc line", moves[0])
			}
			v.prMoveQuiet("at the hand-off", false)

			key := tt.end(f, handoff)
			v.pass()
			back := "<!-- issuedog:transition from=needs-human to=ready " + key + " "
			if !slices.ContainsFunc(f.State().Issues[0].Comments, func(c githubtest.CommentState) bool {
				return c.Author == app && strings.Contains(c.Body, back)
			}) {
				t.Errorf("the task's comments = %+v; want a move back to ready starting %s", f.State().Issues[0].Comments, back)
			}
		})
	}
}

// The PR step's last writes, a crash at each, then a restart: the replay
// ends with the same check and move to pr-open, or, for a head that
// changes nothing, the same hand-off, and nothing else (SPEC §5.2). Each
// pass is a fresh daemon on the fake's own bare clone, so it runs the
// checks again before the PR step, and its third iteration reads what the
// step wrote, so the digest lists a hand-off and notify is assigned to it.
func TestPRMoveReplay(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		empty bool // the head changes nothing on main
		stage string
	}{
		{"pr-open", false, "stage:pr-open"},
		{"nothing to merge", true, "stage:needs-human"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			base := newVerdictRig(t, newFake(t), 3)
			ref := "refs/heads/task"
			head := base.head()
			if tt.empty {
				head, ref = base.otherHead("empty", nil), "refs/heads/empty"
			}
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
				roots[f] = t.TempDir()
				return f
			}, func(f *githubtest.Fake) error {
				g := newRig(t, f, base.o, roots[f])
				if clones[f] == nil {
					clones[f] = g.d.Clone
					gitIn(t, g.d.Clone.Dir(), "fetch", "--quiet", "--no-tags", base.o.work, ref+":"+repo.TasksRef+"1")
				}
				g.d.Clone, g.r.Workspace.Bare = clones[f], clones[f].Dir()
				v := &verdictRig{rig: g, o: base.o, cfg: base.cfg, ran: base.ran}
				v.wire()
				for range 3 {
					if err := v.d.Iterate(context.Background()); err != nil {
						return err
					}
				}
				return nil
			})
			for f := range roots {
				var want []githubtest.CheckRunState
				if !tt.empty {
					want = []githubtest.CheckRunState{prMoveVerified(head)}
				}
				if got := prMoveCheckRuns(f); !slices.Equal(got, want) {
					t.Errorf("after the replay, the check runs = %+v; want %+v", got, want)
				}
				to := strings.TrimPrefix(tt.stage, "stage:")
				if moves := prMoveComments(f, to); len(moves) != 1 || pickStage(f, 1) != tt.stage {
					t.Errorf("after the replay, the moves to %s = %q, at %s; want one, at %s", to, moves, pickStage(f, 1), tt.stage)
				}
			}
		})
	}
}

// prMoveDigest is the digest's body on f, or "" with none.
func prMoveDigest(f *githubtest.Fake) string {
	for _, is := range f.State().Issues {
		if is.Author == app && strings.HasSuffix(is.Body, "<!-- issuedog:digest -->") {
			return is.Body
		}
	}
	return ""
}

// A task back at verifying by a stage set by hand, after its pass moved it
// to pr-open, on the same head and pass, gets nothing more from that pass:
// the move keyed by its run is written, so it waits, with no request for
// its PR and no write. It holds the worker, so the digest lists it as a
// stalled task, which says to set stage:pr-open back or push a new head,
// and notify is assigned to it, while its PR's item ends, and notify is
// unassigned from the PR (SPEC §9.4, §9.6).
func TestPRMoveOnce(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head, _ := v.prTask()
	v.prOpens(head)
	pr := f.State().PRs[0].Number
	f.RemoveLabel(1, "owner", "stage:pr-open")
	f.AddLabel(1, "owner", "stage:verifying")
	v.passes(3)
	if pickStage(f, 1) != "stage:verifying" || len(prMoveComments(f, "pr-open")) != 1 || len(prMoveCheckRuns(f)) != 1 {
		t.Errorf("at %s, the moves %q, the checks %+v; want still at verifying, the one move and the one check",
			pickStage(f, 1), prMoveComments(f, "pr-open"), prMoveCheckRuns(f))
	}
	digest := prMoveDigest(f)
	item := "### 1. #1: Refuse an expired quote\n\n**Stalled task**, priority p1, unblocks 1 open issue (itself included)\n\n" +
		"- This task is back at `verifying`, but its pass, run `rv`, moved it to `pr-open` once already, so the pass moves it no more. " +
		"It holds the worker, so no other task is picked.\n" +
		"- Set `stage:pr-open` back, or push a new head.\n" +
		"- Answer here: https://github.com/o/r/issues/1"
	if !strings.Contains(digest, item) || strings.Contains(digest, "**Pull request**") {
		t.Errorf("the digest =\n%s\nwant it to list\n%s\nand no pull request", digest, item)
	}
	if got, task := assignees(f, pr), assignees(f, 1); len(got) != 0 || !slices.Equal(task, []string{wholeRunOwner}) {
		t.Errorf("the PR's assignees = %q, the task's %q; want nobody on the PR, and %s on the task", got, task, wholeRunOwner)
	}
	from := len(f.Requests())
	v.quietPass("back at verifying")
	if reads, writes := prRequests(f, from); reads != 0 || writes != 0 {
		t.Errorf("the waiting task's pass made %d reads and %d writes of /pulls; want none", reads, writes)
	}
}

// A move keyed by the pass's run that's written after step 1 read the
// issue, as by a pass that crashed after the comment, is found when the
// PR step reads the issue again: its label rules finish the move's labels,
// as the App's own move cut short, in that pass, and the step writes no
// push, PR, check or second comment.
func TestPRMoveWrittenSinceStep1(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head, _ := v.prTask()
	v.pass()
	f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/pulls" }, func() {
		f.AddComment(1, app, prMoveText(7, head)+"<!-- issuedog:transition from=verifying to=pr-open run=rv head="+head+" pr=7 -->")
	})
	writes := len(f.Writes())
	v.pass()
	if got, want := writesOf(f)[writes:], []string{"POST /issues/1/labels", "DELETE /issues/1/labels/stage:verifying"}; !slices.Equal(got, want) {
		t.Errorf("the PR step wrote %q, want %q: only the written move's labels", got, want)
	}
	if pickStage(f, 1) != "stage:pr-open" || v.originTip(prBranch) != "" || len(f.State().PRs) != 0 || len(prMoveCheckRuns(f)) != 0 ||
		len(prMoveComments(f, "pr-open")) != 1 {
		t.Errorf("at %s, the branch at %q, PRs %+v, checks %+v, moves %q; want pr-open, and no push, PR, check or second move",
			pickStage(f, 1), v.originTip(prBranch), f.State().PRs, prMoveCheckRuns(f), prMoveComments(f, "pr-open"))
	}
}
