package daemon

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// DigestHistoryDigests returns the issues d takes as digests, sorted, for
// the title rule's tests in daemon_test.
func DigestHistoryDigests(d *Daemon) []int { return slices.Sorted(maps.Keys(d.digest.issues)) }

// A task in the pipeline renamed as the digest, its origin edited out,
// between a poll and its snapshot's read, so the snapshot has the new
// title and body: the next poll finds it unchanged, so it doesn't mark it
// stale, and the title rule holds it, as its history read fails. The hold
// drops its snapshot and its cached items, and marks it to be read again,
// so once the next read lets it go, as its first version had an origin,
// it's read and visited, rather than left out of the pipeline.
func TestDigestHistoryHoldSnapshotUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("issuedog[bot]", "A task", "Do it.\n\n<!-- issuedog:origin plan=#9 rev=1 item=T1 -->")
	f.EditBody(n, "owner", "Do it.")
	f.RenameIssue(n, "owner", "issuedog digest")
	c := f.Client(t)
	s, err := c.Snapshot(ctx, "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{Step: &Step{Applier: &Applier{Client: c, Owner: "o", Repo: "r", AppSlug: "issuedog"}},
		snaps: map[int]*github.Snapshot{n: s}, stale: map[int]bool{}, digestCache: map[int][]waitItem{n: {{Kind: waitError}}}}
	f.FailGraphQL("Something went wrong")
	if err := d.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, cached := d.digestCache[n]; d.digest.titled[n] == nil || d.snaps[n] != nil || cached || !d.stale[n] {
		t.Errorf("held %v, snapshot %v, cache %v, stale %v; want %d held, with no snapshot or items, and stale",
			d.digest.titled, d.snaps[n], d.digestCache, d.stale, n)
	}
	if err := d.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(d.digest.titled) != 0 || d.digest.has(n) || !d.stale[n] {
		t.Errorf("held %v, digests %v, stale %v; want %d let go, no digest, and stale", d.digest.titled, d.digest.issues, d.stale, n)
	}
}
