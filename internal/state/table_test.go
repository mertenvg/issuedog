package state

import (
	"errors"
	"os"
	"strings"
	"testing"
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
	var n int
	for i, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		// Split on unescaped pipes only, and fail on any table line that
		// isn't a 4-column row, so a new SPEC row can't be skipped.
		cols := strings.Split(strings.ReplaceAll(strings.TrimSpace(line), `\|`, "\x00"), "|")
		if len(cols) != 6 {
			t.Fatalf("SPEC §7.2 line %d doesn't parse as a 4-column row: %q", i, line)
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
