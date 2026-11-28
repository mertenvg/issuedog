package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// AdoptReport is what Applier.Adopt did on a repo run by hand (SPEC §15,
// going live), or with dryRun, what it would do.
type AdoptReport struct {
	// Posted is how many App comments it posted, or with dryRun, would.
	Posted int
	// Done is how many sources an earlier run reposted already, which it
	// never reposts again (state.Adoption.Done).
	Done int
	// Refused says why each refused issue, by number, must be fixed first
	// (state.Adoption.Refused). While it isn't empty, nothing is posted.
	Refused map[int]string
}

// adoptPost is one App comment Adopt posts: on issue s, the repost of
// source from, the kind of each marker it reposts, in order, and its body.
type adoptPost struct {
	s     *github.Snapshot
	from  int64
	kinds []string
	body  string
}

// Adopt reposts as the App, once, the markers login wrote on each open
// pipeline issue while it was run by hand, so the daemon, which reads
// markers only from the App (SPEC §5.1), reads each issue as it did with
// login's markers read as the App's (SPEC §5, the adopted marker; §15,
// going live). It never runs the loop and never posts a check run.
//
// login must be a trusted user (SPEC §6.1) by tr: a login ending [bot],
// the App's included, is refused before trust is asked, as tr never trusts
// a bot. A pipeline issue is an open issue with a stage:* label, not a pull
// request. Each is read once, even one the listing gives twice, with its dirty content (Dirty), and
// state.Adopt says which of login's sources it reposts, which an earlier
// run reposted already, and why it must be fixed first, if it must. Every
// refusal is gathered and logged before any write: while one stands,
// Adopt posts nothing, and returns them in the report with a nil error.
// Otherwise it posts each source's App comment, issues by number and
// sources in their order, logging each. With dryRun it logs each comment
// it would post and posts nothing. A source an App comment reposted is
// done (state.Adoption.Done), so a second run posts nothing, and a rerun
// after a crash posts the rest once, in order. The counts are logged last.
func (a *Applier) Adopt(ctx context.Context, tr github.Trust, login string, dryRun bool) (AdoptReport, error) {
	if login == "" {
		return AdoptReport{}, errors.New("adopt: no login")
	}
	if strings.HasSuffix(strings.ToLower(login), "[bot]") {
		return AdoptReport{}, fmt.Errorf("adopt %s's markers: a bot's, the App's included, are never adopted, as no bot is trusted (SPEC §6.1)", login)
	}
	trusted, err := tr.Trusted(ctx, login)
	if err != nil {
		return AdoptReport{}, fmt.Errorf("adopt %s's markers: %w", login, err)
	}
	if !trusted {
		return AdoptReport{}, fmt.Errorf("adopt %s's markers: %s isn't a trusted user (SPEC §6.1)", login, login)
	}
	listed, err := a.Client.IssuesSince(ctx, a.Owner, a.Repo, time.Time{})
	if err != nil {
		return AdoptReport{}, fmt.Errorf("adopt %s's markers: %w", login, err)
	}
	var numbers []int
	for _, is := range listed {
		if is.GetState() == "open" && !is.IsPullRequest() && slices.ContainsFunc(is.Labels, func(l *gogithub.Label) bool {
			return isStageLabel(l.GetName())
		}) {
			numbers = append(numbers, is.GetNumber())
		}
	}
	// The listing pages by update, so an issue updated while it pages is
	// listed twice: it's read once, or its sources would post twice.
	slices.Sort(numbers)
	numbers = slices.Compact(numbers)
	r := AdoptReport{Refused: map[int]string{}}
	var posts []adoptPost
	for _, n := range numbers {
		s, err := a.Client.Snapshot(ctx, a.Owner, a.Repo, n)
		if err != nil {
			return AdoptReport{}, fmt.Errorf("adopt %s's markers: %w", login, err)
		}
		d, err := a.Dirty(ctx, s, tr)
		if err != nil {
			return AdoptReport{}, fmt.Errorf("adopt %s's markers: %w", login, err)
		}
		ad := state.Adopt(s, login, a.AppSlug, d.Has)
		r.Done += len(ad.Done)
		if ad.Refused != "" {
			r.Refused[n] = ad.Refused
			logr.Errorf("%s/%s#%d: adopt refuses it until it's fixed: %s", a.Owner, a.Repo, n, ad.Refused)
			continue
		}
		for _, p := range ad.Posts {
			post, err := a.adoptPostOf(s, login, p)
			if err != nil {
				return AdoptReport{}, fmt.Errorf("adopt %s's markers: %w", login, err)
			}
			posts = append(posts, post)
		}
	}
	if len(r.Refused) > 0 {
		logr.Errorf("adopt posted nothing, as it refuses %d issues: fix each, then run it again", len(r.Refused))
		return r, nil
	}
	for _, p := range posts {
		what := fmt.Sprintf("the %s markers of %s's %s", strings.Join(p.kinds, ", "), login, adoptSource(p.from))
		if dryRun {
			logr.Infof("%s/%s#%d: adopt would post %s", a.Owner, a.Repo, p.s.Number, what)
			r.Posted++
			continue
		}
		if err := a.comment(ctx, p.s, p.body); err != nil {
			return r, fmt.Errorf("adopt %s's markers: %w", login, err)
		}
		r.Posted++
		logr.Infof("%s/%s#%d: adopt posted %s, as comment %d", a.Owner, a.Repo, p.s.Number, what, p.s.Comments[len(p.s.Comments)-1].ID)
	}
	if dryRun {
		logr.Infof("adopt would post %d comments, and found %d sources reposted already", r.Posted, r.Done)
	} else {
		logr.Infof("adopt posted %d comments, and found %d sources reposted already", r.Posted, r.Done)
	}
	return r, nil
}

// adoptPostOf is the App comment that reposts p on s, login's source: its
// markers, each formatted as written, then the adopted marker naming the
// source, last (SPEC §5).
func (a *Applier) adoptPostOf(s *github.Snapshot, login string, p state.Repost) (adoptPost, error) {
	post := adoptPost{s: s, from: p.From}
	var markers []string
	for _, m := range append(slices.Clone(p.Markers), state.AdoptedMarker(p.From, login)) {
		f, err := github.FormatMarker(m)
		if err != nil {
			return adoptPost{}, fmt.Errorf("repost %s on #%d: %w", adoptSource(p.From), s.Number, err)
		}
		markers = append(markers, f)
		if m.Kind != "adopted" {
			post.kinds = append(post.kinds, m.Kind)
		}
	}
	post.body = renderAdopted(login, p.From, waitLink(a.Owner, a.Repo, s.Number, p.From), markers)
	return post, nil
}

// adoptSource names source from in a log line: "description" for the body,
// or "comment <id>".
func adoptSource(from int64) string {
	if from == state.BodyID {
		return "description"
	}
	return "comment " + strconv.FormatInt(from, 10)
}

// renderAdopted is the App comment that reposts markers, each formatted,
// the adopted marker last, from login's source from, at link: one line
// saying the App copied them from login's comment, or the description,
// written while the pipeline ran by hand, so the daemon reads them as the
// App's, then the markers, a line each. login is a code span, escaped
// (SPEC §5.1). There's no cc line, as there's nothing for anyone to do.
func renderAdopted(login string, from int64, link string, markers []string) string {
	source := "[comment](" + link + ")"
	if from == state.BodyID {
		source = "[description](" + link + ")"
	}
	return fmt.Sprintf("I've copied these markers from `%s`'s %s, written while this pipeline ran by hand, so I read them as mine.\n\n%s",
		github.Escape(login), source, strings.Join(markers, "\n"))
}
