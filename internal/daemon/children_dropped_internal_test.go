package daemon

import (
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/state"
)

// The comment of a dropped child's close names the revision and the item,
// and for an issue below the child, the child too, and links the plan.
func TestDroppedText(t *testing.T) {
	t.Parallel()
	d := droppedItem{parent: 1, rev: 3, root: 4, item: "S2", plan: "https://github.com/o/r/issues/1#issuecomment-9", key: state.Key{Field: "event", ID: "p8"}}
	for _, tt := range []struct {
		n    int
		want string
	}{
		{4, "Closed as not planned, since #1's plan, rev 3, drops S2 ([the plan](https://github.com/o/r/issues/1#issuecomment-9))."},
		{7, "Closed as not planned, since #1's plan, rev 3, drops S2, #4, which this issue is below ([the plan](https://github.com/o/r/issues/1#issuecomment-9))."},
	} {
		if got := droppedText(d, tt.n); got != tt.want {
			t.Errorf("droppedText for #%d = %q, want %q", tt.n, got, tt.want)
		}
	}
}

// A finished story whose record names no children, after a revision that
// dropped them all (state.CreatedNone), says so, rather than listing none.
func TestDroppedWaitDoneNone(t *testing.T) {
	t.Parallel()
	got := waitDoneLines([]int{})
	if len(got) != 2 || got[1].Text != "Its plan's latest revision left it no children." {
		t.Errorf("waitDoneLines(none) = %+v, want its second line to say it has no children", got)
	}
	if got := waitDoneLines([]int{2, 3}); !slices.ContainsFunc(got, func(l waitLine) bool { return l.Text == "Its children: #2, #3." }) {
		t.Errorf("waitDoneLines(#2, #3) = %+v, want them listed", got)
	}
}
