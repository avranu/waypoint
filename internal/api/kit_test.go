package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// kitForge is a Forgejo with a kit repo, Robin/agent-kit, at one commit.
func kitForge(t *testing.T) *httptest.Server {
	files := map[string]string{
		"kit.toml": "[plugins.waypoint]\nrepos = [\"*\"]\n[plugins.demo]\nrepos = [\"acme/*\"]\nwhen = [\"exists:.indexed\"]\n" +
			"[[notes]]\nrepos = [\"WP\"]\ntext = \"Wait with wp pr wait.\"\n",
		".claude-plugin/marketplace.json":       `{"name":"agent-kit","plugins":[{"name":"waypoint","source":"./plugins/waypoint"},{"name":"demo","source":"./plugins/demo"}]}`,
		"plugins/waypoint/skills/ship/SKILL.md": "---\ndescription: Ship it\n---\n",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/Robin/agent-kit")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case p == "":
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case p == "/branches/main":
			_, _ = w.Write([]byte(`{"commit":{"id":"abc1234def"}}`))
		case strings.HasPrefix(p, "/git/trees/"):
			var tree []string
			for f := range files {
				tree = append(tree, fmt.Sprintf(`{"path":%q,"type":"blob"}`, f))
			}
			_, _ = fmt.Fprintf(w, `{"tree":[%s]}`, strings.Join(tree, ","))
		case strings.HasPrefix(p, "/raw/") && files[strings.TrimPrefix(p, "/raw/")] != "":
			_, _ = w.Write([]byte(files[strings.TrimPrefix(p, "/raw/")]))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestKitAPI(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	admin, _ := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "robin@example.com", Password: "correct-horse-battery", IsAdmin: true})
	sam, _ := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "sam", Email: "sam@example.com", Password: "correct-horse-battery"})
	tok, _, _ := svc.CreateAPIToken(ctx, &core.Actor{User: admin}, "robin", false, 0)
	samTok, _, _ := svc.CreateAPIToken(ctx, &core.Actor{User: sam}, "sam", false, 0)
	robin := &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	other := &client{t: t, base: ts.URL, http: http.DefaultClient, token: samTok}

	var kit map[string]any
	if code := other.json("GET", "/api/v1/kit", nil, &kit); code != 200 || kit["configured"] != false {
		t.Fatalf("no kit: %d %v", code, kit)
	}
	if code, _ := other.do("GET", "/api/v1/kit/resolve", nil); code != 400 {
		t.Fatalf("resolve without a repo: %d", code)
	}

	srv := kitForge(t)
	forge, err := svc.CreateForge(ctx, &core.Actor{User: admin}, core.ForgeInput{Kind: ptrS("forgejo"), BaseURL: ptrS(srv.URL), Token: ptrS("tok")})
	if err != nil {
		t.Fatal(err)
	}
	name := "Waypoint"
	p, err := svc.CreateProject(ctx, &core.Actor{User: admin}, core.ProjectInput{Name: &name, Key: ptrS("WP")})
	if err != nil {
		t.Fatal(err)
	}
	pr, err := svc.AddProjectRepo(ctx, &core.Actor{User: admin}, p.Key, forge.ID, "Robin/agent-kit", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddProjectRepo(ctx, &core.Actor{User: admin}, p.Key, forge.ID, "acme/app", false); err != nil {
		t.Fatal(err)
	}
	if code, _ := other.do("PUT", "/api/v1/admin/kit", map[string]any{"repo_id": pr.ID}); code != 403 {
		t.Fatalf("a member picked the kit: %d", code)
	}
	var src struct {
		RepoID     int64            `json:"repo_id"`
		Candidates []map[string]any `json:"candidates"`
	}
	if code := robin.json("PUT", "/api/v1/admin/kit", map[string]any{"repo_id": pr.ID}, &src); code != 200 || src.RepoID != pr.ID || len(src.Candidates) != 2 {
		t.Fatalf("set: %d %+v", code, src)
	}
	svc.Wait()
	if code := other.json("GET", "/api/v1/kit", nil, &kit); code != 200 || kit["commit"] != "abc1234def" || kit["can_manage"] != false {
		t.Fatalf("kit: %d %v", code, kit)
	}

	var res struct {
		Commit, Marketplace string
		GitURL              string `json:"git_url"`
		Attached            bool
		Plugins             []struct {
			Name string
			When []string
		}
		Notes []struct{ Text string }
		All   []string
	}
	if code := other.json("GET", "/api/v1/kit/resolve?repo=acme/app", nil, &res); code != 200 || res.Commit != "abc1234def" || res.Marketplace != "agent-kit" ||
		!strings.HasSuffix(res.GitURL, "/Robin/agent-kit.git") || len(res.Plugins) != 2 || res.Plugins[1].When[0] != "exists:.indexed" || len(res.Notes) != 1 || len(res.All) != 2 {
		t.Fatalf("resolve: %d %+v", code, res)
	}

	if code, b := other.do("POST", "/api/v1/kit/reports", map[string]any{"machine": "laptop", "path": "/src/app", "repo": "acme/app",
		"kit_commit": "abc1234def", "plugins": []string{"waypoint@agent-kit"}, "drift": []map[string]string{{"plugin": "demo", "what": "disabled by hand"}},
		"wp_version": "0.12.0", "future_field": true}); code != 204 {
		t.Fatalf("report: %d %s", code, b)
	}
	var cos []map[string]any
	if code := other.json("GET", "/api/v1/kit/checkouts", nil, &cos); code != 200 || len(cos) != 1 || cos[0]["behind"] != false || len(cos[0]["drift"].([]any)) != 1 {
		t.Fatalf("checkouts: %d %v", code, cos)
	}
	if code := robin.json("GET", "/api/v1/kit/checkouts", nil, &cos); code != 200 || len(cos) != 1 {
		t.Fatalf("admin's checkouts: %d %v", code, cos)
	}
	if code, _ := other.do("POST", "/api/v1/kit/rollout", nil); code != 403 {
		t.Fatalf("a member rolled out: %d", code)
	}
	if code, b := robin.do("POST", "/api/v1/kit/rollout", nil); code != 400 || !strings.Contains(string(b), "no runners") {
		t.Fatalf("rollout without runners: %d %s", code, b)
	}
}

func ptrS(s string) *string { return &s }
