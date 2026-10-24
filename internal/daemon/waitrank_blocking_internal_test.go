package daemon

import (
	"fmt"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
)

// An item whose issue blocks open issues scores them too, from its
// snapshot's Blocking, on top of the open issues below it (SPEC §9.4, The
// order): one blocking three ranks above an otherwise equal, older one
// that blocks none, and one blocking two with an open child scores as
// one blocking three. An item whose issue has no snapshot blocks none.
func TestWaitRankBlocking(t *testing.T) {
	t.Parallel()
	at := func(h int) time.Time { return waitT0.Add(time.Duration(h) * time.Hour) }
	snaps := map[int]*github.Snapshot{
		80: {Number: 80},
		81: {Number: 81, Blocking: 3},
		82: {Number: 82, Blocking: 2},
		83: {Number: 83, Parent: 82},
	}
	item := func(n, created int) waitItem {
		return waitItem{Issue: n, Kind: waitError, Key: fmt.Sprintf("err-%d-labels-0", n), Priority: 2, Created: at(created), Comment: 1}
	}
	items := []waitItem{item(90, 5), item(80, 1), item(81, 3), item(82, 2)}
	waitRank(items, snaps)
	want := []struct{ issue, unblocks int }{{82, 4}, {81, 4}, {80, 1}, {90, 1}}
	if len(items) != len(want) {
		t.Fatalf("got %s, want 4 items", waitRankText(items))
	}
	for i, w := range want {
		if items[i].Issue != w.issue || items[i].Unblocks != w.unblocks {
			t.Errorf("got\n%s\nwant #82 (4), #81 (4), #80 (1), then #90 (1)", waitRankText(items))
			break
		}
	}
}
