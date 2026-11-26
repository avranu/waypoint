// Package api serves Waypoint's HTTP interface: the JSON REST API under
// /api/v1, an MCP endpoint for AI agents at /mcp, the calendar feed, forge
// webhooks, health and metrics, and the embedded web app.
package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"go.opentelemetry.io/otel/trace"

	"git.jmann.me/claude/waypoint/internal/core"
	"git.jmann.me/claude/waypoint/internal/tracing"
)

// Config configures the HTTP server.
type Config struct {
	BaseURL string
	// TrustedProxies are the peers whose X-Forwarded-For / X-Forwarded-Proto
	// are believed (see ParseTrustedProxies); empty means none.
	TrustedProxies []netip.Prefix
	MetricsToken   string // if set, /metrics requires this bearer token
	DevProxy       string // proxy non-API requests to a Vite dev server
	CLIDir         string // where the wp binaries are, for /cli
	Version        string
}

// Server is the HTTP front end.
type Server struct {
	done           chan struct{}
	doneOnce       sync.Once
	svc            *core.Service
	cfg            Config
	log            *slog.Logger
	limiter        *rateLimiter
	userLimiter    *rateLimiter
	jobLimiter     *rateLimiter
	static         *staticFiles
	abandonedAfter time.Duration
}

// defaultAbandonedAfter is Server.abandonedAfter unless a test shortens it.
const defaultAbandonedAfter = 10 * time.Second

// statusClientClosed is nginx's status for a caller that hung up.
const statusClientClosed = 499

// New builds a Server.
func New(svc *core.Service, cfg Config, log *slog.Logger) *Server {
	return &Server{done: make(chan struct{}), svc: svc, cfg: cfg, log: log, limiter: newRateLimiter(10, time.Minute), userLimiter: newRateLimiter(30, 10*time.Minute),
		jobLimiter: newRateLimiter(core.JobTokenRateLimit, time.Minute), static: newStaticFiles(), abandonedAfter: defaultAbandonedAfter}
}

var (
	httpRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "waypoint_http_requests_total", Help: "HTTP requests by route and status class.",
	}, []string{"route", "code"})
	httpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "waypoint_http_request_duration_seconds", Help: "HTTP request latency by route.",
		Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
	}, []string{"route"})
	sseClients = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "waypoint_live_clients", Help: "Connected live-update streams.",
	})
)

// Handler returns the root handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routes(mux)
	return s.middleware(mux)
}

type ctxKey int

const (
	actorKey ctxKey = 1
	noteKey  ctxKey = 2
)

// requestNote is what a handler leaves for the middleware's one record about
// the request.
type requestNote struct {
	err          error
	waits        bool
	unregistered *unregisteredWrite
}

type unregisteredWrite struct {
	user    int64
	machine string
}

func noteFrom(r *http.Request) *requestNote {
	n, _ := r.Context().Value(noteKey).(*requestNote)
	return n
}

// waits says this request holds its connection open until something happens
// (a long poll, a stream), so a caller that leaves after a long while has not
// given up on a slow server.
func waits(r *http.Request) {
	if n := noteFrom(r); n != nil {
		n.waits = true
	}
}

func actorFrom(r *http.Request) *core.Actor {
	a, _ := r.Context().Value(actorKey).(*core.Actor)
	return a
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		r = r.WithContext(core.WithLogSource(r.Context(), core.SourceRequests))
		note := &requestNote{}
		r = r.WithContext(context.WithValue(r.Context(), noteKey, note))
		var span trace.Span
		if tracing.Enabled() {
			var ctx context.Context
			ctx, span = tracing.StartRequest(r, s.trusted(peerIP(r)))
			r = r.WithContext(ctx)
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		if s.cfg.Version != "" {
			h.Set("X-Waypoint-Version", s.cfg.Version) // a wp that differs upgrades itself
		}
		defer func() {
			p := recover()
			var stack string
			abort := p == http.ErrAbortHandler
			if p != nil {
				stack = string(debug.Stack())
				if !sw.wrote && !abort {
					writeJSON(sw, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				}
			}
			if span != nil {
				tracing.EndRequest(span, r.Method, r.Pattern, sw.status)
			}
			route := r.Pattern
			if route == "" {
				route = "other"
			}
			d := time.Since(start)
			httpRequests.WithLabelValues(route, strconv.Itoa(sw.status/100)+"xx").Inc()
			if !strings.HasSuffix(route, "/stream") {
				httpDuration.WithLabelValues(route).Observe(d.Seconds())
			}
			s.logRequest(r, note, route, sw.status, d, p, stack)
			if u := note.unregistered; u != nil && p == nil && sw.status < 400 {
				s.svc.CountUnregisteredWrite(context.WithoutCancel(r.Context()), u.user, u.machine)
			}
			if abort {
				panic(http.ErrAbortHandler)
			}
		}()
		next.ServeHTTP(sw, r)
	})
}

// logRequest writes the one record a request gets. It is logged without the
// request's cancellation: it is the server's statement about the request, not
// part of the work that was called off, and the request's context is
// cancelled exactly when the caller hung up.
func (s *Server) logRequest(r *http.Request, note *requestNote, route string, status int, d time.Duration, panicked any, stack string) {
	after := s.abandonedAfter
	if after == 0 {
		after = defaultAbandonedAfter
	}
	lvl, msg := slog.LevelDebug, "request"
	switch {
	case panicked != nil:
		lvl, msg = slog.LevelError, "request panicked"
	case status >= 500:
		lvl, msg = slog.LevelError, "request failed"
	case status == statusClientClosed && d >= after && !note.waits:
		lvl, msg = slog.LevelWarn, "request abandoned: its caller hung up after a long wait"
	case status >= 400:
		lvl = slog.LevelInfo
	}
	if lvl < slog.LevelWarn && !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/mcp") {
		return
	}
	attrs := []any{"route", route, "method", r.Method, "path", logPath(r), "status", status, "ms", d.Milliseconds()}
	if note.err != nil {
		attrs = append(attrs, "err", note.err)
	}
	if panicked != nil {
		attrs = append(attrs, "panic", panicked, "stack", stack)
	}
	s.log.Log(context.WithoutCancel(r.Context()), lvl, msg, attrs...)
}

// Shutdown ends long-lived responses (live update streams) so a graceful
// shutdown doesn't wait them out; register it with http.Server.RegisterOnShutdown.
func (s *Server) Shutdown() { s.doneOnce.Do(func() { close(s.done) }) }

// logPath is the request path for logs, with secrets in it (invite and
// reset tokens, calendar feed tokens, CLI login codes) replaced by the
// route pattern.
func logPath(r *http.Request) string {
	if strings.Contains(r.Pattern, "{token}") || strings.Contains(r.Pattern, "{file}") || strings.Contains(r.Pattern, "{code}") ||
		strings.HasPrefix(r.URL.Path, "/cal/") {
		_, pattern, _ := strings.Cut(r.Pattern, " ")
		return pattern
	}
	return r.URL.Path
}

// privateNets is what WAYPOINT_TRUST_PROXY=1 trusts: loopback and private
// ranges, where a reverse proxy in front of Waypoint normally sits.
var privateNets = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("fc00::/7"),
}

// ParseTrustedProxies reads WAYPOINT_TRUST_PROXY: off ("" or 0), on (1:
// loopback and private ranges), or a comma list of addresses and CIDRs.
func ParseTrustedProxies(v string) ([]netip.Prefix, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return nil, nil
	case "1", "true", "yes", "on":
		return privateNets, nil
	}
	var out []netip.Prefix
	for _, f := range strings.Split(v, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("%q is not an address or CIDR", f)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

func (s *Server) trusted(ip string) bool {
	a, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return false
	}
	a = a.Unmap().WithZone("")
	for _, p := range s.cfg.TrustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// clientIP is the request's source address. Only when the connection comes
// from a trusted proxy is X-Forwarded-For believed, and then read from the
// right: each proxy appends the address it saw, so the first hop that
// isn't itself a trusted proxy is the client. Anything to its left was
// supplied by the client and can be forged.
func (s *Server) clientIP(r *http.Request) string {
	peer := peerIP(r)
	if !s.trusted(peer) {
		return peer
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hops = append(hops, h)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		if _, err := netip.ParseAddr(hops[i]); err != nil {
			return peer
		}
		if !s.trusted(hops[i]) || i == 0 {
			return hops[i]
		}
	}
	return peer
}

const sessionCookie = "wp_session"

func (s *Server) secureCookies(r *http.Request) bool {
	if strings.HasPrefix(s.cfg.BaseURL, "https://") {
		return true
	}
	return s.trusted(peerIP(r)) && r.Header.Get("X-Forwarded-Proto") == "https"
}

// authenticate resolves the actor from a bearer token or session cookie.
// Cookie-authenticated writes must carry X-Waypoint-Client, which a
// cross-site form or simple request cannot add without a CORS preflight —
// and Waypoint never answers preflights — so this blocks CSRF.
func (s *Server) authenticate(r *http.Request) (*core.Actor, error) {
	if auth := r.Header.Get("Authorization"); auth != "" {
		tok, ok := strings.CutPrefix(auth, "Bearer ")
		if !ok {
			return nil, core.ErrUnauthorized
		}
		a, err := s.svc.TokenActor(r.Context(), strings.TrimSpace(tok))
		if err != nil {
			return nil, err
		}
		if ag := strings.TrimSpace(r.Header.Get("X-Waypoint-Agent")); ag != "" && len(ag) <= 100 && a.Agent == "" {
			a.Agent = ag
		}
		if env := strings.TrimSpace(r.Header.Get("X-Waypoint-Agent-Env")); env != "" && len(env) <= 100 {
			a.AgentEnv = env
		}
		a.Prestart = r.Header.Get("X-Waypoint-Prestart") == "1"
		if a.RunnerJob != 0 && !s.jobLimiter.allow(fmt.Sprint("job:", a.RunnerJob)) {
			return nil, errJobTooMany
		}
		if err := s.checkPermission(r, a); err != nil {
			return nil, err
		}
		if sid := strings.TrimSpace(r.Header.Get("X-Waypoint-Session")); sid != "" && a.User != nil {
			machine := strings.TrimSpace(r.Header.Get("X-Waypoint-Machine"))
			a.Session = s.svc.SessionForHeader(r.Context(), a.User.ID, sid, machine)
			if a.Session == 0 && r.Method != http.MethodGet && r.Method != http.MethodHead && !strings.HasPrefix(r.URL.Path, "/api/v1/sessions/hook") &&
				!strings.HasPrefix(r.URL.Path, "/api/v1/sessions/outstanding") {
				if n := noteFrom(r); n != nil {
					n.unregistered = &unregisteredWrite{user: a.User.ID, machine: machine}
				}
			}
		}
		a.ClientVersion = core.ClientVersionFrom(r.Header.Get("User-Agent"))
		return a, nil
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, core.ErrUnauthorized
	}
	a, err := s.svc.SessionActor(r.Context(), c.Value)
	if err != nil {
		return nil, err
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Waypoint-Client") == "" {
		return nil, fmt.Errorf("missing X-Waypoint-Client header: %w", core.ErrForbidden)
	}
	return a, nil
}

// handler adapts a function returning (value, error) into an authenticated
// JSON endpoint.
type apiFunc func(w http.ResponseWriter, r *http.Request) (any, error)

func (s *Server) authed(f apiFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, err := s.authenticate(r)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		if a.ReadOnly && r.Method != http.MethodGet && r.Method != http.MethodHead {
			s.writeError(w, r, fmt.Errorf("read-only token: %w", core.ErrForbidden))
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), actorKey, a))
		s.serve(w, r, f)
	}
}

// public serves unauthenticated endpoints. Their writes (login, logout,
// accepting an invite) still need X-Waypoint-Client, so another site can't
// sign a visitor in or out.
func (s *Server) public(f apiFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Waypoint-Client") == "" {
			s.writeError(w, r, fmt.Errorf("missing X-Waypoint-Client header: %w", core.ErrForbidden))
			return
		}
		s.serve(w, r, f)
	}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, f apiFunc) {
	v, err := f(w, r)
	if err != nil {
		if a, ok := r.Context().Value(actorKey).(*core.Actor); ok {
			err = s.svc.Refused(r.Context(), a, r.Pattern, err)
		}
		s.writeError(w, r, err)
		return
	}
	switch x := v.(type) {
	case nil:
		w.WriteHeader(http.StatusNoContent)
	case handled:
	case rawResponse:
		w.Header().Set("Content-Type", x.contentType)
		w.Write(x.body)
	default:
		writeJSONCompressed(w, r, http.StatusOK, v)
	}
}

type rawResponse struct {
	contentType string
	body        []byte
}

func (s *Server) shuttingDown() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	msg := "internal error"
	var ie *core.InvalidError
	switch {
	case errors.Is(err, context.Canceled) && (r.Context().Err() != nil || s.shuttingDown()):
		status, msg = statusClientClosed, "the request was cancelled"
	case errors.As(err, &ie):
		status, msg = http.StatusBadRequest, ie.Msg
	case errors.Is(err, core.ErrNotFound):
		status, msg = http.StatusNotFound, err.Error()
	case errors.Is(err, core.ErrUnauthorized):
		status, msg = http.StatusUnauthorized, "not signed in"
	case errors.Is(err, core.ErrForbidden):
		status, msg = http.StatusForbidden, err.Error()
	case errors.Is(err, core.ErrConflict):
		status, msg = http.StatusConflict, err.Error()
	case errors.Is(err, errBodyTooLarge):
		status, msg = http.StatusRequestEntityTooLarge, err.Error()
	case errors.Is(err, errBadRequest):
		status, msg = http.StatusBadRequest, err.Error()
	case errors.Is(err, core.ErrUnavailable):
		status, msg = http.StatusBadGateway, err.Error()
	case errors.Is(err, errTooMany):
		status, msg = http.StatusTooManyRequests, err.Error()
	case errors.Is(err, core.ErrTranscriptQuota):
		status, msg = http.StatusInsufficientStorage, core.TranscriptQuotaRefusal
	case errors.Is(err, core.ErrTranscriptDiskLow):
		status, msg = http.StatusInsufficientStorage, core.TranscriptDiskLowRefusal
	case errors.Is(err, core.ErrTranscriptsOff):
		status, msg = http.StatusNotImplemented, err.Error()
	}
	if status >= 500 && status != http.StatusInsufficientStorage && status != http.StatusNotImplemented {
		if n := noteFrom(r); n != nil {
			n.err = err
		} else {
			s.log.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", logPath(r), "err", err)
		}
	}
	body := map[string]string{"error": msg}
	switch {
	case errors.Is(err, core.ErrReauth):
		body["code"] = "reauth"
	case errors.Is(err, core.ErrTranscriptQuota), errors.Is(err, core.ErrTranscriptDiskLow):
		body["code"] = "transcript_full"
	case errors.Is(err, core.ErrTranscriptJob):
		body["code"] = "transcript_job"
	}
	writeJSON(w, status, body)
}

var (
	errBadRequest = errors.New("bad request")
	// errBodyTooLarge: a request body over what its route reads (413).
	errBodyTooLarge = errors.New("request body too large")
	errTooMany      = errors.New("too many attempts, try again in a minute")
	errJobTooMany   = fmt.Errorf("this runner job's token made too many requests; try again in a minute: %w", errTooMany)
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

var gzPool = sync.Pool{New: func() any { w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed); return w }}

// writeJSONCompressed gzips larger responses for clients that accept it.
func writeJSONCompressed(w http.ResponseWriter, r *http.Request, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "encode failed"})
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Add("Vary", "Accept-Encoding")
	if r.Method == http.MethodGet && status == http.StatusOK {
		h.Set("Cache-Control", "private, no-cache")
		sum := sha256.Sum256(b)
		if notModified(w, r, `W/"`+hex.EncodeToString(sum[:8])+`"`) {
			return
		}
	} else {
		h.Set("Cache-Control", "no-store")
	}
	if len(b) > 1400 && accepts(r, "gzip") {
		h.Set("Content-Encoding", "gzip")
		w.WriteHeader(status)
		gz := gzPool.Get().(*gzip.Writer)
		gz.Reset(w)
		gz.Write(b)
		gz.Close()
		gzPool.Put(gz)
		return
	}
	w.WriteHeader(status)
	w.Write(b)
}

// decode reads a JSON body into v, rejecting unknown fields so typos in
// field names surface as errors instead of being silently ignored.
func decode(r *http.Request, v any) error {
	empty, err := decodeBody(r, v)
	if empty {
		return fmt.Errorf("%w: request body is empty", errBadRequest)
	}
	return err
}

// decodeOptional is decode for endpoints whose body may be left out,
// chunked or not.
func decodeOptional(r *http.Request, v any) error {
	_, err := decodeBody(r, v)
	return err
}

func decodeBody(r *http.Request, v any) (empty bool, err error) {
	return decodeBodyMax(r, v, 2<<20)
}

// decodeBodyLarge is decode for a route whose body may be larger than the
// usual 2 MB (a runner's job result): over max it says so, instead of
// reading the cut-off body as broken JSON.
func decodeBodyLarge(r *http.Request, v any, max int64, tooLarge string) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return fmt.Errorf("%w: %v", errBadRequest, err)
	}
	if int64(len(b)) > max {
		return fmt.Errorf("%w: %s (the body may be %d bytes)", errBadRequest, tooLarge, max)
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	empty, err := decodeBodyMax(r, v, max+1)
	if empty {
		return fmt.Errorf("%w: request body is empty", errBadRequest)
	}
	return err
}

func decodeBodyMax(r *http.Request, v any, max int64) (empty bool, err error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return false, fmt.Errorf("%w: %v", errBadRequest, err)
	}
	if int64(len(b)) > max {
		return false, fmt.Errorf("%w (limit %d)", errBodyTooLarge, max)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		return false, fmt.Errorf("%w: %v", errBadRequest, err)
	}
	if dec.More() {
		return false, fmt.Errorf("%w: unexpected data after the JSON body", errBadRequest)
	}
	return false, nil
}

func pathID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be a number", errBadRequest, name)
	}
	return id, nil
}

// queryIntIn is queryInt for a value that sizes an allocation: outside
// [lo, hi] it is def.
func queryIntIn(r *http.Request, name string, def, lo, hi int) int {
	if v := queryInt(r, name, def); v >= lo && v <= hi {
		return v
	}
	return def
}

func queryInt(r *http.Request, name string, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil {
		return v
	}
	return def
}

func queryBool(r *http.Request, name string) bool {
	v := strings.ToLower(r.URL.Query().Get(name))
	return v == "1" || v == "true" || v == "yes"
}

type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, hits: map[string][]time.Time{}}
}

// allow records an attempt and reports whether it is within the limit,
// as one step, so a burst of concurrent requests can't all slip through.
func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.recent(key, time.Now())) >= l.limit {
		return false
	}
	return l.hitLocked(key)
}

// blocked reports whether key has used up its attempts, without counting one.
func (l *rateLimiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(key, time.Now())) >= l.limit
}

// hit counts an attempt. It returns false if the table is full of live
// entries; callers treat that as limited rather than forgetting everyone.
func (l *rateLimiter) hit(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.hitLocked(key)
}

func (l *rateLimiter) hitLocked(key string) bool {
	now := time.Now()
	h := l.recent(key, now)
	if len(h) == 0 && len(l.hits) >= maxLimiterKeys {
		for k := range l.hits {
			if len(l.recent(k, now)) == 0 {
				delete(l.hits, k)
			}
		}
		if len(l.hits) >= maxLimiterKeys {
			return false
		}
	}
	l.hits[key] = append(h, now)
	return true
}

// recent drops expired attempts for key and returns the rest.
func (l *rateLimiter) recent(key string, now time.Time) []time.Time {
	h, ok := l.hits[key]
	if !ok {
		return nil
	}
	cut := now.Add(-l.window)
	n := 0
	for _, t := range h {
		if t.After(cut) {
			h[n] = t
			n++
		}
	}
	if n == 0 {
		delete(l.hits, key)
		return nil
	}
	l.hits[key] = h[:n]
	return h[:n]
}

const maxLimiterKeys = 10000

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.svc.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "db unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": s.cfg.Version})
}

// directLocal reports whether a request came straight from a loopback or
// private address, not through a reverse proxy.
func (s *Server) directLocal(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && (ip.IsLoopback() || ip.IsPrivate())
}

func (s *Server) metrics() http.Handler {
	h := promhttp.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.MetricsToken != "" {
			got := []byte(r.Header.Get("Authorization"))
			if subtle.ConstantTimeCompare(got, []byte("Bearer "+s.cfg.MetricsToken)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		} else if !s.directLocal(r) {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}
