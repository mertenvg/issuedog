package daemon_test

import (
	"net/http"
	"os"
	"slices"
	"strconv"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
)

// pickRunConfig is .issuedog/config.yaml on main for the pick's whole run.
const pickRunConfig = "types: labels\nnotify: [" + wholeRunOwner + "]\n"

// A whole run against the fakes, with the daemon built as startDaemon
// builds it (rig.wireAsMain), and Daemon.Iterate as the loop: two ready
// tasks, the one first by priority blocked by the other, go through the
// pick, the implementer's run and verifying one at a time, in order. The
// blocker is picked first, and holds the worker at verifying, so nothing
// is picked while it waits there. Once the owner closes it, the next poll
// lists the close, the search is made again, and the other is picked.
// After that, an unchanged pass makes no request but conditional GETs
// answered 304.
func TestPickWholeRun(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.SetPermission(wholeRunOwner, "admin")
	o := newOrigin(t)
	o.commit(".github/CODEOWNERS", "* @"+wholeRunOwner+"\n")
	o.commit(config.Path, pickRunConfig)
	g := newRig(t, f, o, t.TempDir())
	cfg, err := config.Parse([]byte(pickRunConfig))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(g.path("claude", "claude"), []byte(implementerRunClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	g.wireAsMain(cfg)
	for _, n := range []string{"1", "2"} {
		if err := os.WriteFile(g.path("claude", "out."+n), []byte(ivResult(t, implementerOutput(t, "commits"))), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	blocker := pickReady(f, "p1")
	blocked := pickReady(f, "p0")
	f.SetBlockedBy(blocked, blocker)
	w := &wholeRun{rig: g}

	g.pass()
	if got := g.leaseIssues(); !slices.Equal(got, []int{blocker}) {
		t.Fatalf("the first pass's runs leased %v, want #%d alone", got, blocker)
	}
	for n, want := range map[int]string{blocker: "stage:verifying", blocked: "stage:ready"} {
		if got := pickStage(f, n); got != want {
			t.Errorf("after the first pass, #%d is at %s, want %s", n, got, want)
		}
	}
	w.quiet("the blocker at verifying")
	if g.runs() != 1 || pickStage(f, blocked) != "stage:ready" {
		t.Errorf("with the blocker at verifying, claude ran %d times in all, and #%d is at %s; want once, and it left at ready",
			g.runs(), blocked, pickStage(f, blocked))
	}

	f.CloseBy(blocker, wholeRunOwner)
	g.pass()
	if got := g.leaseIssues(); !slices.Equal(got, []int{blocker, blocked}) {
		t.Fatalf("the runs leased %v, want #%d then #%d", got, blocker, blocked)
	}
	if got := pickStage(f, blocked); got != "stage:verifying" || !f.State().Issues[blocker-1].Closed {
		t.Errorf("#%d is at %s, and #%d closed %v; want the first at verifying, and the blocker closed", blocked, got, blocker, f.State().Issues[blocker-1].Closed)
	}
	cs := implementerComments(t, f, blocked)
	if last := cs[len(cs)-1]; last.Body != implementerCommitted("run2", g.trimmed("commit.2"), 1, cs[len(cs)-2].ID) ||
		cs[len(cs)-2].Body != pickMoved(cs[0].ID, cs[0].ID) {
		t.Errorf("#%d's comments end\n%s\n%s\nwant the pick's move, then run2's move to verifying", blocked, cs[len(cs)-2].Body, last.Body)
	}
	if l := g.leaseOf(strconv.Itoa(2)); l.Start != gitIn(t, g.d.Clone.Dir(), "rev-parse", "main") {
		t.Errorf("run2 started at %s, want main's", l.Start)
	}

	w.quiet("both at verifying")
	from := len(f.Requests())
	w.quiet("an unchanged pass")
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("an unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}
}
