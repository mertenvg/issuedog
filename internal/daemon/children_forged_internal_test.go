package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// childEditedOrigins reads, in one query, the body history of each issue
// the App filed whose listed body gives an origin, a sub-issue's, or a
// listed issue's that names the parent's plan, and gives an edited one
// the origin of the newest version of its body the App wrote, back to the
// original post, never one anyone else wrote (#501). An issue never
// edited gets no entry, so its listed body counts, and an edited one
// whose versions of the App's give none gets the zero Origin. A person's
// issue, and an unlinked issue naming another plan, cost no history. A
// history that reaches the listed body is kept.
func TestChildEditedOrigins(t *testing.T) {
	t.Parallel()
	const app = "issuedog[bot]"
	item := func(plan, id string) string {
		return "The task.\n\n<!-- issuedog:origin plan=#" + plan + " rev=1 item=" + id + " -->"
	}
	for _, tt := range []struct {
		name      string
		add       func(f *githubtest.Fake) int
		linked    bool
		entry     bool   // whether it gets an entry
		item      string // the entry's item, "" for the zero Origin
		histories int
	}{
		{"never edited", func(f *githubtest.Fake) int {
			return f.AddIssue(app, "T1", item("1", "T1"))
		}, true, false, "", 1},
		{"the owner's edit, keeping the marker", func(f *githubtest.Fake) int {
			n := f.AddIssue(app, "T1", item("1", "T1"))
			f.EditBody(n, "owner", "Put better.\n\n<!-- issuedog:origin plan=#1 rev=1 item=T1 -->")
			return n
		}, true, true, "T1", 1},
		{"mallory's marker for T2", func(f *githubtest.Fake) int {
			n := f.AddIssue(app, "T1", item("1", "T1"))
			f.EditBody(n, "mallory", item("1", "T2"))
			return n
		}, true, true, "T1", 1},
		{"the owner's marker for another plan", func(f *githubtest.Fake) int {
			n := f.AddIssue(app, "T1", item("1", "T1"))
			f.EditBody(n, "owner", item("9", "T1"))
			return n
		}, true, true, "T1", 1},
		{"the App's rewrite, then mallory's marker", func(f *githubtest.Fake) int {
			n := f.AddIssue(app, "T1", item("1", "T1"))
			f.EditBody(n, app, item("1", "T3"))
			f.EditBody(n, "mallory", item("1", "T2"))
			return n
		}, true, true, "T3", 1},
		{"mallory's marker, the original post's text deleted", func(f *githubtest.Fake) int {
			n := f.AddIssue(app, "T1", item("1", "T1"))
			f.EditBody(n, "mallory", item("1", "T2"))
			f.DeleteRevision(n, 0)
			return n
		}, true, true, "", 1},
		{"unlinked, mallory's marker for T2", func(f *githubtest.Fake) int {
			n := f.AddIssue(app, "T1", item("1", "T1"))
			f.EditBody(n, "mallory", item("1", "T2"))
			return n
		}, false, true, "T1", 1},
		{"unlinked, naming another plan", func(f *githubtest.Fake) int {
			n := f.AddIssue(app, "T1", item("9", "T1"))
			f.EditBody(n, "mallory", item("9", "T2"))
			return n
		}, false, false, "", 0},
		{"a person's issue", func(f *githubtest.Fake) int {
			n := f.AddIssue("owner", "T4", item("1", "T4"))
			f.EditBody(n, "mallory", item("1", "T2"))
			return n
		}, true, false, "", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := githubtest.New(t, "o/r", "issuedog")
			f.AddIssue("owner", "Parent", "The plan.")
			n := tt.add(f)
			if tt.linked {
				f.SetParent(n, 1)
			}
			ctx := context.Background()
			st := &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}}
			subs, err := st.Client.SubIssues(ctx, "o", "r", 1)
			if err != nil {
				t.Fatal(err)
			}
			listed, err := st.Client.IssuesSince(ctx, "o", "r", time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			before := f.HistoryQueries()
			got, err := st.childEditedOrigins(ctx, 1, subs, listed)
			if err != nil {
				t.Fatal(err)
			}
			if q := f.HistoryQueries() - before; q != tt.histories {
				t.Errorf("childEditedOrigins sent %d history queries; want %d", q, tt.histories)
			}
			o, ok := got[n]
			switch {
			case ok != tt.entry:
				t.Errorf("#%d's entry = %+v, %t; want an entry %t", n, o, ok, tt.entry)
			case tt.entry && tt.item == "" && o != (state.Origin{}):
				t.Errorf("#%d's origin = %+v; want the zero Origin", n, o)
			case tt.entry && tt.item != "" && (o.Plan != 1 || o.Rev != 1 || o.Item != tt.item || o.Comment != state.BodyID):
				t.Errorf("#%d's origin = %+v; want item %s of #1's plan, from a version of the body", n, o, tt.item)
			}
			var node string
			for _, is := range listed {
				if is.GetNumber() == n {
					node = is.GetNodeID()
				}
			}
			if _, kept := st.histories[node]; kept != tt.entry {
				t.Errorf("#%d's history kept %t; want %t", n, kept, tt.entry)
			}
		})
	}
}
