package core

import (
	"slices"
	"testing"
)

func TestCheckAgentSpecStates(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "anyone": "", "Anyone": "", " ": "",
		"none": "none", "None": "none", "humans": "none", "human": "none", "Humans only": "none",
		"any": "any", "ANY": "any",
		"claude": "claude", "haiku 5.5": "haiku 5.5",
	} {
		got, err := checkAgentSpec(in)
		if err != nil || got != want {
			t.Errorf("checkAgentSpec(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if AgentMatches("none", "anthropic/claude-opus-5-5") || AgentMatches("", "anthropic/claude-opus-5-5") {
		t.Error("none and empty must match no agent")
	}
	if issueAgentSpec("") != "any" || issueAgentSpec("none") != "" || issueAgentSpec("claude") != "claude" {
		t.Error("issueAgentSpec maps Anyone to any agent and Humans only to none")
	}
}

func TestIssueAnyoneDefaultAndStates(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Who", "")
	mk := func(actor *Actor, agent string) *IssueDetail {
		return e.issue(actor, IssueInput{Project: p.Key, Title: "t " + agent, Agent: agent})
	}
	def := mk(e.admin, "")
	if def.Agent != "" || def.Delegated() {
		t.Errorf("default agent = %q, want Anyone", def.Agent)
	}
	humans := mk(e.admin, "humans")
	if humans.Agent != "none" {
		t.Errorf("humans = %q, want none", humans.Agent)
	}
	if got := mk(e.admin, "anyone").Agent; got != "" {
		t.Errorf("anyone = %q", got)
	}
	any := mk(e.admin, "any")
	cl := mk(e.admin, "claude")
	if any.Agent != "any" || cl.Agent != "claude" || !any.Delegated() || !cl.Delegated() {
		t.Errorf("agents = %q, %q", any.Agent, cl.Agent)
	}
	ag := agentActor(e.admin, "anthropic/claude-opus-5-5")
	if got := e.issue(ag, IssueInput{Project: p.Key, Title: "mine"}).Agent; got != "anthropic/claude-opus-5-5" {
		t.Errorf("an agent's own issue is for %q", got)
	}
	if got := e.issue(ag, IssueInput{Project: p.Key, Title: "open", Agent: "anyone"}).Agent; got != "" {
		t.Errorf("an agent can file an Anyone issue; got %q", got)
	}

	mine := []string{def.Key, humans.Key, any.Key, cl.Key}
	found := func(q string) []string {
		got := slices.DeleteFunc(e.search(e.admin, "project:"+p.Key+" "+q), func(k string) bool { return !slices.Contains(mine, k) })
		slices.Sort(got)
		return got
	}
	for q, want := range map[string][]string{
		"is:anyone":    {def.Key},
		"agent:anyone": {def.Key},
		"is:human":     {humans.Key},
		"agent:none":   {humans.Key},
		"agent:humans": {humans.Key},
		"agent:any":    {any.Key},
		"agent:claude": {cl.Key},
		"is:agent":     {any.Key, cl.Key},
		"is:delegated": {any.Key, cl.Key},
		"-is:human":    {def.Key, any.Key, cl.Key},
		"agent:no":     {},
	} {
		slices.Sort(want)
		if got := found(q); !slices.Equal(got, want) && len(got)+len(want) > 0 {
			t.Errorf("search %q = %v, want %v", q, got, want)
		}
	}
}

func TestAnyoneInTheQueue(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Anyone", "")
	mk := func(title, agent, prio string) *IssueDetail {
		return e.issue(e.admin, IssueInput{Project: p.Key, Title: title, Agent: agent, Priority: prio})
	}
	anyoneUrgent := mk("Anyone urgent", "", "urgent")
	anyoneLow := mk("Anyone low", "", "low")
	delegatedLow := mk("Delegated low", "claude", "low")
	delegatedHigh := mk("Delegated high", "any", "high")
	humans := mk("Humans only", "none", "urgent")
	anyoneBacklog := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Anyone someday", Status: "backlog"})

	q, err := e.s.AgentQueueFor(e.ctx, e.admin, "anthropic/claude-opus-5-5", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{anyoneUrgent.Key, delegatedHigh.Key, delegatedLow.Key, anyoneLow.Key}
	if got := queueKeys(q.Next); !slices.Equal(got, want) {
		t.Errorf("queue = %v, want %v", got, want)
	}
	for _, it := range q.Waiting {
		if it.Issue.Key == anyoneBacklog.Key || it.Issue.Key == humans.Key {
			t.Errorf("%s waits in the queue; only delegated work lists as waiting", it.Issue.Key)
		}
	}

	tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	opus, _ := e.s.TokenActor(e.ctx, tok)
	var order []string
	for range 5 {
		task, err := e.s.AgentNext(e.ctx, opus, AgentQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if task == nil {
			break
		}
		order = append(order, task.Issue.Key)
		if _, err := e.s.AgentDone(e.ctx, opus, task.Issue.Key, AgentDoneInput{}); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(order, want) {
		t.Errorf("next handed out %v, want %v (never the humans-only or backlog ones)", order, want)
	}
	if d := e.get(e.admin, anyoneUrgent.Key); d.Agent != "" {
		t.Errorf("taking an Anyone task changed who may take it: %q", d.Agent)
	}
}

// Runners that start work on their own take delegated work only.
func TestDrainTakesOnlyDelegated(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Drain", "")
	anyone := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Anyone", Priority: "urgent"})
	delegated := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Delegated", Agent: "claude", Priority: "low"})
	tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	opus, _ := e.s.TokenActor(e.ctx, tok)

	task, err := e.s.AgentNext(e.ctx, opus, AgentQuery{Delegated: true})
	if err != nil || task == nil || task.Issue.Key != delegated.Key {
		t.Fatalf("drain next = %+v, %v", task, err)
	}
	if _, err := e.s.AgentDone(e.ctx, opus, delegated.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	if task, err := e.s.AgentNext(e.ctx, opus, AgentQuery{Delegated: true, Peek: true}); err != nil || task != nil {
		t.Errorf("drain took an Anyone task: %+v, %v", task, err)
	}
	if task, err := e.s.AgentNext(e.ctx, opus, AgentQuery{}); err != nil || task == nil || task.Issue.Key != anyone.Key {
		t.Errorf("next = %+v, %v; want the Anyone task", task, err)
	}
}

func TestAnyoneTakeAndPatch(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Take", "")
	anyone := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Anyone"})
	humans := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Humans", Agent: "none"})
	tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	opus, _ := e.s.TokenActor(e.ctx, tok)

	if task, _ := e.s.AgentNext(e.ctx, opus, AgentQuery{Peek: true, Query: "key:" + humans.Key}); task != nil {
		t.Errorf("next offered a humans-only task: %+v", task)
	}
	if task, err := e.s.AgentTake(e.ctx, opus, humans.Key, ""); err != nil || task.Issue.Agent != "anthropic/claude-opus-5-5" {
		t.Errorf("explicit take of a humans-only task = %+v, %v", task, err)
	}
	if task, err := e.s.AgentTake(e.ctx, opus, anyone.Key, ""); err != nil || task.Issue.Agent != "" {
		t.Errorf("explicit take of an Anyone task = %+v, %v", task, err)
	}

	other := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Other"})
	patch := func(spec string) {
		t.Helper()
		if _, err := e.s.UpdateIssue(e.ctx, e.admin, other.Key, IssuePatch{Agent: &spec}); err != nil {
			t.Fatal(err)
		}
	}
	patch("none")
	if d := e.get(e.admin, other.Key); d.Agent != "none" {
		t.Errorf("patched to humans: %q", d.Agent)
	}
	patch("")
	if d := e.get(e.admin, other.Key); d.Agent != "" {
		t.Errorf("patched back to anyone: %q", d.Agent)
	}
	patch("claude")
	patch("anyone")
	rows := e.audit(e.admin, AuditQuery{})
	var acts []string
	for _, r := range rows.Entries {
		if r.Target == other.Key {
			acts = append(acts, r.Action)
		}
	}
	if !slices.Equal(acts, []string{"undelegate", "delegate"}) {
		t.Errorf("audit for %s = %v; Anyone/Humans moves aren't delegations, claude then anyone is delegate then undelegate", other.Key, acts)
	}

	stillHumans := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Still humans", Agent: "none"})
	if err := e.s.MoveInQueue(e.ctx, e.admin, QueueMove{Key: stillHumans.Key, Top: true}); err == nil {
		t.Error("pinned a humans-only task")
	}
	keep := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Pin me"})
	if err := e.s.MoveInQueue(e.ctx, e.admin, QueueMove{Key: keep.Key, Top: true}); err != nil {
		t.Errorf("pinning an Anyone task: %v", err)
	}
	none := "none"
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, keep.Key, IssuePatch{Agent: &none}); err != nil {
		t.Fatal(err)
	}
	if d := e.get(e.admin, keep.Key); d.AgentOrder != nil {
		t.Error("a task made humans-only kept its pin")
	}
}
