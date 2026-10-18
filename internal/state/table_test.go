package state

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
)

func TestLookupFindsEveryRow(t *testing.T) {
	// One case per §7.2 row, in SPEC order. Keep in sync with SPEC.md.
	cases := []struct {
		from, to Stage
		class    Class
		trigger  Trigger
	}{
		{Intake, Clarifying, ClassUntyped, TriggerTrusted},
		{Clarifying, Clarifying, ClassBug, TriggerMoreQuestions},
		{Clarifying, SpecReview, ClassOwnerStory, TriggerSpecWritten},
		{Clarifying, Ready, ClassNewOwnerTask, TriggerTaskWritten},
		{Clarifying, Ready, ClassTask, TriggerAnswered},
		{SpecReview, Clarifying, ClassEpic, TriggerSpecRevise},
		{SpecReview, Planning, ClassEpic, TriggerSpecApproved},
		{Planning, Planning, ClassOwnerStory, TriggerPlanQuestions},
		{Planning, PlanReview, ClassStory, TriggerPlanValid},
		{Planning, Planned, ClassStory, TriggerPlanNoReview},
		{PlanReview, Planning, ClassEpic, TriggerPlanRevise},
		{PlanReview, Planned, ClassOwnerStory, TriggerPlanApproved},
		{InProgress, Ready, ClassTask, TriggerParentChangedTask},
		{Held, NeedsHuman, ClassHumanTask, TriggerParentChangedHumanTask},
		{Planned, Planning, ClassStory, TriggerParentChangedStory},
		{PROpen, Held, ClassTask, TriggerDropped},
		{Planned, Closed, ClassStory, TriggerStoryVerified},
		{Planned, Closed, ClassEpic, TriggerChildrenClosed},
		{Ready, InProgress, ClassBug, TriggerScheduled},
		{InProgress, Clarifying, ClassTask, TriggerImplementerQuestion},
		{InProgress, Verifying, ClassTask, TriggerImplemented},
		{InProgress, NeedsHuman, ClassOwnerTask, TriggerImplementerBlocked},
		{Verifying, InProgress, ClassTask, TriggerFindings},
		{Verifying, NeedsHuman, ClassTask, TriggerAttemptCap},
		{Verifying, Clarifying, ClassTask, TriggerVerifierQuestion},
		{Verifying, PROpen, ClassBug, TriggerVerified},
		{Verifying, PROpen, ClassTask, TriggerPersonPushed},
		{PROpen, InProgress, ClassTask, TriggerReviewChanges},
		{PROpen, Verifying, ClassTask, TriggerNewHead},
		{PROpen, PROpen, ClassTask, TriggerMainMoved},
		{PROpen, Closed, ClassBug, TriggerMerged},
		{PROpen, NeedsHuman, ClassTask, TriggerPRClosed},
		{NeedsHuman, Ready, ClassTask, TriggerHandoffDone},
		{NeedsHuman, Closed, ClassHumanTask, TriggerHumanTaskDone},
		{SpecReview, Held, ClassEpic, TriggerHold},
		{Intake, Held, ClassUntyped, TriggerDiscussionHold},
		{Held, Clarifying, ClassStory, TriggerDiscussionResolved},
		{Clarifying, SpecReview, ClassEpic, TriggerDiscussionNoChange},
		{Clarifying, SpecReview, ClassOwnerStory, TriggerSpecRewritten},
		{Clarifying, Planning, ClassStory, TriggerPlanChanged},
		{Clarifying, Ready, ClassBug, TriggerTaskRewritten},
		{Clarifying, NeedsHuman, ClassHumanTask, TriggerHumanTaskRewritten},
		{Held, InProgress, ClassTask, TriggerResume},
		{Held, Clarifying, ClassEpic, TriggerResumeAfterDiscussion},
		{Held, Closed, ClassHumanTask, TriggerClosed},
		{Closed, Held, ClassTask, TriggerReopened},
	}
	if len(cases) != len(table) {
		t.Fatalf("%d cases for %d table rows", len(cases), len(table))
	}
	found := map[Trigger]bool{}
	for _, c := range cases {
		r, err := Lookup(c.from, c.to, c.class, c.trigger)
		if err != nil {
			t.Errorf("Lookup(%s, %s, %s, %s): %v", c.from, c.to, c.class, c.trigger, err)
			continue
		}
		found[r.Trigger()] = true
	}
	if len(found) != len(table) {
		t.Errorf("found %d distinct rows of %d", len(found), len(table))
	}
}

func TestLookupMissing(t *testing.T) {
	for _, c := range []struct {
		name     string
		from, to Stage
		class    Class
		trigger  Trigger
	}{
		{"story with a parent approved", SpecReview, Planning, ClassStory, TriggerSpecApproved},
		{"task gets a spec", Clarifying, SpecReview, ClassTask, TriggerSpecWritten},
		{"owner task gets a spec", Clarifying, SpecReview, ClassOwnerTask, TriggerSpecWritten},
		{"bug before ready answers a task question", Clarifying, Ready, ClassNewBug, TriggerAnswered},
		{"owner task before ready answers a task question", Clarifying, Ready, ClassNewOwnerTask, TriggerAnswered},
		{"bug after ready is rewritten as a new one", Clarifying, Ready, ClassBug, TriggerTaskWritten},
		{"untyped issue leaves clarifying through origin", Clarifying, PROpen, ClassUntyped, TriggerDiscussionNoChange},
		{"untyped issue resumes past clarifying", Held, Ready, ClassUntyped, TriggerResume},
		{"intake to planning", Intake, Planning, ClassEpic, TriggerTrusted},
		{"wrong trigger", Verifying, PROpen, ClassTask, TriggerFindings},
		{"bug approved", SpecReview, Planning, ClassBug, TriggerSpecApproved},
		{"untyped leaves clarifying", Clarifying, Ready, ClassUntyped, TriggerTaskWritten},
		{"human task scheduled", Ready, InProgress, ClassHumanTask, TriggerScheduled},
		{"blocked task closed by /done", NeedsHuman, Closed, ClassTask, TriggerHumanTaskDone},
		{"resume to held", Held, Held, ClassTask, TriggerResume},
		{"placeholder as input", Held, PreviousAt, ClassTask, TriggerResume},
		{"closed is not open", Closed, Held, ClassTask, TriggerHold},
	} {
		if _, err := Lookup(c.from, c.to, c.class, c.trigger); !errors.Is(err, ErrNoTransition) {
			t.Errorf("%s: got %v, want ErrNoTransition", c.name, err)
		}
	}
}

// TestLookupPlanQuestions: the planner's question round, planning →
// planning, is for an epic and a story with or without a parent, and for
// no task, bug or untyped issue (#253). It's only a planner round's: the
// interviewer's trigger doesn't move planning to planning, and the
// planner's doesn't move clarifying to clarifying.
func TestLookupPlanQuestions(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		class Class
		ok    bool
	}{
		{"epic", ClassEpic, true},
		{"story with a parent", ClassStory, true},
		{"story with no parent", ClassOwnerStory, true},
		{"task", ClassTask, false},
		{"owner task", ClassOwnerTask, false},
		{"human task", ClassHumanTask, false},
		{"bug", ClassBug, false},
		{"new bug", ClassNewBug, false},
		{"new owner task", ClassNewOwnerTask, false},
		{"untyped", ClassUntyped, false},
	} {
		r, err := Lookup(Planning, Planning, c.class, TriggerPlanQuestions)
		switch {
		case c.ok && err != nil:
			t.Errorf("%s: Lookup = %v, want the row", c.name, err)
		case c.ok && (r.From() != Planning || r.To() != Planning || r.Trigger() != TriggerPlanQuestions):
			t.Errorf("%s: Lookup = %s → %s on %s, want planning → planning on %s", c.name, r.From(), r.To(), r.Trigger(), TriggerPlanQuestions)
		case !c.ok && !errors.Is(err, ErrNoTransition):
			t.Errorf("%s: Lookup = %v, want ErrNoTransition", c.name, err)
		}
	}
	if _, err := Lookup(Planning, Planning, ClassEpic, TriggerMoreQuestions); !errors.Is(err, ErrNoTransition) {
		t.Errorf("more-questions at planning: got %v, want ErrNoTransition", err)
	}
	if _, err := Lookup(Clarifying, Clarifying, ClassEpic, TriggerPlanQuestions); !errors.Is(err, ErrNoTransition) {
		t.Errorf("plan-questions at clarifying: got %v, want ErrNoTransition", err)
	}
}

func TestLookupNeedsOneClass(t *testing.T) {
	for _, c := range []Class{0, AnyTask, 1 << 12} {
		_, err := Lookup(Intake, Clarifying, c, TriggerTrusted)
		if err == nil || errors.Is(err, ErrNoTransition) {
			t.Errorf("class %s: got %v, want a class error", c, err)
		}
	}
}

// TestTableMatchesSpec checks the table against SPEC.md row for row. A row's
// classes may be narrower than its "Applies to" column, never wider.
func TestTableMatchesSpec(t *testing.T) {
	raw, err := os.ReadFile("../../SPEC.md")
	if err != nil {
		t.Fatalf("read SPEC: %v", err)
	}
	applies := map[string]Class{
		"all":                               All,
		"epic, or story with no parent":     epicOrOwnerStory,
		"bug, or task filed by the owner":   NewBugOrOwnerTask | ClassBug | ClassOwnerTask,
		"epic, story":                       ClassEpic | AnyStory,
		"story":                             AnyStory,
		"story, or epic planned into tasks": ClassEpic | AnyStory,
		"epic planned into stories":         ClassEpic,
		"task":                              AnyTask | ClassHumanTask,
		"task, bug":                         AnyTask | ClassHumanTask | NewBugOrOwnerTask,
	}
	stage := func(s string) Stage {
		switch s {
		case "any open":
			return AnyOpen
		case "previous (`at`)":
			return PreviousAt
		case "previous (`origin`)":
			return PreviousOrigin
		}
		if st := Stage(s); st.Open() || st == Closed {
			return st
		}
		t.Fatalf("SPEC §7.2: unknown stage %q", s)
		return ""
	}
	_, section, _ := strings.Cut(string(raw), "### 7.2 Transitions")
	section, _, _ = strings.Cut(section, "### 7.3")
	// The table runs from its header to the first blank line. Every line in
	// it is a row, with or without outer pipes, so none can be skipped.
	lines := strings.Split(section, "\n")
	start := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), "| From |") })
	if start < 0 {
		t.Fatal("SPEC §7.2: no table header")
	}
	var n int
	for i := start; i < len(lines) && strings.TrimSpace(lines[i]) != ""; i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "|") {
			line = "|" + line
		}
		if !strings.HasSuffix(line, "|") || strings.HasSuffix(line, `\|`) {
			line += "|"
		}
		// Split on unescaped pipes only, and fail on any line that isn't a
		// 4-column row.
		cols := strings.Split(strings.ReplaceAll(line, `\|`, "\x00"), "|")
		if len(cols) != 6 {
			t.Fatalf("SPEC §7.2 line %d doesn't parse as a 4-column row: %q", i, lines[i])
		}
		if strings.TrimSpace(cols[1]) == "From" || strings.HasPrefix(strings.TrimSpace(cols[1]), "---") {
			continue
		}
		if n >= len(table) {
			t.Fatalf("SPEC §7.2 has more than the table's %d rows", len(table))
		}
		r := table[n]
		n++
		want, ok := applies[strings.TrimSpace(cols[3])]
		if !ok {
			t.Errorf("SPEC §7.2 row %d: unknown Applies to %q", n, cols[3])
		}
		from, to := stage(strings.TrimSpace(cols[1])), stage(strings.TrimSpace(cols[2]))
		if r.From() != from || r.To() != to || r.Classes() == 0 || r.Classes()&^want != 0 {
			t.Errorf("row %d: table has %s → %s for %s, SPEC has %s → %s for %s", n, r.From(), r.To(), r.Classes(), from, to, want)
		}
	}
	if n != len(table) {
		t.Errorf("SPEC §7.2 has %d rows, the table %d", n, len(table))
	}
}

// fitsItems is an item of each class, by the class's name, and one of a
// type the pipeline doesn't know.
var fitsItems = map[string]Item{
	"epic":           {Type: "epic"},
	"story":          {Type: "story", HasParent: true},
	"owner-story":    {Type: "story"},
	"task":           {Type: "task", FromPlan: true},
	"owner-task":     {Type: "task", ReachedReady: true},
	"human-task":     {Type: "task", FromPlan: true, Human: true},
	"bug":            {Type: "bug", ReachedReady: true},
	"untyped":        {},
	"new-bug":        {Type: "bug"},
	"new-owner-task": {Type: "task"},
	"feature":        {Type: "feature"},
}

// Fits, in the acceptance criteria's cases and a few more (plan #7, T1).
func TestFits(t *testing.T) {
	for _, tt := range []struct {
		name string
		s    Stage
		item Item
		want bool
	}{
		{"an epic at ready", Ready, fitsItems["epic"], false},
		{"a task at spec-review", SpecReview, fitsItems["task"], false},
		{"an owner's task at spec-review", SpecReview, fitsItems["new-owner-task"], false},
		{"an untyped issue at spec-review", SpecReview, fitsItems["untyped"], false},
		{"a story with a parent at spec-review", SpecReview, fitsItems["story"], false},
		{"an owner's story at planning", Planning, fitsItems["owner-story"], true},
		{"an epic at spec-review, its type in capitals", SpecReview, Item{Type: "Epic"}, true},
		{"a task at in-progress", InProgress, fitsItems["task"], true},
		{"a new bug at ready", Ready, fitsItems["new-bug"], true},
		{"a new bug at in-progress", InProgress, fitsItems["new-bug"], false},
		{"a human task at in-progress", InProgress, fitsItems["human-task"], false},
		{"an untyped issue at planning", Planning, fitsItems["untyped"], false},
		{"an untyped issue at closed", Closed, fitsItems["untyped"], true},
		{"feature at closed", Closed, fitsItems["feature"], false},
		{"a placeholder from", AnyOpen, fitsItems["epic"], false},
		{"a placeholder to", PreviousAt, fitsItems["task"], false},
		{"a placeholder to, untyped", PreviousOrigin, fitsItems["untyped"], false},
		{"no stage", Stage("nowhere"), fitsItems["task"], false},
		{"an empty stage", Stage(""), fitsItems["untyped"], false},
	} {
		if got := Fits(tt.s, tt.item); got != tt.want {
			t.Errorf("%s: Fits(%s, %+v) = %v, want %v", tt.name, tt.s, tt.item, got, tt.want)
		}
	}
}

// Fits, for every class at every stage: intake, clarifying and held fit
// every type; other stages follow the §7.2 rows, other than those from "any
// open"; an unknown type fits only intake.
func TestFitsEveryClass(t *testing.T) {
	always := []Stage{Intake, Clarifying, Held}
	story := []Stage{Planning, PlanReview, Planned, Closed}
	task := []Stage{Ready, InProgress, Verifying, PROpen, NeedsHuman, Closed}
	want := map[string][]Stage{
		"epic":           append([]Stage{SpecReview}, story...),
		"story":          story,
		"owner-story":    append([]Stage{SpecReview}, story...),
		"task":           task,
		"owner-task":     task,
		"human-task":     {NeedsHuman, Closed},
		"bug":            task,
		"untyped":        {Closed},
		"new-bug":        {Ready, Closed},
		"new-owner-task": {Ready, Closed},
	}
	if len(want)+1 != len(fitsItems) {
		t.Fatalf("%d classes for %d items", len(want), len(fitsItems))
	}
	for name, it := range fitsItems {
		c, err := ClassOf(it)
		fits := slices.Concat(want[name], always)
		switch {
		case name == "feature" && !errors.Is(err, ErrUnknownType):
			t.Fatalf("ClassOf(%+v) = %s, %v; want an unknown type", it, c, err)
		case name == "feature":
			fits = []Stage{Intake}
		case err != nil || c.String() != name:
			t.Fatalf("ClassOf(%+v) = %s, %v; want %s", it, c, err, name)
		}
		for _, s := range append(slices.Clone(openStages), Closed) {
			if got := Fits(s, it); got != slices.Contains(fits, s) {
				t.Errorf("Fits(%s, %s) = %v, want %v", s, name, got, !got)
			}
		}
	}
}

// Leaving out the rows from "any open" leaves out no stage they lead to:
// another row, or the rule for held, has each of their classes there.
func TestFitsAnyOpenRowsAreCovered(t *testing.T) {
	var n int
	for _, r := range table {
		if r.From() != AnyOpen {
			continue
		}
		n++
		for name, it := range fitsItems {
			if c, err := ClassOf(it); err == nil && r.Classes()&c != 0 && !Fits(r.To(), it) {
				t.Errorf("%s → %s on %s: %s doesn't fit there", r.From(), r.To(), r.Trigger(), name)
			}
		}
	}
	if n != 7 {
		t.Errorf("%d rows from any open, want 7", n)
	}
}

// Fits's own rules hold whatever the table says: with no rows, intake,
// clarifying and held still fit every type and nothing else fits; and a
// row from "any open" adds no stage to its class.
func TestFitsRulesBesideTheTable(t *testing.T) {
	saved := table
	t.Cleanup(func() { table = saved })
	table = nil
	for name, it := range fitsItems {
		for _, s := range append(slices.Clone(openStages), Closed) {
			want := s == Intake || (name != "feature" && (s == Clarifying || s == Held))
			if got := Fits(s, it); got != want {
				t.Errorf("with no rows, Fits(%s, %s) = %v, want %v", s, name, got, want)
			}
		}
	}
	table = []Row{{AnyOpen, Ready, ClassEpic, TriggerHold}, {AnyOpen, Closed, All, TriggerClosed}}
	for _, s := range []Stage{Ready, Closed} {
		if Fits(s, fitsItems["epic"]) {
			t.Errorf("with only rows from any open, an epic fits %s", s)
		}
	}
}

// A plan task whose body someone else edited keeps its class when a clean
// App comment carries its origin (#290), so it still fits the task stages;
// with the comment dirty, it's an owner task. A human task's comment, with
// human=1, keeps ClassHumanTask, which fits needs-human and never
// in-progress.
func TestOriginInACommentKeepsTheClass(t *testing.T) {
	t.Parallel()
	const (
		task  = "<!-- issuedog:origin plan=#195 rev=1 item=T1 -->"
		human = "<!-- issuedog:origin plan=#195 rev=1 item=T9 human=1 -->"
		moved = "<!-- issuedog:transition from=planned to=ready event=p9 -->"
	)
	edited := func(origin string) *github.Snapshot {
		s := snap("The task, edited by the owner.", []github.Author{appBot, appBot}, origin, moved)
		s.Type, s.Parent = "task", 195
		s.BodyEdit = github.Edit{By: owner, At: time.Unix(5, 0)}
		return s
	}
	for _, tt := range []struct {
		name  string
		s     *github.Snapshot
		dirty func(int64) bool
		// human sets Item.Human as S3's Derive sets it from the marker's
		// human=1 through Origin (#275), which this branch doesn't have:
		// the case checks that FromPlan, read from the comment, gives the
		// human task's class with it.
		human     bool
		want      Class
		fits, not Stage
	}{
		{"a plan task", edited(task), clean, false, ClassTask, InProgress, SpecReview},
		{"a human task", edited(human), clean, true, ClassHumanTask, NeedsHuman, InProgress},
		{"a plan task whose comment is dirty", edited(task), dirtyIDs(1), false, ClassOwnerTask, InProgress, Planning},
	} {
		it := Derive(tt.s, slug, tt.dirty).Item
		it.Human = tt.human
		if c, err := ClassOf(it); err != nil || c != tt.want {
			t.Errorf("%s: class = %s, %v; want %s", tt.name, c, err, tt.want)
		}
		if !Fits(tt.fits, it) || Fits(tt.not, it) {
			t.Errorf("%s: fits %s = %v and %s = %v, want true and false", tt.name, tt.fits, Fits(tt.fits, it), tt.not, Fits(tt.not, it))
		}
	}
}
