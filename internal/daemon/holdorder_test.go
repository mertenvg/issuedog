package daemon_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// This file is #243: a /hold on an issue A, then a /resume on its
// descendant J, both before the next pass. Step 1 visits the issues in
// number order, so with J's number lower, J's /resume was handled first,
// and A's /hold then held J and the issues below it again, though the
// /resume, posted after it, ends it. Now J's /resume waits for A's /hold,
// so the two apply in the order they were posted (holdOrderWaits).

// The owner's /hold on A, then a /resume on J, in one poll, with nothing
// held before: in every order of their numbers, A's /hold holds all
// three, and J's /resume, handled after it, ends it on J and K, so A alone
// stays held, and J's /resume isn't rejected. A's later /resume releases
// A.
func TestHoldOrderNothingHeld(t *testing.T) {
	t.Parallel()
	for _, order := range raceOrders {
		t.Run(fmt.Sprintf("A J K %v", order), func(t *testing.T) {
			t.Parallel()
			f, a, j, k, _ := raceFamily(t, order, false)
			f.AddComment(a, "owner", "/hold")
			resume := f.AddComment(j, "owner", "/resume")
			resolveSettle(t, f, 1, 2, 3)
			raceStages(t, f, "after both", map[int]string{a: "held", j: "ready", k: "ready"})
			if anyComment(f, j, fmt.Sprintf("reason=rejected cmd=%d ", resume)) {
				t.Errorf("#%d: newest comment %q; want J's /resume applied", j, newestComment(f, j))
			}
			f.AddComment(a, "owner", "/resume")
			resolveSettle(t, f, 1, 2, 3)
			raceStages(t, f, "after A's /resume", map[int]string{a: "ready", j: "ready", k: "ready"})
		})
	}
}

// Only a /hold or a /resume waits, and only for one: with J's number
// lower, J's /priority, posted after A's /hold, applies in J's pass,
// before A's /hold is handled, as before #243; and J's /resume, posted
// after A's /priority, applies in J's pass too.
func TestHoldOrderOtherCommands(t *testing.T) {
	t.Parallel()
	order := [3]int{2, 1, 3} // A #2, J #1, K #3
	t.Run("J's /priority after A's /hold", func(t *testing.T) {
		t.Parallel()
		f, a, j, _, _ := raceFamily(t, order, false)
		f.AddComment(a, "owner", "/hold")
		f.AddComment(j, "owner", "/priority p1")
		resolveSteps(t, f, j)
		if l := f.State().Issues[j-1].Labels; !slices.Contains(l, "priority:p1") || !slices.Contains(l, "stage:ready") {
			t.Errorf("#%d: labels %q; want priority:p1 at ready, before A's /hold", j, l)
		}
	})
	t.Run("J's /resume after A's /priority", func(t *testing.T) {
		t.Parallel()
		f, a, j, k, _ := raceFamily(t, order, true)
		f.AddComment(a, "owner", "/priority p1")
		f.AddComment(j, "owner", "/resume")
		resolveSteps(t, f, j)
		raceStages(t, f, "after J's pass", map[int]string{j: "ready", k: "ready"})
	})
}

// J's /resume waits for no /hold above it that the App never handles, or
// that holds nothing, or handled already: one on a closed ancestor, which
// no pass visits, one on a parent in another repo, where the walk up stops
// (madeAbove), whose comment IDs aren't this repo's to order by, one by
// eve, who isn't trusted, one a write collaborator
// edited, so dirty (SPEC §5.1), and one the App 👎'd, as A's type is one
// the pipeline doesn't know, whose reply is lost, so the 👎 alone says
// it's processed (SPEC §9.2). J's pass alone takes J and K back to ready.
func TestHoldOrderNoWait(t *testing.T) {
	t.Parallel()
	for name, hold := range map[string]func(t testing.TB, f *githubtest.Fake, a int){
		"A closed": func(_ testing.TB, f *githubtest.Fake, a int) {
			f.CloseBy(a, "owner")
			f.AddComment(a, "owner", "/hold")
		},
		"A in another repo": func(_ testing.TB, f *githubtest.Fake, a int) {
			f.AddComment(a, "owner", "/hold")
			f.Transfer(a, "o/other")
		},
		"eve's /hold": func(_ testing.TB, f *githubtest.Fake, a int) { f.AddComment(a, "eve", "/hold") },
		"edited by mallory": func(_ testing.TB, f *githubtest.Fake, a int) {
			f.EditComment(f.AddComment(a, "owner", "/hold"), "mallory", "/hold")
		},
		"rejected, its reply lost": func(t testing.TB, f *githubtest.Fake, a int) {
			f.SetType(a, "Weird")
			f.AddComment(a, "owner", "/hold")
			resolveSteps(t, f, a)
			f.DeleteComment(discRunFind(t, f, a, "reason=rejected"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, a, j, k, _ := raceFamily(t, [3]int{3, 1, 2}, true)
			hold(t, f, a)
			f.AddComment(j, "owner", "/resume")
			resolveSteps(t, f, j)
			raceStages(t, f, "after J's pass", map[int]string{j: "ready", k: "ready"})
		})
	}
}

// A /hold on A that a marker names is handled, so J's /resume, posted
// after it, reads none of its reactions to know that.
func TestHoldOrderMarkedNoRead(t *testing.T) {
	t.Parallel()
	f, a, j, k, _ := raceFamily(t, [3]int{3, 1, 2}, true)
	hold := f.AddComment(a, "owner", "/hold")
	resolveSettle(t, f, 1, 2, 3)
	if !anyComment(f, a, fmt.Sprintf("reason=owner cmd=%d ", hold)) {
		t.Fatalf("#%d: newest comment %q; want A's /hold's marker", a, newestComment(f, a))
	}
	f.AddComment(j, "owner", "/resume")
	from := len(f.Requests())
	resolveSteps(t, f, j)
	raceStages(t, f, "after J's pass", map[int]string{a: "held", j: "ready", k: "ready"})
	for _, r := range f.Requests()[from:] {
		if r.Path == fmt.Sprintf("/issues/comments/%d/reactions", hold) {
			t.Errorf("J's pass sent %+v; want no read of A's /hold, which its marker names", r)
		}
	}
}

// holdOrderMoves is the writes of #n's move out of stage from.
func holdOrderMoves(n int, from string) []string {
	return []string{fmt.Sprintf("POST /issues/%d/comments", n), fmt.Sprintf("POST /issues/%d/labels", n), fmt.Sprintf("DELETE /issues/%d/labels/stage:%s", n, from)}
}

// #243 through the whole run: the owner's /hold on A, then a /resume on
// J, in one poll, J and K held by J's own /hold. With A's number lower,
// A's /hold is handled first, writes no marker on J and K, held already
// (SPEC §6.4), and touches them, so J's /resume is handled on the next pass.
// With J's lower, J's /resume waits for A's /hold, and J is read again,
// so it's handled on the next pass too. Either way it ends A's /hold and
// J's own on J and K, which go back to ready, while A stays held. Every
// pass after, and a restart's, is unchanged, and A's /resume releases A.
func TestHoldOrderRun(t *testing.T) {
	t.Parallel()
	for _, aLow := range []bool{true, false} {
		t.Run(fmt.Sprintf("A lower %v", aLow), func(t *testing.T) {
			t.Parallel()
			f := discRunRaceFamily(aLow)(t)
			a, j, k := discRunRaceIssues(aLow)
			l := newDiscRunLoop(t, f)
			discRunRaceHeld(t, l, aLow)
			l.quiet("J and K held")

			hold := f.AddComment(a, "owner", "/hold")
			resume := f.AddComment(j, "owner", "/resume")
			l.step("A's /hold", slices.Concat(holdOrderMoves(a, "ready"), []string{discRunReact(hold)})...)
			l.step("J's /resume", slices.Concat(holdOrderMoves(k, "held"), holdOrderMoves(j, "held"), []string{discRunReact(resume)})...)
			for n, want := range map[int]string{a: "stage:held", j: "stage:ready", k: "stage:ready"} {
				if got := ivLabels(f, n); !slices.Equal(got, []string{want, "type:task"}) {
					t.Errorf("#%d: labels %q, newest comment %q; want %s", n, got, newestComment(f, n), want)
				}
			}
			for _, n := range []int{j, k} {
				if body := newestComment(f, n); !strings.Contains(body, fmt.Sprintf("from=held to=ready cmd=%d ", resume)) {
					t.Errorf("#%d: newest comment %q; want J's /resume's move", n, body)
				}
			}
			l.quiet("after both")
			l.restart("after both")

			again := f.AddComment(a, "owner", "/resume")
			l.step("A's /resume", slices.Concat(holdOrderMoves(a, "held"), []string{discRunReact(again)})...)
			l.quiet("resumed")
		})
	}
}

// With J's number lower, an earlier /resume on A that the App rejects, as
// nothing is held, touches no issue, and J, which isn't held, wouldn't be
// visited again until it changed: so J's /hold, posted after it, which
// waits for it, reads J again on the next pass, and applies then.
func TestHoldOrderRunRejectedAbove(t *testing.T) {
	t.Parallel()
	f := discRunRaceFamily(false)(t)
	a, j, k := discRunRaceIssues(false)
	l := newDiscRunLoop(t, f)
	l.quiet("all at ready")
	resume := f.AddComment(a, "owner", "/resume")
	hold := f.AddComment(j, "owner", "/hold")
	// The 👎, then the reply.
	l.step("A's /resume, rejected", discRunReact(resume), fmt.Sprintf("POST /issues/%d/comments", a))
	if body := newestComment(f, a); !strings.Contains(body, fmt.Sprintf("reason=rejected cmd=%d ", resume)) {
		t.Errorf("#%d: newest comment %q; want A's /resume rejected", a, body)
	}
	l.step("J's /hold", slices.Concat(holdOrderMoves(k, "ready"), holdOrderMoves(j, "ready"), []string{discRunReact(hold)})...)
	l.quiet("J and K held")
}

// holdOrderRejectedAbove is A's /resume, which the App rejects, then a
// command on J, posted after it, that waits for it, with J's number lower
// (TestHoldOrderRunRejectedAbove): J's /hold, with nothing held, or with
// J held for discussion by mallory's comment before, so the /hold writes
// no marker on J, only its 👍 (SPEC §6.4); or J's /resume, with J and K
// held by J's own /hold before. reason is the one the App gives A's
// /resume in a clean run, and moved the issues J's command moves out of
// from, K first.
var holdOrderRejectedAbove = []struct {
	name, before, cmd, from, reason string
	moved                           []int
}{
	{"J's /hold", "", "/hold", "ready", "nothing here or in its open sub-issues is on an owner hold", []int{4, 2}},
	{"J's /hold, J held for discussion", "mallory", "/hold", "ready",
		"#2 is on hold for discussion, which only a code owner's response there, or `/resume` there, ends", []int{4}},
	{"J's /resume", "J's /hold", "/resume", "held", "the holds left were made on #2, by `/hold` or by hand, so `/resume` there ends them", []int{4, 2}},
}

// holdOrderRejectedAboveBuild settles l on discRunRaceFamily(false), with
// before done first: J's own /hold applied, or mallory's comment on J,
// which holds J for discussion. Then it posts A's /resume and J's cmd,
// and returns their IDs.
func holdOrderRejectedAboveBuild(t *testing.T, l *discRunLoop, before, cmd string) (resume, onJ int64) {
	t.Helper()
	a, j, _ := discRunRaceIssues(false)
	switch before {
	case "J's /hold":
		discRunRaceHeld(t, l, false)
	case "mallory":
		l.f.AddComment(j, "mallory", "Wait.")
		for range discRunSettle {
			from := len(l.f.Writes())
			if err := l.g.d.Iterate(context.Background()); err != nil {
				t.Fatalf("mallory's comment: Iterate: %v", err)
			}
			if len(l.f.Writes()) == from {
				break
			}
		}
		if got := ivLabels(l.f, j); !slices.Contains(got, "stage:held") {
			t.Fatalf("#%d: labels %q; want J held for discussion", j, got)
		}
	}
	l.quiet("before A's /resume")
	return l.f.AddComment(a, "owner", "/resume"), l.f.AddComment(j, "owner", cmd)
}

// A crash just after the App's 👎 on A's /resume, before its reply: the 👎
// alone counts it as processed (SPEC §9.2), so on the next pass J's
// command, which waited for it, is handled before the reply is posted
// again. The reply is worded as of the 👎, without what J's command did,
// so it gives the clean run's reason (reword), not one from J's hold, or
// from its end. The whole is safe to replay after a crash at any write,
// and after the reply, a pass that reads it back and an unchanged one
// write nothing, the unchanged one sending only conditional GETs answered
// 304.
func TestHoldOrderRunRejectedAboveReplay(t *testing.T) {
	t.Parallel()
	for _, tc := range holdOrderRejectedAbove {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			discRunReplay(t, discRunRaceFamily(false), nil, func(l *discRunLoop) { holdOrderRejectedAboveBuild(t, l, tc.before, tc.cmd) })

			f := discRunRaceFamily(false)(t)
			a, _, _ := discRunRaceIssues(false)
			l := newDiscRunLoop(t, f)
			resume, onJ := holdOrderRejectedAboveBuild(t, l, tc.before, tc.cmd)
			// The pass's first write is the 👎.
			from := len(f.Writes())
			f.FailWriteAfter(from + 1)
			if err := l.g.d.Iterate(context.Background()); err == nil {
				t.Fatal("the pass that crashed after the 👎 succeeded")
			}
			if w := f.Writes(); len(w) == from || w[from].Path != fmt.Sprintf("/issues/comments/%d/reactions", resume) {
				t.Fatalf("the pass that crashed wrote %+v; want the 👎 on A's /resume first", w[from:])
			}
			f.FailWrite(0)
			l.start() // the restart after the crash
			var want []string
			for _, n := range tc.moved {
				want = append(want, holdOrderMoves(n, tc.from)...)
			}
			l.step("the restart's pass, which handles J's command, then posts the reply again",
				append(want, discRunReact(onJ), fmt.Sprintf("POST /issues/%d/comments", a))...)
			if body := newestComment(f, a); !strings.Contains(body, "I didn't apply this command: "+tc.reason+".") {
				t.Errorf("#%d: newest comment %q; want the clean run's reason, %q", a, body, tc.reason)
			}
			l.quiet("after the reply")
		})
	}
}

// #243's race through the whole run is safe to replay after a crash at
// any write, with either number lower.
func TestHoldOrderRunReplay(t *testing.T) {
	t.Parallel()
	for _, aLow := range []bool{true, false} {
		t.Run(fmt.Sprintf("A lower %v", aLow), func(t *testing.T) {
			t.Parallel()
			discRunReplay(t, discRunRaceFamily(aLow), nil, func(l *discRunLoop) {
				a, j, _ := discRunRaceIssues(aLow)
				discRunRaceHeld(t, l, aLow)
				l.f.AddComment(a, "owner", "/hold")
				l.f.AddComment(j, "owner", "/resume")
			})
		})
	}
}

// The epic's /hold, then a /resume on a task two levels below it, in one
// poll, the common shape in this pipeline: K's /resume waits for A's
// /hold, its grandparent's, so in every order of their numbers A's /hold
// holds all three, and K's /resume, handled after it, ends it on K alone.
// A and J stay held.
func TestHoldOrderGrandparent(t *testing.T) {
	t.Parallel()
	for _, order := range raceOrders {
		t.Run(fmt.Sprintf("A J K %v", order), func(t *testing.T) {
			t.Parallel()
			f, a, j, k, _ := raceFamily(t, order, false)
			f.AddComment(a, "owner", "/hold")
			resume := f.AddComment(k, "owner", "/resume")
			resolveSettle(t, f, 1, 2, 3)
			raceStages(t, f, "after both", map[int]string{a: "held", j: "held", k: "ready"})
			if anyComment(f, k, fmt.Sprintf("reason=rejected cmd=%d ", resume)) {
				t.Errorf("#%d: newest comment %q; want K's /resume applied", k, newestComment(f, k))
			}
		})
	}
}

// A closed parent doesn't stop the walk up: with J closed, K's /resume
// still waits for the /hold on A, its open grandparent, so in every order
// of their numbers A's /hold holds A and K, and K's /resume, handled after
// it, ends it on K.
func TestHoldOrderClosedParent(t *testing.T) {
	t.Parallel()
	for _, order := range raceOrders {
		t.Run(fmt.Sprintf("A J K %v", order), func(t *testing.T) {
			t.Parallel()
			f, a, j, k, _ := raceFamily(t, order, false)
			f.CloseBy(j, "owner")
			f.AddComment(a, "owner", "/hold")
			resume := f.AddComment(k, "owner", "/resume")
			resolveSettle(t, f, min(a, k), max(a, k))
			raceStages(t, f, "after both", map[int]string{a: "held", k: "ready"})
			if anyComment(f, k, fmt.Sprintf("reason=rejected cmd=%d ", resume)) {
				t.Errorf("#%d: newest comment %q; want K's /resume applied", k, newestComment(f, k))
			}
		})
	}
}

// #243's wait: eve, with no access, posts /hold on A, which holds nothing.
// Then she's given write access and named in CODEOWNERS, so her /hold
// counts, though A never changes, and the owner's /resume on J, numbered
// below A, waits for it. The wait reads A again on the next pass, which
// handles eve's /hold: A goes on hold, and J and K, held already by J's
// own /hold, get no marker. Then J's /resume ends both /holds on J and K,
// which go back to ready. Every pass after is unchanged, so the extra
// reads last only while J's /resume waits.
func TestHoldOrderRunTrustedSince(t *testing.T) {
	t.Parallel()
	holdOrderRunTrustedSince(t, false)
}

// TestHoldOrderRunTrustedSince with A's number lower: A comes first in
// each pass's number order, but nothing on it changed, so no pass visits
// it till the wait reads it again. So J's /resume waits for eve's /hold
// all the same, and lands after it, as with J's number lower.
func TestHoldOrderRunTrustedSinceAncestorLower(t *testing.T) {
	t.Parallel()
	holdOrderRunTrustedSince(t, true)
}

// holdOrderRunTrustedSince is TestHoldOrderRunTrustedSince's run, with A's
// number lower than J's if aLow.
func holdOrderRunTrustedSince(t *testing.T, aLow bool) {
	t.Helper()
	f := discRunRaceFamily(aLow)(t)
	a, j, k := discRunRaceIssues(aLow)
	l := newDiscRunLoop(t, f)
	discRunRaceHeld(t, l, aLow)
	l.quiet("J and K held")
	eve := f.AddComment(a, "eve", "/hold")
	l.quiet("eve's /hold, which holds nothing")

	f.SetPermission("eve", "write")
	l.o.commit(".github/CODEOWNERS", "* @owner @eve\n")
	resume := f.AddComment(j, "owner", "/resume")
	l.step("J's /resume waits")
	l.step("eve's /hold", slices.Concat(holdOrderMoves(a, "ready"), []string{discRunReact(eve)})...)
	l.step("J's /resume", slices.Concat(holdOrderMoves(k, "held"), holdOrderMoves(j, "held"), []string{discRunReact(resume)})...)
	for n, want := range map[int]string{a: "stage:held", j: "stage:ready", k: "stage:ready"} {
		if got := ivLabels(f, n); !slices.Equal(got, []string{want, "type:task"}) {
			t.Errorf("#%d: labels %q, newest comment %q; want %s", n, got, newestComment(f, n), want)
		}
	}
	l.quiet("after J's /resume")
}

// A /hold on J, then the owner's /resume on its child K, in one poll, with
// A above J: K's /resume waits for J's /hold, its parent's, though A, its
// farthest ancestor, has nothing to wait for, so in every order of their
// numbers J's /hold holds J and K, and K's /resume, handled after it, ends
// it on K. J stays held, and A was never held.
func TestHoldOrderMiddleAncestor(t *testing.T) {
	t.Parallel()
	for _, order := range raceOrders {
		t.Run(fmt.Sprintf("A J K %v", order), func(t *testing.T) {
			t.Parallel()
			f, a, j, k, _ := raceFamily(t, order, false)
			f.AddComment(j, "owner", "/hold")
			resume := f.AddComment(k, "owner", "/resume")
			resolveSettle(t, f, 1, 2, 3)
			raceStages(t, f, "after both", map[int]string{a: "ready", j: "held", k: "ready"})
			if anyComment(f, k, fmt.Sprintf("reason=rejected cmd=%d ", resume)) {
				t.Errorf("#%d: newest comment %q; want K's /resume applied", k, newestComment(f, k))
			}
		})
	}
}

// holdOrderDigestParent is discRunRaceFamily(aLow) with the digest, #1,
// made A's parent, as someone may make it.
func holdOrderDigestParent(aLow bool) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		t.Helper()
		f := discRunRaceFamily(aLow)(t)
		a, _, _ := discRunRaceIssues(aLow)
		f.SetParent(a, 1)
		return f
	}
}

// The owner's /hold on the digest, made A's parent, then a /resume on J,
// in one poll, J and K held by J's own /hold. No pass visits the digest
// (SPEC §9.4), so its /hold is never handled, and J's /resume doesn't
// wait for it, as for one on a closed ancestor: with either number lower,
// J's pass takes J and K back to ready, A is never held, and every pass
// after is unchanged, so nothing is read again for the digest's /hold.
func TestHoldOrderRunDigest(t *testing.T) {
	t.Parallel()
	for _, aLow := range []bool{true, false} {
		t.Run(fmt.Sprintf("A lower %v", aLow), func(t *testing.T) {
			t.Parallel()
			f := holdOrderDigestParent(aLow)(t)
			a, j, k := discRunRaceIssues(aLow)
			l := newDiscRunLoop(t, f)
			discRunRaceHeld(t, l, aLow)
			l.quiet("J and K held")

			f.AddComment(1, "owner", "/hold")
			resume := f.AddComment(j, "owner", "/resume")
			l.step("J's /resume", slices.Concat(holdOrderMoves(k, "held"), holdOrderMoves(j, "held"), []string{discRunReact(resume)})...)
			for n, want := range map[int]string{a: "stage:ready", j: "stage:ready", k: "stage:ready"} {
				if got := ivLabels(f, n); !slices.Equal(got, []string{want, "type:task"}) {
					t.Errorf("#%d: labels %q, newest comment %q; want %s", n, got, newestComment(f, n), want)
				}
			}
			l.quiet("after J's /resume")
			if cs := f.State().Issues[0].Comments; cs[len(cs)-1].Body != "/hold" || len(cs[len(cs)-1].Reactions) != 0 {
				t.Errorf("#1: newest comment %+v; want the owner's /hold, which no pass handles", cs[len(cs)-1])
			}
		})
	}
}

// J's pass on its /resume, under a /hold on the digest made A's parent,
// is safe to replay after a crash at any write, with either number lower.
func TestHoldOrderRunDigestReplay(t *testing.T) {
	t.Parallel()
	for _, aLow := range []bool{true, false} {
		t.Run(fmt.Sprintf("A lower %v", aLow), func(t *testing.T) {
			t.Parallel()
			discRunReplay(t, holdOrderDigestParent(aLow), nil, func(l *discRunLoop) {
				_, j, _ := discRunRaceIssues(aLow)
				discRunRaceHeld(t, l, aLow)
				l.f.AddComment(1, "owner", "/hold")
				l.f.AddComment(j, "owner", "/resume")
			})
		})
	}
}

// Only the App's issue is ever the digest (SPEC §9.4), so an issue eve
// files with the digest's title and its identity marker in the body is
// none: it's A, J's parent, numbered above J, and J's /resume, posted
// after the owner's /hold on A, waits for that /hold as on any ancestor.
// A's /hold lands first, then J's /resume ends both /holds on J and K,
// which go back to ready, while A stays held.
func TestHoldOrderRunNotDigest(t *testing.T) {
	t.Parallel()
	f := digestPinned(t)
	var ns []int
	for _, eve := range []bool{false, true, false} { // J #2, A #3, K #4
		author, title, body := "owner", "Part", "A part."
		if eve {
			author, title, body = "eve", "issuedog digest", "A part.\n\n<!-- issuedog:digest -->"
		}
		n := f.AddIssue(author, title, body)
		f.AddLabel(n, "owner", "type:task")
		f.AddLabel(n, app, "stage:ready")
		f.AddComment(n, app, "<!-- issuedog:transition from=intake to=ready run=rready -->")
		ns = append(ns, n)
	}
	j, a, k := ns[0], ns[1], ns[2]
	f.SetParent(j, a)
	f.SetParent(k, j)
	l := newDiscRunLoop(t, f)
	discRunRaceHeld(t, l, false)
	l.quiet("J and K held")

	hold := f.AddComment(a, "owner", "/hold")
	resume := f.AddComment(j, "owner", "/resume")
	l.step("A's /hold", slices.Concat(holdOrderMoves(a, "ready"), []string{discRunReact(hold)})...)
	l.step("J's /resume", slices.Concat(holdOrderMoves(k, "held"), holdOrderMoves(j, "held"), []string{discRunReact(resume)})...)
	for n, want := range map[int]string{a: "stage:held", j: "stage:ready", k: "stage:ready"} {
		if got := ivLabels(f, n); !slices.Equal(got, []string{want, "type:task"}) {
			t.Errorf("#%d: labels %q, newest comment %q; want %s", n, got, newestComment(f, n), want)
		}
	}
	l.quiet("after both")
}
