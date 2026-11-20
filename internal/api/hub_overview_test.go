package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestAgentOverviewChangesAndSessionUsageAPI(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	mk := func(name string, admin bool) (*core.Actor, *client) {
		u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: name, Email: name + "@example.com", Password: "correct-horse-battery", IsAdmin: admin})
		if err != nil {
			t.Fatal(err)
		}
		a := &core.Actor{User: u}
		tok, _, _ := svc.CreateAPIToken(ctx, a, name, false, 0)
		return a, &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	}
	robin, human := mk("robin", true)
	_, stranger := mk("stranger", false)
	agentTok, _, _ := svc.CreateToken(ctx, robin, core.TokenSpec{Name: "code", Agent: "anthropic/claude-code"})
	agent := &client{t: t, base: ts.URL, http: http.DefaultClient, token: agentTok}

	if code, b := human.do("POST", "/api/v1/projects", map[string]any{"name": "Widgets", "key": "WID"}); code != 200 {
		t.Fatalf("project: %d %s", code, b)
	}
	if code, b := human.do("POST", "/api/v1/issues", map[string]any{"project": "WID", "title": "Parser", "agent": "claude", "priority": "high"}); code != 200 {
		t.Fatalf("issue: %d %s", code, b)
	}
	// The agent's session (a hook), a question from it, and a thing it filed.
	var hook core.HookResult
	agent.http = &http.Client{Transport: headerTransport{"X-Waypoint-Machine": "box"}}
	if code := agent.json("POST", "/api/v1/sessions/hook", map[string]any{"events": []map[string]any{
		{"event": "SessionStart", "session_id": "cli-1", "machine": "box", "cwd": "/work/app", "repo": "me/app", "branch": "feat/x"},
		{"event": "UserPromptSubmit", "session_id": "cli-1", "machine": "box", "title": "Parser work", "prompt": "do it"},
	}}, &hook); code != 200 || hook.Session == 0 {
		t.Fatalf("hook: %d %+v", code, hook)
	}
	agent.http = &http.Client{Transport: headerTransport{"X-Waypoint-Machine": "box", "X-Waypoint-Session": "cli-1"}}
	if code, b := agent.do("POST", "/api/v1/requests", map[string]any{"kind": "question", "title": "Which db?", "questions": []map[string]any{{"question": "Which?", "choices": []map[string]any{{"label": "a"}, {"label": "b"}}}}}); code != 200 {
		t.Fatalf("request: %d %s", code, b)
	}
	if code, b := agent.do("POST", "/api/v1/issues", map[string]any{"project": "WID", "title": "Follow up", "status": "backlog"}); code != 200 {
		t.Fatalf("filed: %d %s", code, b)
	}

	var ov core.Overview
	if code := human.json("GET", "/api/v1/agents/overview?days=7", nil, &ov); code != 200 {
		t.Fatalf("overview: %d", code)
	}
	if ov.Days != 7 || ov.SessionsTotal != 1 || ov.Sessions[0].Title != "Parser work" || ov.Sessions[0].URL != "/agents/sessions/"+id64(hook.Session) {
		t.Errorf("sessions: %+v", ov.Sessions)
	}
	if len(ov.Waiting.Requests) != 1 || ov.Waiting.Requests[0].Title != "Which db?" || ov.Summary.Waiting.Count < 1 || ov.Summary.Doing.Count != 1 {
		t.Errorf("waiting %+v, summary %+v", ov.Waiting.Requests, ov.Summary)
	}
	if len(ov.Next) != 1 || ov.Next[0].Agent != "claude" || ov.Next[0].Items[0].Key != "WID-1" {
		t.Errorf("next: %+v", ov.Next)
	}
	if len(ov.Branches) != 1 || ov.Branches[0].Branch.Branch != "feat/x" || len(ov.Branches[0].Sessions) != 1 {
		t.Errorf("branches: %+v", ov.Branches)
	}
	if ov.Environments == nil || ov.Changes == nil || len(ov.Changes.Changes) == 0 || ov.Pulls.Open == nil {
		t.Errorf("parts: %+v", ov)
	}
	// Filters; a bad one; a stranger sees nothing; unauthenticated is refused.
	var narrowed core.Overview
	if code := human.json("GET", "/api/v1/agents/overview?agent=haiku&project=WID", nil, &narrowed); code != 200 || narrowed.SessionsTotal != 0 || len(narrowed.Waiting.Requests) != 0 {
		t.Errorf("narrowed: %d %+v", code, narrowed.SessionsTotal)
	}
	if code, _ := human.do("GET", "/api/v1/agents/overview?project=NOPE", nil); code != 404 {
		t.Errorf("unknown project: %d", code)
	}
	if code, _ := stranger.do("GET", "/api/v1/agents/overview?project=WID", nil); code != 403 && code != 404 {
		t.Errorf("stranger on the project: %d", code)
	}
	var theirs core.Overview
	if code := stranger.json("GET", "/api/v1/agents/overview", nil, &theirs); code != 200 || theirs.SessionsTotal != 0 || len(theirs.Changes.Changes) != 0 {
		t.Errorf("stranger: %d %+v", code, theirs.SessionsTotal)
	}
	if code, _ := (&client{t: t, base: ts.URL, http: http.DefaultClient}).do("GET", "/api/v1/agents/overview", nil); code != 401 {
		t.Errorf("anonymous: %d", code)
	}

	// Changes: kinds, agent filter, paging by time.
	var ch core.ChangeList
	if code := human.json("GET", "/api/v1/changes?kind=issue&limit=1", nil, &ch); code != 200 || len(ch.Changes) != 1 || ch.Changes[0].Kind != "issue" || ch.Changes[0].URL == "" {
		t.Fatalf("changes: %d %+v", code, ch)
	}
	if ch.NextBefore == 0 {
		t.Errorf("a second page was expected: %+v", ch)
	}
	var byAgent core.ChangeList
	if code := human.json("GET", "/api/v1/changes?agent=claude", nil, &byAgent); code != 200 || len(byAgent.Changes) == 0 || byAgent.Changes[0].Agent != "anthropic/claude-code" {
		t.Errorf("agent changes: %d %+v", code, byAgent.Changes)
	}
	if code, _ := human.do("GET", "/api/v1/changes?kind=bogus", nil); code != 400 {
		t.Errorf("bad kind: %d", code)
	}

	// A session's usage: the owner and their agent read it; others are told it is private.
	var u core.SessionUsage
	if code := human.json("GET", "/api/v1/sessions/"+id64(hook.Session)+"/usage", nil, &u); code != 200 || u.HasData {
		t.Errorf("usage: %d %+v", code, u)
	}
	if code, b := agent.do("GET", "/api/v1/sessions/"+id64(hook.Session)+"/usage", nil); code != 200 {
		t.Errorf("agent usage: %d %s", code, b)
	}
	if code, _ := stranger.do("GET", "/api/v1/sessions/"+id64(hook.Session)+"/usage", nil); code != 404 {
		t.Errorf("stranger usage: %d", code)
	}
	if code, b := human.do("GET", "/api/v1/sessions/"+id64(hook.Session), nil); code != 200 || !strings.Contains(string(b), `"/agents/sessions/`) {
		t.Errorf("session: %d %s", code, b)
	}
}

func id64(n int64) string { return strconv.FormatInt(n, 10) }

// headerTransport adds headers to every request, as wp does for a session.
type headerTransport map[string]string

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}
