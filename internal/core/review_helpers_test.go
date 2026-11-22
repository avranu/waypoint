package core

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// Haiku helpers around an audit's model (WP-1320, WP-ADR135).

func hasText(s, format string, args ...any) bool {
	return strings.Contains(s, fmt.Sprintf(format, args...))
}

// helperFixture is a service over a fresh database, with the rows the
// helpers' queries read inserted straight, foreign keys off (a round's run,
// session and issue are not what these tests are about).
type helperFixture struct {
	*env
}

func newHelperFixture(t *testing.T) *helperFixture {
	t.Helper()
	fx := &helperFixture{newEnv(t)}
	// db.W is a single connection, so this holds for the whole test.
	fx.exec(`PRAGMA foreign_keys = OFF`)
	return fx
}

func (fx *helperFixture) exec(q string, args ...any) int64 {
	fx.t.Helper()
	res, err := fx.s.db.W.ExecContext(fx.ctx, q, args...)
	if err != nil {
		fx.t.Fatalf("%s: %v", q, err)
	}
	id, _ := res.LastInsertId()
	return id
}

// audit inserts a done audit with the request key given.
func (fx *helperFixture) audit(key string) int64 {
	fx.t.Helper()
	return fx.exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, owner_id, created_at, request_key, status) VALUES (0, 0, '', 'repo', 0, 1, ?, 'done')`, key)
}

// finding inserts a finding of the audit.
func (fx *helperFixture) finding(audit int64, n int, sev, state, verdict string) int64 {
	fx.t.Helper()
	return fx.exec(`INSERT INTO code_audit_findings(audit_id, ordinal, title, path, line, severity, problem, state, verdict) VALUES (?, ?, ?, 'a.go', 1, ?, 'It is wrong.', ?, ?)`,
		audit, n, fmt.Sprintf("Finding %d", n), sev, state, verdict)
}

// round inserts a review round of the run (0: no run) and returns its id.
func (fx *helperFixture) round(run int64) int64 {
	fx.t.Helper()
	return fx.roundKind("review", run)
}

func (fx *helperFixture) roundKind(kind string, run int64) int64 {
	fx.t.Helper()
	var r any
	if run != 0 {
		r = run
	}
	return fx.exec(`INSERT INTO work_log(issue_id, kind, created_at, updated_at, run_id) VALUES (0, ?, 1, 1, ?)`, kind, r)
}

// helperRun records a helper's run; state "unanswered" is a done run of
// an attempt whose reviewer didn't answer.
func (fx *helperFixture) helperRun(audit int64, check, state string) {
	fx.t.Helper()
	answered := state != "unanswered"
	if !answered {
		state = "done"
	}
	fx.exec(`INSERT INTO audit_helper_runs(audit_id, check_name, state, answered, created_at) VALUES (?, ?, ?, ?, 1)`, audit, check, state, answered)
}

// pastRound is an earlier (or later) round of a run, with its audit and the
// helper runs of that audit.
type pastRound struct {
	run    int64
	kind   string // "" review
	suffix string // the audit's request key's end: "", ":sonnet", ":other", ":check"
	runs   [][2]string
}

func (fx *helperFixture) past(p pastRound) {
	fx.t.Helper()
	kind := p.kind
	if kind == "" {
		kind = "review"
	}
	id := fx.roundKind(kind, p.run)
	a := fx.audit(fmt.Sprintf("review-round:%d%s", id, p.suffix))
	for _, r := range p.runs {
		fx.helperRun(a, r[0], r[1])
	}
}

// onceChecks are the checks that run once in a run.
func onceChecks() []string {
	var out []string
	for _, c := range panelChecks {
		if !c.every {
			out = append(out, c.name)
		}
	}
	return out
}

func done(checks ...string) [][2]string {
	var out [][2]string
	for _, c := range checks {
		out = append(out, [2]string{c, "done"})
	}
	return out
}

func TestPanelChecksShape(t *testing.T) {
	// The tests below rest on these: some checks run once, some every round,
	// and every name is a lead id's prefix.
	if len(onceChecks()) < 2 || len(onceChecks()) >= len(panelChecks) {
		t.Fatalf("once checks %v of %d", onceChecks(), len(panelChecks))
	}
	seen := map[string]bool{}
	for _, c := range panelChecks {
		if seen[c.name] || c.ask == "" || cleanLead(c.name+".1") != c.name+".1" {
			t.Errorf("check %q is repeated, has no ask, or isn't a lead prefix", c.name)
		}
		seen[c.name] = true
	}
	if seen[helperVerify] {
		t.Errorf("a panel check is named %q, the check of findings", helperVerify)
	}
}

func TestHelpersRanInRun(t *testing.T) {
	once := onceChecks()
	allButOne := once[1:]
	for _, tc := range []struct {
		name   string
		noRun  bool // the reviewed round has no run
		before []pastRound
		after  []pastRound // rounds made after the reviewed one
		want   bool
	}{
		{"an earlier round ran every once-check", false, []pastRound{{run: 5, runs: done(once...)}}, nil, true},
		{"checks spread over two earlier rounds", false, []pastRound{{run: 5, runs: done(once[0])}, {run: 5, runs: done(once[1:]...)}}, nil, true},
		{"the :sonnet audit counts", false, []pastRound{{run: 5, suffix: ":sonnet", runs: done(once...)}}, nil, true},
		{"the :other audit counts", false, []pastRound{{run: 5, suffix: ":other", runs: done(once...)}}, nil, true},
		{"every-round checks alongside don't matter", false, []pastRound{{run: 5, runs: append(done(once...), done("quality", "tests")...)}}, nil, true},
		{"one once-check missing", false, []pastRound{{run: 5, runs: done(allButOne...)}}, nil, false},
		{"only every-round checks ran", false, []pastRound{{run: 5, runs: done("quality", "tests", "claims")}}, nil, false},
		{"a failed run doesn't count", false, []pastRound{{run: 5, runs: append(done(allButOne...), [2]string{once[0], "failed"})}}, nil, false},
		{"a skipped run doesn't count", false, []pastRound{{run: 5, runs: append(done(allButOne...), [2]string{once[0], "skipped"})}}, nil, false},
		{"a run whose reviewer didn't answer doesn't count", false, []pastRound{{run: 5, runs: append(done(allButOne...), [2]string{once[0], "unanswered"})}}, nil, false},
		{"the check of findings isn't a once-check", false, []pastRound{{run: 5, runs: append(done(allButOne...), [2]string{helperVerify, "done"})}}, nil, false},
		{"another run's round doesn't count", false, []pastRound{{run: 6, runs: done(once...)}}, nil, false},
		{"a round with no run doesn't count", false, []pastRound{{run: 0, runs: done(once...)}}, nil, false},
		{"a later round doesn't count", false, nil, []pastRound{{run: 5, runs: done(once...)}}, false},
		{"not a review round", false, []pastRound{{run: 5, kind: "ship", runs: done(once...)}}, nil, false},
		{"the round's check of marks audit isn't a round's audit", false, []pastRound{{run: 5, suffix: ":check", runs: done(once...)}}, nil, false},
		{"no earlier round at all", false, nil, nil, false},
		{"a round with no run is its own run", true, []pastRound{{run: 5, runs: done(once...)}}, nil, false},
	} {
		fx := newHelperFixture(t)
		for _, p := range tc.before {
			fx.past(p)
		}
		var run int64 = 5
		if tc.noRun {
			run = 0
		}
		reviewed := fx.round(run)
		for _, p := range tc.after {
			fx.past(p)
		}
		row := &roundRow{id: reviewed}
		if run != 0 {
			row.runID.Int64, row.runID.Valid = run, true
		}
		got, err := fx.s.helpersRanInRun(fx.ctx, row)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: helpersRanInRun = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRoundHelpers(t *testing.T) {
	every := func() []string {
		var out []string
		for _, c := range panelChecks {
			if c.every {
				out = append(out, c.name)
			}
		}
		return out
	}()
	all := func() []string {
		var out []string
		for _, c := range panelChecks {
			out = append(out, c.name)
		}
		return out
	}()
	for _, tc := range []struct {
		name string
		ran  bool
		want []string
	}{
		{"first round of a run: every check", false, all},
		{"a later round: the every-round checks only", true, every},
	} {
		fx := newHelperFixture(t)
		var run int64 = 9
		if tc.ran {
			fx.past(pastRound{run: run, runs: done(onceChecks()...)})
		}
		reviewed := fx.round(run)
		row := &roundRow{id: reviewed}
		row.runID.Int64, row.runID.Valid = run, true
		h, err := fx.s.roundHelpers(fx.ctx, row, "The task: lose nothing.\n\n")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var names []string
		for _, p := range h.Panel {
			names = append(names, p.Name)
			if !strings.HasSuffix(p.Ask, panelRules) || len(p.Ask) <= len(panelRules) {
				t.Errorf("%s: check %s's ask doesn't end with the panel rules", tc.name, p.Name)
			}
		}
		// The order is panelOrder's (TestPanelOrder): which checks run is
		// this test's.
		if !slices.Equal(slices.Sorted(slices.Values(names)), slices.Sorted(slices.Values(tc.want))) {
			t.Errorf("%s: panel = %v, want %v", tc.name, names, tc.want)
		}
		if !h.Verify || h.CLI != helperCLI || h.Model != helperModel {
			t.Errorf("%s: %+v", tc.name, h)
		}
		if !strings.Contains(h.Context, "The task: lose nothing.") {
			t.Errorf("%s: the round's context isn't in %q", tc.name, h.Context)
		}
	}

	// A round with no run is its own run: every check, even with others
	// done in an unrelated run.
	fx := newHelperFixture(t)
	fx.past(pastRound{run: 3, runs: done(onceChecks()...)})
	row := &roundRow{id: fx.round(0)}
	h, err := fx.s.roundHelpers(fx.ctx, row, "")
	if err != nil || len(h.Panel) != len(panelChecks) {
		t.Errorf("round with no run: %d checks (want %d), %v", len(h.Panel), len(panelChecks), err)
	}
}

func TestAuditHelpersFor(t *testing.T) {
	panel := []PanelCheck{{Name: "goals", Ask: "ask"}}
	for _, purpose := range []string{AuditCheck, AuditFootprint, "made-up"} {
		if h := auditHelpersFor(AuditInput{purpose: purpose, helpers: &AuditHelpers{Verify: true}}, "PR"); h != nil {
			t.Errorf("purpose %q got helpers %+v", purpose, h)
		}
	}

	// Nothing set: the findings are checked, nothing else.
	for _, purpose := range []string{"", AuditReview} {
		h := auditHelpersFor(AuditInput{purpose: purpose}, "The PR's text")
		if h == nil || !h.Verify || len(h.Panel) != 0 || h.Context != "" || h.CLI != helperCLI || h.Model != helperModel {
			t.Errorf("purpose %q default = %+v", purpose, h)
		}
	}

	// A panel gets the pull request's text appended to its context.
	in := AuditInput{purpose: AuditReview, helpers: &AuditHelpers{Panel: panel, Context: "round context", CLI: "other", Model: "opus"}}
	h := auditHelpersFor(in, "PR text")
	if h.Context != "round context\n\nPR text" {
		t.Errorf("context = %q", h.Context)
	}
	if h.CLI != helperCLI || h.Model != helperModel {
		t.Errorf("cli/model = %s/%s, want the helpers' own", h.CLI, h.Model)
	}
	if h.Verify {
		t.Errorf("Verify set on a round's helpers that didn't ask for it")
	}
	// The caller's helpers are untouched.
	if h == in.helpers || in.helpers.Context != "round context" || in.helpers.CLI != "other" || in.helpers.Model != "opus" {
		t.Errorf("input mutated or shared: %+v", in.helpers)
	}

	for _, tc := range []struct {
		name    string
		helpers AuditHelpers
		pr      string
		want    string
	}{
		{"panel, no context yet", AuditHelpers{Panel: panel}, "PR text", "PR text"},
		{"panel, no PR text", AuditHelpers{Panel: panel, Context: "ctx"}, "", "ctx"},
		{"no panel: the PR's text isn't added", AuditHelpers{Verify: true, Context: "ctx"}, "PR text", "ctx"},
		{"no panel, no context", AuditHelpers{Verify: true}, "PR text", ""},
	} {
		hs := tc.helpers
		if got := auditHelpersFor(AuditInput{helpers: &hs}, tc.pr).Context; got != tc.want {
			t.Errorf("%s: context = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestHelpersJSONAndExtraTimeout(t *testing.T) {
	if helpersJSON(nil) != "" {
		t.Errorf("no helpers should keep ''")
	}
	var back AuditHelpers
	if err := json.Unmarshal([]byte(helpersJSON(&AuditHelpers{Panel: []PanelCheck{{Name: "goals", Ask: "a"}}, Verify: true, CLI: "claude", Model: "haiku"})), &back); err != nil ||
		len(back.Panel) != 1 || back.Panel[0].Name != "goals" || !back.Verify || back.Model != "haiku" {
		t.Errorf("round trip = %+v, %v", back, err)
	}

	panel := []PanelCheck{{Name: "goals"}}
	for _, tc := range []struct {
		name string
		h    *AuditHelpers
		want int
	}{
		{"nil", nil, 0},
		{"empty", &AuditHelpers{}, 0},
		{"context only", &AuditHelpers{Context: "x"}, 0},
		{"verify", &AuditHelpers{Verify: true}, 570},
		{"panel", &AuditHelpers{Panel: panel}, 930},
		{"panel and verify", &AuditHelpers{Panel: panel, Verify: true}, helpersMaxExtra},
	} {
		if got := tc.h.extraTimeout(); got != tc.want {
			t.Errorf("%s: extraTimeout = %d, want %d", tc.name, got, tc.want)
		}
	}
	if helpersMaxExtra != 1500 {
		t.Errorf("helpersMaxExtra = %d", helpersMaxExtra)
	}
}

func TestCleanVerdict(t *testing.T) {
	for in, want := range map[string]string{
		"confirmed": "confirmed", "refuted": "refuted", "unsupported": "unsupported",
		" Refuted\n": "refuted", "CONFIRMED": "confirmed",
		"": "", "maybe": "", "true": "", "refuted.": "", "not refuted": "", "confirm": "",
	} {
		if got := cleanVerdict(in); got != want {
			t.Errorf("cleanVerdict(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanLead(t *testing.T) {
	for in, want := range map[string]string{
		"goals.1": "goals.1", "Goals.1": "goals.1", " goals.1 ": "goals.1", "`goals.1`": "goals.1",
		"[goals.1]": "goals.1", `"goals.1"`: "goals.1", "'quality.12'": "quality.12", "(tests.3)": "tests.3",
		"": "", "goals": "", "goals.": "", "goals.1234": "", "goals.123": "goals.123", "1.1": "", "go_als.1": "", "goals.1 and 2": "",
		"goals.-1": "", "goals.1.2": "", ".1": "", "none": "", "goals,1": "", "bogus.1": "", "verify.1": "",
	} {
		if got := cleanLead(in); got != want {
			t.Errorf("cleanLead(%q) = %q, want %q", in, got, want)
		}
	}
}

// The one audit schema has no lead property (the panel runs beside the
// reviewer and never tells it anything) and no maxItems (the model is told
// no number, WP-1421), and a panel audit's job is given that same schema.
func TestAuditSchemaHasNoLeadAndNoMaxItems(t *testing.T) {
	if !json.Valid([]byte(auditSchema)) {
		t.Fatalf("schema isn't JSON: %s", auditSchema)
	}
	var parsed struct {
		Properties struct {
			Findings struct {
				MaxItems int `json:"maxItems"`
				Items    struct {
					Required   []string       `json:"required"`
					Properties map[string]any `json:"properties"`
				} `json:"items"`
			} `json:"findings"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(auditSchema), &parsed); err != nil {
		t.Fatal(err)
	}
	items := parsed.Properties.Findings.Items
	if _, ok := items.Properties["lead"]; ok || slices.Contains(items.Required, "lead") {
		t.Error("the schema has a lead")
	}
	if parsed.Properties.Findings.MaxItems != 0 || strings.Contains(auditSchema, "maxItems") {
		t.Errorf("maxItems = %d: the model is told no number", parsed.Properties.Findings.MaxItems)
	}
	for _, k := range []string{"title", "path", "line", "severity", "problem", "confirm"} {
		if !slices.Contains(items.Required, k) {
			t.Errorf("%s is not required", k)
		}
	}

	ae := newAgentEnv(t)
	if _, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Instructions: "Look.", MaxFindings: 7, Via: "web",
		purpose: AuditReview, helpers: &AuditHelpers{Panel: []PanelCheck{{Name: "goals", Ask: "a"}}}}); err != nil {
		t.Fatal(err)
	}
	ae.work()
	if in := agentJobOf(t, ae.take()); in.JSONSchema != auditSchema {
		t.Errorf("a panel audit's job has another schema:\n%s", in.JSONSchema)
	}
}

func TestParseAuditAnswerHelperFields(t *testing.T) {
	long := strings.Repeat("evidence ", 400)
	res := auditResult("Read it.",
		map[string]any{"title": "Confirmed one", "path": "a.go", "line": 1, "severity": "high", "problem": "p", "verdict": " Confirmed ", "verdict_note": "Read a.go:1.", "lead": "`Goals.1`"},
		map[string]any{"title": "Refuted one", "path": "b.go", "line": 2, "severity": "medium", "problem": "p", "verdict": "refuted", "verdict_note": long, "lead": "quality.12"},
		map[string]any{"title": "Garbage fields", "path": "c.go", "line": 3, "severity": "low", "problem": "p", "verdict": "probably", "verdict_note": "  spaced \n out  ", "lead": "goals.1234"},
		map[string]any{"title": "No fields", "path": "d.go", "line": 4, "severity": "low", "problem": "p"},
	)
	a, err := parseAuditAnswer(res)
	if err != nil {
		t.Fatal(err)
	}
	byTitle := map[string]CodeAuditFinding{}
	for _, f := range a.Findings {
		byTitle[f.Title] = f
	}
	if f := byTitle["Confirmed one"]; f.Verdict != VerdictConfirmed || f.VerdictNote != "Read a.go:1." || f.Lead != "goals.1" {
		t.Errorf("confirmed = %+v", f)
	}
	f := byTitle["Refuted one"]
	if f.Verdict != VerdictRefuted || f.Lead != "quality.12" {
		t.Errorf("refuted = %+v", f)
	}
	if n := utf8.RuneCountInString(f.VerdictNote); n != 2000 || !strings.HasSuffix(f.VerdictNote, "…") {
		t.Errorf("verdict_note is %d runes, want 2000 ending in an ellipsis", n)
	}
	if f := byTitle["Garbage fields"]; f.Verdict != "" || f.Lead != "" || f.VerdictNote != "spaced out" {
		t.Errorf("garbage = %+v", f)
	}
	if f := byTitle["No fields"]; f.Verdict != "" || f.VerdictNote != "" || f.Lead != "" {
		t.Errorf("no fields = %+v", f)
	}
}

func TestHelperRunsOf(t *testing.T) {
	name := panelChecks[0].name
	lead := func(id, sev, path string, line int) map[string]any {
		return map[string]any{"id": id, "title": "T", "severity": sev, "path": path, "line": line, "problem": "P"}
	}
	run := func(check, state string, extra map[string]any) map[string]any {
		m := map[string]any{"check": check, "state": state, "cost_usd": 0.02, "ms": 1500}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	result := func(runs ...any) map[string]any { return map[string]any{"helpers": runs} }

	for _, tc := range []struct {
		name string
		res  map[string]any
	}{
		{"no result", nil},
		{"no helpers key", map[string]any{"answer": "x"}},
		{"helpers null", map[string]any{"helpers": nil}},
		{"helpers is a string", map[string]any{"helpers": "none"}},
		{"helpers is an object", map[string]any{"helpers": map[string]any{"check": name}}},
		{"empty list", result()},
	} {
		if got := helperRunsOf(tc.res); len(got) != 0 {
			t.Errorf("%s: %+v", tc.name, got)
		}
	}

	// Unknown names and states are dropped; known ones kept in order.
	var all []any
	for _, c := range panelChecks {
		all = append(all, run(c.name, "done", nil))
	}
	all = append(all, run(helperVerify, "failed", map[string]any{"error": "boom"}))
	all = append(all,
		run("bogus", "done", nil),
		run("", "done", nil),
		run("Goals", "done", nil), // names are exact
		run(name, "running", nil),
		run(name, "DONE", nil),
		run(name, "", nil),
		run(helperVerify, "cancelled", nil),
	)
	got := helperRunsOf(result(all...))
	if len(got) != len(panelChecks)+1 {
		t.Fatalf("kept %d runs, want %d: %+v", len(got), len(panelChecks)+1, got)
	}
	for i, c := range panelChecks {
		if got[i].Check != c.name || got[i].State != "done" || got[i].CostUSD != 0.02 || got[i].MS != 1500 {
			t.Errorf("run %d = %+v", i, got[i])
		}
	}
	if last := got[len(got)-1]; last.Check != helperVerify || last.State != "failed" || last.Error != "boom" {
		t.Errorf("verify run = %+v", last)
	}
	for _, st := range helperStates {
		if r := helperRunsOf(result(run(helperVerify, st, nil))); len(r) != 1 {
			t.Errorf("state %q dropped", st)
		}
	}

	// A run with only bad entries is nothing.
	if r := helperRunsOf(result(run("bogus", "done", nil), run(name, "weird", nil))); len(r) != 0 {
		t.Errorf("only bad entries kept: %+v", r)
	}

	// Leads: every one a runner reports is kept, to the sanity bound, with
	// its id up to three digits; cut leads keep their why and lose any id.
	var many []any
	for i := 1; i <= helperLeadsKeep+20; i++ {
		many = append(many, lead(fmt.Sprintf("goals.%d", i), "high", "a.go", i))
	}
	cutLead := lead("goals.9", "low", "a.go", 1)
	cutLead["why"] = "repeats a lead of the tests check"
	r := helperRunsOf(result(run(name, "done", map[string]any{"leads": many, "cut": []any{cutLead}, "dropped": 3})))
	if len(r) != 1 || len(r[0].Leads) != helperLeadsKeep || r[0].Dropped != 3 || !strings.Contains(r[0].Error, "20 that became findings and 0 cut past that weren't kept") {
		t.Fatalf("leads: %d, dropped %d, error %q", len(r[0].Leads), r[0].Dropped, r[0].Error)
	}
	// Cut leads past the bound: counted in Dropped and the error, however
	// few the runner said it dropped.
	var cuts []any
	for i := 1; i <= helperLeadsKeep+5; i++ {
		c := lead("", "low", "a.go", i)
		c["why"] = "past the prompt's size guard"
		cuts = append(cuts, c)
	}
	if r := helperRunsOf(result(run(name, "done", map[string]any{"cut": cuts, "dropped": 1}))); len(r[0].Cut) != helperLeadsKeep || r[0].Dropped != helperLeadsKeep+5 || !strings.Contains(r[0].Error, "0 that became findings and 5 cut") {
		t.Errorf("cut past the bound: %d kept, dropped %d, error %q", len(r[0].Cut), r[0].Dropped, r[0].Error)
	}
	if r[0].Leads[helperLeadsKeep-1].ID != fmt.Sprintf("goals.%d", helperLeadsKeep) || r[0].Leads[0].Why != "" {
		t.Errorf("the first %d leads aren't kept with their ids: last = %+v", helperLeadsKeep, r[0].Leads[helperLeadsKeep-1])
	}
	if c := r[0].Cut; len(c) != 1 || c[0].ID != "goals.9" || c[0].Why != "repeats a lead of the tests check" || c[0].Severity != "low" {
		t.Errorf("cut = %+v", c)
	}
	if r := helperRunsOf(result(run(name, "done", map[string]any{"dropped": -4}))); r[0].Dropped != 0 {
		t.Errorf("a negative dropped = %d", r[0].Dropped)
	}

	long := strings.Repeat("x", 3000)
	r = helperRunsOf(result(run(name, "failed", map[string]any{
		"error": long,
		"leads": []any{
			lead("`Goals.1`", "high", "a/internal/x.go", 4),
			lead("not an id", "major", "/etc/passwd", 9),
			lead("goals.3", "CRITICAL", "../secret", -5),
			lead("goals.4", "low", "internal/y.go:77", 0),
			map[string]any{"id": "goals.5", "title": long, "severity": "medium", "problem": long},
		},
	})))
	if len(r) != 1 || len(r[0].Leads) != 5 {
		t.Fatalf("leads: %+v", r)
	}
	if n := utf8.RuneCountInString(r[0].Error); n != 500 {
		t.Errorf("error is %d runes, want 500", n)
	}
	ls := r[0].Leads
	if ls[0].ID != "goals.1" || ls[0].Severity != "high" || ls[0].Path != "internal/x.go" || ls[0].Line != 4 {
		t.Errorf("lead 0 = %+v", ls[0])
	}
	if ls[1].ID != "" || ls[1].Severity != "medium" || ls[1].Path != "" {
		t.Errorf("a bad id, label and absolute path stay: %+v", ls[1])
	}
	if ls[2].Severity != "medium" || ls[2].Path != "" || ls[2].Line != 0 {
		t.Errorf("an upper-case label, a path out of the repository and a negative line stay: %+v", ls[2])
	}
	if ls[3].Path != "internal/y.go" || ls[3].Line != 77 {
		t.Errorf("a path:line is split: %+v", ls[3])
	}
	if utf8.RuneCountInString(ls[4].Title) != 200 || utf8.RuneCountInString(ls[4].Problem) != 2000 {
		t.Errorf("title %d runes, problem %d; want 200, 2000", utf8.RuneCountInString(ls[4].Title), utf8.RuneCountInString(ls[4].Problem))
	}
}

// A job's answer with helpers ends in the audit's rows: the finding's
// verdict, note and lead, and one audit_helper_runs row per known helper.
func TestEndAuditKeepsHelperResults(t *testing.T) {
	fx := newHelperFixture(t)
	id := fx.exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, owner_id, created_at, request_key, status) VALUES (0, 0, '', 'repo', 0, 1, 'x', 'running')`)

	res := auditResult("Read it.",
		map[string]any{"title": "Real", "path": "a.go", "line": 1, "severity": "high", "problem": "p", "verdict": "confirmed", "verdict_note": "yes", "lead": "goals.1"},
		map[string]any{"title": "Wrong", "path": "b.go", "line": 2, "severity": "medium", "problem": "p", "verdict": "refuted", "verdict_note": "b.go:2 guards it", "lead": "nonsense"},
	)
	res["helpers"] = []any{
		map[string]any{"check": "goals", "state": "done", "ms": 900, "cost_usd": 0.01, "dropped": 2,
			"leads": []any{map[string]any{"id": "goals.1", "title": "Departs", "severity": "high", "path": "a.go", "line": 1, "problem": "P"}},
			"cut":   []any{map[string]any{"title": "Twice", "severity": "low", "problem": "P", "why": "repeats"}}},
		map[string]any{"check": "verify", "state": "failed", "error": "claude exited"},
		map[string]any{"check": "invented", "state": "done"},
	}
	found, err := parseAuditAnswer(res)
	if err != nil {
		t.Fatal(err)
	}
	a := &CodeAudit{ID: id, Status: AuditRunning}
	a.HelperRuns = helperRunsOf(res)
	fx.s.endAudit(fx.ctx, a, AuditDone, "", found)

	rows, err := fx.s.db.R.QueryContext(fx.ctx, `SELECT title, verdict, verdict_note, lead FROM code_audit_findings WHERE audit_id = ? ORDER BY ordinal`, id)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var title, v, note, lead string
		_ = rows.Scan(&title, &v, &note, &lead)
		got = append(got, strings.Join([]string{title, v, note, lead}, "|"))
	}
	_ = rows.Close()
	want := []string{"Real|confirmed|yes|goals.1", "Wrong|refuted|b.go:2 guards it|"}
	if !slices.Equal(got, want) {
		t.Errorf("findings = %q, want %q", got, want)
	}

	runs, err := fx.s.helperRuns(fx.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || !runs[0].Answered {
		t.Fatalf("helper runs = %+v, want goals and verify only, answered", runs)
	}
	if r := runs[0]; r.Check != "goals" || r.State != "done" || r.MS != 900 || r.CostUSD != 0.01 || r.Dropped != 2 || len(r.Leads) != 1 || r.Leads[0].ID != "goals.1" || r.Leads[0].Title != "Departs" ||
		len(r.Cut) != 1 || r.Cut[0].Title != "Twice" || r.Cut[0].Why != "repeats" {
		t.Errorf("goals run = %+v", r)
	}
	if r := runs[1]; r.Check != helperVerify || r.State != "failed" || r.Error != "claude exited" || len(r.Leads) != 0 {
		t.Errorf("verify run = %+v", r)
	}

	answered := func(audit int64) []bool {
		t.Helper()
		rows, err := fx.s.db.R.QueryContext(fx.ctx, `SELECT answered FROM audit_helper_runs WHERE audit_id = ? ORDER BY id`, audit)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var out []bool
		for rows.Next() {
			var v bool
			_ = rows.Scan(&v)
			out = append(out, v)
		}
		return out
	}
	if got := answered(id); !slices.Equal(got, []bool{true, true}) {
		t.Errorf("answered = %v", got)
	}

	// An attempt sent on to the next model, or ending the audit unanswered:
	// its helpers ran and cost, so they're kept, unanswered (WP-1320's live
	// round 6); the next attempt's runs go alongside.
	c := fx.exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, owner_id, created_at, request_key, status) VALUES (0, 0, '', 'repo', 0, 1, 'z', 'running')`)
	fx.s.requeueAudit(fx.ctx, &CodeAudit{ID: c, Status: AuditRunning, HelperRuns: a.HelperRuns}, 1, 0, nil)
	fx.exec(`UPDATE code_audits SET status = 'running' WHERE id = ?`, c)
	fx.s.endAudit(fx.ctx, &CodeAudit{ID: c, Status: AuditRunning, HelperRuns: a.HelperRuns}, AuditFailed, "no answer", nil)
	if got := answered(c); !slices.Equal(got, []bool{false, false, false, false}) {
		t.Errorf("unanswered attempts: answered = %v", got)
	}
	if runs, _ := fx.s.helperRuns(fx.ctx, c); len(runs) != 4 || runs[0].CostUSD != 0.01 || runs[0].Answered {
		t.Errorf("unanswered attempts' runs = %+v", runs)
	}

	// An audit already ended (cancelled meanwhile) records nothing more.
	b := fx.exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, owner_id, created_at, request_key, status) VALUES (0, 0, '', 'repo', 0, 1, 'y', 'cancelled')`)
	fx.s.endAudit(fx.ctx, &CodeAudit{ID: b, Status: AuditRunning, HelperRuns: a.HelperRuns}, AuditDone, "", found)
	var n int
	_ = fx.s.db.R.QueryRowContext(fx.ctx, `SELECT (SELECT count(*) FROM audit_helper_runs WHERE audit_id = ?) + (SELECT count(*) FROM code_audit_findings WHERE audit_id = ?)`, b, b).Scan(&n)
	if n != 0 {
		t.Errorf("a cancelled audit got %d rows", n)
	}
}

// A verdict stands only when a check of findings checked: with none run, or
// one skipped, failed or stopped, a verdict in the answer is the reviewer's
// own and is cleared (WP-1320's live round 7).
func TestDropUncheckedVerdicts(t *testing.T) {
	answer := func() *auditAnswer {
		return &auditAnswer{Findings: []CodeAuditFinding{{Title: "a", Verdict: VerdictRefuted, VerdictNote: "mine"}, {Title: "b"}}}
	}
	for _, c := range []struct {
		runs []AuditHelperRun
		kept bool
	}{
		{nil, false},
		{[]AuditHelperRun{{Check: "goals", State: "done"}}, false},
		{[]AuditHelperRun{{Check: helperVerify, State: "skipped"}}, false},
		{[]AuditHelperRun{{Check: helperVerify, State: "failed"}}, false},
		{[]AuditHelperRun{{Check: helperVerify, State: "stopped"}}, false},
		{[]AuditHelperRun{{Check: helperVerify, State: "partial"}}, true},
		{[]AuditHelperRun{{Check: helperVerify, State: "done"}}, true},
	} {
		a := answer()
		dropUncheckedVerdicts(a, c.runs)
		if kept := a.Findings[0].Verdict == VerdictRefuted && a.Findings[0].VerdictNote == "mine"; kept != c.kept || (!kept && a.Findings[0].VerdictNote != "") {
			t.Errorf("runs %+v: finding = %+v, want kept %v", c.runs, a.Findings[0], c.kept)
		}
	}
}

// A panel finding keeps its lead and its confirmed verdict whatever the check
// of findings did, when a panel run records its lead as confirmed; a
// reviewer's finding loses a lead it wrote, which the answer form no
// longer asks for (WP-1320).
func TestDropUncheckedVerdictsPanel(t *testing.T) {
	runs := []AuditHelperRun{{Check: "goals", State: "done", Leads: []PanelLead{{ID: "goals.1", Confirmed: true}}},
		{Check: "tests", State: "done", Leads: []PanelLead{{ID: "tests.2", Confirmed: true}, {ID: "tests.1"}}}}
	a := &auditAnswer{Findings: []CodeAuditFinding{
		{Title: "panel", Lead: "goals.1,tests.2", Verdict: VerdictConfirmed, VerdictNote: "seen", Problem: "p"},
		{Title: "forged", Lead: "goals.9", Verdict: VerdictConfirmed, Problem: "p"},
		{Title: "half", Lead: "goals.1,goals.9", Verdict: VerdictConfirmed, Problem: "p"},
		{Title: "reviewer", Lead: "tests.1", Problem: "q"},
	}}
	dropUncheckedVerdicts(a, runs)
	if f := a.Findings[0]; f.Lead != "goals.1,tests.2" || f.Verdict != VerdictConfirmed {
		t.Errorf("panel finding = %+v", f)
	}
	// An id no run confirmed (goals.9, or tests.1 the read didn't confirm)
	// spoils the whole list.
	for _, i := range []int{1, 2} {
		if f := a.Findings[i]; f.Lead != "" || f.Verdict != "" {
			t.Errorf("forged finding %d = %+v", i, f)
		}
	}
	if f := a.Findings[3]; f.Lead != "" {
		t.Errorf("reviewer finding keeps lead %q", f.Lead)
	}
	if got := PanelSource(a.Findings[0]); got != "Haiku panel (goals, tests), confirmed by a second Haiku read" {
		t.Errorf("PanelSource = %q", got)
	}
	if got := PanelSource(a.Findings[3]); got != "" {
		t.Errorf("PanelSource of a reviewer's finding = %q", got)
	}
}

// A done audit keeps a finding's verdict only when a check of findings
// ran (done or partial): one the reviewer wrote itself, in any case of its
// keys, is cleared (WP-1320's review of its live round 7). End to end,
// through settleAudit.
func TestAuditKeepsOnlyCheckedVerdicts(t *testing.T) {
	ae := newAgentEnv(t)
	for i, c := range []struct {
		helpers []any
		want    string
	}{
		{nil, ""},
		{[]any{map[string]any{"check": "verify", "state": "failed", "error": "x"}}, ""},
		{[]any{map[string]any{"check": "verify", "state": "done"}}, VerdictRefuted},
	} {
		n := 7 + i
		ae.prHook("opened", ForgePR{Number: n, Title: "Export", HeadRef: fmt.Sprintf("b%d", n), HeadSHA: fmt.Sprintf("abc%d", n), HeadRepo: "me/app", Author: ae.admin.User.Username})
		a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{PRID: ae.pr(n).ID, Via: "web"})
		if err != nil {
			t.Fatal(err)
		}
		ae.work()
		claim := ae.take()
		res := auditResult("Read it.", map[string]any{"Title": "Mine", "path": "a.go", "line": 1, "severity": "high", "problem": "p", "Verdict": "refuted", "VERDICT_NOTE": "says me"})
		if c.helpers != nil {
			res["helpers"] = c.helpers
		}
		ae.finish(claim.ID, "done", res, "")
		got := ae.audit(a.ID)
		if got.Status != AuditDone || len(got.Findings) != 1 {
			t.Fatalf("case %d: audit = %s %+v (%s)", i, got.Status, got.Findings, got.Error)
		}
		if f := got.Findings[0]; f.Verdict != c.want || (c.want == "" && f.VerdictNote != "") {
			t.Errorf("case %d: verdict %q note %q, want %q", i, f.Verdict, f.VerdictNote, c.want)
		}
	}
}

// The panel's order follows each check's record: one whose leads the
// reviewer took up comes before one whose leads it passed over, every
// round; the same round gets the same order.
func TestPanelOrder(t *testing.T) {
	fx := newHelperFixture(t)
	p := fx.project(fx.admin, "Alpha", "kanban")
	a := fx.exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, owner_id, created_at, request_key, status) VALUES (?, 0, '', 'repo', 0, ?, 'k', 'done')`, p.ID, panelReadsFrom+1)
	leads := func(check string, n int, confirmed bool) string {
		var ls []string
		for i := 1; i <= n; i++ {
			ls = append(ls, fmt.Sprintf(`{"id":"%s.%d","title":"t","severity":"low","problem":"p","confirmed":%v}`, check, i, confirmed))
		}
		return "[" + strings.Join(ls, ",") + "]"
	}
	fx.exec(`INSERT INTO audit_helper_runs(audit_id, check_name, state, leads, created_at, answered) VALUES (?, 'goals', 'done', ?, ?, 1)`, a, leads("goals", 20, false), panelReadsFrom+1)
	fx.exec(`INSERT INTO audit_helper_runs(audit_id, check_name, state, leads, created_at, answered) VALUES (?, 'quality', 'done', ?, ?, 1)`, a, leads("quality", 7, true), panelReadsFrom+1)
	for i := 1; i <= 6; i++ {
		f := fx.finding(a, i, "medium", "fixed", "confirmed")
		fx.exec(`UPDATE code_audit_findings SET lead = ? WHERE id = ?`, fmt.Sprintf("quality.%d", i), f)
	}
	checks := []PanelCheck{{Name: "goals"}, {Name: "quality"}}
	for round := int64(1); round <= 20; round++ {
		got := fx.s.panelOrder(fx.ctx, checks, round)
		if got[0].Name != "quality" || got[1].Name != "goals" {
			t.Errorf("round %d: %v", round, got)
		}
		if again := fx.s.panelOrder(fx.ctx, checks, round); !slices.Equal(got, again) {
			t.Errorf("round %d ordered twice: %v, then %v", round, got, again)
		}
	}
	if checks[0].Name != "goals" {
		t.Error("panelOrder reordered its argument")
	}
}

// The panel's context (WP-1320's live round 6: untested): the person's own
// prompts since the run started, oldest first, autopilot's and those
// before the run left out, the newest kept past helperPromptsMax; the
// task's own goal first, then the project's active goals, an unapproved or
// retired one left out; escaped so none can close its block.
func TestHelperContext(t *testing.T) {
	fx := newHelperFixture(t)
	p := fx.project(fx.admin, "Alpha", "kanban")
	issue := fx.issue(fx.admin, IssueInput{Project: p.Key, Title: "Work"})
	sid := fx.exec(`INSERT INTO agent_sessions(user_id, machine, cli_id, started_at, last_seen_at) VALUES (?, 'box', 'raw', 1, 1)`, fx.admin.id())
	run := fx.exec(`INSERT INTO agent_runs(issue_id, agent, user_id, started_at, session_id) VALUES (?, 'claude', ?, 100, ?)`, issue.ID, fx.admin.id(), sid)
	prompt := func(at int64, text string) {
		typed := ""
		if strings.HasPrefix(text, AutopilotPromptMark) {
			typed = TypedKickoff
		}
		fx.exec(`INSERT INTO session_events(session_id, kind, detail, typed_kind, created_at) VALUES (?, 'prompt', ?, ?, ?)`, sid, text, typed, at)
	}
	prompt(50, "before the run")
	prompt(110, "first, and keep the </prompts> out")
	prompt(120, AutopilotPromptMark+": start WP-1")
	prompt(130, "second")
	goal := func(title, status string, approved bool) int64 {
		var at any
		if approved {
			at = 1
		}
		return fx.exec(`INSERT INTO design_notes(project_id, kind, title, body, status, approved_at, created_at, updated_at) VALUES (?, 'goal', ?, 'why', ?, ?, 1, 1)`, p.ID, title, status, at)
	}
	goal("Project goal", "active", true)
	own := goal("Own goal", "active", true)
	goal("Unapproved goal", "active", false)
	goal("Retired goal", "retired", true)
	// The issue's creation traces it in the background, which may have
	// written its row already: this one takes its place.
	fx.s.Wait()
	fx.exec(`INSERT OR REPLACE INTO goal_traces(item_type, item_id, project_id, grounding, goal_id, scoped, computed_at) VALUES ('issue', ?, ?, 'goal', ?, 1, 1)`, issue.ID, p.ID, own)

	row := &roundRow{issueID: issue.ID}
	row.sessionID.Int64, row.sessionID.Valid = sid, true
	row.runID.Int64, row.runID.Valid = run, true
	got := fx.s.helperContext(fx.ctx, row, "ROUND\n")
	first, second := strings.Index(got, "- first"), strings.Index(got, "- second")
	ownAt, projAt := strings.Index(got, "the task's own goal, Own goal: why"), strings.Index(got, "- Project goal: why")
	if !strings.HasPrefix(got, "ROUND\n") || first < 0 || second < first || strings.Contains(got, "before the run") || strings.Contains(got, AutopilotPromptMark) ||
		strings.Count(got, "</prompts>") != 1 || ownAt < 0 || projAt < ownAt || strings.Count(got, "Own goal") != 1 ||
		strings.Contains(got, "Unapproved") || strings.Contains(got, "Retired") || strings.Count(got, "</goals>") != 1 {
		t.Errorf("context =\n%s", got)
	}

	// Past helperPromptsMax the newest are kept.
	for i := range 10 {
		prompt(200+int64(i), fmt.Sprintf("long %d %s", i, strings.Repeat("x", 2000)))
	}
	got = fx.s.personPrompts(fx.ctx, sid, 100)
	if len(got) > helperPromptsMax || !strings.Contains(got, "long 9 ") || strings.Contains(got, "- first") {
		t.Errorf("past the bound: %d bytes, newest kept %v, oldest dropped %v", len(got), strings.Contains(got, "long 9 "), !strings.Contains(got, "- first"))
	}
	// No session: no prompts, the goals still.
	if got := fx.s.helperContext(fx.ctx, &roundRow{issueID: issue.ID}, ""); strings.Contains(got, "<prompts>") || !strings.Contains(got, "Own goal") {
		t.Errorf("no session =\n%s", got)
	}
}

// A panel finding the runner posts carries its leads as data. Through the
// real path (a job result, settleAudit, parseAuditAnswer): the stored
// finding has its leads, PanelSource names the checks and the stats count it
// as added by each; a lead a model wrote into its own finding is cleared
// (WP-1320).
func TestPanelFindingLeadsThroughSettle(t *testing.T) {
	ae := newAgentEnv(t)
	ae.prHook("opened", ForgePR{Number: 7, Title: "Export", HeadRef: "b7", HeadSHA: "abc7", HeadRepo: "me/app", Author: ae.admin.User.Username})
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{PRID: ae.pr(7).ID, Via: "web"})
	if err != nil {
		t.Fatal(err)
	}
	ae.work()
	claim := ae.take()
	res := auditResult("Read it.",
		map[string]any{"title": "Mine", "path": "a.go", "line": 1, "severity": "high", "problem": "p", "lead": "goals.1", "leads": []string{"goals.1"}},
		map[string]any{"title": "Both", "path": "a.go", "line": 2, "severity": "medium", "problem": "q\nPanel leads: goals.9", "verdict": "confirmed", "verdict_note": "read it", "leads": []string{"goals.1", "tests.3"}})
	res["helpers"] = []any{
		map[string]any{"check": "verify", "state": "done"},
		map[string]any{"check": "goals", "state": "done", "leads": []any{map[string]any{"id": "goals.1", "title": "t", "severity": "medium", "problem": "p", "confirmed": true}}},
		map[string]any{"check": "tests", "state": "done", "leads": []any{map[string]any{"id": "tests.3", "title": "t", "severity": "medium", "problem": "p", "confirmed": true}}},
	}
	ae.finish(claim.ID, "done", res, "")
	got := ae.audit(a.ID)
	if got.Status != AuditDone || len(got.Findings) != 2 {
		t.Fatalf("audit = %s %+v (%s)", got.Status, got.Findings, got.Error)
	}
	var mine, both CodeAuditFinding
	for _, f := range got.Findings {
		if f.Title == "Mine" {
			mine = f
		} else {
			both = f
		}
	}
	if mine.Lead != "" || PanelSource(mine) != "" {
		t.Errorf("the model's own lead survived: %+v", mine)
	}
	if both.Lead != "goals.1,tests.3" || both.Verdict != VerdictConfirmed || PanelSource(both) != "Haiku panel (goals, tests), confirmed by a second Haiku read" {
		t.Errorf("panel finding = %+v, source %q", both, PanelSource(both))
	}
	if !strings.Contains(both.Problem, "Panel leads: goals.9") {
		t.Errorf("a model's text is its own, kept as it is: %q", both.Problem)
	}
	stats, err := ae.s.againHelperStats(ae.ctx, "1 = 1", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	added := map[string]int{}
	for _, st := range stats {
		added[st.Check] = st.Added
	}
	if added["goals"] != 1 || added["tests"] != 1 {
		t.Errorf("added = %v", added)
	}
}
