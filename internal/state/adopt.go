package state

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/mertenvg/issuedog/internal/github"
)

// AdoptKinds are the kinds of marker an adoption reposts (SPEC §5, the
// adopted marker): the ones a person wrote on an issue run by hand that
// the daemon reads as state. Any other kind, such as verify, stays where
// it is and is never read.
var AdoptKinds = []string{"origin", "transition", "revision", "created"}

// adoptedFromBody is the adopted marker's from= for the issue body.
const adoptedFromBody = "body"

// Repost is one App comment an adoption posts: the markers of one source,
// a body or comment the login wrote, each with every field as written but
// for an added seen (Adopt). The comment ends with AdoptedMarker(From,
// login), which Markers doesn't hold.
type Repost struct {
	// From is the source's comment ID, or BodyID for the body.
	From int64
	// Markers are the source's markers of AdoptKinds, in the order it has
	// them.
	Markers []github.Marker
}

// Adoption is what Adopt finds on one issue for one login.
type Adoption struct {
	// Posts are the sources left to repost, in the issue's order: the body,
	// then each comment.
	Posts []Repost
	// Done are the sources a clean App comment's adopted marker names, in
	// the same order: they're reposted already, so they're never again.
	Done []int64
	// Refused says why the issue must be fixed before anything is posted,
	// or is empty. It's set only while Posts isn't empty, so once every
	// source is done, nothing is refused.
	Refused string
}

// Adopt returns which of login's markers on s the App reposts, so that s
// reads as it would if they were the App's (SPEC §5, the adopted marker).
// A source is the body, while login wrote it and made its last edit or
// nobody edited it, then each comment login wrote, any case, that isn't a
// Bot's; dirty content, by comment ID or BodyID as for Derive, is never one.
// Each source's markers are read by github.ParseMarkers, as the caller has
// chosen login's text, as OriginMarker's caller has the App's: login must
// be a trusted user (SPEC §6.1), which Adopt doesn't check. Of those, it
// takes the AdoptKinds, every field as written, and leaves the rest.
//
// A transition with no seen gets seen=<the source comment's ID>: the bound
// it set where it was (RoundBound), so a comment between it and its repost
// is still input after it. So does a revision in a source with no
// transition, as one posted without a move carries seen (SPEC §5). A seen
// already there is kept as it is. The body comes before every comment, and
// has no ID to bound by, so a body's markers get none.
//
// A source named by a clean App comment's adopted marker is Done, and not
// posted again. A source with none of the AdoptKinds isn't one.
//
// While Posts isn't empty, Refused says why the issue must be fixed first,
// each reason that holds, joined by "; ":
//   - it hasn't exactly one stage:* label;
//   - its stage is plan-review, whose plan's hash covers its whole
//     comment, which a repost doesn't copy;
//   - its stage is in-progress, verifying or pr-open, as a run by hand
//     leaves no head, PR or check the daemon could carry on from;
//   - its stage isn't the one its latest transition sets once the posts
//     are made, so the label rules would report a disagreement (SPEC §5.1):
//     the last post's latest, or with none in the posts, the latest in a
//     clean App comment, such as a done source's repost. A post comes after
//     every App comment, so one of an earlier source than a done one's
//     comes after it, which may set an older stage;
//   - a clean App comment holds a transition with no adopted marker beside
//     it, as the daemon has run on the issue.
func Adopt(s *github.Snapshot, login, appSlug string, dirty func(int64) bool) Adoption {
	if login == "" {
		return Adoption{}
	}
	done := map[int64]bool{}
	var appMoves []int64   // clean App comments with a transition and no adopted marker
	var last github.Marker // the latest transition that sets a stage, once the posts are made
	for _, c := range s.Comments {
		if dirty(c.ID) || !IsApp(c.Author, appSlug) {
			continue
		}
		moved, adopted := false, false
		for _, m := range github.TrustedMarkers(c.Author, c.Body, appSlug) {
			if from, ok := adoptedOf(m); ok {
				done[from], adopted = true, true
			}
			if _, ok := RoundBound(m, c.ID); ok {
				last = m
			}
			moved = moved || m.Kind == "transition"
		}
		if moved && !adopted {
			appMoves = append(appMoves, c.ID)
		}
	}
	var a Adoption
	source := func(id int64, author github.Author, body string) {
		if dirty(id) || author.Type == "Bot" || !strings.EqualFold(author.Login, login) {
			return
		}
		r := adoptRepost(id, body)
		switch {
		case len(r.Markers) == 0:
		case done[id]:
			a.Done = append(a.Done, id)
		default:
			for _, m := range r.Markers {
				if _, ok := RoundBound(m, id); ok {
					last = m
				}
			}
			a.Posts = append(a.Posts, r)
		}
	}
	if e := s.BodyEdit; e.At.IsZero() || e.By.Type != "Bot" && strings.EqualFold(e.By.Login, login) {
		source(BodyID, s.Author, s.Body)
	}
	for _, c := range s.Comments {
		source(c.ID, c.Author, c.Body)
	}
	if len(a.Posts) > 0 {
		a.Refused = adoptRefused(s.Labels, last, appMoves)
	}
	return a
}

// adoptRepost returns the repost of source id's text body: its markers of
// AdoptKinds, with seen added as Adopt says.
func adoptRepost(id int64, body string) Repost {
	r := Repost{From: id}
	moved := false
	for _, m := range github.ParseMarkers(body) {
		if slices.Contains(AdoptKinds, m.Kind) {
			r.Markers = append(r.Markers, m)
			moved = moved || m.Kind == "transition"
		}
	}
	if id == BodyID {
		return r
	}
	for _, m := range r.Markers {
		if _, ok := m.Fields[FieldSeen]; ok {
			continue
		}
		if m.Kind == "transition" || m.Kind == "revision" && !moved {
			m.Fields[FieldSeen] = strconv.FormatInt(id, 10)
		}
	}
	return r
}

// adoptRefused returns why an issue with labels, whose latest transition
// once the posts are made is last, or the zero Marker, and whose clean App
// comments appMoves hold transitions adopt didn't write, can't be adopted,
// or "".
func adoptRefused(labels []string, last github.Marker, appMoves []int64) string {
	var why []string
	var stages []Stage
	for _, l := range labels {
		if st, ok := ParseLabel(l); ok {
			stages = append(stages, st)
		}
	}
	if len(stages) != 1 {
		why = append(why, fmt.Sprintf("it has %d stage labels, not one", len(stages)))
	} else {
		label := labelPrefix + string(stages[0])
		switch stages[0] {
		case PlanReview:
			why = append(why, label+": a plan's hash covers its whole comment, which a repost doesn't copy")
		case InProgress, Verifying, PROpen:
			why = append(why, label+": a run by hand left no head, PR or check the daemon could carry on from")
		}
		if to := Stage(last.Fields["to"]); last.Kind != "" && to != stages[0] {
			why = append(why, fmt.Sprintf("%s isn't %s, the stage its latest transition sets once reposted, so the label rules would report a disagreement", label, to))
		}
	}
	for _, id := range appMoves {
		why = append(why, fmt.Sprintf("the App's transition in comment %d isn't adopt's: the daemon has run on this issue", id))
	}
	return strings.Join(why, "; ")
}

// AdoptedMarker returns the marker that ends an adoption's App comment
// (SPEC §5): adopted from=<the source's comment ID, or body> login=<login>,
// the login percent-encoded as EncodeValue does, which leaves a GitHub
// login as it is.
func AdoptedMarker(from int64, login string) github.Marker {
	f := adoptedFromBody
	if from != BodyID {
		f = strconv.FormatInt(from, 10)
	}
	return github.Marker{Kind: "adopted", Fields: map[string]string{"from": f, "login": github.EncodeValue(login)}}
}

// adoptedOf returns the source an adopted marker names: BodyID for
// from=body, or a comment ID above 0. It's false for any other marker, or
// any other from.
func adoptedOf(m github.Marker) (int64, bool) {
	if m.Kind != "adopted" {
		return 0, false
	}
	from := m.Fields["from"]
	if from == adoptedFromBody {
		return BodyID, true
	}
	id, err := strconv.ParseInt(from, 10, 64)
	return id, err == nil && id > 0
}
