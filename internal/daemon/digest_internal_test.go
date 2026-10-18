package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/logr/v2"

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
	t.Parallel()
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

// The App's issues titled as the digest, with no origin marker in the
// body, are judged after the poll's comments (digestTake), from one edit
// history query. Only those whose first version had no origin marker, and
// with no App comment that has one, edited or not, are digests: 3, whose
// marker an edit took out, and 5, closed, whose close is then never judged.
// 1's original post had one, 4's original text was deleted, and 2 and 6
// have an App comment with one, so their histories aren't read. An
// unchanged poll, and one that lists 1 again, reads no history; a new
// daemon's first poll reads it once more. Once 2's comment loses its
// origin, 2 is judged by its history, and taken.
func TestDigestTake(t *testing.T) {
	t.Parallel()
	const app, origin = "issuedog[bot]", "<!-- issuedog:origin plan=#9 rev=1 item=T1 -->"
	f := githubtest.New(t, "o/r", "issuedog")
	f.EditBody(f.AddIssue(app, "issuedog digest", "A task. "+origin), "owner", "A task.") // 1
	f.AddIssue(app, "issuedog digest", "No origin.")                                      // 2
	repost := f.AddComment(2, app, "Reposted. "+origin)
	f.EditBody(f.AddIssue(app, "issuedog digest", "<!-- issuedog:digest -->"), "owner", "gone") // 3
	f.EditBody(f.AddIssue(app, "issuedog digest", "A task. "+origin), "owner", "A task.")       // 4
	f.DeleteRevision(4, 0)
	f.CloseBy(f.AddIssue(app, "issuedog digest", "Closed.", "stage:ready"), "owner")            // 5: a close to judge, were it not a digest
	f.EditBody(f.AddIssue(app, "issuedog digest", "<!-- issuedog:digest -->"), "owner", "gone") // 6
	f.EditComment(f.AddComment(6, app, origin), "owner", "Edited. "+origin)
	newDaemon := func() *Daemon {
		return &Daemon{Step: &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}}, stale: map[int]bool{}}
	}
	poll := func(d *Daemon, histories int) {
		t.Helper()
		if err := d.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := f.HistoryQueries(); got != histories {
			t.Errorf("history queries = %d; want %d", got, histories)
		}
		if len(d.digest.titled) != 0 {
			t.Errorf("titled = %v; want every issue judged", d.digest.titled)
		}
	}
	d := newDaemon()
	poll(d, 1)
	if got := slices.Sorted(maps.Keys(d.digest.issues)); !slices.Equal(got, []int{3, 5}) {
		t.Errorf("digests = %v; want issues 3 and 5", got)
	}
	if got := slices.Sorted(maps.Keys(d.stale)); !slices.Equal(got, []int{1, 2, 4, 6}) {
		t.Errorf("stale = %v; want issues 1, 2, 4 and 6, in the pipeline", got)
	}
	if _, due := d.closingDue[5]; due {
		t.Errorf("closes due = %v; want none for 5, a digest", d.closingDue)
	}
	if got := slices.Sorted(maps.Keys(d.digest.firsts)); !slices.Equal(got, []int{1, 3, 4, 5}) {
		t.Errorf("first versions read for %v; want 1, 3, 4 and 5, but not 2 or 6", got)
	}
	poll(d, 1)
	f.AddComment(1, "owner", "Still a task.")
	poll(d, 1)
	if d.digest.has(1) || !d.stale[1] {
		t.Errorf("digests %v, stale %v; want 1 listed again, and still not a digest", d.digest.issues, d.stale)
	}
	d = newDaemon()
	poll(d, 2)
	if got := slices.Sorted(maps.Keys(d.digest.issues)); !slices.Equal(got, []int{3, 5}) {
		t.Errorf("a new daemon's digests = %v; want issues 3 and 5", got)
	}
	// The App's comment on 2 loses its origin to an edit, so it's
	// forgotten, and 2, listed again, is judged by its history.
	f.EditComment(repost, app, "Reposted.")
	f.AddComment(2, "owner", "Hello.")
	poll(d, 3)
	if got := slices.Sorted(maps.Keys(d.digest.issues)); !slices.Equal(got, []int{2, 3, 5}) {
		t.Errorf("digests = %v; want issues 2, 3 and 5", got)
	}
}

// A failed edit history read is logged, and the issue it was for isn't
// taken as the digest: it stays in the pipeline, and an unchanged poll
// reads nothing more. A poll that lists it again reads its history, and
// takes it, out of the pipeline. The poll after that reads nothing.
func TestDigestTakeHistoryFails(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logr.AddWriter).
	var logs bytes.Buffer
	stop := logr.AddWriter(&logs, logr.WithFilter(logr.E))
	defer stop()
	f := githubtest.New(t, "o/r", "issuedog")
	f.EditBody(f.AddIssue("issuedog[bot]", "issuedog digest", "<!-- issuedog:digest -->"), "owner", "gone")
	f.AddIssue("owner", "Newer", "So 1 isn't at the cursor, which every poll lists again.")
	d := &Daemon{Step: &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}}, stale: map[int]bool{}}
	poll := func(histories int) {
		t.Helper()
		if err := d.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := f.HistoryQueries(); got != histories {
			t.Errorf("history queries = %d; want %d", got, histories)
		}
	}
	f.FailGraphQL("Something went wrong")
	poll(1)
	logr.Wait()
	if d.digest.has(1) || !d.stale[1] || !strings.Contains(logs.String(), "o/r's issues titled \"issuedog digest\" failed to read") {
		t.Errorf("digests %v, stale %v, logs %q; want no digest, 1 stale, and the failure logged", d.digest.issues, d.stale, logs.String())
	}
	poll(1)
	if d.digest.has(1) {
		t.Errorf("digests %v; want none", d.digest.issues)
	}
	f.AddComment(1, "owner", "Hello.")
	poll(2)
	if !d.digest.has(1) || d.stale[1] {
		t.Errorf("digests %v, stale %v; want 1 the digest, out of the pipeline", d.digest.issues, d.stale)
	}
	poll(2)
}

// A poll whose comment listing fails, after its issue listing held issues
// only the title rule would take, keeps them: the next poll judges 1,
// which it doesn't list again, and takes it. 2, renamed in between, is
// judged on the next poll's listing alone, so it isn't taken.
func TestDigestTakeAfterFailedPoll(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	f.EditBody(f.AddIssue("issuedog[bot]", "issuedog digest", "<!-- issuedog:digest -->"), "owner", "gone")
	f.EditBody(f.AddIssue("issuedog[bot]", "issuedog digest", "<!-- issuedog:digest -->"), "owner", "gone")
	f.AddIssue("owner", "Newer", "So 1 isn't at the cursor.")
	d := &Daemon{Step: &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}}, stale: map[int]bool{}}
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Path == "/issues/comments" }, http.StatusInternalServerError)
	if err := d.poll(context.Background()); err == nil || d.digest.has(1) || d.digest.has(2) {
		t.Fatalf("poll = %v, digests %v; want the comment listing's error, and no digest yet", err, d.digest.issues)
	}
	f.RenameIssue(2, "owner", "Renamed")
	if err := d.poll(context.Background()); err != nil || !d.digest.has(1) || d.digest.has(2) || f.HistoryQueries() != 1 {
		t.Errorf("poll = %v, digests %v, history queries %d; want 1 the digest, from one query, and 2 not", err, d.digest.issues, f.HistoryQueries())
	}
}

// A digest marked to be read again, as a run's lease or a command that
// touched it would, is never read or visited: it stays out of the pipeline.
func TestDigestNeverReadAgain(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// digestRewriteDaemon is a daemon of o/r on f, notifying notify, that has
// polled once.
func digestRewriteDaemon(t *testing.T, f *githubtest.Fake, notify ...string) *Daemon {
	t.Helper()
	cfg := config.Defaults()
	cfg.Notify = notify
	d := &Daemon{Step: &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}}, Config: cfg, stale: map[int]bool{}}
	if err := d.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	return d
}

// An announcement is recorded as soon as it's posted: a second rewrite
// before any poll lists it announces nothing again (#158's verify).
func TestDigestRewriteRecordsAtOnce(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("issuedog[bot]", "issuedog digest", digestBody("", nil))
	d := digestRewriteDaemon(t, f, "owner")
	items := []waitItem{{Issue: 2, Kind: waitError, Key: "err-2-labels-0", Title: "T", Link: "https://github.com/o/r/issues/2"}}
	for range 2 {
		if err := d.digestRewrite(context.Background(), 1, items); err != nil {
			t.Fatal(err)
		}
	}
	if w := f.Writes(); len(w) != 2 || w[0].Path != "/issues/1/comments" || w[1].Method != http.MethodPatch {
		t.Errorf("writes = %+v; want one announcement and one edit", w)
	}
	if got := slices.Collect(maps.Keys(d.digest.announced(1))); !slices.Equal(got, []string{"err-2-labels-0"}) {
		t.Errorf("announced = %q; want the item", got)
	}
}

// An announcement that can't be rendered, for an item too big for any
// comment, is logged, and the body is still written, with no error.
func TestDigestRewriteNewsFails(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("issuedog[bot]", "issuedog digest", digestBody("", nil))
	d := digestRewriteDaemon(t, f, "owner")
	items := []waitItem{{Issue: 2, Kind: waitError, Key: "err-2-labels-0", Title: strings.Repeat("x", digestRenderLimit), Link: "https://github.com/o/r/issues/2"}}
	if err := d.digestRewrite(context.Background(), 1, items); err != nil {
		t.Fatalf("digestRewrite = %v; want no error", err)
	}
	if w := f.Writes(); len(w) != 1 || w[0].Method != http.MethodPatch || f.State().Issues[0].Body != digestBody("", items) {
		t.Errorf("writes = %+v; want the body edit alone", w)
	}
}

// An issue found gone on two reads loses its items, as one that closes
// does, and the digest stops listing it.
func TestDigestCacheGone(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	f.SetPermission("owner", "admin")
	f.AddIssue("issuedog[bot]", "issuedog digest", digestBody("", nil))
	n := f.AddIssue("owner", "Mine", "")
	f.AddLabel(n, "issuedog[bot]", "stage:ready")
	f.AddComment(n, "issuedog[bot]", "<!-- issuedog:transition from=intake to=ready run=r1 -->")
	f.AddComment(n, "issuedog[bot]", "Put back. <!-- issuedog:error kind=disagreement key=9 -->")
	st := &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}, Trust: ownerOnly{}}
	d := &Daemon{Step: st, Clone: repo.New(filepath.Join(t.TempDir(), "none"), filepath.Join(t.TempDir(), "r.git")), Config: config.Defaults(), Digest: true}
	if err := d.Iterate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(d.digestCache[n]) != 1 || !strings.Contains(f.State().Issues[0].Body, "1 item waits on you.") {
		t.Fatalf("cache %+v, body %q; want the error cached and listed", d.digestCache, f.State().Issues[0].Body)
	}
	f.DeleteIssue()
	d.reread(n, true) // one read found it gone already
	if err := d.Iterate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.digestCache[n]; ok || f.State().Issues[0].Body != digestBody("", nil) {
		t.Errorf("cache %+v, body %q; want the item gone", d.digestCache, f.State().Issues[0].Body)
	}
}

// A transferred digest's writes, which GitHub answers with a 301 that the
// client follows, and Go turns into a GET that succeeds, are each the
// digest gone, not a write (#160's verify): the edit, the announcement and
// the state change.
func TestDigestMovedRedirect(t *testing.T) {
	t.Parallel()
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path)
		body := `{"number": 1, "repository_url": "https://api.github.com/repos/x/y", "body": "old"}`
		switch {
		case strings.HasPrefix(r.URL.Path, "/repos/o/r/"):
			http.Redirect(w, r, "/repositories/9/"+strings.TrimPrefix(r.URL.Path, "/repos/o/r/"), http.StatusMovedPermanently)
			return
		case strings.HasSuffix(r.URL.Path, "/comments"):
			body = `[]`
		}
		if _, err := fmt.Fprint(w, body); err != nil {
			t.Errorf("write the answer: %v", err)
		}
	}))
	defer srv.Close()
	base := srv.URL + "/"
	// The server's own client, not http.DefaultTransport, which another
	// test's server would close the idle connections of.
	gc, err := gogithub.NewClient(gogithub.WithHTTPClient(srv.Client()), gogithub.WithURLs(&base, &base))
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{Step: &Step{Applier: &Applier{Client: &github.Client{Client: gc}, Owner: "o", Repo: "r", AppSlug: "issuedog"}}}
	d.digest.issues = map[int]digestSeen{1: {number: 1, open: true, body: "old"}}
	for name, write := range map[string]func() error{
		"edit":     func() error { return d.digestEdit(context.Background(), 1, "new") },
		"announce": func() error { return d.digestAnnounce(context.Background(), 1, "<!-- issuedog:digest-new items=a -->") },
		"state":    func() error { return digestGone(d.digestSetState(context.Background(), 1, "open")) },
	} {
		if err := write(); !errors.Is(err, digestErrGone) || !errors.Is(err, github.ErrTransferred) {
			t.Errorf("%s = %v; want the digest gone, transferred", name, err)
		}
	}
	if d.digest.issues[1].body != "old" || len(d.digest.news) != 0 || !d.digest.wrote.IsZero() {
		t.Errorf("digest state %+v; want nothing recorded as written", d.digest)
	}
	if !slices.Contains(got, "GET /repositories/9/issues/1") {
		t.Errorf("requests = %q; want the redirect followed with a GET", got)
	}
}

// digestMoved takes a write answered at this repo's path, by an issue of
// this repo or none, as not moved, matching the repo in any case.
func TestDigestMoved(t *testing.T) {
	t.Parallel()
	a := &Applier{Owner: "O", Repo: "r"}
	resp := func(path string) *gogithub.Response {
		u, err := url.Parse("https://api.github.com" + path)
		if err != nil {
			t.Fatal(err)
		}
		return &gogithub.Response{Response: &http.Response{Request: &http.Request{Method: http.MethodPatch, URL: u}}}
	}
	repoURL := func(u string) *gogithub.Issue { return &gogithub.Issue{RepositoryURL: &u} }
	for _, tc := range []struct {
		resp  *gogithub.Response
		is    *gogithub.Issue
		moved bool
	}{
		{nil, nil, false},
		{resp("/repos/o/R/issues/1"), repoURL("https://api.github.com/repos/o/R"), false},
		{resp("/api/v3/repos/o/r/issues/1"), nil, false},
		{resp("/repositories/9/issues/1"), nil, true},
		{resp("/repos/o/rr/issues/1"), nil, true},
		{nil, repoURL("https://api.github.com/repos/o/r2"), true},
		{&gogithub.Response{}, &gogithub.Issue{}, false},
	} {
		err := digestMoved(a, tc.resp, tc.is)
		if (err != nil) != tc.moved || err != nil && !errors.Is(err, github.ErrTransferred) {
			t.Errorf("digestMoved(%v, %v) = %v; want moved %t", tc.resp, tc.is.GetRepositoryURL(), err, tc.moved)
		}
	}
}

// digestTrust404 fails every lookup with a 404, as a permission check for
// a user GitHub doesn't know might.
type digestTrust404 struct{}

func (digestTrust404) Trusted(context.Context, string) (bool, error) {
	return false, fmt.Errorf("permission: %w", &gogithub.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}})
}

// A trust lookup that fails with a 404, while the close of the digest is
// resolved, is the pass's error, but not the digest gone: it's kept, and
// the issues aren't listed again.
func TestDigestTrust404NotGone(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("issuedog[bot]", "issuedog digest", digestBody("", nil))
	f.CloseBy(1, "owner")
	f.ReopenBy(1, "owner")
	st := &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}, Trust: digestTrust404{}}
	d := &Daemon{Step: st, Clone: repo.New(filepath.Join(t.TempDir(), "none"), filepath.Join(t.TempDir(), "r.git")), Config: config.Defaults(), Digest: true}
	err := d.Iterate(context.Background())
	if err == nil || errors.Is(err, digestErrGone) || !strings.Contains(err.Error(), "trust owner") {
		t.Fatalf("Iterate = %v; want the trust lookup's error, not the digest gone", err)
	}
	if !d.digest.has(1) || d.issuesSince.IsZero() {
		t.Errorf("digests %v, issues cursor %v; want the digest kept, and the cursor too", d.digest.issues, d.issuesSince)
	}
}

// digestGone marks only a 404, a 410 or a transfer, and keeps the error.
func TestDigestGone(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// A reopen waited for is kept per digest (lateReopenSeen): another digest,
// even with no close or reopen event either, waits from its own first pass,
// and so does the same digest found again after it was dropped.
func TestLateReopenSeenPerDigest(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	d := digestRewriteDaemon(t, f)
	now := d.Step.Client.Now()
	long := lateReopenSeen{number: 1, since: now.Add(-time.Hour)}
	d.digest.late = long
	if !d.lateReopen(digestSeen{number: 2, open: true, reason: "reopened", updated: now}, 0, now) {
		t.Errorf("digest 2 doesn't wait, with digest 1's wait from an hour ago kept; want its own wait")
	}
	d.digest.late = long
	d.digestDrop(1, errors.New("404"))
	if !d.lateReopen(digestSeen{number: 1, open: true, reason: "reopened", updated: now}, 0, now) {
		t.Errorf("digest 1, found again after a drop, doesn't wait; want a new wait")
	}
}

// The wait ends at lateReopenWait exactly, by the clock given: a
// nanosecond before, the digest still waits.
func TestLateReopenLastInstant(t *testing.T) {
	t.Parallel()
	d := &Daemon{}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := digestSeen{number: 1, open: true, reason: "reopened", updated: start}
	if !d.lateReopen(s, 7, start) {
		t.Fatalf("no wait at its start; want one")
	}
	if !d.lateReopen(s, 7, start.Add(lateReopenWait-time.Nanosecond)) {
		t.Errorf("no wait a nanosecond before lateReopenWait; want the wait still on")
	}
	if d.lateReopen(s, 7, start.Add(lateReopenWait)) {
		t.Errorf("still waiting at lateReopenWait exactly; want the wait over")
	}
}
