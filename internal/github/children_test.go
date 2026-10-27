package github_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// is422 reports whether err wraps GitHub's 422.
func is422(err error) bool {
	var er *gogithub.ErrorResponse
	return errors.As(err, &er) && er.Response.StatusCode == http.StatusUnprocessableEntity
}

// CreateChild files the App's issue with its title, body and labels, and on
// a repo with issue types its type, in one request, and the timeline shows
// each as the App's change. With no type, it sends none.
func TestCreateChild(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("owner", "Parent", "")
	f.IssueTypeID("Task")
	ctx, c := context.Background(), f.Client(t)
	is, err := c.CreateChild(ctx, "o", "r", github.NewChild{Title: "T1. Child", Body: "the body",
		Labels: []string{"stage:ready", "scope:github"}, Type: "Task"})
	if err != nil {
		t.Fatal(err)
	}
	if is.GetNumber() != 2 || is.GetID() == 0 || is.GetID() == 2 {
		t.Errorf("issue #%d with ID %d, want #2 with an ID of its own", is.GetNumber(), is.GetID())
	}
	st := f.State().Issues[1]
	if st.Author != "issuedog[bot]" || st.Title != "T1. Child" || st.Body != "the body" || st.Type != "Task" ||
		!reflect.DeepEqual(st.Labels, []string{"scope:github", "stage:ready"}) {
		t.Errorf("issue = %+v, want the App's, with its title, body, labels and type", st)
	}
	timeline, err := c.TimelineSince(ctx, "o", "r", 2, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	for _, e := range timeline {
		events = append(events, fmt.Sprintf("%s %s %s", e.GetEvent(), e.GetLabel().GetName(), e.GetActor().GetLogin()))
	}
	if want := []string{"labeled stage:ready issuedog[bot]", "labeled scope:github issuedog[bot]", "issue_type_added  issuedog[bot]"}; !reflect.DeepEqual(events, want) {
		t.Errorf("events = %q, want %q", events, want)
	}
	if w := f.Writes(); len(w) != 1 || w[0].Path != "/issues" || !strings.Contains(w[0].Body, `"type":"Task"`) {
		t.Errorf("writes = %+v, want the one create, with the type", w)
	}
	// With no type and no labels, the request names neither.
	is, err = c.CreateChild(ctx, "o", "r", github.NewChild{Title: "Plain", Body: "b"})
	if err != nil || is.GetNumber() != 3 || is.Type != nil {
		t.Fatalf("plain child = #%d of type %v, %v; want #3 with none", is.GetNumber(), is.GetType(), err)
	}
	if w := f.Writes(); len(w) != 2 || strings.Contains(w[1].Body, `"type"`) || strings.Contains(w[1].Body, `"labels"`) {
		t.Errorf("writes = %+v, want the second without a type or labels", w)
	}
}

// A create GitHub refuses, such as for a type the org hasn't got, is an
// error naming the title and repo, wrapping GitHub's 422.
func TestCreateChildRefused(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	_, err := f.Client(t).CreateChild(context.Background(), "o", "r", github.NewChild{Title: "Child", Type: "Saga"})
	if !is422(err) || !strings.Contains(err.Error(), `create issue "Child" in o/r: `) {
		t.Errorf("err = %v, want a wrapped 422 naming the title and repo", err)
	}
	if n := len(f.State().Issues); n != 0 {
		t.Errorf("%d issues, want none", n)
	}
}

// A snapshot gives the parent's SubIssueRemovedEvents, oldest first, each
// with who removed which sub-issue's link, in which repo, one in another
// repo included, and none of its other events: a comment deletion read in
// the same query is a deletion alone.
func TestSubIssueRemovals(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("owner", "Parent", "")
	for range 2 {
		f.SetParent(f.AddIssue("owner", "Child", ""), 1)
	}
	f.LinkSubIssue(1, "o/other", 3)
	f.AddLabel(1, "owner", "priority:p1")
	first := f.UnlinkBy(3, "mallory")
	deleted := f.DeleteCommentBy("owner", f.AddComment(1, "mallory", "hi"))
	second := f.UnlinkBy(2, "owner")
	other := f.UnlinkLinkedBy(1, "o/other", 3, "mallory")
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	var removals []string
	for _, r := range s.SubIssueRemovals {
		removals = append(removals, fmt.Sprintf("%s %s#%d %s", r.ID, r.Repo, r.Number, r.By.Login))
	}
	want := []string{first + " o/r#3 mallory", second + " o/r#2 owner", other + " o/other#3 mallory"}
	if !slices.Equal(removals, want) {
		t.Errorf("removals = %q, want %q", removals, want)
	}
	if len(s.Deleted) != 1 || s.Deleted[0].ID != deleted {
		t.Errorf("deleted = %+v, want only %s", s.Deleted, deleted)
	}
	if p := f.State().Issues[1].Parent; p != 0 {
		t.Errorf("#2's parent is #%d; want none once unlinked", p)
	}
	if subs := f.State().Issues[0].SubIssues; len(subs) != 0 {
		t.Errorf("the parent's sub-issues are %v; want none once unlinked", subs)
	}
}

// A SubIssueRemovedEvent the fake writes late (LagEvents) isn't in a
// snapshot read before the clock reaches it, and is in one read once it
// has, as GitHub may write the event a while after the removal.
func TestSubIssueRemovalLate(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("owner", "Parent", "")
	f.SetParent(f.AddIssue("owner", "Child", ""), 1)
	f.LagEvents(5 * time.Second)
	id := f.UnlinkBy(2, "mallory")
	f.LagEvents(0)
	removals := func() []string {
		t.Helper()
		s, err := f.Client(t).Snapshot(context.Background(), "o", "r", 1)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, r := range s.SubIssueRemovals {
			ids = append(ids, r.ID)
		}
		return ids
	}
	if got := removals(); len(got) != 0 {
		t.Errorf("removals before the event is written = %q; want none", got)
	}
	f.Advance(4 * time.Second)
	if got := removals(); len(got) != 0 {
		t.Errorf("removals a second before the event is written = %q; want none", got)
	}
	f.Advance(time.Second)
	if got := removals(); !slices.Equal(got, []string{id}) {
		t.Errorf("removals once the event is written = %q; want only %s", got, id)
	}
}

// AddSubIssue links a child by its ID after the parent's other sub-issues,
// and SubIssues lists them in that order, with their bodies and authors.
// Linking one that has a parent already is a wrapped 422, and so is a
// child ID in place of the number GitHub's path wants.
func TestAddSubIssue(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	parent := f.AddIssue("owner", "Parent", "")
	first := f.AddIssue("owner", "First", "")
	f.SetParent(first, parent)
	ctx, c := context.Background(), f.Client(t)
	var ids []int64
	for _, title := range []string{"A", "B"} {
		is, err := c.CreateChild(ctx, "o", "r", github.NewChild{Title: title, Body: title + " body"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, is.GetID())
	}
	for _, id := range []int64{ids[1], ids[0]} {
		if err := c.AddSubIssue(ctx, "o", "r", parent, id); err != nil {
			t.Fatal(err)
		}
	}
	subs, err := c.SubIssues(ctx, "o", "r", parent)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range subs {
		got = append(got, fmt.Sprintf("#%d %s %q by %s", s.GetNumber(), s.GetTitle(), s.GetBody(), s.GetUser().GetLogin()))
	}
	want := []string{`#2 First "" by owner`, `#4 B "B body" by issuedog[bot]`, `#3 A "A body" by issuedog[bot]`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sub-issues = %q, want %q", got, want)
	}
	if got := f.State().Issues[parent-1].SubIssues; !reflect.DeepEqual(got, []int{2, 4, 3}) {
		t.Errorf("State sub-issues = %v, want [2 4 3]", got)
	}
	err = c.AddSubIssue(ctx, "o", "r", parent, ids[0])
	if !is422(err) || !strings.Contains(err.Error(), fmt.Sprintf("link issue ID %d under o/r#1: ", ids[0])) {
		t.Errorf("a second link: err = %v, want a wrapped 422 naming the ID and the parent", err)
	}
	if err := c.AddSubIssue(ctx, "o", "r", 9, ids[0]); err == nil || !strings.Contains(err.Error(), "under o/r#9: ") {
		t.Errorf("a link under a missing issue: err = %v, want one naming o/r#9", err)
	}
}

// SubIssues and BlockedBy read open and closed issues across pages of 100,
// and a repeat of an unchanged listing is a 304 for each page. A missing
// issue's listing is an error naming it.
func TestSubIssuesAndBlockedByPages(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	top := f.AddIssue("owner", "Top", "")
	const n = 103
	for i := range n {
		sub := f.AddIssue("owner", fmt.Sprintf("Sub %d", i), "")
		f.SetParent(sub, top)
		f.SetBlockedBy(top, sub)
		if i%2 == 0 {
			f.Close(sub)
		}
	}
	ctx, c := context.Background(), f.Client(t)
	for _, tc := range []struct {
		name string
		list func() ([]*gogithub.Issue, error)
	}{
		{"sub-issues", func() ([]*gogithub.Issue, error) { return c.SubIssues(ctx, "o", "r", top) }},
		{"blocked-by", func() ([]*gogithub.Issue, error) { return c.BlockedBy(ctx, "o", "r", top) }},
	} {
		got, err := tc.list()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		closed := 0
		for i, is := range got {
			if is.GetNumber() != i+2 {
				t.Errorf("%s: item %d is #%d, want #%d", tc.name, i, is.GetNumber(), i+2)
				break
			}
			if is.GetState() == "closed" {
				closed++
			}
		}
		if len(got) != n || closed != (n+1)/2 {
			t.Errorf("%s: %d issues, %d closed; want %d, %d closed", tc.name, len(got), closed, n, (n+1)/2)
		}
		before := f.NotModified()
		if again, err := tc.list(); err != nil || len(again) != n {
			t.Errorf("%s again: %d issues, %v; want %d", tc.name, len(again), err, n)
		}
		if got := f.NotModified() - before; got != 2 {
			t.Errorf("%s again: %d 304s, want 2, one for each page", tc.name, got)
		}
	}
	if _, err := c.SubIssues(ctx, "o", "r", 999); err == nil || !strings.Contains(err.Error(), "list sub-issues of o/r#999: ") {
		t.Errorf("sub-issues of a missing issue: err = %v, want one naming o/r#999", err)
	}
	if _, err := c.BlockedBy(ctx, "o", "r", 999); err == nil || !strings.Contains(err.Error(), "list issues blocking o/r#999: ") {
		t.Errorf("blocked-by of a missing issue: err = %v, want one naming o/r#999", err)
	}
}

// AddBlockedBy adds a link by the blocker's ID once, and BlockedBy lists it.
// A second identical add is a wrapped 422 naming the ID and the issue.
func TestAddBlockedBy(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	blocked := f.AddIssue("owner", "Blocked", "")
	ctx, c := context.Background(), f.Client(t)
	blocker, err := c.CreateChild(ctx, "o", "r", github.NewChild{Title: "Blocker"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AddBlockedBy(ctx, "o", "r", blocked, blocker.GetID()); err != nil {
		t.Fatal(err)
	}
	got, err := c.BlockedBy(ctx, "o", "r", blocked)
	if err != nil || len(got) != 1 || got[0].GetNumber() != blocker.GetNumber() || got[0].GetState() != "open" {
		t.Errorf("blocked-by = %v, %v; want the blocker, open", got, err)
	}
	err = c.AddBlockedBy(ctx, "o", "r", blocked, blocker.GetID())
	if !is422(err) || !strings.Contains(err.Error(), fmt.Sprintf("add issue ID %d as blocking o/r#1: ", blocker.GetID())) {
		t.Errorf("a second add: err = %v, want a wrapped 422 naming the ID and the issue", err)
	}
	if got := f.State().Issues[blocked-1].BlockedBy; !reflect.DeepEqual(got, []int{2}) {
		t.Errorf("State blocked-by = %v, want [2]", got)
	}
}

// childMarker marks the child childrenPass creates for item id.
func childMarker(id string) string { return "<!-- issuedog:origin plan=#1 rev=1 item=" + id + " -->" }

// childrenPass creates the parent #1's children T1 and T2, typed Task on a
// repo with issue types, links each under #1 in that order, and makes T2
// blocked by T1, reading before each write: the App's issues with their
// markers, #1's sub-issues, and T2's blockers. It's the order of writes
// creating the children makes (SPEC §5.2), cut down to this package's calls.
// writes limits it to those writes, as "create", "link" and "block".
func childrenPass(t *testing.T, typed bool, writes ...string) func(*githubtest.Fake) error {
	does := map[string]bool{}
	for _, w := range writes {
		does[w] = true
	}
	return func(f *githubtest.Fake) error {
		ctx, c := context.Background(), f.Client(t)
		all, err := c.IssuesSince(ctx, "o", "r", githubtest.Start.AddDate(-1, 0, 0))
		if err != nil {
			return err
		}
		children := map[string]*gogithub.Issue{}
		for _, is := range all {
			for _, id := range []string{"T1", "T2"} {
				if is.GetUser().GetLogin() == "issuedog[bot]" && strings.HasSuffix(is.GetBody(), childMarker(id)) {
					children[id] = is
				}
			}
		}
		for _, id := range []string{"T1", "T2"} {
			if children[id] != nil || !does["create"] {
				continue
			}
			child := github.NewChild{Title: id + ". A task", Body: "The task.\n\n" + childMarker(id), Labels: []string{"stage:ready", "priority:p1"}}
			if typed {
				child.Type = "Task"
			}
			if children[id], err = c.CreateChild(ctx, "o", "r", child); err != nil {
				return err
			}
		}
		if does["link"] {
			subs, err := c.SubIssues(ctx, "o", "r", 1)
			if err != nil {
				return err
			}
			linked := map[int]bool{}
			for _, s := range subs {
				linked[s.GetNumber()] = true
			}
			for _, id := range []string{"T1", "T2"} {
				if is := children[id]; is != nil && !linked[is.GetNumber()] {
					if err := c.AddSubIssue(ctx, "o", "r", 1, is.GetID()); err != nil {
						return err
					}
				}
			}
		}
		if t1, t2 := children["T1"], children["T2"]; does["block"] && t1 != nil && t2 != nil {
			blockers, err := c.BlockedBy(ctx, "o", "r", t2.GetNumber())
			if err != nil {
				return err
			}
			for _, b := range blockers {
				if b.GetNumber() == t1.GetNumber() {
					return nil
				}
			}
			return c.AddBlockedBy(ctx, "o", "r", t2.GetNumber(), t1.GetID())
		}
		return nil
	}
}

// childrenSetup is a repo with the parent #1, whose org has the Task type
// if typed. For the link and block writes, the children are filed already,
// as the App, so those are the only writes.
func childrenSetup(t *testing.T, typed bool, filed bool) func(testing.TB) *githubtest.Fake {
	return func(tb testing.TB) *githubtest.Fake {
		f := githubtest.New(tb, "o/r", "issuedog")
		f.AddIssue("owner", "Parent", "")
		if typed {
			f.IssueTypeID("Task")
		}
		if filed {
			if err := childrenPass(t, typed, "create")(f); err != nil {
				tb.Fatal(err)
			}
			f.ForgetWrites()
		}
		return f
	}
}

// Replay passes for each write, by a pass that reads before it writes:
// CreateChild, with labels and on a typed repo its type; AddSubIssue; and
// AddBlockedBy; then all three in one pass.
func TestChildrenReplay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		typed, filed  bool
		writes        []string
		wantWrites    int
		wantSubIssues []int
	}{
		{"create", false, false, []string{"create"}, 2, nil},
		{"create typed", true, false, []string{"create"}, 2, nil},
		{"link", false, true, []string{"link"}, 2, []int{2, 3}},
		{"block", false, true, []string{"block"}, 1, nil},
		{"all, typed", true, false, []string{"create", "link", "block"}, 5, []int{2, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			setup, pass := childrenSetup(t, tc.typed, tc.filed), childrenPass(t, tc.typed, tc.writes...)
			clean := setup(t)
			if err := pass(clean); err != nil {
				t.Fatal(err)
			}
			if w := clean.Writes(); len(w) != tc.wantWrites {
				t.Fatalf("clean run writes = %+v, want %d", w, tc.wantWrites)
			}
			s := clean.State()
			if len(s.Issues) != 3 || !reflect.DeepEqual(s.Issues[0].SubIssues, tc.wantSubIssues) {
				t.Fatalf("clean run state = %+v, want 3 issues, #1's sub-issues %v", s, tc.wantSubIssues)
			}
			if wantType := map[bool]string{true: "Task"}[tc.typed]; s.Issues[1].Type != wantType || s.Issues[2].Type != wantType {
				t.Errorf("children's types %q and %q, want %q", s.Issues[1].Type, s.Issues[2].Type, wantType)
			}
			wantBlocked := map[bool][]int{true: {2}}[slices.Contains(tc.writes, "block")]
			if got := s.Issues[2].BlockedBy; !reflect.DeepEqual(got, wantBlocked) {
				t.Errorf("#3 blocked by %v, want %v", got, wantBlocked)
			}
			githubtest.Replay(t, setup, pass)
		})
	}
}

// is404 reports whether err wraps GitHub's 404.
func is404(err error) bool {
	var er *gogithub.ErrorResponse
	return errors.As(err, &er) && er.Response.StatusCode == http.StatusNotFound
}

// Blocking reads the issues an issue blocks, open and closed, across pages
// of 100, and a repeat of an unchanged listing is a 304 for each page. A
// missing issue's listing is an error naming it.
func TestBlockingPages(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	top := f.AddIssue("owner", "Top", "")
	const n = 103
	for i := range n {
		dep := f.AddIssue("owner", fmt.Sprintf("Blocked %d", i), "")
		f.SetBlockedBy(dep, top)
		if i%2 == 0 {
			f.Close(dep)
		}
	}
	ctx, c := context.Background(), f.Client(t)
	got, err := c.Blocking(ctx, "o", "r", top)
	if err != nil {
		t.Fatal(err)
	}
	closed := 0
	for i, is := range got {
		if is.GetNumber() != i+2 {
			t.Errorf("item %d is #%d, want #%d", i, is.GetNumber(), i+2)
			break
		}
		if is.GetState() == "closed" {
			closed++
		}
	}
	if len(got) != n || closed != (n+1)/2 {
		t.Errorf("%d issues, %d closed; want %d, %d closed", len(got), closed, n, (n+1)/2)
	}
	before := f.NotModified()
	if again, err := c.Blocking(ctx, "o", "r", top); err != nil || len(again) != n {
		t.Errorf("again: %d issues, %v; want %d", len(again), err, n)
	}
	if got := f.NotModified() - before; got != 2 {
		t.Errorf("again: %d 304s, want 2, one for each page", got)
	}
	if _, err := c.Blocking(ctx, "o", "r", 999); err == nil || !strings.Contains(err.Error(), "list issues blocked by o/r#999: ") {
		t.Errorf("blocking of a missing issue: err = %v, want one naming o/r#999", err)
	}
}

// RemoveBlockedBy removes a link by the blocker's ID, and both BlockedBy and
// Blocking show it gone, while the issue's other links stay. A second
// removal of the same link is a wrapped 404 naming the ID and the issue.
func TestRemoveBlockedBy(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	blocked := f.AddIssue("owner", "Blocked", "")
	blocker := f.AddIssue("owner", "Blocker", "")
	other := f.AddIssue("owner", "Other blocker", "")
	f.SetBlockedBy(blocked, blocker)
	f.SetBlockedBy(blocked, other)
	ctx, c := context.Background(), f.Client(t)
	before, err := c.Blocking(ctx, "o", "r", blocker)
	if err != nil || len(before) != 1 || before[0].GetNumber() != blocked {
		t.Fatalf("blocking before = %v, %v; want #%d", before, err, blocked)
	}
	blockers, err := c.BlockedBy(ctx, "o", "r", blocked)
	if err != nil || len(blockers) != 2 || blockers[0].GetNumber() != blocker {
		t.Fatalf("blocked-by before = %v, %v; want #%d first of 2", blockers, err, blocker)
	}
	if err := c.RemoveBlockedBy(ctx, "o", "r", blocked, blockers[0].GetID()); err != nil {
		t.Fatal(err)
	}
	if got, err := c.BlockedBy(ctx, "o", "r", blocked); err != nil || len(got) != 1 || got[0].GetNumber() != other {
		t.Errorf("blocked-by after = %v, %v; want only #%d", got, err, other)
	}
	if got, err := c.Blocking(ctx, "o", "r", blocker); err != nil || len(got) != 0 {
		t.Errorf("blocking after = %v, %v; want none", got, err)
	}
	if got, err := c.Blocking(ctx, "o", "r", other); err != nil || len(got) != 1 || got[0].GetNumber() != blocked {
		t.Errorf("blocking by the other blocker = %v, %v; want #%d", got, err, blocked)
	}
	err = c.RemoveBlockedBy(ctx, "o", "r", blocked, blockers[0].GetID())
	if !is404(err) || !strings.Contains(err.Error(), fmt.Sprintf("remove issue ID %d as blocking o/r#1: ", blockers[0].GetID())) {
		t.Errorf("a second removal: err = %v, want a wrapped 404 naming the ID and the issue", err)
	}
	if got := f.State().Issues[blocked-1].BlockedBy; !reflect.DeepEqual(got, []int{other}) {
		t.Errorf("State blocked-by = %v, want [%d]", got, other)
	}
}

// unlinkPass removes every dependency link to or from issue dropped, reading
// before each write, as a revision does for a dropped child (SPEC §5.2): the
// issues it blocks (Blocking), then the issues blocking it (BlockedBy).
func unlinkPass(t *testing.T, dropped int) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		ctx, c := context.Background(), f.Client(t)
		is, _, err := c.Issues.Get(ctx, "o", "r", dropped)
		if err != nil {
			return err
		}
		blocked, err := c.Blocking(ctx, "o", "r", dropped)
		if err != nil {
			return err
		}
		for _, b := range blocked {
			if err := c.RemoveBlockedBy(ctx, "o", "r", b.GetNumber(), is.GetID()); err != nil {
				return err
			}
		}
		blockers, err := c.BlockedBy(ctx, "o", "r", dropped)
		if err != nil {
			return err
		}
		for _, b := range blockers {
			if err := c.RemoveBlockedBy(ctx, "o", "r", dropped, b.GetID()); err != nil {
				return err
			}
		}
		return nil
	}
}

// Replay passes for a pass that reads before it removes: a crash at any
// removal leaves the rest to the next pass, which removes nothing twice. The
// dropped #2 blocks #1 and #3 and is blocked by #4, and #4's link to #1
// stays. A second pass over the clean result writes nothing.
func TestRemoveBlockedByReplay(t *testing.T) {
	t.Parallel()
	setup := func(tb testing.TB) *githubtest.Fake {
		f := githubtest.New(tb, "o/r", "issuedog")
		for _, title := range []string{"Kept", "Dropped", "Kept too", "Outside"} {
			f.AddIssue("owner", title, "")
		}
		f.SetBlockedBy(1, 2)
		f.SetBlockedBy(1, 4)
		f.SetBlockedBy(3, 2)
		f.SetBlockedBy(2, 4)
		return f
	}
	pass := unlinkPass(t, 2)
	clean := setup(t)
	if err := pass(clean); err != nil {
		t.Fatal(err)
	}
	if w := clean.Writes(); len(w) != 3 {
		t.Fatalf("clean run writes = %+v, want 3", w)
	}
	s := clean.State()
	for i, want := range [][]int{{4}, nil, nil, nil} {
		if got := s.Issues[i].BlockedBy; !reflect.DeepEqual(got, want) {
			t.Errorf("#%d blocked by %v, want %v", i+1, got, want)
		}
	}
	clean.ForgetWrites()
	if err := pass(clean); err != nil {
		t.Fatal(err)
	}
	if w := clean.Writes(); len(w) != 0 {
		t.Errorf("a second pass writes %+v, want none", w)
	}
	githubtest.Replay(t, setup, pass)
}
