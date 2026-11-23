package github

import (
	"context"
	"net/http"
	"testing"
)

// GitHub gives an open pull request a merge_commit_sha, its test merge's,
// which no branch has, so PullRequest leaves it out, and the merger too,
// until the pull request merged; a merged one's are kept.
func TestPullRequestTestMerge(t *testing.T) {
	t.Parallel()
	c := newClientWith(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/pulls/1":
			_, _ = w.Write([]byte(`{"number":1,"state":"open","merged":false,"merge_commit_sha":"aaaa","merged_by":{"login":"ghost"}}`))
		case "/repos/o/r/pulls/2":
			_, _ = w.Write([]byte(`{"number":2,"state":"closed","merged":true,"merge_commit_sha":"bbbb","merged_by":{"login":"owner"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	ctx := context.Background()
	open, err := c.PullRequest(ctx, "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if open.Merged || open.MergeCommitSHA != "" || open.MergedBy != "" {
		t.Errorf("open %+v; want unmerged, with no merge commit and no merger", open)
	}
	merged, err := c.PullRequest(ctx, "o", "r", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !merged.Merged || merged.MergeCommitSHA != "bbbb" || merged.MergedBy != "owner" {
		t.Errorf("merged %+v; want merged by owner, with merge commit bbbb", merged)
	}
}

// A merge GitHub answers 200 with no sha is an error, as the caller needs
// the squash commit (SPEC §7.5's Carrying the check over).
func TestMergePullRequestNoSHA(t *testing.T) {
	t.Parallel()
	c := newClientWith(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/repos/o/r/pulls/1/merge" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"merged":true,"message":"Pull Request successfully merged"}`))
	}))
	if sha, err := c.MergePullRequest(context.Background(), "o", "r", 1, "cccc", "T", "B"); err == nil || sha != "" {
		t.Errorf("merge answered with no sha: %q, %v; want an error", sha, err)
	}
}
