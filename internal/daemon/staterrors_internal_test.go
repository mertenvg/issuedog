package daemon

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

const openErrSlug = "issuedog"

var openErrApp = github.Author{Login: openErrSlug + "[bot]", Type: "Bot"}

// openErrSnap is open issue 7 with the App's body and comments, whose IDs
// are 1, 2, ..., and the settings event 40.
func openErrSnap(body string, comments ...string) *github.Snapshot {
	s := &github.Snapshot{Number: 7, Author: openErrApp, Body: body, SettingsEvent: 40}
	for i, c := range comments {
		s.Comments = append(s.Comments, github.Comment{ID: int64(i + 1), Author: openErrApp, Body: c})
	}
	return s
}

// openErrMarker is a posted state error, or with ack an ack reply to one,
// and with an empty kind one that names none.
func openErrMarker(kind, key, ack string) string {
	m := "<!-- issuedog:error"
	if kind != "" {
		m += " kind=" + kind
	}
	m += " key=" + key
	if ack != "" {
		m += " ack=" + ack
	}
	return m + " -->"
}

// Each kind of state error is open until it's acknowledged or its cause is
// gone (plan #7, assumption 7), and each way it ends ends it.
func TestOpenErrors(t *testing.T) {
	const (
		toClarifying = "<!-- issuedog:transition from=intake to=clarifying cmd=100 -->"
		toSpecReview = "<!-- issuedog:transition from=clarifying to=spec-review run=r1 -->"
		override     = "<!-- issuedog:transition from=clarifying to=planning by=owner event=60 -->"
	)
	var (
		trCmd   = openErrMarker("transition", "50", "")
		disagr  = openErrMarker("disagreement", "9", "")
		attempt = openErrMarker(kindAttempts, "clarifying-1", "")
		labels  = openErrMarker(kindLabels, "40", "")
		typed   = openErrMarker("type", "70", "")
		// The run's key at clarifying from comment 1, with no input, untyped.
		trRun = openErrMarker("transition", moveKey("clarifying-1", state.Item{}), "")
	)
	none := func(int64) bool { return false }
	dirty := func(ids ...int64) func(int64) bool { return func(id int64) bool { return slices.Contains(ids, id) } }
	input := []github.Comment{{ID: 3, Author: github.Author{Login: "owner", Type: "User"}, Body: "more"}}
	for _, tc := range []struct {
		name   string
		s      *github.Snapshot
		dirty  func(int64) bool
		inputs []github.Comment
		want   []string // kind/key@comment
	}{
		{"no error", openErrSnap("", toClarifying), none, nil, nil},

		// transition, keyed by what triggered it, and disagreement.
		{"a command's transition error", openErrSnap("", toClarifying, trCmd), none, nil, []string{"transition/50@2"}},
		{"a disagreement", openErrSnap("", toClarifying, disagr), none, nil, []string{"disagreement/9@2"}},
		{"a later transition ends a transition error", openErrSnap("", trCmd, toClarifying), none, nil, nil},
		{"a later transition ends a disagreement", openErrSnap("", disagr, toSpecReview), none, nil, nil},
		{"an override is a transition", openErrSnap("", toClarifying, disagr, override), none, nil, nil},
		{"one in the same comment isn't later", openErrSnap("", toClarifying+"\n"+trCmd), none, nil, []string{"transition/50@1"}},
		{"one in the same comment, the error first", openErrSnap("", disagr+"\n"+toClarifying), none, nil, []string{"disagreement/9@1"}},
		{"one in the body isn't later", openErrSnap(toClarifying, trCmd), none, nil, []string{"transition/50@1"}},
		{"an error and a transition in the body", openErrSnap(toClarifying + trCmd), none, nil, []string{"transition/50@0"}},
		{"a comment's transition ends a body error", openErrSnap(trCmd, toClarifying), none, nil, nil},
		{"a dirty later transition doesn't end it", openErrSnap("", trCmd, toClarifying), dirty(2), nil, []string{"transition/50@1"}},
		{"a transition to no stage doesn't end it", openErrSnap("", trCmd, "<!-- issuedog:transition to=nowhere cmd=5 -->"), none, nil,
			[]string{"transition/50@1"}},
		{"a dirty error isn't one", openErrSnap("", toClarifying, trCmd), dirty(2), nil, nil},

		// Acks.
		{"an ack of its kind ends it", openErrSnap("", trCmd, openErrMarker("transition", "50", "r8")), none, nil, nil},
		{"a kindless ack ends it", openErrSnap("", trCmd, openErrMarker("", "50", "r8")), none, nil, nil},
		{"an ack of another kind doesn't", openErrSnap("", trCmd, openErrMarker("disagreement", "50", "r8")), none, nil,
			[]string{"transition/50@1"}},
		{"an ack of another key doesn't", openErrSnap("", trCmd, openErrMarker("transition", "51", "r8")), none, nil,
			[]string{"transition/50@1"}},
		{"a dirty ack doesn't", openErrSnap("", trCmd, openErrMarker("transition", "50", "r8")), dirty(2), nil, []string{"transition/50@1"}},
		{"a trusted user's ack doesn't", func() *github.Snapshot {
			s := openErrSnap("", trCmd, openErrMarker("transition", "50", "r8"))
			s.Comments[1].Author = github.Author{Login: "owner", Type: "User"}
			return s
		}(), none, nil, []string{"transition/50@1"}},

		// A run's transition error, keyed by its moveKey.
		{"a run's transition error", openErrSnap("", toClarifying, trRun), none, nil, []string{"transition/" + moveKey("clarifying-1", state.Item{}) + "@2"}},
		{"new input ends a run's error", openErrSnap("", toClarifying, trRun), none, input, nil},
		{"a type ends a run's error", func() *github.Snapshot { s := openErrSnap("", toClarifying, trRun); s.Type = "epic"; return s }(), none, nil, nil},
		{"a parent ends a run's error", func() *github.Snapshot { s := openErrSnap("", toClarifying, trRun); s.Parent = 3; return s }(), none, nil, nil},
		{"a run's error from a round before", openErrSnap("", toClarifying, openErrMarker("transition", moveKey("clarifying-0", state.Item{}), "")),
			none, nil, nil},

		// attempts.
		{"an attempts error", openErrSnap("", toClarifying, attempt), none, nil, []string{"attempts/clarifying-1@2"}},
		{"new input ends an attempts error", openErrSnap("", toClarifying, attempt), none, input, nil},
		{"a move ends an attempts error", openErrSnap("", toClarifying, attempt, toSpecReview), none, nil, nil},
		{"an attempts error ignores a dirty move", openErrSnap("", toClarifying, attempt, toSpecReview), dirty(3), nil,
			[]string{"attempts/clarifying-1@2"}},

		// labels.
		{"a labels error", openErrSnap("", toClarifying, labels), none, nil, []string{"labels/40@2"}},
		{"a new settings event ends it", func() *github.Snapshot { s := openErrSnap("", toClarifying, labels); s.SettingsEvent = 41; return s }(),
			none, nil, nil},
		{"a move doesn't end a labels error", openErrSnap("", labels, toClarifying), none, nil, []string{"labels/40@1"}},

		// type, until T4, and any other kind: only an ack ends it.
		{"a type error outlasts a move", openErrSnap("", typed, toClarifying), none, input, []string{"type/70@1"}},
		{"a type error ends when acknowledged", openErrSnap("", typed, openErrMarker("type", "70", "r8")), none, nil, nil},
		{"another kind outlasts a move", openErrSnap("", openErrMarker("frob", "1", ""), toClarifying), none, nil, []string{"frob/1@1"}},

		{"a closed issue has none", func() *github.Snapshot { s := openErrSnap("", toClarifying, trCmd, typed); s.Closed = true; return s }(),
			none, nil, nil},
		{"oldest first, then by kind and key", openErrSnap(typed, toClarifying, labels+disagr+trCmd, openErrMarker("frob", "1", "")), none, nil,
			[]string{"type/70@0", "disagreement/9@2", "labels/40@2", "transition/50@2", "frob/1@3"}},
	} {
		calls := 0
		inputs := func() ([]github.Comment, error) { calls++; return tc.inputs, nil }
		open, err := openErrors(tc.s, state.Derive(tc.s, openErrSlug, tc.dirty), inputs)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		var got []string
		for _, e := range open {
			got = append(got, fmt.Sprintf("%s/%s@%d", e.Kind, e.Key, e.Comment))
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: open errors = %q, want %q", tc.name, got, tc.want)
		}
		if calls > 1 {
			t.Errorf("%s: inputs called %d times, want at most once", tc.name, calls)
		}
	}
}

// openErrCopies finds every clean comment of the App's that posts the
// error, and nothing else: not the body, a dirty copy, an ack reply, a
// user's copy, or another kind or key.
func TestOpenErrCopies(t *testing.T) {
	posted := openErrMarker("disagreement", "9", "")
	s := openErrSnap(posted,
		posted,                                   // 1
		posted,                                   // 2: dirty
		openErrMarker("disagreement", "9", "r5"), // 3: an ack
		openErrMarker("transition", "9", ""),     // 4: another kind
		openErrMarker("disagreement", "8", ""),   // 5: another key
		posted,                                   // 6: a user's
		"Again.\n\n"+posted)                      // 7
	s.Comments[5].Author = github.Author{Login: "owner", Type: "User"}
	got := openErrCopies(s, openErrSlug, func(id int64) bool { return id == 2 }, state.ErrorKey{Kind: "disagreement", Key: "9"})
	if want := []int64{1, 7}; !slices.Equal(got, want) {
		t.Errorf("copies = %v, want %v", got, want)
	}
}

// openErrors asks for the inputs once, and only for an error keyed on
// them, and returns its error.
func TestOpenErrorsInputs(t *testing.T) {
	const toClarifying = "<!-- issuedog:transition from=intake to=clarifying cmd=100 -->"
	boom := errors.New("boom")
	calls := 0
	failing := func() ([]github.Comment, error) { calls++; return nil, boom }
	none := func(int64) bool { return false }
	keyless := openErrSnap("", toClarifying, openErrMarker("transition", "50", "")+openErrMarker(kindLabels, "40", "")+openErrMarker("disagreement", "9", ""))
	if open, err := openErrors(keyless, state.Derive(keyless, openErrSlug, none), failing); err != nil || len(open) != 3 || calls != 0 {
		t.Errorf("errors keyed on no input: %v, %v, with %d calls; want 3 open errors, no call", open, err, calls)
	}
	for _, key := range []string{"attempts/clarifying-1", "transition/" + moveKey("clarifying-1", state.Item{})} {
		kind, k, _ := strings.Cut(key, "/")
		s := openErrSnap("", toClarifying, openErrMarker(kind, k, ""))
		if _, err := openErrors(s, state.Derive(s, openErrSlug, none), failing); !errors.Is(err, boom) || !strings.Contains(err.Error(), "#7") {
			t.Errorf("%s: err = %v, want boom, naming #7", key, err)
		}
	}
	calls = 0
	both := openErrSnap("", toClarifying, openErrMarker(kindAttempts, "clarifying-1", "")+openErrMarker("transition", moveKey("clarifying-1", state.Item{}), ""))
	counted := func() ([]github.Comment, error) { calls++; return nil, nil }
	if open, err := openErrors(both, state.Derive(both, openErrSlug, none), counted); err != nil || len(open) != 2 || calls != 1 {
		t.Errorf("two errors keyed on input: %v, %v, with %d calls; want both open, with one call", open, err, calls)
	}
}
