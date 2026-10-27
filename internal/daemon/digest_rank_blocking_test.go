package daemon_test

import (
	"fmt"
	"strings"
	"testing"
)

// The digest ranks an issue that blocks open issues above an otherwise
// equal, older one, with them in its count, read from the issue GET the
// pass makes anyway, so no dependency listing is read (SPEC §9.4, The
// order). A closed issue it blocks doesn't count.
func TestDigestRankBlocking(t *testing.T) {
	t.Parallel()
	f := digestPinned(t)
	older := digestError(f, "Older", "5")
	newer := digestError(f, "Newer", "6")
	for _, title := range []string{"Blocked A", "Blocked B", "Blocked and closed"} {
		f.SetBlockedBy(f.AddIssue("owner", title, ""), newer)
	}
	f.Close(len(f.State().Issues))
	from := len(f.Requests())
	iterate(t, digestDaemon(t, f, newOrigin(t), nil))
	body := f.State().Issues[0].Body
	first := fmt.Sprintf("### 1. #%d: Newer\n\n**State error**, priority p2, unblocks 3 open issues (itself included)", newer)
	second := fmt.Sprintf("### 2. #%d: Older\n\n**State error**, priority p2, unblocks 1 open issue (itself included)", older)
	if !strings.Contains(body, first) || !strings.Contains(body, second) {
		t.Errorf("body = %q; want #%d first, unblocking 3, then #%d, unblocking 1", body, newer, older)
	}
	for _, r := range f.Requests()[from:] {
		if strings.Contains(r.Path, "/dependencies/") {
			t.Errorf("the pass sent %s %s; want no dependency listing", r.Method, r.Path)
		}
	}
}
