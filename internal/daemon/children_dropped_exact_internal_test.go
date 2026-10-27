package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// The key of a dropped subtree's close, kept exactly (SPEC §5, #607): a
// close keyed apart from a move the revision's key carries, as
// event=p<id>-closed, keeps that key once a marker carries it, even after
// an edit hides the move that chose it, so a pass cut short after its
// comment finishes it, with no second comment.

// droppedExactFiled files issue 1, the story, then T3's child, #2, which
// a revision drops, at ready, and below it T4's child, #3, at ready too,
// with its first move keyed by the revision's approval, event=p10, as a
// new child moved below a dropped one has, and blocked by a bug, #4,
// filed by hand. It returns the fake, with its writes forgotten, and the
// ID of #3's first move's comment.
func droppedExactFiled(t testing.TB) (*githubtest.Fake, int64) {
	const app = "issuedog[bot]"
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("owner", "The story", "")
	child := func(item string) (int, int64) {
		n := f.AddIssue(app, item+"'s child", fmt.Sprintf("The task.\n\n<!-- issuedog:origin plan=#1 rev=2 item=%s -->", item), "type:task", "stage:ready")
		return n, f.AddComment(n, app, "Created.\n\n<!-- issuedog:transition from=intake to=ready event=p10 -->")
	}
	root, _ := child("T3")
	f.SetParent(root, 1)
	n, first := child("T4")
	f.SetParent(n, root)
	f.SetBlockedBy(n, f.AddIssue("owner", "A bug", "", "type:bug"))
	f.ForgetWrites()
	return f, first
}

// droppedExactPass closes the subtree of #2, T3's child, which revision 2
// of #1's plan drops, keyed event=p10 (droppedWalk), as ChildrenVisit
// does, with a client and state of its own.
func droppedExactPass(t *testing.T) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		ctx := context.Background()
		st := &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}, Trust: ownerOnly{}}
		root, _, err := st.Client.Issues.Get(ctx, "o", "r", 2)
		if err != nil {
			return fmt.Errorf("read the dropped child: %w", err)
		}
		d := droppedItem{parent: 1, rev: 2, root: 2, item: "T3", key: state.Key{Field: "event", ID: "p10"},
			plan: "https://github.com/o/r/issues/1#issuecomment-9"}
		return st.droppedWalk(ctx, d, 2, root.GetID(), map[int]bool{1: true, 2: true})
	}
}

// A pass that crashed after #3's close comment, keyed event=p10-closed,
// but before its close, then an owner's edit of the comment holding #3's
// first move, keyed event=p10, which hides that move: the next pass keeps
// the p10-closed key its close comment carries, so the close is taken as
// written. The label rules finish the close, and #3 loses its link, with
// no second close comment, nor a state error for a move from closed.
// The dropped #2 closes after it. A crash at any write replays.
func TestDroppedExactEditedCrash(t *testing.T) {
	t.Parallel()
	pass := droppedExactPass(t)
	clean := func(tb testing.TB) *githubtest.Fake {
		f, _ := droppedExactFiled(tb)
		return f
	}
	f := clean(t)
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	at := 0
	for i, w := range f.Writes() {
		if w.Method == "PATCH" && w.Path == "/issues/3" && strings.Contains(w.Body, `"state":"closed"`) {
			at = i + 1
		}
	}
	if at == 0 {
		t.Fatalf("a clean pass never closed #3: %+v", f.Writes())
	}
	setup := func(tb testing.TB) *githubtest.Fake {
		f, first := droppedExactFiled(tb)
		f.FailWrite(at)
		if err := pass(f); err == nil {
			tb.Fatalf("the first pass made every write; want it stopped at #3's close, write %d", at)
		}
		f.FailWrite(0)
		f.EditComment(first, "owner", "Created, and edited by the owner.\n\n<!-- issuedog:transition from=intake to=ready event=p10 -->")
		f.ForgetWrites()
		return f
	}
	githubtest.Replay(t, setup, pass)

	f = setup(t)
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	s := f.State()
	n3 := s.Issues[2]
	var closes, errs int
	for _, c := range n3.Comments {
		if strings.HasPrefix(c.Body, "Closed as not planned") {
			closes++
		}
		if strings.Contains(c.Body, "<!-- issuedog:error ") {
			errs++
		}
	}
	if closes != 1 || errs != 0 {
		t.Errorf("#3 has %d close comments and %d state errors, want one close comment and no error: %+v", closes, errs, n3.Comments)
	}
	if !n3.Closed || len(n3.BlockedBy) != 0 {
		t.Errorf("#3 is closed %t, blocked by %v; want it closed and unlinked", n3.Closed, n3.BlockedBy)
	}
	if !s.Issues[1].Closed {
		t.Error("#2, the dropped child, is open; want it closed")
	}
	n := len(f.Writes())
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	if w := f.Writes(); len(w) != n {
		t.Errorf("a pass after it wrote %+v, want nothing", w[n:])
	}
}

// droppedKey keys the close apart, event=p<id>-closed, while a move other
// than a close carries the revision's key, or once any marker carries the
// key apart, as a close comment cut short before its close does, even with
// the move hidden. A close keyed by the key itself keeps it, as does an
// issue with no move keyed so.
func TestDroppedKeyKeepsApart(t *testing.T) {
	t.Parallel()
	key := state.Key{Field: "event", ID: "p10"}
	apart := state.Key{Field: "event", ID: "p10-closed"}
	marker := func(fields ...string) map[string]string {
		m := map[string]string{}
		for i := 0; i+1 < len(fields); i += 2 {
			m[fields[i]] = fields[i+1]
		}
		return m
	}
	issue := func(markers ...map[string]string) state.Issue {
		var is state.Issue
		for _, m := range markers {
			is.Markers = append(is.Markers, github.Marker{Kind: "transition", Fields: m})
		}
		return is
	}
	move := marker("from", "intake", "to", "ready", "event", "p10")
	for _, tt := range []struct {
		name string
		is   state.Issue
		want state.Key
	}{
		{"no move keyed so", issue(), key},
		{"a move keyed so", issue(move), apart},
		{"a close keyed so, cut short", issue(marker("from", "ready", "to", "closed", "reason", "not_planned", "event", "p10")), key},
		{"a close keyed apart, the move clean", issue(move, marker("from", "ready", "to", "closed", "reason", "not_planned", "event", "p10-closed")), apart},
		{"a close keyed apart, the move hidden", issue(marker("from", "ready", "to", "closed", "reason", "not_planned", "event", "p10-closed")), apart},
	} {
		if got := droppedKey(tt.is, key); got != tt.want {
			t.Errorf("%s: droppedKey = %+v, want %+v", tt.name, got, tt.want)
		}
	}
}
