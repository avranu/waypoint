package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// evidenceLLM answers the pointer prep, the evidence check (WP-ADR157) and
// anything else (estimates).
type evidenceLLM struct {
	mu       sync.Mutex
	evidence string
	fail     error
	prompts  []string
	systems  []string
	// during runs in an evidence check and pointing in a pointer prep, before
	// either answers; gate, when set, holds an evidence check until closed.
	during, pointing func()
	gate             chan struct{}
}

func (f *evidenceLLM) Models(ctx context.Context, p *AIProvider) ([]AIModel, error) {
	return []AIModel{{ID: "opencode/big-pickle", Name: "Big Pickle", Free: true}}, nil
}

func (f *evidenceLLM) Complete(ctx context.Context, p *AIProvider, model, system, prompt string, schema map[string]any) (json.RawMessage, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasPrefix(prompt, "Evidence check for task "):
		f.prompts, f.systems = append(f.prompts, prompt), append(f.systems, system)
		answer, fail, during, gate := f.evidence, f.fail, f.during, f.gate
		f.mu.Unlock()
		if during != nil {
			during()
		}
		if gate != nil {
			<-gate
		}
		f.mu.Lock()
		if fail != nil {
			return nil, "", fail
		}
		return json.RawMessage(answer), "", nil
	case strings.HasPrefix(prompt, "Task "):
		if f.pointing != nil {
			f.pointing()
		}
		return json.RawMessage(`{"files": [{"path": "internal/merge.go", "why": "the gate"}], "symbols": [], "tests": [], "related": [], "terms": [], "questions": []}`), "", nil
	}
	return json.RawMessage(`{"points": 3, "confidence": "high", "reason": "Small."}`), "", nil
}

const noEvidence = `{"evidence": [], "claims": []}`

type evidenceEnv struct {
	*env
	llm   *evidenceLLM
	p     *Project
	ag    *Actor
	task  *IssueDetail
	pages map[string]*Page
	notes map[string]*DesignNote
}

func (e *evidenceEnv) page(a *Actor, name, category, project, title, body string, draft bool, issues ...string) {
	e.t.Helper()
	in := PageInput{Title: &title, Body: &body, Category: &category, Draft: draft}
	if project != "" {
		in.Project = Some(project)
	}
	if len(issues) > 0 {
		in.Issues = &issues
	}
	pg, err := e.s.CreatePage(e.ctx, a, in)
	if err != nil {
		e.t.Fatalf("page %s: %v", name, err)
	}
	e.pages[name] = pg
}

func (e *evidenceEnv) decision(name, project, title, body string) {
	e.t.Helper()
	kind := "decision"
	n, err := e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: &title, Body: &body, Kind: &kind, Project: &project})
	if err != nil {
		e.t.Fatalf("decision %s: %v", name, err)
	}
	e.notes[name] = n
}

// supersede links one page as superseding another.
func (e *evidenceEnv) supersede(name, old string) int64 {
	e.t.Helper()
	l, err := e.s.AddItemLink(e.ctx, e.admin, LinkInput{From: LinkEnd{Type: KPage, ID: e.pages[name].ID},
		To: LinkEnd{Type: KPage, ID: e.pages[old].ID}, Rel: "supersedes"})
	if err != nil {
		e.t.Fatalf("%s supersedes %s: %v", name, old, err)
	}
	return l.ID
}

// code is a page's or decision's project-qualified code.
func (e *evidenceEnv) code(name string) string {
	if pg := e.pages[name]; pg != nil {
		return e.p.Key + "-" + pg.Code
	}
	return e.p.Key + "-" + e.notes[name].Code
}

func (e *evidenceEnv) bundle() PrepBundle {
	b := prepTestBundle()
	b.Words = []string{"merge", "checks"}
	return b
}

func (e *evidenceEnv) prep(key string) {
	e.t.Helper()
	if _, err := e.s.UploadPrep(e.ctx, e.admin, key, e.bundle()); err != nil {
		e.t.Fatal(err)
	}
	e.s.Wait()
}

// served is the prep as the API gives it to a wp that prints evidence.
func (e *evidenceEnv) served(a *Actor, key string, shown bool) *TaskPrep {
	e.t.Helper()
	p, err := e.s.GetPrep(e.ctx, a, key, shown)
	if err != nil {
		e.t.Fatal(err)
	}
	e.s.AttachPrepEvidence(e.ctx, a, p, shown)
	return p
}

// claim has the agent take the task, as wp next does before it reads the prep.
func (e *evidenceEnv) claim(a *Actor, key string) {
	e.t.Helper()
	if _, err := e.s.AgentTake(e.ctx, a, key, ""); err != nil {
		e.t.Fatal(err)
	}
}

// forceArm gives a task its arm in the live run before anything draws one.
func (e *evidenceEnv) forceArm(issueID int64, arm string) {
	e.t.Helper()
	if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO experiment_units(run_id, issue_id, arm, via, assigned_at)
		SELECT id, ?, ?, 'prep', 0 FROM experiment_runs WHERE experiment = ? AND stopped_at IS NULL`, issueID, arm, ExpPrepEvidence); err != nil {
		e.t.Fatal(err)
	}
}

func (e *evidenceEnv) units(issueID int64) string {
	e.t.Helper()
	return e.one(`SELECT (SELECT count(*) FROM experiment_units u JOIN experiment_runs r ON r.id = u.run_id WHERE r.experiment = 'prep_evidence' AND u.issue_id = ?1)
		|| ' ' || (SELECT count(*) FROM prep_evidence_units WHERE issue_id = ?1)`, issueID)
}

func newEvidenceEnv(t *testing.T) *evidenceEnv {
	t.Helper()
	e := &evidenceEnv{env: newEnv(t), llm: &evidenceLLM{evidence: noEvidence}, pages: map[string]*Page{}, notes: map[string]*DesignNote{}}
	e.s.LLM = e.llm
	if err := e.s.EnsureDefaultAI(e.ctx, "http://opencode:4096", "", "opencode/big-pickle"); err != nil {
		t.Fatal(err)
	}
	e.p = e.project(e.admin, "App", "")
	e.ag = agentActor(e.admin, "anthropic/claude-code")
	e.task = e.issue(e.admin, IssueInput{Project: e.p.Key, Title: "Merge only when checks pass", Priority: "high",
		Description: "Use `mergeReady`. Most merges fail because checks are slow."})
	if _, err := e.s.AddComment(e.ctx, e.admin, e.task.Key, "The flaky checks cost an hour a day."); err != nil {
		t.Fatal(err)
	}
	return e
}

// seed adds recorded data that may be offered, and some that never may.
func (e *evidenceEnv) seed() {
	e.t.Helper()
	other := e.project(e.admin, "Elsewhere", "")
	// Offered: one linked to the task, others by words.
	e.page(e.ag, "linked", "experiment", e.p.Key, "Gate timing", "Linked measurement: 41 of 200 waited.", false, e.task.Key)
	e.page(e.admin, "ref", "reference", e.p.Key, "Merge checks reference", "Checks take four minutes at the median.", false)
	e.page(e.admin, "note", "note", e.p.Key, "Merge note", "Merge checks were retried twice a day.", false)
	e.decision("assumes", e.p.Key, "Gate merges on checks", "## Decision\nMerge on green checks.\n\n## Assumptions\n- Checks finish within ten minutes.\n\n## Consequences\nLater merge.")
	// Never offered.
	e.page(e.admin, "draft", "experiment", e.p.Key, "Draft merge checks", "merge checks draft", true)
	e.page(e.admin, "personal", "experiment", "", "Personal merge checks", "merge checks personal", false)
	e.page(e.admin, "other", "experiment", other.Key, "Foreign merge checks", "merge checks foreign", false)
	e.page(e.admin, "plan", "plan", e.p.Key, "Plan merge checks", "merge checks plan", false)
	e.page(e.admin, "old", "experiment", e.p.Key, "Replaced merge checks", "merge checks replaced", false)
	e.supersede("plan", "old")
	e.decision("bare", e.p.Key, "Bare merge checks decision", "## Decision\nMerge checks, nothing assumed.")
	e.decision("gone", e.p.Key, "Superseded merge checks decision", "## Assumptions\nMerge checks are instant.")
	superseded := "superseded"
	if _, err := e.s.UpdateDesign(e.ctx, e.admin, e.notes["gone"].ID, DesignInput{Status: &superseded}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *evidenceEnv) answer() string {
	return fmt.Sprintf(`{"evidence": [
		{"ref": %q, "bears_on": "how often\nmerges wait"}, {"ref": "APP-X99", "bears_on": "invented"}, {"ref": %q, "bears_on": "again"},
		{"ref": %q, "bears_on": "what the gate assumes"}, {"ref": %q, "bears_on": "check times"}, {"ref": %q, "bears_on": "a fourth"}],
		"claims": [
		{"quote": "most merges   fail because checks are slow", "test": "count why merges failed", "ref": %q},
		{"quote": "checks cost an hour a day", "test": "time them for a week", "ref": "APP-X99"},
		{"quote": "this sentence is not in the task", "test": "x", "ref": null},
		{"quote": "Use", "test": "too short", "ref": null},
		{"quote": "Merge only when checks pass", "test": "  ", "ref": null}]}`,
		strings.ToLower(e.code("linked")), e.code("linked"), e.code("assumes"), e.code("ref"), e.code("note"), e.code("linked"))
}

func refsOf(ev *PrepEvidence) []string {
	var out []string
	for _, x := range ev.Evidence {
		out = append(out, x.Ref)
	}
	return out
}

// After the pointers, a second request names recorded data and claims to
// test: only candidates and verbatim quotes are kept, and only what any
// project viewer can see is offered (WP-ADR157).
func TestPrepEvidence(t *testing.T) {
	e := newEvidenceEnv(t)
	e.seed()
	e.llm.evidence = e.answer()
	e.prepArm(e.task.ID, prepArmChat)
	e.prep(e.task.Key)

	if len(e.llm.prompts) != 1 {
		t.Fatalf("evidence prompts: %q", e.llm.prompts)
	}
	prompt := e.llm.prompts[0]
	for _, want := range []string{"Evidence check for task " + e.task.Key, "Most merges fail because checks are slow.", "flaky checks",
		e.code("linked"), "Gate timing", "41 of 200 waited", "agent, unreviewed", e.code("ref"), e.code("note"), e.code("assumes"), "Checks finish within ten minutes."} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	for _, not := range []string{"Draft merge", "Personal merge", "Foreign merge", "Plan merge", "Bare merge", "Superseded merge", "Replaced merge", "Later merge."} {
		if strings.Contains(prompt, not) {
			t.Errorf("prompt offers %q:\n%s", not, prompt)
		}
	}
	if n := strings.Count(prompt, "["+e.code("linked")+"]"); n != 1 {
		t.Errorf("the linked page is offered %d times", n)
	}
	if strings.Index(prompt, "["+e.code("linked")+"]") > strings.Index(prompt, "["+e.code("ref")+"]") {
		t.Error("a linked page comes after a matched one")
	}
	if !strings.Contains(e.llm.systems[0], "recorded evidence") {
		t.Errorf("system: %q", e.llm.systems[0])
	}
	if got := e.one(`SELECT state || ' ' || candidates || ' ' || dropped || ' ' || (model != '') || ' ' || error FROM task_prep_evidence WHERE issue_id = ?`, e.task.ID); got != "ready 4 7 1 " {
		t.Fatalf("row = %q", got)
	}

	// A person is given it, and is no unit.
	p := e.served(e.admin, e.task.Key, true)
	ev := p.Evidence
	if ev == nil {
		t.Fatal("a person got no evidence")
	}
	if got, want := refsOf(ev), []string{e.code("linked"), e.code("assumes"), e.code("ref")}; !slices.Equal(got, want) {
		t.Fatalf("evidence = %v; want %v", got, want)
	}
	first, second := ev.Evidence[0], ev.Evidence[1]
	if first.Kind != "page" || first.Category != "experiment" || first.Title != "Gate timing" || first.By != "agent, unreviewed" ||
		first.Date != "2026-10-05" || first.BearsOn != "how often merges wait" {
		t.Errorf("a page: %+v", first)
	}
	if second.Kind != "design" || second.Category != "decision" || second.By != "person" || second.Title != "Gate merges on checks" {
		t.Errorf("a decision: %+v", second)
	}
	if len(ev.Claims) != 2 || ev.Claims[0].Quote != "Most merges fail because checks are slow" || ev.Claims[0].Ref != e.code("linked") ||
		ev.Claims[0].Test != "count why merges failed" || ev.Claims[1].Ref != "" {
		t.Fatalf("claims = %+v", ev.Claims)
	}
	if got := e.units(e.task.ID); got != "0 0" {
		t.Errorf("a person: units %s", got)
	}

	// An agent that isn't taking it, or may take nothing, draws no arm.
	ro := *e.ag
	ro.ReadOnly = true
	for name, got := range map[string]*TaskPrep{"unshown": e.served(e.ag, e.task.Key, false), "read-only": e.served(&ro, e.task.Key, true)} {
		if got.Evidence != nil {
			t.Errorf("%s got evidence", name)
		}
	}
	if got := e.units(e.task.ID); got != "0 0" {
		t.Errorf("unshown and read-only: units %s", got)
	}

	// The shown arm.
	e.claim(e.ag, e.task.Key)
	e.forceArm(e.task.ID, "shown")
	if got := e.served(e.ag, e.task.Key, true); got.Evidence == nil || len(got.Evidence.Evidence) != 3 || got.Notes == nil {
		t.Fatalf("shown arm: %+v", got)
	}
	if got := e.units(e.task.ID); got != "1 1" {
		t.Fatalf("shown arm: units %s", got)
	}
	snapshot := func(id int64) string {
		return e.one(`SELECT issue_type || ' ' || priority || ' ' || text_len || ' ' || text_hash || ' ' || candidates || ' ' || evidence_n || ' ' || claims_n || ' ' || dropped || ' ' || risk || ' ' || evidence || ' ' || (model != '') || ' ' || at
			FROM prep_evidence_units WHERE issue_id = ?`, id)
	}
	// What was given at the first exposure, whole, and the task's risk then.
	given, _ := json.Marshal(e.served(e.admin, e.task.Key, false).Evidence)
	risk, err := e.s.issueRiskByID(e.ctx, e.s.db.R, e.ag, e.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"Gate timing", "how often merges wait", "2026-10-05", "experiment", "Most merges fail because checks are slow", "count why merges failed", e.code("assumes")} {
		if !strings.Contains(string(given), w) {
			t.Errorf("evidence lacks %q: %s", w, given)
		}
	}
	textLen := utf8.RuneCountInString(e.task.Title) + utf8.RuneCountInString(e.task.Description)
	checked := e.one(`SELECT text_hash FROM task_prep_evidence WHERE issue_id = ?`, e.task.ID) // of the text the check read
	want := fmt.Sprintf("%s 3 %d %s 4 3 2 7 %s %s 1 %d", e.task.Type, textLen, checked, risk.Level, given, e.s.nowMS())
	if got := snapshot(e.task.ID); got != want {
		t.Errorf("snapshot = %s; want %s", got, want)
	}
	if got := e.one(`SELECT exposed_at IS NOT NULL FROM experiment_units WHERE issue_id = ?`, e.task.ID); got != "1" {
		t.Errorf("not exposed")
	}
	// Once given, a later read shows it too; the snapshot is the first one.
	e.setClock(e.clockNow().Add(60e9))
	e.served(e.ag, e.task.Key, true)
	if got := snapshot(e.task.ID); got != want {
		t.Errorf("snapshot changed: %s", got)
	}
	for name, got := range map[string]*TaskPrep{"unshown": e.served(e.ag, e.task.Key, false), "read-only": e.served(&ro, e.task.Key, true)} {
		if got.Evidence == nil {
			t.Errorf("%s: the task's arm shows it", name)
		}
	}

	// What can no longer be seen is dropped, and a claim loses that ref.
	if err := e.s.DeletePage(e.ctx, e.admin, e.pages["linked"].ID); err != nil {
		t.Fatal(err)
	}
	retired := "retired"
	if _, err := e.s.UpdateDesign(e.ctx, e.admin, e.notes["assumes"].ID, DesignInput{Status: &retired}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []*Actor{e.admin, e.ag} {
		ev = e.served(a, e.task.Key, true).Evidence
		if ev == nil || !slices.Equal(refsOf(ev), []string{e.code("ref")}) || len(ev.Claims) != 2 || ev.Claims[0].Ref != "" {
			t.Fatalf("after removals: %+v", ev)
		}
	}

	link := e.supersede("plan", "ref")
	if ev = e.served(e.admin, e.task.Key, false).Evidence; ev == nil || len(ev.Evidence) != 0 || len(ev.Claims) != 2 {
		t.Fatalf("a superseded page: %+v", ev)
	}
	if err := e.s.RemoveItemLink(e.ctx, e.admin, link); err != nil {
		t.Fatal(err)
	}

	// Evidence of another text than the pointers', or of a stale prep, is
	// not served.
	if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE task_prep_evidence SET text_hash = 'other'`); err != nil {
		t.Fatal(err)
	}
	if got := e.served(e.admin, e.task.Key, false); got.Evidence != nil {
		t.Errorf("another text's evidence: %+v", got.Evidence)
	}
	e.prep(e.task.Key)
	if got := e.served(e.admin, e.task.Key, false); got.Evidence == nil {
		t.Fatal("made again: none")
	}
	desc := "Something else entirely."
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, e.task.Key, IssuePatch{Description: &desc}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []*Actor{e.admin, e.ag} {
		if got := e.served(a, e.task.Key, true); !got.Stale || got.Evidence != nil {
			t.Errorf("stale: %+v", got)
		}
	}
}

// Both arms are units with the same snapshot; only one is given the
// evidence. None is given without a live run (WP-ADR157).
func TestPrepEvidenceArms(t *testing.T) {
	e := newEvidenceEnv(t)
	e.llm.evidence = `{"evidence": [], "claims": [{"quote": "checks are slow", "test": "time them", "ref": null}]}`
	if run, err := e.s.liveExperimentRun(e.ctx, ExpPrepEvidence); err != nil || run == nil || run.Note != "started by WP-1150" ||
		run.Weights["none"] != 1 || run.Weights["shown"] != 1 || run.StartedAt.IsZero() {
		t.Fatalf("the migration's run: %+v %v", run, err)
	}
	if d, ok := experimentDef(ExpPrepEvidence); !ok || d.control() != "none" || d.Primary != "cost_usd" || d.Client {
		t.Fatalf("definition: %+v", d)
	}
	mk := func(title string) *IssueDetail {
		i := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: title, Description: "The checks are slow."})
		return i
	}
	// Drawn by the experiment: find a task in each arm.
	seen := map[string]string{}
	for n := 0; n < 40 && len(seen) < 2; n++ {
		i := mk(fmt.Sprint("Drawn ", n))
		e.prep(i.Key)
		e.claim(e.ag, i.Key)
		got := e.served(e.ag, i.Key, true)
		arm := e.one(`SELECT arm FROM experiment_units WHERE issue_id = ? AND via = 'prep' AND exposed_at IS NOT NULL`, i.ID)
		if (arm == "shown") != (got.Evidence != nil) {
			t.Fatalf("arm %s: %+v", arm, got.Evidence)
		}
		if units := e.units(i.ID); units != "1 1" {
			t.Fatalf("arm %s: units %s", arm, units)
		}
		if got := e.one(`SELECT evidence_n || ' ' || claims_n || ' ' || dropped || ' ' || (risk != '') || ' ' || json_array_length(evidence, '$.claims') FROM prep_evidence_units WHERE issue_id = ?`, i.ID); got != "0 1 0 1 1" {
			t.Fatalf("arm %s: snapshot %s", arm, got)
		}
		seen[arm] = i.Key
	}
	if seen["none"] == "" || seen["shown"] == "" {
		t.Fatalf("arms seen: %v", seen)
	}
	control := mk("Control")
	e.forceArm(control.ID, "none")
	e.prep(control.Key)
	e.claim(e.ag, control.Key)
	if got := e.served(e.ag, control.Key, true); got.Evidence != nil {
		t.Errorf("control: %+v", got.Evidence)
	}
	if got := e.served(e.ag, control.Key, false); got.Evidence != nil {
		t.Errorf("control, unshown: %+v", got.Evidence)
	}

	// Nothing found: kept, nothing given, no unit.
	e.llm.evidence = noEvidence
	empty := mk("Empty")
	e.prep(empty.Key)
	e.claim(e.ag, empty.Key)
	if got := e.served(e.ag, empty.Key, true); got.Evidence != nil {
		t.Errorf("nothing found: %+v", got.Evidence)
	}
	if got := e.served(e.admin, empty.Key, false); got.Evidence != nil {
		t.Errorf("nothing found, for a person: %+v", got.Evidence)
	}
	if got := e.one(`SELECT state FROM task_prep_evidence WHERE issue_id = ?`, empty.ID) + " " + e.units(empty.ID); got != "ready 0 0" {
		t.Errorf("nothing found: %s", got)
	}

	// No live run: agents get none and are no units; people still read what a
	// run stored.
	stored := mk("Stored")
	e.prep(stored.Key)
	if got := e.one(`SELECT state || ' ' || (notes != '') FROM task_prep_evidence WHERE issue_id = ?`, stored.ID); got != "ready 1" {
		t.Fatalf("stored while live: %s", got)
	}
	if _, err := e.s.StopExperiment(e.ctx, e.admin, ExpPrepEvidence, ""); err != nil {
		t.Fatal(err)
	}
	// Its holder, drawing nothing now, gets none of what is stored.
	e.claim(e.ag, stored.Key)
	if got := e.served(e.ag, stored.Key, true); got.Evidence != nil || e.units(stored.ID) != "0 0" {
		t.Errorf("stored, run stopped: %+v, units %s", got.Evidence, e.units(stored.ID))
	}
	e.llm.evidence = `{"evidence": [], "claims": [{"quote": "checks are slow", "test": "time them", "ref": null}]}`
	late := mk("Late")
	e.prep(late.Key)
	e.claim(e.ag, late.Key)
	if got := e.served(e.ag, late.Key, true); got.Evidence != nil || e.units(late.ID) != "0 0" {
		t.Errorf("no live run: %+v, units %s", got.Evidence, e.units(late.ID))
	}
	if got := e.served(e.ag, seen["shown"], false); got.Evidence != nil {
		t.Errorf("a stopped run's unit: %+v", got.Evidence)
	}
	if got := e.served(e.admin, seen["shown"], false); got.Evidence == nil {
		t.Error("a person got none")
	}
}

// A reader given the task by a share, not a viewer of its project, gets no
// evidence and draws no arm: it can paraphrase any of the project's pages.
func TestPrepEvidenceSharedTask(t *testing.T) {
	e := newEvidenceEnv(t)
	e.llm.evidence = `{"evidence": [], "claims": [{"quote": "checks are slow", "test": "time them", "ref": null}]}`
	e.prep(e.task.Key)
	guest, viewer := e.user("guest", false), e.user("viewer", false)
	if err := e.s.ShareIssue(e.ctx, e.admin, e.task.Key, "guest", "editor"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.SetMember(e.ctx, e.admin, e.p.Key, "viewer", "viewer"); err != nil {
		t.Fatal(err)
	}
	agent := agentActor(guest, "anthropic/claude-code")
	e.served(agent, e.task.Key, true)
	if got := e.units(e.task.ID); got != "0 0" {
		t.Errorf("share only: units %s", got)
	}
	// Nor in a task already given the shown arm.
	e.forceArm(e.task.ID, "shown")
	for _, a := range []*Actor{guest, agent} {
		for _, shown := range []bool{true, false} {
			if got := e.served(a, e.task.Key, shown); got.Evidence != nil {
				t.Errorf("share only (agent %q, shown %v): %+v", a.Agent, shown, got.Evidence)
			}
		}
	}
	if got := e.served(viewer, e.task.Key, false); got.Evidence == nil {
		t.Error("a project viewer got none")
	}
}

// Evidence names a page as it is when read, not as it was when matched.
func TestPrepEvidenceCurrentPage(t *testing.T) {
	e := newEvidenceEnv(t)
	e.seed()
	e.llm.evidence = fmt.Sprintf(`{"evidence": [{"ref": %q, "bears_on": "check times"}], "claims": []}`, e.code("ref"))
	e.prep(e.task.Key)
	e.setClock(e.clockNow().Add(48 * time.Hour))
	title := "Merge checks, measured again"
	pg, err := e.s.UpdatePage(e.ctx, e.ag, e.pages["ref"].ID, PageInput{Title: &title})
	if err != nil {
		t.Fatal(err)
	}
	ev := e.served(e.admin, e.task.Key, false).Evidence
	want := PrepEvidenceItem{Ref: e.code("ref"), Kind: KPage, Title: title, Category: "reference",
		Date: pg.UpdatedAt.Format("2006-01-02"), By: "agent, unreviewed", BearsOn: "check times",
		Updated: e.pages["ref"].UpdatedAt.UnixMilli(), ChangedSince: true}
	if ev == nil || len(ev.Evidence) != 1 || ev.Evidence[0] != want {
		t.Fatalf("evidence = %+v; want %+v", ev, want)
	}
}

// A failed evidence check leaves the pointer prep ready.
func TestPrepEvidenceFailure(t *testing.T) {
	e := newEvidenceEnv(t)
	e.llm.fail = errors.New("the model is on fire " + strings.Repeat("x", 900))
	if _, err := e.s.UploadPrep(e.ctx, e.admin, e.task.Key, e.bundle()); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	if len(e.llm.prompts) == 0 {
		t.Fatal("no evidence check")
	}
	p := e.served(e.ag, e.task.Key, true)
	if p.State != "ready" || p.Error != "" || p.Evidence != nil {
		t.Fatalf("prep = %+v", p)
	}
	if got := e.one(`SELECT state || ' ' || (error != '') || ' [' || notes || ']' FROM task_prep_evidence WHERE issue_id = ?`, e.task.ID); got != "failed 1 []" {
		t.Errorf("row = %q", got)
	}
	if got := e.units(e.task.ID); got != "0 0" {
		t.Errorf("units %s", got)
	}
}

// A surplus evidence check the gateway turns away is no failure.
func TestPrepEvidenceRefused(t *testing.T) {
	e, g, p := surplusEnv(t)
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Task"})
	g.set(func(g *fakeGateway) { g.status = 503 })
	d, err := e.s.issueByID(e.ctx, e.s.db.R, i.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	err = e.s.runPrepEvidence(e.ctx, prepEvidenceTask{issue: d, priority: prioritySurplus, hash: "h", title: d.Title})
	var wait *aiWaitError
	if !errors.As(err, &wait) {
		t.Errorf("returned %v; want the gateway's wait", err)
	}
	if got := e.one(`SELECT state FROM task_prep_evidence WHERE issue_id = ?`, i.ID); got != "refused" {
		t.Errorf("state = %q", got)
	}
}

func TestCheckPrepEvidence(t *testing.T) {
	cands := []evidenceCand{
		{PrepEvidenceItem: PrepEvidenceItem{Ref: "APP-X1", Kind: "page", Title: "One"}},
		{PrepEvidenceItem: PrepEvidenceItem{Ref: "APP-ADR2", Kind: "design", Title: "Two"}},
		{PrepEvidenceItem: PrepEvidenceItem{Ref: "APP-R3", Kind: "page", Title: "Three"}},
		{PrepEvidenceItem: PrepEvidenceItem{Ref: "APP-N4", Kind: "page", Title: "Four"}},
	}
	texts := []string{"Cache the board", "The board takes  nine seconds\nto load on every visit.", "Ünïcode Größe matters here too.", strings.Repeat("long ", 60)}
	pick := func(ref, why string) evidencePick { return evidencePick{Ref: ref, BearsOn: why} }
	claim := func(quote, test, ref string) evidenceClaim {
		c := evidenceClaim{Quote: quote, Test: test}
		if ref != "" {
			c.Ref = &ref
		}
		return c
	}
	for _, tc := range []struct {
		name    string
		in      evidenceAnswer
		refs    []string
		quotes  []string
		claimed []string // each kept claim's ref
		dropped int
	}{
		{name: "empty"},
		{name: "case, spelling and duplicates", in: evidenceAnswer{Evidence: []evidencePick{pick(" app-x1 ", "a"), pick("APP-X1", "b"), pick("X1", "no project"), pick("APP-X9", "c")}},
			refs: []string{"APP-X1"}, dropped: 3},
		{name: "three at most", in: evidenceAnswer{Evidence: []evidencePick{pick("APP-X1", ""), pick("APP-ADR2", ""), pick("APP-R3", ""), pick("APP-N4", "")}},
			refs: []string{"APP-X1", "APP-ADR2", "APP-R3"}, dropped: 1},
		{name: "quotes", in: evidenceAnswer{Claims: []evidenceClaim{
			claim("THE BOARD takes nine\tseconds to load", "time it", "app-adr2"),
			claim("Cache the board The board", "spans title and description", ""),
			claim("nine hours", "not there", ""),
			claim("Cache the b", "eleven runes", ""),
			claim(strings.Repeat("long ", 41), "over 200 runes", ""),
			claim("ünïcode größe matters", "folds", "APP-X9"),
			claim("every visit.", "", ""),
		}}, quotes: []string{"The board takes nine seconds to load", "Ünïcode Größe matters"}, claimed: []string{"APP-ADR2", ""}, dropped: 6},
		{name: "claim refs", in: evidenceAnswer{Claims: []evidenceClaim{
			claim("The board takes", "unknown", "APP-X9"),
			claim("nine seconds to", "blank", " \t "),
			{Quote: "load on every", Test: "empty", Ref: new(string)},
		}}, quotes: []string{"The board takes", "nine seconds to", "load on every"}, claimed: []string{"", "", ""}, dropped: 1},
		{name: "dropped claim with an unknown ref counts once", in: evidenceAnswer{Claims: []evidenceClaim{claim("nine hours", "not there", "APP-X9")}}, dropped: 1},
		{name: "twelve runes", in: evidenceAnswer{Claims: []evidenceClaim{claim("Cache the bo", "t", ""), claim("cache the BO", "again", "")}},
			quotes: []string{"Cache the bo"}, claimed: []string{""}, dropped: 1},
		{name: "200 runes kept, 201 not", in: evidenceAnswer{Claims: []evidenceClaim{claim(strings.Repeat("long ", 41)[:201], "t", ""), claim(strings.Repeat("long ", 41)[1:201], "u", "")}},
			quotes: []string{strings.Repeat("long ", 41)[1:201]}, claimed: []string{""}, dropped: 1},
		{name: "three claims", in: evidenceAnswer{Claims: []evidenceClaim{claim("The board takes", "a", ""), claim("nine seconds to", "b", ""), claim("load on every", "c", ""), claim("Cache the board", "d", "")}},
			quotes: []string{"The board takes", "nine seconds to", "load on every"}, claimed: []string{"", "", ""}, dropped: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped := checkPrepEvidence(&tc.in, cands, texts)
			var quotes, claimed []string
			for _, c := range got.Claims {
				quotes, claimed = append(quotes, c.Quote), append(claimed, c.Ref)
			}
			if !slices.Equal(refsOf(got), tc.refs) || !slices.Equal(quotes, tc.quotes) || !slices.Equal(claimed, tc.claimed) || dropped != tc.dropped {
				t.Errorf("refs %q quotes %q claimed %q dropped %d", refsOf(got), quotes, claimed, dropped)
			}
		})
	}
	got, _ := checkPrepEvidence(&evidenceAnswer{
		Evidence: []evidencePick{pick("APP-X1", "a\n\x1b[31m"+strings.Repeat("b", 400))},
		Claims:   []evidenceClaim{claim("The board takes", "c\n"+strings.Repeat("d", 400), "")},
	}, cands, texts)
	why, test := got.Evidence[0].BearsOn, got.Claims[0].Test
	if utf8.RuneCountInString(why) != 160 || utf8.RuneCountInString(test) != 240 || strings.ContainsAny(why+test, "\n\x1b") || got.Evidence[0].Title != "One" {
		t.Errorf("clipped: %q %q", why, test)
	}
}

// A quote is cut from the text as it spells it, counted in runes: lower case
// takes fewer bytes than U+0130 and more than U+023A.
func TestPrepQuotedIn(t *testing.T) {
	for _, tc := range []struct{ text, quote, want string }{
		{"Zürich — most Merges FAIL here", "merges fail", "Merges FAIL"},
		{"İİİ most Merges FAIL here", "merges fail", "Merges FAIL"},
		{"ȺȺȺ most Merges FAIL here", "merges fail", "Merges FAIL"},
		{"so İSTANBUL merges ȺLL fail", "istanbul MERGES ⱥll", "İSTANBUL merges ȺLL"},
		{"İİ ends in Ⱥ", "in ⱥ", "in Ⱥ"},
		{"İİ most merges pass", "merges fail", ""},
	} {
		if got := quotedIn([]string{"nothing here", tc.text}, tc.quote); got != tc.want {
			t.Errorf("%q in %q: %q; want %q", tc.quote, tc.text, got, tc.want)
		}
	}
}

func TestPrepAssumptions(t *testing.T) {
	for in, want := range map[string]string{
		"## Decision\nx":   "",
		"## Assumptions\n": "",
		"## Assumptions\n- a\n### sub\n- b\n\n## Next\nc":  "- a\n### sub\n- b",
		"x\n\n## assumptions  \nlast\n# Top\nno":           "last",
		"```\n## Assumptions\nin code\n```\n":              "",
		"## Assumptions\n```\n# a comment\n```\nz\r\n## N": "```\n# a comment\n```\nz",
		"### Assumptions\nno":                              "",
		"## Assumptions of the old design\nno":             "",
		// A fence closes only on its own character, at least as long, alone.
		"````\n```\n## Assumptions\nin code\n````\n":    "",
		"```\n~~~\n## Assumptions\nin code\n```\n":      "",
		"```\n``` go\n## Assumptions\nin code\n```\n":   "",
		"~~~\ncode\n~~~~  \n## Assumptions\nafter":      "after",
		"## Assumptions\n~~~~\n~~~\n# x\n~~~~\n# T\nno": "~~~~\n~~~\n# x\n~~~~",
		// Backticks with a backtick after them on the line are inline code,
		// not a fence; tildes open one whatever follows.
		"```go x``` tail\n## Assumptions\nfound":            "found",
		"## Assumptions\n- a\n```go x``` tail\n## Next\nno": "- a\n```go x``` tail",
		"~~~ `x`\n## Assumptions\nin code\n~~~\n":           "",
		// A fence indented four spaces or by a tab is indented code, not a
		// fence; up to three spaces still open one.
		"## Assumptions\n- a\n\n    ```\n\n## Next\nno": "- a\n\n    ```",
		"    ```\n## Assumptions\nafter":                "after",
		"\t```\n## Assumptions\nafter a tab":            "after a tab",
		"   ```\n## Assumptions\nin code\n   ```\n":     "",
	} {
		if got := markdownSection(in, "Assumptions"); got != want {
			t.Errorf("%q: %q; want %q", in, got, want)
		}
	}
}
