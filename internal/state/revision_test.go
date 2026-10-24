package state

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
)

func TestBodyHash(t *testing.T) {
	// printf 'a\nb\n' | sha256sum
	if got := BodyHash("a\nb\n"); got != "911169dd" {
		t.Errorf("BodyHash = %s, want 911169dd", got)
	}
	for _, body := range []string{"a\r\nb\r\n", "a\rb\r"} {
		if got := BodyHash(body); got != "911169dd" {
			t.Errorf("BodyHash(%q) = %s, want its line endings normalised", body, got)
		}
	}
	for _, body := range []string{"a\nb", "a\nb\n<!-- issuedog:origin plan=#1 rev=1 item=T1 -->", ""} {
		if BodyHash(body) == "911169dd" {
			t.Errorf("BodyHash(%q) matches a\\nb\\n", body)
		}
	}
}

func TestLatestRevision(t *testing.T) {
	const (
		rev1 = "<!-- issuedog:revision rev=1 body=aaaaaaaa -->"
		rev2 = "<!-- issuedog:transition from=clarifying to=spec-review rev=2 body=bbbbbbbb run=x -->\n<!-- issuedog:revision rev=2 body=bbbbbbbb -->"
	)
	for _, tt := range []struct {
		name  string
		s     *github.Snapshot
		dirty func(int64) bool
		want  Revision
		ok    bool
	}{
		{"none", snap("", []github.Author{appBot}, "<!-- issuedog:transition from=intake to=clarifying run=x -->"), clean, Revision{}, false},
		{"the last comment's", snap("", []github.Author{appBot, appBot, owner}, rev1, rev2, "hi"), clean,
			Revision{Rev: 2, Hash: "bbbbbbbb", Comment: 2}, true},
		{"a dirty one isn't a revision, and hides the one before it", snap("", []github.Author{appBot, appBot}, rev1, rev2), dirtyIDs(2),
			Revision{Rev: 1, Hash: "aaaaaaaa", Comment: 1, Hidden: true}, true},
		{"any dirty App comment after it hides it, whatever it says", snap("", []github.Author{appBot, appBot}, rev1, "a reply"), dirtyIDs(2),
			Revision{Rev: 1, Hash: "aaaaaaaa", Comment: 1, Hidden: true}, true},
		{"the last dirty App comment decides", snap("", []github.Author{appBot, appBot, appBot}, "x", rev1, "y"), dirtyIDs(1, 3),
			Revision{Rev: 1, Hash: "aaaaaaaa", Comment: 2, Hidden: true}, true},
		{"a dirty comment by anyone else doesn't hide it", snap("", []github.Author{appBot, owner}, rev1, rev2), dirtyIDs(2),
			Revision{Rev: 1, Hash: "aaaaaaaa", Comment: 1}, true},
		{"a dirty App comment before it doesn't hide it", snap("", []github.Author{appBot, appBot}, rev2, rev1), dirtyIDs(1),
			Revision{Rev: 1, Hash: "aaaaaaaa", Comment: 2}, true},
		{"only the App's count", snap("", []github.Author{appBot, owner}, rev1, rev2), clean, Revision{Rev: 1, Hash: "aaaaaaaa", Comment: 1}, true},
		{"the body's don't count", snap(rev1, nil), clean, Revision{}, false},
		{"only revision markers count, not others with rev and body",
			snap("", []github.Author{appBot, appBot}, rev1, "<!-- issuedog:transition from=spec-review to=planning rev=1 body=aaaaaaaa cmd=r9 -->"),
			clean, Revision{Rev: 1, Hash: "aaaaaaaa", Comment: 1}, true},
		{"a malformed one is skipped", snap("", []github.Author{appBot, appBot}, rev1, "<!-- issuedog:revision rev=x body=cccccccc -->"), clean,
			Revision{Rev: 1, Hash: "aaaaaaaa", Comment: 1}, true},
	} {
		got, ok := LatestRevision(tt.s, slug, SpecRevision, tt.dirty)
		if got != tt.want || ok != tt.ok {
			t.Errorf("%s: LatestRevision = %+v, %v, want %+v, %v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
}

// planComment is a plan comment, plan revision rev, as Transition writes
// it: text, a transition marker, then the plan revision marker with the
// hash of everything above it.
func planComment(text string, rev int) string {
	n := strconv.Itoa(rev)
	above := text + "\n\n<!-- issuedog:transition from=planning to=plan-review rev=" + n + " run=p" + n + " -->"
	return above + "\n<!-- issuedog:revision kind=plan rev=" + n + " body=" + BodyHash(above) + " -->"
}

// Each kind is read, numbered, dirty and hidden on its own (SPEC §7.4): a
// reader of one kind never sees the other's markers.
func TestLatestRevisionByKind(t *testing.T) {
	t.Parallel()
	const (
		spec1 = "<!-- issuedog:revision rev=1 body=aaaaaaaa -->"
		spec2 = "<!-- issuedog:revision rev=2 body=bbbbbbbb -->"
		odd   = "<!-- issuedog:revision kind=other rev=9 body=cccccccc -->"
	)
	plan1, plan2 := planComment("Plan one.", 1), planComment("Plan two.", 2)
	hash := func(body string) string {
		h, ok := PlanHash(body)
		if !ok {
			t.Fatalf("PlanHash(%q) isn't a plan comment", body)
		}
		return h
	}
	app := func(n int) []github.Author {
		by := make([]github.Author, n)
		for i := range by {
			by[i] = appBot
		}
		return by
	}
	for _, tt := range []struct {
		name       string
		s          *github.Snapshot
		dirty      func(int64) bool
		spec, plan Revision
	}{
		{"none of either", snap("", app(1), "hi"), clean, Revision{}, Revision{Kind: PlanRevision}},
		{"a plan revision after a spec revision is no spec revision", snap("", app(2), spec1, plan1), clean,
			Revision{Rev: 1, Hash: "aaaaaaaa", Comment: 1}, Revision{Kind: PlanRevision, Rev: 1, Hash: hash(plan1), Comment: 2}},
		{"a spec revision after a plan revision is no plan revision", snap("", app(3), spec1, plan1, spec2), clean,
			Revision{Rev: 2, Hash: "bbbbbbbb", Comment: 3}, Revision{Kind: PlanRevision, Rev: 1, Hash: hash(plan1), Comment: 2}},
		{"only plan revisions: no spec revision", snap("", app(2), plan1, plan2), clean,
			Revision{}, Revision{Kind: PlanRevision, Rev: 2, Hash: hash(plan2), Comment: 2}},
		{"an unknown kind is neither", snap("", app(2), spec1, odd), clean,
			Revision{Rev: 1, Hash: "aaaaaaaa", Comment: 1}, Revision{Kind: PlanRevision}},
		{"a dirty plan comment is no revision, and hides those before it", snap("", app(3), plan1, spec1, plan2), dirtyIDs(3),
			Revision{Rev: 1, Hash: "aaaaaaaa", Comment: 2, Hidden: true}, Revision{Kind: PlanRevision, Rev: 1, Hash: hash(plan1), Comment: 1, Hidden: true}},
		{"a dirty App comment after a plan revision hides it", snap("", app(2), plan1, "a reply"), dirtyIDs(2),
			Revision{Hidden: true}, Revision{Kind: PlanRevision, Rev: 1, Hash: hash(plan1), Comment: 1, Hidden: true}},
		{"a dirty App comment before a plan revision doesn't hide it", snap("", app(2), "a reply", plan1), dirtyIDs(1),
			Revision{Hidden: true}, Revision{Kind: PlanRevision, Rev: 1, Hash: hash(plan1), Comment: 2}},
		{"a dirty comment by anyone else doesn't hide it", snap("", []github.Author{appBot, owner}, plan1, "a reply"), dirtyIDs(2),
			Revision{}, Revision{Kind: PlanRevision, Rev: 1, Hash: hash(plan1), Comment: 1}},
		{"only the App's plan revisions count", snap("", []github.Author{appBot, owner}, plan1, plan2), clean,
			Revision{}, Revision{Kind: PlanRevision, Rev: 1, Hash: hash(plan1), Comment: 1}},
	} {
		if got, ok := LatestRevision(tt.s, slug, SpecRevision, tt.dirty); got != tt.spec || ok != (tt.spec.Comment != 0) {
			t.Errorf("%s: LatestRevision(spec) = %+v, %v, want %+v", tt.name, got, ok, tt.spec)
		}
		if got, ok := LatestRevision(tt.s, slug, PlanRevision, tt.dirty); got != tt.plan || ok != (tt.plan.Comment != 0) {
			t.Errorf("%s: LatestRevision(plan) = %+v, %v, want %+v", tt.name, got, ok, tt.plan)
		}
	}
}

// A dirty plan comment is no revision, so the next plan revision posted
// takes its number; and a deleted App comment hides a plan revision as it
// does a spec revision (SPEC §7.4).
func TestLatestRevisionPlanDirtyAndDeleted(t *testing.T) {
	t.Parallel()
	s := snap("", []github.Author{appBot, appBot}, planComment("Plan one.", 1), planComment("Plan two.", 2))
	if got, _ := LatestRevision(s, slug, PlanRevision, dirtyIDs(2)); got.Rev != 1 || !got.Hidden {
		t.Errorf("LatestRevision with plan revision 2 dirty = %+v, want revision 1, hidden", got)
	}
	posted := time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)
	s.Comments[0].CreatedAt, s.Comments[1].CreatedAt = posted, posted.Add(time.Hour)
	s.Deleted = []github.DeletedComment{{ID: "CDE_1", By: owner, Author: appBot, At: posted.Add(2 * time.Hour)}}
	if got, _ := LatestRevision(s, slug, PlanRevision, clean); got.Rev != 2 || !got.Hidden {
		t.Errorf("LatestRevision with an App comment deleted after plan revision 2 = %+v, want revision 2, hidden", got)
	}
	s.Deleted[0].At = posted.Add(30 * time.Minute)
	if got, _ := LatestRevision(s, slug, PlanRevision, clean); got.Rev != 2 || got.Hidden {
		t.Errorf("LatestRevision with an App comment deleted before plan revision 2 = %+v, want revision 2, not hidden", got)
	}
}

func TestPlanHash(t *testing.T) {
	t.Parallel()
	above := "The plan.\n\n<!-- issuedog:transition from=planning to=plan-review rev=1 run=p -->\n<!-- issuedog:plan-data v=1 -->"
	want := BodyHash(above)
	marker := "<!-- issuedog:revision kind=plan rev=1 body=" + want + " -->"
	body := above + "\n" + marker
	if got, ok := PlanHash(body); got != want || !ok {
		t.Errorf("PlanHash = %s, %v, want %s, true", got, ok, want)
	}
	// A web edit may turn line endings into CRLF: they're normalised.
	if got, ok := PlanHash(strings.ReplaceAll(body, "\n", "\r\n")); got != want || !ok {
		t.Errorf("PlanHash with CRLF = %s, %v, want %s, true", got, ok, want)
	}
	if got, ok := PlanHash(strings.ReplaceAll(body, "\n", "\r")); got != want || !ok {
		t.Errorf("PlanHash with CR = %s, %v, want %s, true", got, ok, want)
	}
	// Any edit above the marker changes the hash, the markers included.
	for _, edited := range []string{
		strings.Replace(body, "The plan.", "The plan!", 1),
		strings.Replace(body, "v=1", "v=2", 1),
		"Another line.\n" + body,
	} {
		if got, ok := PlanHash(edited); got == want || !ok {
			t.Errorf("PlanHash(%q) = %s, %v, want another hash, true", edited, got, ok)
		}
	}
	// The last line must be a plan revision marker, and nothing else.
	for _, bad := range []string{
		marker,          // no line above it
		body + "\n",     // an empty line after it
		body + "\nmore", // text after it
		above + "\n" + marker + " and text",
		above + "\nsee " + marker,
		above + "\n<!-- issuedog:revision rev=1 body=" + want + " -->",            // a spec revision
		above + "\n<!-- issuedog:revision kind=other rev=1 body=" + want + " -->", // another kind
		above + "\n<!-- issuedog:transition from=planning to=plan-review kind=plan rev=1 body=" + want + " run=p -->",
		above + "\n" + marker + marker, // two markers
		"",
	} {
		if got, ok := PlanHash(bad); ok {
			t.Errorf("PlanHash(%q) = %s, true, want false", bad, got)
		}
	}
	// Space around the marker on its line is no edit to the marker.
	if got, ok := PlanHash(above + "\n " + marker + " "); got != want || !ok {
		t.Errorf("PlanHash with spaces around the marker = %s, %v, want %s, true", got, ok, want)
	}
}

func TestRevisionMatches(t *testing.T) {
	t.Parallel()
	plan := planComment("The plan.", 1)
	hash, ok := PlanHash(plan)
	if !ok {
		t.Fatal("planComment isn't a plan comment")
	}
	s := snap("the spec", []github.Author{appBot, appBot}, "<!-- issuedog:revision rev=1 body="+BodyHash("the spec")+" -->", plan)
	withBody := func(body string) *github.Snapshot {
		c := *s
		c.Body = body
		return &c
	}
	withPlan := func(body string) *github.Snapshot {
		c := *s
		c.Comments = slices.Clone(s.Comments)
		c.Comments[1].Body = body
		return &c
	}
	gone := *s
	gone.Comments = s.Comments[:1]
	specRev := Revision{Rev: 1, Hash: BodyHash("the spec"), Comment: 1}
	planRev := Revision{Kind: PlanRevision, Rev: 1, Hash: hash, Comment: 2}
	for _, tt := range []struct {
		name string
		s    *github.Snapshot
		r    Revision
		want bool
	}{
		{"a spec revision matching the body", s, specRev, true},
		{"a spec revision after the body changed", withBody("a new spec"), specRev, false},
		{"a spec revision after the plan comment changed", withPlan("a new plan"), specRev, true},
		{"a plan revision matching its comment", s, planRev, true},
		{"a plan revision doesn't read the body", withBody("a new spec"), planRev, true},
		{"a plan revision whose comment changed", withPlan(strings.Replace(plan, "The plan.", "Another plan.", 1)), planRev, false},
		{"a plan revision whose comment lost its marker", withPlan(plan + "\nmore"), planRev, false},
		{"a plan revision whose comment is gone", &gone, planRev, false},
		{"a plan revision reads its own comment, not another", s, Revision{Kind: PlanRevision, Rev: 1, Hash: hash, Comment: 1}, false},
	} {
		if got := tt.r.Matches(tt.s); got != tt.want {
			t.Errorf("%s: Matches = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// planMove is a plan comment, plan revision rev, whose move goes from
// planning to to, as the planner's Transition writes it with Move.PlanRev
// and a plan-data marker: text, the transition marker with rev but no
// body, the plan-data marker, then the plan revision marker with the hash
// of everything above it. to is plan-review, or planned for a story whose
// plan skips review.
func planMove(text string, rev int, to Stage) string {
	n := strconv.Itoa(rev)
	above := text + "\n\n<!-- issuedog:transition from=planning to=" + string(to) + " rev=" + n + " run=p" + n + " -->" +
		"\n<!-- issuedog:plan-data v=1 data=AAAA -->"
	return above + "\n<!-- issuedog:revision kind=plan rev=" + n + " body=" + BodyHash(above) + " -->"
}

// approval is the move into planned that approves plan revision rev, of
// plan comment plan, from plan-review (SPEC §5, §7.4): rev, body, cmd and
// seen.
func approval(t *testing.T, plan string, rev int) string {
	t.Helper()
	hash, ok := PlanHash(plan)
	if !ok {
		t.Fatalf("approval of no plan comment: %q", plan)
	}
	return "Plan revision " + strconv.Itoa(rev) + " is approved.\n\n<!-- issuedog:transition from=plan-review to=planned rev=" +
		strconv.Itoa(rev) + " body=" + hash + " cmd=r5 seen=9 -->"
}

// apps is n authors, each the App.
func apps(n int) []github.Author {
	by := make([]github.Author, n)
	for i := range by {
		by[i] = appBot
	}
	return by
}

// planRev is plan revision rev, with hash, in comment id.
func planRev(t *testing.T, body string, id int64, rev int) Revision {
	t.Helper()
	hash, ok := PlanHash(body)
	if !ok {
		t.Fatalf("comment %d isn't a plan comment: %q", id, body)
	}
	return Revision{Kind: PlanRevision, Rev: rev, Hash: hash, Comment: id}
}

// PlanApproval gives the latest plan revision, and each reason it can't be
// approved, in a fixed order (SPEC §7.4): hidden, changed, a dirty body.
// With no plan revision, that's the only reason.
func TestPlanApproval(t *testing.T) {
	t.Parallel()
	const spec1 = "<!-- issuedog:revision rev=1 body=aaaaaaaa -->"
	plan1, plan2 := planComment("Plan one.", 1), planComment("Plan two.", 2)
	edited := strings.Replace(plan1, "Plan one.", "Plan one, edited.", 1)
	withComment := func(s *github.Snapshot, id int64, body string) *github.Snapshot {
		s.Comments[id-1].Body = body
		return s
	}
	deleted := func(s *github.Snapshot) *github.Snapshot {
		posted := time.Date(2026, 10, 18, 1, 0, 0, 0, time.UTC)
		for i := range s.Comments {
			s.Comments[i].CreatedAt = posted.Add(time.Duration(i) * time.Minute)
		}
		s.Deleted = []github.DeletedComment{{ID: "CDE_1", By: owner, Author: appBot, At: posted.Add(time.Hour)}}
		return s
	}
	for _, tt := range []struct {
		name  string
		s     *github.Snapshot
		dirty func(int64) bool
		plan  string // the latest plan revision's comment as posted, or "" for none
		id    int64  // its ID
		rev   int
		hide  bool
		why   []PlanReason
	}{
		{"an approvable plan", snap("the spec", apps(2), spec1, plan1), clean, plan1, 2, 1, false, nil},
		{"the latest of two", snap("the spec", apps(3), spec1, plan1, plan2), clean, plan2, 3, 2, false, nil},
		{"a trusted edit to the body changes nothing", snap("an edited spec", apps(2), spec1, plan1), clean, plan1, 2, 1, false, nil},
		{"a dirty plan comment is no revision, and hides the one before it", snap("", apps(2), plan1, plan2), dirtyIDs(2),
			plan1, 1, 1, true, []PlanReason{PlanHidden}},
		{"a dirty App comment after the plan hides it", snap("", apps(2), plan1, "a reply"), dirtyIDs(2),
			plan1, 1, 1, true, []PlanReason{PlanHidden}},
		{"an App comment deleted after the plan hides it", deleted(snap("", apps(2), spec1, plan1)), clean,
			plan1, 2, 1, true, []PlanReason{PlanHidden}},
		{"a dirty comment by anyone else doesn't hide it", snap("", []github.Author{appBot, owner}, plan1, "a reply"), dirtyIDs(2),
			plan1, 1, 1, false, nil},
		{"a plan comment that no longer matches its hash", withComment(snap("", apps(2), spec1, plan1), 2, edited), clean,
			plan1, 2, 1, false, []PlanReason{PlanChanged}},
		{"a plan comment that lost its last line", withComment(snap("", apps(2), spec1, plan1), 2, plan1+"\nmore"), clean,
			plan1, 2, 1, false, []PlanReason{PlanChanged}},
		{"a dirty body", snap("the spec", apps(2), spec1, plan1), dirtyIDs(BodyID), plan1, 2, 1, false, []PlanReason{PlanBodyDirty}},
		{"every reason, in order", withComment(snap("", apps(3), spec1, plan1, "a reply"), 2, edited), dirtyIDs(BodyID, 3),
			plan1, 2, 1, true, []PlanReason{PlanHidden, PlanChanged, PlanBodyDirty}},
		{"no plan revision", snap("the spec", apps(2), spec1, "a reply"), clean, "", 0, 0, false, []PlanReason{PlanNoRevision}},
		{"no plan revision is the only reason, with a dirty body", snap("", apps(1), spec1), dirtyIDs(BodyID),
			"", 0, 0, false, []PlanReason{PlanNoRevision}},
		// LatestRevision reports this one Hidden, with no revision before it.
		{"a dirty plan comment with none before it: no plan revision", snap("", apps(2), spec1, plan1), dirtyIDs(2),
			"", 0, 0, false, []PlanReason{PlanNoRevision}},
		{"a plan revision by anyone else is none", snap("", []github.Author{appBot, owner}, spec1, plan1), clean,
			"", 0, 0, false, []PlanReason{PlanNoRevision}},
		{"spec and plan revisions interleaved: a spec revision after the plan isn't it",
			snap("", apps(3), spec1, plan1, "<!-- issuedog:revision rev=2 body=bbbbbbbb -->"), clean,
			plan1, 2, 1, false, nil},
		{"only spec revisions: no plan revision",
			snap("", apps(2), spec1, "<!-- issuedog:revision rev=2 body=bbbbbbbb -->"), clean,
			"", 0, 0, false, []PlanReason{PlanNoRevision}},
	} {
		want := Revision{Kind: PlanRevision}
		if tt.plan != "" {
			want = planRev(t, tt.plan, tt.id, tt.rev)
			want.Hidden = tt.hide
		}
		got, why := PlanApproval(tt.s, slug, tt.dirty)
		if got != want || !slices.Equal(why, tt.why) {
			t.Errorf("%s: PlanApproval = %+v, %v, want %+v, %v", tt.name, got, why, want, tt.why)
		}
	}
}

// ApprovedPlan names the plan revision the latest move into planned
// carries, from plan-review or, for a plan that skips review, from
// planning, with its comment; and nothing once that comment is gone,
// dirty or edited (SPEC §7.4).
func TestApprovedPlan(t *testing.T) {
	t.Parallel()
	const (
		spec1     = "<!-- issuedog:revision rev=1 body=aaaaaaaa -->"
		spec2     = "<!-- issuedog:revision rev=2 body=bbbbbbbb -->"
		toPlan    = "<!-- issuedog:transition from=spec-review to=planning rev=1 body=aaaaaaaa cmd=r3 seen=1 -->"
		hold      = "<!-- issuedog:transition from=planned to=held at=planned reason=owner cmd=20 -->"
		resume    = "<!-- issuedog:transition from=held to=planned cmd=21 -->"
		noBody    = "<!-- issuedog:transition from=plan-review to=planned rev=1 cmd=r6 -->"
		otherRev  = "<!-- issuedog:transition from=plan-review to=planned rev=3 body=cccccccc cmd=r6 -->"
		specMatch = "<!-- issuedog:transition from=plan-review to=planned rev=1 body=aaaaaaaa cmd=r6 -->"
	)
	plan1, plan2 := planComment("Plan one.", 1), planComment("Plan two.", 2)
	skip1 := planMove("Plan one.", 1, Planned)
	edited := strings.Replace(plan1, "Plan one.", "Plan one, edited.", 1)
	gone := func(s *github.Snapshot, id int64) *github.Snapshot {
		s.Comments = slices.DeleteFunc(s.Comments, func(c github.Comment) bool { return c.ID == id })
		return s
	}
	withComment := func(s *github.Snapshot, id int64, body string) *github.Snapshot {
		s.Comments[id-1].Body = body
		return s
	}
	for _, tt := range []struct {
		name  string
		s     *github.Snapshot
		dirty func(int64) bool
		id    int64 // the approved plan comment, or 0 for none
		rev   int
	}{
		{"a move from plan-review", snap("", apps(4), spec1, toPlan, plan1, approval(t, plan1, 1)), clean, 3, 1},
		{"a move from planning, skipping review", snap("", apps(3), spec1, toPlan, skip1), clean, 3, 1},
		{"the revision the move names, not the latest", snap("", apps(4), spec1, plan1, plan2, approval(t, plan1, 1)), clean, 2, 1},
		{"the latest move counts", snap("", apps(4), plan1, approval(t, plan1, 1), plan2, approval(t, plan2, 2)), clean, 3, 2},
		{"a dirty comment after it doesn't unapprove it", snap("", apps(3), plan1, approval(t, plan1, 1), "a reply"), dirtyIDs(3), 1, 1},
		{"a trusted edit to the body doesn't unapprove it", snap("an edited spec", apps(2), plan1, approval(t, plan1, 1)), clean, 1, 1},
		{"a move back from a hold names nothing, so the move before it counts",
			snap("", apps(4), plan1, approval(t, plan1, 1), hold, resume), clean, 1, 1},
		{"a later move with rev but no body names nothing", snap("", apps(3), plan1, approval(t, plan1, 1), noBody), clean, 1, 1},
		{"spec and plan revisions interleaved", snap("", apps(5), spec1, plan1, spec2, plan2, approval(t, plan2, 2)), clean, 4, 2},
		{"no move into planned", snap("", apps(3), spec1, toPlan, plan1), clean, 0, 0},
		{"a move into planned with rev but no body, from plan-review", snap("", apps(2), plan1, noBody), clean, 0, 0},
		{"a move naming no plan revision there is", snap("", apps(2), plan1, otherRev), clean, 0, 0},
		{"a move naming a spec revision's hash", snap("", apps(2), spec1, specMatch), clean, 0, 0},
		{"a spec approval is no move into planned", snap("", apps(2), spec1, toPlan), clean, 0, 0},
		{"a dirty move", snap("", apps(2), plan1, approval(t, plan1, 1)), dirtyIDs(2), 0, 0},
		{"a move by anyone else", snap("", []github.Author{appBot, owner}, plan1, approval(t, plan1, 1)), clean, 0, 0},
		{"the approved comment is gone", gone(snap("", apps(2), plan1, approval(t, plan1, 1)), 1), clean, 0, 0},
		{"the approved comment is dirty", snap("", apps(2), plan1, approval(t, plan1, 1)), dirtyIDs(1), 0, 0},
		{"the approved comment no longer matches", withComment(snap("", apps(2), plan1, approval(t, plan1, 1)), 1, edited), clean, 0, 0},
		{"the approved comment, a move from planning, is dirty", snap("", apps(2), spec1, skip1), dirtyIDs(2), 0, 0},
		{"the approved comment, a move from planning, no longer matches",
			withComment(snap("", apps(2), spec1, skip1), 2, strings.Replace(skip1, "Plan one.", "Plan one, edited.", 1)), clean, 0, 0},
		{"a move from planning whose plan revision has another rev names nothing",
			snap("", apps(1), strings.Replace(skip1, "to=planned rev=1", "to=planned rev=2", 1)), clean, 0, 0},
		{"so the move before it counts",
			snap("", apps(3), plan1, approval(t, plan1, 1), strings.Replace(skip1, "to=planned rev=1", "to=planned rev=2", 1)), clean, 1, 1},
		{"the approved comment by anyone else is no plan revision", snap("", []github.Author{owner, appBot}, plan1, approval(t, plan1, 1)), clean, 0, 0},
		{"the last clean comment with the revision counts", snap("", apps(3), plan1, approval(t, plan1, 1), plan1), clean, 3, 1},
	} {
		want := Revision{}
		var wantComment github.Comment
		if tt.id != 0 {
			wantComment = tt.s.Comments[slices.IndexFunc(tt.s.Comments, func(c github.Comment) bool { return c.ID == tt.id })]
			want = planRev(t, wantComment.Body, tt.id, tt.rev)
		}
		got, c, ok := ApprovedPlan(tt.s, slug, tt.dirty)
		if got != want || c.ID != wantComment.ID || c.Body != wantComment.Body || ok != (tt.id != 0) {
			t.Errorf("%s: ApprovedPlan = %+v, comment %d, %v; want %+v, comment %d, %v", tt.name, got, c.ID, ok, want, wantComment.ID, tt.id != 0)
		}
	}
}

// ApprovedPlan's revision has the Seen LatestRevision gives it: the plan
// revision marker's seen, else its comment's transition's.
func TestApprovedPlanSeen(t *testing.T) {
	t.Parallel()
	above := "Plan one.\n\n<!-- issuedog:transition from=planning to=plan-review rev=1 run=p1 seen=4 -->"
	plan := above + "\n<!-- issuedog:revision kind=plan rev=1 body=" + BodyHash(above) + " -->"
	s := snap("", apps(2), plan, approval(t, plan, 1))
	got, _, ok := ApprovedPlan(s, slug, clean)
	latest, lok := LatestRevision(s, slug, PlanRevision, clean)
	if !ok || !lok || got.Seen != 4 || got != latest {
		t.Errorf("ApprovedPlan = %+v, %v; want seen 4, as LatestRevision's %+v, %v", got, ok, latest, lok)
	}
}

// ApprovedPlanMove names the comment holding the move into planned whose
// revision ApprovedPlan returns, for the first moves of the children it
// creates (#276): the approval from plan-review, or the plan comment that
// skips review. It stays that move through a hold, whose move back names
// no revision, and is nothing whenever ApprovedPlan finds nothing.
func TestApprovedPlanMove(t *testing.T) {
	t.Parallel()
	const (
		hold   = "<!-- issuedog:transition from=planned to=held at=planned reason=owner cmd=20 -->"
		resume = "<!-- issuedog:transition from=held to=planned cmd=21 -->"
	)
	plan1, plan2 := planComment("Plan one.", 1), planComment("Plan two.", 2)
	skip1 := planMove("Plan one.", 1, Planned)
	for _, tt := range []struct {
		name  string
		s     *github.Snapshot
		dirty func(int64) bool
		id    int64 // the move's comment, or 0 for none
	}{
		{"an approval from plan-review", snap("", apps(2), plan1, approval(t, plan1, 1)), clean, 2},
		{"a move from planning that skips review", snap("", apps(1), skip1), clean, 1},
		{"through a hold and back", snap("", apps(4), plan1, approval(t, plan1, 1), hold, resume), clean, 2},
		{"the latest approval", snap("", apps(4), plan1, approval(t, plan1, 1), plan2, approval(t, plan2, 2)), clean, 4},
		{"no move into planned", snap("", apps(1), plan1), clean, 0},
		{"a dirty approval", snap("", apps(2), plan1, approval(t, plan1, 1)), dirtyIDs(2), 0},
		{"the approved comment is dirty", snap("", apps(2), plan1, approval(t, plan1, 1)), dirtyIDs(1), 0},
	} {
		got, ok := ApprovedPlanMove(tt.s, slug, tt.dirty)
		if ok != (tt.id != 0) || got.ID != tt.id {
			t.Errorf("%s: ApprovedPlanMove = comment %d, %v; want comment %d, %v", tt.name, got.ID, ok, tt.id, tt.id != 0)
		}
		if rev, _, rok := ApprovedPlan(tt.s, slug, tt.dirty); rok != ok {
			t.Errorf("%s: ApprovedPlan = %+v, %v; want found just when ApprovedPlanMove is", tt.name, rev, rok)
		}
	}
}
