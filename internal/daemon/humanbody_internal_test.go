package daemon

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// codeSpanRE is an inline code span with no backtick inside, as checkCode
// writes a value with none.
var codeSpanRE = regexp.MustCompile("`[^`\n]*`")

// childAppBody is a task the App filed under #124 with body, as Derive
// reads it.
func childAppBody(body string) *github.Snapshot {
	return &github.Snapshot{Number: 200, Author: github.Author{Login: childSlug + "[bot]", Type: "Bot"}, Body: body, Type: "task", Parent: 124}
}

// humanItem is the golden story plan's human task, T3, with its "Done
// when" check c, or none for nil.
func humanItem(t *testing.T, c *planCheck) planItem {
	t.Helper()
	it := childItem(t, "story-plan", "T3")
	h := *it.HumanTask
	h.DoneWhen.Check = c
	it.HumanTask = &h
	return it
}

// The goldens: a created human task's body for each kind of check, and for
// plain text, each with its six fields filled, in order, then the cc line
// and its origin marker, which records the check and reads back. With a
// check, "Done when" is the daemon's sentence, naming the check's values
// in inline code; with plain text, it's the planner's text. Here T2
// depends on T3, so "Why it blocks" names T2's issue, #126, except for
// plain text, where nothing does, so it names the parent, #124.
func TestHumanBodyGolden(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		check *planCheck
		done  string // the Done when line
		want  state.Check
	}{
		{"label", &planCheck{Kind: planCheckLabel, Label: "scope:sandbox"},
			"**Done when:** The label `scope:sandbox` exists in this repo. " + humanDoneTail,
			state.Check{Kind: state.CheckLabel, Value: "scope:sandbox"}},
		{"issue-type", &planCheck{Kind: planCheckIssueType, IssueType: "Human task"},
			"**Done when:** This repo's owner, an organization, has the issue type `Human task`, enabled. " + humanDoneTail,
			state.Check{Kind: state.CheckIssueType, Value: "Human task"}},
		{"file", &planCheck{Kind: planCheckFile, Path: ".github/CODEOWNERS"},
			"**Done when:** The file `.github/CODEOWNERS` is on `main`. " + humanDoneTail,
			state.Check{Kind: state.CheckFile, Value: ".github/CODEOWNERS"}},
		{"file-contains", &planCheck{Kind: planCheckFile, Path: ".github/CODEOWNERS", Contains: "* @mertenvg"},
			"**Done when:** The file `.github/CODEOWNERS` on `main` contains `* @mertenvg`. " + humanDoneTail,
			state.Check{Kind: state.CheckFile, Value: ".github/CODEOWNERS", Contains: "* @mertenvg"}},
		{"ruleset", &planCheck{Kind: planCheckRuleset, Check: "issuedog/verify"},
			"**Done when:** An active ruleset on `main` requires the check `issuedog/verify`. " + humanDoneTail,
			state.Check{Kind: state.CheckRuleset, Value: "issuedog/verify"}},
		{"installation", &planCheck{Kind: planCheckInstallation, Owner: "acme"},
			"**Done when:** The App is installed on `acme`, and not suspended. " + humanDoneTail,
			state.Check{Kind: state.CheckInstallation, Value: "acme"}},
		{"plain-text", nil,
			"**Done when:** Production's settings on `main` set `QUOTE_TTL=24h`.",
			state.Check{}},
	} {
		items := []planItem{humanItem(t, tc.check)}
		why, numbers := "#124", map[string]int(nil)
		if tc.check != nil {
			dep := childItem(t, "story-plan", "T2")
			dep.DependsOn = []string{"T3"}
			items = append([]planItem{dep}, items...)
			why, numbers = "#126", map[string]int{"T2": 126}
		}
		specs, err := childSpecs(childParent(124, "priority:p2"), 1, items, false, []string{"mertenvg"})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		c := specs[len(specs)-1]
		if c.Item != "T3" {
			t.Fatalf("%s: the human task is described as %s, not last", tc.name, c.Item)
		}
		body := childBody(t, c, numbers)
		childGolden(t, "human-"+tc.name, body)
		text, _ := childSplit(t, tc.name, body)
		// A value is in inline code, which links no mention.
		checkSafe(t, codeSpanRE.ReplaceAllString(text, "`x`"))
		lines := strings.Split(body, "\n")
		for _, w := range []string{
			"**Why it blocks:** " + why + " can't proceed until production has a quote lifetime, because the booking service won't start without one, and the deploy settings are a path only the owner can change.",
			tc.done,
		} {
			if !slices.Contains(lines, w) {
				t.Errorf("%s: no line %q in\n%s", tc.name, w, body)
			}
		}
		at := -1
		for _, f := range []string{"**Goal:**", "**Why it blocks:**", "**Steps:**", "**Links:**", "**Done when:**", "**Estimated time:**", "cc @mertenvg", "<!-- issuedog:origin"} {
			i := strings.Index(body, "\n"+f)
			if i <= at {
				t.Errorf("%s: %q at %d, not after %d", tc.name, f, i, at)
			}
			at = i
		}
		is := state.Derive(childAppBody(body), childSlug, func(int64) bool { return false })
		if !is.Origin.Human || is.Origin.Check != tc.want {
			t.Errorf("%s: origin %+v, want human with check %+v", tc.name, is.Origin, tc.want)
		}
	}
}

// "Why it blocks" names the issues of the items that depend on the task,
// in number order, or the parent when none does, then the planner's
// reason, escaped, reading on after "until".
func TestHumanWhyBlocks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, reason string
		blocks       []int
		want         string
	}{
		{"none, so the parent", "production has a lifetime.", nil, "#5 can't proceed until production has a lifetime."},
		{"one", "production has a lifetime.", []int{12}, "#12 can't proceed until production has a lifetime."},
		{"two, in number order", "production has a lifetime.", []int{13, 12}, "#12 and #13 can't proceed until production has a lifetime."},
		{"three", "production has a lifetime.", []int{14, 12, 13}, "#12, #13 and #14 can't proceed until production has a lifetime."},
		{"a capital first word", "The service has a lifetime.", []int{12}, "#12 can't proceed until the service has a lifetime."},
		{"a name first", "GitHub has the App.", []int{12}, "#12 can't proceed until GitHub has the App."},
		{"a mention and a marker", "<!-- x --> @someone acts", []int{12}, "#12 can't proceed until &lt;!-- x --> @​someone acts"},
		{"a heading", "# Heading", []int{12}, "#12 can't proceed until # Heading"},
		{"two lines", "production\nhas one", []int{12}, "#12 can't proceed until production has one"},
		{"empty", " ", []int{12}, "None."},
	} {
		if got := humanWhyBlocks(tc.reason, tc.blocks, 5); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	blocks := []int{13, 12}
	humanWhyBlocks("x", blocks, 5)
	if !slices.Equal(blocks, []int{13, 12}) {
		t.Errorf("humanWhyBlocks sorted its argument: %v", blocks)
	}
}

// "Done when" for each kind of check is the daemon's sentence, then when
// it's checked; for plain text, the planner's text, escaped. A value that
// could make a marker, a mention or a code span's end stays inert, inside
// inline code. A kind it doesn't know is an error.
func TestHumanDoneWhen(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, text string
		check      state.Check
		want       string
	}{
		{"plain text", "Production has a `QUOTE_TTL`.", state.Check{}, "Production has a `QUOTE_TTL`."},
		{"plain text, escaped", "<!-- x --> @someone", state.Check{}, "&lt;!-- x --> @​someone"},
		{"plain text, empty", "", state.Check{}, "None."},
		{"label", "ignored", state.Check{Kind: state.CheckLabel, Value: "scope:sandbox"},
			"The label `scope:sandbox` exists in this repo. " + humanDoneTail},
		{"label, a marker", "", state.Check{Kind: state.CheckLabel, Value: "<!-- l -->"},
			"The label `&lt;!-- l -->` exists in this repo. " + humanDoneTail},
		{"issue-type", "", state.Check{Kind: state.CheckIssueType, Value: "Bug"},
			"This repo's owner, an organization, has the issue type `Bug`, enabled. " + humanDoneTail},
		{"file", "", state.Check{Kind: state.CheckFile, Value: "a b/c.md"},
			"The file `a b/c.md` is on `main`. " + humanDoneTail},
		{"file, contains", "", state.Check{Kind: state.CheckFile, Value: ".github/CODEOWNERS", Contains: "@mertenvg"},
			"The file `.github/CODEOWNERS` on `main` contains `@mertenvg`. " + humanDoneTail},
		{"file, contains a backtick", "", state.Check{Kind: state.CheckFile, Value: "x.go", Contains: "a `b` c"},
			"The file `x.go` on `main` contains ``a `b` c``. " + humanDoneTail},
		{"ruleset", "", state.Check{Kind: state.CheckRuleset, Value: "issuedog/verify"},
			"An active ruleset on `main` requires the check `issuedog/verify`. " + humanDoneTail},
		{"installation", "", state.Check{Kind: state.CheckInstallation, Value: "acme"},
			"The App is installed on `acme`, and not suspended. " + humanDoneTail},
	} {
		got, err := humanDoneWhen(tc.text, tc.check)
		if err != nil || got != tc.want {
			t.Errorf("%s: %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
	if got, err := humanDoneWhen("x", state.Check{Kind: "branch", Value: "main"}); err == nil || !strings.Contains(err.Error(), `"branch"`) {
		t.Errorf("an unknown kind: %q, %v; want an error naming it", got, err)
	}
	it := humanItem(t, nil)
	if _, err := humanBody(it, state.Check{Kind: "branch", Value: "main"}, nil, 5, "p2", nil); err == nil || !strings.Contains(err.Error(), "done when") {
		t.Errorf("humanBody with an unknown kind: %v; want an error naming its done when", err)
	}
	it.HumanTask = nil
	if _, err := humanBody(it, state.Check{}, nil, 5, "p2", nil); err == nil {
		t.Error("humanBody with no human task body: no error")
	}
}

// Children are created in plan order, then the human tasks, each after
// every human task that depends on it, otherwise in plan order. A cycle
// leaves the human tasks on it in plan order.
func TestChildOrder(t *testing.T) {
	t.Parallel()
	task := func(id string, deps ...string) planItem { return planItem{ID: id, DependsOn: deps} }
	human := func(id string, deps ...string) planItem { return planItem{ID: id, DependsOn: deps, Human: true} }
	for _, tc := range []struct {
		name  string
		items []planItem
		want  []string
	}{
		{"no human task", []planItem{task("T1"), task("T2", "T1")}, []string{"T1", "T2"}},
		{"human tasks last, in plan order", []planItem{human("T1"), task("T2", "T1"), human("T3"), task("T4")}, []string{"T2", "T4", "T1", "T3"}},
		{"a human task after the one depending on it", []planItem{task("T1"), human("T2"), human("T3", "T2"), task("T4", "T3")}, []string{"T1", "T4", "T3", "T2"}},
		{"a chain", []planItem{human("T1"), human("T2", "T1"), human("T3", "T2")}, []string{"T3", "T2", "T1"}},
		{"two depending on one", []planItem{human("T1"), human("T2", "T1"), human("T3", "T1"), human("T4")}, []string{"T2", "T3", "T1", "T4"}},
		{"a cycle", []planItem{task("T1"), human("T2", "T3"), human("T3", "T2"), human("T4")}, []string{"T1", "T4", "T2", "T3"}},
	} {
		var got []string
		for _, it := range childOrder(tc.items) {
			got = append(got, it.ID)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A human task's body names each item depending on it, which bodyOf needs
// a number for: one with none is an error naming it. Two human tasks, one
// depending on the other, each name the right issue.
func TestChildBodyOfBlocks(t *testing.T) {
	t.Parallel()
	first := humanItem(t, nil)
	first.ID = "T2"
	second := humanItem(t, nil)
	second.ID, second.DependsOn = "T3", []string{"T2"}
	dep := childItem(t, "story-plan", "T1")
	dep.ID, dep.DependsOn = "T4", []string{"T3"}
	specs, err := childSpecs(childParent(10), 1, []planItem{first, second, dep}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, c := range specs {
		order = append(order, c.Item+":"+strings.Join(c.Blocks, ","))
	}
	if want := []string{"T4:", "T3:T4", "T2:T3"}; !slices.Equal(order, want) {
		t.Fatalf("specs %q, want %q", order, want)
	}
	numbers := map[string]int{"T4": 11}
	if _, err := specs[2].bodyOf(numbers); err == nil || !strings.Contains(err.Error(), "T3") {
		t.Errorf("T2's body with no issue for T3: %v; want an error naming T3", err)
	}
	for i, want := range []string{"#11 can't proceed until", "#12 can't proceed until"} {
		c := specs[i+1]
		body := childBody(t, c, numbers)
		if !strings.Contains(body, "\n**Why it blocks:** "+want+" ") {
			t.Errorf("%s: no %q in\n%s", c.Item, want, body)
		}
		numbers[c.Item] = 12 + i
	}
	if body := childBody(t, specs[0], nil); strings.Contains(body, "Why it blocks") {
		t.Errorf("T4, a task, has a Why it blocks:\n%s", body)
	}
}

// A human task's body is checked for size with the widest number for each
// issue it blocks: one that fits only with narrower numbers is an error
// naming it, before anything is filed, and one at the limit isn't.
func TestChildHumanSize(t *testing.T) {
	t.Parallel()
	it := humanItem(t, nil)
	dep := childItem(t, "story-plan", "T2")
	dep.DependsOn = []string{"T3"}
	describe := func() ([]childSpec, error) {
		return childSpecs(childParent(5), 1, []planItem{dep, it}, false, nil)
	}
	specs, err := describe()
	if err != nil {
		t.Fatal(err)
	}
	widest := strconv.Itoa(childWidestNumber)
	small, err := specs[1].human([]int{childWidestNumber})
	if err != nil {
		t.Fatal(err)
	}
	it.HumanTask.EstimatedTime += strings.Repeat("é", childMaxBody-textLen(small)) // one UTF-16 unit each
	specs, err = describe()
	if err != nil {
		t.Fatalf("a body at the limit with the widest number: %v", err)
	}
	at, err := specs[1].human([]int{childWidestNumber})
	if err != nil || textLen(at) != childMaxBody || !strings.Contains(at, "#"+widest+" can't") {
		t.Fatalf("the body at the limit is %d, %v; want %d, naming #%s", textLen(at), err, childMaxBody, widest)
	}
	it.HumanTask.EstimatedTime += "é"
	specs, err = describe()
	if err == nil || !strings.Contains(err.Error(), "item T3") || !strings.Contains(err.Error(), "over 64512") || specs != nil {
		t.Errorf("childSpecs = %d specs, %v; want an error naming T3, though #2 would fit", len(specs), err)
	}
}
