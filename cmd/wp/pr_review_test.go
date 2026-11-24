package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// wp pr review lists a pull request's review bot runs, retries the review
// and cancels queued runs; agents can't cancel the head's review (WP-217).
func TestCLIPRReview(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	prRepo(t, e, me)
	gitRepo(t, "git@git.example.com:robin/waypoint.git")
	ctx := context.Background()

	out := e.wp("pr", "review", "3")
	if !regexp.MustCompile(`robin/waypoint#3 review bot runs, newest first:\n  run 1 +queued +review abc1233  just now\nStop the queued and running ones: wp pr review cancel 3\n`).MatchString(out) {
		t.Errorf("wp pr review:\n%s", out)
	}
	if out, code := e.run("pr", "review", "retry", "3"); code != 0 || !strings.Contains(out, "a review is already queued at abc1233 (run 1); nothing to retry") {
		t.Errorf("retry while queued: %d %s", code, out)
	}
	// An agent cancelling the head's review would let it merge unreviewed.
	if out, code := e.run("pr", "review", "cancel", "3"); code != 1 || !strings.Contains(out, "agents can't cancel the review of a pull request's head") {
		t.Errorf("agent cancels the head's review: %d %s", code, out)
	}
	if out, code := e.run("pr", "review", "cancel", "--run", "1"); code == 0 || !strings.Contains(out, "agents can't cancel") {
		t.Errorf("agent cancels the head's review by --run: %d %s", code, out)
	}
	if _, err := e.svc.CancelBotRun(ctx, me, 1); err != nil {
		t.Fatal(err)
	}
	out = e.wp("pr", "review", "3")
	if !strings.Contains(out, "run 1     cancelled review abc1233") || !strings.Contains(out, "Cancelled by robin") || !strings.Contains(out, "Review the head again: wp pr review retry 3") {
		t.Errorf("wp pr review, cancelled:\n%s", out)
	}
	if out = e.wp("pr", "review", "cancel", "3", "--json"); strings.TrimSpace(out) != "[]" {
		t.Errorf("cancel --json with nothing live: %s", out)
	}
	if out = e.wp("pr", "review", "retry", "3"); !strings.Contains(out, "robin/waypoint#3: review queued again at abc1233 (run 2).") {
		t.Errorf("retry:\n%s", out)
	}

	push(t, e, me, "abc1234def")
	out, code := e.run("pr", "review", "cancel", "3")
	if code != 1 || !regexp.MustCompile(`run 3 \(review at abc1234\): .*agents can't cancel`).MatchString(out) || strings.Contains(out, "run 2") {
		t.Errorf("cancel after a push: %d %s", code, out)
	}
	out, code = e.run("pr", "review", "cancel", "3", "--json")
	var refused []map[string]any
	if err := json.Unmarshal([]byte(out[:strings.LastIndex(out, "]")+1]), &refused); code != 1 || err != nil || len(refused) != 1 || refused[0]["id"] != float64(3) ||
		!strings.Contains(fmt.Sprint(refused[0]["action_error"]), "agents can't cancel") {
		t.Errorf("cancel --json, refused: %d %v %s", code, err, out)
	}
	var runs []struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(e.wp("pr", "review", "3", "--json")), &runs); err != nil || len(runs) != 3 || runs[0].Status != "queued" || runs[1].Status != "skipped" {
		t.Errorf("wp pr review --json: %+v %v", runs, err)
	}
	if out = e.wp("pr", "review", "3", "-n", "1"); !strings.Contains(out, "run 3 ") || !strings.Contains(out, "… 2 older (-n 3)") {
		t.Errorf("wp pr review -n 1:\n%s", out)
	}
	for _, n := range []string{"0", "-1"} {
		if out, code := e.run("pr", "review", "3", "-n", n); code != 2 || !strings.Contains(out, "-n takes") {
			t.Errorf("wp pr review -n %s: %d %s", n, code, out)
		}
	}
	if out = e.wp("pr", "review", "retry", "--run", "1"); !strings.Contains(out, "robin/waypoint#3: review queued again at abc1234 (run 3).") {
		t.Errorf("retry --run:\n%s", out)
	}
	if out, code := e.run("pr", "review", "retry", "3", "--run", "1"); code != 2 || !strings.Contains(out, "not both") {
		t.Errorf("N and --run: %d %s", code, out)
	}
	if out := e.wp("help", "pr"); !strings.Contains(out, "wp pr review [cancel|retry] [N...] [--run ID]") {
		t.Errorf("wp help pr:\n%s", out)
	}
}

// push tells Waypoint pull request 3 of prRepo has a new head.
func push(t *testing.T, e *env, me *core.Actor, sha string) {
	t.Helper()
	ctx := context.Background()
	fs, err := e.svc.ListForges(ctx, me)
	if err != nil || len(fs) == 0 {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"action": "synchronized", "repository": map[string]any{"full_name": "robin/waypoint"},
		"pull_request": map[string]any{"number": 3, "title": "Change 3", "body": "Generated with [Claude Code](https://claude.com/claude-code)", "state": "open",
			"html_url": "https://git.example.com/robin/waypoint/pulls/3", "user": map[string]string{"login": "robin"},
			"head": map[string]string{"ref": "b3", "sha": sha}, "base": map[string]string{"ref": "main"}}})
	secret, _ := e.svc.ForgeSecret(ctx, me, fs[0].ID)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	h := http.Header{}
	h.Set("X-Forgejo-Event", "pull_request")
	h.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	if err := e.svc.HandleForgeWebhook(ctx, fs[0].ID, h, payload); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
}

// With two review rules, retry retries each rule's review that isn't
// waiting already, not just the newest run (WP-217).
func TestCLIPRReviewTwoRules(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	prRepo(t, e, me)
	gitRepo(t, "git@git.example.com:robin/waypoint.git")
	ctx := context.Background()
	rule := core.Rule{Trigger: core.Trigger{Event: "pr_updated"}, Actions: []core.Action{{Type: "review"}}}
	ps, err := e.svc.ListProjects(ctx, me, false)
	if err != nil || len(ps) == 0 {
		t.Fatal(ps, err)
	}
	if _, err := e.svc.CreateAutomation(ctx, me, ps[len(ps)-1].Key, core.AutomationInput{Name: ptr("Second opinion"), Rule: &rule}); err != nil {
		t.Fatal(err)
	}
	push(t, e, me, "abc1234def")
	var runs []struct {
		ID         int64  `json:"id"`
		Status     string `json:"status"`
		Automation string `json:"automation"`
	}
	if err := json.Unmarshal([]byte(e.wp("pr", "review", "3", "--json")), &runs); err != nil || len(runs) != 3 || runs[0].Status != "queued" || runs[1].Status != "queued" {
		t.Fatalf("runs after the push: %+v %v", runs, err)
	}
	if _, err := e.svc.CancelBotRun(ctx, me, runs[1].ID); err != nil {
		t.Fatal(err)
	}
	out := e.wp("pr", "review", "retry", "3")
	if !strings.Contains(out, fmt.Sprintf("queued again at abc1234 (run %d).", runs[0].ID+1)) || !strings.Contains(out, fmt.Sprintf("already queued at abc1234 (run %d)", runs[0].ID)) || !strings.Contains(out, "a review (Second opinion) is") {
		t.Errorf("retry with two rules:\n%s", out)
	}
}
