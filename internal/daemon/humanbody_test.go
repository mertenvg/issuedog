package daemon_test

import (
	"bytes"
	"compress/flate"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// The human task's body (#284): human tasks are created after the plan's
// other children, dependents first, so each one's "Why it blocks" names
// the issues of the items that depend on it, and its check is on its
// origin marker.

// humanPlan is a story's plan of four tasks: T1, a task; T2, a human task
// with a ruleset check; T3, a human task with a file check that depends on
// T2; and T4, a task that depends on T3. So they're created as T1, T4, T3
// and T2: #2 to #5.
const humanPlan = `{
  "kind": "tasks",
  "why": "Two tasks, and two settings only the owner can change.",
  "summary": "Code owners review each PR, once a ruleset makes the verify check required.",
  "assumptions": [],
  "order": "T1 and T2 in parallel, then T3, then T4",
  "items": [
    {"id": "T1", "title": "Add the verify check", "outcome": "Each PR gets the verify check.",
     "criteria": ["Each PR gets the verify check."], "depends_on": [], "scope": "booking",
     "task": {"context": "c", "change": "c", "definition_of_done": ["d"], "test_plan": "t"}},
    {"id": "T2", "title": "Require the verify check", "outcome": "A ruleset on main requires the verify check.",
     "criteria": ["A ruleset on main requires the verify check."], "depends_on": [], "scope": "infra", "human": true,
     "human_task": {"goal": "Require the verify check on main.",
       "why_blocks": "The ruleset requires the verify check, so code owners review only PRs that pass it.",
       "steps": ["Open the repo's rulesets.", "Add a branch ruleset for main that requires issuedog/verify."],
       "links": ["https://github.com/o/r/settings/rules"], "estimated_time": "5 min",
       "done_when": {"text": "A ruleset on main requires the verify check.", "check": {"kind": "ruleset", "check": "issuedog/verify"}}}},
    {"id": "T3", "title": "Name the code owners", "outcome": "Each PR asks the code owners for a review.",
     "criteria": ["Each PR asks the code owners for a review."], "depends_on": ["T2"], "scope": "infra", "human": true,
     "human_task": {"goal": "Name the owner as the code owner of everything.",
       "why_blocks": "code owners are named, since the review rule asks them.",
       "steps": ["Add the line * @owner to .github/CODEOWNERS on main."],
       "links": ["https://github.com/o/r/blob/main/.github/CODEOWNERS"], "estimated_time": "2 min",
       "done_when": {"text": "The code owners file names the owner.", "check": {"kind": "file", "path": ".github/CODEOWNERS", "contains": "* @owner"}}}},
    {"id": "T4", "title": "Ask the code owners in the PR template", "outcome": "The PR template asks for the code owners' review.",
     "criteria": ["The PR template asks for the code owners' review."], "depends_on": ["T3"], "scope": "booking",
     "task": {"context": "c", "change": "c", "definition_of_done": ["d"], "test_plan": "t"}}
  ]
}`

// humanKind is a story whose plan is humanPlan, on a repo that uses type:*
// labels.
var humanKind = childPlan{typ: "Story"}

// humanPlanData is humanPlan's plan-data marker, as the planner's run
// writes it: its JSON, deflated, in base64url.
func humanPlanData(t testing.TB) github.Marker {
	t.Helper()
	var z bytes.Buffer
	w, err := flate.NewWriter(&z, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(humanPlan)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return github.Marker{Kind: "plan-data", Fields: map[string]string{"v": "1", "data": base64.RawURLEncoding.EncodeToString(z.Bytes())}}
}

// humanAtReview is issue 1, a story, at plan-review with humanPlan in
// comment 1002, and the owner's 👍 on it.
var humanAtReview = atPlanning(humanKind, func(t testing.TB, f *githubtest.Fake) {
	moveOn(t, f, 1, daemon.Move{To: state.PlanReview, Trigger: state.TriggerPlanValid, Key: state.Key{Field: "run", ID: "p1"}, PlanRev: 1,
		Text: planText(1, 1), Markers: []github.Marker{humanPlanData(t)}}, 1002)
	thumbsUp(t, f)
})

// The approval creates the tasks, then the human tasks, dependents first:
// T1 #2, T4 #3, T3 #4 and T2 #5, linked in that order, each with its first
// move. T3's "Why it blocks" names #3, T4's issue, and T2's names #4,
// T3's; each one's "Done when" is the daemon's sentence for its check,
// which its origin marker records and reads back. A crash at any write
// replays to the same issues, bodies and first moves, with no second
// issue.
func TestHumanTasksCreated(t *testing.T) {
	t.Parallel()
	pass := childrenPass(t, humanKind)
	githubtest.Replay(t, humanAtReview, pass)
	f := run(t, humanAtReview, pass)
	st := f.State()
	if len(st.Issues) != 5 {
		t.Fatalf("%d issues, want the parent and its four children: %+v", len(st.Issues), st.Issues)
	}
	if got := st.Issues[0].SubIssues; !reflect.DeepEqual(got, []int{2, 3, 4, 5}) {
		t.Errorf("issue 1's sub-issues are %v, want [2 3 4 5]", got)
	}
	p, _ := planMoveID(t, f)
	for i, w := range []struct {
		item, title string
		stage       state.Stage
		check       state.Check
	}{
		{"T1", "Add the verify check", state.Ready, state.Check{}},
		{"T4", "Ask the code owners in the PR template", state.Ready, state.Check{}},
		{"T3", "Name the code owners", state.NeedsHuman, state.Check{Kind: state.CheckFile, Value: ".github/CODEOWNERS", Contains: "* @owner"}},
		{"T2", "Require the verify check", state.NeedsHuman, state.Check{Kind: state.CheckRuleset, Value: "issuedog/verify"}},
	} {
		is := st.Issues[i+1]
		if is.Title != w.title || is.Parent != 1 {
			t.Errorf("#%d is %q under #%d, want %s, %q, under #1", is.Number, is.Title, is.Parent, w.item, w.title)
		}
		var comments []string
		for _, c := range is.Comments {
			comments = append(comments, c.Body)
		}
		if want := []string{firstMove(wantChild{item: w.item, stage: w.stage}, p)}; !reflect.DeepEqual(comments, want) {
			t.Errorf("#%d's comments are\n%q\nwant\n%q", is.Number, comments, want)
		}
		s := &github.Snapshot{Number: is.Number, Author: github.Author{Login: app, Type: "Bot"}, Body: is.Body, Type: "task", Parent: 1}
		o := state.Derive(s, slug, clean).Origin
		if want := (state.Origin{Plan: 1, Rev: 1, Item: w.item, Comment: state.BodyID, Human: w.stage == state.NeedsHuman, Check: w.check}); o != want {
			t.Errorf("#%d's origin is %+v, want %+v", is.Number, o, want)
		}
	}
	if got, want := st.Issues[3].Body, strings.Join([]string{
		"**Parent:** #1   **Scope:** scope:infra   **Priority:** p2",
		"",
		"**Goal:** Name the owner as the code owner of everything.",
		"**Why it blocks:** #3 can't proceed until code owners are named, since the review rule asks them.",
		"**Steps:**",
		"1. Add the line * @\u200bowner to .github/CODEOWNERS on main.",
		"",
		"**Links:**",
		"- https://github.com/o/r/blob/main/.github/CODEOWNERS",
		"",
		"**Done when:** The file `.github/CODEOWNERS` on `main` contains `* @owner`. I check this when you reply `/done` or react 👍 to this issue.",
		"**Estimated time:** 2 min",
		"",
		"cc @owner",
		"",
		"<!-- issuedog:origin plan=#1 rev=1 item=T3 check=file contains=*%20@owner human=1 value=.github/CODEOWNERS -->",
	}, "\n"); got != want {
		t.Errorf("T3's body is\n%s\nwant\n%s", got, want)
	}
	if want := "\n**Why it blocks:** #4 can't proceed until the ruleset requires the verify check, so code owners review only PRs that pass it.\n"; !strings.Contains(st.Issues[4].Body, want) {
		t.Errorf("T2's body has no %q:\n%s", want, st.Issues[4].Body)
	}
	if want := "\n**Done when:** An active ruleset on `main` requires the check `issuedog/verify`. I check this when you reply `/done` or react 👍 to this issue.\n"; !strings.Contains(st.Issues[4].Body, want) {
		t.Errorf("T2's body has no %q:\n%s", want, st.Issues[4].Body)
	}
	noSearch(t, f)
}
