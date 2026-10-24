package githubtest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/issuedog/internal/github"
)

func TestRepeatedGETIsNotModified(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	c := f.Client(t)
	get := func() string {
		t.Helper()
		is, _, err := c.Issues.Get(context.Background(), "o", "r", n)
		if err != nil {
			t.Fatal(err)
		}
		return is.GetBody()
	}
	for range 2 {
		if got := get(); got != "body" {
			t.Fatalf("body = %q, want body", got)
		}
	}
	if got := f.NotModified(); got != 1 {
		t.Errorf("NotModified() = %d after a repeated GET, want 1", got)
	}
	f.AddComment(n, "owner", "a change")
	get()
	if got := f.NotModified(); got != 1 {
		t.Errorf("NotModified() = %d after a GET of a changed issue, want still 1", got)
	}
}

func TestFailWrite(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	c := f.Client(t)
	f.FailWrite(2)
	var statuses []int
	for _, l := range []string{"a", "b", "c"} {
		_, resp, err := c.Issues.AddLabelsToIssue(context.Background(), "o", "r", n, []string{l})
		var er *gogithub.ErrorResponse
		if err != nil && !errors.As(err, &er) {
			t.Fatal(err)
		}
		statuses = append(statuses, resp.StatusCode)
	}
	if want := []int{http.StatusOK, http.StatusInternalServerError, http.StatusOK}; !slices.Equal(statuses, want) {
		t.Errorf("statuses = %v, want %v", statuses, want)
	}
	if got := f.State().Issues[0].Labels; !slices.Equal(got, []string{"a", "c"}) {
		t.Errorf("labels = %v, want the failed write's left out", got)
	}
	want := []Write{
		{"POST", "/issues/1/labels", `["a"]` + "\n", false},
		{"POST", "/issues/1/labels", `["b"]` + "\n", true},
		{"POST", "/issues/1/labels", `["c"]` + "\n", false},
	}
	if got := f.Writes(); !reflect.DeepEqual(got, want) {
		t.Errorf("writes = %+v, want %+v", got, want)
	}
}

// moveStage posts a marker, moves the stage label from intake to clarifying
// and 👍s the command, in the write order of SPEC §5.2. Unless check is
// set, it posts the marker without looking for one first.
func moveStage(t *testing.T, check bool) func(*Fake) error {
	const marker = "<!-- issuedog:transition to=clarifying cmd=1001 -->"
	return func(f *Fake) error {
		ctx, c := context.Background(), f.Client(t)
		s, err := c.Snapshot(ctx, "o", "r", 1)
		if err != nil {
			return err
		}
		if !check || !slices.ContainsFunc(s.Comments, func(c github.Comment) bool { return c.Body == marker }) {
			if _, _, err := c.Issues.CreateComment(ctx, "o", "r", 1, gogithub.IssueCommentRequest{Body: marker}); err != nil {
				return err
			}
		}
		if !slices.Contains(s.Labels, "stage:clarifying") {
			if _, _, err := c.Issues.AddLabelsToIssue(ctx, "o", "r", 1, []string{"stage:clarifying"}); err != nil {
				return err
			}
		}
		if slices.Contains(s.Labels, "stage:intake") {
			if _, err := c.Issues.RemoveLabelForIssue(ctx, "o", "r", 1, "stage:intake"); err != nil {
				return err
			}
		}
		_, _, err = c.Client.Reactions.CreateIssueCommentReaction(ctx, "o", "r", s.Comments[0].ID, "+1")
		return err
	}
}

func TestReplay(t *testing.T) {
	setup := func(t testing.TB) *Fake {
		f := New(t, "o/r", "issuedog")
		f.AddComment(f.AddIssue("owner", "Title", "body", "stage:intake"), "owner", "/start")
		return f
	}
	Replay(t, setup, moveStage(t, true))

	problems := replay(t, setup, moveStage(t, false))
	if len(problems) == 0 {
		t.Fatal("Replay passed a pass that posts its marker twice")
	}
	if !strings.HasPrefix(problems[0], "crash after write 1 applied: the state") {
		t.Errorf("first problem = %q, want a duplicate marker after write 1 applied", problems[0])
	}

	// A pass that retries a failed write duplicates it when GitHub had
	// already applied it. Only a crash after the write applied shows that.
	retry := func(f *Fake) error {
		ctx, c := context.Background(), f.Client(t)
		s, err := c.Snapshot(ctx, "o", "r", 1)
		if err != nil || len(s.Comments) > 1 {
			return err
		}
		req := gogithub.IssueCommentRequest{Body: "<!-- issuedog:transition to=clarifying -->"}
		if _, _, err := c.Issues.CreateComment(ctx, "o", "r", 1, req); err != nil {
			_, _, err = c.Issues.CreateComment(ctx, "o", "r", 1, req)
			return err
		}
		return nil
	}
	problems = replay(t, setup, retry)
	if len(problems) != 1 || !strings.HasPrefix(problems[0], "crash after write 1 applied") {
		t.Errorf("problems = %q, want only the crash after write 1 applied", problems)
	}
	// Reactions are a set: a pass that adds 👀 then 👍 and carries on after an
	// error may redo them in another order, and Replay must accept that.
	react := func(f *Fake) error {
		ctx, c := context.Background(), f.Client(t)
		s, err := c.Snapshot(ctx, "o", "r", 1)
		if err != nil {
			return err
		}
		for _, content := range []string{"eyes", "+1"} {
			if _, _, err := c.Client.Reactions.CreateIssueCommentReaction(ctx, "o", "r", s.Comments[0].ID, content); err != nil {
				t.Logf("react %s: %v", content, err)
			}
		}
		return nil
	}
	if problems := replay(t, setup, react); len(problems) != 0 {
		t.Errorf("problems = %q, want none for reactions redone in another order", problems)
	}
}

// gqlAnswer is the part of a GraphQL answer the fake's tests look at.
type gqlAnswer struct {
	Data struct {
		Repository *struct {
			Issue *struct {
				Comments struct {
					PageInfo struct{ EndCursor string }
					Nodes    []struct{ FullDatabaseID string }
				}
			}
		}
	}
	Errors []struct{ Type, Message string }
}

// gqlQuery sends query to f for issue number from cursor.
func gqlQuery(t *testing.T, f *Fake, query string, number int, cursor any) gqlAnswer {
	t.Helper()
	c := f.Client(t)
	vars := map[string]any{"owner": "o", "name": "r", "number": number, "cursor": cursor}
	req, err := c.NewRequest(context.Background(), "POST", "graphql", map[string]any{"query": query, "variables": vars})
	if err != nil {
		t.Fatal(err)
	}
	var out gqlAnswer
	if _, err := c.Do(req, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGraphQLAnswers(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	var ids []int64
	for range 101 {
		ids = append(ids, f.AddComment(n, "owner", "hi"))
	}
	f.SetPullRequest(f.AddIssue("owner", "PR", "body"))
	for _, tc := range []struct {
		name   string
		answer gqlAnswer
		want   string
	}{
		{"missing issue", gqlQuery(t, f, answered, 9, nil), "NOT_FOUND Could not resolve to an Issue with the number of 9."},
		{"pull request", gqlQuery(t, f, answered, 2, nil), "NOT_FOUND Could not resolve to an Issue with the number of 2."},
		{"bad cursor", gqlQuery(t, f, answered, n, "nope"), " `nope` does not appear to be a valid cursor."},
		{"no fullDatabaseId", gqlQuery(t, f, strings.Replace(answered, "fullDatabaseId", "databaseId", 1), n, nil),
			" githubtest: the query doesn't name query.repository.issue.comments.nodes.fullDatabaseId" +
				" and names unknown query.repository.issue.comments.nodes.databaseId"},
		{"alias", gqlQuery(t, f, strings.Replace(answered, "nodes { id fullDatabaseId body", "nodes { id fullDatabaseId body: url", 1), n, nil),
			" githubtest: the query names unknown query.repository.issue.comments.nodes.url"},
		{"no comment editor", gqlQuery(t, f, strings.Replace(answered, "} editor { login __typename } }", "} }", 1), n, nil),
			" githubtest: the query doesn't name query.repository.issue.comments.nodes.editor, " +
				"query.repository.issue.comments.nodes.editor.__typename, query.repository.issue.comments.nodes.editor.login"},
	} {
		if len(tc.answer.Errors) != 1 || tc.answer.Errors[0].Type+" "+tc.answer.Errors[0].Message != tc.want {
			t.Errorf("%s: errors = %+v, want %q", tc.name, tc.answer.Errors, tc.want)
		}
		if r := tc.answer.Data.Repository; r != nil && r.Issue != nil {
			t.Errorf("%s: got an issue", tc.name)
		}
	}
	// A cursor survives the deletion of the comment it points at.
	end := gqlQuery(t, f, answered, n, nil).Data.Repository.Issue.Comments.PageInfo.EndCursor
	f.DeleteComment(ids[99])
	nodes := gqlQuery(t, f, answered, n, end).Data.Repository.Issue.Comments.Nodes
	if want := strconv.FormatInt(ids[100], 10); len(nodes) != 1 || nodes[0].FullDatabaseID != want {
		t.Errorf("page 2 after a delete = %+v, want only %s", nodes, want)
	}
}

// An inline fragment is part of the path, so one on the wrong type is refused.
func TestGraphQLHistoryFragment(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	q := strings.Replace(answeredHistory, "on Comment", "on IssueComment", 1)
	errs := gqlQuery(t, f, q, 1, nil).Errors
	if want := "names unknown query.nodes.... on IssueComment,"; len(errs) != 1 || !strings.Contains(errs[0].Message, want) {
		t.Errorf("errors = %+v, want one that %s", errs, want)
	}
	if n := f.HistoryQueries(); n != 0 {
		t.Errorf("history queries = %d, want a refused one not counted", n)
	}
}

// An unknown ID's node is null, with a NOT_FOUND error naming its path, as
// on GitHub. The known node is still answered.
func TestGraphQLHistoryUnknownNode(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	f.AddIssue("owner", "Title", "body")
	c := f.Client(t)
	req, err := c.NewRequest(context.Background(), "POST", "graphql", map[string]any{"query": answeredHistory,
		"variables": map[string]any{"ids": []string{"I_1", "IC_9"}}})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Data   struct{ Nodes []*struct{ ID string } }
		Errors []struct {
			Type, Message string
			Path          []any
		}
	}
	if _, err := c.Do(req, &out); err != nil {
		t.Fatal(err)
	}
	if n := out.Data.Nodes; len(n) != 2 || n[0] == nil || n[0].ID != "I_1" || n[1] != nil {
		t.Errorf("nodes = %+v, want I_1 then null", n)
	}
	want := "NOT_FOUND [nodes 1] Could not resolve to a node with the global id of 'IC_9'"
	if e := out.Errors; len(e) != 1 || fmt.Sprintf("%s %v %s", e[0].Type, e[0].Path, e[0].Message) != want {
		t.Errorf("errors = %+v, want %q", e, want)
	}
}

func TestBeforeGraphQLRunsOnce(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	var runs atomic.Int32
	f.BeforeGraphQL(func() { runs.Add(1) })
	for range 2 {
		gqlQuery(t, f, answered, n, nil)
	}
	if n := runs.Load(); n != 1 {
		t.Errorf("the hook ran %d times, want once", n)
	}
}

// A created issue gets its labels and its type, each the App's change with
// its event on the timeline: a labeled event for each label, then
// issue_type_added with the org's type, matched ignoring case. A type the
// org hasn't got, one renamed away or an empty one is refused with a 422,
// and nothing is filed. Under DropTypeChanges, the issue is filed with no
// type.
func TestCreateIssueWithLabelsAndType(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	f.AddIssue("owner", "First", "")
	task := f.IssueTypeID("Task")
	f.IssueTypeID("Bug")
	f.RenameOrgIssueType("Bug", "Defect")
	ctx, c := context.Background(), f.Client(t)
	is, resp, err := c.Issues.Create(ctx, "o", "r", gogithub.CreateIssueRequest{Title: "Child", Body: new("body"),
		Labels: []string{"scope:github", "priority:p1"}, Type: new("Task")})
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %v", err)
	}
	if is.GetNumber() != 2 || is.GetID() == 0 || is.GetType().GetID() != task || len(is.Labels) != 2 {
		t.Errorf("answer = %+v, want #2 with an ID, the Task type and both labels", is)
	}
	st := f.State().Issues[1]
	if st.Author != "issuedog[bot]" || st.Type != "Task" || !reflect.DeepEqual(st.Labels, []string{"priority:p1", "scope:github"}) {
		t.Errorf("issue = %+v, want the App's, with the Task type and both labels", st)
	}
	timeline, err := c.TimelineSince(ctx, "o", "r", 2, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range timeline {
		got = append(got, fmt.Sprintf("%s %s %s", e.GetEvent(), e.GetLabel().GetName(), e.GetActor().GetLogin()))
	}
	want := []string{"labeled scope:github issuedog[bot]", "labeled priority:p1 issuedog[bot]", "issue_type_added  issuedog[bot]"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}
	// Matched ignoring case, the type is the org's Task.
	is, _, err = c.Issues.Create(ctx, "o", "r", gogithub.CreateIssueRequest{Title: "Lower", Type: new("task")})
	if err != nil || is.GetType().GetID() != task {
		t.Errorf("create with task = %+v, %v; want the Task type", is.GetType(), err)
	}
	// With no type, none is set and no issue-type event written.
	is, _, err = c.Issues.Create(ctx, "o", "r", gogithub.CreateIssueRequest{Title: "Untyped", Labels: []string{"a"}})
	if err != nil || is.Type != nil {
		t.Errorf("create with no type = %+v, %v; want no type", is.GetType(), err)
	}
	if timeline, err := c.TimelineSince(ctx, "o", "r", is.GetNumber(), time.Time{}); err != nil || len(timeline) != 1 || timeline[0].GetEvent() != "labeled" {
		t.Errorf("timeline of an untyped issue = %+v, %v; want its one labeled event", timeline, err)
	}
	filed := len(f.State().Issues)
	for _, name := range []string{"Epic", "Bug", ""} {
		_, _, err := c.Issues.Create(ctx, "o", "r", gogithub.CreateIssueRequest{Title: "Refused", Type: new(name)})
		var er *gogithub.ErrorResponse
		if !errors.As(err, &er) || er.Response.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("type %q: err = %v, want a 422", name, err)
		}
	}
	if got := len(f.State().Issues); got != filed {
		t.Errorf("%d issues after the refusals, want %d", got, filed)
	}
	f.DropTypeChanges()
	is, _, err = c.Issues.Create(ctx, "o", "r", gogithub.CreateIssueRequest{Title: "Dropped", Type: new("Task")})
	if err != nil || is.Type != nil || f.State().Issues[is.GetNumber()-1].Type != "" {
		t.Errorf("create under DropTypeChanges = %+v, %v; want no type", is.GetType(), err)
	}
}

// fakeNumberTB is the test a fake is given, which records each Fatalf and
// ends the goroutine that called it, as testing's own does, so a test can
// check that a helper fails it.
type fakeNumberTB struct {
	testing.TB
	mu    sync.Mutex
	fatal []string
}

func (tb *fakeNumberTB) Fatalf(format string, args ...any) {
	tb.mu.Lock()
	tb.fatal = append(tb.fatal, fmt.Sprintf(format, args...))
	tb.mu.Unlock()
	runtime.Goexit()
}

// fakeNumberFatal runs do in a goroutine of its own, as a test does, and
// returns the Fatalf messages it failed tb with, and what it panicked
// with, if it did.
func fakeNumberFatal(tb *fakeNumberTB, do func()) (fatal []string, panicked any) {
	tb.mu.Lock()
	tb.fatal = nil
	tb.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { panicked = recover() }()
		do()
	}()
	<-done
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return tb.fatal, panicked
}

// fakeNumberAnswer sends method to path below the repo, and returns the
// answer's status and message, failing t on anything but an API error.
func fakeNumberAnswer(t *testing.T, c *github.Client, method, path string, body any) (int, string) {
	t.Helper()
	ctx := context.Background()
	req, err := c.Client.NewRequest(ctx, method, "repos/o/r"+path, body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Client.Do(req, nil)
	var er *gogithub.ErrorResponse
	if err != nil && !errors.As(err, &er) {
		t.Fatal(err)
	}
	if er != nil {
		return resp.StatusCode, er.Message
	}
	return resp.StatusCode, ""
}

// A helper given an issue number the fake hasn't got, as the issue or as
// the parent or blocker it links, fails the test with a message naming
// the number, and changes nothing, rather than panicking. So does a number
// that was an issue until DeleteIssue deleted it.
func TestFakeNumberUnknown(t *testing.T) {
	t.Parallel()
	tb := &fakeNumberTB{TB: t}
	f := New(tb, "o/r", "issuedog")
	n := f.AddIssue("owner", "Issue", "body")
	f.AddIssue("owner", "Other", "body")
	before := f.State()
	for _, tc := range []struct {
		name string
		do   func()
	}{
		{"SetParent of an unknown issue", func() { f.SetParent(9, n) }},
		{"SetParent to an unknown parent", func() { f.SetParent(n, 9) }},
		{"SetBlockedBy of an unknown issue", func() { f.SetBlockedBy(9, n) }},
		{"SetBlockedBy by an unknown blocker", func() { f.SetBlockedBy(n, 9) }},
		{"LinkSubIssue to an unknown parent", func() { f.LinkSubIssue(9, "o/other", 1) }},
		{"SetType", func() { f.SetType(9, "Task") }},
		{"SetTypeBy", func() { f.SetTypeBy(9, "owner", "Task") }},
		{"SetPullRequest", func() { f.SetPullRequest(9) }},
		{"SetAssignees", func() { f.SetAssignees(9, "owner") }},
		{"SetPinned", func() { f.SetPinned(9, true) }},
		{"EditBody", func() { f.EditBody(9, "owner", "new") }},
		{"AddComment", func() { f.AddComment(9, "owner", "hi") }},
		{"DeleteRevision", func() { f.DeleteRevision(9, 0) }},
		{"AddLabel", func() { f.AddLabel(9, "owner", "bug") }},
		{"RemoveLabel", func() { f.RemoveLabel(9, "owner", "bug") }},
		{"RenameIssue", func() { f.RenameIssue(9, "owner", "New") }},
		{"Close", func() { f.Close(9) }},
		{"CloseBy", func() { f.CloseBy(9, "owner") }},
		{"CloseAs", func() { f.CloseAs(9, "owner", "not_planned") }},
		{"CloseByPullRequest", func() { f.CloseByPullRequest(9, "owner", ClosingPR{}) }},
		{"CloseByCommit", func() { f.CloseByCommit(9, "owner", "abc", "") }},
		{"CloseByProject", func() { f.CloseByProject(9, "owner") }},
		{"Reopen", func() { f.Reopen(9) }},
		{"ReopenBy", func() { f.ReopenBy(9, "owner") }},
		{"ForbidCloser", func() { f.ForbidCloser(9, 1) }},
		{"FailCloser", func() { f.FailCloser(9, 1, "", "boom") }},
		{"MendCloser", func() { f.MendCloser(9, 1) }},
		{"ForgetCloser", func() { f.ForgetCloser(9, 1) }},
		{"AddIssueReaction", func() { f.AddIssueReaction(9, "owner", "+1") }},
		{"DeleteIssueReaction", func() { f.DeleteIssueReaction(9, 1) }},
		{"Transfer", func() { f.Transfer(9, "o/elsewhere") }},
	} {
		fatal, panicked := fakeNumberFatal(tb, tc.do)
		if panicked != nil {
			t.Errorf("%s: panicked with %v; want the test failed", tc.name, panicked)
		}
		if want := []string{"githubtest: no issue #9: the fake has 2"}; !slices.Equal(fatal, want) {
			t.Errorf("%s: Fatalf %q; want %q", tc.name, fatal, want)
		}
	}
	// Numbers start at 1, so 0 is no issue either.
	fatal, panicked := fakeNumberFatal(tb, func() { f.Close(0) })
	if want := []string{"githubtest: no issue #0: the fake has 2"}; panicked != nil || !slices.Equal(fatal, want) {
		t.Errorf("Close(0): Fatalf %q, panicked with %v; want Fatalf %q", fatal, panicked, want)
	}
	if got := f.State(); !reflect.DeepEqual(got, before) {
		t.Errorf("state after the unknown numbers:\n%+v\nwant it unchanged:\n%+v", got, before)
	}

	// A parent and a blocker the fake hasn't got: the answers that name
	// them are a 500 naming the number, the parents above an issue
	// included, which a sub-issue link checks. No helper leaves such a
	// link, as DeleteIssue drops a deleted issue's links
	// (TestFakeDeleteIssueLinks), so the test makes them itself.
	g := New(t, "o/r", "issuedog")
	blocked, child := g.AddIssue("owner", "Blocked", "body"), g.AddIssue("owner", "Child", "body")
	gone := 3
	g.mu.Lock()
	g.issues[child-1].parent = gone
	g.issues[blocked-1].blockedBy = []int{gone}
	g.mu.Unlock()
	c := g.Client(t)
	for _, tc := range []struct {
		name, method, path string
		body               any
	}{
		{"the parent", http.MethodGet, fmt.Sprintf("/issues/%d/parent", child), nil},
		{"the blockers", http.MethodGet, fmt.Sprintf("/issues/%d/dependencies/blocked_by", blocked), nil},
		{"a sub-issue link", http.MethodPost, fmt.Sprintf("/issues/%d/sub_issues", child), gogithub.SubIssueRequest{SubIssueID: issueID(blocked)}},
	} {
		status, msg := fakeNumberAnswer(t, c, tc.method, tc.path, tc.body)
		if status != http.StatusInternalServerError || !strings.Contains(msg, fmt.Sprintf("no issue #%d: the fake has 2", gone)) {
			t.Errorf("%s: %d %q; want a 500 naming #%d", tc.name, status, msg, gone)
		}
	}
}
