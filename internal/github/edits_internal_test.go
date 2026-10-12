package github

import (
	"encoding/json"
	"testing"
	"time"
)

// TestEditBotLogin decodes an answer shaped like GitHub's, whose Bot logins
// have no "[bot]", and checks that it is added once.
func TestEditBotLogin(t *testing.T) {
	const answer = `{"data":{"repository":{"issue":{
	  "lastEditedAt":"2026-10-12T03:50:22Z","editor":{"login":"issuedog-dev","__typename":"Bot"},
	  "comments":{"pageInfo":{"hasNextPage":false,"endCursor":"Y3Vyc29yOnYyOpHPAAAAAWxE-eE="},"nodes":[
	    {"databaseId":6106375555,"lastEditedAt":"2026-10-12T04:00:00Z","editor":{"login":"issuedog-dev[bot]","__typename":"Bot"}},
	    {"databaseId":6110641371,"lastEditedAt":"2026-10-12T05:00:00Z","editor":{"login":"mertenvg","__typename":"User"}},
	    {"databaseId":6111427041,"lastEditedAt":"2026-10-12T06:00:00Z","editor":null},
	    {"databaseId":6111427042,"lastEditedAt":null,"editor":null}]}}}}}`
	var out editsResponse
	if err := json.Unmarshal([]byte(answer), &out); err != nil {
		t.Fatal(err)
	}
	is := out.Data.Repository.Issue
	at := func(h int) time.Time { return time.Date(2026, 10, 12, h, 0, 0, 0, time.UTC) }
	if got, want := is.edit(), (Edit{By: Author{Login: "issuedog-dev[bot]", Type: "Bot"}, At: at(3).Add(50*time.Minute + 22*time.Second)}); got != want {
		t.Errorf("body edit = %+v, want %+v", got, want)
	}
	want := map[int64]Edit{
		6106375555: {By: Author{Login: "issuedog-dev[bot]", Type: "Bot"}, At: at(4)},
		6110641371: {By: Author{Login: "mertenvg", Type: "User"}, At: at(5)},
		6111427041: {At: at(6)},
		6111427042: {},
	}
	for _, n := range is.Comments.Nodes {
		if got := n.edit(); got != want[n.DatabaseID] {
			t.Errorf("comment %d edit = %+v, want %+v", n.DatabaseID, got, want[n.DatabaseID])
		}
	}
	if len(is.Comments.Nodes) != len(want) {
		t.Errorf("decoded %d comments, want %d", len(is.Comments.Nodes), len(want))
	}
}
