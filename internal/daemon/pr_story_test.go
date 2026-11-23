package daemon_test

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/repo"
)

// A passed task in a story gets one PR into its story branch (SPEC §7.2,
// §7.5, §10.2, §11.5), on a newStoryVerifyRig, with real temporary repos:
// task #1 is in story #2, whose branch the test cuts and pushes to in the
// origin itself, and the task waits at verifying with a clean pass of the
// verifier's on its head, which the story branch's tip is checked for,
// then the head's checks against it run, so the next pass is the PR
// step's. Its PR has no digest item and is never assigned (SPEC §9.4,
// §9.6).

// storyPRTitle is task 1's PR's title, the fallback, as its move to
// verifying keeps no pr-text.
const storyPRTitle = "feat: Refuse an expired quote"

// storyPRReady cuts story #2's branch and pushes to it (storyCut), makes
// a head of task #1 on its tip, files the task at verifying with a clean
// pass on that head, then runs the pass that checks the tip and the one
// that checks the head against it, so the next pass is the PR step's. It
// returns the fake, the rig, the cut, the tip, the head and the pass
// comment's ID.
func storyPRReady(t *testing.T) (*githubtest.Fake, *verdictRig, string, string, string, int64) {
	t.Helper()
	f, v := newStoryVerifyRig(t)
	cut, tip := v.storyCut()
	head := v.storyHead(tip, map[string]string{"quote.txt": "expired\n"})
	storyVerifyTask(t, f, head, "planned")
	pass := f.AddComment(1, app, prPassComment(t, "rv", head, "Fine."))
	v.storyTipJudged(tip)
	v.pass()
	if v.checksRan() != 2 || pickStage(f, 1) != "stage:verifying" || len(f.State().PRs) != 0 {
		t.Fatalf("the head's checks ran the commands %d times in all, the task at %s, PRs %+v; want twice, verifying, and none",
			v.checksRan(), pickStage(f, 1), f.State().PRs)
	}
	return f, v, cut, tip, head, pass
}

// storyPRBody is the body of task 1's PR at head, verified against story
// #2's branch at tip by the pass in comment pass, with the fallback
// summary, the notes "Fine.", and the rig's commands and
// personBrokenCommand all passed (SPEC §11.5): it names the task and the
// story with no closing keyword, and its last line names the branch and
// the tip.
func (v *verdictRig) storyPRBody(head, tip string, pass int64) string {
	return "Task #1, in story #2\n\n## What changed\n\n" +
		"The implementer's summary isn't on #1: the task's move to `verifying` keeps no `pr-text` data, as a move made by hand doesn't.\n\n" +
		strings.Replace(v.prResults(), "| Rules 2 to 7 |", "| `"+personBrokenCommand+"` | pass |\n| Rules 2 to 7 |", 1) +
		"\n\n## Verifier notes\n\nFine.\n\n" +
		"Verified at `" + head + "`, against " + storyAgainst(tip) + ": https://github.com/o/r/issues/1#issuecomment-" + strconv.FormatInt(pass, 10)
}

// storyPROpenText is the comment of task 1's move to pr-open with PR pr,
// verified at head against story #2's branch's tip.
func storyPROpenText(pr int, head, tip string) string {
	return "PR #" + strconv.Itoa(pr) + " into the story branch `" + storyVerifyBranch + "` is open for this task, verified at `" + head +
		"` against the branch's tip `" + tip + "`. The App merges it once it's checked, then closes this task.\n\n"
}

// storyPROpened fails unless task 1 is at pr-open, by one move from
// verifying, keyed run=rv, with head=head, pr=, tip=tip and seen=pass, its
// comment storyPROpenText's, and its one PR is the App's, open from
// prBranch into story #2's branch at head, with storyPRBody's body for
// pass and the task's priority label. It returns the PR's number.
func storyPROpened(t *testing.T, f *githubtest.Fake, v *verdictRig, head, tip string, pass int64) int {
	t.Helper()
	prs := f.State().PRs
	if len(prs) != 1 || pickStage(f, 1) != "stage:pr-open" {
		t.Fatalf("the PRs = %+v, the task at %s; want one, and pr-open", prs, pickStage(f, 1))
	}
	pr := prs[0]
	want := githubtest.PRState{Number: pr.Number, Head: prBranch, Base: storyVerifyBranch, HeadSHA: head, Title: storyPRTitle,
		Body: v.storyPRBody(head, tip, pass), State: "open"}
	if pr != want {
		t.Errorf("the PR =\n%+v\nwant\n%+v", pr, want)
	}
	if strings.Contains(pr.Body, "Closes") || strings.Contains(pr.Body, "`main` at") {
		t.Errorf("the PR's body =\n%s\nwant no closing keyword, and no main", pr.Body)
	}
	if got := f.State().Issues[pr.Number-1]; !slices.Equal(got.Labels, []string{"priority:p1"}) || got.Author != app {
		t.Errorf("the PR, by %s, has labels %q; want the App's, with priority:p1", got.Author, got.Labels)
	}
	moves := personComments(t, f, "<!-- issuedog:transition from=verifying to=pr-open ")
	if len(moves) != 1 {
		t.Fatalf("the moves to pr-open = %+v; want one", moves)
	}
	fields := map[string]string{"from": "verifying", "to": "pr-open", "run": "rv", "head": head, "pr": strconv.Itoa(pr.Number), "tip": tip,
		"seen": strconv.FormatInt(pass, 10)}
	if got := prMoveFields(t, moves[0].Body); !maps.Equal(got, fields) {
		t.Errorf("the move's marker has %v; want %v", got, fields)
	}
	if text := storyPROpenText(pr.Number, head, tip); !strings.HasPrefix(moves[0].Body, text) {
		t.Errorf("the move's comment =\n%s\nwant it to start\n%s", moves[0].Body, text)
	}
	return pr.Number
}

// storyPRVerified is the App's success on head, as State has it, naming
// the story's baseline, cut, the newest main commit its commands ran
// with, in its external_id (SPEC §10.4).
func storyPRVerified(head, cut string) githubtest.CheckRunState {
	return githubtest.CheckRunState{SHA: head, Name: github.VerifyCheck, Conclusion: "success", AppID: githubtest.AppID, ExternalID: "main=" + cut}
}

// storyPROpensAtVerifying runs the PR step's pass with GitHub refusing the
// head's check once, as a glitch might, so that pass fails after the PR's
// writes and before the move: the task waits at verifying, with its PR
// into the story branch open at head and no check of the head's.
func (v *verdictRig) storyPROpensAtVerifying(head string) int {
	v.t.Helper()
	v.f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/check-runs" }, http.StatusInternalServerError)
	if err := v.d.Iterate(context.Background()); err == nil {
		v.t.Fatal("the PR step's pass whose check GitHub refused gave no error")
	}
	prs := v.f.State().PRs
	if len(prs) != 1 || prs[0].HeadSHA != head || prs[0].Base != storyVerifyBranch || prs[0].State != "open" || pickStage(v.f, 1) != "stage:verifying" ||
		slices.ContainsFunc(v.f.State().CheckRuns, func(c githubtest.CheckRunState) bool { return c.SHA == head }) {
		v.t.Fatalf("the PRs = %+v, the task at %s, the checks %+v; want one, open into %s at %s, the task still at verifying, and no check on the head",
			prs, pickStage(v.f, 1), v.f.State().CheckRuns, storyVerifyBranch, head)
	}
	return prs[0].Number
}

// The PR step on a passed task in a story: it pushes the head to the
// task's branch, opens one PR from it into the story branch, whose body
// names the task and the story with no closing keyword and ends naming the
// branch and the tip, labels it with the task's priority, posts
// issuedog/verify as success on the head, naming the story's baseline,
// its summary naming the branch and the tip, and moves the task to
// pr-open with head=, pr=, tip= and seen=, saying the App merges the PR
// once it's checked. No command runs. Once that's read, the passes after
// make no request but 304s, none of them for a PR.
func TestPRStoryOpens(t *testing.T) {
	t.Parallel()
	f, v, cut, tip, head, pass := storyPRReady(t)
	writes := len(f.Writes())
	v.pass()
	if got := v.originTip(prBranch); got != head {
		t.Errorf("the branch %s is at %q, want the head %s", prBranch, got, head)
	}
	pr := storyPROpened(t, f, v, head, tip, pass)
	if got, want := writesOf(f)[writes:], append([]string{"POST /pulls", "POST /issues/" + strconv.Itoa(pr) + "/labels"}, prMoveWrites...); !slices.Equal(got, want) {
		t.Errorf("the PR step wrote %q, want %q", got, want)
	}
	if got := f.State().CheckRuns; !slices.Contains(got, storyPRVerified(head, cut)) {
		t.Errorf("the check runs = %+v; want %+v among them", got, storyPRVerified(head, cut))
	}
	summary := "issuedog verified `" + head + "` for #1: the checks against " + storyAgainst(tip) + " passed, and so did the verifier"
	if check := tipLast(t, f); check.HeadSHA != head || check.Name != github.VerifyCheck || !strings.HasPrefix(check.Output.Summary, summary) {
		t.Errorf("the head's check = %+v; want issuedog/verify on %s, its summary starting\n%s", check, head, summary)
	}
	if v.checksRan() != 2 || v.runs() != 0 {
		t.Errorf("the commands ran %d times in all, claude %d; want twice, and never", v.checksRan(), v.runs())
	}
	v.pass()
	from := len(f.Requests())
	v.quietPass("after the PR step")
	v.quietPass("again")
	if reads, writes := prRequests(f, from); reads != 0 || writes != 0 {
		t.Errorf("the quiet passes read /pulls %d times, and wrote it %d times; want neither", reads, writes)
	}
}

// A task's PR into its story branch waits on the App, not the owner (SPEC
// §9.4, §9.6): the digest lists no item for the task, and nobody is
// assigned to the PR or the task, on the passes after the move to pr-open,
// a restart's included.
func TestPRStoryNoDigestItem(t *testing.T) {
	t.Parallel()
	f, v, _, _, _, _ := storyPRReady(t)
	v.passes(3)
	if pickStage(f, 1) != "stage:pr-open" || len(f.State().PRs) != 1 {
		t.Fatalf("the task at %s, PRs %+v; want pr-open, and one", pickStage(f, 1), f.State().PRs)
	}
	pr := f.State().PRs[0].Number
	check := func(step string) {
		t.Helper()
		digest := prMoveDigest(f)
		if digest == "" || strings.Contains(digest, "#1: ") || strings.Contains(digest, "**Pull request**") {
			t.Errorf("%s: the digest =\n%s\nwant it to list nothing for #1", step, digest)
		}
		if got := assignees(f, pr); len(got) != 0 {
			t.Errorf("%s: the PR's assignees = %q; want nobody", step, got)
		}
		if got := assignees(f, 1); len(got) != 0 {
			t.Errorf("%s: the task's assignees = %q; want nobody", step, got)
		}
	}
	check("at pr-open")
	v.quietPass("at pr-open")
	v = v.restart()
	v.passes(2)
	check("after a restart")
}

// A PR into the story branch closed at verifying is reopened, in a request
// that sets its state alone, by the task's next pass through the PR step
// (SPEC §7.5's A closed PR): its body is as it was, so nothing else of it
// is written. Then come the head's check and the move, with tip=.
func TestPRStoryReopens(t *testing.T) {
	t.Parallel()
	f, v, cut, tip, head, pass := storyPRReady(t)
	pr := v.storyPROpensAtVerifying(head)
	f.ClosePR(pr, "owner")
	writes := len(f.Writes())
	v.pass()
	w := f.Writes()[writes:]
	if len(w) != 1+len(prMoveWrites) || w[0].Path != "/pulls/"+strconv.Itoa(pr) || w[0].Body != `{"state":"open"}`+"\n" ||
		!slices.Equal(writesOf(f)[writes+1:], prMoveWrites) {
		t.Errorf("the PR step wrote %+v; want the reopen, its state alone, then %q", w, prMoveWrites)
	}
	if got := storyPROpened(t, f, v, head, tip, pass); got != pr {
		t.Errorf("the task's PR is #%d; want #%d, reopened", got, pr)
	}
	if got := f.State().CheckRuns; !slices.Contains(got, storyPRVerified(head, cut)) {
		t.Errorf("the check runs = %+v; want %+v among them", got, storyPRVerified(head, cut))
	}
}

// A story branch whose tip moves under an open PR at verifying: the new
// tip is checked, then the head's checks run again against it, and then
// the PR step updates the PR's body alone, which names the new tip, with
// no push, no second PR, no state and no title sent. Then come the head's
// check and the move, whose tip= is the new tip.
func TestPRStoryUpdates(t *testing.T) {
	t.Parallel()
	f, v, _, _, head, pass := storyPRReady(t)
	pr := v.storyPROpensAtVerifying(head)
	moved := v.tipPush(storyVerifyBranch, map[string]string{"more.txt": "more of the story\n"})
	v.storyTipJudgedAgain(moved)
	v.pass()
	if v.checksRan() != 4 || pickStage(f, 1) != "stage:verifying" {
		t.Fatalf("the commands ran %d times in all, the task at %s; want 4, the head's on the new tip, and verifying", v.checksRan(), pickStage(f, 1))
	}
	writes := len(f.Writes())
	v.pass()
	w := f.Writes()[writes:]
	if len(w) != 1+len(prMoveWrites) || w[0].Method != http.MethodPatch || w[0].Path != "/pulls/"+strconv.Itoa(pr) ||
		!strings.HasPrefix(w[0].Body, `{"body":`) || strings.Contains(w[0].Body, `"state"`) || strings.Contains(w[0].Body, `"title"`) ||
		!slices.Equal(writesOf(f)[writes+1:], prMoveWrites) {
		t.Errorf("the update wrote %+v; want one PATCH of the PR's body alone, then %q", w, prMoveWrites)
	}
	storyPROpened(t, f, v, head, moved, pass)
}

// storyTipJudgedAgain runs the pass that checks tip, story #2's branch's
// new tip, as success, one more run of the commands, with the task still
// at verifying.
func (v *verdictRig) storyTipJudgedAgain(tip string) {
	v.t.Helper()
	ran := v.checksRan()
	v.pass()
	if got := v.f.State().CheckRuns; !slices.Contains(got, tipCheck(tip, storyVerifyBranch, "success")) || v.checksRan() != ran+1 ||
		pickStage(v.f, 1) != "stage:verifying" {
		v.t.Fatalf("the new tip's pass left the check runs %+v, ran the commands %d times, with the task at %s; want its success, once, and verifying",
			got, v.checksRan()-ran, pickStage(v.f, 1))
	}
}

// A head that changes nothing on the story branch, as the branch's tip
// itself, gets the nothing-to-merge hand-off in place of a PR, naming the
// branch: the task moves verifying → needs-human on nothing-to-merge,
// keyed run=rv, with head= and seen=, with nothing pushed, no PR and no
// check, and the digest's hand-off item says the head changes nothing on
// its story branch.
func TestPRStoryNothingToMerge(t *testing.T) {
	t.Parallel()
	f, v := newStoryVerifyRig(t)
	_, tip := v.storyCut()
	gitIn(t, v.o.work, "branch", "--quiet", "-f", "storytask", tip)
	v.storyFetch(tip)
	storyVerifyTask(t, f, tip, "planned")
	pass := f.AddComment(1, app, prPassComment(t, "rv", tip, "Nothing needed to change."))
	v.storyTipJudged(tip)
	v.passes(2)
	if v.originTip(prBranch) != "" || len(f.State().PRs) != 0 || len(f.State().CheckRuns) != 1 || pickStage(f, 1) != "stage:needs-human" {
		t.Fatalf("the branch at %q, PRs %+v, checks %+v, at %s; want no push, no PR, the tip's check alone, and at needs-human",
			v.originTip(prBranch), f.State().PRs, f.State().CheckRuns, pickStage(f, 1))
	}
	moves := personComments(t, f, "<!-- issuedog:transition from=verifying to=needs-human ")
	if len(moves) != 1 {
		t.Fatalf("the hand-offs = %+v; want one", moves)
	}
	want := map[string]string{"from": "verifying", "to": "needs-human", "run": "rv", "head": tip, "seen": strconv.FormatInt(pass, 10)}
	if got := prMoveFields(t, moves[0].Body); !maps.Equal(got, want) {
		t.Errorf("the hand-off's marker has %v; want %v", got, want)
	}
	branch := "`" + storyVerifyBranch + "`"
	for _, text := range []string{
		"The verifier passed `" + tip + "`, but it changes nothing on " + branch + ": its own diff from its merge base with " + branch + " is empty, so it gets no PR.\n\n",
		"**Goal:** Decide whether this task needs a change on " + branch + ", as its verified head makes none.\n",
	} {
		if !strings.Contains(moves[0].Body, text) || strings.Contains(moves[0].Body, "`main`") {
			t.Errorf("the hand-off =\n%s\nwant it to hold\n%s\nand never name main", moves[0].Body, text)
		}
	}
	v.pass()
	item := "A hand-off waits for you: the verifier passed a head that changes nothing on its story branch, so it gets no PR."
	if digest := prMoveDigest(f); !strings.Contains(digest, item) {
		t.Errorf("the digest =\n%s\nwant it to hold\n%s", digest, item)
	}
}

// Someone else's open PR from the task's branch into the story branch is
// never the task's (SPEC §7.5): GitHub refuses the App's own open there,
// so the task waits at verifying, and the digest lists it as a stalled
// task, naming her PR and the story branch. Once she closes it, the next
// pass opens the App's, and the task moves to pr-open.
func TestPRStorySomeoneElsesPR(t *testing.T) {
	t.Parallel()
	f, v, _, tip, head, pass := storyPRReady(t)
	gitIn(t, v.o.work, "push", "--quiet", v.o.url, head+":refs/heads/"+prBranch)
	theirs := f.OpenPRAs("mallory", prBranch, storyVerifyBranch, "mallory's", "Mine.")
	v.pass()
	if prs := f.State().PRs; len(prs) != 1 || pickStage(f, 1) != "stage:verifying" {
		t.Fatalf("the PR step left the PRs %+v, the task at %s; want mallory's alone, and verifying", prs, pickStage(f, 1))
	}
	v.pass()
	lines := []string{
		"This task's head passed, but someone else's open PR #" + strconv.Itoa(theirs) + " from its branch to `" + storyVerifyBranch + "` stands, " +
			"so GitHub won't open the task's, and it waits at `verifying`. It holds the worker, so no other task is picked.",
		"Answer here: https://github.com/o/r/pull/" + strconv.Itoa(theirs),
	}
	digest := prMoveDigest(f)
	for _, line := range lines {
		if !strings.Contains(digest, line) {
			t.Errorf("the digest =\n%s\nwant it to hold\n%s", digest, line)
		}
	}
	v.pass()
	v.quietPass("waiting on mallory's PR")

	f.ClosePR(theirs, "mallory")
	v.pass()
	prs := f.State().PRs
	if len(prs) != 2 || prs[1].State != "open" || prs[1].Base != storyVerifyBranch || prs[1].HeadSHA != head || pickStage(f, 1) != "stage:pr-open" {
		t.Fatalf("once mallory's PR closed: PRs %+v, the task at %s; want the App's open into %s at %s, and the task at pr-open",
			prs, pickStage(f, 1), storyVerifyBranch, head)
	}
	if prs[1].Body != v.storyPRBody(head, tip, pass) {
		t.Errorf("the App's PR's body =\n%s\nwant\n%s", prs[1].Body, v.storyPRBody(head, tip, pass))
	}
}

// A person's head on a task in a story whose PR into the story branch is
// open: step 2 moves the task to verifying on it, the checks and the
// verifier pass it against the story branch's tip, and then the person's
// head's pass pushes nothing: the PR's body names the head, with its first
// line naming the story and its last the branch and the tip, issuedog/verify
// is a success on the head, and the task is back at pr-open on
// person-pushed, with tip= the tip, saying the App merges the PR once it's
// checked.
func TestPRStoryPersonHead(t *testing.T) {
	t.Parallel()
	f, v, cut, tip, head, _ := storyPRReady(t)
	v.passes(3)
	pr := f.State().PRs[0]
	if pickStage(f, 1) != "stage:pr-open" {
		t.Fatalf("the task is at %s; want pr-open", pickStage(f, 1))
	}
	person, _ := v.personPush(head, "person.txt")
	v.pass()
	if v.checksRan() != 3 || v.runs() != 0 || pickStage(f, 1) != "stage:verifying" {
		t.Fatalf("the checks' pass ran the commands %d times in all, claude %d, the task at %s; want 3, never, and verifying",
			v.checksRan(), v.runs(), pickStage(f, 1))
	}
	v.out(1, verifierPass(t, "Fine."))
	v.pass()
	passes := personComments(t, f, "<!-- issuedog:verify verdict=pass ")
	if v.runs() != 1 || len(passes) != 2 {
		t.Fatalf("after %d runs, the passes %+v; want one, and the verifier's pass on %s", v.runs(), passes, person)
	}
	run := github.ParseMarkers(passes[1].Body)[0].Fields["run"]

	v.pass()
	move, fields := personBack(t, f, person)
	want := map[string]string{"from": "verifying", "to": "pr-open", "run": run, "head": person, "pr": strconv.Itoa(pr.Number), "tip": tip,
		"seen": strconv.FormatInt(passes[1].ID, 10)}
	if !maps.Equal(fields, want) {
		t.Errorf("the move's marker has %v; want %v", fields, want)
	}
	text := "The verifier passed `" + person + "`, a head the daemon didn't push to this task's branch, so the task is back at `pr-open`, with nothing pushed and no attempt counted. " +
		"PR #" + strconv.Itoa(pr.Number) + "'s body names the head, and its `issuedog/verify` check there is a success. The App merges it into the story branch once it's checked, then closes this task.\n\n"
	if !strings.HasPrefix(move.Body, text) {
		t.Errorf("the move's comment =\n%s\nwant it to start\n%s", move.Body, text)
	}
	now := f.State().PRs[0]
	if body := v.storyPRBody(person, tip, passes[1].ID); len(f.State().PRs) != 1 || now.Body != body || now.Title != pr.Title || now.Base != storyVerifyBranch || now.HeadSHA != person {
		t.Errorf("the PRs are %+v; want the one, its title %q kept, into %s at %s, and its body\n%s", f.State().PRs, pr.Title, storyVerifyBranch, person, body)
	}
	if got := f.State().CheckRuns; !slices.Contains(got, storyPRVerified(person, cut)) {
		t.Errorf("the check runs = %+v; want %+v among them", got, storyPRVerified(person, cut))
	}
	if got := v.originTip(prBranch); got != person {
		t.Errorf("the branch is at %s; want the person's head %s, with nothing pushed", got, person)
	}
	personNever(t, f)
}

// A person's head on a task in a story that fails verify, by a failing
// command or the verifier's request_changes: the task is back at pr-open
// on person-pushed, with no tip=, as the head isn't verified, and its PR
// into the story branch, found among the PRs into it, gets the failed
// body, whose first line names the task and the story with no closing
// keyword, and issuedog/verify is a failure on the head.
func TestPRStoryPersonHeadFails(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, file string
		verifier   bool
	}{
		{"a failing command", "broken.txt", false},
		{"the verifier's request_changes", "person.txt", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, v, _, _, head, _ := storyPRReady(t)
			v.passes(3)
			pr := f.State().PRs[0]
			person, _ := v.personPush(head, tt.file)
			if tt.verifier {
				v.pass()
				v.out(1, ivResult(t, verifierOutput(t, "request_changes")))
			}
			v.pass()
			_, fields := personBack(t, f, person)
			if _, ok := fields["tip"]; ok || fields["pr"] != strconv.Itoa(pr.Number) {
				t.Errorf("the move's marker has %v; want pr=%d, and no tip", fields, pr.Number)
			}
			body := strings.Replace(personFailedBody(person), "Closes #1\n\n", "Task #1, in story #2\n\n", 1)
			if now := f.State().PRs[0]; len(f.State().PRs) != 1 || now.Body != body || now.Base != storyVerifyBranch {
				t.Errorf("the PRs are %+v; want the one into %s, its body\n%s", f.State().PRs, storyVerifyBranch, body)
			}
			if checks := personChecks(t, f, person); len(checks) != 1 || checks[0].Conclusion != "failure" {
				t.Errorf("the checks on %s = %+v; want one failure", person, checks)
			}
			personNever(t, f)
		})
	}
}

// A task's PR into its story branch closed at pr-open hands the task off,
// as a PR to main's close does (SPEC §7.2), but its first step says the
// reopened PR waits for the App's merge, not the owner's. A trusted reopen
// then brings the task back to pr-open, its comment saying the App merges
// the PR into the story branch once it's checked.
func TestPRStoryCloseAndReopen(t *testing.T) {
	t.Parallel()
	f, v, _, _, _, _ := storyPRReady(t)
	v.passes(3)
	pr := f.State().PRs[0].Number
	f.ClosePR(pr, wholeRunOwner)
	v.pass()
	handoffs := personComments(t, f, "<!-- issuedog:transition from=pr-open to=needs-human ")
	step := "1. To keep the PR, reopen it: the task goes back to `pr-open`, where the App merges it into the story branch once it's checked.\n"
	if len(handoffs) != 1 || pickStage(f, 1) != "stage:needs-human" || !strings.Contains(handoffs[0].Body, step) ||
		strings.Contains(handoffs[0].Body, "your review and merge") {
		t.Fatalf("the task at %s, its hand-offs %+v; want needs-human, by one holding\n%s\nand never the owner's review and merge", pickStage(f, 1), handoffs, step)
	}
	f.ReopenPR(pr, wholeRunOwner)
	v.passes(2)
	moves := personComments(t, f, "<!-- issuedog:transition from=needs-human to=pr-open ")
	text := "`" + wholeRunOwner + "` reopened PR #" + strconv.Itoa(pr) + ", so this task is back at `pr-open`. " +
		"The App merges it into the story branch once it's checked, then closes this task.\n\n"
	if len(moves) != 1 || pickStage(f, 1) != "stage:pr-open" || !strings.HasPrefix(moves[0].Body, text) {
		t.Errorf("the task at %s, its moves back %+v; want pr-open, by one starting\n%s", pickStage(f, 1), moves, text)
	}
}

// A fresh daemon between the PR's open and the move: it reads the tip's
// check once and runs the head's checks again, then its PR step finds the
// task's PR among those into the story branch, opens no second one, and
// posts the check and the move, with tip=.
func TestPRStoryRestart(t *testing.T) {
	t.Parallel()
	f, v, cut, tip, head, pass := storyPRReady(t)
	pr := v.storyPROpensAtVerifying(head)
	v = v.restart()
	v.pass()
	if v.checksRan() != 3 || pickStage(f, 1) != "stage:verifying" {
		t.Fatalf("the restart's first pass ran the commands %d times in all, the task at %s; want 3, and verifying", v.checksRan(), pickStage(f, 1))
	}
	writes := len(f.Writes())
	v.pass()
	if got := writesOf(f)[writes:]; !slices.Equal(got, prMoveWrites) {
		t.Errorf("the PR step after the restart wrote %q; want %q, and no PR write", got, prMoveWrites)
	}
	if got := storyPROpened(t, f, v, head, tip, pass); got != pr {
		t.Errorf("the task's PR is #%d; want #%d, found again", got, pr)
	}
	if got := f.State().CheckRuns; !slices.Contains(got, storyPRVerified(head, cut)) {
		t.Errorf("the check runs = %+v; want %+v among them", got, storyPRVerified(head, cut))
	}
}

// The PR step of a task in a story, a crash at each of its writes, then a
// restart: the replay ends with the same push, PR, label, check and move
// (SPEC §5.2). Each pass is a fresh daemon on the fake's own bare clone,
// so it reads the tip's check and runs the head's checks again before the
// PR step, and its third iteration reads what the step wrote. The origin
// is shared, so each setup deletes the task's branch there.
func TestPRStoryReplay(t *testing.T) {
	t.Parallel()
	_, base := newStoryVerifyRig(t)
	_, tip := base.storyCut()
	head := base.storyHead(tip, map[string]string{"quote.txt": "expired\n"})
	roots, clones := map[*githubtest.Fake]string{}, map[*githubtest.Fake]*repo.Repo{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		if gitIn(t, base.o.url, "for-each-ref", "refs/heads/"+prBranch) != "" {
			gitIn(t, base.o.url, "update-ref", "-d", "refs/heads/"+prBranch)
		}
		f := newFake(tb)
		f.SetPermission(wholeRunOwner, "admin")
		f.SetGitRemote(base.o.url)
		storyVerifyTask(tb, f, head, "planned")
		f.AddComment(1, app, prPassComment(tb, "rv", head, "Fine."))
		if _, err := f.Client(tb).PostCheck(context.Background(), "o", "r", github.TipCheck, github.NewCheckRun{HeadSHA: tip, Conclusion: "success",
			Title: "t", Summary: "s", ExternalID: "branch=" + storyVerifyBranch}); err != nil {
			tb.Fatal(err)
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
		for range 3 {
			if err := v.d.Iterate(context.Background()); err != nil {
				return err
			}
		}
		return nil
	})
	for f := range roots {
		prs := f.State().PRs
		if len(prs) != 1 || prs[0].HeadSHA != head || prs[0].Base != storyVerifyBranch || prs[0].State != "open" ||
			!strings.HasPrefix(prs[0].Body, "Task #1, in story #2\n\n") || pickStage(f, 1) != "stage:pr-open" {
			t.Fatalf("after the replay, the PRs = %+v, the task at %s; want one, open into %s at %s, with the PR step's body, and pr-open",
				prs, pickStage(f, 1), storyVerifyBranch, head)
		}
		if got := f.State().Issues[prs[0].Number-1]; !slices.Equal(got.Labels, []string{"priority:p1"}) || len(got.Assignees) != 0 {
			t.Errorf("after the replay, the PR's labels = %q, its assignees %q; want priority:p1, and nobody", got.Labels, got.Assignees)
		}
	}
}
