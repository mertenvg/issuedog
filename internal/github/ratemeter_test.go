package github_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// rateMeterClient is a client of f whose answers rewrite may change, below
// the client's meter, as GitHub's own headers would differ.
func rateMeterClient(t *testing.T, f *githubtest.Fake, rewrite func(*http.Response)) *github.Client {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	key := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	c, err := github.New(github.Config{AppID: githubtest.AppID, InstallationID: 1, Key: key, BaseURL: f.URL(),
		Transport: rateMeterRewrite{next: f.Transport(), rewrite: rewrite}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// rateMeterRewrite hands each answer to rewrite, but a token mint's.
type rateMeterRewrite struct {
	next    http.RoundTripper
	rewrite func(*http.Response)
}

func (t rateMeterRewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err == nil && req.URL.Path != "/app/installations/1/access_tokens" {
		t.rewrite(resp)
	}
	return resp, err
}

// The meter counts each answer by its X-RateLimit-Resource, a 304 apart
// from the answers that cost a request, though the ETag cache hands the
// 304 on as a 200, and keeps each resource's newest remaining and limit:
// core for REST, search and GraphQL. Take resets the counts but keeps the
// rates, and neither counting nor Take makes a request.
func TestRateMeterCountsByResource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	c := f.Client(t)
	read := func() {
		t.Helper()
		if _, _, err := c.Issues.Get(ctx, "o", "r", n); err != nil {
			t.Fatal(err)
		}
	}
	read()
	read() // a 304
	if _, err := c.ReadyBlocked(ctx, "o", "r"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.IsPinned(ctx, "o", "r", n); err != nil {
		t.Fatal(err)
	}
	requests := len(f.Requests())
	got := c.Meter.Take()
	want := github.PassCost{Resources: map[string]github.Usage{
		"core":    {Requests: 1, NotModified: 1, Rate: github.Rate{Remaining: 4999, Limit: 5000}},
		"search":  {Requests: 1, Rate: github.Rate{Remaining: 29, Limit: 30}},
		"graphql": {Requests: 1, Rate: github.Rate{Remaining: 4999, Limit: 5000}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the first Take = %+v, want %+v", got, want)
	}
	if got.Requests() != 3 || got.NotModified() != 1 {
		t.Errorf("the first Take's totals are %d requests and %d 304s, want 3 and 1", got.Requests(), got.NotModified())
	}
	got = c.Meter.Take()
	kept := github.PassCost{Resources: map[string]github.Usage{
		"core":    {Rate: github.Rate{Remaining: 4999, Limit: 5000}},
		"search":  {Rate: github.Rate{Remaining: 29, Limit: 30}},
		"graphql": {Rate: github.Rate{Remaining: 4999, Limit: 5000}},
	}}
	if !reflect.DeepEqual(got, kept) {
		t.Errorf("a Take with no answer since = %+v, want no counts and the rates kept: %+v", got, kept)
	}
	read() // a 304 again
	got = c.Meter.Take()
	kept.Resources["core"] = github.Usage{NotModified: 1, Rate: github.Rate{Remaining: 4999, Limit: 5000}}
	if !reflect.DeepEqual(got, kept) {
		t.Errorf("a Take after a 304 = %+v, want %+v", got, kept)
	}
	if len(f.Requests()) != requests+1 {
		t.Errorf("the fake got %d requests since the reads, want the one read alone: neither counting nor Take asks", len(f.Requests())-requests)
	}
}

// An answer counts under its X-RateLimit-Resource, any name GitHub gives,
// and under core with none. Its Remaining and Limit become its resource's
// rate, with its Reset when it has one. An answer with no rate keeps the
// rate an earlier one gave.
func TestRateMeterHeaders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	var mu sync.Mutex
	var headers map[string]string
	c := rateMeterClient(t, f, func(resp *http.Response) {
		mu.Lock()
		defer mu.Unlock()
		for k, v := range headers {
			if v == "" {
				resp.Header.Del(k)
			} else {
				resp.Header.Set(k, v)
			}
		}
	})
	answer := func(h map[string]string) {
		t.Helper()
		mu.Lock()
		headers = h
		mu.Unlock()
		// A comment, a write, so the ETag cache keeps none of it.
		if _, _, err := c.Issues.CreateComment(ctx, "o", "r", n, gogithub.IssueCommentRequest{Body: "a comment"}); err != nil {
			t.Fatal(err)
		}
	}
	reset := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	answer(map[string]string{"X-RateLimit-Reset": "1767229200"})
	answer(map[string]string{"X-RateLimit-Resource": "", "X-RateLimit-Remaining": "4000", "X-RateLimit-Limit": "4500"})
	answer(map[string]string{"X-RateLimit-Resource": "code_search", "X-RateLimit-Remaining": "9", "X-RateLimit-Limit": "10"})
	answer(map[string]string{"X-RateLimit-Resource": "code_search", "X-RateLimit-Remaining": "", "X-RateLimit-Limit": ""})
	answer(map[string]string{"X-RateLimit-Resource": "odd name", "X-RateLimit-Remaining": "x", "X-RateLimit-Limit": "10"})
	got := c.Meter.Take()
	want := github.PassCost{Resources: map[string]github.Usage{
		"core":        {Requests: 2, Rate: github.Rate{Remaining: 4000, Limit: 4500}},
		"code_search": {Requests: 2, Rate: github.Rate{Remaining: 9, Limit: 10}},
		"odd name":    {Requests: 1},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Take = %+v, want %+v", got, want)
	}
	answer(map[string]string{"X-RateLimit-Reset": "1767229200"})
	if got := c.Meter.Take().Resources["core"]; got.Rate.Reset != reset || got.Rate.Remaining != f.RateRemaining("core") {
		t.Errorf("core after an answer with a reset = %+v, want the reset %v and the fake's remaining %d", got, reset, f.RateRemaining("core"))
	}
}

// Answers counted from many goroutines at once, with Takes between them,
// add up to every answer, each counted once.
func TestRateMeterConcurrent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := githubtest.New(t, "o/r", "issuedog")
	const workers, reads = 4, 10
	var numbers []int
	for range workers {
		numbers = append(numbers, f.AddIssue("owner", "Title", "body"))
	}
	c := f.Client(t)
	var mu sync.Mutex
	var requests, notModified int
	take := func() {
		cost := c.Meter.Take()
		mu.Lock()
		defer mu.Unlock()
		requests += cost.Requests()
		notModified += cost.NotModified()
	}
	var wg sync.WaitGroup
	for _, n := range numbers {
		wg.Go(func() {
			for range reads {
				if _, _, err := c.Issues.Get(ctx, "o", "r", n); err != nil {
					t.Error(err)
					return
				}
				take()
			}
		})
	}
	wg.Wait()
	take()
	if requests != workers || notModified != workers*(reads-1) {
		t.Errorf("the Takes counted %d requests and %d 304s, want %d and %d", requests, notModified, workers, workers*(reads-1))
	}
}

// A pass's cost is one line: its requests and 304s, then each resource by
// name with its remaining of its limit, and its own counts if it had any.
// A name that isn't a plain word is quoted, as GitHub wrote it.
func TestPassCostString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cost github.PassCost
		want string
	}{
		{"nothing yet", github.PassCost{}, "0 requests, 0 304s"},
		{"an unchanged pass", github.PassCost{Resources: map[string]github.Usage{
			"search": {Rate: github.Rate{Remaining: 29, Limit: 30}},
			"core":   {NotModified: 14, Rate: github.Rate{Remaining: 4986, Limit: 5000}},
		}}, "0 requests, 14 304s; core 4986/5000 left (0 requests, 14 304s), search 29/30 left"},
		{"one of each", github.PassCost{Resources: map[string]github.Usage{
			"core":    {Requests: 1, NotModified: 1, Rate: github.Rate{Remaining: 4985, Limit: 5000}},
			"graphql": {Requests: 2, Rate: github.Rate{Remaining: 4998, Limit: 5000}},
		}}, "3 requests, 1 304; core 4985/5000 left (1 request, 1 304), graphql 4998/5000 left (2 requests, 0 304s)"},
		{"no rate, and a name to quote", github.PassCost{Resources: map[string]github.Usage{
			"core":       {Requests: 1},
			"odd\nname!": {Requests: 1, Rate: github.Rate{Remaining: 0, Limit: 10}},
		}}, `2 requests, 0 304s; core with no rate yet (1 request, 0 304s), "odd\nname!" 0/10 left (1 request, 0 304s)`},
	}
	for _, tt := range tests {
		if got := tt.cost.String(); got != tt.want {
			t.Errorf("%s: String() = %q, want %q", tt.name, got, tt.want)
		}
	}
}
