package daemon_test

import (
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

// The verifier's probe of #243's wait: eve, with no access, posts /hold on
// A, which holds nothing. Then she's given write access and named in
// CODEOWNERS, so her /hold counts, though A never changes, and the owner's
// /resume on J, numbered below A, waits for it. The wait reads A again on
// the next pass, which handles eve's /hold: A goes on hold, and J and K,
// held already by J's own /hold, get no marker. Then J's /resume ends both
// /holds on J and K, which go back to ready. Every pass after is
// unchanged, so the extra reads last only while J's /resume waits.
func TestHoldOrderRunTrustedSince(t *testing.T) {
	t.Parallel()
	f := discRunRaceFamily(false)(t)
	a, j, k := discRunRaceIssues(false)
	l := newDiscRunLoop(t, f)
	discRunRaceHeld(t, l, false)
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
