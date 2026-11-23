package githubtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/issuedog/internal/github"
)

// reviewTimeline is pull request n's timeline as the fake serves it, each
// event as raw JSON fields, in the order served.
func reviewTimeline(t *testing.T, f *Fake, n int) []map[string]json.RawMessage {
	t.Helper()
	var events []map[string]json.RawMessage
	if status, msg := pullDo(t, f, "GET", fmt.Sprintf("repos/o/r/issues/%d/timeline", n), nil, &events); status != http.StatusOK {
		t.Fatalf("timeline of #%d: %d %s", n, status, msg)
	}
	return events
}

// reviewCommentsAt lists the comments at query on review id of pull
// request n, as "<id> <user> <path>:<line>/<original_line> <subject_type>
// <review id> <body>", in the order served, with the status of a refusal.
func reviewCommentsAt(t *testing.T, f *Fake, n int, id int64, query string) ([]string, int) {
	t.Helper()
	var comments []*gogithub.PullRequestComment
	status, _ := pullDo(t, f, "GET", fmt.Sprintf("repos/o/r/pulls/%d/reviews/%d/comments?%s", n, id, query), nil, &comments)
	var got []string
	for _, cm := range comments {
		line, original := "-", "-"
		if cm.Line != nil {
			line = fmt.Sprint(*cm.Line)
		}
		if cm.OriginalLine != nil {
			original = fmt.Sprint(*cm.OriginalLine)
		}
		got = append(got, fmt.Sprintf("%d %s %s:%s/%s %s %d %s", cm.GetID(), cm.GetUser().GetLogin(), cm.GetPath(), line, original,
			cm.GetSubjectType(), cm.GetPullRequestReviewID(), cm.GetBody()))
	}
	return got, status
}

// A review is served in its pull request's timeline as GitHub serves a
// reviewed event: with the review's ID, the reviewer as its user, its
// state lower case, its body and its submitted_at, and no actor or
// created_at, in time order among the other events. It moves the pull
// request's updated_at to its submitted_at.
func TestReviewPRTimeline(t *testing.T) {
	t.Parallel()
	f, _ := pullFake(t, "a")
	n := pullOpen(t, f, "A", "a", "main", "")
	f.AddComment(n, "owner", "before")
	id := f.ReviewPR(n, "owner", github.ReviewChangesRequested, "Fix <this>.", nil)
	f.AddComment(n, "owner", "after")
	approved := f.ReviewPR(n, "mallory", github.ReviewApproved, "", nil)
	if id == 0 || approved <= id {
		t.Errorf("review IDs %d, then %d; want IDs that grow", id, approved)
	}
	events := reviewTimeline(t, f, n)
	var got []string
	for _, e := range events {
		var event string
		if err := json.Unmarshal(e["event"], &event); err != nil {
			t.Fatal(err)
		}
		got = append(got, event)
	}
	if want := []string{"commented", "reviewed", "commented", "reviewed"}; !slices.Equal(got, want) {
		t.Fatalf("timeline %q; want %q", got, want)
	}
	var r struct {
		ID          int64
		User        *gogithub.User
		State, Body string
		SubmittedAt gogithub.Timestamp
	}
	if err := json.Unmarshal(events[1]["id"], &r.ID); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(events[1]["user"], &r.User); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(events[1]["state"], &r.State); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(events[1]["body"], &r.Body); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(events[1]["submitted_at"], &r.SubmittedAt); err != nil {
		t.Fatal(err)
	}
	if r.ID != id || r.User.GetLogin() != "owner" || r.User.GetType() != "User" || r.State != "changes_requested" || r.Body != "Fix <this>." ||
		r.SubmittedAt.IsZero() {
		t.Errorf("reviewed event %+v by %s; want #%d by owner, changes_requested, its body and a submitted_at", r, r.User.GetLogin(), id)
	}
	for i, e := range events {
		if _, ok := e["actor"]; ok && i%2 == 1 {
			t.Errorf("reviewed event %d has an actor %s; want none, as GitHub's", i, e["actor"])
		}
		if _, ok := e["created_at"]; ok && i%2 == 1 {
			t.Errorf("reviewed event %d has a created_at %s; want none, as GitHub's", i, e["created_at"])
		}
	}
	var issues []*gogithub.Issue
	pullDo(t, f, "GET", "repos/o/r/issues?state=all", nil, &issues)
	var last gogithub.Timestamp
	if err := json.Unmarshal(events[3]["submitted_at"], &last); err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || !issues[0].GetUpdatedAt().Time.Equal(last.Time) {
		t.Errorf("listed %+v; want the pull request, updated at the last review's submitted_at, %s", issues, last)
	}
}

// A review's line comments are served oldest first, paged, each with its
// ID, the reviewer, its file, its line as both line and original_line,
// or neither for a comment on the whole file, and the review's ID. A
// review the pull request hasn't got, another's included, and an issue the
// fake didn't open as a pull request are a 404.
func TestReviewPRComments(t *testing.T) {
	t.Parallel()
	f, _ := pullFake(t, "a", "b")
	n := pullOpen(t, f, "A", "a", "main", "")
	other := pullOpen(t, f, "B", "b", "main", "")
	marked := f.AddIssue("owner", "Marked", "")
	f.SetPullRequest(marked)
	var lines []ReviewLine
	for i := range 31 {
		lines = append(lines, ReviewLine{Path: fmt.Sprintf("f%d.go", i), Line: i, Body: fmt.Sprintf("c%d", i)})
	}
	id := f.ReviewPR(n, "owner", github.ReviewCommented, "See the lines.", lines)
	none := f.ReviewPR(n, "owner", github.ReviewApproved, "", nil)
	theirs := f.ReviewPR(other, "owner", github.ReviewCommented, "", []ReviewLine{{Path: "b.go", Line: 1, Body: "theirs"}})
	first, status := reviewCommentsAt(t, f, n, id, "")
	if status != http.StatusOK || len(first) != 30 {
		t.Fatalf("page 1: %d, %d comments; want 200 and GitHub's default 30", status, len(first))
	}
	second, _ := reviewCommentsAt(t, f, n, id, "page=2")
	all := append(first, second...)
	if len(all) != 31 {
		t.Fatalf("listed %d over two pages; want 31", len(all))
	}
	if got, want := strings.SplitN(all[0], " ", 2)[1], fmt.Sprintf("owner f0.go:-/- file %d c0", id); got != want {
		t.Errorf("first comment %q; want %q, a comment on the whole file", got, want)
	}
	if got, want := strings.SplitN(all[30], " ", 2)[1], fmt.Sprintf("owner f30.go:30/30 line %d c30", id); got != want {
		t.Errorf("last comment %q; want %q", got, want)
	}
	for i := 1; i < len(all); i++ {
		var prev, cur int64
		if _, err := fmt.Sscan(all[i-1], &prev); err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Sscan(all[i], &cur); err != nil {
			t.Fatal(err)
		}
		if cur <= prev {
			t.Errorf("comment %d has ID %d after %d; want oldest first, each with an ID of its own", i, cur, prev)
		}
	}
	c := f.Client(t)
	req, err := c.NewRequest(t.Context(), "GET", fmt.Sprintf("repos/o/r/pulls/%d/reviews/%d/comments?per_page=30", n, id), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.NextPage != 2 {
		t.Errorf("first page links page %d next; want 2", resp.NextPage)
	}
	if got, status := reviewCommentsAt(t, f, n, none, ""); status != http.StatusOK || len(got) != 0 {
		t.Errorf("a review with no comments: %d %q; want 200 and none", status, got)
	}
	for _, tc := range []struct {
		name string
		n    int
		id   int64
	}{
		{"another's review", n, theirs},
		{"a review never made", n, theirs + 100},
		{"an issue the fake didn't open", marked, id},
		{"no such issue", 999, id},
	} {
		if got, status := reviewCommentsAt(t, f, tc.n, tc.id, ""); status != http.StatusNotFound {
			t.Errorf("%s: %d %q; want 404", tc.name, status, got)
		}
	}
	if status, _ := pullDo(t, f, "GET", fmt.Sprintf("repos/o/r/pulls/%d/reviews/x/comments", n), nil, nil); status != http.StatusNotFound {
		t.Errorf("a review ID that isn't a number: %d; want 404", status)
	}
}

// ReviewPR fails the test, naming why, for an issue the fake didn't open
// as a pull request, a closed pull request, an issue it hasn't got and a
// state the reviewed event doesn't give, its upper-case form included.
func TestReviewPRFails(t *testing.T) {
	t.Parallel()
	tb := &fakeNumberTB{TB: t}
	f, _ := pullFake(tb, "a", "b")
	marked := f.AddIssue("owner", "Marked", "")
	f.SetPullRequest(marked)
	n := pullOpen(t, f, "A", "a", "main", "")
	closed := pullOpen(t, f, "B", "b", "main", "")
	f.ClosePR(closed, "owner")
	for _, tc := range []struct {
		name string
		do   func()
		want string
	}{
		{"marked", func() { f.ReviewPR(marked, "owner", github.ReviewCommented, "", nil) }, fmt.Sprintf("githubtest: #%d is no pull request the fake opened", marked)},
		{"closed", func() { f.ReviewPR(closed, "owner", github.ReviewCommented, "", nil) },
			fmt.Sprintf("githubtest: review #%d: it's closed, and only an open pull request is reviewed here", closed)},
		{"no such issue", func() { f.ReviewPR(999, "owner", github.ReviewCommented, "", nil) }, "githubtest: no issue #999"},
		{"upper case", func() { f.ReviewPR(n, "owner", "APPROVED", "", nil) }, fmt.Sprintf(`githubtest: review #%d: state "APPROVED" is none of`, n)},
		{"pending", func() { f.ReviewPR(n, "owner", "pending", "", nil) }, fmt.Sprintf(`githubtest: review #%d: state "pending" is none of`, n)},
	} {
		fatal, panicked := fakeNumberFatal(tb, tc.do)
		if panicked != nil || len(fatal) != 1 || !strings.HasPrefix(fatal[0], tc.want) {
			t.Errorf("%s: Fatalf %q, panicked with %v; want one Fatalf starting %q", tc.name, fatal, panicked, tc.want)
		}
	}
	if got := reviewTimeline(t, f, n); len(got) != 0 {
		t.Errorf("timeline of #%d after refused reviews: %d events; want none", n, len(got))
	}
}
