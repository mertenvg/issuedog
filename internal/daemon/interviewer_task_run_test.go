package daemon_test

import (
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/agent"
	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// taskRunClaude is wholeRunClaude, but run n first commits run.<n>.txt in
// its clone, as implementerRunClaude's runs do, when $FAKE/commits.<n>
// exists: the implementer's run commits, and the interviewer's don't.
var taskRunClaude = strings.Replace(wholeRunClaude, `[ -f "$FAKE/out.$n" ] || exit 4`,
	`if [ -f "$FAKE/commits.$n" ]; then
	echo "$n" > "run.$n.txt" && git add "run.$n.txt" && git commit --quiet -m "run $n" &&
		git rev-parse HEAD > "$FAKE/commit.$n" || exit 5
fi
[ -f "$FAKE/out.$n" ] || exit 4`, 1)

// A whole run against the fakes, on real temporary repos, with the daemon
// built as startDaemon builds it (rig.wireAsMain) and Daemon.Iterate as
// the loop: mertenvg files a bug at intake, and eve comments on it. The
// move into the pipeline says nothing of waiting, and the interviewer
// asks round 1 at once. While the round waits, a pass makes no request but
// conditional GETs answered 304, and starts no run. mertenvg's answer
// starts the next run, whose task the daemon writes into the body, with
// scope:booking, and the bug moves to ready, keyed by the run (SPEC §9.1,
// §11.2). Then the pick moves it to in-progress, and the implementer's run
// reads the task as issue.md's body.
func TestTaskWholeRun(t *testing.T) {
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
	if err := os.WriteFile(g.path("claude", "claude"), []byte(taskRunClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	g.wireAsMain(cfg)
	g.d.Digest, g.d.Assign = false, false // their writes are their own whole runs'
	for i, out := range []map[string]any{ivRound(1, ""), taskOut(t), implementerOutput(t, "commits")} {
		if err := os.WriteFile(g.path("claude", "out."+strconv.Itoa(i+1)), []byte(ivResult(t, out)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(g.path("claude", "commits.3"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	n := f.AddIssue(wholeRunOwner, "Expired quotes book", "Booking takes a quote after it expired.", "type:bug", "stage:intake")
	f.AddComment(n, "eve", wholeRunEve)
	w := &wholeRun{rig: g, n: n}

	// Admitted, then round 1, at once.
	g.pass()
	cs := w.comments()
	if want := wholeRunOwner + " opened this issue, and it has `stage:intake`, so it's in the pipeline (SPEC §6.2). I've moved it to `clarifying`."; !strings.HasPrefix(cs[1].Body, want+"\n\n<!-- issuedog:transition from=intake to=clarifying ") {
		t.Errorf("the move into the pipeline =\n%s\nwant it to start %q, with nothing of waiting", cs[1].Body, want)
	}
	if g.runs() != 1 || !strings.Contains(cs[len(cs)-1].Body, "### Questions for #1 (round 1)") {
		t.Fatalf("after the first pass, %d runs, lease %+v, the newest comment\n%s\nwant round 1, from one run", g.runs(), g.lease(), cs[len(cs)-1].Body)
	}
	w.labels("round 1", "stage:clarifying", "type:bug")

	// The round waits: the pass after, which reads the round's own write,
	// writes nothing and starts no run, and an unchanged pass then makes no
	// request at all but conditional GETs answered 304: every request
	// counts, whatever its status.
	w.quiet("round 1 posted")
	from := len(f.Requests())
	w.quiet("round 1 waiting")
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("a pass with the round waiting sent %+v; want only conditional GETs answered 304", r)
		}
	}
	if len(f.Requests()) == from {
		t.Errorf("a pass with the round waiting sent no request; want its conditional GETs")
	}

	// The answer: the task, at ready.
	answer := f.AddComment(n, wholeRunOwner, "1a")
	g.pass()
	if got, want := f.State().Issues[n-1].Body, taskBody(t, "run2", "", "none", "p2"); got != want {
		t.Errorf("body =\n%s\nwant\n%s", got, want)
	}
	moved := w.newest("the task", fmt.Sprintf("%s\n\n<!-- issuedog:transition from=clarifying to=ready run=run2 seen=%d -->", taskMovedText, answer))
	w.labels("the task", "scope:booking", "stage:ready", "type:bug")
	if g.runs() != 2 {
		t.Fatalf("%d runs in all, want the interviewer's two", g.runs())
	}
	for k := 1; k <= 2; k++ {
		sys := w.context(k, state.Clarifying)["system.md"]
		for _, want := range []string{"- The outcomes you may return here: `questions` and `task`. Any other outcome fails the run.",
			"- A task's scope is one of: `booking`. Any other scope is a question to the owner, never a guess."} {
			if !strings.Contains(sys, want) {
				t.Errorf("interviewer run %d's system.md lacks %q:\n%s", k, want, sys)
			}
		}
	}

	// The pick, then the implementer's run.
	g.pass()
	cs = w.comments()
	at := slices.IndexFunc(cs, func(c github.Comment) bool { return c.ID == moved.ID })
	if want := pickMoved(moved.ID, moved.ID); at+1 >= len(cs) || cs[at+1].Body != want {
		t.Errorf("the comments after the move to ready %+v; want the pick's\n%s", cs[at+1:], want)
	}
	if g.runs() != 3 {
		t.Fatalf("%d runs in all, want the implementer's third", g.runs())
	}
	w.labels("the implementer's run", "scope:booking", "stage:verifying", "type:bug")
	files := w.context(3, state.InProgress)
	if issue := files["issue.md"]; !strings.Contains(issue, "## Acceptance criteria\n\n- Booking a quote issued 24 hours and 1 minute ago") {
		t.Errorf("the implementer's issue.md =\n%s\nwant the task's body", issue)
	}
	w.quiet("at verifying")
}
