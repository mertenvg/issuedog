package daemon_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// trustOnceSpec files issue 1, by filer, whom CODEOWNERS doesn't name, at
// spec-review: owner3 edits the body, the App rewrites it whole, then
// owner2 and owner edit it, and revision 1 is of that last body. All three
// editors are code owners on o's main, so a lookup of any of them is a
// permission request. Each pass visits the issue, as it's at spec-review.
func trustOnceSpec(t *testing.T) (*githubtest.Fake, *origin) {
	t.Helper()
	f := newFake(t)
	for _, l := range []string{"owner2", "owner3"} {
		f.SetPermission(l, "write")
	}
	n := f.AddIssue("filer", "Spec", "an idea")
	f.AddLabel(n, app, "stage:spec-review")
	f.EditBody(n, "owner3", "an idea, put better")
	f.EditBody(n, app, rewriteSpec)
	f.EditBody(n, "owner2", "the spec")
	f.EditBody(n, "owner", "the spec, edited")
	f.AddComment(n, app, revision(1, "the spec, edited"))
	o := newOrigin(t)
	o.commit(".github/CODEOWNERS", "* @owner @owner2 @owner3\n")
	return f, o
}

// trustOnceAsked counts the permission requests in reqs, by login.
func trustOnceAsked(reqs []githubtest.Request) map[string]int {
	asked := map[string]int{}
	for _, r := range reqs {
		if l, ok := strings.CutPrefix(r.Path, "/collaborators/"); ok && strings.HasSuffix(l, "/permission") {
			asked[strings.TrimSuffix(l, "/permission")]++
		}
	}
	return asked
}

// A visit asks for each editor's permission once, however many of its
// calls find the issue's dirty content, and not at all for owner3, whose
// edit comes before the App's full rewrite (#174). It asks again on the
// next pass, unchanged as it is.
func TestIterateTrustOnceUnchanged(t *testing.T) {
	f, o := trustOnceSpec(t)
	d := newDaemon(t, f, o, nil)
	want := map[string]int{"owner": 1, "owner2": 1}
	for _, pass := range []string{"first", "second", "third"} {
		from := len(f.Requests())
		iterate(t, d)
		if got := trustOnceAsked(f.Requests()[from:]); !reflect.DeepEqual(got, want) {
			t.Errorf("the %s pass asked for permissions %v, want %v: owner and owner2 once each, owner3 never", pass, got, want)
		}
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("the passes wrote %+v; want none: the body is clean, its edits all trusted", w)
	}
}

// Trust isn't kept from one pass to the next: once owner2 leaves CODEOWNERS
// on main, the next pass finds owner2's edit dirty and notes it.
func TestIterateTrustOnceChangeBetweenPasses(t *testing.T) {
	f, o := trustOnceSpec(t)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	if w := f.Writes(); len(w) != 0 {
		t.Fatalf("the first pass wrote %+v; want none", w)
	}
	o.commit(".github/CODEOWNERS", "* @owner @owner3\n")
	iterate(t, d)
	if body := f.State().Issues[0].Body; !strings.HasPrefix(body, "<!-- issuedog:dirty by=owner2 -->\n") {
		t.Errorf("body after owner2 lost trust = %q, want it noted dirty by owner2", body)
	}
	o.commit(".github/CODEOWNERS", "* @owner @owner2 @owner3\n")
	writes := len(f.Writes())
	iterate(t, d)
	if body := f.State().Issues[0].Body; !strings.HasPrefix(body, "<!-- issuedog:dirty by=owner2 -->\n") || len(f.Writes()) != writes {
		t.Errorf("body once owner2 is trusted again = %q, writes %+v; want the note kept, as SPEC §5.1 says", body, f.Writes()[writes:])
	}
}

// No verdict outlives Iterate's visit: step 1 run outside one, as a caller
// other than Iterate runs it, asks Step.Trust as it is then.
func TestIterateTrustOnceEndsWithTheVisit(t *testing.T) {
	f, o := trustOnceSpec(t)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	d.Step.Trust = trusted{"owner": true}
	s, err := d.Step.Client.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Step.Run(context.Background(), s); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if body := f.State().Issues[0].Body; !strings.HasPrefix(body, "<!-- issuedog:dirty by=owner2 -->\n") {
		t.Errorf("body after Run with owner2 untrusted = %q, want it noted dirty by owner2", body)
	}
}

// A failed lookup fails the visit, wrapped, and counts as no verdict: the
// next pass reads the issue again and asks again.
func TestIterateTrustOnceLookupFails(t *testing.T) {
	f, o := trustOnceSpec(t)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Path == "/collaborators/owner2/permission" }, http.StatusInternalServerError)
	err := d.Iterate(context.Background())
	var er *gogithub.ErrorResponse
	if !errors.As(err, &er) || er.Response.StatusCode != http.StatusInternalServerError || !strings.Contains(err.Error(), "o/r#1") {
		t.Errorf("Iterate = %v, want the failed lookup's 500, for o/r#1", err)
	}
	from := len(f.Requests())
	iterate(t, d)
	if got, want := trustOnceAsked(f.Requests()[from:]), map[string]int{"owner": 1, "owner2": 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("the pass after the failure asked for permissions %v, want %v", got, want)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("the passes wrote %+v; want none", w)
	}
}

// Dirty doesn't ask about an editor whose edits all come before the body's
// last full rewrite: their trust decides nothing, so a lookup that would
// fail doesn't fail the call (#174).
func TestDirtyTrustOnceSkipsReplacedEditors(t *testing.T) {
	f := githubtest.New(t, "o/r", slug)
	n := f.AddIssue("filer", "Title", "an idea")
	f.EditBody(n, "mallory", "evil")
	f.EditBody(n, "Owner2", "the idea, put better")
	f.EditBody(n, app, rewriteSpec)
	f.EditBody(n, "owner", "spec v2")
	f.EditBody(n, "OWNER2", "spec v3")
	a, s, err := applier(t, f)
	if err != nil {
		t.Fatal(err)
	}
	trust := countTrust{trusted{"owner": true, "OWNER2": true}, map[string]int{}}
	d, err := a.Dirty(context.Background(), s, trust)
	if err != nil || !reflect.DeepEqual(d, daemon.Dirt{}) {
		t.Errorf("Dirty = %v, %v; want the body clean", d, err)
	}
	if want := map[string]int{"OWNER2": 1, "owner": 1}; !reflect.DeepEqual(trust.checks, want) {
		t.Errorf("trust checks = %v, want %v: none for mallory, nor Owner2's edit before the rewrite", trust.checks, want)
	}
	if d, err := a.Dirty(context.Background(), s, malloryFails{trusted{"owner": true, "OWNER2": true}}); err != nil || len(d) != 0 {
		t.Errorf("Dirty with mallory's lookup failing = %v, %v; want the body clean, and mallory not asked", d, err)
	}
	f.EditBody(n, "mallory", "evil again")
	s, err = a.Client.Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	if d, err := a.Dirty(context.Background(), s, trusted{"owner": true, "owner2": true}); err != nil || !reflect.DeepEqual(d, daemon.Dirt{state.BodyID: "mallory"}) {
		t.Errorf("Dirty after mallory's edit since the rewrite = %v, %v; want the body dirty by mallory", d, err)
	}
	if d, err := a.Dirty(context.Background(), s, malloryFails{trusted{"owner": true, "owner2": true}}); !errors.Is(err, errTrust) {
		t.Errorf("Dirty with mallory's lookup failing, after mallory's edit = %v, %v; want the error", d, err)
	}
}

// malloryFails fails a lookup of mallory, and asks trusted about others.
type malloryFails struct{ trusted }

func (m malloryFails) Trusted(ctx context.Context, login string) (bool, error) {
	if strings.EqualFold(login, "mallory") {
		return false, errTrust
	}
	return m.trusted.Trusted(ctx, login)
}
