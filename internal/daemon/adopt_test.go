package daemon_test

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/logtest"
	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/issuedog/internal/trust"
	"github.com/mertenvg/logr/v2"
)

// The issues adoptByHand files, by number.
const (
	adoptEpic       = 1 // an epic at planned, with its spec, plan and created record
	adoptStory      = 2 // the epic's story S1, with its body's origin, its plan and a verify
	adoptLabelsTask = 3 // the story's task T1, with only labels and its body's origin
	adoptBareTask   = 4 // a task with only labels
	adoptHumanTask  = 5 // the story's human task T3, with a percent-encoded check
	adoptClosed     = 6 // closed, with a stage label
	adoptNoStage    = 7 // open, with no stage label
	adoptPR         = 8 // a pull request with a stage label
	adoptUncreated  = 9 // an epic at planned with no created record and no plan-data
)

// adoptPipeline are adoptByHand's open pipeline issues, which adopt reads.
var adoptPipeline = []int{adoptEpic, adoptStory, adoptLabelsTask, adoptBareTask, adoptHumanTask, adoptUncreated}

// adoptFirstComment is the ID of adoptByHand's first comment, on the epic.
// The epic's comments are 1001 to 1006, mallory's answer 1002, and the
// story's 1007 to 1009. The epic with no created record's are 1013 and
// 1014 (adoptUncreatedComment).
const adoptFirstComment = 1001

// adoptUncreatedComment is the ID of the first comment on the epic with
// no created record.
const adoptUncreatedComment = 1013

// adoptSources is how many sources adoptByHand's owner wrote markers of the
// kinds adopt reposts in: the epic's five comments, the story's body and
// two comments, the two tasks' bodies, and the other epic's two comments.
const adoptSources = 12

// adoptByHand is a repo run by hand, as this one was before going live
// (SPEC §15): every marker on its issues is owner's, and none is the App's.
// It's on a fresh fake with owner an admin and mallory a writer, and its
// git remote is a new origin whose CODEOWNERS names owner, which it returns.
func adoptByHand(t testing.TB) (*githubtest.Fake, *origin) {
	t.Helper()
	f := newFake(t)
	o := newOrigin(t)
	f.SetGitRemote(o.url)
	label := func(n int, labels ...string) {
		for _, l := range labels {
			f.AddLabel(n, "owner", l)
		}
	}

	epic := f.AddIssue("owner", "The epic", "The epic's spec.")
	label(epic, "stage:planned", "type:epic", "priority:p1")
	if id := f.AddComment(epic, "owner", "Spec rev 1.\n<!-- issuedog:revision rev=1 body=4208a67d -->"); id != adoptFirstComment {
		t.Fatalf("the first comment's ID = %d, want %d", id, adoptFirstComment)
	}
	f.AddComment(epic, "mallory", "An answer after the spec.")
	f.AddComment(epic, "owner", "Approved.\n<!-- issuedog:transition from=spec-review to=planning rev=1 body=4208a67d cmd=r444222706 -->")
	f.AddComment(epic, "owner", "## Plan\n- S1 the story\n<!-- issuedog:revision kind=plan rev=1 body=07e95f15 -->")
	f.AddComment(epic, "owner", "<!-- issuedog:transition from=plan-review to=planned rev=1 cmd=r444226234 -->")
	f.AddComment(epic, "owner", "Created\n- S1 #2\n<!-- issuedog:created rev=1 children=2 -->")

	story := f.AddIssue("owner", "The story", "The story.\n<!-- issuedog:origin plan=#1 rev=1 item=S1 -->")
	label(story, "stage:planned", "type:story", "priority:p1")
	f.SetParent(story, epic)
	f.AddComment(story, "owner", "## Plan\n- T1 the task\n- T3 the ruleset\n<!-- issuedog:revision kind=plan rev=1 body=d2dd7be2 -->")
	f.AddComment(story, "owner", "<!-- issuedog:transition from=plan-review to=planned rev=1 cmd=r444241553 -->")
	f.AddComment(story, "owner", "Passed.\n<!-- issuedog:verify verdict=pass attempt=1 sha=d34da9c -->")

	// A task filed by hand, as a story's tasks were while the pipeline ran
	// by hand: its labels and its body's origin.
	task := f.AddIssue("owner", "The task", "The task.\n<!-- issuedog:origin plan=#2 rev=1 item=T1 -->")
	label(task, "stage:ready", "type:task", "priority:p1")
	f.SetParent(task, story)

	bare := f.AddIssue("owner", "A bare task", "A task with only labels.")
	label(bare, "stage:ready", "type:task", "priority:p2")

	human := f.AddIssue("owner", "The ruleset", "Add the ruleset.\n"+
		"<!-- issuedog:origin plan=#2 rev=1 item=T3 check=ruleset value=issuedog%2Fverify human=1 -->")
	label(human, "stage:needs-human", "type:task", "priority:p1")
	f.SetParent(human, story)
	// The tasks at ready wait on the human task, so no pass picks one.
	f.SetBlockedBy(task, human)
	f.SetBlockedBy(bare, human)

	closed := f.AddIssue("owner", "Closed", "Done.")
	label(closed, "stage:pr-open")
	f.AddComment(closed, "owner", "<!-- issuedog:transition from=verifying to=pr-open run=v1 -->")
	f.CloseBy(closed, "owner")
	noStage := f.AddIssue("owner", "No stage", "Not in the pipeline.")
	label(noStage, "priority:p2")
	f.AddComment(noStage, "owner", "<!-- issuedog:transition from=intake to=ready cmd=1 -->")
	pr := f.AddIssue("owner", "A PR", "A pull request.")
	label(pr, "stage:ready")
	f.AddComment(pr, "owner", "<!-- issuedog:transition from=intake to=ready cmd=1 -->")
	f.SetPullRequest(pr)
	if pr != adoptPR {
		t.Fatalf("the PR is #%d, want #%d", pr, adoptPR)
	}

	// An epic planned by hand whose children were never created by the
	// App: no created record and no plan-data.
	uncreated := f.AddIssue("owner", "Planned, not created", "Another epic's spec.")
	label(uncreated, "stage:planned", "type:epic", "priority:p2")
	if id := f.AddComment(uncreated, "owner", "## Plan\n- S1 a story\n<!-- issuedog:revision kind=plan rev=1 body=1c633d38 -->"); id != adoptUncreatedComment {
		t.Fatalf("the uncreated epic's first comment's ID = %d, want %d", id, adoptUncreatedComment)
	}
	f.AddComment(uncreated, "owner", "<!-- issuedog:transition from=plan-review to=planned rev=1 cmd=r484326042 -->")

	// The epic is updated last, so a listing by update has it last.
	f.AddComment(epic, "mallory", "A late comment.")
	return f, o
}

// adoptTrust is trust from main on o, as adopt loads it (trust.Load).
func adoptTrust(t testing.TB, f *githubtest.Fake, o *origin) *trust.Set {
	t.Helper()
	ctx := context.Background()
	clone := repo.New(o.url, filepath.Join(t.TempDir(), "r.git"))
	if err := clone.Sync(ctx, ""); err != nil {
		t.Fatal(err)
	}
	tr, err := trust.Load(ctx, clone, f.Client(t).Client, "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// adoptRun runs Adopt for login on a fresh Applier of f, as a run of
// issuedog adopt does.
func adoptRun(t testing.TB, f *githubtest.Fake, tr github.Trust, login string, dryRun bool) (daemon.AdoptReport, error) {
	a := &daemon.Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: slug}
	return a.Adopt(context.Background(), tr, login, dryRun)
}

// adoptComments are the authors and bodies of issue n's comments, from the
// from'th on.
func adoptComments(f *githubtest.Fake, n, from int) []string {
	var bodies []string
	for _, is := range f.State().Issues {
		if is.Number != n {
			continue
		}
		for _, c := range is.Comments[min(from, len(is.Comments)):] {
			bodies = append(bodies, c.Author+": "+c.Body)
		}
	}
	return bodies
}

// adoptPost is the App comment adopt posts on issue n for owner's source
// from, with markers, as "<author>: <body>".
func adoptPost(n int, from int64, markers ...string) string {
	link, source, f := "https://github.com/o/r/issues/"+strconv.Itoa(n), "description", "body"
	if from != state.BodyID {
		link += "#issuecomment-" + strconv.FormatInt(from, 10)
		source, f = "comment", strconv.FormatInt(from, 10)
	}
	return app + ": I've copied these markers from `owner`'s [" + source + "](" + link + "), written while this pipeline ran by hand, " +
		"so I read them as mine.\n\n" + strings.Join(markers, "\n") + "\n<!-- issuedog:adopted from=" + f + " login=owner -->"
}

// adoptWant are the App comments adopt posts on adoptByHand, by issue, in
// order.
var adoptWant = map[int][]string{
	adoptEpic: {
		adoptPost(adoptEpic, 1001, "<!-- issuedog:revision rev=1 body=4208a67d seen=1001 -->"),
		adoptPost(adoptEpic, 1003, "<!-- issuedog:transition from=spec-review to=planning rev=1 body=4208a67d cmd=r444222706 seen=1003 -->"),
		adoptPost(adoptEpic, 1004, "<!-- issuedog:revision kind=plan rev=1 body=07e95f15 seen=1004 -->"),
		adoptPost(adoptEpic, 1005, "<!-- issuedog:transition from=plan-review to=planned rev=1 cmd=r444226234 seen=1005 -->"),
		adoptPost(adoptEpic, 1006, "<!-- issuedog:created rev=1 children=2 -->"),
	},
	adoptStory: {
		adoptPost(adoptStory, state.BodyID, "<!-- issuedog:origin plan=#1 rev=1 item=S1 -->"),
		adoptPost(adoptStory, 1007, "<!-- issuedog:revision kind=plan rev=1 body=d2dd7be2 seen=1007 -->"),
		adoptPost(adoptStory, 1008, "<!-- issuedog:transition from=plan-review to=planned rev=1 cmd=r444241553 seen=1008 -->"),
	},
	adoptUncreated: {
		adoptPost(adoptUncreated, 1013, "<!-- issuedog:revision kind=plan rev=1 body=1c633d38 seen=1013 -->"),
		adoptPost(adoptUncreated, 1014, "<!-- issuedog:transition from=plan-review to=planned rev=1 cmd=r484326042 seen=1014 -->"),
	},
	adoptLabelsTask: {adoptPost(adoptLabelsTask, state.BodyID, "<!-- issuedog:origin plan=#2 rev=1 item=T1 -->")},
	adoptHumanTask: {adoptPost(adoptHumanTask, state.BodyID,
		"<!-- issuedog:origin plan=#2 rev=1 item=T3 check=ruleset human=1 value=issuedog%2Fverify -->")},
}

// adoptCommentCounts is each issue's comment count, by number.
func adoptCommentCounts(f *githubtest.Fake) map[int]int {
	counts := map[int]int{}
	for _, is := range f.State().Issues {
		counts[is.Number] = len(is.Comments)
	}
	return counts
}

// Adopt refuses a login that isn't trusted (SPEC §6.1), a bot's and the
// App's included, and reads and posts nothing on any issue.
func TestAdoptRefusesLogin(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ login, want string }{
		{"mallory", "adopt mallory's markers: mallory isn't a trusted user (SPEC §6.1)"},
		{"stranger", "adopt stranger's markers: stranger isn't a trusted user (SPEC §6.1)"},
		{"dependabot[bot]", "adopt dependabot[bot]'s markers: a bot's, the App's included, are never adopted"},
		{app, "adopt " + app + "'s markers: a bot's, the App's included, are never adopted"},
		{"Owner[BOT]", "adopt Owner[BOT]'s markers: a bot's, the App's included, are never adopted"},
		{"", "adopt: no login"},
	} {
		f, o := adoptByHand(t)
		tr := adoptTrust(t, f, o)
		from := len(f.Requests())
		r, err := adoptRun(t, f, tr, tt.login, false)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q: Adopt = %+v, %v; want the error %q", tt.login, r, err, tt.want)
		}
		if w := f.Writes(); len(w) != 0 {
			t.Errorf("%q: wrote %+v, want nothing", tt.login, w)
		}
		for _, req := range f.Requests()[from:] {
			if strings.HasPrefix(req.Path, "/issues") {
				t.Errorf("%q: read %s, want no issue read", tt.login, req.Path)
			}
		}
	}
}

// While state.Adopt refuses any open pipeline issue, Adopt posts nothing,
// and logs and returns each refusal, naming its issue, with no error. It
// reads every open pipeline issue first, so it names each one refused.
func TestAdoptRefusesIssues(t *testing.T) {
	// Not parallel: it reads the process-wide logr output.
	f, o := adoptByHand(t)
	planReview := f.AddIssue("owner", "At plan-review", "A plan.")
	f.AddLabel(planReview, "owner", "stage:plan-review")
	f.AddComment(planReview, "owner", "## Plan\n<!-- issuedog:transition from=planning to=plan-review run=p1 -->")
	held := f.AddIssue("owner", "Held", "A held task.")
	f.AddLabel(held, "owner", "stage:held")
	f.AddComment(held, "owner", "<!-- issuedog:transition from=intake to=ready cmd=1 -->")
	f.AddComment(held, "owner", "<!-- issuedog:transition from=ready to=held at=ready reason=owner cmd=2 -->")
	tr := adoptTrust(t, f, o)
	var logs bytes.Buffer
	stop := logtest.CaptureTo(t, &logs, logr.E)
	r, err := adoptRun(t, f, tr, "owner", false)
	logr.Wait()
	stop()
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if len(r.Refused) != 2 || !strings.Contains(r.Refused[planReview], "stage:plan-review: a plan's hash covers its whole comment") ||
		!strings.Contains(r.Refused[held], "stage:held: a hold's repost would bound input at itself, not where the hold did, so /resume it first") {
		t.Errorf("refused = %v, want #%d at plan-review and #%d held", r.Refused, planReview, held)
	}
	if r.Posted != 0 {
		t.Errorf("posted %d, want none", r.Posted)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("wrote %+v, want nothing", w)
	}
	for _, want := range []string{
		"o/r#" + strconv.Itoa(planReview) + ": adopt refuses it until it's fixed: stage:plan-review: ",
		"o/r#" + strconv.Itoa(held) + ": adopt refuses it until it's fixed: stage:held: ",
		"adopt posted nothing, as it refuses 2 issues: fix each, then run it again",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs = %q, want %q", logs.String(), want)
		}
	}
}

// Adopt posts each source's App comment, in order, logging each with its
// issue, source, kinds and new comment, then the counts, and leaves a
// closed issue, one with no stage label and a pull request alone. A second
// run posts nothing, and finds every source done.
func TestAdoptPostsOnce(t *testing.T) {
	// Not parallel: it reads the process-wide logr output.
	f, o := adoptByHand(t)
	tr := adoptTrust(t, f, o)
	before := adoptCommentCounts(f)
	var logs bytes.Buffer
	stop := logtest.CaptureTo(t, &logs, logr.I)
	r, err := adoptRun(t, f, tr, "owner", false)
	logr.Wait()
	stop()
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if want := (daemon.AdoptReport{Posted: adoptSources, Refused: map[int]string{}}); !reflect.DeepEqual(r, want) {
		t.Errorf("report = %+v, want %+v", r, want)
	}
	for n := adoptEpic; n <= adoptUncreated; n++ {
		if got := adoptComments(f, n, before[n]); !reflect.DeepEqual(got, adoptWant[n]) {
			t.Errorf("#%d: posted\n%s\nwant\n%s", n, strings.Join(got, "\n"), strings.Join(adoptWant[n], "\n"))
		}
	}
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", adoptStory)
	if err != nil {
		t.Fatal(err)
	}
	last := s.Comments[len(s.Comments)-1].ID
	for _, want := range []string{
		"o/r#1: adopt posted the revision markers of owner's comment 1001, as comment ",
		"o/r#1: adopt posted the created markers of owner's comment 1006, as comment ",
		"o/r#2: adopt posted the origin markers of owner's description, as comment ",
		"o/r#2: adopt posted the transition markers of owner's comment 1008, as comment " + strconv.FormatInt(last, 10),
		"o/r#5: adopt posted the origin markers of owner's description, as comment ",
		"adopt posted 12 comments, and found 0 sources reposted already",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs = %q, want %q", logs.String(), want)
		}
	}

	writes := len(f.Writes())
	logs.Reset()
	stop = logtest.CaptureTo(t, &logs, logr.I)
	r, err = adoptRun(t, f, tr, "owner", false)
	logr.Wait()
	stop()
	if want := (daemon.AdoptReport{Done: adoptSources, Refused: map[int]string{}}); err != nil || !reflect.DeepEqual(r, want) {
		t.Errorf("second run = %+v, %v; want %+v", r, err, want)
	}
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("the second run wrote %+v, want nothing", w)
	}
	if want := "adopt posted 0 comments, and found 12 sources reposted already"; !strings.Contains(logs.String(), want) {
		t.Errorf("second run's logs = %q, want %q", logs.String(), want)
	}
}

// With dryRun, Adopt lists each comment it would post, and the counts,
// and posts nothing.
func TestAdoptDryRun(t *testing.T) {
	// Not parallel: it reads the process-wide logr output.
	f, o := adoptByHand(t)
	tr := adoptTrust(t, f, o)
	var logs bytes.Buffer
	stop := logtest.CaptureTo(t, &logs, logr.I)
	r, err := adoptRun(t, f, tr, "owner", true)
	logr.Wait()
	stop()
	if want := (daemon.AdoptReport{Posted: adoptSources, Refused: map[int]string{}}); err != nil || !reflect.DeepEqual(r, want) {
		t.Errorf("Adopt = %+v, %v; want %+v", r, err, want)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("wrote %+v, want nothing", w)
	}
	if got := strings.Count(logs.String(), ": adopt would post the "); got != adoptSources {
		t.Errorf("logs = %q, want %d would-be posts", logs.String(), got)
	}
	for _, want := range []string{
		"o/r#1: adopt would post the transition markers of owner's comment 1005\n",
		"o/r#3: adopt would post the origin markers of owner's description\n",
		"adopt would post 12 comments, and found 0 sources reposted already",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs = %q, want %q", logs.String(), want)
		}
	}
	if strings.Contains(logs.String(), "adopt posted") {
		t.Errorf("logs = %q, want no post", logs.String())
	}
	// The issues come by number, each source in its order.
	at := -1
	for _, line := range []string{"o/r#1: adopt would post the revision markers of owner's comment 1001\n",
		"o/r#1: adopt would post the created markers of owner's comment 1006\n",
		"o/r#2: adopt would post the origin markers of owner's description\n",
		"o/r#3: adopt would post the origin markers of owner's description\n",
		"o/r#5: adopt would post the origin markers of owner's description\n",
		"o/r#9: adopt would post the transition markers of owner's comment 1014\n"} {
		i := strings.Index(logs.String(), line)
		if i <= at {
			t.Errorf("logs = %q, want %q after the line before it", logs.String(), line)
		}
		at = i
	}
}

// Adopt reads an issue with its dirty content (Dirty), and reposts no
// marker from it: a comment of owner's that mallory edited is no source.
func TestAdoptSkipsDirty(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := f.AddIssue("owner", "Dirty", "A story.")
	f.AddLabel(n, "owner", "stage:planned")
	moved := f.AddComment(n, "owner", "<!-- issuedog:transition from=intake to=planned cmd=1 -->")
	created := f.AddComment(n, "owner", "<!-- issuedog:created rev=1 children=5 -->")
	f.EditComment(created, "mallory", "<!-- issuedog:created rev=1 children=6 -->")
	r, err := adoptRun(t, f, trusted{"owner": true}, "owner", false)
	if want := (daemon.AdoptReport{Posted: 1, Refused: map[int]string{}}); err != nil || !reflect.DeepEqual(r, want) {
		t.Errorf("Adopt = %+v, %v; want %+v", r, err, want)
	}
	want := []string{adoptPost(n, moved, "<!-- issuedog:transition from=intake to=planned cmd=1 seen="+strconv.FormatInt(moved, 10)+" -->")}
	if got := adoptComments(f, n, 2); !reflect.DeepEqual(got, want) {
		t.Errorf("posted\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// An issue updated while the issue list pages, which pages by update, is
// listed twice: on its first page and again on the last. Adopt reads it
// once, and posts each of its sources once.
func TestAdoptListedTwice(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := f.AddIssue("owner", "Planned", "An epic.")
	f.AddLabel(n, "owner", "stage:planned")
	moved := f.AddComment(n, "owner", "<!-- issuedog:transition from=intake to=planned cmd=1 -->")
	// A hundred more issues, a full page, so the list has a second.
	for range 100 {
		f.AddIssue("owner", "Not in the pipeline", "Filler.")
	}
	f.BeforeRequest(func(r githubtest.Request) bool {
		return r.Path == "/issues" && strings.Contains(r.Query, "page=2")
	}, func() { f.AddComment(n, "mallory", "A comment while the list pages.") })
	r, err := adoptRun(t, f, trusted{"owner": true}, "owner", false)
	if want := (daemon.AdoptReport{Posted: 1, Refused: map[int]string{}}); err != nil || !reflect.DeepEqual(r, want) {
		t.Errorf("Adopt = %+v, %v; want %+v", r, err, want)
	}
	listed := 0
	for _, req := range f.Requests() {
		if req.Path == "/issues" && strings.Contains(req.Query, "page=2") {
			listed++
		}
	}
	if listed != 1 {
		t.Fatalf("the list's second page was read %d times, want once, so the issue is listed twice", listed)
	}
	want := []string{adoptPost(n, moved, "<!-- issuedog:transition from=intake to=planned cmd=1 seen="+strconv.FormatInt(moved, 10)+" -->")}
	if got := adoptComments(f, n, 2); !reflect.DeepEqual(got, want) {
		t.Errorf("posted\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Adopt is safe to run again after a crash at any post: a rerun posts the
// rest once, in order (SPEC §5.2).
func TestAdoptReplay(t *testing.T) {
	t.Parallel()
	setup := func(t testing.TB) *githubtest.Fake {
		f, _ := adoptByHand(t)
		return f
	}
	githubtest.Replay(t, setup, func(f *githubtest.Fake) error {
		_, err := adoptRun(t, f, trusted{"owner": true}, "owner", false)
		return err
	})
}

// adoptAsApp is s with owner's content read as the App's, as an adoption
// promises (SPEC §5, the adopted marker): the body and each comment owner
// wrote, which adoptByHand never edits. The App's comments, adopt's
// reposts, are left out, so s is read as it was before them. Its verify
// marker reads as the App's too, which nothing adoptReading holds reads.
func adoptAsApp(s *github.Snapshot) *github.Snapshot {
	bot := github.Author{Login: app, Type: "Bot"}
	c := *s
	c.Comments = nil
	if s.Author.Login == "owner" {
		c.Author = bot
	}
	for _, cm := range s.Comments {
		switch cm.Author.Login {
		case app:
			continue
		case "owner":
			cm.Author = bot
		}
		c.Comments = append(c.Comments, cm)
	}
	return &c
}

// adoptReading is what an adoption leaves as it was: Derive's stage, round
// bound, origin and item, and the created record of revision 1. The
// origin's comment is left out, as a repost's ID is never its source's.
type adoptReading struct {
	Stage   state.Stage
	Round   int64
	Origin  state.Origin
	Item    state.Item
	Created []int
}

func adoptRead(s *github.Snapshot) adoptReading {
	is := state.Derive(s, slug, func(int64) bool { return false })
	r := adoptReading{Stage: is.Stage, Round: is.Round, Origin: is.Origin, Item: is.Item}
	r.Origin.Comment = 0
	r.Created, _ = state.CreatedRecord(is, 1)
	return r
}

// adoptSnapshots reads each of adoptPipeline's issues on f.
func adoptSnapshots(t *testing.T, f *githubtest.Fake) map[int]*github.Snapshot {
	t.Helper()
	snaps := map[int]*github.Snapshot{}
	for _, n := range adoptPipeline {
		s, err := f.Client(t).Snapshot(context.Background(), "o", "r", n)
		if err != nil {
			t.Fatal(err)
		}
		snaps[n] = s
	}
	return snaps
}

// After adopt, a daemon start and its passes read each issue as it read
// with owner's markers read as the App's, with no state error, and a task
// with only labels gets its override on the first pass, at its label's
// stage. An epic planned by hand with no created record and no plan-data
// reads the same too, and no pass writes to it. The daemon reads markers
// only from the App, before and after:
// owner's originals, and a marker owner posts later, count for nothing.
// With adopt's comments in place, an unchanged pass makes no request but
// the poll's 304s, and neither adopt nor the passes post a check run.
func TestAdoptThenDaemon(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, o := adoptByHand(t)
	// The issue with no stage is ignored before the start, as SPEC §15's
	// going live has it, so triage leaves it.
	f.AddComment(adoptNoStage, "owner", "/ignore")

	byHand := adoptSnapshots(t, f)
	for _, n := range adoptPipeline {
		if is := state.Derive(byHand[n], slug, func(int64) bool { return false }); is.Stage != state.Intake || len(is.Markers) != 0 {
			t.Errorf("#%d before adopt: stage %s, markers %+v; want owner's markers read as nothing", n, is.Stage, is.Markers)
		}
	}
	if _, err := adoptRun(t, f, adoptTrust(t, f, o), "owner", false); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	adopted := adoptSnapshots(t, f)
	for _, n := range adoptPipeline {
		if got, want := adoptRead(adopted[n]), adoptRead(adoptAsApp(byHand[n])); !reflect.DeepEqual(got, want) {
			t.Errorf("#%d after adopt reads %+v, want %+v", n, got, want)
		}
	}
	// So the comparison above can't pass by reading nothing either way.
	if r := adoptRead(adopted[adoptEpic]); r.Stage != state.Planned || r.Round != 1005 || !reflect.DeepEqual(r.Created, []int{adoptStory}) {
		t.Errorf("the epic reads %+v, want planned, round 1005 and created #2", r)
	}
	if r := adoptRead(adopted[adoptHumanTask]); !r.Origin.Human || r.Origin.Item != "T3" ||
		r.Origin.Check != (state.Check{Kind: state.CheckRuleset, Value: "issuedog/verify"}) {
		t.Errorf("the human task reads %+v, want T3 with the ruleset check issuedog/verify", r)
	}
	if r := adoptRead(adopted[adoptUncreated]); r.Stage != state.Planned || r.Round != adoptUncreatedComment+1 || r.Created != nil {
		t.Errorf("the epic with no created record reads %+v, want planned, round %d and no created record", r, adoptUncreatedComment+1)
	}

	// A start, as startDaemon makes it.
	g := newRig(t, f, o, t.TempDir())
	cfg, err := config.Parse([]byte("types: labels\nloop:\n  idle_sleep: 7s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := github.EnsureLabels(ctx, f.Client(t), "o", "r", daemon.LabelOptions(cfg)); err != nil {
		t.Fatal(err)
	}
	g.wireAsMain(cfg)
	from := len(f.Writes())
	iterate(t, g.d)
	var overrides []string
	for _, w := range f.Writes()[from:] {
		if strings.Contains(w.Body, "by hand, so I've recorded it as an owner override") {
			overrides = append(overrides, w.Path+" "+w.Body[strings.Index(w.Body, "<!--"):])
		}
	}
	want := []string{
		"/issues/3/comments <!-- issuedog:transition from=intake to=ready by=owner event=",
		"/issues/4/comments <!-- issuedog:transition from=intake to=ready by=owner event=",
		"/issues/5/comments <!-- issuedog:transition from=intake to=needs-human by=owner event=",
	}
	if len(overrides) != len(want) {
		t.Errorf("the first pass's overrides = %q, want %q", overrides, want)
	}
	for i := range min(len(overrides), len(want)) {
		if !strings.HasPrefix(overrides[i], want[i]) {
			t.Errorf("the first pass's overrides = %q, want %q", overrides, want)
		}
	}
	started := adoptSnapshots(t, f)
	for n, st := range map[int]state.Stage{adoptEpic: state.Planned, adoptStory: state.Planned, adoptLabelsTask: state.Ready,
		adoptBareTask: state.Ready, adoptHumanTask: state.NeedsHuman, adoptUncreated: state.Planned} {
		if got := state.Derive(started[n], slug, func(int64) bool { return false }); got.Stage != st {
			t.Errorf("#%d after the first pass is at %s, want %s", n, got.Stage, st)
		}
	}
	for _, n := range adoptPipeline {
		got, want := adoptRead(started[n]), adoptRead(adopted[n])
		// The overrides above set the tasks' stage, round bound and
		// whether they reached ready, and nothing else.
		got.Stage, got.Round, got.Item.ReachedReady, want.Stage, want.Round, want.Item.ReachedReady = "", 0, false, "", 0, false
		if !reflect.DeepEqual(got, want) {
			t.Errorf("#%d after the first pass reads %+v, want %+v as adopt left it", n, got, want)
		}
	}

	// A marker owner posts after adopt counts for nothing.
	f.AddComment(adoptStory, "owner", "<!-- issuedog:transition from=planned to=ready cmd=1 -->")
	iterate(t, g.d)
	iterate(t, g.d)
	s, err := f.Client(t).Snapshot(ctx, "o", "r", adoptStory)
	if err != nil {
		t.Fatal(err)
	}
	if is := state.Derive(s, slug, func(int64) bool { return false }); is.Stage != state.Planned || is.Keys[state.Key{Field: "cmd", ID: "1"}] {
		t.Errorf("the story with owner's later marker: stage %s, keys %v; want planned, and the marker not read", is.Stage, is.Keys)
	}

	// Quiet: once the passes settle, an unchanged pass costs no rate limit.
	iterate(t, g.d)
	writes := len(f.Writes())
	iterate(t, g.d)
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("a settled pass wrote %+v, want nothing", w)
	}
	reqs, notModified := len(f.Requests()), f.NotModified()
	iterate(t, g.d)
	for _, r := range f.Requests()[reqs:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("an unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}
	if f.NotModified() == notModified {
		t.Error("an unchanged pass got no 304")
	}

	for _, w := range f.Writes() {
		if strings.Contains(w.Body, "issuedog:error") {
			t.Errorf("wrote a state error: %s %s %s", w.Method, w.Path, w.Body)
		}
	}
	if runs := f.State().CheckRuns; len(runs) != 0 {
		t.Errorf("check runs = %+v, want none", runs)
	}
	// The epic with no created record and no plan-data is left as adopt
	// left it: no pass files its children, closes it or writes to it.
	for _, w := range f.Writes()[from:] {
		if strings.HasPrefix(w.Path, "/issues/"+strconv.Itoa(adoptUncreated)) {
			t.Errorf("a pass wrote to the epic with no created record: %s %s %s", w.Method, w.Path, w.Body)
		}
	}
}
