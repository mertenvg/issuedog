package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"
)

// An HTTP 401 or 403 answer to the pinIssue mutation is a refusal (#226):
// the error wraps ErrGraphQL and GitHub's ErrorResponse. A rate limit's
// 403, primary or secondary, isn't one, nor is a 500 or a 404, nor a 403
// on the GET of the issue before the mutation.
func TestDigestPinFailPinIssueHTTP(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		getStatus       int // the GET's status, 0 for a good answer
		status          int
		header, body    string
		refused         bool
		wantInErrorText string
	}{
		{"401", 0, http.StatusUnauthorized, "", `{"message":"Bad credentials"}`, true, "Bad credentials"},
		{"403", 0, http.StatusForbidden, "", `{"message":"Resource not accessible by integration"}`, true, "Resource not accessible by integration"},
		{"primary rate limit", 0, http.StatusForbidden, "0", `{"message":"API rate limit exceeded"}`, false, "API rate limit exceeded"},
		{"secondary rate limit", 0, http.StatusForbidden, "",
			`{"message":"You have exceeded a secondary rate limit","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`,
			false, "secondary rate limit"},
		{"500", 0, http.StatusInternalServerError, "", `{"message":"Server Error"}`, false, "Server Error"},
		{"404", 0, http.StatusNotFound, "", `{"message":"Not Found"}`, false, "Not Found"},
		{"403 on the GET", http.StatusForbidden, http.StatusOK, "", `{"data":{"pinIssue":{"issue":{"isPinned":true}}}}`, false, "Forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newClientWith(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				status, body := tc.status, tc.body
				switch r.Method + " " + r.URL.Path {
				case "GET /repos/o/r/issues/7":
					status, body = http.StatusOK, `{"number":7,"node_id":"I_7"}`
					if tc.getStatus != 0 {
						status, body = tc.getStatus, `{"message":"Forbidden"}`
					}
				case "POST /graphql":
					if tc.header != "" {
						w.Header().Set("X-RateLimit-Remaining", tc.header)
					}
				default:
					status, body = http.StatusNotFound, `{"message":"Not Found"}`
				}
				w.WriteHeader(status)
				if _, err := io.WriteString(w, body); err != nil {
					t.Errorf("reply to %s %s: %v", r.Method, r.URL.Path, err)
				}
			}))
			err := c.PinIssue(context.Background(), "o", "r", 7)
			if err == nil || !strings.HasPrefix(err.Error(), "pin o/r#7: ") || !strings.Contains(err.Error(), tc.wantInErrorText) {
				t.Fatalf("err = %v; want one naming o/r#7 and %q", err, tc.wantInErrorText)
			}
			if got := errors.Is(err, ErrGraphQL); got != tc.refused {
				t.Errorf("errors.Is(%v, ErrGraphQL) = %t; want %t", err, got, tc.refused)
			}
			var er *gogithub.ErrorResponse
			if tc.refused && (!errors.As(err, &er) || er.Message != tc.wantInErrorText) {
				t.Errorf("err = %v; want GitHub's ErrorResponse, with its message, still wrapped", err)
			}
		})
	}
}
