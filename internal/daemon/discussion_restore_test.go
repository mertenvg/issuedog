package daemon_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// discHeldAgain is discHeld for a restored hold of an issue that was held
// before the act, with its sentence, its owner hold reposted and its
// notice.
func discHeldAgain(at, what, fields, owner, key string) string {
	return fmt.Sprintf("On hold for discussion, from `%s`: %s. Only a code owner can move this issue, "+
		"so it stays on hold until they resolve this. It was on hold already, so it also stays on hold until `/resume` here."+
		"\n\ncc @owner\n\n<!-- issuedog:transition %s -->\n<!-- issuedog:transition %s -->\n%s", at, what, fields, owner, discNoticeMarker(key))
}

// discTwoMoves is discFake's o/r with #1 the owner's task moved to
// clarifying in comment 1001, then to ready in comment 1002, each with
// the App's labels after its marker.
func discTwoMoves(t testing.TB) *githubtest.Fake {
	f := githubtest.New(t, "o/r", slug)
	f.SetPermission("owner", "admin")
	f.SetPermission("mallory", "write")
	n := f.AddIssue("owner", "Issue", "body")
	f.SetType(n, "task")
	f.AddComment(n, app, "<!-- issuedog:transition from=intake to=clarifying run=a -->")
	f.AddLabel(n, app, "stage:clarifying")
	f.AddComment(n, app, "<!-- issuedog:transition from=clarifying to=ready run=b -->")
	f.AddLabel(n, app, "stage:ready")
	f.RemoveLabel(n, app, "stage:clarifying")
	return f
}

// discHeldBy is discFake with #1 held: by the owner's /hold, comment 1002,
// whose hold is comment 1003, or else by mallory's comment 1002, whose
// hold is comment 1003.
func discHeldBy(owner bool) func(t testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		if owner {
			f.AddComment(1, "owner", "/hold")
		} else {
			f.AddComment(1, "mallory", "Wait.")
		}
		if err := discPass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		f.ForgetWrites()
		return f
	}
}

// discDeleted is the node ID of the issue's newest deletion.
func discDeleted(t testing.TB, f *githubtest.Fake) string {
	t.Helper()
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Deleted) == 0 {
		t.Fatal("no deletion")
	}
	return s.Deleted[len(s.Deleted)-1].ID
}

// A write collaborator's edit or deletion of the App's comment that holds
// the move setting the issue's stage holds the issue at that stage, as its
// label shows: no label moves but held's, and no disagreement is posted.
// An owner hold it hid is reposted, and the first hold's at kept.
func TestDiscRestore(t *testing.T) {
	type act struct {
		name string
		do   func(f *githubtest.Fake, id int64)
		key  func(t testing.TB, f *githubtest.Fake, id int64) string
		what string
	}
	acts := []act{
		{"an edit", func(f *githubtest.Fake, id int64) { f.EditComment(id, "mallory", "nothing here") },
			func(t testing.TB, f *githubtest.Fake, id int64) string { return discEditKey(t, f, 1, id) }, "`mallory` edited comment %d by `issuedog[bot]`"},
		{"a deletion", func(f *githubtest.Fake, id int64) { f.DeleteCommentBy("mallory", id) },
			func(t testing.TB, f *githubtest.Fake, _ int64) string { return "d" + discDeleted(t, f) }, "`mallory` deleted a comment by `issuedog[bot]`"},
	}
	for _, a := range acts {
		what := func(id int64) string {
			if strings.Contains(a.what, "%d") {
				return fmt.Sprintf(a.what, id)
			}
			return a.what
		}
		t.Run(a.name+" of the only move", func(t *testing.T) {
			setup := discWith(func(_ testing.TB, f *githubtest.Fake) { a.do(f, 1001) })
			discCheck(t, setup, 1, func(f *githubtest.Fake) map[int]issueWant {
				key := a.key(t, f, 1001)
				return map[int]issueWant{1: {[]string{"stage:held"}, []string{discHeld("ready", what(1001),
					"from=ready to=held at=ready origin=ready reason=discussion event="+key, discNoticeMarker(key))}}}
			})
		})
		t.Run(a.name+" of the latest move", func(t *testing.T) {
			setup := func(t testing.TB) *githubtest.Fake {
				f := discTwoMoves(t)
				a.do(f, 1002)
				return f
			}
			discCheck(t, setup, 1, func(f *githubtest.Fake) map[int]issueWant {
				key := a.key(t, f, 1002)
				return map[int]issueWant{1: {[]string{"stage:held"}, []string{discHeld("ready", what(1002),
					"from=ready to=held at=ready origin=ready reason=discussion event="+key+" seen=1001", discNoticeMarker(key))}}}
			})
		})
		for _, owner := range []bool{true, false} {
			name := " of the owner's hold"
			if !owner {
				name = " of a discussion hold"
			}
			t.Run(a.name+name, func(t *testing.T) {
				setup := func(t testing.TB) *githubtest.Fake {
					f := discHeldBy(owner)(t)
					a.do(f, 1003)
					return f
				}
				discCheck(t, setup, 1, func(f *githubtest.Fake) map[int]issueWant {
					key := a.key(t, f, 1003)
					return map[int]issueWant{1: {[]string{"stage:held"}, []string{discHeldAgain("ready", what(1003),
						"from=held to=held at=ready origin=ready reason=discussion event="+key+" seen=1001",
						"from=held to=held at=ready reason=owner event="+key+" seen=1001", key)}}}
				})
			})
		}
	}
}

// discRestore holds nothing, and the label rules act as before, when the
// owner set a stage by hand after the act, which wins; when the App's own
// write was cut short, which is finished; when the issue has two stage
// labels, which is a disagreement; and when the label and the markers left
// agree, as on a held issue whose App note was edited, which gets only its
// notice.
func TestDiscRestoreLeftAlone(t *testing.T) {
	t.Run("an override", func(t *testing.T) {
		f := run(t, discWith(func(_ testing.TB, f *githubtest.Fake) {
			f.EditComment(1001, "mallory", "nothing here")
			f.RemoveLabel(1, "owner", "stage:ready")
			f.AddLabel(1, "owner", "stage:clarifying")
		}), discPass(t, 1))
		is := f.State().Issues[0]
		if last := is.Comments[len(is.Comments)-1].Body; !slices.Equal(is.Labels, []string{"stage:clarifying"}) || !strings.Contains(last, "owner override") {
			t.Errorf("labels %q, newest comment %q; want the owner's override recorded, and no hold", is.Labels, last)
		}
	})
	t.Run("an unfinished write", func(t *testing.T) {
		setup := discWith(func(_ testing.TB, f *githubtest.Fake) {
			f.AddComment(1, app, "<!-- issuedog:transition from=ready to=held at=ready reason=owner cmd=9 seen=1001 -->") // 1002, labels not set
			f.EditComment(1001, "mallory", "nothing here")
		})
		discCheck(t, setup, 1, func(f *githubtest.Fake) map[int]issueWant {
			key := discEditKey(t, f, 1, 1001)
			return map[int]issueWant{1: {[]string{"stage:held"}, []string{discNotice("`mallory` edited comment 1001 by `issuedog[bot]`", key)}}}
		})
	})
	t.Run("two stage labels", func(t *testing.T) {
		f := run(t, discWith(func(_ testing.TB, f *githubtest.Fake) {
			f.RemoveLabel(1, app, "stage:ready")
			f.AddLabel(1, app, "stage:clarifying")
			f.AddLabel(1, app, "stage:ready")
			f.EditComment(f.AddComment(1, app, "A note."), "mallory", "A note, changed.")
		}), discPass(t, 1))
		is := f.State().Issues[0]
		var bodies []string
		for _, c := range is.Comments {
			bodies = append(bodies, c.Body)
		}
		if !slices.ContainsFunc(bodies, func(b string) bool { return strings.Contains(b, "kind=disagreement") }) ||
			!strings.HasPrefix(bodies[len(bodies)-1], "On hold for discussion, from `ready`") || !slices.Equal(is.Labels, []string{"stage:held"}) {
			t.Errorf("labels %q, comments %q; want the disagreement reported, then a hold from ready", is.Labels, bodies)
		}
	})
	t.Run("a held issue's note", func(t *testing.T) {
		setup := func(t testing.TB) *githubtest.Fake {
			f := discHeldBy(true)(t)
			f.EditComment(f.AddComment(1, app, "A note."), "mallory", "A note, changed.")
			return f
		}
		discCheck(t, setup, 1, func(f *githubtest.Fake) map[int]issueWant {
			key := discEditKey(t, f, 1, 1005)
			return map[int]issueWant{1: {[]string{"stage:held"}, []string{discNotice("`mallory` edited comment 1005 by `issuedog[bot]`", key)}}}
		})
	})
}

// Through the loop, with the trust CODEOWNERS gives, on an issue the owner
// held: a question of trust alone, about someone the snapshot gives no
// association for, costs no permission request. Neither eve's deleted
// comment nor her 👍 on mallory's comment, which mallory edits later,
// makes an unchanged pass ask about her.
func TestDiscStrangerAsksNothingThroughTheLoop(t *testing.T) {
	for name, act := range map[string]func(f *githubtest.Fake, n int){
		"a deletion of her comment": func(f *githubtest.Fake, n int) { f.DeleteCommentBy("mallory", f.AddComment(n, "eve", "Spam.")) },
		"her 👍 on an edited comment": func(f *githubtest.Fake, n int) {
			id := f.AddComment(n, "mallory", "Wait.")
			f.AddReaction(id, "eve", "+1")
			f.EditComment(id, "mallory", "Wait, please.")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			n := fileAt(f, "ready", "do it")
			f.AddComment(n, "owner", "/hold")
			d := newDaemon(t, f, newOrigin(t), nil)
			iterate(t, d)
			act(f, n)
			iterate(t, d)
			iterate(t, d) // reads the App's writes back
			from := len(f.Requests())
			iterate(t, d)
			for _, r := range f.Requests()[from:] {
				if strings.Contains(r.Path, "/collaborators/eve/") || r.Method != "GET" || !r.Conditional || r.Status != 304 {
					t.Errorf("the unchanged pass sent %+v; want only conditional GETs answered 304, none about eve", r)
				}
			}
			if l := f.State().Issues[n-1].Labels; !slices.Equal(l, []string{"stage:held"}) {
				t.Errorf("labels %q; want the issue still held", l)
			}
		})
	}
}

// The restore runs where the label rules do with the issue's dirty
// content: a run's apply holds the issue at the stage an edit of the App's
// move hid during the run, with no disagreement, and drops the result.
func TestDiscRestoreAtApply(t *testing.T) {
	f := newFake(t)
	answer := interview(f) // the move to clarifying is comment 1001
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.claude(okOut, 0)
	g.stage.during = func() {
		g.stage.during = nil
		f.EditComment(1001, "mallory", "Questions, changed.")
	}
	g.pass()
	is := f.State().Issues[0]
	if specApplied(f, 1, "run1", answer) || !slices.Equal(is.Labels, []string{"stage:held"}) ||
		!strings.HasPrefix(newestComment(f, 1), "On hold for discussion, from `clarifying`") {
		t.Errorf("labels %q, newest comment %q; want run1 dropped, and the issue held from clarifying", is.Labels, newestComment(f, 1))
	}
	for _, c := range is.Comments {
		if strings.Contains(c.Body, "kind=disagreement") {
			t.Errorf("comment %q; want no disagreement", c.Body)
		}
	}
}

// A command's moves of other issues restore them too: a /hold on the
// parent finds its child held at the stage an edit of the child's App move
// hid, and doesn't hold it again from the stage the markers left.
func TestDiscRestoreOnFamily(t *testing.T) {
	setup := func(t testing.TB) *githubtest.Fake {
		f := githubtest.New(t, "o/r", slug)
		f.SetPermission("owner", "admin")
		f.SetPermission("mallory", "write")
		file(f, 0, ready) // #1, comment 1001
		file(f, 1, ready) // #2, comment 1002
		f.EditComment(1002, "mallory", "nothing here")
		f.AddComment(1, "owner", "/hold")
		return f
	}
	githubtest.Replay(t, setup, discPass(t, 1))
	f := run(t, setup, discPass(t, 1))
	child := f.State().Issues[1]
	var bodies []string
	for _, c := range child.Comments[1:] {
		bodies = append(bodies, c.Body)
	}
	if !slices.Equal(child.Labels, []string{"stage:held"}) || len(bodies) != 1 || !strings.HasPrefix(bodies[0], "On hold for discussion, from `ready`") {
		t.Errorf("#2: labels %q, new comments %q; want only the discussion hold from ready", child.Labels, bodies)
	}
}

// An issue the owner brought in by label is bounded by the label's time:
// a comment between the label and the App's record of it holds the issue.
func TestDiscAfterAdoptingLabel(t *testing.T) {
	setup := func(t testing.TB) *githubtest.Fake {
		f := githubtest.New(t, "o/r", slug)
		f.SetPermission("owner", "admin")
		f.SetPermission("mallory", "write")
		n := f.AddIssue("owner", "Issue", "body")
		f.SetType(n, "task")
		f.AddLabel(n, "owner", "stage:ready")
		f.AddComment(n, "mallory", "Wait.") // 1001
		return f
	}
	githubtest.Replay(t, setup, discPass(t, 1))
	f := run(t, setup, discPass(t, 1))
	is := f.State().Issues[0]
	if !slices.Equal(is.Labels, []string{"stage:held"}) || !strings.Contains(newestComment(f, 1), "reason=discussion event=c1001 ") {
		t.Errorf("labels %q, newest comment %q; want the adoption recorded, then a hold keyed c1001", is.Labels, newestComment(f, 1))
	}
}

// After the owner's hold is restored, its /resume ends that hold but the
// issue stays held while the act is open; and a parent's /resume doesn't
// end the reposted hold, which only /resume on the issue itself ends.
func TestDiscRestoredHoldStays(t *testing.T) {
	f := discHeldBy(true)(t)
	f.EditComment(1003, "mallory", "nothing here")
	if err := discPass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.AddComment(1, "owner", "/resume")
	if err := discPass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	is := f.State().Issues[0]
	last := is.Comments[len(is.Comments)-1].Body
	if !slices.Equal(is.Labels, []string{"stage:held"}) || !strings.Contains(last, "#1 also on hold for discussion") {
		t.Errorf("labels %q, newest comment %q; want #1 still held, the /resume rejected for the discussion", is.Labels, last)
	}
	for _, c := range is.Comments {
		if strings.Contains(c.Body, "kind=disagreement") {
			t.Errorf("comment %q; want no disagreement", c.Body)
		}
	}
}

// An act between the owner's hand-set stage label, which ends a discussion
// hold, and the App's record of it, holds the issue again: the triggers are
// bounded by the label's event, not the record, comparing comments by
// when they were posted. The act that held it before stays resolved.
func TestDiscAfterHandSetLabel(t *testing.T) {
	for name, act := range map[string]func(f *githubtest.Fake) string{
		"a comment": func(f *githubtest.Fake) string { return fmt.Sprintf("c%d", f.AddComment(1, "mallory", "Wait again.")) },
		"a rename":  func(f *githubtest.Fake) string { return fmt.Sprint(f.RenameIssue(1, "mallory", "Wait again")) },
	} {
		t.Run(name, func(t *testing.T) {
			var key string
			setup := func(t testing.TB) *githubtest.Fake {
				f := discHeldBy(false)(t)
				f.RemoveLabel(1, "owner", "stage:held")
				f.AddLabel(1, "owner", "stage:ready")
				key = act(f)
				return f
			}
			githubtest.Replay(t, setup, discPass(t, 1))
			f := run(t, setup, discPass(t, 1))
			is := f.State().Issues[0]
			last := is.Comments[len(is.Comments)-1].Body
			if !slices.Equal(is.Labels, []string{"stage:held"}) || !strings.Contains(last, "reason=discussion event="+key+" ") ||
				!strings.Contains(is.Comments[len(is.Comments)-2].Body, "recorded it as an owner override") {
				t.Errorf("labels %q, newest comments %q; want the override recorded, then a hold keyed %s", is.Labels, is.Comments[len(is.Comments)-2:], key)
			}
		})
	}
}

// A deletion of a comment GitHub names no author of holds the issue,
// failing closed, and gets its notice.
func TestDiscDeletionOfUnnamed(t *testing.T) {
	var node string
	setup := discWith(func(_ testing.TB, f *githubtest.Fake) {
		node = f.DeleteCommentBy("mallory", f.AddComment(1, "", "A ghost's note."))
	})
	discCheck(t, setup, 1, func(*githubtest.Fake) map[int]issueWant {
		return map[int]issueWant{1: {[]string{"stage:held"}, []string{discHeld("ready", "`mallory` deleted a comment by someone GitHub doesn't name",
			"from=ready to=held at=ready origin=ready reason=discussion event=d"+node+" seen=1001", discNoticeMarker("d"+node))}}}
	})
}

// On an issue the owner held, a standalone notice deleted or made dirty
// isn't posted again, and a deletion of a comment GitHub names no author
// of, after a trigger, may have been its notice, so it isn't posted.
func TestDiscLostStandaloneNotice(t *testing.T) {
	for name, lose := range map[string]func(f *githubtest.Fake, id int64){
		"deleted": func(f *githubtest.Fake, id int64) { f.DeleteCommentBy("owner", id) },
		"dirty":   func(f *githubtest.Fake, id int64) { f.EditComment(id, "eve", "nothing here") },
	} {
		t.Run(name, func(t *testing.T) {
			f := discHeldBy(true)(t)
			ev := f.RenameIssue(1, "mallory", "Wait")
			if err := discPass(t, 1)(f); err != nil {
				t.Fatal(err)
			}
			ids := commentIDs(t, f, 1)
			if cs := f.State().Issues[0].Comments; cs[len(cs)-1].Body != discNotice("`mallory` renamed this issue to “Wait”", fmt.Sprint(ev)) {
				t.Fatalf("newest comment %q; want the rename's notice", cs[len(cs)-1].Body)
			}
			lose(f, ids[len(ids)-1])
			writes := len(f.Writes())
			for range 2 {
				if err := discPass(t, 1)(f); err != nil {
					t.Fatal(err)
				}
			}
			for _, w := range f.Writes()[writes:] {
				if w.Method == "POST" && strings.Contains(w.Body, "issuedog:notice") {
					t.Errorf("wrote %+v; want no second notice", w)
				}
			}
			if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:held"}) {
				t.Errorf("labels %q; want the issue still held", l)
			}
		})
	}
	t.Run("an unnamed author's deletion after the trigger", func(t *testing.T) {
		f := discHeldBy(true)(t)
		ev := f.RenameIssue(1, "mallory", "Wait")
		f.DeleteCommentBy("owner", f.AddComment(1, "", "A ghost's note."))
		writes := len(f.Writes())
		if err := discPass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		for _, w := range f.Writes()[writes:] {
			if strings.Contains(w.Body, "notice key="+fmt.Sprint(ev)) {
				t.Errorf("wrote %+v; want no notice that may be a second one", w)
			}
		}
	})
}

// A move the table lacks, as for a type the pipeline doesn't know, holds
// nothing: the type error stands for it, and a second pass writes nothing.
func TestDiscStateErrorHoldsNothing(t *testing.T) {
	setup := func(t testing.TB) *githubtest.Fake {
		f := githubtest.New(t, "o/r", slug)
		f.SetPermission("owner", "admin")
		f.SetPermission("mallory", "write")
		n := f.AddIssue("owner", "Issue", "body")
		f.SetType(n, "Feature")
		f.AddLabel(n, app, "stage:clarifying")
		f.AddComment(n, app, "<!-- issuedog:transition from=intake to=clarifying run=a -->")
		f.AddComment(n, "mallory", "Wait.")
		return f
	}
	githubtest.Replay(t, setup, discPass(t, 1))
	f := run(t, setup, discPass(t, 1))
	is := f.State().Issues[0]
	if !slices.Equal(is.Labels, []string{"stage:clarifying"}) {
		t.Errorf("labels %q; want the issue left at clarifying", is.Labels)
	}
	for _, c := range is.Comments {
		if strings.Contains(c.Body, "reason=discussion") {
			t.Errorf("comment %q; want no hold", c.Body)
		}
	}
}
