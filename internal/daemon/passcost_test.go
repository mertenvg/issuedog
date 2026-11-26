package daemon_test

import (
	"context"
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/logtest"
)

// passCostLimits are the fake's rate limits, by resource.
var passCostLimits = map[string]int{"core": 5000, "search": 30, "graphql": 5000}

// passCostRig is the pick's rig, run through Run, one pass at a time, with
// the cost the fake saw so far: each resource's rate, kept from pass to
// pass as the client's meter keeps it.
type passCostRig struct {
	g     *rig
	rates map[string]github.Rate
}

// pass runs one pass of the rig's daemon through Run, which logs its
// cost. Work, the pass's last step but for the unassigns put off, which
// this rig has none of, ends Run's context, so Run returns at its sleep.
// It returns the cost the fake saw in the pass, by its request log and
// RateRemaining, and checks that Run logged it, on one line.
func (p *passCostRig) pass(name string) github.PassCost {
	t := p.g.t
	t.Helper()
	f := p.g.f
	from := len(f.Requests())
	stop := logtest.Capture(t, logr.I)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	work := p.g.d.Work
	p.g.d.Work = func(ctx context.Context, ready []daemon.Ready) error {
		defer cancel()
		return work(ctx, ready)
	}
	p.g.d.Run(ctx)
	p.g.d.Work = work
	logs := stop()
	want := github.PassCost{Resources: map[string]github.Usage{}}
	for _, r := range f.Requests()[from:] {
		if strings.HasPrefix(r.Path, "/app/installations/") { // a token mint, below the meter
			continue
		}
		resource := "core"
		switch {
		case strings.HasPrefix(r.Path, "/search/"):
			resource = "search"
		case r.Path == "/graphql":
			resource = "graphql"
		}
		u := want.Resources[resource]
		if r.Status == http.StatusNotModified {
			u.NotModified++
		} else {
			u.Requests++
		}
		want.Resources[resource] = u
		p.rates[resource] = github.Rate{Remaining: f.RateRemaining(resource), Limit: passCostLimits[resource]}
	}
	for resource, rate := range p.rates {
		u := want.Resources[resource]
		u.Rate = rate
		want.Resources[resource] = u
	}
	line := "pass on o/r cost " + want.String()
	if strings.Count(logs, "pass on o/r cost") != 1 || !strings.Contains(logs, line+"\n") {
		t.Errorf("%s: Run logged\n%s\nwant one line of the pass's cost: %q", name, logs, line)
	}
	return want
}

// Each pass Run makes logs one line of what it cost against the rate
// limit (SPEC §8.1, Polling cost): its requests, its 304s, and each
// resource's remaining of its limit, as the fake counted them. The
// start's first pass reads every issue, makes the pick's search, as a
// ready task waits, and reads the edit history of a spec its author
// edited, a GraphQL read. An unchanged pass logs no request but 304s,
// with each remaining as it was: counting makes no request. An issue
// changed since costs requests again, its edit history's read a GraphQL
// one.
func TestRunLogsPassCost(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logtest.Capture).
	f := newFake(t)
	waiting := pickReady(f, "p1")
	f.SetBlockedBy(waiting, f.AddIssue("owner", "Something first", ""))
	spec := f.AddIssue("owner", "Issue", "the spec")
	f.AddLabel(spec, app, "stage:spec-review")
	f.EditBody(spec, "owner", "the spec, edited")
	f.AddComment(spec, app, revision(1, "the spec, edited"))
	p := &passCostRig{g: pickRig(t, f), rates: map[string]github.Rate{}}

	first := p.pass("the start's first pass")
	if first.Resources["search"].Requests != 1 || first.Resources["graphql"].Requests == 0 || first.Resources["core"].Requests == 0 {
		t.Errorf("the start's first pass cost %v; want the pick's search, a GraphQL read and core requests", first)
	}
	p.pass("the pass after the start's first") // whose reads may still miss the ETag cache
	from := len(f.Requests())
	unchanged := p.pass("an unchanged pass")
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("an unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}
	if unchanged.Requests() != 0 || unchanged.NotModified() == 0 {
		t.Errorf("an unchanged pass cost %v; want no request but 304s", unchanged)
	}
	if again := p.pass("another unchanged pass"); !maps.Equal(again.Resources, unchanged.Resources) {
		t.Errorf("another unchanged pass cost %v; want what the last cost, %v, with each remaining as it was", again, unchanged)
	}
	f.EditBody(spec, "owner", "the spec, edited again")
	changed := p.pass("a pass after an edit")
	if changed.Resources["core"].Requests == 0 || changed.Resources["graphql"].Requests == 0 {
		t.Errorf("a pass after an edit cost %v; want core requests, and a GraphQL read of the edit history", changed)
	}
	for resource, u := range changed.Resources {
		if was := unchanged.Resources[resource].Rate.Remaining; u.Rate.Remaining != was-u.Requests {
			t.Errorf("%s has %d left after a pass of %d requests, from %d; want each request to cost one", resource, u.Rate.Remaining, u.Requests, was)
		}
	}
	if p.g.runs() != 0 {
		t.Errorf("claude ran %d times, want none, as the ready task is blocked", p.g.runs())
	}
}
