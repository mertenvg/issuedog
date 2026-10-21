package githubtest

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
)

// issueReactions lists the reactions to issue n itself as "login content",
// two to a page, through the API.
func issueReactions(t *testing.T, c *github.Client, n int) []string {
	t.Helper()
	got := []string{}
	opts := &gogithub.ListReactionOptions{ListOptions: gogithub.ListOptions{PerPage: 2}}
	for r, err := range c.Client.Reactions.ListIssueReactionsIter(context.Background(), "o", "r", n, opts) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, r.GetUser().GetLogin()+" "+r.GetContent())
	}
	return got
}

// The reactions to an issue itself are served oldest first, across pages,
// apart from its comments' and other issues'. A reaction someone has
// already changes nothing and gives its ID. A deleted one is gone from the
// next read, and deleting it again reports it wasn't there. None of this
// moves the issue's updated_at.
func TestIssueReactions(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	other := f.AddIssue("owner", "Other", "body")
	f.AddReaction(f.AddComment(n, "owner", "hi"), "owner", "heart")
	f.AddIssueReaction(other, "owner", "rocket")
	c := f.Client(t)
	at := issueUpdatedAt(t, c, n)
	up := f.AddIssueReaction(n, "owner", "+1")
	f.AddIssueReaction(n, "mallory", "+1")
	eyes := f.AddIssueReaction(n, "issuedog[bot]", "eyes")
	if again := f.AddIssueReaction(n, "owner", "+1"); again != up {
		t.Errorf("a repeated reaction got ID %d, want %d, the one it has", again, up)
	}
	if up == eyes || up == 0 {
		t.Errorf("IDs %d and %d, want each its own", up, eyes)
	}
	if got, want := issueReactions(t, c, n), []string{"owner +1", "mallory +1", "issuedog[bot] eyes"}; !reflect.DeepEqual(got, want) {
		t.Errorf("reactions = %q, want %q", got, want)
	}
	if got, want := issueReactions(t, c, other), []string{"owner rocket"}; !reflect.DeepEqual(got, want) {
		t.Errorf("reactions to #%d = %q, want %q", other, got, want)
	}
	if !f.DeleteIssueReaction(n, up) {
		t.Errorf("DeleteIssueReaction = false, want true for a reaction that's there")
	}
	if f.DeleteIssueReaction(n, up) {
		t.Errorf("DeleteIssueReaction = true, want false once it's gone")
	}
	if f.DeleteIssueReaction(other, eyes) {
		t.Errorf("DeleteIssueReaction(%d, %d) = true for a reaction to #%d, want false", other, eyes, n)
	}
	if got, want := issueReactions(t, c, n), []string{"mallory +1", "issuedog[bot] eyes"}; !reflect.DeepEqual(got, want) {
		t.Errorf("reactions after a delete = %q, want %q", got, want)
	}
	if got := issueUpdatedAt(t, c, n); !got.Equal(at) {
		t.Errorf("updated_at %v, want %v unchanged", got, at)
	}
	if got := f.State().Issues[n-1].Comments[0].Reactions; !reflect.DeepEqual(got, []string{"owner heart"}) {
		t.Errorf("the comment's reactions = %q, want its own only", got)
	}
}

// A repeat of an unchanged listing of an issue's reactions is a 304, and an
// added reaction changes it.
func TestIssueReactionsNotModified(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	c := f.Client(t)
	if got := issueReactions(t, c, n); len(got) != 0 {
		t.Errorf("reactions = %q, want none", got)
	}
	notModified := f.NotModified()
	issueReactions(t, c, n)
	if got := f.NotModified(); got != notModified+1 {
		t.Errorf("NotModified() = %d after a repeated listing, want %d", got, notModified+1)
	}
	f.AddIssueReaction(n, "owner", "+1")
	if got, want := issueReactions(t, c, n), []string{"owner +1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("reactions after an add = %q, want %q", got, want)
	}
	if got := f.NotModified(); got != notModified+1 {
		t.Errorf("NotModified() = %d after an add, want %d still", got, notModified+1)
	}
	if _, resp, err := c.Client.Reactions.ListIssueReactions(context.Background(), "o", "r", 9, nil); err == nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("reactions to a missing issue: %v, want a 404", err)
	}
}

// A blocked-by listing carries each blocker's state as it is now: closing
// a blocker changes the listing and its ETag, so the next read is a 200
// with the blocker closed, and the read after it a 304. Reopening it does
// the same.
func TestBlockedByLiveState(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	blocked := f.AddIssue("owner", "Blocked", "body")
	blocker := f.AddIssue("owner", "Blocker", "body")
	f.SetBlockedBy(blocked, blocker)
	ctx, c := context.Background(), f.Client(t)
	var etags []string
	read := func(name, wantState string, wantNotModified int) {
		t.Helper()
		before := f.NotModified()
		is, resp, err := c.Issues.ListBlockedBy(ctx, "o", "r", int64(blocked), nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := []string{}
		for _, b := range is {
			got = append(got, fmt.Sprintf("#%d %s", b.GetNumber(), b.GetState()))
		}
		if want := []string{fmt.Sprintf("#%d %s", blocker, wantState)}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: blockers = %q, want %q", name, got, want)
		}
		if got := f.NotModified() - before; got != wantNotModified {
			t.Errorf("%s: %d 304s, want %d", name, got, wantNotModified)
		}
		etags = append(etags, resp.Header.Get("ETag"))
	}
	read("first", "open", 0)
	read("unchanged", "open", 1)
	f.Close(blocker)
	read("after the close", "closed", 0)
	read("unchanged after the close", "closed", 1)
	f.Reopen(blocker)
	read("after the reopen", "open", 0)
	if etags[0] != etags[1] || etags[1] == etags[2] || etags[2] != etags[3] || etags[3] == etags[4] {
		t.Errorf("ETags = %q, want a new one after each change only", etags)
	}
}

// fakeReactionList reads a reaction listing with opts, as both of go-github's
// reaction listings do.
type fakeReactionList func(opts *gogithub.ListReactionOptions) ([]*gogithub.Reaction, *gogithub.Response, error)

// fakeReactionRead lists the reactions read serves for content, two to a
// page, every page, as "login content".
func fakeReactionRead(t *testing.T, read fakeReactionList, content string) []string {
	t.Helper()
	got := []string{}
	opts := &gogithub.ListReactionOptions{Content: content, ListOptions: gogithub.ListOptions{PerPage: 2}}
	for {
		rs, resp, err := read(opts)
		if err != nil {
			t.Fatalf("content %q: %v", content, err)
		}
		for _, r := range rs {
			got = append(got, r.GetUser().GetLogin()+" "+r.GetContent())
		}
		if resp.NextPage == 0 {
			return got
		}
		opts.Page = resp.NextPage
	}
}

// Both reaction listings, an issue's and a comment's, serve only the
// reactions with the content= asked for, such as +1, oldest first and
// across pages, and every reaction for none. A content nothing has is an
// empty list.
func TestFakeReactionContent(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	cm := f.AddComment(n, "owner", "hi")
	for _, r := range []struct{ author, content string }{
		{"owner", "+1"}, {"mallory", "eyes"}, {"issuedog[bot]", "+1"}, {"mallory", "-1"}, {"mallory", "+1"},
	} {
		f.AddIssueReaction(n, r.author, r.content)
		f.AddReaction(cm, r.author, r.content)
	}
	ctx, c := context.Background(), f.Client(t)
	for _, tc := range []struct {
		name string
		read fakeReactionList
	}{
		{"the issue's", func(opts *gogithub.ListReactionOptions) ([]*gogithub.Reaction, *gogithub.Response, error) {
			return c.Client.Reactions.ListIssueReactions(ctx, "o", "r", n, opts)
		}},
		{"the comment's", func(opts *gogithub.ListReactionOptions) ([]*gogithub.Reaction, *gogithub.Response, error) {
			return c.Client.Reactions.ListIssueCommentReactions(ctx, "o", "r", cm, opts)
		}},
	} {
		for content, want := range map[string][]string{
			"+1":    {"owner +1", "issuedog[bot] +1", "mallory +1"},
			"-1":    {"mallory -1"},
			"heart": {},
			"":      {"owner +1", "mallory eyes", "issuedog[bot] +1", "mallory -1", "mallory +1"},
		} {
			if got := fakeReactionRead(t, tc.read, content); !reflect.DeepEqual(got, want) {
				t.Errorf("%s reactions with content %q = %q, want %q", tc.name, content, got, want)
			}
		}
	}
}
