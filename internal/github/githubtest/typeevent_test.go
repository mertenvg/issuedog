package githubtest

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
)

// issueTypeRevertLast returns issue n's newest timeline event, failing t.
func issueTypeRevertLast(t *testing.T, c *github.Client, n int) *gogithub.Timeline {
	t.Helper()
	timeline, err := c.TimelineSince(context.Background(), "o", "r", n, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return timeline[len(timeline)-1]
}

// The timeline serves an issue-type event's types, so CheckLabels, asked
// to guard the issue type, finds what an untrusted change left and what
// stands, and reverts it. The App's write has its own issue-type event,
// which the next CheckLabels takes for the App's restore, not a change.
func TestSetTypeByRevert(t *testing.T) {
	f := New(t, "o/r", "app")
	n := f.AddIssue("owner", "Issue", "")
	owner := f.SetTypeBy(n, "owner", "Task")
	flip := f.SetTypeBy(n, "mallory", "Epic")
	c := f.Client(t)
	none := func(int64) bool { return false }
	found, err := c.CheckLabels(context.Background(), "o", "r", n, nil, "app", typeByTrust{}, none, nil, &github.IssueTypeGuard{Type: "epic"})
	if err != nil {
		t.Fatal(err)
	}
	want := &github.IssueTypeRevert{Key: strconv.FormatInt(flip, 10), By: "mallory", From: "Epic", To: "Task", ToID: f.IssueTypeID("Task")}
	if found.IssueType != owner || found.IssueTypeRevert == nil || *found.IssueTypeRevert != *want {
		t.Fatalf("IssueType %d and revert %+v; want %d and %+v", found.IssueType, found.IssueTypeRevert, owner, want)
	}
	if got := f.State().Issues[0].Type; got != "Task" {
		t.Errorf("type %q; want Task back", got)
	}
	writes := f.Writes()
	if len(writes) != 2 || writes[1].Method != http.MethodPatch || writes[1].Path != "/issues/1" || writes[1].Body != `{"type":"Task"}`+"\n" {
		t.Errorf("writes %+v; want the comment, then the type", writes)
	}
	if last := issueTypeRevertLast(t, c, n); last.GetEvent() != "issue_type_changed" || last.GetActor().GetLogin() != "app[bot]" {
		t.Errorf("last event %s by %s; want issue_type_changed by app[bot]", last.GetEvent(), last.GetActor().GetLogin())
	}
	found, err = c.CheckLabels(context.Background(), "o", "r", n, nil, "app", typeByTrust{}, none, nil, &github.IssueTypeGuard{Type: "task"})
	if err != nil || found.IssueType != owner || found.IssueTypeRevert != nil || len(f.Writes()) != 2 {
		t.Errorf("after the revert, IssueType %d, revert %+v, writes %d, %v; want %d, none and 2", found.IssueType, found.IssueTypeRevert, len(f.Writes()), err, owner)
	}
}

// An untrusted change whose event GitHub hasn't written yet (LagEvents) is
// held back, for HoldBound from when it was first found, by seen's memory,
// then left out, for the type that stands, until it has stayed unexplained
// for UnexplainedWait, then reverted all the same, keyed by the type and
// the newest event. Then the type is the one that stands, so nothing more
// is written, though the App's own event isn't written yet either.
func TestSetTypeByRevertLate(t *testing.T) {
	f := New(t, "o/r", "app")
	n := f.AddIssue("owner", "Issue", "")
	owner := f.SetTypeBy(n, "owner", "Task")
	f.LagEvents(time.Hour)
	f.SetTypeBy(n, "mallory", "Epic")
	c := f.Client(t)
	seen := &github.Unexplained{}
	check := func(typ string) github.LabelCheck {
		t.Helper()
		found, err := c.CheckLabels(context.Background(), "o", "r", n, nil, "app", typeByTrust{}, func(int64) bool { return false }, seen,
			&github.IssueTypeGuard{Type: typ})
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
	for _, wait := range []time.Duration{0, github.HoldBound - 2*time.Second} {
		f.Advance(wait)
		if found := check("epic"); len(found.Held) != 1 || found.IssueType != owner || found.IssueTypeRevert != nil || len(f.Writes()) != 0 {
			t.Fatalf("after %v: held %q, IssueType %d, revert %+v, writes %+v; want the type held back, and nothing written",
				wait, found.Held, found.IssueType, found.IssueTypeRevert, f.Writes())
		}
	}
	leftOut := github.IssueTypeLeftOut{Name: "epic", To: "Task", ToID: f.IssueTypeID("Task")}
	for _, wait := range []time.Duration{3 * time.Second, github.UnexplainedWait - github.HoldBound - 2*time.Second} {
		f.Advance(wait) // past the bound, then just short of the wait
		found := check("epic")
		if len(found.Held) != 0 || found.IssueType != owner || found.IssueTypeRevert != nil || len(f.Writes()) != 0 {
			t.Fatalf("left out: held %q, IssueType %d, revert %+v, writes %+v; want nothing held, the owner's change, and nothing written",
				found.Held, found.IssueType, found.IssueTypeRevert, f.Writes())
		}
		if found.IssueTypeLeftOut == nil || *found.IssueTypeLeftOut != leftOut {
			t.Fatalf("left out %+v; want %+v", found.IssueTypeLeftOut, leftOut)
		}
		if got, ok := seen.IssueTypeLeftOut("o", "r", n); !ok || got != leftOut {
			t.Errorf("seen's left out %+v, %t; want %+v", got, ok, leftOut)
		}
	}
	f.Advance(3 * time.Second) // past the wait
	want := github.IssueTypeRevert{Key: "issue-type:epic@" + strconv.FormatInt(owner, 10), From: "epic", To: "Task", ToID: f.IssueTypeID("Task")}
	if found := check("epic"); len(found.Held) != 0 || found.IssueTypeLeftOut != nil || found.IssueTypeRevert == nil || *found.IssueTypeRevert != want {
		t.Fatalf("held %q and revert %+v; want %+v", found.Held, found.IssueTypeRevert, want)
	}
	if got := f.State().Issues[0].Type; got != "Task" || len(f.Writes()) != 2 {
		t.Errorf("type %q, writes %+v; want Task back, with the comment and the type", got, f.Writes())
	}
	if _, ok := seen.IssueTypeLeftOut("o", "r", n); ok {
		t.Error("after the revert, seen still leaves the type out")
	}
	if found := check("task"); len(found.Held) != 0 || found.IssueTypeRevert != nil || len(f.Writes()) != 2 {
		t.Errorf("after the revert: held %q, revert %+v, writes %d; want nothing", found.Held, found.IssueTypeRevert, len(f.Writes()))
	}
}

// issueTypeRevertChecker is CheckLabels on issue n of f, guarding the issue
// type typ, with seen's memory. It fails t on an error.
func issueTypeRevertChecker(t *testing.T, f *Fake, n int, seen *github.Unexplained) func(typ string) github.LabelCheck {
	c := f.Client(t)
	return func(typ string) github.LabelCheck {
		t.Helper()
		found, err := c.CheckLabels(context.Background(), "o", "r", n, nil, "app", typeByTrust{}, func(int64) bool { return false }, seen,
			&github.IssueTypeGuard{Type: typ})
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
}

// The hold starts again when the unexplained type changes, or a late event
// lands while the type is still unexplained: GitHub is catching up.
func TestSetTypeByRevertHoldRestarts(t *testing.T) {
	t.Run("another type", func(t *testing.T) {
		f := New(t, "o/r", "app")
		n := f.AddIssue("owner", "Issue", "")
		f.SetTypeBy(n, "owner", "Task")
		f.LagEvents(time.Hour)
		f.SetTypeBy(n, "mallory", "Epic")
		check := issueTypeRevertChecker(t, f, n, &github.Unexplained{})
		if found := check("epic"); len(found.Held) != 1 {
			t.Fatalf("held %q; want epic held back", found.Held)
		}
		f.Advance(github.HoldBound - 5*time.Second)
		f.SetTypeBy(n, "mallory", "Bug")
		if found := check("bug"); len(found.Held) != 1 {
			t.Fatalf("held %q; want bug held back", found.Held)
		}
		f.Advance(10 * time.Second)
		if found := check("bug"); len(found.Held) != 1 || len(f.Writes()) != 0 {
			t.Errorf("past epic's bound: held %q, writes %+v; want bug still held back, from when it was first found", found.Held, f.Writes())
		}
	})
	t.Run("a late event lands", func(t *testing.T) {
		f := New(t, "o/r", "app")
		n := f.AddIssue("owner", "Issue", "")
		f.SetTypeBy(n, "owner", "Task")
		f.LagEvents(20 * time.Second)
		f.SetTypeBy(n, "mallory", "Bug")
		f.LagEvents(time.Hour)
		f.SetTypeBy(n, "mallory", "Epic")
		check := issueTypeRevertChecker(t, f, n, &github.Unexplained{})
		if found := check("epic"); len(found.Held) != 1 {
			t.Fatalf("held %q; want epic held back", found.Held)
		}
		f.Advance(25 * time.Second) // bug's event lands
		if found := check("epic"); len(found.Held) != 1 {
			t.Fatalf("held %q; want epic held back", found.Held)
		}
		f.Advance(10 * time.Second)
		if found := check("epic"); len(found.Held) != 1 || len(f.Writes()) != 0 {
			t.Errorf("past the first bound: held %q, writes %+v; want epic still held back, from when bug's event landed", found.Held, f.Writes())
		}
	})
}

// Once the type is explained, or reverted, the memory of it is forgotten,
// so the same unexplained type later is held back afresh, not reverted at
// once by the old time.
func TestSetTypeByRevertHoldForgets(t *testing.T) {
	for _, tc := range []struct {
		name  string
		clear func(t *testing.T, f *Fake, n int, check func(string) github.LabelCheck)
	}{
		{"explained", func(t *testing.T, f *Fake, n int, check func(string) github.LabelCheck) {
			f.SetType(n, "Task")
			if found := check("task"); len(found.Held) != 0 {
				t.Fatalf("held %q; want nothing", found.Held)
			}
		}},
		{"reverted", func(t *testing.T, f *Fake, n int, check func(string) github.LabelCheck) {
			if found := check("epic"); found.IssueTypeRevert == nil {
				t.Fatalf("revert %+v; want epic reverted at the wait", found.IssueTypeRevert)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := New(t, "o/r", "app")
			n := f.AddIssue("owner", "Issue", "")
			f.SetTypeBy(n, "owner", "Task")
			f.LagEvents(time.Hour)
			f.SetTypeBy(n, "mallory", "Epic")
			check := issueTypeRevertChecker(t, f, n, &github.Unexplained{})
			if found := check("epic"); len(found.Held) != 1 {
				t.Fatalf("held %q; want epic held back", found.Held)
			}
			f.Advance(github.UnexplainedWait + time.Second)
			tc.clear(t, f, n, check)
			f.SetType(n, "Epic") // no event
			if found := check("epic"); len(found.Held) != 1 || found.IssueTypeRevert != nil {
				t.Errorf("the same type again: held %q, revert %+v; want it held back afresh", found.Held, found.IssueTypeRevert)
			}
		})
	}
}

// A late event that lands within HoldBound explains the type, so an
// untrusted change is reverted keyed by its event, and a trusted one stands,
// with no revert.
func TestSetTypeByRevertLateLands(t *testing.T) {
	for _, tc := range []struct {
		login string
		want  bool // a revert
	}{{"mallory", true}, {"owner", false}} {
		t.Run(tc.login, func(t *testing.T) {
			f := New(t, "o/r", "app")
			n := f.AddIssue("owner", "Issue", "")
			owner := f.SetTypeBy(n, "owner", "Task")
			f.LagEvents(2 * time.Second)
			flip := f.SetTypeBy(n, tc.login, "Epic")
			c := f.Client(t)
			seen := &github.Unexplained{}
			guard := &github.IssueTypeGuard{Type: "epic"}
			found, err := c.CheckLabels(context.Background(), "o", "r", n, nil, "app", typeByTrust{}, func(int64) bool { return false }, seen, guard)
			if err != nil || len(found.Held) != 1 || found.IssueType != owner {
				t.Fatalf("first check: held %q, IssueType %d, %v; want the type held back", found.Held, found.IssueType, err)
			}
			f.Advance(3 * time.Second)
			if found, err = c.CheckLabels(context.Background(), "o", "r", n, nil, "app", typeByTrust{}, func(int64) bool { return false }, seen, guard); err != nil {
				t.Fatal(err)
			}
			if got := found.IssueTypeRevert != nil; got != tc.want || len(found.Held) != 0 {
				t.Errorf("revert %+v, held %q; want a revert %t, nothing held", found.IssueTypeRevert, found.Held, tc.want)
			}
			if tc.want && found.IssueTypeRevert.Key != strconv.FormatInt(flip, 10) {
				t.Errorf("revert key %q; want the event's, %d", found.IssueTypeRevert.Key, flip)
			}
			if !tc.want && found.IssueType != flip {
				t.Errorf("IssueType %d; want the owner's change, %d", found.IssueType, flip)
			}
		})
	}
}

// An untrusted type on an issue with no type that stands is removed with
// null, which writes an issue_type_removed event by the App.
func TestSetTypeByRevertRemoves(t *testing.T) {
	f := New(t, "o/r", "app")
	n := f.AddIssue("owner", "Issue", "")
	flip := f.SetTypeBy(n, "mallory", "Bug")
	c := f.Client(t)
	if err := c.RevertIssueType(context.Background(), "o", "r", n, github.IssueTypeRevert{Key: strconv.FormatInt(flip, 10), By: "mallory", From: "Bug"}); err != nil {
		t.Fatal(err)
	}
	if got := f.State().Issues[0].Type; got != "" {
		t.Errorf("type %q; want none", got)
	}
	if last := issueTypeRevertLast(t, c, n); last.GetEvent() != "issue_type_removed" || last.GetActor().GetLogin() != "app[bot]" {
		t.Errorf("last event %s by %s; want issue_type_removed by app[bot]", last.GetEvent(), last.GetActor().GetLogin())
	}
}

// Setting the type an issue has already, or removing none, writes no event.
func TestSetTypeByRevertNoChange(t *testing.T) {
	f := New(t, "o/r", "app")
	typed := f.AddIssue("owner", "Typed", "")
	f.SetTypeBy(typed, "owner", "Task")
	untyped := f.AddIssue("owner", "Untyped", "")
	c := f.Client(t)
	for _, tc := range []struct {
		n    int
		to   string
		want string
	}{{typed, "task", "Task"}, {untyped, "", ""}} {
		before, err := c.TimelineSince(context.Background(), "o", "r", tc.n, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.RevertIssueType(context.Background(), "o", "r", tc.n, github.IssueTypeRevert{Key: "1", To: tc.to, Posted: true}); err != nil {
			t.Fatal(err)
		}
		after, err := c.TimelineSince(context.Background(), "o", "r", tc.n, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(before) || f.State().Issues[tc.n-1].Type != tc.want {
			t.Errorf("#%d: %d events after, %d before, type %q; want no new event and %q", tc.n, len(after), len(before), f.State().Issues[tc.n-1].Type, tc.want)
		}
	}
}

// Under DropTypeChanges, the type stays, as GitHub keeps it for anyone
// without push access, and RevertIssueType says so. A failed write changes
// nothing, and a type that is neither a name nor null is refused.
func TestSetTypeByRevertRefused(t *testing.T) {
	rv := func(key int64) github.IssueTypeRevert {
		return github.IssueTypeRevert{Key: strconv.FormatInt(key, 10), By: "mallory", From: "Epic", To: "Task", Posted: true}
	}
	t.Run("dropped", func(t *testing.T) {
		f := New(t, "o/r", "app")
		n := f.AddIssue("owner", "Issue", "")
		f.SetTypeBy(n, "owner", "Task")
		key := f.SetTypeBy(n, "mallory", "Epic")
		f.DropTypeChanges()
		err := f.Client(t).RevertIssueType(context.Background(), "o", "r", n, rv(key))
		if !errors.Is(err, github.ErrIssueTypeRevertDropped) || f.State().Issues[0].Type != "Epic" || len(f.Writes()) != 1 {
			t.Errorf("RevertIssueType = %v, type %q, writes %+v; want ErrIssueTypeRevertDropped, Epic kept, one write", err, f.State().Issues[0].Type, f.Writes())
		}
	})
	t.Run("failed", func(t *testing.T) {
		f := New(t, "o/r", "app")
		n := f.AddIssue("owner", "Issue", "")
		f.SetTypeBy(n, "owner", "Task")
		key := f.SetTypeBy(n, "mallory", "Epic")
		f.FailWrite(1)
		if err := f.Client(t).RevertIssueType(context.Background(), "o", "r", n, rv(key)); err == nil || f.State().Issues[0].Type != "Epic" {
			t.Errorf("RevertIssueType = %v, type %q; want an error, Epic kept", err, f.State().Issues[0].Type)
		}
	})
	t.Run("not a name", func(t *testing.T) {
		f := New(t, "o/r", "app")
		n := f.AddIssue("owner", "Issue", "")
		c := f.Client(t)
		for _, body := range []any{map[string]any{"type": ""}, map[string]any{"type": 7}} {
			req, err := c.NewRequest(context.Background(), http.MethodPatch, "repos/o/r/issues/1", body)
			if err != nil {
				t.Fatal(err)
			}
			if resp, err := c.Do(req, nil); err == nil || resp.StatusCode != http.StatusUnprocessableEntity {
				t.Errorf("PATCH %v = %v; want a 422", body, err)
			}
		}
		if got := f.State().Issues[n-1].Type; got != "" {
			t.Errorf("type %q; want none", got)
		}
	})
}

// The fake gives each org issue type an ID, the same for every issue and
// event, and SetType's too. RenameOrgIssueType keeps the ID and shows the
// new name on the issues, and the old name is no type any more, so the API
// refuses it (#183).
func TestRenameOrgIssueType(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "app")
	n := f.AddIssue("owner", "Issue", "")
	other := f.AddIssue("owner", "Other", "")
	f.SetTypeBy(n, "owner", "Task")
	f.SetType(other, "task")
	c := f.Client(t)
	read := func(n int) *gogithub.IssueType {
		t.Helper()
		issue, _, err := c.Issues.Get(context.Background(), "o", "r", n)
		if err != nil {
			t.Fatal(err)
		}
		return issue.GetType()
	}
	id := f.IssueTypeID("Task")
	if id == 0 || read(n).GetID() != id || read(other).GetID() != id || f.IssueTypeID("Epic") == id {
		t.Fatalf("IDs %d, %d and %d; want Task's, %d, on both, and Epic's another", read(n).GetID(), read(other).GetID(), f.IssueTypeID("Epic"), id)
	}
	f.RenameOrgIssueType("Task", "Chore")
	for _, i := range []int{n, other} {
		if got := read(i); got.GetName() != "Chore" || got.GetID() != id {
			t.Errorf("#%d after the rename: type %q, %d; want Chore, %d", i, got.GetName(), got.GetID(), id)
		}
	}
	if f.IssueTypeID("chore") != id {
		t.Errorf("Chore's ID %d; want Task's, %d", f.IssueTypeID("chore"), id)
	}
	patch := func(name string) error {
		t.Helper()
		req, err := c.NewRequest(context.Background(), http.MethodPatch, "repos/o/r/issues/2", map[string]any{"type": name})
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Do(req, nil)
		return err
	}
	var gh *gogithub.ErrorResponse
	if err := patch("Task"); !errors.As(err, &gh) || gh.Response.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("PATCH the old name = %v; want a 422", err)
	}
	if err := patch("Epic"); err != nil || read(other).GetName() != "Epic" {
		t.Errorf("PATCH Epic = %v, type %q; want Epic", err, read(other).GetName())
	}
}

// After the org renames Task to Chore, the owner's issue-type event still
// names Task, with the same ID as the issue's Chore. CheckLabels, given
// the issue's type with its ID, finds it is the type that stands: nothing
// is held back, left out or reverted, then or later. Without the ID, the
// names can't tell, so the type is held back as unexplained.
func TestSetTypeByRenamedInOrg(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "app")
	n := f.AddIssue("owner", "Issue", "")
	owner := f.SetTypeBy(n, "owner", "Task")
	f.RenameOrgIssueType("Task", "Chore")
	id := f.IssueTypeID("Chore")
	seen := &github.Unexplained{}
	c := f.Client(t)
	check := func(guard *github.IssueTypeGuard) github.LabelCheck {
		t.Helper()
		found, err := c.CheckLabels(context.Background(), "o", "r", n, nil, "app", typeByTrust{}, func(int64) bool { return false }, seen, guard)
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
	for _, wait := range []time.Duration{0, github.HoldBound + time.Second, github.UnexplainedWait} {
		f.Advance(wait)
		found := check(&github.IssueTypeGuard{Type: "Chore", ID: id})
		if len(found.Held) != 0 || found.IssueTypeLeftOut != nil || found.IssueTypeRevert != nil || found.IssueType != owner || len(f.Writes()) != 0 {
			t.Errorf("%v on: held %q, left out %+v, revert %+v, IssueType %d, writes %+v; want nothing, and the owner's change, %d",
				wait, found.Held, found.IssueTypeLeftOut, found.IssueTypeRevert, found.IssueType, f.Writes(), owner)
		}
	}
	if found := check(&github.IssueTypeGuard{Type: "Chore"}); len(found.Held) != 1 {
		t.Errorf("with no ID: held %q; want Chore held back, by name", found.Held)
	}
}

// mallory's change to Epic, which the org then renames Huge, is still
// explained by mallory's event, which names Epic with Huge's ID: it's
// reverted at once, keyed by that event, not held back as unexplained.
func TestSetTypeByRenamedInOrgUntrusted(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "app")
	n := f.AddIssue("owner", "Issue", "")
	owner := f.SetTypeBy(n, "owner", "Task")
	flip := f.SetTypeBy(n, "mallory", "Epic")
	f.RenameOrgIssueType("Epic", "Huge")
	c := f.Client(t)
	found, err := c.CheckLabels(context.Background(), "o", "r", n, nil, "app", typeByTrust{}, func(int64) bool { return false }, nil,
		&github.IssueTypeGuard{Type: "Huge", ID: f.IssueTypeID("Huge")})
	if err != nil {
		t.Fatal(err)
	}
	want := &github.IssueTypeRevert{Key: strconv.FormatInt(flip, 10), By: "mallory", From: "Epic", To: "Task", ToID: f.IssueTypeID("Task")}
	if len(found.Held) != 0 || found.IssueType != owner || found.IssueTypeRevert == nil || *found.IssueTypeRevert != *want {
		t.Fatalf("held %q, IssueType %d, revert %+v; want nothing held, %d, and %+v", found.Held, found.IssueType, found.IssueTypeRevert, owner, want)
	}
	if got := f.State().Issues[0].Type; got != "Task" {
		t.Errorf("type %q; want Task back", got)
	}
}
