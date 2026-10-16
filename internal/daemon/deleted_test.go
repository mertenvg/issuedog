package daemon_test

import (
	"context"
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// deletedProbe is #87's note 3: revision 1 of "body" with the owner's 👍
// (1002), revision 2 of "body v2" (1003), and revision 3 of "body" again
// (1004), which mallory and then the owner delete, so the latest revision
// left is revision 1, whose hash the body has again. Then it runs more.
func deletedProbe(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return review(func(t testing.TB, f *githubtest.Fake) {
		f.AddReaction(1001, "owner", "+1")
		f.EditBody(1, "owner", "body v2")
		post(t, f, 1003, app, revision(2, "body v2"))
		f.EditBody(1, "owner", "body")
		post(t, f, 1004, app, revision(3, "body"))
		f.DeleteCommentBy("mallory", 1004)
		f.DeleteCommentBy("owner", 1003)
		more(t, f)
	})
}

// An App comment deleted after the latest revision hides it, as a dirty
// one does (#87's assumption 11): the old 👍 approves nothing, and a fresh
// revision is posted, after which a 👍 approves again. Who deleted it
// doesn't matter, and nothing is held. A deletion of anyone else's comment,
// or of an App comment before the revision was posted, hides nothing.
func TestDeletedHidesRevision(t *testing.T) {
	atReview := []string{"stage:spec-review"}
	planning := []string{"stage:planning"}
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		want  outcome
	}{
		{"the old 👍 doesn't approve, and a fresh revision is posted",
			deletedProbe(func(testing.TB, *githubtest.Fake) {}),
			outcome{comments: []string{posted(2, "body", "", "", "1001")}, labels: atReview}},
		{"an /approve is rejected, and a fresh revision is posted",
			deletedProbe(func(t testing.TB, f *githubtest.Fake) { post(t, f, 1005, "owner", "/approve") }),
			outcome{comments: []string{final("/approve", "I didn't apply this command", hidden, "1005"), posted(2, "body", "", "", "1005")},
				labels: atReview, downs: []string{"/approve"}}},
		{"once a fresh revision follows, a 👍 on it approves",
			deletedProbe(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1005, app, revision(2, "body"))
				f.AddReaction(1005, "owner2", "+1") // 1006
			}),
			outcome{comments: []string{approvedRev(2, "owner2's 👍", "r1006", "1005")}, labels: planning}},
		{"the owner deleting an App comment hides the revision too, and holds nothing",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, app, "Noted.")
				f.DeleteCommentBy("owner", 1002)
				f.AddReaction(1001, "owner", "+1")
			}),
			outcome{comments: []string{posted(2, "body", "", "", "1001")}, labels: atReview}},
		{"a deleted comment whose author GitHub doesn't name hides it",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "", "Noted.")
				f.DeleteCommentBy("owner", 1002)
				f.AddReaction(1001, "owner", "+1")
			}),
			outcome{comments: []string{posted(2, "body", "", "", "1001")}, labels: atReview}},
		{"the owner deleting anyone else's comment hides nothing",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "mallory", "Noted.")
				f.DeleteCommentBy("owner", 1002)
				f.AddReaction(1001, "owner", "+1") // 1003
			}),
			outcome{comments: []string{approvedRev(1, "owner's 👍", "r1003", "1001")}, labels: planning}},
		{"an App comment deleted before the revision was posted hides nothing",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, app, "Noted.")
				f.DeleteCommentBy("owner", 1002)
				post(t, f, 1003, app, revision(2, "body"))
				f.AddReaction(1003, "owner", "+1") // 1004
			}),
			outcome{comments: []string{approvedRev(2, "owner's 👍", "r1004", "1003")}, labels: planning}},
		{"a deletion of the owner's comment by someone GitHub doesn't name hides nothing",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "Noted.")
				f.DeleteCommentBy("", 1002)
				f.AddReaction(1001, "owner", "+1") // 1003
			}),
			outcome{comments: []string{approvedRev(1, "owner's 👍", "r1003", "1001")}, labels: planning}},
	} {
		t.Run(tt.name, func(t *testing.T) { check(t, tt.setup, visitPass(t), tt.want) })
	}
}

// Through the loop, with the trust CODEOWNERS gives: the deletions are
// read, the old 👍 approves nothing, a fresh revision is posted and
// nothing is held: eve can read the repo, but not write to it, so her
// deletion is no discussion trigger (SPEC §6.4; mallory's is,
// TestDiscDeletionHolds), and her permission is a conditional read. Once the App's write is read back, an unchanged pass
// makes only conditional GETs, answered 304, and no history query: the
// deletions come in the snapshot's own GraphQL query. Then the owner's 👍
// on the fresh revision approves it.
func TestDeletedThroughTheLoop(t *testing.T) {
	f := newFake(t)
	f.SetPermission("eve", "read")
	n := fileAt(f, "spec-review", "body", "type:story") // comment 1001
	rev1 := f.AddComment(n, app, revision(1, "body"))   // 1002
	f.AddReaction(rev1, "owner", "+1")                  // 1003
	f.EditBody(n, "owner", "body v2")
	rev2 := f.AddComment(n, app, revision(2, "body v2")) // 1004
	f.EditBody(n, "owner", "body")
	rev3 := f.AddComment(n, app, revision(3, "body")) // 1005
	f.DeleteCommentBy("eve", rev3)
	f.DeleteCommentBy("owner", rev2)
	d := newDaemon(t, f, newOrigin(t), nil)
	d.Step.Reactions = daemon.ApproveByReaction(d.Step.CurrentTrust())

	snap, err := f.Client(t).Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Deleted) != 2 || snap.Deleted[0].By.Login != "eve" || snap.Deleted[1].By.Login != "owner" ||
		!state.IsApp(snap.Deleted[0].Author, slug) || !state.IsApp(snap.Deleted[1].Author, slug) {
		t.Fatalf("deleted = %+v; want eve's deletion of the App's comment, then the owner's", snap.Deleted)
	}

	iterate(t, d)
	comments := func() []string {
		var bodies []string
		for _, c := range f.State().Issues[n-1].Comments {
			bodies = append(bodies, c.Body)
		}
		return bodies
	}
	want := []string{"<!-- issuedog:transition from=intake to=spec-review run=rspec-review -->", revision(1, "body"), posted(2, "body", "", "", "1002")}
	if got := comments(); !slices.Equal(got, want) {
		t.Fatalf("after the first pass the comments are\n%q\nwant\n%q", got, want)
	}
	if l := f.State().Issues[n-1].Labels; !slices.Equal(l, []string{"stage:spec-review", "type:story"}) {
		t.Errorf("labels = %v; want the issue still at spec-review, not held", l)
	}
	iterate(t, d) // reads the App's revision back
	writes, histories, from := len(f.Writes()), f.HistoryQueries(), len(f.Requests())
	iterate(t, d)
	for _, r := range f.Requests()[from:] {
		if r.Method != "GET" || !r.Conditional || r.Status != 304 {
			t.Errorf("the unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}
	if len(f.Writes()) != writes || f.HistoryQueries() != histories {
		t.Errorf("the unchanged pass wrote %+v and sent %d history queries; want none", f.Writes()[writes:], f.HistoryQueries()-histories)
	}

	f.AddReaction(1006, "owner", "+1") // on the fresh revision
	iterate(t, d)
	got := comments()
	if len(got) != 4 || got[3] != approvedRev(2, "owner's 👍", "r1007", "1006") {
		t.Errorf("after the 👍 on the fresh revision the comments are\n%q\nwant its approval last", got)
	}
}

// A snapshot of an issue with deletions sends the same requests as one
// without: the deletions come in the comments' GraphQL query.
func TestDeletedNoExtraRequest(t *testing.T) {
	sent := func(del bool) []string {
		f := githubtest.New(t, "o/r", slug)
		n := f.AddIssue("owner", "Title", "body")
		id := f.AddComment(n, app, "Noted.")
		f.AddComment(n, "owner", "hi")
		if del {
			f.DeleteCommentBy("owner", id)
		}
		from := len(f.Requests())
		s, err := f.Client(t).Snapshot(context.Background(), "o", "r", n)
		if err != nil {
			t.Fatal(err)
		}
		if want := map[bool]int{false: 0, true: 1}[del]; len(s.Deleted) != want {
			t.Errorf("deleted = %+v; want %d", s.Deleted, want)
		}
		var reqs []string
		for _, r := range f.Requests()[from:] {
			if r.Path != "/app/installations/1/access_tokens" {
				reqs = append(reqs, r.Method+" "+r.Path)
			}
		}
		return reqs
	}
	without, with := sent(false), sent(true)
	if !slices.Equal(with, without) {
		t.Errorf("a snapshot with a deletion sent\n%q\nand one without\n%q; want the same", with, without)
	}
	if n := slices.Index(with, "POST /graphql"); n < 0 || slices.Index(with[n+1:], "POST /graphql") >= 0 {
		t.Errorf("the snapshot sent %q; want one GraphQL query", with)
	}
}
