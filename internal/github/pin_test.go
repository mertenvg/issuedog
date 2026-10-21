package github_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// Pinning works, IsPinned reports it, and pinning again is idempotent.
func TestPinIssue(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("issuedog[bot]", "issuedog digest", "")
	other := f.AddIssue("owner", "Other", "")
	ctx, c := context.Background(), f.Client(t)
	if pinned, err := c.IsPinned(ctx, "o", "r", n); err != nil || pinned {
		t.Fatalf("IsPinned before the pin = %v, %v; want false", pinned, err)
	}
	for i := range 2 {
		if err := c.PinIssue(ctx, "o", "r", n); err != nil {
			t.Fatalf("pin %d: %v", i+1, err)
		}
		if pinned, err := c.IsPinned(ctx, "o", "r", n); err != nil || !pinned {
			t.Errorf("IsPinned after pin %d = %v, %v; want true", i+1, pinned, err)
		}
	}
	if pinned, err := c.IsPinned(ctx, "o", "r", other); err != nil || pinned {
		t.Errorf("IsPinned of another issue = %v, %v; want false", pinned, err)
	}
	if is := f.State().Issues; !is[n-1].Pinned || is[other-1].Pinned {
		t.Errorf("issues = %+v, want only #%d pinned", is, n)
	}
}

// A pin GitHub refuses gives an error naming GitHub's message.
func TestPinIssueRefused(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("issuedog[bot]", "issuedog digest", "")
	f.FailPin("Resource not accessible by integration")
	err := f.Client(t).PinIssue(context.Background(), "o", "r", n)
	if !errors.Is(err, github.ErrGraphQL) || !strings.Contains(err.Error(), "pin o/r#1: graphql errors: Resource not accessible by integration") {
		t.Errorf("err = %v, want GitHub's message, wrapped", err)
	}
	if f.State().Issues[0].Pinned {
		t.Error("pinned after a refused pin")
	}
}

// Pinning or asking about a missing issue fails, naming the issue.
func TestPinMissingIssue(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	ctx, c := context.Background(), f.Client(t)
	if err := c.PinIssue(ctx, "o", "r", 9); err == nil || !strings.Contains(err.Error(), "pin o/r#9: ") {
		t.Errorf("PinIssue: err = %v, want one naming o/r#9", err)
	}
	if _, err := c.IsPinned(ctx, "o", "r", 9); !errors.Is(err, github.ErrGraphQL) || !strings.Contains(err.Error(), "is o/r#9 pinned: ") {
		t.Errorf("IsPinned: err = %v, want GitHub's NOT_FOUND, naming o/r#9", err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes = %+v, want none for a missing issue", w)
	}
}

// CreateIssue files the App's issue and returns its number. ReopenIssue
// reopens it, and reopening an open issue changes nothing.
func TestCreateAndReopenIssue(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("owner", "First", "")
	ctx, c := context.Background(), f.Client(t)
	n, err := c.CreateIssue(ctx, "o", "r", "issuedog digest", "<!-- issuedog:digest -->")
	if err != nil || n != 2 {
		t.Fatalf("CreateIssue = %d, %v; want #2", n, err)
	}
	f.Close(n)
	for range 2 {
		if err := c.ReopenIssue(ctx, "o", "r", n); err != nil {
			t.Fatal(err)
		}
	}
	is := f.State().Issues[n-1]
	if is.Closed || is.Author != "issuedog[bot]" || is.Title != "issuedog digest" || is.Body != "<!-- issuedog:digest -->" {
		t.Errorf("issue = %+v, want the App's, open, with its title and body", is)
	}
	if err := c.ReopenIssue(ctx, "o", "r", 9); err == nil || !strings.Contains(err.Error(), "reopen o/r#9: ") {
		t.Errorf("reopen of a missing issue: err = %v, want one naming o/r#9", err)
	}
	f.FailWrite(len(f.Writes()) + 1)
	if _, err := c.CreateIssue(ctx, "o", "r", "issuedog digest", ""); err == nil || !strings.Contains(err.Error(), `create issue "issuedog digest" in o/r: `) {
		t.Errorf("a failed create: err = %v, want one naming the title and repo", err)
	}
}

// digestPass finds the App's issue with the digest marker, creating it if
// there is none, reopens it if it's closed, and pins it unless it's pinned:
// the writes the digest makes (SPEC §9.4), each checked before it's made.
func digestPass(t *testing.T) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		ctx, c := context.Background(), f.Client(t)
		all, err := c.IssuesSince(ctx, "o", "r", githubtest.Start.AddDate(-1, 0, 0))
		if err != nil {
			return err
		}
		n, closed := 0, false
		for _, is := range all {
			if is.GetUser().GetLogin() == "issuedog[bot]" && strings.Contains(is.GetBody(), "<!-- issuedog:digest -->") {
				n, closed = is.GetNumber(), is.GetState() == "closed"
			}
		}
		if n == 0 {
			if n, err = c.CreateIssue(ctx, "o", "r", "issuedog digest", "<!-- issuedog:digest -->"); err != nil {
				return err
			}
		}
		if closed {
			if err := c.ReopenIssue(ctx, "o", "r", n); err != nil {
				return err
			}
		}
		pinned, err := c.IsPinned(ctx, "o", "r", n)
		if err != nil || pinned {
			return err
		}
		return c.PinIssue(ctx, "o", "r", n)
	}
}

// Replay sees create, reopen and pin as writes, and a crash at any of them
// replays to the clean run's state.
func TestReplayCreateReopenPin(t *testing.T) {
	for name, tt := range map[string]struct {
		setup  func(testing.TB) *githubtest.Fake
		writes []string
	}{
		"create and pin": {func(t testing.TB) *githubtest.Fake {
			f := githubtest.New(t, "o/r", "issuedog")
			f.AddIssue("owner", "Someone's issue", "<!-- issuedog:digest -->")
			return f
		}, []string{"POST /issues", "POST /graphql"}},
		"reopen and pin": {func(t testing.TB) *githubtest.Fake {
			f := githubtest.New(t, "o/r", "issuedog")
			f.Close(f.AddIssue("issuedog[bot]", "issuedog digest", "<!-- issuedog:digest -->"))
			return f
		}, []string{"PATCH /issues/1", "POST /graphql"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := tt.setup(t)
			if err := digestPass(t)(f); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, w := range f.Writes() {
				got = append(got, w.Method+" "+w.Path)
			}
			if strings.Join(got, ", ") != strings.Join(tt.writes, ", ") {
				t.Errorf("writes = %q, want %q", got, tt.writes)
			}
			if is := f.State().Issues; is[len(is)-1].Closed || !is[len(is)-1].Pinned {
				t.Errorf("issues = %+v, want the digest open and pinned", is)
			}
			githubtest.Replay(t, tt.setup, digestPass(t))
		})
	}
}
