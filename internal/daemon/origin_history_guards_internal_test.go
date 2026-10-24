package daemon

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// This file tests the guards on the edit histories kept for an App-filed
// child's body (#549, from #501's verify): which ones MarkDirty extends,
// which ones childEditedOrigins and originLost keep, and the warning
// childEditedOrigins logs for a forged origin, once.

// historyGuardApp is the App's login in these tests.
const historyGuardApp = "issuedog[bot]"

// historyGuardBody is a child's body with the App's origin marker for
// item of plan #1, at rev.
func historyGuardBody(rev, item string) string {
	return "The task.\n\n<!-- issuedog:origin plan=#1 rev=" + rev + " item=" + item + " -->"
}

// historyGuardApplier is an Applier on f's repo, o/r.
func historyGuardApplier(t *testing.T, f *githubtest.Fake) *Applier {
	return &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}
}

// MarkDirty extends a kept history with its note only while that history
// is the body's as the note replaces it, by its last edit and its text,
// both. Here the owner's edit of T1's child, which kept its marker, made
// Dirty keep that history. Then the App's revision rewrote the body,
// naming revision 2, and mallory edited it: to take the marker out, or
// back to the owner's text, which the kept history matches but for its
// edit time. A history that matched the edit time alone, with other text,
// would be no more the noted body's. The history kept for the owner's
// edit lacks the rewrite: extended with the note, it would be taken for
// the noted body's, and originRepost would repost revision 1's marker
// from the original post, not revision 2's from the rewrite.
func TestMarkDirtyExtendsOnlyItsBodysHistory(t *testing.T) {
	t.Parallel()
	const owners = "The task, put better.\n\n<!-- issuedog:origin plan=#1 rev=1 item=T1 -->"
	for name, tt := range map[string]struct {
		mallorys string
		sameAt   bool // whether the kept history takes the time of mallory's edit
	}{
		"an earlier edit's history":           {"The task, without its marker.", false},
		"a later edit back to the kept text":  {owners, false},
		"the same edit time, with other text": {"The task, without its marker.", true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := githubtest.New(t, "o/r", "issuedog")
			f.AddIssue("owner", "Parent", "The plan.")
			n := f.AddIssue(historyGuardApp, "T1", historyGuardBody("1", "T1"))
			f.EditBody(n, "owner", owners)
			a := historyGuardApplier(t, f)
			s, err := a.Client.Snapshot(ctx, "o", "r", n)
			if err != nil {
				t.Fatal(err)
			}
			d, err := a.Dirty(ctx, s, ownerOnly{})
			if err != nil {
				t.Fatal(err)
			}
			if h, ok := a.histories[s.BodyNode]; len(d) != 0 || !ok || len(h.edits) != 2 {
				t.Fatalf("after the owner's edit, the dirt is %v and the kept history %+v, %t; want no dirt, and the history kept", d, h, ok)
			}

			f.EditBody(n, historyGuardApp, "<!-- issuedog:rewrite run=p2 -->\nThe task, revised.\n\n<!-- issuedog:origin plan=#1 rev=2 item=T1 -->")
			f.EditBody(n, "mallory", tt.mallorys)
			if s, err = a.Client.Snapshot(ctx, "o", "r", n); err != nil {
				t.Fatal(err)
			}
			if tt.sameAt {
				h := a.histories[s.BodyNode]
				h.at = s.BodyEdit.At
				a.histories[s.BodyNode] = h
			}
			if d, err = a.Dirty(ctx, s, ownerOnly{}); err != nil {
				t.Fatal(err)
			}
			if d[state.BodyID] != "mallory" {
				t.Fatalf("after mallory's edit, the dirt is %v; want the body dirty by mallory", d)
			}
			if err := a.MarkDirty(ctx, s, d); err != nil {
				t.Fatal(err)
			}
			if err := a.originRepost(ctx, s, d); err != nil {
				t.Fatal(err)
			}
			newest := s.Comments[len(s.Comments)-1].Body
			if want := "<!-- issuedog:origin plan=#1 rev=2 item=T1 -->"; !strings.HasSuffix(newest, "\n\n"+want) {
				t.Errorf("the repost is %q; want it to carry %s, from the App's rewrite", newest, want)
			}
		})
	}
}

// childEditedOrigins keeps a history only while its newest version is
// the body as listed. One whose newest version's text someone deleted
// doesn't show that body, so it isn't kept, and the child's dirty check
// reads it again, rather than take it for the listed body's. Its origin
// still comes from the App's original post.
func TestChildEditedOriginsNewestTextDeleted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("owner", "Parent", "The plan.")
	n := f.AddIssue(historyGuardApp, "T1", historyGuardBody("1", "T1"))
	f.EditBody(n, "owner", "The task, put better.\n\n<!-- issuedog:origin plan=#1 rev=1 item=T1 -->")
	f.DeleteRevision(n, 1)
	f.SetParent(n, 1)
	st := &Step{Applier: historyGuardApplier(t, f)}
	subs, err := st.Client.SubIssues(ctx, "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := st.Client.IssuesSince(ctx, "o", "r", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.childEditedOrigins(ctx, 1, subs, listed)
	if err != nil {
		t.Fatal(err)
	}
	if o := got[n]; o.Plan != 1 || o.Item != "T1" || o.Comment != state.BodyID {
		t.Errorf("#%d's origin = %+v; want item T1 of #1's plan, from the original post", n, o)
	}
	s, err := st.Client.Snapshot(ctx, "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	if _, kept := st.histories[s.BodyNode]; kept {
		t.Errorf("#%d's history is kept; want none kept, as its newest version isn't the listed body", n)
	}
	before := f.HistoryQueries()
	if _, err := st.Dirty(ctx, s, ownerOnly{}); err != nil {
		t.Fatal(err)
	}
	if q := f.HistoryQueries() - before; q != 1 {
		t.Errorf("the dirty check sent %d history queries; want 1, as no history was kept for the body", q)
	}
}

// originLost keeps the history it reads only when that history reaches
// the body's last edit, so the next call for the same edit reads none. A
// history behind the edit the snapshot shows, as when GitHub's history
// lags it, isn't kept, so the next call reads it again, and finds the
// edit once GitHub shows it.
func TestOriginLostKeepsOnlyReachingHistory(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		lag     time.Duration
		queries []int // by call
	}{
		"reaching the last edit": {0, []int{1, 0}},
		"behind the last edit":   {time.Minute, []int{1, 1}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := githubtest.New(t, "o/r", "issuedog")
			f.AddIssue("owner", "Parent", "The plan.")
			n := f.AddIssue(historyGuardApp, "T1", historyGuardBody("1", "T1"))
			f.EditBody(n, "mallory", "The task, without its marker.")
			a := historyGuardApplier(t, f)
			s, err := a.Client.Snapshot(ctx, "o", "r", n)
			if err != nil {
				t.Fatal(err)
			}
			s.BodyEdit.At = s.BodyEdit.At.Add(tt.lag)
			for call, want := range tt.queries {
				before := f.HistoryQueries()
				m, ok, err := a.originLost(ctx, s, Dirt{})
				if err != nil {
					t.Fatal(err)
				}
				if o, whole := state.OriginOf(m); !ok || !whole || o.Item != "T1" {
					t.Errorf("call %d: originLost = %+v, %t; want T1's marker", call, m, ok)
				}
				if q := f.HistoryQueries() - before; q != want {
					t.Errorf("call %d sent %d history queries; want %d", call, q, want)
				}
			}
		})
	}
}

// childEditedOrigins warns of a child's forged origin once while its
// listed body stays the same, not on each visit before the record stands:
// a marker naming another item, and a body with no version of the App's
// that has one. A trusted user's edit is never noted dirty, so its body
// stays as it is from visit to visit. A new edit is warned of again.
func TestChildForgedWarnOnce(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logr.AddWriter).
	ctx := context.Background()
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("owner", "Parent", "The plan.")
	t1 := f.AddIssue(historyGuardApp, "T1", historyGuardBody("1", "T1"))
	f.EditBody(t1, "owner", "The task, put better.\n\n<!-- issuedog:origin plan=#1 rev=1 item=T2 -->")
	f.SetParent(t1, 1)
	t3 := f.AddIssue(historyGuardApp, "T3", historyGuardBody("1", "T3"))
	f.EditBody(t3, "owner", "Another task, put better.\n\n<!-- issuedog:origin plan=#1 rev=1 item=T3 -->")
	f.DeleteRevision(t3, 0)
	f.SetParent(t3, 1)
	st := &Step{Applier: historyGuardApplier(t, f)}
	visit := func() {
		t.Helper()
		subs, err := st.Client.SubIssues(ctx, "o", "r", 1)
		if err != nil {
			t.Fatal(err)
		}
		listed, err := st.Client.IssuesSince(ctx, "o", "r", time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.childEditedOrigins(ctx, 1, subs, listed); err != nil {
			t.Fatal(err)
		}
	}
	const forged = `o/r#2: its description names item "T2" of #1's plan, but the App filed it for item T1 of #1's`
	const none = `o/r#3: its description names item "T3" of #1's plan, but no version of it the App wrote has an origin marker`
	var warnings bytes.Buffer
	t.Cleanup(logr.AddWriter(&warnings, logr.WithFilter(logr.W)))
	visit()
	visit()
	logr.Wait()
	if got := warnings.String(); strings.Count(got, forged) != 1 || strings.Count(got, none) != 1 {
		t.Errorf("after two visits, warnings = %q; want one each of %q and %q", got, forged, none)
	}
	f.EditBody(t1, "owner", "The task, put better again.\n\n<!-- issuedog:origin plan=#1 rev=1 item=T2 -->")
	visit()
	logr.Wait()
	if got := warnings.String(); strings.Count(got, forged) != 2 || strings.Count(got, none) != 1 {
		t.Errorf("after a new edit of #2, warnings = %q; want a second %q, and still one %q", got, forged, none)
	}
}
