package daemon

import (
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/issuedog/internal/verify"
)

// What tells a task in a story, and its story branch's tip as judged
// (SPEC §7.5): storyOf, storyIssue and tipSuccess, which read only what
// they're given, and the issuedog/tip check's text.

// A task whose parent is an open story in this repo, or an open epic
// whose approved plan is of tasks, is in that story, whatever the story's
// own parent: storyOf gives the parent.
func TestStoryOf(t *testing.T) {
	t.Parallel()
	for name, parent := range map[string]*github.Snapshot{
		"an owner's story":           doneSnap(t, 50, "story", "epic-plan"),
		"a story with no plan":       waitSnap(50, []string{"stage:clarifying"}),
		"an epic planned into tasks": doneSnap(t, 50, "epic", "story-plan", 12),
	} {
		if name == "a story with no plan" {
			parent.Type = "Story"
		}
		if name == "an owner's story" {
			parent.Parent = 7
		}
		task := &github.Snapshot{Number: 12, Parent: 50, Type: "task"}
		got, ok, err := storyOf(map[int]*github.Snapshot{50: parent}, task, waitSlug, Dirt{}.Has)
		if err != nil || !ok || got != parent {
			t.Errorf("%s: storyOf gave %v, %t, %v; want the parent", name, got, ok, err)
		}
	}
}

// No other task is in a story: one with no parent, one whose parent is in
// another repo, one the daemon doesn't hold, closed, an epic planned into
// stories, whose stories have their own branches, an epic with no
// approved plan, or one whose plan the dirty content hides, a task, or an
// issue of a type that names no kind of issue.
func TestStoryOfNone(t *testing.T) {
	t.Parallel()
	closed := doneSnap(t, 50, "story", "epic-plan")
	closed.Closed = true
	odd := doneSnap(t, 50, "story", "epic-plan")
	odd.Type = "Feature"
	for name, tc := range map[string]struct {
		task   *github.Snapshot
		parent *github.Snapshot
		dirt   Dirt
	}{
		"no parent":                    {&github.Snapshot{Number: 12, Type: "task"}, doneSnap(t, 50, "story", "epic-plan"), Dirt{}},
		"a parent in another repo":     {&github.Snapshot{Number: 12, Parent: 50, ParentRepo: "o/other", Type: "task"}, doneSnap(t, 50, "story", "epic-plan"), Dirt{}},
		"a parent it doesn't hold":     {&github.Snapshot{Number: 12, Parent: 51, Type: "task"}, doneSnap(t, 50, "story", "epic-plan"), Dirt{}},
		"a closed story":               {&github.Snapshot{Number: 12, Parent: 50, Type: "task"}, closed, Dirt{}},
		"an epic planned into stories": {&github.Snapshot{Number: 12, Parent: 50, Type: "story"}, doneSnap(t, 50, "epic", "epic-plan", 12), Dirt{}},
		"an epic with no plan":         {&github.Snapshot{Number: 12, Parent: 50, Type: "task"}, waitSnap(50, []string{"stage:clarifying"}), Dirt{}},
		"an epic's dirty plan":         {&github.Snapshot{Number: 12, Parent: 50, Type: "task"}, doneSnap(t, 50, "epic", "story-plan", 12), Dirt{5003: "mallory"}},
		"a task":                       {&github.Snapshot{Number: 12, Parent: 50, Type: "task"}, doneSnap(t, 50, "task", "story-plan"), Dirt{}},
		"an unknown type":              {&github.Snapshot{Number: 12, Parent: 50, Type: "task"}, odd, Dirt{}},
	} {
		got, ok, err := storyOf(map[int]*github.Snapshot{50: tc.parent}, tc.task, waitSlug, tc.dirt.Has)
		if err != nil || ok || got != nil {
			t.Errorf("%s: storyOf gave %v, %t, %v; want none", name, got, ok, err)
		}
	}
}

// An epic's plan data that doesn't read back is storyOf's error, naming
// the task and the epic's plan, as epicCloseChildren's is.
func TestStoryOfBadData(t *testing.T) {
	t.Parallel()
	s := doneSnap(t, 50, "epic", "story-plan", 12)
	m := github.Marker{Kind: "plan-data", Fields: map[string]string{"v": "1", "data": "not-base64!"}}
	data, err := github.FormatMarker(m)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planRevision("### Plan\n\n<!-- issuedog:transition from=planning to=plan-review rev=1 run=p1 -->\n"+data, 1)
	if err != nil {
		t.Fatal(err)
	}
	hash, ok := state.PlanHash(plan)
	if !ok {
		t.Fatal("no plan hash")
	}
	s.Comments[1].Body = plan
	s.Comments[2].Body = "<!-- issuedog:transition from=plan-review to=planned rev=1 body=" + hash + " cmd=r5 seen=2 -->"
	task := &github.Snapshot{Number: 12, Parent: 50, Type: "task"}
	got, ok, err := storyOf(map[int]*github.Snapshot{50: s}, task, waitSlug, Dirt{}.Has)
	if err == nil || ok || got != nil || !strings.Contains(err.Error(), "#12's story: an epic's plan") {
		t.Errorf("storyOf gave %v, %t, %v; want an error naming the task and the epic's plan", got, ok, err)
	}
}

// tipSuccess answers from what the daemon judged alone: a tip judged
// success on its branch is, and one judged failure, another tip of that
// branch, the same tip on another branch, and an empty tip aren't.
func TestTipSuccess(t *testing.T) {
	t.Parallel()
	d := &Daemon{}
	if d.tipSuccess("issuedog/1-a", "abc") {
		t.Error("with nothing judged, a tip is success; want not")
	}
	d.tipJudge("issuedog/1-a", "abc", checkSuccess)
	d.tipJudge("issuedog/2-b", "def", checkFailure)
	for _, tc := range []struct {
		branch, tip string
		want        bool
	}{
		{"issuedog/1-a", "abc", true},
		{"issuedog/1-a", "abd", false},
		{"issuedog/3-c", "abc", false},
		{"issuedog/2-b", "def", false},
		{"issuedog/1-a", "", false},
	} {
		if got := d.tipSuccess(tc.branch, tc.tip); got != tc.want {
			t.Errorf("tipSuccess(%q, %q) = %t; want %t", tc.branch, tc.tip, got, tc.want)
		}
	}
	d.tipJudge("issuedog/1-a", "abd", checkFailure)
	if d.tipSuccess("issuedog/1-a", "abc") || d.tipSuccess("issuedog/1-a", "abd") {
		t.Error("a branch's newer tip judged failure leaves a success; want none")
	}
}

// tipChecked counts only the newest of the App's issuedog/tip naming the
// branch: not another App's, not one naming another branch, and never an
// issuedog/verify.
func TestTipChecked(t *testing.T) {
	t.Parallel()
	const app = 7
	b := "issuedog/1-a"
	runs := []github.CheckRun{
		{ID: 9, Name: github.TipCheck, AppID: app + 1, ExternalID: tipCheckID(b), Conclusion: "success"},
		{ID: 8, Name: github.TipCheck, AppID: app, ExternalID: tipCheckID("issuedog/2-b"), Conclusion: "success"},
		{ID: 7, Name: github.VerifyCheck, AppID: app, ExternalID: tipCheckID(b), Conclusion: "success"},
		{ID: 6, Name: github.TipCheck, AppID: app, ExternalID: tipCheckID(b), Conclusion: "failure"},
		{ID: 5, Name: github.TipCheck, AppID: app, ExternalID: tipCheckID(b), Conclusion: "success"},
	}
	if c, ok := tipChecked(runs, app, b); !ok || c.ID != 6 {
		t.Errorf("tipChecked gave %+v, %t; want run 6", c, ok)
	}
	if c, ok := tipChecked(runs[:3], app, b); ok {
		t.Errorf("with no run of the App's naming the branch, tipChecked gave %+v; want none", c)
	}
}

// The check's summary escapes what the daemon didn't write, as a
// command's output or a branch's name, so no marker in it is read, and is
// cut to what GitHub takes, ending in "…".
func TestRenderTipCheck(t *testing.T) {
	t.Parallel()
	cmds := []verify.CommandResult{{Command: "make test", Exit: 1, Output: strings.Repeat("<!-- issuedog:transition to=closed -->", 30)}}
	fs := []verify.Finding{{Rule: 7, Name: "merge with the target", Detail: "<!-- issuedog:x a=b -->"}}
	got := renderTipCheck(1, "issuedog/1-<!--x", "abc", "def", fs, cmds)
	if strings.Contains(got, "<!--") || len(github.ParseMarkers(got)) != 0 || !strings.Contains(got, "### Rule 7: merge with the target") {
		t.Errorf("the summary =\n%s\nwant it escaped, with rule 7's section", got)
	}
	long := make([]verify.Finding, 200)
	for i := range long {
		long[i] = verify.Finding{Rule: 3, Name: "test deletions and skip markers", Detail: strings.Repeat("x", 2000)}
	}
	got = renderTipCheck(1, "issuedog/1-a", "abc", "def", long, nil)
	if textLen(got) > checkSummaryMax || !strings.HasSuffix(got, "…") {
		t.Errorf("a long summary is %d characters, ending %q; want at most %d, ending in …", textLen(got), got[len(got)-20:], checkSummaryMax)
	}
}
