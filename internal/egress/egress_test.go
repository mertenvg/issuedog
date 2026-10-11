package egress

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	var n atomic.Int32
	p := New([]string{allowedHost})
	p.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		n.Add(1)
		var d net.Dialer
		return d.DialContext(ctx, network, upstream)
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
	for _, url := range []string{"http://example.com/", "http://" + allowedHost + "/"} {
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

func TestConnectUpstreamDown(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	down := l.Addr().String()
	l.Close()
	proxy, _ := startProxy(t, down)
	resp, _, _ := connect(t, proxy, allowedHost+":443")
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status %d, want 502", resp.StatusCode)
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
