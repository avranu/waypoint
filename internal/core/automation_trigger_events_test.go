package core

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func (e *env) notifyRule(trig Trigger) Rule {
	return Rule{Trigger: trig, Actions: []Action{{Type: "notify", User: "admins", Title: "fired", Message: "{{.Trigger.Kind}}"}}}
}

func TestRequestTriggersFireFromRealRequests(t *testing.T) {
	r := newReqEnv(t)
	question := r.addAuto(r.p, "question", r.notifyRule(Trigger{Event: "request_created", Extra: map[string]any{"kind": RequestQuestion}}))
	permission := r.addAuto(r.p, "permission", r.notifyRule(Trigger{Event: "request_created", Extra: map[string]any{"kind": RequestPermission}}))
	anyKind := r.addAuto(r.p, "any", r.notifyRule(Trigger{Event: "request_created"}))
	decided := r.addAuto(r.p, "decided", r.notifyRule(Trigger{Event: "request_decided", Extra: map[string]any{"kind": RequestQuestion}}))
	late := r.addAuto(r.p, "late", Rule{Trigger: Trigger{Event: "request_waiting", Extra: map[string]any{"after": "15m"}},
		Actions: []Action{{Type: "notify", User: "admins", Message: "{{.Trigger.Data.code}} {{.Trigger.Data.kind}}"}}})

	q := r.ask(RequestInput{Kind: RequestQuestion, Title: "Which database?", Questions: choiceQ()})
	counts := func() []int {
		return []int{len(r.runs(question)), len(r.runs(permission)), len(r.runs(anyKind)), len(r.runs(decided))}
	}
	if got := counts(); !slices.Equal(got, []int{1, 0, 1, 0}) {
		t.Fatalf("after a question: %v", got)
	}
	ag := agentActor(r.ana, "anthropic/claude-code")
	if _, err := r.s.SessionHook(r.ctx, ag, []HookEvent{hook("box", "cli-1", "PermissionDenied", func(h *HookEvent) {
		h.Tool, h.ToolInput, h.ToolUseID, h.Reason = "Bash", json.RawMessage(`{"command":"rm -rf build"}`), "tu-1", "auto mode denied it"
	})}); err != nil {
		t.Fatal(err)
	}
	r.s.DrainAutomationRuns(r.ctx)
	if got := counts(); !slices.Equal(got, []int{1, 0, 1, 0}) {
		t.Fatalf("after a refused tool call: %v", got)
	}
	r.ask(RequestInput{Kind: RequestPermission, Title: "Allow Bash?", Tool: "Bash", ToolInput: json.RawMessage(`{"command":"rm -rf build"}`)})
	if got := counts(); !slices.Equal(got, []int{1, 1, 2, 0}) {
		t.Fatalf("after a permission request: %v", got)
	}
	run := r.run(r.runs(permission)[0].ID)
	if run.Event == nil || run.Event.Data["kind"] != RequestPermission {
		t.Errorf("the permission run's event = %+v", run.Event)
	}
	if _, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "answer", Text: "sqlite"}); err != nil {
		t.Fatal(err)
	}
	if got := counts(); !slices.Equal(got, []int{1, 1, 2, 1}) {
		t.Fatalf("after the answer: %v", got)
	}
	r.setClock(r.clockNow().Add(16 * time.Minute))
	r.s.ExpireAutomationWaits(r.ctx)
	r.s.DrainAutomationRuns(r.ctx)
	runs := r.runs(late)
	if len(runs) != 1 {
		t.Fatalf("%d late runs, want the undecided request only", len(runs))
	}
	if ev := r.run(runs[0].ID).Event; ev == nil || ev.Data["kind"] != RequestPermission || ev.Data["code"] != "R-2" {
		t.Errorf("late run for %+v", ev)
	}
}

func TestSessionTriggersAndTheWaitingCard(t *testing.T) {
	r := newReqEnv(t)
	ag := agentActor(r.ana, "anthropic/claude-code")
	waiting := r.addAuto(r.p, "waiting", r.notifyRule(Trigger{Event: "session_waiting"}))
	card := r.addAuto(r.p, "Tell me when an agent waits more than 15 minutes", templateRule(t, "Tell me when an agent waits more than 15 minutes"))
	if _, err := r.s.SessionHook(r.ctx, ag, []HookEvent{hook("box", "cli-2", "SessionStart", nil)}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.db.W.Exec(`UPDATE agent_sessions SET project_id = ?, repo = 'acme/app' WHERE cli_id = 'cli-2'`, r.p.ID); err != nil {
		var n int
		_ = r.s.db.R.QueryRow(`SELECT count(*) FROM agent_sessions`).Scan(&n)
		t.Fatalf("project for the second session (%d sessions): %v", n, err)
	}
	notification := func(id string) {
		t.Helper()
		if _, err := r.s.SessionHook(r.ctx, ag, []HookEvent{hook("box", id, "Notification", func(h *HookEvent) { h.NotificationType, h.Message = "idle_prompt", "Claude is waiting" })}); err != nil {
			t.Fatal(err)
		}
	}
	notification("cli-1")
	notification("cli-2")
	r.s.DrainAutomationRuns(r.ctx)
	if n := len(r.runs(waiting)); n != 2 {
		t.Fatalf("session.waiting fired %d times", n)
	}
	if ev := r.run(r.runs(waiting)[0].ID).Event; ev == nil || ev.Data["reason"] != "idle_prompt" || ev.Data["repo"] != "acme/app" {
		t.Errorf("event = %+v", ev)
	}
	if _, err := r.s.SessionHook(r.ctx, ag, []HookEvent{hook("box", "cli-1", "UserPromptSubmit", func(h *HookEvent) { h.Prompt = "go on" })}); err != nil {
		t.Fatal(err)
	}
	r.setClock(r.clockNow().Add(16 * time.Minute))
	r.s.DrainAutomationRuns(r.ctx)
	r.s.DrainAutomationRuns(r.ctx)
	ns, _, _ := r.s.ListNotifications(r.ctx, r.admin, false, 20)
	told := 0
	for _, n := range ns {
		if n.Title == "An agent is waiting for you" {
			told++
		}
	}
	if told != 1 {
		t.Errorf("%d waiting notifications; only the session that stayed put should notify", told)
	}
	for _, run := range r.runs(card) {
		if run.Status != RunSucceeded {
			t.Errorf("card run %d: %s %s", run.ID, run.Status, run.Error)
		}
	}
}

func TestEnvironmentTriggersFireFromChecksAndDeploys(t *testing.T) {
	x := newEnvT(t)
	f, srv := newFlaky(t)
	down := x.addAuto(x.p, "down", x.notifyRule(Trigger{Event: "env_down", Extra: map[string]any{"environment": "stag*"}}))
	downProd := x.addAuto(x.p, "down prod", x.notifyRule(Trigger{Event: "env_down", Extra: map[string]any{"environment": "prod*"}}))
	up := x.addAuto(x.p, "up", x.notifyRule(Trigger{Event: "env_up"}))
	deployed := x.addAuto(x.p, "deployed", x.notifyRule(Trigger{Event: "env_deployed", Extra: map[string]any{"environment": "prod"}}))
	failed := x.addAuto(x.p, "deploy failed", x.notifyRule(Trigger{Event: "env_deploy_failed"}))
	card := x.addAuto(x.p, "Tell me when an environment goes down", templateRule(t, "Tell me when an environment goes down"))

	d := x.create("staging", srv.URL)
	x.check(d.ID)
	f.code.Store(503)
	x.setClock(x.clockNow().Add(time.Minute))
	x.check(d.ID)
	x.setClock(x.clockNow().Add(time.Minute))
	x.check(d.ID)
	if len(x.runs(down)) != 1 || len(x.runs(downProd)) != 0 || len(x.runs(up)) != 0 || len(x.runs(card)) != 1 {
		t.Fatalf("after it went down: down %d, prod %d, up %d, card %d", len(x.runs(down)), len(x.runs(downProd)), len(x.runs(up)), len(x.runs(card)))
	}
	if ev := x.run(x.runs(down)[0].ID).Event; ev == nil || ev.Data["name"] != "staging" || ev.Data["status"] != EnvDown {
		t.Errorf("down event = %+v", ev)
	}
	ns, _, _ := x.s.ListNotifications(x.ctx, x.admin, false, 20)
	var cardNote string
	for _, n := range ns {
		if n.Title == "staging is down" {
			cardNote = n.Body
		}
	}
	if cardNote == "" {
		t.Errorf("the card sent nothing: %+v", ns)
	}
	f.code.Store(200)
	x.setClock(x.clockNow().Add(time.Minute))
	x.check(d.ID)
	if len(x.runs(up)) != 1 || len(x.runs(down)) != 1 {
		t.Errorf("after recovery: up %d, down %d", len(x.runs(up)), len(x.runs(down)))
	}

	if _, err := x.s.RecordDeploymentNamed(x.ctx, x.editor, x.p.Key, "prod", DeployInput{Commit: "abcdef1234", Branch: "main", Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.RecordDeploymentNamed(x.ctx, x.editor, x.p.Key, "staging", DeployInput{Commit: "1111111", Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	if len(x.runs(deployed)) != 1 || len(x.runs(failed)) != 1 {
		t.Errorf("deploys: deployed %d, failed %d", len(x.runs(deployed)), len(x.runs(failed)))
	}
}

func TestPullRequestAndCodeTriggersFireFromWebhooks(t *testing.T) {
	se := newShipEnv(t, "github")
	se.gate("P2")
	n := func(name string, trig Trigger) *Automation {
		return se.addAuto(se.p, name, Rule{Trigger: trig, Actions: []Action{{Type: "notify", User: "admins", Title: name,
			Message: "{{.Trigger.Kind}} {{if .PR}}{{.PR.Title}} #{{.PR.Number}}{{end}} {{.Trigger.Data.branch}} {{.Trigger.Data.tag}}"}}})
	}
	ciPassed := n("ci passed", Trigger{Event: "pr_ci_passed"})
	ciFailed := n("ci failed", Trigger{Event: "pr_ci_failed"})
	gateGreen := n("gate green", Trigger{Event: "pr_review_gate", Extra: map[string]any{"state": "success"}})
	approved := n("approved", Trigger{Event: "pr_review", Extra: map[string]any{"verdict": "approved"}})
	pushedMain := n("pushed main", Trigger{Event: "repo_pushed", Extra: map[string]any{"branch": "main"}})
	docs := n("docs changed", Trigger{Event: "files_changed", Extra: map[string]any{"paths": []any{"docs/**", "**/*.md"}}})
	web := n("web changed", Trigger{Event: "files_changed", Extra: map[string]any{"paths": []any{"web/**"}, "branch": "main"}})
	created := n("branch created", Trigger{Event: "branch_created"})
	release := n("release", Trigger{Event: "release_published"})

	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.pullHook("opened", testPR)
	se.s.Wait()
	se.sg.setChecks(testPR.HeadSHA, CommitCheck{Name: ciBackend, State: CheckFailed})
	se.poll()
	se.s.DrainAutomationRuns(se.ctx)
	if len(se.env.runs(ciFailed)) != 1 || len(se.env.runs(ciPassed)) != 0 {
		t.Fatalf("CI failed: %d / passed %d", len(se.env.runs(ciFailed)), len(se.env.runs(ciPassed)))
	}
	if ev := se.run(se.env.runs(ciFailed)[0].ID); ev.Event == nil || ev.Event.Data["state"] != "failed" || ev.Status != RunSucceeded {
		t.Errorf("ci failed run = %+v", ev)
	}
	next := testPR
	next.HeadSHA = "2222222bbbb"
	se.sg.setChecks(next.HeadSHA, CommitCheck{Name: ciBackend, State: CheckSuccess})
	se.pullHook("synchronize", next)
	se.s.Wait()
	se.drain()
	se.s.Wait()
	se.s.DrainAutomationRuns(se.ctx)
	if len(se.env.runs(ciPassed)) != 1 {
		t.Errorf("CI passed: %d runs", len(se.env.runs(ciPassed)))
	}
	ns, _, _ := se.s.ListNotifications(se.ctx, se.admin, false, 30)
	var withPR bool
	for _, x := range ns {
		withPR = withPR || (x.Title == "ci passed" && x.Body == "pr.ci Fix login #5")
	}
	if !withPR {
		t.Errorf("the pull request wasn't available to a pr.ci run: %+v", ns)
	}
	if len(se.env.runs(gateGreen)) == 0 {
		t.Errorf("the review gate trigger didn't fire")
	}
	if err := se.hook(se.f, "pull_request_review", map[string]any{"action": "submitted", "review": map[string]any{"id": 7, "state": "approved", "user": map[string]string{"login": "ann"}, "commit_id": next.HeadSHA, "body": "LGTM"},
		"pull_request": map[string]any{"number": 5}, "repository": map[string]any{"full_name": "me/app"}, "sender": map[string]string{"login": "ann"}}); err != nil {
		t.Fatal(err)
	}
	se.s.Wait()
	if len(se.env.runs(approved)) != 1 {
		t.Errorf("review submitted: %d runs", len(se.env.runs(approved)))
	}

	push := func(branch string, isNew bool, files ...string) {
		t.Helper()
		before := "1111111111111111111111111111111111111111"
		if isNew {
			before = "0000000000000000000000000000000000000000"
		}
		if err := se.hook(se.f, "push", map[string]any{"ref": "refs/heads/" + branch, "before": before, "after": "3333333333333333333333333333333333333333", "pusher": map[string]string{"name": "sam"},
			"repository": map[string]any{"full_name": "me/app"}, "commits": []map[string]any{{"id": "3333", "message": "m", "url": "u", "added": files, "modified": []string{}, "removed": []string{}}}}); err != nil {
			t.Fatal(err)
		}
		se.s.Wait()
	}
	push("main", false, "docs/guide.md", "internal/x.go")
	push("feature", true, "web/src/App.tsx")
	push("main", false, "web/src/App.tsx")
	counts := []int{len(se.env.runs(pushedMain)), len(se.env.runs(docs)), len(se.env.runs(web)), len(se.env.runs(created))}
	if !slices.Equal(counts, []int{2, 1, 1, 1}) {
		t.Errorf("pushes: main %d, docs %d, web (on main) %d, branch created %d", counts[0], counts[1], counts[2], counts[3])
	}
	if ev := se.run(se.env.runs(docs)[0].ID).Event; ev == nil || !slices.Contains(anyStrings(ev.Data["paths"]), "docs/guide.md") || ev.Data["pusher"] != "sam" {
		t.Errorf("files-changed event = %+v", ev)
	}
	if err := se.hook(se.f, "release", map[string]any{"action": "published", "release": map[string]any{"tag_name": "v1.2.0", "name": "One two", "html_url": "https://x/r", "draft": false},
		"repository": map[string]any{"full_name": "me/app"}}); err != nil {
		t.Fatal(err)
	}
	se.s.Wait()
	if runs := se.env.runs(release); len(runs) != 1 {
		t.Errorf("release: %d runs", len(runs))
	} else if ev := se.run(runs[0].ID).Event; ev == nil || ev.Data["tag"] != "v1.2.0" {
		t.Errorf("release event = %+v", ev)
	}
}
