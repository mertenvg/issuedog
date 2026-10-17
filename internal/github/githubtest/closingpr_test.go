package githubtest

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	gogithub "github.com/google/go-github/v92/github"
)

// closingServed is issue n's closed and reopened events as the timeline
// serves them, as "<event> <actor> <node_id> <state_reason> <commit_id>".
func closingServed(t *testing.T, f *Fake, n int) []string {
	t.Helper()
	c := f.Client(t)
	req, err := c.NewRequest(context.Background(), "GET", fmt.Sprintf("repos/o/r/issues/%d/timeline?per_page=100", n), nil)
	if err != nil {
		t.Fatal(err)
	}
	var events []struct {
		Event       string
		Actor       *gogithub.User
		NodeID      string `json:"node_id"`
		StateReason string `json:"state_reason"`
		CommitID    string `json:"commit_id"`
	}
	if _, err := c.Do(req, &events); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		if e.Event == "closed" || e.Event == "reopened" {
			got = append(got, fmt.Sprintf("%s %s %s %s %s", e.Event, e.Actor.GetLogin(), e.NodeID, e.StateReason, e.CommitID))
		}
	}
	return got
}

// closingAsk asks the fake the closer query for node id in owner/name, and
// returns its answer as JSON-decoded data and its errors' types.
func closingAsk(t *testing.T, f *Fake, owner, name, id string) (map[string]any, []string) {
	t.Helper()
	c := f.Client(t)
	req, err := c.NewRequest(context.Background(), "POST", "graphql", map[string]any{"query": answeredCloser,
		"variables": map[string]any{"id": id, "owner": owner, "name": name}})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Data   map[string]any
		Errors []struct{ Type, Message string }
	}
	if _, err := c.Do(req, &out); err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, e := range out.Errors {
		types = append(types, e.Type)
	}
	return out.Data, types
}

// As on GitHub, every closed and reopened event names its node, no closed
// event carries a state_reason, and only a commit's close names a commit.
// GraphQL's ClosedEvent gives each close's state reason and closer: a pull
// request with its merge, base and repo, a commit with its repo, or none.
func TestClosingServed(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Issue", "")
	byHand := f.CloseAs(n, "owner", "duplicate")
	f.ReopenBy(n, "owner")
	pr := f.CloseByPullRequest(n, "mallory", ClosingPR{Merged: true})
	f.ReopenBy(n, "owner")
	if f.CloseByCommit(n, "eve", "def", "x/y") == 0 || f.CloseByCommit(n, "eve", "ghi", "") != 0 || f.CloseByPullRequest(n, "eve", ClosingPR{}) != 0 {
		t.Error("a close gave no event, or a second one for a closed issue")
	}
	f.ReopenBy(n, "owner")
	ctx, c := context.Background(), f.Client(t)
	if _, _, err := c.Issues.Update(ctx, "o", "r", n, gogithub.UpdateIssueRequest{State: new("closed"), StateReason: new("not_planned")}); err != nil {
		t.Fatal(err)
	}
	want := []string{"closed owner CE_9001  ", "reopened owner REE_9002  ", "closed mallory CE_9003  ", "reopened owner REE_9004  ",
		"closed eve CE_9005  def", "reopened owner REE_9006  ", "closed issuedog[bot] CE_9007  "}
	if got := closingServed(t, f, n); !reflect.DeepEqual(got, want) {
		t.Errorf("events %q; want %q", got, want)
	}
	repo := map[string]any{"nameWithOwner": "o/r", "defaultBranchRef": map[string]any{"name": "main"}}
	for _, tc := range []struct {
		id    string
		node  any
		types []string
	}{
		{fmt.Sprintf("CE_%d", byHand), map[string]any{"stateReason": "DUPLICATE", "closer": nil}, nil},
		{fmt.Sprintf("CE_%d", pr), map[string]any{"stateReason": "COMPLETED", "closer": map[string]any{"__typename": "PullRequest", "merged": true,
			"baseRefName": "main", "baseRepository": map[string]any{"nameWithOwner": "o/r"}}}, nil},
		{"CE_9005", map[string]any{"stateReason": "COMPLETED", "closer": map[string]any{"__typename": "Commit", "repository": map[string]any{"nameWithOwner": "x/y"}}}, nil},
		{"CE_9007", map[string]any{"stateReason": "NOT_PLANNED", "closer": nil}, nil},
		{"REE_9002", map[string]any{}, nil},
		{"CE_9002", nil, []string{"NOT_FOUND"}},
		{"X_1", nil, []string{"NOT_FOUND"}},
	} {
		data, types := closingAsk(t, f, "o", "r", tc.id)
		if !reflect.DeepEqual(data, map[string]any{"repository": repo, "node": tc.node}) || !reflect.DeepEqual(types, tc.types) {
			t.Errorf("%s: data %v, errors %q; want node %v, errors %q", tc.id, data, types, tc.node, tc.types)
		}
	}
	m := f.AddIssue("owner", "Another", "")
	project := f.CloseByProject(m, "github-project-automation[bot]")
	if data, types := closingAsk(t, f, "o", "r", fmt.Sprintf("CE_%d", project)); !reflect.DeepEqual(data["node"],
		map[string]any{"stateReason": "COMPLETED", "closer": map[string]any{"__typename": "ProjectV2"}}) || types != nil {
		t.Errorf("a project's close: %v, %q; want a ProjectV2 closer", data, types)
	}
	f.ForbidCloser(m, project)
	var errs struct {
		Errors []struct {
			Type string
			Path []any
		}
	}
	req, err := c.NewRequest(ctx, "POST", "graphql", map[string]any{"query": answeredCloser,
		"variables": map[string]any{"id": fmt.Sprintf("CE_%d", project), "owner": "o", "name": "r"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(req, &errs); err != nil {
		t.Fatal(err)
	}
	if data, _ := closingAsk(t, f, "o", "r", fmt.Sprintf("CE_%d", project)); !reflect.DeepEqual(data["node"], map[string]any{"stateReason": "COMPLETED", "closer": nil}) ||
		len(errs.Errors) != 1 || errs.Errors[0].Type != "FORBIDDEN" || !reflect.DeepEqual(errs.Errors[0].Path, []any{"node", "closer"}) {
		t.Errorf("a forbidden closer: node %v, errors %+v; want a null closer, FORBIDDEN at node.closer", data["node"], errs.Errors)
	}
	f.ForgetCloser(n, pr)
	if data, _ := closingAsk(t, f, "o", "r", fmt.Sprintf("CE_%d", pr)); !reflect.DeepEqual(data["node"], map[string]any{}) {
		t.Errorf("a forgotten closer's node is %v; want no ClosedEvent's fields", data["node"])
	}
	if data, types := closingAsk(t, f, "o", "other", "CE_9001"); data["repository"] != nil || !reflect.DeepEqual(types, []string{"NOT_FOUND"}) {
		t.Errorf("another repo: %v, %q; want a null repository, NOT_FOUND", data, types)
	}
}
