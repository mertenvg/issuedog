package daemon_test

import (
	"maps"
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

// This file is a whole run of the implementer against the fakes: a task
// the owner filed and set to in-progress by hand, as the pick does for a
// ready task (schedule_run_test.go), through the real Implementer at
// in-progress, as daemon.RegisterStages registers it, with the daemon
// built as startDaemon builds it (rig.wireAsMain), so step 1 is
// daemon.NewStep's and the digest and assignment are on, and
// Daemon.Iterate as the loop. Config, trust and the implementer's prompt
// come from main on the origin.

// implementerRunConfig is .issuedog/config.yaml on main for the whole run:
// its own title types, diff limit, protected paths and questions per
// round, so the preamble's values are main's, not the defaults.
const implementerRunConfig = "types: labels\nnotify: [" + wholeRunOwner + "]\nscopes: [booking]\n" +
	"pr: {title_types: [feat, fix, docs]}\nverify: {max_diff_lines: 250, protected_paths: [secrets/**]}\n" +
	"loop: {max_questions_per_round: 5}\n"

// implementerRunPrompt is .issuedog/prompts/implementer.md on main.
const implementerRunPrompt = "Implement the task the house way.\n"

// implementerRunClaude is wholeRunClaude, but each run first commits
// run.<n>.txt in its clone, with no identity of its own, so git takes the
// run's environment's, and keeps the commit as $FAKE/commit.<n>, and its
// author and committer, as "<name> <email>" a line, as $FAKE/author.<n>.
var implementerRunClaude = strings.Replace(wholeRunClaude, `[ -f "$FAKE/out.$n" ] || exit 4`,
	`echo "$n" > "run.$n.txt" && git add "run.$n.txt" && git commit --quiet -m "run $n" &&
	git rev-parse HEAD > "$FAKE/commit.$n" && git log -1 --format='%an <%ae>%n%cn <%ce>' > "$FAKE/author.$n" || exit 5
[ -f "$FAKE/out.$n" ] || exit 4`, 1)

// A task the owner set to in-progress gets an implementer run as agent, in
// a clone of main, as a Committer, with the context of SPEC §12: issue.md
// with the task's priority and scope, the conversation without eve's
// comment, which no file has, as a task collects no untrusted input,
// input.md with none, and system.md, the preamble with main's limits,
// then main's prompt. Its commit, by the App, is the task's head in the
// bare repo, and the task moves to verifying with head=. The passes after
// write nothing, and an unchanged one makes no request but conditional
// GETs answered 304.
func TestImplementerWholeRun(t *testing.T) {
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
	if err := os.WriteFile(g.path("claude", "out.1"), []byte(ivResult(t, implementerOutput(t, "commits"))), 0o644); err != nil {
		t.Fatal(err)
	}
	n := f.AddIssue(wholeRunOwner, "Refuse an expired quote", "Booking an expired quote returns 410.",
		"type:task", "priority:p1", "scope:booking")
	note := f.AddComment(n, wholeRunOwner, "Keep the error text short.")
	f.AddComment(n, "eve", wholeRunEve)
	f.AddLabel(n, wholeRunOwner, "stage:in-progress")
	main := gitIn(t, g.d.Clone.Dir(), "rev-parse", "main")

	g.pass()
	if g.runs() != 1 {
		t.Fatalf("claude ran %d times, want once", g.runs())
	}
	w := &wholeRun{rig: g, n: n}
	files := w.context(1, state.InProgress)
	if l := g.leaseOf("1"); l.Start != main {
		t.Errorf("the run started at %s, want main's %s", l.Start, main)
	}
	if names := slices.Sorted(maps.Keys(files)); !slices.Equal(names, []string{"conversation.md", "input.md", "issue.md", "prompt.md", "system.md"}) {
		t.Errorf("the run's context files are %v; want issue.md, conversation.md, input.md, prompt.md and system.md, and no untrusted.md", names)
	}
	wantIssue := "# Issue #1\n\n- Title: Refuse an expired quote\n- Type: task\n- Parent: none\n- Author: " + wholeRunOwner +
		"\n- Priority: p1\n- Scope: `booking`\n\n## Body\n\n```text\nBooking an expired quote returns 410.\n```\n"
	if files["issue.md"] != wantIssue {
		t.Errorf("issue.md =\n%s\nwant\n%s", files["issue.md"], wantIssue)
	}
	conversation := files["conversation.md"]
	if !strings.Contains(conversation, "## Comment "+strconv.FormatInt(note, 10)+" by "+wholeRunOwner+", before round 1\n\n```text\nKeep the error text short.\n```") {
		t.Errorf("conversation.md =\n%s\nwant the owner's comment in it", conversation)
	}
	for name, text := range files {
		if strings.Contains(text, "EVE-COMMENT") {
			t.Errorf("%s has eve's comment; want it in no file", name)
		}
	}
	if want := "# Input for this run\n\n" + "What's new since the implementer last ran, or since the task came to in-progress. Verify's findings on the task's head, if any, come first: fix every one.\n\nnone: first run\n"; files["input.md"] != want {
		t.Errorf("input.md =\n%s\nwant\n%s", files["input.md"], want)
	}
	system := files["system.md"]
	for _, want := range []string{
		"- A PR title's `type` is one of: `feat`, `fix`, `docs` (`pr.title_types`), exactly as written.",
		"at most 250 changed lines (`verify.max_diff_lines`), counting only files outside `verify.test_globs`, which is empty, so every file counts. It's a hard limit.",
		"`.mcp.json`, `secrets/**`. A change that needs one is an owner action.",
		"so a question round now is round 1. At most 5 questions in a round",
	} {
		if !strings.Contains(system, want) {
			t.Errorf("system.md =\n%s\nwant it to say %q", system, want)
		}
	}
	if !strings.HasPrefix(system, "# issuedog's rules for this run\n\n") || !strings.HasSuffix(system, "\n\n"+implementerRunPrompt) {
		t.Errorf("system.md =\n%s\nwant the preamble, then main's prompt, %q", system, implementerRunPrompt)
	}

	head := g.trimmed("commit.1")
	wantAuthor := "issuedog[bot] <issuedog[bot]@users.noreply.github.com>"
	if got := g.trimmed("author.1"); got != wantAuthor+"\n"+wantAuthor {
		t.Errorf("the run's commit has author and committer\n%s\nwant the App's, %s, as both", got, wantAuthor)
	}
	if got := g.bareRefs(); got != wantRefs(main, head) {
		t.Errorf("the bare repo's refs =\n%s\nwant\n%s", got, wantRefs(main, head))
	}
	if labels := ivLabels(f, n); !slices.Contains(labels, "stage:verifying") || slices.Contains(labels, "stage:in-progress") {
		t.Errorf("the task's labels are %v, want stage:verifying in place of stage:in-progress", labels)
	}
	cs := w.comments()
	last := cs[len(cs)-1]
	moves := github.ParseMarkers(last.Body)
	if last.Author.Login != app || len(moves) != 1 || moves[0].Fields["from"] != "in-progress" || moves[0].Fields["to"] != "verifying" ||
		moves[0].Fields["run"] != "run1" || moves[0].Fields["head"] != head ||
		!strings.HasPrefix(last.Body, "The implementer committed `"+head+"`.\n\n**PR title:** feat: refuse booking an expired quote") {
		t.Errorf("the task's newest comment, by %s, =\n%s\nwant the App's move to verifying by run1, naming head %s", last.Author.Login, last.Body, head)
	}

	w.quiet("after the run")
	from := len(f.Requests())
	w.quiet("an unchanged pass")
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("an unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}
}
