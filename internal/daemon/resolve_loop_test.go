package daemon_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// resolveSteps runs resolvePass on each of ns in turn, as one Iterate
// visits them, failing t on an error.
func resolveSteps(t testing.TB, f *githubtest.Fake, ns ...int) {
	t.Helper()
	for _, n := range ns {
		if err := resolvePass(t, n)(f); err != nil {
			t.Fatal(err)
		}
	}
}

// resolveSettle runs passes over ns, in order, until one writes nothing.
func resolveSettle(t testing.TB, f *githubtest.Fake, ns ...int) {
	t.Helper()
	for i := 0; ; i++ {
		w := len(f.Writes())
		resolveSteps(t, f, ns...)
		if len(f.Writes()) == w {
			return
		}
		if i > 6 {
			t.Fatalf("still writing after %d rounds: %+v", i, f.Writes()[w:])
		}
	}
}

// resolveLabels checks each issue's labels: want[i] is #(i+1)'s stage.
func resolveLabels(t *testing.T, f *githubtest.Fake, when string, want ...string) {
	t.Helper()
	for i, stage := range want {
		if l := f.State().Issues[i].Labels; !slices.Equal(l, []string{"stage:" + stage}) {
			t.Errorf("%s: #%d: labels %q, newest comment %q; want %s", when, i+1, l, newestComment(f, i+1), stage)
		}
	}
}

// resolveRejectedWith reports whether the newest comment on issue n is the
// App's rejection of the command in comment id, for reason.
func resolveRejectedWith(f *githubtest.Fake, n int, id int64, reason string) bool {
	body := newestComment(f, n)
	return strings.Contains(body, "I didn't apply this command: "+reason+".") &&
		strings.HasSuffix(body, fmt.Sprintf("<!-- issuedog:reply reason=rejected cmd=%d -->", id))
}

// resolveLoop is the daemon over f, from origin o, which settle iterates
// until a pass writes nothing, and restart replaces with a new one.
type resolveLoop struct {
	t *testing.T
	f *githubtest.Fake
	o *origin
	d *daemon.Daemon
}

func newResolveLoop(t *testing.T, f *githubtest.Fake) *resolveLoop {
	o := newOrigin(t)
	return &resolveLoop{t: t, f: f, o: o, d: resolveDaemon(t, f, o)}
}

func (l *resolveLoop) restart() { l.d = resolveDaemon(l.t, l.f, l.o) }

func (l *resolveLoop) settle(when string) {
	l.t.Helper()
	for i := 0; ; i++ {
		w := len(l.f.Writes())
		iterate(l.t, l.d)
		if len(l.f.Writes()) == w {
			return
		}
		if i > 6 {
			l.t.Fatalf("%s: still writing after %d passes: %+v", when, i, l.f.Writes()[w:])
		}
	}
}

// task files a task at ready under parent, if any, and returns its number.
func (l *resolveLoop) task(parent int) int {
	n := fileAt(l.f, "ready", "do it")
	l.f.SetType(n, "task")
	if parent != 0 {
		l.f.SetParent(n, parent)
	}
	return n
}

// unchanged checks that one more pass of l writes nothing and costs no
// rate limit: every request is answered 304 (SPEC §8.1).
func (l *resolveLoop) unchanged(when string) {
	l.t.Helper()
	w, r := len(l.f.Writes()), len(l.f.Requests())
	iterate(l.t, l.d)
	if x := l.f.Writes()[w:]; len(x) != 0 {
		l.t.Errorf("%s: an unchanged pass wrote %+v", when, x)
	}
	for _, q := range l.f.Requests()[r:] {
		if q.Status != http.StatusNotModified {
			l.t.Errorf("%s: an unchanged pass sent %+v", when, q)
		}
	}
}

// delete has mallory delete comment id on issue n, then eve, who can only
// read, comment there, which holds nothing but makes the poll list the
// issue again, as the deletion alone doesn't in the fake.
func (l *resolveLoop) delete(n int, id int64) {
	l.f.DeleteCommentBy("mallory", id)
	l.f.SetPermission("eve", "read")
	l.f.AddComment(n, "eve", "Bump.")
}

// anyComment reports whether a comment on issue n contains sub.
func anyComment(f *githubtest.Fake, n int, sub string) bool {
	return slices.ContainsFunc(f.State().Issues[n-1].Comments, func(c githubtest.CommentState) bool { return strings.Contains(c.Body, sub) })
}

// stages checks each issue's stage, want[i] for issues[i].
func (l *resolveLoop) stages(when string, issues []int, want ...string) {
	l.t.Helper()
	for i, stage := range want {
		if got := l.f.State().Issues[issues[i]-1].Labels; !slices.Equal(got, []string{"stage:" + stage}) {
			l.t.Errorf("%s: #%d: labels %q, newest comment %q; want %s", when, issues[i], got, newestComment(l.f, issues[i]), stage)
		}
	}
}

// thumbNotice has the owner 👍 the App's notice of the deletion on issue
// n, which accepts it, if it's the deletion's only trigger.
func (l *resolveLoop) thumbNotice(n int) {
	l.t.Helper()
	ids := commentIDs(l.t, l.f, n)
	found := false
	for i, c := range l.f.State().Issues[n-1].Comments {
		if strings.Contains(c.Body, "issuedog:notice key=d") {
			l.f.AddReaction(ids[i], "owner", "+1")
			found = true
		}
	}
	if !found {
		l.t.Fatalf("#%d: no notice of the deletion; newest comment %q", n, newestComment(l.f, n))
	}
}

// resumeApplied reports whether the newest /resume on issue n has the
// App's 👍.
func (l *resolveLoop) resumeApplied(n int) bool {
	cs := l.f.State().Issues[n-1].Comments
	for i := len(cs) - 1; i >= 0; i-- {
		if cs[i].Body == "/resume" {
			for _, r := range cs[i].Reactions {
				if r == slug+"[bot] +1" {
					return true
				}
			}
			return false
		}
	}
	return false
}
