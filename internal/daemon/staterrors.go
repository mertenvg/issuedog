package daemon

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// A state error (SPEC §5.3) is open until a trusted 👍 acknowledges it or
// its cause is gone (SPEC §5.3, Open). openErrors says which are open, and
// ErrorByReaction records each acknowledgment (SPEC §5.3, Which 👍).
// Iterate runs step 1 on every pass on an issue with an open error, since
// a 👍 changes no updated_at.

// openErr is an open state error: its kind and key, and the ID of the
// earliest clean comment that carries it (state.Issue.Errors), or
// state.BodyID.
type openErr struct {
	state.ErrorKey
	Comment int64
}

// openErrors returns the open state errors of s, in state is, oldest
// first. None is open on a closed issue, nor one a trusted user
// acknowledged (state.Issue.Acked). Otherwise each kind ends when its
// cause is gone:
//   - transition, disagreement and children: at a transition marker in a
//     comment after the error's. One in the same comment isn't after it,
//     nor is one in the body, which comes before every comment.
//   - transition, keyed by a run's moveKey rather than by what triggered
//     the move (triageAdmissionKey): also when the moveKey changes, as on
//     new input or a change of type or parent.
//   - attempts: when the attempt key changes (attemptKey).
//   - labels: when the change to the model and effort labels that stands
//     changes (labelsKey), which CheckLabels must have set on s.
//   - type: when the type is no longer the cause (typeErrStands), or the
//     type change that stands changes (typeErrKey), which CheckLabels must
//     have set on s too.
//   - typegone: when the revert of the issue type that the org's deleted
//     type blocks changes, or none does (typeGoneKey), which CheckLabels
//     must have set on s too.
//   - closer: when closerEnded, if set, reports it ended for the error's
//     key: the question of how its close was made has been answered while
//     the daemon runs, or step 1 no longer asks it, as the close is no
//     longer the one to judge (Applier.closerErrEnded).
//   - digesthistory: at once, as step 1 visits only an issue the digest's
//     title rule has judged (digestHistoryHold).
//   - any other kind: only an ack ends it.
//
// inputs gives step 1's Inputs for s. It's called at most once, and only
// for a run's transition error or an attempts error, which key on them.
func openErrors(s *github.Snapshot, is state.Issue, inputs func() ([]github.Comment, error), closerEnded func(key string) bool) ([]openErr, error) {
	if s.Closed {
		return nil, nil
	}
	var attempt string
	attemptOf := func() (string, error) {
		if attempt == "" { // attemptKey is never ""
			in, err := inputs()
			if err != nil {
				return "", fmt.Errorf("open state errors of #%d: %w", s.Number, err)
			}
			attempt = attemptKey(is, in)
		}
		return attempt, nil
	}
	var open []openErr
	for k, id := range is.Errors {
		ended := is.Acked(k.Kind, k.Key)
		if k.Kind == "transition" || k.Kind == "disagreement" || k.Kind == childrenErrKind {
			ended = ended || id < is.TransitionComment
		}
		switch {
		case ended:
		case k.Kind == "transition" && !triageAdmissionKey(k.Key), k.Kind == kindAttempts:
			a, err := attemptOf()
			if err != nil {
				return nil, err
			}
			if k.Kind == "transition" {
				a = moveKey(a, is.Item)
			}
			ended = k.Key != a
		case k.Kind == kindLabels:
			ended = k.Key != labelsKey(s)
		case k.Kind == typeErrKind:
			ended = k.Key != typeErrKey(s) || !typeErrStands(s, is)
		case k.Kind == typeGoneErrKind:
			ended = k.Key != typeGoneKey(s)
		case k.Kind == closerErrKind:
			ended = closerEnded != nil && closerEnded(k.Key)
		case k.Kind == digestHistoryErrKind:
			ended = true
		}
		if !ended {
			open = append(open, openErr{ErrorKey: k, Comment: id})
		}
	}
	slices.SortFunc(open, func(a, b openErr) int {
		return cmp.Or(cmp.Compare(a.Comment, b.Comment), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Key, b.Key))
	})
	return open, nil
}

// openErrCopies returns the clean comments of s that carry the state error
// k, posted (not an ack reply), oldest first. A race may have posted it
// more than once, and a 👍 on any copy counts. The body is left out: its
// 👍s aren't a comment's.
func openErrCopies(s *github.Snapshot, appSlug string, dirty func(int64) bool, k state.ErrorKey) []int64 {
	var ids []int64
	for _, c := range s.Comments {
		if dirty(c.ID) {
			continue
		}
		if slices.ContainsFunc(github.TrustedMarkers(c.Author, c.Body, appSlug), func(m github.Marker) bool {
			return m.Kind == "error" && m.Fields["ack"] == "" && m.Fields["kind"] == k.Kind && m.Fields["key"] == k.Key
		}) {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

// openErrInputs returns s's Inputs as step 1 gives them (Step.Run): the
// comments state.Issue.Inputs takes from those by a user trust trusts, not
// a bot, that aren't dirty.
func openErrInputs(ctx context.Context, trust github.Trust, s *github.Snapshot, is state.Issue, d Dirt) ([]github.Comment, error) {
	counts := map[int64]bool{}
	known := map[string]bool{}
	for _, c := range s.Comments {
		if c.Author.Type == "Bot" || d.Has(c.ID) {
			continue
		}
		t, ok := known[c.Author.Login]
		if !ok {
			var err error
			if t, err = trust.Trusted(ctx, c.Author.Login); err != nil {
				return nil, fmt.Errorf("inputs of #%d: trust %s: %w", s.Number, c.Author.Login, err)
			}
			known[c.Author.Login] = t
		}
		counts[c.ID] = t
	}
	return is.Inputs(s, func(c github.Comment) bool { return counts[c.ID] }), nil
}

// ErrorByReaction returns the reaction trigger for Step.Reactions that
// acknowledges a state error by 👍 (SPEC §5.3, Which 👍 and Kind: an ack
// names the error's kind). For each open error (openErrors), the earliest
// 👍 from a user trust trusts, not a bot, on any clean comment carrying it
// gets one reply, "Acknowledged, by `<login>`'s 👍.", with
// <!-- issuedog:error kind=<kind> key=<key> ack=r<reaction id> -->. The
// reply is the record: the error is acknowledged, so it's no longer open
// and is never posted again (state.Issue.Reported), and a replay finds it.
// So removing the 👍 later changes nothing, and an error that has ended,
// acknowledged or with its cause gone, gets no read of its reactions: a 👍
// on it is ignored. An error in the body only, which the App never posts,
// can't be acknowledged this way.
func ErrorByReaction(trust github.Trust) func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error {
	return func(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt) error {
		wrap := func(err error) error {
			return fmt.Errorf("acknowledgments on %s/%s#%d: %w", a.Owner, a.Repo, s.Number, err)
		}
		is := state.Derive(s, a.AppSlug, d.Has)
		if s.Closed || len(is.Errors) == 0 {
			return nil
		}
		known := map[string]bool{}
		trusted := func(login string) (bool, error) {
			t, ok := known[login]
			if !ok {
				var err error
				if t, err = trust.Trusted(ctx, login); err != nil {
					return false, fmt.Errorf("trust %s: %w", login, err)
				}
				known[login] = t
			}
			return t, nil
		}
		open, err := openErrors(s, is, func() ([]github.Comment, error) { return openErrInputs(ctx, trust, s, is, d) }, a.closerErrEnded(s, d.Has, trust))
		if err != nil {
			return wrap(err)
		}
		for _, e := range open {
			var first *github.Reaction
			for _, id := range openErrCopies(s, a.AppSlug, d.Has, e.ErrorKey) {
				reactions, err := a.Client.Reactions(ctx, a.Owner, a.Repo, id)
				if err != nil {
					return wrap(err)
				}
				for _, r := range reactions {
					if r.Content != "+1" || r.User.Type == "Bot" || (first != nil && first.ID < r.ID) {
						continue
					}
					t, err := trusted(r.User.Login)
					if err != nil {
						return wrap(err)
					}
					if t {
						first = &r
					}
				}
			}
			if first == nil {
				continue
			}
			marker, err := github.FormatMarker(github.Marker{Kind: "error", Fields: map[string]string{
				"kind": e.Kind, "key": e.Key, "ack": "r" + strconv.FormatInt(first.ID, 10)}})
			if err != nil {
				return wrap(err)
			}
			// The login isn't the App's text: it's escaped and defused.
			text := fmt.Sprintf("Acknowledged, by `%s`'s 👍.", defuse(login(first.User)))
			if err := a.comment(ctx, s, join(github.Escape(text), marker)); err != nil {
				return wrap(err)
			}
			logr.Infof("%s/%s#%d: %s acknowledged the %s state error keyed %s by 👍", a.Owner, a.Repo, s.Number, login(first.User), e.Kind, e.Key)
		}
		return nil
	}
}

// openErrAny reports whether s, which step 1 just ran on and gave inputs,
// with dirty content d, has an open state error (openErrors), so Iterate
// visits it on every pass. Iterate finds d once, for the digest's items too.
func (st *Step) openErrAny(s *github.Snapshot, inputs []github.Comment, d Dirt) (bool, error) {
	open, err := openErrors(s, state.Derive(s, st.AppSlug, d.Has), func() ([]github.Comment, error) { return inputs, nil }, st.closerErrEnded(s, d.Has, st.trustOnceTrust()))
	if err != nil {
		return false, fmt.Errorf("open state errors of %s/%s#%d: %w", st.Owner, st.Repo, s.Number, err)
	}
	return len(open) > 0, nil
}
