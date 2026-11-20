package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestBabysitAPI(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	user := func(name string, admin bool) *core.Actor {
		u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: name, Email: name + "@example.com", Password: "correct-horse-battery", IsAdmin: admin})
		if err != nil {
			t.Fatal(err)
		}
		return &core.Actor{User: u}
	}
	a, vic, sam := user("robin", true), user("vic", false), user("sam", false)
	cl := func(who *core.Actor, spec core.TokenSpec) *client {
		spec.Name = who.User.Username
		tok, _, err := svc.CreateToken(ctx, who, spec)
		if err != nil {
			t.Fatal(err)
		}
		return &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	}
	robin, readOnly := cl(a, core.TokenSpec{}), cl(a, core.TokenSpec{ReadOnly: true})
	readPulls := cl(a, core.TokenSpec{Agent: "anthropic/claude-sonnet-4-6", Permissions: []core.Permission{core.PermPullsRead}})
	viewer, stranger := cl(vic, core.TokenSpec{}), cl(sam, core.TokenSpec{})

	kind, base := "forgejo", "https://git.example.com"
	f, err := svc.CreateForge(ctx, a, core.ForgeInput{Kind: &kind, BaseURL: &base})
	if err != nil {
		t.Fatal(err)
	}
	// sam is a Waypoint person on the forge: their pull request isn't from outside (WP-281).
	if _, err := svc.SetForgePeople(ctx, a, f.ID, []core.ForgePersonInput{{Login: "sam", User: "sam", PersonOnly: true}}); err != nil {
		t.Fatal(err)
	}
	name := "App"
	p, err := svc.CreateProject(ctx, a, core.ProjectInput{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddProjectRepo(ctx, a, p.Key, f.ID, "me/app", false); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetMember(ctx, a, p.Key, "vic", "viewer"); err != nil {
		t.Fatal(err)
	}
	secret, _ := svc.ForgeSecret(ctx, a, f.ID)
	body, _ := json.Marshal(map[string]any{"action": "opened", "repository": map[string]any{"full_name": "me/app"},
		"pull_request": map[string]any{"number": 7, "title": "Fix it", "state": "open", "html_url": base + "/me/app/pulls/7", "user": map[string]string{"login": "sam"},
			"head": map[string]string{"ref": "fix", "sha": "abc1234"}, "base": map[string]string{"ref": "main"}}})
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/v1/hooks/forge/%d", ts.URL, f.ID), bytes.NewReader(body))
	req.Header.Set("X-Forgejo-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("webhook: %v %v", resp, err)
	}
	resp.Body.Close()
	svc.Wait()
	var pr struct {
		ID int64 `json:"id"`
	}
	if code := robin.json("GET", "/api/v1/projects/"+p.Key+"/pulls/7", nil, &pr); code != 200 || pr.ID == 0 {
		t.Fatalf("pull request: %d", code)
	}
	path := fmt.Sprintf("/api/v1/pulls/%d/babysit", pr.ID)

	var view struct {
		Babysit  *struct{ State, Model string } `json:"babysit"`
		Comments []any                          `json:"comments"`
		CanEdit  bool                           `json:"can_edit"`
	}
	if code := viewer.json("GET", path, nil, &view); code != 200 || view.Babysit != nil || view.CanEdit || view.Comments == nil {
		t.Errorf("viewer looks before: %d %+v", code, view)
	}
	for _, c := range []struct {
		who  *client
		code int
	}{{viewer, 403}, {stranger, 404}, {readOnly, 403}, {readPulls, 403}} {
		if code, b := c.who.do("POST", path, map[string]any{}); code != c.code {
			t.Errorf("start by %s: %d %s", c.who.token[:4], code, b)
		}
	}
	if code, b := stranger.do("GET", path, nil); code != 404 {
		t.Errorf("a stranger looks: %d %s", code, b)
	}
	var b struct {
		State string `json:"state"`
		Model string `json:"model"`
		Via   string `json:"via"`
	}
	if code := robin.json("POST", path, map[string]any{"model": "opus"}, &b); code != 200 || b.Model != "opus" || b.Via != "cli" {
		t.Fatalf("start: %d %+v", code, b)
	}
	svc.Wait()
	if code := readPulls.json("GET", path, nil, &view); code != 200 || view.Babysit == nil || view.Babysit.Model != "opus" {
		t.Errorf("an agent with pulls.read looks: %d %+v", code, view)
	}
	var todo struct {
		Text   string `json:"text"`
		Number int    `json:"number"`
	}
	if code := viewer.json("GET", fmt.Sprintf("/api/v1/pulls/%d/todo", pr.ID), nil, &todo); code != 200 || todo.Number != 7 || todo.Text == "" {
		t.Errorf("todo: %d %+v", code, todo)
	}
	for _, c := range []struct {
		path string
		body any
		code int
	}{
		{"/api/v1/review-comments/ack", map[string]any{"ids": []string{"c99"}, "verdict": "accepted"}, 404},
		{"/api/v1/review-comments/ack", map[string]any{"ids": []string{"nope"}, "verdict": "accepted"}, 400},
		{"/api/v1/review-comments/ack", map[string]any{"ids": []string{"c1"}, "verdict": "maybe"}, 400},
		{"/api/v1/review-comments/fixed", map[string]any{"ids": []string{}}, 400},
		{"/api/v1/review-comments/c99/dismiss", map[string]any{"note": "x"}, 404},
	} {
		if code, out := robin.do("POST", c.path, c.body); code != c.code {
			t.Errorf("%s %v: %d %s", c.path, c.body, code, out)
		}
	}
	if code, out := readPulls.do("POST", "/api/v1/review-comments/ack", map[string]any{"ids": []string{"c1"}, "verdict": "accepted"}); code != 403 {
		t.Errorf("ack without pulls.write: %d %s", code, out)
	}
	if code, out := viewer.do("DELETE", path, nil); code != 403 {
		t.Errorf("a viewer stops it: %d %s", code, out)
	}
	if code := robin.json("DELETE", path, nil, &b); code != 200 || b.State != "stopped" {
		t.Errorf("stop: %d %+v", code, b)
	}
}
