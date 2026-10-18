package daemon

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

const childSlug = "issuedog"

// childGolden compares got with testdata/children/<name>.md, or writes it
// there with -update.
func childGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "children", name+".md")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got+"\n" != string(want) {
		t.Errorf("%s: got\n%s\nwant\n%s", name, got, want)
	}
}

// childParent is a parent snapshot: issue n with labels.
func childParent(n int, labels ...string) *github.Snapshot {
	return &github.Snapshot{Number: n, Labels: labels}
}

// childItem is item id of the golden plan name.
func childItem(t *testing.T, plan, id string) planItem {
	t.Helper()
	for _, it := range goldenPlan(t, plan).Items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("%s has no item %s", plan, id)
	return planItem{}
}

// childOne is childSpecs for it alone, which must succeed.
func childOne(t *testing.T, parent *github.Snapshot, it planItem, issueTypes bool) childSpec {
	t.Helper()
	specs, err := childSpecs(parent, 1, []planItem{it}, issueTypes, []string{"mertenvg"})
	if err != nil {
		t.Fatalf("childSpecs(%s): %v", it.ID, err)
	}
	if len(specs) != 1 {
		t.Fatalf("childSpecs(%s): %d specs, want 1", it.ID, len(specs))
	}
	return specs[0]
}

// childBody is c's body, given numbers, the issue number of each child
// filed before it, by item, which must give one (childSpec.bodyOf). A
// human task's Body is "" until then.
func childBody(t *testing.T, c childSpec, numbers map[string]int) string {
	t.Helper()
	if c.human != nil && c.Body != "" {
		t.Errorf("%s: a human task with a Body before its blockers are filed:\n%s", c.Item, c.Body)
	}
	body, err := c.bodyOf(numbers)
	if err != nil {
		t.Fatalf("%s: %v", c.Item, err)
	}
	return body
}

// childSplit splits body into its text and its last line, which must be
// its only marker, an origin marker.
func childSplit(t *testing.T, name, body string) (string, github.Marker) {
	t.Helper()
	text, last, ok := strings.Cut(body, "\n\n<!-- issuedog:origin ")
	if !ok || strings.Contains(last, "\n") {
		t.Fatalf("%s: the body doesn't end in its origin line:\n%s", name, body)
	}
	ms := github.ParseMarkers(body)
	if len(ms) != 1 || ms[0].Kind != "origin" {
		t.Fatalf("%s: markers %+v, want the origin marker alone", name, ms)
	}
	return text, ms[0]
}

// The goldens: a story from the epic plan, and a task and a human task
// from the story plan, each with its sections in order, its origin line
// last and no other marker, and safe to post.
func TestChildSpecsGolden(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, plan, item string
		parent           *github.Snapshot
		title            string
		headings         []string // the body's # and ## headings, in order
		lines            []string // lines the body has
		origin           string   // the origin line
	}{
		{"story", "epic-plan", "S2", childParent(123, "priority:p1"), "S2. Customers see when a quote expires",
			[]string{"## Outcome", "## Acceptance criteria"},
			[]string{"**Parent:** #123   **Priority:** p1", "Depends on: S1.", "- A quote from the MCP server includes its expiry time."},
			"<!-- issuedog:origin plan=#123 rev=1 item=S2 -->"},
		{"task", "story-plan", "T2", childParent(124, "priority:p2"), "Refuse an expired quote at booking",
			[]string{"## Context", "## Change", "## Acceptance criteria", "## Definition of done", "## Test plan"},
			[]string{"**Parent:** #124   **Scope:** scope:booking   **Priority:** p1",
				"- `internal/booking/book.go`: `Book` checks the quote's status and creates the booking in one transaction.",
				"- No new skip markers"},
			"<!-- issuedog:origin plan=#124 rev=1 item=T2 -->"},
		{"human-task", "story-plan", "T3", childParent(124, "priority:p2"), "Set the quote lifetime in production",
			nil,
			[]string{"**Parent:** #124   **Scope:** scope:infra   **Priority:** p2",
				"**Goal:** Set the quote lifetime to 24 hours in production's settings.",
				// Alone, nothing of its plan depends on it, so it names the parent.
				"**Why it blocks:** #124 can't proceed until production has a quote lifetime, because the booking service won't start without one, and the deploy settings are a path only the owner can change.",
				"**Steps:**", "2. Add the line `QUOTE_TTL=24h`.",
				"**Links:**", "- https://github.com/acme/booking/blob/main/deploy/production.env",
				"**Done when:** The file `deploy/production.env` on `main` contains `QUOTE_TTL=24h`. I check this when you reply `/done` or react 👍 to this issue.",
				"**Estimated time:** 5 min", "cc @mertenvg"},
			"<!-- issuedog:origin plan=#124 rev=1 item=T3 check=file contains=QUOTE_TTL=24h human=1 value=deploy/production.env -->"},
	} {
		c := childOne(t, tc.parent, childItem(t, tc.plan, tc.item), false)
		body := childBody(t, c, nil)
		childGolden(t, tc.name, body)
		if c.Item != tc.item || c.Title != tc.title {
			t.Errorf("%s: item %q, title %q; want %q, %q", tc.name, c.Item, c.Title, tc.item, tc.title)
		}
		text, _ := childSplit(t, tc.name, body)
		checkSafe(t, text)
		if h := headings(text); !slices.Equal(h, tc.headings) {
			t.Errorf("%s: headings %q, want %q", tc.name, h, tc.headings)
		}
		lines := strings.Split(body, "\n")
		if last := lines[len(lines)-1]; last != tc.origin {
			t.Errorf("%s: last line %q, want %q", tc.name, last, tc.origin)
		}
		for _, w := range tc.lines {
			if !slices.Contains(lines, w) {
				t.Errorf("%s: no line %q in\n%s", tc.name, w, body)
			}
		}
	}
}

// A story that depends on nothing says so, and one that depends on
// several names each, in the plan's order.
func TestChildStoryDependsOn(t *testing.T) {
	t.Parallel()
	it := childItem(t, "epic-plan", "S1")
	if body := childOne(t, childParent(123), it, false).Body; !strings.Contains(body, "\n\nDepends on: nothing.\n\n") {
		t.Errorf("S1: no \"Depends on: nothing.\" in\n%s", body)
	}
	it.ID, it.DependsOn = "S4", []string{"S3", "S1"}
	if body := childOne(t, childParent(123), it, false).Body; !strings.Contains(body, "\n\nDepends on: S3, S1.\n\n") {
		t.Errorf("S4: no \"Depends on: S3, S1.\" in\n%s", body)
	}
}

// A human task's body has every §9.3 field, in order, and its cc line
// before its origin line.
func TestChildHumanTaskFieldOrder(t *testing.T) {
	t.Parallel()
	body := childBody(t, childOne(t, childParent(124), childItem(t, "story-plan", "T3"), false), nil)
	at := -1
	for _, f := range []string{"**Parent:**", "**Goal:**", "**Why it blocks:**", "**Steps:**", "**Links:**", "**Done when:**", "**Estimated time:**", "cc @mertenvg", "<!-- issuedog:origin"} {
		i := strings.Index(body, "\n"+f)
		if f == "**Parent:**" && strings.HasPrefix(body, f) {
			i = 0
		}
		if i <= at {
			t.Errorf("%q at %d, not after %d in\n%s", f, i, at, body)
		}
		at = i
	}
}

// Titles: a story's starts with its ID, once, whatever the planner wrote;
// a task's is as planned. Either is one line, trimmed.
func TestChildTitle(t *testing.T) {
	t.Parallel()
	task := childItem(t, "story-plan", "T2")
	for _, tc := range []struct {
		name, id, title, want string
	}{
		{"a story", "S2", "Plan review", "S2. Plan review"},
		{"a story with its prefix", "S2", "S2. Plan review", "S2. Plan review"},
		{"a story with another's prefix", "S1", "S12. Plan review", "S1. S12. Plan review"},
		{"a story with no space after its prefix", "S2", "S2.Plan review", "S2. S2.Plan review"},
		{"a story on two lines", "S2", "  Plan\nreview ", "S2. Plan review"},
		{"a task", "T2", "Plan review", "Plan review"},
		{"a task with a prefix", "T2", "T2. Plan review", "T2. Plan review"},
	} {
		it := planItem{ID: tc.id, Title: tc.title, Outcome: "Done.", Criteria: []string{"It works."}}
		if strings.HasPrefix(tc.id, "T") {
			it = task
			it.ID, it.Title = tc.id, tc.title
		}
		if got := childOne(t, childParent(9), it, false).Title; got != tc.want {
			t.Errorf("%s: title %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Labels: type, scope for a task, priority and stage on a labels repo; on
// an issue-types repo no type label, and the issue type instead. The stage
// is each kind's first, and the created row for its class enters it.
func TestChildLabelsAndStage(t *testing.T) {
	t.Parallel()
	parent := childParent(124, "priority:p2")
	story := childItem(t, "epic-plan", "S2")
	task := childItem(t, "story-plan", "T1")
	human := childItem(t, "story-plan", "T3")
	for _, tc := range []struct {
		name       string
		it         planItem
		issueTypes bool
		labels     []string
		issueType  string
		stage      state.Stage
	}{
		{"story, labels", story, false, []string{"type:story", "priority:p2", "stage:planning"}, "", state.Planning},
		{"task, labels", task, false, []string{"type:task", "scope:booking", "priority:p2", "stage:ready"}, "", state.Ready},
		{"human task, labels", human, false, []string{"type:task", "scope:infra", "priority:p2", "stage:needs-human"}, "", state.NeedsHuman},
		{"story, issue types", story, true, []string{"priority:p2", "stage:planning"}, "Story", state.Planning},
		{"task, issue types", task, true, []string{"scope:booking", "priority:p2", "stage:ready"}, "Task", state.Ready},
		{"human task, issue types", human, true, []string{"scope:infra", "priority:p2", "stage:needs-human"}, "Task", state.NeedsHuman},
	} {
		c := childOne(t, parent, tc.it, tc.issueTypes)
		if !slices.Equal(c.Labels, tc.labels) || c.IssueType != tc.issueType || c.Stage != tc.stage {
			t.Errorf("%s: labels %q, type %q, stage %s; want %q, %q, %s", tc.name, c.Labels, c.IssueType, c.Stage, tc.labels, tc.issueType, tc.stage)
		}
	}
}

// What the daemon makes of a created child: its origin is the plan item,
// a human task's human=1 makes it one, and its check reads back, and with
// its parent and type its class has a created row to its first stage.
func TestChildSpecsDerive(t *testing.T) {
	t.Parallel()
	app := github.Author{Login: childSlug + "[bot]", Type: "Bot"}
	for _, tc := range []struct {
		name, plan, item, typ string
		class                 state.Class
		check                 state.Check
	}{
		{"story", "epic-plan", "S2", "story", state.ClassStory, state.Check{}},
		{"task", "story-plan", "T1", "task", state.ClassTask, state.Check{}},
		{"human task", "story-plan", "T3", "task", state.ClassHumanTask,
			state.Check{Kind: state.CheckFile, Value: "deploy/production.env", Contains: "QUOTE_TTL=24h"}},
	} {
		c := childOne(t, childParent(124), childItem(t, tc.plan, tc.item), false)
		s := &github.Snapshot{Number: 200, Author: app, Body: childBody(t, c, nil), Type: tc.typ, Parent: 124}
		is := state.Derive(s, childSlug, func(int64) bool { return false })
		want := state.Origin{Plan: 124, Rev: 1, Item: tc.item, Comment: state.BodyID, Human: tc.class == state.ClassHumanTask, Check: tc.check}
		if is.Origin != want {
			t.Errorf("%s: origin %+v, want %+v", tc.name, is.Origin, want)
		}
		cl, err := state.ClassOf(is.Item)
		if err != nil || cl != tc.class {
			t.Errorf("%s: class %s, %v; want %s", tc.name, cl, err, tc.class)
		}
		if _, err := state.Lookup(state.Intake, c.Stage, cl, state.TriggerCreated); err != nil {
			t.Errorf("%s: no created row to %s: %v", tc.name, c.Stage, err)
		}
		if is.Stage != state.Intake {
			t.Errorf("%s: stage %s before its first move, want intake", tc.name, is.Stage)
		}
	}
}

// Priority: the item's, else the parent's priority:pN in any case, else
// p2. It's in the body's Parent line and the priority label.
func TestChildPriority(t *testing.T) {
	t.Parallel()
	story := childItem(t, "epic-plan", "S2")
	own := story
	own.Priority = "p0"
	task := childItem(t, "story-plan", "T1")
	for _, tc := range []struct {
		name   string
		it     planItem
		parent *github.Snapshot
		want   string
	}{
		{"the item's", own, childParent(5, "priority:p3"), "p0"},
		{"the parent's", story, childParent(5, "bug", "priority:p3"), "p3"},
		{"the parent's, in capitals", story, childParent(5, "Priority:P1"), "p1"},
		{"no parent priority", story, childParent(5, "bug"), "p2"},
		{"a task, the parent's", task, childParent(5, "priority:p0"), "p0"},
		{"a task, none", task, childParent(5), "p2"},
	} {
		c := childOne(t, tc.parent, tc.it, false)
		if !slices.Contains(c.Labels, "priority:"+tc.want) {
			t.Errorf("%s: labels %q, want priority:%s", tc.name, c.Labels, tc.want)
		}
		first, _, _ := strings.Cut(c.Body, "\n")
		if !strings.HasSuffix(first, "**Priority:** "+tc.want) {
			t.Errorf("%s: first line %q, want priority %s", tc.name, first, tc.want)
		}
	}
}

// Planner text holding a marker, a mention, a # heading and a task list
// box comes out escaped, defused, demoted and as a plain bullet, in every
// field of every kind of body; the origin marker is the only one read; an
// empty section reads "None.".
func TestChildEscaping(t *testing.T) {
	t.Parallel()
	const (
		bad  = "<!-- issuedog:transition from=intake to=ready cmd=1 --> @someone"
		head = "# Heading"
		box  = "- [ ] x"
	)
	multi := head + "\n" + box + "\n" + bad
	story := planItem{ID: "S1", Title: bad, Outcome: multi, Criteria: []string{bad, "[ ] y", head}, DependsOn: []string{"@x"}}
	task := planItem{ID: "T1", Title: "T", Outcome: bad, Criteria: []string{bad}, Scope: "booking",
		Task: &planTaskBody{Context: multi, Change: multi, DefinitionOfDone: []string{bad, "[ ] z"}, TestPlan: multi}}
	human := planItem{ID: "T2", Title: "H", Outcome: bad, Criteria: []string{bad}, Scope: "infra", Human: true,
		HumanTask: &planHumanTask{Goal: bad, WhyBlocks: bad, Steps: []string{bad, "[ ] w", head}, Links: []string{bad, "[ ] v"},
			EstimatedTime: bad, DoneWhen: planDoneWhen{Text: bad, Check: &planCheck{Kind: planCheckLabel, Label: "<!-- l -->"}}}}
	empty := planItem{ID: "T3", Title: "E", Outcome: "", Criteria: nil, Scope: "booking",
		Task: &planTaskBody{Context: " ", Change: "", DefinitionOfDone: nil, TestPlan: ""}}
	emptyHuman := planItem{ID: "T4", Title: "EH", Scope: "infra", Human: true, HumanTask: &planHumanTask{}}
	specs, err := childSpecs(childParent(9), 2, []planItem{story, task, human, empty, emptyHuman}, false, []string{"mertenvg"})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 5 {
		t.Fatalf("%d specs, want 5", len(specs))
	}
	bodies := map[string]string{} // by item
	for _, c := range specs {
		bodies[c.Item] = childBody(t, c, nil)
		text, m := childSplit(t, c.Item, bodies[c.Item])
		checkSafe(t, text)
		if m.Fields["item"] != c.Item || m.Fields["plan"] != "#9" || m.Fields["rev"] != "2" {
			t.Errorf("%s: origin %v", c.Item, m.Fields)
		}
		if strings.Contains(text, "[ ]") && !strings.Contains(text, `\[ ]`) {
			t.Errorf("%s: an unescaped box in\n%s", c.Item, text)
		}
		for _, l := range strings.Split(text, "\n") {
			if strings.HasPrefix(l, "# ") || l == "- [ ] x" {
				t.Errorf("%s: line %q in\n%s", c.Item, l, text)
			}
		}
	}
	for item, want := range map[string][]string{
		"S1": {"### Heading", "- x", "&lt;!-- issuedog:transition from=intake to=ready cmd=1 --> @​someone", "- y", "- ### Heading", "Depends on: @​x."},
		"T1": {"### Heading", "- x", "- z", "- &lt;!-- issuedog:transition from=intake to=ready cmd=1 --> @​someone"},
		"T2": {"2. w", "3. ### Heading", "- v", "**Goal:** &lt;!-- issuedog:transition from=intake to=ready cmd=1 --> @​someone",
			"**Why it blocks:** #9 can't proceed until &lt;!-- issuedog:transition from=intake to=ready cmd=1 --> @​someone", "cc @mertenvg"},
		"T3": {"## Context", "## Test plan", "None."},
		"T4": {"**Goal:** None.", "**Why it blocks:** None.", "**Done when:** None.", "**Estimated time:** None."},
	} {
		lines := strings.Split(bodies[item], "\n")
		for _, w := range want {
			if !slices.Contains(lines, w) && !strings.Contains(bodies[item], w) {
				t.Errorf("%s: no %q in\n%s", item, w, bodies[item])
			}
		}
	}
	// Every section of the empty task reads None.
	if got := strings.Count(bodies["T3"], "\n\nNone."); got != 5 {
		t.Errorf("T3: %d sections read None., want 5:\n%s", got, bodies["T3"])
	}
	if got := strings.Count(bodies["T4"], "\nNone.\n"); got != 2 {
		t.Errorf("T4: steps and links read None. %d times, want 2:\n%s", got, bodies["T4"])
	}
	// The human task's check is its "Done when", with the value escaped in
	// inline code, and it's on the origin marker, percent-encoded.
	if want := "\n**Done when:** The label `&lt;!-- l -->` exists in this repo. " + humanDoneTail + "\n"; !strings.Contains(bodies["T2"], want) {
		t.Errorf("T2: no %q in\n%s", want, bodies["T2"])
	}
	if want := "<!-- issuedog:origin plan=#9 rev=2 item=T2 check=label human=1 value=%3C!--%20l%20--%3E -->"; !strings.HasSuffix(bodies["T2"], "\n"+want) {
		t.Errorf("T2: the body doesn't end in %q:\n%s", want, bodies["T2"])
	}
}

// A body over childMaxBody is an error naming its item, and one at it
// isn't. Nothing is described for any item then.
func TestChildSize(t *testing.T) {
	t.Parallel()
	task := childItem(t, "story-plan", "T1")
	task.ID = "T7"
	task.Task = &planTaskBody{Context: "x", Change: "", DefinitionOfDone: []string{"d"}, TestPlan: "p"}
	small := childOne(t, childParent(5), task, false)
	room := childMaxBody - textLen(small.Body) + len("None.")
	task.Task.Change = strings.Repeat("é", room) // one UTF-16 unit each
	at := childOne(t, childParent(5), task, false)
	if n := textLen(at.Body); n != childMaxBody {
		t.Fatalf("the body at the limit is %d, not %d", n, childMaxBody)
	}
	task.Task.Change += "€" // one more, so one over
	first := childItem(t, "story-plan", "T2")
	specs, err := childSpecs(childParent(5), 1, []planItem{first, task}, false, nil)
	if err == nil || !strings.Contains(err.Error(), "item T7") || !strings.Contains(err.Error(), "over 64512") || specs != nil {
		t.Errorf("childSpecs = %d specs, %v; want an error naming T7", len(specs), err)
	}
}

// An item childSpecs can't describe is an error naming it: a task with no
// scope, a human task with no human task body, and a task with none.
func TestChildSpecsErrors(t *testing.T) {
	t.Parallel()
	task := childItem(t, "story-plan", "T1")
	noScope := task
	noScope.Scope = ""
	noHuman := childItem(t, "story-plan", "T3")
	noHuman.HumanTask = nil
	noTask := task
	noTask.Task = nil
	for _, tc := range []struct {
		name string
		it   planItem
		want string
	}{
		{"no scope", noScope, "no scope"},
		{"no human task body", noHuman, "no human task body"},
		{"no task body", noTask, "no task body"},
	} {
		specs, err := childSpecs(childParent(5), 1, []planItem{tc.it}, false, nil)
		if err == nil || !strings.Contains(err.Error(), "item "+tc.it.ID+": ") || !strings.Contains(err.Error(), tc.want) || specs != nil {
			t.Errorf("%s: %v, %v; want an error naming %s", tc.name, specs, err, tc.it.ID)
		}
	}
}

// childSpecs keeps plan order, with the human task last, as it is here,
// and describes every item.
func TestChildSpecsOrder(t *testing.T) {
	t.Parallel()
	p := goldenPlan(t, "story-plan")
	specs, err := childSpecs(childParent(124), 3, p.Items, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range specs {
		got = append(got, c.Item)
	}
	if want := []string{"T1", "T2", "T3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("items %q, want %q", got, want)
	}
	// With no notify, a human task has no cc line.
	if body := childBody(t, specs[2], map[string]int{"T1": 125, "T2": 126}); strings.Contains(body, "cc @") {
		t.Errorf("a cc line with no notify:\n%s", body)
	}
}
