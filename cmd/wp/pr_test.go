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

func TestCLIPullRequests(t *testing.T) {
	e := newEnv(t)
	me := e.login() // an agent token (claude opus)
	ctx := context.Background()
	kind, base := "forgejo", "https://git.example.com"
	f, err := e.svc.CreateForge(ctx, me, core.ForgeInput{Kind: &kind, BaseURL: &base})
	if err != nil {
		t.Fatal(err)
	}
	forgePeople(t, e, me, f)
	p, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Waypoint")})
	if err != nil {
		t.Fatal(err)
	}
	pr, err := e.svc.AddProjectRepo(ctx, me, p.Key, f.ID, "robin/waypoint", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetRepoMergePolicy(ctx, me, p.Key, pr.ID, core.MergePolicyInput{ExternalReviewers: &[]string{"scanner/codeql"}}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	hook := func(number int, text, author string) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"action": "opened", "repository": map[string]any{"full_name": "robin/waypoint"},
			"pull_request": map[string]any{"number": number, "title": fmt.Sprintf("Change %d", number), "body": text, "state": "open",
				"html_url": fmt.Sprintf("%s/robin/waypoint/pulls/%d", base, number), "user": map[string]string{"login": author},
				"head": map[string]any{"ref": fmt.Sprintf("b%d", number), "sha": fmt.Sprintf("abc123%d", number), "repo": map[string]string{"full_name": "robin/waypoint"}},
				"base": map[string]string{"ref": "main"}}})
		secret, _ := e.svc.ForgeSecret(ctx, me, f.ID)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(payload)
		h := http.Header{}
		h.Set("X-Forgejo-Event", "pull_request")
		h.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		if err := e.svc.HandleForgeWebhook(ctx, f.ID, h, payload); err != nil {
			t.Fatal(err)
		}
		e.svc.Wait()
	}
	hook(3, "A person's change.", "sam")
	hook(4, "Generated with [Claude Code](https://claude.com/claude-code)", "robin")
	gitRepo(t, "git@git.example.com:robin/waypoint.git")

	out := e.wp("pr")
	if !strings.Contains(out, "robin/waypoint#3") || !strings.Contains(out, "robin/waypoint#4") || !strings.Contains(out, "open") {
		t.Errorf("wp pr:\n%s", out)
	}
	out = e.wp("pr", "show", "3")
	if !strings.Contains(out, "robin/waypoint#3  Change 3  [open]") || !strings.Contains(out, "Not ready:") || !strings.Contains(out, "checks haven't been read yet") {
		t.Errorf("wp pr show:\n%s", out)
	}
	// Merge automatically (WP-133) armed it, and it waits for the reviewers.
	if !strings.Contains(out, "Armed by Merge automatically (repo setting)") || !strings.Contains(out, "scanner/codeql on abc1233 until") {
		t.Errorf("wp pr show, armed by the setting:\n%s", out)
	}
	// What holds it, since when (WP-893).
	if !regexp.MustCompile(`\nHeld \(UTC\):\n  \d\d-\d\d \d\d:\d\d-now +0m  .*\(abc1233\)\n`).MatchString(out) {
		t.Errorf("wp pr show, holds:\n%s", out)
	}
	var d struct {
		ID       int64 `json:"id"`
		Shipping struct {
			Readiness struct {
				Ready bool `json:"ready"`
			} `json:"readiness"`
		} `json:"shipping"`
	}
	if err := json.Unmarshal([]byte(e.wp("pr", "show", "#3", "--json")), &d); err != nil || d.ID == 0 || d.Shipping.Readiness.Ready {
		t.Errorf("wp pr show --json: %+v %v", d, err)
	}
	if out, code := e.run("pr", "show", "99"); code == 0 || !strings.Contains(out, "not found") {
		t.Errorf("unknown pull request: %d %s", code, out)
	}
	if out, code := e.run("pr", "show", "abc"); code != 2 || !strings.Contains(out, "isn't a pull request number") {
		t.Errorf("bad number: %d %s", code, out)
	}
	// An agent arms only what it wrote.
	if out, code := e.run("pr", "merge", "3", "--when-ready"); code == 0 || !strings.Contains(out, "agents merge only the pull requests they wrote") {
		t.Errorf("an agent armed a person's pull request: %d %s", code, out)
	}
	out = e.wp("pr", "merge", "4", "--when-ready", "--style", "squash")
	if !strings.Contains(out, "robin/waypoint#4 will merge when it is ready.") || !strings.Contains(out, "Waiting for:") {
		t.Errorf("arm:\n%s", out)
	}
	out = e.wp("pr", "show", "4")
	if !strings.Contains(out, "Armed by") || !strings.Contains(out, "squash") {
		t.Errorf("show armed:\n%s", out)
	}
	if out = e.wp("pr", "ls"); !strings.Contains(out, "armed") {
		t.Errorf("ls armed:\n%s", out)
	}
	if out = e.wp("pr", "merge", "4", "--disarm"); !strings.Contains(out, "no longer merges when ready") {
		t.Errorf("disarm:\n%s", out)
	}
	// Several in one call (WP-221): the one it can't arm doesn't stop the rest.
	out, code := e.run("pr", "merge", "3,", "4", "--when-ready")
	if code != 1 || !strings.Contains(out, "agents merge only the pull requests they wrote") || !strings.Contains(out, "robin/waypoint#4 will merge when it is ready.") {
		t.Errorf("arm two, one a person's: %d %s", code, out)
	}
	if out, code := e.run("pr", "merge", "3", "4", "--title", "One"); code != 2 || !strings.Contains(out, "one merge commit's") {
		t.Errorf("one title for two merges: %d %s", code, out)
	}
	if out = e.wp("pr", "merge", "4", "--disarm"); !strings.Contains(out, "no longer merges when ready") {
		t.Errorf("disarm again:\n%s", out)
	}
	// Merging now: the forge has no token, so Waypoint says so.
	if out, code := e.run("pr", "merge", "4"); code == 0 || !strings.Contains(out, "no API token") {
		t.Errorf("merge now: %d %s", code, out)
	}
	if out, code := e.run("pr", "merge", "4", "--style", "fast"); code == 0 || !strings.Contains(out, "merge style") {
		t.Errorf("a bad style: %d %s", code, out)
	}
	out = e.wp("pr", "check", "4")
	if !strings.Contains(out, "robin/waypoint#4") {
		t.Errorf("wp pr check:\n%s", out)
	}
	// Findings: an agent must say why it dismisses one.
	if out, code := e.run("pr", "dismiss", "99999"); code == 0 || !strings.Contains(out, "say why") {
		t.Errorf("dismiss without a note: %d %s", code, out)
	}
	if out, code := e.run("pr", "dismiss", "99999", "-m", "a false positive"); code == 0 || !strings.Contains(out, "not found") {
		t.Errorf("dismiss an unknown finding: %d %s", code, out)
	}
	if out, code := e.run("pr", "reopen", "x1"); code != 2 || !strings.Contains(out, "isn't a finding number") {
		t.Errorf("reopen a bad number: %d %s", code, out)
	}
	if out := e.wp("help", "pr"); !strings.Contains(out, "wp pr merge N... [--when-ready]") || !strings.Contains(out, "dismiss ID -m WHY") {
		t.Errorf("wp help pr:\n%s", out)
	}
}

func TestCLIProjectRepo(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	ctx := context.Background()
	kind, base := "forgejo", "https://git.example.com"
	f, err := e.svc.CreateForge(ctx, me, core.ForgeInput{Kind: &kind, BaseURL: &base})
	if err != nil {
		t.Fatal(err)
	}
	forgePeople(t, e, me, f)
	p, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Waypoint")})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"robin/waypoint", "robin/docs"} {
		if _, err := e.svc.AddProjectRepo(ctx, me, p.Key, f.ID, r, false); err != nil {
			t.Fatal(err)
		}
	}
	e.svc.Wait()
	gitRepo(t, "git@git.example.com:robin/waypoint.git")

	out := e.wp("project", "repo")
	if !strings.Contains(out, "robin/waypoint on git.example.com") || !strings.Contains(out, "merge automatically: on · required checks: none · review wait 30m · quiet 10m · external reviewers: none") {
		t.Errorf("wp project repo:\n%s", out)
	}
	// The git repo here is the default; a name picks another.
	out = e.wp("project", "repo", "set", "--auto-merge", "off", "--required-check", "local-ci", "--review-wait", "1h", "--quiet", "5", "--no-reviewers")
	if !strings.Contains(out, "robin/waypoint on") || !strings.Contains(out, "merge automatically: off · required checks: local-ci · review wait 1h · quiet 5m · external reviewers: none") {
		t.Errorf("set:\n%s", out)
	}
	out = e.wp("project", "repo", "set", "docs", "--reviewer", "coderabbit,scanner/codeql", "--quiet", "0")
	if !strings.Contains(out, "robin/docs on") || !strings.Contains(out, "quiet none · external reviewers: coderabbit, scanner/codeql") {
		t.Errorf("set docs:\n%s", out)
	}
	// Merge automatically into some base branches only (WP-1041).
	out = e.wp("project", "repo", "set", "docs", "--into", "release/**")
	if !strings.Contains(out, "merge automatically: on (into release/**) ·") {
		t.Errorf("set --into:\n%s", out)
	}
	out = e.wp("project", "repo", "set", "docs", "--into", "any")
	if !strings.Contains(out, "merge automatically: on · required") {
		t.Errorf("set --into any:\n%s", out)
	}
	// Whether the merge line builds candidates (WP-1659).
	out = e.wp("project", "repo", "set", "docs", "--line-candidates", "off")
	if !strings.Contains(out, "line candidates: off") {
		t.Errorf("set --line-candidates off:\n%s", out)
	}
	out = e.wp("project", "repo", "set", "docs", "--line-candidates", "on")
	if !strings.Contains(out, "line candidates: on") {
		t.Errorf("set --line-candidates on:\n%s", out)
	}
	if out, code := e.run("project", "repo", "set", "docs", "--line-candidates", "bogus"); code == 0 || !strings.Contains(out, "--line-candidates is on or off") {
		t.Errorf("--line-candidates bogus: %d %s", code, out)
	}
	if out, code := e.run("project", "repo", "set", "docs"); code == 0 || !strings.Contains(out, "say what to change") {
		t.Errorf("nothing to change: %d %s", code, out)
	}
	for _, bad := range [][]string{{"--auto-merge", "maybe"}, {"--quiet", "soon"}, {"--reviewer", "x", "--no-reviewers"}, {}, {"nope", "--quiet", "5m"}, {"--into", "others", "--into", "release/*"}, {"--into", "match"}} {
		if out, code := e.run(append([]string{"project", "repo", "set"}, bad...)...); code == 0 {
			t.Errorf("wp project repo set %v: %s", bad, out)
		}
	}
}
