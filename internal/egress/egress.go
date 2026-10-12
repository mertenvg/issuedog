// Package egress is the HTTP CONNECT proxy that agent processes use for all
// network access. It only tunnels to hosts listed in network.allow.
package egress

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
// ignores ASCII case, but nothing else: no wildcards, subdomains, IP
// addresses or non-ASCII names.
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

// allowed reports whether a CONNECT to authority (host:port) may go through,
// and returns the canonical authority to dial, so the name dialed is exactly
// the name checked.
func (p *Proxy) allowed(authority string) (string, bool) {
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		return "", false
	}
	if port != "443" && port != "80" {
		return "", false
	}
	if net.ParseIP(host) != nil {
		return "", false
	}
	// Host names are ASCII. Refusing other bytes keeps Unicode case folding
	// (such as the Kelvin sign lowering to "k") from matching a listed name.
	for i := 0; i < len(host); i++ {
		if host[i] >= 0x80 {
			return "", false
		}
	}
	host = strings.ToLower(host)
	if !p.allow[host] {
		return "", false
	}
	return net.JoinHostPort(host, port), true
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target, ok := p.allowed(r.Host)
	if r.Method != http.MethodConnect || !ok {
		logr.With(logr.M("method", r.Method).With("host", r.Host)).Warn("egress denied")
		http.Error(w, "egress not allowed", http.StatusForbidden)
		return
	}

	upstream, err := p.dial(r.Context(), "tcp", target)
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
	relay(target, client, buf.Reader, upstream)
}

// relay copies bytes both ways until either side closes, then closes both.
// Reads from the client go through br, which may hold bytes the server
// buffered before the hijack.
func relay(target string, client net.Conn, br *bufio.Reader, upstream net.Conn) {
	var once sync.Once
	closeBoth := func() {
		client.Close()
		upstream.Close()
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := io.Copy(upstream, br)
		logCopy(target, "to upstream", err)
		once.Do(closeBoth)
	}()
	go func() {
		defer wg.Done()
		_, err := io.Copy(client, upstream)
		logCopy(target, "to client", err)
		once.Do(closeBoth)
	}()
	wg.Wait()
}

// logCopy logs how one direction of a tunnel ended. A side closing or
// resetting is how tunnels end, so that is debug; anything else is a warning.
func logCopy(target, direction string, err error) {
	if err == nil {
		return
	}
	l := logr.With(logr.M("host", target).With("direction", direction).With("error", err.Error()))
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		l.Debug("egress tunnel closed")
		return
	}
	l.Warn("egress tunnel copy failed")
}
