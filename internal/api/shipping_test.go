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
	"net/http/httptest"
	"strings"
	"sync"
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
	if code, b := robin.do("PATCH", fmt.Sprintf("/api/v1/projects/%s/repos/%d", p.Key, repo.ID), map[string]any{"sync_issues": true, "bots": false, "gate_severity": "urgent"}); code != 400 {
		t.Errorf("bad severity with other switches: %d %s", code, b)
	}
	if code := robin.json("GET", fmt.Sprintf("/api/v1/projects/%s/repos", p.Key), nil, &repos); code != 200 || repos[0]["sync_issues"] == true || repos[0]["bots"] != true {
		t.Errorf("a refused request changed the repo: %d %v", code, repos[0])
	}
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
	if code, _ := robin.do("POST", "/api/v1/findings/99/dismiss", map[string]string{"note": "x"}); code != 404 {
		t.Errorf("unknown finding: %d", code)
	}
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

// The ship's -m and --summary go with it only when given: the request from
// before they existed is still decoded, and the ones with them are not
// refused as unknown fields (WP-1370).
func TestOpenPullShipWordsDecode(t *testing.T) {
	// Absent keeps the stored value; "" clears it.
	for name, tc := range map[string]struct {
		raw          string
		msg, summary *string
	}{
		"absent": {`{"head":"b"}`, nil, nil},
		"empty":  {`{"head":"b","message":"","summary":""}`, ptrS(""), ptrS("")},
		"given":  {`{"head":"b","message":"m","summary":"s"}`, ptrS("m"), ptrS("s")},
	} {
		var in openPullBody
		if err := json.Unmarshal([]byte(tc.raw), &in); err != nil {
			t.Fatal(err)
		}
		same := func(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
		if !same(in.Message, tc.msg) || !same(in.Summary, tc.summary) {
			t.Errorf("%s: decoded message %v summary %v", name, in.Message, in.Summary)
		}
	}
	ts, svc := setup(t)
	ctx := context.Background()
	u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "robin@example.com", Password: "correct-horse-battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, _ := svc.CreateAPIToken(ctx, &core.Actor{User: u}, "robin", false, 0)
	c := &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	for name, body := range map[string]map[string]any{
		"old cli":    {"head": "b", "review": "auto"},
		"new cli":    {"head": "b", "review": "auto", "message": "m", "summary": "s"},
		"given none": {"head": "b", "message": "", "summary": ""},
	} {
		code, raw := c.do("POST", "/api/v1/projects/NOPE/pulls", body)
		if strings.Contains(string(raw), "unknown field") {
			t.Errorf("%s: %d %s", name, code, raw)
		}
	}
	if code, raw := c.do("POST", "/api/v1/projects/NOPE/pulls", map[string]any{"head": "b", "nonsense": 1}); code != http.StatusBadRequest || !strings.Contains(string(raw), "unknown field") {
		t.Errorf("a key nobody knows should still be refused: %d %s", code, raw)
	}
}

// After a reviewed round the completeness check had the put-off notes as
// leads, so the open doesn't print them again; the open subtasks no check
// reads stay, and a direct open keeps everything (WP-1370).
func TestPutOffAfterOpenSkipsNotesAfterAReviewedRound(t *testing.T) {
	_, svc := setup(t)
	ctx := context.Background()
	admin, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "robin@example.com", Password: "correct-horse-battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	a := &core.Actor{User: admin}
	name := "App"
	p, err := svc.CreateProject(ctx, a, core.ProjectInput{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := svc.CreateIssue(ctx, a, core.IssueInput{Project: p.Key, Title: "Speed up the parser", Description: "Step 2 only if the first pass is still slow."})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateIssue(ctx, a, core.IssueInput{Parent: parent.Key, Type: "subtask", Title: "Measure it"}); err != nil {
		t.Fatal(err)
	}
	s := &Server{svc: svc}

	direct := s.putOffAfterOpen(ctx, a, parent.Key, false)
	if direct == nil || len(direct.Notes) == 0 || len(direct.Open) != 1 {
		t.Fatalf("a direct open should show the notes and the open subtask: %+v", direct)
	}
	reviewed := s.putOffAfterOpen(ctx, a, parent.Key, true)
	if reviewed == nil || len(reviewed.Notes) != 0 || reviewed.More != 0 || len(reviewed.Open) != 1 {
		t.Fatalf("after a reviewed round only the open subtask should stay: %+v", reviewed)
	}
}

// PATCH /pulls/{id} carries a base to the service and the open body's
// stacked_on is decoded: against a fake forge that records what it is told
// (WP-1670).
func TestPullBaseAndStackedOnAPI(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	admin, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "robin@example.com", Password: "correct-horse-battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	a := &core.Actor{User: admin}
	tok, _, _ := svc.CreateAPIToken(ctx, a, "robin", false, 0)
	robin := &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}

	var mu sync.Mutex
	var patched, created []map[string]any
	prJSON := func(n int, head, base string) map[string]any {
		return map[string]any{"number": n, "title": head, "state": "open", "html_url": fmt.Sprintf("https://forge.test/me/app/pulls/%d", n),
			"user": map[string]string{"login": "robin"}, "head": map[string]string{"ref": head, "sha": "abc1234"}, "base": map[string]string{"ref": base}}
	}
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/repos/me/app":
			_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"})
		case "GET /api/v1/repos/me/app/pulls/7":
			_ = json.NewEncoder(w).Encode(prJSON(7, "fix", "main"))
		case "PATCH /api/v1/repos/me/app/pulls/7":
			patched = append(patched, body)
			_ = json.NewEncoder(w).Encode(prJSON(7, "fix", "release"))
		case "POST /api/v1/repos/me/app/pulls":
			created = append(created, body)
			_ = json.NewEncoder(w).Encode(prJSON(8, "child", fmt.Sprint(body["base"])))
		default:
			t.Logf("fake forge: unhandled %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(forge.Close)

	kind := "forgejo"
	tokn := "t"
	f, err := svc.CreateForge(ctx, a, core.ForgeInput{Kind: &kind, BaseURL: &forge.URL, Token: &tokn})
	if err != nil {
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
	secret, _ := svc.ForgeSecret(ctx, a, f.ID)
	body, _ := json.Marshal(map[string]any{"action": "opened", "repository": map[string]any{"full_name": "me/app"}, "pull_request": prJSON(7, "fix", "main")})
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/v1/hooks/forge/%d", ts.URL, f.ID), bytes.NewReader(body))
	req.Header.Set("X-Forgejo-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("webhook: %v %v", resp, err)
	} else {
		resp.Body.Close()
	}
	svc.Wait()
	var d struct {
		ID int64 `json:"id"`
	}
	if code := robin.json("GET", fmt.Sprintf("/api/v1/projects/%s/pulls/7", p.Key), nil, &d); code != 200 || d.ID == 0 {
		t.Fatalf("pull: %d %+v", code, d)
	}
	pull := fmt.Sprintf("/api/v1/pulls/%d", d.ID)

	for _, in := range []map[string]any{{}, {"base": "  "}, {"base": "fix"}} {
		if code, b := robin.do("PATCH", pull, in); code != 400 {
			t.Errorf("edit %v: %d %s", in, code, b)
		}
	}
	if len(patched) != 0 {
		t.Fatalf("the forge was told %v of a refused edit", patched)
	}
	if code, b := robin.do("PATCH", pull, map[string]any{"base": "release"}); code != 200 && code != 204 {
		t.Fatalf("edit the base: %d %s", code, b)
	}
	mu.Lock()
	if len(patched) != 1 || patched[0]["base"] != "release" {
		t.Errorf("forge was told %v", patched)
	}
	mu.Unlock()

	if code, b := robin.do("POST", fmt.Sprintf("/api/v1/projects/%s/pulls", p.Key), map[string]any{"repo": "me/app", "head": "child", "title": "child", "ready": true, "stacked_on": []string{"scratch", "fix"}}); code != 200 && code != 201 {
		t.Fatalf("open: %d %s", code, b)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(created) != 1 || created[0]["base"] != "fix" {
		t.Errorf("the forge was asked to open %v", created)
	}
}
