package github

import (
	"context"
	"errors"
	"testing"
)

// IsPinned's error wraps ErrIssueNotFound only for GitHub's NOT_FOUND
// with the repo found and the issue null, as GitHub answers a lookup by
// number of a deleted or transferred issue (#428). An HTTP 404 from the
// GraphQL endpoint itself, a NOT_FOUND for the repo, or another error
// type for the issue isn't one.
func TestIsPinnedNotFound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, gql string
		notFound  bool
	}{
		{"the issue NOT_FOUND", `{"data":{"repository":{"issue":null}},"errors":[{"type":"NOT_FOUND","path":["repository","issue"],` +
			`"message":"Could not resolve to an Issue with the number of 7."}]}`, true},
		{"an HTTP 404 from /graphql", "", false},
		{"the repo NOT_FOUND", `{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","path":["repository"],` +
			`"message":"Could not resolve to a Repository with the name 'o/r'."}]}`, false},
		{"the issue FORBIDDEN", `{"data":{"repository":{"issue":null}},"errors":[{"type":"FORBIDDEN","path":["repository","issue"],` +
			`"message":"Resource not accessible by integration"}]}`, false},
		{"no issue and no error", `{"data":{"repository":{"issue":null}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := pinServer(t, "", tc.gql, "").IsPinned(context.Background(), "o", "r", 7)
			if err == nil || errors.Is(err, ErrIssueNotFound) != tc.notFound {
				t.Errorf("IsPinned = %v; want an error, ErrIssueNotFound %t", err, tc.notFound)
			}
			if tc.notFound && (!errors.Is(err, ErrGraphQL) ||
				err.Error() != "is o/r#7 pinned: GitHub found no such issue: graphql errors: Could not resolve to an Issue with the number of 7.") {
				t.Errorf("IsPinned = %v; want it to wrap ErrGraphQL too, naming GitHub's message", err)
			}
		})
	}
}
