package daemon

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
)

// digestPinFailHTTP is go-github's error for an HTTP status with message,
// wrapped as a client's request wraps it, and in github.ErrGraphQL too if
// refused, as PinIssue wraps a 401 or 403 (#226).
func digestPinFailHTTP(status int, message string, refused bool) error {
	err := error(&gogithub.ErrorResponse{Response: &http.Response{StatusCode: status, Request: &http.Request{Method: "POST"}}, Message: message})
	if refused {
		err = fmt.Errorf("%w: %w", github.ErrGraphQL, err)
	}
	return fmt.Errorf("pin o/r#1: %w", err)
}

// The note for a failed check or pin names what failed, and the HTTP
// status if there is one, but not the rest of the error, which may name a
// URL: it's the App's own text.
func TestDigestPinFailNote(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err        error
		what, want string
	}{
		{digestPinFailHTTP(http.StatusBadGateway, "Bad Gateway <!-- issuedog:x --> @eve", false), "checking whether it's pinned failed",
			"I couldn't pin this issue yet: checking whether it's pinned failed (HTTP 502). I'll try again when the list changes, or pin it by hand."},
		{errors.New(`Post "https://api.github.com/graphql": dial tcp: i/o timeout`), "pinning it failed",
			"I couldn't pin this issue yet: pinning it failed. I'll try again when the list changes, or pin it by hand."},
	} {
		if got := digestPinFailNote(tc.err, tc.what); got != tc.want {
			t.Errorf("digestPinFailNote(%q, %q) = %q, want %q", tc.err, tc.what, got, tc.want)
		}
	}
}

// A refused pin's note for an HTTP 401 or 403 names the status and
// GitHub's message from its answer, escaped, and not the URL go-github's
// error names; with no message, the status alone.
func TestDigestPinFailRefusedNote(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want string
	}{
		{digestPinFailHTTP(http.StatusForbidden, "Resource not accessible <!-- issuedog:x --> `x`", true),
			"I couldn't pin this issue: `HTTP 403: Resource not accessible &lt;!-- issuedog:x --> 'x'`. Pin it by hand."},
		{digestPinFailHTTP(http.StatusUnauthorized, "", true), "I couldn't pin this issue: `HTTP 401`. Pin it by hand."},
	} {
		if got := digestPinNote(tc.err); got != tc.want {
			t.Errorf("digestPinNote(%q) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// A dropped digest forgets a failed check, so the digest found again
// after it, or another, has no note but its own.
func TestDigestPinFailDropped(t *testing.T) {
	t.Parallel()
	d := &Daemon{Step: &Step{Applier: &Applier{Owner: "o", Repo: "r"}}}
	d.digest.pinFailed, d.digest.note = 1, "Note."
	if got := d.digest.noteOf(1); got != "Note." {
		t.Fatalf("noteOf(1) = %q before the drop; want the failed check's", got)
	}
	if got := d.digest.noteOf(2); got != "" {
		t.Errorf("noteOf(2) = %q; want none for another digest", got)
	}
	d.digestDrop(1, errors.New("404"))
	if g := d.digest; g.pinFailed != 0 || g.note != "" || g.noteOf(1) != "" {
		t.Errorf("after the drop: pinFailed %d, note %q; want both forgotten", g.pinFailed, g.note)
	}
}
