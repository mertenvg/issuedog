package daemon

import (
	"context"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// childOrigins reads a sub-issue's body history only for one the App
// filed whose origin isn't read, and whose body's markers aren't read
// (originLost, #347): it takes the origin from the newest version the App
// wrote that has one, back to the original post, and never from a
// version anyone else wrote. A sub-issue whose origin is read, from a
// clean App comment, costs no history, nor does a person's issue, whose
// original post the App didn't write, nor an App issue whose body is
// read and has none. The step trusts nobody, so every edit here is dirty,
// and Dirty reads no history of its own. A history read is kept, so a
// second call reads none.
func TestChildOriginsHistory(t *testing.T) {
	t.Parallel()
	const app = "issuedog[bot]"
	item := func(id string) string { return "<!-- issuedog:origin plan=#1 rev=1 item=" + id + " -->" }
	for _, tt := range []struct {
		name      string
		add       func(f *githubtest.Fake) int
		item      string // "" for none
		fromBody  bool   // whether it's read from a version of the body
		histories int
	}{
		{"a repost, the body edited", func(f *githubtest.Fake) int {
			n := f.AddIssue(app, "T1", "The task.\n\n"+item("T1"))
			f.EditBody(n, "owner", "Edited.")
			f.AddComment(n, app, item("T1"))
			return n
		}, "T1", false, 0},
		{"no repost yet, the App's original post", func(f *githubtest.Fake) int {
			n := f.AddIssue(app, "T1", "The task.\n\n"+item("T1"))
			f.EditBody(n, "owner", "Edited.")
			return n
		}, "T1", true, 1},
		{"no repost yet, the App's rewrite over the original post", func(f *githubtest.Fake) int {
			n := f.AddIssue(app, "T1", "The task.\n\n"+item("T1"))
			f.EditBody(n, app, "The task, rewritten.\n\n"+item("T3"))
			f.EditBody(n, "owner", "Edited.")
			return n
		}, "T3", true, 1},
		{"mallory's marker in an earlier edit", func(f *githubtest.Fake) int {
			n := f.AddIssue(app, "T1", "The task.")
			f.EditBody(n, "mallory", item("T2"))
			f.EditBody(n, "owner", "Edited.")
			return n
		}, "", false, 1},
		{"a person's issue, its original post's marker", func(f *githubtest.Fake) int {
			n := f.AddIssue("owner", "T4", "The task.\n\n"+item("T4"))
			f.EditBody(n, "mallory", "Edited.")
			return n
		}, "", false, 0},
		{"the App's issue, its body unedited with none", func(f *githubtest.Fake) int {
			return f.AddIssue(app, "T1", "The task.")
		}, "", false, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := githubtest.New(t, "o/r", "issuedog")
			f.AddIssue("owner", "Parent", "The plan.")
			n := tt.add(f)
			f.SetParent(n, 1)
			ctx := context.Background()
			st := &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}}
			subs, err := st.Client.SubIssues(ctx, "o", "r", 1)
			if err != nil {
				t.Fatal(err)
			}
			before := f.HistoryQueries()
			got, err := st.childOrigins(ctx, subs)
			if err != nil {
				t.Fatal(err)
			}
			if q := f.HistoryQueries() - before; q != tt.histories {
				t.Errorf("childOrigins sent %d history queries; want %d", q, tt.histories)
			}
			// The history read is kept for the same edit (originLost), so a
			// second call, as a closed child's parent's next visit makes,
			// reads none (#501).
			before = f.HistoryQueries()
			if _, err := st.childOrigins(ctx, subs); err != nil {
				t.Fatal(err)
			}
			if q := f.HistoryQueries() - before; q != 0 {
				t.Errorf("a second childOrigins sent %d history queries; want none", q)
			}
			o, ok := got[n]
			if tt.item == "" {
				if ok {
					t.Errorf("#%d's origin = %+v; want none", n, o)
				}
				return
			}
			if !ok || o.Plan != 1 || o.Rev != 1 || o.Item != tt.item || (o.Comment == state.BodyID) != tt.fromBody {
				t.Errorf("#%d's origin = %+v, %t; want item %s of #1's plan, from a version of the body %t", n, o, ok, tt.item, tt.fromBody)
			}
		})
	}
}
