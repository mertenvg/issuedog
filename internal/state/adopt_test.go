package state

import (
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
)

// adoptMarkerRE finds the start of each marker, with its kind.
var adoptMarkerRE = regexp.MustCompile(`<!--(\s*issuedog:([a-z][a-z0-9-]*))`)

// adoptKindsOnly is text with every marker of a kind Adopt doesn't take
// escaped, so that only the AdoptKinds are read from it.
func adoptKindsOnly(text string) string {
	return adoptMarkerRE.ReplaceAllStringFunc(text, func(m string) string {
		if slices.Contains(AdoptKinds, adoptMarkerRE.FindStringSubmatch(m)[2]) {
			return m
		}
		return "&lt;!--" + strings.TrimPrefix(m, "<!--")
	})
}

// asApp is s with login's clean sources re-authored as the App's: the body,
// with its last edit, while login wrote it and made its last edit or nobody
// did, and each comment login wrote, any case, that isn't a Bot's. That's
// the reading an adoption promises (SPEC §5, the adopted marker): login's
// markers read as the App's. It promises it for the AdoptKinds alone, so a
// marker of any other kind in them is escaped. An App comment with an
// adopted marker, an earlier adoption's repost, is left out: its source
// is read in its place, as it was before any adoption. dirty is left to
// the reader, which skips the same content either way.
func asApp(s *github.Snapshot, login, appSlug string) *github.Snapshot {
	app := github.Author{Login: appSlug + "[bot]", Type: "Bot"}
	mine := func(a github.Author) bool { return a.Type != "Bot" && strings.EqualFold(a.Login, login) }
	c := *s
	c.Comments = nil
	if e := s.BodyEdit; mine(s.Author) && (e.At.IsZero() || mine(e.By)) {
		c.Author, c.Body = app, adoptKindsOnly(s.Body)
		if !e.At.IsZero() {
			c.BodyEdit.By = app
		}
	}
	for _, cm := range s.Comments {
		reposted := slices.ContainsFunc(github.TrustedMarkers(cm.Author, cm.Body, appSlug), func(m github.Marker) bool {
			return m.Kind == "adopted"
		})
		switch {
		case reposted:
			continue
		case mine(cm.Author):
			cm.Author, cm.Body = app, adoptKindsOnly(cm.Body)
		}
		c.Comments = append(c.Comments, cm)
	}
	return &c
}

// withReposts is s with a's posts appended as clean App comments, each
// ending with its adopted marker, as an adoption posts them, with IDs
// above every comment's in s.
func withReposts(t *testing.T, s *github.Snapshot, a Adoption, login string) *github.Snapshot {
	t.Helper()
	c := *s
	c.Comments = slices.Clone(s.Comments)
	next := int64(10000)
	for _, cm := range s.Comments {
		next = max(next, cm.ID+1)
	}
	for _, p := range a.Posts {
		var b strings.Builder
		b.WriteString("Reposted from " + login + "'s source.\n")
		for _, m := range append(slices.Clone(p.Markers), AdoptedMarker(p.From, login)) {
			f, err := github.FormatMarker(m)
			if err != nil {
				t.Fatalf("format repost of %d: %v", p.From, err)
			}
			b.WriteString(f + "\n")
		}
		c.Comments = append(c.Comments, github.Comment{ID: next, Author: appBot, Body: b.String()})
		next++
	}
	return &c
}

// adoptReading is what an adoption must leave as it was (SPEC §5, the
// adopted marker): Derive's stage, round bound, keys, origin, item and so
// class, a hold's at and origin, and the created records. Origin.Comment
// is left out: a repost's ID is never its source's.
type adoptReading struct {
	Stage      Stage
	Round      int64
	Keys       map[Key]bool
	Origin     Origin
	Item       Item
	Class      Class
	ClassErr   string
	HeldAt     Stage
	HeldOrigin Stage
	Created    map[int][]int
}

func readForAdopt(s *github.Snapshot, dirty func(int64) bool) adoptReading {
	is := Derive(s, slug, dirty)
	r := adoptReading{Stage: is.Stage, Round: is.Round, Keys: is.Keys, Origin: is.Origin, Item: is.Item,
		HeldAt: is.HeldStack.At, HeldOrigin: is.HeldStack.Origin, Created: map[int][]int{}}
	r.Origin.Comment = 0
	class, err := ClassOf(is.Item)
	r.Class = class
	if err != nil {
		r.ClassErr = err.Error()
	}
	for rev := 1; rev <= 3; rev++ {
		if children, ok := CreatedRecord(is, rev); ok {
			r.Created[rev] = children
		}
	}
	return r
}

func adoptComment(id int64, by github.Author, body string) github.Comment {
	return github.Comment{ID: id, Author: by, Body: body}
}

// adoptPosts writes a's posts as "<from>:" and each marker, formatted, so a
// case states them as the App would post them.
func adoptPosts(t *testing.T, a Adoption) []string {
	t.Helper()
	var out []string
	for _, p := range a.Posts {
		s := strconv.FormatInt(p.From, 10) + ":"
		for _, m := range p.Markers {
			f, err := github.FormatMarker(m)
			if err != nil {
				t.Fatalf("format marker of %d: %v", p.From, err)
			}
			s += " " + f
		}
		out = append(out, s)
	}
	return out
}

const adoptLogin = "owner"

var (
	adoptOwnerCased = github.Author{Login: "Owner", Type: "User"}
	adoptOwnerBot   = github.Author{Login: "owner", Type: "Bot"}
	adoptEditAt     = time.Unix(50, 0)
)

func adoptMove(fields string) string { return "<!-- issuedog:transition " + fields + " -->" }

type adoptCase struct {
	name    string
	s       *github.Snapshot
	dirty   func(int64) bool
	posts   []string
	done    []int64
	refused []string // each must be in Refused; none means Refused is ""
}

func adoptCases() []adoptCase {
	planText := "## Plan\n- S1 the first\n" + adoptMove("from=planning to=plan-review run=p1 rev=1") +
		"\n<!-- issuedog:plan-data v=1 data=abc -->\n<!-- issuedog:revision kind=plan rev=1 body=bbbb2222 -->"
	epic := &github.Snapshot{Number: 5, Author: owner, Body: "An epic.", Type: "epic", Labels: []string{"stage:planned", "priority:p1"},
		Comments: []github.Comment{
			adoptComment(100, owner, adoptMove("from=intake to=clarifying cmd=90")),
			adoptComment(200, owner, "Spec rev 1\n"+adoptMove("from=clarifying to=spec-review run=s1 rev=1 body=aaaa1111")+
				"\n<!-- issuedog:revision rev=1 body=aaaa1111 -->"),
			adoptComment(250, mallory, "Looks good."),
			adoptComment(300, owner, adoptMove("from=spec-review to=planning rev=1 body=aaaa1111 cmd=r5")),
			adoptComment(400, owner, planText),
			adoptComment(500, owner, adoptMove("from=plan-review to=planned rev=1 body=bbbb2222 cmd=r6")),
			adoptComment(600, owner, "Created\n- S1 #11\n- S2 #12\n<!-- issuedog:created rev=1 children=11,12 -->"),
		}}
	story := &github.Snapshot{Number: 11, Author: owner, Type: "story", Parent: 5, Labels: []string{"stage:planned"},
		Body: "A story.\n<!-- issuedog:origin plan=#5 rev=1 item=S1 -->",
		Comments: []github.Comment{
			adoptComment(100, owner, adoptMove("from=intake to=planned event=p600")),
			adoptComment(200, owner, "Passed\n<!-- issuedog:verify verdict=pass attempt=1 run=v9 head=abc123 -->"),
		}}
	labelsOnly := &github.Snapshot{Number: 12, Author: owner, Type: "task", Parent: 11, Body: "A task.",
		Labels: []string{"stage:ready"}, Comments: []github.Comment{adoptComment(100, owner, "Working on it.")}}
	labelsOrigin := &github.Snapshot{Number: 29, Author: owner, Type: "task", Parent: 11, Labels: []string{"stage:ready"},
		Body: "A task, filed by hand from its story's plan.\n<!-- issuedog:origin plan=#11 rev=1 item=T1 -->"}
	humanTask := &github.Snapshot{Number: 31, Author: owner, Type: "task", Parent: 11, Labels: []string{"stage:needs-human"},
		Body: "Add the ruleset.\n<!-- issuedog:origin plan=#11 rev=1 item=T7 check=ruleset value=issuedog%2Fverify human=1 -->"}
	held := &github.Snapshot{Number: 13, Author: owner, Type: "task", Labels: []string{"stage:held"},
		Comments: []github.Comment{
			adoptComment(100, owner, adoptMove("from=intake to=ready cmd=90")),
			adoptComment(200, owner, adoptMove("from=ready to=held at=ready reason=owner cmd=150")),
		}}
	resumed := &github.Snapshot{Number: 32, Author: owner, Type: "task", Labels: []string{"stage:ready"},
		Comments: []github.Comment{
			adoptComment(100, owner, adoptMove("from=intake to=ready cmd=90")),
			adoptComment(200, owner, adoptMove("from=ready to=held at=ready reason=owner cmd=150")),
			adoptComment(250, mallory, "A comment while it's held."),
			adoptComment(300, owner, adoptMove("from=held to=ready cmd=290")),
		}}
	answer := &github.Snapshot{Number: 14, Author: owner, Labels: []string{"stage:spec-review"},
		Comments: []github.Comment{
			adoptComment(100, owner, adoptMove("from=clarifying to=spec-review run=s1 rev=1 body=aaaa1111")+
				"<!-- issuedog:revision rev=1 body=aaaa1111 -->"),
			adoptComment(150, mallory, "An answer after the spec."),
		}}
	revisionAlone := &github.Snapshot{Number: 15, Author: owner, Labels: []string{"stage:spec-review"},
		Comments: []github.Comment{
			adoptComment(100, owner, adoptMove("from=clarifying to=spec-review run=s1 rev=1 body=aaaa1111")),
			adoptComment(200, owner, "<!-- issuedog:revision rev=2 body=cccc3333 -->"),
		}}
	seenKept := &github.Snapshot{Number: 16, Author: owner, Labels: []string{"stage:spec-review"},
		Comments: []github.Comment{
			adoptComment(100, owner, adoptMove("from=clarifying to=spec-review run=s1 rev=1 body=aaaa1111 seen=90")),
			adoptComment(200, owner, "<!-- issuedog:revision rev=2 body=cccc3333 seen=180 -->"),
		}}
	dirtyComment := &github.Snapshot{Number: 17, Author: owner, Labels: []string{"stage:clarifying"},
		Comments: []github.Comment{
			adoptComment(100, owner, adoptMove("from=intake to=clarifying cmd=90")),
			adoptComment(200, owner, adoptMove("from=clarifying to=spec-review run=s1 rev=1 body=aaaa1111")),
		}}
	bodyEditedByOther := &github.Snapshot{Number: 18, Author: owner, Type: "story", Parent: 5, Labels: []string{"stage:planned"},
		Body:     "A story.\n<!-- issuedog:origin plan=#5 rev=1 item=S2 -->",
		BodyEdit: github.Edit{By: mallory, At: adoptEditAt},
		Comments: []github.Comment{adoptComment(100, owner, adoptMove("from=intake to=planned event=p600"))}}
	bodyEditedByLogin := &github.Snapshot{Number: 19, Author: owner, Type: "story", Parent: 5, Labels: []string{"stage:planned"},
		Body:     "A story.\n<!-- issuedog:origin plan=#5 rev=1 item=S2 -->",
		BodyEdit: github.Edit{By: adoptOwnerCased, At: adoptEditAt},
		Comments: []github.Comment{adoptComment(100, owner, adoptMove("from=intake to=planned event=p600"))}}
	bodyEditedByBot := &github.Snapshot{Number: 27, Author: owner, Type: "story", Parent: 5, Labels: []string{"stage:planned"},
		Body:     "A story.\n<!-- issuedog:origin plan=#5 rev=1 item=S2 -->",
		BodyEdit: github.Edit{By: adoptOwnerBot, At: adoptEditAt},
		Comments: []github.Comment{adoptComment(100, owner, adoptMove("from=intake to=planned event=p600"))}}
	otherLogins := &github.Snapshot{Number: 20, Author: mallory, Type: "story", Parent: 5, Labels: []string{"stage:planned"},
		Body: "<!-- issuedog:origin plan=#5 rev=1 item=S3 -->",
		Comments: []github.Comment{
			adoptComment(100, adoptOwnerCased, adoptMove("from=intake to=planned event=p600")),
			adoptComment(200, mallory, adoptMove("from=planned to=ready cmd=190")),
			adoptComment(300, adoptOwnerBot, adoptMove("from=planned to=ready cmd=290")),
		}}
	appsOwn := &github.Snapshot{Number: 21, Author: owner, Type: "story", Parent: 5, Labels: []string{"stage:planned"},
		Body: "A story.",
		Comments: []github.Comment{
			adoptComment(100, owner, adoptMove("from=intake to=planned event=p600")),
			adoptComment(200, appBot, "Origin\n<!-- issuedog:origin plan=#5 rev=1 item=S4 -->"),
			adoptComment(300, appBot, "<!-- issuedog:error kind=disagreement key=77 -->"),
			adoptComment(400, appBot, adoptMove("from=planned to=ready cmd=390")),
		}}
	done := &github.Snapshot{Number: 22, Author: owner, Labels: []string{"stage:planned"},
		Comments: []github.Comment{
			adoptComment(100, owner, adoptMove("from=intake to=clarifying cmd=90")),
			adoptComment(200, owner, adoptMove("from=clarifying to=planned cmd=190")),
			adoptComment(300, appBot, adoptMove("from=intake to=clarifying cmd=90 seen=100")+
				"<!-- issuedog:adopted from=100 login=owner -->"),
		}}
	doneLater := &github.Snapshot{Number: 23, Author: owner, Labels: []string{"stage:planned"},
		Comments: []github.Comment{
			adoptComment(100, owner, adoptMove("from=intake to=clarifying cmd=90")),
			adoptComment(200, owner, adoptMove("from=clarifying to=planned cmd=190")),
			adoptComment(300, appBot, adoptMove("from=intake to=clarifying cmd=90 seen=100")+
				"<!-- issuedog:adopted from=100 login=owner -->"),
			adoptComment(400, appBot, adoptMove("from=clarifying to=planned cmd=190 seen=200")+
				"<!-- issuedog:adopted from=200 login=owner -->"),
		}}
	doneRecordLeft := func(label string) *github.Snapshot {
		return &github.Snapshot{Number: 24, Author: owner, Labels: []string{label},
			Comments: []github.Comment{
				adoptComment(100, owner, adoptMove("from=intake to=clarifying cmd=90")),
				adoptComment(200, owner, "<!-- issuedog:created rev=1 children=11 -->"),
				adoptComment(300, appBot, adoptMove("from=intake to=clarifying cmd=90 seen=100")+
					"<!-- issuedog:adopted from=100 login=owner -->"),
			}}
	}
	refusing := func(label string, more ...github.Comment) *github.Snapshot {
		s := &github.Snapshot{Number: 30, Author: owner, Labels: []string{label},
			Comments: []github.Comment{adoptComment(100, owner, adoptMove("from=intake to=clarifying cmd=90"))}}
		if label == "" {
			s.Labels = nil
		}
		s.Comments = append(s.Comments, more...)
		return s
	}
	twoLabels := refusing("stage:clarifying")
	twoLabels.Labels = append(twoLabels.Labels, "stage:ready")
	move100 := "100: " + adoptMove("from=intake to=clarifying cmd=90 seen=100")
	return []adoptCase{
		{name: "a planned epic", s: epic, dirty: clean, posts: []string{
			"100: " + adoptMove("from=intake to=clarifying cmd=90 seen=100"),
			"200: " + adoptMove("from=clarifying to=spec-review rev=1 body=aaaa1111 run=s1 seen=200") + " <!-- issuedog:revision rev=1 body=aaaa1111 -->",
			"300: " + adoptMove("from=spec-review to=planning rev=1 body=aaaa1111 cmd=r5 seen=300"),
			"400: " + adoptMove("from=planning to=plan-review rev=1 run=p1 seen=400") + " <!-- issuedog:revision kind=plan rev=1 body=bbbb2222 -->",
			"500: " + adoptMove("from=plan-review to=planned rev=1 body=bbbb2222 cmd=r6 seen=500"),
			"600: <!-- issuedog:created rev=1 children=11,12 -->",
		}},
		{name: "a story with its body's origin, and a verify left where it is", s: story, dirty: clean, posts: []string{
			"0: <!-- issuedog:origin plan=#5 rev=1 item=S1 -->",
			"100: " + adoptMove("from=intake to=planned event=p600 seen=100"),
		}},
		{name: "a task with only labels", s: labelsOnly, dirty: clean},
		{name: "a task with only labels and its body's origin", s: labelsOrigin, dirty: clean, posts: []string{
			"0: <!-- issuedog:origin plan=#11 rev=1 item=T1 -->",
		}},
		{name: "a human task with a percent-encoded check", s: humanTask, dirty: clean, posts: []string{
			"0: <!-- issuedog:origin plan=#11 rev=1 item=T7 check=ruleset human=1 value=issuedog%2Fverify -->",
		}},
		{name: "refused while held", s: held, dirty: clean, posts: []string{
			"100: " + adoptMove("from=intake to=ready cmd=90 seen=100"),
			"200: " + adoptMove("from=ready to=held at=ready reason=owner cmd=150 seen=200"),
		}, refused: []string{"stage:held: a hold's repost would bound input at itself, not where the hold did, so /resume it first"}},
		{name: "a hold resumed", s: resumed, dirty: clean, posts: []string{
			"100: " + adoptMove("from=intake to=ready cmd=90 seen=100"),
			"200: " + adoptMove("from=ready to=held at=ready reason=owner cmd=150 seen=200"),
			"300: " + adoptMove("from=held to=ready cmd=290 seen=300"),
		}},
		{name: "an answer after a transition", s: answer, dirty: clean, posts: []string{
			"100: " + adoptMove("from=clarifying to=spec-review rev=1 body=aaaa1111 run=s1 seen=100") + " <!-- issuedog:revision rev=1 body=aaaa1111 -->",
		}},
		{name: "a revision with no transition", s: revisionAlone, dirty: clean, posts: []string{
			"100: " + adoptMove("from=clarifying to=spec-review rev=1 body=aaaa1111 run=s1 seen=100"),
			"200: <!-- issuedog:revision rev=2 body=cccc3333 seen=200 -->",
		}},
		{name: "a seen kept", s: seenKept, dirty: clean, posts: []string{
			"100: " + adoptMove("from=clarifying to=spec-review rev=1 body=aaaa1111 run=s1 seen=90"),
			"200: <!-- issuedog:revision rev=2 body=cccc3333 seen=180 -->",
		}},
		{name: "a dirty comment", s: dirtyComment, dirty: dirtyIDs(200), posts: []string{move100}},
		{name: "a dirty body", s: story, dirty: dirtyIDs(BodyID), posts: []string{
			"100: " + adoptMove("from=intake to=planned event=p600 seen=100"),
		}},
		{name: "a body someone else edited", s: bodyEditedByOther, dirty: clean, posts: []string{
			"100: " + adoptMove("from=intake to=planned event=p600 seen=100"),
		}},
		{name: "a body the login edited", s: bodyEditedByLogin, dirty: clean, posts: []string{
			"0: <!-- issuedog:origin plan=#5 rev=1 item=S2 -->",
			"100: " + adoptMove("from=intake to=planned event=p600 seen=100"),
		}},
		{name: "a body a bot of the login's name edited", s: bodyEditedByBot, dirty: clean, posts: []string{
			"100: " + adoptMove("from=intake to=planned event=p600 seen=100"),
		}},
		{name: "another login's markers, and a bot of the login's name", s: otherLogins, dirty: clean, posts: []string{
			"100: " + adoptMove("from=intake to=planned event=p600 seen=100"),
		}},
		{name: "the App's own", s: appsOwn, dirty: dirtyIDs(400), posts: []string{
			"100: " + adoptMove("from=intake to=planned event=p600 seen=100"),
		}},
		{name: "a done source", s: done, dirty: clean, done: []int64{100}, posts: []string{
			"200: " + adoptMove("from=clarifying to=planned cmd=190 seen=200"),
		}},
		{name: "a done source's transition, with a record left", s: doneRecordLeft("stage:clarifying"), dirty: clean, done: []int64{100},
			posts: []string{"200: <!-- issuedog:created rev=1 children=11 -->"}},
		{name: "refused for a done source's transition that isn't the label's", s: doneRecordLeft("stage:ready"), dirty: clean,
			done: []int64{100}, posts: []string{"200: <!-- issuedog:created rev=1 children=11 -->"},
			refused: []string{"stage:ready isn't clarifying, the stage its latest transition sets once reposted"}},
		{name: "refused for a repost that would come after a later source's", s: doneLater, dirty: dirtyIDs(300), done: []int64{200},
			posts: []string{move100}, refused: []string{"stage:planned isn't clarifying"}},
		{name: "every source done, at a stage it refuses", s: func() *github.Snapshot {
			s := *done
			s.Labels = []string{"stage:pr-open"}
			s.Comments = append(slices.Clone(done.Comments), adoptComment(400, appBot,
				adoptMove("from=clarifying to=planned cmd=190 seen=200")+"<!-- issuedog:adopted from=200 login=owner -->"))
			return &s
		}(), dirty: clean, done: []int64{100, 200}},
		{name: "refused at plan-review", s: refusing("stage:plan-review"), dirty: clean, posts: []string{move100},
			refused: []string{"stage:plan-review: a plan's hash covers its whole comment", "stage:plan-review isn't clarifying"}},
		{name: "refused at in-progress", s: refusing("stage:in-progress"), dirty: clean, posts: []string{move100},
			refused: []string{"stage:in-progress: a run by hand left no head, PR or check"}},
		{name: "refused at verifying", s: refusing("stage:verifying"), dirty: clean, posts: []string{move100},
			refused: []string{"stage:verifying: a run by hand left no head, PR or check"}},
		{name: "refused at pr-open", s: refusing("Stage:PR-Open"), dirty: clean, posts: []string{move100},
			refused: []string{"stage:pr-open: a run by hand left no head, PR or check"}},
		{name: "refused for a label that isn't the latest transition's", s: refusing("stage:ready"), dirty: clean, posts: []string{move100},
			refused: []string{"stage:ready isn't clarifying, the stage its latest transition sets once reposted"}},
		{name: "refused with no stage label", s: refusing(""), dirty: clean, posts: []string{move100},
			refused: []string{"it has 0 stage labels, not one"}},
		{name: "refused with two stage labels", s: twoLabels, dirty: clean, posts: []string{move100},
			refused: []string{"it has 2 stage labels, not one"}},
		{name: "refused for an App transition adopt didn't write", s: refusing("stage:clarifying",
			adoptComment(200, appBot, adoptMove("from=clarifying to=clarifying round=1 run=q1"))), dirty: clean, posts: []string{move100},
			refused: []string{"the App's transition in comment 200 isn't adopt's"}},
	}
}

// TestAdoptPosts checks which sources Adopt reposts, with which markers,
// which it finds done, and what it refuses.
func TestAdoptPosts(t *testing.T) {
	t.Parallel()
	for _, tt := range adoptCases() {
		a := Adopt(tt.s, adoptLogin, slug, tt.dirty)
		if got := adoptPosts(t, a); !reflect.DeepEqual(got, tt.posts) {
			t.Errorf("%s: posts =\n%s\nwant\n%s", tt.name, strings.Join(got, "\n"), strings.Join(tt.posts, "\n"))
		}
		if !reflect.DeepEqual(a.Done, tt.done) {
			t.Errorf("%s: done = %v, want %v", tt.name, a.Done, tt.done)
		}
		if len(tt.refused) == 0 && a.Refused != "" {
			t.Errorf("%s: refused %q, want nothing", tt.name, a.Refused)
		}
		for _, want := range tt.refused {
			if !strings.Contains(a.Refused, want) {
				t.Errorf("%s: refused %q, want it to say %q", tt.name, a.Refused, want)
			}
		}
	}
}

// TestAdoptReadsTheSame checks SPEC §5's adopted marker: an issue read with
// its reposts as App comments reads as it does with the login's markers
// read as the App's, and a second read finds every source done, so it
// posts and refuses nothing.
func TestAdoptReadsTheSame(t *testing.T) {
	t.Parallel()
	for _, tt := range adoptCases() {
		a := Adopt(tt.s, adoptLogin, slug, tt.dirty)
		reposted := withReposts(t, tt.s, a, adoptLogin)
		if len(tt.refused) == 0 {
			want := readForAdopt(asApp(tt.s, adoptLogin, slug), tt.dirty)
			if got := readForAdopt(reposted, tt.dirty); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: read with its reposts = %+v, want %+v", tt.name, got, want)
			}
		}
		again := Adopt(reposted, adoptLogin, slug, tt.dirty)
		wantDone := slices.Clone(tt.done)
		for _, p := range a.Posts {
			wantDone = append(wantDone, p.From)
		}
		slices.Sort(wantDone)
		if len(again.Posts) != 0 || again.Refused != "" || !reflect.DeepEqual(again.Done, wantDone) {
			t.Errorf("%s: second read = %+v, want no posts, no refusal and done %v", tt.name, again, wantDone)
		}
	}
}

// TestAdoptReadsAnEpicAsPlanned pins a case's reading, so the comparison
// above can't pass by reading nothing either way.
func TestAdoptReadsAnEpicAsPlanned(t *testing.T) {
	t.Parallel()
	for _, tt := range adoptCases() {
		if tt.name != "a planned epic" {
			continue
		}
		a := Adopt(tt.s, adoptLogin, slug, tt.dirty)
		got := readForAdopt(withReposts(t, tt.s, a, adoptLogin), tt.dirty)
		if got.Stage != Planned || got.Round != 500 || !got.Keys[ReactionKey(6)] || !reflect.DeepEqual(got.Created[1], []int{11, 12}) || got.Class != ClassEpic {
			t.Errorf("epic read with its reposts = %+v, want planned, round 500, cmd=r6, created 11,12 and an epic", got)
		}
		return
	}
	t.Fatal("no planned epic case")
}

// TestAdoptSeenKeepsAnAnswerAfter checks that a reposted transition bounds
// input where its source did: an answer posted between the source and the
// repost is still input after it.
func TestAdoptSeenKeepsAnAnswerAfter(t *testing.T) {
	t.Parallel()
	s := &github.Snapshot{Number: 14, Author: owner, Labels: []string{"stage:spec-review"},
		Comments: []github.Comment{
			adoptComment(100, owner, adoptMove("from=clarifying to=spec-review run=s1 rev=1 body=aaaa1111")),
			adoptComment(150, mallory, "An answer after the spec."),
		}}
	reposted := withReposts(t, s, Adopt(s, adoptLogin, slug, clean), adoptLogin)
	is := Derive(reposted, slug, clean)
	if is.Round != 100 || is.TransitionComment <= 150 {
		t.Errorf("round = %d from comment %d, want 100 from a repost after the answer, 150", is.Round, is.TransitionComment)
	}
}

// TestAdoptStoryOrigin checks that a story's body origin, reposted, gives
// its class and plan item as the App's body marker would.
func TestAdoptStoryOrigin(t *testing.T) {
	t.Parallel()
	s := &github.Snapshot{Number: 11, Author: owner, Type: "story", Parent: 5, Labels: []string{"stage:planned"},
		Body: "A story.\n<!-- issuedog:origin plan=#5 rev=1 item=S1 -->"}
	is := Derive(withReposts(t, s, Adopt(s, adoptLogin, slug, clean), adoptLogin), slug, clean)
	class, err := ClassOf(is.Item)
	if err != nil {
		t.Fatalf("class: %v", err)
	}
	if is.Origin.Plan != 5 || is.Origin.Item != "S1" || class != ClassStory {
		t.Errorf("origin = %+v, class %v, want plan #5 item S1 and a story", is.Origin, class)
	}
}

// TestAdoptHumanTaskCheck checks that a human task's body origin, reposted,
// keeps human=1 and its check, whose value is percent-encoded as written:
// it reads back once, not encoded a second time.
func TestAdoptHumanTaskCheck(t *testing.T) {
	t.Parallel()
	s := &github.Snapshot{Number: 31, Author: owner, Type: "task", Parent: 11, Labels: []string{"stage:needs-human"},
		Body: "Add the ruleset.\n<!-- issuedog:origin plan=#11 rev=1 item=T7 check=ruleset value=issuedog%2Fverify human=1 -->"}
	is := Derive(withReposts(t, s, Adopt(s, adoptLogin, slug, clean), adoptLogin), slug, clean)
	if want := (Check{Kind: CheckRuleset, Value: "issuedog/verify"}); !is.Origin.Human || is.Origin.Check != want || is.Origin.Item != "T7" {
		t.Errorf("origin = %+v, want human task T7 with the check %+v", is.Origin, want)
	}
}

// TestAdoptBodyGetsNoSeen checks that the body's markers are reposted as
// written: the body has no comment ID to bound input by.
func TestAdoptBodyGetsNoSeen(t *testing.T) {
	t.Parallel()
	s := &github.Snapshot{Number: 25, Author: owner, Labels: []string{"stage:planned"},
		Body: adoptMove("from=intake to=planned event=p600") + "<!-- issuedog:revision rev=1 body=aaaa1111 -->"}
	got := adoptPosts(t, Adopt(s, adoptLogin, slug, clean))
	want := []string{"0: " + adoptMove("from=intake to=planned event=p600") + " <!-- issuedog:revision rev=1 body=aaaa1111 -->"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("posts = %q, want %q", got, want)
	}
}

// TestAdoptNoLogin checks that no login adopts nothing, not even content
// GitHub names no author for.
func TestAdoptNoLogin(t *testing.T) {
	t.Parallel()
	s := &github.Snapshot{Number: 26, Labels: []string{"stage:planned"},
		Comments: []github.Comment{adoptComment(100, github.Author{}, adoptMove("from=intake to=planned event=p600"))}}
	if a := Adopt(s, "", slug, clean); !reflect.DeepEqual(a, Adoption{}) {
		t.Errorf("adoption with no login = %+v, want none", a)
	}
}

func TestAdoptedMarker(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		from int64
		want string
	}{
		{BodyID, "<!-- issuedog:adopted from=body login=owner -->"},
		{6245648446, "<!-- issuedog:adopted from=6245648446 login=owner -->"},
	} {
		m := AdoptedMarker(tt.from, adoptLogin)
		got, err := github.FormatMarker(m)
		if err != nil {
			t.Fatalf("format adopted from %d: %v", tt.from, err)
		}
		if got != tt.want {
			t.Errorf("adopted from %d = %s, want %s", tt.from, got, tt.want)
		}
		if from, ok := adoptedOf(github.ParseMarkers(got)[0]); !ok || from != tt.from {
			t.Errorf("adoptedOf(%s) = %d, %v, want %d", got, from, ok, tt.from)
		}
	}
	if m := AdoptedMarker(1, "a b"); m.Fields["login"] != "a%20b" {
		t.Errorf("login a b = %q, want it percent-encoded", m.Fields["login"])
	}
	for _, text := range []string{
		"<!-- issuedog:adopted from=0 login=owner -->",
		"<!-- issuedog:adopted from=-4 login=owner -->",
		"<!-- issuedog:adopted from=Body login=owner -->",
		"<!-- issuedog:adopted login=owner -->",
		"<!-- issuedog:origin from=100 -->",
	} {
		if from, ok := adoptedOf(github.ParseMarkers(text)[0]); ok {
			t.Errorf("adoptedOf(%s) = %d, want none", text, from)
		}
	}
}
