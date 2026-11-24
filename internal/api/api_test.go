package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
	"git.jmann.me/claude/waypoint/internal/db"
)

type client struct {
	t     *testing.T
	base  string
	http  *http.Client
	token string
	web   bool
}

func (c *client) do(method, path string, body any) (int, []byte) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		if s, ok := body.(string); ok {
			rd = strings.NewReader(s)
		} else {
			b, _ := json.Marshal(body)
			rd = strings.NewReader(string(b))
		}
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.web {
		req.Header.Set("X-Waypoint-Client", "web")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (c *client) json(method, path string, body any, out any) int {
	c.t.Helper()
	code, b := c.do(method, path, body)
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			c.t.Fatalf("%s %s: decode %q: %v", method, path, b, err)
		}
	}
	return code
}

func setup(t *testing.T) (*httptest.Server, *core.Service) { return setupServer(t, nil) }

func setupServer(t *testing.T, configure func(*http.Server)) (*httptest.Server, *core.Service) {
	ts, svc, _ := setupServerDB(t, configure)
	return ts, svc
}

// setupServerDB is setupServer with the database too, for a test that has
// to put the data in a state the service only reaches over many steps.
func setupServerDB(t *testing.T, configure func(*http.Server)) (*httptest.Server, *core.Service, *db.DB) {
	ctx := context.Background()
	d, err := db.OpenFresh(ctx, filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	svc := core.New(d, core.Config{BaseURL: "http://wp.test"}, nil)
	svc.StartHub()
	ts := httptest.NewUnstartedServer(New(svc, Config{BaseURL: "http://wp.test", Version: "test"}, svc.Log()).Handler())
	if configure != nil {
		configure(ts.Config)
	}
	ts.Start()
	t.Cleanup(ts.Close)
	t.Cleanup(svc.Wait)
	return ts, svc, d
}

func TestAPIFlow(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	link, err := svc.BootstrapInvite(ctx)
	if err != nil || link == nil {
		t.Fatal("expected bootstrap invite", err)
	}
	tok := link.URL[strings.LastIndex(link.URL, "/")+1:]
	jar, _ := cookiejar.New(nil)
	web := &client{t: t, base: ts.URL, http: &http.Client{Jar: jar}}

	var info map[string]any
	if code := web.json("GET", "/api/v1/auth/link/"+tok, nil, &info); code != 200 || info["kind"] != "invite" || info["is_admin"] != true {
		t.Fatalf("link: %d %v", code, info)
	}
	var me map[string]any
	web.web = true
	if code := web.json("POST", "/api/v1/auth/invite/"+tok, map[string]any{"username": "robin", "email": "robin@example.com", "password": "correct horse battery"}, &me); code != 200 {
		t.Fatalf("accept: %d %v", code, me)
	}
	web.web = false
	if code := web.json("GET", "/api/v1/me", nil, &me); code != 200 || me["username"] != "robin" {
		t.Fatalf("me: %d %v", code, me)
	}
	if code, b := web.do("POST", "/api/v1/projects", map[string]any{"name": "Waypoint", "key": "WAY"}); code != 403 {
		t.Fatalf("write without CSRF header: %d %s", code, b)
	}
	web.web = true
	var proj map[string]any
	if code := web.json("POST", "/api/v1/projects", map[string]any{"name": "Waypoint", "key": "WAY"}, &proj); code != 200 {
		t.Fatalf("create project: %d %v", code, proj)
	}
	if code, b := web.do("POST", "/api/v1/issues", map[string]any{"project": "WAY", "title": "x", "asignee": "me"}); code != 400 || !strings.Contains(string(b), "asignee") {
		t.Fatalf("typo accepted: %d %s", code, b)
	}
	var created map[string]any
	web.json("POST", "/api/v1/me/tokens", map[string]any{"name": "claude"}, &created)
	agent := &client{t: t, base: ts.URL, http: http.DefaultClient, token: created["token"].(string)}
	var issue map[string]any
	if code := agent.json("POST", "/api/v1/issues", map[string]any{"project": "WAY", "title": "Ship v1", "type": "epic", "priority": "high"}, &issue); code != 200 || issue["key"] != "WAY-1" {
		t.Fatalf("agent create: %d %v", code, issue)
	}
	agent.json("POST", "/api/v1/issues", map[string]any{"parent": "WAY-1", "title": "Write README", "assignees": []string{"me"}, "due_date": "tomorrow"}, &issue)
	code, body := agent.do("GET", "/api/v1/issues?q=assignee:me+is:open&format=text", nil)
	if code != 200 || !strings.Contains(string(body), `WAY-2 [To Do] "Write README" @robin`) {
		t.Fatalf("text search: %d %s", code, body)
	}
	var slim map[string]any
	agent.json("GET", "/api/v1/issues?q=type:epic&fields=key,title", nil, &slim)
	if got := slim["issues"].([]any)[0].(map[string]any); len(got) != 2 || got["key"] != "WAY-1" {
		t.Fatalf("fields: %v", slim)
	}
	if code := agent.json("POST", "/api/v1/issues/WAY-2/transition", map[string]any{"status": "done", "comment": "done!"}, &issue); code != 200 {
		t.Fatalf("transition: %d %v", code, issue)
	}
	agent.json("GET", "/api/v1/issues/WAY-1", nil, &issue)
	if issue["status"].(map[string]any)["category"] != "done" {
		t.Errorf("parent not closed: %v", issue["status"])
	}
	if code, b := agent.do("GET", "/api/v1/issues/WAY-99", nil); code != 404 || !strings.Contains(string(b), "WAY-99") {
		t.Errorf("missing issue: %d %s", code, b)
	}
	if code, _ := agent.do("GET", "/api/v1/issues?q=nope:x", nil); code != 400 {
		t.Errorf("bad query: %d", code)
	}
	if code, _ := (&client{t: t, base: ts.URL, http: http.DefaultClient, token: "wpt_bogus"}).do("GET", "/api/v1/me", nil); code != 401 {
		t.Errorf("bad token: %d", code)
	}
	for _, p := range []string{"/api/v1/projects/WAY/board", "/api/v1/projects/WAY/backlog", "/api/v1/projects/WAY/reports/forecast",
		"/api/v1/projects/WAY/reports/throughput", "/api/v1/projects/WAY/reports/flow", "/api/v1/projects/WAY/reports/roadmap",
		"/api/v1/reports/time", "/api/v1/notifications", "/api/v1/automation-templates", "/api/v1/help", "/healthz", "/metrics"} {
		if code, b := agent.do("GET", p, nil); code != 200 {
			t.Errorf("GET %s: %d %s", p, code, b)
		}
	}
	web.do("POST", "/api/v1/auth/logout", nil)
	if code, _ := web.do("GET", "/api/v1/me", nil); code != 401 {
		t.Errorf("after logout: %d", code)
	}
}

func TestLoginRateLimit(t *testing.T) {
	ts, svc := setup(t)
	svc.CreateUser(context.Background(), core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "j@example.com", Password: "correct horse battery"})
	c := &client{t: t, base: ts.URL, http: http.DefaultClient}
	if code, _ := c.do("POST", "/api/v1/auth/login", map[string]any{"login": "robin", "password": "correct horse battery"}); code != 403 {
		t.Errorf("login without X-Waypoint-Client: %d", code)
	}
	c.web = true
	// Successful logins don't count against the login rate limit.
	for n := 0; n < 12; n++ {
		if code, _ := c.do("POST", "/api/v1/auth/login", map[string]any{"login": "robin", "password": "correct horse battery"}); code != 200 {
			t.Fatalf("login %d: %d", n, code)
		}
	}
	var last int
	for n := 0; n < 12; n++ {
		last, _ = c.do("POST", "/api/v1/auth/login", map[string]any{"login": "robin", "password": "wrong password"})
	}
	if last != 429 {
		t.Errorf("expected rate limit, got %d", last)
	}
}

func TestMCP(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	u, _ := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "j@example.com", Password: "correct horse battery", IsAdmin: true})
	a := &core.Actor{User: u}
	name := "House"
	svc.CreateProject(ctx, a, core.ProjectInput{Name: &name, Key: &name})
	tok, _, _ := svc.CreateAPIToken(ctx, a, "mcp", false, 0)
	c := &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	rpc := func(method string, params any) map[string]any {
		t.Helper()
		var out map[string]any
		code := c.json("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}, &out)
		if code != 200 || out["error"] != nil {
			t.Fatalf("%s: %d %v", method, code, out)
		}
		return out["result"].(map[string]any)
	}
	init := rpc("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test"}})
	if init["protocolVersion"] != "2025-06-18" {
		t.Errorf("version: %v", init)
	}
	if code, _ := c.do("POST", "/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != 202 {
		t.Errorf("notification: %d", code)
	}
	tools := rpc("tools/list", nil)["tools"].([]any)
	if len(tools) < 10 {
		t.Errorf("tools: %d", len(tools))
	}
	text := func(res map[string]any) string {
		return res["content"].([]any)[0].(map[string]any)["text"].(string)
	}
	res := rpc("tools/call", map[string]any{"name": "create_issue", "arguments": map[string]any{"project": "HOUSE", "title": "Fix sink", "priority": "high", "labels": []string{"plumbing"}}})
	if !strings.Contains(text(res), `HOUSE-1 [To Do] high "Fix sink"`) {
		t.Errorf("create: %s", text(res))
	}
	res = rpc("tools/call", map[string]any{"name": "update_issue", "arguments": map[string]any{"key": "HOUSE-1", "points": 3, "due_date": "2026-12-01"}})
	if !strings.Contains(text(res), "3pts due:2026-12-01") {
		t.Errorf("update: %s", text(res))
	}
	res = rpc("tools/call", map[string]any{"name": "search_issues", "arguments": map[string]any{"query": "label:plumbing"}})
	if !strings.Contains(text(res), "HOUSE-1") {
		t.Errorf("search: %s", text(res))
	}
	res = rpc("tools/call", map[string]any{"name": "get_issue", "arguments": map[string]any{"key": "HOUSE-404"}})
	if res["isError"] != true {
		t.Errorf("missing issue should be a tool error: %v", res)
	}
	res = rpc("tools/call", map[string]any{"name": "timer", "arguments": map[string]any{"action": "start", "issue": "HOUSE-1"}})
	if text(res) != "Timer running on HOUSE-1" {
		t.Errorf("timer: %s", text(res))
	}
	if code, _ := (&client{t: t, base: ts.URL, http: http.DefaultClient}).do("POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); code != 401 {
		t.Errorf("anonymous mcp: %d", code)
	}
}

func TestStream(t *testing.T) {
	// Production's ReadTimeout (2 minutes, cmd/waypoint/main.go:378) must not end live streams.
	ts, svc := setupServer(t, func(s *http.Server) { s.ReadTimeout = 300 * time.Millisecond })
	ctx := context.Background()
	u, _ := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "j@example.com", Password: "correct horse battery", IsAdmin: true})
	other, _ := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "zed", Email: "z@example.com", Password: "correct horse battery"})
	a := &core.Actor{User: u}
	name := "Live"
	svc.CreateProject(ctx, a, core.ProjectInput{Name: &name})
	listen := func(user *core.User) (<-chan string, func()) {
		tok, _, _ := svc.CreateAPIToken(ctx, &core.Actor{User: user}, "s", true, 0)
		req, _ := http.NewRequest("GET", ts.URL+"/api/v1/stream", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		cctx, cancel := context.WithCancel(ctx)
		resp, err := http.DefaultClient.Do(req.WithContext(cctx))
		if err != nil {
			t.Fatal(err)
		}
		ch := make(chan string, 16)
		go func() {
			sc := bufio.NewScanner(resp.Body)
			for sc.Scan() {
				if strings.HasPrefix(sc.Text(), "event: ") {
					ch <- strings.TrimPrefix(sc.Text(), "event: ")
				}
			}
		}()
		return ch, func() { cancel(); resp.Body.Close() }
	}
	mine, stop1 := listen(u)
	defer stop1()
	theirs, stop2 := listen(other)
	defer stop2()
	<-mine
	<-theirs
	time.Sleep(700 * time.Millisecond)
	svc.CreateIssue(ctx, a, core.IssueInput{Project: "LIVE", Title: "hi"})
	select {
	case ev := <-mine:
		if ev != "issue.created" {
			t.Errorf("event = %s", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no live event")
	}
	select {
	case ev := <-theirs:
		t.Errorf("non-member received %s", ev)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestClientIPBehindProxy(t *testing.T) {
	private, _ := ParseTrustedProxies("1")
	s := &Server{cfg: Config{TrustedProxies: private}}
	for _, tc := range []struct {
		peer string
		xff  []string
		want string
	}{
		{"10.9.0.1", nil, "10.9.0.1"},
		{"10.9.0.1", []string{"198.51.100.7"}, "198.51.100.7"},
		{"10.9.0.1", []string{"1.2.3.4, 198.51.100.7"}, "198.51.100.7"},
		{"10.9.0.1", []string{"1.2.3.4", "198.51.100.7"}, "198.51.100.7"},
		{"127.0.0.1", []string{"198.51.100.7, 10.9.0.1"}, "198.51.100.7"},
		{"10.9.0.1", []string{"10.9.0.9"}, "10.9.0.9"},
		{"10.9.0.1", []string{"not-an-ip"}, "10.9.0.1"},
		{"203.0.113.5", []string{"1.2.3.4"}, "203.0.113.5"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = net.JoinHostPort(tc.peer, "1234")
		for _, v := range tc.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := s.clientIP(r); got != tc.want {
			t.Errorf("peer %s xff %q: got %q, want %q", tc.peer, tc.xff, got, tc.want)
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.5:1234"
	r.Header.Set("X-Forwarded-Proto", "https")
	if s.secureCookies(r) {
		t.Error("an untrusted peer marked cookies Secure")
	}
	r.RemoteAddr = "10.9.0.1:1234"
	if !s.secureCookies(r) {
		t.Error("the proxy's X-Forwarded-Proto was ignored")
	}
	untrusted := &Server{}
	r = httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.9.0.1:1234"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := untrusted.clientIP(r); got != "10.9.0.1" {
		t.Errorf("untrusted proxy header honoured: %q", got)
	}
	list, err := ParseTrustedProxies("203.0.113.0/24, 2001:db8::1")
	if err != nil || len(list) != 2 {
		t.Fatalf("parse list: %v %v", list, err)
	}
	if _, err := ParseTrustedProxies("proxy.example"); err == nil {
		t.Error("a host name was accepted")
	}
	custom := &Server{cfg: Config{TrustedProxies: list}}
	r.RemoteAddr = "[2001:db8::1]:443"
	if got := custom.clientIP(r); got != "1.2.3.4" {
		t.Errorf("listed proxy not trusted: %q", got)
	}
	r.RemoteAddr = "10.9.0.1:1234"
	if got := custom.clientIP(r); got != "10.9.0.1" {
		t.Errorf("unlisted private peer trusted: %q", got)
	}
}

func TestRateLimiterBurst(t *testing.T) {
	l := newRateLimiter(10, time.Minute)
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.allow("k") {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 10 {
		t.Errorf("allowed %d of a burst, limit 10", allowed)
	}
}

func TestLogPathRedactsCodes(t *testing.T) {
	mux := http.NewServeMux()
	var got string
	mux.HandleFunc("GET /api/v1/cli/login/{code}", func(w http.ResponseWriter, r *http.Request) { got = logPath(r) })
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/cli/login/BCDF-GHJK", nil))
	if strings.Contains(got, "BCDF") {
		t.Errorf("logged %q", got)
	}
}

func TestAttachments(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	robin, _ := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "j@example.com", Password: "correct horse battery", IsAdmin: true})
	zed, _ := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "zed", Email: "z@example.com", Password: "correct horse battery"})
	name := "Files"
	svc.CreateProject(ctx, &core.Actor{User: robin}, core.ProjectInput{Name: &name})
	issue, _ := svc.CreateIssue(ctx, &core.Actor{User: robin}, core.IssueInput{Project: "FILE", Title: "Has files"})
	tok, _, _ := svc.CreateAPIToken(ctx, &core.Actor{User: robin}, "t", false, 0)
	ztok, _, _ := svc.CreateAPIToken(ctx, &core.Actor{User: zed}, "t", false, 0)
	c := &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}

	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")
	var img, page core.Attachment
	if code := c.json("POST", "/api/v1/issues/"+issue.Key+"/attachments?name=../../shot.png", string(png), &img); code != 200 {
		t.Fatalf("upload: %d", code)
	}
	if img.Name != "shot.png" || !img.Inline || img.ContentType != "image/png" {
		t.Errorf("image attachment = %+v", img)
	}
	if code := c.json("POST", "/api/v1/issues/"+issue.Key+"/attachments?name=evil.html", "<html><script>alert(1)</script>", &page); code != 200 {
		t.Fatalf("upload html: %d", code)
	}
	get := func(token, url string) *http.Response {
		req, _ := http.NewRequest("GET", ts.URL+url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if r := get(tok, img.URL); r.StatusCode != 200 || r.Header.Get("Content-Type") != "image/png" || !strings.HasPrefix(r.Header.Get("Content-Disposition"), "inline") {
		t.Errorf("image served as %d %q %q", r.StatusCode, r.Header.Get("Content-Type"), r.Header.Get("Content-Disposition"))
	}
	r := get(tok, page.URL)
	if r.Header.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(r.Header.Get("Content-Disposition"), "attachment") ||
		!strings.Contains(r.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("html served as %q %q %q", r.Header.Get("Content-Type"), r.Header.Get("Content-Disposition"), r.Header.Get("Content-Security-Policy"))
	}
	if r := get(ztok, img.URL); r.StatusCode != 404 {
		t.Errorf("outsider fetched attachment: %d", r.StatusCode)
	}
	z := &client{t: t, base: ts.URL, http: http.DefaultClient, token: ztok}
	if code, _ := z.do("DELETE", fmt.Sprintf("/api/v1/attachments/%d", img.ID), nil); code != 404 {
		t.Errorf("outsider delete: %d", code)
	}
	var d core.IssueDetail
	c.json("GET", "/api/v1/issues/"+issue.Key, nil, &d)
	if len(d.Files) != 2 {
		t.Errorf("issue lists %d attachments", len(d.Files))
	}
	if code, _ := c.do("DELETE", fmt.Sprintf("/api/v1/attachments/%d", page.ID), nil); code != 204 {
		t.Errorf("delete: %d", code)
	}
}

func TestConditionalAndCompressed(t *testing.T) {
	ts, svc := setup(t)
	u, _ := svc.CreateUser(context.Background(), core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "j@example.com", Password: "correct horse battery"})
	tok, _, _ := svc.CreateAPIToken(context.Background(), &core.Actor{User: u}, "t", false, 0)
	get := func(path string, hdr map[string]string) *http.Response {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	r := get("/api/v1/me", nil)
	etag := r.Header.Get("ETag")
	if r.StatusCode != 200 || etag == "" || r.Header.Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("first: %d etag %q cc %q", r.StatusCode, etag, r.Header.Get("Cache-Control"))
	}
	if r := get("/api/v1/me", map[string]string{"If-None-Match": etag}); r.StatusCode != 304 {
		t.Errorf("revalidate: %d", r.StatusCode)
	}
	for _, tc := range []struct{ accept, want string }{
		{"gzip, br", "br"}, {"gzip", "gzip"}, {"br;q=0, gzip", "gzip"}, {"identity", ""},
	} {
		if !accepts(&http.Request{Header: http.Header{"Accept-Encoding": {tc.accept}}}, "br") && tc.want == "br" {
			t.Errorf("%q should accept br", tc.accept)
		}
	}
	if accepts(&http.Request{Header: http.Header{"Accept-Encoding": {"br;q=0"}}}, "br") {
		t.Error("q=0 accepted")
	}
}

func TestAgentAPIAndDeviceLogin(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	u, _ := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "j@example.com", Password: "correct horse battery", IsAdmin: true})
	name := "Agents"
	svc.CreateProject(ctx, &core.Actor{User: u}, core.ProjectInput{Name: &name})
	svc.CreateIssue(ctx, &core.Actor{User: u}, core.IssueInput{Project: "AGEN", Title: "Write the parser", Agent: "claude", Workflow: "Agent pipeline"})
	svc.CreateIssue(ctx, &core.Actor{User: u}, core.IssueInput{Project: "AGEN", Title: "Human chore", Agent: "none"})

	cli := &client{t: t, base: ts.URL, http: http.DefaultClient, web: true}
	var start core.DeviceLogin
	if code := cli.json("POST", "/api/v1/cli/login", map[string]string{"name": "laptop", "agent": "anthropic/claude-opus-5-5"}, &start); code != 200 || start.UserCode == "" {
		t.Fatalf("start: %d %+v", code, start)
	}
	var poll map[string]string
	cli.json("POST", "/api/v1/cli/login/poll", map[string]string{"device_code": start.DeviceCode}, &poll)
	if poll["status"] != "pending" {
		t.Fatalf("poll before approval: %v", poll)
	}
	tok, _, _ := svc.CreateAPIToken(ctx, &core.Actor{User: u}, "approver", false, 0)
	me := &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	var info map[string]any
	if code := me.json("GET", "/api/v1/cli/login/"+start.UserCode, nil, &info); code != 200 || info["same_address"] != true || info["your_ip"] != "127.0.0.1" {
		t.Errorf("login info: %d %v", code, info)
	}
	if code, b := me.do("POST", "/api/v1/cli/login/"+strings.ToLower(strings.ReplaceAll(start.UserCode, "-", ""))+"/approve", nil); code != 204 {
		t.Fatalf("approve: %d %s", code, b)
	}
	cli.json("POST", "/api/v1/cli/login/poll", map[string]string{"device_code": start.DeviceCode}, &poll)
	if poll["status"] != "approved" || !strings.HasPrefix(poll["token"], "wpt_") {
		t.Fatalf("poll after approval: %v", poll)
	}
	if code, _ := cli.do("POST", "/api/v1/cli/login/poll", map[string]string{"device_code": start.DeviceCode}); code != 404 {
		t.Errorf("token handed out twice: %d", code)
	}

	agent := &client{t: t, base: ts.URL, http: http.DefaultClient, token: poll["token"]}
	code, b := agent.do("GET", "/api/v1/agent", nil)
	if code != 200 || !strings.Contains(string(b), "next") || len(b) > 2500 {
		t.Errorf("index: %d (%d bytes)", code, len(b))
	}
	code, b = agent.do("POST", "/api/v1/agent/next", nil)
	if code != 200 || !strings.Contains(string(b), "Write the parser") || !strings.Contains(string(b), "Step 1/5") || strings.Contains(string(b), "Human chore") {
		t.Fatalf("next: %d %s", code, b)
	}
	key := strings.Fields(string(b))[0]
	code, b = agent.do("POST", "/api/v1/agent/done/"+key, map[string]any{"comment": "Planned.", "usage": map[string]any{"input_tokens": 10}})
	if code != 200 || !strings.Contains(string(b), "verify") {
		t.Errorf("done: %d %s", code, b)
	}
	plain, _, _ := svc.CreateAPIToken(ctx, &core.Actor{User: u}, "plain", false, 0)
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/agent/next?format=json", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	req.Header.Set("X-Waypoint-Agent", "openai/gpt-6")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out["task"] != nil {
		t.Errorf("a non-Claude agent got a task meant for claude: %v", out["task"])
	}
	ro, _, _ := svc.CreateAPIToken(ctx, &core.Actor{User: u}, "ro", true, 0)
	look := &client{t: t, base: ts.URL, http: http.DefaultClient, token: ro}
	if code, b := look.do("GET", "/api/v1/agent/next?agent=anthropic/claude-sonnet-5-5&step=verify", nil); code != 200 || !strings.Contains(string(b), "Write the parser") {
		t.Errorf("read-only peek: %d %s", code, b)
	}
	if code, _ := look.do("POST", "/api/v1/agent/next", map[string]any{"agent": "anthropic/claude-sonnet-5-5"}); code != 403 {
		t.Errorf("read-only claim: %d", code)
	}

	code, b = cli.do("GET", "/cli", nil)
	if code != 200 || !strings.Contains(string(b), "base='http://wp.test'") {
		t.Errorf("installer: %d", code)
	}
	if !strings.Contains(string(b), "setup=0") || !strings.Contains(string(b), `exec "$dir/wp" setup`) {
		t.Errorf("installer can't go on to wp setup:\n%s", b)
	}
	code, b = cli.do("GET", "/setup", nil)
	if code != 200 || !strings.Contains(string(b), "setup=1") || !strings.Contains(string(b), "base='http://wp.test'") {
		t.Errorf("setup script: %d\n%s", code, b)
	}
	if _, b = cli.do("GET", "/cli/..%2F..%2Fetc%2Fpasswd", nil); strings.Contains(string(b), "root:") {
		t.Error("cli file path escaped")
	}
	if code, _ = cli.do("GET", "/cli/wp-linux-amd64", nil); code != 404 {
		t.Errorf("missing binary: %d", code)
	}
}
