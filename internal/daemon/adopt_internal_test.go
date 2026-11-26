package daemon

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// renderAdopted names its source, a comment or the description, by a link,
// and the login as a code span, its "<!--" escaped (SPEC §5.1), then each
// marker on a line of its own, and has no cc line.
func TestRenderAdopted(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, login string
		from        int64
		link, want  string
	}{
		{"a comment", "owner", 1001, "https://github.com/o/r/issues/1#issuecomment-1001",
			"I've copied these markers from `owner`'s [comment](https://github.com/o/r/issues/1#issuecomment-1001), " +
				"written while this pipeline ran by hand, so I read them as mine.\n\n<!-- m1 -->\n<!-- m2 -->"},
		{"the description", "owner", state.BodyID, "https://github.com/o/r/issues/2",
			"I've copied these markers from `owner`'s [description](https://github.com/o/r/issues/2), " +
				"written while this pipeline ran by hand, so I read them as mine.\n\n<!-- m1 -->\n<!-- m2 -->"},
		{"a login with a marker's start", "a<!--b", 1001, "https://github.com/o/r/issues/1#issuecomment-1001",
			"I've copied these markers from `a&lt;!--b`'s [comment](https://github.com/o/r/issues/1#issuecomment-1001), " +
				"written while this pipeline ran by hand, so I read them as mine.\n\n<!-- m1 -->\n<!-- m2 -->"},
	} {
		got := renderAdopted(tt.login, tt.from, tt.link, []string{"<!-- m1 -->", "<!-- m2 -->"})
		if got != tt.want {
			t.Errorf("%s:\n%s\nwant\n%s", tt.name, got, tt.want)
		}
		if strings.Contains(got, "cc ") {
			t.Errorf("%s: %q has a cc line", tt.name, got)
		}
	}
}

// adoptChildrenFound is childrenFind on each parent of issues filed by
// author on a fresh fake: an epic, its story S1, with its body's origin,
// and the story's task T1, with its body's origin. With adopt true, it
// first adopts owner's markers. It returns, by parent, each item's child.
func adoptChildrenFound(t *testing.T, author string, adopt bool) map[int]map[string]childFound {
	t.Helper()
	ctx := context.Background()
	f := githubtest.New(t, "o/r", "issuedog")
	epic := f.AddIssue(author, "The epic", "The epic.")
	f.AddLabel(epic, "owner", "stage:planned")
	f.AddComment(epic, author, "<!-- issuedog:transition from=plan-review to=planned rev=1 cmd=r1 -->")
	story := f.AddIssue(author, "The story", "The story.\n<!-- issuedog:origin plan=#1 rev=1 item=S1 -->")
	f.AddLabel(story, "owner", "stage:planned")
	f.SetParent(story, epic)
	task := f.AddIssue(author, "The task", "The task.\n<!-- issuedog:origin plan=#2 rev=1 item=T1 -->")
	f.AddLabel(task, "owner", "stage:ready")
	f.SetParent(task, story)
	a := &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}
	if adopt {
		if _, err := a.Adopt(ctx, ownerOnly{}, "owner", false); err != nil {
			t.Fatalf("Adopt: %v", err)
		}
	}
	st := &Step{Applier: a, Trust: ownerOnly{}}
	found := map[int]map[string]childFound{}
	for _, n := range []int{epic, story} {
		s, err := a.Client.Snapshot(ctx, "o", "r", n)
		if err != nil {
			t.Fatal(err)
		}
		if found[n], err = st.childrenFind(ctx, s, time.Time{}); err != nil {
			t.Fatalf("childrenFind on #%d: %v", n, err)
		}
		for item, c := range found[n] {
			c.ID = 0 // the fakes' IDs may differ
			found[n][item] = c
		}
	}
	return found
}

// After adopt, each parent's children are matched to their items as
// creation finds them (SPEC §5.2), as they are when the App filed the
// same issues. Before it, owner's origins count for nothing, so no child
// is matched.
func TestAdoptChildrenMatched(t *testing.T) {
	t.Parallel()
	want := adoptChildrenFound(t, "issuedog[bot]", false)
	if want[1]["S1"].Number != 2 || want[2]["T1"].Number != 3 {
		t.Fatalf("the App's children = %+v, want S1 #2 and T1 #3", want)
	}
	if got := adoptChildrenFound(t, "owner", true); !reflect.DeepEqual(got, want) {
		t.Errorf("children after adopt = %+v, want %+v", got, want)
	}
	if got := adoptChildrenFound(t, "owner", false); len(got[1]) != 0 || len(got[2]) != 0 {
		t.Errorf("children before adopt = %+v, want none", got)
	}
}
