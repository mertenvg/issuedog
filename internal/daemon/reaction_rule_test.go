package daemon_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// One rule takes the 👍 of a plan's approval (SPEC §7.4), a hand-off's end
// (SPEC §9.3) and a follow-up's approval (SPEC §10.2): the earliest 👍 by
// a user trust trusts, not a bot (SPEC §6.1). This runs one set of
// reactions through all three, so they can't drift apart.

// ruleThumb is one reaction of a case: its user and its content.
type ruleThumb struct{ login, content string }

// ruleTrust trusts owner and owner2, and the App and a helper bot, so only
// their being bots leaves them out.
var ruleTrust = trusted{"owner": true, "owner2": true, app: true, "helper[bot]": true}

// ruleTrigger is one of the three triggers: the fake it acts on, with
// issue 1 waiting for its 👍, how to add a reaction where it reads them,
// and the trigger itself.
type ruleTrigger struct {
	name  string
	setup func(testing.TB) *githubtest.Fake
	react func(f *githubtest.Fake, login, content string) int64
	run   func(github.Trust) func(ctx context.Context, a *daemon.Applier, s *github.Snapshot, d daemon.Dirt) error
}

var ruleTriggers = []ruleTrigger{
	{"plan approval", planReview(func(testing.TB, *githubtest.Fake) {}),
		func(f *githubtest.Fake, login, content string) int64 { return f.AddReaction(1003, login, content) }, daemon.ApproveByReaction},
	{"hand-off", handbackTask(func(testing.TB, *githubtest.Fake) {}),
		func(f *githubtest.Fake, login, content string) int64 { return f.AddReaction(1002, login, content) }, daemon.HandoffByReaction},
	{"follow-up", approvalFiled(func(testing.TB, *githubtest.Fake) {}),
		func(f *githubtest.Fake, login, content string) int64 { return f.AddIssueReaction(1, login, content) }, daemon.FollowUpByReaction},
}

// TestReactionRuleShared runs each case's reactions, in order, through each
// trigger's step 1, and checks which 👍 its move is keyed by: the want'th
// reaction, from 0, or none for -1.
func TestReactionRuleShared(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		thumbs []ruleThumb
		want   int
	}{
		{"one trusted 👍", []ruleThumb{{"owner", "+1"}}, 0},
		{"the earliest of two trusted 👍s", []ruleThumb{{"owner2", "+1"}, {"owner", "+1"}}, 0},
		{"an untrusted 👍, bots' 👍s, a ❤️ and a 👎 before a trusted 👍",
			[]ruleThumb{{"eve", "+1"}, {"helper[bot]", "+1"}, {app, "+1"}, {"owner", "heart"}, {"owner", "-1"}, {"owner2", "+1"}, {"owner", "+1"}}, 5},
		{"no trusted 👍", []ruleThumb{{"eve", "+1"}, {app, "+1"}, {"helper[bot]", "+1"}, {"owner", "laugh"}}, -1},
		{"no reaction", nil, -1},
	} {
		for _, tr := range ruleTriggers {
			t.Run(tc.name+", "+tr.name, func(t *testing.T) {
				t.Parallel()
				f := tr.setup(t)
				var ids []int64
				for _, th := range tc.thumbs {
					ids = append(ids, tr.react(f, th.login, th.content))
				}
				writes := len(f.Writes())
				a, s, err := applier(t, f)
				if err != nil {
					t.Fatal(err)
				}
				st := &daemon.Step{Applier: a, Trust: ruleTrust, Commands: map[string]daemon.Handler{}, Reactions: tr.run(ruleTrust)}
				if _, err := st.Run(context.Background(), s); err != nil {
					t.Fatalf("step 1: %v", err)
				}
				// The newest comment, or none on a follow-up nobody has
				// commented on that no 👍 moved.
				last := ""
				if comments := f.State().Issues[0].Comments; len(comments) > 0 {
					last = comments[len(comments)-1].Body
				}
				for i, id := range ids {
					if keyed := strings.Contains(last, fmt.Sprintf(" cmd=r%d ", id)); keyed != (i == tc.want) {
						t.Errorf("the newest comment %q is keyed by reaction %d (%s's %s): %t; want %t", last, i, tc.thumbs[i].login, tc.thumbs[i].content, keyed, i == tc.want)
					}
				}
				if w := f.Writes()[writes:]; tc.want < 0 && len(w) != 0 {
					t.Errorf("wrote %+v; want nothing", w)
				}
			})
		}
	}
}
