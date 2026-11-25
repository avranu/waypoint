package core

// The review gate (WP-D21). On repos where it is on, Waypoint posts a
// `waypoint/review` commit status on each pull request's head: pending
// while a review is queued or running, failure while findings at or above
// the gate severity are open, success when none are. The forge's branch
// protection then requires it like any other status.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Gate states, the words forges use for a commit status.
const (
	GatePending = "pending"
	GateSuccess = "success"
	GateFailure = "failure"
)

const (
	gateRecheck   = 2 * time.Minute // how often a failing gate is looked at again
	gateNoteLimit = 140             // GitHub's limit for a status description
)

// PRGate is a pull request's review gate as last posted.
type PRGate struct {
	Enabled  bool   `json:"enabled"`
	Severity string `json:"severity"` // findings at or above block: critical, high, medium, low
	State    string `json:"state"`    // pending, success, failure; "" before the first post
	Note     string `json:"note"`
	SHA      string `json:"sha"`
	Open     int    `json:"open"`  // blocking findings
	Below    int    `json:"below"` // open findings below the gate
}

// gateConfigErr says whether a repo has the review gate on and at what
// severity (the strictest, when several projects attach the repo). A failed
// read of the setting is an error, never "gate off": callers hold or retry.
func (s *Service) gateConfigErr(ctx context.Context, forgeID int64, repo string) (bool, string, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT gate_severity FROM project_repos WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND review_gate = 1`, forgeID, repo)
	if err != nil {
		return false, "", err
	}
	defer rows.Close()
	on, sev := false, "critical"
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return false, "", err
		}
		if v, ok := parseSeverity(v); ok && (!on || severityRank(v) > severityRank(sev)) {
			sev = v
		}
		on = true
	}
	if err := rows.Err(); err != nil {
		return false, "", err
	}
	if !on {
		return false, "", nil
	}
	return true, sev, nil
}

// shortClaim is the start of a finding's claim, for a status line.
func shortClaim(claim string, max int) string {
	c := strings.Join(strings.Fields(claim), " ")
	if i := strings.IndexAny(c, ".:;"); i > 8 && i < max {
		c = c[:i]
	}
	if r := []rune(c); len(r) > max {
		c = strings.TrimSpace(string(r[:max-1])) + "…"
	}
	return c
}

// clipRunes cuts s to n runes, with an ellipsis.
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// computeGate decides a pull request's gate from its runs and findings.
func (s *Service) computeGate(ctx context.Context, pr *PullRequest, sev string) (state, note string, open, below int, err error) {
	findings, err := s.prFindings(ctx, s.db.R, pr.ID)
	if err != nil {
		return
	}
	var blocking []*findingRow
	total := 0
	for _, f := range findings {
		total++
		if f.State != FindingOpen {
			continue
		}
		if blocksGate(f.Severity, sev) {
			blocking = append(blocking, f)
		} else {
			below++
		}
	}
	open = len(blocking)
	var running, queued int
	if err = s.db.R.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN status = 'running' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = 'queued' THEN 1 ELSE 0 END), 0)
		FROM bot_runs WHERE pr_id = ? AND kind = 'review' AND head_sha = ? AND status IN ('queued','running')`, pr.ID, pr.HeadSHA).Scan(&running, &queued); err != nil {
		return
	}
	// A queued review holds the gate only within the repository's review wait,
	// the same deadline reviewGoing uses; a running one always holds.
	if queued > 0 && s.reviewWaitOver(ctx, pr) {
		queued = 0
	}
	running += queued
	var last, lastErr, lastSummary string
	// No review run for the head is an answer (last stays ""); a failed read
	// must not read as one: a failed review could pass the gate.
	if rerr := s.db.R.QueryRowContext(ctx, `SELECT status, error, summary FROM bot_runs WHERE pr_id = ? AND kind = 'review' AND head_sha = ? ORDER BY id DESC LIMIT 1`, pr.ID, pr.HeadSHA).Scan(&last, &lastErr, &lastSummary); rerr != nil && !errors.Is(rerr, sql.ErrNoRows) {
		return state, note, open, below, rerr
	}
	switch {
	case running > 0:
		return GatePending, "Review in progress", open, below, nil
	case last == "failed":
		note = "The review didn't run (every model failed); comment @waypoint review to try again"
		if lastErr != "" {
			note = "The review didn't run: " + lastErr + "; comment @waypoint review to try again"
		}
		return GateFailure, clipRunes(note, gateNoteLimit), open, below, nil
	case open > 0:
		items := make([]string, 0, open)
		for _, f := range blocking {
			if f.private {
				items = append(items, priority(f.Severity)+" (a private file)") // statuses can reach GitHub: no text
			} else {
				items = append(items, priority(f.Severity)+" "+shortClaim(f.Claim, 32))
			}
		}
		head := fmt.Sprintf("%d open finding%s: ", open, plural(open))
		return GateFailure, clipRunes(head+strings.Join(items, ", "), gateNoteLimit), open, below, nil
	case below > 0:
		return GateSuccess, fmt.Sprintf("No blocking findings (%d below %s)", below, priority(sev)), open, below, nil
	case total > 0:
		return GateSuccess, fmt.Sprintf("All %d finding%s resolved", total, plural(total)), open, below, nil
	case last == "done" && strings.HasPrefix(lastSummary, signedOffSummary), last == "skipped" && carriedSignedOff(lastSummary):
		// The head's sign-off: a review with nothing to say isn't posted (WP-234).
		return GateSuccess, "Reviewed: no issues found", open, below, nil
	}
	return GateSuccess, "No open findings", open, below, nil
}

// evalGate works out a pull request's gate and posts the status when it
// changed (or the head did).
func (s *Service) evalGate(ctx context.Context, prID int64) error {
	defer forgeLocks.lock(fmt.Sprintf("gate:%d", prID))() // one evaluation at a time: no status or event twice
	pr, err := s.prByID(ctx, s.db.R, prID)
	if errors.Is(err, ErrNotFound) {
		return nil // gone: nothing to evaluate
	}
	if err != nil {
		return err
	}
	if (pr.State != "open" && pr.State != "draft") || pr.HeadSHA == "" {
		return nil
	}
	enabled, sev, err := s.gateConfigErr(ctx, pr.ForgeID, pr.Repo)
	if err != nil {
		return err // an unreadable setting must not read as "gate off" and post success
	}
	if !enabled {
		return s.gateOff(ctx, pr)
	}
	state, note, open, below, err := s.computeGate(ctx, pr, sev)
	if err != nil {
		return err
	}
	now := s.nowMS()
	if pr.GateState == state && pr.gateNote == note && pr.gateSHA == pr.HeadSHA && pr.gateOpen == open {
		_, err = s.db.W.ExecContext(ctx, `UPDATE pull_requests SET gate_checked_at = ? WHERE id = ?`, now, pr.ID)
		return err
	}
	// Read before the status is posted: a failed read after it would record
	// the change without the event that tells the projects.
	projects, err := s.projectsOfRepo(ctx, pr.ForgeID, pr.Repo)
	if err != nil {
		return fmt.Errorf("read the projects of %s: %w", pr.Repo, err)
	}
	if err := s.postGateStatus(ctx, pr, state, note); err != nil {
		return err
	}
	changed := pr.GateState != state || pr.gateSHA != pr.HeadSHA || pr.gateOpen != open
	err = s.write(ctx, SystemActor(), func(t *tx) error {
		if _, err := t.ExecContext(ctx, `UPDATE pull_requests SET gate_state = ?, gate_note = ?, gate_sha = ?, gate_open = ?, gate_checked_at = ? WHERE id = ?`,
			state, note, pr.HeadSHA, open, now, pr.ID); err != nil {
			return err
		}
		if !changed || pr.Outside {
			return nil
		}
		for _, pid := range projects {
			t.emit(&Event{Kind: EvPRReviewGate, ProjectID: pid, New: state, Data: prEventData(pr, map[string]any{
				"pr": pr.ID, "sha": pr.HeadSHA, "state": state, "open": open, "below": below, "note": note})})
		}
		return nil
	})
	if err == nil && state == GateSuccess {
		s.goBG(func() { s.maybeAutoMerge(withoutLineLocks(context.WithoutCancel(ctx)), pr.ID) })
	}
	return err
}

// gateOff takes down a gate that was switched off: a failing status must
// not stay on a pull request the gate no longer applies to.
func (s *Service) gateOff(ctx context.Context, pr *PullRequest) error {
	if pr.GateState == "" {
		return nil
	}
	if pr.GateState != GateSuccess || pr.gateSHA != pr.HeadSHA {
		if err := s.postGateStatus(ctx, pr, GateSuccess, "The review gate is off for this repo"); err != nil {
			return err
		}
	}
	_, err := s.db.W.ExecContext(ctx, `UPDATE pull_requests SET gate_state = '', gate_note = '', gate_sha = '', gate_open = 0 WHERE id = ?`, pr.ID)
	return err
}

func (s *Service) postGateStatus(ctx context.Context, pr *PullRequest, state, note string) error {
	cl, ok := s.Forges.(ForgeChecks)
	if !ok {
		return nil
	}
	f, err := s.forgeByID(ctx, pr.ForgeID)
	if errors.Is(err, ErrNotFound) {
		return nil // no forge to post to; Waypoint still knows the gate
	}
	if err != nil {
		return fmt.Errorf("read the forge to post the %s status: %w", gateContext, err) // not recorded as posted: tried again
	}
	if f.token == "" {
		return nil // no way to post; Waypoint still knows the gate
	}
	target := ""
	if base := s.BaseURL(); base != "" {
		target = fmt.Sprintf("%s/agents?tab=pulls&pr=%d", base, pr.ID)
	}
	err = cl.SetCommitStatus(ctx, f, pr.Repo, pr.HeadSHA, CommitStatusPost{Context: gateContext, State: state, Description: note, TargetURL: target})
	if err != nil {
		return fmt.Errorf("post the %s status: %w", gateContext, err)
	}
	return nil
}

// checkPR re-reads a pull request's threads and re-evaluates its gate:
// after review and comment webhooks, a person's action in Waypoint, an
// "@waypoint check", and every few minutes while the gate fails.
func (s *Service) checkPR(ctx context.Context, prID int64, why string) {
	if err := s.checkPRErr(ctx, prID, why); readFault(err) && !errors.Is(err, ErrNotFound) && !isStatus(err, http.StatusNotFound) { // gone: nothing to check
		s.log.WarnContext(ctx, "check a pull request", "pr", prID, "why", why, "err", err)
	}
}

// checkPRErr is checkPR that also returns why the gate could not be
// evaluated, for callers (the final merge check) that must not go on then.
func (s *Service) checkPRErr(ctx context.Context, prID int64, why string) error {
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil {
		return err
	}
	if pr.State != "open" && pr.State != "draft" {
		return nil
	}
	var findings int
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM bot_findings WHERE pr_id = ? AND state != 'dropped'`, pr.ID).Scan(&findings); err != nil {
		return err
	}
	if findings > 0 {
		f, err := s.forgeByID(ctx, pr.ForgeID)
		if err == nil {
			_, err = s.syncFindings(ctx, f, pr, true)
		}
		if err != nil {
			// Stale threads could pass a gate they would fail: the gate is
			// still evaluated (and posted), but the caller hears of it.
			s.log.WarnContext(ctx, "sync review threads", "repo", pr.Repo, "number", pr.Number, "why", why, "err", err)
			if gerr := s.evalGate(ctx, pr.ID); gerr != nil {
				return gerr
			}
			return fmt.Errorf("sync review threads: %w", err)
		}
	}
	if err := s.evalGate(ctx, pr.ID); err != nil {
		s.log.WarnContext(ctx, "review gate", "repo", pr.Repo, "number", pr.Number, "why", why, "err", err)
		return err
	}
	return nil
}

// CheckGates looks again at pull requests whose gate is failing (Forgejo
// sends no event when a thread is resolved) and at pending ones whose
// review may have stopped.
func (s *Service) CheckGates(ctx context.Context) {
	cut := s.nowMS() - gateRecheck.Milliseconds() + 5000
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, gate_state FROM pull_requests WHERE gate_state IN ('failure','pending') AND state IN ('open','draft') AND gate_checked_at < ? ORDER BY id LIMIT 100`, cut)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "list the pull requests whose gate is due", "err", err)
		}
		return
	}
	type item struct {
		id    int64
		state string
	}
	var list []item
	for rows.Next() {
		var x item
		if rows.Scan(&x.id, &x.state) == nil {
			list = append(list, x)
		}
	}
	rows.Close()
	for _, x := range list {
		if ctx.Err() != nil {
			return
		}
		if x.state == GateFailure {
			s.checkPR(ctx, x.id, "periodic")
		} else if err := s.evalGate(ctx, x.id); err != nil {
			s.log.WarnContext(ctx, "review gate", "pull request", x.id, "err", err)
		}
	}
}

// SetRepoGate turns a repo's review gate on or off and sets the severity
// it blocks at (P0..P3 or critical..low); nil leaves a setting alone.
func (s *Service) SetRepoGate(ctx context.Context, actor *Actor, projectRef string, id int64, on *bool, severity *string) error {
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleOwner); err != nil {
		return err
	}
	sev := ""
	if severity != nil {
		v, ok := parseSeverity(*severity)
		if !ok {
			return invalid("gate severity must be P0, P1, P2 or P3 (critical, high, medium, low)")
		}
		sev = v
	}
	var forgeID int64
	var repo string
	err = s.write(ctx, actor, func(t *tx) error {
		if err := t.QueryRowContext(ctx, `SELECT forge_id, repo FROM project_repos WHERE id = ? AND project_id = ?`, id, p.ID).Scan(&forgeID, &repo); err != nil {
			return notFound("project repo")
		}
		if on != nil {
			if _, err := t.ExecContext(ctx, `UPDATE project_repos SET review_gate = ? WHERE id = ?`, boolInt(*on), id); err != nil {
				return err
			}
		}
		if severity != nil {
			if _, err := t.ExecContext(ctx, `UPDATE project_repos SET gate_severity = ? WHERE id = ?`, sev, id); err != nil {
				return err
			}
		}
		t.audit(AuditEntry{Action: "repo.review_gate", Category: AuditSettings, ProjectID: p.ID, TargetType: "repo", TargetID: id, Target: repo,
			Detail: "changed the review gate on " + repo})
		return nil
	})
	if err != nil {
		return err
	}
	s.goBG(func() { s.regate(withoutLineLocks(context.WithoutCancel(ctx)), forgeID, repo) })
	return nil
}

// regate evaluates the gate of every open pull request of a repo, after
// its settings changed.
func (s *Service) regate(ctx context.Context, forgeID int64, repo string) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM pull_requests WHERE forge_id = ? AND repo = ? COLLATE NOCASE AND state IN ('open','draft')`, forgeID, repo)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if err := s.evalGate(ctx, id); readFault(err) && !errors.Is(err, ErrNotFound) && !isStatus(err, http.StatusNotFound) { // gone, or the forge no longer has the commit
			s.log.WarnContext(ctx, "evaluate a pull request's review gate", "pr", id, "repo", repo, "err", err)
		}
	}
}

// markGatePending stops a stored green gate from standing for a head whose
// gate couldn't be re-evaluated: readiness then waits, and CheckGates looks
// again (WP-428). A failing or unset gate is left alone.
func (s *Service) markGatePending(ctx context.Context, prID int64) {
	if _, err := s.db.W.ExecContext(ctx, `UPDATE pull_requests SET gate_state = ?, gate_checked_at = 0 WHERE id = ? AND gate_state = ?`, GatePending, prID, GateSuccess); err != nil {
		s.log.WarnContext(ctx, "best-effort write failed", "err", err)
	}
}
