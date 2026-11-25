package broker

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Credential is a header the broker writes into requests to a host whose
// rule names it, e.g. {"Authorization", "Bearer …"}.
type Credential struct {
	Header string
	Value  string
}

// Verdicts in a log entry.
const (
	Allowed = "allowed"
	Denied  = "denied"
	Failed  = "failed"
)

// Entry is one log line: a request, or a tunnel. It never holds a query
// string, a body or a header value.
type Entry struct {
	Time      time.Time `json:"time"`
	Method    string    `json:"method"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	Path      string    `json:"path,omitempty"`
	Status    int       `json:"status,omitempty"`
	BytesUp   int64     `json:"bytes_up,omitempty"`
	BytesDown int64     `json:"bytes_down,omitempty"`
	Verdict   string    `json:"verdict"`
	Reason    string    `json:"reason,omitempty"`
	Template  string    `json:"template,omitempty"`
}

// Config is what a broker runs with.
type Config struct {
	Policy Policy
	// CA signs the leaf certificates of inspected hosts. Required when any
	// rule inspects TLS.
	CA *CA
	// Creds are the values of the credentials rules name.
	Creds map[string]Credential
	// Log receives every entry; it must not block for long.
	Log func(Entry)
	// OnLimit is called once, when the first limit trips.
	OnLimit func(limit string)
	// RootCAs checks upstream certificates; nil means the system's.
	RootCAs *x509.CertPool
	// Gateway serves the routes marked Gateway: the push gateway
	// (internal/sandbox/pushgate). Required when any route names it.
	Gateway http.Handler
}

// Broker is one session's egress proxy. Serve it on the session's socket.
type Broker struct {
	rules   map[string]*Rule
	limits  Limits
	ca      *CA
	creds   map[string]Credential
	log     func(Entry)
	onLimit func(string)
	gateway http.Handler

	requests, up, down atomic.Int64
	tripMu             sync.Mutex
	tripped            string

	transport *http.Transport

	resolve  func(context.Context, string) ([]netip.Addr, error)
	local    map[netip.Addr]bool
	testAddr func(netip.Addr) bool
}

// New checks the policy and makes a broker.
func New(c Config) (*Broker, error) {
	if err := c.Policy.Validate(); err != nil {
		return nil, err
	}
	b := &Broker{
		rules:   map[string]*Rule{},
		limits:  c.Policy.Limits,
		ca:      c.CA,
		creds:   c.Creds,
		log:     c.Log,
		onLimit: c.OnLimit,
		gateway: c.Gateway,
		resolve: defaultResolve,
		local:   localAddrs(),
	}
	for i := range c.Policy.Rules {
		r := c.Policy.Rules[i]
		if r.Cred != "" {
			cr, ok := c.Creds[r.Cred]
			if !ok || cr.Header == "" || cr.Value == "" {
				return nil, fmt.Errorf("%w: %s names credential %q, which isn't configured", ErrPolicy, r.Host, r.Cred)
			}
		}
		if r.gateway() && c.Gateway == nil {
			return nil, fmt.Errorf("%w: %s sends pushes to the push gateway, but there is none", ErrPolicy, r.Host)
		}
		if r.mode() == Inspect && !r.HTTP && c.CA == nil {
			return nil, fmt.Errorf("%w: %s is inspected but there is no sandbox CA", ErrPolicy, r.Host)
		}
		b.rules[r.key()] = &r
	}
	b.transport = &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			r := b.rules[addr]
			if r == nil {
				return nil, fmt.Errorf("no rule for %s", addr)
			}
			return b.dial(ctx, r)
		},
		TLSClientConfig:       &tls.Config{RootCAs: c.RootCAs, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
	}
	return b, nil
}

// Serve answers proxy requests on l until it closes.
func (b *Broker) Serve(l net.Listener) error {
	srv := &http.Server{
		Handler:           b,
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	return srv.Serve(l)
}

// ServeHTTP handles CONNECT and absolute-form plain-HTTP requests.
func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodConnect:
		b.connect(w, r)
	case r.URL.IsAbs() && r.URL.Scheme == "http":
		host, port, ok := splitHostPort(r.URL.Host, 80)
		rule := b.rules[ruleKey(host, port)]
		e := Entry{Method: r.Method, Host: host, Port: port, Path: logPath(r.URL.EscapedPath())}
		if !ok || rule == nil || !rule.HTTP {
			b.refuse(w, e, "host")
			return
		}
		b.forward(w, r, rule, "http")
	default:
		http.Error(w, "Waypoint sandbox broker: send requests through the proxy", http.StatusBadRequest)
	}
}

// connect handles CONNECT host:port: a tunnel, or TLS ended here.
func (b *Broker) connect(w http.ResponseWriter, r *http.Request) {
	host, port, ok := splitHostPort(r.Host, 0)
	e := Entry{Method: http.MethodConnect, Host: host, Port: port}
	rule := b.rules[ruleKey(host, port)]
	if !ok || port == 0 || rule == nil || rule.HTTP {
		b.refuse(w, e, "host")
		return
	}
	e.Template = rule.Template
	if t := b.trippedLimit(); t != "" {
		b.refuse(w, e, "limit")
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "can't hijack", http.StatusInternalServerError)
		return
	}
	raw, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = raw.Close() }()
	conn := raw
	if n := brw.Reader.Buffered(); n > 0 {
		early := make([]byte, n)
		_, _ = io.ReadFull(brw.Reader, early)
		conn = &replayConn{Conn: raw, r: bytes.NewReader(early)}
	}
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	sni, hello, err := peekSNI(conn, 15*time.Second)
	if err != nil || strings.TrimSuffix(strings.ToLower(sni), ".") != host {
		e.Verdict, e.Reason = Denied, "sni"
		b.emit(e)
		return
	}
	conn = &replayConn{Conn: conn, r: bytes.NewReader(hello)}
	if rule.mode() == Tunnel {
		b.tunnel(conn, rule, e)
		return
	}
	b.inspect(conn, rule, e)
}

// tunnel copies bytes both ways, counting them against the limits.
func (b *Broker) tunnel(conn net.Conn, rule *Rule, e Entry) {
	if !b.countRequest() {
		e.Verdict, e.Reason = Denied, "limit"
		b.emit(e)
		return
	}
	up, err := b.dial(context.Background(), rule)
	if err != nil {
		e.Verdict, e.Reason = Failed, upstreamReason(err)
		b.emit(e)
		return
	}
	defer func() { _ = up.Close() }()
	var sent, got int64
	done := make(chan struct{}, 2)
	go func() {
		sent, _ = io.Copy(&limitWriter{w: up, b: b, up: true}, conn)
		closeWrite(up)
		done <- struct{}{}
	}()
	go func() {
		got, _ = io.Copy(&limitWriter{w: conn, b: b}, up)
		closeWrite(conn)
		done <- struct{}{}
	}()
	<-done
	t := time.NewTimer(30 * time.Second)
	pending := 1
	select {
	case <-done:
		pending = 0
	case <-t.C:
		_ = conn.Close()
		_ = up.Close()
		<-done
	}
	t.Stop()
	_ = conn.Close()
	_ = up.Close()
	for ; pending > 0; pending-- {
		<-done
	}
	e.Verdict, e.Status, e.BytesUp, e.BytesDown = Allowed, http.StatusOK, sent, got
	b.emit(e)
}

func closeWrite(c net.Conn) {
	type cw interface{ CloseWrite() error }
	for {
		switch x := c.(type) {
		case cw:
			_ = x.CloseWrite()
			return
		case *replayConn:
			c = x.Conn
		default:
			_ = c.Close()
			return
		}
	}
}

// inspect ends the session's TLS with a leaf for the host and serves the
// requests inside, each checked and forwarded on its own.
func (b *Broker) inspect(conn net.Conn, rule *Rule, e Entry) {
	leaf, err := b.ca.Leaf(rule.Host)
	if err != nil {
		e.Verdict, e.Reason = Failed, "certificate"
		b.emit(e)
		return
	}
	tc := tls.Server(conn, &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		NextProtos:   []string{"http/1.1"},
		MinVersion:   tls.VersionTLS12,
	})
	_ = tc.SetDeadline(time.Now().Add(15 * time.Second))
	if err := tc.Handshake(); err != nil {
		e.Verdict, e.Reason = Failed, "handshake"
		b.emit(e)
		return
	}
	_ = tc.SetDeadline(time.Time{})
	l := &oneConnListener{conn: tc, closed: make(chan struct{})}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b.forward(w, r, rule, "https")
		}),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          log.New(io.Discard, "", 0),
		ConnState: func(_ net.Conn, s http.ConnState) {
			if s == http.StateClosed || s == http.StateHijacked {
				_ = l.Close()
			}
		},
	}
	_ = srv.Serve(l)
}

// stripped are auth headers a session's own request may carry: on a host
// where the broker writes a credential, none of the session's survive.
var stripped = []string{"Authorization", "Proxy-Authorization", "Cookie"}

var authWords = []string{"auth", "token", "key", "secret", "session", "password", "credential", "cookie"}

func stripAuth(h http.Header) {
	for _, k := range stripped {
		h.Del(k)
	}
	for k := range h {
		lk := strings.ToLower(k)
		for _, w := range authWords {
			if strings.Contains(lk, w) {
				h.Del(k)
				break
			}
		}
	}
}

// forward checks one request against its host's rule and sends it on.
func (b *Broker) forward(w http.ResponseWriter, r *http.Request, rule *Rule, scheme string) {
	path := r.URL.EscapedPath()
	e := Entry{Method: r.Method, Host: rule.Host, Port: rule.port(), Path: logPath(path), Template: rule.Template}
	if !sameHost(r.Host, rule) {
		b.refuse(w, e, "host")
		return
	}
	d := rule.check(r.Method, path, r.URL.RawQuery)
	if !d.ok {
		b.refuse(w, e, d.reason)
		return
	}
	if !b.countRequest() {
		b.refuse(w, e, "limit")
		return
	}
	if d.gateway {
		b.toGateway(w, r, e)
		return
	}
	if rule.Cred != "" {
		stripAuth(r.Header)
		c := b.creds[rule.Cred]
		r.Header.Set(c.Header, c.Value)
	}
	var sent atomic.Int64
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = &limitReader{rc: r.Body, b: b, n: &sent}
	}
	cw := &countingWriter{ResponseWriter: w, b: b}
	target := rule.Host
	if (scheme == "https" && rule.port() != 443) || (scheme == "http" && rule.port() != 80) {
		target = ruleKey(rule.Host, rule.port())
	}
	var upErr error
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = scheme
			pr.Out.URL.Host = target
			pr.Out.Host = ""
		},
		Transport: b.transport,
		ErrorLog:  log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			upErr = err
			w.Header().Set("X-Waypoint-Sandbox", "upstream failed")
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	defer func() {
		e.Verdict, e.Status, e.BytesUp, e.BytesDown = Allowed, cw.status, sent.Load(), cw.n
		if upErr != nil {
			e.Verdict, e.Reason = Failed, upstreamReason(upErr)
		} else if cw.limit {
			e.Verdict, e.Reason = Failed, "limit"
		}
		b.emit(e)
	}()
	rp.ServeHTTP(cw, r)
}

// toGateway hands a push to the push gateway, which checks it and
// forwards it with the repo's deploy key (WP-469). The session's own auth
// headers never reach it, and the traffic counts against the limits like
// any other.
func (b *Broker) toGateway(w http.ResponseWriter, r *http.Request, e Entry) {
	stripAuth(r.Header)
	var sent atomic.Int64
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = &limitReader{rc: r.Body, b: b, n: &sent}
	}
	cw := &countingWriter{ResponseWriter: w, b: b}
	b.gateway.ServeHTTP(cw, r)
	e.Verdict, e.Status, e.BytesUp, e.BytesDown, e.Reason = Allowed, cw.status, sent.Load(), cw.n, "gateway"
	b.emit(e)
}

func upstreamReason(err error) string {
	switch {
	case errors.Is(err, ErrAddress):
		return "address"
	case errors.Is(err, errLimit):
		return "limit"
	}
	return "upstream"
}

// sameHost says whether a request's Host header names the rule's host.
func sameHost(hostHeader string, rule *Rule) bool {
	def := 443
	if rule.HTTP {
		def = 80
	}
	h, p, ok := splitHostPort(hostHeader, def)
	return ok && h == rule.Host && p == rule.port()
}

// splitHostPort reads host[:port], lower-cased and without a trailing dot;
// def is the port when none is given (0: a port is required).
func splitHostPort(s string, def int) (string, int, bool) {
	host, ps, err := net.SplitHostPort(s)
	if err != nil {
		if def == 0 {
			return strings.ToLower(s), 0, false
		}
		host, ps = s, strconv.Itoa(def)
	}
	port, err := strconv.Atoi(ps)
	if err != nil || port <= 0 || port > 65535 {
		return host, 0, false
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	return host, port, host != ""
}

func (b *Broker) refuse(w http.ResponseWriter, e Entry, reason string) {
	e.Verdict, e.Reason, e.Status = Denied, reason, http.StatusForbidden
	w.Header().Set("X-Waypoint-Sandbox", "denied: "+reason)
	what := e.Host
	if e.Path != "" {
		what += e.Path
	}
	http.Error(w, fmt.Sprintf("Waypoint sandbox: %s %s is not allowed (%s). A person can allow it in the project's sandbox settings.",
		e.Method, what, reasonText[reason]), http.StatusForbidden)
	b.emit(e)
}

var reasonText = map[string]string{
	"host":   "host not in this session's policy",
	"method": "method not allowed on this host",
	"path":   "path not allowed on this host",
	"module": "Go module not in this repository's go.mod",
	"limit":  "the session's traffic limit is used up",
}

func (b *Broker) emit(e Entry) {
	if b.log == nil {
		return
	}
	e.Time = time.Now()
	b.log(e)
}

// logPath keeps a log line short.
func logPath(p string) string {
	if len(p) > 256 {
		return p[:256] + "…"
	}
	return p
}

var errLimit = errors.New("session traffic limit reached")

func (b *Broker) trippedLimit() string {
	b.tripMu.Lock()
	defer b.tripMu.Unlock()
	return b.tripped
}

func (b *Broker) trip(limit string) {
	b.tripMu.Lock()
	first := b.tripped == ""
	if first {
		b.tripped = limit
	}
	b.tripMu.Unlock()
	if first && b.onLimit != nil {
		b.onLimit(limit)
	}
}

// countRequest counts one request (or tunnel); false once a limit is used.
func (b *Broker) countRequest() bool {
	if b.trippedLimit() != "" {
		return false
	}
	n := b.requests.Add(1)
	if b.limits.Requests > 0 && n > b.limits.Requests {
		b.trip("requests")
		return false
	}
	return true
}

// addBytes counts bytes one way; false once that way's limit is passed.
func (b *Broker) addBytes(up bool, n int) bool {
	if up {
		if t := b.up.Add(int64(n)); b.limits.BytesUp > 0 && t > b.limits.BytesUp {
			b.trip("bytes_up")
			return false
		}
		return true
	}
	if t := b.down.Add(int64(n)); b.limits.BytesDown > 0 && t > b.limits.BytesDown {
		b.trip("bytes_down")
		return false
	}
	return true
}

type limitWriter struct {
	w  io.Writer
	b  *Broker
	up bool
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if !l.b.addBytes(l.up, len(p)) {
		return 0, errLimit
	}
	return l.w.Write(p)
}

type limitReader struct {
	rc io.ReadCloser
	b  *Broker
	n  *atomic.Int64
}

func (l *limitReader) Read(p []byte) (int, error) {
	n, err := l.rc.Read(p)
	l.n.Add(int64(n))
	if n > 0 && !l.b.addBytes(true, n) {
		return n, errLimit
	}
	return n, err
}

func (l *limitReader) Close() error { return l.rc.Close() }

type countingWriter struct {
	http.ResponseWriter
	b      *Broker
	status int
	n      int64
	limit  bool
}

func (c *countingWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if !c.b.addBytes(false, len(p)) {
		c.limit = true
		return 0, errLimit
	}
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

func (c *countingWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// oneConnListener hands out one connection, then waits to be closed.
type oneConnListener struct {
	mu     sync.Mutex
	conn   net.Conn
	closed chan struct{}
	once   sync.Once
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	c := l.conn
	l.conn = nil
	l.mu.Unlock()
	if c != nil {
		return c, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return dummyAddr{} }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "sandbox" }
func (dummyAddr) String() string  { return "sandbox" }
