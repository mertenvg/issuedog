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
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// This file pins SPEC §10.1's checks at verifying where a wrong choice
// would still pass the other tests: the scope rule 4 reads, the seen= of
// the findings move, the checks' place in Work, after task work, and the
// head they judge.

// Rule 4 reads the task's scope from its labels: a task in scope tooling
// (verify.tooling_scope) that changes .golangci.yml, a config path, passes
// every rule through a whole pass, so it waits at verifying with nothing
// written, after its commands ran once.
func TestVerifyToolingScopeChangesConfig(t *testing.T) {
	t.Parallel()
	ran := filepath.Join(t.TempDir(), "ran")
	o := verifyOrigin(t, verifyConfig("echo ran >> '"+ran+"'"))
	f := newFake(t)
	g := verifyRig(t, f, o)
	head := startHead(t, o, map[string]string{".golangci.yml": "x\n", "a.go": "package a\n", "a_test.go": "package a\n"}, false)
	n, _ := verifyingTask(f, head)
	f.RemoveLabel(n, app, "scope:booking")
	f.AddLabel(n, app, "scope:tooling")
	fetchStartHead(t, g, o, head)
	comments := len(f.State().Issues[n-1].Comments)

	g.pass()
	if got := ranCount(t, ran); got != 1 {
		t.Errorf("the commands ran %d times, want once", got)
	}
	cs := implementerComments(t, f, n)
	if len(cs) != comments || !slices.Contains(ivLabels(f, n), "stage:verifying") {
		t.Errorf("the task has %d comments, the newest\n%s\nand labels %v; want %d, and still stage:verifying, as scope tooling may change .golangci.yml",
			len(cs), cs[len(cs)-1].Body, ivLabels(f, n), comments)
	}
}

// A comment posted during the checks, after step 1 read the issue, is
// newer than the findings move's seen=, which stays the newest comment
// step 1 read, the move to verifying, though the move reads the issue
// again before it writes (SPEC §5, §10.1). So the comment is above the
// next run's bound.
func TestVerifyFindingsSeenFromStepOne(t *testing.T) {
	t.Parallel()
	o := verifyOrigin(t, verifyConfig("false"))
	f := newFake(t)
	g := verifyRig(t, f, o)
	head := startHead(t, o, map[string]string{"a.go": "package a\n", "a_test.go": "package a\n"}, false)
	n, move := verifyingTask(f, head)
	fetchStartHead(t, g, o, head)
	// A pass with no checks loads main's config and trust.
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
	var during int64
	// The next read of the issue, or of its comments, is the one before
	// the move.
	f.BeforeRequest(func(r githubtest.Request) bool {
		return r.Method == http.MethodGet && strings.HasPrefix(r.Path+"/", "/issues/"+strconv.Itoa(n)+"/")
	}, func() { during = f.AddComment(n, "owner", "Keep the error text short.") })
	if err := g.r.Work(ctx, []daemon.Ready{{S: s, Inputs: in}}); err != nil {
		t.Fatal(err)
	}
	cs := implementerComments(t, f, n)
	if len(cs) < 2 || cs[len(cs)-2].ID != during || during <= move {
		t.Fatalf("the task's comments are %+v; want the owner's comment %d, posted during the checks after the move %d, before the findings", cs, during, move)
	}
	if got, want := cs[len(cs)-1].Body, findingsMarker(move, head, move); !strings.HasSuffix(got, want) {
		t.Errorf("the findings comment =\n%s\nwant it to end %s, with seen= the move to verifying, not the owner's comment %d", got, want, during)
	}
}

// With one task at in-progress and another at verifying, as a stage label
// set by hand can leave them, a pass runs the implementer on the first,
// and no checks: task work comes before the checks (SPEC §8.1, §10.1).
// The pass after, with no task work left, runs the checks, on the task at
// verifying with the higher priority.
func TestVerifyAfterTaskWork(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	working := implementerTask(f)
	g := roundRig(t, f, roundConfig(3, "high"), ivResult(t, implementerOutput(t, "commits")))
	checked, move := verifyingTask(f, "")
	f.RemoveLabel(checked, app, "priority:p1")
	f.AddLabel(checked, app, "priority:p0")
	comments := len(f.State().Issues[checked-1].Comments)

	g.pass()
	if g.runs() != 1 || pickStage(f, working) != "stage:verifying" {
		t.Fatalf("after pass 1: %d runs, #%d at %s; want one, at verifying", g.runs(), working, pickStage(f, working))
	}
	if got := len(f.State().Issues[checked-1].Comments); got != comments || pickStage(f, checked) != "stage:verifying" {
		t.Errorf("after pass 1, #%d has %d comments, at %s; want %d, still at verifying, as task work ran first",
			checked, got, pickStage(f, checked), comments)
	}

	g.pass()
	cs := implementerComments(t, f, checked)
	if got, want := cs[len(cs)-1].Body, findingsMarker(move, "", move); g.runs() != 1 || !strings.HasSuffix(got, want) {
		t.Errorf("after pass 2, %d runs, #%d's newest comment =\n%s\nwant one run, and the findings, ending %s", g.runs(), checked, got, want)
	}
}

// The checks judge the head of the task's latest move into verifying, not
// an earlier round's: with a findings move between two moves into
// verifying, a command that fails only on a head holding bad.txt passes
// the task when only the earlier head holds it, writing nothing, and
// fails it when only the newest does, with findings naming the newest.
func TestVerifyJudgesTheNewestHead(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		newestPasses bool // whether bad.txt is only in the earlier head
	}{
		{"the newest head passes", true},
		{"the newest head fails", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := verifyOrigin(t, verifyConfig("test ! -e bad.txt"))
			f := newFake(t)
			g := verifyRig(t, f, o)
			files := map[string]string{"a.go": "package a\n", "a_test.go": "package a\n"}
			if tt.newestPasses {
				files["bad.txt"] = "bad\n"
			}
			earlier := startHead(t, o, files, false)
			gitIn(t, o.work, "checkout", "--quiet", "task")
			if tt.newestPasses {
				gitIn(t, o.work, "rm", "--quiet", "bad.txt")
			} else {
				if err := os.WriteFile(filepath.Join(o.work, "bad.txt"), []byte("bad\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitIn(t, o.work, "add", "bad.txt")
			}
			gitIn(t, o.work, "commit", "--quiet", "-m", "the task's newest run")
			newest := gitIn(t, o.work, "rev-parse", "HEAD")
			gitIn(t, o.work, "checkout", "--quiet", "main")

			n, first := verifyingTask(f, earlier)
			f.AddComment(n, app, "Verify failed.\n\n"+findingsMarker(first, earlier, first))
			f.AddComment(n, app, "The implementer committed.\n\n<!-- issuedog:transition from=in-progress to=verifying run=r2 head="+newest+" -->")
			fetchStartHead(t, g, o, newest)
			comments := len(f.State().Issues[n-1].Comments)

			g.pass()
			got := f.State().Issues[n-1].Comments
			if tt.newestPasses {
				if len(got) != comments || !slices.Contains(ivLabels(f, n), "stage:verifying") {
					t.Errorf("the task has %d comments and labels %v after its newest head passed; want %d, and stage:verifying",
						len(got), ivLabels(f, n), comments)
				}
				return
			}
			if len(got) != comments+1 {
				t.Fatalf("the task has %d comments after its newest head failed, want %d: the findings", len(got), comments+1)
			}
			if body := got[len(got)-1].Body; !strings.Contains(body, "head="+newest) || strings.Contains(body, earlier) {
				t.Errorf("the findings are %q; want them on the newest head, %s, not the earlier, %s", body, newest, earlier)
			}
		})
	}
}
