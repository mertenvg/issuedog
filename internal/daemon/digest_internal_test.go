package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"testing"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/repo"
)

// The poll records the digests and the App's digest-new comments on any
// issue. An edited one counts for nothing, nor does one by anyone else. A
// comment whose marker an edit took out is forgotten. A digest whose marker
// and title edits changed stays one, but a new daemon finds it no more: the
// gap listed documents. One of the App's titled as the digest, with no
// marker, is found, unless it has an origin marker, and so is one of the
// App's with the marker and another title. Another bot's never is.
func TestDigestPollRecords(t *testing.T) {
	const app = "issuedog[bot]"
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue(app, "issuedog digest", "<!-- issuedog:digest -->")
	f.AddIssue("owner", "Mine", "<!-- issuedog:digest -->")
	first := f.AddComment(1, app, "New. <!-- issuedog:digest-new items=q-1-1,rev-1-2 -->")
	f.EditComment(f.AddComment(1, app, "<!-- issuedog:digest-new items=edited -->"), "owner", "<!-- issuedog:digest-new items=edited -->")
	f.AddComment(1, "owner", "<!-- issuedog:digest-new items=owners -->")
	f.AddComment(2, app, "<!-- issuedog:digest-new items=elsewhere -->")
	f.SetPullRequest(f.AddIssue(app, "A pull request", "<!-- issuedog:digest -->"))
	f.AddIssue(app, "issuedog digest", "Its marker was taken out.")                              // 4: a digest
	f.AddIssue(app, "issuedog digest", "A task. <!-- issuedog:origin plan=#1 rev=1 item=T1 -->") // 5: not one
	f.AddIssue(app, "A task", "No marker.")                                                      // 6: not one
	f.AddIssue(app, "Renamed", "<!-- issuedog:digest -->")                                       // 7: a digest
	f.AddIssue("other[bot]", "issuedog digest", "<!-- issuedog:digest -->")                      // 8: not one
	newDaemon := func() *Daemon {
		return &Daemon{Step: &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}}, stale: map[int]bool{}}
	}
	d := newDaemon()
	poll := func(d *Daemon) {
		t.Helper()
		if err := d.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	poll(d)
	keys := func(n int) []string { return slices.Sorted(maps.Keys(d.digest.announced(n))) }
	if got := slices.Sorted(maps.Keys(d.digest.issues)); !slices.Equal(got, []int{1, 4, 7}) || !slices.Equal(keys(1), []string{"q-1-1", "rev-1-2"}) ||
		!slices.Equal(keys(2), []string{"elsewhere"}) {
		t.Errorf("digests %v, announced on 1 %q and on 2 %q; want issues 1, 4 and 7, q-1-1 and rev-1-2 on 1, and elsewhere on 2", got, keys(1), keys(2))
	}
	if got := slices.Sorted(maps.Keys(d.stale)); !slices.Equal(got, []int{2, 5, 6, 8}) {
		t.Errorf("stale = %v; want issues 2, 5, 6 and 8, none of them digests", got)
	}
	f.EditComment(first, app, "No marker now.")
	f.EditBody(1, "mallory", "No marker now.")
	f.RenameIssue(1, "mallory", "Renamed")
	poll(d)
	if !d.digest.has(1) || d.stale[1] || len(keys(1)) != 0 {
		t.Errorf("digests %v, stale %v, announced on 1 %q; want issue 1 still a digest, not stale, and the comment forgotten",
			d.digest.issues, d.stale, keys(1))
	}
	d = newDaemon()
	poll(d)
	if got := slices.Sorted(maps.Keys(d.digest.issues)); !slices.Equal(got, []int{4, 7}) {
		t.Errorf("a new daemon's digests = %v; want issues 4 and 7", got)
	}
}

// A digest marked to be read again, as a run's lease or a command that
// touched it would, is never read or visited: it stays out of the pipeline.
func TestDigestNeverReadAgain(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("issuedog[bot]", "issuedog digest", "<!-- issuedog:digest -->")
	f.AddIssue("owner", "Mine", "")
	var visits []int
	st := &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}, Trust: ownerOnly{},
		Reactions: func(_ context.Context, _ *Applier, s *github.Snapshot, _ Dirt) error {
			visits = append(visits, s.Number)
			return nil
		}}
	// The clone can't sync, so the trust and config given stay.
	d := &Daemon{Step: st, Clone: repo.New(filepath.Join(t.TempDir(), "none"), filepath.Join(t.TempDir(), "r.git")), Config: config.Defaults()}
	for i := range 2 {
		if i > 0 {
			d.reread(1, false)
		}
		if err := d.Iterate(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if d.snaps[1] != nil || d.stale[1] || !slices.Equal(visits, []int{2}) {
		t.Errorf("snapshot %v, stale %v, visits %v; want no snapshot of the digest, and only issue 2 visited", d.snaps[1], d.stale, visits)
	}
}

// digestEdit writes a body once: the body it wrote is the one it compares
// with until the poll lists the digest again, as T8 relies on.
func TestDigestEditOnce(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("issuedog[bot]", "issuedog digest", "<!-- issuedog:digest -->")
	d := &Daemon{Step: &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}}, stale: map[int]bool{}}
	if err := d.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := d.digestEdit(context.Background(), 1, "new <!-- issuedog:digest -->"); err != nil {
			t.Fatal(err)
		}
	}
	if w := f.Writes(); len(w) != 1 || f.State().Issues[0].Body != "new <!-- issuedog:digest -->" {
		t.Errorf("writes = %+v; want one edit, to the new body", w)
	}
}

// A body GitHub keeps with "\r\n" line breaks, or space around it, is
// the same body: digestEdit writes nothing for it.
func TestDigestEditNormalised(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("issuedog[bot]", "issuedog digest", "Nothing is waiting on you.\r\n\r\n<!-- issuedog:digest -->\r\n")
	d := &Daemon{Step: &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}}, stale: map[int]bool{}}
	if err := d.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := d.digestEdit(context.Background(), 1, digestBody("", nil)); err != nil {
		t.Fatal(err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes = %+v; want none for the same body", w)
	}
	for a, b := range map[string]string{"a\r\nb": "a\nb", " a\n": "a", "a\n\nb": "a\r\n\r\nb"} {
		if !digestSameBody(a, b) {
			t.Errorf("digestSameBody(%q, %q) = false, want true", a, b)
		}
	}
	for a, b := range map[string]string{"a\nb": "a b", "a\n\nb": "a\nb", "a": "A"} {
		if digestSameBody(a, b) {
			t.Errorf("digestSameBody(%q, %q) = true, want false", a, b)
		}
	}
}

// digestRewrite writes the body with the pin note this start's check found
// for this digest, and with none when the check was for another, so an
// edit that took the marker out is put right.
func TestDigestRewriteNote(t *testing.T) {
	for _, tc := range []struct {
		pinned int
		want   string
	}{
		{1, "Note.\n\n" + digestBody("", nil)},
		{2, digestBody("", nil)},
	} {
		f := githubtest.New(t, "o/r", "issuedog")
		f.AddIssue("issuedog[bot]", "issuedog digest", "gone")
		d := &Daemon{Step: &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}}, stale: map[int]bool{}}
		if err := d.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
		d.digest.pinned, d.digest.note = tc.pinned, "Note."
		if err := d.digestRewrite(context.Background(), 1, nil); err != nil {
			t.Fatal(err)
		}
		if got := f.State().Issues[0].Body; got != tc.want {
			t.Errorf("checked #%d: body = %q, want %q", tc.pinned, got, tc.want)
		}
	}
}

// digestGone marks only a 404, a 410 or a transfer, and keeps the error.
func TestDigestGone(t *testing.T) {
	status := func(code int) error {
		return fmt.Errorf("edit: %w", &gogithub.ErrorResponse{Response: &http.Response{StatusCode: code}})
	}
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{status(http.StatusNotFound), true},
		{status(http.StatusGone), true},
		{fmt.Errorf("read: %w", github.ErrTransferred), true},
		{status(http.StatusForbidden), false},
		{status(http.StatusInternalServerError), false},
		{errors.New("boom"), false},
	} {
		got := digestGone(tc.err)
		if errors.Is(got, digestErrGone) != tc.want || !errors.Is(got, tc.err) {
			t.Errorf("digestGone(%v) = %v; want gone %t, wrapping it", tc.err, got, tc.want)
		}
	}
}

// digestTrustAll trusts every login it's asked about.
type digestTrustAll struct{}

func (digestTrustAll) Trusted(context.Context, string) (bool, error) { return true, nil }

// No close or reopen by a bot, the App's included, or a ghost stands, even
// where trust would say yes; a trusted user's does.
func TestDigestTrusted(t *testing.T) {
	d := &Daemon{Step: &Step{Applier: &Applier{AppSlug: "issuedog"}, Trust: digestTrustAll{}}}
	for _, tc := range []struct {
		actor *gogithub.User
		want  bool
	}{
		{nil, false},
		{&gogithub.User{Login: new("issuedog[bot]"), Type: new("Bot")}, false},
		{&gogithub.User{Login: new("other[bot]"), Type: new("Bot")}, false},
		{&gogithub.User{Login: new("owner"), Type: new("User")}, true},
	} {
		if got, err := d.digestTrusted(context.Background(), tc.actor); err != nil || got != tc.want {
			t.Errorf("digestTrusted(%s) = %t, %v; want %t", tc.actor.GetLogin(), got, err, tc.want)
		}
	}
}

// The pin note names GitHub's message alone when the error wraps
// github.ErrGraphQL, and the whole error otherwise, on one line.
func TestDigestPinNote(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("pin o/r#1: %w: Resource not accessible", github.ErrGraphQL),
			"I couldn't pin this issue: `Resource not accessible`. Pin it by hand."},
		{errors.New("two\nlines"), "I couldn't pin this issue: `two lines`. Pin it by hand."},
	} {
		if got := digestPinNote(tc.err); got != tc.want {
			t.Errorf("digestPinNote(%q) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
