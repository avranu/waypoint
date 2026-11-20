package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestKnowledgeAPI(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "robin@example.com", Password: "correct-horse-battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	me := &core.Actor{User: u}
	tok, _, _ := svc.CreateAPIToken(ctx, me, "robin", false, 0)
	human := &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	agentTok, _, _ := svc.CreateToken(ctx, me, core.TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	agent := &client{t: t, base: ts.URL, http: http.DefaultClient, token: agentTok}

	if code, b := human.do("POST", "/api/v1/projects", map[string]any{"name": "Widgets", "key": "WID"}); code != 200 {
		t.Fatalf("project: %d %s", code, b)
	}
	var issue map[string]any
	human.json("POST", "/api/v1/issues", map[string]any{"project": "WID", "title": "Build the board"}, &issue)

	// Glossary: add, lock, look up as text; agents can't change locked terms.
	var g map[string]any
	if code := human.json("POST", "/api/v1/glossary", map[string]any{"term": "Board", "definition": "Columns of statuses.", "aliases": []string{"kanban"},
		"project": "WID", "locked": true, "links": []string{"WID-1", "web/src/pages/Board.tsx"}}, &g); code != 200 || g["locked"] != true {
		t.Fatalf("create term: %d %v", code, g)
	}
	id := int(g["id"].(float64))
	code, b := agent.do("GET", "/api/v1/glossary/lookup?term=kanban&project=WID&format=text", nil)
	if code != 200 || !strings.Contains(string(b), "Board  (WID glossary") || !strings.Contains(string(b), "locked by") || !strings.Contains(string(b), "file web/src/pages/Board.tsx") {
		t.Errorf("lookup: %d %s", code, b)
	}
	// An alias routes to the term to use, without the entry listing its aliases.
	if !strings.HasPrefix(string(b), `"kanban" is another name for "Board"`) || strings.Contains(string(b), "aliases:") {
		t.Errorf("lookup by alias: %s", b)
	}
	if code, b := agent.do("PATCH", "/api/v1/glossary/"+itoa(id), map[string]any{"definition": "Something else"}); code != 403 || !strings.Contains(string(b), "locked") {
		t.Errorf("agent edit of locked term: %d %s", code, b)
	}
	if code, _ := agent.do("PATCH", "/api/v1/glossary/"+itoa(id), map[string]any{"locked": false}); code != 403 {
		t.Errorf("agent unlock: %d", code)
	}
	// MCP calls count as an agent's even with the person's own token.
	if text := callMCPTool(t, ts.URL, tok, "save_glossary", map[string]any{"id": id, "definition": "via mcp"}); !strings.Contains(text, "locked") {
		t.Errorf("mcp edit of locked term: %s", text)
	}

	// Design: a person's note is approved; an agent's edit becomes a proposal.
	var note map[string]any
	if code := human.json("POST", "/api/v1/design", map[string]any{"title": "Board view", "kind": "feature", "project": "WID",
		"body": "The [[Board]] shows work by status.", "links": []string{"WID-1"}}, &note); code != 200 || note["approved"] != true {
		t.Fatalf("create note: %d %v", code, note)
	}
	nid := itoa(int(note["id"].(float64)))
	var edited map[string]any
	if code := agent.json("PATCH", "/api/v1/design/"+nid, map[string]any{"body": "Rewritten by an agent."}, &edited); code != 200 || edited["body"] != "The [[Board]] shows work by status." || edited["notice"] == "" {
		t.Errorf("agent edit: %d %v", code, edited)
	}
	if code, _ := agent.do("POST", "/api/v1/design/"+nid+"/additions", map[string]any{"body": "Add swimlanes."}); code != 200 {
		t.Errorf("addition: %d", code)
	}
	if code, _ := agent.do("DELETE", "/api/v1/design/"+nid, nil); code != 403 {
		t.Errorf("agent delete note: %d", code)
	}
	var queue []map[string]any
	if code := human.json("GET", "/api/v1/design/review?project=WID", nil, &queue); code != 200 || len(queue) != 2 {
		t.Fatalf("review queue: %d %v", code, queue)
	}
	rid := itoa(int(queue[1]["revision"].(map[string]any)["id"].(float64)))
	if code, _ := agent.do("POST", "/api/v1/design/revisions/"+rid+"/approve", nil); code != 403 {
		t.Errorf("agent approve: %d", code)
	}
	if code, b := human.do("POST", "/api/v1/design/revisions/"+rid+"/approve", map[string]any{"note": "ok"}); code != 200 {
		t.Errorf("approve: %d %s", code, b)
	}
	// The spec, as an agent reads it.
	code, b = agent.do("GET", "/api/v1/spec?topic=board+view&project=WID&format=text", nil)
	s := string(b)
	if code != 200 || !strings.Contains(s, "SPEC: Board view") || !strings.Contains(s, "Approved by robin") || !strings.Contains(s, "Add swimlanes.") ||
		!strings.Contains(s, "UNAPPROVED agent edits") || !strings.Contains(s, "Board: Columns of statuses.") || !strings.Contains(s, "WID-1 Build the board") {
		t.Errorf("spec text: %d %s", code, s)
	}
	if code, b := agent.do("GET", "/api/v1/spec?project=WID&format=text", nil); code != 200 || !strings.Contains(string(b), "features: #") {
		t.Errorf("overview: %d %s", code, b)
	}
	// The issue's links show the note; next/take mention it.
	var links []map[string]any
	if code := human.json("GET", "/api/v1/links?type=issue&key=WID-1", nil, &links); code != 200 || len(links) != 2 {
		t.Errorf("issue links: %d %v", code, links)
	}
	code, b = agent.do("POST", "/api/v1/agent/take/WID-1", nil)
	if code != 200 || !strings.Contains(string(b), "Design: #") || !strings.Contains(string(b), "Assess and complete") {
		t.Errorf("take: %d %s", code, b)
	}
	// Search everything, as text and JSON.
	code, b = agent.do("GET", "/api/v1/search/all?q=board&project=WID&format=text", nil)
	if code != 200 || !strings.Contains(string(b), "Issues (1)") || !strings.Contains(string(b), "Design notes (1)") || !strings.Contains(string(b), "Glossary (1)") {
		t.Errorf("search text: %d %s", code, b)
	}
	var res core.AllResult
	if code := agent.json("GET", "/api/v1/search/all?q=board&kinds=glossary", nil, &res); code != 200 || len(res.Groups) != 1 || res.Groups[0].Kind != "glossary" {
		t.Errorf("search json: %d %+v", code, res)
	}
	// Wikilinks resolve.
	var it map[string]any
	if code := human.json("GET", "/api/v1/resolve?ref=Board&project=WID", nil, &it); code != 200 || it["type"] != "glossary" {
		t.Errorf("resolve: %d %v", code, it)
	}
	// The agent index stays short.
	if code, b := agent.do("GET", "/api/v1/agent", nil); code != 200 || len(b) > 3200 {
		t.Errorf("index: %d (%d bytes)", code, len(b))
	}
	for _, topic := range []string{"design", "glossary", "search"} {
		if code, _ := agent.do("GET", "/api/v1/agent/help/"+topic, nil); code != 200 {
			t.Errorf("help %s: %d", topic, code)
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// callMCPTool calls one MCP tool and returns its text.
func callMCPTool(t *testing.T, base, token, name string, args map[string]any) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	req, _ := http.NewRequest("POST", base+"/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Result.Content) == 0 {
		t.Fatalf("mcp %s: %v", name, err)
	}
	return out.Result.Content[0].Text
}
