package githubtest

import (
	"context"
	"errors"
	"net/http"
	"testing"

	gogithub "github.com/google/go-github/v92/github"
)

// FailPinStatus answers each pin with the status alone and no pin, still
// logged as a write, until it's cleared (#226).
func TestDigestPinFailPinStatus(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("issuedog[bot]", "issuedog digest", "")
	ctx, c := context.Background(), f.Client(t)
	f.FailPinStatus(http.StatusForbidden)
	for range 2 {
		var er *gogithub.ErrorResponse
		if err := c.PinIssue(ctx, "o", "r", n); !errors.As(err, &er) || er.Response.StatusCode != http.StatusForbidden || er.Message != "Forbidden" {
			t.Errorf("err = %v; want a 403 with its status text", err)
		}
	}
	if _, pinned := pinOf(f, n); pinned {
		t.Error("pinned after a failed pin")
	}
	if w := f.Writes(); len(w) != 2 || w[0].Path != "/graphql" {
		t.Errorf("writes = %+v; want each failed pin logged", w)
	}
	f.FailPinStatus(0)
	if err := c.PinIssue(ctx, "o", "r", n); err != nil {
		t.Fatal(err)
	}
	if _, pinned := pinOf(f, n); !pinned {
		t.Error("not pinned once FailPinStatus is cleared")
	}
}

// FailIsPinned answers each isPinned query with the status alone, until
// it's cleared, and leaves pins and other queries alone (#226).
func TestDigestPinFailIsPinned(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("issuedog[bot]", "issuedog digest", "")
	ctx, c := context.Background(), f.Client(t)
	f.FailIsPinned(http.StatusBadGateway)
	for range 2 {
		var er *gogithub.ErrorResponse
		if _, err := c.IsPinned(ctx, "o", "r", n); !errors.As(err, &er) || er.Response.StatusCode != http.StatusBadGateway {
			t.Errorf("err = %v; want a 502", err)
		}
	}
	if err := c.PinIssue(ctx, "o", "r", n); err != nil {
		t.Fatalf("a pin under FailIsPinned: %v", err)
	}
	if _, err := c.Snapshot(ctx, "o", "r", n); err != nil {
		t.Errorf("a snapshot, with its comments query, under FailIsPinned: %v", err)
	}
	f.FailIsPinned(0)
	if pinned, err := c.IsPinned(ctx, "o", "r", n); err != nil || !pinned {
		t.Errorf("IsPinned once cleared = %v, %v; want true", pinned, err)
	}
}
