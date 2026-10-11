// Package egress is the HTTP CONNECT proxy that agent processes use for all
// network access. It only tunnels to hosts listed in network.allow.
package egress

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mertenvg/logr/v2"
)

const dialTimeout = 10 * time.Second

// Proxy tunnels CONNECT requests to allowed hosts on ports 443 and 80, and
// answers 403 to everything else, including plain (non-CONNECT) requests.
type Proxy struct {
	allow map[string]bool
	dial  func(ctx context.Context, network, addr string) (net.Conn, error)
}

// New returns a proxy that allows exactly the given host names. Matching
// ignores case, but nothing else: no wildcards, subdomains or IP addresses.
func New(hosts []string) *Proxy {
	allow := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		allow[strings.ToLower(h)] = true
	}
	d := &net.Dialer{Timeout: dialTimeout}
	return &Proxy{allow: allow, dial: d.DialContext}
}

// Listen opens a listener for the proxy on 127.0.0.1. Port 0 picks a free
// port. Serve it with http.Serve(l, p).
func Listen(port int) (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}

// allowed reports whether a CONNECT to authority (host:port) may go through.
func (p *Proxy) allowed(authority string) bool {
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		return false
	}
	if port != "443" && port != "80" {
		return false
	}
	if net.ParseIP(host) != nil {
		return false
	}
	return p.allow[strings.ToLower(host)]
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect || !p.allowed(r.Host) {
		logr.With(logr.M("method", r.Method).With("host", r.Host)).Warn("egress denied")
		http.Error(w, "egress not allowed", http.StatusForbidden)
		return
	}

	upstream, err := p.dial(r.Context(), "tcp", r.Host)
	if err != nil {
		logr.With(logr.M("host", r.Host).With("error", err.Error())).Warn("egress dial failed")
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "tunneling not supported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		client.Close()
		upstream.Close()
		return
	}
	relay(client, buf.Reader, upstream)
}

// relay copies bytes both ways until either side closes, then closes both.
// Reads from the client go through br, which may hold bytes the server
// buffered before the hijack.
func relay(client net.Conn, br *bufio.Reader, upstream net.Conn) {
	var once sync.Once
	closeBoth := func() {
		client.Close()
		upstream.Close()
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(upstream, br)
		once.Do(closeBoth)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, upstream)
		once.Do(closeBoth)
	}()
	wg.Wait()
}
