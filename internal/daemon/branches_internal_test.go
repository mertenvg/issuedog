package daemon

import (
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// A branches error's key is the names, sorted, each percent-encoded as a
// digest-new marker's keys are, joined by commas, or none, then ":" and an
// ID for a new occurrence of the same set (SPEC §5.3). Its set is the key
// up to the ":".
func TestBranchesKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		names []string
		after int64
		want  string
	}{
		{nil, 0, "none"},
		{nil, 12, "none:12"},
		{[]string{"issuedog/3-b", "issuedog/3-a"}, 0, "issuedog/3-a,issuedog/3-b"},
		{[]string{"issuedog/3-b", "issuedog/3-a"}, 1004, "issuedog/3-a,issuedog/3-b:1004"},
		{[]string{"issuedog/3-a,b", "issuedog/3-100%<x>"}, 0, "issuedog/3-100%25%3Cx%3E,issuedog/3-a%2Cb"},
	} {
		got := branchesKey(tc.names, tc.after)
		if got != tc.want {
			t.Errorf("branchesKey(%q, %d) = %q; want %q", tc.names, tc.after, got, tc.want)
		}
		if set := branchesSetOf(got); set != branchesKey(tc.names, 0) {
			t.Errorf("branchesSetOf(%q) = %q; want %q", got, set, branchesKey(tc.names, 0))
		}
		if _, err := github.FormatMarker(github.Marker{Kind: "error", Fields: map[string]string{"kind": branchesErrKind, "key": got}}); err != nil {
			t.Errorf("the key %q doesn't go in a marker: %v", got, err)
		}
	}
}

// A branch is an issue's by its issuedog/<n>- prefix alone, whatever its
// slug.
func TestBranchesIssue(t *testing.T) {
	t.Parallel()
	for branch, want := range map[string]int{
		"issuedog/12-quote-expiry": 12,
		"issuedog/12-":             12,
		"issuedog/123-x":           123,
		"issuedog/12":              0,
		"issuedog/012-x":           0,
		"issuedog/0-x":             0,
		"issuedog/x-12":            0,
		"main":                     0,
		"feature/12-x":             0,
	} {
		n, ok := branchesIssue(branch)
		if n != want || ok != (want > 0) {
			t.Errorf("branchesIssue(%q) = %d, %t; want %d, %t", branch, n, ok, want, want > 0)
		}
	}
}

// The key of the branches error for a set (SPEC §5.3): the set with no
// comment for it; else the newest comment's key, unless the set changed
// since it, by a newer comment for another set or a change the daemon saw
// after it, which makes a new occurrence keyed by that comment. An ack
// reply, a dirty comment and a person's marker are no comment for a set.
func TestBranchesKeyNow(t *testing.T) {
	t.Parallel()
	const ab, abc = "issuedog/7-a,issuedog/7-b", "issuedog/7-a,issuedog/7-b,issuedog/7-c"
	posted := func(key string) string { return openErrMarker(branchesErrKind, key, "") }
	none := func(int64) bool { return false }
	for _, tc := range []struct {
		name     string
		comments []string
		dirty    func(int64) bool
		person   bool // comment 1 is a person's, not the App's
		seen     branchesSeen
		want     string
	}{
		{name: "no comment", want: ab},
		{name: "one for the set", comments: []string{posted(ab)}, want: ab},
		{name: "one for another set", comments: []string{posted(abc)}, want: ab},
		{name: "one for the set, then another set", comments: []string{posted(ab), posted(abc)}, want: ab + ":1"},
		{name: "another set, then the set", comments: []string{posted(abc), posted(ab)}, want: ab},
		{name: "a new occurrence stands", comments: []string{posted(ab), posted(abc), posted(ab + ":1")}, want: ab + ":1"},
		{name: "a change seen after it", comments: []string{posted(ab)}, seen: branchesSeen{Set: ab, Since: 1}, want: ab + ":1"},
		{name: "a change seen before it", comments: []string{"text", posted(ab)}, seen: branchesSeen{Set: ab, Since: 1}, want: ab},
		{name: "a change to another set seen", comments: []string{posted(ab)}, seen: branchesSeen{Set: abc, Since: 1}, want: ab},
		{name: "the post after a change seen", comments: []string{posted(ab), posted(ab + ":1")}, seen: branchesSeen{Set: ab, Since: 1}, want: ab + ":1"},
		{name: "an ack for another set", comments: []string{posted(ab), openErrMarker(branchesErrKind, abc, "r5")}, want: ab},
		{name: "another kind", comments: []string{posted(ab), openErrMarker(kindLabels, "40", "")}, want: ab},
		{name: "a dirty comment for another set", comments: []string{posted(ab), posted(abc)}, dirty: func(id int64) bool { return id == 2 }, want: ab},
		{name: "a dirty comment for the set", comments: []string{posted(ab)}, dirty: func(id int64) bool { return id == 1 }, want: ab},
		{name: "a person's marker", comments: []string{posted(ab)}, person: true, want: ab},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := openErrSnap("", tc.comments...)
			if tc.person {
				s.Comments[0].Author = github.Author{Login: "owner", Type: "User"}
			}
			dirty := tc.dirty
			if dirty == nil {
				dirty = none
			}
			if got := branchesKeyNow(s, openErrSlug, dirty, ab, tc.seen); got != tc.want {
				t.Errorf("the key = %q; want %q", got, tc.want)
			}
		})
	}
}

// A branches error ends when its key isn't the one that stands for the
// issue, as when its set changed or no error stands for it, and stays open
// while the issue's branches aren't judged, as on a restart's first step 1,
// or with no way to judge them (SPEC §5.3, Open). An ack ends it too.
func TestOpenErrorsBranches(t *testing.T) {
	t.Parallel()
	const key = "issuedog/7-a,issuedog/7-b"
	for _, tc := range []struct {
		name  string
		now   func(n int) (string, bool)
		acked bool
		open  bool
	}{
		{"the same key", func(int) (string, bool) { return key, true }, false, true},
		{"another key", func(int) (string, bool) { return key + ":3", true }, false, false},
		{"no error stands", func(int) (string, bool) { return "", true }, false, false},
		{"not judged", func(int) (string, bool) { return "", false }, false, true},
		{"no judge", nil, false, true},
		{"acknowledged", func(int) (string, bool) { return key, true }, true, false},
		{"only another issue's judged", func(n int) (string, bool) { return "", n != 7 }, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			comments := []string{openErrMarker(branchesErrKind, key, "")}
			if tc.acked {
				comments = append(comments, openErrMarker(branchesErrKind, key, "r9"))
			}
			s := openErrSnap("", comments...)
			open, err := openErrors(s, state.Derive(s, openErrSlug, func(int64) bool { return false }), nil, nil, tc.now)
			if err != nil {
				t.Fatal(err)
			}
			want := []openErr(nil)
			if tc.open {
				want = []openErr{{ErrorKey: state.ErrorKey{Kind: branchesErrKind, Key: key}, Comment: 1}}
			}
			if !slices.Equal(open, want) {
				t.Errorf("the open errors are %+v; want %+v", open, want)
			}
		})
	}
}
