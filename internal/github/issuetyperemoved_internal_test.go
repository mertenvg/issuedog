package github

import (
	"encoding/json"
	"testing"
)

// The ID of the type an issue-type event left is the ID of the type it set:
// none for a removal, which leaves no type. GitHub's REST description gives
// a removal only a prev_issue_type, but if it ever gives an issue_type too,
// that ID isn't the type it left either (#246), as TestIssueTypeRevertLeft
// checks for the name. A change whose issue_type is null left no type
// GitHub names, so no ID either.
func TestTypeRemovedLeftID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		json string
		want int64
	}{
		{`{"event":"issue_type_added","issue_type":{"id":1,"name":"Task"}}`, 1},
		{`{"event":"issue_type_changed","issue_type":{"id":2,"name":"Epic"},"prev_issue_type":{"id":1,"name":"Task"}}`, 2},
		{`{"event":"issue_type_removed","prev_issue_type":{"id":1,"name":"Task"}}`, 0},
		{`{"event":"issue_type_removed","issue_type":{"id":1,"name":"Task"},"prev_issue_type":{"id":1,"name":"Task"}}`, 0},
		{`{"event":"issue_type_removed","issue_type":{"id":1,"name":"Task"}}`, 0},
		{`{"event":"issue_type_changed","issue_type":null,"prev_issue_type":{"id":1,"name":"Task"}}`, 0},
	} {
		var e issueTypeRevertEvent
		if err := json.Unmarshal([]byte(tc.json), &e); err != nil {
			t.Fatal(err)
		}
		if got := issueTypeRevertLeftID(&e); got != tc.want {
			t.Errorf("issueTypeRevertLeftID(%s) = %d; want %d", tc.json, got, tc.want)
		}
	}
}
