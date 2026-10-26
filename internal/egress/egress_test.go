package egress

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/logtest"
	"github.com/mertenvg/logr/v2"
)

const allowedHost = "api.anthropic.com"

// echoServer accepts connections and echoes back whatever it reads.
func echoServer(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return l.Addr().String()
}

// startProxy runs a proxy that allows allowedHost and sends every dial to
// upstream. dials counts the dial attempts.
func startProxy(t *testing.T, upstream string) (addr string, dials *atomic.Int32) {
	t.Helper()
	return startProxyDialing(t, func(ctx context.Context, network string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, upstream)
	})
}

// startProxyDialing runs a proxy that allows allowedHost and makes every
// dial with dial. dials counts the dial attempts.
func startProxyDialing(t *testing.T, dial func(ctx context.Context, network string) (net.Conn, error)) (addr string, dials *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	p := New([]string{allowedHost, "pkg.example"})
	p.dial = func(ctx context.Context, network, target string) (net.Conn, error) {
		n.Add(1)
		host, _, _ := net.SplitHostPort(target)
		if host != allowedHost && host != "pkg.example" {
			t.Errorf("dialed %q, want a canonical listed host", target)
		}
		return dial(ctx, network)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), &n
}

// connect sends a CONNECT for authority through the proxy and returns the
// response and the open connection.
func connect(t *testing.T, proxy, authority string) (*http.Response, net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, "CONNECT "+authority+" HTTP/1.1\r\nHost: "+authority+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	return resp, c, br
}

func TestConnectAllowedHostRelays(t *testing.T) {
	proxy, dials := startProxy(t, echoServer(t))
	for _, authority := range []string{allowedHost + ":443", allowedHost + ":80", "API.Anthropic.COM:443"} {
		resp, c, br := connect(t, proxy, authority)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", authority, resp.StatusCode)
		}
		if _, err := io.WriteString(c, "ping\n"); err != nil {
			t.Fatal(err)
		}
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("%s: reading echo: %v", authority, err)
		}
		if line != "ping\n" {
			t.Errorf("%s: echo %q, want %q", authority, line, "ping\n")
		}
	}
	if dials.Load() != 3 {
		t.Errorf("dialed %d times, want 3", dials.Load())
	}
}

func TestConnectDenied(t *testing.T) {
	proxy, dials := startProxy(t, echoServer(t))
	for _, authority := range []string{
		"example.com:443",                 // not listed
		"api.anthropic.com.evil.test:443", // lookalike suffix
		"evil.api.anthropic.com:443",      // subdomain
		"api.anthropic.com.:443",          // trailing dot
		"127.0.0.1:443",                   // IPv4 literal
		"[::1]:443",                       // IPv6 literal
		"160.79.104.10:443",               // an IP, even if a listed host resolves to it
		allowedHost + ":8443",             // listed host, other port
		allowedHost + ":22",               // listed host, other port
		allowedHost,                       // no port
	} {
		resp, _, _ := connect(t, proxy, authority)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", authority, resp.StatusCode)
		}
	}
	if dials.Load() != 0 {
		t.Errorf("dialed %d times, want 0", dials.Load())
	}
}

func TestPlainRequestsDenied(t *testing.T) {
	proxy, dials := startProxy(t, echoServer(t))
	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(mustURL(t, "http://"+proxy)),
	}}
	for _, url := range []string{"http://example.com/", "http://" + allowedHost + ":80/", "http://" + allowedHost + ":443/"} {
		resp, err := client.Get(url)
		if err != nil {
			t.Fatalf("%s: %v", url, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", url, resp.StatusCode)
		}
	}
	if dials.Load() != 0 {
		t.Errorf("dialed %d times, want 0", dials.Load())
	}
}

// TestConnectUpstreamDown checks that a failed dial to the upstream is a
// 502. The dial is refused on purpose: a closed listener's port could be
// taken by another test binary, which would answer and make it a 200.
func TestConnectUpstreamDown(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
	proxy, dials := startProxyDialing(t, func(context.Context, string) (net.Conn, error) {
		return nil, refused
	})
	resp, _, _ := connect(t, proxy, allowedHost+":443")
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status %d, want 502", resp.StatusCode)
	}
	if dials.Load() != 1 {
		t.Errorf("dialed %d times, want 1", dials.Load())
	}
}

func TestListenLoopbackOnly(t *testing.T) {
	l, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	addr := l.Addr().(*net.TCPAddr)
	if !addr.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Errorf("listening on %s, want 127.0.0.1", addr.IP)
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// A client may send tunnel bytes in the same write as the CONNECT. The proxy
// must forward the bytes the server read ahead before the hijack.
func TestConnectForwardsBytesSentWithRequest(t *testing.T) {
	proxy, _ := startProxy(t, echoServer(t))
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	authority := allowedHost + ":443"
	req := "CONNECT " + authority + " HTTP/1.1\r\nHost: " + authority + "\r\n\r\nping\n"
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading echo: %v", err)
	}
	if line != "ping\n" {
		t.Errorf("echo %q, want %q", line, "ping\n")
	}
}

// net/http refuses non-ASCII Host headers, but not every request reaches
// allowed through one, so the check is tested directly.
func TestAllowed(t *testing.T) {
	p := New([]string{allowedHost, "pkg.example"})
	for authority, want := range map[string]string{
		"api.anthropic.com:443":      "api.anthropic.com:443",
		"API.Anthropic.COM:80":       "api.anthropic.com:80",
		"pkg.example:443":            "pkg.example:443",
		"p\u212Ag.example:443":       "", // Kelvin sign lowers to "k"
		"api.anthrop\u0130c.com:443": "", // dotted capital I
		"example.com:443":            "",
	} {
		got, ok := p.allowed(authority)
		if ok != (want != "") || got != want {
			t.Errorf("allowed(%q) = %q, %v; want %q", authority, got, ok, want)
		}
	}
}

// TestLogCopy checks that a tunnel side closing or resetting is debug, and
// anything else is a warning.
func TestLogCopy(t *testing.T) {
	// Not parallel: it reads the process-wide logr output.
	reset := &net.OpError{Op: "read", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}
	for _, tc := range []struct {
		err   error
		level logr.Type
		want  string
	}{
		{io.EOF, logr.D, "egress tunnel closed"},
		{net.ErrClosed, logr.D, "egress tunnel closed"},
		{reset, logr.D, "egress tunnel closed"},
		{fmt.Errorf("write: %w", syscall.EPIPE), logr.D, "egress tunnel closed"},
		{errors.New("disk on fire"), logr.W, "egress tunnel copy failed"},
	} {
		// The writer only takes the expected level, so a wrong level fails.
		var logs bytes.Buffer
		stop := logtest.CaptureTo(t, &logs, tc.level)
		logCopy(allowedHost, "to client", tc.err)
		logr.Wait()
		stop()
		if !strings.Contains(logs.String(), tc.want) {
			t.Errorf("%v: logs = %q, want %q", tc.err, logs.String(), tc.want)
		}
	}
}
