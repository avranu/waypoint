package hermes

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"git.jmann.me/claude/waypoint/internal/hermes/hermestest"
)

// fakeHermes is a small stand-in for Hermes's dashboard behind the door: a
// password login that sets a cookie, routes that want it, and a JSON-RPC
// gateway on /api/ws the test can push frames through.
type fakeHermes struct {
	t        *testing.T
	password string

	mu       sync.Mutex
	logins   int
	sessions map[string]bool
	tickets  map[string]bool
	n        int
	paths    []string
	keySeen  []string
	conn     *websocket.Conn
	conns    int
	replies  chan map[string]any
	expireAt map[string]bool
}

func newFakeHermes(t *testing.T) *fakeHermes {
	return &fakeHermes{t: t, password: "hunter2", sessions: map[string]bool{}, tickets: map[string]bool{},
		replies: make(chan map[string]any, 64), expireAt: map[string]bool{}}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// expireSessions forgets every login, as Hermes does when its sessions lapse.
func (f *fakeHermes) expireSessions() {
	f.mu.Lock()
	f.sessions = map[string]bool{}
	f.mu.Unlock()
}

func (f *fakeHermes) loginCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins
}

func (f *fakeHermes) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	f.keySeen = append(f.keySeen, r.Header.Get(DefaultKeyHeader)+r.Header.Get(otherKeyHeader))
	f.mu.Unlock()
	if r.URL.Path == "/auth/password-login" && r.Method == http.MethodPost {
		var in struct{ Provider, Username, Password string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Provider != "basic" || in.Username != "waypoint" || in.Password != f.password {
			writeJSON(w, 401, map[string]string{"detail": "bad credentials"})
			return
		}
		f.mu.Lock()
		f.logins++
		f.n++
		tok := fmt.Sprintf("sess-%d", f.n)
		f.sessions[tok] = true
		f.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "hermes_session", Value: tok, Path: "/"})
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		writeJSON(w, 404, map[string]string{"detail": "Not Found"})
		return
	}
	f.mu.Lock()
	if f.expireAt[r.URL.Path] {
		delete(f.expireAt, r.URL.Path)
		f.sessions = map[string]bool{}
	}
	c, err := r.Cookie("hermes_session")
	ok := err == nil && f.sessions[c.Value]
	f.mu.Unlock()
	if !ok {
		reason := "unauthenticated"
		if err == nil {
			reason = "session_expired"
		}
		writeJSON(w, 401, map[string]string{"error": reason})
		return
	}
	switch r.URL.Path {
	case "/api/auth/me":
		writeJSON(w, 200, map[string]string{"username": "waypoint"})
	case "/api/sessions":
		writeJSON(w, 200, map[string]any{"sessions": []any{map[string]string{"id": "s1", "title": "Groceries"}}})
	case "/api/not-yours":
		writeJSON(w, 401, map[string]string{"error": "not_yours"})
	case "/api/auth/ws-ticket":
		f.mu.Lock()
		f.n++
		tk := fmt.Sprintf("tk%d", f.n)
		f.tickets[tk] = true
		f.mu.Unlock()
		writeJSON(w, 200, map[string]string{"ticket": tk})
	case "/api/ws":
		f.serveWS(w, r)
	default:
		writeJSON(w, 404, map[string]string{"detail": "Not Found"})
	}
}

func (f *fakeHermes) serveWS(w http.ResponseWriter, r *http.Request) {
	ticket := ""
	for _, p := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		if t, ok := strings.CutPrefix(strings.TrimSpace(p), "hermes-gateway-ticket."); ok {
			ticket = t
		}
	}
	f.mu.Lock()
	valid := f.tickets[ticket]
	delete(f.tickets, ticket)
	f.mu.Unlock()
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"hermes-gateway-v1"}})
	if err != nil {
		return
	}
	if !valid {
		c.Close(4401, "bad ticket")
		return
	}
	defer c.CloseNow()
	f.mu.Lock()
	f.conn = c
	f.conns++
	f.mu.Unlock()
	ctx := r.Context()
	send := func(v any) {
		b, _ := json.Marshal(v)
		_ = c.Write(ctx, websocket.MessageText, b)
	}
	send(map[string]any{"jsonrpc": "2.0", "method": "event", "params": map[string]any{"type": "gateway.ready", "payload": map[string]any{}}})
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(data, &m)
		if m.Method == "" {
			var reply map[string]any
			_ = json.Unmarshal(data, &reply)
			f.replies <- reply
			continue
		}
		switch m.Method {
		case "client.capabilities", "gateway.ping":
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"ok": true}})
		case "echo":
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": m.Params})
		default:
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "no such method: " + m.Method}})
		}
	}
}

// push sends one frame to the connected gateway client.
func (f *fakeHermes) push(t *testing.T, v any) {
	t.Helper()
	f.mu.Lock()
	c := f.conn
	f.mu.Unlock()
	if c == nil {
		t.Fatal("no gateway connection to push to")
	}
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

// reply waits for the client's answer to one of our requests.
func (f *fakeHermes) reply(t *testing.T) map[string]any {
	t.Helper()
	select {
	case r := <-f.replies:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("the client never answered")
		return nil
	}
}

// door is a TLS door with the given key in front of h; it returns the
// door, its server and the CA to trust.
func tlsDoor(t *testing.T, key string, h http.Handler) (*hermestest.Door, *httptest.Server, string) {
	t.Helper()
	d := hermestest.NewDoor(key, h)
	ts := httptest.NewTLSServer(d)
	t.Cleanup(ts.Close)
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}))
	return d, ts, ca
}

// otherCA is a CA that signed nothing the door serves.
func otherCA(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "not the gate"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// otherKeyHeader is a gate's own key header, other than the default.
const otherKeyHeader = "X-Other-Key"

func newClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func tctx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestNewValidates(t *testing.T) {
	ca := otherCA(t)
	for _, c := range []struct {
		name string
		cfg  Config
		want string
		url  string
	}{
		{"https with CA", Config{URL: " https://10.0.0.1:9443/some/path?x=1 ", CAPEM: ca}, "", "https://10.0.0.1:9443"},
		{"http without CA", Config{URL: "http://door.test/"}, "", "http://door.test"},
		{"https without CA", Config{URL: "https://door.test"}, "needs its CA", ""},
		{"CA not PEM", Config{URL: "https://door.test", CAPEM: "not a cert"}, "not PEM", ""},
		{"other scheme", Config{URL: "ftp://door.test"}, "http(s)://", ""},
		{"no host", Config{URL: "https://"}, "http(s)://", ""},
		{"garbage", Config{URL: "::nope"}, "http(s)://", ""},
		{"own key header", Config{URL: "http://door.test", KeyHeader: " X-Door-Key "}, "", "http://door.test"},
		{"key header not a name", Config{URL: "http://door.test", KeyHeader: "X Door: Key"}, "header name", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			cl, err := New(c.cfg)
			if c.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if cl.URL() != c.url {
					t.Errorf("URL() = %q, want %q", cl.URL(), c.url)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

func TestCAPinning(t *testing.T) {
	fh := newFakeHermes(t)
	_, ts, ca := tlsDoor(t, "k", fh)
	ctx := tctx(t)

	good := newClient(t, Config{URL: ts.URL, CAPEM: ca, Key: "k", Username: "waypoint", Password: "hunter2"})
	if _, err := good.GateStatus(ctx); err != nil {
		t.Fatalf("pinned to the gate's CA: %v", err)
	}
	bad := newClient(t, Config{URL: ts.URL, CAPEM: otherCA(t), Key: "k", Username: "waypoint", Password: "hunter2"})
	_, err := bad.GateStatus(ctx)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("a door whose certificate the pinned CA didn't sign: err = %v, want ErrUnreachable", err)
	}
	var ua x509.UnknownAuthorityError
	if !errors.As(err, &ua) && !strings.Contains(err.Error(), "certificate") {
		t.Errorf("err = %v, want a certificate error", err)
	}
	if err := bad.JSON(ctx, http.MethodGet, "/api/sessions", nil, nil, nil); !errors.Is(err, ErrUnreachable) {
		t.Errorf("hermes call through the wrong CA: %v", err)
	}
	if fh.loginCount() != 0 {
		t.Errorf("a login reached Hermes through an unpinned connection")
	}
}

func TestKeyHeader(t *testing.T) {
	if hermestest.DefaultKeyHeader != DefaultKeyHeader {
		t.Fatalf("the fake door reads %s, the client sends %s", hermestest.DefaultKeyHeader, DefaultKeyHeader)
	}
	for _, header := range []string{"", otherKeyHeader} {
		t.Run("header="+header, func(t *testing.T) { testKeyHeader(t, header) })
	}
}

func testKeyHeader(t *testing.T, header string) {
	fh := newFakeHermes(t)
	door := hermestest.NewDoor("door-key", fh)
	door.Header = header
	ctx := tctx(t)
	var mu sync.Mutex
	var keys []string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.URL.Path+"="+r.Header.Get(door.KeyHeader()))
		mu.Unlock()
		door.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}))

	c := newClient(t, Config{URL: ts.URL, CAPEM: ca, Key: "door-key", KeyHeader: header, Username: "waypoint", Password: "hunter2"})
	if _, err := c.GateStatus(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.JSON(ctx, http.MethodGet, "/api/sessions", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	for _, k := range keys {
		if !strings.HasSuffix(k, "=door-key") {
			t.Errorf("request without the key: %s", k)
		}
	}
	if len(keys) < 3 {
		t.Errorf("door saw %v", keys)
	}
	mu.Unlock()
	fh.mu.Lock()
	for _, k := range fh.keySeen {
		if k != "" {
			t.Errorf("the key reached Hermes: %q", k)
		}
	}
	fh.mu.Unlock()

	wrong := newClient(t, Config{URL: ts.URL, CAPEM: ca, Key: "guess", KeyHeader: header, Username: "waypoint", Password: "hunter2"})
	_, err := wrong.GateStatus(ctx)
	var he *Error
	if !errors.As(err, &he) || he.Status != http.StatusUnauthorized {
		t.Errorf("gate with the wrong key: %v", err)
	}
	if err := wrong.JSON(ctx, http.MethodGet, "/api/sessions", nil, nil, nil); !errors.As(err, &he) || he.Status != http.StatusUnauthorized {
		t.Errorf("hermes with the wrong key: %v", err)
	}
	if fh.loginCount() != 1 {
		t.Errorf("logins = %d: a keyless request reached Hermes", fh.loginCount())
	}
}

func TestReLogin(t *testing.T) {
	fh := newFakeHermes(t)
	_, ts, ca := tlsDoor(t, "k", fh)
	ctx := tctx(t)
	c := newClient(t, Config{URL: ts.URL, CAPEM: ca, Key: "k", Username: "waypoint", Password: "hunter2"})

	var out struct {
		Sessions []map[string]string `json:"sessions"`
	}
	if err := c.JSON(ctx, http.MethodGet, "/api/sessions", nil, nil, &out); err != nil || len(out.Sessions) != 1 {
		t.Fatalf("first call: %v %+v", err, out)
	}
	if fh.loginCount() != 1 {
		t.Fatalf("logins = %d, want 1 (signed in on first use)", fh.loginCount())
	}
	if err := c.JSON(ctx, http.MethodGet, "/api/sessions", nil, nil, nil); err != nil || fh.loginCount() != 1 {
		t.Fatalf("second call: %v, logins %d", err, fh.loginCount())
	}

	fh.mu.Lock()
	fh.expireAt["/api/sessions"] = true
	fh.mu.Unlock()
	if err := c.JSON(ctx, http.MethodGet, "/api/sessions", nil, nil, &out); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	if fh.loginCount() != 2 {
		t.Errorf("logins = %d, want 2", fh.loginCount())
	}

	fh.expireSessions()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- c.JSON(ctx, http.MethodGet, "/api/sessions", nil, nil, nil)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent call: %v", err)
		}
	}
	if fh.loginCount() != 3 {
		t.Errorf("logins = %d after a concurrent expiry, want 3", fh.loginCount())
	}

	err := c.JSON(ctx, http.MethodGet, "/api/not-yours", nil, nil, nil)
	var he *Error
	if !errors.As(err, &he) || he.Status != 401 || he.Msg != "not_yours" {
		t.Errorf("domain 401: %v", err)
	}
	if fh.loginCount() != 3 {
		t.Errorf("a domain 401 caused a login (%d)", fh.loginCount())
	}

	bad := newClient(t, Config{URL: ts.URL, CAPEM: ca, Key: "k", Username: "waypoint", Password: "nope"})
	if err := bad.JSON(ctx, http.MethodGet, "/api/sessions", nil, nil, nil); err == nil || !strings.Contains(err.Error(), "refused Waypoint's login") {
		t.Errorf("wrong password: %v", err)
	}

	me, err := c.Check(ctx)
	if err != nil || me["username"] != "waypoint" {
		t.Errorf("check: %v %v", me, err)
	}
	if fh.loginCount() != 4 {
		t.Errorf("check didn't sign in afresh: logins %d", fh.loginCount())
	}
}

// TestReLoginOnlyOnce: a session that keeps answering 401 isn't retried
// forever.
func TestReLoginOnlyOnce(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/password-login" {
			writeJSON(w, 200, map[string]bool{"ok": true})
			return
		}
		writeJSON(w, 401, map[string]string{"error": "unauthenticated"})
	})
	_, ts, ca := tlsDoor(t, "k", h)
	c := newClient(t, Config{URL: ts.URL, CAPEM: ca, Key: "k", Username: "waypoint", Password: "x"})
	resp, err := c.Do(tctx(t), http.MethodGet, "/api/sessions", nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 401 || !strings.Contains(string(b), "unauthenticated") {
		t.Errorf("got %d %s", resp.StatusCode, b)
	}
}

func TestGateAPI(t *testing.T) {
	fh := newFakeHermes(t)
	door, ts, ca := tlsDoor(t, "k", fh)
	ctx := tctx(t)
	c := newClient(t, Config{URL: ts.URL, CAPEM: ca, Key: "k", Username: "waypoint", Password: "hunter2"})

	code := door.Ask("terminal", "rm -rf ~/Downloads/old")
	st, err := c.GateStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Frozen || !st.Signal || len(st.Pending) != 1 || st.Pending[0].Code != code || st.Pending[0].Status != "pending" {
		t.Errorf("status = %+v", st)
	}
	ap, err := c.Approvals(ctx, 10)
	if err != nil || len(ap.Pending) != 1 || len(ap.Recent) != 1 {
		t.Errorf("approvals = %+v %v", ap, err)
	}
	logs, err := c.Logs(ctx, "squid", 5)
	if err != nil || logs.Name != "squid" || len(logs.Lines) != 5 {
		t.Errorf("logs = %+v %v", logs, err)
	}

	status, raw, err := c.PutConnection(ctx, "github", map[string]any{"kind": "mcp", "url": "https://api.github.com/mcp",
		"headers": map[string]any{"Authorization": "Bearer ghp_secret"}})
	if err != nil || status != http.StatusAccepted {
		t.Fatalf("put: %d %s %v", status, raw, err)
	}
	var pending struct{ Status, Code string }
	_ = json.Unmarshal(raw, &pending)
	if pending.Status != "pending" || pending.Code == "" {
		t.Fatalf("pending answer = %s", raw)
	}
	st, _ = c.GateStatus(ctx)
	if len(st.Connections) != 0 || len(st.Pending) != 2 {
		t.Errorf("before approval: %+v", st)
	}
	door.Decide(pending.Code, true)
	st, _ = c.GateStatus(ctx)
	if len(st.Connections) != 1 || st.Connections[0].Name != "github" || strings.Join(st.Connections[0].HeaderNames, ",") != "Authorization" {
		t.Fatalf("after approval: %+v", st.Connections)
	}
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), "ghp_secret") {
		t.Error("a header value came back")
	}
	status, _, err = c.PutConnection(ctx, "github", map[string]any{"kind": "mcp", "url": "https://api.github.com/mcp",
		"headers": map[string]any{"Authorization": "Bearer ghp_rotated"}})
	if err != nil || status != http.StatusOK {
		t.Errorf("unchanged put: %d %v", status, err)
	}
	var he *Error
	if _, _, err := c.PutConnection(ctx, "Bad Name", map[string]any{"kind": "mcp", "url": "https://x"}); !errors.As(err, &he) || he.Status != 400 || !strings.Contains(he.Msg, "lowercase") {
		t.Errorf("bad name: %v", err)
	}
	if err := c.DeleteConnection(ctx, "github"); err != nil {
		t.Errorf("delete: %v", err)
	}
	if err := c.DeleteConnection(ctx, "github"); !errors.As(err, &he) || he.Status != 404 {
		t.Errorf("delete again: %v", err)
	}

	if err := c.Freeze(ctx); err != nil {
		t.Fatal(err)
	}
	if !door.Frozen() {
		t.Error("door not frozen")
	}
	if st, _ := c.GateStatus(ctx); !st.Frozen {
		t.Error("status not frozen")
	}
	if err := c.JSON(ctx, http.MethodGet, "/api/sessions", nil, nil, nil); !errors.As(err, &he) || he.Status != http.StatusBadGateway {
		t.Errorf("hermes while frozen: %v", err)
	}
	audit, err := c.Audit(ctx, 50, "control")
	if err != nil || len(audit) == 0 {
		t.Errorf("audit = %v %v", audit, err)
	}
	for _, a := range audit {
		if a["kind"] != "control" {
			t.Errorf("audit kind filter ignored: %v", a)
		}
	}
}

func TestErrorFrom(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		want   string
	}{
		{400, `{"error":"name taken"}`, "name taken"},
		{422, `{"detail":"bad field"}`, "bad field"},
		{422, `{"detail":[{"loc":["body","x"],"msg":"field required"}]}`, `[{"loc":["body","x"],"msg":"field required"}]`},
		{500, `upstream broke`, "upstream broke"},
		{502, `<html><body>Bad Gateway</body></html>`, "assistant answered 502 Bad Gateway"},
		{503, strings.Repeat("x", 400), "assistant answered 503 Service Unavailable"},
		{401, ``, "assistant answered 401 Unauthorized"},
	} {
		rec := httptest.NewRecorder()
		rec.WriteHeader(c.status)
		rec.WriteString(c.body)
		err := errorFrom(rec.Result())
		var he *Error
		if !errors.As(err, &he) || he.Status != c.status || he.Msg != c.want {
			t.Errorf("%d %.40q: got %v, want %q", c.status, c.body, err, c.want)
		}
	}
}
