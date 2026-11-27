package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// clear reports a /clear of the session, its end heard first or second, and
// works in the new session from then on.
func (a *apEnv) clear(endFirst bool) {
	a.t.Helper()
	a.setClock(a.clockNow().Add(time.Minute))
	end := hook("box", a.cli, "SessionEnd", func(h *HookEvent) { h.EndReason = "clear" })
	start := hook("box", a.cli+"-c", "SessionStart", func(h *HookEvent) { h.Source = "clear" })
	var newID int64
	for _, ev := range map[bool][]HookEvent{true: {end, start}, false: {start, end}}[endFirst] {
		res, err := a.s.SessionHook(a.ctx, a.admin, []HookEvent{ev})
		if err != nil {
			a.t.Fatal(err)
		}
		if ev.Event == "SessionStart" {
			newID = res.Session
		}
	}
	a.cli, a.session, a.ag.Session = a.cli+"-c", newID, newID
}

func (a *apEnv) openSessions() []*AutopilotOpenSession {
	a.t.Helper()
	o, err := a.s.AutopilotOverview(a.ctx, a.admin)
	if err != nil {
		a.t.Fatal(err)
	}
	for _, p := range o.Projects {
		if p.ProjectKey == a.p.Key {
			return p.Sessions
		}
	}
	a.t.Fatal("no project in the overview")
	return nil
}

// The overview lists a person's open sessions beside the steps, busy or
// idle; Start has autopilot start the next task in an idle one at once,
// one that finished a task or a fresh one that never had any (WP-307).
func TestAutopilotStartsAnIdleSession(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	second := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude", Priority: "high"})
	third := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Third", Agent: "claude"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.prompt("Go on")
	list := a.openSessions()
	if len(list) != 1 || list[0].ID != a.session || list[0].Task == nil || list[0].Task.Key != first.Key || list[0].Startable || list[0].Why == "" {
		t.Fatalf("busy session: %+v", list)
	}
	if err := a.s.StartAutopilotSession(a.ctx, a.admin, a.session); err == nil {
		t.Fatal("started a task in a session working on one")
	}
	a.setClock(a.clockNow().Add(time.Minute))
	if _, err := a.s.AgentDone(a.ctx, a.ag, first.Key, AgentDoneInput{Comment: "done"}); err != nil {
		t.Fatal(err)
	}
	if res := a.stopWith("Done."); strings.Contains(res.Note, "Autopilot") {
		t.Fatalf("note = %q", res.Note)
	}
	list = a.openSessions()
	if len(list) != 1 || list[0].State != SessionIdle || !list[0].Startable || list[0].Task != nil {
		t.Fatalf("idle session: %+v", list)
	}
	if err := a.s.StartAutopilotSession(a.ctx, a.ag, a.session); err == nil {
		t.Fatal("an agent started a task with autopilot")
	}
	if err := a.s.StartAutopilotSession(a.ctx, a.admin, a.session); err != nil {
		t.Fatal(err)
	}
	a.s.Wait()
	if len(a.jobs) != 1 {
		t.Fatalf("Start queued no job: %+v", a.step())
	}
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[0].Input, &in)
	if in.Key != second.Key || in.Done != first.Key || in.Agent != "anthropic/claude-opus-5-5" {
		t.Errorf("job = %+v", in)
	}

	f, err := a.s.CreateForge(a.ctx, a.admin, ForgeInput{Kind: ptr("forgejo"), BaseURL: ptr("https://git.example.com"), Token: ptr("t")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.db.W.Exec(`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'acme/app', 1)`, a.p.ID, f.ID); err != nil {
		t.Fatal(err)
	}
	res, err := a.s.SessionHook(a.ctx, a.admin, []HookEvent{hook("box", "cli-fresh", "SessionStart", func(h *HookEvent) { h.Source, h.Repo = "startup", "acme/app" })})
	if err != nil {
		t.Fatal(err)
	}
	var fresh *AutopilotOpenSession
	for _, x := range a.openSessions() {
		if x.ID == res.Session {
			fresh = x
		}
	}
	if fresh == nil || fresh.State != SessionWorking || !fresh.Fresh || !fresh.Startable {
		t.Fatalf("fresh session: %+v", fresh)
	}
	if err := a.s.StartAutopilotSession(a.ctx, a.admin, res.Session); err != nil {
		t.Fatal(err)
	}
	a.s.Wait()
	if len(a.jobs) != 2 {
		t.Fatal("Start queued no job for the fresh session")
	}
	_ = json.Unmarshal(a.jobs[1].Input, &in)
	if in.Key != third.Key || in.Done != "" || in.CLIID != "cli-fresh" || in.Agent != "anthropic/claude-code" {
		t.Errorf("fresh job = %+v", in)
	}
	var stepID int64
	_ = a.s.db.R.QueryRow(`SELECT id FROM autopilot_steps WHERE session_id = ?`, res.Session).Scan(&stepID)
	if err := a.s.AutopilotStepAction(a.ctx, a.admin, stepID, "stop"); err != nil {
		t.Fatal(err)
	}
	if d := a.get(a.admin, third.Key); d.ClaimedBy != "" {
		t.Errorf("stopping kept %s reserved for %q", third.Key, d.ClaimedBy)
	}
	if _, err := a.s.SessionHook(a.ctx, a.admin, []HookEvent{hook("box", "cli-fresh", "UserPromptSubmit", func(h *HookEvent) { h.Prompt = "Look at the logs" })}); err != nil {
		t.Fatal(err)
	}
	if err := a.s.StartAutopilotSession(a.ctx, a.admin, res.Session); err == nil || !strings.Contains(err.Error(), "working") {
		t.Errorf("started a task in a session someone is using: %v", err)
	}
}

// Background work reads as such, each label short.
func TestAutopilotBackgroundDetail(t *testing.T) {
	seen := map[string]int64{bgSig("shell", "cd /x && go test ./... -run TestSomethingVeryLongIndeed -count=1 -timeout 20m\nmore"): 10}
	live, _ := liveBackground(seen, 0, 20)
	if len(live) != 1 || !strings.HasSuffix(live[0], "…") || len([]rune(live[0])) > 61 {
		t.Errorf("label = %q", live)
	}
}
