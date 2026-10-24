package daemon

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
)

// digestPinGone takes GraphQL's NOT_FOUND for the digest, a REST 404 or
// 410, and a transfer as the digest gone, and an HTTP 404 or 410 from
// /graphql itself, at any base path, as an ordinary failure (#428).
func TestDigestPinGone(t *testing.T) {
	t.Parallel()
	status := func(code int, path string) error {
		r := &http.Response{StatusCode: code}
		if path != "" {
			u, err := url.Parse("https://api.github.com" + path)
			if err != nil {
				t.Fatal(err)
			}
			r.Request = &http.Request{Method: http.MethodPost, URL: u}
		}
		return fmt.Errorf("pin: %w", &gogithub.ErrorResponse{Response: r})
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"NOT_FOUND", fmt.Errorf("is o/r#1 pinned: %w: %w", github.ErrIssueNotFound, github.ErrGraphQL), true},
		{"transferred", fmt.Errorf("pin o/r#1: %w", github.ErrTransferred), true},
		{"REST 404", status(http.StatusNotFound, "/repos/o/r/issues/1"), true},
		{"REST 410", status(http.StatusGone, "/repos/o/r/issues/1"), true},
		{"404 with no request", status(http.StatusNotFound, ""), true},
		{"/graphql 404", status(http.StatusNotFound, "/graphql"), false},
		{"/api/graphql 410", status(http.StatusGone, "/api/graphql"), false},
		{"/graphql 500", status(http.StatusInternalServerError, "/graphql"), false},
		{"REST 500", status(http.StatusInternalServerError, "/repos/o/r/issues/1"), false},
		{"another GraphQL error", fmt.Errorf("is o/r#1 pinned: %w", github.ErrGraphQL), false},
		{"none", nil, false},
	} {
		if got := digestPinGone(tc.err); got != tc.want {
			t.Errorf("%s: digestPinGone(%v) = %t; want %t", tc.name, tc.err, got, tc.want)
		}
	}
}
