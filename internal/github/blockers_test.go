package github_test

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// Blockers reports an open and a closed blocker, in the order they were
// added, and a repeat with nothing changed is a conditional 304. Closing the
// open one makes the next call a 200 with its new state, and the call after
// that a 304 again. Reopening it shows it open.
func TestBlockers(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Human task", "body")
	open := f.AddIssue("owner", "Open blocker", "body")
	closed := f.AddIssue("owner", "Closed blocker", "body")
	free := f.AddIssue("owner", "Unblocked", "body")
	f.SetBlockedBy(n, closed)
	f.SetBlockedBy(n, open)
	f.Close(closed)
	ctx, c := context.Background(), f.Client(t)
	// read calls Blockers, wanting blockers and the one read it makes, as
	// readsSince shows it.
	read := func(name string, want []github.Blocker, wantRead string) {
		t.Helper()
		from := len(f.Requests())
		got, err := c.Blockers(ctx, "o", "r", n)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: blockers = %+v, want %+v", name, got, want)
		}
		if reads := readsSince(f, from); !slices.Equal(reads, []string{wantRead}) {
			t.Errorf("%s: requests = %q, want %q", name, reads, wantRead)
		}
	}
	const path = "GET /issues/1/dependencies/blocked_by "
	bothOpen := []github.Blocker{{Number: closed, Closed: true}, {Number: open}}
	bothClosed := []github.Blocker{{Number: closed, Closed: true}, {Number: open, Closed: true}}
	read("first", bothOpen, path+"200 plain")
	notModified := f.NotModified()
	read("unchanged", bothOpen, path+"304 conditional")
	f.Close(open)
	read("after the close", bothClosed, path+"200 conditional")
	read("unchanged after the close", bothClosed, path+"304 conditional")
	if got := f.NotModified(); got != notModified+2 {
		t.Errorf("NotModified() = %d, want %d: one for each unchanged read", got, notModified+2)
	}
	f.Reopen(open)
	read("after the reopen", bothOpen, path+"200 conditional")

	if got, err := c.Blockers(ctx, "o", "r", free); err != nil || len(got) != 0 {
		t.Errorf("blockers of #%d = %+v, %v; want none", free, got, err)
	}
	if _, err := c.Blockers(ctx, "o", "r", 999); err == nil || !strings.Contains(err.Error(), "blockers: list issues blocking o/r#999: ") {
		t.Errorf("blockers of a missing issue: err = %v, want one naming o/r#999", err)
	}
}
