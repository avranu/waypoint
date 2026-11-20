package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestAutomationAPI(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "robin@example.com", Password: "correct-horse-battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	me := &core.Actor{User: u}
	tok, _, _ := svc.CreateAPIToken(ctx, me, "robin", false, 0)
	person := &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	agentTok, _, _ := svc.CreateToken(ctx, me, core.TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	agent := &client{t: t, base: ts.URL, http: http.DefaultClient, token: agentTok}
	anon := &client{t: t, base: ts.URL, http: http.DefaultClient}

	if code, b := person.do("POST", "/api/v1/projects", map[string]any{"name": "Widgets", "key": "WID"}); code != 200 {
		t.Fatalf("project: %d %s", code, b)
	}

	// The catalog.
	var cat core.Catalog
	if code := person.json("GET", "/api/v1/automation-catalog", nil, &cat); code != 200 || len(cat.Triggers) < 30 || len(cat.Actions) < 25 {
		t.Fatalf("catalog: %d %d triggers, %d actions", code, len(cat.Triggers), len(cat.Actions))
	}
	var sawSystem, sawParams bool
	for _, d := range cat.Triggers {
		sawSystem = sawSystem || d.Admin
		sawParams = sawParams || (d.Name == "count_crossed" && len(d.Params) == 3)
	}
	if !sawSystem || !sawParams {
		t.Errorf("catalog lacks admin triggers (%v) or parameters (%v)", sawSystem, sawParams)
	}
	if code, _ := anon.do("GET", "/api/v1/automation-catalog", nil); code != 401 {
		t.Errorf("catalog without a token: %d", code)
	}

	// Create a manual automation that allows agents, with inputs; run it.
	rule := map[string]any{
		"trigger": map[string]any{"event": "manual"}, "allow_agents": true, "concurrency": "skip",
		"inputs":  []any{map[string]any{"name": "title", "type": "text", "required": true}},
		"actions": []any{map[string]any{"type": "create_issue", "id": "mk", "title": "{{.Inputs.title}}"}, map[string]any{"type": "delay", "id": "later", "duration": "1m"}},
	}
	var a core.Automation
	if code, b := person.do("POST", "/api/v1/projects/WID/automations", map[string]any{"name": "Make", "rule": rule}); code != 200 {
		t.Fatalf("create: %d %s", code, b)
	} else if err := jsonInto(b, &a); err != nil {
		t.Fatal(err)
	}
	if !a.Durable || a.Rule.Concurrency != "skip" || a.Rule.Actions[1].Extra["duration"] != "1m" {
		t.Errorf("created = %+v", a)
	}
	var run core.AutomationRun
	if code, b := person.do("POST", autoPath("/api/v1/automations/", a.ID, "/run"), map[string]any{"inputs": map[string]any{"title": "From the API"}}); code != 200 {
		t.Fatalf("run: %d %s", code, b)
	} else if err := jsonInto(b, &run); err != nil {
		t.Fatal(err)
	}
	if run.Status != core.RunQueued || run.Via != "api" {
		t.Errorf("run = %+v", run)
	}
	svc.DrainAutomationRuns(ctx)
	var got core.AutomationRun
	if code := person.json("GET", autoPath("/api/v1/automation-runs/", run.ID, ""), nil, &got); code != 200 || got.Status != core.RunWaiting || len(got.Steps) != 2 || got.Steps[0].Outputs["Key"] == nil {
		t.Fatalf("run detail: %d %+v", code, got)
	}
	if code, b := person.do("POST", autoPath("/api/v1/automations/", a.ID, "/run"), map[string]any{"inputs": map[string]any{"title": "again"}}); code != 200 || !strings.Contains(string(b), `"skipped"`) {
		t.Errorf("second run while one waits: %d %s", code, b)
	}
	var list []core.AutomationRun
	if code := person.json("GET", autoPath("/api/v1/automations/", a.ID, "/runs?limit=5"), nil, &list); code != 200 || len(list) != 2 {
		t.Errorf("runs: %d %d", code, len(list))
	}
	if code, b := person.do("POST", autoPath("/api/v1/automations/", a.ID, "/run"), map[string]any{"inputs": map[string]any{}}); code != 400 || !strings.Contains(string(b), "title is required") {
		t.Errorf("missing input: %d %s", code, b)
	}
	// An agent may run it, since it allows agents; not a stricter one.
	if code, b := agent.do("POST", autoPath("/api/v1/automations/", a.ID, "/run"), map[string]any{"inputs": map[string]any{"title": "by agent"}}); code != 200 {
		t.Errorf("agent run of an agent-friendly automation: %d %s", code, b)
	}
	strict := map[string]any{"trigger": map[string]any{"event": "manual"}, "actions": []any{map[string]any{"type": "comment", "message": "x"}}}
	var s2 core.Automation
	if code, b := person.do("POST", "/api/v1/projects/WID/automations", map[string]any{"name": "Strict", "rule": strict}); code != 200 {
		t.Fatalf("create strict: %d %s", code, b)
	} else if err := jsonInto(b, &s2); err != nil {
		t.Fatal(err)
	}
	if code, b := agent.do("POST", autoPath("/api/v1/automations/", s2.ID, "/run"), nil); code != 403 {
		t.Errorf("agent ran an automation that doesn't allow agents: %d %s", code, b)
	}
	// Cancel, and run again.
	var cancelled core.AutomationRun
	if code := person.json("POST", autoPath("/api/v1/automation-runs/", run.ID, "/cancel"), nil, &cancelled); code != 200 || cancelled.Status != core.RunCancelled {
		t.Errorf("cancel: %d %+v", code, cancelled)
	}
	var again core.AutomationRun
	if code := person.json("POST", autoPath("/api/v1/automation-runs/", run.ID, "/rerun"), nil, &again); code != 200 || again.RerunOf == nil || again.Inputs["title"] != "From the API" {
		t.Errorf("rerun: %d %+v", code, again)
	}

	// A test run changes nothing.
	var res core.TestResult
	if code := person.json("POST", autoPath("/api/v1/automations/", a.ID, "/test"), map[string]any{"inputs": map[string]any{"title": "Dry"}}, &res); code != 200 || len(res.Steps) != 2 || res.Steps[0].Status != "would_run" {
		t.Errorf("test: %d %+v", code, res)
	}

	// Cron preview.
	var next struct {
		Next []string `json:"next"`
	}
	if code := person.json("GET", "/api/v1/automation-cron?expr=0+9+*+*+1-5&tz=Europe/London&n=3", nil, &next); code != 200 || len(next.Next) != 3 {
		t.Errorf("cron: %d %+v", code, next)
	}
	if code, _ := person.do("GET", "/api/v1/automation-cron?expr=nope", nil); code != 400 {
		t.Errorf("bad cron: %d", code)
	}

	// The incoming webhook.
	hook := map[string]any{"trigger": map[string]any{"event": "webhook", "secret": "s3cret"}, "actions": []any{map[string]any{"type": "create_issue", "title": "{{.Trigger.Data.what}}"}}}
	var h core.Automation
	if code, b := person.do("POST", "/api/v1/projects/WID/automations", map[string]any{"name": "Inbox", "rule": hook}); code != 200 {
		t.Fatalf("create hook: %d %s", code, b)
	} else if err := jsonInto(b, &h); err != nil {
		t.Fatal(err)
	}
	path := strings.TrimPrefix(h.HookURL, "http://wp.test")
	if !strings.HasPrefix(path, "/hooks/automations/") {
		t.Fatalf("hook url = %q", h.HookURL)
	}
	body := `{"what":"Disk full"}`
	post := func(sig string) (int, []byte) {
		req, _ := http.NewRequest("POST", ts.URL+path, bytes.NewReader([]byte(body)))
		if sig != "" {
			req.Header.Set("X-Hub-Signature-256", sig)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		buf.ReadFrom(resp.Body)
		return resp.StatusCode, buf.Bytes()
	}
	if code, _ := post(""); code != 401 {
		t.Errorf("unsigned hook: %d", code)
	}
	m := hmac.New(sha256.New, []byte("s3cret"))
	m.Write([]byte(body))
	if code, b := post("sha256=" + hex.EncodeToString(m.Sum(nil))); code != 202 || !strings.Contains(string(b), `"status":"succeeded"`) {
		t.Errorf("signed hook: %d %s", code, b)
	}
	var found struct {
		Issues []struct{ Title string } `json:"issues"`
	}
	if code := person.json("GET", "/api/v1/search?q=title:Disk&project=WID", nil, &found); code != 200 || len(found.Issues) != 1 {
		t.Errorf("hook issue: %d %+v", code, found)
	}
	if code, _ := anon.do("POST", "/hooks/automations/"+strings.Repeat("x", 40), nil); code != 404 {
		t.Errorf("unknown address: %d", code)
	}
	big := bytes.Repeat([]byte("x"), maxHookBody+10)
	req, _ := http.NewRequest("POST", ts.URL+path, bytes.NewReader(big))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 413 {
		t.Errorf("oversized body: %v %v", resp, err)
	}

	// Admin automations.
	if code, _ := agent.do("GET", "/api/v1/admin/automations", nil); code != 403 {
		t.Errorf("agent listed admin automations: %d", code)
	}
	adminRule := map[string]any{"trigger": map[string]any{"event": "system_resources"}, "actions": []any{map[string]any{"type": "notify", "user": "admins", "message": "{{.Trigger.Data.text}}"}}}
	if code, b := person.do("POST", "/api/v1/admin/automations", map[string]any{"name": "Low disk", "rule": adminRule}); code != 200 {
		t.Fatalf("admin create: %d %s", code, b)
	}
	var admins []core.Automation
	if code := person.json("GET", "/api/v1/admin/automations", nil, &admins); code != 200 || len(admins) != 1 || !admins[0].Admin {
		t.Errorf("admin list: %d %+v", code, admins)
	}
	var sys struct {
		Resources []core.ResourceStatus `json:"resources"`
		Limits    core.ResourceLimits   `json:"limits"`
	}
	if code := person.json("GET", "/api/v1/admin/system", nil, &sys); code != 200 || len(sys.Resources) == 0 {
		t.Errorf("system: %d %+v", code, sys)
	}
	if code, b := person.do("PUT", "/api/v1/admin/system-limits", map[string]any{"disk_free_mb": 2048, "database_mb": 0, "memory_free_mb": 128}); code != 200 {
		t.Errorf("limits: %d %s", code, b)
	}
	if code, _ := agent.do("PUT", "/api/v1/admin/system-limits", map[string]any{"disk_free_mb": 1}); code != 403 {
		t.Errorf("agent set limits: %d", code)
	}

	// MCP tools.
	text := callMCPTool(t, ts.URL, agentTok, "list_automations", map[string]any{"project": "WID"})
	if !strings.Contains(text, "Make") || !strings.Contains(text, "you may") || !strings.Contains(text, "Strict") || !strings.Contains(text, "you may not") {
		t.Errorf("list_automations: %s", text)
	}
}

func autoPath(prefix string, id int64, suffix string) string {
	return prefix + strconv.FormatInt(id, 10) + suffix
}

func jsonInto(b []byte, v any) error { return json.Unmarshal(b, v) }
