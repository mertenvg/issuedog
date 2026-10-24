package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// childrenCrashTimed is s with its comments posted a minute apart, in
// order, from a fixed time.
func childrenCrashTimed(s *github.Snapshot) *github.Snapshot {
	for i := range s.Comments {
		s.Comments[i].CreatedAt = time.Date(2026, 10, 1, 12, i, 0, 0, time.UTC)
	}
	return s
}

// childrenCrashReapproved is planTestPlanned's issue 7, sent back to
// plan-review by the owner (104) and its plan approved again by 👍 (105).
func childrenCrashReapproved(t *testing.T) *github.Snapshot {
	t.Helper()
	s := planTestPlanned(t, 7, "epic", false, "app: from=planned to=plan-review event=11 by=owner")
	again := s.Comments[2]
	again.ID, again.Body = 105, strings.Replace(again.Body, "cmd=r9", "cmd=r10", 1)
	s.Comments = append(s.Comments, again)
	return childrenCrashTimed(s)
}

// childrenCrashComment is s's comment id, or fails t.
func childrenCrashComment(t *testing.T, s *github.Snapshot, id int64) github.Comment {
	t.Helper()
	i := slices.IndexFunc(s.Comments, func(c github.Comment) bool { return c.ID == id })
	if i < 0 {
		t.Fatalf("#%d has no comment %d", s.Number, id)
	}
	return s.Comments[i]
}

// The children are listed since the approved revision's first approval:
// the earliest clean App comment holding a move into planned that names
// that revision, or the latest approval's, if that's earlier (SPEC §5.2,
// "Finding a child, without search"). A dirty comment, a person's marker,
// a move into planned that names no revision, as from a hold, and an
// approval of another revision don't count. A revision is planned against
// the children listed since then too.
func TestChildrenSinceFirstApproval(t *testing.T) {
	t.Parallel()
	clean := func(int64) bool { return false }
	reapproved := childrenCrashReapproved(t)
	before := func(c github.Comment) *github.Snapshot {
		s := childrenCrashReapproved(t)
		s.Comments = slices.Insert(s.Comments, 0, c)
		return childrenCrashTimed(s)
	}
	held := github.Comment{ID: 99, Author: planTestApp, Body: "Resumed.\n\n<!-- issuedog:transition from=held to=planned cmd=98 -->"}
	byOwner := github.Comment{ID: 99, Author: github.Author{Login: "owner", Type: "User"},
		Body: "<!-- issuedog:transition from=plan-review to=planned rev=1 cmd=r8 -->"}
	for _, tc := range []struct {
		name         string
		s            *github.Snapshot
		dirty        func(int64) bool
		rev          int
		move, wantID int64 // the latest approval's comment, and the one since which the children are listed
	}{
		{"one approval", childrenCrashTimed(planTestPlanned(t, 7, "epic", false)), clean, 1, 103, 103},
		{"approved again", reapproved, clean, 1, 105, 103},
		{"the first approval dirty", reapproved, func(id int64) bool { return id == 103 }, 1, 105, 105},
		{"a move with no revision before it", before(held), clean, 1, 105, 103},
		{"a person's marker before it", before(byOwner), clean, 1, 105, 103},
		{"approvals of another revision before it", reapproved, clean, 2, 105, 105},
	} {
		got := childrenSince(tc.s, "issuedog", tc.dirty, tc.rev, childrenCrashComment(t, tc.s, tc.move))
		if want := childrenCrashComment(t, tc.s, tc.wantID).CreatedAt; !got.Equal(want) {
			t.Errorf("%s: since %s, want %s, comment %d's", tc.name, got, want, tc.wantID)
		}
	}

	pr, err := planPriorOf(reapproved, "issuedog", state.Derive(reapproved, "issuedog", clean), clean)
	if err != nil || pr == nil {
		t.Fatalf("planPriorOf: %+v, %v; want a revision's prior", pr, err)
	}
	first := childrenCrashComment(t, reapproved, 103).CreatedAt
	if pr.move.ID != 105 || !pr.since.Equal(first) {
		t.Errorf("planPriorOf: the move is comment %d, and the children are listed since %s; want 105, and since %s", pr.move.ID, pr.since, first)
	}
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("owner", "Plan me", "body")
	st := &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}}
	if err := pr.read(context.Background(), st, &github.Snapshot{Number: 1}); err != nil {
		t.Fatal(err)
	}
	since := "since=" + url.QueryEscape(first.UTC().Format(time.RFC3339))
	if !slices.ContainsFunc(f.Requests(), func(r githubtest.Request) bool {
		return r.Method == http.MethodGet && r.Path == "/issues" && strings.Contains(r.Query, since)
	}) {
		t.Errorf("planPrior.read listed no issues since the first approval, %s: %+v", first, f.Requests())
	}
}

// The children's state error ends at a transition marker in a later
// comment, as a transition error does, so the parent's next move ends it,
// and an ack ends it too (SPEC §5.3, Open).
func TestChildrenErrorOpen(t *testing.T) {
	t.Parallel()
	const toPlanned = "<!-- issuedog:transition from=plan-review to=planned rev=1 body=abcd1234 cmd=r9 -->"
	stuck := openErrMarker(childrenErrKind, "1", "")
	none := func(int64) bool { return false }
	for _, tc := range []struct {
		name string
		s    *github.Snapshot
		want []string // kind/key@comment
	}{
		{"posted at planned", openErrSnap("", toPlanned, stuck), []string{"children/1@2"}},
		{"the parent's next move ends it", openErrSnap("", toPlanned, stuck,
			"<!-- issuedog:transition from=planned to=held at=planned reason=owner cmd=7 -->"), nil},
		{"an ack ends it", openErrSnap("", toPlanned, stuck, openErrMarker(childrenErrKind, "1", "r8")), nil},
	} {
		open, err := openErrors(tc.s, state.Derive(tc.s, openErrSlug, none), func() ([]github.Comment, error) { return nil, nil }, nil)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		var got []string
		for _, e := range open {
			got = append(got, fmt.Sprintf("%s/%s@%d", e.Kind, e.Key, e.Comment))
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: open errors = %q, want %q", tc.name, got, tc.want)
		}
	}
}
