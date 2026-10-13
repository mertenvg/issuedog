package githubtest

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v92/github"
)

// Request is one request the fake answered: its method, its path below the
// repo (or from the root, as /app and /graphql), its query, its status, and
// whether it was conditional (If-None-Match).
type Request struct {
	Method, Path, Query string
	Status              int
	Conditional         bool
}

// Requests returns every request so far, in order.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

// URL is the API root, for github.Config.BaseURL.
func (f *Fake) URL() string { return f.srv.URL }

// SetPermission gives login a role on the repo, such as "admin", "write" or
// "read", for the permission endpoint. A login with none gets a 404.
func (f *Fake) SetPermission(login, role string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.perms == nil {
		f.perms = map[string]string{}
	}
	f.perms[strings.ToLower(login)] = role
}

// AddRepoLabel creates a repo label, as the owner would by hand.
func (f *Fake) AddRepoLabel(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.labels = append(f.labels, &gogithub.Label{Name: name})
}

// RepoLabels returns the repo's label names, oldest first.
func (f *Fake) RepoLabels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for _, l := range f.labels {
		names = append(names, l.GetName())
	}
	return names
}

// Close closes an issue.
func (f *Fake) Close(number int) {
	f.edit(number, func(is *issue) { is.State, is.UpdatedAt = new("closed"), f.tick() })
}

// DeleteIssue deletes the newest issue and its comments, after which its
// paths answer 404. Issues are kept by number, so only the newest can go.
func (f *Fake) DeleteIssue() {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.issues)
	f.issues = f.issues[:n-1]
	f.comments = slices.DeleteFunc(f.comments, func(c *comment) bool { return c.issue == n })
}

// Transfer gives an issue the repository URL of repo ("owner/name"), as a
// read of an issue transferred there does once its 301 is followed.
func (f *Fake) Transfer(number int, repo string) {
	f.edit(number, func(is *issue) { is.RepositoryURL = new(f.srv.URL + "/repos/" + repo) })
}

// AnswerOnce answers the next request that match accepts with status alone,
// as a proxy or a glitch might, without serving it.
func (f *Fake) AnswerOnce(match func(Request) bool, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answerOnce = func(r Request) int {
		if !match(r) {
			return 0
		}
		f.answerOnce = nil
		return status
	}
}

// BeforeRequest runs do, once, just before the fake answers the next request
// that match accepts (only its Method, Path and Query are set), as when
// something changes while a list pages. do may call the fake's methods.
func (f *Fake) BeforeRequest(match func(Request) bool, do func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beforeReq = func(r Request) func() {
		if !match(r) {
			return nil
		}
		f.beforeReq = nil
		return do
	}
}

// handleApp registers GET /app, the issue list, the repo labels and the
// permission endpoint.
func (f *Fake) handleApp() {
	f.mux.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			send(w, http.StatusUnauthorized, message("GET /app needs the App's JWT"))
			return
		}
		f.serve(w, r, false, func(*call) (int, any) {
			return http.StatusOK, gogithub.App{ID: new(int64(1)), Slug: &f.slug, Name: &f.slug}
		})
	})
	f.handle("GET /issues", f.listIssues)
	f.handle("GET /labels", func(c *call) (int, any) { return page(c, f.labels) })
	f.handle("POST /labels", f.createLabel)
	f.handle("GET /collaborators/{user}/permission", f.permission)
}

// logged adds each request to the log once it's answered.
func (f *Fake) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := Request{Method: r.Method, Path: strings.TrimPrefix(r.URL.Path, "/repos/"+f.repo), Query: r.URL.RawQuery}
		f.mu.Lock()
		var do func()
		if f.beforeReq != nil {
			do = f.beforeReq(req)
		}
		status := 0
		if f.answerOnce != nil {
			status = f.answerOnce(req)
		}
		f.mu.Unlock()
		if do != nil {
			do()
		}
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		if status != 0 {
			send(rec, status, message(http.StatusText(status)))
		} else {
			next.ServeHTTP(rec, r)
		}
		req.Status, req.Conditional = rec.status, r.Header.Get("If-None-Match") != ""
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, req)
	})
}

// recorder keeps the status a handler sent.
type recorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *recorder) WriteHeader(status int) {
	if !r.wrote {
		r.status, r.wrote = status, true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	n, err := r.ResponseWriter.Write(b)
	if err != nil {
		return n, fmt.Errorf("githubtest: write a reply: %w", err)
	}
	return n, nil
}

// listIssues serves the repo's issues and pull requests, open ones unless
// state= says otherwise, from since= on, by number or, for sort=updated, by
// update in direction=.
func (f *Fake) listIssues(c *call) (int, any) {
	q := c.r.URL.Query()
	var from time.Time
	if s := q.Get("since"); s != "" {
		var err error
		if from, err = time.Parse(time.RFC3339, s); err != nil {
			return http.StatusUnprocessableEntity, message(err.Error())
		}
	}
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	out := []gogithub.Issue{}
	for _, is := range f.issues {
		if (state == "all" || is.GetState() == state) && !is.GetUpdatedAt().Before(from) {
			out = append(out, is.Issue)
		}
	}
	if q.Get("sort") == "updated" {
		slices.SortStableFunc(out, func(a, b gogithub.Issue) int { return a.GetUpdatedAt().Compare(b.GetUpdatedAt().Time) })
		if q.Get("direction") != "asc" {
			slices.Reverse(out)
		}
	}
	return page(c, out)
}

// createLabel creates a repo label, or answers 422 if one has the name.
func (f *Fake) createLabel(c *call) (int, any) {
	var l gogithub.Label
	if status, msg := c.decode(&l); msg != nil {
		return status, msg
	}
	if slices.ContainsFunc(f.labels, func(x *gogithub.Label) bool { return strings.EqualFold(x.GetName(), l.GetName()) }) {
		return http.StatusUnprocessableEntity, message("Validation Failed: already_exists")
	}
	f.labels = append(f.labels, &l)
	return http.StatusCreated, l
}

// permission answers with the role SetPermission gave, as both the
// permission and the role name.
func (f *Fake) permission(c *call) (int, any) {
	login := c.r.PathValue("user")
	role, ok := f.perms[strings.ToLower(login)]
	if !ok {
		return http.StatusNotFound, message("Not Found")
	}
	return http.StatusOK, gogithub.RepositoryPermissionLevel{Permission: &role, RoleName: &role, User: user(login)}
}
