package daemon_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// discNoticeKeys returns the key of each notice marker on issue n, in the
// order they were posted.
func discNoticeKeys(f *githubtest.Fake, n int) []string {
	var keys []string
	for _, c := range f.State().Issues[n-1].Comments {
		if c.Author != app {
			continue
		}
		for _, m := range github.ParseMarkers(c.Body) {
			if m.Kind == "notice" {
				keys = append(keys, m.Fields["key"])
			}
		}
	}
	return keys
}

// discUnderHold files #1, a task at ready, and n, a task under it at
// ready, and posts /hold on #1, which holds both. A /resume on #1 ends n's
// owner hold, but not a discussion hold on it (SPEC §6.4), which the
// record it writes on n then says (T4): so an act that's still open keeps
// n held, and one that isn't lets it go back to ready.
func discUnderHold(f *githubtest.Fake) (parent, n int) {
	parent = fileAt(f, "ready", "do it")
	f.SetType(parent, "task")
	n = fileAt(f, "ready", "do it")
	f.SetType(n, "task")
	f.SetParent(n, parent)
	f.AddComment(parent, "owner", "/hold")
	return parent, n
}

// discResumeRejected checks that issue n is still held after a /resume on
// its parent (discUnderHold), with the record that the discussion keeps
// it held, and the notices want, and that another pass writes nothing, and
// costs no rate limit: each of its requests is answered 304.
func discResumeRejected(t *testing.T, f *githubtest.Fake, d *daemon.Daemon, n int, want []string) {
	t.Helper()
	is := f.State().Issues[n-1]
	if !slices.Equal(is.Labels, []string{"stage:held"}) || !slices.ContainsFunc(is.Comments, func(c githubtest.CommentState) bool {
		return strings.Contains(c.Body, "stays on hold for discussion")
	}) {
		t.Errorf("labels %q, comments %q; want the issue still held for the recorded act after its parent's /resume", is.Labels, is.Comments)
	}
	if got := discNoticeKeys(f, n); !slices.Equal(got, want) {
		t.Errorf("notices %q; want %q, none posted again", got, want)
	}
	writes, requests := len(f.Writes()), len(f.Requests())
	iterate(t, d)
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("another pass wrote %+v; want nothing", w)
	}
	for _, r := range f.Requests()[requests:] {
		if r.Status != http.StatusNotModified {
			t.Errorf("another pass sent %+v; want only requests answered 304", r)
		}
	}
}

// An edit a notice records stays a trigger after the same writer edits
// the same comment again and loses access before the next pass: the
// content's newest edit is no longer the recorded one, and nobody would be
// asked about the new one, but the record still holds the issue, so a
// /resume on its parent doesn't take it out of held. With or without an
// earlier comment of theirs, which shows them as a collaborator.
func TestDiscRecordedEditOutlivesChange(t *testing.T) {
	for _, mode := range []string{"", " after her comment"} {
		t.Run("edited twice"+mode, func(t *testing.T) {
			f := newFake(t)
			parent, n := discUnderHold(f)
			d := newDaemon(t, f, newOrigin(t), nil)
			iterate(t, d)
			if mode != "" {
				f.AddComment(n, "mallory", "Hello.")
			}
			x := f.AddComment(n, "owner", "Ship it.")
			iterate(t, d)
			f.EditComment(x, "mallory", "Don't.")
			first := discEditKey(t, f, n, x)
			iterate(t, d)
			iterate(t, d)
			if got := discNoticeKeys(f, n); !slices.Equal(got, []string{first}) {
				t.Fatalf("notices %q; want one for %s", got, first)
			}
			f.EditComment(x, "mallory", "Really don't.")
			f.RemovePermission("mallory")
			f.AddComment(parent, "owner", "/resume")
			// The parent's visit, then the child's, which its /resume touched.
			iterate(t, d)
			iterate(t, d)
			iterate(t, d)
			discResumeRejected(t, f, d, n, []string{first})
		})
	}
}

// An act a notice records stays a trigger whatever changes its content
// after it, and a /resume on the issue's parent leaves it held:
//   - mallory's own approved comment, edited again, as she loses access;
//   - the owner's comment she edited, edited again by another writer who
//     then loses access;
//   - that comment, then deleted by her as she loses access;
//   - that comment, then edited by a bot with write access, which loses
//     nothing and is no trigger itself.
func TestDiscRecordedActOutlivesChange(t *testing.T) {
	for _, name := range []string{"own edit twice", "second editor", "then deleted", "bot edit"} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.SetPermission("trudy", "write")
			parent, n := discUnderHold(f)
			d := newDaemon(t, f, newOrigin(t), nil)
			iterate(t, d)
			var x int64
			if name == "own edit twice" {
				x = f.AddComment(n, "mallory", "Do it this way.")
				f.AddReaction(x, "owner", "+1")
				iterate(t, d)
				f.EditComment(x, "mallory", "Do it that way.")
			} else {
				x = f.AddComment(n, "owner", "Ship it.")
				iterate(t, d)
				f.EditComment(x, "mallory", "Don't.")
			}
			first := discEditKey(t, f, n, x)
			iterate(t, d)
			iterate(t, d)
			if got := discNoticeKeys(f, n); !slices.Equal(got, []string{first}) {
				t.Fatalf("notices %q; want one for %s", got, first)
			}
			switch name {
			case "own edit twice":
				f.EditComment(x, "mallory", "Do it my way.")
				f.RemovePermission("mallory")
			case "second editor":
				f.EditComment(x, "trudy", "Never.")
				f.RemovePermission("trudy")
			case "bot edit":
				f.EditComment(x, "helper[bot]", "Bot text.")
			case "then deleted":
				f.DeleteCommentBy("mallory", x)
				f.RemovePermission("mallory")
			}
			f.AddComment(parent, "owner", "/resume")
			// The parent's visit, then the child's, which its /resume touched.
			iterate(t, d)
			iterate(t, d)
			iterate(t, d)
			discResumeRejected(t, f, d, n, []string{first})
		})
	}
}

// A new edit of content whose earlier edit a notice records is a trigger
// of its own, with its own notice: two notices are then open on the same
// comment, and neither is posted again, nor does a /resume on the
// issue's parent take it out of held.
func TestDiscRecordedEditThenNewEdit(t *testing.T) {
	f := newFake(t)
	parent, n := discUnderHold(f)
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	x := f.AddComment(n, "owner", "Ship it.")
	iterate(t, d)
	f.EditComment(x, "mallory", "Don't.")
	first := discEditKey(t, f, n, x)
	iterate(t, d)
	f.EditComment(x, "mallory", "Really don't.")
	second := discEditKey(t, f, n, x)
	if first == second {
		t.Fatalf("both edits keyed %s; want each its own key", first)
	}
	iterate(t, d)
	iterate(t, d)
	f.AddComment(parent, "owner", "/resume")
	// The parent's visit, then the child's, which its /resume touched.
	iterate(t, d)
	iterate(t, d)
	iterate(t, d)
	discResumeRejected(t, f, d, n, []string{first, second})
}
