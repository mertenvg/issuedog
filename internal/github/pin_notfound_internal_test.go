package github

import (
	"context"
	"errors"
	"testing"
)

// PinIssue's error wraps ErrIssueNotFound, with ErrGraphQL, only for
// GitHub's NOT_FOUND with no pin, as GitHub answers a node ID it can't
// resolve, such as a deleted issue's (#495). Another error type, such as
// a refusal's FORBIDDEN, or a NOT_FOUND with a pin, isn't one.
func TestPinIssueNotFound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, gql string
		notFound  bool
	}{
		{"the issue NOT_FOUND", `{"data":{"pinIssue":null},"errors":[{"type":"NOT_FOUND","path":["pinIssue"],` +
			`"message":"Could not resolve to a node with the global id of 'I_7'"}]}`, true},
		{"the issue FORBIDDEN", `{"data":{"pinIssue":null},"errors":[{"type":"FORBIDDEN","path":["pinIssue"],` +
			`"message":"Resource not accessible by integration"}]}`, false},
		{"a NOT_FOUND with a pin", `{"data":{"pinIssue":{"issue":{"isPinned":true}}},"errors":[{"type":"NOT_FOUND",` +
			`"message":"Could not resolve to a node with the global id of 'X_1'"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := pinServer(t, `{"number":7,"node_id":"I_7"}`, tc.gql, "").PinIssue(context.Background(), "o", "r", 7)
			if !errors.Is(err, ErrGraphQL) || errors.Is(err, ErrIssueNotFound) != tc.notFound {
				t.Errorf("PinIssue = %v; want ErrGraphQL, and ErrIssueNotFound %t", err, tc.notFound)
			}
			if tc.notFound && err.Error() != "pin o/r#7: GitHub found no such issue: graphql errors: Could not resolve to a node with the global id of 'I_7'" {
				t.Errorf("PinIssue = %v; want it to name GitHub's message", err)
			}
		})
	}
}
