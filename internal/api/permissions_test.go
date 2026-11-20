package api

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// authedHandlers serve signed-in requests without the a(…) wrapper; they
// call s.authenticate themselves, so they need a permission too.
var authedHandlers = []string{"attachment", "avatar", "stream", "mcp", "assistantLive"}

// registeredRoutes reads the route patterns out of the package's source,
// and says which ones are signed in.
func registeredRoutes(t *testing.T) map[string]bool {
	t.Helper()
	routes, _ := parseRoutes(t)
	return routes
}

// parseRoutes also says which routes are personUI ones.
func parseRoutes(t *testing.T) (map[string]bool, map[string]bool) {
	t.Helper()
	personUI := map[string]bool{}
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	out := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "HandleFunc" {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok {
				t.Errorf("%s: route pattern is not a literal", fset.Position(call.Pos()))
				return true
			}
			pattern, _ := strconv.Unquote(lit.Value)
			authed := false
			switch h := call.Args[1].(type) {
			case *ast.CallExpr: // a(s.f), s.authed(s.f)
				switch fn := h.Fun.(type) {
				case *ast.Ident:
					authed = fn.Name == "a"
				case *ast.SelectorExpr:
					authed = fn.Sel.Name == "authed" || fn.Sel.Name == "personUI"
					personUI[pattern] = fn.Sel.Name == "personUI"
				}
			case *ast.SelectorExpr: // s.stream
				authed = slices.Contains(authedHandlers, h.Sel.Name)
			}
			out[pattern] = authed
			return true
		})
	}
	if len(out) < 300 {
		t.Fatalf("found only %d routes; has registration moved?", len(out))
	}
	return out, personUI
}

func TestEveryRouteHasAPermission(t *testing.T) {
	routes := registeredRoutes(t)
	for pattern, authed := range routes {
		_, mapped := routePermissions[pattern]
		if authed && !mapped {
			t.Errorf("%s has no entry in routePermissions: permission tokens are refused it until it has one", pattern)
		}
	}
	for pattern, p := range routePermissions {
		if _, ok := routes[pattern]; !ok {
			t.Errorf("routePermissions has %s, which is not a route", pattern)
		}
		if p != permAlways && p != permNever && p != permMCP && !core.KnownPermission(p) {
			t.Errorf("%s needs %q, which is not in the catalog", pattern, p)
		}
	}
	// Changing what tokens may do is never open to a permission token.
	for _, pattern := range []string{"POST /api/v1/me/tokens", "PATCH /api/v1/me/tokens/{id}", "DELETE /api/v1/me/tokens/{id}",
		"GET /api/v1/me/tokens", "POST /api/v1/cli/login/{code}/approve", "POST /api/v1/me/password"} {
		if routePermissions[pattern] != permNever {
			t.Errorf("%s should be permNever", pattern)
		}
	}
}

func TestEveryMCPToolHasAPermission(t *testing.T) {
	for _, tool := range mcpTools {
		p, ok := mcpToolPermissions[tool.Name]
		if !ok {
			t.Errorf("MCP tool %s has no entry in mcpToolPermissions", tool.Name)
		} else if p != permAlways && !core.KnownPermission(p) {
			t.Errorf("MCP tool %s needs %q, which is not in the catalog", tool.Name, p)
		}
	}
	for name := range mcpToolPermissions {
		if !slices.ContainsFunc(mcpTools, func(t mcpTool) bool { return t.Name == name }) {
			t.Errorf("mcpToolPermissions has %s, which is not a tool", name)
		}
	}
}

func TestPresetsAreSafe(t *testing.T) {
	cat := (&core.Service{}).Permissions()
	risk := map[core.Permission]core.Risk{}
	for _, g := range cat.Groups {
		for _, p := range g.Permissions {
			if _, dup := risk[p.Key]; dup {
				t.Errorf("%s is in the catalog twice", p.Key)
			}
			risk[p.Key] = p.Risk
		}
	}
	for _, pr := range cat.Presets {
		for _, p := range pr.Permissions {
			if r, ok := risk[p]; !ok {
				t.Errorf("preset %s has unknown %s", pr.Key, p)
			} else if r == core.RiskDangerous {
				t.Errorf("preset %s grants dangerous %s", pr.Key, p)
			}
		}
	}
}

func TestPermissionTokens(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	link, err := svc.BootstrapInvite(ctx)
	if err != nil || link == nil {
		t.Fatal("expected bootstrap invite", err)
	}
	jar, _ := cookiejar.New(nil)
	web := &client{t: t, base: ts.URL, http: &http.Client{Jar: jar}, web: true}
	inv := link.URL[strings.LastIndex(link.URL, "/")+1:]
	if code := web.json("POST", "/api/v1/auth/invite/"+inv, map[string]any{"username": "robin", "email": "robin@example.com", "password": "correct horse battery"}, nil); code != 200 {
		t.Fatalf("accept: %d", code)
	}
	web.json("POST", "/api/v1/projects", map[string]any{"name": "House", "key": "HOUSE"}, nil)

	var cat core.PermissionCatalog
	if code := web.json("GET", "/api/v1/me/token-permissions", nil, &cat); code != 200 || len(cat.Presets) == 0 {
		t.Fatalf("catalog: %d %v", code, cat)
	}
	var assistant []core.Permission
	for _, p := range cat.Presets {
		if p.Key == "assistant" {
			assistant = p.Permissions
		}
	}
	var created struct {
		Token string        `json:"token"`
		Info  core.APIToken `json:"info"`
	}
	if code := web.json("POST", "/api/v1/me/tokens", map[string]any{"name": "hermes", "agent": "nous/hermes", "permissions": assistant}, &created); code != 200 || len(created.Info.Permissions) != len(assistant) {
		t.Fatalf("create: %d %+v", code, created.Info)
	}
	if code, b := web.do("POST", "/api/v1/me/tokens", map[string]any{"name": "x", "permissions": []string{"issues.fly"}}); code != 400 {
		t.Errorf("unknown permission: %d %s", code, b)
	}
	hermes := &client{t: t, base: ts.URL, http: http.DefaultClient, token: created.Token}

	// What it was given works.
	var me map[string]any
	if code := hermes.json("GET", "/api/v1/me", nil, &me); code != 200 || me["token"] == nil {
		t.Fatalf("me: %d %v", code, me)
	}
	if code, b := hermes.do("POST", "/api/v1/issues", map[string]any{"project": "HOUSE", "title": "Fix sink"}); code != 200 {
		t.Fatalf("create issue: %d %s", code, b)
	}
	if code, b := hermes.do("POST", "/api/v1/issues/HOUSE-1/comments", map[string]any{"body": "on it"}); code != 200 {
		t.Fatalf("comment: %d %s", code, b)
	}
	if code, b := hermes.do("POST", "/api/v1/glossary", map[string]any{"term": "Sink", "definition": "Where dishes go.", "project": "HOUSE"}); code != 200 {
		t.Fatalf("glossary: %d %s", code, b)
	}
	if code, _ := hermes.do("GET", "/api/v1/search/all?q=sink", nil); code != 200 {
		t.Errorf("search: %d", code)
	}

	// What it wasn't is refused, and recorded for Robin.
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/v1/agents/start/HOUSE-1"},
		{"POST", "/api/v1/runner-jobs"},
		{"DELETE", "/api/v1/issues/HOUSE-1"},
		{"POST", "/api/v1/projects"},
		{"GET", "/api/v1/audit"},
	} {
		if code, b := hermes.do(c.method, c.path, map[string]any{}); code != 403 || !strings.Contains(string(b), "permission") {
			t.Errorf("%s %s: %d %s", c.method, c.path, code, b)
		}
	}
	// Tokens are out of its reach, whatever it holds.
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/v1/me/tokens", nil},
		{"POST", "/api/v1/me/tokens", map[string]any{"name": "more"}},
		{"PATCH", "/api/v1/me/tokens/" + strconv.FormatInt(created.Info.ID, 10), map[string]any{"permissions": nil}},
		{"POST", "/api/v1/me/password", map[string]any{"current": "x", "new": "y"}},
	} {
		if code, b := hermes.do(c.method, c.path, c.body); code != 403 {
			t.Errorf("%s %s: %d %s", c.method, c.path, code, b)
		}
	}

	// MCP: it sees and calls only its tools.
	rpc := func(method string, params any) map[string]any {
		t.Helper()
		var out map[string]any
		if code := hermes.json("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}, &out); code != 200 || out["error"] != nil {
			t.Fatalf("%s: %d %v", method, code, out)
		}
		return out["result"].(map[string]any)
	}
	var names []string
	for _, tl := range rpc("tools/list", nil)["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if !slices.Contains(names, "save_page") || slices.Contains(names, "run_automation") || slices.Contains(names, "next_task") {
		t.Errorf("tools: %v", names)
	}
	if res := rpc("tools/call", map[string]any{"name": "run_automation", "arguments": map[string]any{"id": 1}}); res["isError"] != true {
		t.Errorf("run_automation: %v", res)
	}
	if res := rpc("tools/call", map[string]any{"name": "save_page", "arguments": map[string]any{"project": "HOUSE", "title": "Plan", "body": "Fix the sink."}}); res["isError"] == true {
		t.Errorf("save_page: %v", res)
	}

	// Robin sees what it was refused, and grants one.
	var list []core.APIToken
	web.json("GET", "/api/v1/me/tokens", nil, &list)
	var refused []core.Permission
	for _, r := range list[0].Refusals {
		refused = append(refused, r.Permission)
	}
	for _, p := range []core.Permission{core.PermSessionsStart, core.PermRunnersRun, core.PermIssuesDelete, core.PermAutomationsManage} {
		if !slices.Contains(refused, p) {
			t.Errorf("refusals %v lack %s", refused, p)
		}
	}
	id := strconv.FormatInt(created.Info.ID, 10)
	var updated core.APIToken
	if code := web.json("PATCH", "/api/v1/me/tokens/"+id, map[string]any{"permissions": append(slices.Clone(assistant), core.PermIssuesDelete)}, &updated); code != 200 || !slices.Contains(updated.Permissions, core.PermIssuesDelete) {
		t.Fatalf("grant: %d %+v", code, updated)
	}
	if code, b := hermes.do("DELETE", "/api/v1/issues/HOUSE-1", nil); code != 204 && code != 200 {
		t.Errorf("delete after grant: %d %s", code, b)
	}
	if code := web.json("DELETE", "/api/v1/me/tokens/"+id+"/refusals", nil, nil); code != 204 {
		t.Errorf("clear refusals: %d", code)
	}

	// An agent with a full token can't change what tokens may do.
	full, _, _ := svc.CreateToken(ctx, mustActor(t, svc, int64(me["id"].(float64))), core.TokenSpec{Name: "claude", Agent: "anthropic/claude"})
	claude := &client{t: t, base: ts.URL, http: http.DefaultClient, token: full}
	if code, b := claude.do("PATCH", "/api/v1/me/tokens/"+id, map[string]any{"permissions": nil}); code != 403 {
		t.Errorf("agent widened a token: %d %s", code, b)
	}
	// Robin can lift the limits altogether.
	if code := web.json("PATCH", "/api/v1/me/tokens/"+id, map[string]any{"permissions": nil}, &updated); code != 200 || updated.Permissions != nil {
		t.Errorf("unrestrict: %d %+v", code, updated)
	}
	if code, _ := hermes.do("GET", "/api/v1/audit", nil); code != 200 {
		t.Errorf("unrestricted token refused: %d", code)
	}
}

func mustActor(t *testing.T, svc *core.Service, id int64) *core.Actor {
	t.Helper()
	u, err := svc.UserByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return &core.Actor{User: u}
}

// Gaps an adversarial review found (WP-111): a permission token without an
// agent identity still acts as an agent; it can't remove others' files,
// withdraw others' requests, smuggle a comment past MCP, or open the stream.
func TestPermissionTokenLimits(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	u, _ := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "j@example.com", Password: "correct horse battery", IsAdmin: true})
	robin := &core.Actor{User: u}
	name := "House"
	if _, err := svc.CreateProject(ctx, robin, core.ProjectInput{Name: &name, Key: &name}); err != nil {
		t.Fatal(err)
	}
	full, _, _ := svc.CreateAPIToken(ctx, robin, "robin", false, 0)
	me := &client{t: t, base: ts.URL, http: http.DefaultClient, token: full}
	assistant := svc.Permissions().Presets[0].Permissions
	tok, _, err := svc.CreateToken(ctx, robin, core.TokenSpec{Name: "hermes", Permissions: assistant})
	if err != nil {
		t.Fatal(err)
	}
	hermes := &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}

	// No agent identity given: it still proposes design notes rather than approving them.
	var note map[string]any
	if code := hermes.json("POST", "/api/v1/design", map[string]any{"title": "Sinks", "body": "Steel.", "project": "HOUSE"}, &note); code != 200 || note["approved"] == true {
		t.Errorf("design note from a permission token: %d %v", code, note)
	}
	var issue map[string]any
	me.json("POST", "/api/v1/issues", map[string]any{"project": "HOUSE", "title": "Sink", "draft": true}, &issue)
	if code, _ := hermes.do("GET", "/api/v1/issues/HOUSE-1", nil); code != 404 {
		t.Errorf("a permission token saw a draft: %d", code)
	}
	me.do("POST", "/api/v1/issues/HOUSE-1/publish", nil)

	// Robin's file is out of its reach; its own is not.
	var att map[string]any
	if code := me.json("POST", "/api/v1/issues/HOUSE-1/attachments?name=receipt.txt", "paid", &att); code != 200 {
		t.Fatalf("upload: %d %v", code, att)
	}
	robinFile := strconv.FormatFloat(att["id"].(float64), 'f', 0, 64)
	if code, _ := hermes.do("DELETE", "/api/v1/attachments/"+robinFile, nil); code != 403 {
		t.Errorf("deleted Robin's file: %d", code)
	}
	hermes.json("POST", "/api/v1/issues/HOUSE-1/attachments?name=notes.txt", "mine", &att)
	if code, b := hermes.do("DELETE", "/api/v1/attachments/"+strconv.FormatFloat(att["id"].(float64), 'f', 0, 64), nil); code != 204 {
		t.Errorf("own file: %d %s", code, b)
	}

	// A request someone else filed can't be withdrawn by it.
	claudeTok, _, _ := svc.CreateToken(ctx, robin, core.TokenSpec{Name: "claude", Agent: "anthropic/claude"})
	agent := &client{t: t, base: ts.URL, http: http.DefaultClient, token: claudeTok}
	var req map[string]any
	if code := agent.json("POST", "/api/v1/requests", map[string]any{"kind": "question", "title": "Which sink?", "project": "HOUSE", "questions": []map[string]any{{"question": "Which sink?"}}}, &req); code != 200 {
		t.Fatalf("request: %d %v", code, req)
	}
	if code, _ := hermes.do("POST", "/api/v1/requests/"+req["ref"].(string)+"/withdraw", nil); code != 403 {
		t.Errorf("withdrew someone else's request: %d", code)
	}

	// No live stream.
	if code, _ := hermes.do("GET", "/api/v1/stream", nil); code != 403 {
		t.Errorf("stream: %d", code)
	}

	// MCP reads arguments once: a duplicate key can't hide a comment.
	edit, _, _ := svc.CreateToken(ctx, robin, core.TokenSpec{Name: "edit only", Permissions: []core.Permission{core.PermIssuesRead, core.PermIssuesEdit}})
	editor := &client{t: t, base: ts.URL, http: http.DefaultClient, token: edit}
	var out map[string]any
	editor.json("POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"update_issue","arguments":{"key":"HOUSE-1","comment":5,"comment":"SMUGGLED"}}}`, &out)
	if res, _ := out["result"].(map[string]any); res == nil || res["isError"] != true {
		t.Errorf("smuggled comment: %v", out)
	}
	if code, _ := editor.do("PATCH", "/api/v1/issues/HOUSE-1", map[string]any{"comment": "via PATCH"}); code != 403 {
		t.Errorf("comment via PATCH without issues.comment: %d", code)
	}
}
