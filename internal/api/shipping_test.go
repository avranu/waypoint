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
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestShippingAPI(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	admin, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "robin@example.com", Password: "correct-horse-battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	sam, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "sam", Email: "sam@example.com", Password: "correct-horse-battery"})
	if err != nil {
		t.Fatal(err)
	}
	a := &core.Actor{User: admin}
	tok, _, _ := svc.CreateAPIToken(ctx, a, "robin", false, 0)
	samTok, _, _ := svc.CreateAPIToken(ctx, &core.Actor{User: sam}, "sam", false, 0)
	robin := &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	stranger := &client{t: t, base: ts.URL, http: http.DefaultClient, token: samTok}

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
	repo, err := svc.AddProjectRepo(ctx, a, p.Key, f.ID, "me/app", false)
	if err != nil {
		t.Fatal(err)
	}
	// A pull request arrives by webhook.
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

	// The review gate is set with the other per-repo switches.
	if code, b := robin.do("PATCH", fmt.Sprintf("/api/v1/projects/%s/repos/%d", p.Key, repo.ID), map[string]any{"review_gate": true, "gate_severity": "P1"}); code != 200 && code != 204 {
		t.Fatalf("set the gate: %d %s", code, b)
	}
	var repos []map[string]any
	if code := robin.json("GET", fmt.Sprintf("/api/v1/projects/%s/repos", p.Key), nil, &repos); code != 200 || repos[0]["review_gate"] != true || repos[0]["gate_severity"] != "high" {
		t.Fatalf("repos: %d %v", code, repos)
	}
	if code, b := robin.do("PATCH", fmt.Sprintf("/api/v1/projects/%s/repos/%d", p.Key, repo.ID), map[string]any{"gate_severity": "urgent"}); code != 400 {
		t.Errorf("bad severity: %d %s", code, b)
	}
	// A refused gate leaves the other switches in the same request unapplied.
	if code, b := robin.do("PATCH", fmt.Sprintf("/api/v1/projects/%s/repos/%d", p.Key, repo.ID), map[string]any{"sync_issues": true, "bots": false, "gate_severity": "urgent"}); code != 400 {
		t.Errorf("bad severity with other switches: %d %s", code, b)
	}
	if code := robin.json("GET", fmt.Sprintf("/api/v1/projects/%s/repos", p.Key), nil, &repos); code != 200 || repos[0]["sync_issues"] == true || repos[0]["bots"] != true {
		t.Errorf("a refused request changed the repo: %d %v", code, repos[0])
	}
	// So is the merge policy (WP-133): on by default, which armed #7; off takes that off.
	if repos[0]["auto_merge"] != true || fmt.Sprint(repos[0]["external_reviewers"]) != "[]" {
		t.Errorf("default policy: %v", repos[0])
	}
	if code, b := robin.do("PATCH", fmt.Sprintf("/api/v1/projects/%s/repos/%d", p.Key, repo.ID), map[string]any{"auto_merge": false, "required_checks": []string{"local-ci"}, "quiet_minutes": 5}); code != 200 && code != 204 {
		t.Fatalf("set the policy: %d %s", code, b)
	}
	if code, b := robin.do("PATCH", fmt.Sprintf("/api/v1/projects/%s/repos/%d", p.Key, repo.ID), map[string]any{"review_wait_minutes": -1}); code != 400 {
		t.Errorf("bad review wait: %d %s", code, b)
	}
	svc.Wait()
	if code := robin.json("GET", fmt.Sprintf("/api/v1/projects/%s/repos", p.Key), nil, &repos); code != 200 || repos[0]["auto_merge"] != false ||
		fmt.Sprint(repos[0]["required_checks"]) != "[local-ci]" || repos[0]["quiet_minutes"] != float64(5) || repos[0]["review_wait_minutes"] != float64(30) {
		t.Errorf("policy: %d %v", code, repos[0])
	}
	// Whether the merge line builds candidates (WP-1659): on by default.
	if repos[0]["line_candidates"] != true {
		t.Errorf("line_candidates default: %v", repos[0])
	}
	for _, want := range []bool{false, true} {
		if code, b := robin.do("PATCH", fmt.Sprintf("/api/v1/projects/%s/repos/%d", p.Key, repo.ID), map[string]any{"line_candidates": want}); code != 200 && code != 204 {
			t.Fatalf("set line_candidates %v: %d %s", want, code, b)
		}
		if code := robin.json("GET", fmt.Sprintf("/api/v1/projects/%s/repos", p.Key), nil, &repos); code != 200 || repos[0]["line_candidates"] != want {
			t.Errorf("line_candidates %v read back: %d %v", want, code, repos[0]["line_candidates"])
		}
	}

	// A pull request by number, with how it stands for merging.
	var d struct {
		ID       int64 `json:"id"`
		Shipping struct {
			Gate struct {
				Enabled  bool   `json:"enabled"`
				Severity string `json:"severity"`
			} `json:"gate"`
			Readiness struct {
				Ready   bool     `json:"ready"`
				Reasons []string `json:"reasons"`
			} `json:"readiness"`
			CanMerge bool `json:"can_merge"`
			Armed    any  `json:"armed"`
		} `json:"shipping"`
	}
	path := fmt.Sprintf("/api/v1/projects/%s/pulls/7", p.Key)
	if code := robin.json("GET", path, nil, &d); code != 200 || d.ID == 0 || !d.Shipping.Gate.Enabled || d.Shipping.Gate.Severity != "high" ||
		d.Shipping.Readiness.Ready || len(d.Shipping.Readiness.Reasons) == 0 || !d.Shipping.CanMerge || d.Shipping.Armed != nil {
		t.Fatalf("pull: %d %+v", code, d)
	}
	if code, _ := robin.do("GET", path+"?repo=other/repo", nil); code != 404 {
		t.Errorf("another repo: %d", code)
	}
	if code, _ := robin.do("GET", fmt.Sprintf("/api/v1/projects/%s/pulls/99", p.Key), nil); code != 404 {
		t.Errorf("unknown number: %d", code)
	}
	if code, _ := stranger.do("GET", path, nil); code != 404 {
		t.Errorf("a stranger read it: %d", code)
	}

	// Arm it; the page says so; disarm.
	var res map[string]any
	pull := fmt.Sprintf("/api/v1/pulls/%d", d.ID)
	if code := robin.json("POST", pull+"/merge", map[string]any{"when_ready": true, "style": "squash"}, &res); code != 200 || res["armed"] != true || res["merged"] == true {
		t.Fatalf("arm: %d %v", code, res)
	}
	if code := robin.json("GET", path, nil, &d); code != 200 || d.Shipping.Armed == nil {
		t.Fatalf("armed: %d %+v", code, d)
	}
	if code, b := robin.do("DELETE", pull+"/merge", nil); code != 200 && code != 204 {
		t.Errorf("disarm: %d %s", code, b)
	}
	if code, b := robin.do("POST", pull+"/merge", map[string]any{"style": "fast-forward"}); code != 400 {
		t.Errorf("bad style: %d %s", code, b)
	}
	if code, b := robin.do("POST", pull+"/merge", nil); code != 400 || !strings.Contains(string(b), "no API token") {
		t.Errorf("merge now: %d %s", code, b)
	}
	if code, _ := stranger.do("POST", pull+"/merge", map[string]any{"when_ready": true}); code != 404 {
		t.Errorf("a stranger armed: %d", code)
	}
	var rd struct {
		Ready bool `json:"ready"`
	}
	if code := robin.json("GET", pull+"/readiness", nil, &rd); code != 200 || rd.Ready {
		t.Errorf("readiness: %d %+v", code, rd)
	}
	var fc []any
	if code := robin.json("GET", pull+"/failed-checks", nil, &fc); code != 200 || len(fc) != 0 {
		t.Errorf("failed checks: %d %v", code, fc)
	}
	// Logs and the wait (WP-132): no failed checks; checks unread, so a
	// wait with no time left says it is still waiting.
	var logs struct {
		Number int   `json:"number"`
		Checks []any `json:"checks"`
	}
	if code := robin.json("GET", pull+"/logs", nil, &logs); code != 200 || logs.Number != 7 || logs.Checks == nil || len(logs.Checks) != 0 {
		t.Errorf("logs: %d %+v", code, logs)
	}
	if code, b := robin.do("GET", pull+"/logs?check=e2e", nil); code != 404 || !strings.Contains(string(b), "no checks") {
		t.Errorf("logs of an unknown check: %d %s", code, b)
	}
	var w struct {
		Verdict string `json:"verdict"`
		Why     string `json:"why"`
	}
	if code := robin.json("GET", pull+"/wait?timeout=0", nil, &w); code != 200 || w.Verdict != "waiting" || w.Why != "checks haven't been read yet" {
		t.Errorf("wait: %d %+v", code, w)
	}
	for _, p := range []string{"/logs", "/wait"} {
		if code, _ := stranger.do("GET", pull+p, nil); code != 404 {
			t.Errorf("a stranger read %s: %d", p, code)
		}
	}
	// Findings: unknown ones are 404; agents don't decide.
	if code, _ := robin.do("POST", "/api/v1/findings/99/dismiss", map[string]string{"note": "x"}); code != 404 {
		t.Errorf("unknown finding: %d", code)
	}
	// Branches in work: the one pull request's, listed after a push.
	push, _ := json.Marshal(map[string]any{"ref": "refs/heads/fix", "before": strings.Repeat("0", 40), "after": "abc1234", "pusher": map[string]string{"username": "sam"},
		"repository": map[string]any{"full_name": "me/app"}, "commits": []map[string]any{{"id": "abc1234", "message": "x", "added": []string{"a.go"}}}})
	mac = hmac.New(sha256.New, []byte(secret))
	mac.Write(push)
	req, _ = http.NewRequest("POST", fmt.Sprintf("%s/api/v1/hooks/forge/%d", ts.URL, f.ID), bytes.NewReader(push))
	req.Header.Set("X-Forgejo-Event", "push")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	if resp, err = http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("push webhook: %v %v", resp, err)
	}
	resp.Body.Close()
	var branches []struct {
		Branch string `json:"branch"`
		Pusher string `json:"pusher"`
		PR     *struct {
			Number int `json:"number"`
		} `json:"pr"`
	}
	if code := robin.json("GET", "/api/v1/branches?project="+p.Key, nil, &branches); code != 200 || len(branches) != 1 || branches[0].Branch != "fix" || branches[0].Pusher != "sam" || branches[0].PR == nil || branches[0].PR.Number != 7 {
		t.Fatalf("branches: %d %+v", code, branches)
	}
	if code, _ := stranger.do("GET", "/api/v1/branches?project="+p.Key, nil); code != 404 {
		t.Errorf("a stranger listed branches: %d", code)
	}
	if code := stranger.json("GET", "/api/v1/branches", nil, &branches); code != 200 || len(branches) != 0 {
		t.Errorf("branches of nothing the user sees: %d %+v", code, branches)
	}
}
