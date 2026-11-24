package core

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// On the to-do paths a finding filed as an issue (confirmed, linked, medium or lower) is dropped; a confirmed one with no issue is kept.

type filedEnv struct {
	*apEnv
	prID, forge, issue int64
}

// filedPR is an env with a merged pull request #7 and an issue to file under.
func filedPR(t *testing.T) *filedEnv {
	t.Helper()
	a, _ := auditAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	a.linkPR(first.ID, 7, "aaa")
	e := &filedEnv{apEnv: a, issue: first.ID}
	if err := a.s.db.R.QueryRow(`SELECT id, forge_id FROM pull_requests WHERE number = 7`).Scan(&e.prID, &e.forge); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.db.W.Exec(`UPDATE pull_requests SET state = 'merged' WHERE id = ?`, e.prID); err != nil {
		t.Fatal(err)
	}
	return e
}

type seedFinding struct {
	severity string
	filed    bool
	state    string
	cat      string
	dupOf    string
}

// issueIn makes an issue in a status of this category and returns its ID.
func (e *filedEnv) issueIn(cat string) int64 {
	e.t.Helper()
	made := e.apEnv.issue(e.admin, IssueInput{Project: e.p.Key, Title: "Filed " + cat, Agent: "claude"})
	e.setCategory(made.ID, cat)
	return made.ID
}

func (e *filedEnv) setCategory(issue int64, cat string) {
	e.t.Helper()
	if _, err := e.s.db.W.Exec(`UPDATE issues SET status_id = (SELECT id FROM statuses WHERE project_id = ? AND category = ? LIMIT 1) WHERE id = ?`, e.p.ID, cat, issue); err != nil {
		e.t.Fatal(err)
	}
}

// duplicateOf makes an issue in the canceled category that duplicates a
// new issue in dupCat ("A duplicates B": A is the source, B the one kept).
func (e *filedEnv) duplicateOf(dupCat string) int64 {
	e.t.Helper()
	dup := e.issueIn(CatCanceled)
	kept := e.issueIn(dupCat)
	if _, err := e.s.db.W.Exec(`INSERT INTO issue_relations(source_id, target_id, kind, created_at) VALUES (?, ?, 'duplicates', ?)`, dup, kept, e.s.nowMS()); err != nil {
		e.t.Fatal(err)
	}
	return dup
}

// audit seeds a finished audit on the pull request with these findings (path
// app/f<N>.go, in order) and returns its ID.
func (e *filedEnv) audit(agent, key string, fs ...seedFinding) int64 {
	e.t.Helper()
	res, err := e.s.db.W.Exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, pr_id, pr_number, ref, status, model, models, title, owner_id, created_at, requested_agent, request_key)
		VALUES (?, ?, 'acme/app', 'pr', ?, 7, 'aaa', 'done', 'm', '[]', 'acme/app#7', ?, ?, ?, ?)`, e.p.ID, e.forge, e.prID, e.admin.User.ID, e.s.nowMS(), agent, key)
	if err != nil {
		e.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	for i, f := range fs {
		state := cmp(f.state, "confirmed")
		var issue any
		if f.filed {
			issue = e.issue
			switch {
			case f.dupOf != "":
				issue = e.duplicateOf(f.dupOf)
			case f.cat != "":
				issue = e.issueIn(f.cat)
			}
		}
		if _, err := e.s.db.W.Exec(`INSERT INTO code_audit_findings(audit_id, ordinal, title, path, line, severity, problem, state, issue_id) VALUES (?, ?, ?, ?, 1, ?, 'p', ?, ?)`,
			id, i+1, fmt.Sprintf("Finding %d", i+1), fmt.Sprintf("app/f%d.go", i+1), f.severity, state, issue); err != nil {
			e.t.Fatal(err)
		}
	}
	return id
}

var filedMedium, unfiledMedium = seedFinding{severity: "medium", filed: true}, seedFinding{severity: "medium"}

func TestFiledFindingLeftOpen(t *testing.T) {
	e := filedPR(t)
	id := e.audit(leftoverAgent, "", filedMedium, unfiledMedium)
	out := e.s.leftOpen(e.ctx, e.prID)
	if out == nil || len(out.Audits) != 1 || out.Audits[0].ID != id || out.Audits[0].Findings != 2 || out.Audits[0].Open != 1 {
		t.Fatalf("left open = %+v", out)
	}
}

func TestFiledFindingOpenNitpicks(t *testing.T) {
	e := filedPR(t)
	e.audit(nitpickAgent, "", filedMedium, unfiledMedium)
	list, err := e.s.OpenNitpicks(e.ctx, e.admin, e.p.Key, []string{"app/f1.go", "app/f2.go"}, 0)
	if err != nil || len(list) != 1 || list[0].Path != "app/f2.go" {
		t.Fatalf("open nitpicks = %+v, %v", list, err)
	}
}

// A filed medium finding beside an open low one: the audit is not asked
// for, though the open low one would make a task if it were. The control
// audit holds an unfiled medium one and is asked for.
func TestFiledFindingMergedRoundAuditTasks(t *testing.T) {
	e := filedPR(t)
	low := seedFinding{severity: "low", state: "open"}
	filed := e.audit("", "review-round:1:sonnet", filedMedium, low)
	unfiled := e.audit("", "review-round:2:sonnet", unfiledMedium, low)
	e.s.mergedRoundAuditTasks(e.ctx, 0)
	var f, u int64
	if err := e.s.db.R.QueryRow(`SELECT coalesce((SELECT fix_issue_id FROM code_audits WHERE id = ?), 0), coalesce((SELECT fix_issue_id FROM code_audits WHERE id = ?), 0)`, filed, unfiled).Scan(&f, &u); err != nil {
		t.Fatal(err)
	}
	if f != 0 || u == 0 {
		t.Fatalf("fix tasks: filed audit %d, unfiled audit %d", f, u)
	}
}

func TestFiledFindingListAudits(t *testing.T) {
	e := filedPR(t)
	id := e.audit("", "", filedMedium, unfiledMedium)
	list, err := e.s.ListAudits(e.ctx, e.admin, AuditFilter{})
	if err != nil || len(list) != 1 || list[0].ID != id || list[0].Counts.Total != 2 || list[0].Counts.Open != 1 {
		t.Fatalf("audits = %+v, %v", list, err)
	}
}

func TestFiledFindingLedgerDealt(t *testing.T) {
	e := filedPR(t)
	id := e.audit("", "", filedMedium, unfiledMedium)
	a := &CodeAudit{ID: id}
	if err := e.s.loadFindings(e.ctx, a); err != nil || len(a.Findings) != 2 {
		t.Fatalf("findings = %+v, %v", a.Findings, err)
	}
	if got := e.s.findingItem(e.ctx, id, a.Findings[0]); !got.Dealt {
		t.Error("a filed finding is not dealt with")
	}
	if got := e.s.findingItem(e.ctx, id, a.Findings[1]); got.Dealt {
		t.Error("an unfiled confirmed finding is dealt with")
	}
}

// WP-1554: a filed finding stays filed while its issue is open or done, and
// is to do again once the issue is canceled (won't do, duplicate).
var (
	filedCanceled = seedFinding{severity: "medium", filed: true, cat: CatCanceled}
	filedDone     = seedFinding{severity: "medium", filed: true, cat: CatDone}
)

func TestCanceledFindingLeftOpen(t *testing.T) {
	e := filedPR(t)
	e.audit(leftoverAgent, "", filedCanceled, filedDone)
	out := e.s.leftOpen(e.ctx, e.prID)
	if out == nil || len(out.Audits) != 1 || out.Audits[0].Findings != 2 || out.Audits[0].Open != 1 {
		t.Fatalf("left open = %+v", out)
	}
}

func TestCanceledFindingOpenNitpicks(t *testing.T) {
	e := filedPR(t)
	e.audit(nitpickAgent, "", filedCanceled, filedDone)
	list, err := e.s.OpenNitpicks(e.ctx, e.admin, e.p.Key, []string{"app/f1.go", "app/f2.go"}, 0)
	if err != nil || len(list) != 1 || list[0].Path != "app/f1.go" {
		t.Fatalf("open nitpicks = %+v, %v", list, err)
	}
}

func TestCanceledFindingMergedRoundAuditTasks(t *testing.T) {
	e := filedPR(t)
	low := seedFinding{severity: "low", state: "open"}
	canceled := e.audit("", "review-round:1:sonnet", filedCanceled, low)
	done := e.audit("", "review-round:2:sonnet", filedDone, low)
	e.s.mergedRoundAuditTasks(e.ctx, 0)
	var c, d int64
	if err := e.s.db.R.QueryRow(`SELECT coalesce((SELECT fix_issue_id FROM code_audits WHERE id = ?), 0), coalesce((SELECT fix_issue_id FROM code_audits WHERE id = ?), 0)`, canceled, done).Scan(&c, &d); err != nil {
		t.Fatal(err)
	}
	if c == 0 || d != 0 {
		t.Fatalf("fix tasks: canceled audit %d, done audit %d", c, d)
	}
}

func TestCanceledFindingListAudits(t *testing.T) {
	e := filedPR(t)
	e.audit("", "", filedCanceled, filedDone)
	list, err := e.s.ListAudits(e.ctx, e.admin, AuditFilter{})
	if err != nil || len(list) != 1 || list[0].Counts.Total != 2 || list[0].Counts.Open != 1 {
		t.Fatalf("audits = %+v, %v", list, err)
	}
}

func TestCanceledFindingLedgerDealt(t *testing.T) {
	e := filedPR(t)
	id := e.audit("", "", filedCanceled, filedDone)
	a := &CodeAudit{ID: id}
	if err := e.s.loadFindings(e.ctx, a); err != nil || len(a.Findings) != 2 {
		t.Fatalf("findings = %+v, %v", a.Findings, err)
	}
	if got := e.s.findingItem(e.ctx, id, a.Findings[0]); got.Dealt {
		t.Error("a finding whose issue is canceled is dealt with")
	}
	if got := e.s.findingItem(e.ctx, id, a.Findings[1]); !got.Dealt {
		t.Error("a finding whose issue is done is not dealt with")
	}
}

// The round packet reads the SQL form of findingFiled.
func TestCanceledFindingAgainFindings(t *testing.T) {
	e := filedPR(t)
	id := e.audit("", "", filedCanceled, filedDone)
	got, err := e.s.againFindings(e.ctx, id, 0)
	if err != nil || len(got) != 2 {
		t.Fatalf("again findings = %+v, %v", got, err)
	}
	if got[0].filed || !got[1].filed {
		t.Errorf("filed = %v (canceled issue), %v (done issue)", got[0].filed, got[1].filed)
	}
}

// WP-1554, audit 1296: a canceled issue that duplicates one that is not
// canceled still tracks the work, one hop only.
var (
	filedDupOfOpen     = seedFinding{severity: "medium", filed: true, dupOf: CatTodo}
	filedDupOfCanceled = seedFinding{severity: "medium", filed: true, dupOf: CatCanceled}
)

// A finding's filing is void when its issue is canceled and no live issue
// stands for it; loadFindings says so, and findingToDo follows.
func TestFilingVoidRule(t *testing.T) {
	for _, c := range []struct {
		name string
		f    seedFinding
		void bool
	}{
		{"canceled", filedCanceled, true},
		{"done", filedDone, false},
		{"in progress", seedFinding{severity: "medium", filed: true, cat: CatInProgress}, false},
		{"backlog", seedFinding{severity: "medium", filed: true, cat: CatBacklog}, false},
		{"duplicate of an open issue", filedDupOfOpen, false},
		{"duplicate of a done issue", seedFinding{severity: "medium", filed: true, dupOf: CatDone}, false},
		{"duplicate of a canceled issue", filedDupOfCanceled, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := filedPR(t)
			id := e.audit("", "", c.f)
			a := &CodeAudit{ID: id}
			if err := e.s.loadFindings(e.ctx, a); err != nil || len(a.Findings) != 1 {
				t.Fatalf("findings = %+v, %v", a.Findings, err)
			}
			f := a.Findings[0]
			if f.FilingVoid != c.void || findingToDo(f) != c.void {
				t.Errorf("void = %v, to do = %v; want %v", f.FilingVoid, findingToDo(f), c.void)
			}
			if a.Counts.Open != map[bool]int{true: 1, false: 0}[c.void] {
				t.Errorf("open = %d", a.Counts.Open)
			}
		})
	}
}

// Reopening the canceled issue files the finding again.
func TestFilingVoidEndsWhenTheIssueReopens(t *testing.T) {
	e := filedPR(t)
	id := e.audit("", "", filedCanceled)
	var issue int64
	if err := e.s.db.R.QueryRow(`SELECT issue_id FROM code_audit_findings WHERE audit_id = ?`, id).Scan(&issue); err != nil {
		t.Fatal(err)
	}
	a := &CodeAudit{ID: id}
	if err := e.s.loadFindings(e.ctx, a); err != nil || !findingToDo(a.Findings[0]) {
		t.Fatalf("canceled: %+v, %v", a.Findings, err)
	}
	e.setCategory(issue, CatTodo)
	a = &CodeAudit{ID: id}
	if err := e.s.loadFindings(e.ctx, a); err != nil || findingToDo(a.Findings[0]) {
		t.Fatalf("reopened: %+v, %v", a.Findings, err)
	}
	if out := e.s.leftOpen(e.ctx, e.prID); out != nil && len(out.Audits) > 0 && out.Audits[0].Open != 0 {
		t.Errorf("left open = %+v", out)
	}
}

// The SQL form agrees, in each query that embeds it.
func TestFilingVoidSQLPaths(t *testing.T) {
	e := filedPR(t)
	e.audit(leftoverAgent, "", filedCanceled, filedDupOfOpen, filedDupOfCanceled)
	out := e.s.leftOpen(e.ctx, e.prID)
	if out == nil || len(out.Audits) != 1 || out.Audits[0].Findings != 3 || out.Audits[0].Open != 2 {
		t.Fatalf("left open = %+v", out)
	}
}

func TestFilingVoidAgainFindings(t *testing.T) {
	e := filedPR(t)
	id := e.audit("", "", filedCanceled, filedDupOfOpen, filedDupOfCanceled)
	got, err := e.s.againFindings(e.ctx, id, 0)
	if err != nil || len(got) != 3 {
		t.Fatalf("again findings = %+v, %v", got, err)
	}
	for i, want := range []struct{ filed, void bool }{{false, true}, {true, false}, {false, true}} {
		if got[i].filed != want.filed || got[i].void != want.void {
			t.Errorf("finding %d: filed %v void %v, want %v", i+1, got[i].filed, got[i].void, want)
		}
	}
	text := got[0].mark()
	if !strings.Contains(text, "closed unfixed") || !strings.Contains(text, got[0].issue) {
		t.Errorf("mark = %q", text)
	}
}

func TestFilingVoidCheckClaims(t *testing.T) {
	e := filedPR(t)
	id := e.audit("", "", filedCanceled, filedDupOfOpen)
	_, ids, err := e.s.checkClaims(e.ctx, id)
	if want := []string{fmt.Sprintf("%d.2", id)}; err != nil || !slices.Equal(ids, want) {
		t.Fatalf("claims = %v, %v; want %v", ids, err, want)
	}
}

func TestFilingVoidAgainTodo(t *testing.T) {
	e := filedPR(t)
	id := e.audit("", "", filedCanceled, filedDupOfOpen)
	todo, _, err := e.s.againTodoLines(e.ctx, id)
	if want := []string{fmt.Sprintf("%d.1", id)}; err != nil || !slices.Equal(todo, want) {
		t.Fatalf("to do = %v, %v; want %v", todo, err, want)
	}
}

func TestFilingVoidMarkCounts(t *testing.T) {
	e := filedPR(t)
	id := e.audit("", "", filedCanceled, filedDupOfOpen, filedDone)
	total, fixed, rejected, filed, err := e.s.markCounts(e.ctx, id, 0)
	if err != nil || total != 3 || fixed != 0 || rejected != 0 || filed != 2 {
		t.Fatalf("counts = %d %d %d %d, %v", total, fixed, rejected, filed, err)
	}
}

func TestFilingVoidFindingText(t *testing.T) {
	var b strings.Builder
	writeAuditFinding(&b, CodeAuditFinding{ID: "7.1", Severity: "medium", Title: "t", State: "confirmed", Issue: "WP-5", FilingVoid: true})
	for _, want := range []string{"WP-5", "closed unfixed", "to do here"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("text lacks %q: %s", want, b.String())
		}
	}
	b.Reset()
	writeAuditFinding(&b, CodeAuditFinding{ID: "7.1", Severity: "medium", Title: "t", State: "confirmed", Issue: "WP-5"})
	if strings.Contains(b.String(), "closed unfixed") {
		t.Errorf("a live filing reads as void: %s", b.String())
	}
}

// Filing into an issue that is already closed unfixed is refused, with a way
// out; one into a duplicate of a live issue, or a done one, is taken.
func TestMarkRefusesAVoidFiling(t *testing.T) {
	e := filedPR(t)
	id := e.audit("", "", unfiledMedium, unfiledMedium, unfiledMedium)
	key := func(issue int64) string {
		var k string
		if err := e.s.db.R.QueryRow(`SELECT p.key || '-' || i.number FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.id = ?`, issue).Scan(&k); err != nil {
			t.Fatal(err)
		}
		return k
	}
	mark := func(n int, issue int64) error {
		_, err := e.s.MarkAuditFindings(e.ctx, e.admin, MarkFindings{IDs: []string{fmt.Sprintf("%d.%d", id, n)}, State: "confirmed", Issue: key(issue)})
		return err
	}
	err := mark(1, e.issueIn(CatCanceled))
	if err == nil || !strings.Contains(err.Error(), "canceled") || !strings.Contains(err.Error(), "reject") {
		t.Fatalf("a canceled issue: %v", err)
	}
	if err := mark(2, e.duplicateOf(CatTodo)); err != nil {
		t.Errorf("a duplicate of an open issue: %v", err)
	}
	if err := mark(3, e.issueIn(CatDone)); err != nil {
		t.Errorf("a done issue: %v", err)
	}
	var state string
	if err := e.s.db.R.QueryRow(`SELECT state FROM code_audit_findings WHERE audit_id = ? AND ordinal = 1`, id).Scan(&state); err != nil || state != "confirmed" {
		t.Errorf("a refused mark changed the finding: %q, %v", state, err)
	}
}
