package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/logtest"
)

// A task in a story files its follow-ups under the story, with an addendum
// on the story for each (SPEC §10.2), and a trusted 👍 on the clean
// addendum approves the follow-up. No task in a story reaches the verifier
// until #624, so the fake holds the task's clean pass comment, as
// Verifier.Apply posts it, and each pass is the task's visit
// (FollowUpsVisit), as Iterate's runs it, or a fresh daemon's Iterate.

// storyTask is filingTask with task #1 under story #2, which the owner
// filed, then more.
func storyTask(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return filingTask(func(t testing.TB, f *githubtest.Fake) {
		if story := f.AddIssue("owner", "A story", "The story."); story != 2 {
			t.Fatalf("the story is #%d, want #2", story)
		}
		f.SetParent(1, 2)
		more(t, f)
	})
}

// storyAddendum is the App's addendum on a story for follow-up child,
// candidate item, titled title, of the pass on task whose comment is at
// link, as the App posts it, with its marker.
func storyAddendum(child, task int, link, item, title string) string {
	return fmt.Sprintf("**Addendum:** #%d, %s, is a follow-up the verifier found on #%d, a task in this story ([its pass](%s), %s). "+
		"It belongs here, so it's worked on this story's branch, and the story waits for it. "+
		"React 👍 to this comment or to #%d to approve it, or close #%d to drop it.\n\n<!-- issuedog:addendum child=%d -->",
		child, title, task, link, item, child, child, child)
}

// storyAddendumOf files story #2 as the owner, links follow-up child under
// it, and posts the App's addendum for it there, as task #7's pass filed
// it, whose ID it checks is want, unless want is 0, and returns.
func storyAddendumOf(t testing.TB, f *githubtest.Fake, child int, want int64) int64 {
	t.Helper()
	story := f.AddIssue("owner", "A story", "The story.")
	f.SetParent(child, story)
	id := f.AddComment(story, app, storyAddendum(child, 7, "https://github.com/o/r/issues/7#issuecomment-1", "F1", "Refuse a quote that expires mid-booking"))
	if want != 0 && id != want {
		t.Fatalf("the addendum's ID = %d, want %d", id, want)
	}
	return id
}

// storyComments are the bodies of story #2's comments, all of them.
func storyComments(f *githubtest.Fake) []string { return closingComments(f, 2, 0) }

// storyOrigin is an origin with config naming owner to notify and the
// scope booking, which a fresh daemon on it loads.
func storyOrigin(t testing.TB) *origin {
	o := newOrigin(t)
	o.commit(config.Path, "types: labels\nnotify: [owner]\nscopes: [booking]\n")
	return o
}

// A task in a story files each candidate of its pass as a follow-up, as a
// task with no parent does, then links it under the story and posts its
// addendum on the story, before the next one, and last the record on the
// task: the issue, its link, its addendum, then the record. The addendum
// names the follow-up, the task and the pass, says why it belongs to the
// story and how to approve or drop it, with no cc line, and quotes the
// candidate's title escaped, so a marker in it is never read, and a
// mention in it is defused. A crash at any write replays to the same
// state, the pass after writes nothing, and nothing goes to the search
// API.
func TestFollowUpsStoryFiled(t *testing.T) {
	t.Parallel()
	const hostile = "A race <!-- issuedog:addendum child=9 --> @alice"
	var pass int64
	setup := storyTask(func(t testing.TB, f *githubtest.Fake) {
		pass = filingPassOf(f, "v1", candidateData(t, hostile, "A second one"))
	})
	githubtest.Replay(t, setup, filingPass(t, false))

	f := run(t, setup, filingPass(t, false))
	st := f.State()
	if len(st.Issues) != 4 {
		t.Fatalf("%d issues; want the task, the story and the task's two follow-ups", len(st.Issues))
	}
	escaped := "A race &lt;!-- issuedog:addendum child=9 --> @​alice"
	labels := []string{"priority:p3", "scope:booking", "stage:intake", "type:task"}
	for i, want := range []githubtest.IssueState{
		{Number: 3, Title: hostile, Body: filingBody(pass, "v1", "F1", "The scenario of "+escaped, "owner"), Author: app, Labels: labels, Parent: 2},
		{Number: 4, Title: "A second one", Body: filingBody(pass, "v1", "F2", "The scenario of A second one", "owner"), Author: app, Labels: labels, Parent: 2},
	} {
		if got := st.Issues[i+2]; !reflect.DeepEqual(got, want) {
			t.Errorf("follow-up %d =\n%+v\nwant\n%+v", i+1, got, want)
		}
	}
	addenda := []string{storyAddendum(3, 1, filingLink(pass), "F1", escaped), storyAddendum(4, 1, filingLink(pass), "F2", "A second one")}
	if got := storyComments(f); !slices.Equal(got, addenda) {
		t.Errorf("the story's comments =\n%q\nwant the addenda\n%q", got, addenda)
	}
	for _, body := range storyComments(f) {
		if ms := github.ParseMarkers(body); len(ms) != 1 || ms[0].Kind != "addendum" {
			t.Errorf("an addendum's markers = %+v; want its own alone", ms)
		}
	}
	want := filingRecord(pass, "v1", []int{3, 4}, "- F1 #3: "+escaped, "- F2 #4: A second one")
	if got := filingComments(f, "filed"); !slices.Equal(got, []string{want}) {
		t.Errorf("the records =\n%q\nwant\n%q", got, []string{want})
	}
	order := []string{"POST /issues", "POST /issues/2/sub_issues", "POST /issues/2/comments",
		"POST /issues", "POST /issues/2/sub_issues", "POST /issues/2/comments", "POST /issues/1/comments"}
	if got := writesOf(f); !slices.Equal(got, order) {
		t.Errorf("the writes = %q; want %q", got, order)
	}
	noSearch(t, f)
}

// A crash after each write, each follow-up, its link and its addendum,
// then the record, and then a fresh daemon: the next passes file no
// follow-up, link or addendum twice, and post the record once.
func TestFollowUpsStoryCrashThenFreshDaemon(t *testing.T) {
	t.Parallel()
	o := storyOrigin(t)
	for n := 1; n <= 7; n++ {
		t.Run("after write "+strconv.Itoa(n), func(t *testing.T) {
			t.Parallel()
			var pass int64
			f := storyTask(func(t testing.TB, f *githubtest.Fake) {
				pass = filingPassOf(f, "v1", candidateData(t, "One", "Two"))
			})(t)
			f.FailWriteAfter(n)
			if err := newDaemon(t, f, o, nil).Iterate(context.Background()); err == nil {
				t.Fatalf("the pass that crashed after write %d gave no error", n)
			}
			if w := f.Writes(); len(w) != n || !w[n-1].Failed {
				t.Fatalf("the writes = %+v; want the crash at write %d, the last", w, n)
			}
			f.FailWrite(0)
			iterate(t, newDaemon(t, f, o, nil))
			iterate(t, newDaemon(t, f, o, nil))
			st := f.State()
			if len(st.Issues) != 4 || st.Issues[2].Title != "One" || st.Issues[3].Title != "Two" || st.Issues[2].Parent != 2 || st.Issues[3].Parent != 2 {
				t.Fatalf("the issues = %+v; want the task, the story and the two follow-ups under it, each once", st.Issues)
			}
			addenda := []string{storyAddendum(3, 1, filingLink(pass), "F1", "One"), storyAddendum(4, 1, filingLink(pass), "F2", "Two")}
			if got := storyComments(f); !slices.Equal(got, addenda) {
				t.Errorf("the story's comments =\n%q\nwant one addendum each\n%q", got, addenda)
			}
			want := filingRecord(pass, "v1", []int{3, 4}, "- F1 #3: One", "- F2 #4: Two")
			if got := filingComments(f, "filed"); !slices.Equal(got, []string{want}) {
				t.Errorf("the records =\n%q\nwant one\n%q", got, want)
			}
			noSearch(t, f)
		})
	}
}

// What's written already isn't written again: F1's follow-up, filed
// before the pass comment, as no listing since it holds it, is found among
// the story's sub-issues, linked, with its addendum, so it gets no write.
// F2's, filed but not linked, as by a pass that stopped there, is found
// by the listing, linked, and gets its addendum. F3 gets all three. The
// record names the three, in the pass's order.
func TestFollowUpsStoryFoundBefore(t *testing.T) {
	t.Parallel()
	var pass int64
	setup := storyTask(func(t testing.TB, f *githubtest.Fake) {
		first := f.AddIssue(app, "One, as filed", followUpBody(1, "v1", "F1"), "type:task", "priority:p3", "stage:intake")
		f.SetParent(first, 2)
		pass = filingPassOf(f, "v1", candidateData(t, "One", "Two", "Three"))
		f.AddComment(2, app, storyAddendum(first, 1, filingLink(pass), "F1", "One"))
		f.AddIssue(app, "Two, as filed", followUpBody(1, "v1", "F2"), "type:task", "priority:p3", "stage:intake")
	})
	githubtest.Replay(t, setup, filingPass(t, false))

	f := setup(t)
	writes := len(f.Writes())
	if err := filingPass(t, false)(f); err != nil {
		t.Fatal(err)
	}
	order := []string{"POST /issues/2/sub_issues", "POST /issues/2/comments",
		"POST /issues", "POST /issues/2/sub_issues", "POST /issues/2/comments", "POST /issues/1/comments"}
	if got := writesOf(f)[writes:]; !slices.Equal(got, order) {
		t.Errorf("the writes = %q; want %q", got, order)
	}
	st := f.State()
	if len(st.Issues) != 5 || st.Issues[4].Title != "Three" || st.Issues[3].Parent != 2 || st.Issues[4].Parent != 2 {
		t.Fatalf("the issues = %+v; want F3's filed, and F2's and F3's under the story", st.Issues)
	}
	addenda := []string{storyAddendum(3, 1, filingLink(pass), "F1", "One"), storyAddendum(4, 1, filingLink(pass), "F2", "Two"), storyAddendum(5, 1, filingLink(pass), "F3", "Three")}
	if got := storyComments(f); !slices.Equal(got, addenda) {
		t.Errorf("the story's comments =\n%q\nwant\n%q", got, addenda)
	}
	want := filingRecord(pass, "v1", []int{3, 4, 5}, "- F1 #3: One", "- F2 #4: Two", "- F3 #5: Three")
	if got := filingComments(f, "filed"); !slices.Equal(got, []string{want}) {
		t.Errorf("the records =\n%q\nwant\n%q", got, []string{want})
	}
}

// A sub-issue of the story in another repo is skipped: its number isn't
// this repo's, so another repo's #3 under the story doesn't make this
// repo's #3, F1's follow-up, filed before but not linked, a sub-issue, and
// it's linked.
func TestFollowUpsStoryOtherRepoSubIssue(t *testing.T) {
	t.Parallel()
	f := storyTask(func(t testing.TB, f *githubtest.Fake) {
		filingPassOf(f, "v1", candidateData(t, "One"))
		f.LinkSubIssue(2, "o/other", 3)
		if n := f.AddIssue(app, "One, as filed", followUpBody(1, "v1", "F1"), "type:task", "priority:p3", "stage:intake"); n != 3 {
			t.Fatalf("F1's follow-up is #%d, want #3", n)
		}
	})(t)
	if err := filingPass(t, false)(f); err != nil {
		t.Fatal(err)
	}
	if st := f.State(); len(st.Issues) != 3 || st.Issues[2].Parent != 2 || len(filingComments(f, "filed")) != 1 {
		t.Errorf("the issues = %+v, records %q; want #3 linked under the story, and the record", st.Issues, filingComments(f, "filed"))
	}
}

// An addendum someone else edited counts for nothing, as dirty content
// (SPEC §5.1): F1's follow-up, filed and linked, whose addendum eve edited,
// gets a new one, and the record follows. The edited one is left as it is.
func TestFollowUpsStoryEditedAddendum(t *testing.T) {
	t.Parallel()
	var pass, edited int64
	setup := storyTask(func(t testing.TB, f *githubtest.Fake) {
		pass = filingPassOf(f, "v1", candidateData(t, "One"))
		first := f.AddIssue(app, "One, as filed", followUpBody(1, "v1", "F1"), "type:task", "priority:p3", "stage:intake")
		f.SetParent(first, 2)
		edited = f.AddComment(2, app, storyAddendum(first, 1, filingLink(pass), "F1", "One"))
		f.EditComment(edited, "eve", storyAddendum(first, 1, filingLink(pass), "F1", "One")+" Eve was here.")
	})
	githubtest.Replay(t, setup, filingPass(t, false))

	f := run(t, setup, filingPass(t, false))
	addenda := []string{storyAddendum(3, 1, filingLink(pass), "F1", "One") + " Eve was here.", storyAddendum(3, 1, filingLink(pass), "F1", "One")}
	if got := storyComments(f); !slices.Equal(got, addenda) {
		t.Errorf("the story's comments =\n%q\nwant the edited addendum, then a clean one\n%q", got, addenda)
	}
	if got := filingComments(f, "filed"); len(got) != 1 {
		t.Errorf("the records = %q; want one", got)
	}
}

// followUpsFind's ties, with and without a parent: in one body, the first
// follow-up origin counts, so an App issue whose body names F2, then F1,
// is F2's, and F1 is filed. Of two App issues naming F1, the lower number
// wins, though the listing, oldest update first, gives the higher one
// first, and F2's is filed.
func TestFollowUpsFindTies(t *testing.T) {
	t.Parallel()
	for _, story := range []bool{false, true} {
		for _, tt := range []struct {
			name string
			// more files the App's issues, the first numbered issue.
			more func(t testing.TB, f *githubtest.Fake, issue int)
			// found is the candidate the first issue is found for, F1 or
			// F2: the other is filed.
			found string
		}{
			{"the first origin in a body counts", func(t testing.TB, f *githubtest.Fake, issue int) {
				both := "Two, then one.\n\n<!-- issuedog:origin verify=#1 run=v1 item=F2 -->\n<!-- issuedog:origin verify=#1 run=v1 item=F1 -->"
				if n := f.AddIssue(app, "Two, as filed", both, "type:task", "priority:p3", "stage:intake"); n != issue {
					t.Fatalf("the App's issue is #%d, want #%d", n, issue)
				}
			}, "F2"},
			{"the lower number wins", func(t testing.TB, f *githubtest.Fake, issue int) {
				lower := f.AddIssue(app, "One, as filed", followUpBody(1, "v1", "F1"), "type:task", "priority:p3", "stage:intake")
				f.AddIssue(app, "One, filed again", followUpBody(1, "v1", "F1"), "type:task", "priority:p3", "stage:intake")
				f.AddLabel(lower, app, "scope:booking") // its update is now the newer
				if lower != issue {
					t.Fatalf("the lower is #%d, want #%d", lower, issue)
				}
			}, "F1"},
		} {
			t.Run(tt.name+", in a story "+strconv.FormatBool(story), func(t *testing.T) {
				t.Parallel()
				first, setup := 2, filingTask
				if story {
					first, setup = 3, storyTask
				}
				var pass int64
				f := setup(func(t testing.TB, f *githubtest.Fake) {
					pass = filingPassOf(f, "v1", candidateData(t, "One", "Two"))
					tt.more(t, f, first)
				})(t)
				if err := filingPass(t, false)(f); err != nil {
					t.Fatal(err)
				}
				filed := len(f.State().Issues) // the last, the one filed
				f1, f2 := first, filed
				if tt.found == "F2" {
					f1, f2 = filed, first
				}
				want := filingRecord(pass, "v1", []int{f1, f2}, "- F1 #"+strconv.Itoa(f1)+": One", "- F2 #"+strconv.Itoa(f2)+": Two")
				if got := filingComments(f, "filed"); !slices.Equal(got, []string{want}) {
					t.Errorf("the records =\n%q\nwant\n%q", got, []string{want})
				}
			})
		}
	}
}

// Of two App issues naming one candidate, the one kept is logged as a
// warning, naming both. A follow-up both lists hold, the listing since the
// pass comment and the story's sub-issues, is one issue, so it's no twin
// of its own, and nothing is logged.
func TestFollowUpsFindTwinWarning(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logtest.Capture).
	for _, tt := range []struct {
		name string
		more func(f *githubtest.Fake)
		want string // "" for no warning
	}{
		{"one issue in both lists", func(f *githubtest.Fake) {
			f.SetParent(f.AddIssue(app, "One, as filed", followUpBody(1, "v1", "F1"), "type:task", "priority:p3", "stage:intake"), 2)
		}, ""},
		{"two issues", func(f *githubtest.Fake) {
			f.AddIssue(app, "One, as filed", followUpBody(1, "v1", "F1"), "type:task", "priority:p3", "stage:intake")
			f.AddIssue(app, "One, filed again", followUpBody(1, "v1", "F1"), "type:task", "priority:p3", "stage:intake")
		}, "o/r: #3 and #4 are both the follow-up of candidate F1 of #1's pass in run v1, so #3 is the one kept"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := storyTask(func(t testing.TB, f *githubtest.Fake) {
				filingPassOf(f, "v1", candidateData(t, "One"))
				tt.more(f)
			})(t)
			stop := logtest.Capture(t, logr.W)
			err := filingPass(t, false)(f)
			logs := stop()
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(logs, "are both the follow-up"); got != (tt.want != "") || tt.want != "" && !strings.Contains(logs, tt.want) {
				t.Errorf("the warnings =\n%s\nwant %q", logs, tt.want)
			}
		})
	}
}

// A link GitHub refuses with a 422 is a failure no later pass gets past,
// as the followups state error, once: for a story with 100 sub-issues,
// GitHub's cap, and for a follow-up someone linked under another parent.
// The follow-up stays filed, gets no addendum, and no record is posted.
// The error names the follow-up and the story, and quotes nothing of
// GitHub's answer.
func TestFollowUpsStoryLinkRefused(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		more func(t testing.TB, f *githubtest.Fake)
	}{
		{"a story with 100 sub-issues", func(t testing.TB, f *githubtest.Fake) {
			for n := range 99 { // the task is the 100th
				f.LinkSubIssue(2, "o/other", n+1)
			}
		}},
		{"a follow-up under another parent", func(t testing.TB, f *githubtest.Fake) {
			found := f.AddIssue(app, "One, as filed", followUpBody(1, "v1", "F1"), "type:task", "priority:p3", "stage:intake")
			other := f.AddIssue("owner", "Another story", "Another.")
			f.SetParent(found, other)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var pass int64
			setup := storyTask(func(t testing.TB, f *githubtest.Fake) {
				pass = filingPassOf(f, "v1", candidateData(t, "One"))
				tt.more(t, f)
			})
			githubtest.Replay(t, setup, filingPass(t, false))

			f := run(t, setup, filingPass(t, false))
			if got, want := filingComments(f, "error"), []string{filingErr(pass, filingMove(t, f), "GitHub refused to link #3, the follow-up of candidate F1, under #2, its story")}; !slices.Equal(got, want) {
				t.Errorf("the state errors =\n%q\nwant\n%q", got, want)
			}
			if got := filingComments(f, "filed"); len(got) != 0 || len(storyComments(f)) != 0 || f.State().Issues[2].Parent == 2 {
				t.Errorf("the records = %q, the story's comments %q, #3 under #%d; want none, none, and not under the story",
					got, storyComments(f), f.State().Issues[2].Parent)
			}
		})
	}
}

// A held task files its follow-ups as any other does, as its pass came
// before the hold, and in a story links each and posts its addendum too,
// so the record stands. The task stays held.
func TestFollowUpsStoryHeldTask(t *testing.T) {
	t.Parallel()
	var pass int64
	f := storyTask(func(t testing.TB, f *githubtest.Fake) {
		pass = filingPassOf(f, "v1", candidateData(t, "One"))
		f.AddComment(1, "owner", "/hold")
	})(t)
	if err := filingPass(t, false)(f); err != nil {
		t.Fatal(err)
	}
	if got := pickStage(f, 1); got != "stage:held" {
		t.Fatalf("the task is at %s, want stage:held", got)
	}
	if st := f.State(); len(st.Issues) != 3 || st.Issues[2].Parent != 2 {
		t.Fatalf("the issues = %+v; want the follow-up under the story", st.Issues)
	}
	if got, want := storyComments(f), []string{storyAddendum(3, 1, filingLink(pass), "F1", "One")}; !slices.Equal(got, want) {
		t.Errorf("the story's comments =\n%q\nwant\n%q", got, want)
	}
	if got := filingComments(f, "filed"); len(got) != 1 {
		t.Errorf("the records = %q; want one", got)
	}
}

// A candidate's title over GitHub's 256 characters, which GitHub refuses
// with a 422 on every try, is cut short to fit, ending in "…", in the
// issue's title, the record and the addendum, so the pass is filed.
func TestFollowUpsLongTitle(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", 300)
	short := strings.Repeat("é", 255) + "…"
	var pass int64
	f := storyTask(func(t testing.TB, f *githubtest.Fake) {
		pass = filingPassOf(f, "v1", candidateData(t, long))
	})(t)
	if err := filingPass(t, false)(f); err != nil {
		t.Fatal(err)
	}
	if got := f.State().Issues[2].Title; got != short {
		t.Errorf("the follow-up's title = %q (%d runes); want %q", got, len([]rune(got)), short)
	}
	if got, want := storyComments(f), []string{storyAddendum(3, 1, filingLink(pass), "F1", short)}; !slices.Equal(got, want) {
		t.Errorf("the story's comments =\n%q\nwant\n%q", got, want)
	}
	if got, want := filingComments(f, "filed"), []string{filingRecord(pass, "v1", []int{3}, "- F1 #3: "+short)}; !slices.Equal(got, want) {
		t.Errorf("the records =\n%q\nwant\n%q", got, want)
	}
}

// A task whose parent is in another repo files nothing, as filing under a
// story there isn't built (SPEC §14): this repo's issue of that number
// isn't its story. The visit makes no request.
func TestFollowUpsParentElsewhere(t *testing.T) {
	t.Parallel()
	f := filingTask(func(t testing.TB, f *githubtest.Fake) {
		filingPassOf(f, "v1", candidateData(t, "One"))
	})(t)
	st := filingStep(t, f, false, "booking")
	s, err := st.Client.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	s.Parent, s.ParentRepo = 1, "o/other"
	from, writes := len(f.Requests()), len(f.Writes())
	if err := st.FollowUpsVisit(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if got := f.Requests()[from:]; len(got) != 0 || len(f.Writes()) != writes {
		t.Errorf("the visit sent %+v; want no request", got)
	}
}

// storyApproval is approvalFiled with follow-up #1 under story #2, whose
// addendum for it, comment 1001, the App posted, then more.
func storyApproval(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return approvalFiled(func(t testing.TB, f *githubtest.Fake) {
		storyAddendumOf(t, f, 1, 1001)
		more(t, f)
	})
}

// storyApproved is the App's comment approving follow-up #1 on who's 👍
// r on its addendum, comment 1001 on story #2.
func storyApproved(who string, r int64) string {
	return fmt.Sprintf("`%s`'s 👍 on [its addendum](https://github.com/o/r/issues/2#issuecomment-1001) on #2 approves this follow-up, so it goes to `ready`.\n\n"+
		"<!-- issuedog:transition from=intake to=ready cmd=r%d -->", who, r)
}

// The earliest trusted 👍 on a follow-up's clean addendum, not a bot's,
// approves it as a 👍 on the issue does: it moves intake → ready, keyed by
// the 👍, and its comment names the 👍's user and links the addendum.
// eve's 👍, untrusted, the helper bot's and the App's, both trusted, and
// owner's ❤️ count for nothing, nor does owner's later 👍. The App writes
// nothing to the story, and the pass after writes nothing.
func TestFollowUpApprovedByAddendum(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	var thumb int64
	setup := storyApproval(func(t testing.TB, f *githubtest.Fake) {
		for _, login := range []string{"eve", "helper[bot]", app} {
			f.AddReaction(1001, login, "+1")
		}
		f.AddReaction(1001, "owner", "heart")
		thumb = f.AddReaction(1001, "owner2", "+1")
		f.AddReaction(1001, "owner", "+1")
	})
	githubtest.Replay(t, setup, approvalPass(t, o))

	f := setup(t)
	d := approvalDaemon(t, f, o)
	iterate(t, d)
	approvalState(t, f, []string{"priority:p3", "stage:ready"}, []string{storyApproved("owner2", thumb)})
	if got, want := writesOf(f), []string{"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:intake"}; !slices.Equal(got, want) {
		t.Errorf("writes %q, want %q", got, want)
	}
	writes := len(f.Writes())
	iterate(t, d)
	if got := f.Writes()[writes:]; len(got) != 0 {
		t.Errorf("the pass after wrote %+v, want nothing", got)
	}
}

// Of a 👍 on the issue and one on its addendum, both trusted, the earlier
// approves the follow-up, by reaction ID, though the issue's reactions are
// read first, and so come first, whichever is earlier.
func TestFollowUpEarlierThumb(t *testing.T) {
	t.Parallel()
	for _, addendumFirst := range []bool{true, false} {
		t.Run("the addendum's first "+strconv.FormatBool(addendumFirst), func(t *testing.T) {
			t.Parallel()
			var first int64
			f := storyApproval(func(t testing.TB, f *githubtest.Fake) {
				if addendumFirst {
					first = f.AddReaction(1001, "owner2", "+1")
					f.AddIssueReaction(1, "owner", "+1")
				} else {
					first = f.AddIssueReaction(1, "owner", "+1")
					f.AddReaction(1001, "owner2", "+1")
				}
			})(t)
			iterate(t, approvalDaemon(t, f, newOrigin(t)))
			want := approvalMove("owner", first, "")
			if addendumFirst {
				want = storyApproved("owner2", first)
			}
			approvalState(t, f, []string{"priority:p3", "stage:ready"}, []string{want})
		})
	}
}

// An addendum someone else edited counts for nothing, as dirty content
// (SPEC §5.1): owner's 👍 on it approves nothing, and its reactions aren't
// read. A clean addendum posted after it, as the next filing posts one,
// counts: owner's 👍 there approves the follow-up.
func TestFollowUpEditedAddendumThumb(t *testing.T) {
	t.Parallel()
	f := storyApproval(func(t testing.TB, f *githubtest.Fake) {
		f.EditComment(1001, "owner", "Edited.\n\n<!-- issuedog:addendum child=1 -->")
		f.AddReaction(1001, "owner", "+1")
	})(t)
	d := approvalDaemon(t, f, newOrigin(t))
	iterate(t, d)
	iterate(t, d)
	approvalState(t, f, []string{"priority:p3", "stage:intake"}, nil)
	for _, r := range f.Requests() {
		if r.Path == "/issues/comments/1001/reactions" {
			t.Errorf("read the edited addendum's reactions: %+v", r)
		}
	}
	clean := f.AddComment(2, app, storyAddendum(1, 7, "https://github.com/o/r/issues/7#issuecomment-1", "F1", "Refuse a quote that expires mid-booking"))
	r := f.AddReaction(clean, "owner", "+1")
	iterate(t, d)
	approvalState(t, f, []string{"priority:p3", "stage:ready"}, []string{
		fmt.Sprintf("`owner`'s 👍 on [its addendum](https://github.com/o/r/issues/2#issuecomment-%d) on #2 approves this follow-up, so it goes to `ready`.\n\n"+
			"<!-- issuedog:transition from=intake to=ready cmd=r%d -->", clean, r)})
}

// /track on a follow-up in a story is rejected, with the App's 👎 and one
// reply saying a 👍 on it or on its addendum approves it.
func TestFollowUpStoryTrack(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	setup := storyApproval(func(t testing.TB, f *githubtest.Fake) {
		if id := f.AddComment(1, "owner", "/track"); id != 1002 {
			t.Fatalf("/track's comment ID = %d, want 1002", id)
		}
	})
	githubtest.Replay(t, setup, approvalPass(t, o))

	f := run(t, setup, approvalPass(t, o))
	approvalState(t, f, []string{"priority:p3", "stage:intake"}, []string{"/track",
		"> /track\n\nI didn't apply this command: this is a follow-up, which `/track` doesn't admit. " +
			"React 👍 to it or to its addendum on its story to approve it, or close it to drop it.\n\n<!-- issuedog:reply reason=rejected cmd=1002 -->"})
}

// A whole run on the daemon main builds (rig.wireAsMain), with triage as
// its Unlabeled stage and every stage registered: a follow-up waiting in a
// story, at planned, starts no run of any stage, triage's included. A pass
// where nothing changed sends only conditional GETs answered 304, each
// counted whatever its answer: the poll, the visit's read of the
// follow-up's timeline, as any visited issue's, and two reads for a 👍,
// its reactions and its addendum's. Then owner's 👍 on the addendum
// approves it, and at ready it waits, unpicked, as any task with a parent
// does until #624: no run starts, and it stays at ready.
func TestFollowUpStoryWaitsRun(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	story := fileAt(f, "planned", "A story.")
	f.SetType(story, "Story")
	fu := f.AddIssue(app, "Refuse a quote that expires mid-booking", approvalBody)
	f.SetType(fu, "task")
	f.AddLabel(fu, app, "priority:p3")
	f.AddLabel(fu, app, "stage:intake")
	f.SetParent(fu, story)
	addendum := f.AddComment(story, app, storyAddendum(fu, 7, "https://github.com/o/r/issues/7#issuecomment-1", "F1", "Refuse a quote that expires mid-booking"))
	g := newRig(t, f, newOrigin(t), t.TempDir())
	if err := os.WriteFile(g.path("claude", "claude"), []byte(wholeRunClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	g.wireAsMain(g.d.Config)
	for range 2 {
		iterate(t, g.d)
	}
	if n := g.runs(); n != 0 {
		t.Errorf("%d claude runs; want none, of triage or any stage", n)
	}
	from := len(f.Requests())
	iterate(t, g.d)
	var sent []string
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("a pass where nothing changed sent %+v; want only conditional GETs answered 304", r)
		}
		sent = append(sent, r.Method+" "+r.Path)
	}
	want := []string{"GET /issues", "GET /issues/comments", "GET /issues/2/timeline", "GET /issues/2/reactions",
		"GET /issues/comments/" + strconv.FormatInt(addendum, 10) + "/reactions"}
	if !slices.Equal(sent, want) {
		t.Errorf("a pass where nothing changed sent %q; want %q", sent, want)
	}
	f.AddReaction(addendum, "owner", "+1")
	for range 3 {
		iterate(t, g.d)
	}
	if got := pickStage(f, fu); got != "stage:ready" {
		t.Errorf("the follow-up is at %s after owner's 👍 on its addendum; want stage:ready", got)
	}
	if n := g.runs(); n != 0 {
		t.Errorf("%d claude runs with the approved follow-up at ready; want none, as it's in a story", n)
	}
	if l := g.lease(); l.RunID != "" {
		t.Errorf("the lease = %+v; want none", l)
	}
}

// With step 1 as NewStep builds it, every 👍 trigger wired, a trusted 👍
// on a follow-up's addendum approves it: the daemon's snapshot of the
// story gives the addendum.
func TestFollowUpAddendumNewStep(t *testing.T) {
	t.Parallel()
	f := storyApproval(func(t testing.TB, f *githubtest.Fake) { f.AddReaction(1001, "owner", "+1") })(t)
	iterate(t, newStepDaemon(t, f))
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"priority:p3", "stage:ready"}) {
		t.Errorf("the follow-up's labels are %q after a 👍 on its addendum; want ready", l)
	}
}

// The pass whose step 1 files the follow-ups in a story touches what it
// writes to: the task, the story, which gets a link or an addendum, and
// F1's follow-up, filed before, if it's found unlinked, as it gets its
// link. Work gets none of them that pass, though the story and F1's
// follow-up come after the task, and the next pass reads them again and
// gives them to Work. The story is touched whichever of the two it gets.
func TestFollowUpsStoryTouched(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		linked  bool // F1's follow-up is under the story already
		posted  bool // and its addendum is on the story already
		touched []int
	}{
		{"F1 unlinked, F2 filed", false, false, []int{1, 2, 3}},
		{"F1 linked, with no addendum", true, false, []int{1, 2}},
		{"F1 unlinked, with its addendum", false, true, []int{1, 2, 3}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := storyTask(func(t testing.TB, f *githubtest.Fake) {
				titles := []string{"One", "Two"}
				if tt.linked || tt.posted { // F1 alone, so the story gets only what F1 lacks
					titles = titles[:1]
				}
				pass := filingPassOf(f, "v1", candidateData(t, titles...))
				first := f.AddIssue(app, "One, as filed", followUpBody(1, "v1", "F1"), "type:task", "priority:p3", "stage:intake")
				if tt.linked {
					f.SetParent(first, 2)
				}
				if tt.posted {
					f.AddComment(2, app, storyAddendum(first, 1, filingLink(pass), "F1", "One"))
				}
			})(t)
			d := newDaemon(t, f, storyOrigin(t), nil)
			var worked [][]int
			d.Work = func(_ context.Context, ready []daemon.Ready) error {
				var ns []int
				for _, r := range ready {
					ns = append(ns, r.S.Number)
				}
				worked = append(worked, ns)
				return nil
			}
			iterate(t, d)
			if len(filingComments(f, "filed")) != 1 {
				t.Fatalf("the records = %q; want one", filingComments(f, "filed"))
			}
			iterate(t, d)
			if len(worked) != 2 {
				t.Fatalf("Work got %v; want two passes", worked)
			}
			for _, n := range tt.touched {
				if slices.Contains(worked[0], n) || !slices.Contains(worked[1], n) {
					t.Errorf("Work got %v; want no #%d on the filing's pass, and #%d on the next", worked, n, n)
				}
			}
		})
	}
}

// Outside Iterate's visits, the daemon holds no snapshot to give.
func TestHeldSnapOutsideIterate(t *testing.T) {
	t.Parallel()
	f := storyApproval(func(testing.TB, *githubtest.Fake) {})(t)
	d := newStepDaemon(t, f)
	iterate(t, d)
	if s := d.Step.HeldSnap(2); s != nil {
		t.Errorf("HeldSnap(2) after the pass = #%d; want nil", s.Number)
	}
	var st daemon.Step
	if s := st.HeldSnap(2); s != nil {
		t.Errorf("a Step's HeldSnap(2) = #%d; want nil", s.Number)
	}
}

// With two follow-ups under one story, each with its addendum, a 👍 on the
// second's addendum approves the second alone: an addendum approves only
// the follow-up its marker names. The first stays at intake, and nothing
// is written to it.
func TestFollowUpAddendumNamesItsChild(t *testing.T) {
	t.Parallel()
	var thumb int64
	f := storyApproval(func(t testing.TB, f *githubtest.Fake) {
		second := f.AddIssue(app, "A second follow-up", approvalBody)
		f.SetType(second, "task")
		f.AddLabel(second, app, "priority:p3")
		f.AddLabel(second, app, "stage:intake")
		f.SetParent(second, 2)
		c := f.AddComment(2, app, storyAddendum(second, 7, "https://github.com/o/r/issues/7#issuecomment-1", "F2", "A second follow-up"))
		thumb = f.AddReaction(c, "owner", "+1")
	})(t)
	iterate(t, approvalDaemon(t, f, newOrigin(t)))
	approvalState(t, f, []string{"priority:p3", "stage:intake"}, nil)
	second := f.State().Issues[2]
	if !slices.Equal(second.Labels, []string{"priority:p3", "stage:ready"}) {
		t.Errorf("the second follow-up's labels = %q; want it at ready", second.Labels)
	}
	if len(second.Comments) != 1 || !strings.Contains(second.Comments[0].Body, fmt.Sprintf("from=intake to=ready cmd=r%d -->", thumb)) {
		t.Errorf("the second follow-up's comments = %+v; want its move to ready on owner's 👍", second.Comments)
	}
}

// A comment someone else posts on the story with an addendum marker is no
// addendum (SPEC §5.1): owner's 👍 on it approves nothing, though owner is
// trusted and wrote it.
func TestFollowUpForgedAddendumThumb(t *testing.T) {
	t.Parallel()
	f := approvalFiled(func(t testing.TB, f *githubtest.Fake) {
		story := f.AddIssue("owner", "A story", "The story.")
		f.SetParent(1, story)
		c := f.AddComment(story, "owner", "Mine.\n\n<!-- issuedog:addendum child=1 -->")
		f.AddReaction(c, "owner", "+1")
	})(t)
	d := approvalDaemon(t, f, newOrigin(t))
	iterate(t, d)
	iterate(t, d)
	approvalState(t, f, []string{"priority:p3", "stage:intake"}, nil)
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes %+v, want none", w)
	}
}

// A 👍 on the addendum that a marker on the follow-up names counts for
// nothing, as one on the follow-up itself doesn't.
func TestFollowUpNamedAddendumThumb(t *testing.T) {
	t.Parallel()
	f := storyApproval(func(t testing.TB, f *githubtest.Fake) {
		named := f.AddReaction(1001, "owner", "+1")
		f.AddComment(1, app, fmt.Sprintf("<!-- issuedog:reply reason=rejected cmd=r%d -->", named))
	})(t)
	d := approvalDaemon(t, f, newOrigin(t))
	iterate(t, d)
	if is := f.State().Issues[0]; !slices.Equal(is.Labels, []string{"priority:p3", "stage:intake"}) {
		t.Errorf("labels %q; want the follow-up still at intake", is.Labels)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes %+v, want none", w)
	}
}

// Someone else's comment on the story carrying the follow-up's addendum
// marker, posted after the pass, where the filing looks, doesn't stand in
// for the App's: the filing still posts the real addendum.
func TestFollowUpsStoryForgedAddendum(t *testing.T) {
	t.Parallel()
	const forged = "Already done.\n\n<!-- issuedog:addendum child=3 -->"
	var pass int64
	f := storyTask(func(t testing.TB, f *githubtest.Fake) {
		pass = filingPassOf(f, "v1", candidateData(t, "One"))
		f.AddComment(2, "owner", forged)
	})(t)
	if err := filingPass(t, false)(f); err != nil {
		t.Fatal(err)
	}
	want := []string{forged, storyAddendum(3, 1, filingLink(pass), "F1", "One")}
	if got := storyComments(f); !slices.Equal(got, want) {
		t.Errorf("the story's comments =\n%q\nwant the App's addendum after the forged one\n%q", got, want)
	}
}
