package github

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Rate is one resource's primary rate limit as GitHub's newest answer under
// it gave it: the requests left, the limit, and when it resets.
type Rate struct {
	Remaining, Limit int
	Reset            time.Time
}

// Usage is what a pass's answers under one resource cost: those that count
// against its limit, the 304s apart, which don't (SPEC §8.1, Polling
// cost), and its Rate, zero until an answer under it carried one.
type Usage struct {
	Requests, NotModified int
	Rate                  Rate
}

// Meter counts GitHub's answers to a Client's installation requests, by
// their X-RateLimit-Resource, and keeps each resource's newest Rate. It
// only reads the answers' headers, so counting makes no request. It is
// safe for concurrent use.
type Meter struct {
	mu        sync.Mutex
	resources map[string]Usage
}

// PassCost is what a pass cost against the rate limit: its answers by
// resource, with each resource's newest Rate, kept from an earlier pass
// for one this pass didn't ask.
type PassCost struct {
	Resources map[string]Usage
}

// Take returns the cost since the last Take and starts counting again. The
// Rates stay, as the newest GitHub gave.
func (m *Meter) Take() PassCost {
	m.mu.Lock()
	defer m.mu.Unlock()
	cost := PassCost{Resources: maps.Clone(m.resources)}
	for name, u := range m.resources {
		m.resources[name] = Usage{Rate: u.Rate}
	}
	return cost
}

// count adds one answer, read from its headers.
func (m *Meter) count(resp *http.Response) {
	name := resp.Header.Get("X-RateLimit-Resource")
	if name == "" {
		name = "core" // as GitHub names the REST API's own
	}
	rate, ok := rateOf(resp.Header)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.resources == nil {
		m.resources = map[string]Usage{}
	}
	u := m.resources[name]
	if resp.StatusCode == http.StatusNotModified {
		u.NotModified++
	} else {
		u.Requests++
	}
	if ok {
		u.Rate = rate
	}
	m.resources[name] = u
}

// rateOf reads a Rate from an answer's headers, reporting whether it had
// one: a Remaining and a Limit. A Reset is kept when there's one.
func rateOf(h http.Header) (Rate, bool) {
	remaining, rerr := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	limit, lerr := strconv.Atoi(h.Get("X-RateLimit-Limit"))
	if rerr != nil || lerr != nil {
		return Rate{}, false
	}
	rate := Rate{Remaining: remaining, Limit: limit}
	if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		rate.Reset = time.Unix(reset, 0).UTC()
	}
	return rate, true
}

// Requests is the pass's answers that count against a limit, all
// resources' together.
func (p PassCost) Requests() int {
	n := 0
	for _, u := range p.Resources {
		n += u.Requests
	}
	return n
}

// NotModified is the pass's 304s, all resources' together.
func (p PassCost) NotModified() int {
	n := 0
	for _, u := range p.Resources {
		n += u.NotModified
	}
	return n
}

// String is the pass's cost on one line: its requests and 304s, then each
// resource, by name, with its remaining of its limit, and its own requests
// and 304s when it had any, such as
// "1 request, 14 304s; core 4986/5000 left (1 request, 14 304s), search 29/30 left".
// A resource name GitHub gave that isn't a plain word is quoted.
func (p PassCost) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s, %s", plural(p.Requests(), "request"), plural(p.NotModified(), "304"))
	for i, name := range slices.Sorted(maps.Keys(p.Resources)) {
		u := p.Resources[name]
		sep := ", "
		if i == 0 {
			sep = "; "
		}
		b.WriteString(sep + plainName(name))
		if u.Rate.Limit != 0 {
			fmt.Fprintf(&b, " %d/%d left", u.Rate.Remaining, u.Rate.Limit)
		} else {
			b.WriteString(" with no rate yet")
		}
		if u.Requests != 0 || u.NotModified != 0 {
			fmt.Fprintf(&b, " (%s, %s)", plural(u.Requests, "request"), plural(u.NotModified, "304"))
		}
	}
	return b.String()
}

// plural is n with what, plural but for one.
func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return strconv.Itoa(n) + " " + what + "s"
}

// plainName is a resource's name as GitHub gave it, quoted unless it's
// only lower-case letters, digits and underscores, as GitHub's own are.
func plainName(name string) string {
	if name != "" && strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789_") == "" {
		return name
	}
	return strconv.Quote(name)
}

// meterTransport counts each answer next gives in m. It sits below the
// ETag cache, so it sees GitHub's own 304s before the cache turns them
// into 200s (Client.Meter).
type meterTransport struct {
	next http.RoundTripper
	m    *Meter
}

func (t meterTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("rate meter: %w", err)
	}
	t.m.count(resp)
	return resp, nil
}
