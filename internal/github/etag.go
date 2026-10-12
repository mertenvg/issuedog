package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// maxCacheBytes bounds the cache, since a moving since= leaves stale entries.
const maxCacheBytes = 64 << 20

// etagCache makes GETs conditional (§8.1): it keeps each 200 with an ETag by Accept
// and URL, and turns a free 304 into that 200 for go-github. It is memory only (§5),
// one per Client above auth, so installations never share it.
type etagCache struct {
	next    http.RoundTripper
	mu      sync.Mutex
	entries map[string]*etagEntry
	size    int
}

type etagEntry struct {
	etag   string
	header http.Header
	body   []byte
}

func newETagCache(next http.RoundTripper) *etagCache {
	return &etagCache{next: next, entries: map[string]*etagEntry{}}
}

func (c *etagCache) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.roundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("github conditional %s %s: %w", req.Method, req.URL.Path, err)
	}
	return resp, nil
}

func (c *etagCache) roundTrip(req *http.Request) (*http.Response, error) {
	// Leave other methods and the caller's own conditional GETs alone.
	if req.Method != http.MethodGet || req.Header.Get("If-None-Match") != "" || req.Header.Get("If-Modified-Since") != "" {
		return c.next.RoundTrip(req)
	}
	key := req.Header.Get("Accept") + " " + req.URL.String()
	c.mu.Lock()
	cached := c.entries[key]
	c.mu.Unlock()
	if cached != nil {
		req = req.Clone(req.Context())
		req.Header.Set("If-None-Match", cached.etag)
	}
	resp, err := c.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotModified && cached != nil:
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		// The 304 carries fresh rate-limit headers; keep the cached rest,
		// such as the Link header go-github paginates with.
		h := cached.header.Clone()
		maps.Copy(h, resp.Header)
		fill(resp, h, cached.body)
	case resp.StatusCode == http.StatusOK && resp.Header.Get("ETag") != "":
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read body: %w", err)
		}
		if !fullLastPage(req, resp.Header, body) {
			c.store(key, &etagEntry{etag: resp.Header.Get("ETag"), header: resp.Header.Clone(), body: body})
		}
		fill(resp, resp.Header, body)
	}
	return resp, nil
}

// store replaces the entry for key, starting over past maxCacheBytes.
func (c *etagCache) store(key string, e *etagEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.entries[key]; old != nil {
		c.size -= len(old.body)
	}
	if c.size+len(e.body) > maxCacheBytes {
		clear(c.entries)
		c.size = 0
	}
	c.entries[key] = e
	c.size += len(e.body)
}

// fullLastPage spots a full page with no next: its Link-less 304 would hide a new page.
func fullLastPage(req *http.Request, h http.Header, body []byte) bool {
	n, err := strconv.Atoi(req.URL.Query().Get("per_page"))
	if err != nil || strings.Contains(h.Get("Link"), `rel="next"`) {
		return false
	}
	var items []json.RawMessage
	return json.Unmarshal(body, &items) == nil && len(items) == n
}

// fill turns resp into a 200 with its own copy of body, so readers never share bytes.
func fill(resp *http.Response, h http.Header, body []byte) {
	resp.StatusCode, resp.Status = http.StatusOK, "200 OK"
	resp.Header = h
	resp.Header.Del("Content-Encoding")
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.ContentLength = int64(len(body))
	resp.Body = io.NopCloser(bytes.NewReader(bytes.Clone(body)))
}
