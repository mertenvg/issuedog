package daemon

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// A finished story in the digest and in assignment (#278, plan #249's
// T6): what waitFind finds on a story at planned, how it ranks against
// the other kinds, how the digest and assignment judge it against what
// the poll listed, and how the digest renders it.

// doneRecord is the App's record of children on its issue, for plan
// revision 1, as childrenRecord writes it.
func doneRecord(children ...int) string {
	lines := []string{"I've created the children of plan rev 1:"}
	var nums []string
	for i, c := range children {
		lines = append(lines, fmt.Sprintf("- T%d #%d: Child %d", i+1, c, i+1))
		nums = append(nums, fmt.Sprint(c))
	}
	return strings.Join(lines, "\n") + "\n\n<!-- issuedog:created rev=1 children=" + strings.Join(nums, ",") + " -->"
}

// doneSnap is open issue n of type typ at planned, with priority:p1, as
// the App leaves it once it has created the children of its plan:
// comment n*100+1, the move into planning; n*100+2, revision 1 of a plan
// holding golden's plan data (internal/agent's golden planner output),
// from planning to plan-review; n*100+3, the owner's 👍 on it, the move
// into planned; and, unless children is empty, n*100+4, the App's record
// of children (#277).
func doneSnap(t *testing.T, n int, typ, golden string, children ...int) *github.Snapshot {
	t.Helper()
	m, err := planDataMarker(goldenPlan(t, golden))
	if err != nil {
		t.Fatal(err)
	}
	data, err := github.FormatMarker(m)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planRevision("### Plan for the issue (rev 1)\n\nThe plan.\n\ncc @owner\n\n<!-- issuedog:transition from=planning to=plan-review rev=1 run=p1 -->\n"+data, 1)
	if err != nil {
		t.Fatal(err)
	}
	hash, ok := state.PlanHash(plan)
	if !ok {
		t.Fatalf("no plan hash in %q", plan)
	}
	comments := []string{"<!-- issuedog:transition from=spec-review to=planning rev=1 body=aaaaaaaa cmd=r3 -->", plan,
		"<!-- issuedog:transition from=plan-review to=planned rev=1 body=" + hash + " cmd=r5 seen=2 -->"}
	if len(children) > 0 {
		comments = append(comments, doneRecord(children...))
	}
	s := waitSnap(n, []string{"stage:planned", "priority:p1"}, comments...)
	s.Type = typ
	return s
}

// doneLinesWant is the lines of a finished story whose record names
// children 12 and 13.
var doneLinesWant = []string{
	"Every child of this story is closed, so it waits for story verification, which runs by hand for now. Close it once that passes.",
	"Its children: #12, #13.",
}

// A story at planned whose record of its approved plan revision's
// children stands is one item, keyed done-<n>, linking the issue, with
// the record's children in its order, at the story's priority. It's
// found whether or not the children are closed, as waitFind can't tell:
// the digest and assignment judge that (digestDoneStands). A story from an
// epic's plan, with a parent and an origin marker, is one too.
func TestWaitFindDone(t *testing.T) {
	t.Parallel()
	s := doneSnap(t, 30, "story", "story-plan", 12, 13)
	got := waitFindOne(t, waitIn(s, Dirt{}))
	if got.Issue != 30 || got.Kind != waitDone || got.Key != "done-30" || got.Link != "https://github.com/o/r/issues/30" ||
		got.Comment != state.BodyID || got.Priority != 1 || got.Title != s.Title || !got.Created.Equal(s.CreatedAt) {
		t.Errorf("got #%d %s %s %s comment %d p%d %q %v; want #30 done done-30, the issue's link, the body, p1, the raw title and %v",
			got.Issue, got.Kind, got.Key, got.Link, got.Comment, got.Priority, got.Title, got.Created, s.CreatedAt)
	}
	if !slices.Equal(got.Children, []int{12, 13}) || got.Plan != 0 || got.Index != 0 {
		t.Errorf("got children %v, plan %d, index %d; want [12 13], 0 and 0", got.Children, got.Plan, got.Index)
	}
	if !slices.Equal(waitTexts(got.Lines), doneLinesWant) || slices.ContainsFunc(got.Lines, func(l waitLine) bool { return l.Copied }) {
		t.Errorf("lines = %+v, want %q, none copied", got.Lines, doneLinesWant)
	}
	if news := got.news(); !slices.Equal(news, []string{"done-30"}) {
		t.Errorf("news() = %q, want its key", news)
	}
	// The record's own order, not sorted.
	child := doneSnap(t, 31, "story", "story-plan", 17, 9, 12)
	child.Parent, child.Author = 40, waitApp
	child.Body = "The story.\n\n<!-- issuedog:origin plan=#40 rev=1 item=S2 -->"
	got = waitFindOne(t, waitIn(child, Dirt{}))
	if got.Kind != waitDone || got.Key != "done-31" || !slices.Equal(got.Children, []int{17, 9, 12}) || got.Lines[1].Text != "Its children: #17, #9, #12." {
		t.Errorf("a story from an epic's plan gave %s %s %v %q; want done-31 with children 17, 9 and 12", got.Kind, got.Key, got.Children, waitTexts(got.Lines))
	}
}

// An epic planned into tasks acts as its own story (SPEC §4), so it's
// listed the same way. An epic planned into stories isn't: each story is
// verified on its own.
func TestWaitFindDoneEpic(t *testing.T) {
	t.Parallel()
	got := waitFindOne(t, waitIn(doneSnap(t, 32, "epic", "story-plan", 12, 13), Dirt{}))
	if got.Kind != waitDone || got.Key != "done-32" || !slices.Equal(got.Children, []int{12, 13}) || !slices.Equal(waitTexts(got.Lines), doneLinesWant) {
		t.Errorf("an epic planned into tasks gave %+v; want done-32 with children 12 and 13", got)
	}
	if items, err := waitFind(waitIn(doneSnap(t, 33, "epic", "epic-plan", 12, 13), Dirt{})); err != nil || len(items) != 0 {
		t.Errorf("an epic planned into stories gave %+v, %v; want nothing", items, err)
	}
}

// A story at planned gives no item with no record yet, as while its
// children are being created, nor with a record of another revision, a
// dirty one or someone else's, nor with no approved revision to read the
// record for. Nor does one held or closed, nor an issue at planned whose
// type is no story's, a task or none. An epic whose approved plan has no
// data to tell what it was planned into gives none either.
func TestWaitFindDoneNothing(t *testing.T) {
	t.Parallel()
	norecord := doneSnap(t, 40, "story", "story-plan")
	otherRev := doneSnap(t, 41, "story", "story-plan")
	otherRev.Comments = append(otherRev.Comments, github.Comment{ID: 4104, Author: waitApp, Body: strings.Replace(doneRecord(12), "rev=1", "rev=2", 1)})
	dirty := doneSnap(t, 42, "story", "story-plan", 12)
	someone := doneSnap(t, 43, "story", "story-plan")
	someone.Comments = append(someone.Comments, github.Comment{ID: 4304, Author: waitOwner, Body: doneRecord(12)})
	unapproved := doneSnap(t, 44, "story", "story-plan", 12)
	unapproved.Comments[2].Body = "<!-- issuedog:transition from=plan-review to=planned cmd=r5 seen=2 -->"
	held := doneSnap(t, 45, "story", "story-plan", 12)
	held.Comments = append(held.Comments, github.Comment{ID: 4505, Author: waitApp, Body: "<!-- issuedog:transition from=planned to=held at=planned reason=owner cmd=9 -->"})
	closed := doneSnap(t, 46, "story", "story-plan", 12)
	closed.Closed = true
	task := doneSnap(t, 47, "task", "story-plan", 12)
	untyped := doneSnap(t, 48, "", "story-plan", 12)
	nodata := doneSnap(t, 49, "epic", "story-plan", 12)
	nodata.Comments[1].Body = waitPlanComment(t, 1)
	nodata.Comments[2].Body = "<!-- issuedog:transition from=plan-review to=planned rev=1 body=" + waitPlanHash(t, 1) + " cmd=r5 seen=2 -->"
	for name, in := range map[string]waitInput{
		"no record":                 waitIn(norecord, Dirt{}),
		"another revision's":        waitIn(otherRev, Dirt{}),
		"a dirty record":            waitIn(dirty, Dirt{4204: "mallory"}),
		"someone else's record":     waitIn(someone, Dirt{}),
		"no approved revision":      waitIn(unapproved, Dirt{}),
		"held":                      waitIn(held, Dirt{}),
		"closed":                    waitIn(closed, Dirt{}),
		"a task":                    waitIn(task, Dirt{}),
		"untyped":                   waitIn(untyped, Dirt{}),
		"an epic with no plan data": waitIn(nodata, Dirt{}),
	} {
		items, err := waitFind(in)
		if err != nil || len(items) != 0 {
			t.Errorf("%s: got %+v, %v; want nothing", name, items, err)
		}
	}
}

// An epic's plan data that doesn't read back is waitFind's error, naming
// the issue, and Iterate keeps the items the issue had (digestCacheSet).
func TestWaitFindDoneBadData(t *testing.T) {
	t.Parallel()
	s := doneSnap(t, 50, "epic", "story-plan", 12)
	m := github.Marker{Kind: "plan-data", Fields: map[string]string{"v": "1", "data": "not-base64!"}}
	data, err := github.FormatMarker(m)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planRevision("### Plan\n\n<!-- issuedog:transition from=planning to=plan-review rev=1 run=p1 -->\n"+data, 1)
	if err != nil {
		t.Fatal(err)
	}
	hash, ok := state.PlanHash(plan)
	if !ok {
		t.Fatal("no plan hash")
	}
	s.Comments[1].Body = plan
	s.Comments[2].Body = "<!-- issuedog:transition from=plan-review to=planned rev=1 body=" + hash + " cmd=r5 seen=2 -->"
	items, err := waitFind(waitIn(s, Dirt{}))
	if err == nil || len(items) != 0 || !strings.Contains(err.Error(), "o/r#50") || !strings.Contains(err.Error(), "a finished epic's plan") {
		t.Errorf("got %+v, %v; want an error naming o/r#50 and the epic's plan", items, err)
	}
	// A story's data isn't read: it's a story whatever its plan holds.
	s.Type = "story"
	if got := waitFindOne(t, waitIn(s, Dirt{})); got.Key != "done-50" {
		t.Errorf("a story with bad plan data gave %s; want done-50", got.Key)
	}
}

// doneStoryDaemon is a daemon whose cache holds a finished story's item on
// issue 30, with children 12 and 13, and an error on issue 31, with snaps
// for both, and the closed issues listed, their closes judged and
// accepted (closingSeen).
func doneStoryDaemon(closed ...int) *Daemon {
	listed, seen := map[int]bool{}, map[int]closingListed{}
	for _, n := range closed {
		listed[n], seen[n] = true, closingListed{}
	}
	return &Daemon{
		digestCache: map[int][]waitItem{
			30: {{Issue: 30, Kind: waitDone, Key: "done-30", Children: []int{12, 13}, Priority: 2, Created: waitT0}},
			31: {{Issue: 31, Kind: waitError, Key: "err-31-labels-0", Priority: 2, Created: waitT0, Comment: 1}},
		},
		snaps:        map[int]*github.Snapshot{30: {Number: 30}, 31: {Number: 31}},
		closedListed: listed,
		closingSeen:  seen,
	}
}

// doneKeys is the keys of items.
func doneKeys(items []waitItem) []string {
	var keys []string
	for _, it := range items {
		keys = append(keys, it.Key)
	}
	return keys
}

// The digest and assignment keep a finished story's item only when every
// child its record names is one the poll last listed closed, and no open
// snapshot names the story as its parent: a child not listed closed, such
// as one listed open, one filed this pass and not listed yet, or one
// whose read failed, keeps it out, and so does one listed closed whose
// close wasn't judged and accepted (closingSeen), or is due to be judged
// again (closingDue), and an open issue below the story that the record
// doesn't name, such as one added by hand. A snapshot below it read as
// closed, which the next poll drops, doesn't.
// Every other item stands whatever the snapshots hold.
func TestDigestDoneStands(t *testing.T) {
	t.Parallel()
	below := func(n int, closed bool) func(d *Daemon) {
		return func(d *Daemon) { d.snaps[n] = &github.Snapshot{Number: n, Parent: 30, Closed: closed} }
	}
	for _, tc := range []struct {
		name   string
		closed []int
		more   func(d *Daemon)
		want   bool
	}{
		{"every child closed", []int{12, 13}, nil, true},
		{"and other issues closed", []int{12, 13, 99}, nil, true},
		{"one child not listed closed", []int{12}, nil, false},
		{"none listed closed", nil, nil, false},
		{"one child's close not judged", []int{12, 13}, func(d *Daemon) { delete(d.closingSeen, 13) }, false},
		{"one child's close due again", []int{12, 13}, func(d *Daemon) { d.closingDue = map[int]*gogithub.Issue{13: {}} }, false},
		{"a named child open below it", []int{12}, below(13, false), false},
		{"an open issue below it the record doesn't name", []int{12, 13}, below(14, false), false},
		{"an issue below it read as closed", []int{12, 13}, below(14, true), true},
		{"an open issue below another", []int{12, 13}, func(d *Daemon) { d.snaps[15] = &github.Snapshot{Number: 15, Parent: 31} }, true},
	} {
		d := doneStoryDaemon(tc.closed...)
		if tc.more != nil {
			tc.more(d)
		}
		want := []string{"err-31-labels-0"}
		if tc.want {
			want = []string{"err-31-labels-0", "done-30"}
		}
		if got := doneKeys(d.digestCacheRanked()); !slices.Equal(got, want) {
			t.Errorf("%s: the digest lists %q; want %q", tc.name, got, want)
		}
		if got := len(d.digestCacheOf(30)) > 0; got != tc.want {
			t.Errorf("%s: assignment sees an item on #30: %v; want %v", tc.name, got, tc.want)
		}
		if got := doneKeys(d.digestCacheOf(31)); !slices.Equal(got, []string{"err-31-labels-0"}) {
			t.Errorf("%s: #31's items are %q; want its error", tc.name, got)
		}
		if got := len(d.digestCache[30]); got != 1 {
			t.Errorf("%s: the cache holds %d items on #30 after; want its 1 kept, to be judged again next pass", tc.name, got)
		}
	}
}

// The poll keeps which issues it last listed closed: each issue listed
// closed is added, and each listed open dropped, as a reopened one is.
// A pull request never counts, nor does a digest listed open. A pass
// with nothing changed keeps them, and a new daemon's first poll, which
// lists every issue, finds the same.
func TestPollClosedListed(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	open := f.AddIssue("owner", "Open", "")
	closed := f.AddIssue("owner", "Closed", "")
	f.Close(closed)
	pr := f.AddIssue("owner", "A pull request", "")
	f.SetPullRequest(pr)
	f.Close(pr)
	digest := f.AddIssue("issuedog[bot]", "issuedog digest", "<!-- issuedog:digest -->")
	newDaemon := func() *Daemon {
		return &Daemon{Step: &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}}, stale: map[int]bool{}}
	}
	d := newDaemon()
	poll := func(d *Daemon) {
		t.Helper()
		if err := d.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	listed := func(d *Daemon) []int { return slices.Sorted(maps.Keys(d.closedListed)) }
	poll(d)
	if got := listed(d); !slices.Equal(got, []int{closed}) {
		t.Errorf("after the first poll, closed listed = %v; want [%d]", got, closed)
	}
	poll(d)
	if got := listed(d); !slices.Equal(got, []int{closed}) {
		t.Errorf("after a poll with nothing changed, closed listed = %v; want [%d]", got, closed)
	}
	f.Close(open)
	f.Reopen(closed)
	poll(d)
	if got := listed(d); !slices.Equal(got, []int{open}) {
		t.Errorf("after a close and a reopen, closed listed = %v; want [%d]", got, open)
	}
	f.Close(digest)
	poll(d)
	if got := listed(d); !slices.Equal(got, []int{open, digest}) {
		t.Errorf("after the digest's close, closed listed = %v; want [%d %d]", got, open, digest)
	}
	if got := listed(newDaemon()); len(got) != 0 {
		t.Errorf("a new daemon has %v listed before it polls; want none", got)
	}
	fresh := newDaemon()
	poll(fresh)
	if got := listed(fresh); !slices.Equal(got, []int{open, digest}) {
		t.Errorf("a new daemon's first poll listed %v closed; want [%d %d]", got, open, digest)
	}
}

// Q3a's order of kinds puts a finished story after every other kind,
// a flag and a human task included, and a kind not mapped after it.
func TestWaitDoneRank(t *testing.T) {
	t.Parallel()
	for _, k := range []waitKind{waitError, waitHold, waitApproval, waitPlanApproval, waitQuestion, waitHuman, waitFlag} {
		if k.rank() >= waitDone.rank() {
			t.Errorf("%s ranks %d, a finished story %d; want %s first", k, k.rank(), waitDone.rank(), k)
		}
	}
	for _, k := range []waitKind{waitDone + 1, waitKind(9), waitKind(20)} {
		if k.rank() <= waitDone.rank() {
			t.Errorf("%s ranks %d, a finished story %d; want the finished story first", k, k.rank(), waitDone.rank())
		}
	}
	if waitDone.String() != "done" || waitKeyPrefix[waitDone] != "done" {
		t.Errorf("a finished story's name is %q and its key prefix %q; want done and done", waitDone, waitKeyPrefix[waitDone])
	}
}

// doneRankItems are the items the finished story's ranking golden ranks,
// in the order it gives them: one of each kind, each unblocking 1 but
// 70's, with priorities that would rank them the other way round.
func doneRankItems() []waitItem {
	return []waitItem{
		{Issue: 60, Kind: waitDone, Key: "done-60", Priority: 0, Created: waitT0},
		{Issue: 61, Kind: waitFlag, Key: "flag-61-1", Priority: 0, Created: waitT0, Comment: 1},
		{Issue: 62, Kind: waitKind(9), Key: "kind9-62-1", Priority: 0, Created: waitT0, Comment: 1},
		{Issue: 63, Kind: waitHuman, Key: "human-63", Priority: 3, Created: waitT0},
		{Issue: 64, Kind: waitDone, Key: "done-64", Priority: 1, Created: waitT0},
		{Issue: 65, Kind: waitQuestion, Key: "q-65-1", Priority: 3, Created: waitT0, Comment: 1},
		{Issue: 66, Kind: waitPlanApproval, Key: "plan-66-1", Priority: 3, Created: waitT0, Comment: 1},
		{Issue: 67, Kind: waitApproval, Key: "rev-67-1", Priority: 3, Created: waitT0, Comment: 1},
		{Issue: 68, Kind: waitHold, Key: "hold-68-c1", Priority: 3, Created: waitT0},
		{Issue: 69, Kind: waitError, Key: "err-69-labels-0", Priority: 3, Created: waitT0, Comment: 1},
		{Issue: 70, Kind: waitDone, Key: "done-70", Priority: 3, Created: waitT0}, // unblocks 2: first, whatever its kind
	}
}

// doneRankSnaps are the issues that golden holds: 71, open, is below 70.
func doneRankSnaps() map[int]*github.Snapshot {
	snaps := map[int]*github.Snapshot{71: {Number: 71, Parent: 70}}
	for n := 60; n <= 70; n++ {
		snaps[n] = &github.Snapshot{Number: n}
	}
	return snaps
}

// A finished story ranks against the other kinds by Q3a, after a flag and
// before a kind not mapped, and any order of the same items ranks the
// same. Only the unblock count ranks one higher, which the digest never
// shows, as an open issue below a story keeps it out (digestDoneStands).
func TestWaitDoneRankGolden(t *testing.T) {
	t.Parallel()
	items := doneRankItems()
	waitRank(items, doneRankSnaps())
	waitGolden(t, "done-rank", waitRankText(items))
	reversed := doneRankItems()
	slices.Reverse(reversed)
	waitRank(reversed, doneRankSnaps())
	if got, want := waitRankText(reversed), waitRankText(items); got != want {
		t.Errorf("reversed input ranks\n%s\nwant\n%s", got, want)
	}
}

// The digest's goldens: a finished story found on its snapshot, whose
// title tries a mention and HTML, ranked after a flag, and the comment
// that announces it, as no comment of its own mentions notify. Neither
// the body nor the announcement mentions anyone but in its cc line, and
// the announcement's marker names done-<n>. Once announced, it isn't new.
func TestWaitDoneDigestGolden(t *testing.T) {
	t.Parallel()
	flag := triageOutput{Verdict: triageHandle, Reason: "it's a bug", Summary: "Crash on save"}
	snaps := map[int]*github.Snapshot{
		4:  waitSnap(4, []string{"triage:ask", "priority:p3"}, "Triage ran.", join(triageFlagText(flag, 4, []string{"owner"}), waitFlagMarker("handle"))),
		50: doneSnap(t, 50, "story", "story-plan", 12, 13),
	}
	snaps[50].Title = "Quotes expire @eve <b>[x](https://evil.example)</b>"
	var items []waitItem
	for _, n := range []int{50, 4} {
		found, err := waitFind(waitIn(snaps[n], Dirt{}))
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, found...)
	}
	waitRank(items, snaps)
	if got, want := doneKeys(items), []string{"flag-4-402", "done-50"}; !slices.Equal(got, want) {
		t.Fatalf("ranked %q, want %q", got, want)
	}
	body := digestRenderBody("", items)
	digestRenderGolden(t, "done", body)
	digestRenderCheck(t, body)
	if !strings.Contains(body, "**Finished story**, priority p1, unblocks 1 open issue (itself included)") {
		t.Errorf("the body doesn't name the finished story's kind:\n%s", body)
	}
	done := items[1]
	if !digestRenderMentions(done) {
		t.Error("digestRenderMentions = false for a finished story; want the digest's mention")
	}
	news, err := digestRenderNews(digestRenderNew([]waitItem{done}, nil), []string{"owner"})
	if err != nil {
		t.Fatal(err)
	}
	digestRenderGolden(t, "done-news", news)
	if ms := github.ParseMarkers(news); len(ms) != 1 || ms[0].Kind != "digest-new" || ms[0].Fields["items"] != "done-50" {
		t.Errorf("the announcement's markers are %+v; want digest-new with items=done-50", ms)
	}
	if fresh := digestRenderNew([]waitItem{done}, map[string]bool{"done-50": true}); len(fresh) != 0 {
		t.Errorf("digestRenderNew = %+v once done-50 is announced; want nothing new", fresh)
	}
	if digestRenderKindName(waitDone) != "Finished story" {
		t.Errorf("the kind's name is %q; want Finished story", digestRenderKindName(waitDone))
	}
}

// The finished story's item, cached from the story's last visit, comes
// and goes with what each poll lists, with no visit of the story: the
// filter reads only the poll's list and the snapshots (digestDoneStands).
func TestDigestDoneNoVisit(t *testing.T) {
	t.Parallel()
	d := doneStoryDaemon(12)
	if got := doneKeys(d.digestCacheRanked()); slices.Contains(got, "done-30") {
		t.Fatalf("with 13 open, the digest lists %q; want no done-30", got)
	}
	d.closedListed[13], d.closingSeen[13] = true, closingListed{}
	if got := doneKeys(d.digestCacheRanked()); !slices.Contains(got, "done-30") {
		t.Errorf("with 13 listed closed, the digest lists %q; want done-30", got)
	}
	delete(d.closedListed, 12)
	if got := doneKeys(d.digestCacheRanked()); slices.Contains(got, "done-30") {
		t.Errorf("with 12 listed open again, the digest lists %q; want no done-30", got)
	}
}
