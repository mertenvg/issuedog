package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/issuedog/internal/state"
)

// originFromHistory takes the whole origin marker of the newest version
// the App wrote that has one, back to the original post, which counts
// whoever GitHub names as its author. Versions by anyone else, deleted
// ones, the App's escaped text and markers that aren't whole count for
// nothing. Every field is kept as written.
func TestOriginFromHistory(t *testing.T) {
	t.Parallel()
	const (
		rev1  = "<!-- issuedog:origin plan=#195 rev=1 item=T2 -->"
		rev2  = "<!-- issuedog:origin plan=#195 rev=2 item=T2 -->"
		check = "<!-- issuedog:origin plan=#195 rev=1 item=T9 check=label human=1 value=a%20b extra=x -->"
	)
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	owner := github.Author{Login: "owner", Type: "User"}
	at := func(s int64) time.Time { return time.Unix(s, 0) }
	e := func(by github.Author, s int64, text string) github.Edit {
		return github.Edit{By: by, At: at(s), Text: text}
	}
	for _, tt := range []struct {
		name string
		h    []github.Edit
		want string // "" for none
	}{
		{"the original post", []github.Edit{e(app, 1, "a\n\n"+rev1), e(owner, 2, "b")}, rev1},
		{"the App's rewrite over the original post", []github.Edit{e(app, 1, rev1), e(app, 2, rev2), e(owner, 3, "b")}, rev2},
		{"the App's rewrite deleted", []github.Edit{e(app, 1, rev1), e(app, 2, ""), e(owner, 3, "b")}, rev1},
		{"the owner's version that keeps a marker", []github.Edit{e(app, 1, rev1), e(owner, 2, rev2)}, rev1},
		{"the App's note, which escapes the text it keeps", []github.Edit{e(app, 1, rev1), e(owner, 2, rev2), e(app, 3, github.Escape(rev2))}, rev1},
		{"an original post GitHub names nobody for", []github.Edit{e(github.Author{}, 1, rev1), e(owner, 2, "b")}, rev1},
		{"every field kept", []github.Edit{e(app, 1, check), e(owner, 2, "b")}, check},
		{"a marker that isn't whole, then a whole one", []github.Edit{e(app, 1, "<!-- issuedog:origin plan=#195 item=T2 --> "+rev1), e(owner, 2, "b")}, rev1},
		{"the original post deleted", []github.Edit{e(app, 1, ""), e(owner, 2, "b")}, ""},
		{"only the owner's markers after the original post", []github.Edit{e(app, 1, "a"), e(owner, 2, rev1)}, ""},
		{"no history", nil, ""},
	} {
		m, ok := originFromHistory(tt.h, "issuedog")
		if tt.want == "" {
			if ok {
				t.Errorf("%s: marker %+v; want none", tt.name, m)
			}
			continue
		}
		got, err := github.FormatMarker(m)
		if !ok || err != nil {
			t.Errorf("%s: marker %+v, %t, %v; want %s", tt.name, m, ok, err, tt.want)
			continue
		}
		if want := github.ParseMarkers(tt.want)[0]; m.Kind != want.Kind || len(m.Fields) != len(want.Fields) {
			t.Errorf("%s: marker %s; want %s", tt.name, got, tt.want)
		}
		for k, v := range github.ParseMarkers(tt.want)[0].Fields {
			if m.Fields[k] != v {
				t.Errorf("%s: field %s = %q; want %q", tt.name, k, m.Fields[k], v)
			}
		}
	}
}

// A human task's reposted origin gives its check back whole, so the check
// /done runs (humanCheck) is the one the plan recorded, and it passes on a
// main that has the file with the text.
func TestOriginRepostCheckRuns(t *testing.T) {
	t.Parallel()
	const m = "<!-- issuedog:origin plan=#195 rev=1 item=T9 check=file contains=%3C!--%25 human=1 value=docs/a%20b.md -->"
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("issuedog[bot]", "T9. Turn it on", "The human task.\n\n"+m)
	f.EditBody(n, "owner", "The human task, put better.")
	ctx := context.Background()
	a := &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog",
		ReadMain: func(_ context.Context, path string) ([]byte, error) {
			if path != "docs/a b.md" {
				return nil, repo.ErrNotFound
			}
			return []byte("x <!--% y"), nil
		}}
	s, err := a.Client.Snapshot(ctx, "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	d, err := a.Dirty(ctx, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.originRepost(ctx, s, d); err != nil {
		t.Fatal(err)
	}
	o := state.Derive(s, a.AppSlug, d.Has).Origin
	if want := (state.Check{Kind: state.CheckFile, Value: "docs/a b.md", Contains: "<!--%"}); o.Check != want || !o.Human || o.Comment == state.BodyID {
		t.Fatalf("origin = %+v; want a human task's, from the repost, with check %+v", o, want)
	}
	if pass, found, err := a.humanCheck(ctx, o.Check); err != nil || !pass {
		t.Errorf("humanCheck = %t, %q, %v; want it to pass", pass, found, err)
	}
}

// childOrigins reads, from the snapshot, the Origin of each sub-issue the
// App filed whose listed body gives none: from a clean App comment, as a
// repost is (#291), and never from one someone else edited, which is
// dirty. A sub-issue whose listed body gives an Origin, or that isn't the
// App's, isn't read, so it isn't in the map.
func TestChildOrigins(t *testing.T) {
	t.Parallel()
	const app = "issuedog[bot]"
	item := func(id string) string { return "<!-- issuedog:origin plan=#1 rev=1 item=" + id + " -->" }
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("owner", "Parent", "The plan.")        // 1
	f.AddIssue(app, "T1", "Edited.")                  // 2: its origin reposted
	f.AddIssue(app, "T2", "Edited.")                  // 3: its repost edited by mallory
	f.AddIssue(app, "T3", "The task.\n\n"+item("T3")) // 4: the body's own
	f.AddIssue("owner", "T4", "Edited.")              // 5: not the App's
	f.AddComment(2, app, item("T1"))                  // a clean repost
	f.EditComment(f.AddComment(3, app, item("T2")), "mallory", item("T9"))
	f.AddComment(5, app, item("T4"))
	for n := 2; n <= 5; n++ {
		f.SetParent(n, 1)
	}
	ctx := context.Background()
	st := &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}}
	subs, err := st.Client.SubIssues(ctx, "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.childOrigins(ctx, subs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[2].Plan != 1 || got[2].Item != "T1" || got[2].Comment == state.BodyID {
		t.Errorf("childOrigins = %+v; want #2 alone, item T1 of #1's plan, from its comment", got)
	}
}
