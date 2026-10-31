package daemon_test

import (
	"context"
	"net/http"
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
// the findings move, and the checks' place in Work, after task work.

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
