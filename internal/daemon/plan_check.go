package daemon

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// planFacts is what checkPlan checks a planner output against.
type planFacts struct {
	Issue int         // the issue planned
	Class state.Class // its class: an epic, or a story with or without a parent
	// Scopes is config's scopes, the only ones a task may have.
	Scopes   []string
	MaxTasks int // plan.max_tasks, a hard limit (#247's Q2a)
	Models   config.Models
	// MaxQuestions is loop.max_questions_per_round, and Round the round
	// a question round is asked as.
	MaxQuestions int
	Round        int
	// SubIssues is the issue's sub-issues before the plan's are created.
	SubIssues int
	// Priority is the issue's priority, p0 to p3, which an item without
	// one of its own gets, or "" if the issue has none.
	Priority string
	Notify   []string // config's notify, for the cc line
	// Rev is the plan revision the plan is posted as, and NoReview is set
	// when the plan skips review (planView.NoReview): they change the
	// comment's text, which the length rule counts.
	Rev      int
	NoReview bool
	// Fields are the move's marker fields beside from, to, rev and run,
	// as Result.Move sets them: seen=, model=, effort= and ran=. They're
	// in the comment too, so the length rule counts them.
	Fields map[string]string
}

// view is the planView a plan is rendered with under f.
func (f planFacts) view() planView {
	return planView{Issue: f.Issue, Rev: f.Rev, Priority: f.Priority, Notify: f.Notify, NoReview: f.NoReview,
		MaxTasks: f.MaxTasks, SubIssues: f.SubIssues}
}

// The rules checkPlan enforces beside the interviewer's question rules,
// ruleOutcome, ruleOneLine and ruleLength. Each failure names one.
const (
	ruleStructure = "structure"
	ruleItemID    = "item-id"
	ruleScope     = "scope"
	ruleMaxTasks  = "max-tasks"
	ruleDependsOn = "depends-on"
	ruleCycle     = "cycle"
	ruleSubIssues = "sub-issues"
	ruleLinks     = "links"
	ruleModel     = "model"
	ruleEffort    = "effort"
	rulePriority  = "priority"
	ruleHumanTask = "human-task"
	ruleDoneWhen  = "done-when"
	ruleFilePaths = "file-paths"
)

// planItemIDRE is a plan item's ID: S<n> for a story, T<n> for a task.
var planItemIDRE = regexp.MustCompile(`^[ST][1-9][0-9]*$`)

// planPriorities are the priorities an item may have.
var planPriorities = []string{"p0", "p1", "p2", "p3"}

// planLengthRunID stands in for the run's ID in the transition marker the
// length rule counts. It's as long as a run's ID (Runs.newID).
const planLengthRunID = "00000000"

// checkPlan checks out against f after the schema, which it doesn't
// trust: the daemon fails closed, so every bound the schema has is
// checked again here, with those it leaves out (SPEC §11.3, plan #247's
// T6). Its error joins a *ruleError for every rule broken, each naming the
// item and field. Nothing may be written from an output that fails.
func checkPlan(out planOutput, f planFacts) error {
	var errs []error
	fail := func(rule, format string, args ...any) {
		errs = append(errs, &ruleError{Rule: rule, Detail: fmt.Sprintf(format, args...)})
	}
	switch out.Outcome {
	case outcomePlan:
		if out.Plan == nil || len(out.Questions) > 0 {
			fail(ruleOutcome, "a plan has a plan, and no questions")
		}
		if out.Plan != nil {
			checkPlanBody(*out.Plan, f, fail)
		}
	case outcomeQuestions:
		if out.Plan != nil || len(out.Questions) == 0 {
			fail(ruleOutcome, "a question round has questions, and no plan")
		}
		checkQuestions(out.Questions, interviewFacts{MaxQuestions: f.MaxQuestions}, fail)
		text := renderQuestions(interviewOutput{Questions: out.Questions}, f.Issue, f.Round, f.Notify)
		if n := textLen(text); n > maxText-textRoom {
			fail(ruleLength, "the question round's comment is %d characters, over %d", n, maxText-textRoom)
		}
	default:
		fail(ruleOutcome, "unknown outcome %q", out.Outcome)
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("check planner output: %w", errors.Join(errs...))
}

// checkPlanBody checks a plan's rules, calling fail for each one broken.
func checkPlanBody(p planBody, f planFacts, fail func(string, string, ...any)) {
	switch {
	case p.Kind != planStories && p.Kind != planTasks:
		fail(ruleStructure, "unknown plan kind %q", p.Kind)
	case f.Class == state.ClassEpic:
	case f.Class&state.AnyStory != 0:
		if p.Kind != planTasks {
			fail(ruleStructure, "a story's plan has tasks, not %s", p.Kind)
		}
	default:
		fail(ruleStructure, "only an epic or a story is planned")
	}
	if len(p.Items) == 0 {
		fail(ruleStructure, "the plan has no items")
	}
	if strings.TrimSpace(p.Summary) == "" {
		fail(ruleStructure, "the plan has no summary")
	}
	line := func(what, s string) {
		if !oneLine(s) {
			fail(ruleOneLine, "%s: %q", what, s)
		}
	}
	line("why", p.Why)
	line("order", p.Order)
	// visible is every field the plan comment shows, for the file-paths
	// rule (plan #247, assumption 8). Of a task's body, only a human task's
	// "Done when" text is shown.
	visible := []rendered{{"the why", p.Why}, {"the summary", p.Summary}, {"the message", p.Message}, {"the order", p.Order}}
	for i, a := range p.Assumptions {
		what := "assumption " + strconv.Itoa(i+1)
		line(what, a)
		visible = append(visible, rendered{what, a})
	}

	ids := map[string]bool{}
	blocking := map[string]int{}
	for _, it := range p.Items {
		for _, d := range it.DependsOn {
			blocking[d]++
		}
	}
	tasks := 0
	for i, it := range p.Items {
		at := it.ID
		if !planItemIDRE.MatchString(it.ID) {
			at = "item " + strconv.Itoa(i+1)
		}
		switch {
		case !planItemIDRE.MatchString(it.ID):
			fail(ruleItemID, "%s's ID %q isn't S<n> or T<n>", at, it.ID)
		case ids[it.ID]:
			fail(ruleItemID, "%s is the ID of more than one item", at)
		case p.Kind == planStories && it.ID[0] != 'S':
			fail(ruleItemID, "%s is in a plan of stories, whose IDs start with S", at)
		case p.Kind == planTasks && it.ID[0] != 'T':
			fail(ruleItemID, "%s is in a plan of tasks, whose IDs start with T", at)
		}
		ids[it.ID] = true

		line(at+"'s title", it.Title)
		line(at+"'s outcome", it.Outcome)
		if len(it.Criteria) == 0 {
			fail(ruleStructure, "%s has no criteria", at)
		}
		visible = append(visible, rendered{at + "'s title", it.Title}, rendered{at + "'s outcome", it.Outcome})
		for j, c := range it.Criteria {
			what := fmt.Sprintf("%s's criterion %d", at, j+1)
			line(what, c)
			visible = append(visible, rendered{what, c})
		}

		if it.Priority != "" && !slices.Contains(planPriorities, it.Priority) {
			fail(rulePriority, "%s's priority %q isn't p0 to p3", at, it.Priority)
		}
		if _, ok := f.Models.Resolve(it.Model); it.Model != "" && !ok {
			fail(ruleModel, "%s's model %q isn't in models.allowed", at, it.Model)
		}
		if it.Effort != "" && !config.ValidEffort(it.Effort) {
			fail(ruleEffort, "%s's effort %q isn't one of %s", at, it.Effort, strings.Join(config.EffortLevels(), ", "))
		}
		if n := len(it.DependsOn); n > planMaxLinks {
			fail(ruleLinks, "%s is blocked by %d items, over %d", at, n, planMaxLinks)
		}
		if n := blocking[it.ID]; n > planMaxLinks {
			fail(ruleLinks, "%s blocks %d items, over %d", at, n, planMaxLinks)
		}

		if p.Kind == planStories {
			if it.Scope != "" || it.Human || it.Task != nil || it.HumanTask != nil {
				fail(ruleStructure, "%s is a story, with no scope, human, task or human_task", at)
			}
		}
		if p.Kind == planTasks {
			tasks++
			switch {
			case (it.Task == nil) == (it.HumanTask == nil):
				fail(ruleStructure, "%s has %s, not exactly one of task and human_task", at, planBodies(it))
			case it.Human != (it.HumanTask != nil):
				fail(ruleStructure, "%s has human %v with %s", at, it.Human, planBodies(it))
			}
			if it.Scope == "" {
				fail(ruleScope, "%s has no scope", at)
			} else if !slices.Contains(f.Scopes, it.Scope) {
				fail(ruleScope, "%s's scope %q isn't on the list", at, it.Scope)
			}
		}
		if t := it.Task; t != nil {
			for _, s := range []rendered{{"context", t.Context}, {"change", t.Change}, {"test plan", t.TestPlan}} {
				if strings.TrimSpace(s.text) == "" {
					fail(ruleStructure, "%s's task body has no %s", at, s.what)
				}
			}
			if len(t.DefinitionOfDone) == 0 {
				fail(ruleStructure, "%s's task body has no definition of done", at)
			}
			for j, d := range t.DefinitionOfDone {
				line(fmt.Sprintf("%s's definition of done %d", at, j+1), d)
			}
		}
		if h := it.HumanTask; h != nil {
			checkHumanTask(at, *h, fail)
			visible = append(visible, rendered{at + "'s done when", h.DoneWhen.Text})
		}
	}
	if p.Kind == planTasks && tasks > f.MaxTasks {
		fail(ruleMaxTasks, "%d tasks, over plan.max_tasks of %d", tasks, f.MaxTasks)
	}
	if n := f.SubIssues + len(p.Items); n > planMaxSubIssues {
		fail(ruleSubIssues, "%d sub-issues already and %d items make %d, over %d", f.SubIssues, len(p.Items), n, planMaxSubIssues)
	}
	checkPlanDeps(p, ids, fail)
	for _, v := range visible {
		if path := planFilePath(v.text); path != "" {
			fail(ruleFilePaths, "%s names the file path %q", v.what, path)
		}
	}
	checkPlanLength(p, f, fail)
}

// planBodies names which of task and human_task it has, for a detail.
func planBodies(it planItem) string {
	switch {
	case it.Task != nil && it.HumanTask != nil:
		return "task and human_task"
	case it.Task != nil:
		return "task"
	case it.HumanTask != nil:
		return "human_task"
	}
	return "neither task nor human_task"
}

// checkHumanTask checks that a human task fills every field of the SPEC
// §9.3 template, the one-line fields are one line, and its "Done when"
// check, if any, is valid (validCheck).
func checkHumanTask(at string, h planHumanTask, fail func(string, string, ...any)) {
	filled := func(what, s string) {
		switch {
		case strings.TrimSpace(s) == "":
			fail(ruleHumanTask, "%s's %s is empty", at, what)
		case !oneLine(s):
			fail(ruleOneLine, "%s's %s: %q", at, what, s)
		}
	}
	filled("goal", h.Goal)
	filled("why it blocks", h.WhyBlocks)
	if len(h.Steps) == 0 {
		fail(ruleHumanTask, "%s has no steps", at)
	}
	for j, s := range h.Steps {
		filled("step "+strconv.Itoa(j+1), s)
	}
	if len(h.Links) == 0 {
		fail(ruleHumanTask, "%s has no links", at)
	}
	for j, l := range h.Links {
		filled("link "+strconv.Itoa(j+1), l)
	}
	filled("done when", h.DoneWhen.Text)
	filled("estimated time", h.EstimatedTime)
	if c := h.DoneWhen.Check; c != nil {
		if err := validCheck(*c); err != nil {
			fail(ruleDoneWhen, "%s's %v", at, err)
		}
	}
}

// The bounds of a "Done when" check's values (#224's reconcile, decision
// 5).
const (
	planMaxLabel    = 50  // characters in a label's name, as GitHub caps it
	planMaxContains = 256 // bytes of a file's contains text
)

// validCheck reports whether c is a "Done when" check the daemon can run:
// a known kind, exactly that kind's fields, and values within S4's bounds
// (#224's reconcile, decision 5):
//   - label: a label of at most 50 characters, and not "." or "..", which
//     go-github refuses or reads as the label list;
//   - issue-type: an issue type that isn't empty, on one line;
//   - file: a path from the repo root, on one line, with no leading "/",
//     no backslash, and no "", "." or ".." segment, and, if set, contains
//     of at most 256 bytes on one line;
//   - ruleset: a check name that isn't empty, on one line;
//   - installation: an owner that's a user's or organization's login.
//
// checkPlan checks a plan's checks with it, and S4 checks the check it
// runs with it, so the two can't differ. Its error names every value out
// of bounds.
func validCheck(c planCheck) error {
	var bad []string
	fields := map[string]string{"label": c.Label, "issue_type": c.IssueType, "path": c.Path, "contains": c.Contains, "check": c.Check, "owner": c.Owner}
	var own []string
	switch c.Kind {
	case planCheckLabel:
		own = []string{"label"}
		switch n := utf8.RuneCountInString(c.Label); {
		case !oneLine(c.Label):
			bad = append(bad, fmt.Sprintf("label %q isn't one line", c.Label))
		case n > planMaxLabel:
			bad = append(bad, fmt.Sprintf("label is %d characters, over %d", n, planMaxLabel))
		case c.Label == "." || c.Label == "..":
			bad = append(bad, fmt.Sprintf("label %q isn't a label's name", c.Label))
		}
	case planCheckIssueType:
		own = []string{"issue_type"}
		if !oneLine(c.IssueType) {
			bad = append(bad, fmt.Sprintf("issue_type %q isn't one line", c.IssueType))
		}
	case planCheckFile:
		own = []string{"path", "contains"}
		if why := badCheckPath(c.Path); why != "" {
			bad = append(bad, fmt.Sprintf("path %q %s", c.Path, why))
		}
		if c.Contains != "" {
			if !oneLine(c.Contains) {
				bad = append(bad, fmt.Sprintf("contains %q isn't one line", c.Contains))
			}
			if n := len(c.Contains); n > planMaxContains {
				bad = append(bad, fmt.Sprintf("contains is %d bytes, over %d", n, planMaxContains))
			}
		}
	case planCheckRuleset:
		own = []string{"check"}
		if !oneLine(c.Check) {
			bad = append(bad, fmt.Sprintf("check %q isn't one line", c.Check))
		}
	case planCheckInstallation:
		own = []string{"owner"}
		if m := ivLoginRE.FindStringSubmatch(c.Owner); m == nil || m[2] != "" || len(m[1]) > ivMaxLogin {
			bad = append(bad, fmt.Sprintf("owner %q isn't a user's or organization's login", c.Owner))
		}
	default:
		return fmt.Errorf("check has the unknown kind %q", c.Kind)
	}
	for _, name := range []string{"label", "issue_type", "path", "contains", "check", "owner"} {
		if fields[name] != "" && !slices.Contains(own, name) {
			bad = append(bad, fmt.Sprintf("%s isn't a field of kind %s", name, c.Kind))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("%s check: %s", c.Kind, strings.Join(bad, "; "))
}

// badCheckPath is why path isn't a file check's path, or "" if it is one.
func badCheckPath(path string) string {
	switch {
	case !oneLine(path):
		return "isn't one line"
	case strings.HasPrefix(path, "/"):
		return "starts with /"
	case strings.Contains(path, `\`):
		return "has a backslash"
	case strings.TrimLeft(path, " \t") != path:
		return "starts with a space"
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Sprintf("has the segment %q", seg)
		}
	}
	return ""
}

// checkPlanDeps checks each item's dependencies: no unknown ID, no item
// depending on itself or naming an item twice, and no cycle. ids is the
// plan's item IDs.
func checkPlanDeps(p planBody, ids map[string]bool, fail func(string, string, ...any)) {
	deps := map[string][]string{}
	for _, it := range p.Items {
		seen := map[string]bool{}
		for _, d := range it.DependsOn {
			switch {
			case d == it.ID:
				fail(ruleDependsOn, "%s depends on itself", it.ID)
			case !ids[d]:
				fail(ruleDependsOn, "%s depends on %q, which isn't an item of this plan", it.ID, d)
			case seen[d]:
				fail(ruleDependsOn, "%s depends on %s more than once", it.ID, d)
			default:
				deps[it.ID] = append(deps[it.ID], d)
			}
			seen[d] = true
		}
	}
	// A depth-first search, in plan order, finds each cycle by the
	// dependency that closes it, and names it from where it starts.
	const (
		unseen = iota
		onPath
		done
	)
	mark := map[string]int{}
	var path []string
	var visit func(id string)
	visit = func(id string) {
		mark[id] = onPath
		path = append(path, id)
		for _, d := range deps[id] {
			switch mark[d] {
			case unseen:
				visit(d)
			case onPath:
				cycle := append(slices.Clone(path[slices.Index(path, d):]), d)
				fail(ruleCycle, "%s", strings.Join(cycle, " → "))
			}
		}
		path = path[:len(path)-1]
		mark[id] = done
	}
	for _, it := range p.Items {
		if mark[it.ID] == unseen && ids[it.ID] {
			visit(it.ID)
		}
	}
}

// Links are left out of the text planFilePath looks in: a link may well
// point at a file, and that's no file path in the plan's text.
var (
	// planLinkRE is a URL, bare, in an autolink or as a link's or link
	// reference definition's destination. A link's text stays.
	planLinkRE = regexp.MustCompile(`(?i)\]\([^)]*\)|\]:[ \t]*\S+|<[a-z][a-z0-9+.-]*:[^>\s]*>|\b(?:[a-z][a-z0-9+.-]*://|www\.)\S+`)
	// planPathRE is a word with a directory and a file extension, as plan
	// #247's assumption 8 defines a file path. A bare file name, with no
	// directory, isn't one.
	planPathRE = regexp.MustCompile(`[\w.-]+/[\w./-]*\.[A-Za-z0-9]+\b`)
)

// planFilePath is the first file path in text outside a link, or "".
func planFilePath(text string) string {
	return planPathRE.FindString(planLinkRE.ReplaceAllString(text, " "))
}

// checkPlanLength checks that the whole plan comment, planCommentBody, fits
// GitHub's limit with textRoom to spare (#256's verify). The data is about
// 40% of the plan's JSON, so the text alone could fit a comment GitHub
// then refuses on every attempt.
func checkPlanLength(p planBody, f planFacts, fail func(string, string, ...any)) {
	body, err := planCommentBody(p, f)
	if err != nil {
		fail(ruleLength, "%v", err)
		return
	}
	if n := textLen(body); n > maxText-textRoom {
		fail(ruleLength, "the plan comment is %d characters, with its markers, over %d", n, maxText-textRoom)
	}
}

// planCommentBody is the plan comment p is posted as under f, as
// Applier.Transition writes it: the rendered plan, the move's transition
// marker, with f.Fields, the plan-data marker, and the plan revision
// marker. Only the run's ID stands in, planLengthRunID, as long as the
// real one.
func planCommentBody(p planBody, f planFacts) (string, error) {
	to := state.PlanReview
	if f.NoReview {
		to = state.Planned
	}
	fields := maps.Clone(f.Fields)
	if fields == nil {
		fields = map[string]string{}
	}
	fields["from"], fields["to"], fields["rev"], fields["run"] = string(state.Planning), string(to), strconv.Itoa(f.Rev), planLengthRunID
	transition, err := github.FormatMarker(github.Marker{Kind: "transition", Fields: fields})
	if err != nil {
		return "", fmt.Errorf("the plan comment's transition marker: %w", err)
	}
	m, err := planDataMarker(p)
	if err != nil {
		return "", fmt.Errorf("the plan comment's %w", err)
	}
	data, err := github.FormatMarker(m)
	if err != nil {
		return "", fmt.Errorf("the plan comment's plan-data marker: %w", err)
	}
	body, err := planRevision(join(github.Escape(renderPlan(p, f.view())), transition+"\n"+data), f.Rev)
	if err != nil {
		return "", fmt.Errorf("the plan comment's %w", err)
	}
	return body, nil
}
