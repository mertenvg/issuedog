package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// The comment Apply posts for a plan is exactly planCommentBody of the
// facts Check holds it to, but for the run's ID and so the hash, with the
// run's seen, model, effort and ran in its transition marker: the length
// rule counts what's posted (#257's verify). So for a story's plan that
// skips review.
func TestPlannerApplyPostsCheckedBody(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, typ, golden string
		review            bool
		to                state.Stage
	}{
		{"an epic's plan", "epic", "epic-plan", true, state.PlanReview},
		{"a story's plan that skips review", "story", "story-plan", false, state.Planned},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := githubtest.New(t, "o/r", "issuedog")
			n := f.AddIssue("owner", "Title", "body", "stage:planning", "type:"+tt.typ)
			f.AddComment(n, "issuedog[bot]", "<!-- issuedog:transition from=spec-review to=planning rev=1 body=aaaaaaaa cmd=r5 -->")
			c := f.Client(t)
			s, err := c.Snapshot(context.Background(), "o", "r", n)
			if err != nil {
				t.Fatal(err)
			}
			cfg := config.Defaults()
			cfg.Scopes, cfg.Notify, cfg.Plan.Review = []string{"booking", "infra"}, []string{"owner"}, tt.review
			cfg.Models.Allowed = []config.Model{{Name: "opus"}, {Name: "sonnet"}}
			a := &Applier{Client: c, Owner: "o", Repo: "r", AppSlug: "issuedog"}
			p := &Planner{Daemon: &Daemon{Config: cfg, Step: &Step{Applier: a}}}
			raw, err := json.Marshal(plannerOutput(t, tt.golden))
			if err != nil {
				t.Fatal(err)
			}
			r := Result{RunID: "abcdefgh", Seen: 1001, S: s, Dirt: Dirt{}, Output: raw,
				fields: map[string]string{"model": "opus", "effort": "high", "ran": "claude-opus-4-5"}}
			if err := p.Check(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			is := state.Derive(s, a.AppSlug, r.Dirt.Has)
			class, err := state.ClassOf(is.Item)
			if err != nil {
				t.Fatal(err)
			}
			facts := p.facts(r, is, class, 0)
			if facts.NoReview != (tt.to == state.Planned) || facts.Rev != 1 {
				t.Fatalf("facts %+v, want rev 1, skipping review %t", facts, tt.to == state.Planned)
			}
			want, err := planCommentBody(*plannerOutput(t, tt.golden).Plan, facts)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Apply(context.Background(), a, r); err != nil {
				t.Fatal(err)
			}
			posted := s.Comments[len(s.Comments)-1].Body
			if textLen(want) < textLen(posted) {
				t.Errorf("planCommentBody is %d characters, under the posted comment's %d", textLen(want), textLen(posted))
			}
			want = strings.Replace(want, "run="+planLengthRunID, "run=abcdefgh", 1)
			if cut := func(s string) string { return s[:strings.LastIndex(s, " body=")] }; cut(want) != cut(posted) {
				t.Errorf("planCommentBody\n%s\nposted\n%s", want, posted)
			}
			if !strings.Contains(posted, "<!-- issuedog:transition from=planning to="+string(tt.to)+" rev=1 run=abcdefgh effort=high model=opus ran=claude-opus-4-5 seen=1001 -->") {
				t.Errorf("the posted comment's transition isn't the run's move to %s:\n%s", tt.to, posted)
			}
		})
	}
}

// planEntry is the move that put the issue at planning, looking through
// the planner's rounds and an owner's hold, but not through a move out of
// held that's an entry of its own: an override, or a discussion's end.
func TestPlanEntry(t *testing.T) {
	t.Parallel()
	const entry = "from=spec-review to=planning cmd=r7 rev=1 body=abcd1234"
	for _, tt := range []struct {
		name  string
		moves []string
		want  int // the index in moves of the entry
	}{
		{"the approval", []string{entry}, 0},
		{"after a round", []string{entry, "from=planning to=planning run=p1 round=1 questions=1 seen=1"}, 0},
		{"after an owner's hold", []string{entry, "from=planning to=held at=planning reason=owner cmd=9 seen=1", "from=held to=planning cmd=10 seen=1"}, 0},
		{"after a discussion", []string{entry, "from=clarifying to=planning run=iv origin=planning seen=1"}, 1},
		{"after an override out of held", []string{entry, "from=planning to=held at=planning reason=owner cmd=9 seen=1", "from=held to=planning by=owner event=5 seen=1"}, 2},
		{"after a resume to another stage", []string{"from=clarifying to=spec-review run=a seen=1", "from=spec-review to=held at=spec-review reason=owner cmd=9 seen=1",
			"from=held to=planning cmd=10 seen=1"}, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var ms []github.Marker
			for _, m := range tt.moves {
				ms = append(ms, github.ParseMarkers("<!-- issuedog:transition "+m+" -->")...)
			}
			got := planEntry(state.Issue{Markers: ms})
			if want := ms[tt.want]; got.Kind != "transition" || got.Fields["from"] != want.Fields["from"] || got.Fields["to"] != want.Fields["to"] ||
				got.Fields["origin"] != want.Fields["origin"] || got.Fields["by"] != want.Fields["by"] || got.Fields["cmd"] != want.Fields["cmd"] {
				t.Errorf("planEntry = %+v, want %+v", got, want)
			}
		})
	}
}
