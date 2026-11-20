package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// deny has auto mode refuse a call, which the PermissionDenied hook logs
// (WP-232), and the agent then ask for it itself with a permission request.
func deny(t *testing.T, e *env, a *Actor, cli, tool, input string) {
	t.Helper()
	tu := fmt.Sprint("tu-", e.clockNow().UnixNano())
	res, err := e.s.SessionHook(e.ctx, a, []HookEvent{hook("box", cli, "PermissionDenied", func(h *HookEvent) {
		h.Tool, h.ToolInput, h.ToolUseID, h.Reason = tool, json.RawMessage(input), tu, "auto mode denied it"
	})})
	if err != nil {
		t.Fatal(err)
	}
	in := *a
	in.Session = res.Session
	if _, _, err := e.s.CreateRequest(e.ctx, &in, RequestInput{Kind: RequestPermission, Title: "Allow " + tool + "?", Tool: tool, ToolInput: json.RawMessage(input), ToolUseID: tu, Reason: "auto mode denied it"}); err != nil {
		t.Fatal(err)
	}
}

func latestPermission(t *testing.T, e *env, a *Actor) *Request {
	t.Helper()
	page, err := e.s.ListRequests(e.ctx, a, RequestQuery{State: "all", Kind: RequestPermission})
	if err != nil || len(page.Requests) == 0 {
		t.Fatalf("no permission request: %v", err)
	}
	top := page.Requests[0]
	for _, r := range page.Requests {
		if r.Number > top.Number {
			top = r
		}
	}
	got, err := e.s.GetRequest(e.ctx, a, top.Ref, 0)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A refused call's decision doesn't wake the agent, which has usually moved
// on; it hears at its next prompt and isn't told to retry (WP-ADR114).
func TestPermissionDecisionWaitsForPrompt(t *testing.T) {
	r := newReqEnv(t)
	ag := agentActor(r.ana, "anthropic/claude-code")
	deny(t, r.env, ag, "cli-1", "Bash", `{"command":"wp request show R-9","description":"look"}`)
	req := latestPermission(t, r.env, r.ana)
	if strings.Join(ruleList(req.Rules), "|") != "Bash(wp request show R-9)|Bash(wp request show *)" || req.RuleRepo != "acme/app" {
		t.Fatalf("rules offered: %+v in %q", req.Rules, req.RuleRepo)
	}
	if _, err := r.s.DecideRequest(r.ctx, r.ana, req.Ref, RequestDecision{Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	if res := r.stop(); strings.Contains(res.Text, req.Code) {
		t.Fatalf("the approval woke the agent: %+v", res)
	}
	res, err := r.s.SessionHook(r.ctx, ag, []HookEvent{hook("box", "cli-1", "UserPromptSubmit", func(h *HookEvent) { h.Prompt = "go on" })})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, req.Code) || !strings.Contains(res.Text, "No rule was added") || strings.Contains(strings.ToLower(res.Text), "retry") {
		t.Fatalf("what the agent hears at its next prompt: %q", res.Text)
	}
	if got, _ := r.s.GetRequest(r.ctx, r.ana, req.Ref, 0); got.Rules != nil {
		t.Fatalf("a decided request still offers rules: %+v", got.Rules)
	}
}

func TestPermissionApproveWithRule(t *testing.T) {
	se := newSetupEnv(t)
	ag := agentActor(se.admin, "anthropic/claude-code")
	res, err := se.s.SessionHook(se.ctx, ag, []HookEvent{hook("box", "cli-1", "SessionStart", nil)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := se.s.db.W.Exec(`UPDATE agent_sessions SET project_id = ?, repo = 'acme/one' WHERE id = ?`, se.acme.ID, res.Session); err != nil {
		t.Fatal(err)
	}
	deny(t, se.env, ag, "cli-1", "Bash", `{"command":"wp pr show 12"}`)
	req := latestPermission(t, se.env, se.admin)

	// A rule the check refuses, or with another decision, leaves the request open.
	for _, d := range []RequestDecision{{Decision: "approve", Rule: "Bash(wp *)"}, {Decision: "approve", Rule: "Bash(rm -rf *)"}, {Decision: "deny", Rule: "Bash(wp pr show *)"}} {
		if _, err := se.s.DecideRequest(se.ctx, se.admin, req.Ref, d); !IsInvalid(err) {
			t.Fatalf("%+v: %v", d, err)
		}
	}
	if got, _ := se.s.GetRequest(se.ctx, se.admin, req.Ref, 0); got.State != RequestOpen {
		t.Fatalf("a refused rule decided it: %s", got.State)
	}

	got, err := se.s.DecideRequest(se.ctx, se.admin, req.Ref, RequestDecision{Decision: "approve", Rule: "Bash(wp pr show *)"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestApproved || got.Result.Rule != "Bash(wp pr show *)" || got.Result.PRURL == "" || !strings.HasPrefix(got.Result.Branch, "waypoint/allow-") {
		t.Fatalf("approved with a rule: %+v", got.Result)
	}
	prs := se.fake.prsTo("acme/one")
	if len(prs) != 1 || !prs[0].labelled || !strings.Contains(prs[0].body, "wp pr show 12") {
		t.Fatalf("the rule's pull request: %+v", prs)
	}
	settings := se.fake.lastCommit("acme/one").files[settingsPath]
	var v struct {
		Hooks       map[string]any `json:"hooks"`
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(settings), &v); err != nil || len(v.Hooks) != 1 || len(v.Permissions.Allow) != 1 || v.Permissions.Allow[0] != "Bash(wp pr show *)" {
		t.Fatalf("settings written: %s (%v)", settings, err)
	}
	text := DecisionsText([]*Request{got})
	if !strings.Contains(text, "for later sessions") || !strings.Contains(text, got.Result.PRURL) || strings.Contains(strings.ToLower(text), "retry") {
		t.Fatalf("what the agent is told: %q", text)
	}

	// A rule that couldn't be added isn't reported as chosen: Rule stays
	// empty and Reason says why, for the agent and the person (c5448).
	se.fake.failPR = true
	deny(t, se.env, ag, "cli-1", "Bash", `{"command":"wp pr log 14"}`)
	failed := latestPermission(t, se.env, se.admin)
	got, err = se.s.DecideRequest(se.ctx, se.admin, failed.Ref, RequestDecision{Decision: "approve", Rule: "Bash(wp pr log *)"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestApproved || got.Result.Rule != "" || got.Result.PRURL != "" || !strings.Contains(got.Result.Reason, "wasn't added") {
		t.Fatalf("a rule whose PR failed: %+v", got.Result)
	}
	if text := DecisionsText([]*Request{got}); !strings.Contains(text, "wasn't added") || strings.Contains(text, "They chose the rule") {
		t.Fatalf("what the agent is told of a failed rule: %q", text)
	}
	se.fake.failPR = false

	// A session with no repo Waypoint knows can't take a rule.
	if _, err := se.s.db.W.Exec(`UPDATE agent_sessions SET repo = 'elsewhere/x' WHERE id = ?`, res.Session); err != nil {
		t.Fatal(err)
	}
	deny(t, se.env, ag, "cli-1", "Bash", `{"command":"wp pr show 13"}`)
	other := latestPermission(t, se.env, se.admin)
	if _, err := se.s.DecideRequest(se.ctx, se.admin, other.Ref, RequestDecision{Decision: "approve", Rule: "Bash(wp pr show *)"}); !IsInvalid(err) {
		t.Fatalf("a rule for an unknown repo: %v", err)
	}
}

// A rule picked from the refused-calls log (WP-232 + WP-233): the log ranks
// the rules that would have let repeated refusals through, and a person's
// pick is added by pull request; agents can't pick one.
func TestRefusedCallRule(t *testing.T) {
	se := newSetupEnv(t)
	ag := agentActor(se.admin, "anthropic/claude-code")
	refuse := func(cli, cmd string) {
		t.Helper()
		_, err := se.s.SessionHook(se.ctx, ag, []HookEvent{hook("box", cli, "PermissionDenied", func(h *HookEvent) {
			h.Tool, h.ToolInput, h.ToolUseID, h.Reason = "Bash", json.RawMessage(`{"command":"`+cmd+`"}`), fmt.Sprint("tu-", se.clockNow().UnixNano()), "auto mode denied it"
		})})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, cli := range []string{"cli-1", "cli-2"} {
		res, err := se.s.SessionHook(se.ctx, ag, []HookEvent{hook("box", cli, "SessionStart", nil)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := se.s.db.W.Exec(`UPDATE agent_sessions SET project_id = ?, repo = 'acme/one' WHERE id = ?`, se.acme.ID, res.Session); err != nil {
			t.Fatal(err)
		}
	}
	refuse("cli-1", "wp pr show 12")
	refuse("cli-1", "wp pr show 12") // a repeat: the same row
	refuse("cli-2", "wp pr show 13")
	refuse("cli-2", "make a && make b") // chained: no rule covers it
	refuse("cli-2", "make lint")        // once only: not offered at the top

	page, err := se.s.ListRefusals(se.ctx, se.admin, RefusalQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, g := range page.Rules {
		got = append(got, fmt.Sprintf("%s %d/%d/%d", g.Rule, g.Refusals, g.Calls, g.Sessions))
	}
	if strings.Join(got, "|") != "Bash(wp pr show *) 2/3/2|Bash(wp pr show 12) 1/2/1" || page.Rules[0].Repo != "acme/one" || page.Rules[0].Added {
		t.Fatalf("rules offered at the top: %q", got)
	}
	var show12, rm *ToolRefusal
	for _, f := range page.Refusals {
		switch f.Command {
		case "wp pr show 12":
			show12 = f
		case "make a && make b":
			rm = f
		}
	}
	if show12 == nil || strings.Join(ruleList(show12.Rules), "|") != "Bash(wp pr show 12)|Bash(wp pr show *)" || show12.RuleRepo != "acme/one" || rm == nil || len(rm.Rules) != 0 {
		t.Fatalf("a row's rules: %+v / %+v", show12, rm)
	}
	if page.Rules[0].Refusal == show12.ID {
		t.Fatalf("the top rule's example should be the newest call it covers, not %d", show12.ID)
	}

	// Only a person picks a rule, and only one that passes the checks.
	if _, err := se.s.AllowRefusedCall(se.ctx, ag, show12.ID, "Bash(wp pr show *)"); err == nil || !strings.Contains(err.Error(), "a person picks") {
		t.Fatalf("an agent picked a rule: %v", err)
	}
	for _, bad := range []string{"Bash(wp *)", "Bash(rm -rf *)", "WebFetch(domain:example.com)"} {
		if _, err := se.s.AllowRefusedCall(se.ctx, se.admin, show12.ID, bad); !IsInvalid(err) {
			t.Fatalf("%s: %v", bad, err)
		}
	}
	if n := len(se.fake.prsTo("acme/one")); n != 0 {
		t.Fatalf("%d pull requests before a good pick", n)
	}

	f, err := se.s.AllowRefusedCall(se.ctx, se.admin, show12.ID, "Bash(wp pr show *)")
	if err != nil {
		t.Fatal(err)
	}
	if f.Rule != "Bash(wp pr show *)" || f.RulePR == "" || f.RuleReason != "" || len(f.Rules) != 0 {
		t.Fatalf("the picked row: %+v", f)
	}
	prs := se.fake.prsTo("acme/one")
	if len(prs) != 1 || !prs[0].labelled || !strings.Contains(prs[0].body, "wp pr show 12") || !strings.Contains(prs[0].body, "2 times") {
		t.Fatalf("the rule's pull request: %+v", prs)
	}
	var v struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(se.fake.lastCommit("acme/one").files[settingsPath]), &v); err != nil || len(v.Permissions.Allow) != 1 || v.Permissions.Allow[0] != "Bash(wp pr show *)" {
		t.Fatalf("settings written: %+v (%v)", v, err)
	}
	if _, err := se.s.AllowRefusedCall(se.ctx, se.admin, show12.ID, "Bash(wp pr show 12)"); !IsInvalid(err) {
		t.Fatalf("a second rule for the same call: %v", err)
	}
	page, _ = se.s.ListRefusals(se.ctx, se.admin, RefusalQuery{})
	last := page.Rules[len(page.Rules)-1]
	if last.Rule != "Bash(wp pr show *)" || !last.Added || last.PR != f.RulePR {
		t.Fatalf("an added rule goes last, marked: %+v", page.Rules)
	}
	// A rule whose every call has one is no longer offered: its button
	// would pick for a call that already has a rule.
	for _, g := range page.Rules {
		if g.Rule == "Bash(wp pr show 12)" {
			t.Fatalf("offered for a call that has a rule: %+v", g)
		}
	}

	// A pull request that fails leaves no rule, says why, and the call
	// still offers its rules.
	se.fake.failPR = true
	refuse("cli-1", "wp pr log 14")
	page, _ = se.s.ListRefusals(se.ctx, se.admin, RefusalQuery{Limit: 1})
	f, err = se.s.AllowRefusedCall(se.ctx, se.admin, page.Refusals[0].ID, "Bash(wp pr log *)")
	if err != nil {
		t.Fatal(err)
	}
	if f.Rule != "" || f.RulePR != "" || !strings.Contains(f.RuleReason, "wasn't added") || len(f.Rules) == 0 {
		t.Fatalf("a rule whose PR failed: %+v", f)
	}

	// A pick on its way can't be made again (another tab, the row and the
	// suggestion at once) until it is stale; a failed one can.
	se.fake.failPR = false
	if _, err := se.s.db.W.Exec(`UPDATE tool_refusals SET rule_at = ?, rule_reason = '' WHERE id = ?`, se.clockNow().UnixMilli(), f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := se.s.AllowRefusedCall(se.ctx, se.admin, f.ID, "Bash(wp pr log *)"); !IsInvalid(err) || !strings.Contains(err.Error(), "on its way") {
		t.Fatalf("a second pick while the first is on its way: %v", err)
	}
	page, _ = se.s.ListRefusals(se.ctx, se.admin, RefusalQuery{})
	for _, g := range page.Rules {
		if g.Refusal == f.ID {
			t.Fatalf("offered for a call whose pick is on its way: %+v", g)
		}
	}
	se.setClock(se.clockNow().Add(refusalPickStale + time.Minute))
	if f, err = se.s.AllowRefusedCall(se.ctx, se.admin, f.ID, "Bash(wp pr log *)"); err != nil || f.Rule != "Bash(wp pr log *)" || f.RulePR == "" {
		t.Fatalf("a pick again after a stale one: %+v %v", f, err)
	}
}
