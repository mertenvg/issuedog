package daemon_test

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/base64"
	"net/http"
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

// A pass's candidates filed as follow-ups (SPEC §10.2), on fakes: task #1 is
// at verifying, as the implementer's move left it, and its pass comments are
// the App's, as Verifier.Apply posts them. Each pass is a fresh step 1 on the
// task, then FollowUpsVisit, as Iterate's visit runs them, or a fresh
// daemon's Iterate.

// filingTask is newFake with task #1 at verifying on head abc, then more.
func filingTask(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		if n, _ := verifierTask(f, "abc"); n != 1 {
			t.Fatalf("the task is #%d, want #1", n)
		}
		more(t, f)
		return f
	}
}

// filingPassOf adds a pass comment of run's to task #1, with data as its
// verify-data marker's data field, and returns its ID.
func filingPassOf(f *githubtest.Fake, run, data string) int64 {
	return f.AddComment(1, app, candidatePass(run, data))
}

// filingStep is step 1 on f, trusting owner, with owner to notify, the
// scopes given, and issue types on an issue-types repo.
func filingStep(t testing.TB, f *githubtest.Fake, issueTypes bool, scopes ...string) *daemon.Step {
	tr := trusted{"owner": true}
	a := &daemon.Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: slug, IssueTypes: issueTypes, Notify: []string{"owner"}, Scopes: scopes}
	return &daemon.Step{Applier: a, Trust: tr, Commands: daemon.Commands(tr)}
}

// filingPass reads task #1 and runs step 1, then FollowUpsVisit, on it, on
// a fresh filingStep with the scope booking, as after a restart.
func filingPass(t testing.TB, issueTypes bool) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		st := filingStep(t, f, issueTypes, "booking")
		s, err := st.Client.Snapshot(context.Background(), "o", "r", 1)
		if err != nil {
			return err
		}
		if _, err := st.Run(context.Background(), s); err != nil {
			return err
		}
		return st.FollowUpsVisit(context.Background(), s)
	}
}

// filingLink is the link to comment id on task #1.
func filingLink(id int64) string {
	return "https://github.com/o/r/issues/1#issuecomment-" + strconv.FormatInt(id, 10)
}

// filingBody is the body of the follow-up of candidate item of run's pass
// on task #1, whose comment is id, its fields text's as candidateData gives
// them, escaped as the App escapes them, with notify on its cc line.
func filingBody(id int64, run, item, scenario, notify string) string {
	return "The verifier found this as it passed #1: it's " + item + " of [that pass](" + filingLink(id) + "), rarer than the blocking bar, so it didn't hold #1 back.\n\n" +
		"- Scope: booking\n- Scenario: " + scenario + "\n- Likelihood: Rare\n- Impact: A refund\n\n" +
		"It waits at `intake` for your approval. React 👍 to this issue to approve it, and it goes to `ready`, or close it to drop it.\n\n" +
		"cc @" + notify + "\n\n<!-- issuedog:origin verify=#1 run=" + run + " item=" + item + " -->"
}

// filingRecord is the filed record of run's pass, in comment id, naming
// each follow-up as "F<k> #<n>: <title>".
func filingRecord(id int64, run string, children []int, lines ...string) string {
	nums := make([]string, len(children))
	for i, n := range children {
		nums[i] = strconv.Itoa(n)
	}
	return "I've filed the follow-ups of [this pass](" + filingLink(id) + "), each waiting at `intake` for its approval:\n" +
		strings.Join(lines, "\n") + "\n\n<!-- issuedog:filed run=" + run + " children=" + strings.Join(nums, ",") + " -->"
}

// filingComments are the bodies of task #1's comments that carry kind's
// marker.
func filingComments(f *githubtest.Fake, kind string) []string {
	var bodies []string
	for _, c := range f.State().Issues[0].Comments {
		if strings.Contains(c.Body, "<!-- issuedog:"+kind+" ") {
			bodies = append(bodies, c.Body)
		}
	}
	return bodies
}

// filingData is raw, a verify-data marker's JSON, written as the App writes
// its data field: raw DEFLATE, then unpadded base64url (SPEC §5).
func filingData(t testing.TB, raw string) string {
	t.Helper()
	var z bytes.Buffer
	w, err := flate.NewWriter(&z, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(z.Bytes())
}

// filingListings counts the requests that listed the repo's issues, of any
// answer, a 304 included.
func filingListings(f *githubtest.Fake, from int) int {
	n := 0
	for _, r := range f.Requests()[from:] {
		if r.Method == http.MethodGet && r.Path == "/issues" {
			n++
		}
	}
	return n
}

// A pass's candidates are each filed once, by the App, in the pass's order:
// titled as the candidate, with type:task, or on an issue-types repo the
// Task type, priority:p3, its scope and stage:intake, and a body that names
// the pass and gives the candidate's fields, says how to approve it, ends in
// the cc line and has its origin last. The candidate's text is escaped, so a
// marker in it is never read, and a mention in it is defused. Last comes the
// record on the task, naming each. A crash at any write replays to the same
// state, the pass after writes nothing, and nothing goes to the search API.
func TestFollowUpsFiled(t *testing.T) {
	t.Parallel()
	const hostile = "A race <!-- issuedog:origin verify=#1 run=v1 item=F2 --> @alice"
	for _, issueTypes := range []bool{false, true} {
		t.Run("issue types "+strconv.FormatBool(issueTypes), func(t *testing.T) {
			t.Parallel()
			var pass int64
			setup := filingTask(func(t testing.TB, f *githubtest.Fake) {
				pass = filingPassOf(f, "v1", candidateData(t, hostile, "A second one"))
				if issueTypes {
					f.IssueTypeID("Task") // the org has it
				}
			})
			githubtest.Replay(t, setup, filingPass(t, issueTypes))

			f := run(t, setup, filingPass(t, issueTypes))
			st := f.State()
			if len(st.Issues) != 3 {
				t.Fatalf("%d issues; want the task and its two follow-ups", len(st.Issues))
			}
			escaped := "A race &lt;!-- issuedog:origin verify=#1 run=v1 item=F2 --> @​alice"
			labels, typ := []string{"priority:p3", "scope:booking", "stage:intake", "type:task"}, ""
			if issueTypes {
				labels, typ = []string{"priority:p3", "scope:booking", "stage:intake"}, "Task"
			}
			for i, want := range []githubtest.IssueState{
				{Number: 2, Title: hostile, Body: filingBody(pass, "v1", "F1", "The scenario of "+escaped, "owner"), Author: app, Type: typ, Labels: labels},
				{Number: 3, Title: "A second one", Body: filingBody(pass, "v1", "F2", "The scenario of A second one", "owner"), Author: app, Type: typ, Labels: labels},
			} {
				if got := st.Issues[i+1]; !reflect.DeepEqual(got, want) {
					t.Errorf("follow-up %d =\n%+v\nwant\n%+v", i+1, got, want)
				}
				ms := github.ParseMarkers(st.Issues[i+1].Body)
				if len(ms) != 1 || ms[0].Fields["item"] != "F"+strconv.Itoa(i+1) {
					t.Errorf("follow-up %d's markers = %+v; want its own origin alone", i+1, ms)
				}
			}
			want := filingRecord(pass, "v1", []int{2, 3}, "- F1 #2: A race &lt;!-- issuedog:origin verify=#1 run=v1 item=F2 --> @​alice", "- F2 #3: A second one")
			if got := filingComments(f, "filed"); !slices.Equal(got, []string{want}) {
				t.Errorf("the records =\n%q\nwant\n%q", got, []string{want})
			}
			noSearch(t, f)
		})
	}
}

// A crash after each write, the first follow-up, the second, then the
// record, and then a fresh daemon: the next pass files no follow-up twice,
// and posts the record once. Each is found among the issues listed since
// the pass comment, as the search API is never asked.
func TestFollowUpsCrashThenFreshDaemon(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	o.commit(config.Path, "types: labels\nnotify: [owner]\nscopes: [booking]\n")
	for n := 1; n <= 3; n++ {
		t.Run("after write "+strconv.Itoa(n), func(t *testing.T) {
			t.Parallel()
			var pass int64
			f := filingTask(func(t testing.TB, f *githubtest.Fake) {
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
			if len(st.Issues) != 3 || st.Issues[1].Title != "One" || st.Issues[2].Title != "Two" {
				t.Fatalf("the issues = %+v; want the task and its two follow-ups, each once", st.Issues)
			}
			want := filingRecord(pass, "v1", []int{2, 3}, "- F1 #2: One", "- F2 #3: Two")
			if got := filingComments(f, "filed"); !slices.Equal(got, []string{want}) {
				t.Errorf("the records =\n%q\nwant one\n%q", got, want)
			}
			noSearch(t, f)
		})
	}
}

// A follow-up filed before, as by a pass that stopped before its record, is
// found by the listing, open or closed, and reused: only the other
// candidate is filed, and the record names both, the one found by its
// number.
func TestFollowUpsFoundBefore(t *testing.T) {
	t.Parallel()
	var pass int64
	setup := filingTask(func(t testing.TB, f *githubtest.Fake) {
		pass = filingPassOf(f, "v1", candidateData(t, "One", "Two"))
		found := f.AddIssue(app, "Two, as filed", followUpBody(1, "v1", "F2"), "type:task", "priority:p3", "stage:intake")
		f.CloseAs(found, "owner", "not_planned")
	})
	githubtest.Replay(t, setup, filingPass(t, false))

	f := run(t, setup, filingPass(t, false))
	st := f.State()
	if len(st.Issues) != 3 || st.Issues[2].Title != "One" {
		t.Fatalf("the issues = %+v; want the task, the one found and F1's", st.Issues)
	}
	want := filingRecord(pass, "v1", []int{3, 2}, "- F1 #3: One", "- F2 #2: Two")
	if got := filingComments(f, "filed"); !slices.Equal(got, []string{want}) {
		t.Errorf("the records =\n%q\nwant\n%q", got, []string{want})
	}
}

// Only an origin the App wrote names a follow-up: a person's issue with
// F1's marker, a pull request of the App's with F2's, an App issue naming
// another run, and one naming another task, are none of the pass's
// follow-ups, so both candidates are filed, and the record names them.
func TestFollowUpsForgedOrigin(t *testing.T) {
	t.Parallel()
	var pass int64
	setup := filingTask(func(t testing.TB, f *githubtest.Fake) {
		pass = filingPassOf(f, "v1", candidateData(t, "One", "Two"))
		f.AddIssue("eve", "A person's forgery", followUpBody(1, "v1", "F1"))
		pull := f.AddIssue(app, "A pull request", followUpBody(1, "v1", "F2"))
		f.SetPullRequest(pull)
		f.AddIssue(app, "Another run's", followUpBody(1, "v0", "F1"), "type:task", "priority:p3", "stage:intake")
		f.AddIssue(app, "Another task's", followUpBody(9, "v1", "F1"), "type:task", "priority:p3", "stage:intake")
	})
	githubtest.Replay(t, setup, filingPass(t, false))

	f := run(t, setup, filingPass(t, false))
	st := f.State()
	if len(st.Issues) != 7 || st.Issues[5].Title != "One" || st.Issues[6].Title != "Two" {
		t.Fatalf("the issues = %+v; want the four left alone, and both candidates filed", st.Issues)
	}
	want := filingRecord(pass, "v1", []int{6, 7}, "- F1 #6: One", "- F2 #7: Two")
	if got := filingComments(f, "filed"); !slices.Equal(got, []string{want}) {
		t.Errorf("the records =\n%q\nwant\n%q", got, []string{want})
	}
}

// What files nothing, writes nothing and lists nothing: a pass with no
// candidates, or whose data is an empty list, a pass comment someone else
// edited, which is dirty, whether step 1 has noted it or not yet, the App's
// comment on a request_changes, whose candidates aren't kept, a closed
// task, and a task with a parent, whose follow-ups go under its story,
// which isn't built yet.
func TestFollowUpsNothingToFile(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		more func(t testing.TB, f *githubtest.Fake)
		// unnoted skips step 1, so the visit finds a dirty comment that no
		// dirty note has escaped yet, by its own dirty check.
		unnoted bool
	}{
		{"a pass with no candidates", func(t testing.TB, f *githubtest.Fake) {
			f.AddComment(1, app, "The verifier passed `abc`.\n\nFine.\n\n<!-- issuedog:verify verdict=pass run=v1 head=abc -->")
		}, false},
		{"a pass whose data is an empty list", func(t testing.TB, f *githubtest.Fake) {
			filingPassOf(f, "v1", filingData(t, `[]`))
		}, false},
		{"a pass comment an untrusted user edited", func(t testing.TB, f *githubtest.Fake) {
			id := filingPassOf(f, "v1", candidateData(t, "One"))
			f.EditComment(id, "eve", candidatePass("v1", candidateData(t, "One", "Eve's own")))
		}, false},
		{"a pass comment a trusted user edited, not noted yet", func(t testing.TB, f *githubtest.Fake) {
			id := filingPassOf(f, "v1", candidateData(t, "One"))
			f.EditComment(id, "owner", candidatePass("v1", candidateData(t, "One", "The owner's own")))
		}, true},
		{"a request_changes", func(t testing.TB, f *githubtest.Fake) {
			f.AddLabel(1, app, "stage:in-progress")
			f.RemoveLabel(1, app, "stage:verifying")
			f.AddComment(1, app, "The verifier asked for changes.\n\n<!-- issuedog:transition from=verifying to=in-progress run=v1 head=abc attempt=1 -->\n"+
				"<!-- issuedog:verify-data v=1 data="+candidateData(t, "One")+" -->")
		}, false},
		{"a closed task", func(t testing.TB, f *githubtest.Fake) {
			filingPassOf(f, "v1", candidateData(t, "One"))
			f.CloseAs(1, "owner", "completed")
		}, true},
		{"a task with a parent", func(t testing.TB, f *githubtest.Fake) {
			filingPassOf(f, "v1", candidateData(t, "One"))
			story := f.AddIssue("owner", "A story", "The story.")
			f.SetParent(1, story)
		}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := filingTask(tt.more)(t)
			issues := len(f.State().Issues)
			st := filingStep(t, f, false, "booking")
			s, err := st.Client.Snapshot(context.Background(), "o", "r", 1)
			if err != nil {
				t.Fatal(err)
			}
			if !tt.unnoted {
				if _, err := st.Run(context.Background(), s); err != nil {
					t.Fatal(err)
				}
			}
			from, writes := len(f.Requests()), len(f.Writes())
			if err := st.FollowUpsVisit(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			if got := f.Requests()[from:]; len(got) != 0 {
				t.Errorf("the visit sent %+v; want no request", got)
			}
			if got := f.Writes()[writes:]; len(got) != 0 || len(f.State().Issues) != issues {
				t.Errorf("the visit wrote %+v, and left %d issues; want nothing, and %d", got, len(f.State().Issues), issues)
			}
		})
	}
}

// filingErr is the followups state error on task #1 for the pass in
// comment pass, while its latest move is in comment move, saying why.
func filingErr(pass, move int64, why string) string {
	return "I can't file the follow-ups of [this pass](" + filingLink(pass) + "), as " + why + ", so I've stopped filing them. " +
		"I won't try again until this issue moves, as by `/hold` and `/resume`. The daemon's log has the error.\n\n" +
		"<!-- issuedog:error kind=followups key=" + strconv.FormatInt(pass, 10) + "-" + strconv.FormatInt(move, 10) + " -->"
}

// filingMove is the ID of the comment holding task #1's move into
// verifying, its latest move.
func filingMove(t testing.TB, f *githubtest.Fake) int64 {
	t.Helper()
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range s.Comments {
		if strings.Contains(c.Body, "<!-- issuedog:transition from=in-progress to=verifying ") {
			return c.ID
		}
	}
	t.Fatal("task #1 has no move into verifying")
	return 0
}

// What no later pass gets past files nothing more of the pass, and posts
// one followups state error on the task, keyed by the pass comment and the
// task's latest move, which quotes nothing of the data or of the error:
// data that won't read back, as with a key no candidate has, its text
// SECRET-TEXT, or a field left blank, a candidate whose title is blank on
// one line, one whose issue would be too long, and a create GitHub refuses
// with a 422, as for the Task type on a personal account's repo. A crash at
// any write replays to the same state, the pass after writes nothing, and
// no record is posted.
func TestFollowUpsStuck(t *testing.T) {
	t.Parallel()
	const secret = "SECRET-TEXT"
	whole := `"scope":"booking","scenario":"` + secret + `","likelihood":"Rare","impact":"A refund"`
	for _, tt := range []struct {
		name, why  string
		issueTypes bool
		data       func(t testing.TB, f *githubtest.Fake) string
	}{
		{"a key no candidate has", "its candidates don't read back from the pass comment", false, func(t testing.TB, _ *githubtest.Fake) string {
			return filingData(t, `[{"title":"One",`+whole+`,"`+secret+`":"x"}]`)
		}},
		{"a blank field", "its candidates don't read back from the pass comment", false, func(t testing.TB, _ *githubtest.Fake) string {
			return filingData(t, `[{"title":"  ",`+whole+`}]`)
		}},
		{"a blank title on one line", "candidate F2 has no title an issue can have", false, func(t testing.TB, _ *githubtest.Fake) string {
			return filingData(t, `[{"title":"One",`+whole+`},{"title":"\u0001",`+whole+`}]`)
		}},
		{"an issue too long to file", "the issue of candidate F1 would be too long to file", false, func(t testing.TB, _ *githubtest.Fake) string {
			return filingData(t, `[{"title":"One","scope":"booking","scenario":"`+strings.Repeat("x", 65000)+`","likelihood":"Rare","impact":"A refund"}]`)
		}},
		{"a 422", "GitHub refused to file the issue of candidate F1", true, func(t testing.TB, f *githubtest.Fake) string {
			f.SetPersonalAccount()
			return candidateData(t, secret)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var pass int64
			setup := filingTask(func(t testing.TB, f *githubtest.Fake) {
				pass = filingPassOf(f, "v1", tt.data(t, f))
			})
			githubtest.Replay(t, setup, filingPass(t, tt.issueTypes))

			f := run(t, setup, filingPass(t, tt.issueTypes))
			if got, want := filingComments(f, "error"), []string{filingErr(pass, filingMove(t, f), tt.why)}; !slices.Equal(got, want) {
				t.Errorf("the state errors =\n%q\nwant\n%q", got, want)
			}
			if got := filingComments(f, "filed"); len(got) != 0 || len(f.State().Issues) != 1 {
				t.Errorf("the records = %q, with %d issues; want none, and the task alone", got, len(f.State().Issues))
			}
		})
	}
}

// A 422 on the second create, after the first is filed: the first stays
// filed, the second isn't, and the state error is posted once, with no
// record, and nothing of GitHub's answer. The pass after writes nothing,
// and makes no request for the pass. Once the task moves, by /hold then
// /resume, the next visit tries again: it finds the first, files the
// second and posts the record.
func TestFollowUpsRefusedThenRetried(t *testing.T) {
	t.Parallel()
	var pass int64
	f := filingTask(func(t testing.TB, f *githubtest.Fake) {
		pass = filingPassOf(f, "v1", candidateData(t, "One", "Two"))
	})(t)
	creates := 0
	f.AnswerOnce(func(r githubtest.Request) bool {
		if r.Method == http.MethodPost && r.Path == "/issues" {
			creates++
		}
		return creates == 2
	}, http.StatusUnprocessableEntity)
	if err := filingPass(t, false)(f); err != nil {
		t.Fatal(err)
	}
	move := filingMove(t, f)
	if got, want := filingComments(f, "error"), []string{filingErr(pass, move, "GitHub refused to file the issue of candidate F2")}; !slices.Equal(got, want) {
		t.Errorf("the state errors =\n%q\nwant\n%q", got, want)
	}
	if st := f.State(); len(st.Issues) != 2 || st.Issues[1].Title != "One" || len(filingComments(f, "filed")) != 0 {
		t.Fatalf("the issues = %+v, records %q; want the task and F1's, and no record", st.Issues, filingComments(f, "filed"))
	}
	from, writes := len(f.Requests()), len(f.Writes())
	if err := filingPass(t, false)(f); err != nil {
		t.Fatal(err)
	}
	if got := f.Writes()[writes:]; len(got) != 0 || filingListings(f, from) != 0 {
		t.Errorf("the pass after wrote %+v and listed the issues %d times; want nothing", got, filingListings(f, from))
	}

	o := newOrigin(t)
	o.commit(config.Path, "types: labels\nnotify: [owner]\nscopes: [booking]\n")
	d := newDaemon(t, f, o, nil)
	f.AddComment(1, "owner", "/hold")
	iterate(t, d)
	f.AddComment(1, "owner", "/resume")
	iterate(t, d)
	iterate(t, d)
	st := f.State()
	if len(st.Issues) != 3 || st.Issues[2].Title != "Two" || pickStage(f, 1) != "stage:verifying" {
		t.Fatalf("the issues = %+v, the task at %s; want F2's filed too, and the task back at verifying", st.Issues, pickStage(f, 1))
	}
	want := filingRecord(pass, "v1", []int{2, 3}, "- F1 #2: One", "- F2 #3: Two")
	if got := filingComments(f, "filed"); !slices.Equal(got, []string{want}) {
		t.Errorf("the records =\n%q\nwant\n%q", got, []string{want})
	}
	if got := filingComments(f, "error"); len(got) != 1 {
		t.Errorf("the state errors = %q; want the first alone", got)
	}
}

// Quiet pass: once the record stands, a visit to the task makes no request
// for the pass, of any answer, a 304 included, and a later pass that visits
// the task, as for a comment on it, lists no issues since the pass comment,
// only the poll's.
func TestFollowUpsQuietOnceRecorded(t *testing.T) {
	t.Parallel()
	var pass int64
	f := filingTask(func(t testing.TB, f *githubtest.Fake) {
		pass = filingPassOf(f, "v1", candidateData(t, "One", "Two"))
	})(t)
	at := commentAt(t, f, 1, pass)
	if err := filingPass(t, false)(f); err != nil {
		t.Fatal(err)
	}
	if got := listingsSince(t, f, at); got != 1 {
		t.Fatalf("the filing listed the issues since the pass comment %d times, want once", got)
	}
	if len(filingComments(f, "filed")) != 1 {
		t.Fatalf("the records = %q; want one", filingComments(f, "filed"))
	}
	st := filingStep(t, f, false, "booking")
	s, err := st.Client.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	from := len(f.Requests())
	if err := st.FollowUpsVisit(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if got := f.Requests()[from:]; len(got) != 0 {
		t.Errorf("a visit once the record stands sent %+v; want no request", got)
	}

	o := newOrigin(t)
	o.commit(config.Path, "types: labels\nnotify: [owner]\nscopes: [booking]\n")
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	f.AddComment(1, "owner", "A note on the task.")
	from = len(f.Requests())
	iterate(t, d)
	if !slices.ContainsFunc(f.Requests()[from:], func(r githubtest.Request) bool { return r.Path == "/issues/1" }) {
		t.Fatalf("the pass after the comment didn't read the task: %+v", f.Requests()[from:])
	}
	if got := listingsSince(t, f, at); got != 1 {
		t.Errorf("the issues were listed since the pass comment %d times in all; want once, by the filing, and by none of the passes after", got)
	}
	if len(f.State().Issues) != 3 || len(filingComments(f, "filed")) != 1 {
		t.Errorf("%d issues, records %q; want the task and its two follow-ups, and the one record", len(f.State().Issues), filingComments(f, "filed"))
	}
}

// The pass whose step 1 files the follow-ups touches the task, as it writes
// to it: Work doesn't get it that pass, and the next pass reads it again
// and gives it to Work.
func TestFollowUpsTouchedTask(t *testing.T) {
	t.Parallel()
	f := filingTask(func(t testing.TB, f *githubtest.Fake) {
		filingPassOf(f, "v1", candidateData(t, "One"))
	})(t)
	o := newOrigin(t)
	o.commit(config.Path, "types: labels\nnotify: [owner]\nscopes: [booking]\n")
	d := newDaemon(t, f, o, nil)
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
	if len(worked) != 2 || slices.Contains(worked[0], 1) || !slices.Contains(worked[1], 1) {
		t.Errorf("Work got %v; want no task #1 on the filing's pass, and task #1 on the next", worked)
	}
}

// A candidate whose scope a reload has since dropped from config's list is
// filed without a scope label, and a warning names it.
func TestFollowUpsScopeLeftOff(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logtest.Capture).
	f := filingTask(func(t testing.TB, f *githubtest.Fake) {
		filingPassOf(f, "v1", candidateData(t, "One"))
	})(t)
	st := filingStep(t, f, false, "payments")
	s, err := st.Client.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	stop := logtest.Capture(t, logr.W)
	err = st.FollowUpsVisit(context.Background(), s)
	logs := stop()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f.State().Issues[1].Labels, []string{"priority:p3", "stage:intake", "type:task"}; !slices.Equal(got, want) {
		t.Errorf("the follow-up's labels = %q, want %q", got, want)
	}
	want := `o/r#1: filed #2 for candidate F1 of run v1's pass without a scope label, as its scope "booking" isn't on config's list any more`
	if !strings.Contains(logs, want) {
		t.Errorf("the warnings =\n%s\nwant one holding %q", logs, want)
	}
}
