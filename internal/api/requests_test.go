package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// reqClient calls the API with extra headers (the agent's identity, session
// and machine).
type reqClient struct {
	t       *testing.T
	base    string
	token   string
	headers map[string]string
}

func (c *reqClient) do(method, path string, body any) (int, []byte) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+c.token)
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (c *reqClient) json(method, path string, body, out any) int {
	c.t.Helper()
	code, b := c.do(method, path, body)
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			c.t.Fatalf("%s %s: decode %q: %v", method, path, b, err)
		}
	}
	return code
}

type reqEnv struct {
	t       *testing.T
	ts      string
	svc     *core.Service
	robin   *core.Actor
	person  *reqClient
	agent   *reqClient
	project *core.Project
}

func newReqEnv(t *testing.T) *reqEnv {
	ts, svc := setup(t)
	ctx := context.Background()
	u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "j@example.com", Password: "correct horse battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	robin := &core.Actor{User: u}
	name := "Acme"
	p, err := svc.CreateProject(ctx, robin, core.ProjectInput{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	human, _, _ := svc.CreateAPIToken(ctx, robin, "person", false, 0)
	agentTok, _, err := svc.CreateToken(ctx, robin, core.TokenSpec{Name: "agent", Agent: "anthropic/claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	e := &reqEnv{t: t, ts: ts.URL, svc: svc, robin: robin, project: p}
	e.person = &reqClient{t: t, base: ts.URL, token: human}
	e.agent = &reqClient{t: t, base: ts.URL, token: agentTok, headers: map[string]string{"X-Waypoint-Machine": "box"}}
	var res core.HookResult
	if code := e.agent.json("POST", "/api/v1/sessions/hook", map[string]any{"events": []map[string]any{
		{"event": "SessionStart", "session_id": "cli-1", "machine": "box", "cwd": "/work/app", "repo": "acme/app", "branch": "main", "model": "claude-opus-5"},
		{"event": "UserPromptSubmit", "session_id": "cli-1", "machine": "box", "cwd": "/work/app", "prompt": "do it", "title": "Doing it"},
	}}, &res); code != 200 || res.Session == 0 {
		t.Fatalf("hook: %d %+v", code, res)
	}
	e.agent.headers["X-Waypoint-Session"] = "cli-1"
	if _, err := svc.ListAgentSessions(ctx, robin, core.AgentSessionQuery{}); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestSessionsAPI(t *testing.T) {
	e := newReqEnv(t)
	var page struct {
		Sessions []map[string]any `json:"sessions"`
		Total    int              `json:"total"`
	}
	if code := e.person.json("GET", "/api/v1/sessions?machine=box&state=active&agent=claude", nil, &page); code != 200 || page.Total != 1 {
		t.Fatalf("list: %d %+v", code, page)
	}
	s := page.Sessions[0]
	if s["machine"] != "box" || s["state"] != "working" || s["title"] != "Doing it" || s["cwd"] != "/work/app" || s["agent"] != "anthropic/claude-code" {
		t.Fatalf("session: %v", s)
	}
	if code := e.person.json("GET", "/api/v1/sessions?machine=laptop", nil, &page); code != 200 || page.Total != 0 {
		t.Fatalf("filtered: %d %+v", code, page)
	}
	if code, _ := e.person.do("GET", "/api/v1/sessions?state=nonsense", nil); code != 400 {
		t.Fatalf("bad state: %d", code)
	}

	var created map[string]any
	if code := e.agent.json("POST", "/api/v1/issues", map[string]any{"project": e.project.Key, "title": "Follow up", "status": "Backlog"}, &created); code != 200 && code != 201 {
		t.Fatalf("create issue: %d %v", code, created)
	}
	var sum struct {
		Session  map[string]any `json:"session"`
		Doing    map[string]any `json:"doing"`
		Done     map[string]any `json:"done"`
		Deferred map[string]any `json:"deferred"`
		Timeline []map[string]any
	}
	id := int(s["id"].(float64))
	if code := e.person.json("GET", "/api/v1/sessions/"+itoaJSON(id), nil, &sum); code != 200 {
		t.Fatalf("summary: %d", code)
	}
	if len(sum.Done["issues_created"].([]any)) != 1 || len(sum.Deferred["issues"].([]any)) != 1 || sum.Doing["title"] != "Doing it" || len(sum.Timeline) < 3 {
		t.Fatalf("summary: %+v", sum)
	}
	if code, _ := e.person.do("GET", "/api/v1/sessions/999", nil); code != 404 {
		t.Fatalf("missing session: %d", code)
	}
	if code, _ := (&reqClient{t: e.t, base: e.ts, token: "wpt_nope"}).do("POST", "/api/v1/sessions/hook", map[string]any{"events": []any{}}); code != 401 {
		t.Fatalf("a bad token: %d", code)
	}
	if code, _ := e.agent.do("POST", "/api/v1/sessions/hook", map[string]any{"events": []map[string]any{{"event": "Stop"}}}); code != 400 {
		t.Fatalf("an event without a session id: %d", code)
	}
}

func itoaJSON(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestRequestsAPI(t *testing.T) {
	e := newReqEnv(t)
	var q struct {
		core.Request
		New bool `json:"new"`
	}
	body := map[string]any{"kind": "question", "title": "Which database?", "body": "Matters for tests.",
		"questions": []map[string]any{{"question": "Which database?", "choices": []map[string]any{{"label": "sqlite"}, {"label": "postgres"}}}}}
	if code := e.agent.json("POST", "/api/v1/requests", body, &q); code != 200 || !q.New || q.Ref != "me:R-1" || q.Session == nil || q.Session.Machine != "box" || q.State != "open" {
		t.Fatalf("create: %d %+v", code, q.Request)
	}
	ref := q.Ref

	perm := map[string]any{"kind": "permission", "title": "Allow Bash?", "tool": "Bash", "tool_input": map[string]any{"command": "make"}}
	var p1, p2 struct {
		ID  int64 `json:"id"`
		New bool  `json:"new"`
	}
	e.agent.json("POST", "/api/v1/requests", perm, &p1)
	e.agent.json("POST", "/api/v1/requests", perm, &p2)
	if !p1.New || p2.New || p1.ID != p2.ID {
		t.Fatalf("dedupe: %+v %+v", p1, p2)
	}

	var page core.RequestPage
	if code := e.agent.json("GET", "/api/v1/requests?mine=1", nil, &page); code != 200 || page.Total != 2 {
		t.Fatalf("list: %d %d", code, page.Total)
	}
	if code := e.agent.json("GET", "/api/v1/requests?state=answered", nil, &page); code != 200 || page.Total != 0 {
		t.Fatalf("answered: %d %d", code, page.Total)
	}
	if code, _ := e.agent.do("GET", "/api/v1/requests?state=bogus", nil); code != 400 {
		t.Fatalf("bad state: %d", code)
	}
	var w core.Waiting
	if code := e.person.json("GET", "/api/v1/waiting", nil, &w); code != 200 || len(w.Requests) != 2 || w.Total < 2 {
		t.Fatalf("waiting: %d %+v", code, w)
	}

	for _, c := range []struct {
		path string
		body any
	}{
		{"/decide", map[string]any{"decision": "answer", "text": "sqlite"}},
		{"/snooze", map[string]any{"minutes": 5}},
		{"/cells/1/run", map[string]any{}},
	} {
		if code, b := e.agent.do("POST", "/api/v1/requests/"+ref+c.path, c.body); code != 403 {
			t.Fatalf("agent %s: %d %s", c.path, code, b)
		}
	}
	ro, _, _ := e.svc.CreateAPIToken(context.Background(), e.robin, "ro", true, 0)
	if code, _ := (&reqClient{t: t, base: e.ts, token: ro}).do("POST", "/api/v1/requests/"+ref+"/decide", map[string]any{"decision": "dismiss"}); code != 403 {
		t.Fatalf("read-only decided: %d", code)
	}
	if code, _ := e.person.do("POST", "/api/v1/requests/"+ref+"/decide", map[string]any{"decision": "answer", "answers": []map[string]any{{"choices": []string{"mysql"}}}}); code != 400 {
		t.Fatalf("a choice that isn't one: %d", code)
	}
	if code, _ := e.person.do("POST", "/api/v1/requests/"+ref+"/decide", map[string]any{"decision": "answer", "bogus": 1}); code != 400 {
		t.Fatalf("unknown field: %d", code)
	}

	go func() {
		time.Sleep(300 * time.Millisecond)
		e.person.do("POST", "/api/v1/requests/"+ref+"/decide", map[string]any{"decision": "answer", "answers": []map[string]any{{"choices": []string{"postgres"}}}, "note": "go on"})
	}()
	start := time.Now()
	var got core.Request
	if code := e.agent.json("GET", "/api/v1/requests/"+ref+"?wait=10", nil, &got); code != 200 || got.State != "answered" || got.Answer != "postgres" || got.Note != "go on" {
		t.Fatalf("wait: %d %+v", code, got)
	}
	if time.Since(start) > 8*time.Second {
		t.Fatal("wait took the whole time")
	}
	code, b := e.agent.do("POST", "/api/v1/agent/next", nil)
	if code != 200 || strings.Contains(string(b), "answered request") {
		t.Fatalf("next after reading: %d %s", code, b)
	}

	var sn core.Request
	if code := e.person.json("POST", "/api/v1/requests/"+itoaJSON(int(p1.ID))+"/snooze", map[string]any{"minutes": 30}, &sn); code != 200 || sn.SnoozedUntil == nil {
		t.Fatalf("snooze: %d %+v", code, sn)
	}
	var wd core.Request
	if code := e.agent.json("POST", "/api/v1/requests/"+itoaJSON(int(p1.ID))+"/withdraw", nil, &wd); code != 200 || wd.State != "withdrawn" {
		t.Fatalf("withdraw: %d %+v", code, wd)
	}
	if code, _ := e.person.do("GET", "/api/v1/requests/R-404", nil); code != 404 {
		t.Fatalf("missing: %d", code)
	}

	var act, own core.Request
	e.agent.json("POST", "/api/v1/requests", map[string]any{"kind": "action", "title": "Create the repo"}, &act)
	if code := e.agent.json("POST", "/api/v1/requests/"+act.Ref+"/decide", map[string]any{"decision": "done", "note": "main is pushed"}, &own); code != 200 || own.State != "withdrawn" || own.Note != "main is pushed" {
		t.Fatalf("agent's done: %d %+v", code, own)
	}
	e.agent.json("POST", "/api/v1/requests", map[string]any{"kind": "action", "title": "Rotate the key"}, &act)
	if code := e.agent.json("POST", "/api/v1/requests/"+act.Ref+"/withdraw", map[string]any{"note": "rotated by the runner"}, &own); code != 200 || own.State != "withdrawn" || own.Note != "rotated by the runner" {
		t.Fatalf("withdraw with a note: %d %+v", code, own)
	}
}

func TestNextMentionsAnsweredRequests(t *testing.T) {
	e := newReqEnv(t)
	var q core.Request
	e.agent.json("POST", "/api/v1/requests", map[string]any{"kind": "action", "title": "Set the secret"}, &q)
	e.person.do("POST", "/api/v1/requests/"+q.Ref+"/decide", map[string]any{"decision": "done"})
	code, b := e.agent.do("POST", "/api/v1/agent/next", nil)
	if code != 200 || !strings.Contains(string(b), "1 answered request: wp request ls --answered") {
		t.Fatalf("next: %d %s", code, b)
	}
	var js map[string]any
	e.agent.json("GET", "/api/v1/agent/next?format=json", nil, &js)
	if js["answered_requests"] != float64(1) {
		t.Fatalf("json: %v", js)
	}
	var page core.RequestPage
	e.agent.json("GET", "/api/v1/requests?mine=1&state=answered", nil, &page)
	if page.Total != 1 {
		t.Fatalf("answered list: %d", page.Total)
	}
	var res core.HookResult
	e.agent.json("POST", "/api/v1/sessions/hook", map[string]any{"events": []map[string]any{{"event": "Stop", "session_id": "cli-1", "machine": "box"}}}, &res)
	if len(res.Deliver) != 1 || !strings.Contains(res.Text, "did it") {
		t.Fatalf("stop: %+v", res)
	}
	if code, b := e.agent.do("POST", "/api/v1/agent/next", nil); code != 200 || strings.Contains(string(b), "answered request") {
		t.Fatalf("next after delivery: %d %s", code, b)
	}
}

func TestRequestMCPTools(t *testing.T) {
	e := newReqEnv(t)
	call := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		var out struct {
			Result struct {
				Content []struct{ Text string } `json:"content"`
				IsError bool                    `json:"isError"`
			} `json:"result"`
		}
		code := e.agent.json("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}}, &out)
		if code != 200 || len(out.Result.Content) == 0 {
			t.Fatalf("%s: %d %+v", name, code, out)
		}
		return out.Result.Content[0].Text, out.Result.IsError
	}
	text, isErr := call("create_request", map[string]any{"kind": "question", "title": "Tabs or spaces?", "choices": []string{"tabs", "spaces"}})
	if isErr || !strings.Contains(text, "Filed me:R-1") {
		t.Fatalf("create_request: %v %s", isErr, text)
	}
	text, _ = call("create_request", map[string]any{"kind": "change", "title": "Edit the rules", "files": []map[string]any{{"path": "CLAUDE.md", "content": "# New\n"}}, "message": "docs"})
	if !strings.Contains(text, "R-2") {
		t.Fatalf("change: %s", text)
	}
	text, _ = call("create_request", map[string]any{"kind": "command", "title": "Migrate", "commands": []string{"make migrate"}, "dir": "db"})
	if !strings.Contains(text, "R-3") {
		t.Fatalf("command: %s", text)
	}
	if _, isErr = call("create_request", map[string]any{"kind": "bogus", "title": "x"}); !isErr {
		t.Fatal("a bad kind should be a tool error")
	}
	text, _ = call("list_requests", nil)
	for _, want := range []string{"R-1", "R-2", "R-3", "Tabs or spaces?"} {
		if !strings.Contains(text, want) {
			t.Fatalf("list_requests lacks %s: %s", want, text)
		}
	}
	e.person.do("POST", "/api/v1/requests/me:R-1/decide", map[string]any{"decision": "answer", "answers": []map[string]any{{"choices": []string{"spaces"}}}})
	text, _ = call("get_request", map[string]any{"ref": "R-1"})
	if !strings.Contains(text, "answered") || !strings.Contains(text, "spaces") {
		t.Fatalf("get_request: %s", text)
	}
	if text, _ = call("list_requests", map[string]any{"state": "answered"}); strings.Contains(text, "R-1") {
		t.Fatalf("read request still listed as answered: %s", text)
	}
}

func TestAgentHelpHasRequests(t *testing.T) {
	topic, ok := agentHelp("requests")
	if !ok || !strings.Contains(topic, "wp request ask") || !strings.Contains(topic, "Only people decide") {
		t.Fatalf("topic: %v %q", ok, topic)
	}
	if idx, _ := agentHelp("index"); !strings.Contains(idx, "requests:") {
		t.Fatal("the index doesn't mention requests")
	}
}
