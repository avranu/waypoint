package api

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"git.jmann.me/claude/waypoint/internal/core"
	"git.jmann.me/claude/waypoint/internal/hermes/hermestest"
)

// apiHermes stands in for Hermes behind the door: a cookie login, a
// sessions route whose first call finds the session gone, files of every
// kind (each trying to set a cookie), and a JSON-RPC gateway.
type apiHermes struct {
	mu      sync.Mutex
	logins  int
	valid   map[string]bool
	expired bool // /api/sessions has found the session gone once
	paths   []string
	tickets map[string]bool
	conn    *websocket.Conn
	replies chan map[string]any
}

func (h *apiHermes) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.paths = append(h.paths, r.URL.Path)
	if r.URL.Path == "/auth/password-login" {
		h.logins++
		tok := fmt.Sprintf("hs%d", h.logins)
		h.valid[tok] = true
		h.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "hermes", Value: tok, Path: "/"})
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
		return
	}
	if r.URL.Path == "/api/sessions" && !h.expired {
		h.expired = true
		h.valid = map[string]bool{}
	}
	c, err := r.Cookie("hermes")
	ok := err == nil && h.valid[c.Value]
	h.mu.Unlock()
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		io.WriteString(w, `{"error":"session_expired"}`)
		return
	}
	// Hermes's own cookies must never reach the browser.
	http.SetCookie(w, &http.Cookie{Name: "hermes_refresh", Value: "leak", Path: "/"})
	switch r.URL.Path {
	case "/api/auth/me":
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"username":"waypoint"}`)
	case "/api/sessions":
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		io.WriteString(w, `{"sessions":[{"id":"s1"}]}`)
	case "/api/files/download":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Disposition", `inline; filename="report.html"`)
		io.WriteString(w, `<script>alert(document.cookie)</script>`)
	case "/api/files/nameless":
		w.Header().Set("Content-Type", "application/javascript")
		io.WriteString(w, `alert(1)`)
	case "/api/media/pic.png":
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "4")
		io.WriteString(w, "\x89PNG")
	case "/api/media/pic.svg":
		w.Header().Set("Content-Type", "image/svg+xml")
		io.WriteString(w, `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	case "/api/media/notes.txt":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "hello")
	case "/api/media/doc.pdf":
		w.Header().Set("Content-Type", "application/pdf")
		io.WriteString(w, "%PDF-1.4")
	case "/api/media/untyped":
		w.Header()["Content-Type"] = nil // no sniffing either
		io.WriteString(w, "<html>")
	case "/api/auth/ws-ticket":
		h.mu.Lock()
		h.tickets["t1"] = true
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ticket":"t1"}`)
	case "/api/ws":
		h.serveWS(w, r)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		io.WriteString(w, `{"detail":"Not Found"}`)
	}
}

func (h *apiHermes) serveWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"hermes-gateway-v1"}})
	if err != nil {
		return
	}
	defer c.CloseNow()
	h.mu.Lock()
	h.conn = c
	h.mu.Unlock()
	ctx := r.Context()
	send := func(v any) {
		b, _ := json.Marshal(v)
		_ = c.Write(ctx, websocket.MessageText, b)
	}
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
		switch m.Method {
		case "":
			var rep map[string]any
			_ = json.Unmarshal(data, &rep)
			h.replies <- rep
		case "session.fail":
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": 4002, "message": "no such session"}})
		default:
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"method": m.Method, "params": m.Params}})
		}
	}
}

func (h *apiHermes) push(t *testing.T, v any) {
	t.Helper()
	h.mu.Lock()
	c := h.conn
	h.mu.Unlock()
	if c == nil {
		t.Fatal("no gateway connection")
	}
	b, _ := json.Marshal(v)
	if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func (h *apiHermes) saw(sub string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, p := range h.paths {
		if strings.Contains(p, sub) {
			out = append(out, p)
		}
	}
	return out
}

// asstClient calls the API as a browser (a session cookie and the client
// header) or with a token. It doesn't follow redirects.
type asstClient struct {
	t       *testing.T
	base    string
	session string
	token   string
	noWeb   bool
}

var noRedirects = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (c *asstClient) req(method, path string, body any) *http.Response {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if c.session != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: c.session})
		if !c.noWeb {
			req.Header.Set("X-Waypoint-Client", "web")
		}
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := noRedirects.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	return resp
}

func (c *asstClient) do(method, path string, body any) (int, []byte, http.Header) {
	c.t.Helper()
	resp := c.req(method, path, body)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

type asstAPIEnv struct {
	t               *testing.T
	base            string
	svc             *core.Service
	door            *hermestest.Door
	hermes          *apiHermes
	doorURL, ca     string
	robin, ann, bob *asstClient // robin is an admin; ann owns the assistant
	annActor        *core.Actor
	id              int64
}

func newAsstAPIEnv(t *testing.T) *asstAPIEnv {
	t.Helper()
	ts, svc := setup(t)
	ctx := context.Background()
	e := &asstAPIEnv{t: t, base: ts.URL, svc: svc,
		hermes: &apiHermes{valid: map[string]bool{}, tickets: map[string]bool{}, replies: make(chan map[string]any, 16)}}
	e.door = hermestest.NewDoor("door-key", e.hermes)
	door := httptest.NewTLSServer(e.door)
	t.Cleanup(door.Close)
	e.doorURL = door.URL
	e.ca = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: door.Certificate().Raw}))

	person := func(name string, admin bool) (*asstClient, *core.Actor) {
		u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: name, Email: name + "@example.com", Password: "correct horse battery", IsAdmin: admin})
		if err != nil {
			t.Fatal(err)
		}
		tok, _, err := svc.Login(ctx, name, "correct horse battery", "test", "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		return &asstClient{t: t, base: ts.URL, session: tok}, &core.Actor{User: u}
	}
	e.robin, _ = person("robin", true)
	e.ann, e.annActor = person("ann", false)
	e.bob, _ = person("bob", false)

	var a core.Assistant
	code, b, _ := e.robin.do("POST", "/api/v1/assistants", map[string]any{"name": "Sage", "owner_id": e.annActor.User.ID,
		"url": door.URL, "ca_pem": e.ca, "key": "door-key", "password": "hunter2-SECRET"})
	if code != 200 {
		t.Fatalf("create: %d %s", code, b)
	}
	if strings.Contains(string(b), "SECRET") || strings.Contains(string(b), "door-key") || strings.Contains(string(b), "BEGIN CERTIFICATE") {
		t.Errorf("create answered with a secret: %s", b)
	}
	_ = json.Unmarshal(b, &a)
	e.id = a.ID
	t.Cleanup(func() { _ = svc.DeleteAssistant(ctx, &core.Actor{User: &core.User{ID: 1, IsAdmin: true}}, a.ID) })
	svc.Wait()
	return e
}

func (e *asstAPIEnv) path(rest string) string {
	return fmt.Sprintf("/api/v1/assistants/%d%s", e.id, rest)
}

func TestAssistantRouteAccess(t *testing.T) {
	e := newAsstAPIEnv(t)
	ctx := context.Background()
	tokenFor := func(spec core.TokenSpec) *asstClient {
		tok, _, err := e.svc.CreateToken(ctx, e.annActor, spec)
		if err != nil {
			t.Fatal(err)
		}
		return &asstClient{t: t, base: e.base, token: tok}
	}
	var every []core.Permission
	for _, g := range e.svc.Permissions().Groups {
		for _, p := range g.Permissions {
			every = append(every, p.Key)
		}
	}
	full := tokenFor(core.TokenSpec{Name: "full"})
	agent := tokenFor(core.TokenSpec{Name: "agent", Agent: "nous/hermes"})
	perms := tokenFor(core.TokenSpec{Name: "everything", Permissions: every})
	noHeader := &asstClient{t: t, base: e.base, session: e.ann.session, noWeb: true}

	routes := []struct{ method, path string }{
		{"GET", e.path("")},
		{"POST", e.path("/check")},
		{"POST", e.path("/freeze")},
		{"POST", e.path("/rpc")},
		{"POST", e.path("/answer")},
		{"GET", e.path("/gate/approvals")},
		{"GET", e.path("/gate/audit")},
		{"GET", e.path("/gate/logs/squid")},
		{"GET", e.path("/gate/connections")},
		{"PUT", e.path("/gate/connections/github")},
		{"DELETE", e.path("/gate/connections/github")},
		{"GET", e.path("/links")},
		{"POST", e.path("/links")},
		{"GET", e.path("/live")},
		{"GET", e.path("/api/sessions")},
		{"POST", e.path("/api/sessions")},
		{"PATCH", e.path("")},
		{"DELETE", e.path("")},
	}
	bodies := map[string]any{"/rpc": map[string]any{"method": "session.list"}, "/answer": map[string]any{"id": "x"},
		"/links": map[string]any{"session_id": "s", "issue": "X-1"}, "/gate/connections/github": map[string]any{"kind": "mcp", "url": "https://x"},
		"": map[string]any{"name": "x"}}
	body := func(method, p string) any {
		if method == "GET" || method == "DELETE" {
			return nil
		}
		for k, v := range bodies {
			if strings.HasSuffix(p, fmt.Sprintf("%d%s", e.id, k)) {
				return v
			}
		}
		return map[string]any{}
	}
	for _, c := range []struct {
		name string
		cl   *asstClient
		want int // for every route
	}{
		{"another person", e.bob, 404},
		{"owner's full token", full, 404},
		{"owner's agent token", agent, 404},
		{"owner's token with every permission", perms, 403},
		{"owner's browser without the client header", noHeader, 403},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, r := range routes {
				if c.cl == noHeader && r.method == "GET" {
					continue
				}
				want := c.want
				if (r.method == "PATCH" || r.method == "DELETE") && r.path == e.path("") {
					want = 403 // configuring is for admins: refused before the lookup
				}
				if code, b, _ := c.cl.do(r.method, r.path, body(r.method, r.path)); code != want {
					t.Errorf("%s %s = %d %s, want %d", r.method, r.path, code, b, want)
				}
			}
			if c.want == 404 {
				// The list is empty rather than refused.
				code, b, _ := c.cl.do("GET", "/api/v1/assistants", nil)
				if code != 200 || strings.TrimSpace(string(b)) != "[]" {
					t.Errorf("list = %d %s", code, b)
				}
			}
		})
	}
	// An admin who doesn't own it can see and change its settings, not use it.
	if code, _, _ := e.robin.do("GET", e.path(""), nil); code != 200 {
		t.Errorf("admin get = %d", code)
	}
	for _, r := range []struct{ method, path string }{{"GET", e.path("/api/sessions")}, {"POST", e.path("/freeze")}, {"GET", e.path("/gate/approvals")}, {"GET", e.path("/live")}} {
		if code, _, _ := e.robin.do(r.method, r.path, nil); code != 404 {
			t.Errorf("admin %s %s = %d", r.method, r.path, code)
		}
	}
	// Only admins configure.
	if code, b, _ := e.ann.do("POST", "/api/v1/assistants", map[string]any{"name": "Mine", "url": e.doorURL, "ca_pem": e.ca, "key": "k", "password": "p"}); code != 403 {
		t.Errorf("owner create = %d %s", code, b)
	}
	if code, _, _ := e.ann.do("PATCH", e.path(""), map[string]any{"name": "Renamed"}); code != 403 {
		t.Errorf("owner update = %d", code)
	}
	if code, _, _ := e.ann.do("DELETE", e.path(""), nil); code != 403 {
		t.Errorf("owner delete = %d", code)
	}
	if e.door.Frozen() {
		t.Error("a refused request froze the agent")
	}
	if len(e.hermes.saw("/api/sessions")) != 0 {
		t.Error("a refused request reached Hermes")
	}
	// The owner, in a browser, is let through.
	if code, b, _ := e.ann.do("GET", e.path(""), nil); code != 200 || !strings.Contains(string(b), `"mine":true`) {
		t.Errorf("owner get = %d %s", code, b)
	}
}

func TestAssistantProxy(t *testing.T) {
	e := newAsstAPIEnv(t)
	// The first call signs in, finds the session gone, signs in again.
	code, b, h := e.ann.do("GET", e.path("/api/sessions"), nil)
	if code != 200 || !strings.Contains(string(b), `"s1"`) || h.Get("Content-Type") != "application/json" {
		t.Fatalf("sessions = %d %s %v", code, b, h)
	}
	e.hermes.mu.Lock()
	logins := e.hermes.logins
	e.hermes.mu.Unlock()
	if logins != 2 {
		t.Errorf("logins = %d, want 2 (one, then again after session_expired)", logins)
	}

	for _, c := range []struct {
		path, ctype, disp string
	}{
		{"/api/sessions", "application/json", ""},
		{"/api/files/download?path=/tmp/report.html", "application/octet-stream", `attachment; filename=report.html`},
		{"/api/files/nameless", "application/octet-stream", `attachment; filename=download`},
		{"/api/media/pic.png", "image/png", "inline"},
		{"/api/media/pic.svg", "application/octet-stream", "attachment; filename=download"},
		{"/api/media/notes.txt", "text/plain", "inline"},
		{"/api/media/doc.pdf", "application/pdf", "inline"},
		{"/api/media/untyped", "application/octet-stream", "attachment; filename=download"},
	} {
		code, b, h := e.ann.do("GET", e.path(c.path), nil)
		if code != 200 {
			t.Errorf("%s: %d %s", c.path, code, b)
			continue
		}
		if h.Get("Content-Type") != c.ctype || h.Get("Content-Disposition") != c.disp {
			t.Errorf("%s: Content-Type %q, Content-Disposition %q; want %q, %q", c.path, h.Get("Content-Type"), h.Get("Content-Disposition"), c.ctype, c.disp)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") ||
			!strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") {
			t.Errorf("%s: nosniff %q, CSP %q", c.path, h.Get("X-Content-Type-Options"), h.Get("Content-Security-Policy"))
		}
		if len(h.Values("Set-Cookie")) != 0 {
			t.Errorf("%s: forwarded Set-Cookie %v", c.path, h.Values("Set-Cookie"))
		}
	}

	// Refused paths never reach Hermes, however they're spelled. (The mux
	// redirects a literal "./", but routes "%2e" with {path...} unescaped
	// to "env/./reveal".)
	for _, p := range []string{"env/reveal", "env/reveal/OPENAI_API_KEY", "hermes/update", "auth/password-login", "auth/ws-ticket",
		"sessions/%2e%2e/auth/me", "env/%2e/reveal", "env%2Freveal", "sessions/../auth/me", "env/./reveal", "cron/%2e/fire", "skills/%2E/hub/install"} {
		code, b, _ := e.ann.do("GET", e.path("/api/"+p), nil)
		if code == 200 {
			t.Errorf("%s: %d %s", p, code, b)
		}
	}
	for _, bad := range []string{"reveal", "fire", "hub/install", "/api/hermes", "/api/auth/me", "/api/auth/ws-ticket"} {
		if got := e.hermes.saw(bad); len(got) != 0 {
			t.Errorf("Hermes saw %v", got)
		}
	}
	// Allowed elsewhere in the tree.
	if code, b, _ := e.ann.do("GET", e.path("/api/cron/jobs"), nil); code != 404 || !strings.Contains(string(b), "Not Found") {
		t.Errorf("cron/jobs should reach Hermes (which has no such route here): %d %s", code, b)
	}
}

func TestAssistantGateRoutes(t *testing.T) {
	e := newAsstAPIEnv(t)
	code, b, _ := e.ann.do("PUT", e.path("/gate/connections/github"), map[string]any{"kind": "mcp", "url": "https://api.github.com/mcp",
		"headers": map[string]any{"Authorization": "Bearer ghp_x"}})
	var pending struct{ Status, Code string }
	_ = json.Unmarshal(b, &pending)
	if code != 202 || pending.Status != "pending" || pending.Code == "" {
		t.Fatalf("put = %d %s", code, b)
	}
	if code, b, _ := e.ann.do("PUT", e.path("/gate/connections/Bad%20Name"), map[string]any{"kind": "mcp", "url": "https://x"}); code != 400 {
		t.Errorf("bad name = %d %s", code, b)
	}
	if code, b, _ := e.ann.do("GET", e.path("/gate/approvals"), nil); code != 200 || !strings.Contains(string(b), pending.Code) {
		t.Errorf("approvals = %d %s", code, b)
	}
	e.door.Decide(pending.Code, true)
	if code, b, _ := e.ann.do("GET", e.path("/gate/connections"), nil); code != 200 || !strings.Contains(string(b), `"github"`) || strings.Contains(string(b), "ghp_x") {
		t.Errorf("connections = %d %s", code, b)
	}
	if code, b, _ := e.ann.do("GET", e.path("/gate/logs/squid?lines=3"), nil); code != 200 || !strings.Contains(string(b), "squid") {
		t.Errorf("logs = %d %s", code, b)
	}
	if code, b, _ := e.ann.do("DELETE", e.path("/gate/connections/nope"), nil); code != 404 {
		t.Errorf("delete missing = %d %s", code, b)
	}
	code, b, _ = e.ann.do("POST", e.path("/test"), nil)
	if code != 200 || strings.Count(string(b), `"ok":true`) != 2 {
		t.Errorf("test = %d %s", code, b)
	}
	// Freezing stops the agent at the gate; Waypoint then refuses to pass
	// anything on.
	var a core.Assistant
	code, b, _ = e.ann.do("POST", e.path("/freeze"), nil)
	_ = json.Unmarshal(b, &a)
	if code != 200 || !a.State.Frozen || !e.door.Frozen() {
		t.Fatalf("freeze = %d %s", code, b)
	}
	if code, b, _ := e.ann.do("GET", e.path("/api/sessions"), nil); code != 502 {
		t.Errorf("api while frozen = %d %s", code, b)
	}
	if code, b, _ := e.ann.do("POST", e.path("/rpc"), map[string]any{"method": "session.list"}); code != 502 {
		t.Errorf("rpc while frozen = %d %s", code, b)
	}
}

func TestAssistantChatRoutes(t *testing.T) {
	e := newAsstAPIEnv(t)
	// The live stream says hello first.
	resp := e.ann.req("GET", e.path("/live"), nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("live = %d %v", resp.StatusCode, resp.Header)
	}
	rd := bufio.NewReader(resp.Body)
	lines := make(chan string, 256)
	go func() {
		defer close(lines)
		for {
			l, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			lines <- strings.TrimSpace(l)
		}
	}()
	defer resp.Body.Close()
	waitLine := func(want string) string {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("stream ended before %q", want)
				}
				if strings.Contains(l, want) {
					return l
				}
			case <-deadline:
				t.Fatalf("no %q on the stream", want)
			}
		}
	}
	waitLine("event: hello")

	// Allowed methods go through; others don't leave Waypoint.
	code, b, _ := e.ann.do("POST", e.path("/rpc"), map[string]any{"method": "session.list", "params": map[string]any{"limit": 5}})
	if code != 200 || !strings.Contains(string(b), `"method":"session.list"`) || !strings.Contains(string(b), `"limit":5`) {
		t.Fatalf("rpc = %d %s", code, b)
	}
	if code, b, _ := e.ann.do("POST", e.path("/rpc"), map[string]any{"method": "shell.exec"}); code != 400 {
		t.Errorf("refused method = %d %s", code, b)
	}
	if code, b, _ := e.ann.do("POST", e.path("/rpc"), map[string]any{"method": "session.fail"}); code != 400 || !strings.Contains(string(b), "no such session") {
		t.Errorf("hermes error = %d %s", code, b)
	}

	// The agent's question reaches the stream; the owner answers it once.
	e.hermes.push(t, map[string]any{"jsonrpc": "2.0", "id": "q1", "method": "approval", "params": map[string]any{"command": "ls"}})
	waitLine(`"method":"approval"`)
	if code, b, _ := e.ann.do("POST", e.path("/answer"), map[string]any{"id": "q1", "result": map[string]any{"choice": "once"}}); code/100 != 2 {
		t.Fatalf("answer = %d %s", code, b)
	}
	select {
	case r := <-e.hermes.replies:
		if r["id"] != "q1" {
			t.Errorf("hermes got %v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the answer never reached Hermes")
	}
	waitLine("bridge.answered")
	if code, _, _ := e.ann.do("POST", e.path("/answer"), map[string]any{"id": "q1", "result": map[string]any{}}); code != 400 {
		t.Errorf("second answer = %d", code)
	}
	// A secret request is declined by Waypoint and shown as such.
	e.hermes.push(t, map[string]any{"jsonrpc": "2.0", "id": "q2", "method": "secret", "params": map[string]any{"name": "OPENAI_API_KEY"}})
	waitLine("bridge.declined")
	select {
	case r := <-e.hermes.replies:
		if r["id"] != "q2" || r["error"] == nil {
			t.Errorf("hermes got %v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the decline never reached Hermes")
	}
}
