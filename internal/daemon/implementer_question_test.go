package daemon_test

import (
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/agent"
	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/issuedog/internal/state"
)

// implementerAsked files a task that the interviewer asked round 1 of, two
// questions, and the planner round 2, of one, before it came to
// in-progress, as the App moved it there, and returns its number.
func implementerAsked(f *githubtest.Fake) int {
	n := f.AddIssue("owner", "Issue", "Refuse an expired quote.")
	f.SetType(n, "Task")
	for _, l := range []string{"stage:in-progress", "priority:p1", "scope:booking"} {
		f.AddLabel(n, app, l)
	}
	f.AddComment(n, app, "<!-- issuedog:transition from=intake to=clarifying run=r0 -->")
	f.AddComment(n, app, "Round 1.\n\n<!-- issuedog:transition from=clarifying to=clarifying round=1 run=r1 questions=2 -->")
	f.AddComment(n, app, "<!-- issuedog:transition from=clarifying to=planning run=r2 -->")
	f.AddComment(n, app, "Round 2.\n\n<!-- issuedog:transition from=planning to=planning round=2 run=r3 questions=1 -->")
	f.AddComment(n, app, "<!-- issuedog:transition from=planning to=in-progress run=r4 -->")
	return n
}

// implementerAskedOrigin is newOrigin with notify: [owner], so a round
// ends with the cc line.
func implementerAskedOrigin(t *testing.T) *origin {
	t.Helper()
	o := newOrigin(t)
	o.commit(config.Path, "types: labels\nnotify: [owner]\n")
	return o
}

// implementerRound is the App's comment on the move to clarifying of run,
// which asks the golden question output as round round on issue n, with
// seen, and the cc line for owner.
func implementerRound(n, round int, run string, seen int64) string {
	return fmt.Sprintf("### Questions for #%d (round %d)\n\n", n, round) +
		"**1. Should a quote booked at the exact second it expires be refused or accepted?** Blocks: #126\n" +
		"   a. Refused (recommended): the expiry time is the first moment the quote is no longer valid, as the quote API documents it\n" +
		"   b. Accepted, and refused only after that second\n\n" +
		"Reply in one comment, e.g. `1a 2c 3: free text`.\n\n" +
		"cc @owner\n\n" +
		fmt.Sprintf("<!-- issuedog:transition from=in-progress to=clarifying round=%d run=%s questions=1 seen=%d -->", round, run, seen)
}

// A question moves the task in-progress → clarifying, keyed by the run,
// with round= and questions=, and a comment that is the round in the SPEC
// §9.1 format, then the cc line. Its round is numbered with the
// interviewer's and the planner's on the task: round 3 after their 1 and
// 2. The task's head moves to the run's commit, as for any applied result.
// Nothing runs on the task at clarifying, and an unchanged pass after makes
// no request but conditional GETs answered 304.
func TestImplementerQuestionRound(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := implementerAsked(f)
	g := newImplementerRig(t, f, implementerAskedOrigin(t), t.TempDir(), nil, implementerOutput(t, "question"))
	main := gitIn(t, g.d.Clone.Dir(), "rev-parse", "main")

	g.pass()
	if g.runs() != 1 {
		t.Fatalf("claude ran %d times, want once", g.runs())
	}
	if labels := f.State().Issues[n-1].Labels; !slices.Contains(labels, "stage:clarifying") || slices.Contains(labels, "stage:in-progress") {
		t.Errorf("the task's labels are %v, want stage:clarifying in place of stage:in-progress", labels)
	}
	cs := implementerComments(t, f, n)
	last := cs[len(cs)-1]
	if want := implementerRound(n, 3, "run1", cs[len(cs)-2].ID); last.Author.Login != app || last.Body != want {
		t.Errorf("the task's newest comment, by %s, =\n%s\nwant the App's\n%s", last.Author.Login, last.Body, want)
	}
	if got, head := g.bareRefs(), g.trimmed("commit.1"); got != wantRefs(main, head) {
		t.Errorf("the bare repo's refs =\n%s\nwant\n%s", got, wantRefs(main, head))
	}

	writes := len(f.Writes())
	g.pass()
	if w := f.Writes(); len(w) != writes || g.runs() != 1 {
		t.Errorf("the pass after wrote %+v, with %d runs in all; want nothing, and 1", w[writes:], g.runs())
	}
	from := len(f.Requests())
	g.pass()
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("an unchanged pass at clarifying sent %+v; want only conditional GETs answered 304", r)
		}
	}
}

// A question result is checked as the interviewer's question round is
// (SPEC §9.1, §12): it has questions, and no title, summary or hand-off,
// each a rule of its own; at most loop.max_questions_per_round of them,
// with options no two the same; and a comment within the length limit.
// The schema holds the rest, such as one-line text. Each broken rule is a
// failed attempt, with nothing written, and the lease names it.
func TestImplementerQuestionCheckRules(t *testing.T) {
	t.Parallel()
	commits, handoff := implementerOutput(t, "commits"), implementerOutput(t, "handoff")
	question := func(out map[string]any) map[string]any {
		return out["questions"].([]any)[0].(map[string]any)
	}
	for _, tt := range []struct {
		name   string
		change func(out map[string]any)
		want   string
	}{
		{"no questions", func(out map[string]any) { delete(out, "questions") }, "rule outcome: question has questions"},
		{"a title", func(out map[string]any) { out["title"] = commits["title"] }, "rule outcome: question has no title"},
		{"a summary", func(out map[string]any) { out["summary"] = commits["summary"] }, "rule outcome: question has no summary"},
		{"a hand-off", func(out map[string]any) { out["handoff"] = handoff["handoff"] }, "rule outcome: question has no hand-off"},
		{"over the questions per round", func(out map[string]any) {
			q := out["questions"].([]any)[0]
			out["questions"] = []any{q, q, q, q, q, q, q, q}
		}, "rule question-count: 8 questions, over the limit of 7"},
		{"options that are the same", func(out map[string]any) { question(out)["others"] = []any{" REFUSED "} },
			`rule options: question 1: option b is the same as option a, "Refused"`},
		{"a comment over the limit", func(out map[string]any) { question(out)["text"] = strings.Repeat("a", 64512) },
			"rule length: the question round's comment is "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			implementerTask(f)
			out := implementerOutput(t, "question")
			tt.change(out)
			g := newImplementerRig(t, f, newOrigin(t), t.TempDir(), nil, out)
			main := gitIn(t, g.d.Clone.Dir(), "rev-parse", "main")
			writes := len(f.Writes())
			g.pass()
			if g.runs() != 1 {
				t.Fatalf("claude ran %d times, want once", g.runs())
			}
			if w := f.Writes()[writes:]; len(w) != 0 {
				t.Errorf("the failed run wrote %+v; want nothing", w)
			}
			if l := g.lease(); !l.Failed || l.Attempt != 1 || !strings.Contains(l.PublicFailure, tt.want) {
				t.Errorf("the lease = %+v; want a failed attempt, 1, naming %q", l, tt.want)
			}
			if got := g.bareRefs(); got != wantRefs(main, "") {
				t.Errorf("the bare repo's refs =\n%s\nwant main's only:\n%s", got, wantRefs(main, ""))
			}
		})
	}
}

// The move to clarifying, a crash at each of its writes, then a restart:
// the replay ends with the same writes, the round numbered as the clean
// run's (SPEC §5.2).
func TestImplementerQuestionReplay(t *testing.T) {
	t.Parallel()
	o := implementerAskedOrigin(t)
	roots, clones := map[*githubtest.Fake]string{}, map[*githubtest.Fake]*repo.Repo{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		implementerAsked(f)
		roots[f] = t.TempDir()
		return f
	}, func(f *githubtest.Fake) error {
		g := newImplementerRig(t, f, o, roots[f], clones[f], implementerOutput(t, "question"))
		// The commit's dates are fixed, so each replay's commit is the clean
		// run's.
		g.r.Env = append(g.r.Env, "GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z")
		if clones[f] == nil {
			clones[f] = g.d.Clone
		}
		return g.d.Iterate(t.Context())
	})
	for f := range clones {
		labels, cs := f.State().Issues[0].Labels, implementerComments(t, f, 1)
		last := cs[len(cs)-1]
		if want := implementerRound(1, 3, "run1", cs[len(cs)-2].ID); last.Body != want || !slices.Contains(labels, "stage:clarifying") {
			t.Errorf("after the replay, the newest comment =\n%s\nwith labels %v; want\n%s\nat clarifying", last.Body, labels, want)
		}
	}
}

// A question whose move to clarifying is written, but whose labels a crash
// cut short, only finishes its move when the result is applied again, as
// the question's: a label write that fails names the move to clarifying
// on implementer-question, and once it works, the task is at clarifying.
// The output isn't read again, so even none at all applies (SPEC §8.2,
// Checks).
func TestImplementerQuestionMovedRunFinishes(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := implementerTask(f)
	f.AddComment(n, app, "Round 1.\n\n<!-- issuedog:transition from=in-progress to=clarifying round=1 run=run1 questions=1 -->")
	g := newImplementerRig(t, f, newOrigin(t), t.TempDir(), nil, implementerOutput(t, "question"))
	im := &daemon.Implementer{Daemon: g.d}
	apply := func() error {
		s, err := g.d.Step.Client.Snapshot(t.Context(), "o", "r", n)
		if err != nil {
			t.Fatal(err)
		}
		return im.Apply(t.Context(), g.d.Step.Applier, daemon.Result{RunID: "run1", S: s, Dirt: daemon.Dirt{}})
	}
	f.FailWrite(len(f.Writes()) + 1) // the label
	if err := apply(); err == nil || !strings.Contains(err.Error(), "to clarifying on implementer-question") {
		t.Fatalf("Apply, its label write failing = %v; want the question's move to clarifying named", err)
	}
	f.FailWrite(0)
	if err := apply(); err != nil {
		t.Fatalf("Apply again: %v", err)
	}
	if labels := f.State().Issues[n-1].Labels; !slices.Contains(labels, "stage:clarifying") || slices.Contains(labels, "stage:in-progress") {
		t.Errorf("the task's labels are %v, want stage:clarifying in place of stage:in-progress", labels)
	}
	if cs := implementerComments(t, f, n); len(cs) != 2 {
		t.Errorf("the task has %d comments, want 2: its move to in-progress, and the one round", len(cs))
	}
}

// A whole question round against the fakes, wired as main wires the
// daemon, with the digest and assignment on (SPEC §7.2, §9.1, §9.4, §9.6):
// the task the owner set to in-progress gets an implementer run that asks
// round 1, which moves the task to clarifying. While it waits, the digest
// lists the round, and notify is assigned. The owner's answer moves the
// task back to ready in step 1, keyed by the answer, which ends the item
// and the assignment. Once the owner sets in-progress again by hand, as the
// scheduler will (8.1), the next run's input.md holds the answer, labelled
// with its round, and its commits move the task to verifying.
func TestImplementerQuestionWholeRun(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.SetPermission(wholeRunOwner, "admin")
	o := newOrigin(t)
	o.commit(".github/CODEOWNERS", "* @"+wholeRunOwner+"\n")
	o.commit(config.Path, implementerRunConfig)
	o.commit(agent.PromptDir+"/implementer.md", implementerRunPrompt)
	g := newRig(t, f, o, t.TempDir())
	cfg, err := config.Parse([]byte(implementerRunConfig))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(g.path("claude", "claude"), []byte(implementerRunClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	g.wireAsMain(cfg)
	for i, name := range []string{"question", "commits"} {
		if err := os.WriteFile(g.path("claude", "out."+strconv.Itoa(i+1)), []byte(ivResult(t, implementerOutput(t, name))), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	n := f.AddIssue(wholeRunOwner, "Refuse an expired quote", "Booking an expired quote returns 410.",
		"type:task", "priority:p1", "scope:booking")
	f.AddLabel(n, wholeRunOwner, "stage:in-progress")
	w := &wholeRun{rig: g, n: n}

	g.pass()
	if g.runs() != 1 {
		t.Fatalf("claude ran %d times, want once", g.runs())
	}
	round := w.comments()[len(w.comments())-1]
	if !strings.HasPrefix(round.Body, "### Questions for #1 (round 1)\n\n") || !strings.Contains(round.Body, "\n\ncc @"+wholeRunOwner+"\n\n") ||
		!strings.HasSuffix(round.Body, fmt.Sprintf("from=in-progress to=clarifying round=1 run=run1 questions=1 seen=%d -->", round.ID-1)) {
		t.Errorf("the task's newest comment =\n%s\nwant round 1, its cc line, and its move to clarifying", round.Body)
	}
	g.pass()
	digest := len(f.State().Issues)
	if got, want := digestRunItems(f.State().Issues[digest-1].Body), []string{"#1 Question round p1 1"}; !slices.Equal(got, want) {
		t.Errorf("the digest lists %q; want %q", got, want)
	}
	assignWant(t, f, "round 1 waits", map[int][]string{n: {wholeRunOwner}})
	w.quiet("round 1 waits")
	if g.runs() != 1 {
		t.Fatalf("claude ran %d times while round 1 waits, want once in all", g.runs())
	}

	answer := f.AddComment(n, wholeRunOwner, "1a")
	answerAt := len(f.State().Issues[n-1].Comments) - 1
	// The answer's move to ready frees the worker, so the same pass's
	// step 4 picks the task again and runs it (8.1).
	g.pass()
	if g.runs() != 2 {
		t.Fatalf("claude ran %d times once the answer moved the task to ready, want twice", g.runs())
	}
	var moved, picked bool
	for _, c := range w.comments() {
		if c.ID <= answer || c.Author.Login != app {
			continue
		}
		if want := fmt.Sprintf("Round 1 has an answer, so the task goes back to `ready`. The implementer's next run gets it.\n\n"+
			"<!-- issuedog:transition from=clarifying to=ready cmd=%d seen=%d -->", answer, answer); c.Body == want {
			moved = true
		}
		if moved && strings.Contains(c.Body, "<!-- issuedog:transition from=ready to=in-progress ") {
			picked = true
		}
	}
	if !moved || !picked {
		t.Errorf("after the answer, the App's move to ready is there: %v, and its pick after it: %v; want both", moved, picked)
	}
	if r := f.State().Issues[n-1].Comments[answerAt].Reactions; !slices.Equal(r, []string{app + " +1"}) {
		t.Errorf("the answer's reactions = %q, want the App's 👍", r)
	}
	g.pass()
	if got := digestRunItems(f.State().Issues[digest-1].Body); len(got) != 0 {
		t.Errorf("the digest lists %q once the round is answered; want nothing", got)
	}
	assignWant(t, f, "the answer", nil)
	if g.runs() != 2 {
		t.Fatalf("claude ran %d times at verifying, want twice in all", g.runs())
	}

	files := w.context(2, state.InProgress)
	if names := slices.Sorted(maps.Keys(files)); !slices.Equal(names, []string{"conversation.md", "input.md", "issue.md", "prompt.md", "system.md"}) {
		t.Errorf("run 2's context files are %v; want issue.md, conversation.md, input.md, prompt.md and system.md", names)
	}
	want := "# Input for this run\n\nWhat's new since the implementer last ran, or since the task came to in-progress.\n\n" +
		"## Comment " + strconv.FormatInt(answer, 10) + " by " + wholeRunOwner + ": an answer to round 1\n\n```text\n1a\n```\n"
	if files["input.md"] != want {
		t.Errorf("run 2's input.md =\n%s\nwant\n%s", files["input.md"], want)
	}
	if !strings.Contains(files["system.md"], "Questions so far on this issue: round 1 (Q1.1). A question round now is round 2.") {
		t.Errorf("run 2's system.md =\n%s\nwant round 1 so far, and round 2 next", files["system.md"])
	}
	w.labels("run 2", "priority:p1", "scope:booking", "stage:verifying", "type:task")
}
