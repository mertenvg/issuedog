// Package githubtest is an in-memory GitHub for tests of code that talks to
// it through internal/github.
package githubtest

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/logr/v2"
)

// Start is the fake clock's first reading. Every change moves it on a second.
var Start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Fake serves one repo's issues, comments, labels, timelines, reactions,
// parents and sub-issues, a closed event's closer (closing.go), a
// sub-issue's link and an issue's dependencies, listed both ways, and
// blocked-by links added and removed by the issue's ID, which every issue has apart from its number
// (children.go, dependencies.go), a created issue's labels and type, a
// transferred issue's 301s and new home (transfer.go), the
// GraphQL queries for their edits, deleted
// comments and pins, and the pinIssue mutation. Its API writes, comment edits, created issues and
// pins included, are made by the App, "<slug>[bot]" of type Bot. A GET whose
// answer hasn't changed gets a 304. Every write (any method but GET, and a
// GraphQL mutation) is logged, and FailWrite can fail one.
//
// To add an endpoint, write a handler and register it in New with handle, or
// with serve for a POST that only reads, as GraphQL does.
type Fake struct {
	repo, slug string
	t          testing.TB // New's, which a helper given an issue number or a comment ID the fake hasn't got fails (fakeNumberIssue, fakeCommentID)
	srv        *httptest.Server
	mux        *http.ServeMux

	mu          sync.Mutex
	now         time.Time
	lastID      int64
	lastEvent   int64         // timeline events have IDs of their own
	eventLag    time.Duration // see LagEvents
	issues      []*issue      // by number, from 1
	comments    []*comment    // oldest first
	writes      []Write
	failAt      int
	failAfter   bool // FailWriteAfter rather than FailWrite
	notModified int
	gqlErrors   []string         // for the next GraphQL answer
	pinFailure  string           // see FailPin
	pinStatus   int              // see FailPinStatus
	pinnedFail  int              // see FailIsPinned
	beforeGQL   func()           // runs before the next GraphQL answer
	histories   int              // edit history queries answered
	historyCap  int              // see HistoryCap
	linked      map[int][]linked // see LinkSubIssue
	dropTypes   bool             // see DropTypeChanges
	lastLink    int64            // the last sub-issue link made (linkUnder)

	// The org's issue types (typeevent.go): their IDs by lowercase name,
	// their names as the org shows them, by ID, and the last ID given. A
	// name renamed away (RenameOrgIssueType) has no ID by it.
	orgTypes      map[string]int64
	orgTypeNames  map[int64]string
	lastIssueType int64

	unassignable map[string]bool // logins in lower case, see Unassignable (assign.go)

	closingErrs []map[string]any // for the next GraphQL answer, with their paths (FailGraphQLAt)

	historyNotFound int // the issue whose body node the next history query answers null (HistoryNotFoundOnce)

	repoIDs map[string]int64 // the repos issues were transferred to, in lower case, by ID (transfer.go)

	checks checkState // what the check reads answer (checks.go)

	// The App, permission and repo label endpoints, and the request log (app.go).
	perms      map[string]string
	labels     []*gogithub.Label
	requests   []Request
	beforeReq  func(Request) func() // see BeforeRequest
	answerOnce func(Request) int    // see AnswerOnce
}

// discard swallows a reply, for a write that takes effect but answers 500.
type discard struct{ http.ResponseWriter }

func (discard) Header() http.Header         { return http.Header{} }
func (discard) WriteHeader(int)             {}
func (discard) Write(b []byte) (int, error) { return len(b), nil }

// issue and comment keep the API's own types, which are also their replies.
type issue struct {
	gogithub.Issue // labels sorted by name
	parent         int
	linkSeq        int64 // when it was linked under parent, for the sub-issues' order (linkUnder)
	blockedBy      []int // the issues blocking it, by number, in the order added (dependencies.go)
	lastEdit       lastEdit
	history        []lastEdit
	labelEvents    []*gogithub.Timeline            // labeled, unlabeled, renamed, closed, reopened and issue-type events, oldest first
	pinned         bool                            // to the repo's issue list (PinIssue)
	typeEvents     map[int64]issueTypeRevertChange // the types of each issue-type event, by its ID
	deletions      []deletedEvent                  // CommentDeletedEvents, oldest first (DeleteCommentBy)
	closingClosers map[int64]closingCloser         // how each closed event was made, by its ID (closingAnswer)
	movedTo        string                          // the repo Transfer moved it to, in lower case, or "" (transfer.go)
	reactions      []*gogithub.Reaction            // to the issue itself, oldest first (reactions_issue.go)
}

type comment struct {
	gogithub.IssueComment
	issue     int
	reactions []*gogithub.Reaction
	lastEdit  lastEdit
	history   []lastEdit
}

// lastEdit is who last edited a body, and when. The zero lastEdit means never.
// In a history, it is one edit, with the text after it, which an issue
// body's history answers as its diff (DeleteRevision empties it).
type lastEdit struct {
	by   string
	at   time.Time
	text string
}

// record makes e the last edit and adds it to history, oldest first. As on
// GitHub, the first edit also adds the original post, by its author, with
// the text before the edit, was.
func record(history *[]lastEdit, last *lastEdit, author string, created time.Time, was string, e lastEdit) {
	if len(*history) == 0 {
		*history = append(*history, lastEdit{author, created, was})
	}
	*history, *last = append(*history, e), e
}

// Write is one write request: its method, its path below the repo (such as
// /issues/7/labels), its body, and whether FailWrite failed it.
type Write struct {
	Method, Path, Body string
	Failed             bool
}

// call is one request, with the issue and comment its path names.
type call struct {
	w       http.ResponseWriter
	r       *http.Request
	issue   *issue
	comment *comment
}

// handler serves one call with the fake locked. It returns the status and
// the value to send as JSON.
type handler func(c *call) (int, any)

// New starts a fake of repo ("owner/name") for the App with this slug.
func New(t testing.TB, repo, slug string) *Fake {
	t.Helper()
	f := &Fake{repo: repo, slug: slug, t: t, mux: http.NewServeMux(), now: Start, lastID: 1000, historyCap: 100}
	f.mux.HandleFunc("POST /app/installations/{id}/access_tokens", mintToken)
	f.handle("GET /issues/{n}", f.getIssue)
	f.handle("GET /issues/{n}/parent", f.getParent)
	f.handle("GET /issues/{n}/sub_issues", f.subIssues)
	f.handle("POST /issues/{n}/sub_issues", f.addSubIssue)
	f.handle("GET /issues/{n}/dependencies/blocked_by", f.blockedBy)
	f.handle("POST /issues/{n}/dependencies/blocked_by", f.addBlockedBy)
	f.handle("DELETE /issues/{n}/dependencies/blocked_by/{issue_id}", f.removeBlockedBy)
	f.handle("GET /issues/{n}/dependencies/blocking", f.blocking)
	f.handle("GET /issues/{n}/comments", f.listComments)
	f.handle("GET /issues/comments", f.listComments)
	f.handle("POST /issues/{n}/comments", f.addComment)
	f.handle("POST /issues", f.createIssue)
	f.handle("PATCH /issues/{n}", f.editIssue)
	f.handle("PATCH /issues/comments/{id}", f.editComment)
	f.handle("GET /issues/comments/{id}/reactions", f.listReactions)
	f.handle("POST /issues/comments/{id}/reactions", f.addReaction)
	f.handle("GET /issues/{n}/reactions", f.listIssueReactions)
	f.handle("POST /issues/{n}/labels", f.addLabels)
	f.handle("DELETE /issues/{n}/labels/{name}", f.removeLabel)
	f.handle("GET /issues/{n}/timeline", f.timeline)
	f.mux.HandleFunc("POST /graphql", f.serveGraphQL)
	f.handleApp()
	f.handleChecks()
	f.handleAssign()
	f.handleTransfer()
	f.srv = httptest.NewServer(f.logged(f.mux))
	t.Cleanup(f.srv.Close)
	return f
}

var testKey = sync.OnceValues(func() ([]byte, error) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate test key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), nil
})

// Client returns a new client of the fake, with its own token and ETag cache.
// It talks to the fake over the fake's own Transport.
func (f *Fake) Client(t testing.TB) *github.Client {
	t.Helper()
	key, err := testKey()
	if err != nil {
		t.Fatal(err)
	}
	c, err := github.New(github.Config{AppID: 1, InstallationID: 1, Key: key, BaseURL: f.srv.URL, Transport: f.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// AddIssue files an issue by author and returns its number.
func (f *Fake) AddIssue(author, title, body string, labels ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fileIssue(author, title, body, labels).GetNumber()
}

// fileIssue files an issue by author, with its labels added by author, and
// returns it. Its node ID is the one GraphQL knows it by (issueNode), and
// its ID the one sub-issue and dependency writes name it by (issueID).
func (f *Fake) fileIssue(author, title, body string, labels []string) *issue {
	now := f.tick()
	n := len(f.issues) + 1
	is := &issue{Issue: gogithub.Issue{Number: &n, ID: new(issueID(n)), NodeID: new(issueNode(n)), Title: &title, Body: &body, State: new("open"),
		User: user(author), CreatedAt: now, UpdatedAt: now}}
	for _, l := range labels {
		if is.addLabel(l) {
			f.labelEvent(is, "labeled", author, l, now)
		}
	}
	f.issues = append(f.issues, is)
	return is
}

// SetType sets an issue's type, as on org repos, with the type's ID in the
// org (IssueTypeID), and no event.
func (f *Fake) SetType(number int, name string) {
	f.t.Helper()
	f.edit(number, func(is *issue) { is.Type = f.issueTypeNamed(name) })
}

// SetPullRequest makes number a pull request, which REST lists with the issues.
func (f *Fake) SetPullRequest(number int) {
	f.t.Helper()
	f.edit(number, func(is *issue) {
		is.PullRequestLinks = &gogithub.PullRequestLinks{URL: new(fmt.Sprintf("%s/repos/%s/pulls/%d", f.srv.URL, f.repo, number))}
	})
}

// SetParent makes number a sub-issue of parent, listed after its other
// sub-issues, or of none for 0. It moves no updated_at. A parent the fake
// hasn't got fails the test, as a number does (edit).
func (f *Fake) SetParent(number, parent int) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.fakeNumberIssue(number)
	if parent != 0 {
		f.fakeNumberIssue(parent)
	}
	f.linkUnder(is, parent)
}

// edit runs change on issue number with the fake locked, or fails the test
// if the fake hasn't got that issue (fakeNumberIssue).
func (f *Fake) edit(number int, change func(*issue)) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f.fakeNumberIssue(number))
}

// fakeNumberIssue returns issue number for a helper a test called with it,
// or fails the test New was given, naming the number, if the fake hasn't
// got it: one never filed, or one DeleteIssue deleted. f is locked.
func (f *Fake) fakeNumberIssue(number int) *issue {
	f.t.Helper()
	is, err := f.fakeNumberFind(number)
	if err != nil {
		f.t.Fatalf("githubtest: %v", err)
	}
	return is
}

// fakeNumberFind returns issue number, or an error naming it if the fake
// hasn't got it, for an answer that names an issue by the number a parent
// or a blocked-by link holds. No helper leaves a link to an issue the fake
// hasn't got, as DeleteIssue drops a deleted issue's links, but an answer
// that meets one fails clearly, not with a panic. f is locked.
func (f *Fake) fakeNumberFind(number int) (*issue, error) {
	if number < 1 || number > len(f.issues) {
		return nil, fmt.Errorf("no issue #%d: the fake has %d", number, len(f.issues))
	}
	return f.issues[number-1], nil
}

// AddComment posts a comment by author and returns its ID.
func (f *Fake) AddComment(number int, author, body string) int64 {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addCommentBy(f.fakeNumberIssue(number), author, body).GetID()
}

// EditBody edits an issue's body as login. An empty login means GitHub names no editor.
func (f *Fake) EditBody(number int, login, body string) {
	f.t.Helper()
	f.edit(number, func(is *issue) { is.editBy(login, body, f.tick()) })
}

// DeleteRevision deletes the text of entry i, oldest first, of an issue
// body's edit history, as someone with write access can on GitHub: the
// entry keeps its editor and time, and its diff is null. An entry the
// history hasn't got fails the test, naming it and the history's length,
// as an issue number the fake hasn't got does (fakeNumberIssue). A body
// never edited has no history, and its first edit adds two entries: the
// original post, 0, and the edit, 1.
func (f *Fake) DeleteRevision(number, i int) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.fakeNumberIssue(number)
	if i < 0 || i >= len(is.history) {
		f.t.Fatalf("githubtest: no revision %d of issue #%d: its history has %d", i, number, len(is.history))
	}
	is.history[i].text = ""
}

// EditComment edits a comment as login. An empty login means GitHub names
// no editor. A comment the fake hasn't got fails the test (fakeCommentID).
func (f *Fake) EditComment(id int64, login, body string) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fakeCommentID(id).editBy(login, body, f.tick())
}

// DeleteComment deletes a comment, as its author or a maintainer can, with
// no CommentDeletedEvent: DeleteCommentBy records one. A comment the fake
// hasn't got, one deleted already included, fails the test
// (fakeCommentID).
func (f *Fake) DeleteComment(id int64) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fakeCommentID(id)
	f.comments = slices.DeleteFunc(f.comments, func(c *comment) bool { return c.GetID() == id })
}

// AddReaction reacts to a comment as author and returns the reaction's ID.
// A comment the fake hasn't got fails the test (fakeCommentID).
func (f *Fake) AddReaction(commentID int64, author, content string) int64 {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	r, _ := f.react(&f.fakeCommentID(commentID).reactions, author, content)
	return r.GetID()
}

// FailGraphQL makes the next GraphQL answer list these errors beside its data.
func (f *Fake) FailGraphQL(messages ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gqlErrors = messages
}

// FailWrite makes the nth write since New (1-based) fail with a 500 before
// it changes anything. 0 fails none.
func (f *Fake) FailWrite(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAt, f.failAfter = n, false
}

// FailWriteAfter makes the nth write since New (1-based) take effect but
// answer with a 500, as when GitHub applies a write and the response is lost.
// 0 fails none.
func (f *Fake) FailWriteAfter(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAt, f.failAfter = n, true
}

// Writes returns every write so far, failed ones included, in order.
func (f *Fake) Writes() []Write {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

// NotModified counts the GETs answered with a 304.
func (f *Fake) NotModified() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.notModified
}

// handle registers h for "METHOD /path below the repo". Only GETs are reads.
func (f *Fake) handle(pattern string, h handler) {
	method, path, _ := strings.Cut(pattern, " ")
	f.mux.HandleFunc(method+" /repos/"+f.repo+path, func(w http.ResponseWriter, r *http.Request) {
		f.serve(w, r, method != http.MethodGet, h)
	})
}

// serve runs h with the fake locked. A write is logged and may be failed. A
// path naming a missing issue or comment gets a 404, and one naming a
// transferred issue or its comment a 301 to its new home (moved, which also
// serves the new home). A read gets an ETag over
// its body and Link header, and a 304 if that matches If-None-Match. Every
// answer is dated by the fake clock, as GitHub dates its own.
func (f *Fake) serve(w http.ResponseWriter, r *http.Request, write bool, h handler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Date", f.now.UTC().Format(http.TimeFormat))
	if write {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			send(w, http.StatusBadRequest, message(err.Error()))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		failed := len(f.writes)+1 == f.failAt
		f.writes = append(f.writes, Write{r.Method, strings.TrimPrefix(r.URL.Path, "/repos/"+f.repo), string(body), failed})
		if failed && !f.failAfter {
			send(w, http.StatusInternalServerError, message("FailWrite"))
			return
		}
		if failed {
			// The write takes effect, but the client only sees a 500.
			defer send(w, http.StatusInternalServerError, message("FailWriteAfter"))
			w = discard{w}
		}
	}
	c := &call{w: w, r: r}
	if n, err := strconv.Atoi(r.PathValue("n")); err == nil && n >= 1 && n <= len(f.issues) {
		c.issue = f.issues[n-1]
	}
	if id, err := strconv.ParseInt(r.PathValue("id"), 10, 64); err == nil {
		c.comment = f.findComment(id)
	}
	if (r.PathValue("n") != "" && c.issue == nil) || (r.PathValue("id") != "" && c.comment == nil) {
		send(w, http.StatusNotFound, message("Not Found"))
		return
	}
	if f.moved(c) {
		return
	}
	status, v := h(c)
	data, err := json.Marshal(v)
	if err != nil {
		send(w, http.StatusInternalServerError, message(err.Error()))
		return
	}
	if !write && status == http.StatusOK {
		etag := fmt.Sprintf(`"%x"`, sha256.Sum256(append(data, w.Header().Get("Link")...)))
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			f.notModified++
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(data); err != nil {
		logr.Warnf("githubtest: write a reply: %v", err)
	}
}

// send writes v as JSON, logging a failure.
func send(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logr.Warnf("githubtest: write a reply: %v", err)
	}
}

func message(m string) map[string]string { return map[string]string{"message": m} }

// decode reads the request's JSON body into v, or returns the 422 to send.
func (c *call) decode(v any) (int, any) {
	if err := json.NewDecoder(c.r.Body).Decode(v); err != nil {
		return http.StatusUnprocessableEntity, message(err.Error())
	}
	return 0, nil
}

func mintToken(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		send(w, http.StatusUnauthorized, message("missing app JWT"))
		return
	}
	// ghinstallation checks the expiry against the real clock.
	send(w, http.StatusCreated, map[string]string{"token": "fake-token", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
}

// tick moves the clock on and returns the new time.
func (f *Fake) tick() *gogithub.Timestamp {
	f.now = f.now.Add(time.Second)
	return &gogithub.Timestamp{Time: f.now}
}

func (f *Fake) nextID() int64 { f.lastID++; return f.lastID }

func (f *Fake) addCommentBy(is *issue, author, body string) *comment {
	now := f.tick()
	c := &comment{IssueComment: gogithub.IssueComment{ID: new(f.nextID()), Body: &body, User: user(author), CreatedAt: now, UpdatedAt: now,
		IssueURL: new(fmt.Sprintf("%s/repos/%s/issues/%d", f.srv.URL, f.repo, is.GetNumber()))}, issue: is.GetNumber()}
	f.comments = append(f.comments, c)
	is.UpdatedAt = now
	return c
}

// react adds a reaction to a comment's or an issue's reactions unless author
// has it already, and reports whether it did.
func (f *Fake) react(reactions *[]*gogithub.Reaction, author, content string) (*gogithub.Reaction, bool) {
	for _, r := range *reactions {
		if r.GetUser().GetLogin() == author && r.GetContent() == content {
			return r, false
		}
	}
	r := &gogithub.Reaction{ID: new(f.nextID()), User: user(author), Content: &content, CreatedAt: f.tick()}
	*reactions = append(*reactions, r)
	return r, true
}

// fakeCommentID returns comment id for a helper a test called with it, or
// fails the test New was given, naming the ID, if the fake hasn't got it:
// one never posted, or one deleted, by itself or with its issue
// (DeleteIssue), as fakeNumberIssue fails one for an issue number. f is
// locked.
func (f *Fake) fakeCommentID(id int64) *comment {
	f.t.Helper()
	c := f.findComment(id)
	if c == nil {
		f.t.Fatalf("githubtest: no comment %d: the fake has %d", id, len(f.comments))
	}
	return c
}

func (f *Fake) findComment(id int64) *comment {
	i := slices.IndexFunc(f.comments, func(c *comment) bool { return c.GetID() == id })
	if i < 0 {
		return nil
	}
	return f.comments[i]
}

// addLabel adds a label unless the issue has it already, and reports whether it did.
func (is *issue) addLabel(name string) bool {
	if slices.ContainsFunc(is.Labels, func(l *gogithub.Label) bool { return strings.EqualFold(l.GetName(), name) }) {
		return false
	}
	is.Labels = append(is.Labels, &gogithub.Label{Name: name})
	slices.SortFunc(is.Labels, func(a, b *gogithub.Label) int { return strings.Compare(a.GetName(), b.GetName()) })
	return true
}

func user(login string) *gogithub.User {
	if strings.HasSuffix(login, "[bot]") {
		return &gogithub.User{Login: &login, Type: new("Bot")}
	}
	return &gogithub.User{Login: &login, Type: new("User")}
}

// page returns the page of items that per_page and page ask for, and links
// the next, at the path the request came with (homePath).
func page[T any](c *call, items []T) (int, any) {
	q := c.r.URL.Query()
	per, err := strconv.Atoi(q.Get("per_page"))
	if err != nil || per < 1 || per > 100 {
		per = 30
	}
	n, err := strconv.Atoi(q.Get("page"))
	if err != nil || n < 1 {
		n = 1
	}
	if n*per < len(items) {
		q.Set("page", strconv.Itoa(n+1))
		c.w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?%s>; rel="next"`, c.r.Host, homePath(c.r), q.Encode()))
	}
	return http.StatusOK, append([]T{}, items[min((n-1)*per, len(items)):min(n*per, len(items))]...)
}

// getIssue serves an issue, with its author's association (discAssociation),
// which the shallower field puts in the JSON in place of the Issue's own.
func (f *Fake) getIssue(c *call) (int, any) {
	return http.StatusOK, struct {
		gogithub.Issue
		AuthorAssociation string `json:"author_association"`
	}{c.issue.Issue, f.discAssociation(c.issue.GetUser().GetLogin())}
}

// getParent serves an issue's parent with its repository_url, this repo's
// unless Transfer set another. A parent the fake hasn't got, which no
// helper leaves (fakeNumberFind), is a 500 naming it.
func (f *Fake) getParent(c *call) (int, any) {
	if c.issue.parent == 0 {
		return http.StatusNotFound, message("Not Found")
	}
	is, err := f.fakeNumberFind(c.issue.parent)
	if err != nil {
		return http.StatusInternalServerError, message(fmt.Sprintf("githubtest: the parent of #%d: %v", c.issue.GetNumber(), err))
	}
	parent := is.Issue
	if parent.RepositoryURL == nil {
		parent.RepositoryURL = new(fmt.Sprintf("%s/repos/%s", f.srv.URL, f.repo))
	}
	return http.StatusOK, parent
}

// listComments serves one issue's comments oldest first, or the repo's,
// sorted by update in direction= for sort=updated, from since= on. The
// repo's leave out those of a transferred issue.
func (f *Fake) listComments(c *call) (int, any) {
	var from time.Time
	if s := c.r.URL.Query().Get("since"); s != "" {
		var err error
		if from, err = time.Parse(time.RFC3339, s); err != nil {
			return http.StatusUnprocessableEntity, message(err.Error())
		}
	}
	list := slices.DeleteFunc(slices.Clone(f.comments), func(cm *comment) bool {
		return (c.issue != nil && cm.issue != c.issue.GetNumber()) || (c.issue == nil && !f.issues[cm.issue-1].listed()) ||
			cm.GetUpdatedAt().Before(from)
	})
	if c.issue == nil && c.r.URL.Query().Get("sort") == "updated" {
		slices.SortStableFunc(list, func(a, b *comment) int { return a.GetUpdatedAt().Compare(b.GetUpdatedAt().Time) })
		if c.r.URL.Query().Get("direction") != "asc" {
			slices.Reverse(list)
		}
	}
	out := []gogithub.IssueComment{}
	for _, cm := range list {
		out = append(out, cm.IssueComment)
	}
	return page(c, out)
}

func (f *Fake) addComment(c *call) (int, any) {
	var req struct{ Body string }
	if status, msg := c.decode(&req); msg != nil {
		return status, msg
	}
	return http.StatusCreated, f.addCommentBy(c.issue, f.slug+"[bot]", req.Body).IssueComment
}

// editIssue edits an issue's title, its body, its state (open or closed),
// its type, or any of them, as the App. A state change is setState's, with
// state_reason if given, and a type change is issueTypeRevertEdit's. A new
// title adds the App's renamed event to the timeline, as RenameIssue does.
// Nothing else of an issue is edited yet.
func (f *Fake) editIssue(c *call) (int, any) {
	var req struct {
		Title, Body, State *string
		StateReason        string `json:"state_reason"`
		// Type is the type field as sent, null included, or nil if absent.
		Type json.RawMessage
	}
	if status, msg := c.decode(&req); msg != nil {
		return status, msg
	}
	if req.Title == nil && req.Body == nil && req.State == nil && req.Type == nil {
		return http.StatusUnprocessableEntity, message("githubtest: only the title, the body, the state and the type can be edited")
	}
	if req.Title != nil && *req.Title != c.issue.GetTitle() {
		from, title := c.issue.GetTitle(), *req.Title
		c.issue.Title, c.issue.UpdatedAt = &title, f.tick()
		f.lastEvent++
		id := firstEvent + f.lastEvent
		c.issue.labelEvents = append(c.issue.labelEvents, &gogithub.Timeline{ID: &id, Event: new("renamed"), Actor: user(f.slug + "[bot]"),
			Rename: &gogithub.Rename{From: &from, To: &title}, CreatedAt: &gogithub.Timestamp{Time: c.issue.UpdatedAt.Time}})
	}
	if req.State != nil && *req.State != "open" && *req.State != "closed" {
		return http.StatusUnprocessableEntity, message(fmt.Sprintf("Validation Failed: state %q is not open or closed", *req.State))
	}
	if req.Type != nil {
		if status, msg := f.issueTypeRevertEdit(c.issue, req.Type); msg != nil {
			return status, msg
		}
	}
	if req.Body != nil {
		c.issue.editBy(f.slug+"[bot]", *req.Body, f.tick())
	}
	if req.State != nil {
		f.setState(c.issue, *req.State, req.StateReason, f.slug+"[bot]")
	}
	return http.StatusOK, c.issue.Issue
}

func (is *issue) editBy(login, body string, now *gogithub.Timestamp) {
	was := is.GetBody()
	is.Body, is.UpdatedAt = &body, now
	record(&is.history, &is.lastEdit, is.GetUser().GetLogin(), is.GetCreatedAt().Time, was, lastEdit{login, now.Time, body})
}

func (f *Fake) editComment(c *call) (int, any) {
	var req struct{ Body string }
	if status, msg := c.decode(&req); msg != nil {
		return status, msg
	}
	c.comment.editBy(f.slug+"[bot]", req.Body, f.tick())
	return http.StatusOK, c.comment.IssueComment
}

func (c *comment) editBy(login, body string, now *gogithub.Timestamp) {
	was := c.GetBody()
	c.Body, c.UpdatedAt = &body, now
	record(&c.history, &c.lastEdit, c.GetUser().GetLogin(), c.GetCreatedAt().Time, was, lastEdit{login, now.Time, body})
}

// listReactions serves the reactions to a comment, oldest first, only
// those with the content= asked for, if any (fakeReactionFiltered), paged
// by per_page and page
// (GET /repos/{owner}/{repo}/issues/comments/{comment_id}/reactions).
func (f *Fake) listReactions(c *call) (int, any) {
	reactions, refused := fakeReactionFiltered(c, c.comment.reactions)
	if refused != "" {
		return http.StatusUnprocessableEntity, message(refused)
	}
	return page(c, reactions)
}

// fakeReactionContents are the reactions GitHub has, the only values a
// reaction listing's content= takes.
var fakeReactionContents = []string{"+1", "-1", "laugh", "confused", "heart", "hooray", "rocket", "eyes"}

// fakeReactionFiltered is reactions, or for a request with content=, such
// as +1, only those with that content, as GitHub filters both reaction
// listings, a comment's and an issue's. A content= that is no reaction
// GitHub has (fakeReactionContents) is refused instead, with the message
// that the listings answer with a 422, as GitHub likely does for a value
// its docs' list leaves out. GitHub's real answer is unconfirmed, and the
// daemon never sends content=.
func fakeReactionFiltered(c *call, reactions []*gogithub.Reaction) (filtered []*gogithub.Reaction, refused string) {
	content := c.r.URL.Query().Get("content")
	if content == "" {
		return reactions, ""
	}
	if !slices.Contains(fakeReactionContents, content) {
		return nil, fmt.Sprintf("Validation Failed: content %q is not one of %s", content, strings.Join(fakeReactionContents, ", "))
	}
	return slices.DeleteFunc(slices.Clone(reactions), func(r *gogithub.Reaction) bool { return r.GetContent() != content }), ""
}

// addReaction answers 201 for a new reaction, and 200 for one the App has already.
func (f *Fake) addReaction(c *call) (int, any) {
	var req struct{ Content string }
	if status, msg := c.decode(&req); msg != nil {
		return status, msg
	}
	r, created := f.react(&c.comment.reactions, f.slug+"[bot]", req.Content)
	if created {
		return http.StatusCreated, r
	}
	return http.StatusOK, r
}

func (f *Fake) addLabels(c *call) (int, any) {
	var names []string
	if status, msg := c.decode(&names); msg != nil {
		return status, msg
	}
	now := f.tick()
	for _, n := range names {
		if c.issue.addLabel(n) {
			f.labelEvent(c.issue, "labeled", f.slug+"[bot]", n, now)
		}
	}
	c.issue.UpdatedAt = now
	return http.StatusOK, c.issue.Labels
}

func (f *Fake) removeLabel(c *call) (int, any) {
	i := slices.IndexFunc(c.issue.Labels, func(l *gogithub.Label) bool { return strings.EqualFold(l.GetName(), c.r.PathValue("name")) })
	if i < 0 {
		return http.StatusNotFound, message("Label does not exist")
	}
	name := c.issue.Labels[i].GetName()
	c.issue.Labels, c.issue.UpdatedAt = slices.Delete(c.issue.Labels, i, i+1), f.tick()
	f.labelEvent(c.issue, "unlabeled", f.slug+"[bot]", name, c.issue.UpdatedAt)
	return http.StatusOK, c.issue.Labels
}
