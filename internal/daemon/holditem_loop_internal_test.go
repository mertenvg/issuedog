package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/repo"
)

// Hold items through Iterate (#192, #192's verify), reading the cache the
// digest and assignment share: the key across a hold, and a failed look
// for triggers.

// holdItemLoop is o/r with issue 1, the owner's, at ready, and a daemon
// over it whose trust knows owner as trusted, mallory as a write
// collaborator who isn't, and fails on bob (newDiscCounting). inject, if
// set, is added to the snapshot after step 1's comments, as Reactions.
func holdItemLoop(t *testing.T, inject *github.Comment) (*githubtest.Fake, *Daemon) {
	t.Helper()
	f := githubtest.New(t, "o/r", "issuedog")
	f.SetPermission("owner", "admin")
	f.SetPermission("mallory", "write")
	n := f.AddIssue("owner", "Mine", "do it")
	f.AddLabel(n, "issuedog[bot]", "stage:ready")
	f.AddComment(n, "issuedog[bot]", "<!-- issuedog:transition from=intake to=ready run=r1 -->")
	st := &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}, Trust: newDiscCounting(),
		Reactions: func(_ context.Context, _ *Applier, s *github.Snapshot, _ Dirt) error {
			if inject != nil && inject.ID != 0 {
				s.Comments = append(s.Comments, *inject)
			}
			return nil
		}}
	st.Commands = Commands(st.CurrentTrust())
	return f, &Daemon{Step: st, Clone: repo.New(filepath.Join(t.TempDir(), "none"), filepath.Join(t.TempDir(), "r.git")), Config: config.Defaults()}
}

// holdItemKeyAfter runs a pass of d, and returns issue 1's one hold item's
// key, and how many triggers it lists, or "" with no item.
func holdItemKeyAfter(t *testing.T, d *Daemon) (string, int) {
	t.Helper()
	if err := d.Iterate(context.Background()); err != nil {
		t.Fatal(err)
	}
	items := d.digestCache[1]
	if len(items) == 0 {
		return "", 0
	}
	if len(items) != 1 || items[0].Kind != waitHold {
		t.Fatalf("items %+v; want one hold", items)
	}
	return items[0].Key, len(holdItemTriggers(items[0]))
}

// The verifier's scenario: the owner /holds the issue, and mallory
// comments, renames it, and deletes her comment. The key names the
// /hold's marker throughout, though the oldest trigger goes. After a
// stage set by hand ends the hold, mallory's next comment holds the issue
// afresh, with a key of its own: her comment's, which its marker carries.
func TestHoldItemKeyAcrossTheHold(t *testing.T) {
	f, d := holdItemLoop(t, nil)
	hold := f.AddComment(1, "owner", "/hold")
	if key, _ := holdItemKeyAfter(t, d); key != "" {
		t.Fatalf("key %q under the owner hold alone; want no item", key)
	}
	if l := f.State().Issues[0].Labels; len(l) != 1 || l[0] != "stage:held" {
		t.Fatalf("labels %q; want the /hold applied", l)
	}
	want := "hold-1-cmd" + strconv.FormatInt(hold, 10)
	c := f.AddComment(1, "mallory", "Wait.")
	if key, n := holdItemKeyAfter(t, d); key != want || n != 1 {
		t.Errorf("after the comment: key %q with %d triggers; want %s with 1", key, n, want)
	}
	f.RenameIssue(1, "mallory", "Wait")
	if key, n := holdItemKeyAfter(t, d); key != want || n != 2 {
		t.Errorf("after the rename: key %q with %d triggers; want %s with 2", key, n, want)
	}
	f.DeleteCommentBy("mallory", c)
	if key, n := holdItemKeyAfter(t, d); key != want || n != 1 {
		t.Errorf("after the comment's deletion: key %q with %d triggers; want %s with 1, the rename", key, n, want)
	}
	f.RemoveLabel(1, "owner", "stage:held")
	f.AddLabel(1, "owner", "stage:ready")
	if key, _ := holdItemKeyAfter(t, d); key != "" {
		t.Fatalf("key %q after the owner's ready; want no item", key)
	}
	c2 := f.AddComment(1, "mallory", "Wait again.")
	if key, n := holdItemKeyAfter(t, d); key != "hold-1-c"+strconv.FormatInt(c2, 10) || n != 1 {
		t.Errorf("after the new hold: key %q with %d triggers; want hold-1-c%d with 1", key, n, c2)
	}
}

// A look for triggers for the hold item that fails, here asking trust
// about bob, fails the issue's pass: its items stay as they were, and it's
// read again, though step 1 succeeded.
func TestHoldItemFindFails(t *testing.T) {
	var inject github.Comment
	f, d := holdItemLoop(t, &inject)
	f.AddComment(1, "mallory", "Wait.")
	key, _ := holdItemKeyAfter(t, d)
	if !strings.HasPrefix(key, "hold-1-c") {
		t.Fatalf("key %q; want mallory's comment to hold the issue", key)
	}
	inject = github.Comment{ID: 1 << 40, Author: github.Author{Login: "bob", Type: "User"}, Association: "COLLABORATOR", Body: "Hm."}
	err := d.Iterate(context.Background())
	if !errors.Is(err, errTrustOnce) || !strings.Contains(err.Error(), "the digest's hold item") {
		t.Errorf("Iterate = %v; want the hold item's trust error", err)
	}
	if items := d.digestCache[1]; len(items) != 1 || items[0].Key != key || !d.stale[1] {
		t.Errorf("items %+v, stale %t; want the item kept and the issue read again", items, d.stale[1])
	}
	inject = github.Comment{}
	if got, _ := holdItemKeyAfter(t, d); got != key {
		t.Errorf("key %q once trust answers; want %s", got, key)
	}
}
