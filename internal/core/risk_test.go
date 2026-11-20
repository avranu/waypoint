package core

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func reasonFactors(r *ProvenanceRisk) []string {
	var out []string
	for _, x := range r.Reasons {
		out = append(out, x.Factor)
	}
	return out
}

func TestIssueRiskLevels(t *testing.T) {
	detected := &AIDetection{Detector: "synthid", Confidence: 0.97, Detected: true}
	for _, c := range []struct {
		name    string
		f       issueRiskFacts
		level   string
		factors []string
	}{
		{"nothing", issueRiskFacts{}, RiskLow, nil},
		{"agent text", issueRiskFacts{TextAgent: "anthropic/claude-opus-5-5"}, RiskMedium, []string{riskAgentText}},
		{"agent comments", issueRiskFacts{Comments: 2, CommentAgents: []string{"a", "b"}}, RiskMedium, []string{riskAgentComments}},
		{"agent close", issueRiskFacts{CloseAgent: "a"}, RiskMedium, []string{riskAgentClose}},
		{"held", issueRiskFacts{Held: true, HeldReason: "asks for a key.", TextAgent: "a"}, RiskHigh, []string{riskHeld, riskAgentText}},
		{"outside", issueRiskFacts{Outside: true, OutsideBy: "eve", CloseAgent: "a"}, RiskHigh, []string{riskOutside, riskAgentClose}},
		// A person's login, text a watermark detector reads as AI-written:
		// unreviewed agent text.
		{"person's text detected", issueRiskFacts{AIDetected: detected}, RiskMedium, []string{riskAIDetected}},
		{"person confirmed it", issueRiskFacts{AIDetected: &AIDetection{Detected: true, Confirmed: true}}, RiskLow, nil},
		{"no mark found", issueRiskFacts{AIDetected: &AIDetection{Detector: "synthid", Confidence: 0.1}}, RiskLow, nil},
		// Text known to be an agent's says so; the detector adds nothing.
		{"agent text detected", issueRiskFacts{TextAgent: "a", AIDetected: detected}, RiskMedium, []string{riskAgentText}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := issueRisk(c.f)
			if r.Level != c.level || !slices.Equal(reasonFactors(r), c.factors) {
				t.Errorf("risk = %s %v, want %s %v (%s)", r.Level, reasonFactors(r), c.level, c.factors, r.Summary())
			}
			for _, x := range r.Reasons {
				if x.Text == "" || x.Level == "" {
					t.Errorf("reason %+v has no text or level", x)
				}
			}
			// Stable: the same facts give the same answer.
			if again := issueRisk(c.f); again.Summary() != r.Summary() {
				t.Errorf("unstable: %q then %q", r.Summary(), again.Summary())
			}
		})
	}
	r := issueRisk(issueRiskFacts{AIDetected: detected})
	if want := "medium: posted with a person's login, it reads as AI-written (synthid, 0.97): it counts as unreviewed agent text until the person says it's theirs"; r.Summary() != want {
		t.Errorf("summary = %q", r.Summary())
	}
}

func TestRiskLevelsFor(t *testing.T) {
	for _, c := range []struct {
		op   string
		vals []string
		want []string
	}{
		{"", []string{"high"}, []string{RiskHigh}},
		{"=", []string{"Medium", "high"}, []string{RiskMedium, RiskHigh}},
		{"!=", []string{"low"}, []string{RiskMedium, RiskHigh}},
		{">", []string{"low"}, []string{RiskMedium, RiskHigh}},
		{">=", []string{"medium"}, []string{RiskMedium, RiskHigh}},
		{"<", []string{"high"}, []string{RiskLow, RiskMedium}},
		{"<=", []string{"low"}, []string{RiskLow}},
	} {
		got, err := riskLevelsFor(c.op, c.vals)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s%v = %v, %v; want %v", c.op, c.vals, got, err, c.want)
		}
	}
	if _, err := riskLevelsFor("=", []string{"severe"}); !IsInvalid(err) {
		t.Errorf("unknown level: %v", err)
	}
	if _, err := riskLevelsFor("~", []string{"low"}); !IsInvalid(err) {
		t.Errorf("unknown operator: %v", err)
	}
}

func TestDesignAndPRRisk(t *testing.T) {
	agent := "anthropic/claude-opus-5-5"
	for _, c := range []struct {
		name  string
		n     DesignNote
		level string
	}{
		{"a person's", DesignNote{}, RiskLow},
		{"an agent's proposal", DesignNote{CreatedAgent: agent}, RiskMedium},
		{"approved", DesignNote{CreatedAgent: agent, Approved: true}, RiskLow},
		{"requested", DesignNote{CreatedAgent: agent, Requested: &DesignRequest{}}, RiskLow},
	} {
		if r := designRisk(&c.n); r.Level != c.level {
			t.Errorf("design %s = %s", c.name, r.Summary())
		}
	}

	medium := &ProvenanceRisk{Level: RiskMedium, Reasons: []RiskReason{riskReason(riskAgentText, "agent x wrote it")}}
	low := &ProvenanceRisk{Level: RiskLow}
	for _, c := range []struct {
		name    string
		outside bool
		agent   string
		tasks   map[string]*ProvenanceRisk
		order   []string
		level   string
		factors []string
	}{
		{"a person's, no task", false, "", nil, nil, RiskLow, nil},
		{"an agent's, no task", false, agent, nil, nil, RiskMedium, []string{riskNoTask}},
		{"an agent's, reviewed task", false, agent, map[string]*ProvenanceRisk{"WP-1": low}, []string{"WP-1"}, RiskLow, nil},
		{"carries an unreviewed task", false, agent, map[string]*ProvenanceRisk{"WP-1": low, "WP-2": medium}, []string{"WP-1", "WP-2"}, RiskMedium, []string{riskTask}},
		{"a stranger's", true, "", nil, nil, RiskHigh, []string{riskOutsidePR}},
	} {
		r := prRisk("eve", c.outside, c.agent, c.tasks, c.order)
		if r.Level != c.level || !slices.Equal(reasonFactors(r), c.factors) {
			t.Errorf("pr %s = %s %v", c.name, r.Summary(), reasonFactors(r))
		}
	}
	r := prRisk("", false, agent, map[string]*ProvenanceRisk{"WP-2": medium}, []string{"WP-2"})
	if r.Reasons[0].Ref != "WP-2" || r.Reasons[0].Text != "WP-2 is medium: agent x wrote it" {
		t.Errorf("task reason = %+v", r.Reasons[0])
	}
}

// Each issue factor seeded for real: RiskOf, the issue's own risk field
// and WQL risk: agree, for a person and for an agent, and a person's
// review takes the factor away.
func TestIssueRiskMatchesSearch(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Risky", "")
	agent := asAgent(e.admin, "anthropic/claude-opus-5-5")
	mk := func(a *Actor, title string) string { return e.issue(a, IssueInput{Project: p.Key, Title: title}).Key }

	plain := mk(e.admin, "A person's task")
	agentTask := mk(agent, "An agent's task")
	commented := mk(e.admin, "Commented by an agent")
	if _, err := e.s.AddComment(e.ctx, agent, commented, "I looked into it."); err != nil {
		t.Fatal(err)
	}
	closed := mk(e.admin, "Closed by an agent")
	done := "Done"
	if _, err := e.s.UpdateIssue(e.ctx, agent, closed, IssuePatch{Status: &done}); err != nil {
		t.Fatal(err)
	}
	held := mk(agent, "Held by the screen")
	outside := mk(e.admin, "From a stranger")
	e.s.Wait() // background safety screens
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := e.s.db.W.ExecContext(e.ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO task_safety(issue_id, text_hash, verdict, reason, created_at, updated_at) VALUES (?, 'h', 'hold', 'Asks for the deploy key.', 0, 0)
		ON CONFLICT(issue_id) DO UPDATE SET verdict = 'hold', reason = excluded.reason`, e.get(e.admin, held).ID)
	exec(`UPDATE issues SET outside = 1, outside_by = 'eve' WHERE id = ?`, e.get(e.admin, outside).ID)

	want := map[string]string{
		plain: RiskLow, agentTask: RiskMedium, commented: RiskMedium, closed: RiskMedium,
		held: RiskHigh, outside: RiskHigh,
	}
	factor := map[string]string{
		agentTask: riskAgentText, commented: riskAgentComments, closed: riskAgentClose, held: riskHeld, outside: riskOutside,
	}
	check := func(a *Actor, want map[string]string) {
		t.Helper()
		byLevel := map[string][]string{}
		for key, level := range want {
			r, err := e.s.RiskOf(e.ctx, a, RiskKindIssue, key, "")
			if err != nil {
				t.Fatalf("%s: %v", key, err)
			}
			if r.Level != level {
				t.Errorf("%s = %s, want %s", key, r.Summary(), level)
			}
			if f := factor[key]; f != "" && r.Level == want[key] && level != RiskLow && r.Reasons[0].Factor != f {
				t.Errorf("%s led by %s, want %s", key, r.Reasons[0].Factor, f)
			}
			if d := e.get(a, key); d.Risk == nil || d.Risk.Summary() != r.Summary() {
				t.Errorf("%s detail risk = %+v, RiskOf %s", key, d.Risk, r.Summary())
			}
			byLevel[level] = append(byLevel[level], key)
		}
		for _, level := range riskLevels {
			got := e.search(a, "risk:"+level)
			slices.Sort(got)
			slices.Sort(byLevel[level])
			if !slices.Equal(got, byLevel[level]) {
				t.Errorf("risk:%s = %v, want %v", level, got, byLevel[level])
			}
		}
		got, wantAtLeast := e.search(a, "risk:>=medium"), append(slices.Clone(byLevel[RiskMedium]), byLevel[RiskHigh]...)
		slices.Sort(got)
		slices.Sort(wantAtLeast)
		if !slices.Equal(got, wantAtLeast) {
			t.Errorf("risk:>=medium = %v, want %v", got, wantAtLeast)
		}
	}
	check(e.admin, want)

	// An agent never reads a stranger's text, so it isn't a factor for one;
	// the rest read the same.
	agentWant := map[string]string{}
	for k, v := range want {
		agentWant[k] = v
	}
	agentWant[outside] = RiskLow
	check(agent, agentWant)

	// A person's review clears what it covers.
	for _, key := range []string{agentTask, commented, closed, held} {
		if _, err := e.s.Review(e.ctx, e.admin, ReviewIssue, key, ""); err != nil {
			t.Fatal(err)
		}
		want[key] = RiskLow
	}
	check(e.admin, want)
}

func TestRiskOfOtherKinds(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Kinds", "")
	agent := asAgent(e.admin, "anthropic/claude-opus-5-5")
	level := func(kind, ref string) string {
		t.Helper()
		r, err := e.s.RiskOf(e.ctx, e.admin, kind, ref, p.Key)
		if err != nil {
			t.Fatalf("%s %s: %v", kind, ref, err)
		}
		return r.Level
	}
	review := func(kind, ref string) {
		t.Helper()
		if _, err := e.s.Review(e.ctx, e.admin, kind, ref, p.Key); err != nil {
			t.Fatal(err)
		}
	}

	task := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Somewhere to talk"})
	c, err := e.s.AddComment(e.ctx, agent, task.Key, "An agent's note.")
	if err != nil {
		t.Fatal(err)
	}
	ref := itoa64(c.ID)
	if l := level(RiskKindComment, ref); l != RiskMedium {
		t.Errorf("agent comment = %s", l)
	}
	review(ReviewComment, ref)
	if l := level(RiskKindComment, ref); l != RiskLow {
		t.Errorf("reviewed comment = %s", l)
	}

	pg, err := e.s.CreatePage(e.ctx, agent, PageInput{Title: str("Agent notes"), Project: Opt[string]{Set: true, Value: &p.Key}})
	if err != nil {
		t.Fatal(err)
	}
	if l := level(RiskKindPage, itoa64(pg.ID)); l != RiskMedium {
		t.Errorf("agent page = %s", l)
	}
	review(ReviewPage, itoa64(pg.ID))
	if l := level(RiskKindPage, itoa64(pg.ID)); l != RiskLow {
		t.Errorf("reviewed page = %s", l)
	}

	g, err := e.s.CreateGlossary(e.ctx, agent, GlossaryInput{Term: str("Lease"), Definition: str("A claim with a timer."), Project: str(p.Key)})
	if err != nil {
		t.Fatal(err)
	}
	if l := level(RiskKindGlossary, itoa64(g.ID)); l != RiskMedium {
		t.Errorf("agent glossary = %s", l)
	}
	review(ReviewGlossary, itoa64(g.ID))
	if l := level(RiskKindGlossary, itoa64(g.ID)); l != RiskLow {
		t.Errorf("reviewed glossary = %s", l)
	}

	proposal, err := e.s.CreateDesign(e.ctx, agent, DesignInput{Title: str("An idea"), Project: str(p.Key)})
	if err != nil {
		t.Fatal(err)
	}
	asked, err := e.s.CreateDesign(e.ctx, agent, DesignInput{Title: str("Asked for"), Project: str(p.Key), For: task.Key})
	if err != nil {
		t.Fatal(err)
	}
	if l := level(RiskKindDesign, itoa64(proposal.ID)); l != RiskMedium {
		t.Errorf("proposal = %s", l)
	}
	if l := level(RiskKindDesign, itoa64(asked.ID)); l != RiskLow {
		t.Errorf("requested note = %s", l)
	}
	if _, err := e.s.ApproveDesign(e.ctx, e.admin, proposal.ID, ""); err != nil {
		t.Fatal(err)
	}
	if l := level(RiskKindDesign, itoa64(proposal.ID)); l != RiskLow {
		t.Errorf("approved proposal = %s", l)
	}

	if _, err := e.s.RiskOf(e.ctx, e.admin, "galaxy", "1", ""); !IsInvalid(err) {
		t.Errorf("unknown kind: %v", err)
	}
	if _, err := e.s.RiskOf(e.ctx, e.admin, RiskKindComment, "99999", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing comment: %v", err)
	}
}

func TestPullRisk(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Pulls", "")
	agent := asAgent(e.admin, "anthropic/claude-opus-5-5")
	f := e.forge("forgejo", "https://git.example.com", false)
	exec := func(q string, args ...any) int64 {
		t.Helper()
		res, err := e.s.db.W.ExecContext(e.ctx, q, args...)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	exec(`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'acme/app', 0)`, p.ID, f.ID)
	pr := func(n int, agentID string, outside bool) string {
		return itoa64(exec(`INSERT INTO pull_requests(forge_id, repo, number, url, state, agent, outside, forge_author, created_at, updated_at)
			VALUES (?, 'acme/app', ?, 'https://git.example.com/acme/app/pulls/' || ?, 'open', ?, ?, 'eve', 0, 0)`, f.ID, n, n, agentID, outside))
	}
	level := func(a *Actor, ref string) (*ProvenanceRisk, error) {
		return e.s.RiskOf(e.ctx, a, RiskKindPull, ref, "")
	}

	loose := pr(1, agent.Agent, false)
	if r, err := level(e.admin, loose); err != nil || r.Level != RiskMedium || r.Reasons[0].Factor != riskNoTask {
		t.Errorf("an agent's PR for no task = %+v, %v", r, err)
	}

	task := e.issue(agent, IssueInput{Project: p.Key, Title: "Agent-filed work"})
	tasked := pr(2, agent.Agent, false)
	if _, err := e.s.AddLink(e.ctx, e.admin, task.Key, "https://git.example.com/acme/app/pulls/2", ""); err != nil {
		t.Fatal(err)
	}
	r, err := level(e.admin, tasked)
	if err != nil || r.Level != RiskMedium || r.Reasons[0].Factor != riskTask || r.Reasons[0].Ref != task.Key {
		t.Errorf("a PR for an unreviewed task = %+v, %v", r, err)
	}
	if _, err := e.s.Review(e.ctx, e.admin, ReviewIssue, task.Key, ""); err != nil {
		t.Fatal(err)
	}
	if r, err := level(e.admin, tasked); err != nil || r.Level != RiskLow {
		t.Errorf("a PR for a reviewed task = %+v, %v", r, err)
	}

	stranger := pr(3, "", true)
	if r, err := level(e.admin, stranger); err != nil || r.Level != RiskHigh || r.Reasons[0].Factor != riskOutsidePR {
		t.Errorf("a stranger's PR = %+v, %v", r, err)
	}
	if _, err := level(agent, stranger); !errors.Is(err, ErrNotFound) {
		t.Errorf("an agent read a stranger's PR: %v", err)
	}
}

// Prompts Waypoint queues for a session (WP-560): a person's own follow-up
// is low; an automation rule an agent wrote makes what it sends medium,
// and the session reads who wrote it, until a person saves a rule of
// their own.
func TestPromptAndAutomationRisk(t *testing.T) {
	f := newFUEnv(t)
	p := f.project(f.admin, "Prompts", "")
	agent := asAgent(f.admin, "anthropic/claude-opus-5-5")
	level := func(kind string, id int64) *ProvenanceRisk {
		t.Helper()
		r, err := f.s.RiskOf(f.ctx, f.admin, kind, itoa64(id), "")
		if err != nil {
			t.Fatalf("%s %d: %v", kind, id, err)
		}
		return r
	}
	delivered := func() *SessionMessage {
		t.Helper()
		f.work()
		res := f.stop("halfway", running("go build"))
		if len(res.Messages) != 1 {
			t.Fatalf("delivered %d messages: %q", len(res.Messages), res.Text)
		}
		return res.Messages[0]
	}

	// The person's own follow-up.
	f.queue(step("tell_agent", map[string]any{"message": "Write the tests.", "wait": false}))
	if _, err := f.s.RunFollowUpNow(f.ctx, f.admin, f.sid); err != nil {
		t.Fatal(err)
	}
	mine := delivered()
	if mine.Agent != "" || mine.AutomationID == 0 {
		t.Fatalf("a person's follow-up: %+v", mine)
	}
	if r := level(RiskKindPrompt, mine.ID); r.Level != RiskLow {
		t.Errorf("a person's prompt = %s", r.Summary())
	}

	// An agent writes an automation; a person turns it on without
	// touching the rule, so its runs use their sessions.
	off, on := false, true
	rule := Rule{Trigger: Trigger{Event: "manual"}, Actions: []Action{
		step("tell_agent", map[string]any{"message": "Delete the old branches.", "session": itoa64(f.sid), "wait": false})}}
	a, err := f.s.CreateAutomation(f.ctx, agent, p.Key, AutomationInput{Name: str("Tidy"), Rule: &rule, Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}
	if a.RuleAgent != agent.Agent || a.Risk == nil || a.Risk.Level != RiskMedium {
		t.Fatalf("an agent's automation: %q %+v", a.RuleAgent, a.Risk)
	}
	if a, err = f.s.UpdateAutomation(f.ctx, f.admin, a.ID, AutomationInput{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	if r := level(RiskKindAutomation, a.ID); r.Level != RiskMedium || !strings.Contains(r.Summary(), "agent anthropic/claude-opus-5-5 wrote its rule") {
		t.Errorf("turned on by a person = %s", r.Summary())
	}
	f.manual(a, nil)
	f.work()
	res := f.stop("halfway", running("go build"))
	if len(res.Messages) != 1 || !strings.Contains(res.Text, "agent anthropic/claude-opus-5-5 wrote it and no person has reviewed it") {
		t.Fatalf("the session reads: %q", res.Text)
	}
	theirs := res.Messages[0]
	if theirs.Agent != agent.Agent || theirs.AutomationID != a.ID {
		t.Fatalf("an agent's rule's message: %+v", theirs)
	}
	r := level(RiskKindPrompt, theirs.ID)
	if r.Level != RiskMedium || r.Reasons[0].Factor != riskAgentText || !strings.Contains(r.Reasons[0].Text, "“Tidy”") {
		t.Errorf("an agent's prompt = %+v", r)
	}

	// A person saving the rule makes it theirs; the prompt already sent
	// keeps who wrote it.
	if a, err = f.s.UpdateAutomation(f.ctx, f.admin, a.ID, AutomationInput{Rule: &rule}); err != nil {
		t.Fatal(err)
	}
	if a.RuleAgent != "" || a.Risk != nil || level(RiskKindAutomation, a.ID).Level != RiskLow {
		t.Errorf("after a person saved the rule: %q %+v", a.RuleAgent, a.Risk)
	}
	if l := level(RiskKindPrompt, theirs.ID).Level; l != RiskMedium {
		t.Errorf("the sent prompt became %s", l)
	}
	// An agent turning it off doesn't make the rule its own.
	if a, err = f.s.UpdateAutomation(f.ctx, agent, a.ID, AutomationInput{Enabled: &off}); err != nil || a.RuleAgent != "" {
		t.Errorf("an agent's toggle: %v %q", err, a.RuleAgent)
	}

	// Someone else sees neither the prompt nor the automation.
	other := f.user("rory", false)
	if _, err := f.s.RiskOf(f.ctx, other, RiskKindPrompt, itoa64(theirs.ID), ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("another person's prompt: %v", err)
	}
	if _, err := f.s.RiskOf(f.ctx, other, RiskKindAutomation, itoa64(a.ID), ""); err == nil {
		t.Error("another project's automation: no error")
	}
	if _, err := f.s.RiskOf(f.ctx, f.admin, RiskKindPrompt, "99999", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing prompt: %v", err)
	}
}
