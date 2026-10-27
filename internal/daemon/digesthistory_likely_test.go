package daemon_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// The digest itself, its identity marker in its body and still titled
// `issuedog digest`, on a start whose edit history reads fail, here past
// loop.max_attempts 2, gets no digesthistory state error (SPEC §5.3, §9.4,
// #527): it would stay on the digest once a read works and takes it back.
// It's held all the same, so no digest is created, and the reads are made
// as for any held issue: on passes 1 and 2, then once every 2 passes, the
// read on pass 4 failing too. Pass 6's read works, and takes the digest
// back: untouched, with nothing to write, or edited by mallory, marker
// kept, with its body written back. The passes after read nothing, and an
// unchanged pass makes no request about the digest.
func TestDigestHistoryLikelyOutage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, edit string
		back       []string // pass 6's writes
	}{
		{"untouched", "", nil},
		{"edited by mallory, marker kept", "Mallory was here.\n\n" + digestMarkerTrustMarker, []string{"PATCH /issues/1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, o := newFake(t), digestHistoryOrigin(t)
			iterate(t, digestDaemon(t, f, o, nil))
			want := f.State().Issues[0].Body
			if tc.edit != "" {
				f.EditBody(1, "mallory", tc.edit)
			}
			body := f.State().Issues[0].Body
			var visits []int
			d := digestDaemon(t, f, o, &visits)
			for i, read := range []int{1, 1, 0, 1, 0, 1, 0, 0} {
				if read == 1 && i < 5 {
					f.FailGraphQL(digestHistoryFailure)
				}
				from, queries := len(f.Writes()), f.HistoryQueries()
				iterate(t, d)
				w := digestWrites(f, from)
				if got := f.HistoryQueries() - queries; got != read {
					t.Errorf("pass %d: %d history queries; want %d", i+1, got, read)
				}
				if i == 5 && !reflect.DeepEqual(w, tc.back) || i != 5 && len(w) != 0 {
					t.Errorf("pass %d wrote %q; want %q", i+1, w, map[bool][]string{true: tc.back}[i == 5])
				}
			}
			is := f.State().Issues
			if len(is) != 1 || is[0].Body != want || !is[0].Pinned || len(visits) != 0 {
				t.Fatalf("issues %+v, step 1 visited %v; want the one digest, pinned, its body %q, and no visit", is, visits, want)
			}
			if n := digestHistoryErrors(f, 1, body); n != 0 || len(is[0].Comments) != 0 {
				t.Errorf("the digest has %d state errors, comments %+v; want none", n, is[0].Comments)
			}
			from, reqs := len(f.Writes()), len(f.Requests())
			iterate(t, d)
			if w, got := digestWrites(f, from), digestRequests(f, reqs); len(w) != 0 || len(got) != 0 {
				t.Errorf("the unchanged pass wrote %q and sent %+v about the digest or GraphQL; want neither", w, got)
			}
		})
	}
}

// Replay passes for TestDigestHistoryLikelyOutage's edited digest: an
// outage past loop.max_attempts posts nothing, and the read that works
// writes the body back. A restart after that edit meets no outage, and
// its read takes the digest back at once.
func TestDigestHistoryLikelyOutageReplay(t *testing.T) {
	t.Parallel()
	o := digestHistoryOrigin(t)
	githubtest.Replay(t, func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		if err := digestDaemon(t, f, o, nil).Iterate(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.EditBody(1, "mallory", "Mallory was here.\n\n"+digestMarkerTrustMarker)
		f.ForgetWrites()
		return f
	}, func(f *githubtest.Fake) error {
		d := digestDaemon(t, f, o, nil)
		over := f.State().Issues[0].BodyEditedBy == app
		for i := range 6 {
			if !over && i < 4 && i != 2 {
				f.FailGraphQL(digestHistoryFailure)
			}
			if err := d.Iterate(context.Background()); err != nil {
				return err
			}
		}
		return nil
	})
}
