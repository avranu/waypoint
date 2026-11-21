package core

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// At the wp ship that ends a task's first review before its pull request,
// findings scored serious, or fixes that look partial, get a second round
// before the pull request opens; that round's wp ship opens it, with no
// third. Scored low, the first round's wp ship opens it as before (WP-695).
// Findings the session rejected, every one, get no second round and no
// model is asked; and from a reviewer whose record doesn't back its claims,
// only risky fixes get one (WP-1001).
func TestReviewAgainBeforePR(t *testing.T) {
	const trusted, untrusted = "claude-haiku-5-5", "gateway/unorouter/gemini-3.6-flash:free"
	type record struct {
		n       int
		state   string // what the session marked them
		model   string // "": the audit's
		noRound bool   // an audit no review round had
	}
	for _, tc := range []struct {
		name          string
		issues, fixes string
		arm           string // "": nothing recorded
		model         string // the audit's
		record        record // earlier findings
		allRejected   bool   // the session rejects both findings
		pastCap       bool   // reviewAgainFindings more, rejected, and one fixed past what the model reads
		wait          bool   // the model can't answer now
		refuted       bool   // the check against the code refuted the fixed finding: no signal, and its severity reaches no bar
		belowFloor    bool   // the fixed finding is labelled low: no signal, the scores decide
		afterPR       bool   // the run's newest round is one after a pull request
		trustFails    bool   // the reviewer's record can't be read
	}{
		{name: "serious", issues: "critical", fixes: "complete", arm: AgainReview, model: trusted},
		{name: "partial fixes", issues: "moderate", fixes: "partial", arm: AgainReview, model: trusted},
		{name: "enough", issues: "minor", fixes: "complete", arm: AgainEnough, model: trusted, belowFloor: true},
		{name: "a refuted finding's severity reaches no bar", issues: "critical", fixes: "complete", arm: AgainEnough, model: trusted, refuted: true},
		// What the session changed for a refuted finding is still code no
		// reviewer has read: the fixes call counts it.
		{name: "a refuted finding's fixes still count", issues: "critical", fixes: "partial", arm: AgainReview, model: trusted, refuted: true},
		{name: "a fixed finding over the floor scored minor", issues: "minor", fixes: "complete", arm: AgainReview, model: trusted},
		{name: "a signal from an untrusted reviewer", issues: "critical", fixes: "complete", arm: AgainReview, model: untrusted},
		{name: "a runner's short name", issues: "critical", fixes: "complete", arm: AgainReview, model: "claude/sonnet"},
		{name: "every finding rejected", issues: "critical", fixes: "risky", arm: AgainRejected, model: trusted, allRejected: true},
		{name: "untrusted reviewer", issues: "critical", fixes: "partial", arm: AgainUntrusted, model: untrusted, belowFloor: true},
		// A record the rule needs and can't read refuses the ship: nothing
		// is decided, and no pull request opens on a rule not applied.
		{name: "the reviewer's record unreadable", issues: "critical", fixes: "partial", model: untrusted, belowFloor: true, trustFails: true},
		// One it doesn't need (a signal calls the round) stops nothing.
		{name: "the reviewer's record unreadable, a signal", issues: "critical", fixes: "complete", arm: AgainReview, model: untrusted, trustFails: true},
		{name: "untrusted reviewer, risky fixes", issues: "minor", fixes: "risky", arm: AgainReview, model: untrusted},
		{name: "a seed whose findings didn't hold", issues: "critical", fixes: "partial", arm: AgainUntrusted, model: trusted, record: record{n: 20, state: "rejected"}, belowFloor: true},
		{name: "a model whose findings held", issues: "critical", fixes: "complete", arm: AgainReview, model: untrusted, record: record{n: 20, state: "fixed"}},
		{name: "another model's record", issues: "critical", fixes: "complete", arm: AgainReview, model: trusted, record: record{n: 20, state: "rejected", model: untrusted}},
		{name: "a record outside review rounds", issues: "critical", fixes: "complete", arm: AgainReview, model: trusted, record: record{n: 20, state: "rejected", noRound: true}},
		{name: "a finding past what the model reads stands", issues: "critical", fixes: "risky", arm: AgainReview, model: trusted, allRejected: true, pastCap: true},
		{name: "model waiting", issues: "critical", fixes: "risky", arm: AgainReview, model: trusted, wait: true},
		{name: "model waiting, no signal", issues: "critical", fixes: "risky", model: trusted, wait: true, belowFloor: true},
		{name: "round after a pull request", issues: "critical", fixes: "risky", model: trusted, afterPR: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, box, g := firstEnv(t)
			j := &jevScript{choice: map[string]string{"": tc.issues, "fixes": tc.fixes}}
			if tc.wait {
				j.err = &aiWaitError{}
			}
			a.s.LLM = &gatewayScoringLLM{taskLLM: a.s.LLM.(*taskLLM), jevScript: j}
			if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Merge: true,
				Limits: AutopilotLimits{Review: ptrOf(true), SpacingMinutes: ptrOf(120)}}); err != nil {
				t.Fatal(err)
			}
			first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
			a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
			if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
				t.Fatal(err)
			}
			a.setClock(a.clockNow().Add(time.Minute))
			// One review round: wp ship, the Stop, the audit, its prompt.
			review := func(titles ...string) int64 {
				t.Helper()
				if res, err := a.ship(first.Key, ShipAuto); err != nil || res.Held == "" {
					t.Fatalf("ship = %+v, %v", res, err)
				}
				a.stopWith("Pushed; the review comes first.")
				round := a.step().reviewID.Int64
				a.tick(61 * time.Second)
				a.endAudit(box, "done", titles...)
				a.tick(time.Second)
				if len(a.jobs) == 0 {
					t.Fatal("no review job")
				}
				var in AutopilotJob
				_ = json.Unmarshal(a.jobs[len(a.jobs)-1].Input, &in)
				a.prompt(in.Review)
				a.jobDone(len(a.jobs), AutopilotJobResult{Outcome: "reviewing"})
				if r := a.round(round); r.state != RoundRunning {
					t.Fatalf("round %d: %+v", round, r)
				}
				return round
			}
			round := review("Merge loses an update", "A comment is out of date")
			var pastRound int64 // a round made up for the record, out of the way once the ship read it
			_, au := a.roundAuditRow(round)
			full, err := a.s.GetAudit(a.ctx, a.admin, au.ID)
			if err != nil || len(full.Findings) != 2 {
				t.Fatalf("audit = %+v %v", full, err)
			}
			if _, err := a.s.db.W.Exec(`UPDATE code_audits SET model = ? WHERE id = ?`, tc.model, au.ID); err != nil {
				t.Fatal(err)
			}
			if tc.record.n > 0 {
				// An earlier round's audit, its findings marked.
				var cols []string
				rows, err := a.s.db.R.Query(`SELECT name FROM pragma_table_info('code_audits') WHERE name NOT IN ('id', 'request_key')`)
				if err != nil {
					t.Fatal(err)
				}
				for rows.Next() {
					var c string
					_ = rows.Scan(&c)
					cols = append(cols, c)
				}
				_ = rows.Close()
				list := strings.Join(cols, ", ")
				res, err := a.s.db.W.Exec(`INSERT INTO code_audits(`+list+`) SELECT `+list+` FROM code_audits WHERE id = ?`, au.ID)
				if err != nil {
					t.Fatal(err)
				}
				earlier, _ := res.LastInsertId()
				if _, err := a.s.db.W.Exec(`UPDATE code_audits SET model = ? WHERE id = ?`, cmp(tc.record.model, tc.model), earlier); err != nil {
					t.Fatal(err)
				}
				for i := 1; i <= tc.record.n; i++ {
					if _, err := a.s.db.W.Exec(`INSERT INTO code_audit_findings(audit_id, ordinal, title, path, line, severity, problem, state) VALUES (?, ?, 'Wrong', 'a.go', 1, 'high', 'Wrong', ?)`, earlier, i, tc.record.state); err != nil {
						t.Fatal(err)
					}
				}
				if !tc.record.noRound {
					res, err := a.s.db.W.Exec(`INSERT INTO work_log(issue_id, kind, state, body, created_at, updated_at, audit_id) SELECT issue_id, 'review', 'done', '', created_at, updated_at, ? FROM work_log WHERE id = ?`, earlier, round)
					if err != nil {
						t.Fatal(err)
					}
					pastRound, _ = res.LastInsertId()
				}
			}
			if tc.pastCap {
				for i := 3; i <= reviewAgainFindings+3; i++ {
					sev, state := "low", "rejected"
					if i == reviewAgainFindings+3 {
						sev, state = "low", "fixed"
					}
					if _, err := a.s.db.W.Exec(`INSERT INTO code_audit_findings(audit_id, ordinal, title, path, line, severity, problem, state) VALUES (?, ?, 'Loud', 'a.go', 1, ?, 'Loud', ?)`, au.ID, i, sev, state); err != nil {
						t.Fatal(err)
					}
				}
			}
			mark, note := "fixed", "Merged under the row lock."
			if tc.allRejected {
				mark, note = "rejected", "Merged under the row lock already, so nothing to fix."
			}
			if _, err := a.s.MarkAuditFindings(a.ctx, a.ag, MarkFindings{IDs: []string{full.Findings[0].ID}, State: mark, Note: note}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.s.MarkAuditFindings(a.ctx, a.ag, MarkFindings{IDs: []string{full.Findings[1].ID}, State: "rejected", Note: "The comment says it already."}); err != nil {
				t.Fatal(err)
			}
			if tc.refuted {
				if _, err := a.s.db.W.Exec(`UPDATE code_audit_findings SET verdict = ? WHERE audit_id = ? AND ordinal = 1`, VerdictRefuted, au.ID); err != nil {
					t.Fatal(err)
				}
			}
			if tc.belowFloor {
				if _, err := a.s.db.W.Exec(`UPDATE code_audit_findings SET severity = 'low' WHERE audit_id = ? AND ordinal = 1`, au.ID); err != nil {
					t.Fatal(err)
				}
			}

			if tc.afterPR {
				// The run's first round is over, and its newest is one on a
				// pull request since closed: that round's ship is no first
				// round's, so nothing is scored (audit 626).
				var cols []string
				rows, err := a.s.db.R.Query(`SELECT name FROM pragma_table_info('work_log') WHERE name != 'id'`)
				if err != nil {
					t.Fatal(err)
				}
				for rows.Next() {
					var c string
					_ = rows.Scan(&c)
					cols = append(cols, c)
				}
				_ = rows.Close()
				list := strings.Join(cols, ", ")
				if _, err := a.s.db.W.Exec(`INSERT INTO work_log(`+list+`) SELECT `+list+` FROM work_log WHERE id = ?`, round); err != nil {
					t.Fatal(err)
				}
				if _, err := a.s.db.W.Exec(`UPDATE work_log SET before_pr = '' WHERE id = (SELECT max(id) FROM work_log WHERE kind = 'review')`); err != nil {
					t.Fatal(err)
				}
				if _, err := a.s.db.W.Exec(`UPDATE work_log SET state = 'done' WHERE id = ?`, round); err != nil {
					t.Fatal(err)
				}
				res, err := a.ship(first.Key, ShipPR)
				var n int
				if err := a.s.db.R.QueryRow(`SELECT count(*) FROM review_again_decisions`).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if err != nil || res.Held != "" || len(j.asked()) != 0 || n != 0 {
					t.Fatalf("after a pull request: %+v, %v; asked %d, %d decisions", res, err, len(j.asked()), n)
				}
				return
			}
			if tc.trustFails {
				a.s.againTrustFault = errors.New("database is locked")
			}
			// A decision recorded tells the loop's health check, for the
			// task's project (review_health.go).
			var recordedMu sync.Mutex
			var recorded []int64
			prev := a.s.Hub.observe
			a.s.Hub.observe = func(e *Event) {
				prev(e)
				if e.Kind == EvReviewLoopRecorded {
					recordedMu.Lock()
					recorded = append(recorded, e.ProjectID)
					recordedMu.Unlock()
				}
			}
			// The session's rejections are checked first, and found right.
			res, err := a.shipChecked(box, first.Key)
			if tc.trustFails && tc.arm == "" {
				var n int
				if qerr := a.s.db.R.QueryRow(`SELECT count(*) FROM review_again_decisions`).Scan(&n); qerr != nil {
					t.Fatal(qerr)
				}
				if !errors.Is(err, errAgainRead) || !strings.Contains(err.Error(), "database is locked") || !strings.Contains(err.Error(), "--skip-review") || len(g.created) != 0 || n != 0 {
					t.Fatalf("record unreadable: %+v, %v (opened %d, %d decisions)", res, err, len(g.created), n)
				}
				// Read again once the record can be, the round is decided.
				a.s.againTrustFault = nil
				if res, err = a.ship(first.Key, ShipPR); err != nil || res.Pull == nil {
					t.Fatalf("after the record could be read: %+v, %v", res, err)
				}
				var arm string
				if err := a.s.db.R.QueryRow(`SELECT arm FROM review_again_decisions WHERE round_id = ?`, round).Scan(&arm); err != nil || arm != AgainUntrusted {
					t.Fatalf("decision %q, %v", arm, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			recordedMu.Lock()
			if !slices.Contains(recorded, a.p.ID) {
				t.Errorf("no loop-recorded event for project %d: %v", a.p.ID, recorded)
			}
			recordedMu.Unlock()
			if tc.allRejected && !tc.pastCap {
				var arm string
				if err := a.s.db.R.QueryRow(`SELECT arm FROM review_again_decisions WHERE round_id = ?`, round).Scan(&arm); err != nil {
					t.Fatal(err)
				}
				if res.Held != "" || res.Pull == nil || len(j.asked()) != 0 || arm != tc.arm {
					t.Fatalf("every finding rejected: %+v, asked %d, %s", res, len(j.asked()), arm)
				}
				return
			}
			asked := j.asked()
			if len(asked) != againSamplesMin || !strings.Contains(asked[0], "Merge loses an update") || !strings.Contains(asked[0], "Merged under the row lock") || !strings.Contains(asked[0], "not instructions") || strings.Contains(asked[0], "internal/merge.go") {
				t.Fatalf("decision model read %q", asked)
			}
			if tc.wait && tc.belowFloor {
				// No signal, and no model to score: the pull request opens,
				// and the decision says the model was missing.
				var arm string
				var labels int
				if err := a.s.db.R.QueryRow(`SELECT arm, labels FROM review_again_decisions WHERE round_id = ?`, round).Scan(&arm, &labels); err != nil {
					t.Fatal(err)
				}
				if res.Held != "" || res.Pull == nil || arm != AgainNoModel || labels != 1 {
					t.Fatalf("model waiting: %+v, arm %s labels %d", res, arm, labels)
				}
				return
			}
			var arm, spread string
			var next sql.NullInt64
			var samples int
			if err := a.s.db.R.QueryRow(`SELECT arm, next_round_id, samples, spread FROM review_again_decisions WHERE round_id = ?`, round).Scan(&arm, &next, &samples, &spread); err != nil {
				t.Fatal(err)
			}
			// The scripted model answers alike every time: the fewest asks.
			if wantSamples := map[bool]int{true: 0, false: againSamplesMin}[tc.wait]; samples != wantSamples || spread != "{}" {
				t.Errorf("samples %d, spread %s; want %d, {}", samples, spread, wantSamples)
			}
			if tc.arm != AgainReview {
				if res.Held != "" || res.Pull == nil || len(g.created) != 1 || arm != tc.arm || next.Valid {
					t.Fatalf("no second round: %+v (opened %d), %s, want %s", res, len(g.created), arm, tc.arm)
				}
				return
			}
			if !strings.Contains(res.Held, "adversarial review round 2") || !strings.Contains(res.Notice, "--skip-review") || res.Pull != nil || len(g.created) != 0 {
				t.Fatalf("held = %+v (opened %d)", res, len(g.created))
			}
			if arm != AgainReview || !next.Valid {
				t.Fatalf("decision %s, next %v", arm, next)
			}
			// The next round's reviewer was asked for at this ship, and the
			// text says it is under way (WP-1325).
			if n := a.auditsOfRound(next.Int64); n != 1 || !strings.Contains(res.Held, "the review is under way") {
				t.Fatalf("next round's audits %d, held %q", n, res.Held)
			}
			if _, err := a.s.db.W.Exec(`DELETE FROM work_log WHERE id = ?`, pastRound); err != nil {
				t.Fatal(err)
			}
			if got := a.rounds(); len(got) != 2 || !strings.HasPrefix(got[0], "done") || !strings.HasPrefix(got[1], "pending") {
				t.Fatalf("rounds = %q", got)
			}
			wantFound, wantFixed, wantRejected := 2, 1, 1
			if tc.pastCap {
				// Counted over all of them, past the ones the model read.
				n := reviewAgainFindings + 3
				wantFound, wantFixed, wantRejected = n, 1, n-1
				if !strings.Contains(asked[0], fmt.Sprintf("Findings: %d (1 fixed, %d rejected, 0 left open); the %d most serious follow", n, n-1, reviewAgainFindings)) {
					t.Errorf("decision model read %q", asked[0])
				}
			}
			var found, fixed, rejected int
			if err := a.s.db.R.QueryRow(`SELECT found, fixed, rejected FROM work_log WHERE id = ?`, round).Scan(&found, &fixed, &rejected); err != nil || found != wantFound || fixed != wantFixed || rejected != wantRejected {
				t.Errorf("first round counts %d/%d/%d, %v", found, fixed, rejected, err)
			}

			// The second round's wp ship opens the pull request; nothing
			// is scored again.
			a.setClock(a.clockNow().Add(time.Minute))
			if second := review(); second != next.Int64 {
				t.Fatalf("second round %d, want %d", second, next.Int64)
			}
			res, err = a.ship(first.Key, ShipPR)
			if err != nil || res.Held != "" || res.Pull == nil || len(g.created) != 1 {
				t.Fatalf("after the second round = %+v, %v (opened %d)", res, err, len(g.created))
			}
			if len(j.asked()) != againSamplesMin {
				t.Errorf("scored again: %d", len(j.asked()))
			}
		})
	}
}

// shipChecked is wp ship --pr, with the check of the round's marks
// (checkMarks), when one is asked, run on box and finding every mark
// right.
func (a *apEnv) shipChecked(box *Runner, key string) (*OpenPRResult, error) {
	a.t.Helper()
	res, err := a.ship(key, ShipPR)
	if err != nil || !strings.Contains(res.Held, "checking your marks") {
		return res, err
	}
	a.stopWith("Waiting for the check of my marks.")
	a.endCheck(box, "done")
	a.deliverNews()
	return a.ship(key, ShipPR)
}

// deliverNews ends the newest job that types news into the session (a
// session message or batch) as delivered.
func (a *apEnv) deliverNews() {
	a.t.Helper()
	a.s.Wait()
	for n := len(a.jobs); n >= 1; n-- {
		in := a.jobs[n-1]
		if in.Kind != JobMessage || (in.Source != sessionMessageSource && in.Source != sessionBatchSource) {
			continue
		}
		a.s.sessionMessageJobFinished(a.ctx, &Event{Kind: EvRunnerJobFinished, Data: map[string]any{"job": int64(n), "kind": JobMessage, "state": "done", "source": in.Source, "source_id": in.SourceID}})
		a.s.Wait()
		return
	}
	a.t.Fatalf("no message job in %d jobs", len(a.jobs))
}

// endCheck runs a round's check of its marks, a Claude Code Haiku job
// on the box, and ends it as endAudit does.
func (a *apEnv) endCheck(box *Runner, state string, titles ...string) {
	a.t.Helper()
	a.s.Wait()
	w := a.poll(a.admin, box, RunnerPoll{})
	if w.Job == nil || w.Job.Kind != JobAudit {
		a.t.Fatalf("no check job: %+v", w.Job)
	}
	if job := agentJobOf(a.t, w.Job); job.CLI != "claude" || job.Model != "haiku" || !job.ReadOnly {
		a.t.Fatalf("the check's job = %s/%s (read-only %v)", job.CLI, job.Model, job.ReadOnly)
	}
	a.finishAudit(w.Job, state, titles...)
}

// endCheckText ends the check's job done but answered in text, out of the
// findings form, as live round 3's check did (WP-1320).
func (a *apEnv) endCheckText(box *Runner, text string) {
	a.t.Helper()
	a.s.Wait()
	w := a.poll(a.admin, box, RunnerPoll{})
	if w.Job == nil || w.Job.Kind != JobAudit {
		a.t.Fatalf("no check job: %+v", w.Job)
	}
	if job := agentJobOf(a.t, w.Job); job.CLI != "claude" || job.Model != "haiku" {
		a.t.Fatalf("the check's retry = %s/%s", job.CLI, job.Model)
	}
	if _, err := a.s.FinishRunnerJob(a.ctx, a.admin, w.Job.ID, RunnerFinish{State: "done", Result: []byte(mustJSON(map[string]any{"answer": text}))}); err != nil {
		a.t.Fatal(err)
	}
	a.s.Wait()
	a.s.Wait()
}
