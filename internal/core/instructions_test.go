package core

import (
	"errors"
	"strings"
	"testing"
)

func sp(v string) *string { return &v }

// instr adds an instruction as a, failing the test on an error.
func (e *env) instr(a *Actor, in InstructionInput) *Instruction {
	e.t.Helper()
	x, err := e.s.CreateInstruction(e.ctx, a, in)
	if err != nil {
		e.t.Fatal(err)
	}
	return x
}

// hookIn answers a hook event of session cli, a session of project p (as
// one in a repo attached to p is).
func (e *env) hookIn(a *Actor, p *Project, cli, event string, mod func(*HookEvent)) *HookResult {
	e.t.Helper()
	if _, err := e.s.SessionHook(e.ctx, a, []HookEvent{hook("box", cli, "Notification", nil)}); err != nil {
		e.t.Fatal(err)
	}
	e.exec(`UPDATE agent_sessions SET project_id = ? WHERE cli_id = ?`, p.ID, cli)
	res, err := e.s.SessionHook(e.ctx, a, []HookEvent{hook("box", cli, event, mod)})
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

func (e *env) deliveries(id int64) (out []string) {
	e.t.Helper()
	rows, err := e.s.db.R.QueryContext(e.ctx, `SELECT event || ':' || source || ':' || agent_type || ':' || coalesce(model, 'NULL') || ':' || outcome
		FROM agent_instruction_deliveries WHERE instruction_id = ? ORDER BY id`, id)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestInstructionForAudience(t *testing.T) {
	mk := func(scope, moment string, models, types []string) *Instruction {
		return &Instruction{Scope: scope, Moment: moment, Models: models, AgentTypes: types}
	}
	main := InstructionAudience{Scope: InstrScopeMain, Model: "claude-fable-5-2"}
	haiku := InstructionAudience{Scope: InstrScopeSubagent, Model: "claude-haiku-5-5", AgentType: "haiku-worker"}
	for _, c := range []struct {
		name         string
		x            *Instruction
		a            InstructionAudience
		yes, unknown bool
	}{
		{"any to main", mk("any", "start", nil, nil), main, true, false},
		{"main only, to a sub-agent", mk("main", "start", nil, nil), haiku, false, false},
		{"model named, matches", mk("main", "start", []string{"Fable", "opus"}, nil), main, true, false},
		{"model named, other model", mk("any", "start", []string{"fable", "opus"}, nil), haiku, false, false},
		{"model named, unknown model", mk("subagent", "start", []string{"sonnet"}, nil), InstructionAudience{Scope: InstrScopeSubagent, AgentType: "x"}, false, true},
		{"unknown model, wrong type: not for it at all", mk("subagent", "start", []string{"sonnet"}, []string{"y"}), InstructionAudience{Scope: InstrScopeSubagent, AgentType: "x"}, false, false},
		{"type glob", mk("subagent", "start", nil, []string{"*-worker"}), haiku, true, false},
		{"type glob, case", mk("subagent", "start", nil, []string{"Haiku-*"}), haiku, true, false},
		{"type left out", mk("subagent", "start", nil, []string{"*-worker", "!haiku-worker"}), haiku, false, false},
		{"only left out: the rest", mk("subagent", "start", nil, []string{"!Explore"}), haiku, true, false},
		{"compact only, at a start", mk("main", "compact", nil, nil), main, false, false},
		{"compact only, after one", mk("main", "compact", nil, nil), InstructionAudience{Scope: InstrScopeMain, Compact: true}, true, false},
	} {
		if yes, unknown := instrFor(c.x, c.a); yes != c.yes || unknown != c.unknown {
			t.Errorf("%s: got %v/%v, want %v/%v", c.name, yes, unknown, c.yes, c.unknown)
		}
	}
}

// A main thread and a sub-agent each get only what is for them, and each
// answer is recorded with what the session was.
func TestInstructionsDeliveredByAudience(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "kanban")
	other := e.project(e.admin, "Beta", "kanban")
	lead := e.instr(e.admin, InstructionInput{Project: p.Key, Title: sp("Delegate"), Body: sp("Hand reading to haiku workers."), Scope: sp("main"), Models: &[]string{"fable", "opus"}})
	worker := e.instr(e.admin, InstructionInput{Project: p.Key, Title: sp("Workers report"), Body: sp("Commit, push and report; don't ship."), Scope: sp("subagent"), AgentTypes: &[]string{"*-worker"}})
	everyone := e.instr(e.admin, InstructionInput{Title: sp("Everywhere"), Body: sp("Ask with wp ask.")})
	elsewhere := e.instr(e.admin, InstructionInput{Project: other.Key, Title: sp("Beta's"), Body: sp("Beta only.")})
	sonnetOnly := e.instr(e.admin, InstructionInput{Project: p.Key, Title: sp("Sonnet"), Body: sp("Fan out to haiku."), Scope: sp("subagent"), Models: &[]string{"sonnet"}})
	agent := agentActor(e.admin, "anthropic/claude-fable-5-2")

	res := e.hookIn(agent, p, "s1", "SessionStart", func(ev *HookEvent) { ev.Model, ev.Source = "claude-fable-5-2", "startup" })
	if want := "Ask with wp ask.\n\nHand reading to haiku workers."; res.Instructions != want {
		t.Fatalf("main thread got %q, want %q", res.Instructions, want)
	}
	res = e.hookIn(agent, p, "s1", "SubagentStart", func(ev *HookEvent) {
		ev.AgentID, ev.AgentType, ev.AgentModel = "a1", "haiku-worker", "claude-haiku-5-5"
	})
	if want := "Ask with wp ask.\n\nCommit, push and report; don't ship."; res.Instructions != want {
		t.Fatalf("haiku worker got %q, want %q", res.Instructions, want)
	}
	// A sub-agent whose model wasn't known: no guess, and the miss is counted.
	res = e.hookIn(agent, p, "s1", "SubagentStart", func(ev *HookEvent) { ev.AgentID, ev.AgentType = "a2", "Explore" })
	if want := "Ask with wp ask."; res.Instructions != want {
		t.Fatalf("Explore got %q, want %q", res.Instructions, want)
	}
	for id, want := range map[int64][]string{
		lead.ID:       {"SessionStart:startup::claude-fable-5-2:sent"},
		worker.ID:     {"SubagentStart::haiku-worker:claude-haiku-5-5:sent"},
		everyone.ID:   {"SessionStart:startup::claude-fable-5-2:sent", "SubagentStart::haiku-worker:claude-haiku-5-5:sent", "SubagentStart::Explore:NULL:sent"},
		elsewhere.ID:  nil,
		sonnetOnly.ID: {"SubagentStart::Explore:NULL:unknown_model"},
	} {
		if got := e.deliveries(id); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("instruction %d: recorded %v, want %v", id, got, want)
		}
	}
	got, err := e.s.GetInstruction(e.ctx, e.admin, everyone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s := got.Sent; s.Sent != 3 || s.Sessions != 1 || s.Subagents != 2 || s.Last == nil {
		t.Errorf("sent summary: %+v", s)
	}
}

// After a compaction the start instructions are sent again with the ones only
// for that moment; a resume sends only what the session didn't have.
func TestInstructionsAfterCompactionAndResume(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "kanban")
	start := e.instr(e.admin, InstructionInput{Project: p.Key, Title: sp("Start"), Body: sp("Always.")})
	after := e.instr(e.admin, InstructionInput{Project: p.Key, Title: sp("After"), Body: sp("Read the task again with wp next."), Scope: sp("main"), Moment: sp("compact")})
	agent := agentActor(e.admin, "anthropic/claude-opus-5-5")
	at := func(source string) string {
		return e.hookIn(agent, p, "s1", "SessionStart", func(ev *HookEvent) { ev.Model, ev.Source = "claude-opus-5-5", source }).Instructions
	}
	if got := at("startup"); got != "Always." {
		t.Fatalf("startup: %q", got)
	}
	if got := at("resume"); got != "" {
		t.Fatalf("resume with nothing new: %q", got)
	}
	if got := at("compact"); got != "Always.\n\nRead the task again with wp next." {
		t.Fatalf("compact: %q", got)
	}
	// A person's edit is the next thing a resumed session is told.
	if _, err := e.s.UpdateInstruction(e.ctx, e.admin, start.ID, InstructionInput{Body: sp("Always, reworded.")}); err != nil {
		t.Fatal(err)
	}
	if got := at("resume"); got != "Always, reworded." {
		t.Fatalf("resume after an edit: %q", got)
	}
	if got := e.deliveries(after.ID); len(got) != 1 {
		t.Errorf("compact-only instruction recorded %v", got)
	}
	x, err := e.s.GetInstruction(e.ctx, e.admin, start.ID)
	if err != nil {
		t.Fatal(err)
	}
	if x.Sent.Sent != 3 || x.Sent.Compactions != 1 {
		t.Errorf("sent summary: %+v", x.Sent)
	}
}

// An agent's instruction or edit waits for a person; only approved text is
// delivered, and an agent can't switch one on or off, approve or delete.
func TestInstructionAgentChangesWait(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "kanban")
	agent := agentActor(e.admin, "anthropic/claude-opus-5-5")
	n := 0
	main := func() string {
		n++
		return e.hookIn(agent, p, "fresh-"+strings.Repeat("x", n), "SessionStart", func(ev *HookEvent) { ev.Source = "startup" }).Instructions
	}
	proposed := e.instr(agent, InstructionInput{Project: p.Key, Title: sp("Mine"), Body: sp("Trust me.")})
	if proposed.Rev != 0 || proposed.Pending == nil || proposed.Pending.State != InstrPending {
		t.Fatalf("an agent's instruction: %+v", proposed)
	}
	if got := main(); got != "" {
		t.Fatalf("an unapproved instruction was delivered: %q", got)
	}
	for name, err := range map[string]error{
		"approve": func() error { _, err := e.s.ReviewInstruction(e.ctx, agent, proposed.ID, 0, true); return err }(),
		"switch": func() error {
			on := false
			_, err := e.s.UpdateInstruction(e.ctx, agent, proposed.ID, InstructionInput{Enabled: &on})
			return err
		}(),
		"delete": e.s.DeleteInstruction(e.ctx, agent, proposed.ID),
	} {
		if !errors.Is(err, ErrForbidden) {
			t.Errorf("%s by an agent: %v", name, err)
		}
	}
	x, err := e.s.ReviewInstruction(e.ctx, e.admin, proposed.ID, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if x.Rev != 1 || x.Pending != nil {
		t.Fatalf("approved: %+v", x)
	}
	if got := main(); got != "Trust me." {
		t.Fatalf("approved instruction: %q", got)
	}
	// An agent's edit: the approved text stays until a person decides.
	x, err = e.s.UpdateInstruction(e.ctx, agent, proposed.ID, InstructionInput{Body: sp("Trust me more.")})
	if err != nil {
		t.Fatal(err)
	}
	if x.Body != "Trust me." || x.Pending == nil || x.Pending.Body != "Trust me more." || x.Pending.Title != "Mine" {
		t.Fatalf("pending edit: %+v pending %+v", x, x.Pending)
	}
	if x, err = e.s.ReviewInstruction(e.ctx, e.admin, proposed.ID, 0, false); err != nil || x.Body != "Trust me." || x.Pending != nil {
		t.Fatalf("rejected edit: %+v, %v", x, err)
	}
	// Off, and off for the whole project.
	off := false
	if _, err := e.s.UpdateInstruction(e.ctx, e.admin, proposed.ID, InstructionInput{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if got := main(); got != "" {
		t.Fatalf("an instruction turned off was delivered: %q", got)
	}
}

func TestInstructionProjectOptOutAndBudget(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "kanban")
	big := strings.Repeat("a", instrBodyMax)
	a := e.instr(e.admin, InstructionInput{Project: p.Key, Title: sp("A"), Body: &big, Scope: sp("main")})
	b := e.instr(e.admin, InstructionInput{Project: p.Key, Title: sp("B"), Body: &big, Scope: sp("main")})
	small := e.instr(e.admin, InstructionInput{Title: sp("Small"), Body: sp("Short.")})
	agent := agentActor(e.admin, "anthropic/claude-opus-5-5")
	res := e.hookIn(agent, p, "s1", "SessionStart", func(ev *HookEvent) { ev.Source = "startup" })
	// Every project's come first, then the project's in order; the one that
	// no longer fits is left out whole and counted.
	if want := "Short.\n\n" + big; res.Instructions != want {
		t.Fatalf("got %d characters, want %d", len(res.Instructions), len(want))
	}
	if got := e.deliveries(b.ID); len(got) != 1 || !strings.HasSuffix(got[0], ":cut") {
		t.Errorf("over budget: %v", got)
	}
	if got := e.deliveries(a.ID); len(got) != 1 || !strings.HasSuffix(got[0], ":sent") {
		t.Errorf("within budget: %v", got)
	}
	if _, err := e.s.CreateInstruction(e.ctx, e.admin, InstructionInput{Project: p.Key, Title: sp("Long"), Body: sp(big + "a")}); !IsInvalid(err) {
		t.Errorf("a text over the limit: %v", err)
	}
	set := ProjectSettings{NoAgentInstructions: true}
	if _, err := e.s.UpdateProject(e.ctx, e.admin, p.Key, ProjectInput{Settings: &set}); err != nil {
		t.Fatal(err)
	}
	res = e.hookIn(agent, p, "s2", "SessionStart", func(ev *HookEvent) { ev.Source = "startup" })
	if res.Instructions != "" || len(e.deliveries(small.ID)) != 1 {
		t.Errorf("project turned delivery off: got %q, recorded %v", res.Instructions, e.deliveries(small.ID))
	}
}

func TestInstructionAccess(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "kanban")
	editor, viewer, nobody := e.user("eddie", false), e.user("vera", false), e.user("nobody", false)
	for u, role := range map[*Actor]string{editor: "editor", viewer: "viewer"} {
		if err := e.s.SetMember(e.ctx, e.admin, p.Key, u.User.Username, role); err != nil {
			t.Fatal(err)
		}
	}
	mine := e.instr(editor, InstructionInput{Project: p.Key, Title: sp("Ours"), Body: sp("For Alpha.")})
	if mine.Rev != 1 {
		t.Fatalf("a person's instruction stands as written: %+v", mine)
	}
	if _, err := e.s.CreateInstruction(e.ctx, viewer, InstructionInput{Project: p.Key, Title: sp("No"), Body: sp("No.")}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a viewer wrote one: %v", err)
	}
	if _, err := e.s.CreateInstruction(e.ctx, editor, InstructionInput{Title: sp("Everywhere"), Body: sp("No.")}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a non-admin wrote one for every project: %v", err)
	}
	if _, err := e.s.GetInstruction(e.ctx, nobody, mine.ID); err == nil {
		t.Error("someone outside the project read its instruction")
	}
	if list, err := e.s.ListInstructions(e.ctx, viewer, p.Key); err != nil || len(list) != 1 {
		t.Errorf("a viewer's list: %v, %v", list, err)
	}
	for name, in := range map[string]InstructionInput{
		"types on main":       {Project: p.Key, Title: sp("x"), Body: sp("x"), Scope: sp("main"), AgentTypes: &[]string{"Explore"}},
		"compact on subagent": {Project: p.Key, Title: sp("x"), Body: sp("x"), Scope: sp("subagent"), Moment: sp("compact")},
		"bad glob":            {Project: p.Key, Title: sp("x"), Body: sp("x"), AgentTypes: &[]string{"[a"}},
		"bad scope":           {Project: p.Key, Title: sp("x"), Body: sp("x"), Scope: sp("everyone")},
		"no text":             {Project: p.Key, Title: sp("x")},
	} {
		if _, err := e.s.CreateInstruction(e.ctx, editor, in); !IsInvalid(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if pv, err := e.s.PreviewInstructions(e.ctx, viewer, p.Key, InstructionAudience{Scope: "main"}); err != nil || pv.Text != "For Alpha." || len(pv.Items) != 1 {
		t.Errorf("preview: %+v, %v", pv, err)
	}
	if len(e.deliveries(mine.ID)) != 0 {
		t.Error("a preview was recorded as a delivery")
	}
}
