package daemon_test

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// The digest's identity marker counts only in a version of its body the
// App wrote (SPEC §5.1, §9.4, #498): an untrusted write collaborator who
// adds it to another App issue doesn't take that issue out of the
// pipeline, and the real digest is still found on a restart.

// digestMarkerTrustMarker is the identity marker mallory adds.
const digestMarkerTrustMarker = "<!-- issuedog:digest -->"

// digestMarkerTrustFake is o/r with A, #1, an App task at ready with an
// origin marker, the parent of J, #2, the owner's task at ready, and the
// pinned digest, #3, numbered above A, so A would win as the lowest
// digest were it taken for one.
func digestMarkerTrustFake(t testing.TB) *githubtest.Fake {
	t.Helper()
	f := newFake(t)
	for _, author := range []string{app, "owner"} {
		n := f.AddIssue(author, "Part", digestTaskBody)
		if author == "owner" {
			f.EditBody(n, "owner", "A part.")
		}
		f.AddLabel(n, "owner", "type:task")
		f.AddLabel(n, app, "stage:ready")
		f.AddComment(n, app, "<!-- issuedog:transition from=intake to=ready run=rready -->")
	}
	f.SetParent(2, 1)
	f.SetPinned(f.AddIssue(app, "issuedog digest", digestList), true)
	return f
}

// digestMarkerTrustSettle runs l's Iterates till one writes nothing, within
// discRunSettle of them.
func digestMarkerTrustSettle(t *testing.T, l *discRunLoop) {
	t.Helper()
	for range discRunSettle {
		from := len(l.f.Writes())
		if err := l.g.d.Iterate(context.Background()); err != nil {
			t.Fatalf("Iterate: %v", err)
		}
		if len(l.f.Writes()) == from {
			return
		}
	}
	t.Fatalf("the loop still wrote on its Iterate %d", discRunSettle)
}

// mallory adds the identity marker to A's body, on a later pass or while
// no daemon ran. A stays in the pipeline: step 1 visits it, and takes the
// edit as any untrusted edit of App text (SPEC §6.4), so A is held for
// discussion, the owner is told, and the digest, still #3, lists the hold.
// The owner's /hold on A is handled, with the App's 👍, and an unchanged
// pass after costs no rate limit.
func TestDigestMarkerUntrusted(t *testing.T) {
	t.Parallel()
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart %v", restart), func(t *testing.T) {
			t.Parallel()
			f := digestMarkerTrustFake(t)
			l := newDiscRunLoop(t, f)
			digestMarkerTrustSettle(t, l)
			f.EditBody(1, "mallory", digestTaskBody+"\n\n"+digestMarkerTrustMarker)
			if restart {
				l.start()
			}
			digestMarkerTrustSettle(t, l)
			is := f.State().Issues
			if len(is) != 3 || !is[2].Pinned || !strings.HasSuffix(is[2].Body, "\n\n"+digestMarkerTrustMarker) || !strings.Contains(is[2].Body, "### 1. #1: Part\n\n**Discussion hold**") {
				t.Fatalf("issues %+v; want #3 still the digest, alone, listing A's hold", is)
			}
			if got := is[0].Labels; !slices.Equal(got, []string{"stage:held", "type:task"}) || strings.Contains(is[0].Body, "waits on you") || strings.Contains(is[0].Body, "Nothing is waiting") {
				t.Errorf("A: labels %q, body %q; want it held, and never written as a digest", got, is[0].Body)
			}
			if c := newestComment(f, 1); !strings.Contains(c, "`mallory` edited the issue description") || !strings.Contains(c, "reason=discussion") || !strings.Contains(c, "cc @owner") {
				t.Errorf("A's newest comment = %q; want the discussion hold for mallory's edit, with the owner told", c)
			}
			hold := f.AddComment(1, "owner", "/hold")
			digestMarkerTrustSettle(t, l)
			for _, c := range f.State().Issues[0].Comments {
				if c.Body == "/hold" && !slices.Equal(c.Reactions, []string{app + " +1"}) {
					t.Errorf("the owner's /hold, %d, has reactions %q; want the App's 👍", hold, c.Reactions)
				}
			}
			l.quiet("after the owner's /hold")
		})
	}
}

// Replay passes for a restart after mallory adds the identity marker to
// A's body: the dirty note, the origin's repost, the discussion hold, the
// digest's edit and the assignment.
func TestDigestMarkerUntrustedReplay(t *testing.T) {
	t.Parallel()
	discRunReplay(t, digestMarkerTrustFake, nil, func(l *discRunLoop) {
		digestMarkerTrustSettle(t, l)
		l.f.EditBody(1, "mallory", digestTaskBody+"\n\n"+digestMarkerTrustMarker)
	})
}

// A restart finds the real digest by a version of its body the App wrote,
// from one edit history query on its first pass: untouched; renamed by the
// owner, so only its marker finds it; with its marker taken out by
// mallory, which the title rule finds (SPEC §9.4), and the App's own edit
// finds too once mallory has deleted the original post's text; and edited
// by mallory, marker kept, renamed or not. No digest is created, the body
// is written back where an edit changed it, and once the pass after has
// listed that, an unchanged pass makes no request about the digest.
func TestDigestMarkerTrustRestart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, edit      string
		renamed, delete bool
		writes          []string
	}{
		{"untouched", "", false, false, nil},
		{"renamed by the owner", "", true, false, nil},
		{"its marker taken out by mallory", "gone", false, false, []string{"PATCH /issues/1"}},
		{"its marker and original post taken out by mallory", "gone", false, true, []string{"PATCH /issues/1"}},
		{"edited by mallory, marker kept", "Mallory was here.\n\n" + digestMarkerTrustMarker, false, false, []string{"PATCH /issues/1"}},
		{"renamed, and edited by mallory, marker kept", "Mallory was here.\n\n" + digestMarkerTrustMarker, true, false, []string{"PATCH /issues/1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, o := newFake(t), newOrigin(t)
			iterate(t, digestDaemon(t, f, o, nil))
			want := f.State().Issues[0].Body
			if tc.edit != "" {
				f.EditBody(1, "mallory", tc.edit)
			}
			if tc.delete {
				f.DeleteRevision(1, 0)
			}
			if tc.renamed {
				f.RenameIssue(1, "owner", "Renamed")
			}
			var visits []int
			d := digestDaemon(t, f, o, &visits)
			from, histories := len(f.Writes()), f.HistoryQueries()
			iterate(t, d)
			if got := digestWrites(f, from); !reflect.DeepEqual(got, tc.writes) || f.HistoryQueries()-histories != 1 {
				t.Errorf("writes %q, %d history queries; want %q, and one", got, f.HistoryQueries()-histories, tc.writes)
			}
			if is := f.State().Issues; len(is) != 1 || is[0].Body != want || len(visits) != 0 {
				t.Errorf("issues %+v, step 1 visited %v; want the one digest, its body %q, and no visit", is, visits, want)
			}
			iterate(t, d)
			from, reqs := len(f.Writes()), len(f.Requests())
			iterate(t, d)
			if w, got := digestWrites(f, from), digestRequests(f, reqs); len(w) != 0 || len(got) != 0 {
				t.Errorf("the unchanged pass wrote %q and sent %+v about the digest or GraphQL; want neither", w, got)
			}
		})
	}
}

// digestMarkerTrustRenamed is f with the digest a first daemon created and
// pinned, renamed by the owner, so only its identity marker can find it.
func digestMarkerTrustRenamed(t testing.TB, f *githubtest.Fake, o *origin) {
	t.Helper()
	if err := digestDaemon(t, f, o, nil).Iterate(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.RenameIssue(1, "owner", "Renamed")
}

// A restart whose edit history reads fail can't tell whether the renamed
// digest, its identity marker in its body, is the digest: it creates no
// digest, and after loop.max_attempts failed reads posts the state error
// that says why, the marker's text, not the title rule's. The next
// restart's read works, and takes the digest back, with no new one.
func TestDigestMarkerTrustHistoryFails(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), digestHistoryOrigin(t)
	digestMarkerTrustRenamed(t, f, o)
	body := f.State().Issues[0].Body
	d := digestDaemon(t, f, o, nil)
	from := len(f.Writes())
	for range 2 {
		f.FailGraphQL(digestHistoryFailure)
		iterate(t, d)
	}
	if w := digestWrites(f, from); !reflect.DeepEqual(w, []string{"POST /issues/1/comments"}) || digestHistoryErrors(f, 1, body) != 1 {
		t.Fatalf("writes %q; want the state error alone, and no digest created", w)
	}
	c := f.State().Issues[0].Comments[0].Body
	if !strings.Contains(c, "its body has the digest's identity marker, `issuedog:digest`") || !strings.Contains(c, "take the marker out of its body") || strings.Contains(c, "has no `origin` marker") {
		t.Errorf("the state error = %q; want the identity marker's text", c)
	}
	from = len(f.Writes())
	iterate(t, digestDaemon(t, f, o, nil))
	if w, is := digestWrites(f, from), f.State().Issues; len(w) != 0 || len(is) != 1 || is[0].Body != body {
		t.Errorf("writes %q, issues %+v; want nothing written, and the one digest", w, is)
	}
}

// Replay passes for the state error of TestDigestMarkerTrustHistoryFails.
func TestDigestMarkerTrustHistoryFailsReplay(t *testing.T) {
	t.Parallel()
	o := digestHistoryOrigin(t)
	githubtest.Replay(t, func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		digestMarkerTrustRenamed(t, f, o)
		f.ForgetWrites()
		return f
	}, func(f *githubtest.Fake) error {
		d := digestDaemon(t, f, o, nil)
		for range 2 {
			f.FailGraphQL(digestHistoryFailure)
			if err := d.Iterate(context.Background()); err != nil {
				return err
			}
		}
		return nil
	})
}
