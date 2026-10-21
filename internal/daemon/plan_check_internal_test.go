package daemon

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// epicFacts are the facts the golden epic plan is checked against: the
// epic #123 at p1, with no sub-issues yet.
func epicFacts() planFacts {
	return planFacts{Issue: 123, Class: state.ClassEpic, Scopes: []string{"booking", "infra", "docs"}, MaxTasks: 10,
		Models:       config.Models{Allowed: []config.Model{{Name: "opus"}, {Name: "claude-sonnet-4-5", Label: "sonnet"}}},
		MaxQuestions: 7, Round: 1, Priority: "p1", Notify: []string{"mertenvg"}, Rev: 1}
}

// storyFacts are the facts the golden story plan and question round are
// checked against: the story #124 at p2, with a parent. Its golden model,
// sonnet, is a label suffix Models.Resolve takes.
func storyFacts() planFacts {
	f := epicFacts()
	f.Issue, f.Class, f.Priority, f.Round = 124, state.ClassStory, "p2", 2
	return f
}

// factsFor is the facts golden output name is checked against.
func factsFor(name string) planFacts {
	if name == "epic-plan" {
		return epicFacts()
	}
	return storyFacts()
}

// T4's golden epic and story plans pass, and so does its question round.
// So do a story plan for an epic, a story without a parent, and a plan
// that skips review.
func TestCheckPlanGolden(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"epic-plan", "story-plan", "questions"} {
		if err := checkPlan(plannerOutput(t, name), factsFor(name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	f := epicFacts()
	if err := checkPlan(plannerOutput(t, "story-plan"), f); err != nil {
		t.Errorf("an epic's plan of tasks: %v", err)
	}
	f = storyFacts()
	f.Class = state.ClassOwnerStory
	if err := checkPlan(plannerOutput(t, "story-plan"), f); err != nil {
		t.Errorf("a story without a parent: %v", err)
	}
	f = storyFacts()
	f.NoReview, f.Rev = true, 2
	if err := checkPlan(plannerOutput(t, "story-plan"), f); err != nil {
		t.Errorf("a plan that skips review: %v", err)
	}
}

// humanTask is the golden story plan's human task, T3.
func humanTask(o *planOutput) *planHumanTask { return o.Plan.Items[2].HumanTask }

// Each rule fails a golden output broken in only that rule, by name, and
// its detail names the item and field.
func TestCheckPlanRules(t *testing.T) {
	t.Parallel()
	item := func(o *planOutput, i int) *planItem { return &o.Plan.Items[i] }
	const path = "see internal/booking/quote.go"
	cases := []struct {
		name, base, rule string
		detail           string // what the error says
		change           func(*planOutput, *planFacts)
	}{
		// outcome
		{"unknown outcome", "story-plan", ruleOutcome, `unknown outcome "spec"`, func(o *planOutput, _ *planFacts) { o.Outcome = "spec" }},
		{"plan with no plan", "story-plan", ruleOutcome, "a plan has a plan", func(o *planOutput, _ *planFacts) { o.Plan = nil }},
		{"plan with questions", "story-plan", ruleOutcome, "a plan has a plan, and no questions", func(o *planOutput, _ *planFacts) {
			o.Questions = plannerOutput(t, "questions").Questions
		}},
		{"questions with a plan", "questions", ruleOutcome, "a question round has questions, and no plan", func(o *planOutput, _ *planFacts) {
			o.Plan = plannerOutput(t, "story-plan").Plan
		}},
		{"questions with none", "questions", ruleOutcome, "a question round has questions", func(o *planOutput, _ *planFacts) { o.Questions = nil }},

		// structure
		{"stories for a story", "epic-plan", ruleStructure, "a story's plan has tasks, not stories", func(_ *planOutput, f *planFacts) {
			f.Class = state.ClassStory
		}},
		{"stories for an owner's story", "epic-plan", ruleStructure, "a story's plan has tasks, not stories", func(_ *planOutput, f *planFacts) {
			f.Class = state.ClassOwnerStory
		}},
		{"a task planned", "story-plan", ruleStructure, "only an epic or a story is planned", func(_ *planOutput, f *planFacts) {
			f.Class = state.ClassTask
		}},
		{"unknown kind", "epic-plan", ruleStructure, `unknown plan kind "epics"`, func(o *planOutput, _ *planFacts) { o.Plan.Kind = "epics" }},
		{"no items", "epic-plan", ruleStructure, "the plan has no items", func(o *planOutput, _ *planFacts) { o.Plan.Items = nil }},
		{"blank summary", "epic-plan", ruleStructure, "the plan has no summary", func(o *planOutput, _ *planFacts) { o.Plan.Summary = " \n " }},
		{"no criteria", "epic-plan", ruleStructure, "S2 has no criteria", func(o *planOutput, _ *planFacts) { item(o, 1).Criteria = nil }},
		{"story with a scope", "epic-plan", ruleStructure, "S1 is a story", func(o *planOutput, _ *planFacts) { item(o, 0).Scope = "booking" }},
		{"story marked human", "epic-plan", ruleStructure, "S2 is a story", func(o *planOutput, _ *planFacts) { item(o, 1).Human = true }},
		{"story with a task body", "epic-plan", ruleStructure, "S3 is a story", func(o *planOutput, _ *planFacts) {
			item(o, 2).Task = plannerOutput(t, "story-plan").Plan.Items[0].Task
		}},
		{"story with a human task body", "epic-plan", ruleStructure, "S1 is a story", func(o *planOutput, _ *planFacts) {
			h := plannerOutput(t, "story-plan")
			item(o, 0).HumanTask = humanTask(&h)
		}},
		{"task with no body", "story-plan", ruleStructure, "T1 has neither task nor human_task", func(o *planOutput, _ *planFacts) {
			item(o, 0).Task = nil
		}},
		{"task with both bodies", "story-plan", ruleStructure, "T3 has task and human_task", func(o *planOutput, _ *planFacts) {
			item(o, 2).Task = item(o, 0).Task
		}},
		{"human task body, not human", "story-plan", ruleStructure, "T3 has human false with human_task", func(o *planOutput, _ *planFacts) {
			item(o, 2).Human = false
		}},
		{"task body, human", "story-plan", ruleStructure, "T1 has human true with task", func(o *planOutput, _ *planFacts) {
			item(o, 0).Human = true
		}},
		{"task body with no context", "story-plan", ruleStructure, "T1's task body has no context", func(o *planOutput, _ *planFacts) {
			item(o, 0).Task.Context = " "
		}},
		{"task body with no change", "story-plan", ruleStructure, "T2's task body has no change", func(o *planOutput, _ *planFacts) {
			item(o, 1).Task.Change = ""
		}},
		{"task body with no test plan", "story-plan", ruleStructure, "T1's task body has no test plan", func(o *planOutput, _ *planFacts) {
			item(o, 0).Task.TestPlan = "\n"
		}},
		{"task body with no definition of done", "story-plan", ruleStructure, "T1's task body has no definition of done", func(o *planOutput, _ *planFacts) {
			item(o, 0).Task.DefinitionOfDone = nil
		}},

		// item-id
		{"duplicate ID", "epic-plan", ruleItemID, "S2 is the ID of more than one item", func(o *planOutput, _ *planFacts) { item(o, 2).ID = "S2" }},
		{"malformed ID", "epic-plan", ruleItemID, `item 2's ID "X2"`, func(o *planOutput, _ *planFacts) { item(o, 1).ID = "X2" }},
		{"ID numbered from 0", "epic-plan", ruleItemID, `item 2's ID "S0"`, func(o *planOutput, _ *planFacts) { item(o, 1).ID = "S0" }},
		{"task ID among stories", "epic-plan", ruleItemID, "T2 is in a plan of stories", func(o *planOutput, _ *planFacts) { item(o, 1).ID = "T2" }},
		{"story ID among tasks", "story-plan", ruleItemID, "S2 is in a plan of tasks", func(o *planOutput, _ *planFacts) { item(o, 1).ID = "S2" }},

		// scope
		{"scope not on the list", "story-plan", ruleScope, `T3's scope "web" isn't on the list`, func(o *planOutput, _ *planFacts) {
			item(o, 2).Scope = "web"
		}},
		{"no scope", "story-plan", ruleScope, "T1 has no scope", func(o *planOutput, _ *planFacts) { item(o, 0).Scope = "" }},
		{"scope in another case", "story-plan", ruleScope, `T1's scope "Booking"`, func(o *planOutput, _ *planFacts) { item(o, 0).Scope = "Booking" }},

		// max-tasks: a hard limit (#247's Q2a)
		{"over max tasks", "story-plan", ruleMaxTasks, "3 tasks, over plan.max_tasks of 2", func(_ *planOutput, f *planFacts) { f.MaxTasks = 2 }},

		// depends-on and cycle
		{"unknown dependency", "story-plan", ruleDependsOn, `T1 depends on "T9"`, func(o *planOutput, _ *planFacts) {
			item(o, 0).DependsOn = []string{"T9"}
		}},
		{"self-dependency", "story-plan", ruleDependsOn, "T1 depends on itself", func(o *planOutput, _ *planFacts) {
			item(o, 0).DependsOn = []string{"T1"}
		}},
		{"dependency twice", "story-plan", ruleDependsOn, "T2 depends on T1 more than once", func(o *planOutput, _ *planFacts) {
			item(o, 1).DependsOn = []string{"T1", "T3", "T1"}
		}},
		{"two-item cycle", "epic-plan", ruleCycle, "S1 → S2 → S1", func(o *planOutput, _ *planFacts) {
			item(o, 0).DependsOn = []string{"S2"}
		}},
		{"three-item cycle", "story-plan", ruleCycle, "T2 → T3 → T2", func(o *planOutput, _ *planFacts) {
			item(o, 2).DependsOn = []string{"T2"}
		}},

		// sub-issues
		{"over 100 sub-issues", "story-plan", ruleSubIssues, "98 sub-issues already and 3 items make 101, over 100", func(_ *planOutput, f *planFacts) {
			f.SubIssues = 98
		}},

		// model, effort and priority
		{"model not allowed", "story-plan", ruleModel, `T1's model "haiku" isn't in models.allowed`, func(o *planOutput, _ *planFacts) {
			item(o, 0).Model = "haiku"
		}},
		{"model with no models allowed", "epic-plan", ruleModel, `S1's model "opus"`, func(_ *planOutput, f *planFacts) { f.Models = config.Models{} }},
		{"unknown effort", "epic-plan", ruleEffort, `S3's effort "extreme" isn't one of low, medium, high, xhigh, max`, func(o *planOutput, _ *planFacts) {
			item(o, 2).Effort = "extreme"
		}},
		{"priority p4", "story-plan", rulePriority, `T2's priority "p4" isn't p0 to p3`, func(o *planOutput, _ *planFacts) {
			item(o, 1).Priority = "p4"
		}},
		{"priority in capitals", "epic-plan", rulePriority, `S1's priority "P1"`, func(o *planOutput, _ *planFacts) { item(o, 0).Priority = "P1" }},

		// human-task
		{"no goal", "story-plan", ruleHumanTask, "T3's goal is empty", func(o *planOutput, _ *planFacts) { humanTask(o).Goal = " " }},
		{"no why it blocks", "story-plan", ruleHumanTask, "T3's why it blocks is empty", func(o *planOutput, _ *planFacts) { humanTask(o).WhyBlocks = "" }},
		{"no steps", "story-plan", ruleHumanTask, "T3 has no steps", func(o *planOutput, _ *planFacts) { humanTask(o).Steps = nil }},
		{"blank step", "story-plan", ruleHumanTask, "T3's step 2 is empty", func(o *planOutput, _ *planFacts) { humanTask(o).Steps[1] = "" }},
		{"no links", "story-plan", ruleHumanTask, "T3 has no links", func(o *planOutput, _ *planFacts) { humanTask(o).Links = []string{} }},
		{"blank link", "story-plan", ruleHumanTask, "T3's link 1 is empty", func(o *planOutput, _ *planFacts) { humanTask(o).Links[0] = "\t" }},
		{"no done when", "story-plan", ruleHumanTask, "T3's done when is empty", func(o *planOutput, _ *planFacts) { humanTask(o).DoneWhen.Text = "" }},
		{"no estimated time", "story-plan", ruleHumanTask, "T3's estimated time is empty", func(o *planOutput, _ *planFacts) {
			humanTask(o).EstimatedTime = ""
		}},

		// done-when: each bound is in TestValidCheck, which this calls.
		{"check path with ..", "story-plan", ruleDoneWhen, `T3's file check: path "deploy/../production.env" has the segment ".."`, func(o *planOutput, _ *planFacts) {
			humanTask(o).DoneWhen.Check.Path = "deploy/../production.env"
		}},
		{"check of an unknown kind", "story-plan", ruleDoneWhen, `T3's check has the unknown kind "url"`, func(o *planOutput, _ *planFacts) {
			humanTask(o).DoneWhen.Check.Kind = "url"
		}},

		// file-paths: in every field the comment shows
		{"path in why", "epic-plan", ruleFilePaths, `the why names the file path "internal/booking/quote.go"`, func(o *planOutput, _ *planFacts) {
			o.Plan.Why += " " + path
		}},
		{"path in summary", "epic-plan", ruleFilePaths, "the summary names", func(o *planOutput, _ *planFacts) { o.Plan.Summary += "\n\n`" + path + "`" }},
		{"path in message", "story-plan", ruleFilePaths, "the message names", func(o *planOutput, _ *planFacts) { o.Plan.Message = path }},
		{"path in an assumption", "epic-plan", ruleFilePaths, "assumption 2 names", func(o *planOutput, _ *planFacts) { o.Plan.Assumptions[1] += path }},
		{"path in order", "epic-plan", ruleFilePaths, "the order names", func(o *planOutput, _ *planFacts) { o.Plan.Order += ", " + path }},
		{"path in a title", "epic-plan", ruleFilePaths, "S2's title names", func(o *planOutput, _ *planFacts) { item(o, 1).Title = "Change web/app.tsx" }},
		{"path in an outcome", "story-plan", ruleFilePaths, "T1's outcome names", func(o *planOutput, _ *planFacts) { item(o, 0).Outcome += " " + path }},
		{"path in a criterion", "story-plan", ruleFilePaths, "T2's criterion 3 names", func(o *planOutput, _ *planFacts) {
			item(o, 1).Criteria[2] = "The handler in api/errors.go maps it."
		}},
		{"path in done when", "story-plan", ruleFilePaths, `T3's done when names the file path "deploy/production.env"`, func(o *planOutput, _ *planFacts) {
			humanTask(o).DoneWhen.Text = "`deploy/production.env` sets it."
		}},
		{"path as a link's text", "epic-plan", ruleFilePaths, `the summary names the file path "docs/quotes.md"`, func(o *planOutput, _ *planFacts) {
			o.Plan.Summary = "See [docs/quotes.md](https://example.com/acme/docs/quotes.md)."
		}},

		// one-line
		{"two-line why", "epic-plan", ruleOneLine, "why", func(o *planOutput, _ *planFacts) { o.Plan.Why = "a\nb" }},
		{"blank order", "epic-plan", ruleOneLine, "order", func(o *planOutput, _ *planFacts) { o.Plan.Order = " " }},
		{"two-line assumption", "epic-plan", ruleOneLine, "assumption 3", func(o *planOutput, _ *planFacts) { o.Plan.Assumptions[2] = "a\r\nb" }},
		{"two-line title", "epic-plan", ruleOneLine, "S1's title", func(o *planOutput, _ *planFacts) { item(o, 0).Title = "a\nb" }},
		{"blank title", "story-plan", ruleOneLine, "T2's title", func(o *planOutput, _ *planFacts) { item(o, 1).Title = "" }},
		{"two-line outcome", "story-plan", ruleOneLine, "T1's outcome", func(o *planOutput, _ *planFacts) { item(o, 0).Outcome = "a\rb" }},
		{"two-line criterion", "epic-plan", ruleOneLine, "S3's criterion 2", func(o *planOutput, _ *planFacts) { item(o, 2).Criteria[1] = "a\nb" }},
		{"two-line definition of done", "story-plan", ruleOneLine, "T2's definition of done 1", func(o *planOutput, _ *planFacts) {
			item(o, 1).Task.DefinitionOfDone[0] = "a\nb"
		}},
		{"two-line goal", "story-plan", ruleOneLine, "T3's goal", func(o *planOutput, _ *planFacts) { humanTask(o).Goal = "a\nb" }},
		{"two-line step", "story-plan", ruleOneLine, "T3's step 3", func(o *planOutput, _ *planFacts) { humanTask(o).Steps[2] = "a\nb" }},
		{"two-line done when", "story-plan", ruleOneLine, "T3's done when", func(o *planOutput, _ *planFacts) { humanTask(o).DoneWhen.Text = "a\nb" }},

		// length
		{"long question round", "questions", ruleLength, "the question round's comment is", func(o *planOutput, _ *planFacts) {
			o.Questions[0].Text = strings.Repeat("x", maxText)
		}},
		{"long plan comment", "epic-plan", ruleLength, "the plan comment is", func(o *planOutput, _ *planFacts) {
			o.Plan.Summary = strings.Repeat("Quotes expire. ", maxText/15)
		}},

		// questions: the interviewer's rules
		{"too many questions", "questions", ruleQuestionCount, "3 questions, over the limit of 2", func(o *planOutput, f *planFacts) {
			o.Questions, f.MaxQuestions = append(o.Questions, o.Questions[0]), 2
		}},
		{"question that blocks nothing", "questions", ruleBlocks, "question 2 blocks nothing", func(o *planOutput, _ *planFacts) {
			o.Questions[1].Blocks = nil
		}},
		{"one option", "questions", ruleOptions, "question 1 has 1 options, not 2 to 4", func(o *planOutput, _ *planFacts) {
			o.Questions[0].Others = nil
		}},
		{"five options", "questions", ruleOptions, "question 2 has 5 options, not 2 to 4", func(o *planOutput, _ *planFacts) {
			o.Questions[1].Others = []string{"b", "c", "d", "e"}
		}},
		{"two-line question", "questions", ruleOneLine, "question 1", func(o *planOutput, _ *planFacts) { o.Questions[0].Text = "a\nb" }},
	}
	for _, tc := range cases {
		out, f := plannerOutput(t, tc.base), factsFor(tc.base)
		tc.change(&out, &f)
		err := checkPlan(out, f)
		if got := rules(err); !slices.Equal(got, []string{tc.rule}) {
			t.Errorf("%s: rules %q, want only %s (%v)", tc.name, got, tc.rule, err)
			continue
		}
		if !strings.Contains(err.Error(), "check planner output: rule "+tc.rule+": ") || !strings.Contains(err.Error(), tc.detail) {
			t.Errorf("%s: error %q, want rule %s naming %q", tc.name, err, tc.rule, tc.detail)
		}
	}
}

// A plan that breaks three rules gets all three, each naming its item.
func TestCheckPlanEveryRule(t *testing.T) {
	t.Parallel()
	out := plannerOutput(t, "story-plan")
	out.Plan.Items[0].Effort = "extreme"
	out.Plan.Items[1].Model = "haiku"
	out.Plan.Items[2].Scope = "web"
	err := checkPlan(out, storyFacts())
	got := rules(err)
	slices.Sort(got)
	if want := []string{ruleEffort, ruleModel, ruleScope}; !slices.Equal(got, want) {
		t.Fatalf("rules %q, want %q", got, want)
	}
	for _, want := range []string{`T1's effort "extreme"`, `T2's model "haiku"`, `T3's scope "web"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q doesn't say %q", err, want)
		}
	}
}

// Every cycle is named, from where it starts, by the dependency that
// closes it, and an item that leads into a cycle, with no way back, isn't
// on it, though the search starts there.
func TestCheckPlanCycles(t *testing.T) {
	t.Parallel()
	item := func(id string, deps ...string) planItem {
		return planItem{ID: id, Title: "Title", Outcome: "Outcome.", Criteria: []string{"Criterion."}, DependsOn: deps}
	}
	p := planBody{Kind: planStories, Why: "Why.", Summary: "Summary.", Order: "Order", Items: []planItem{
		item("S1", "S2"),                                     // into the cycle, first, not on it
		item("S2", "S3"), item("S3", "S4"), item("S4", "S2"), // S2 → S3 → S4 → S2
		item("S5", "S6"), item("S6", "S5"), // a second cycle
	}}
	err := checkPlan(planOutput{Outcome: outcomePlan, Plan: &p}, epicFacts())
	if got := rules(err); !slices.Equal(got, []string{ruleCycle, ruleCycle}) {
		t.Fatalf("rules %q, want two cycles (%v)", got, err)
	}
	for _, want := range []string{"rule cycle: S2 → S3 → S4 → S2", "rule cycle: S5 → S6 → S5"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q doesn't say %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "S1 →") {
		t.Errorf("error %q puts S1 on a cycle", err)
	}
}

// manyStories is an epic plan of n stories, each blocked by the stories
// deps gives it.
func manyStories(n int, deps func(i int) []string) planOutput {
	p := planBody{Kind: planStories, Why: "Why.", Summary: "Summary.", Order: "In order"}
	for i := 1; i <= n; i++ {
		p.Items = append(p.Items, planItem{ID: fmt.Sprintf("S%d", i), Title: "Title", Outcome: "Outcome.",
			Criteria: []string{"Criterion."}, DependsOn: deps(i)})
	}
	return planOutput{Outcome: outcomePlan, Plan: &p}
}

// The 50-link limit, blocked by and blocking, and the 100 sub-issue limit
// at their bounds: 50 links and 100 sub-issues pass, one more fails.
func TestCheckPlanLinkLimits(t *testing.T) {
	t.Parallel()
	blockedBy := func(n int) func(int) []string {
		return func(i int) []string {
			if i != n+1 {
				return []string{}
			}
			var d []string
			for j := 1; j <= n; j++ {
				d = append(d, fmt.Sprintf("S%d", j))
			}
			return d
		}
	}
	blocking := func(i int) []string {
		if i == 1 {
			return nil
		}
		return []string{"S1"}
	}
	f := epicFacts()
	for _, tc := range []struct {
		name   string
		out    planOutput
		subs   int
		detail string // "" to pass
	}{
		{"blocked by 50", manyStories(51, blockedBy(50)), 0, ""},
		{"blocked by 51", manyStories(52, blockedBy(51)), 0, "rule links: S52 is blocked by 51 items, over 50"},
		{"blocking 50", manyStories(51, blocking), 0, ""},
		{"blocking 51", manyStories(52, blocking), 0, "rule links: S1 blocks 51 items, over 50"},
		{"100 sub-issues", manyStories(40, blocking), 60, ""},
		{"101 sub-issues", manyStories(41, blocking), 60, "rule sub-issues: 60 sub-issues already and 41 items make 101, over 100"},
	} {
		f.SubIssues = tc.subs
		err := checkPlan(tc.out, f)
		switch {
		case tc.detail == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.detail == "":
		case err == nil || len(rules(err)) != 1 || !strings.Contains(err.Error(), tc.detail):
			t.Errorf("%s: error %v, want only %q", tc.name, err, tc.detail)
		}
	}
}

// A file path is a word with a directory and an extension, outside a link.
// A bare file name, a URL to a file, a link's destination and words with
// a slash but no extension are none. Nor, so prose passes (#311), is a
// word whose extension doesn't start with a lowercase letter or is a
// module's version, whose directory has no letter or is only versions, or
// that is capitalized names ending in ".js" or ".io". Capitalized paths,
// extensions with digits and directories named "x" or "v2" are paths.
func TestPlanFilePath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ text, want string }{
		{"Edit internal/daemon/plan.go today.", "internal/daemon/plan.go"},
		{"Edit internal/daemon/plan.go.", "internal/daemon/plan.go"},
		{"`deploy/production.env`", "deploy/production.env"},
		{"Change ./web/app.tsx.", "./web/app.tsx"},
		{"See cmd/main.go, then deploy/.env.", "cmd/main.go"},
		{"Set it in deploy/.env.", "deploy/.env"},
		{"In a/b.go.", "a/b.go"},
		{"pkg/x.v2.go", "pkg/x.v2.go"},
		{"src/components/Button.js", "src/components/Button.js"},
		{"Sources/App/main.swift", "Sources/App/main.swift"},
		{"Assets/Scripts/Player.cs", "Assets/Scripts/Player.cs"},
		{"Controllers/HomeController.cs", "Controllers/HomeController.cs"},
		{"MyApp/Views/HomeView.swift", "MyApp/Views/HomeView.swift"},
		{"Sources/App/Main.swift", "Sources/App/Main.swift"},
		{"Run scripts/install.ps1.", "scripts/install.ps1"},
		{"media/intro.mp4", "media/intro.mp4"},
		{"dist/x.tar.bz2", "dist/x.tar.bz2"},
		{"rtl/top.v", "rtl/top.v"},
		{"x/main.go", "x/main.go"},
		{"v2/main.go", "v2/main.go"},
		{"api/v1.2/routes.go", "api/v1.2/routes.go"},
		{"/etc/hosts.conf", "/etc/hosts.conf"},
		{"React/Next.js, and web/app.tsx", "web/app.tsx"},
		{"Supports TLS 1.2/1.3.", ""},
		{"Go 1.22/1.23", ""},
		{"Python 3.11/3.12, or 2.7/3.x.", ""},
		{"Node 18/20.x", ""},
		{"Speaks HTTP/1.1.", ""},
		{"An Express/Node.js or React/Next.js app.", ""},
		{"A Vue/Nuxt.js or Express/Socket.io app.", ""},
		{"Python 2.x/3.x", ""},
		{"Vue 2.x/3.x", ""},
		{"Node 18.x/20.x", ""},
		{"Webpack 4.x/5.x.", ""},
		{"Node v18.x/v20.x", ""},
		{"Go 1.x/2.x/3.x", ""},
		{"Parses with gopkg.in/yaml.v3.", ""},
		{"A net/http.Handler, as internal/appkey.Load reads it.", ""},
		{"The [settings file](deploy/production.env) ...", ""},
		{"[cmd/main.go](https://github.com/acme/booking/blob/main/cmd/main.go)", "cmd/main.go"},
		{"See https://github.com/acme/booking/blob/main/deploy/production.env for it.", ""},
		{"See <https://example.com/docs/page.html>.", ""},
		{"See www.example.com/docs/page.html.", ""},
		{"[docs]: https://example.com/docs/page.html", ""},
		{"Only planner.md changes.", ""},
		{"Reads and/or writes, at 50/50.", ""},
		{"TCP/IP.", ""},
		{"", ""},
	} {
		if got := planFilePath(tc.text); got != tc.want {
			t.Errorf("planFilePath(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

// A plan whose visible fields hold prose with a slash, such as versions,
// protocols, framework names and Go module paths, passes (#311).
func TestCheckPlanSlashedProse(t *testing.T) {
	t.Parallel()
	out := plannerOutput(t, "story-plan")
	out.Plan.Why += " It speaks HTTP/1.1, over TLS 1.2/1.3."
	out.Plan.Summary += "\n\nIt builds on Go 1.22/1.23 and Python 3.11/3.12."
	out.Plan.Items[0].Outcome += " Express/Node.js, React/Next.js and Express/Socket.io apps get it too."
	out.Plan.Order += ", on Node 18.x/20.x"
	out.Plan.Items[1].Criteria[0] += " The config parses with gopkg.in/yaml.v3."
	if err := checkPlan(out, storyFacts()); err != nil {
		t.Errorf("a plan of prose with slashes: %v", err)
	}
}

// Each check bound fails on its own, and a value at the bound passes.
func TestValidCheck(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		c      planCheck
		detail string // "" for a valid check
	}{
		{"label", planCheck{Kind: "label", Label: "scope:booking"}, ""},
		{"50-character label", planCheck{Kind: "label", Label: strings.Repeat("é", 50)}, ""},
		{"51-character label", planCheck{Kind: "label", Label: strings.Repeat("l", 51)}, "label check: label is 51 characters, over 50"},
		{"empty label", planCheck{Kind: "label"}, `label check: label "" isn't one line`},
		{"two-line label", planCheck{Kind: "label", Label: "a\nb"}, `label "a\nb" isn't one line`},
		{"label .", planCheck{Kind: "label", Label: "."}, `label "." isn't a label's name`},
		{"label ..", planCheck{Kind: "label", Label: ".."}, `label ".." isn't a label's name`},
		{"label ...", planCheck{Kind: "label", Label: "..."}, ""},
		{"issue type", planCheck{Kind: "issue-type", IssueType: "Epic"}, ""},
		{"empty issue type", planCheck{Kind: "issue-type", IssueType: " "}, `issue-type check: issue_type " " isn't one line`},
		{"two-line issue type", planCheck{Kind: "issue-type", IssueType: "Epic\nTask"}, "isn't one line"},
		{"file", planCheck{Kind: "file", Path: "deploy/production.env"}, ""},
		{"file containing", planCheck{Kind: "file", Path: "README.md", Contains: "QUOTE_TTL=24h"}, ""},
		{"file name with dots", planCheck{Kind: "file", Path: "a..b/.github/c..d"}, ""},
		{"path with ..", planCheck{Kind: "file", Path: "../secrets"}, `file check: path "../secrets" has the segment ".."`},
		{"path with .. inside", planCheck{Kind: "file", Path: "a/../b"}, `has the segment ".."`},
		{"path with .", planCheck{Kind: "file", Path: "a/./b"}, `has the segment "."`},
		{"path of a directory", planCheck{Kind: "file", Path: "deploy/"}, `has the segment ""`},
		{"absolute path", planCheck{Kind: "file", Path: "/etc/passwd"}, `path "/etc/passwd" starts with /`},
		{"path with a backslash", planCheck{Kind: "file", Path: `deploy\production.env`}, "has a backslash"},
		{"path with a space first", planCheck{Kind: "file", Path: " deploy"}, "starts with a space"},
		{"empty path", planCheck{Kind: "file"}, `path "" isn't one line`},
		{"two-line path", planCheck{Kind: "file", Path: "a\nb"}, "isn't one line"},
		{"256-byte contains", planCheck{Kind: "file", Path: "a", Contains: strings.Repeat("c", 256)}, ""},
		{"257-byte contains", planCheck{Kind: "file", Path: "a", Contains: strings.Repeat("c", 257)}, "file check: contains is 257 bytes, over 256"},
		{"contains of 129 two-byte characters", planCheck{Kind: "file", Path: "a", Contains: strings.Repeat("é", 129)}, "contains is 258 bytes, over 256"},
		{"two-line contains", planCheck{Kind: "file", Path: "a", Contains: "a\nb"}, `contains "a\nb" isn't one line`},
		{"blank contains", planCheck{Kind: "file", Path: "a", Contains: " "}, `contains " " isn't one line`},
		{"ruleset", planCheck{Kind: "ruleset", Check: "issuedog/verify"}, ""},
		{"empty check name", planCheck{Kind: "ruleset"}, `ruleset check: check "" isn't one line`},
		{"two-line check name", planCheck{Kind: "ruleset", Check: "a\nb"}, "isn't one line"},
		{"installation", planCheck{Kind: "installation", Owner: "mertenvg"}, ""},
		{"installation for an EMU login", planCheck{Kind: "installation", Owner: "merten_acme"}, ""},
		{"39-character owner", planCheck{Kind: "installation", Owner: strings.Repeat("o", 39)}, ""},
		{"40-character owner", planCheck{Kind: "installation", Owner: strings.Repeat("o", 40)}, "isn't a user's or organization's login"},
		{"owner with a leading hyphen", planCheck{Kind: "installation", Owner: "-acme"}, `installation check: owner "-acme" isn't`},
		{"owner with a double hyphen", planCheck{Kind: "installation", Owner: "ac--me"}, "isn't a user's"},
		{"bot owner", planCheck{Kind: "installation", Owner: "issuedog[bot]"}, "isn't a user's"},
		{"owner with a slash", planCheck{Kind: "installation", Owner: "acme/booking"}, "isn't a user's"},
		{"empty owner", planCheck{Kind: "installation"}, `owner "" isn't`},
		{"unknown kind", planCheck{Kind: "url", Path: "a"}, `check has the unknown kind "url"`},
		{"no kind", planCheck{Label: "a"}, `check has the unknown kind ""`},
		{"label with a path", planCheck{Kind: "label", Label: "a", Path: "b"}, "label check: path isn't a field of kind label"},
		{"file with an owner", planCheck{Kind: "file", Path: "a", Owner: "acme"}, "owner isn't a field of kind file"},
		{"ruleset with contains", planCheck{Kind: "ruleset", Check: "c", Contains: "x"}, "contains isn't a field of kind ruleset"},
		{"installation with a label", planCheck{Kind: "installation", Owner: "acme", Label: "x"}, "label isn't a field of kind installation"},
		{"issue type with a check", planCheck{Kind: "issue-type", IssueType: "Epic", Check: "c"}, "check isn't a field of kind issue-type"},
		{"label with an issue type", planCheck{Kind: "label", Label: "a", IssueType: "Epic"}, "issue_type isn't a field of kind label"},
	} {
		err := validCheck(tc.c)
		switch {
		case tc.detail == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.detail == "":
		case err == nil:
			t.Errorf("%s: valid, want %q", tc.name, tc.detail)
		case !strings.Contains(err.Error(), tc.detail):
			t.Errorf("%s: %q, want %q", tc.name, err, tc.detail)
		}
	}
}

// bigPlan is the golden story plan grown to n tasks, each a copy of T1
// with no dependencies, whose task body carries extra characters of text
// that doesn't compress, as task bodies that name many files and changes
// would.
func bigPlan(t *testing.T, n, extra int) planOutput {
	t.Helper()
	out := plannerOutput(t, "story-plan")
	r := rand.New(rand.NewPCG(1, 2))
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 "
	var items []planItem
	for i := 1; i <= n; i++ {
		it := plannerOutput(t, "story-plan").Plan.Items[0]
		it.ID = fmt.Sprintf("T%d", i)
		b := make([]byte, extra)
		for j := range b {
			b[j] = alphabet[r.IntN(len(alphabet))]
		}
		it.Task.Change += "\n\n" + string(b)
		items = append(items, it)
	}
	out.Plan.Items, out.Plan.Order = items, "All in parallel"
	return out
}

// A plan comment that can't be built is an error, not a broken length
// rule: its error keeps the chain of what failed (#311), and the other
// rules' failures stay beside it.
func TestCheckPlanLengthError(t *testing.T) {
	t.Parallel()
	out := plannerOutput(t, "story-plan")
	out.Plan.Items[0].Effort = "extreme"
	f := storyFacts()
	f.Fields = map[string]string{"model": "a b"}
	err := checkPlan(out, f)
	if err == nil {
		t.Fatal("a plan comment that can't be built passes")
	}
	if got := rules(err); !slices.Equal(got, []string{ruleEffort}) {
		t.Errorf("rules %q, want only effort (%v)", got, err)
	}
	joined, ok := errors.Unwrap(err).(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("error %q joins no errors", err)
	}
	var internal error
	for _, x := range joined.Unwrap() {
		if _, ok := x.(*ruleError); !ok {
			internal = x
		}
	}
	const cause = `the plan comment's transition marker: format transition marker: bad field "model=a b"`
	if internal == nil || internal.Error() != "check the plan comment's length: "+cause {
		t.Fatalf("error %q has no error checking the length", err)
	}
	if got := errors.Unwrap(internal); got == nil || got.Error() != cause {
		t.Errorf("the length error %q wraps %v, want %q", internal, got, cause)
	}
}

// The length rule counts the whole posted comment, not only its text: a
// plan whose rendered text fits, but whose plan-data marker takes it over
// the limit, fails. A 10-task plan of real size passes.
func TestCheckPlanLengthCountsData(t *testing.T) {
	t.Parallel()
	f := storyFacts()
	f.MaxTasks = 10
	// Real 10-task plans come to 35–41K characters in all (#256's verify).
	sized := bigPlan(t, 10, 3000)
	body, err := planCommentBody(*sized.Plan, f)
	if err != nil {
		t.Fatal(err)
	}
	if n := textLen(body); n < 35000 || n > 45000 {
		t.Errorf("the 10-task plan's comment is %d characters, not of real size", n)
	}
	if err := checkPlan(sized, f); err != nil {
		t.Errorf("a 10-task plan of real size: %v", err)
	}
	out := bigPlan(t, 10, 6000)
	if n := textLen(renderPlan(*out.Plan, f.view())); n > (maxText-textRoom)/2 {
		t.Fatalf("the rendered text is %d characters, so it doesn't show the data is counted", n)
	}
	err = checkPlan(out, f)
	if got := rules(err); !slices.Equal(got, []string{ruleLength}) || !strings.Contains(err.Error(), "the plan comment is") {
		t.Errorf("a plan whose data doesn't fit: rules %q (%v), want length", got, err)
	}
}

// planCommentBody is exactly the comment Applier.Transition posts for the
// plan, with a run ID as long, so the length rule counts what's posted. A
// plan that skips review is counted with its move to planned.
func TestPlanCommentBodyIsPosted(t *testing.T) {
	t.Parallel()
	p := *bigPlan(t, 3, 100).Plan
	f := epicFacts()
	m, err := planDataMarker(p)
	if err != nil {
		t.Fatal(err)
	}
	fake := planningEpicFake(t)
	c := fake.Client(t)
	s, err := c.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	a := &Applier{Client: c, Owner: "o", Repo: "r", AppSlug: "issuedog"}
	mv := Move{To: state.PlanReview, Trigger: state.TriggerPlanValid, Key: state.Key{Field: "run", ID: "abcdefgh"}, PlanRev: f.Rev,
		Text: renderPlan(p, f.view()), Markers: []github.Marker{m}}
	if err := a.Transition(context.Background(), s, func(int64) bool { return false }, mv); err != nil {
		t.Fatal(err)
	}
	if s, err = c.Snapshot(context.Background(), "o", "r", 1); err != nil {
		t.Fatal(err)
	}
	posted := s.Comments[len(s.Comments)-1].Body
	want, err := planCommentBody(p, f)
	if err != nil {
		t.Fatal(err)
	}
	if textLen(want) != textLen(posted) {
		t.Errorf("planCommentBody is %d characters, the posted comment %d", textLen(want), textLen(posted))
	}
	// Only the run's ID, and so the hash, differ.
	want = strings.Replace(want, "run="+planLengthRunID, "run=abcdefgh", 1)
	if cut := func(s string) string { return s[:strings.LastIndex(s, " body=")] }; cut(want) != cut(posted) {
		t.Errorf("planCommentBody\n%s\nposted\n%s", want, posted)
	}

	f.NoReview = true
	got, err := planCommentBody(p, f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "\n\n<!-- issuedog:transition from=planning to=planned rev=1 run="+planLengthRunID+" -->\n<!-- issuedog:plan-data v=1 ") ||
		!strings.Contains(got, "This plan skips review") {
		t.Errorf("a plan that skips review is counted as\n%s", got)
	}
}
