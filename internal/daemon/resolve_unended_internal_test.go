package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// A /hold whose comment is gone, known by its markers alone, counts as
// posted on the farthest issue on the path with its marker
// (resolveUnended, SPEC §6.4): a /resume the App applied there ends it,
// and one above it doesn't.
func TestResolveUnendedFarthestMarker(t *testing.T) {
	t.Parallel()
	at := func(s int) time.Time { return time.Date(2026, 1, 1, 0, 0, s, 0, time.UTC) }
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	owner := github.Author{Login: "owner", Type: "User"}
	mallory := github.Author{Login: "mallory", Type: "User"}
	appSays := func(id int64, fields string) github.Comment {
		return github.Comment{ID: id, Author: app, Body: "<!-- issuedog:transition " + fields + " -->", CreatedAt: at(int(id))}
	}
	// #1, held for discussion by mallory's comment 11, under #2, #3 and #4.
	self := node{s: &github.Snapshot{Number: 1, Parent: 2, Comments: []github.Comment{
		appSays(10, "from=intake to=ready run=a"),
		{ID: 11, Author: mallory, Association: "COLLABORATOR", Body: "Wait.", CreatedAt: at(11)},
		appSays(12, "from=ready to=held at=ready origin=ready reason=discussion event=c11 seen=10"),
	}}, open: true}
	// issue is the ancestor number, its first move at base, held by the
	// owner's hand, or with marked by the gone /hold in comment 20, and,
	// with resumed, its /resume the App applied.
	issue := func(number int, base int64, marked, resumed bool) node {
		cs := []github.Comment{appSays(base, "from=intake to=ready run=a")}
		if marked {
			cs = append(cs, appSays(base+1, "from=ready to=held at=ready reason=owner cmd=20"))
		} else {
			cs = append(cs, appSays(base+1, "from=ready to=held at=ready reason=owner by=owner event=9"))
		}
		if resumed {
			cs = append(cs, github.Comment{ID: base + 2, Author: owner, Association: "OWNER", Body: "/resume", CreatedAt: at(int(base + 2))},
				appSays(base+3, fmt.Sprintf("from=held to=ready cmd=%d", base+2)))
		}
		return node{s: &github.Snapshot{Number: number, Parent: number + 1, Comments: cs}, open: true}
	}
	for _, tt := range []struct {
		name string
		path []node
		left int
	}{
		{"a /resume on the farthest issue with its marker ends it",
			[]node{issue(2, 30, true, false), issue(3, 40, true, true), issue(4, 50, false, false)}, 0},
		{"a /resume above the farthest issue with its marker doesn't",
			[]node{issue(2, 30, true, false), issue(3, 40, true, false), issue(4, 50, false, true)}, 1},
		{"a /resume on an issue without its marker, above the farthest, doesn't",
			[]node{issue(2, 30, true, false), issue(3, 40, false, true), issue(4, 50, false, false)}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := &Applier{Client: noReactions(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}
			clean := func(int64) bool { return false }
			is := state.Derive(self.s, a.AppSlug, clean)
			left, err := a.resolveUnended(context.Background(), self, tt.path, is, resolvePeriodOf(self.s, a.AppSlug, clean), newDiscCounting(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(left) != tt.left || tt.left > 0 && left[0].Fields["cmd"] != "20" {
				t.Errorf("left %v; want %d hold of the /hold in comment 20", left, tt.left)
			}
		})
	}
}

// A gone hold's value names the issue and the deletion's second, and
// nothing else parses: the record that names it is read back only when
// it's well formed (resolvePeriodOf).
func TestResolveGoneParse(t *testing.T) {
	t.Parallel()
	m := resolveGoneHold(12, time.Unix(1767225600, 0))
	if v := m.Fields[resolveGone]; v != "12-1767225600" || resolveHoldKey(m) != "g12-1767225600" {
		t.Fatalf("gone hold %v, key %q; want gone=12-1767225600, key g12-1767225600", m, resolveHoldKey(m))
	}
	if n, when, ok := resolveGoneParse("12-1767225600"); !ok || n != 12 || !when.Equal(time.Unix(1767225600, 0)) {
		t.Errorf("parse = %d, %v, %v; want 12 at 1767225600", n, when, ok)
	}
	for _, v := range []string{"", "12", "12-", "-1767225600", "0-1767225600", "x-1", "12-0", "12-x", "12-1-2"} {
		if _, _, ok := resolveGoneParse(v); ok {
			t.Errorf("parse(%q) is well formed; want not", v)
		}
	}
}
