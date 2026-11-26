package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// A task held out from the pointers has no pointer notes for agents, and
// evidence like any other task: its holder draws an arm and is a unit, so every
// pointer arm sees evidence at the same rate.
func TestPrepEvidenceHeldOut(t *testing.T) {
	for _, arm := range []string{"shown", "none"} {
		t.Run(arm, func(t *testing.T) {
			e := newEvidenceEnv(t)
			e.llm.evidence = oneClaim
			e.prepArm(e.task.ID, prepArmNone)
			e.prep(e.task.Key)
			e.claim(e.ag, e.task.Key)
			e.forceArm(e.task.ID, arm)
			for _, shown := range []bool{true, false} {
				got := e.served(e.ag, e.task.Key, shown)
				if !got.HeldOut || got.Notes != nil || (got.Evidence != nil) != (arm == "shown") {
					t.Errorf("shown %v: held out %v, notes %+v, evidence %+v", shown, got.HeldOut, got.Notes, got.Evidence)
				}
			}
			if got := e.units(e.task.ID); got != "1 1" {
				t.Errorf("units %s", got)
			}
			if got := e.served(e.admin, e.task.Key, false); got.Evidence == nil {
				t.Error("a person got none")
			}
		})
	}
}

// Only the agent holding the task draws its arm.
func TestPrepEvidenceHolder(t *testing.T) {
	e := newEvidenceEnv(t)
	e.llm.evidence = oneClaim
	e.prep(e.task.Key)
	e.served(e.ag, e.task.Key, true)
	if got := e.units(e.task.ID); got != "0 0" {
		t.Fatalf("an unclaimed task: units %s", got)
	}
	e.claim(agentActor(e.admin, "openai/codex"), e.task.Key)
	e.served(e.ag, e.task.Key, true)
	if got := e.units(e.task.ID); got != "0 0" {
		t.Fatalf("another agent's task: units %s", got)
	}
	other := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: "Mine", Description: "The checks are slow."})
	e.prep(other.Key)
	e.claim(e.ag, other.Key)
	e.served(e.ag, other.Key, true)
	if got := e.units(other.ID); got != "1 1" {
		t.Fatalf("the holder: units %s", got)
	}
}

// An agent that isn't holding the task is given its evidence only when the
// task is a unit in the shown arm already, and never makes it one.
func TestPrepEvidenceNonHolder(t *testing.T) {
	for _, c := range []struct {
		arm, units string
		given      bool
	}{
		{arm: "shown", units: "1 0", given: true},
		{arm: "none", units: "1 0"},
		{units: "0 0"},
	} {
		t.Run(cmp(c.arm, "no unit"), func(t *testing.T) {
			e := newEvidenceEnv(t)
			e.llm.evidence = oneClaim
			e.prep(e.task.Key)
			e.claim(agentActor(e.admin, "openai/codex"), e.task.Key)
			if c.arm != "" {
				e.forceArm(e.task.ID, c.arm)
			}
			for _, shown := range []bool{true, false} {
				if got := e.served(e.ag, e.task.Key, shown).Evidence; (got != nil) != c.given {
					t.Errorf("shown %v: evidence %+v", shown, got)
				}
			}
			if got := e.units(e.task.ID); got != c.units {
				t.Errorf("units %s; want %s", got, c.units)
			}
		})
	}
}

// A snapshot that can't be kept leaves the arm in force; the next fetch keeps it.
func TestPrepEvidenceSnapshotFailure(t *testing.T) {
	e := newEvidenceEnv(t)
	e.llm.evidence = oneClaim
	e.prep(e.task.Key)
	e.claim(e.ag, e.task.Key)
	e.forceArm(e.task.ID, "shown")
	exec := func(q string) {
		t.Helper()
		if _, err := e.s.db.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TRIGGER no_snapshot BEFORE INSERT ON prep_evidence_units BEGIN SELECT RAISE(ABORT, 'no snapshot'); END`)
	if got := e.served(e.ag, e.task.Key, true); got.Evidence == nil {
		t.Error("the shown arm got none")
	}
	if got := e.units(e.task.ID); got != "1 0" {
		t.Fatalf("units %s", got)
	}
	exec(`DROP TRIGGER no_snapshot`)
	e.served(e.ag, e.task.Key, true)
	if got := e.units(e.task.ID); got != "1 1" {
		t.Errorf("units %s", got)
	}
}

// jsonOf is the evidence as the API sends it.
func (e *evidenceEnv) sent() (items, claims []map[string]any) {
	e.t.Helper()
	raw, err := json.Marshal(e.served(e.admin, e.task.Key, false).Evidence)
	if err != nil {
		e.t.Fatal(err)
	}
	var got struct {
		Evidence []map[string]any `json:"evidence"`
		Claims   []map[string]any `json:"claims"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		e.t.Fatal(err)
	}
	return got.Evidence, got.Claims
}

// What was edited after it was matched says so, for a pick and a claim's ref.
func TestPrepEvidenceChangedSince(t *testing.T) {
	e := newEvidenceEnv(t)
	e.seed()
	e.llm.evidence = fmt.Sprintf(`{"evidence": [{"ref": %q, "bears_on": "check times"}, {"ref": %q, "bears_on": "the gate"}],
		"claims": [{"quote": "checks are slow", "test": "time them", "ref": %q}, {"quote": "Most merges fail", "test": "count", "ref": %q}]}`,
		e.code("ref"), e.code("assumes"), e.code("ref"), e.code("assumes"))
	e.prep(e.task.Key)
	items, claims := e.sent()
	if len(items) != 2 || len(claims) != 2 {
		t.Fatalf("%v %v", items, claims)
	}
	for _, m := range slices.Concat(items, claims) {
		if m["changed_since"] != nil {
			t.Errorf("unedited: %v", m)
		}
	}
	e.setClock(e.clockNow().Add(48 * time.Hour))
	body := "Checks take nine minutes now."
	if _, err := e.s.UpdatePage(e.ctx, e.admin, e.pages["ref"].ID, PageInput{Body: &body}); err != nil {
		t.Fatal(err)
	}
	items, claims = e.sent()
	if items[0]["changed_since"] != true || items[1]["changed_since"] != nil || claims[0]["changed_since"] != true || claims[1]["changed_since"] != nil {
		t.Errorf("after an edit: %v %v", items, claims)
	}
}

// A claim's ref carries the kind of what it names, and loses it with the ref.
func TestPrepEvidenceRefKind(t *testing.T) {
	e := newEvidenceEnv(t)
	e.seed()
	e.llm.evidence = fmt.Sprintf(`{"evidence": [], "claims": [{"quote": "checks are slow", "test": "time them", "ref": %q}, {"quote": "Most merges fail", "test": "count", "ref": %q}]}`,
		e.code("ref"), e.code("assumes"))
	e.prep(e.task.Key)
	_, claims := e.sent()
	if len(claims) != 2 || claims[0]["ref_kind"] != "page" || claims[1]["ref_kind"] != "design" {
		t.Fatalf("claims %v", claims)
	}
	retired := "retired"
	if _, err := e.s.UpdateDesign(e.ctx, e.admin, e.notes["assumes"].ID, DesignInput{Status: &retired}); err != nil {
		t.Fatal(err)
	}
	if _, claims = e.sent(); claims[1]["ref"] != nil || claims[1]["ref_kind"] != nil {
		t.Errorf("a retired decision: %v", claims[1])
	}
}

func (e *evidenceEnv) candidates(i *IssueDetail) []evidenceCand {
	e.t.Helper()
	d, err := e.s.issueByID(e.ctx, e.s.db.R, i.ID, true)
	if err != nil {
		e.t.Fatal(err)
	}
	c, err := e.s.evidenceCandidates(e.ctx, d, []string{"merge", "checks"})
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

// Only the section's own heading counts towards the search's limit.
func TestPrepEvidenceAssumptionsLimit(t *testing.T) {
	e := newEvidenceEnv(t)
	for n := range 20 {
		e.decision(fmt.Sprint("sub", n), e.p.Key, "Merge checks", "## Decision\nmerge checks merge checks merge checks\n\n### Assumptions\nmerge checks")
	}
	filler := strings.Repeat("unrelated filler words here ", 200)
	e.decision("first", e.p.Key, "Gate", "## Assumptions\n- A merge waits. "+filler)
	e.decision("later", e.p.Key, "Queue", "## Decision\nQueue.\n\n## Assumptions\n- A merge waits. "+filler)
	var got []string
	for _, c := range e.candidates(e.task) {
		got = append(got, c.Ref)
	}
	slices.Sort(got)
	want := []string{e.code("first"), e.code("later")}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("candidates %v; want %v", got, want)
	}
}

// Evidence names pages and decisions and says what is in them, so a token that
// can't read both is given none of it, and as holder draws no arm.
func TestPrepEvidenceNeedsReadPermissions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		perms []Permission
		given bool
	}{
		{"unrestricted", nil, true},
		{"both", []Permission{PermIssuesRead, PermPagesRead, PermDesignRead}, true},
		{"no pages.read", []Permission{PermIssuesRead, PermDesignRead}, false},
		{"no design.read", []Permission{PermIssuesRead, PermPagesRead}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEvidenceEnv(t)
			e.llm.evidence = `{"evidence": [], "claims": [{"quote": "checks are slow", "test": "time them", "ref": null}]}`
			e.prep(e.task.Key)
			e.claim(e.ag, e.task.Key)
			holder, reader := *e.ag, *e.admin
			holder.Permissions, reader.Permissions = permissionSet(tc.perms), permissionSet(tc.perms)
			if tc.given {
				e.forceArm(e.task.ID, "shown")
			}
			if got := e.served(&holder, e.task.Key, true); (got.Evidence != nil) != tc.given || got.Notes == nil {
				t.Errorf("the holder: %+v", got)
			}
			want := "1 1"
			if !tc.given {
				want = "0 0"
			}
			if got := e.units(e.task.ID); got != want {
				t.Errorf("units %s; want %s", got, want)
			}
			// Nor is it given to a read of a task already in the shown arm; a
			// restricted token reads as an agent does.
			if !tc.given {
				e.forceArm(e.task.ID, "shown")
			}
			for name, a := range map[string]*Actor{"the holder": &holder, "another read": &reader} {
				if got := e.served(a, e.task.Key, false); (got.Evidence != nil) != tc.given {
					t.Errorf("%s: %+v", name, got.Evidence)
				}
			}
		})
	}
}

// Superseded pages don't count towards either page query's limit, linked or
// matched by words.
func TestPrepEvidenceSupersededPagesLimit(t *testing.T) {
	filler := strings.Repeat("unrelated filler words here ", 200)
	for _, tc := range []struct {
		name, title, body, oldTitle, oldBody string
		linked                               bool
	}{
		{"linked", "Zebra", "Zebras sleep.", "Zebra", "Zebras wake.", true},
		{"matched", "Gate", "A merge waits for checks. " + filler, "Merge checks", "merge checks merge checks", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEvidenceEnv(t)
			var issues []string
			if tc.linked {
				issues = []string{e.task.Key}
			}
			e.page(e.admin, "valid", "reference", e.p.Key, tc.title, tc.body, false, issues...)
			e.page(e.admin, "newer", "plan", e.p.Key, "Replacement", "Replaced.", false)
			for n := range 21 {
				name := fmt.Sprint("old", n)
				e.page(e.admin, name, "reference", e.p.Key, tc.oldTitle, tc.oldBody, false, issues...)
				e.supersede("newer", name)
			}
			got := e.candidates(e.task)
			if len(got) != 1 || got[0].Ref != e.code("valid") {
				t.Errorf("candidates %+v; want %s", got, e.code("valid"))
			}
		})
	}
}

// linkNote links a decision or design note to a task.
func (e *evidenceEnv) linkNote(name string, issueID int64) {
	e.t.Helper()
	if _, err := e.s.AddItemLink(e.ctx, e.admin, LinkInput{From: LinkEnd{Type: KDesign, ID: e.notes[name].ID},
		To: LinkEnd{Type: KIssue, ID: issueID}, Rel: "relates"}); err != nil {
		e.t.Fatalf("link %s: %v", name, err)
	}
}

// The newest 20 linked decisions are the newest with an Assumptions section:
// those without one don't count towards the limit.
func TestPrepEvidenceLinkedAssumptionsLimit(t *testing.T) {
	e := newEvidenceEnv(t)
	link := func(name, body string) {
		e.decision(name, e.p.Key, "Zebra", body)
		e.linkNote(name, e.task.ID)
	}
	link("older", "## Assumptions\n- Zebras sleep.")
	for n := range 21 {
		link(fmt.Sprint("bare", n), "## Decision\nZebra.\n\n### Assumptions\nZebra.")
	}
	got := e.candidates(e.task)
	if len(got) != 1 || got[0].Ref != e.code("older") {
		t.Errorf("candidates %+v; want %s", got, e.code("older"))
	}
}

// Only a decision's Assumptions are evidence: another design note with such a
// section is offered neither by its link nor by its words.
func TestPrepEvidenceOnlyDecisions(t *testing.T) {
	e := newEvidenceEnv(t)
	for _, kind := range []string{"feature", "note", "requirement"} {
		title, body := "Merge checks "+kind, "## Assumptions\n- Merge checks are quick."
		n, err := e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: &title, Body: &body, Kind: &kind, Project: &e.p.Key})
		if err != nil {
			t.Fatal(err)
		}
		e.notes[kind] = n
	}
	e.linkNote("feature", e.task.ID)
	e.decision("matched", e.p.Key, "Merge checks decision", "## Assumptions\n- Merge checks are quick.")
	got := e.candidates(e.task)
	if len(got) != 1 || got[0].Ref != e.code("matched") {
		t.Errorf("candidates %+v; want only %s", got, e.code("matched"))
	}
}

// With no page the answer names to show and no claim, the holder is given
// nothing and the task is no unit.
func TestPrepEvidenceAllHidden(t *testing.T) {
	e := newEvidenceEnv(t)
	e.page(e.admin, "ref", "reference", e.p.Key, "Merge checks reference", "Checks take four minutes.", false)
	e.llm.evidence = fmt.Sprintf(`{"evidence": [{"ref": %q, "bears_on": "check times"}], "claims": []}`, e.code("ref"))
	e.prep(e.task.Key)
	if got := e.served(e.admin, e.task.Key, false); got.Evidence == nil || len(got.Evidence.Evidence) != 1 {
		t.Fatalf("before: %+v", got.Evidence)
	}
	if err := e.s.DeletePage(e.ctx, e.admin, e.pages["ref"].ID); err != nil {
		t.Fatal(err)
	}
	e.claim(e.ag, e.task.Key)
	for _, a := range []*Actor{e.ag, e.admin} {
		if got := e.served(a, e.task.Key, true); got.Evidence != nil {
			t.Errorf("agent %q: %+v", a.Agent, got.Evidence)
		}
	}
	if got := e.units(e.task.ID); got != "0 0" {
		t.Errorf("units %s", got)
	}
}

// Only surplus work is refused: any other check the gateway asks to wait is a
// failed one.
func TestPrepEvidenceWaitAtNormalPriority(t *testing.T) {
	e, g := newBudgetEnv(t)
	g.set(func(g *budgetGateway) { g.busy = "600" })
	p := e.project(e.admin, "App", "")
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Task"})
	d, err := e.s.issueByID(e.ctx, e.s.db.R, i.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	err = e.s.runPrepEvidence(e.ctx, prepEvidenceTask{issue: d, priority: "normal", hash: "h", title: d.Title})
	var wait *aiWaitError
	if !errors.As(err, &wait) {
		t.Fatalf("returned %v; want the gateway's wait", err)
	}
	if got := e.one(`SELECT state FROM task_prep_evidence WHERE issue_id = ?`, i.ID); got != "failed" {
		t.Errorf("state = %q", got)
	}
}

// With nothing recorded to offer, the check still reads the task for claims.
func TestPrepEvidenceNoCandidates(t *testing.T) {
	e := newEvidenceEnv(t)
	e.prep(e.task.Key)
	if len(e.llm.prompts) != 1 {
		t.Fatalf("evidence prompts: %q", e.llm.prompts)
	}
	for _, want := range []string{"Candidates: none.", "Merge only when checks pass", "Most merges fail because checks are slow.", "flaky checks"} {
		if !strings.Contains(e.llm.prompts[0], want) {
			t.Errorf("prompt lacks %q:\n%s", want, e.llm.prompts[0])
		}
	}
}

func TestPrepEvidenceLongTitle(t *testing.T) {
	e := newEvidenceEnv(t)
	long := "Merge checks " + strings.Repeat("é", 180)
	e.page(e.admin, "long", "experiment", e.p.Key, long, "merge checks", false)
	e.decision("longer", e.p.Key, long, "## Assumptions\nMerge checks.")
	got := e.candidates(e.task)
	if len(got) != 2 {
		t.Fatalf("candidates %+v", got)
	}
	for _, c := range got {
		if n := utf8.RuneCountInString(c.Title); n != 120 {
			t.Errorf("%s: title of %d runes", c.Ref, n)
		}
	}
}

func TestPrepEvidenceCandidateCaps(t *testing.T) {
	e := newEvidenceEnv(t)
	for n := range evidencePageMax + 3 {
		e.page(e.admin, fmt.Sprint("p", n), []string{"experiment", "reference", "note", "problem"}[n%4], e.p.Key, fmt.Sprint("Merge checks ", n), "merge checks", false)
	}
	for n := range evidenceDesignMax + 3 {
		e.decision(fmt.Sprint("d", n), e.p.Key, fmt.Sprint("Merge checks ", n), "## Assumptions\nMerge checks.")
	}
	count := map[string]int{}
	for _, c := range e.candidates(e.task) {
		count[c.Kind]++
	}
	if count[KPage] != 8 || count[KDesign] != 4 {
		t.Errorf("candidates %v", count)
	}
}

// A page linked to the parent is offered to a sub-task, a person's review of
// an agent's page is said, and a draft's supersedes link retires nothing.
func TestPrepEvidenceCandidateSources(t *testing.T) {
	e := newEvidenceEnv(t)
	story := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: "Story", Type: "story"})
	child := e.issue(e.admin, IssueInput{Parent: story.Key, Title: "Slice", Type: "subtask"})
	e.page(e.ag, "parented", "experiment", e.p.Key, "Unrelated words", "Nothing in common.", false, story.Key)
	got := e.candidates(child)
	if len(got) != 1 || got[0].Ref != e.code("parented") || got[0].By != "agent, unreviewed" {
		t.Fatalf("from the parent: %+v", got)
	}
	if _, err := e.s.db.W.Exec(`UPDATE pages SET reviewed_at = ?, reviewed_by = ? WHERE id = ?`, e.s.nowMS(), e.admin.User.ID, e.pages["parented"].ID); err != nil {
		t.Fatal(err)
	}
	e.page(e.admin, "replacement", "experiment", e.p.Key, "Draft", "draft", true)
	e.supersede("replacement", "parented")
	got = e.candidates(child)
	if len(got) != 1 || got[0].By != "reviewed by a person" {
		t.Errorf("reviewed, with a draft's supersedes link: %+v", got)
	}
}

// A surplus check that fails for anything but a wait is a failure, and the
// error kept is cut to 500 bytes.
func TestPrepEvidenceSurplusError(t *testing.T) {
	e, g, p := surplusEnv(t)
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Task"})
	// 413 is a request too large, not the gateway declining surplus work.
	g.set(func(g *fakeGateway) { g.status = 413 })
	d, err := e.s.issueByID(e.ctx, e.s.db.R, i.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	task := prepEvidenceTask{issue: d, priority: prioritySurplus, hash: "h", title: d.Title}
	if err := e.s.runPrepEvidence(e.ctx, task); err == nil {
		t.Error("no error returned")
	}
	if got := e.one(`SELECT state FROM task_prep_evidence WHERE issue_id = ?`, i.ID); got != "failed" {
		t.Errorf("state = %q", got)
	}
	cause := errors.New(strings.Repeat("x", 900))
	check, err := e.s.beginPrepEvidence(e.ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.storePrepEvidence(e.ctx, task, check, "failed", nil, "", "", nil, 0, cause); !errors.Is(err, cause) {
		t.Errorf("returned %v", err)
	}
	if got := e.one(`SELECT length(CAST(error AS BLOB)) FROM task_prep_evidence WHERE issue_id = ?`, i.ID); got != "500" {
		t.Errorf("error kept at %s bytes", got)
	}
}

// The migration's run ends at 100 finished tasks per arm, not at a date.
func TestPrepEvidenceRunTarget(t *testing.T) {
	e := newEnv(t)
	run, err := e.s.liveExperimentRun(e.ctx, ExpPrepEvidence)
	if err != nil || run == nil || run.Target != 100 || run.EndsAt != nil {
		t.Fatalf("run %+v %v", run, err)
	}
	if d, _ := experimentDef(ExpPrepEvidence); !strings.HasPrefix(d.Exposure, "the agent holding the task fetched ") {
		t.Errorf("exposure: %q", d.Exposure)
	}
}

// The results say what each arm's compared tasks were at first exposure, from
// the run's own snapshots (WP-ADR157).
func TestPrepEvidenceBalanceNotes(t *testing.T) {
	e := newEvidenceEnv(t)
	ue := &uaEnv{env: e.env, me: e.admin}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := e.s.db.W.ExecContext(e.ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	old := e.one(`SELECT id FROM experiment_runs WHERE experiment = ? AND stopped_at IS NULL`, ExpPrepEvidence)
	if _, err := e.s.StopExperiment(e.ctx, e.admin, ExpPrepEvidence, ""); err != nil {
		t.Fatal(err)
	}
	e.setClock(e.clockNow().Add(-20 * time.Hour))
	if _, err := e.s.StartExperiment(e.ctx, e.admin, ExpPrepEvidence, ExperimentStart{}); err != nil {
		t.Fatal(err)
	}
	e.setClock(e.clockNow().Add(20 * time.Hour))
	run := e.one(`SELECT id FROM experiment_runs WHERE experiment = ? AND stopped_at IS NULL`, ExpPrepEvidence)
	// The migration's run started on the real clock, later than the test's.
	exec(`UPDATE experiment_runs SET started_at = 0 WHERE id = ?`, old)
	n := 0
	unit := func(arm string, exposed, used, done bool) int64 {
		t.Helper()
		n++
		i := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: fmt.Sprint("Unit ", n)})
		var at any
		if exposed {
			at = e.s.nowMS()
		}
		exec(`INSERT INTO experiment_units(run_id, issue_id, arm, via, assigned_at, exposed_at) VALUES (?, ?, ?, 'prep', 0, ?)`, run, i.ID, arm, at)
		if used {
			cli := fmt.Sprint("s", n)
			ue.run(ue.session(e.admin, "m1", cli, 0), i.ID, ue.hour(-10), ue.hour(-9))
			ue.ingest(e.admin, "c"+cli, "m1", UsageChunk{Messages: []UsageMessageRow{msg(ue.hour(-10), cli, "acme/web", "claude-opus-5-5", "main", 1, 1000, 100, 0, 0, 0)}})
		}
		if done {
			status := "done"
			if _, err := e.s.UpdateIssue(e.ctx, e.admin, i.Key, IssuePatch{Status: &status}); err != nil {
				t.Fatal(err)
			}
		}
		return i.ID
	}
	snapshot := func(run string, id int64, typ string, priority int, risk string, evidence, claims, dropped int) {
		t.Helper()
		exec(`INSERT INTO prep_evidence_units(run_id, issue_id, issue_type, priority, text_len, text_hash, candidates, evidence_n, claims_n, dropped, risk, evidence, model, at)
			VALUES (?, ?, ?, ?, 1, 'h', 5, ?, ?, ?, ?, '{}', 'm', 0)`, run, id, typ, priority, evidence, claims, dropped, risk)
	}
	a := unit("shown", true, true, true)
	snapshot(run, a, "bug", 3, "high", 2, 0, 1)
	b := unit("shown", true, true, false)
	snapshot(run, b, "task", 2, "low", 0, 1, 0)
	snapshot(old, unit("shown", true, true, true), "story", 4, "medium", 9, 9, 9)  // another run's
	snapshot(run, unit("shown", true, false, true), "story", 4, "medium", 9, 9, 9) // not measured
	snapshot(run, unit("shown", false, true, true), "story", 4, "medium", 9, 9, 9) // not exposed
	snapshot(run, unit("none", true, true, false), "task", 0, "medium", 3, 0, 0)

	check := func(scope string, want ...string) {
		t.Helper()
		r, err := e.s.ExperimentResults(e.ctx, e.admin, ExpPrepEvidence, 0, scope)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, note := range r.Notes {
			if strings.HasPrefix(note, "At first exposure") {
				got = append(got, note)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s:\n got %q\nwant %q", scope, got, want)
		}
	}
	control := "At first exposure, Evidence found, not shown (1 task): 1 with evidence items, 0 with a claim; mean 3.0 items kept and 0.0 dropped; risk medium 1; types task 1; priority none 1."
	check("all", control,
		"At first exposure, Evidence shown (3 tasks): 1 with evidence items, 1 with a claim; mean 1.5 items kept and 0.5 dropped; risk high 1, low 1; types bug 1, task 1; priority high 1, medium 1; 1 without a snapshot.")
	// Only finished tasks: the control has none, so no note.
	check("done", "At first exposure, Evidence shown (2 tasks): 1 with evidence items, 0 with a claim; mean 2.0 items kept and 1.0 dropped; risk high 1; types bug 1; priority high 1; 1 without a snapshot.")
	exec(`DELETE FROM prep_evidence_units WHERE run_id = ? AND issue_id = ?`, run, a)
	check("done", "At first exposure, Evidence shown (2 tasks): 2 without a snapshot.")
	exec(`UPDATE experiment_runs SET weights = '{"none":0,"shown":1}' WHERE id = ?`, run)
	check("all",
		"At first exposure, Evidence shown (3 tasks): 0 with evidence items, 1 with a claim; mean 1.0 items kept and 0.0 dropped; risk low 1; types task 1; priority medium 1; 2 without a snapshot.")
}

// A decision reads as reviewed when a person approved it, not when it only
// counts on a person's request: nobody approved that one.
func TestPrepEvidenceDecisionReviewed(t *testing.T) {
	e := newEvidenceEnv(t)
	write := func(name, forTask string) {
		title, kind, body := "Merge checks "+name, "decision", "## Assumptions\n- Merge checks are quick."
		n, err := e.s.CreateDesign(e.ctx, e.ag, DesignInput{Title: &title, Body: &body, Kind: &kind, Project: &e.p.Key, For: forTask})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		e.notes[name] = n
	}
	write("approved", "")
	write("proposed", "")
	write("requested", e.task.Key)
	if !e.notes["requested"].Approved || e.notes["proposed"].Approved {
		t.Fatalf("approved: requested %v, proposed %v", e.notes["requested"].Approved, e.notes["proposed"].Approved)
	}
	if _, err := e.s.ApproveDesign(e.ctx, e.admin, e.notes["approved"].ID, ""); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range e.candidates(e.task) {
		got[c.Ref] = c.By
	}
	for name, want := range map[string]string{"approved": "reviewed by a person", "proposed": "agent, unreviewed", "requested": "agent, unreviewed"} {
		if got[e.code(name)] != want {
			t.Errorf("%s: by %q; want %q", name, got[e.code(name)], want)
		}
	}
}

// Headings that are nearly "Assumptions" take no place from a decision that has
// the section, however many there are and however it is spaced.
func TestPrepEvidenceNearMissHeadings(t *testing.T) {
	filler := strings.Repeat("unrelated filler words here ", 200)
	nearMiss := "## Assumptions and risks\nmerge checks merge checks merge checks"
	check := func(t *testing.T, e *evidenceEnv, name string) {
		t.Helper()
		got := e.candidates(e.task)
		if len(got) != 1 || got[0].Ref != e.code(name) {
			t.Errorf("candidates %+v; want %s", got, e.code(name))
		}
	}
	t.Run("matched", func(t *testing.T) {
		e := newEvidenceEnv(t)
		for n := range 21 {
			e.decision(fmt.Sprint("near", n), e.p.Key, "Merge checks", nearMiss)
		}
		e.decision("real", e.p.Key, "Gate", "## Assumptions\n- A merge waits. "+filler)
		check(t, e, "real")
	})
	t.Run("linked", func(t *testing.T) {
		e := newEvidenceEnv(t)
		link := func(name, body string) {
			e.decision(name, e.p.Key, "Zebra", body)
			e.linkNote(name, e.task.ID)
		}
		link("older", "## Assumptions\n- Zebras sleep.")
		for n := range 21 {
			link(fmt.Sprint("near", n), "## Assumptions and risks\nZebra.")
		}
		check(t, e, "older")
	})
	t.Run("two spaces", func(t *testing.T) {
		e := newEvidenceEnv(t)
		e.decision("spaced", e.p.Key, "Merge checks", "##  Assumptions\n- Merge checks are quick.")
		check(t, e, "spaced")
	})
}

// A run that leaves a project out gives its tasks no evidence, so none is
// checked for them, first or again.
func TestPrepEvidenceRunProjects(t *testing.T) {
	e := newRetryEnv(t)
	e.llm.fail = errors.New("boom")
	e.prep(e.task.Key)
	e.llm.fail = nil
	other := e.project(e.admin, "Elsewhere", "")
	restart := func(projects ...string) {
		t.Helper()
		if _, err := e.s.StopExperiment(e.ctx, e.admin, ExpPrepEvidence, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := e.s.StartExperiment(e.ctx, e.admin, ExpPrepEvidence, ExperimentStart{Projects: projects}); err != nil {
			t.Fatal(err)
		}
	}
	restart(other.Key)
	asked := e.asked()
	row := func() string {
		return e.one(`SELECT state || ' ' || created_at FROM task_prep_evidence WHERE issue_id = ?`, e.task.ID)
	}
	before := row()
	e.later(prepRetryFailed + time.Minute)
	if e.polled(0); e.asked() != asked || row() != before {
		t.Errorf("retry: asked %d (was %d), row %s (was %s)", e.asked(), asked, row(), before)
	}
	out := e.issue(e.admin, IssueInput{Project: e.p.Key, Title: "Left out", Description: "The checks are slow."})
	e.prep(out.Key)
	if e.asked() != asked || e.row(out.ID) != "none" {
		t.Errorf("left out: asked %d (was %d), row %s", e.asked(), asked, e.row(out.ID))
	}
	in := e.issue(e.admin, IssueInput{Project: other.Key, Title: "Taken in", Description: "The checks are slow."})
	e.prep(in.Key)
	if e.asked() != asked+1 || e.row(in.ID) != "ready" {
		t.Errorf("taken in: asked %d (was %d), row %s", e.asked(), asked, e.row(in.ID))
	}
	// A run that names the project takes it in, whatever the case.
	restart(strings.ToLower(e.p.Key))
	e.polled(0)
	if got := e.row(e.task.ID); got != "ready" {
		t.Errorf("retry in a run that names the project: row %s", got)
	}
}

// The experiment says which commands print the evidence or leave it out.
func TestPrepEvidenceWhere(t *testing.T) {
	d, ok := experimentDef(ExpPrepEvidence)
	if !ok {
		t.Fatal("no definition")
	}
	for _, c := range []string{"wp next", "wp take", "wp show", "wp brief", "wp prep show"} {
		if !strings.Contains(d.Where, c) {
			t.Errorf("Where doesn't name %s: %s", c, d.Where)
		}
	}
}
