package github_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// A snapshot's Blocking counts the open issues its issue blocks, from the
// issue_dependencies_summary of the issue GET it makes anyway, with no
// dependency listing (SPEC §9.4, The order): a closed one it blocks, and
// one that blocks it, don't count. Once a blocked issue closes, the next
// read counts one fewer.
func TestSnapshotBlocking(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	blocker := f.AddIssue("owner", "Blocker", "body")
	first := f.AddIssue("owner", "First blocked", "body")
	second := f.AddIssue("owner", "Second blocked", "body")
	closed := f.AddIssue("owner", "Closed blocked", "body")
	above := f.AddIssue("owner", "Blocks the blocker", "body")
	for _, n := range []int{first, second, closed} {
		f.SetBlockedBy(n, blocker)
	}
	f.SetBlockedBy(blocker, above)
	f.Close(closed)
	ctx, c := context.Background(), f.Client(t)
	read := func(name string, n, want int) {
		t.Helper()
		from := len(f.Requests())
		s, err := c.Snapshot(ctx, "o", "r", n)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if s.Blocking != want {
			t.Errorf("%s: #%d's Blocking = %d, want %d", name, n, s.Blocking, want)
		}
		for _, r := range readsSince(f, from) {
			if strings.Contains(r, "/dependencies/") {
				t.Errorf("%s: the snapshot of #%d sent %q; want no dependency listing", name, n, r)
			}
		}
	}
	read("the blocker", blocker, 2)
	read("a blocked issue", first, 0)
	read("the blocker's blocker", above, 1)
	f.Close(second)
	read("the blocker once one it blocks closed", blocker, 1)
}

// The fake's issue list gives each issue its issue_dependencies_summary, as
// GitHub's does: the open issues blocking it and the ones it blocks, and
// all of each, open and closed.
func TestIssuesSinceDependencySummary(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	blocker := f.AddIssue("owner", "Blocker", "body")
	open := f.AddIssue("owner", "Open blocked", "body")
	closed := f.AddIssue("owner", "Closed blocked", "body")
	closedBlocker := f.AddIssue("owner", "Closed blocker", "body")
	f.SetBlockedBy(open, blocker)
	f.SetBlockedBy(closed, blocker)
	f.SetBlockedBy(open, closedBlocker)
	f.Close(closed)
	f.Close(closedBlocker)
	issues, err := f.Client(t).IssuesSince(context.Background(), "o", "r", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[int][4]int{blocker: {0, 0, 1, 2}, open: {1, 2, 0, 0}, closed: {1, 1, 0, 0}, closedBlocker: {0, 0, 1, 1}}
	for _, is := range issues {
		d := is.GetIssueDependenciesSummary()
		if got := [4]int{d.GetBlockedBy(), d.GetTotalBlockedBy(), d.GetBlocking(), d.GetTotalBlocking()}; d == nil || got != want[is.GetNumber()] {
			t.Errorf("#%d's summary (blocked by, of all, blocking, of all) = %v, want %v", is.GetNumber(), got, want[is.GetNumber()])
		}
	}
	if len(issues) != len(want) {
		t.Errorf("listed %d issues, want %d", len(issues), len(want))
	}
}
