package core

import (
	"fmt"
	"testing"
)

// WP-1547 / audit 1278.6: each path that lists what is still to do drops a
// finding filed as an issue (confirmed, issue linked, medium or lower) and
// keeps a confirmed one with no issue. Each test fails if its path goes back
// to counting every open or confirmed finding.

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
	filed    bool // confirmed with the issue linked; else confirmed with none
	state    string
	cat      string // the linked issue's status category when filed ("": the shared issue)
}

// issueIn makes an issue in a status of this category and returns its ID.
func (e *filedEnv) issueIn(cat string) int64 {
	e.t.Helper()
	made := e.apEnv.issue(e.admin, IssueInput{Project: e.p.Key, Title: "Filed " + cat, Agent: "claude"})
	if _, err := e.s.db.W.Exec(`UPDATE issues SET status_id = (SELECT id FROM statuses WHERE project_id = ? AND category = ? LIMIT 1) WHERE id = ?`, e.p.ID, cat, made.ID); err != nil {
		e.t.Fatal(err)
	}
	return made.ID
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
			if f.cat != "" {
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
