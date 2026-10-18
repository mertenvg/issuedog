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

// Each kind is read, numbered, dirty and hidden on its own (plan #247,
// assumption 2): a reader of one kind never sees the other's markers.
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
