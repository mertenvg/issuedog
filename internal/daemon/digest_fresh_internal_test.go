package daemon

import (
	"testing"
	"time"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
)

// The 10-minute listing brings a held snapshot's count of the open issues
// its issue blocks up to date from the issue_dependencies_summary it
// lists, and a snapshot listed with no summary keeps the count it has
// (SPEC §9.4, The order). An issue with no snapshot that the digest may
// list items for that aren't cached has what the listing showed recorded
// for its kept items, and one it may not has nothing recorded.
func TestTransferUnlistedBlocking(t *testing.T) {
	t.Parallel()
	opened := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	d := &Daemon{
		snaps:                map[int]*github.Snapshot{7: {Number: 7, Blocking: 2}, 8: {Number: 8, Blocking: 2}},
		digestRestartUnknown: map[int]bool{9: true},
	}
	listed := func(n, blocking int, summary bool) *gogithub.Issue {
		is := &gogithub.Issue{Number: new(n), CreatedAt: &gogithub.Timestamp{Time: opened}}
		if summary {
			is.IssueDependenciesSummary = &gogithub.IssueDependenciesSummary{Blocking: new(blocking)}
		}
		return is
	}
	for _, is := range []*gogithub.Issue{listed(7, 3, true), listed(8, 0, false), listed(9, 4, true), listed(10, 5, true)} {
		d.transferUnlistedBlocking(is)
	}
	if got := d.snaps[7].Blocking; got != 3 {
		t.Errorf("#7's Blocking = %d; want 3, as listed", got)
	}
	if got := d.snaps[8].Blocking; got != 2 {
		t.Errorf("#8's Blocking = %d; want 2 kept, as it's listed with no summary", got)
	}
	want := map[int]digestKeptListing{9: {Created: opened, Blocking: 4}}
	if len(d.digestKeptListed) != len(want) || d.digestKeptListed[9] != want[9] {
		t.Errorf("recorded for kept items %v; want %v", d.digestKeptListed, want)
	}
}
