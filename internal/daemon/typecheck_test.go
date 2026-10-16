package daemon_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// typeErrWant is the type error for type typ at stage, keyed key: one
// that doesn't fit, or with unknown, one the pipeline doesn't know.
func typeErrWant(typ, stage string, key int64, unknown bool) string {
	what := fmt.Sprintf("This issue's type, `%s`,", typ)
	if typ == "" {
		what = "This issue has no type, which"
	}
	why, to := fmt.Sprintf("doesn't fit its stage, `%s`", stage), "one"
	if unknown {
		why, to = "isn't one the pipeline knows", "one the pipeline knows"
	}
	return fmt.Sprintf("%s %s (SPEC §4). It stays at `%s` until its type changes to %s that can be at `%s`, or someone moves it by hand. "+
		"A 👍 on this comment acknowledges it.\n\n<!-- issuedog:error kind=type key=%d -->", what, why, stage, to, stage, key)
}

// typeErrEpic files issue 1, an epic its owner typed with type:epic (event
// 9001), at spec-review, as the App set it (event 9002) with revision 1
// of its body (comment 1001), then runs more.
func typeErrEpic(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		t.Helper()
		f := newFake(t)
		f.AddIssue("owner", "Epic", "body", "type:epic")
		flagWant(t, f.AddLabel(1, app, "stage:spec-review"), 9002)
		flagWant(t, f.AddComment(1, app, revision(1, "body")), 1001)
		if more != nil {
			more(t, f)
		}
		return f
	}
}

// typeErrRetype makes the owner change issue n's type label from one type
// to another, and returns the labeled event's ID.
func typeErrRetype(f *githubtest.Fake, n int, from, to string) int64 {
	f.RemoveLabel(n, "owner", "type:"+from)
	return f.AddLabel(n, "owner", "type:"+to)
}

// typeErrTrust trusts owner, as CODEOWNERS does in newOrigin.
var typeErrTrust = trusted{"owner": true}

// typeErrPass runs step 1 on issue 1 as newStep does, with the command
// handlers, /approve among them, and the reaction triggers for a 👍 on a
// revision and on a state error. issueTypes is the Applier's IssueTypes.
func typeErrPass(t testing.TB, issueTypes bool) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		c := f.Client(t)
		s, err := c.Snapshot(context.Background(), "o", "r", 1)
		if err != nil {
			return err
		}
		approve, ack := daemon.ApproveByReaction(typeErrTrust), daemon.ErrorByReaction(typeErrTrust)
		st := &daemon.Step{Applier: &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug, IssueTypes: issueTypes}, Trust: typeErrTrust,
			Reactions: func(ctx context.Context, a *daemon.Applier, s *github.Snapshot, d daemon.Dirt) error {
				if err := approve(ctx, a, s, d); err != nil {
					return err
				}
				return ack(ctx, a, s, d)
			}}
		st.Commands = daemon.Commands(typeErrTrust)
		st.Commands["approve"] = daemon.ApproveCommand()
		_, err = st.Run(context.Background(), s)
		return err
	}
}

// typeErrQuiet runs pass on f and fails t unless it writes nothing.
func typeErrQuiet(t *testing.T, f *githubtest.Fake, pass func(*githubtest.Fake) error) {
	t.Helper()
	n := len(f.Writes())
	errAckRun(t, f, pass)
	if w := f.Writes()[n:]; len(w) != 0 {
		t.Errorf("the pass wrote %+v; want nothing", w)
	}
}

// Step 1 posts one type error, keyed by the type change that stands, on an
// issue whose type can't be at its stage, under Replay, and the issue
// stays where it is. A second pass writes nothing. The type isn't the
// App's text, so it's escaped and defused.
func TestTypeErrCheck(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(testing.TB) *githubtest.Fake
		want   []string // new comments
		labels []string
	}{
		{"an epic at spec-review retyped as a task", typeErrEpic(func(t testing.TB, f *githubtest.Fake) {
			flagWant(t, typeErrRetype(f, 1, "epic", "task"), 9004)
		}), []string{typeErrWant("task", "spec-review", 9004, false)}, []string{"stage:spec-review", "type:task"}},
		{"an epic at spec-review whose type is removed", typeErrEpic(func(t testing.TB, f *githubtest.Fake) {
			flagWant(t, f.RemoveLabel(1, "owner", "type:epic"), 9003)
		}), []string{typeErrWant("", "spec-review", 9003, false)}, []string{"stage:spec-review"}},
		{"a type the pipeline doesn't know at clarifying", func(t testing.TB) *githubtest.Fake {
			f := newFake(t)
			fileAt(f, "clarifying", "body")
			flagWant(t, f.AddLabel(1, "owner", "type:feature"), 9002)
			return f
		}, []string{typeErrWant("feature", "clarifying", 9002, true)}, []string{"stage:clarifying", "type:feature"}},
		{"an unknown type is escaped and defused", func(t testing.TB) *githubtest.Fake {
			f := newFake(t)
			fileAt(f, "ready", "body")
			flagWant(t, f.AddLabel(1, "owner", "type:<!--x@owner"), 9002)
			return f
		}, []string{typeErrWant("&lt;!--x@​owner", "ready", 9002, true)}, []string{"stage:ready", "type:<!--x@owner"}},
		{"a transition marker in the body counts", func(t testing.TB) *githubtest.Fake {
			f := newFake(t)
			f.AddIssue(app, "Epic", "<!-- issuedog:transition from=planning to=ready run=r1 -->", "type:epic")
			f.AddLabel(1, app, "stage:ready")
			return f
		}, []string{typeErrWant("epic", "ready", 9001, false)}, []string{"stage:ready", "type:epic"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pass := typeErrPass(t, false)
			githubtest.Replay(t, tc.setup, pass)
			f := tc.setup(t)
			before := commentBodies(f, 1)
			errAckRun(t, f, pass)
			if got := commentBodies(f, 1)[len(before):]; !slices.Equal(got, tc.want) {
				t.Errorf("new comments %q; want %q", got, tc.want)
			}
			if got := f.State().Issues[0].Labels; !slices.Equal(got, tc.labels) {
				t.Errorf("labels %v; want %v", got, tc.labels)
			}
			typeErrQuiet(t, f, pass)
		})
	}
}

// No type error where no type change is behind where the issue is, or the
// type fits: the issue is untyped with no type event, typed with an issue
// type on a repo that uses labels, a story that gained a parent (plan #7,
// #153's note), closed, or has no transition marker.
func TestTypeErrNone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
	}{
		{"a type that fits", typeErrEpic(nil)},
		{"untyped at ready, never typed", func(t testing.TB) *githubtest.Fake {
			f := newFake(t)
			fileAt(f, "ready", "body")
			return f
		}},
		{"an issue type on a repo that uses labels", func(t testing.TB) *githubtest.Fake {
			f := newFake(t)
			fileAt(f, "spec-review", "body")
			f.SetTypeBy(1, "owner", "Task")
			return f
		}},
		{"an owner's story that gains a parent", func(t testing.TB) *githubtest.Fake {
			f := typeErrEpic(func(_ testing.TB, f *githubtest.Fake) { typeErrRetype(f, 1, "epic", "story") })(t)
			f.AddIssue("owner", "Parent", "")
			f.SetParent(1, 2)
			return f
		}},
		{"a closed issue", typeErrEpic(func(_ testing.TB, f *githubtest.Fake) {
			typeErrRetype(f, 1, "epic", "task")
			f.Close(1)
		})},
		{"no transition marker", func(t testing.TB) *githubtest.Fake {
			f := newFake(t)
			f.AddIssue("owner", "Idea", "body", "type:feature")
			return f
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.setup(t)
			typeErrQuiet(t, f, typeErrPass(t, false))
		})
	}
}

// An owner's story at spec-review that gained a parent has no type error,
// and a 👍 on its revision asks for a move the table lacks: that is a
// transition error, keyed by the 👍, as before.
func TestTypeErrParentIsTransition(t *testing.T) {
	setup := typeErrEpic(func(t testing.TB, f *githubtest.Fake) {
		typeErrRetype(f, 1, "epic", "story")
		f.AddIssue("owner", "Parent", "")
		f.SetParent(1, 2)
		flagWant(t, f.AddReaction(1001, "owner", "+1"), 1002)
	})
	githubtest.Replay(t, setup, typeErrPass(t, false))
	f := setup(t)
	errAckRun(t, f, typeErrPass(t, false))
	if errs := errorMarkers(f, 1); len(errs) != 1 || errs[0] != "<!-- issuedog:error kind=transition key=r1002 -->" {
		t.Errorf("errors %q; want one transition error keyed r1002", errs)
	}
}

// A 👍 on the revision of an epic retyped as a task, or /approve, posts no
// second error: the 👍 is only logged, as Transition holds the transition
// error back, and /approve gets a rejection. The issue stays at
// spec-review.
func TestTypeErrApproval(t *testing.T) {
	for _, tc := range []struct {
		name string
		more func(t testing.TB, f *githubtest.Fake)
		last string // in the last comment
	}{
		{"a 👍 on its revision", func(t testing.TB, f *githubtest.Fake) {
			flagWant(t, f.AddReaction(1001, "owner", "+1"), 1002)
		}, "kind=type key=9004"},
		{"/approve", func(t testing.TB, f *githubtest.Fake) {
			flagWant(t, f.AddComment(1, "owner", "/approve"), 1002)
		}, "reason=rejected cmd=1002"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := typeErrEpic(func(t testing.TB, f *githubtest.Fake) {
				flagWant(t, typeErrRetype(f, 1, "epic", "task"), 9004)
				tc.more(t, f)
			})
			pass := typeErrPass(t, false)
			githubtest.Replay(t, setup, pass)
			f := setup(t)
			errAckRun(t, f, pass)
			if errs := errorMarkers(f, 1); len(errs) != 1 || errs[0] != "<!-- issuedog:error kind=type key=9004 -->" {
				t.Errorf("errors %q; want the type error alone", errs)
			}
			got := commentBodies(f, 1)
			if !strings.Contains(got[len(got)-1], tc.last) {
				t.Errorf("comments %q; want the last to hold %q", got, tc.last)
			}
			if labels := f.State().Issues[0].Labels; !slices.Contains(labels, "stage:spec-review") {
				t.Errorf("labels %v; want it still at spec-review", labels)
			}
			typeErrQuiet(t, f, pass)
		})
	}
}

// Setting type:epic again ends the type error, so a 👍 on it gets no ack
// and no read, and setting type:bug after that posts a new one, keyed by
// its own event.
func TestTypeErrRetypedAgain(t *testing.T) {
	f := typeErrEpic(func(t testing.TB, f *githubtest.Fake) { flagWant(t, typeErrRetype(f, 1, "epic", "task"), 9004) })(t)
	pass := typeErrPass(t, false)
	errAckRun(t, f, pass)
	want := []string{revision(1, "body"), typeErrWant("task", "spec-review", 9004, false)}
	if got := commentBodies(f, 1); !slices.Equal(got, want) {
		t.Fatalf("comments %q; want %q", got, want)
	}
	typeErrRetype(f, 1, "task", "epic")
	f.AddReaction(1002, "owner", "+1")
	from := len(f.Requests())
	typeErrQuiet(t, f, pass)
	if n := errAckReads(f.Requests()[from:], 1002); n != 0 {
		t.Errorf("%d reads of the ended type error's reactions; want none", n)
	}
	flagWant(t, typeErrRetype(f, 1, "epic", "bug"), 9008)
	errAckRun(t, f, pass)
	want = append(want, typeErrWant("bug", "spec-review", 9008, false))
	if got := commentBodies(f, 1); !slices.Equal(got, want) {
		t.Errorf("comments %q; want %q", got, want)
	}
	typeErrQuiet(t, f, pass)
}

// A trusted 👍 on an open type error acknowledges it, and the error is
// never posted again, nor is a transition error for the same cause.
func TestTypeErrAck(t *testing.T) {
	f := typeErrEpic(func(t testing.TB, f *githubtest.Fake) { flagWant(t, typeErrRetype(f, 1, "epic", "task"), 9004) })(t)
	pass := typeErrPass(t, false)
	errAckRun(t, f, pass)
	r := f.AddReaction(1002, "owner", "+1")
	errAckRun(t, f, pass)
	f.AddReaction(1001, "owner", "+1")
	typeErrQuiet(t, f, pass)
	want := []string{revision(1, "body"), typeErrWant("task", "spec-review", 9004, false), errAckReply("owner", "type", "9004", r)}
	if got := commentBodies(f, 1); !slices.Equal(got, want) {
		t.Errorf("comments %q; want %q", got, want)
	}
}

// Transition posts the type error itself when the type is why the table
// lacks the move and step 1 hasn't posted it, as after a type change
// during a run, and only logs the move's error. Once the type error is
// acknowledged, it posts nothing.
func TestTypeErrTransition(t *testing.T) {
	m := daemon.Move{To: state.Planning, Trigger: state.TriggerSpecApproved, Key: state.Key{Field: "run", ID: "x"}}
	pass := func(f *githubtest.Fake) error {
		a, s, err := applier(t, f)
		if err != nil {
			return err
		}
		none := func(int64) bool { return false }
		if err := a.CheckLabels(context.Background(), s, typeErrTrust, none); err != nil {
			return err
		}
		if err := a.Transition(context.Background(), s, none, m); !daemon.StateError(err) {
			return fmt.Errorf("Transition = %v; want a state error", err)
		}
		return nil
	}
	setup := typeErrEpic(func(t testing.TB, f *githubtest.Fake) { flagWant(t, typeErrRetype(f, 1, "epic", "task"), 9004) })
	githubtest.Replay(t, setup, pass)
	f := setup(t)
	errAckRun(t, f, pass)
	want := []string{revision(1, "body"), typeErrWant("task", "spec-review", 9004, false)}
	if got := commentBodies(f, 1); !slices.Equal(got, want) {
		t.Errorf("comments %q; want %q", got, want)
	}
	acked := typeErrEpic(func(t testing.TB, f *githubtest.Fake) {
		flagWant(t, typeErrRetype(f, 1, "epic", "task"), 9004)
		f.AddComment(1, app, errAckReply("owner", "type", "9004", 1))
	})(t)
	typeErrQuiet(t, acked, pass)
}

// The move held back for the type error is logged at info level when the
// type error is posted, and at debug level on every pass after, as a
// reported error is (#156's verify).
func TestTypeErrHoldBackLog(t *testing.T) {
	var logs bytes.Buffer
	stop := logr.AddWriter(&logs, logr.WithFilter(logr.I))
	defer stop()
	m := daemon.Move{To: state.Planning, Trigger: state.TriggerSpecApproved, Key: state.Key{Field: "run", ID: "x"}}
	f := typeErrEpic(func(t testing.TB, f *githubtest.Fake) { flagWant(t, typeErrRetype(f, 1, "epic", "task"), 9004) })(t)
	for range 3 {
		a, s, err := applier(t, f)
		if err != nil {
			t.Fatal(err)
		}
		none := func(int64) bool { return false }
		if err := a.CheckLabels(context.Background(), s, typeErrTrust, none); err != nil {
			t.Fatal(err)
		}
		if err := a.Transition(context.Background(), s, none, m); !daemon.StateError(err) {
			t.Fatalf("Transition = %v; want a state error", err)
		}
	}
	logr.Wait()
	if got := strings.Count(logs.String(), "state error held back, as the type error keyed 9004 has the same cause"); got != 1 {
		t.Errorf("the hold-back is logged at info level %d times; want once, with the post:\n%s", got, logs.String())
	}
}

// On a repo that uses issue types, the type error keys on the newest
// issue-type event. Issue types aren't checked for trust, and nothing
// reverts them (github.LabelCheck.IssueType; SPEC §16 for T10, #162): so
// an untrusted user's type change is followed like anyone's, ending the
// owner's type error and starting a new one. A repo that uses labels
// ignores the issue-type events.
func TestTypeErrIssueTypes(t *testing.T) {
	setup := func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		f.AddIssue("owner", "Epic", "body")
		flagWant(t, f.SetTypeBy(1, "owner", "Epic"), 9001)
		f.AddLabel(1, app, "stage:spec-review")
		flagWant(t, f.AddComment(1, app, revision(1, "body")), 1001)
		flagWant(t, f.SetTypeBy(1, "owner", "Task"), 9003)
		return f
	}
	pass := typeErrPass(t, true)
	githubtest.Replay(t, setup, pass)
	f := setup(t)
	errAckRun(t, f, pass)
	want := []string{revision(1, "body"), typeErrWant("task", "spec-review", 9003, false)}
	if got := commentBodies(f, 1); !slices.Equal(got, want) {
		t.Fatalf("comments %q; want %q", got, want)
	}
	typeErrQuiet(t, f, pass)
	// mallory, untrusted, sets the type back, then to bug.
	f.SetTypeBy(1, "mallory", "Epic")
	f.AddReaction(1002, "owner", "+1")
	typeErrQuiet(t, f, pass)
	flagWant(t, f.SetTypeBy(1, "mallory", "Bug"), 9005)
	errAckRun(t, f, pass)
	want = append(want, typeErrWant("bug", "spec-review", 9005, false))
	if got := commentBodies(f, 1); !slices.Equal(got, want) {
		t.Errorf("comments %q; want mallory's type change followed: %q", got, want)
	}
	typeErrQuiet(t, setup(t), typeErrPass(t, false))
}

// The loop takes the type change from the issue types or the labels, as
// the config's types says, through each reload.
func TestTypeErrConfigTypes(t *testing.T) {
	for _, types := range []string{"labels", "issue-types"} {
		t.Run(types, func(t *testing.T) {
			f := newFake(t)
			fileAt(f, "spec-review", "body")
			f.SetTypeBy(1, "owner", "Epic")
			event := f.SetTypeBy(1, "owner", "Task")
			o := newOrigin(t)
			o.commit(config.Path, "types: "+types+"\n")
			d := newDaemon(t, f, o, nil)
			iterate(t, d)
			var want []string
			if types == "issue-types" {
				want = []string{fmt.Sprintf("<!-- issuedog:error kind=type key=%d -->", event)}
			}
			if errs := errorMarkers(f, 1); !slices.Equal(errs, want) {
				t.Errorf("errors %q; want %q", errs, want)
			}
		})
	}
}

// Triage on an issue of a type the pipeline doesn't know, set by a type
// change, runs once: its move's state error is the type error, with no
// transition error beside it, and no run starts again while it stands,
// not even on new input. A change to another unknown type gives triage
// no run either (#179's Q1a): step 1 posts its type error, once, and an
// ack of that lets no run start. A change to a type the
// pipeline knows lets triage run again.
func TestTypeErrTriageUnknownType(t *testing.T) {
	f := newFake(t)
	f.AddIssue("owner", "Idea", "An idea.", "type:feature")
	g := triageRig(t, f, triageOrigin(t), t.TempDir())
	g.claude(triageOut("handle", true, triageReason, triageSummary), 0)
	for range 3 {
		g.pass()
	}
	f.AddComment(1, "owner", "Please look again.")
	g.pass()
	if errs := errorMarkers(f, 1); g.runs() != 1 || !slices.Equal(errs, []string{"<!-- issuedog:error kind=type key=9001 -->"}) {
		t.Errorf("%d runs, errors %q; want one run and the type error alone", g.runs(), errs)
	}
	next := typeErrRetype(f, 1, "feature", "idea")
	for range 3 {
		g.pass()
	}
	want := []string{"<!-- issuedog:error kind=type key=9001 -->", fmt.Sprintf("<!-- issuedog:error kind=type key=%d -->", next)}
	if errs := errorMarkers(f, 1); g.runs() != 1 || !slices.Equal(errs, want) {
		t.Fatalf("%d runs, errors %q after a change to another unknown type; want still one run, and %q", g.runs(), errs, want)
	}
	if got := commentBodies(f, 1); len(got) == 0 || got[len(got)-1] != typeErrWant("idea", "intake", next, true) {
		t.Errorf("comments %q; want the last %q", got, typeErrWant("idea", "intake", next, true))
	}
	// The rig records no ack, so the App's ack reply stands for one.
	r := f.AddReaction(digestRunCommentID(t, f, 1, want[1]), "owner", "+1")
	f.AddComment(1, app, errAckReply("owner", "type", strconv.FormatInt(next, 10), r))
	for range 2 {
		g.pass()
	}
	want = append(want, fmt.Sprintf("<!-- issuedog:error kind=type key=%d ack=r%d -->", next, r))
	if errs := errorMarkers(f, 1); g.runs() != 1 || !slices.Equal(errs, want) {
		t.Fatalf("%d runs, errors %q after the 👍; want still one run, and %q", g.runs(), errs, want)
	}
	typeErrRetype(f, 1, "idea", "task")
	g.pass()
	if g.runs() != 2 {
		t.Errorf("%d runs after a change to a known type; want triage to run again", g.runs())
	}
}

// A run whose Apply asks for a move the table lacks, without Transition,
// after its issue's type changed to one the pipeline doesn't know during
// the run, is blocked by the type error, posted in that pass, not by a
// transition error, and no run starts while it stands.
func TestTypeErrBlockedRun(t *testing.T) {
	f := newFake(t)
	f.AddIssue("owner", "Idea", "the idea", "type:epic")
	f.AddLabel(1, app, "stage:clarifying")
	f.AddComment(1, app, "Questions.\n\n<!-- issuedog:transition from=intake to=clarifying run=r0 -->")
	f.AddComment(1, "owner", "An answer.")
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.stage.pending = always
	g.stage.applyErr = fmt.Errorf("no row: %w", state.ErrNoTransition)
	var event int64
	g.stage.during = func() {
		if event == 0 {
			event = typeErrRetype(f, 1, "epic", "feature")
		}
	}
	g.claude(okOut, 0)
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	want := []string{fmt.Sprintf("<!-- issuedog:error kind=type key=%d -->", event)}
	if errs := errorMarkers(f, 1); g.runs() != 1 || !slices.Equal(errs, want) {
		t.Fatalf("%d runs, errors %q; want one run and %q in its pass", g.runs(), errs, want)
	}
	for range 2 {
		if err := g.work(1); err != nil {
			t.Fatal(err)
		}
	}
	if errs := errorMarkers(f, 1); g.runs() != 1 || !slices.Equal(errs, want) {
		t.Errorf("%d runs, errors %q; want still one run and %q", g.runs(), errs, want)
	}
}

// typeErrLoginTrust trusts as its trusted does, but fails for login with
// errTrust.
type typeErrLoginTrust struct {
	trusted
	login string
}

func (e typeErrLoginTrust) Trusted(ctx context.Context, login string) (bool, error) {
	if login == e.login {
		return false, fmt.Errorf("503: %w", errTrust)
	}
	return e.trusted.Trusted(ctx, login)
}

// An error keyed on step 1's inputs keeps the loop visiting its issue on
// every pass: an attempts error and a run's transition error, each keyed
// on a trusted user's comment, so a 👍 on it in a later pass is acked.
func TestTypeErrLoopKeyedOnInputs(t *testing.T) {
	for _, tc := range []struct{ kind, key string }{
		{"attempts", "clarifying-1002"},
		{"transition", "clarifying-1002-" + state.BodyHash(fmt.Sprintf("%+v", state.Item{}))},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			f := newFake(t)
			fileAt(f, "clarifying", "body")
			flagWant(t, f.AddComment(1, "owner", "more"), 1002)
			flagWant(t, f.AddComment(1, app, fmt.Sprintf("Failed.\n\n<!-- issuedog:error kind=%s key=%s -->", tc.kind, tc.key)), 1003)
			var visits []int
			d := newDaemon(t, f, newOrigin(t), &visits)
			record, ack := d.Step.Reactions, daemon.ErrorByReaction(d.Step.CurrentTrust())
			d.Step.Reactions = func(ctx context.Context, a *daemon.Applier, s *github.Snapshot, dt daemon.Dirt) error {
				if err := record(ctx, a, s, dt); err != nil {
					return err
				}
				return ack(ctx, a, s, dt)
			}
			iterate(t, d)
			iterate(t, d) // the App's 👍 on the answer changes the issue
			visits = nil
			iterate(t, d)
			if !slices.Equal(visits, []int{1}) {
				t.Fatalf("an unchanged pass ran step 1 on %v; want [1], as the error is open", visits)
			}
			r := f.AddReaction(1003, "owner", "+1")
			iterate(t, d)
			got := commentBodies(f, 1)
			if want := errAckReply("owner", tc.kind, tc.key, r); got[len(got)-1] != want {
				t.Errorf("comments %q; want the ack last: %q", got, want)
			}
		})
	}
}

// openErrInputs' failure to check a commenter's trust comes back wrapped,
// so errors.Is finds it.
func TestTypeErrInputsTrustError(t *testing.T) {
	f := newFake(t)
	fileAt(f, "clarifying", "body")
	flagWant(t, f.AddComment(1, app, "Failed.\n\n<!-- issuedog:error kind=attempts key=clarifying-1001 -->"), 1002)
	f.AddComment(1, "mallory", "more")
	a, s, err := applier(t, f)
	if err != nil {
		t.Fatal(err)
	}
	err = daemon.ErrorByReaction(typeErrLoginTrust{errAckTrust, "mallory"})(context.Background(), a, s, daemon.Dirt{})
	if !errors.Is(err, errTrust) || !strings.HasPrefix(err.Error(), "acknowledgments on o/r#1: open state errors of #1: inputs of #1: trust mallory: ") {
		t.Errorf("err = %v; want errTrust, wrapped", err)
	}
}

// runStage holds a run back while any type error reported on the issue is
// still the cause, not only the one keyed by the type change that stands
// (#179's Q1a): on an issue at intake whose type error for type:feature
// was posted, changed to type:idea, triage doesn't run even where step 1
// hasn't posted the new key's error, as when Work is given the issue
// straight after the label rules. An ack reply alone, as after the error's
// comment was deleted, counts as reported too, but a state error of
// another kind, posted or acknowledged, doesn't, nor does an ack with no
// kind, which is older than type errors: triage runs, and its move reports
// the type error.
func TestTypeHoldAnyKey(t *testing.T) {
	labels := "Labels.\n\n<!-- issuedog:error kind=labels key=1 -->"
	kindless := "Acknowledged, by `owner`'s 👍.\n\n<!-- issuedog:error key=9001 ack=r5 -->" // from before acks named a kind
	for name, tc := range map[string]struct {
		reported []string
		runs     int
		errs     []string
	}{
		"posted":       {[]string{typeErrWant("feature", "intake", 9001, true)}, 0, []string{"<!-- issuedog:error kind=type key=9001 -->"}},
		"acknowledged": {[]string{errAckReply("owner", "type", "9001", 5)}, 0, []string{"<!-- issuedog:error kind=type key=9001 ack=r5 -->"}},
		"another kind": {[]string{labels, errAckReply("owner", "labels", "1", 5)}, 1, []string{"<!-- issuedog:error kind=labels key=1 -->",
			"<!-- issuedog:error kind=labels key=1 ack=r5 -->", "<!-- issuedog:error kind=type key=9003 -->"}},
		"a kindless ack": {[]string{kindless}, 1, []string{"<!-- issuedog:error key=9001 ack=r5 -->", "<!-- issuedog:error kind=type key=9003 -->"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.AddIssue("owner", "Idea", "An idea.", "type:feature")
			for _, c := range tc.reported {
				f.AddComment(1, app, c)
			}
			typeErrRetype(f, 1, "feature", "idea")
			g := triageRig(t, f, triageOrigin(t), t.TempDir())
			g.claude(triageOut("handle", true, triageReason, triageSummary), 0)
			s := g.snapshot(1)
			if err := g.d.Step.CheckLabels(context.Background(), s, g.d.Step.Trust, func(int64) bool { return false }); err != nil {
				t.Fatal(err)
			}
			if err := g.r.Work(context.Background(), []daemon.Ready{{S: s}}); err != nil {
				t.Fatal(err)
			}
			if errs := errorMarkers(f, 1); g.runs() != tc.runs || !slices.Equal(errs, tc.errs) {
				t.Errorf("%d runs, errors %q; want %d, and %q", g.runs(), errs, tc.runs, tc.errs)
			}
		})
	}
}

// A 👍 on the revision of an epic retyped as a task, given while the type
// error holds the approval back, still counts once the type is fixed
// (#179's Q2a): setting type:epic again moves the issue to planning on
// that 👍, as the revision it approves still matches the body (SPEC §7.4).
func TestTypeHoldApprovalCountsLater(t *testing.T) {
	f := typeErrEpic(func(t testing.TB, f *githubtest.Fake) {
		flagWant(t, typeErrRetype(f, 1, "epic", "task"), 9004)
		flagWant(t, f.AddReaction(1001, "owner", "+1"), 1002)
	})(t)
	pass := typeErrPass(t, false)
	errAckRun(t, f, pass)
	if labels := f.State().Issues[0].Labels; !slices.Contains(labels, "stage:spec-review") {
		t.Fatalf("labels %v while the type error stands; want it still at spec-review", labels)
	}
	typeErrRetype(f, 1, "task", "epic")
	errAckRun(t, f, pass)
	got := commentBodies(f, 1)
	if len(got) == 0 || !strings.HasPrefix(got[len(got)-1], "Revision 1 of the spec is approved, by owner's 👍.") ||
		!strings.Contains(got[len(got)-1], "cmd=r1002") {
		t.Errorf("comments %q; want the last to approve revision 1 on the 👍 r1002", got)
	}
	if labels := f.State().Issues[0].Labels; !slices.Contains(labels, "stage:planning") {
		t.Errorf("labels %v once the type is fixed; want it at planning", labels)
	}
}
