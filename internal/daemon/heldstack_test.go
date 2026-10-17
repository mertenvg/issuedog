package daemon_test

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// heldStackPass runs step 1 on issue n with /hold, /resume, /revise and
// /approve, "owner" trusted, from a fresh client, as after a restart. It
// keeps the bodies of Run's Inputs in *in, if in isn't nil.
func heldStackPass(t testing.TB, n int, in *[]string) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		c := f.Client(t)
		s, err := c.Snapshot(context.Background(), "o", "r", n)
		if err != nil {
			return err
		}
		tr := trusted{"owner": true}
		cmds := daemon.Commands(tr)
		cmds["revise"], cmds["approve"] = daemon.ReviseCommand(), daemon.ApproveCommand()
		st := &daemon.Step{Applier: &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}, Trust: tr, Commands: cmds}
		got, err := st.Run(context.Background(), s)
		if err != nil {
			return err
		}
		if in != nil {
			*in = nil
			for _, c := range got {
				*in = append(*in, c.Body)
			}
		}
		return nil
	}
}

// heldStackStep is a person's change to the fake, do, then a pass on issue
// on.
type heldStackStep struct {
	on int
	do func(t testing.TB, f *githubtest.Fake)
}

// heldStackSetup runs base, then each of steps with its pass, but for the
// last one's pass, then forgets the writes so far.
func heldStackSetup(base func(testing.TB) *githubtest.Fake, steps []heldStackStep) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := base(t)
		for i, s := range steps {
			s.do(t, f)
			if i < len(steps)-1 {
				if err := heldStackPass(t, s.on, nil)(f); err != nil {
					t.Fatalf("step %d: %v", i+1, err)
				}
			}
		}
		f.ForgetWrites()
		return f
	}
}

// heldStackSay has the owner post body on issue n, keeping its ID in *id.
func heldStackSay(n int, body string, id *int64) func(testing.TB, *githubtest.Fake) {
	return func(_ testing.TB, f *githubtest.Fake) { *id = f.AddComment(n, "owner", body) }
}

// heldStackReadd has the owner remove stage:held from issue n and add it
// back, a hold by hand on a held issue, keeping the labeled event's ID in
// *ev.
func heldStackReadd(n int, ev *int64) func(testing.TB, *githubtest.Fake) {
	return func(_ testing.TB, f *githubtest.Fake) {
		f.RemoveLabel(n, "owner", "stage:held")
		*ev = f.AddLabel(n, "owner", "stage:held")
	}
}

// heldStackHand is the App's comment on a hold by hand on a held issue.
func heldStackHand(fields string) string {
	return moved("owner set `stage:held` by hand on this held issue, so I've recorded it as an owner hold of its own. "+
		"It stays held until `/resume` here.", fields)
}

// heldStackTasks files #1, a task at ready (comment 1001), and its child #2
// at in-progress (comment 1002).
func heldStackTasks(t testing.TB) *githubtest.Fake {
	f := githubtest.New(t, "o/r", slug)
	f.SetPermission("owner", "admin") // a real collaborator, as discWho asks (#188's verify, round 2)
	file(f, 0, ready)
	file(f, 1, "from=ready to=in-progress run=a")
	return f
}

// heldStackFamily is heldStackTasks with a second child, #3, at ready
// (comment 1003).
func heldStackFamily(t testing.TB) *githubtest.Fake {
	f := heldStackTasks(t)
	file(f, 1, ready)
	return f
}

// heldStackCheck runs the last of steps under Replay, then twice, the second
// time writing nothing, and checks each issue's labels and new comments
// against want, and the App's reaction to the last command, if react.
func heldStackCheck(t *testing.T, base func(testing.TB) *githubtest.Fake, steps []heldStackStep, wants func() map[int]issueWant, react string, cmd *int64) {
	t.Helper()
	setup := heldStackSetup(base, steps)
	last := steps[len(steps)-1].on
	githubtest.Replay(t, setup, heldStackPass(t, last, nil))
	before := setup(t).State()
	got := run(t, setup, heldStackPass(t, last, nil)).State()
	want := wants() // after setup, which sets the IDs it names
	for i, is := range got.Issues {
		w := want[is.Number]
		var added []string
		for _, c := range is.Comments[len(before.Issues[i].Comments):] {
			added = append(added, c.Body)
		}
		if !reflect.DeepEqual(is.Labels, w.labels) || !reflect.DeepEqual(added, w.comments) {
			t.Errorf("#%d: labels %q and new comments %q, want %q and %q", is.Number, is.Labels, added, w.labels, w.comments)
		}
	}
	if react == "" {
		return
	}
	// The command is the owner's last comment on its issue.
	comments := got.Issues[last-1].Comments
	i := 0
	for j, c := range comments {
		if c.Author == "owner" {
			i = j
		}
	}
	if want := []string{app + " " + react}; !reflect.DeepEqual(comments[i].Reactions, want) {
		t.Errorf("command %d's reactions = %q, want %q", *cmd, comments[i].Reactions, want)
	}
}

// #87's note 1 and Q2b: a hold by hand on an issue under an owner /hold
// writes one marker. Its parent's /resume ends the /hold, but not the hold
// by hand, and names the issue, which records that the /hold ended; the
// issue leaves held only on its own /resume. Each move carries the round bound from before the hold.
func TestHeldStackHandSetHold(t *testing.T) {
	var hold, resume1, resume2, ev int64
	steps := []heldStackStep{
		{1, heldStackSay(1, "/hold", &hold)},
		{2, heldStackReadd(2, &ev)},
		{1, heldStackSay(1, "/resume", &resume1)},
		{2, heldStackSay(2, "/resume", &resume2)},
	}
	t.Run("/hold on the parent", func(t *testing.T) {
		heldStackCheck(t, heldStackFamily, steps[:1], func() map[int]issueWant {
			return map[int]issueWant{
				1: {[]string{"stage:held"}, []string{moved("On hold by `/hold`, from `ready`.", fmt.Sprintf("from=ready to=held at=ready reason=owner cmd=%d seen=1001", hold))}},
				2: {[]string{"stage:held"}, []string{moved("On hold by `/hold` on #1, from `in-progress`.",
					fmt.Sprintf("from=in-progress to=held at=in-progress reason=owner cmd=%d seen=1002", hold))}},
				3: {[]string{"stage:held"}, []string{moved("On hold by `/hold` on #1, from `ready`.", fmt.Sprintf("from=ready to=held at=ready reason=owner cmd=%d seen=1003", hold))}},
			}
		}, "+1", &hold)
	})
	t.Run("a hold by hand on the held child writes one marker", func(t *testing.T) {
		heldStackCheck(t, heldStackFamily, steps[:2], func() map[int]issueWant {
			return map[int]issueWant{
				1: {[]string{"stage:held"}, nil},
				2: {[]string{"stage:held"}, []string{heldStackHand(fmt.Sprintf("from=held to=held at=in-progress reason=owner by=owner event=%d seen=1002", ev))}},
				3: {[]string{"stage:held"}, nil},
			}
		}, "", nil)
	})
	t.Run("the parent's /resume leaves the child held, and names it", func(t *testing.T) {
		heldStackCheck(t, heldStackFamily, steps[:3], func() map[int]issueWant {
			return map[int]issueWant{
				1: {[]string{"stage:ready"}, []string{moved("Resumed by `/resume`, back to `ready`. Still on hold: #2, by a hold this `/resume` doesn't end: one set by hand, a `/hold` below this issue, or a deleted comment that was, or may have been, a `/hold`. `/resume` there ends it.",
					fmt.Sprintf("from=held to=ready cmd=%d seen=1001", resume1))}},
				// The record that the /hold ended, which keeps it ended
				// (#188's verify, round 3, note 1).
				2: {[]string{"stage:held"}, []string{fmt.Sprintf("`/resume` on #1 ended an owner hold on this issue, but it stays on hold by one that `/resume` doesn't end: "+
					"one set by hand, a `/hold` below #1, or a deleted comment that was, or may have been, a `/hold`. `/resume` here ends it.\n\n"+
					"<!-- issuedog:ended reason=owner key=c%d cmd=%d -->", hold, resume1)}},
				3: {[]string{"stage:ready"}, []string{moved("Resumed by `/resume` on #1, back to `ready`.", fmt.Sprintf("from=held to=ready cmd=%d seen=1003", resume1))}},
			}
		}, "+1", &resume1)
	})
	t.Run("the child's own /resume ends its hold by hand", func(t *testing.T) {
		heldStackCheck(t, heldStackFamily, steps, func() map[int]issueWant {
			return map[int]issueWant{
				1: {[]string{"stage:ready"}, nil},
				2: {[]string{"stage:in-progress"}, []string{moved("Resumed by `/resume`, back to `in-progress`.", fmt.Sprintf("from=held to=in-progress cmd=%d seen=1002", resume2))}},
				3: {[]string{"stage:ready"}, nil},
			}
		}, "+1", &resume2)
	})
}

// A /resume that would end a /hold on a child, but leave it held by hand,
// takes nothing out of held, so it's rejected, naming the child, where
// /resume ends the rest. A hand-set stage out of held ends every hold on
// the parent, and keeps its round too.
func TestHeldStackResumeNamesTheRest(t *testing.T) {
	var hold, resume, ev int64
	steps := []heldStackStep{
		{1, heldStackSay(1, "/hold", &hold)},
		{1, func(_ testing.TB, f *githubtest.Fake) {
			f.RemoveLabel(1, "owner", "stage:held")
			ev = f.AddLabel(1, "owner", "stage:ready")
		}},
		{2, heldStackReadd(2, new(int64))},
		{1, heldStackSay(1, "/resume", &resume)},
	}
	t.Run("the parent set back to ready by hand", func(t *testing.T) {
		heldStackCheck(t, heldStackTasks, steps[:2], func() map[int]issueWant {
			return map[int]issueWant{
				1: {[]string{"stage:ready"}, []string{overridden("ready", fmt.Sprintf("from=held to=ready by=owner event=%d seen=1001", ev))}},
				2: {[]string{"stage:held"}, nil},
			}
		}, "", nil)
	})
	t.Run("the parent's /resume is rejected", func(t *testing.T) {
		heldStackCheck(t, heldStackTasks, steps, func() map[int]issueWant {
			return map[int]issueWant{
				1: {[]string{"stage:ready"}, []string{final("/resume", "I didn't apply this command", madeOn("#2"), fmt.Sprint(resume))}},
				2: {[]string{"stage:held"}, nil},
			}
		}, "-1", &resume)
	})
}

// #185's verify: a hold that starts and ends in one pass keeps the round
// too. /hold, a comment and /resume, all posted before one poll, leave the
// comment an answer at once, as a replay after a crash part way does, and
// the next pass writes nothing.
func TestHeldStackHoldAndResumeInOnePass(t *testing.T) {
	var request, hold, during, resume int64
	steps := []heldStackStep{
		{1, heldStackSay(1, "too long", &request)},
		{1, func(t testing.TB, f *githubtest.Fake) {
			heldStackSay(1, "/hold", &hold)(t, f)
			heldStackSay(1, "use X", &during)(t, f)
			heldStackSay(1, "/resume", &resume)(t, f)
		}},
	}
	setup := heldStackSetup(review(func(testing.TB, *githubtest.Fake) {}), steps)
	var in []string
	githubtest.Replay(t, setup, heldStackPass(t, 1, &in))
	before := len(setup(t).State().Issues[0].Comments)
	got := run(t, setup, heldStackPass(t, 1, &in)).State().Issues[0]
	var added, acked []string
	for i, c := range got.Comments {
		if i >= before {
			added = append(added, c.Body)
		}
		if slices.Contains(c.Reactions, app+" +1") {
			acked = append(acked, c.Body)
		}
	}
	want := []string{
		moved("On hold by `/hold`, from `clarifying`.", fmt.Sprintf("from=clarifying to=held at=clarifying reason=owner cmd=%d seen=%d", hold, request)),
		moved("Resumed by `/resume`, back to `clarifying`.", fmt.Sprintf("from=held to=clarifying cmd=%d seen=%d", resume, request)),
	}
	if !reflect.DeepEqual(added, want) || !reflect.DeepEqual(got.Labels, []string{"stage:clarifying"}) {
		t.Errorf("new comments %q and labels %q, want %q and clarifying", added, got.Labels, want)
	}
	if w := []string{"too long", "/hold", "use X", "/resume"}; !reflect.DeepEqual(acked, w) {
		t.Errorf("👍'd %q, want %q", acked, w)
	}
	if w := []string{"too long", "use X"}; !reflect.DeepEqual(in, w) {
		t.Errorf("inputs %q, want %q", in, w)
	}
}

// #185's verify, attempt 2: holds that start and end more than once in one
// pass. Each comment is handled in the stage the issue had right after the
// last move out of held before it, as a replay after a crash anywhere in
// the pass does, and the next pass writes nothing.
func TestHeldStackHoldsInOnePass(t *testing.T) {
	var request int64
	ids := map[string]*int64{}
	say := func(bodies ...string) heldStackStep {
		return heldStackStep{1, func(t testing.TB, f *githubtest.Fake) {
			for _, b := range bodies {
				id := new(int64)
				heldStackSay(1, b, id)(t, f)
				ids[b] = id
			}
		}}
	}
	sentBackAt := []heldStackStep{{1, heldStackSay(1, "too long", &request)}}
	for _, tt := range []struct {
		name   string
		steps  []heldStackStep
		labels []string
		acked  []string // the comments the App 👍'd, in order
		inputs []string
		// markers are the fields of the transition markers this pass posts,
		// in order.
		markers func() []string
	}{
		{"at clarifying: a comment between two holds is an answer, and one in the second waits",
			append(sentBackAt, say("/hold", "c1", "/resume", "/hold ", "c2")), []string{"stage:held"},
			[]string{"too long", "/hold", "c1", "/resume", "/hold "}, nil,
			func() []string {
				return []string{
					fmt.Sprintf("from=clarifying to=held at=clarifying reason=owner cmd=%d seen=%d", *ids["/hold"], request),
					fmt.Sprintf("from=held to=clarifying cmd=%d seen=%d", *ids["/resume"], request),
					fmt.Sprintf("from=clarifying to=held at=clarifying reason=owner cmd=%d seen=%d", *ids["/hold "], request),
				}
			}},
		{"at clarifying: a comment in each of two holds is an answer once both end",
			append(sentBackAt, say("/hold", "c1", "/resume", "/hold ", "c2", "/resume ")), []string{"stage:clarifying"},
			[]string{"too long", "/hold", "c1", "/resume", "/hold ", "c2", "/resume "}, []string{"too long", "c1", "c2"},
			func() []string {
				return []string{
					fmt.Sprintf("from=clarifying to=held at=clarifying reason=owner cmd=%d seen=%d", *ids["/hold"], request),
					fmt.Sprintf("from=held to=clarifying cmd=%d seen=%d", *ids["/resume"], request),
					fmt.Sprintf("from=clarifying to=held at=clarifying reason=owner cmd=%d seen=%d", *ids["/hold "], request),
					fmt.Sprintf("from=held to=clarifying cmd=%d seen=%d", *ids["/resume "], request),
				}
			}},
		{"at spec-review: the comment while held is the request, and the one after it an answer",
			[]heldStackStep{say("/hold", "too long", "/resume", "use X")}, []string{"stage:clarifying"},
			[]string{"/hold", "too long", "/resume", "use X"}, []string{"too long", "use X"},
			func() []string {
				return []string{
					fmt.Sprintf("from=spec-review to=held at=spec-review reason=owner cmd=%d seen=1001", *ids["/hold"]),
					fmt.Sprintf("from=held to=spec-review cmd=%d seen=1001", *ids["/resume"]),
					fmt.Sprintf("from=spec-review to=clarifying cmd=%d seen=%[1]d", *ids["too long"]),
				}
			}},
		{"at spec-review: the request between two holds sends the spec back before the second",
			[]heldStackStep{say("/hold", "c1", "/resume", "/hold ", "/resume ")}, []string{"stage:clarifying"},
			[]string{"/hold", "c1", "/resume", "/hold ", "/resume "}, []string{"c1"},
			func() []string {
				return []string{
					fmt.Sprintf("from=spec-review to=held at=spec-review reason=owner cmd=%d seen=1001", *ids["/hold"]),
					fmt.Sprintf("from=held to=spec-review cmd=%d seen=1001", *ids["/resume"]),
					fmt.Sprintf("from=spec-review to=clarifying cmd=%d seen=%[1]d", *ids["c1"]),
					fmt.Sprintf("from=clarifying to=held at=clarifying reason=owner cmd=%d seen=%d", *ids["/hold "], *ids["c1"]),
					fmt.Sprintf("from=held to=clarifying cmd=%d seen=%d", *ids["/resume "], *ids["c1"]),
				}
			}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setup := heldStackSetup(review(func(testing.TB, *githubtest.Fake) {}), tt.steps)
			var in []string
			githubtest.Replay(t, setup, heldStackPass(t, 1, &in))
			before := len(setup(t).State().Issues[0].Comments)
			got := run(t, setup, heldStackPass(t, 1, &in)).State().Issues[0]
			var markers, acked []string
			for i, c := range got.Comments {
				if i >= before {
					_, m, _ := strings.Cut(c.Body, "<!-- issuedog:transition ")
					markers = append(markers, strings.TrimSuffix(m, " -->"))
				}
				if slices.Contains(c.Reactions, app+" +1") {
					acked = append(acked, c.Body)
				}
			}
			if !reflect.DeepEqual(markers, tt.markers()) || !reflect.DeepEqual(got.Labels, tt.labels) {
				t.Errorf("new markers %q and labels %q, want %q and %q", markers, got.Labels, tt.markers(), tt.labels)
			}
			if !reflect.DeepEqual(acked, tt.acked) || !reflect.DeepEqual(in, tt.inputs) {
				t.Errorf("👍'd %q and inputs %q, want %q and %q", acked, in, tt.acked, tt.inputs)
			}
		})
	}
}

// #80's note 2 and #83's note: a hold at clarifying keeps the round, so a
// comment posted while held is an answer after it, and the revision
// request's notes are still input.
func TestHeldStackKeepsTheRound(t *testing.T) {
	var request, hold, during, resume int64
	steps := []heldStackStep{
		{1, heldStackSay(1, "too long", &request)},
		{1, heldStackSay(1, "/hold", &hold)},
		{1, heldStackSay(1, "use X", &during)},
		{1, heldStackSay(1, "/resume", &resume)},
		{1, func(testing.TB, *githubtest.Fake) {}},
	}
	base := review(func(testing.TB, *githubtest.Fake) {})
	wants := []struct {
		name     string
		comments func() []string
		labels   []string
		inputs   []string
		acked    []string // the comments the App 👍'd, in order
	}{
		{"the request sends the spec back", func() []string { return []string{sentBack(fmt.Sprint(request))} },
			[]string{"stage:clarifying"}, []string{"too long"}, []string{"too long"}},
		{"/hold keeps the round's bound", func() []string {
			return []string{moved("On hold by `/hold`, from `clarifying`.", fmt.Sprintf("from=clarifying to=held at=clarifying reason=owner cmd=%d seen=%d", hold, request))}
		}, []string{"stage:held"}, nil, []string{"too long", "/hold"}},
		{"a comment while held waits", func() []string { return nil }, []string{"stage:held"}, nil, []string{"too long", "/hold"}},
		{"/resume keeps it too, so the notes and the comment are input, and the comment is an answer at once", func() []string {
			return []string{moved("Resumed by `/resume`, back to `clarifying`.", fmt.Sprintf("from=held to=clarifying cmd=%d seen=%d", resume, request))}
		}, []string{"stage:clarifying"}, []string{"too long", "use X"}, []string{"too long", "/hold", "use X", "/resume"}},
		{"the next pass keeps them as the round's input", func() []string { return nil },
			[]string{"stage:clarifying"}, []string{"too long", "use X"}, []string{"too long", "/hold", "use X", "/resume"}},
	}
	for k, w := range wants {
		t.Run(w.name, func(t *testing.T) {
			setup := heldStackSetup(base, steps[:k+1])
			var in []string
			githubtest.Replay(t, setup, heldStackPass(t, 1, &in))
			before := len(setup(t).State().Issues[0].Comments)
			got := run(t, setup, heldStackPass(t, 1, &in)).State().Issues[0]
			var added, acked []string
			for i, c := range got.Comments {
				if i >= before {
					added = append(added, c.Body)
				}
				if slices.Contains(c.Reactions, app+" +1") {
					acked = append(acked, c.Body)
				}
			}
			if !reflect.DeepEqual(added, w.comments()) || !reflect.DeepEqual(got.Labels, w.labels) {
				t.Errorf("new comments %q and labels %q, want %q and %q", added, got.Labels, w.comments(), w.labels)
			}
			if !reflect.DeepEqual(in, w.inputs) || !reflect.DeepEqual(acked, w.acked) {
				t.Errorf("inputs %q and 👍'd %q, want %q and %q", in, acked, w.inputs, w.acked)
			}
		})
	}
}
