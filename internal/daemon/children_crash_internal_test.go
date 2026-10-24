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

// childrenCrashRevised is planTestPlanned's issue 7, sent back to planning
// by the owner (104), then planned again as plan revision 2 (105), which
// the owner's 👍 approved (106), so its plan was first approved at
// revision 1 (103) and again at revision 2.
func childrenCrashRevised(t *testing.T) *github.Snapshot {
	t.Helper()
	s := planTestPlanned(t, 7, "epic", false, "app: from=planned to=planning event=11 by=owner")
	body, err := planCommentBody(goldenPlan(t, "epic-plan"), planFacts{Issue: 7, Rev: 2, MaxTasks: 10, Priority: "p2"})
	if err != nil {
		t.Fatal(err)
	}
	ms := github.ParseMarkers(body)
	s.Comments = append(s.Comments, github.Comment{ID: 105, Author: planTestApp, Body: body}, github.Comment{ID: 106, Author: planTestApp,
		Body: fmt.Sprintf("Approved.\n\n<!-- issuedog:transition from=plan-review to=planned cmd=r10 rev=2 body=%s seen=105 -->",
			ms[len(ms)-1].Fields["body"])})
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

// The children are listed since the plan's first approval: the earliest
// clean App comment holding a move into planned that names a plan
// revision, any of them, or the latest approval's, if that's earlier (SPEC
// §5.2, "Finding a child, without search"). An approval of an earlier
// revision counts, so a revision's kept child that someone unlinked by
// hand is still found (#362). A dirty comment, a person's marker, and a
// move into planned that names no revision, as from a hold, or a rev that
// isn't one, don't count. A revision is planned against the children
// listed since then too, as for a plan approved at revision 1 and again
// at revision 2.
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
	noRev := github.Comment{ID: 99, Author: planTestApp, Body: "Approved.\n\n<!-- issuedog:transition from=plan-review to=planned rev=x cmd=r8 -->"}
	// revised is reapproved with its latest approval, 105, approving
	// revision 2, so 103 approved an earlier revision than the one
	// approved now.
	revised := childrenCrashReapproved(t)
	last := &revised.Comments[len(revised.Comments)-1]
	last.Body = strings.Replace(last.Body, "rev=1", "rev=2", 1)
	for _, tc := range []struct {
		name         string
		s            *github.Snapshot
		dirty        func(int64) bool
		move, wantID int64 // the latest approval's comment, and the one since which the children are listed
	}{
		{"one approval", childrenCrashTimed(planTestPlanned(t, 7, "epic", false)), clean, 103, 103},
		{"approved again", reapproved, clean, 105, 103},
		{"the first approval dirty", reapproved, func(id int64) bool { return id == 103 }, 105, 105},
		{"a move with no revision before it", before(held), clean, 105, 103},
		{"a person's marker before it", before(byOwner), clean, 105, 103},
		{"a move whose rev isn't a revision before it", before(noRev), clean, 105, 103},
		{"an earlier revision's approval before it", revised, clean, 105, 103},
		{"an earlier revision's approval dirty", revised, func(id int64) bool { return id == 103 }, 105, 105},
	} {
		got := childrenSince(tc.s, "issuedog", tc.dirty, childrenCrashComment(t, tc.s, tc.move))
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
	// A plan approved at revision 1, then again at revision 2, is a
	// revision of revision 2, moved by its approval, with the children
	// listed since revision 1's.
	revisedAgain := childrenCrashRevised(t)
	pr2, err := planPriorOf(revisedAgain, "issuedog", state.Derive(revisedAgain, "issuedog", clean), clean)
	if err != nil || pr2 == nil {
		t.Fatalf("planPriorOf after revision 2's approval: %+v, %v; want a revision's prior", pr2, err)
	}
	if first2 := childrenCrashComment(t, revisedAgain, 103).CreatedAt; pr2.Rev != 2 || pr2.move.ID != 106 || !pr2.since.Equal(first2) {
		t.Errorf("planPriorOf after revision 2's approval: revision %d, moved by comment %d, the children listed since %s; "+
			"want revision 2, moved by 106, since %s, revision 1's approval", pr2.Rev, pr2.move.ID, pr2.since, first2)
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
