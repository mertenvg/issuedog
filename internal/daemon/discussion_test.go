package daemon_test

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// discTrust trusts owner and knows mallory as a write collaborator who
// isn't trusted (github.DiscTrust). Nobody else is either, no bot
// included.
type discTrust struct{}

func (discTrust) Trusted(_ context.Context, login string) (bool, error) { return login == "owner", nil }

func (discTrust) DiscAccess(_ context.Context, login string) (bool, bool, error) {
	return login == "owner", login == "mallory", nil
}

// discFake is o/r with owner an admin and mallory a writer, as the
// permission endpoint and authorAssociation say, and eve with no access.
// #1 is the owner's task at ready, with the App's move in comment 1001.
func discFake(t testing.TB) *githubtest.Fake {
	f := githubtest.New(t, "o/r", slug)
	f.SetPermission("owner", "admin")
	f.SetPermission("mallory", "write")
	file(f, 0, ready)
	return f
}

// discWith is discFake, then do.
func discWith(do func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		do(t, f)
		return f
	}
}

// discPass runs step 1 on issue n with discTrust, the hold commands and
// owner to notify, from a fresh client, as after a restart.
func discPass(t testing.TB, n int) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		c := f.Client(t)
		s, err := c.Snapshot(context.Background(), "o", "r", n)
		if err != nil {
			return err
		}
		tr := discTrust{}
		st := &daemon.Step{Applier: &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug, Notify: []string{"owner"}}, Trust: tr,
			Commands: daemon.Commands(tr)}
		_, err = st.Run(context.Background(), s)
		return err
	}
}

// discCheck runs discPass on issue n under Replay, then twice, the second
// time writing nothing, and checks each issue's labels and new comments
// against want, which it calls after setup, for the keys setup makes.
func discCheck(t *testing.T, setup func(testing.TB) *githubtest.Fake, n int, want func(f *githubtest.Fake) map[int]issueWant) {
	t.Helper()
	githubtest.Replay(t, setup, discPass(t, n))
	before := setup(t)
	wants := want(before)
	got := run(t, setup, discPass(t, n)).State()
	for i, is := range got.Issues {
		w, ok := wants[is.Number]
		if !ok {
			w = issueWant{labels: before.State().Issues[i].Labels}
		}
		var added []string
		for _, c := range is.Comments[len(before.State().Issues[i].Comments):] {
			added = append(added, c.Body)
		}
		if !slices.Equal(is.Labels, w.labels) || !slices.Equal(added, w.comments) {
			t.Errorf("#%d: labels %q and new comments\n%q\nwant %q and\n%q", is.Number, is.Labels, added, w.labels, w.comments)
		}
	}
}

// discHeld is the App's comment holding an issue at at for what, with the
// transition fields, and any more markers, such as its notice.
func discHeld(at, what, fields string, more ...string) string {
	text := fmt.Sprintf("On hold for discussion, from `%s`: %s. Only a code owner can move this issue, "+
		"so it stays on hold until they resolve this.\n\ncc @owner\n\n<!-- issuedog:transition %s -->", at, what, fields)
	for _, m := range more {
		text += "\n" + m
	}
	return text
}

// discNotice is the App's notice of what, keyed key, on a held issue.
func discNotice(what, key string) string {
	return fmt.Sprintf("Also on hold for discussion: %s. This issue stays on hold until a code owner accepts it.\n\ncc @owner\n\n"+
		"<!-- issuedog:notice key=%s -->", what, key)
}

// discNoticeMarker is a notice marker keyed key.
func discNoticeMarker(key string) string { return "<!-- issuedog:notice key=" + key + " -->" }

// discEditKey is the key of the newest edit of comment id on issue n, as
// the fake has it now: e<id>-<edit time>.
func discEditKey(t testing.TB, f *githubtest.Fake, n int, id int64) string {
	t.Helper()
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		return fmt.Sprintf("e0-%d", s.BodyEdit.At.Unix())
	}
	for _, c := range s.Comments {
		if c.ID == id {
			return fmt.Sprintf("e%d-%d", id, c.Edit.At.Unix())
		}
	}
	t.Fatalf("no comment %d on #%d", id, n)
	return ""
}

// A write collaborator's comment holds the issue, with reason=discussion,
// at, origin, the round's bound and its key, c<comment id>, and a cc of
// the notify users. A comment gets no notice.
func TestDiscCommentHolds(t *testing.T) {
	t.Parallel()
	discCheck(t, discWith(func(_ testing.TB, f *githubtest.Fake) { f.AddComment(1, "mallory", "Hold on, is this right?") }), 1,
		func(*githubtest.Fake) map[int]issueWant {
			return map[int]issueWant{1: {[]string{"stage:held"}, []string{discHeld("ready", "`mallory` posted comment 1002",
				"from=ready to=held at=ready origin=ready reason=discussion event=c1002 seen=1001")}}}
		})
}

// Comments hold nothing when they're by someone without write access, the
// owner, the App or another bot, or from before the issue's first
// transition marker, so old comments don't hold issues on the day this
// ships. Nor does an issue outside the pipeline, with no transition marker.
func TestDiscCommentsThatHoldNothing(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(testing.TB) *githubtest.Fake{
		"no write access": discWith(func(_ testing.TB, f *githubtest.Fake) { f.AddComment(1, "eve", "Hold on!") }),
		"the owner":       discWith(func(_ testing.TB, f *githubtest.Fake) { f.AddComment(1, "owner", "Looks fine.") }),
		"a bot":           discWith(func(_ testing.TB, f *githubtest.Fake) { f.AddComment(1, "helper[bot]", "Hold on!") }),
		"before the first transition marker": func(t testing.TB) *githubtest.Fake {
			f := githubtest.New(t, "o/r", slug)
			f.SetPermission("owner", "admin")
			f.SetPermission("mallory", "write")
			n := f.AddIssue("owner", "Issue", "body")
			f.SetType(n, "task")
			f.AddComment(n, "mallory", "An old remark.")
			f.AddLabel(n, app, "stage:ready")
			f.AddComment(n, app, "<!-- issuedog:transition "+ready+" -->")
			return f
		},
		"outside the pipeline": func(t testing.TB) *githubtest.Fake {
			f := githubtest.New(t, "o/r", slug)
			f.SetPermission("mallory", "write")
			f.AddComment(f.AddIssue("owner", "Issue", "body"), "mallory", "Hold on!")
			return f
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := run(t, setup, discPass(t, 1))
			if w := f.Writes(); len(w) != 0 {
				t.Errorf("writes %+v; want none", w)
			}
		})
	}
}

// Edits from before the first transition marker hold nothing: one of the
// owner's comment, and one of their own comment after a trusted 👍.
func TestDiscEditsBeforeTheFloor(t *testing.T) {
	t.Parallel()
	for name, do := range map[string]func(f *githubtest.Fake, n int){
		"the owner's comment": func(f *githubtest.Fake, n int) {
			f.EditComment(f.AddComment(n, "owner", "Ship it."), "mallory", "Don't ship it.")
		},
		"their approved comment": func(f *githubtest.Fake, n int) {
			id := f.AddComment(n, "mallory", "Do it this way.")
			f.AddReaction(id, "owner", "+1")
			f.EditComment(id, "mallory", "Do it that way.")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := run(t, func(t testing.TB) *githubtest.Fake {
				f := githubtest.New(t, "o/r", slug)
				f.SetPermission("owner", "admin")
				f.SetPermission("mallory", "write")
				n := f.AddIssue("owner", "Issue", "body")
				f.SetType(n, "task")
				do(f, n)
				f.AddLabel(n, app, "stage:ready")
				f.AddComment(n, app, "<!-- issuedog:transition "+ready+" -->")
				return f
			}, discPass(t, 1))
			if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:ready"}) {
				t.Errorf("labels %q; want the issue still at ready", l)
			}
		})
	}
}

// The history an edit's key is read from is kept: once the note is on, a
// pass on the same Applier reads none.
func TestDiscEditHistoryKept(t *testing.T) {
	t.Parallel()
	f := discWith(func(_ testing.TB, f *githubtest.Fake) {
		f.EditComment(f.AddComment(1, "owner", "Ship it."), "mallory", "Don't ship it.")
	})(t)
	c := f.Client(t)
	st := &daemon.Step{Applier: &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}, Trust: discTrust{}}
	for i := range 3 {
		s, err := c.Snapshot(context.Background(), "o", "r", 1)
		if err != nil {
			t.Fatal(err)
		}
		before := f.HistoryQueries()
		if _, err := st.Run(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		if got := f.HistoryQueries() - before; i > 0 && got != 0 {
			t.Errorf("pass %d sent %d history queries; want none, the history kept", i+1, got)
		}
	}
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:held"}) {
		t.Errorf("labels %q; want the issue held", l)
	}
}

// A stranger's comment costs no permission request, as their
// authorAssociation says they have no access: GitHub would answer it with
// a 404 on every pass, which no conditional request saves.
func TestDiscStrangerAsksNothing(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(testing.TB) *githubtest.Fake{
		"a comment": discWith(func(_ testing.TB, f *githubtest.Fake) { f.AddComment(1, "eve", "Hold on!") }),
		"the issue author's rename": func(t testing.TB) *githubtest.Fake {
			f := githubtest.New(t, "o/r", slug)
			n := f.AddIssue("eve", "Issue", "body")
			f.SetType(n, "task")
			f.AddLabel(n, app, "stage:ready")
			f.AddComment(n, app, "<!-- issuedog:transition "+ready+" -->")
			f.RenameIssue(n, "eve", "My issue")
			return f
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			if err := discPass(t, 1)(f); err != nil {
				t.Fatal(err)
			}
			for _, r := range f.Requests() {
				if strings.Contains(r.Path, "/collaborators/eve/") {
					t.Errorf("asked %+v; want no permission request for eve", r)
				}
			}
			if w := f.Writes(); len(w) != 0 {
				t.Errorf("writes %+v; want none", w)
			}
		})
	}
}

// A write collaborator's edit of the App's or a trusted user's content
// holds the issue, keyed e<id>-<edit time>, with the content's note
// (MarkDirty) and a notice in the hold's comment. Their edit of an
// untrusted user's comment holds nothing.
func TestDiscEditHolds(t *testing.T) {
	t.Parallel()
	t.Run("the owner's comment", func(t *testing.T) {
		setup := discWith(func(_ testing.TB, f *githubtest.Fake) {
			f.EditComment(f.AddComment(1, "owner", "Ship it."), "mallory", "Don't ship it.")
		})
		discCheck(t, setup, 1, func(f *githubtest.Fake) map[int]issueWant {
			key := discEditKey(t, f, 1, 1002)
			return map[int]issueWant{1: {[]string{"stage:held"}, []string{discHeld("ready", "`mallory` edited comment 1002 by `owner`",
				"from=ready to=held at=ready origin=ready reason=discussion event="+key+" seen=1001", discNoticeMarker(key))}}}
		})
	})
	t.Run("the App's comment", func(t *testing.T) {
		setup := discWith(func(_ testing.TB, f *githubtest.Fake) {
			f.EditComment(f.AddComment(1, app, "A note."), "mallory", "A note, changed.")
		})
		discCheck(t, setup, 1, func(f *githubtest.Fake) map[int]issueWant {
			key := discEditKey(t, f, 1, 1002)
			return map[int]issueWant{1: {[]string{"stage:held"}, []string{discHeld("ready", "`mallory` edited comment 1002 by `issuedog[bot]`",
				"from=ready to=held at=ready origin=ready reason=discussion event="+key+" seen=1001", discNoticeMarker(key))}}}
		})
	})
	// An edit of the App's marker, which hides the stage, is
	// TestDiscRestore's.
	t.Run("the owner's description", func(t *testing.T) {
		setup := discWith(func(_ testing.TB, f *githubtest.Fake) { f.EditBody(1, "mallory", "do it differently") })
		discCheck(t, setup, 1, func(f *githubtest.Fake) map[int]issueWant {
			key := discEditKey(t, f, 1, 0)
			return map[int]issueWant{1: {[]string{"stage:held"}, []string{discHeld("ready",
				"`mallory` edited the issue description, written by `owner`",
				"from=ready to=held at=ready origin=ready reason=discussion event="+key+" seen=1001", discNoticeMarker(key))}}}
		})
	})
	t.Run("an untrusted user's comment", func(t *testing.T) {
		f := run(t, discWith(func(_ testing.TB, f *githubtest.Fake) {
			f.EditComment(f.AddComment(1, "eve", "A remark."), "mallory", "A better remark.")
		}), discPass(t, 1))
		if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:ready"}) {
			t.Errorf("labels %q; want the issue still at ready", l)
		}
	})
}

// A write collaborator's edit of their own comment after a trusted 👍
// approved it is a trigger of its own, with one notice. An edit before
// the 👍, or after an untrusted 👍, isn't. On an issue the owner held,
// neither their comment nor their edit writes a hold marker.
func TestDiscOwnApprovedEdit(t *testing.T) {
	t.Parallel()
	owner := "from=ready to=held at=ready reason=owner cmd=9 seen=1001"
	base := func(t testing.TB, f *githubtest.Fake) int64 {
		f.RemoveLabel(1, app, "stage:ready")
		f.AddLabel(1, app, "stage:held")
		f.AddComment(1, app, "<!-- issuedog:transition "+owner+" -->") // 1002
		return f.AddComment(1, "mallory", "Shouldn't this wait?")      // 1003
	}
	t.Run("after a trusted 👍", func(t *testing.T) {
		setup := discWith(func(t testing.TB, f *githubtest.Fake) {
			id := base(t, f)
			f.AddReaction(id, "owner", "+1")
			f.EditComment(id, "mallory", "Shouldn't this wait a week?")
		})
		discCheck(t, setup, 1, func(f *githubtest.Fake) map[int]issueWant {
			key := discEditKey(t, f, 1, 1003)
			return map[int]issueWant{1: {[]string{"stage:held"}, []string{
				discNotice("`mallory` edited their comment 1003 after a code owner approved it", key)}}}
		})
	})
	// Their comment from before the first transition marker holds nothing,
	// but their edit of it after a trusted 👍 holds the issue.
	t.Run("holding an issue that isn't held", func(t *testing.T) {
		setup := func(t testing.TB) *githubtest.Fake {
			f := githubtest.New(t, "o/r", slug)
			f.SetPermission("owner", "admin")
			f.SetPermission("mallory", "write")
			n := f.AddIssue("owner", "Issue", "body")
			f.SetType(n, "task")
			id := f.AddComment(n, "mallory", "Do it this way.") // 1001
			f.AddLabel(n, app, "stage:ready")
			f.AddComment(n, app, "<!-- issuedog:transition "+ready+" -->") // 1002
			f.AddReaction(id, "owner", "+1")
			f.EditComment(id, "mallory", "Do it that way.")
			return f
		}
		discCheck(t, setup, 1, func(f *githubtest.Fake) map[int]issueWant {
			key := discEditKey(t, f, 1, 1001)
			return map[int]issueWant{1: {[]string{"stage:held"}, []string{discHeld("ready",
				"`mallory` edited their comment 1001 after a code owner approved it",
				"from=ready to=held at=ready origin=ready reason=discussion event="+key+" seen=1002", discNoticeMarker(key))}}}
		})
	})
	for name, do := range map[string]func(f *githubtest.Fake, id int64){
		"before the 👍": func(f *githubtest.Fake, id int64) {
			f.EditComment(id, "mallory", "Shouldn't this wait a week?")
			f.AddReaction(id, "owner", "+1")
		},
		"after an untrusted 👍": func(f *githubtest.Fake, id int64) {
			f.AddReaction(id, "eve", "+1")
			f.AddReaction(id, app, "+1")
			f.EditComment(id, "mallory", "Shouldn't this wait a week?")
		},
		"after the owner's 👀": func(f *githubtest.Fake, id int64) {
			f.AddReaction(id, "owner", "eyes")
			f.EditComment(id, "mallory", "Shouldn't this wait a week?")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := run(t, discWith(func(t testing.TB, f *githubtest.Fake) { do(f, base(t, f)) }), discPass(t, 1))
			if w := f.Writes(); len(w) != 0 {
				t.Errorf("writes %+v; want none", w)
			}
		})
	}
}

// A write collaborator's deletion of the owner's or the App's comment, or
// of one GitHub names no author of, holds the issue, keyed d<event node
// ID>, with a notice in the hold's comment. Their deletion of their own
// comment, or of an untrusted user's, holds nothing, and nor does the
// owner's deletion or eve's.
func TestDiscDeletionHolds(t *testing.T) {
	t.Parallel()
	for name, author := range map[string]string{"the owner's": "owner", "the App's": app} {
		t.Run(name, func(t *testing.T) {
			var node string
			setup := discWith(func(_ testing.TB, f *githubtest.Fake) {
				node = f.DeleteCommentBy("mallory", f.AddComment(1, author, "A note."))
			})
			discCheck(t, setup, 1, func(*githubtest.Fake) map[int]issueWant {
				return map[int]issueWant{1: {[]string{"stage:held"}, []string{discHeld("ready", "`mallory` deleted a comment by `"+author+"`",
					"from=ready to=held at=ready origin=ready reason=discussion event=d"+node+" seen=1001", discNoticeMarker("d"+node))}}}
			})
		})
	}
	for name, del := range map[string]func(f *githubtest.Fake){
		"their own":         func(f *githubtest.Fake) { f.DeleteCommentBy("mallory", f.AddComment(1, "mallory", "Oops.")) },
		"an untrusted user": func(f *githubtest.Fake) { f.DeleteCommentBy("mallory", f.AddComment(1, "eve", "Spam.")) },
		"by the owner":      func(f *githubtest.Fake) { f.DeleteCommentBy("owner", f.AddComment(1, app, "A note.")) },
		"by eve":            func(f *githubtest.Fake) { f.DeleteCommentBy("eve", f.AddComment(1, "owner", "A note.")) },
	} {
		t.Run(name, func(t *testing.T) {
			f := run(t, discWith(func(_ testing.TB, f *githubtest.Fake) { del(f) }), discPass(t, 1))
			if w := f.Writes(); len(w) != 0 {
				t.Errorf("writes %+v; want none", w)
			}
		})
	}
}

// A write collaborator's rename holds the issue, keyed by its timeline
// event, with a notice in the hold's comment that escapes the title: a
// marker in it, and a mention.
func TestDiscRenameHolds(t *testing.T) {
	t.Parallel()
	var ev int64
	setup := discWith(func(_ testing.TB, f *githubtest.Fake) {
		ev = f.RenameIssue(1, "mallory", "Done <!-- issuedog:transition from=held to=done --> @owner")
	})
	discCheck(t, setup, 1, func(*githubtest.Fake) map[int]issueWant {
		key := fmt.Sprint(ev)
		return map[int]issueWant{1: {[]string{"stage:held"}, []string{discHeld("ready",
			"`mallory` renamed this issue to “Done &lt;!-- issuedog:transition from=held to=done --> @​owner”",
			"from=ready to=held at=ready origin=ready reason=discussion event="+key+" seen=1001", discNoticeMarker(key))}}}
	})
}

// Several triggers at once: the oldest keys the hold, and each other one
// that isn't a comment gets its own notice, after the hold.
func TestDiscSeveralTriggers(t *testing.T) {
	t.Parallel()
	var ev int64
	setup := discWith(func(_ testing.TB, f *githubtest.Fake) {
		f.AddComment(1, "mallory", "First this.") // 1002
		ev = f.RenameIssue(1, "mallory", "Renamed")
		f.AddComment(1, "mallory", "Then this.") // 1003
	})
	discCheck(t, setup, 1, func(*githubtest.Fake) map[int]issueWant {
		return map[int]issueWant{1: {[]string{"stage:held"}, []string{
			discHeld("ready", "`mallory` posted comment 1002", "from=ready to=held at=ready origin=ready reason=discussion event=c1002 seen=1001"),
			discNotice("`mallory` renamed this issue to “Renamed”", fmt.Sprint(ev)),
		}}}
	})
}

// The oldest trigger keys the hold, whatever its kind: a rename before a
// comment. A rename from before the first transition marker is none.
func TestDiscOldestKeysTheHold(t *testing.T) {
	t.Parallel()
	var ev int64
	setup := func(t testing.TB) *githubtest.Fake {
		f := githubtest.New(t, "o/r", slug)
		f.SetPermission("owner", "admin")
		f.SetPermission("mallory", "write")
		n := f.AddIssue("owner", "Issue", "body")
		f.SetType(n, "task")
		f.RenameIssue(n, "mallory", "Too early")
		f.AddLabel(n, app, "stage:ready")
		f.AddComment(n, app, "<!-- issuedog:transition "+ready+" -->") // 1001
		ev = f.RenameIssue(n, "mallory", "Renamed")
		f.AddComment(n, "mallory", "Then this.") // 1002
		return f
	}
	discCheck(t, setup, 1, func(*githubtest.Fake) map[int]issueWant {
		key := fmt.Sprint(ev)
		return map[int]issueWant{1: {[]string{"stage:held"}, []string{discHeld("ready", "`mallory` renamed this issue to “Renamed”",
			"from=ready to=held at=ready origin=ready reason=discussion event="+key+" seen=1001", discNoticeMarker(key))}}}
	})
}

// /resume on an issue held only for discussion ends the hold, accepting
// its triggers, and restores at (SPEC §6.4, T4).
func TestDiscResumeOnDiscussionHold(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	f.AddComment(1, "mallory", "Wait.")
	if err := discPass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	resume := f.AddComment(1, "owner", "/resume")
	if err := discPass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	is := f.State().Issues[0]
	want := moved("Resumed by `/resume`, back to `ready`.", fmt.Sprintf("from=held to=ready cmd=%d seen=1001", resume))
	if last := is.Comments[len(is.Comments)-1]; !slices.Equal(is.Labels, []string{"stage:ready"}) || last.Body != want {
		t.Errorf("labels %q, newest comment %q; want #1 back at ready: %q", is.Labels, last.Body, want)
	}
}

// On an issue already held, a trigger writes no hold marker (Q2b): a
// comment writes nothing, and anything else only its notice, once. /resume
// on the issue ends the owner's hold and accepts the triggers (T4), back
// to ready.
func TestDiscOnHeldIssue(t *testing.T) {
	t.Parallel()
	var ev int64
	setup := discWith(func(_ testing.TB, f *githubtest.Fake) {
		f.AddComment(1, "owner", "/hold") // 1002
	})
	f := run(t, setup, discPass(t, 1))
	f.AddComment(1, "mallory", "Wait.")
	ev = f.RenameIssue(1, "mallory", "Wait")
	writes := len(f.Writes())
	for range 2 {
		if err := discPass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
	}
	var paths []string
	for _, w := range f.Writes()[writes:] {
		paths = append(paths, w.Method+" "+w.Path)
	}
	is := f.State().Issues[0]
	if !slices.Equal(paths, []string{"POST /issues/1/comments"}) || is.Comments[len(is.Comments)-1].Body != discNotice("`mallory` renamed this issue to “Wait”", fmt.Sprint(ev)) {
		t.Errorf("writes %q, newest comment %q; want only the rename's notice, once", paths, is.Comments[len(is.Comments)-1].Body)
	}
	resume := f.AddComment(1, "owner", "/resume")
	if err := discPass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	is = f.State().Issues[0]
	last := is.Comments[len(is.Comments)-1]
	if !slices.Equal(is.Labels, []string{"stage:ready"}) || !strings.Contains(last.Body, fmt.Sprintf("from=held to=ready cmd=%d ", resume)) {
		t.Errorf("labels %q, newest comment %q; want #1 back at ready by the /resume", is.Labels, last.Body)
	}
}

// Replay over the notice on a held issue: a crash at any write, then a
// pass, ends as a clean pass does, with one notice.
func TestDiscNoticeReplay(t *testing.T) {
	t.Parallel()
	var ev int64
	held := "from=ready to=held at=ready reason=owner cmd=9 seen=1001"
	setup := discWith(func(_ testing.TB, f *githubtest.Fake) {
		f.RemoveLabel(1, app, "stage:ready")
		f.AddLabel(1, app, "stage:held")
		f.AddComment(1, app, "<!-- issuedog:transition "+held+" -->")
		ev = f.RenameIssue(1, "mallory", "Wait")
	})
	discCheck(t, setup, 1, func(*githubtest.Fake) map[int]issueWant {
		return map[int]issueWant{1: {[]string{"stage:held"}, []string{discNotice("`mallory` renamed this issue to “Wait”", fmt.Sprint(ev))}}}
	})
}

// A notice made dirty or deleted isn't posted again, so no key gets a
// second notice: the trigger stays open.
func TestDiscLostNoticeNotPostedAgain(t *testing.T) {
	t.Parallel()
	for name, lose := range map[string]func(f *githubtest.Fake, id int64){
		"dirty":   func(f *githubtest.Fake, id int64) { f.EditComment(id, "eve", "nothing here") },
		"deleted": func(f *githubtest.Fake, id int64) { f.DeleteCommentBy("owner", id) },
	} {
		t.Run(name, func(t *testing.T) {
			f := discFake(t)
			ev := f.RenameIssue(1, "mallory", "Wait")
			if err := discPass(t, 1)(f); err != nil {
				t.Fatal(err)
			}
			cs := f.State().Issues[0].Comments
			hold := cs[len(cs)-1]
			if !strings.Contains(hold.Body, discNoticeMarker(fmt.Sprint(ev))) {
				t.Fatalf("newest comment %q; want the hold with the rename's notice", hold.Body)
			}
			ids := commentIDs(t, f, 1)
			lose(f, ids[len(ids)-1])
			writes := len(f.Writes())
			for range 2 {
				if err := discPass(t, 1)(f); err != nil {
					t.Fatal(err)
				}
			}
			for _, c := range f.State().Issues[0].Comments {
				if strings.Contains(c.Body, discNoticeMarker(fmt.Sprint(ev))) {
					t.Errorf("comment %q; want no notice of the rename once its notice is gone", c.Body)
				}
			}
			for _, w := range f.Writes()[writes:] {
				if w.Method == "POST" && strings.Contains(w.Body, "notice key="+fmt.Sprint(ev)) {
					t.Errorf("wrote %+v; want no second notice", w)
				}
			}
		})
	}
}

// commentIDs lists the IDs of issue n's comments, oldest first.
func commentIDs(t testing.TB, f *githubtest.Fake, n int) []int64 {
	t.Helper()
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, c := range s.Comments {
		ids = append(ids, c.ID)
	}
	return ids
}

// A move out of held resolves every trigger before it: a stage label the
// owner set by hand ends every hold (SPEC §6.4), so the comment that held
// the issue doesn't hold it again, and a later one does.
func TestDiscOverrideResolves(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	f.AddComment(1, "mallory", "Wait.")
	if err := discPass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.RemoveLabel(1, "owner", "stage:held")
	f.AddLabel(1, "owner", "stage:ready")
	if err := discPass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:ready"}) {
		t.Fatalf("labels %q after the owner's override; want ready, the comment resolved", l)
	}
	writes := len(f.Writes())
	if err := discPass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("a pass after the override wrote %+v; want nothing", w)
	}
	id := f.AddComment(1, "mallory", "Wait again.")
	if err := discPass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	cs := f.State().Issues[0].Comments
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:held"}) || !strings.Contains(cs[len(cs)-1].Body, fmt.Sprintf("event=c%d ", id)) {
		t.Errorf("labels %q, newest comment %q; want the new comment to hold the issue", l, cs[len(cs)-1].Body)
	}
}

// A hold in the clarifying after a hold keeps the origin its markers carry
// forward; any other stage is its own origin.
func TestDiscOriginCarriedForward(t *testing.T) {
	t.Parallel()
	setup := func(t testing.TB) *githubtest.Fake {
		f := githubtest.New(t, "o/r", slug)
		f.SetPermission("owner", "admin")
		f.SetPermission("mallory", "write")
		file(f, 0, "from=held to=clarifying at=spec-review origin=spec-review event=c9 seen=7")
		f.AddComment(1, "mallory", "And another thing.")
		return f
	}
	discCheck(t, setup, 1, func(*githubtest.Fake) map[int]issueWant {
		return map[int]issueWant{1: {[]string{"stage:held"}, []string{discHeld("clarifying", "`mallory` posted comment 1002",
			"from=clarifying to=held at=clarifying origin=spec-review reason=discussion event=c1002 seen=7")}}}
	})
}

// A discussion hold holds only its issue: its parent and its children
// carry on, at their stages.
func TestDiscHoldsOnlyItsIssue(t *testing.T) {
	t.Parallel()
	setup := func(t testing.TB) *githubtest.Fake {
		f := githubtest.New(t, "o/r", slug)
		f.SetPermission("owner", "admin")
		f.SetPermission("mallory", "write")
		file(f, 0, ready)                             // #1, comment 1001
		file(f, 1, "from=ready to=in-progress run=a") // #2, comment 1002
		file(f, 2, ready)                             // #3, comment 1003
		f.AddComment(2, "mallory", "Wait.")           // 1004
		return f
	}
	discCheck(t, setup, 2, func(*githubtest.Fake) map[int]issueWant {
		return map[int]issueWant{2: {[]string{"stage:held"}, []string{discHeld("in-progress", "`mallory` posted comment 1004",
			"from=in-progress to=held at=in-progress origin=in-progress reason=discussion event=c1004 seen=1002")}}}
	})
}

// A /resume on the parent ends the hold its /hold put on a child, but a
// child with an open discussion trigger stays held, and the parent's move
// says so.
func TestDiscResumeOnParent(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", slug)
	f.SetPermission("owner", "admin") // a real collaborator, as discWho asks (#188's verify, attempt 3)
	f.SetPermission("mallory", "write")
	file(f, 0, above("ready")) // #1, held by its /hold in comment 1001
	file(f, 1, above("ready")) // #2, held by the same /hold
	f.AddComment(2, "mallory", "Wait.")
	resume := f.AddComment(1, "owner", "/resume")
	if err := discPass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	st := f.State()
	if !slices.Equal(st.Issues[0].Labels, []string{"stage:ready"}) || !slices.Equal(st.Issues[1].Labels, []string{"stage:held"}) {
		t.Fatalf("labels %q and %q; want #1 back at ready, and #2 still held for discussion", st.Issues[0].Labels, st.Issues[1].Labels)
	}
	want := moved("Resumed by `/resume`, back to `ready`. Still on hold for discussion: #2.", fmt.Sprintf("from=held to=ready cmd=%d", resume))
	if cs := st.Issues[0].Comments; cs[len(cs)-1].Body != want {
		t.Errorf("newest comment on #1 %q; want %q", cs[len(cs)-1].Body, want)
	}
}

// A /resume on the parent held for discussion too accepts the parent's
// triggers, back to ready, and ends the hold its /hold put on the child,
// which stays held for its own discussion: the parent's move names it, and
// the child gets a record that its owner hold ended (resolveResumed).
func TestDiscResumeOnParentHeldForDiscussion(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", slug)
	f.SetPermission("owner", "admin") // a real collaborator, as discWho asks (#188's verify, attempt 3)
	f.SetPermission("mallory", "write")
	file(f, 0, above("ready")) // #1, held by its /hold in comment 1001
	file(f, 1, above("ready")) // #2, held by the same /hold
	f.AddComment(1, "mallory", "Wait.")
	f.AddComment(2, "mallory", "Wait too.")
	resume := f.AddComment(1, "owner", "/resume")
	if err := discPass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	st := f.State()
	if !slices.Equal(st.Issues[0].Labels, []string{"stage:ready"}) || !slices.Equal(st.Issues[1].Labels, []string{"stage:held"}) {
		t.Fatalf("labels %q and %q; want #1 back at ready, and #2 still held for discussion", st.Issues[0].Labels, st.Issues[1].Labels)
	}
	want := moved("Resumed by `/resume`, back to `ready`. Still on hold for discussion: #2.", fmt.Sprintf("from=held to=ready cmd=%d", resume))
	if cs := st.Issues[0].Comments; cs[len(cs)-1].Body != want {
		t.Errorf("newest comment on #1 %q; want %q", cs[len(cs)-1].Body, want)
	}
	record := fmt.Sprintf("`/resume` on #1 ended the owner hold on this issue, but it stays on hold for discussion until a code owner resolves that, "+
		"or posts `/resume` here.\n\n<!-- issuedog:ended reason=owner key=c1001 cmd=%d -->", resume)
	if cs := st.Issues[1].Comments; cs[len(cs)-1].Body != record {
		t.Errorf("newest comment on #2 %q; want %q", cs[len(cs)-1].Body, record)
	}
}

// With no notify users, the hold has no cc line.
func TestDiscNoNotify(t *testing.T) {
	t.Parallel()
	f := discWith(func(_ testing.TB, f *githubtest.Fake) { f.AddComment(1, "mallory", "Wait.") })(t)
	c := f.Client(t)
	s, err := c.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	st := &daemon.Step{Applier: &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}, Trust: discTrust{}}
	if _, err := st.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	cs := f.State().Issues[0].Comments
	if body := cs[len(cs)-1].Body; strings.Contains(body, "cc @") || !strings.HasPrefix(body, "On hold for discussion") {
		t.Errorf("hold %q; want no cc line", body)
	}
}

// A trust lookup that fails fails the pass, before anything is written.
func TestDiscTrustErrorFailsThePass(t *testing.T) {
	t.Parallel()
	f := discWith(func(_ testing.TB, f *githubtest.Fake) { f.AddComment(1, "mallory", "Wait.") })(t)
	c := f.Client(t)
	s, err := c.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	st := &daemon.Step{Applier: &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}, Trust: discFailTrust{}}
	if _, err := st.Run(context.Background(), s); err == nil || !strings.Contains(err.Error(), "discussion") {
		t.Errorf("Run = %v; want the discussion's trust error", err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes %+v; want none", w)
	}
}

// discFailTrust trusts nobody, and fails every DiscAccess.
type discFailTrust struct{}

func (discFailTrust) Trusted(context.Context, string) (bool, error) { return false, nil }
func (discFailTrust) DiscAccess(context.Context, string) (bool, bool, error) {
	return false, false, fmt.Errorf("permission: 502")
}

// Through the loop, with the trust CODEOWNERS gives: mallory's comment
// holds the issue, and the held issue is visited on every pass, but an
// unchanged pass makes only conditional reads, answered 304, and writes
// nothing. A restart writes nothing new.
func TestDiscThroughTheLoop(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := fileAt(f, "ready", "do it") // comment 1001
	f.AddComment(n, "mallory", "Wait.")
	o := newOrigin(t)
	o.commit(config.Path, "types: labels\nnotify: [owner]\n")
	var visits []int
	d := newDaemon(t, f, o, &visits)
	iterate(t, d)
	if l := f.State().Issues[n-1].Labels; !slices.Equal(l, []string{"stage:held"}) {
		t.Fatalf("labels %q; want the issue held for discussion", l)
	}
	if c := newestComment(f, n); !strings.Contains(c, "\n\ncc @owner\n\n") {
		t.Errorf("hold %q; want a cc of config's notify", c)
	}
	iterate(t, d) // reads the App's writes back
	writes, from := len(f.Writes()), len(f.Requests())
	visits = nil
	iterate(t, d)
	if !reflect.DeepEqual(visits, []int{n}) {
		t.Errorf("the unchanged pass visited %v; want the held issue, [%d]", visits, n)
	}
	for _, r := range f.Requests()[from:] {
		if r.Method != "GET" || !r.Conditional || r.Status != 304 {
			t.Errorf("the unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}
	if len(f.Writes()) != writes {
		t.Errorf("the unchanged pass wrote %+v; want nothing", f.Writes()[writes:])
	}
	iterate(t, newDaemon(t, f, o, nil))
	if len(f.Writes()) != writes {
		t.Errorf("a restart wrote %+v; want nothing", f.Writes()[writes:])
	}
}

// A write collaborator's comment during a run holds the issue at apply
// time, and the result is dropped, uncounted: nothing from it is posted.
// The next pass starts no run on the held issue.
func TestDiscDuringRunDrops(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.claude(okOut, 0)
	g.stage.during = func() {
		g.stage.during = nil
		f.AddComment(1, "mallory", "Wait, that's wrong.")
	}
	g.pass()
	is := f.State().Issues[0]
	if specApplied(f, 1, "run1", answer) || !slices.Equal(is.Labels, []string{"stage:held"}) {
		t.Fatalf("labels %q; want run1 dropped and the issue held", is.Labels)
	}
	for _, c := range is.Comments {
		if strings.Contains(c.Body, "run=run1") {
			t.Errorf("comment %q; want nothing posted from run1", c.Body)
		}
	}
	if !strings.Contains(newestComment(f, 1), "reason=discussion") || g.lease().RunID != "" || len(g.leftovers()) != 0 {
		t.Errorf("newest comment %q, lease %+v, left %v; want the hold, and the lease and files gone", newestComment(f, 1), g.lease(), g.leftovers())
	}
	g.pass()
	if g.runs() != 1 {
		t.Errorf("%d runs; want none more on the held issue", g.runs())
	}
}

// Replay over the hold the apply-time check writes: a saved result whose
// issue a write collaborator commented on is dropped, after the hold, and
// a crash at any write ends as a clean pass does.
func TestDiscDuringRunReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	roots := map[*githubtest.Fake]string{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		roots[f] = t.TempDir()
		saveRun(tb, roots[f], "saved", okOut, interview(f))
		f.AddComment(1, "mallory", "Wait, that's wrong.")
		return f
	}, func(f *githubtest.Fake) error {
		g := newRig(t, f, o, roots[f])
		g.d.Step.Trust = discTrust{}
		g.stage.pending = func(daemon.Input) bool { return false }
		if err := g.r.Work(context.Background(), nil); err != nil {
			return err
		}
		if l := g.lease(); l.RunID != "" {
			return fmt.Errorf("the pass left the lease %+v; want the result dropped", l)
		}
		if specApplied(f, 1, "saved", 0) || !strings.Contains(newestComment(f, 1), "reason=discussion event=c") {
			return fmt.Errorf("newest comment %q; want the hold, and nothing of the result", newestComment(f, 1))
		}
		return nil
	})
}

// The snapshot carries the renames only once CheckLabels has read them.
func TestDiscRenamesFromCheckLabels(t *testing.T) {
	t.Parallel()
	f := discFake(t)
	ev := f.RenameIssue(1, "mallory", "Renamed")
	c := f.Client(t)
	s, err := c.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if s.DiscRenames != nil {
		t.Fatalf("renames %+v before CheckLabels; want none", s.DiscRenames)
	}
	a := &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}
	if err := a.CheckLabels(context.Background(), s, discTrust{}, func(int64) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if len(s.DiscRenames) != 1 || s.DiscRenames[0].ID != ev || s.DiscRenames[0].By != (github.Author{Login: "mallory", Type: "User"}) ||
		s.DiscRenames[0].Title != "Renamed" || s.DiscRenames[0].At.IsZero() {
		t.Errorf("renames %+v; want mallory's rename, event %d, to Renamed", s.DiscRenames, ev)
	}
}
