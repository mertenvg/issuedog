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
	"time"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/verify"
)

// This file is SPEC §10.1's checks on a task at verifying, on real
// temporary repos: the task's head in the daemon's clone, main's config
// naming the rules' globs, and shell commands as verify.commands, as the
// image has no Go toolchain.

// verifyConfig is .issuedog/config.yaml on main for the checks, with
// commands as verify.commands: Go files are source, *_test.go files
// tests, .golangci.yml a config path only scope tooling may change, at
// most 4 changed lines outside tests, and secrets/** protected.
func verifyConfig(commands ...string) string {
	quoted := make([]string, len(commands))
	for i, c := range commands {
		quoted[i] = strconv.Quote(c)
	}
	return "types: labels\nscopes: [booking, tooling]\nverify:\n" +
		"  commands: [" + strings.Join(quoted, ", ") + "]\n" +
		"  test_globs: [\"**/*_test.go\"]\n  source_globs: [\"**/*.go\"]\n  config_paths: [.golangci.yml]\n" +
		"  tooling_scope: tooling\n  max_diff_lines: 4\n  protected_paths: [\"secrets/**\"]\n"
}

// verifyOrigin is an origin whose main has cfg as its config, then f.txt,
// which a head may conflict on.
func verifyOrigin(t *testing.T, cfg string) *origin {
	t.Helper()
	o := newOrigin(t)
	o.commit(config.Path, cfg)
	o.commit("f.txt", "the base's\n")
	return o
}

// verifyRig is newRig on o with Commands on its workspace, run as the
// daemon, as tests can't switch to agent, each under a minute.
func verifyRig(t *testing.T, f *githubtest.Fake, o *origin) *rig {
	t.Helper()
	g := newRig(t, f, o, t.TempDir())
	g.r.Commands = &verify.Commands{Workspace: g.r.Workspace, Proxy: "http://127.0.0.1:3128", Timeout: func() time.Duration { return time.Minute }}
	return g
}

// verifyingTask files a task at verifying, with priority:p1 and
// scope:booking, as the implementer's move left it: the App's comment
// with its move by run r1, naming head as head=, if set. It returns the
// task's number and the move's comment ID.
func verifyingTask(f *githubtest.Fake, head string) (int, int64) {
	n := f.AddIssue("owner", "Refuse an expired quote.", "Booking an expired quote returns 410.")
	f.SetType(n, "Task")
	for _, l := range []string{"stage:verifying", "priority:p1", "scope:booking"} {
		f.AddLabel(n, app, l)
	}
	marker := "<!-- issuedog:transition from=in-progress to=verifying run=r1"
	if head != "" {
		marker += " head=" + head
	}
	return n, f.AddComment(n, app, "The implementer committed.\n\n"+marker+" -->")
}

// findingsMarker is the transition marker of the move back on findings,
// keyed by move, the comment holding the move to verifying, with head, if
// set, and seen, for a move to verifying that names no attempt.
func findingsMarker(move int64, head string, seen int64) string {
	return findingsAttemptMarker(move, head, 0, seen)
}

// findingsAttemptMarker is findingsMarker with attempt=, the attempt the
// move to verifying names, if above 0.
func findingsAttemptMarker(move int64, head string, attempt int, seen int64) string {
	m := "<!-- issuedog:transition from=verifying to=in-progress"
	if attempt > 0 {
		m += " attempt=" + strconv.Itoa(attempt)
	}
	m += " event=v" + strconv.FormatInt(move, 10)
	if head != "" {
		m += " head=" + head
	}
	return m + " seen=" + strconv.FormatInt(seen, 10) + " -->"
}

// ranCount is how many times the commands appended to the file at path:
// its lines, 0 when it's not there.
func ranCount(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

// skipMarker is a skip marker of verify.skip_markers' defaults, written
// in two parts, so this file holds none.
const skipMarker = "t.Sk" + "ip("

// Each rule of SPEC §10.1 fails a head in turn through a whole pass, and
// moves the task verifying → in-progress on findings, keyed by the
// comment holding its move to verifying, with head= and seen=, in a
// comment that names the head and main's commit, then the failed rule
// with its detail, fenced, and for rule 1, the failed command and its
// output. Every other rule passes each head. A head that fails rule 2,
// one with no merge base with main, one the bare repo lacks and none at
// all run no command; every other head runs every command.
func TestVerifyEachRule(t *testing.T) {
	t.Parallel()
	good := map[string]string{"a.go": "package a\n", "a_test.go": "package a\n"}
	with := func(more map[string]string) map[string]string {
		files := map[string]string{}
		for p, c := range good {
			files[p] = c
		}
		for p, c := range more {
			files[p] = c
		}
		return files
	}
	for _, tt := range []struct {
		name    string
		head    map[string]string // the head's change off main, nil for no head
		orphan  bool              // whether the head shares no history with main
		missing bool              // whether the head named is one the bare repo lacks
		unnamed bool              // whether the move to verifying names no head, so the task's head ref is checked
		fail    bool              // whether verify.commands fails too
		main    map[string]string // main's change after the head
		heading string            // the failed rule's section heading
		want    []string          // what the comment says, beside the heading
		ran     bool              // whether the commands ran
	}{
		{name: "rule 1", head: good, fail: true, heading: "### Rule 1: verify.commands", ran: true,
			want: []string{"This command failed, exit status 1:\n\n```text\nsh -c 'echo boom >&2; exit 1'\n```\n\nIts output ends:\n\n```text\nboom\n```"}},
		{name: "rule 2", head: with(map[string]string{"secrets/key": "k\n"}), heading: "### Rule 2: protected paths",
			want: []string{"The task's diff touches protected paths (SPEC 6.3 and verify.protected_paths):\n- secrets/key\n```"}},
		{name: "rule 2, with no merge base", head: good, orphan: true, heading: "### Rule 2: protected paths",
			want: []string{"doesn't have exactly one merge base with main's"}},
		{name: "rule 3", head: with(map[string]string{"a_test.go": "package a\n\nfunc TestA(t *testing.T) { " + skipMarker + "\"later\") }\n"}),
			heading: "### Rule 3: test deletions and skip markers", ran: true,
			want: []string{"The diff adds skip markers (verify.skip_markers) to test files:\n- a_test.go:3 adds " + strconv.Quote(skipMarker)}},
		{name: "rule 4", head: with(map[string]string{".golangci.yml": "x\n"}), heading: "### Rule 4: config paths", ran: true,
			want: []string{"only a task in scope \"tooling\" (verify.tooling_scope) may:\n- .golangci.yml\n```"}},
		{name: "rule 5", head: map[string]string{"a.go": "package a\n"}, heading: "### Rule 5: tests alongside source", ran: true,
			want: []string{"but adds or changes no test file (verify.test_globs):\n- a.go\n```"}},
		{name: "rule 5, on the task's head ref", head: map[string]string{"a.go": "package a\n"}, unnamed: true,
			heading: "### Rule 5: tests alongside source", ran: true, want: []string{"but adds or changes no test file (verify.test_globs):\n- a.go\n```"}},
		{name: "rule 6", head: with(map[string]string{"big.txt": "1\n2\n3\n4\n"}), heading: "### Rule 6: diff size and binaries", ran: true,
			want: []string{verify.TooLarge + ": the diff changes 5 lines in files outside verify.test_globs, over verify.max_diff_lines of 4."}},
		{name: "rule 7", head: with(map[string]string{"f.txt": "the head's\n"}), main: map[string]string{"f.txt": "main's\n"},
			heading: "### Rule 7: merge with the target", ran: true, want: []string{"These files conflict:\n- f.txt\n```"}},
		{name: "a head the bare repo lacks", missing: true, heading: "### The head",
			want: []string{"The head " + strings.Repeat("1", 40) + " isn't in the bare repo."}},
		{name: "no head", heading: "### The head", want: []string{"The task has no head: its move to verifying names none, and there's no refs/issuedog/tasks/1."}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ran := filepath.Join(t.TempDir(), "ran")
			commands := []string{"echo ran >> '" + ran + "'"}
			if tt.fail {
				commands = append(commands, "sh -c 'echo boom >&2; exit 1'")
			}
			o := verifyOrigin(t, verifyConfig(commands...))
			f := newFake(t)
			g := verifyRig(t, f, o)
			head := ""
			switch {
			case tt.missing:
				head = strings.Repeat("1", 40)
			case tt.head != nil:
				head = startHead(t, o, tt.head, tt.orphan)
			}
			for p, c := range tt.main {
				o.commit(p, c)
			}
			main := gitIn(t, o.work, "rev-parse", "main")
			named := head
			if tt.unnamed {
				named = ""
			}
			n, move := verifyingTask(f, named)
			if tt.head != nil {
				fetchStartHead(t, g, o, head)
			}

			g.pass()
			cs := implementerComments(t, f, n)
			last := cs[len(cs)-1]
			intro := "Verify failed at `" + head + "`, checked against `main` at `" + main + "`.\n\n"
			if head == "" {
				intro = "Verify failed with no head to check, against `main` at `" + main + "`.\n\n"
			}
			if last.Author.Login != app || !strings.HasPrefix(last.Body, intro+tt.heading+"\n\n") ||
				!strings.HasSuffix(last.Body, "\n\nSo the task is back at in-progress, for the implementer to fix.\n\n"+findingsMarker(move, head, move)) {
				t.Errorf("the task's newest comment, by %s, =\n%s\nwant the App's, starting %q, then %q, and ending with the move %s",
					last.Author.Login, last.Body, intro, tt.heading, findingsMarker(move, head, move))
			}
			for _, want := range tt.want {
				if !strings.Contains(last.Body, want) {
					t.Errorf("the findings comment =\n%s\nwant it to say %q", last.Body, want)
				}
			}
			if got := strings.Count(last.Body, "\n### "); got != 1 {
				t.Errorf("the findings comment has %d sections, want only %q's:\n%s", got, tt.heading, last.Body)
			}
			if labels := ivLabels(f, n); !slices.Contains(labels, "stage:in-progress") || slices.Contains(labels, "stage:verifying") {
				t.Errorf("the task's labels are %v, want stage:in-progress in place of stage:verifying", labels)
			}
			want := 0
			if tt.ran {
				want = 1
			}
			if got := ranCount(t, ran); got != want {
				t.Errorf("the commands ran %d times, want %d", got, want)
			}
			if left := g.leftovers(); len(left) != 0 {
				t.Errorf("the checks left %v; want no checkout, HOME or context dir", left)
			}
		})
	}
}

// Once rule 2 passes, every check runs, even after one fails, so the
// findings name every failure at once, in the order checked: each
// command, though the first fails, then rules 3 to 7 (SPEC §10.1).
func TestVerifyEveryRuleRuns(t *testing.T) {
	t.Parallel()
	ran := filepath.Join(t.TempDir(), "ran")
	o := verifyOrigin(t, verifyConfig("sh -c 'exit 3'", "echo ran >> '"+ran+"'"))
	f := newFake(t)
	g := verifyRig(t, f, o)
	head := startHead(t, o, map[string]string{
		"a.go":          "package a\n",
		"a_test.go":     "package a\n\nfunc TestA(t *testing.T) { " + skipMarker + "\"later\") }\n",
		".golangci.yml": "x\n",
		"big.txt":       "1\n2\n3\n4\n",
		"f.txt":         "the head's\n",
	}, false)
	o.commit("f.txt", "main's\n")
	n, _ := verifyingTask(f, head)
	fetchStartHead(t, g, o, head)

	g.pass()
	cs := implementerComments(t, f, n)
	body := cs[len(cs)-1].Body
	var headings []string
	for line := range strings.Lines(body) {
		if strings.HasPrefix(line, "### ") {
			headings = append(headings, strings.TrimSpace(line))
		}
	}
	want := []string{"### Rule 1: verify.commands", "### Rule 3: test deletions and skip markers", "### Rule 4: config paths",
		"### Rule 6: diff size and binaries", "### Rule 7: merge with the target"}
	if !slices.Equal(headings, want) {
		t.Errorf("the findings comment's sections are %q, want %q:\n%s", headings, want, body)
	}
	if !strings.Contains(body, "This command failed, exit status 3:") || ranCount(t, ran) != 1 {
		t.Errorf("the findings comment =\n%s\nwith the second command run %d times; want the first command's exit status 3, and the second run once",
			body, ranCount(t, ran))
	}
}

// A check that can't run, here as its commands' checkout can't be made, is
// the pass's error, not a finding: nothing is written, and the task stays
// at verifying, for the next pass to check again.
func TestVerifyErrorMovesNothing(t *testing.T) {
	t.Parallel()
	o := verifyOrigin(t, verifyConfig("true"))
	f := newFake(t)
	g := verifyRig(t, f, o)
	ws := *g.r.Workspace
	ws.ContextDir = g.path("missing")
	g.r.Commands.Workspace = &ws
	head := startHead(t, o, map[string]string{"a.go": "package a\n"}, false)
	n, _ := verifyingTask(f, head)
	fetchStartHead(t, g, o, head)
	comments := len(f.State().Issues[n-1].Comments)

	err := g.d.Iterate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rule 1") {
		t.Errorf("Iterate = %v, want rule 1's error", err)
	}
	if got := len(f.State().Issues[n-1].Comments); got != comments || !slices.Contains(ivLabels(f, n), "stage:verifying") {
		t.Errorf("the task has %d comments and labels %v; want %d, and still stage:verifying", got, ivLabels(f, n), comments)
	}
	g.r.Commands.Workspace = g.r.Workspace
	g.pass()
	if labels := ivLabels(f, n); !slices.Contains(labels, "stage:in-progress") {
		t.Errorf("the task's labels are %v once the checks could run, want stage:in-progress, as its head fails rule 5", labels)
	}
}

// A whole findings comment, exactly: a head that adds source with no
// test, checked against main's commit, gets rule 5's section, its detail
// fenced, and nothing else.
func TestVerifyFindingsComment(t *testing.T) {
	t.Parallel()
	o := verifyOrigin(t, verifyConfig("true"))
	f := newFake(t)
	g := verifyRig(t, f, o)
	head := startHead(t, o, map[string]string{"a.go": "package a\n"}, false)
	main := gitIn(t, o.work, "rev-parse", "main")
	n, move := verifyingTask(f, head)
	fetchStartHead(t, g, o, head)

	g.pass()
	cs := implementerComments(t, f, n)
	want := "Verify failed at `" + head + "`, checked against `main` at `" + main + "`.\n\n" +
		"### Rule 5: tests alongside source\n\n" +
		"```text\nThe diff adds or changes source files (verify.source_globs), but adds or changes no test file (verify.test_globs):\n- a.go\n```\n\n" +
		"So the task is back at in-progress, for the implementer to fix.\n\n" + findingsMarker(move, head, move)
	if last := cs[len(cs)-1]; last.Body != want {
		t.Errorf("the findings comment =\n%s\nwant\n%s", last.Body, want)
	}
}

// A head that passes every rule writes nothing, and the task waits at
// verifying (SPEC §10.1). The pass is kept by the head and main's commit:
// an unchanged pass after makes no request but conditional GETs answered
// 304, and runs no command. A new main commit runs the checks again,
// once.
func TestVerifyPassWaits(t *testing.T) {
	t.Parallel()
	ran := filepath.Join(t.TempDir(), "ran")
	o := verifyOrigin(t, verifyConfig("echo ran >> '"+ran+"'"))
	f := newFake(t)
	g := verifyRig(t, f, o)
	head := startHead(t, o, map[string]string{"a.go": "package a\n", "a_test.go": "package a\n"}, false)
	n, _ := verifyingTask(f, head)
	fetchStartHead(t, g, o, head)
	comments := len(f.State().Issues[n-1].Comments)

	g.pass()
	if got := ranCount(t, ran); got != 1 {
		t.Fatalf("the commands ran %d times, want once", got)
	}
	if got := len(f.State().Issues[n-1].Comments); got != comments || !slices.Contains(ivLabels(f, n), "stage:verifying") {
		t.Errorf("the task has %d comments and labels %v after its head passed; want %d, and stage:verifying", got, ivLabels(f, n), comments)
	}
	g.pass()
	from, writes := len(f.Requests()), len(f.Writes())
	g.pass()
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("an unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}
	if w := f.Writes(); len(w) != writes || ranCount(t, ran) != 1 {
		t.Errorf("an unchanged pass wrote %+v, and the commands ran %d times in all; want nothing, and once", w[writes:], ranCount(t, ran))
	}

	o.commit("docs.md", "Docs.\n")
	g.pass()
	g.pass()
	if got := ranCount(t, ran); got != 2 {
		t.Errorf("the commands ran %d times after main moved, want twice: once more", got)
	}
	if got := len(f.State().Issues[n-1].Comments); got != comments {
		t.Errorf("the task has %d comments after main moved, want %d", got, comments)
	}
}

// Once the findings move is written, an unchanged pass makes no request
// but conditional GETs answered 304, and runs no command: with no stage
// at in-progress, nothing runs on the task.
func TestVerifyQuietAfterFindings(t *testing.T) {
	t.Parallel()
	ran := filepath.Join(t.TempDir(), "ran")
	o := verifyOrigin(t, verifyConfig("echo ran >> '"+ran+"'", "false"))
	f := newFake(t)
	g := verifyRig(t, f, o)
	head := startHead(t, o, map[string]string{"a.go": "package a\n", "a_test.go": "package a\n"}, false)
	n, _ := verifyingTask(f, head)
	fetchStartHead(t, g, o, head)

	g.pass()
	if labels := ivLabels(f, n); !slices.Contains(labels, "stage:in-progress") {
		t.Fatalf("the task's labels are %v after its command failed, want stage:in-progress", labels)
	}
	g.pass()
	from, writes := len(f.Requests()), len(f.Writes())
	g.pass()
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("an unchanged pass after the findings move sent %+v; want only conditional GETs answered 304", r)
		}
	}
	if w := f.Writes(); len(w) != writes || ranCount(t, ran) != 1 {
		t.Errorf("an unchanged pass after the findings move wrote %+v, and the commands ran %d times in all; want nothing, and once",
			w[writes:], ranCount(t, ran))
	}
}

// A task at verifying with a parent gets no checks, uncounted, as it gets
// no run, and nor does a human task, which gets no PR: no command runs,
// and nothing is written on it.
func TestVerifySkips(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"a task with a parent", "a human task"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ran := filepath.Join(t.TempDir(), "ran")
			o := verifyOrigin(t, verifyConfig("echo ran >> '"+ran+"'", "false"))
			f := newFake(t)
			g := verifyRig(t, f, o)
			var n int
			if name == "a task with a parent" {
				story := f.AddIssue("owner", "A story", "The story.")
				f.SetType(story, "Story")
				n, _ = verifyingTask(f, "")
				f.SetParent(n, story)
			} else {
				n = f.AddIssue(app, "Add the secret", "Add the secret.\n\n<!-- issuedog:origin plan=#9 rev=1 item=T1 human=1 -->")
				f.SetType(n, "Task")
				f.AddLabel(n, app, "stage:verifying")
				f.AddComment(n, app, "<!-- issuedog:transition from=intake to=verifying run=rverifying -->")
			}
			comments := len(f.State().Issues[n-1].Comments)
			g.pass()
			g.pass()
			if got := len(f.State().Issues[n-1].Comments); got != comments || ranCount(t, ran) != 0 {
				t.Errorf("%s has %d comments after two passes, and the commands ran %d times; want %d, and never",
					name, got, ranCount(t, ran), comments)
			}
			if labels := ivLabels(f, n); !slices.Contains(labels, "stage:verifying") {
				t.Errorf("%s's labels are %v, want it still at verifying", name, labels)
			}
		})
	}
}

// The issue is read again before the findings move: a task closed, held
// by a discussion trigger, or moved by hand during the checks gets no
// move.
func TestVerifyMovedDuringChecks(t *testing.T) {
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
			o := verifyOrigin(t, verifyConfig("false"))
			f := newFake(t)
			g := verifyRig(t, f, o)
			head := startHead(t, o, map[string]string{"a.go": "package a\n", "a_test.go": "package a\n"}, false)
			n, _ := verifyingTask(f, head)
			fetchStartHead(t, g, o, head)
			// A pass with no checks loads main's config and trust, which
			// knows the write collaborators a discussion hold asks about.
			commands := g.r.Commands
			g.r.Commands = nil
			g.pass()
			g.r.Commands = commands

			ctx := context.Background()
			s, err := g.d.Step.Client.Snapshot(ctx, "o", "r", n)
			if err != nil {
				t.Fatal(err)
			}
			in, err := g.d.Step.Run(ctx, s)
			if err != nil {
				t.Fatal(err)
			}
			// The next read of the issue, or of its comments, is the one
			// before the move.
			f.BeforeRequest(func(r githubtest.Request) bool {
				return r.Method == http.MethodGet && strings.HasPrefix(r.Path+"/", "/issues/"+strconv.Itoa(n)+"/")
			}, func() { tt.during(f, n) })
			if err := g.r.Work(ctx, []daemon.Ready{{S: s, Inputs: in}}); err != nil {
				t.Fatal(err)
			}
			for _, c := range f.State().Issues[n-1].Comments {
				for _, m := range github.ParseMarkers(c.Body) {
					if m.Kind == "transition" && m.Fields["to"] == "in-progress" {
						t.Errorf("the task got the move %v; want no findings move", m.Fields)
					}
				}
			}
			if labels := ivLabels(f, n); !slices.Contains(labels, tt.label) {
				t.Errorf("the task's labels are %v, want %s", labels, tt.label)
			}
		})
	}
}

// The findings move, a crash at each of its writes, then a restart: the
// replay ends with the same writes (SPEC §5.2). Each pass is a fresh
// daemon, so it runs the checks again, on the head main's branch task
// holds, as the move names it.
func TestVerifyFindingsReplay(t *testing.T) {
	t.Parallel()
	cfg := verifyConfig("true")
	o := verifyOrigin(t, cfg)
	head := startHead(t, o, map[string]string{"a.go": "package a\n"}, false)
	gitIn(t, o.work, "push", "--quiet", o.url, "task")
	parsed, err := config.Parse([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		verifyingTask(f, head)
		return f
	}, func(f *githubtest.Fake) error {
		g := verifyRig(t, f, o)
		g.d.Config = parsed
		return g.work(1)
	})
}
