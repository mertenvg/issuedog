package daemon

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// plannerOutput decodes internal/agent's golden planner output name.
func plannerOutput(t *testing.T, name string) planOutput {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "agent", "testdata", "outputs", "planner", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := decodePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// goldenPlan is internal/agent's golden planner output name's plan.
func goldenPlan(t *testing.T, name string) planBody {
	t.Helper()
	out := plannerOutput(t, name)
	if out.Outcome != outcomePlan || out.Plan == nil {
		t.Fatalf("%s is a %q output, not a plan", name, out.Outcome)
	}
	return *out.Plan
}

// planGolden compares got with testdata/plan/<name>.md, or writes it there
// with -update.
func planGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "plan", name+".md")
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

// epicView is the view the golden epic plan is rendered with: an epic at
// p1, with no sub-issues yet.
func epicView() planView {
	return planView{Issue: 123, Rev: 1, Priority: "p1", Notify: []string{"mertenvg"}, MaxTasks: 10}
}

// storyView is the view the golden story plan is rendered with: a story at
// p2, after the golden question round, which blocks #124.
func storyView() planView {
	return planView{Issue: 124, Rev: 1, Priority: "p2", Notify: []string{"mertenvg"}, MaxTasks: 10}
}

// filePathRE is a word with a directory and a file extension, as SPEC
// §11.3's file-paths rule defines a file path.
var filePathRE = regexp.MustCompile(`[\w.-]+/[\w./-]*\.[A-Za-z0-9]+\b`)

// The goldens: the epic plan, and the story plan with its human task,
// reviewed and not. Each is safe to post, with no # or ## heading and no
// file path, and its plan text is everything the item fields give.
func TestRenderPlanGolden(t *testing.T) {
	t.Parallel()
	noReview := storyView()
	noReview.NoReview = true
	for _, tc := range []struct {
		name, plan string
		view       planView
		want       []string // lines the comment has
	}{
		{"epic", "epic-plan", epicView(), []string{
			"### Plan for #123 (rev 1)",
			"**S1. Quotes expire after 24 hours** · p1 · model:opus · effort:high",
			"**S2. Customers see when a quote expires** · p1",
			"**S3. Expired quotes in the monthly report** · p2 · effort:medium",
			"Depends on: nothing", "Depends on: S1",
			"**Assumptions to confirm with your 👍:**",
			"**Order:** S1, then S2 and S3 in parallel",
			"Validation (11.3): 3 stories, with no tasks; no dependency cycles; 3 sub-issues in all, within 100; at most 2 dependency links of one type per issue, within 50.",
			"cc @mertenvg", planApproveLine,
		}},
		{"story", "story-plan", storyView(), []string{
			"### Plan for #124 (rev 1)",
			"**T1. Record when each quote expires** · scope:booking · p2 · model:sonnet · effort:medium",
			"**T2. Refuse an expired quote at booking** · scope:booking · p1",
			"**T3. Set the quote lifetime in production** · scope:infra · p2 · human",
			"Done when: Production's settings on `main` set `QUOTE_TTL=24h`. (checked automatically: file)",
			"Depends on: T1, T3",
			"Validation (11.3): 3 tasks, within `plan.max_tasks` of 10, each with one scope from the list; no dependency cycles; 3 sub-issues in all, within 100; at most 2 dependency links of one type per issue, within 50.",
			"cc @mertenvg", planApproveLine,
		}},
		{"story-no-review", "story-plan", noReview, []string{
			"**Assumptions:**",
			"This plan skips review, as `plan.review` is false: #124 moves straight to `planned`.",
		}},
	} {
		got := renderPlan(goldenPlan(t, tc.plan), tc.view)
		planGolden(t, tc.name, got)
		checkSafe(t, got)
		if h := headings(got); len(h) > 0 {
			t.Errorf("%s: headings %q", tc.name, h)
		}
		if p := filePathRE.FindAllString(got, -1); len(p) > 0 {
			t.Errorf("%s: file paths %q", tc.name, p)
		}
		lines := strings.Split(got, "\n")
		for _, w := range tc.want {
			if !slices.Contains(lines, w) {
				t.Errorf("%s: no line %q in\n%s", tc.name, w, got)
			}
		}
		if strings.Contains(got, "<!--") {
			t.Errorf("%s: a marker in the plan text", tc.name)
		}
		for _, it := range goldenPlan(t, tc.plan).Items {
			for _, s := range append([]string{it.Title, it.Outcome}, it.Criteria...) {
				if !strings.Contains(got, s) {
					t.Errorf("%s: %s's %q isn't shown", tc.name, it.ID, s)
				}
			}
			if it.Human != strings.Contains(got, "**"+it.ID+". "+it.Title+"** · scope:"+it.Scope+" · p2 · human") {
				t.Errorf("%s: %s is human %v, and its line says otherwise", tc.name, it.ID, it.Human)
			}
		}
	}
	if got := renderPlan(goldenPlan(t, "story-plan"), noReview); strings.Contains(got, "cc @") || strings.Contains(got, "To approve") {
		t.Errorf("a plan that skips review asks for review:\n%s", got)
	}
}

// No field of a task's body is shown, nor any of a human task's but its
// "Done when" text: not its goal, steps, links or estimate, and not its
// check's path.
func TestRenderPlanHidesTaskBodies(t *testing.T) {
	t.Parallel()
	p := goldenPlan(t, "story-plan")
	got := renderPlan(p, storyView())
	var hidden []string
	for _, it := range p.Items {
		if b := it.Task; b != nil {
			hidden = append(append(hidden, b.Context, b.Change, b.TestPlan), b.DefinitionOfDone...)
		}
		if h := it.HumanTask; h != nil {
			hidden = append(append(append(hidden, h.Goal, h.WhyBlocks, h.EstimatedTime), h.Steps...), h.Links...)
			if c := h.DoneWhen.Check; c != nil {
				hidden = append(hidden, c.Path)
			}
		}
	}
	for _, s := range hidden {
		if strings.Contains(got, s) {
			t.Errorf("the comment shows %q", s)
		}
	}
	if strings.Contains(got, "deploy/production.env") || strings.Contains(got, "production.env") {
		t.Errorf("the comment shows the file check's path:\n%s", got)
	}
}

// A human task's check is shown by its kind only, whatever the kind: never
// its label, issue type, path, contents, check name or owner.
func TestRenderPlanCheckKindOnly(t *testing.T) {
	t.Parallel()
	checks := []planCheck{
		{Kind: planCheckLabel, Label: "secret-label"},
		{Kind: planCheckIssueType, IssueType: "SecretType"},
		{Kind: planCheckFile, Path: "secret/dir/file.go", Contains: "SECRET_TEXT"},
		{Kind: planCheckRuleset, Check: "secret-check"},
		{Kind: planCheckInstallation, Owner: "secret-owner"},
	}
	for _, c := range checks {
		p := planBody{Kind: planTasks, Why: "Why.", Summary: "Summary.", Order: "T1", Items: []planItem{{
			ID: "T1", Title: "Do it", Outcome: "Done.", Criteria: []string{"It's done."}, Scope: "infra", Human: true,
			HumanTask: &planHumanTask{Goal: "Goal.", WhyBlocks: "Nothing.", Steps: []string{"Step."}, Links: []string{"https://example.com"},
				EstimatedTime: "5 min", DoneWhen: planDoneWhen{Text: "It's there.", Check: &c}},
		}}}
		got := renderPlan(p, storyView())
		if want := "Done when: It's there. (checked automatically: " + c.Kind + ")"; !strings.Contains(got, want+"\n") {
			t.Errorf("%s: no line %q in\n%s", c.Kind, want, got)
		}
		for _, v := range []string{c.Label, c.IssueType, c.Path, c.Contains, c.Check, c.Owner} {
			if v != "" && strings.Contains(got, v) {
				t.Errorf("%s: the comment shows %q:\n%s", c.Kind, v, got)
			}
		}
	}
	p := goldenPlan(t, "story-plan")
	p.Items[2].HumanTask.DoneWhen.Check = nil
	if got := renderPlan(p, storyView()); !strings.Contains(got, "\nDone when: Production's settings on `main` set `QUOTE_TTL=24h`.\nDepends on: nothing") {
		t.Errorf("a plain-text Done when isn't shown as it is:\n%s", got)
	}
}

// An epic's stories show no scope and no "human", and no "Done when", even
// with those fields set: only a task has them.
func TestRenderPlanStoriesNoTaskFields(t *testing.T) {
	t.Parallel()
	p := goldenPlan(t, "epic-plan")
	p.Items[0].Scope, p.Items[0].Human = "booking", true
	p.Items[0].HumanTask = &planHumanTask{DoneWhen: planDoneWhen{Text: "Shown?"}}
	got := renderPlan(p, epicView())
	for _, s := range []string{"scope:", "human", "Done when", "Shown?"} {
		if strings.Contains(got, s) {
			t.Errorf("an epic's plan shows %q:\n%s", s, got)
		}
	}
	// With no default priority, an item without one shows none.
	v := epicView()
	v.Priority = ""
	if got := renderPlan(goldenPlan(t, "epic-plan"), v); !strings.Contains(got, "\n**S2. Customers see when a quote expires**\n") {
		t.Errorf("S2 shows a priority it doesn't have:\n%s", got)
	}
}

// What changed since the last revision is shown from rev 2, before the
// message. At rev 1, the message is shown alone, and with no message
// there's neither.
func TestRenderPlanChanges(t *testing.T) {
	t.Parallel()
	p := goldenPlan(t, "story-plan")
	v := storyView()
	v.Rev = 3
	got := renderPlan(p, v)
	if want := "### Plan for #124 (rev 3)"; !strings.HasPrefix(got, want+"\n\n") {
		t.Errorf("rev 3 starts %q, want %q", got[:40], want)
	}
	if want := "**Changes since rev 2.**\n\nYour answers are used: "; !strings.Contains(got, want) {
		t.Errorf("rev 3 has no %q:\n%s", want, got)
	}
	if i, j := strings.Index(got, "This plan adds:"), strings.Index(got, "**Changes since"); i < 0 || j < i {
		t.Errorf("the changes don't follow the summary:\n%s", got)
	}
	if i, j := strings.Index(got, "**Changes since"), strings.Index(got, "**Assumptions"); j < i {
		t.Errorf("the assumptions don't follow the changes:\n%s", got)
	}
	if got := renderPlan(p, storyView()); strings.Contains(got, "Changes since") || !strings.Contains(got, "\n\nYour answers are used: ") {
		t.Errorf("rev 1's message:\n%s", got)
	}
	p.Message = " \n"
	if got := renderPlan(p, v); strings.Contains(got, "Changes since") {
		t.Errorf("rev 3 with no message:\n%s", got)
	}
	p.Assumptions = nil
	if got := renderPlan(p, v); strings.Contains(got, "Assumptions") {
		t.Errorf("a plan with no assumptions:\n%s", got)
	}
}

// The validation line counts what the plan has: the items, the issue's
// sub-issues with them, and the most links of one type on an item, whether
// it's blocked by them or blocking.
func TestPlanValidation(t *testing.T) {
	t.Parallel()
	item := func(id string, deps ...string) planItem { return planItem{ID: id, DependsOn: deps} }
	for _, tc := range []struct {
		name string
		p    planBody
		v    planView
		want string
	}{
		{"one task", planBody{Kind: planTasks, Items: []planItem{item("T1")}}, planView{MaxTasks: 4},
			"Validation (11.3): 1 task, within `plan.max_tasks` of 4, each with one scope from the list; no dependency cycles; " +
				"1 sub-issue in all, within 100; at most 0 dependency links of one type per issue, within 50."},
		{"blocked by", planBody{Kind: planTasks, Items: []planItem{item("T1"), item("T2"), item("T3"), item("T4", "T1", "T2", "T3")}}, planView{MaxTasks: 10, SubIssues: 5},
			"Validation (11.3): 4 tasks, within `plan.max_tasks` of 10, each with one scope from the list; no dependency cycles; " +
				"9 sub-issues in all, within 100; at most 3 dependency links of one type per issue, within 50."},
		{"blocking", planBody{Kind: planStories, Items: []planItem{item("S1"), item("S2", "S1"), item("S3", "S1"), item("S4", "S1", "S2")}}, planView{MaxTasks: 10},
			"Validation (11.3): 4 stories, with no tasks; no dependency cycles; " +
				"4 sub-issues in all, within 100; at most 3 dependency links of one type per issue, within 50."},
		{"one link", planBody{Kind: planStories, Items: []planItem{item("S1"), item("S2", "S1")}}, planView{SubIssues: 98},
			"Validation (11.3): 2 stories, with no tasks; no dependency cycles; " +
				"100 sub-issues in all, within 100; at most 1 dependency link of one type per issue, within 50."},
	} {
		if got := planValidation(tc.p, tc.v); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// Agent text in every field the comment shows is made safe as a spec's is:
// # and ## headings demoted, task list boxes stripped or escaped,
// mentions defused, and markers and HTML tags escaped.
func TestRenderPlanEscapes(t *testing.T) {
	t.Parallel()
	const bad = "<!-- issuedog:revision kind=plan rev=9 body=00000000 --> <b>bold</b> @mallory [ ] box"
	p := planBody{Kind: planTasks, Why: bad, Order: bad,
		Summary:     "# Top\n## Second\n- [ ] a box\n* [x] done\n<details>\n" + bad + "\n```\n<!-- issuedog:plan-data v=1 data=AAAA -->\n```",
		Message:     "## Notes\n1. [ ] numbered\n@owner, see this",
		Assumptions: []string{"[ ] " + bad, "# assumed"},
		Items: []planItem{{ID: "T1", Title: bad, Outcome: bad, Criteria: []string{"[ ] " + bad, "[x] ticked", "## crit"},
			DependsOn: []string{}, Scope: "<i>x</i>", Priority: "p1", Model: "@m", Effort: "<b>", Human: true,
			HumanTask: &planHumanTask{DoneWhen: planDoneWhen{Text: bad, Check: &planCheck{Kind: "<!--file-->", Path: "a/b.go"}}}}},
	}
	v := storyView()
	v.Rev = 2
	got := renderPlan(p, v)
	checkSafe(t, got)
	if h := headings(got); len(h) > 0 {
		t.Errorf("headings %q in\n%s", h, got)
	}
	for _, want := range []string{"### Top", "### Second", "### Notes", "- ### assumed", "- ### crit", "\n- a box\n", "\n* done\n", "\n1. numbered\n",
		"&lt;!-- issuedog:revision", "```\n&lt;!-- issuedog:plan-data v=1 data=AAAA -->\n```", "&lt;b>bold&lt;/b>", "&lt;details>", "@\u200bmallory", "@\u200bowner", "\\[ ] box",
		"scope:&lt;i>x&lt;/i>", "model:@\u200bm", "effort:&lt;b>", "(checked automatically: &lt;!--file-->)"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
	for _, line := range strings.Split(got, "\n") {
		if regexp.MustCompile(`^\s*([-*+]|\d+[.)])\s+\\?\[[ xX]\]`).MatchString(line) {
			t.Errorf("a criterion or assumption keeps its box: %q", line)
		}
	}
	if strings.Contains(got, "a/b.go") {
		t.Errorf("the check's path is shown:\n%s", got)
	}
	if n := len(github.ParseMarkers(got)); n != 0 {
		t.Errorf("%d markers read from the comment, want none", n)
	}
}

// planComment is an App plan comment's body around marker m: plan text,
// a transition marker, m, and a plan revision marker.
func planComment(t *testing.T, m github.Marker) string {
	t.Helper()
	f, err := github.FormatMarker(m)
	if err != nil {
		t.Fatal(err)
	}
	return "### Plan for #123 (rev 1)\n\nText.\n\n<!-- issuedog:transition from=planning to=plan-review rev=1 run=p1 -->\n" + f +
		"\n<!-- issuedog:revision kind=plan rev=1 body=00000000 -->"
}

var appAuthor = github.Author{Login: "issuedog[bot]", Type: "Bot"}

// readPlanData gives back exactly the plan planDataMarker was given: each
// golden plan, and plans whose lists are empty or missing, with text that
// looks like markers.
func TestPlanDataRoundTrip(t *testing.T) {
	t.Parallel()
	plans := map[string]planBody{"epic-plan": goldenPlan(t, "epic-plan"), "story-plan": goldenPlan(t, "story-plan")}
	edge := goldenPlan(t, "story-plan")
	edge.Assumptions = []string{}
	edge.Items[0].DependsOn = nil
	edge.Items[1].Criteria = []string{}
	edge.Summary = "<!-- issuedog:plan-data v=1 data=AAAA --> --> é ✓ \u200b\r\n"
	plans["empty lists"] = edge
	bare := goldenPlan(t, "epic-plan")
	bare.Assumptions, bare.Items = nil, nil
	plans["no lists"] = bare
	for name, p := range plans {
		m, err := planDataMarker(p)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if m.Kind != "plan-data" || len(m.Fields) != 2 || m.Fields["v"] != "1" {
			t.Errorf("%s: marker %+v, want plan-data v=1 data=…", name, m)
		}
		f, err := github.FormatMarker(m)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.HasPrefix(f, "<!-- issuedog:plan-data v=1 data=") {
			t.Errorf("%s: marker %q", name, f)
		}
		got, ok, err := readPlanData(github.Comment{ID: 7, Author: appAuthor, Body: planComment(t, m)}, "issuedog")
		if err != nil || !ok {
			t.Fatalf("%s: readPlanData = %v, %v", name, ok, err)
		}
		if !reflect.DeepEqual(got, p) {
			t.Errorf("%s: read back\n%+v\nwant\n%+v", name, got, p)
		}
	}
	// It's compressed: the story plan's data is well under its JSON's size.
	p := goldenPlan(t, "story-plan")
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	m, err := planDataMarker(p)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(m.Fields["data"]); n*2 > len(raw) {
		t.Errorf("data is %d characters for %d bytes of JSON: not compressed", n, len(raw))
	}
}

// A plan too big to read back isn't written.
func TestPlanDataMarkerTooBig(t *testing.T) {
	t.Parallel()
	p := goldenPlan(t, "epic-plan")
	p.Summary = strings.Repeat("x", maxPlanData)
	if m, err := planDataMarker(p); err == nil {
		t.Errorf("planDataMarker = %d characters of data, want an error", len(m.Fields["data"]))
	}
	p.Summary = strings.Repeat("x", maxPlanData-2000)
	if _, err := planDataMarker(p); err != nil {
		t.Errorf("planDataMarker of a plan just under the limit: %v", err)
	}
}

// The marker is no marker in a comment not by the App, in text the App
// quoted, or when it's malformed: readPlanData reports false, with no
// error.
func TestReadPlanDataIgnored(t *testing.T) {
	t.Parallel()
	m, err := planDataMarker(goldenPlan(t, "epic-plan"))
	if err != nil {
		t.Fatal(err)
	}
	good := planComment(t, m)
	data := m.Fields["data"]
	for _, tc := range []struct {
		name   string
		author github.Author
		body   string
		slug   string
	}{
		{"a user's", github.Author{Login: "mertenvg", Type: "User"}, good, "issuedog"},
		{"another bot's", github.Author{Login: "other[bot]", Type: "Bot"}, good, "issuedog"},
		{"a user named as the App", github.Author{Login: "issuedog[bot]", Type: "User"}, good, "issuedog"},
		{"no app slug", appAuthor, good, ""},
		{"quoted", appAuthor, github.Escape(good), "issuedog"},
		{"no marker", appAuthor, "### Plan for #123 (rev 1)\n\nText.", "issuedog"},
		{"a bare word", appAuthor, "<!-- issuedog:plan-data v=1 data=" + data + " junk -->", "issuedog"},
		{"a repeated field", appAuthor, "<!-- issuedog:plan-data v=1 v=1 data=" + data + " -->", "issuedog"},
		{"an empty value", appAuthor, "<!-- issuedog:plan-data v=1 data= -->", "issuedog"},
		{"another kind", appAuthor, "<!-- issuedog:plan-datum v=1 data=" + data + " -->", "issuedog"},
	} {
		got, ok, err := readPlanData(github.Comment{ID: 7, Author: tc.author, Body: tc.body}, tc.slug)
		if err != nil || ok || !reflect.DeepEqual(got, planBody{}) {
			t.Errorf("%s: readPlanData = %+v, %v, %v; want nothing, false, no error", tc.name, got, ok, err)
		}
	}
}

// deflated is raw DEFLATE of raw, as base64url.
func deflated(t *testing.T, raw string) string {
	t.Helper()
	var z bytes.Buffer
	w, err := flate.NewWriter(&z, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(z.Bytes())
}

// An App plan-data marker that doesn't read back is an error, and gives
// no plan: corrupted data, another version, or more than one marker.
func TestReadPlanDataCorrupted(t *testing.T) {
	t.Parallel()
	m, err := planDataMarker(goldenPlan(t, "story-plan"))
	if err != nil {
		t.Fatal(err)
	}
	data := m.Fields["data"]
	flipped := []byte(data)
	if flipped[len(flipped)/2] == 'A' {
		flipped[len(flipped)/2] = 'B'
	} else {
		flipped[len(flipped)/2] = 'A'
	}
	marker := func(fields string) string { return "Plan.\n<!-- issuedog:plan-data " + fields + " -->" }
	for _, tc := range []struct{ name, body string }{
		{"version 2", marker("v=2 data=" + data)},
		{"no version", marker("data=" + data)},
		{"no data", marker("v=1")},
		{"not base64url", marker("v=1 data=" + data[:20] + "+/" + data[22:])},
		{"padded", marker("v=1 data=" + data + "==")},
		{"truncated", marker("v=1 data=" + data[:len(data)-8])},
		{"a character changed", marker("v=1 data=" + string(flipped))},
		{"not DEFLATE", marker("v=1 data=" + base64.RawURLEncoding.EncodeToString([]byte(`{"kind":"tasks"}`)))},
		{"not JSON", marker("v=1 data=" + deflated(t, "plan"))},
		{"an unknown field", marker("v=1 data=" + deflated(t, `{"kind":"tasks","extra":1}`))},
		{"a wrong type", marker("v=1 data=" + deflated(t, `{"kind":1}`))},
		{"more after the plan", marker("v=1 data=" + deflated(t, `{"kind":"tasks"} {}`))},
		{"too big", marker("v=1 data=" + deflated(t, `{"kind":"tasks"}`+strings.Repeat(" ", maxPlanData)))},
		{"two markers", planComment(t, m) + "\n" + marker("v=1 data="+data)},
	} {
		got, ok, err := readPlanData(github.Comment{ID: 7, Author: appAuthor, Body: tc.body}, "issuedog")
		if err == nil || ok || !reflect.DeepEqual(got, planBody{}) {
			t.Errorf("%s: readPlanData = %+v, %v, %v; want nothing, false, an error", tc.name, got, ok, err)
		} else if !strings.Contains(err.Error(), "comment 7") {
			t.Errorf("%s: error %q doesn't name the comment", tc.name, err)
		}
	}
	// Just under the limit is read.
	body := marker("v=1 data=" + deflated(t, `{"kind":"tasks"}`+strings.Repeat(" ", maxPlanData-len(`{"kind":"tasks"}`))))
	if got, ok, err := readPlanData(github.Comment{ID: 7, Author: appAuthor, Body: body}, "issuedog"); err != nil || !ok || got.Kind != planTasks {
		t.Errorf("a plan of exactly the limit: readPlanData = %+v, %v, %v", got, ok, err)
	}
}

// planningEpicFake is an epic at planning, after spec revision 1's
// approval.
func planningEpicFake(t testing.TB) *githubtest.Fake {
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body", "stage:planning")
	f.SetType(n, "epic")
	f.AddComment(n, "issuedog[bot]", "<!-- issuedog:transition from=clarifying to=spec-review rev=1 body=aaaaaaaa run=a -->\n<!-- issuedog:revision rev=1 body=aaaaaaaa -->")
	f.AddComment(n, "issuedog[bot]", "<!-- issuedog:transition from=spec-review to=planning rev=1 body=aaaaaaaa cmd=r5 seen=1002 -->")
	return f
}

// The plan comment, as the planner stage will post it with Move.Markers
// and Move.PlanRev: the rendered plan, the transition marker, the
// plan-data marker, and last the plan revision marker, whose hash covers
// the data. Read back from the fake, the revision matches, the data reads
// back, and a comment with other data wouldn't match the hash.
func TestPlanCommentCarriesData(t *testing.T) {
	t.Parallel()
	p := goldenPlan(t, "epic-plan")
	m, err := planDataMarker(p)
	if err != nil {
		t.Fatal(err)
	}
	clean := func(int64) bool { return false }
	mv := Move{To: state.PlanReview, Trigger: state.TriggerPlanValid, Key: state.Key{Field: "run", ID: "p1"}, PlanRev: 1,
		Text: renderPlan(p, epicView()), Markers: []github.Marker{m}}
	read := func(f *githubtest.Fake) (*Applier, *github.Snapshot, error) {
		c := f.Client(t)
		s, err := c.Snapshot(context.Background(), "o", "r", 1)
		return &Applier{Client: c, Owner: "o", Repo: "r", AppSlug: "issuedog"}, s, err
	}
	pass := func(f *githubtest.Fake) error {
		a, s, err := read(f)
		if err != nil {
			return err
		}
		return a.Transition(context.Background(), s, clean, mv)
	}
	githubtest.Replay(t, planningEpicFake, pass)

	f := planningEpicFake(t)
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	_, s, err := read(f)
	if err != nil {
		t.Fatal(err)
	}
	c := s.Comments[len(s.Comments)-1]
	lines := strings.Split(c.Body, "\n")
	if len(lines) < 4 {
		t.Fatalf("plan comment %q", c.Body)
	}
	f1, err := github.FormatMarker(m)
	if err != nil {
		t.Fatal(err)
	}
	if want := renderPlan(p, epicView()) + "\n\n<!-- issuedog:transition from=planning to=plan-review rev=1 run=p1 -->\n" + f1 + "\n"; !strings.HasPrefix(c.Body, want) {
		t.Errorf("plan comment\n%s\nwant it to start\n%s", c.Body, want)
	}
	if !strings.HasPrefix(lines[len(lines)-1], "<!-- issuedog:revision kind=plan rev=1 body=") || lines[len(lines)-2] != f1 {
		t.Errorf("the plan comment's last lines are %q, want the plan-data marker, then the plan revision", lines[len(lines)-2:])
	}
	rev, ok := state.LatestRevision(s, "issuedog", state.PlanRevision, clean)
	if !ok || rev.Rev != 1 || rev.Comment != c.ID || !rev.Matches(s) {
		t.Fatalf("latest plan revision = %+v, %v, matching %v; want rev 1 on the plan comment, matching", rev, ok, rev.Matches(s))
	}
	got, ok, err := readPlanData(c, "issuedog")
	if err != nil || !ok || !reflect.DeepEqual(got, p) {
		t.Errorf("readPlanData of the plan comment = %+v, %v, %v; want the plan", got, ok, err)
	}
	// The hash covers the data: another plan's data in its place doesn't
	// match the revision, though it reads back.
	other, err := planDataMarker(goldenPlan(t, "story-plan"))
	if err != nil {
		t.Fatal(err)
	}
	f2, err := github.FormatMarker(other)
	if err != nil {
		t.Fatal(err)
	}
	s.Comments[len(s.Comments)-1].Body = strings.Replace(c.Body, f1, f2, 1)
	if rev.Matches(s) {
		t.Error("the revision matches a plan comment with other data")
	}
	if got, ok, err := readPlanData(s.Comments[len(s.Comments)-1], "issuedog"); err != nil || !ok || got.Kind != planTasks {
		t.Errorf("readPlanData of the changed comment = %+v, %v, %v", got, ok, err)
	}
}

// decodePlan reads the golden question round, and refuses a field the
// types don't have, at any depth.
func TestDecodePlan(t *testing.T) {
	t.Parallel()
	out := plannerOutput(t, "questions")
	if out.Outcome != outcomeQuestions || out.Plan != nil || len(out.Questions) != 2 || out.Questions[0].Recommended.Text != "booking" {
		t.Errorf("questions = %+v", out)
	}
	for _, raw := range []string{
		`{"outcome":"plan","extra":1}`,
		`{"outcome":"plan","plan":{"kind":"tasks","extra":1}}`,
		`{"outcome":"plan","plan":{"items":[{"id":"T1","extra":1}]}}`,
		`{"outcome":"plan","plan":{"items":[{"id":"T1","human_task":{"done_when":{"check":{"kind":"file","extra":1}}}}]}}`,
		`{"outcome":`,
	} {
		if got, err := decodePlan(json.RawMessage(raw)); err == nil {
			t.Errorf("decodePlan(%s) = %+v, want an error", raw, got)
		}
	}
}
