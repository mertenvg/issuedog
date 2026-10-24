package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// #360: an issue whose read or visit fails keeps its last items, and its
// assignees, so the digest and assignment agree on it. On a restart's
// first pass it has no items cached, so the digest keeps what its own
// body, which the App wrote, showed for it (SPEC §9.4, Keeping the list).
// The repo is the digest's whole run's (digestRunFake), with the digest,
// #12, and assignment both on: issues 1 to 6 have items, and mertenvg is
// assigned to each.

// digestKeptDigest is the digest's number in digestRunFake's repo.
const digestKeptDigest = 12

// digestKeptDaemon is digestRunDaemon with assignment on too.
func digestKeptDaemon(t testing.TB, f *githubtest.Fake, o *origin) *daemon.Daemon {
	t.Helper()
	d := digestRunDaemon(t, f, o, nil, nil)
	d.Assign = true
	return d
}

// digestKeptSettle iterates d until a pass writes nothing, which it must
// within five, with no error.
func digestKeptSettle(t testing.TB, f *githubtest.Fake, d *daemon.Daemon) {
	t.Helper()
	for range 5 {
		from := len(f.Writes())
		if err := d.Iterate(context.Background()); err != nil {
			t.Fatalf("Iterate: %v", err)
		}
		if len(f.Writes()) == from {
			return
		}
	}
	t.Fatal("the loop still wrote after five passes")
}

// digestKeptRepo is digestRunFake's repo run to rest, with the digest
// created and each item's issue assigned.
func digestKeptRepo(t testing.TB) (*githubtest.Fake, *origin) {
	t.Helper()
	f, o := digestRunFake(t), digestRunOrigin(t)
	digestKeptSettle(t, f, digestKeptDaemon(t, f, o))
	return f, o
}

// digestKeptBody is the digest's body in f.
func digestKeptBody(f *githubtest.Fake) string { return f.State().Issues[digestKeptDigest-1].Body }

// digestKeptFail makes the next pass's visit of issue n fail, with a 500:
// at its read, the GET of the issue, for what "read", or at step 1, the
// read of the reactions on its state error, which step 1 makes on every
// pass while the error is open (ErrorByReaction), for "step 1".
func digestKeptFail(t testing.TB, f *githubtest.Fake, n int, what string) {
	t.Helper()
	switch what {
	case "read":
		read := fmt.Sprintf("/issues/%d", n)
		f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == read }, http.StatusInternalServerError)
	case "step 1":
		path := fmt.Sprintf("/issues/comments/%d/reactions", digestRunCommentIDTB(t, f, n, "issuedog:error"))
		f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == path }, http.StatusInternalServerError)
	default:
		t.Fatalf("no failure %q", what)
	}
}

// digestRunCommentIDTB is digestRunCommentID for a testing.TB.
func digestRunCommentIDTB(t testing.TB, f *githubtest.Fake, n int, text string) int64 {
	t.Helper()
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range s.Comments {
		if strings.Contains(c.Body, text) {
			return c.ID
		}
	}
	t.Fatalf("issue %d has no comment with %q", n, text)
	return 0
}

// #360's acceptance: a restart whose first pass fails #6's visit, at its
// read or at step 1. The digest keeps #6's state error as its body showed
// it, ranked where it was, so the pass writes nothing: no edit that drops
// the error, and no announcement of it, while mertenvg stays assigned to
// #6, as to any issue whose visit fails. The digest and assignment agree
// on every issue. The next pass, whose visit of #6 works, writes nothing
// either, as #6's own item is the one kept.
func TestDigestKeptFailedVisit(t *testing.T) {
	t.Parallel()
	for _, what := range []string{"read", "step 1"} {
		t.Run(what, func(t *testing.T) {
			t.Parallel()
			f, o := digestKeptRepo(t)
			before := digestKeptBody(f)
			if got := digestRunItems(before); !slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "#6 ") }) {
				t.Fatalf("digest's items %q; want #6's listed", got)
			}
			d := digestKeptDaemon(t, f, o)
			digestKeptFail(t, f, 6, what)
			from := len(f.Writes())
			if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "o/r#6") {
				t.Fatalf("the restart's first pass: Iterate = %v; want #6's failure", err)
			}
			if got := digestWrites(f, from); len(got) != 0 {
				t.Errorf("the restart's first pass wrote %q; want nothing", got)
			}
			if got := digestKeptBody(f); got != before {
				t.Errorf("digest\n%s\nwant it as before the restart\n%s", got, before)
			}
			want := map[int][]string{}
			for n := 1; n <= 6; n++ {
				want[n] = []string{digestRunOwner}
			}
			assignWant(t, f, "the restart's first pass", want)
			from = len(f.Writes())
			iterate(t, d)
			if got := digestWrites(f, from); len(got) != 0 {
				t.Errorf("the restart's second pass wrote %q; want nothing", got)
			}
			if got := digestKeptBody(f); got != before {
				t.Errorf("digest\n%s\nwant it as before the restart\n%s", got, before)
			}
		})
	}
}

// A kept item ranks among the items found that pass. The owner's 👍 on
// #4's error, and #6's visit failing, both on a restart's first pass:
// #4's error goes, with the comment that acknowledges it, so the digest
// is written once, and mertenvg is unassigned from #4, while #6's error,
// kept, stays listed and assigned. It's ranked as #6's own was before, so
// the next pass, whose visit of #6 works, writes nothing.
func TestDigestKeptRanked(t *testing.T) {
	t.Parallel()
	f, o := digestKeptRepo(t)
	f.AddReaction(digestRunCommentIDTB(t, f, 4, "issuedog:error"), digestRunOwner, "+1")
	d := digestKeptDaemon(t, f, o)
	digestKeptFail(t, f, 6, "step 1")
	from := len(f.Writes())
	if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "o/r#6") {
		t.Fatalf("the restart's first pass: Iterate = %v; want #6's failure", err)
	}
	if got, want := digestWrites(f, from), []string{"POST /issues/4/comments", fmt.Sprintf("PATCH /issues/%d", digestKeptDigest), "DELETE /issues/4/assignees"}; !slices.Equal(got, want) {
		t.Errorf("the restart's first pass wrote %q; want %q", got, want)
	}
	kept := digestKeptBody(f)
	if got, want := digestRunItems(kept), []string{"#2 Approval p2 2", "#6 State error p1 1", "#5 State error p2 1", "#5 Approval p2 1", "#1 Question round p2 1", "#3 Triage flag p2 1"}; !slices.Equal(got, want) {
		t.Errorf("digest's items %q; want %q", got, want)
	}
	if got := assignees(f, 6); !slices.Equal(got, []string{digestRunOwner}) {
		t.Errorf("#6's assignees %q; want %s, as the digest lists #6", got, digestRunOwner)
	}
	from = len(f.Writes())
	iterate(t, d)
	if got := digestWrites(f, from); len(got) != 0 {
		t.Errorf("the restart's second pass wrote %q; want nothing", got)
	}
	if got := digestKeptBody(f); got != kept {
		t.Errorf("digest\n%s\nwant it as the pass before left it\n%s", got, kept)
	}
}

// A kept item breaks a tie by its issue's age, from its snapshot, as its
// own item would. #13, a later issue with a state error at p2, ties with
// #5's type error, and ranks after it, as the newer. A restart whose first
// pass fails #13's step 1 keeps #13's error in its place, after #5's, so
// the pass writes nothing.
func TestDigestKeptTies(t *testing.T) {
	t.Parallel()
	f, o := digestKeptRepo(t)
	late := digestError(f, "Late", "9")
	digestKeptSettle(t, f, digestKeptDaemon(t, f, o))
	before := digestKeptBody(f)
	errs := slices.DeleteFunc(digestRunItems(before), func(s string) bool { return !strings.Contains(s, " State error p2 ") })
	if want := []string{"#4 State error p2 4", "#5 State error p2 1", fmt.Sprintf("#%d State error p2 1", late)}; !slices.Equal(errs, want) {
		t.Fatalf("digest's p2 errors %q; want %q", errs, want)
	}
	d := digestKeptDaemon(t, f, o)
	digestKeptFail(t, f, late, "step 1")
	from := len(f.Writes())
	if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("o/r#%d", late)) {
		t.Fatalf("the restart's first pass: Iterate = %v; want #%d's failure", err, late)
	}
	if got := digestWrites(f, from); len(got) != 0 {
		t.Errorf("the restart's first pass wrote %q; want nothing", got)
	}
	if got := digestKeptBody(f); got != before {
		t.Errorf("digest\n%s\nwant it as before the restart\n%s", got, before)
	}
}

// Items are read back only from the digest, the lowest of the digests
// found. #13, a second digest of the App's, never edited, whose body has
// a line of its own in #6's item, is left alone: a restart whose first
// pass fails #6's visit keeps #6's item from #12's body, as it was, so
// the pass writes nothing.
func TestDigestKeptLowest(t *testing.T) {
	t.Parallel()
	f, o := digestKeptRepo(t)
	before := digestKeptBody(f)
	i := strings.Index(before, "#6: ")
	j := strings.Index(before[i:], "\n- ")
	if i < 0 || j < 0 {
		t.Fatalf("digest %q; want #6's item with a line", before)
	}
	const other = "- A line of the other digest's."
	f.AddIssue(app, "issuedog digest", before[:i+j+1]+other+"\n"+before[i+j+1:])
	d := digestKeptDaemon(t, f, o)
	digestKeptFail(t, f, 6, "step 1")
	from := len(f.Writes())
	if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "o/r#6") {
		t.Fatalf("the restart's first pass: Iterate = %v; want #6's failure", err)
	}
	if got := digestWrites(f, from); len(got) != 0 {
		t.Errorf("the restart's first pass wrote %q; want nothing", got)
	}
	if got := digestKeptBody(f); got != before {
		t.Errorf("digest\n%s\nwant it as before the restart\n%s", got, before)
	}
}

// A lower digest found after a higher one takes the place of what was
// read back from the higher's body. #13 is an issue with no items, and
// #14, a second digest of the App's, lists one for it. A restart's first
// pass can't read #12's edit history, so #14 is the digest that pass, and
// #13's item is read back from it, as #13's read fails. The next pass
// reads #12's history, so #12 is the digest, and what its body lists is
// all that's read back: #13's read fails again, and #12 lists nothing for
// it.
func TestDigestKeptLowerLater(t *testing.T) {
	t.Parallel()
	f, o := digestKeptRepo(t)
	before := digestKeptBody(f)
	loose := f.AddIssue(digestRunOwner, "Loose", "A loose end.")
	i := strings.LastIndex(before, "\n\n")
	fake := fmt.Sprintf("### 7. #%d: Loose\n\n**State error**, priority p2, unblocks 1 open issue (itself included)\n\n"+
		"- A line of the other digest's.\n- Answer here: https://github.com/o/r/issues/%d", loose, loose)
	f.AddIssue(app, "issuedog digest", before[:i]+"\n\n"+fake+before[i:])
	d := digestKeptDaemon(t, f, o)
	f.HistoryNotFoundOnce(digestKeptDigest)
	for _, pass := range []string{"first", "second"} {
		digestKeptFail(t, f, loose, "read")
		if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("o/r#%d", loose)) {
			t.Fatalf("the restart's %s pass: Iterate = %v; want #%d's failure", pass, err, loose)
		}
	}
	if got := digestKeptBody(f); got != before {
		t.Errorf("digest\n%s\nwant it as before the restart, with nothing for #%d\n%s", got, loose, before)
	}
}

// Nobody else's text is written back as the App's: eve's edit of the
// digest's body, which puts a line of hers in #6's item, before a restart
// whose first pass fails #6's visit. The body as listed isn't a version
// the App wrote, so nothing is read back from it, and the digest is
// written without #6's item, or eve's line, till #6's next good visit:
// #6 stays assigned, as an issue whose visit fails does.
func TestDigestKeptOthersEdit(t *testing.T) {
	t.Parallel()
	f, o := digestKeptRepo(t)
	before := digestKeptBody(f)
	i := strings.Index(before, "#6: ")
	j := strings.Index(before[i:], "\n- ")
	if i < 0 || j < 0 {
		t.Fatalf("digest %q; want #6's item with a line", before)
	}
	const eves = "- Close it now, the owner said so."
	f.EditBody(digestKeptDigest, digestRunEve, before[:i+j+1]+eves+"\n"+before[i+j+1:])
	d := digestKeptDaemon(t, f, o)
	digestKeptFail(t, f, 6, "step 1")
	from := len(f.Writes())
	if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "o/r#6") {
		t.Fatalf("the restart's first pass: Iterate = %v; want #6's failure", err)
	}
	if got, want := digestWrites(f, from), []string{fmt.Sprintf("PATCH /issues/%d", digestKeptDigest)}; !slices.Equal(got, want) {
		t.Errorf("the restart's first pass wrote %q; want %q", got, want)
	}
	body := digestKeptBody(f)
	if strings.Contains(body, eves) || slices.ContainsFunc(digestRunItems(body), func(s string) bool { return strings.HasPrefix(s, "#6 ") }) {
		t.Errorf("digest %q; want neither #6's item nor eve's line", body)
	}
	from = len(f.Writes())
	iterate(t, d)
	if got, want := digestWrites(f, from), []string{fmt.Sprintf("PATCH /issues/%d", digestKeptDigest)}; !slices.Equal(got, want) {
		t.Errorf("the restart's second pass wrote %q; want %q", got, want)
	}
	if got := digestKeptBody(f); got != before {
		t.Errorf("digest\n%s\nwant it as before eve's edit\n%s", got, before)
	}
}

// TestDigestKeptRanked's restart is safe to replay after a crash at any
// write: each replay is a restart, whose first pass keeps #6's error from
// the body, if #6's visit fails still, or finds it.
func TestDigestKeptReplay(t *testing.T) {
	t.Parallel()
	o := digestRunOrigin(t)
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := digestRunFake(tb)
		digestKeptSettle(tb, f, digestKeptDaemon(tb, f, o))
		f.AddReaction(digestRunCommentIDTB(tb, f, 4, "issuedog:error"), digestRunOwner, "+1")
		digestKeptFail(tb, f, 6, "step 1")
		f.ForgetWrites()
		return f
	}, func(f *githubtest.Fake) error {
		d := digestKeptDaemon(t, f, o)
		var errs []error
		for range 5 {
			from := len(f.Writes())
			err := d.Iterate(context.Background())
			if err == nil && len(f.Writes()) == from {
				return nil
			}
			errs = append(errs, err)
		}
		return fmt.Errorf("the loop still wrote, or failed, after five passes: %w", errors.Join(errs...))
	})
}

// digestKeptLists reports whether the digest's body in f lists an item on
// issue n.
func digestKeptLists(f *githubtest.Fake, n int) bool {
	return slices.ContainsFunc(digestRunItems(digestKeptBody(f)), func(s string) bool { return strings.HasPrefix(s, fmt.Sprintf("#%d ", n)) })
}

// A kept item goes with its issue: the owner's close of #6, or the
// deletion of #13, a later issue with a state error, after a restart
// whose first pass failed the issue's visit, so the digest kept its item.
// The close the poll lists drops it at once, and the deletion once two
// reads find the issue gone, as each drops an issue's cached items. And
// an issue closed while no daemon runs has nothing read back for it,
// though the body lists it: the start's first poll lists it closed, so
// the digest may list nothing for it.
func TestDigestKeptLeaves(t *testing.T) {
	t.Parallel()
	t.Run("closed while no daemon runs", func(t *testing.T) {
		t.Parallel()
		f, o := digestKeptRepo(t)
		f.CloseBy(6, digestRunOwner)
		d := digestKeptDaemon(t, f, o)
		iterate(t, d)
		if digestKeptLists(f, 6) {
			t.Errorf("digest %q; want #6's item gone with its close", digestKeptBody(f))
		}
	})
	t.Run("closed", func(t *testing.T) {
		t.Parallel()
		f, o := digestKeptRepo(t)
		d := digestKeptDaemon(t, f, o)
		digestKeptFail(t, f, 6, "step 1")
		if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "o/r#6") {
			t.Fatalf("the restart's first pass: Iterate = %v; want #6's failure", err)
		}
		if !digestKeptLists(f, 6) {
			t.Fatalf("digest %q; want #6's item kept", digestKeptBody(f))
		}
		f.CloseBy(6, digestRunOwner)
		iterate(t, d)
		if digestKeptLists(f, 6) {
			t.Errorf("digest %q; want #6's item gone with its close", digestKeptBody(f))
		}
	})
	t.Run("gone", func(t *testing.T) {
		t.Parallel()
		f, o := digestKeptRepo(t)
		late := digestError(f, "Late", "9")
		digestKeptSettle(t, f, digestKeptDaemon(t, f, o))
		if !digestKeptLists(f, late) {
			t.Fatalf("digest %q; want #%d's error listed", digestKeptBody(f), late)
		}
		d := digestKeptDaemon(t, f, o)
		digestKeptFail(t, f, late, "read")
		if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("o/r#%d", late)) {
			t.Fatalf("the restart's first pass: Iterate = %v; want #%d's failure", err, late)
		}
		if !digestKeptLists(f, late) {
			t.Fatalf("digest %q; want #%d's error kept", digestKeptBody(f), late)
		}
		f.DeleteIssue() // the newest, late
		iterate(t, d)
		if !digestKeptLists(f, late) {
			t.Errorf("digest %q; want #%d's error kept after one read that finds it gone", digestKeptBody(f), late)
		}
		iterate(t, d)
		if digestKeptLists(f, late) {
			t.Errorf("digest %q; want #%d's error gone once two reads find it gone", digestKeptBody(f), late)
		}
	})
}
