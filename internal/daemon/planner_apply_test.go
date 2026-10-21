package daemon_test

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// The planner's results, checked and applied (#259), through the harness
// with the real Planner at planning.

// planner puts the real Planner at planning on g, trusting ivTrust, typing
// by label, notifying owner, with the scopes and models the golden plans
// use, the planner's model opus and effort high, plan.review set to
// review, and its clone synced, so it can read its prompt.
func (g *rig) planner(review bool) {
	g.t.Helper()
	g.d.Step.Trust = ivTrust
	g.d.Config.Types = "labels"
	g.d.Config.Notify = []string{"owner"}
	g.d.Config.Scopes = []string{"booking", "infra", "docs"}
	g.d.Config.Plan.Review = review
	g.allow()
	g.d.Config.Stages.Model.Planner = "opus"
	g.d.Config.Stages.Effort.Planner = "high"
	g.r.Stages[state.Planning] = &daemon.Planner{Daemon: g.d}
	if err := g.d.Clone.Sync(context.Background(), ""); err != nil {
		g.t.Fatal(err)
	}
}

// plRig is a rig on f with the real Planner (rig.planner), and a fake
// claude that also keeps each run's input.md as $FAKE/input.<n>.
func plRig(t *testing.T, f *githubtest.Fake, review bool) *rig {
	t.Helper()
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.planner(review)
	if err := os.WriteFile(g.path("claude", "claude"), []byte(plReadingClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	return g
}

// plReadingClaude is the fake claude, which also copies the run's input.md
// from the context directory, as the agent reads it, to $FAKE/input.<n>.
var plReadingClaude = strings.Replace(fakeClaude, `cat "$FAKE/out"`,
	`for i in "$FAKE"/../ctx/*/input.md; do [ -f "$i" ] && cp "$i" "$FAKE/input.$n"; done
cat "$FAKE/out"`, 1)

// plEntry is the move into planning by the spec's approval.
const plEntry = "from=spec-review to=planning cmd=r7 rev=1 body=abcd1234"

// plFiled files owner's issue "Quotes" of type typ, as a label the owner
// added, with a parent if parent is set, at planning, with an App comment
// for each of moves, a transition marker's fields, in order. It returns
// the issue's number and the last comment's ID.
func plFiled(f *githubtest.Fake, typ string, parent bool, moves ...string) (int, int64) {
	n := f.AddIssue("owner", "Quotes", "The spec.")
	f.AddLabel(n, "owner", "type:"+typ)
	if parent {
		p := f.AddIssue("owner", "The epic", "")
		f.AddLabel(p, "owner", "type:epic")
		f.SetParent(n, p)
	}
	f.AddLabel(n, app, "stage:planning")
	var id int64
	for _, m := range moves {
		id = f.AddComment(n, app, "Moved.\n\n<!-- issuedog:transition "+m+" -->")
	}
	return n, id
}

// plGolden is internal/agent's golden planner output name, decoded.
func plGolden(t testing.TB, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "agent", "testdata", "outputs", "planner", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// plItem is item i of out's plan.
func plItem(t testing.TB, out map[string]any, i int) map[string]any {
	t.Helper()
	plan, ok := out["plan"].(map[string]any)
	if !ok {
		t.Fatalf("output %v has no plan", out)
	}
	items, ok := plan["items"].([]any)
	if !ok || i >= len(items) {
		t.Fatalf("plan %v has no item %d", plan, i)
	}
	it, ok := items[i].(map[string]any)
	if !ok {
		t.Fatalf("item %d is %v, not an object", i, items[i])
	}
	return it
}

// plComment splits issue n's newest comment into its text and its markers,
// each line after the text, and checks it's the App's.
func plComment(t *testing.T, f *githubtest.Fake, n int) (string, []string) {
	t.Helper()
	cs := f.State().Issues[n-1].Comments
	c := cs[len(cs)-1]
	if c.Author != app {
		t.Fatalf("the newest comment is %s's, not the App's: %q", c.Author, c.Body)
	}
	text, markers, ok := strings.Cut(c.Body, "\n\n<!-- issuedog:")
	if !ok {
		t.Fatalf("the newest comment has no markers: %q", c.Body)
	}
	return text, strings.Split("<!-- issuedog:"+markers, "\n")
}

// plData decodes the plan-data marker m back to JSON, as the plan it
// holds, for comparison with a golden output's plan.
func plData(t *testing.T, m string) any {
	t.Helper()
	ms := github.ParseMarkers(m)
	if len(ms) != 1 || ms[0].Kind != "plan-data" || ms[0].Fields["v"] != "1" {
		t.Fatalf("%q isn't a plan-data marker of version 1", m)
	}
	z, err := base64.RawURLEncoding.DecodeString(ms[0].Fields["data"])
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(flate.NewReader(bytes.NewReader(z)))
	if err != nil {
		t.Fatal(err)
	}
	var plan any
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	return plan
}

// plRevision checks that issue n's latest plan revision is rev, in its
// newest comment, that nothing hides it and that its hash still matches,
// and that its latest spec revision isn't it.
func plRevision(t *testing.T, g *rig, n, rev int) {
	t.Helper()
	s := g.snapshot(n)
	got, ok := state.LatestRevision(s, slug, state.PlanRevision, func(int64) bool { return false })
	if newest := s.Comments[len(s.Comments)-1].ID; !ok || got.Rev != rev || got.Comment != newest || got.Hidden || !got.Matches(s) {
		t.Errorf("the latest plan revision = %+v (found %t, matches %t); want rev %d in comment %d, its hash matching", got, ok, got.Matches(s), rev, newest)
	}
	if spec, ok := state.LatestRevision(s, slug, state.SpecRevision, func(int64) bool { return false }); ok && spec.Comment == got.Comment {
		t.Errorf("the plan comment %d is read as spec revision %+v", got.Comment, spec)
	}
}

// TestPlannerEpicPlan: a valid epic plan posts one comment, the plan in
// the 11.4 format, then its transition marker, with rev and the run's
// fields, the plan-data marker holding the plan, which reads back exactly,
// and the plan revision marker, whose hash matches; the issue moves to
// plan-review. A plan revision posted before makes this one rev 2.
func TestPlannerEpicPlan(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		old  bool // a plan revision 1 was posted and sent back
		rev  int
	}{
		{"the first plan", false, 1},
		{"after a plan sent back", true, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			n, seen := plFiled(f, "epic", false, plEntry)
			if tt.old {
				f.AddComment(n, app, "An old plan.\n\n<!-- issuedog:transition from=planning to=plan-review rev=1 run=old -->\n"+
					"<!-- issuedog:revision kind=plan rev=1 body=deadbeef -->")
				seen = f.AddComment(n, app, "Sent back.\n\n<!-- issuedog:transition from=plan-review to=planning cmd=r8 -->")
			}
			g := plRig(t, f, true)
			out := plGolden(t, "epic-plan")
			g.claude(ivResult(t, out), 0)
			comments := len(f.State().Issues[n-1].Comments)
			if err := g.work(n); err != nil {
				t.Fatal(err)
			}
			if got := len(f.State().Issues[n-1].Comments); got != comments+1 {
				t.Errorf("%d comments posted, want 1", got-comments)
			}
			// The sub-issues are read once for the context, and once for the
			// check, whose count Apply takes.
			reads := 0
			for _, r := range f.Requests() {
				if r.Method == http.MethodGet && r.Path == fmt.Sprintf("/issues/%d/sub_issues", n) {
					reads++
				}
			}
			if reads != 2 {
				t.Errorf("the sub-issues were read %d times, want 2: the context's and the check's", reads)
			}
			text, markers := plComment(t, f, n)
			for _, want := range []string{
				fmt.Sprintf("### Plan for #%d (rev %d)\n\n", n, tt.rev),
				"Validation (11.3): 3 stories, with no tasks; no dependency cycles; 3 sub-issues in all, within 100;",
				"\n\ncc @owner\n\n**To approve this plan, react 👍 to this comment.**",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("the plan text lacks %q:\n%s", want, text)
				}
			}
			if !strings.HasPrefix(text, fmt.Sprintf("### Plan for #%d (rev %d)", n, tt.rev)) {
				t.Errorf("the plan text doesn't start with its heading:\n%s", text)
			}
			if len(markers) != 3 {
				t.Fatalf("markers %q, want the transition, the plan data and the revision", markers)
			}
			want := fmt.Sprintf("<!-- issuedog:transition from=planning to=plan-review rev=%d run=run1 effort=high model=opus seen=%d -->", tt.rev, seen)
			if markers[0] != want {
				t.Errorf("the transition marker = %s, want %s", markers[0], want)
			}
			if got, want := plData(t, markers[1]), out["plan"]; !reflect.DeepEqual(got, want) {
				t.Errorf("the plan data reads back as\n%v\nwant\n%v", got, want)
			}
			if !strings.HasPrefix(markers[2], fmt.Sprintf("<!-- issuedog:revision kind=plan rev=%d body=", tt.rev)) {
				t.Errorf("the last line = %s, want plan revision %d's marker", markers[2], tt.rev)
			}
			plRevision(t, g, n, tt.rev)
			if labels := ivLabels(f, n); !slices.Equal(labels, []string{"stage:plan-review", "type:epic"}) {
				t.Errorf("labels %v, want stage:plan-review", labels)
			}
			writes := len(f.Writes())
			if err := g.work(n); err != nil || len(f.Writes()) != writes || g.runs() != 1 {
				t.Errorf("the pass after = %v, with writes %v and %d runs; want no run and no write", err, f.Writes()[writes:], g.runs())
			}
		})
	}
}

// TestPlannerReview: a story's plan skips review, moving straight to
// planned on plan-no-review with a line saying so, only when plan.review
// is false and the move that put it at planning carries no origin, looking
// through the planner's rounds and an owner's hold. After a discussion
// hold, whose moves carry origin, it goes to plan-review, and so does an
// epic's plan, and any plan with plan.review true.
func TestPlannerReview(t *testing.T) {
	t.Parallel()
	const (
		fromHold = "from=clarifying to=planning run=iv9 origin=planning seen=3"
		hold     = "from=planning to=held at=planning reason=owner cmd=900001 seen=3"
		resume   = "from=held to=planning cmd=900002 seen=3"
	)
	for _, tt := range []struct {
		name   string
		typ    string
		parent bool
		review bool
		moves  []string
		round  bool // a planner round and its answer before the plan
		to     string
	}{
		{"a story with a parent", "story", true, false, []string{plEntry}, false, "planned"},
		{"a story without a parent", "story", false, false, []string{plEntry}, false, "planned"},
		{"a story with review on", "story", true, true, []string{plEntry}, false, "plan-review"},
		{"an epic", "epic", false, false, []string{plEntry}, false, "plan-review"},
		{"a story after a discussion hold", "story", true, false, []string{plEntry, fromHold}, false, "plan-review"},
		{"a story after a discussion hold and a round", "story", true, false, []string{plEntry, fromHold}, true, "plan-review"},
		{"a story after a round", "story", true, false, []string{plEntry}, true, "planned"},
		{"a story after a discussion hold, then an owner's", "story", true, false, []string{plEntry, fromHold, hold, resume}, false, "plan-review"},
		{"a story after an owner's hold", "story", true, false, []string{plEntry, hold, resume}, false, "planned"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			n, seen := plFiled(f, tt.typ, tt.parent, tt.moves...)
			if tt.round {
				planAsked(f, n, 1, 1, seen)
				seen = f.AddComment(n, "owner", "1a")
			}
			g := plRig(t, f, tt.review)
			g.claude(ivResult(t, plGolden(t, "story-plan")), 0)
			if err := g.work(n); err != nil {
				t.Fatal(err)
			}
			text, markers := plComment(t, f, n)
			want := fmt.Sprintf("<!-- issuedog:transition from=planning to=%s rev=1 run=run1 effort=high model=opus seen=%d -->", tt.to, seen)
			if len(markers) != 3 || markers[0] != want {
				t.Fatalf("markers %q, want %s first, then the plan data and the revision", markers, want)
			}
			skips := fmt.Sprintf("This plan skips review, as `plan.review` is false: #%d moves straight to `planned`.", n)
			if strings.HasSuffix(text, skips) != (tt.to == "planned") || strings.Contains(text, "To approve this plan") == (tt.to == "planned") {
				t.Errorf("the plan text ends\n%s\nwant the skip line alone only for a plan that skips review", text[max(0, len(text)-300):])
			}
			plRevision(t, g, n, 1)
			if labels := ivLabels(f, n); !slices.Equal(labels, []string{"stage:" + tt.to, "type:" + tt.typ}) {
				t.Errorf("labels %v, want stage:%s", labels, tt.to)
			}
		})
	}
}

// TestPlannerRound: a question round moves planning → planning on
// plan-questions, with the next round of every stage's, here 3 after the
// interviewer's 1 and 2, and its questions, and the run's fields. Nothing
// runs until an answer; owner's answer then starts the next run, whose
// input.md has every answer since, alice's too, as answers to round 3.
func TestPlannerRound(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n, entry := ivFiled(f, "epic", false, "from=intake to=clarifying event=5")
	ivAsked(f, n, 1, 2, entry)
	a1 := f.AddComment(n, "owner", "1a")
	ivAsked(f, n, 2, 1, a1)
	a2 := f.AddComment(n, "owner", "1a 2b")
	f.AddComment(n, app, fmt.Sprintf("Spec.\n\n<!-- issuedog:transition from=clarifying to=spec-review run=sp seen=%d rev=1 body=abcd1234 -->", a2))
	f.RemoveLabel(n, app, "stage:clarifying")
	f.AddLabel(n, app, "stage:planning")
	approval := f.AddComment(n, app, "Approved.\n\n<!-- issuedog:transition "+plEntry+" -->")
	g := plRig(t, f, true)
	out := plGolden(t, "questions")
	g.claude(ivResult(t, out), 0)
	if err := g.work(n); err != nil {
		t.Fatal(err)
	}
	text, markers := plComment(t, f, n)
	want := fmt.Sprintf("<!-- issuedog:transition from=planning to=planning round=3 run=run1 effort=high model=opus questions=2 seen=%d -->", approval)
	if !slices.Equal(markers, []string{want}) {
		t.Errorf("markers %q, want %s alone", markers, want)
	}
	if !strings.HasPrefix(text, fmt.Sprintf("### Questions for #%d (round 3)\n\n**1. ", n)) || !strings.HasSuffix(text, "\n\ncc @owner") ||
		!strings.Contains(text, "**2. ") || strings.Contains(text, "**3. ") {
		t.Errorf("the round =\n%s\nwant round 3's two questions in the SPEC §9.1 format, then the cc line", text)
	}
	if labels := ivLabels(f, n); !slices.Equal(labels, []string{"stage:planning", "type:epic"}) {
		t.Errorf("labels %v, want the issue to stay at planning", labels)
	}
	writes := len(f.Writes())
	if err := g.work(n); err != nil || len(f.Writes()) != writes || g.runs() != 1 {
		t.Errorf("work before an answer = %v, with writes %v and %d runs; want no run and no write", err, f.Writes()[writes:], g.runs())
	}
	b1 := f.AddComment(n, "alice", "1a")
	b2 := f.AddComment(n, "owner", "2: a setting, please")
	g.claude(ivResult(t, plGolden(t, "epic-plan")), 0)
	if err := g.work(n); err != nil {
		t.Fatal(err)
	}
	input := g.file("input.2")
	for _, want := range []string{
		fmt.Sprintf("## Comment %d by alice: an answer to round 3\n\n```text\n1a\n```", b1),
		fmt.Sprintf("## Comment %d by owner: an answer to round 3\n\n```text\n2: a setting, please\n```", b2),
	} {
		if !strings.Contains(input, want) {
			t.Errorf("run 2's input.md lacks\n%s\nin\n%s", want, input)
		}
	}
	if _, markers := plComment(t, f, n); g.runs() != 2 || len(markers) != 3 || !strings.Contains(markers[0], "to=plan-review rev=1 run=run2 ") {
		t.Errorf("after the answers: %d runs, newest markers %q; want run2's plan applied", g.runs(), markers)
	}
}

// TestPlannerCheckFailsAttempt: output that breaks a T6 rule writes
// nothing, and is never rendered: with loop.max_attempts 1, the only
// comment is the one kind=attempts error, naming the rule, fenced, and no
// plan text. No run follows until new input, which starts a new count.
func TestPlannerCheckFailsAttempt(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, golden, typ, rule string
		change                  func(t testing.TB, out map[string]any)
	}{
		{"a scope not on the list", "story-plan", "story", `rule scope: T3's scope "web" isn't on the list`,
			func(t testing.TB, out map[string]any) { plItem(t, out, 2)["scope"] = "web" }},
		{"a plan of stories for a story", "epic-plan", "story", "rule structure: a story's plan has tasks, not stories", nil},
		{"a cycle", "story-plan", "story", "rule cycle: T1 → T2 → T1",
			func(t testing.TB, out map[string]any) { plItem(t, out, 0)["depends_on"] = []string{"T2"} }},
		{"a file path in a title", "epic-plan", "epic", `rule file-paths: S1's title names the file path "internal/quote.go"`,
			func(t testing.TB, out map[string]any) {
				plItem(t, out, 0)["title"] = "Expire quotes in internal/quote.go"
			}},
		{"a question round of 8", "questions", "epic", "rule question-count: 8 questions, over the limit of 7",
			func(t testing.TB, out map[string]any) {
				qs, ok := out["questions"].([]any)
				if !ok || len(qs) == 0 {
					t.Fatalf("output %v has no questions", out)
				}
				for len(qs) < 8 {
					qs = append(qs, qs[0])
				}
				out["questions"] = qs
			}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			n, entry := plFiled(f, tt.typ, false, plEntry)
			g := plRig(t, f, true)
			g.d.Config.Loop.MaxAttempts = 1
			out := plGolden(t, tt.golden)
			if tt.change != nil {
				tt.change(t, out)
			}
			g.claude(ivResult(t, out), 0)
			before := commentBodies(f, n)
			if err := g.work(n); err != nil {
				t.Fatal(err)
			}
			after := commentBodies(f, n)
			if l := g.lease(); g.runs() != 1 || !l.Failed || l.Attempt != 1 || !strings.Contains(l.LastFailure, tt.rule) {
				t.Errorf("after %d runs, lease %+v; want one failed attempt, its last failure naming %q", g.runs(), l, tt.rule)
			}
			if len(after) != len(before)+1 {
				t.Fatalf("comments %q, want the attempts error alone", after[len(before):])
			}
			cap := after[len(after)-1]
			if !strings.HasPrefix(cap, "The planner run failed 1 times in a row, so I've stopped running it here. ") ||
				// It quotes the broken rules alone, not the check's wording
				// around them, as the comment quotes only a *ruleError (#320).
				!strings.Contains(cap, "\n\nThe last attempt failed:\n\n```text\nrule ") ||
				!strings.Contains(cap, tt.rule) ||
				!strings.HasSuffix(cap, fmt.Sprintf("\n```\n\n<!-- issuedog:error kind=attempts key=planning-%d -->", entry)) ||
				strings.Contains(cap, "### ") {
				t.Errorf("the attempts error =\n%s\nwant it to name %q, fenced, and no plan text", cap, tt.rule)
			}
			if labels := ivLabels(f, n); !slices.Equal(labels, []string{"stage:planning", "type:" + tt.typ}) {
				t.Errorf("labels %v, want the issue left at planning", labels)
			}
			writes := len(f.Writes())
			if err := g.work(n); err != nil || g.runs() != 1 || len(f.Writes()) != writes {
				t.Errorf("the pass after = %v, %d runs, writes %v; want no run and no write", err, g.runs(), f.Writes()[writes:])
			}
			f.AddComment(n, "owner", "Please try again.")
			if err := g.work(n); err != nil || g.runs() != 2 {
				t.Errorf("after new input: %v, %d runs; want a second run", err, g.runs())
			}
		})
	}
}

// TestPlannerCheckIssue: Check fails output on an issue the planner no
// longer runs on, read again after the run: one retyped to a task, or one
// planned before whose approved plan can't be read back, so a revision
// can't be checked. A valid plan on an issue it runs on passes.
func TestPlannerCheckIssue(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, typ string
		moves     []string
		want      string
	}{
		{"a story", "story", []string{plEntry}, ""},
		{"a task", "task", []string{plEntry}, `the planner no longer runs on #1, with type "task" and a parent false: it runs only on an epic or a story`},
		{"planned before, with no approved plan to revise", "story", []string{plEntry, "from=planning to=planned rev=1 run=p0", "from=planned to=planning by=owner event=8"},
			"the planner no longer runs on #1: it was planned before, but it has no approved plan revision whose data reads back, so it can't be revised"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			n, _ := plFiled(f, tt.typ, false, tt.moves...)
			d := ivDaemon(t, f, newOrigin(t))
			d.Config.Scopes = []string{"booking", "infra"}
			d.Config.Models.Allowed = []config.Model{{Name: "sonnet"}}
			in := ivInput(t, d, n)
			err := (&daemon.Planner{Daemon: d}).Check(context.Background(),
				daemon.Result{RunID: "run1", S: in.S, Dirt: in.Dirt, Output: ivJSON(t, plGolden(t, "story-plan"))})
			if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("Check = %v, want an error with %q, or nil for \"\"", err, tt.want)
			}
		})
	}
}

// TestPlannerCheckReadFails: a failed read of the sub-issues while the
// result is checked is a failure to apply, not a failed attempt: nothing is
// written, the lease and the result stay, and the next pass applies them.
func TestPlannerCheckReadFails(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n, _ := plFiled(f, "story", false, plEntry)
	g := plRig(t, f, true)
	g.claude(ivResult(t, plGolden(t, "story-plan")), 0)
	// The first read is the context's, before the run; the second, Check's.
	reads := 0
	f.AnswerOnce(func(r githubtest.Request) bool {
		if r.Method == http.MethodGet && r.Path == fmt.Sprintf("/issues/%d/sub_issues", n) {
			reads++
		}
		return reads == 2
	}, http.StatusInternalServerError)
	before := commentBodies(f, n)
	err := g.work(n)
	if err == nil || !strings.Contains(err.Error(), "the planner stage's check: the planner's facts on #1: its sub-issues") {
		t.Errorf("work = %v, want the check's read error", err)
	}
	if l := g.lease(); l.Failed || l.ApplyFailures != 1 || l.RunID != "run1" || !slices.Equal(commentBodies(f, n), before) {
		t.Errorf("lease %+v, comments %q; want the lease kept, one failure to apply, and nothing written", l, commentBodies(f, n)[len(before):])
	}
	if err := g.work(n); err != nil {
		t.Fatal(err)
	}
	if _, markers := plComment(t, f, n); g.runs() != 1 || len(markers) != 3 || !strings.Contains(markers[0], "to=plan-review rev=1 run=run1 ") {
		t.Errorf("after the read works: %d runs, newest markers %q; want run1's plan applied, with no second run", g.runs(), markers)
	}
}

// TestPlannerApplyChecksFirst: Apply on its own, without Check, renders and
// posts nothing from output that fails checkPlan, so the plan comment's
// validation line is never untrue. A valid round applied on its own reads
// the sub-issues itself.
func TestPlannerApplyChecksFirst(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n, _ := plFiled(f, "story", false, plEntry)
	d := ivDaemon(t, f, newOrigin(t))
	d.Config.Scopes = []string{"booking"} // not infra, T3's scope
	d.Config.Models.Allowed = []config.Model{{Name: "sonnet"}}
	in := ivInput(t, d, n)
	p := &daemon.Planner{Daemon: d}
	err := p.Apply(context.Background(), d.Step.Applier, daemon.Result{RunID: "run1", S: in.S, Dirt: in.Dirt, Output: ivJSON(t, plGolden(t, "story-plan"))})
	if err == nil || !strings.Contains(err.Error(), "apply planner run run1 on o/r#1: check planner output: rule scope: T3's scope \"infra\" isn't on the list") {
		t.Errorf("Apply = %v, want the scope rule's error", err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("Apply wrote %v", w)
	}
	if err := p.Apply(context.Background(), d.Step.Applier, daemon.Result{RunID: "run2", S: in.S, Dirt: in.Dirt, Output: ivJSON(t, plGolden(t, "questions"))}); err != nil {
		t.Fatal(err)
	}
	if _, markers := plComment(t, f, n); !slices.Equal(markers, []string{"<!-- issuedog:transition from=planning to=planning round=1 run=run2 questions=2 -->"}) {
		t.Errorf("the round's markers = %q, want round 1's move", markers)
	}
	for _, out := range []string{`{"outcome":"plan"}`, `{"outcome":"done"}`, `{"outcome":"plan","extra":1}`} {
		writes := len(f.Writes())
		in := ivInput(t, d, n)
		if err := p.Apply(context.Background(), d.Step.Applier, daemon.Result{RunID: "run3", S: in.S, Dirt: in.Dirt, Output: []byte(out)}); err == nil || len(f.Writes()) != writes {
			t.Errorf("Apply of %s = %v, writing %v; want an error, and nothing written", out, err, f.Writes()[writes:])
		}
	}
}

// TestPlannerApplyReplay: a plan's writes, to plan-review and to planned,
// and a round's, are safe to replay after a crash at any of them: the
// comment, the stage label, and the old label's removal. Once the comment
// is posted, a restart only finishes the labels, with no second comment.
func TestPlannerApplyReplay(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		typ, golden string
		review      bool
	}{
		"an epic's plan, to plan-review": {"epic", "epic-plan", true},
		"a story's plan, to planned":     {"story", "story-plan", false},
		"a question round":               {"story", "questions", true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			replayRuns(t, func(_ testing.TB, f *githubtest.Fake, _ string) {
				plFiled(f, tt.typ, true, plEntry)
			}, func(g *rig) {
				g.planner(tt.review)
				g.claude(ivResult(t, plGolden(t, tt.golden)), 0)
			})
		})
	}
}

// TestPlannerRestartAfterComment: a restart after the plan comment was
// posted, before its labels, applies the saved result again: it moves the
// labels, with no second comment and no check of the result.
func TestPlannerRestartAfterComment(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n, _ := plFiled(f, "story", false, plEntry)
	root := t.TempDir()
	g := newRig(t, f, newOrigin(t), root)
	g.planner(false)
	g.claude(ivResult(t, plGolden(t, "story-plan")), 0)
	// The label's write fails, as a crash just before it would leave it.
	f.AnswerOnce(func(r githubtest.Request) bool {
		return r.Method == http.MethodPost && r.Path == fmt.Sprintf("/issues/%d/labels", n)
	}, http.StatusInternalServerError)
	if err := g.work(n); err == nil {
		t.Fatal("work = nil, want the label's error")
	}
	comments := len(f.State().Issues[n-1].Comments)
	if labels := ivLabels(f, n); !slices.Equal(labels, []string{"stage:planning", "type:story"}) {
		t.Fatalf("labels %v after the failed label, want stage:planning still", labels)
	}
	// The scopes change, so the result would fail a check now: it isn't
	// checked again, as its move is written.
	restarted := newRig(t, f, newOrigin(t), root)
	restarted.planner(false)
	restarted.d.Config.Scopes = nil
	if err := restarted.work(n); err != nil {
		t.Fatal(err)
	}
	if got := len(f.State().Issues[n-1].Comments); got != comments || g.runs() != 1 {
		t.Errorf("%d comments posted and %d runs after the restart; want none and 1", got-comments, g.runs())
	}
	if labels := ivLabels(f, n); !slices.Equal(labels, []string{"stage:planned", "type:story"}) {
		t.Errorf("labels %v after the restart, want stage:planned", labels)
	}
}

// TestPlannerRunSettings: the planner's run gets its stages.* model,
// effort, fallback models and turns, an issue's model:* and effort:* labels
// override the model and effort, and its timeout kills a run that takes
// too long, as a failed attempt.
func TestPlannerRunSettings(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	plFiled(f, "story", false, plEntry)
	n2, _ := plFiled(f, "story", false, plEntry)
	f.AddLabel(n2, "owner", "model:haiku")
	f.AddLabel(n2, "owner", "effort:low")
	g := plRig(t, f, true)
	c := g.d.Config
	c.Stages.Model = config.PerStage[string]{Interviewer: "haiku", Planner: "sonnet", Implementer: "opus", Verifier: "opus"}
	c.Stages.Effort = config.PerStage[string]{Interviewer: "low", Planner: "max", Implementer: "high", Verifier: "high"}
	c.Stages.FallbackModel.Planner = []string{"opus"}
	c.Stages.MaxTurns.Planner = 9
	g.claude(ivResult(t, plGolden(t, "questions")), 0)
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	g.checkFlags(1, "sonnet", "max", "opus")
	if got := g.flags(1)["--max-turns"]; got != "9" {
		t.Errorf("--max-turns = %q, want the planner's 9", got)
	}
	if err := g.work(n2); err != nil {
		t.Fatal(err)
	}
	g.checkFlags(2, "haiku", "low", "opus")
	if _, markers := plComment(t, f, n2); len(markers) != 1 || !strings.Contains(markers[0], " effort=low model=haiku ") {
		t.Errorf("#%d's round markers %q, want the labels' model and effort", n2, markers)
	}
	c.Stages.Timeouts.Planner = 200 * time.Millisecond
	if err := os.WriteFile(g.path("claude", "sleep"), []byte("10"), 0o644); err != nil {
		t.Fatal(err)
	}
	n3, _ := plFiled(f, "story", false, plEntry)
	start := time.Now()
	if err := g.work(n3); err != nil {
		t.Fatal(err)
	}
	if took, l := time.Since(start), g.lease(); took > 5*time.Second || !l.Failed || l.Attempt != 1 || l.Issue != n3 {
		t.Errorf("the run took %s, lease %+v; want it killed at the planner's timeout, a failed attempt", took, l)
	}
}
