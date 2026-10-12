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

// Fake serves one repo's issues, comments, labels, reactions and parents, and
// the GraphQL queries for their edits. Its API writes, comment edits
// included, are made by the App, "<slug>[bot]" of type Bot. A GET whose
// answer hasn't changed gets a 304. Every write (any method but GET) is
// logged, and FailWrite can fail one.
//
// To add an endpoint, write a handler and register it in New with handle, or
// with serve for a POST that only reads, as GraphQL does.
type Fake struct {
	repo, slug string
	srv        *httptest.Server
	mux        *http.ServeMux

	mu          sync.Mutex
	now         time.Time
	lastID      int64
	issues      []*issue   // by number, from 1
	comments    []*comment // oldest first
	writes      []Write
	failAt      int
	failAfter   bool // FailWriteAfter rather than FailWrite
	notModified int
	gqlErrors   []string // for the next GraphQL answer
	beforeGQL   func()   // runs before the next GraphQL answer
	histories   int      // edit history queries answered
	historyCap  int      // see HistoryCap
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
	lastEdit       lastEdit
	history        []lastEdit
}

type comment struct {
	gogithub.IssueComment
	issue     int
	reactions []*gogithub.Reaction
	lastEdit  lastEdit
	history   []lastEdit
}

// lastEdit is who last edited a body, and when. The zero lastEdit means never.
// In a history, it is one edit.
type lastEdit struct {
	by string
	at time.Time
}

// record makes e the last edit and adds it to history, oldest first. As on
// GitHub, the first edit also adds the original post, by its author.
func record(history *[]lastEdit, last *lastEdit, author string, created time.Time, e lastEdit) {
	if len(*history) == 0 {
		*history = append(*history, lastEdit{author, created})
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
	f := &Fake{repo: repo, slug: slug, mux: http.NewServeMux(), now: Start, lastID: 1000, historyCap: 100}
	f.mux.HandleFunc("POST /app/installations/{id}/access_tokens", mintToken)
	f.handle("GET /issues/{n}", f.getIssue)
	f.handle("GET /issues/{n}/parent", f.getParent)
	f.handle("GET /issues/{n}/comments", f.listComments)
	f.handle("GET /issues/comments", f.listComments)
	f.handle("POST /issues/{n}/comments", f.addComment)
	f.handle("PATCH /issues/{n}", f.editIssue)
	f.handle("PATCH /issues/comments/{id}", f.editComment)
	f.handle("GET /issues/comments/{id}/reactions", f.listReactions)
	f.handle("POST /issues/comments/{id}/reactions", f.addReaction)
	f.handle("POST /issues/{n}/labels", f.addLabels)
	f.handle("DELETE /issues/{n}/labels/{name}", f.removeLabel)
	f.mux.HandleFunc("POST /graphql", f.serveGraphQL)
	f.srv = httptest.NewServer(f.mux)
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
func (f *Fake) Client(t testing.TB) *github.Client {
	t.Helper()
	key, err := testKey()
	if err != nil {
		t.Fatal(err)
	}
	c, err := github.New(github.Config{AppID: 1, InstallationID: 1, Key: key, BaseURL: f.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// AddIssue files an issue by author and returns its number.
func (f *Fake) AddIssue(author, title, body string, labels ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.tick()
	is := &issue{Issue: gogithub.Issue{Number: new(len(f.issues) + 1), Title: &title, Body: &body, State: new("open"),
		User: user(author), CreatedAt: now, UpdatedAt: now}}
	for _, l := range labels {
		is.addLabel(l)
	}
	f.issues = append(f.issues, is)
	return is.GetNumber()
}

// SetType sets an issue's type, as on org repos.
func (f *Fake) SetType(number int, name string) {
	f.edit(number, func(is *issue) { is.Type = &gogithub.IssueType{Name: &name} })
}

// SetPullRequest makes number a pull request, which REST lists with the issues.
func (f *Fake) SetPullRequest(number int) {
	f.edit(number, func(is *issue) {
		is.PullRequestLinks = &gogithub.PullRequestLinks{URL: new(fmt.Sprintf("%s/repos/%s/pulls/%d", f.srv.URL, f.repo, number))}
	})
}

// SetParent makes number a sub-issue of parent.
func (f *Fake) SetParent(number, parent int) { f.edit(number, func(is *issue) { is.parent = parent }) }

func (f *Fake) edit(number int, change func(*issue)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f.issues[number-1])
}

// AddComment posts a comment by author and returns its ID.
func (f *Fake) AddComment(number int, author, body string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addCommentBy(f.issues[number-1], author, body).GetID()
}

// EditBody edits an issue's body as login. An empty login means GitHub names no editor.
func (f *Fake) EditBody(number int, login, body string) {
	f.edit(number, func(is *issue) { is.editBy(login, body, f.tick()) })
}

// EditComment edits a comment as login. An empty login means GitHub names no editor.
func (f *Fake) EditComment(id int64, login, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.findComment(id).editBy(login, body, f.tick())
}

// DeleteComment deletes a comment, as its author or a maintainer can.
func (f *Fake) DeleteComment(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = slices.DeleteFunc(f.comments, func(c *comment) bool { return c.GetID() == id })
}

// AddReaction reacts to a comment as author and returns the reaction's ID.
func (f *Fake) AddReaction(commentID int64, author, content string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, _ := f.react(f.findComment(commentID), author, content)
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
// path naming a missing issue or comment gets a 404. A read gets an ETag over
// its body and Link header, and a 304 if that matches If-None-Match.
func (f *Fake) serve(w http.ResponseWriter, r *http.Request, write bool, h handler) {
	f.mu.Lock()
	defer f.mu.Unlock()
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

// react adds a reaction unless author has it already, and reports whether it did.
func (f *Fake) react(c *comment, author, content string) (*gogithub.Reaction, bool) {
	for _, r := range c.reactions {
		if r.GetUser().GetLogin() == author && r.GetContent() == content {
			return r, false
		}
	}
	r := &gogithub.Reaction{ID: new(f.nextID()), User: user(author), Content: &content, CreatedAt: f.tick()}
	c.reactions = append(c.reactions, r)
	return r, true
}

func (f *Fake) findComment(id int64) *comment {
	i := slices.IndexFunc(f.comments, func(c *comment) bool { return c.GetID() == id })
	if i < 0 {
		return nil
	}
	return f.comments[i]
}

func (is *issue) addLabel(name string) {
	if !slices.ContainsFunc(is.Labels, func(l *gogithub.Label) bool { return strings.EqualFold(l.GetName(), name) }) {
		is.Labels = append(is.Labels, &gogithub.Label{Name: name})
		slices.SortFunc(is.Labels, func(a, b *gogithub.Label) int { return strings.Compare(a.GetName(), b.GetName()) })
	}
}

func user(login string) *gogithub.User {
	if strings.HasSuffix(login, "[bot]") {
		return &gogithub.User{Login: &login, Type: new("Bot")}
	}
	return &gogithub.User{Login: &login, Type: new("User")}
}

// page returns the page of items that per_page and page ask for, and links the next.
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
		c.w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?%s>; rel="next"`, c.r.Host, c.r.URL.Path, q.Encode()))
	}
	return http.StatusOK, append([]T{}, items[min((n-1)*per, len(items)):min(n*per, len(items))]...)
}

func (f *Fake) getIssue(c *call) (int, any) { return http.StatusOK, c.issue.Issue }

func (f *Fake) getParent(c *call) (int, any) {
	if c.issue.parent == 0 {
		return http.StatusNotFound, message("Not Found")
	}
	return http.StatusOK, f.issues[c.issue.parent-1].Issue
}

// listComments serves one issue's comments oldest first, or the repo's,
// sorted by update for sort=updated, from since= on.
func (f *Fake) listComments(c *call) (int, any) {
	var from time.Time
	if s := c.r.URL.Query().Get("since"); s != "" {
		var err error
		if from, err = time.Parse(time.RFC3339, s); err != nil {
			return http.StatusUnprocessableEntity, message(err.Error())
		}
	}
	list := slices.DeleteFunc(slices.Clone(f.comments), func(cm *comment) bool {
		return (c.issue != nil && cm.issue != c.issue.GetNumber()) || cm.GetUpdatedAt().Before(from)
	})
	if c.issue == nil && c.r.URL.Query().Get("sort") == "updated" {
		slices.SortStableFunc(list, func(a, b *comment) int { return a.GetUpdatedAt().Compare(b.GetUpdatedAt().Time) })
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

// editIssue edits an issue's body. Nothing else of an issue is edited yet.
func (f *Fake) editIssue(c *call) (int, any) {
	var req struct{ Body *string }
	if status, msg := c.decode(&req); msg != nil {
		return status, msg
	}
	if req.Body == nil {
		return http.StatusUnprocessableEntity, message("githubtest: only the body can be edited")
	}
	c.issue.editBy(f.slug+"[bot]", *req.Body, f.tick())
	return http.StatusOK, c.issue.Issue
}

func (is *issue) editBy(login, body string, now *gogithub.Timestamp) {
	is.Body, is.UpdatedAt = &body, now
	record(&is.history, &is.lastEdit, is.GetUser().GetLogin(), is.GetCreatedAt().Time, lastEdit{login, now.Time})
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
	c.Body, c.UpdatedAt = &body, now
	record(&c.history, &c.lastEdit, c.GetUser().GetLogin(), c.GetCreatedAt().Time, lastEdit{login, now.Time})
}

func (f *Fake) listReactions(c *call) (int, any) {
	return page(c, c.comment.reactions)
}

// addReaction answers 201 for a new reaction, and 200 for one the App has already.
func (f *Fake) addReaction(c *call) (int, any) {
	var req struct{ Content string }
	if status, msg := c.decode(&req); msg != nil {
		return status, msg
	}
	r, created := f.react(c.comment, f.slug+"[bot]", req.Content)
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
	for _, n := range names {
		c.issue.addLabel(n)
	}
	c.issue.UpdatedAt = f.tick()
	return http.StatusOK, c.issue.Labels
}

func (f *Fake) removeLabel(c *call) (int, any) {
	i := slices.IndexFunc(c.issue.Labels, func(l *gogithub.Label) bool { return strings.EqualFold(l.GetName(), c.r.PathValue("name")) })
	if i < 0 {
		return http.StatusNotFound, message("Label does not exist")
	}
	c.issue.Labels, c.issue.UpdatedAt = slices.Delete(c.issue.Labels, i, i+1), f.tick()
	return http.StatusOK, c.issue.Labels
}
