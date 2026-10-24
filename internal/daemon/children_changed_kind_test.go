package daemon_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// changedKindRevision is changedStoryRevision with the item at index i of
// its items replaced by from, the item at index j of story-plan, taken as
// item id, changed and retitled title, with no dependencies: so a task
// becomes a human task, or a human task a task.
func changedKindRevision(t testing.TB, i int, id string, j int, title string) map[string]any {
	t.Helper()
	out := changedStoryRevision(t)
	from := plItem(t, plGolden(t, "story-plan"), j)
	from["id"], from["status"], from["title"], from["depends_on"] = id, "changed", title, []any{}
	plan, ok := out["plan"].(map[string]any)
	if !ok {
		t.Fatalf("output %v has no plan", out)
	}
	items, ok := plan["items"].([]any)
	if !ok || i >= len(items) {
		t.Fatalf("plan %v has no item %d", plan, i)
	}
	items[i] = from
	return out
}

// A changed child whose body someone made dirty, and whose item the
// revision turns into the other kind of task, is rewritten whole, which
// cleans the body (SPEC §5.1), and the rest of the pass reads its class
// from the rewrite, not from the dirty body's stand-in: the origin the App
// reposted, which names the old kind (SPEC §6.4). So T2's child, a task
// at in-progress, goes to needs-human as the human task it now is, and
// T3's, a human task at needs-human, goes to ready as a task. A crash at
// any write, before or after it applies, writes nothing twice.
func TestChildrenChangedDirtyKind(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		n      func(changedIDs) int
		origin string // the App's repost of the child's revision 1 origin
		data   func(testing.TB) map[string]any
		from   state.Stage
		to     state.Stage
		class  state.Class
		suffix string // the end of the rewritten body's origin marker
	}{
		"a task becomes a human task": {
			n:      func(r changedIDs) int { return r.t2 },
			origin: "<!-- issuedog:origin plan=#1 rev=1 item=T2 -->",
			data: func(t testing.TB) map[string]any {
				return changedKindRevision(t, 1, "T2", 2, "Turn on expired-quote refusals in production")
			},
			from: state.InProgress, to: state.NeedsHuman, class: state.ClassHumanTask,
			suffix: " human=1 value=deploy/production.env -->",
		},
		"a human task becomes a task": {
			n:      func(r changedIDs) int { return r.t3 },
			origin: "<!-- issuedog:origin plan=#1 rev=1 item=T3 human=1 -->",
			data: func(t testing.TB) map[string]any {
				return changedKindRevision(t, 2, "T3", 1, "Default the quote lifetime in the booking service")
			},
			from: state.NeedsHuman, to: state.Ready, class: state.ClassTask,
			suffix: " item=T3 -->",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var n int
			setup := func(t testing.TB) *githubtest.Fake {
				f := newFake(t)
				changedStoryWith(t, f, false, func(f *githubtest.Fake, r changedIDs) {
					n = tt.n(r)
					f.EditBody(n, "eve", "The task, rewritten by eve.")
					f.AddComment(n, app, originRepost(tt.origin))
				}, changedPlanData(t, tt.data(t)))
				return f
			}
			githubtest.Replay(t, setup, childrenPass(t, revisionPlan))
			f := run(t, setup, childrenPass(t, revisionPlan))
			p, _ := changedApproval(t, f)

			c := f.Client(t)
			s, err := c.Snapshot(context.Background(), "o", "r", n)
			if err != nil {
				t.Fatal(err)
			}
			a := &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}
			d, err := a.Dirty(context.Background(), s, trusted{"owner": true})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(s.Body, "<!-- issuedog:rewrite run=p2 replaced=eve -->\n") || !strings.HasSuffix(s.Body, tt.suffix) || d.Has(state.BodyID) {
				t.Errorf("#%d's body, dirty %t, is\n%s\nwant the App's clean rewrite, naming eve's edit, whose origin ends %q", n, d.Has(state.BodyID), s.Body, tt.suffix)
			}
			is := state.Derive(s, slug, d.Has)
			if class, err := state.ClassOf(is.Item); err != nil || class != tt.class || is.Stage != tt.to {
				t.Errorf("#%d is class %v (%v) at %s, want class %v at %s", n, class, err, is.Stage, tt.class, tt.to)
			}
			to, _ := tt.to.Label()
			if labels := f.State().Issues[n-1].Labels; !slices.Contains(labels, to) {
				t.Errorf("#%d is labelled %v, want %s", n, labels, to)
			}
			m := changedLastMarker(t, f, n)
			if want := fmt.Sprintf("p%d", p); m.Fields["from"] != string(tt.from) || m.Fields["to"] != string(tt.to) || m.Fields["event"] != want {
				t.Errorf("#%d's newest marker has %v, want %s → %s, keyed event=%s", n, m.Fields, tt.from, tt.to, want)
			}
			for _, e := range errorMarkers(f, n) {
				t.Errorf("#%d has the state error %q, want none", n, e)
			}
		})
	}
}
