package broker

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// upstream is a test server reached under a DNS name.
type upstream struct {
	host string
	port int
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []*http.Request
}

func (u *upstream) seen() []*http.Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]*http.Request{}, u.reqs...)
}

// newUpstream starts a server for host. TLS servers get a certificate from
// ca, which the broker trusts as the upstream's root.
func newUpstream(t *testing.T, host string, ca *CA) *upstream {
	t.Helper()
	u := &upstream{host: host}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.reqs = append(u.reqs, r.Clone(context.Background()))
		u.mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "hello from "+host)
	})
	u.srv = httptest.NewUnstartedServer(h)
	if ca != nil {
		leaf, err := ca.Leaf(host)
		if err != nil {
			t.Fatal(err)
		}
		u.srv.TLS = &tls.Config{Certificates: []tls.Certificate{*leaf}, MinVersion: tls.VersionTLS12}
		u.srv.StartTLS()
	} else {
		u.srv.Start()
	}
	t.Cleanup(u.srv.Close)
	_, ps, _ := net.SplitHostPort(u.srv.Listener.Addr().String())
	u.port, _ = strconv.Atoi(ps)
	return u
}

type harness struct {
	b        *Broker
	proxy    *url.URL
	sandbox  *CA // what the session trusts
	upstream *CA // what the upstreams' certificates chain to
	mu       sync.Mutex
	log      []Entry
	limits   []string
}

func (h *harness) entries() []Entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Entry{}, h.log...)
}

// start runs a broker whose upstream names all resolve to resolveTo, and
// whose address check allows loopback only when loopback is true.
func start(t *testing.T, p Policy, creds map[string]Credential, resolveTo string, loopback bool) *harness {
	t.Helper()
	return startWith(t, p, creds, resolveTo, loopback, nil)
}

func startWith(t *testing.T, p Policy, creds map[string]Credential, resolveTo string, loopback bool, gateway http.Handler) *harness {
	t.Helper()
	sb, err := NewCA("test sandbox CA")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{sandbox: sb, upstream: testUpstreamCA}
	b, err := New(Config{
		Policy: p, CA: sb, Creds: creds, RootCAs: testUpstreamCA.Pool(), Gateway: gateway,
		Log: func(e Entry) { h.mu.Lock(); h.log = append(h.log, e); h.mu.Unlock() },
		OnLimit: func(l string) {
			h.mu.Lock()
			h.limits = append(h.limits, l)
			h.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	b.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr(resolveTo)}, nil
	}
	b.local = map[netip.Addr]bool{}
	if loopback {
		b.testAddr = func(ip netip.Addr) bool { return ip.IsLoopback() || publicAddr(ip) }
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = b.Serve(l) }()
	t.Cleanup(func() { _ = l.Close() })
	h.b = b
	h.proxy = &url.URL{Scheme: "http", Host: l.Addr().String()}
	return h
}

// client is the session: it trusts the given roots and proxies everything.
func (h *harness) client(roots *CA) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(h.proxy),
			TLSClientConfig: &tls.Config{RootCAs: roots.Pool(), MinVersion: tls.VersionTLS12},
		},
	}
}

var testUpstreamCA = func() *CA {
	ca, err := NewCA("test upstream CA")
	if err != nil {
		panic(err)
	}
	return ca
}()

func urlFor(u *upstream, scheme, path string) string {
	return scheme + "://" + u.host + ":" + strconv.Itoa(u.port) + path
}

func get(t *testing.T, c *http.Client, method, u string, hdr map[string]string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, u, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err.Error(), nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

func TestInspectSubstitutesCredentialAndChecksRoutes(t *testing.T) {
	up := newUpstream(t, "wp.example.org", testUpstreamCA)
	rule := Waypoint(WaypointConfig{Host: up.host, Cred: "waypoint"})[0]
	rule.Port = up.port
	h := start(t, Policy{Rules: []Rule{rule}},
		map[string]Credential{"waypoint": {Header: "Authorization", Value: "Bearer real-session-token"}}, "127.0.0.1", true)
	c := h.client(h.sandbox)

	code, body, _ := get(t, c, "GET", urlFor(up, "https", "/api/v1/issues/WP-1?x=secret"), map[string]string{
		"Authorization": "Bearer placeholder",
		"X-Api-Key":     "attacker",
		"Cookie":        "s=1",
	})
	if code != 200 || !strings.Contains(body, "hello from") {
		t.Fatalf("allowed request: %d %q", code, body)
	}
	got := up.seen()
	if len(got) != 1 {
		t.Fatalf("upstream saw %d requests", len(got))
	}
	if a := got[0].Header.Get("Authorization"); a != "Bearer real-session-token" {
		t.Errorf("Authorization upstream = %q, want the substituted token", a)
	}
	if got[0].Header.Get("X-Api-Key") != "" || got[0].Header.Get("Cookie") != "" {
		t.Errorf("the session's own auth headers reached the upstream: %v", got[0].Header)
	}
	if got[0].Host != up.host+":"+strconv.Itoa(up.port) {
		t.Errorf("upstream Host = %q", got[0].Host)
	}

	for _, tc := range []struct{ method, path, reason string }{
		{"POST", "/api/v1/me/tokens", "path"},
		{"GET", "/api/v1/sandbox/key", "path"},
		{"POST", "/api/v1/cli/login", "path"},
		{"PATCH", "/api/v1/projects/WP/sandbox", "path"},
		{"GET", "/api/v1/issues/../me/tokens", "path"},
		{"GET", "/api/v1/issues/%2e%2e/admin", "path"},
	} {
		code, _, hdr := get(t, c, tc.method, urlFor(up, "https", tc.path), nil)
		if code != http.StatusForbidden || hdr.Get("X-Waypoint-Sandbox") != "denied: "+tc.reason {
			t.Errorf("%s %s: %d %q, want 403 denied: %s", tc.method, tc.path, code, hdr.Get("X-Waypoint-Sandbox"), tc.reason)
		}
	}
	if n := len(up.seen()); n != 1 {
		t.Errorf("refused requests reached the upstream: %d", n)
	}
	for _, e := range h.entries() {
		if strings.Contains(e.Path, "secret") || strings.Contains(e.Path, "?") {
			t.Errorf("a query string reached the log: %+v", e)
		}
	}
}

func TestDomainFrontingRefused(t *testing.T) {
	up := newUpstream(t, "pkg.example.org", testUpstreamCA)
	h := start(t, Policy{Rules: []Rule{{Host: up.host, Port: up.port}}}, nil, "127.0.0.1", true)
	c := h.client(h.sandbox)

	// The Host header names another host than the one connected to.
	code, _, hdr := get(t, c, "GET", urlFor(up, "https", "/"), map[string]string{"Host": "evil.example.org"})
	if code != http.StatusForbidden || hdr.Get("X-Waypoint-Sandbox") != "denied: host" {
		t.Errorf("fronted Host header: %d %v", code, hdr)
	}

	// The TLS server name names another host than CONNECT did.
	conn, err := net.Dial("tcp", h.proxy.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	target := net.JoinHostPort(up.host, strconv.Itoa(up.port))
	_, _ = io.WriteString(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
	buf := make([]byte, 64)
	n, _ := conn.Read(buf)
	if !strings.HasPrefix(string(buf[:n]), "HTTP/1.1 200") {
		t.Fatalf("CONNECT answer %q", buf[:n])
	}
	tc := tls.Client(conn, &tls.Config{ServerName: "evil.example.org", RootCAs: h.sandbox.Pool(), MinVersion: tls.VersionTLS12})
	_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tc.Handshake(); err == nil {
		t.Error("a handshake for another server name succeeded")
	}
	waitFor(t, func() bool {
		for _, e := range h.entries() {
			if e.Reason == "sni" && e.Verdict == Denied {
				return true
			}
		}
		return false
	})
	if len(up.seen()) != 0 {
		t.Error("a fronted request reached the upstream")
	}
}

func TestUnknownHostAndMethod(t *testing.T) {
	up := newUpstream(t, "pkg.example.org", testUpstreamCA)
	h := start(t, Policy{Rules: []Rule{{Host: up.host, Port: up.port}}}, nil, "127.0.0.1", true)
	c := h.client(h.sandbox)

	if code, body, _ := get(t, c, "GET", "https://other.example.org:"+strconv.Itoa(up.port)+"/", nil); code == 200 {
		t.Errorf("unknown host allowed: %q", body)
	}
	code, _, hdr := get(t, c, "POST", urlFor(up, "https", "/-/publish"), nil)
	if code != http.StatusForbidden || hdr.Get("X-Waypoint-Sandbox") != "denied: method" {
		t.Errorf("POST to a read-only host: %d %v", code, hdr)
	}
	if code, _, _ := get(t, c, "GET", urlFor(up, "https", "/lodash"), nil); code != 200 {
		t.Errorf("GET on a read-only host: %d", code)
	}
}

func TestTunnelPassesThroughUntouched(t *testing.T) {
	up := newUpstream(t, "api.anthropic.test", testUpstreamCA)
	h := start(t, Policy{Rules: []Rule{{Host: up.host, Port: up.port, Mode: Tunnel, Template: "anthropic"}}}, nil, "127.0.0.1", true)
	// The session trusts the real upstream: nothing in between.
	c := h.client(testUpstreamCA)
	code, body, _ := get(t, c, "POST", urlFor(up, "https", "/v1/messages"), map[string]string{"Authorization": "Bearer setup-token"})
	if code != 200 {
		t.Fatalf("tunnel: %d %q", code, body)
	}
	if a := up.seen()[0].Header.Get("Authorization"); a != "Bearer setup-token" {
		t.Errorf("tunnelled auth = %q, want the session's own", a)
	}
	// A tunnel is logged when it ends.
	c.CloseIdleConnections()
	waitFor(t, func() bool {
		for _, e := range h.entries() {
			if e.Method == "CONNECT" && e.Verdict == Allowed && e.BytesUp > 0 && e.BytesDown > 0 {
				return true
			}
		}
		return false
	})
}

func TestAddressChecks(t *testing.T) {
	up := newUpstream(t, "git.example.org", testUpstreamCA)
	rule := Rule{Host: up.host, Port: up.port}

	// No pin, and loopback isn't public: refused after DNS.
	h := start(t, Policy{Rules: []Rule{rule}}, nil, "127.0.0.1", false)
	code, _, _ := get(t, h.client(h.sandbox), "GET", urlFor(up, "https", "/"), nil)
	if code != http.StatusBadGateway {
		t.Errorf("loopback without a pin: %d, want 502", code)
	}
	if len(up.seen()) != 0 {
		t.Error("an unpinned private address was reached")
	}
	found := false
	for _, e := range h.entries() {
		found = found || (e.Verdict == Failed && e.Reason == "address")
	}
	if !found {
		t.Errorf("no address failure logged: %+v", h.entries())
	}

	// Pinned: the same address is allowed for this host.
	rule.Pin = []string{"127.0.0.1"}
	h = start(t, Policy{Rules: []Rule{rule}}, nil, "127.0.0.1", false)
	if code, _, _ := get(t, h.client(h.sandbox), "GET", urlFor(up, "https", "/"), nil); code != 200 {
		t.Errorf("pinned address: %d", code)
	}
}

func TestLimits(t *testing.T) {
	up := newUpstream(t, "pkg.example.org", testUpstreamCA)
	h := start(t, Policy{Rules: []Rule{{Host: up.host, Port: up.port}}, Limits: Limits{Requests: 2}}, nil, "127.0.0.1", true)
	c := h.client(h.sandbox)
	for i := 0; i < 2; i++ {
		if code, _, _ := get(t, c, "GET", urlFor(up, "https", "/"), nil); code != 200 {
			t.Fatalf("request %d: %d", i, code)
		}
	}
	for i := 0; i < 2; i++ {
		code, _, hdr := get(t, c, "GET", urlFor(up, "https", "/"), nil)
		if code != http.StatusForbidden || hdr.Get("X-Waypoint-Sandbox") != "denied: limit" {
			t.Errorf("over the limit: %d %v", code, hdr)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.limits) != 1 || h.limits[0] != "requests" {
		t.Errorf("OnLimit calls = %v", h.limits)
	}
}

func TestPlainHTTP(t *testing.T) {
	up := newUpstream(t, "archive.example.org", nil)
	h := start(t, Policy{Rules: []Rule{{Host: up.host, Port: up.port, HTTP: true}}}, nil, "127.0.0.1", true)
	c := h.client(h.sandbox)
	if code, body, _ := get(t, c, "GET", urlFor(up, "http", "/ubuntu/dists/noble/Release"), nil); code != 200 {
		t.Errorf("plain GET: %d %q", code, body)
	}
	if code, _, _ := get(t, c, "PUT", urlFor(up, "http", "/ubuntu/x"), nil); code != http.StatusForbidden {
		t.Errorf("plain PUT: %d", code)
	}
	// A plain-HTTP rule isn't reachable by CONNECT.
	if code, _, _ := get(t, c, "GET", urlFor(up, "https", "/"), nil); code == 200 {
		t.Error("CONNECT to a plain-HTTP rule was allowed")
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

func TestPushesGoToTheGateway(t *testing.T) {
	up := newUpstream(t, "git.example.org", testUpstreamCA)
	rule := Forge(ForgeConfig{Host: up.host, Repos: []string{"Robin/demo"}, Cred: "forge", Push: true})[0]
	rule.Port = up.port
	var mu sync.Mutex
	var got []*http.Request
	gw := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Clone(context.Background()))
		mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "from the gateway")
	})
	h := startWith(t, Policy{Rules: []Rule{rule}},
		map[string]Credential{"forge": {Header: "Authorization", Value: "token read-only"}}, "127.0.0.1", true, gw)
	c := h.client(h.sandbox)

	for _, p := range []string{"/Robin/demo.git/info/refs?service=git-receive-pack", "/Robin/demo/info/refs?service=git-receive-pack"} {
		code, body, _ := get(t, c, "GET", urlFor(up, "https", p), map[string]string{"Authorization": "Basic c2Vzc2lvbg=="})
		if code != 200 || body != "from the gateway" {
			t.Fatalf("%s: %d %q", p, code, body)
		}
	}
	code, body, _ := get(t, c, "POST", urlFor(up, "https", "/Robin/demo.git/git-receive-pack"), nil)
	if code != 200 || body != "from the gateway" {
		t.Fatalf("receive-pack: %d %q", code, body)
	}
	code, body, _ = get(t, c, "GET", urlFor(up, "https", "/Robin/demo.git/info/refs?service=git-upload-pack"), nil)
	if code != 200 || !strings.Contains(body, "hello from") {
		t.Fatalf("fetch: %d %q", code, body)
	}
	code, _, _ = get(t, c, "POST", urlFor(up, "https", "/Robin/other.git/git-receive-pack"), nil)
	if code != http.StatusForbidden {
		t.Fatalf("push to another repo: %d", code)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("gateway saw %d requests", len(got))
	}
	for _, r := range got {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("an auth header reached the gateway: %v", r.Header)
		}
	}
	if n := len(up.seen()); n != 1 {
		t.Errorf("the forge saw %d requests, want only the fetch", n)
	}
	var gwEntries int
	for _, e := range h.entries() {
		if e.Reason == "gateway" && e.Verdict == Allowed {
			gwEntries++
		}
	}
	if gwEntries != 3 {
		t.Errorf("%d gateway log entries, want 3", gwEntries)
	}
}

func TestGatewayRoutesNeedAGateway(t *testing.T) {
	rule := Forge(ForgeConfig{Host: "git.example.org", Repos: []string{"Robin/demo"}, Push: true})[0]
	ca, err := NewCA("x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Policy: Policy{Rules: []Rule{rule}}, CA: ca}); err == nil {
		t.Fatal("a policy with gateway routes and no gateway was accepted")
	}
	bad := Rule{Host: "git.example.org", HTTP: true, Routes: []Route{{Methods: []string{"POST"}, Path: "/a/b/git-receive-pack", Gateway: true}}}
	if err := (&Policy{Rules: []Rule{bad}}).Validate(); err == nil {
		t.Fatal("gateway route on plain HTTP accepted")
	}
	deny := Rule{Host: "git.example.org", Routes: []Route{{Methods: []string{"GET"}, Path: "/**"}}, Deny: []Route{{Methods: []string{"POST"}, Path: "/x", Gateway: true}}}
	if err := (&Policy{Rules: []Rule{deny}}).Validate(); err == nil {
		t.Fatal("gateway deny route accepted")
	}
	if Forge(ForgeConfig{Host: "git.example.org", Repos: []string{"Robin/demo"}})[0].gateway() {
		t.Fatal("Forge without Push routes pushes to the gateway")
	}
}
