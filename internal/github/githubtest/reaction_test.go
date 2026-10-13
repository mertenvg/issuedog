package githubtest

import (
	"reflect"
	"testing"
)

func TestRemoveReaction(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	id := f.AddComment(f.AddIssue("owner", "Title", "body"), "owner", "hi")
	f.AddReaction(id, "owner", "heart")
	up := f.AddReaction(id, "owner", "+1")
	if !f.RemoveReaction(id, up) {
		t.Errorf("RemoveReaction = false, want true for a reaction that's there")
	}
	if f.RemoveReaction(id, up) {
		t.Errorf("RemoveReaction = true, want false once it's gone")
	}
	if got := f.State().Issues[0].Comments[0].Reactions; !reflect.DeepEqual(got, []string{"owner heart"}) {
		t.Errorf("reactions = %q, want only the other one left", got)
	}
}
