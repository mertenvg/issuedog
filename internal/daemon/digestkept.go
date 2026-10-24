package daemon

import (
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// An issue whose read or step 1 fails keeps its last items (digestCacheSet)
// and its assignees (assignPass), so the digest and assignment agree on it
// that pass. On a start there are no last items: the cache is the last
// daemon's, and went with it, while the assignees are on GitHub. So an
// issue whose read or visit fails on a restart's first pass would drop
// out of the digest for that pass, an edit, while it stays assigned, and
// come back on the next good visit, another edit (#360). So the start
// reads the items back from the digest's own body, where the last daemon
// rendered them (SPEC §9.4, Keeping the list): each item there whose issue
// the digest may list items for that aren't cached
// (Daemon.digestRestartUnknown) is kept as the body showed it
// (waitItem.Kept), until step 1 succeeds on the issue, which caches its
// own, or the issue closes, is dropped as gone or is taken as a digest.
// The same goes for an issue that skips the pass with nothing cached, as
// one a failed visit wrote to, or whose read again failed, or whose
// labels are held back (digestrestart.go).
//
// The body is read back only if the App wrote it as listed: the start
// reads the digest's edit history anyway (digestTake), and the body must
// be its original post, never edited, or the text of its newest edit, the
// App's (digestKeptApp). So nobody else's text is ever written back as
// the App's. An item is read back only in the form digestRenderItem
// writes it, with its kind and priority read from the line under its
// heading, and the rest copied as it was: anything else in the body is
// passed over. An item cut from the body to fit GitHub's limit isn't
// there to read back.
//
// A kept item ranks as any other, by the snapshots held (waitRank): its
// issue's created time and the open issues it blocks come from its
// snapshot, if there is one, and its comment from its link. With no
// snapshot held, as when the issue's read failed, they come from the last
// listing that had the issue (digestKeptList): the start's first poll,
// which lists every issue, a later poll, or the 10-minute listing of every
// open issue, each of which gives an issue's created_at and its
// issue_dependencies_summary with no request of its own. So the item
// ranks where its own was, and the digest's body isn't edited for it. It
// has no key, so it's never new (waitItem.news):
// the body listed it already. Assignment never takes one: it judges only
// the issues step 1 succeeded on this pass, and step 1's success drops
// the issue's kept items.

// waitKept is what the digest's body showed of an item read back from it
// (digestKeptParse): its heading's title and the lines under it, but for
// the link, each as rendered. Blocking is how many open issues its issue
// blocks, as the last listing of it showed (digestKeptListing), which
// waitRank takes while no snapshot of the issue is held.
type waitKept struct {
	Title    string
	Lines    []string
	Blocking int
}

// digestKeptListing is what a listing showed of an issue the digest may
// list items for that aren't cached (Daemon.digestRestartUnknown): when
// it was opened, and how many open issues it blocks, the blocking of its
// issue_dependencies_summary (SPEC §9.4, The order).
type digestKeptListing struct {
	Created  time.Time
	Blocking int
}

// The heading of an item, and the line under it with its kind, priority
// and score, as digestRenderItem writes them.
var (
	digestKeptHeadingRE = regexp.MustCompile(`^### [1-9][0-9]*\. #([1-9][0-9]*)(?:: (.+))?$`)
	digestKeptAboutRE   = regexp.MustCompile(`^\*\*([^*]+)\*\*, priority p([0-3])(?:, unblocks [1-9][0-9]* open issues? \(itself included\))?$`)
	digestKeptCommentRE = regexp.MustCompile(`#issuecomment-([1-9][0-9]*)$`)
)

// digestKeptAnswer starts an item's last line, its link.
const digestKeptAnswer = "- Answer here: "

// digestKeptApp reports whether body, an App issue's body as listed, is a
// version the App wrote, from h, its edit history, oldest first: with no
// edit, h is empty and body is the original post, which the App filed;
// otherwise body is the text of the newest entry, which must be the
// App's, or the original post if it's the only one (digestMarkedHistory).
func digestKeptApp(h []github.Edit, body, appSlug string) bool {
	if len(h) == 0 {
		return true
	}
	last := h[len(h)-1]
	return (len(h) == 1 || state.IsApp(last.By, appSlug)) && last.Text != "" && digestSameBody(last.Text, body)
}

// digestKeptParse reads the items back from body, a digest's body the App
// wrote (digestRenderBody), by issue, in the body's order. Each is a
// heading, the line with its kind and priority, and its lines, the last
// of which is its link, as digestRenderItem writes them. Anything else is
// passed over: the pin note, the count, the line on items cut to fit and
// the identity marker, and an item in any other form.
func digestKeptParse(body string) map[int][]waitItem {
	parts := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n")
	kept := map[int][]waitItem{}
	for i := 0; i+2 < len(parts); i++ {
		it, ok := digestKeptItem(parts[i], parts[i+1], parts[i+2])
		if !ok {
			continue
		}
		kept[it.Issue] = append(kept[it.Issue], it)
		i += 2
	}
	return kept
}

// digestKeptItem reads one item back from its heading, the line under it
// and its lines, or reports that they aren't one.
func digestKeptItem(heading, about, list string) (waitItem, bool) {
	h, a := digestKeptHeadingRE.FindStringSubmatch(heading), digestKeptAboutRE.FindStringSubmatch(about)
	if h == nil || a == nil {
		return waitItem{}, false
	}
	n, err := strconv.Atoi(h[1])
	if err != nil { // more digits than an int holds
		return waitItem{}, false
	}
	kind, ok := digestKeptKind(a[1])
	if !ok {
		return waitItem{}, false
	}
	lines := strings.Split(list, "\n")
	link, ok := strings.CutPrefix(lines[len(lines)-1], digestKeptAnswer)
	if !ok || link == "" || slices.ContainsFunc(lines, func(l string) bool { return !strings.HasPrefix(l, "- ") && !strings.HasPrefix(l, "  - ") }) {
		return waitItem{}, false
	}
	comment := state.BodyID
	if m := digestKeptCommentRE.FindStringSubmatch(link); m != nil {
		if comment, err = strconv.ParseInt(m[1], 10, 64); err != nil {
			comment = state.BodyID
		}
	}
	return waitItem{Issue: n, Kind: kind, Priority: int(a[2][0] - '0'), Link: link, Comment: comment,
		Kept: &waitKept{Title: h[2], Lines: lines[:len(lines)-1]}}, true
}

// digestKeptKind is the kind digestRenderKindName names name, if any.
func digestKeptKind(name string) (waitKind, bool) {
	for k := waitError; k <= waitDone; k++ {
		if digestRenderKindName(k) == name {
			return k, true
		}
	}
	return 0, false
}

// digestKeptRead reads the items back from body, digest n's as listed,
// which the App wrote (digestKeptApp), if n is the digest, the lowest of
// those found, for each issue whose items the digest may list but aren't
// cached (Daemon.digestRestartUnknown). Those it had read back from
// another digest go.
func (d *Daemon) digestKeptRead(n int, body string) {
	if slices.Min(slices.Collect(maps.Keys(d.digest.issues))) != n {
		return
	}
	d.digestKept = nil
	for m, items := range digestKeptParse(body) {
		if !d.digestRestartUnknown[m] {
			continue
		}
		if d.digestKept == nil {
			d.digestKept = map[int][]waitItem{}
		}
		d.digestKept[m] = items
	}
}

// digestKeptRanked returns the items read back for each issue, which has
// none cached, as step 1's success drops them (digestRestartKnow), each
// with its issue's created time, for waitRank: from its snapshot, if one
// is held, which waitRank takes the open issues it blocks from too, or
// else from the last listing that had it (digestKeptList), with the open
// issues it blocks then.
func (d *Daemon) digestKeptRanked() []waitItem {
	var items []waitItem
	for _, n := range slices.Sorted(maps.Keys(d.digestKept)) {
		for _, it := range d.digestKept[n] {
			if s := d.snaps[n]; s != nil {
				it.Created = s.CreatedAt
			} else if l, ok := d.digestKeptListed[n]; ok {
				k := *it.Kept
				it.Created, k.Blocking, it.Kept = l.Created, l.Blocking, &k
			}
			items = append(items, it)
		}
	}
	return items
}

// digestKeptList records what a listing, a poll's or the 10-minute one of
// every open issue (transferUnlistedCheck), showed of open issue is, if
// the digest may list items for it that aren't cached
// (Daemon.digestRestartUnknown), for its kept items' rank while no
// snapshot of it is held (digestKeptRanked). The listing gives both with
// no request of its own.
func (d *Daemon) digestKeptList(is *gogithub.Issue) {
	n := is.GetNumber()
	if !d.digestRestartUnknown[n] {
		return
	}
	if d.digestKeptListed == nil {
		d.digestKeptListed = map[int]digestKeptListing{}
	}
	d.digestKeptListed[n] = digestKeptListing{Created: is.GetCreatedAt().Time, Blocking: is.GetIssueDependenciesSummary().GetBlocking()}
}
