package daemon

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// childMatch finds the children of #1's plan items among its sub-issues
// and the issue list, as GitHub lists them, by their origin as Derive
// reads it (state.Issue.Origin), whatever its rev. It ignores issues of
// other plans, issues not by the App, pull requests, origins that aren't
// whole, and sub-issues in other repos. An App body someone edited still
// matches: the listing has no edit history, and the marker is kept. Of two
// issues for one item, the sub-issue wins, or else the one filed first.
func TestChildMatch(t *testing.T) {
	t.Parallel()
	const app = childSlug + "[bot]"
	origin := func(plan, rev, item string) string {
		return "A child.\n\n<!-- issuedog:origin plan=#" + plan + " rev=" + rev + " item=" + item + " -->"
	}
	f := githubtest.New(t, "o/r", childSlug)
	f.AddIssue("owner", "Parent", "body")             // 1
	f.AddIssue(app, "S1", origin("1", "1", "S1"))     // 2: a sub-issue
	f.AddIssue(app, "S2", origin("1", "1", "S2"))     // 3: filed, not linked
	f.AddIssue(app, "S3", origin("9", "1", "S3"))     // 4: another plan's
	f.AddIssue("owner", "S3", origin("1", "1", "S3")) // 5: not the App's
	f.AddIssue(app, "S4", origin("1", "1", "S4"))     // 6: edited by the owner, below
	f.AddIssue(app, "S5", origin("1", "2", "S5"))     // 7: another rev, closed, below
	f.AddIssue(app, "S2 again", origin("1", "1", "S2"))
	f.AddIssue(app, "S6", origin("1", "1", "S6"))       // 9: filed first, but not linked
	f.AddIssue(app, "S6 again", origin("1", "1", "S6")) // 10: linked, below
	f.AddIssue(app, "S7", origin("1", "1", "S7"))       // 11: a pull request
	f.AddIssue(app, "S8", "<!-- issuedog:origin plan=#1 item=S8 -->")
	f.SetParent(2, 1)
	f.SetParent(10, 1)
	f.SetPullRequest(11)
	f.EditBody(6, "owner", origin("1", "1", "S4")+"\n\nEdited.")
	f.Close(7)
	// #3 is updated after #8, its duplicate, so the list gives #8 first.
	f.EditBody(3, app, origin("1", "1", "S2"))
	// A sub-issue in another repo, with the number of #3 here: it isn't
	// #3, so #3 is still found.
	f.LinkSubIssue(1, "o/other", 3)

	c := f.Client(t)
	ctx := context.Background()
	subs, err := c.SubIssues(ctx, "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := c.IssuesSince(ctx, "o", "r", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int]int64{}
	for _, is := range listed {
		ids[is.GetNumber()] = is.GetID()
	}
	a := &Applier{Owner: "o", Repo: "r"}
	got := childMatch(1, childSlug, subs, listed, a.inRepo, nil)
	want := map[string]childFound{
		"S1": {Number: 2, ID: ids[2], Linked: true},
		"S2": {Number: 3, ID: ids[3]},
		"S4": {Number: 6, ID: ids[6]},
		"S5": {Number: 7, ID: ids[7], Closed: true}, // closed, as the listing says
		"S6": {Number: 10, ID: ids[10], Linked: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("childMatch = %+v\nwant %+v", got, want)
	}

	// The edited body's marker isn't read from the issue as Snapshot reads
	// it, with its edit history: only from the listing.
	s, err := c.Snapshot(ctx, "o", "r", 6)
	if err != nil {
		t.Fatal(err)
	}
	if o := state.Derive(s, childSlug, func(int64) bool { return false }).Origin; o.Plan != 0 {
		t.Errorf("Derive on #6's snapshot read origin %+v; want none, as the owner edited the body last", o)
	}

	// Another plan's children are those of its own parent.
	if got := childMatch(9, childSlug, nil, listed, a.inRepo, nil); !reflect.DeepEqual(got, map[string]childFound{"S3": {Number: 4, ID: ids[4]}}) {
		t.Errorf("childMatch for #9 = %+v, want S3, #4", got)
	}
	// With no listing, only the sub-issues in this repo count.
	if got := childMatch(1, childSlug, subs, nil, a.inRepo, nil); !reflect.DeepEqual(got, map[string]childFound{
		"S1": {Number: 2, ID: ids[2], Linked: true}, "S6": {Number: 10, ID: ids[10], Linked: true}}) {
		t.Errorf("childMatch of the sub-issues alone = %+v, want S1, #2, and S6, #10", got)
	}
}

// childMatch takes an issue a person filed as a child only through
// origins, what its snapshot gives (childOrigins), as for an adopted
// issue whose clean App comment carries its origin (#372): #2, linked
// first, is S1's child, and wins over the App's #5, linked after it. A
// person's body never gives an origin, so #4, a sub-issue with no entry
// in origins, isn't one, nor is #3, which isn't a sub-issue, whatever its
// body says.
func TestAdoptedChildMatch(t *testing.T) {
	t.Parallel()
	const app = childSlug + "[bot]"
	origin := func(item string) string {
		return "A child.\n\n<!-- issuedog:origin plan=#1 rev=1 item=" + item + " -->"
	}
	f := githubtest.New(t, "o/r", childSlug)
	f.AddIssue("owner", "Parent", "body")      // 1
	f.AddIssue("owner", "S1", "Owner's words") // 2: adopted, linked
	f.AddIssue("owner", "S2", origin("S2"))    // 3: not linked
	f.AddIssue("owner", "S3", origin("S3"))    // 4: linked, no entry in origins
	f.AddIssue(app, "S1 again", origin("S1"))  // 5: linked after #2
	f.SetParent(2, 1)
	f.SetParent(4, 1)
	f.SetParent(5, 1)
	c := f.Client(t)
	ctx := context.Background()
	subs, err := c.SubIssues(ctx, "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := c.IssuesSince(ctx, "o", "r", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int]int64{}
	for _, is := range listed {
		ids[is.GetNumber()] = is.GetID()
	}
	a := &Applier{Owner: "o", Repo: "r"}
	origins := map[int]state.Origin{2: {Plan: 1, Rev: 1, Item: "S1", Comment: 7}}
	if got, want := childMatch(1, childSlug, subs, listed, a.inRepo, origins), map[string]childFound{
		"S1": {Number: 2, ID: ids[2], Linked: true}}; !reflect.DeepEqual(got, want) {
		t.Errorf("childMatch = %+v\nwant %+v", got, want)
	}
	if got, want := childMatch(1, childSlug, subs, listed, a.inRepo, nil), map[string]childFound{
		"S1": {Number: 5, ID: ids[5], Linked: true}}; !reflect.DeepEqual(got, want) {
		t.Errorf("childMatch with no origins = %+v\nwant %+v, the App's alone", got, want)
	}
}
