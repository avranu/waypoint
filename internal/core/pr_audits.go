package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// An audit that ends limited, failed or cancelled holds nothing: the feature fails open. Only high and critical findings hold the merge.

const (
	// auditWaitMax is how long after a runner took it an audit still holds
	// the babysitter and the merge back; auditQueueWait is how long one no
	// runner has taken yet does. A busy runner (one job at a time, a babysit
	// round on it) must not stall a pull request: the feature fails open.
	auditWaitMax   = 5 * time.Hour // past gatewayAuditTimeout: the job ends itself first
	auditQueueWait = 15 * time.Minute
	// prAuditKey makes the request once for a pull request's head.
	prAuditKey = "pr-audit:%d:%s"
	// AuditFootprint is the purpose of the drift guard's check (WP-440):
	// not the pull request's audit, so it neither stands in for one at a
	// head nor hides one's findings.
	AuditFootprint = "footprint"
	// AuditCheck is the purpose of a review round's check of the findings
	// its session rejected or fixed (checkMarks, WP-1320): like a footprint
	// check, never the pull request's audit.
	AuditCheck = "check"
)

// auditStartsRound: a finding of this severity, still to do, is work for a
// babysitting round. A low one goes in the packet but starts nothing.
func auditStartsRound(severity string) bool {
	return severity == "critical" || severity == "high" || severity == "medium"
}

// auditHoldsMerge: a finding of this severity, still to do, holds the merge.
func auditHoldsMerge(severity string) bool { return severity == "critical" || severity == "high" }

// onPRAudit is dispatch's part: a pull request opened or marked ready gets
// its audit, and news of an audit moves its pull request on.
func (s *Service) onPRAudit(ctx context.Context, e *Event) {
	switch {
	case e.Kind == EvPRUpdated && e.New == "opened", e.Kind == EvPRActivity && e.New == "ready":
		prID := eventPRID(e)
		if prID == 0 || e.ProjectID == 0 {
			return
		}
		if p, err := s.projectByID(ctx, s.db.R, e.ProjectID); err != nil || !p.Settings.AuditPRs || p.Archived {
			return
		}
		// Not in the background: the babysitter sees this same event next and
		// may merge an armed pull request, so the audit that holds the merge
		// must exist by then.
		s.requestPRAudit(context.WithoutCancel(ctx), prID)
	case e.Kind == EvCodeAudit:
		ended := e.Field == "" && e.Old != e.New && slices.Contains([]string{AuditDone, AuditFailed, AuditLimited, AuditCancelled}, e.New)
		if e.Field != "findings" && !ended {
			return
		}
		id, _ := e.Data["audit"].(int64)
		var prID int64
		var handed bool
		if id == 0 || s.db.R.QueryRowContext(ctx, `SELECT coalesce(pr_id, 0), purpose = '' AND request_key NOT LIKE 'pr-audit:%'
				AND EXISTS (SELECT 1 FROM work_log w WHERE w.kind = 'review' AND w.audit_id = code_audits.id)
			FROM code_audits WHERE id = ?`, id).Scan(&prID, &handed) != nil || prID == 0 {
			return
		}
		// A review round's audit handed to the pull request was its review: if it fails or is limited, the pull request asks for its own audit. Other audits failing ask for nothing here.
		if ended && handed && (e.New == AuditFailed || e.New == AuditLimited) && e.ProjectID != 0 {
			if p, err := s.projectByID(ctx, s.db.R, e.ProjectID); err == nil && p.Settings.AuditPRs && !p.Archived {
				s.requestPRAudit(context.WithoutCancel(ctx), prID)
			}
		}
		s.goBG(func() { s.maybeAutoMerge(context.WithoutCancel(ctx), prID) })
	}
}

// requestPRAudit asks for the audit of a pull request's head, once. It runs
// on the runner of whoever armed the pull request, else its author, under the
// runner gate RequestAudit applies; a pull request that gate refuses (a
// stranger's, a fork's) is not audited, and nothing else is told.
func (s *Service) requestPRAudit(ctx context.Context, prID int64) {
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil || pr.Outside || (pr.State != "open" && pr.State != "draft") || pr.HeadSHA == "" {
		return
	}
	if pr.headRepo != "" && !strings.EqualFold(pr.headRepo, pr.Repo) {
		return
	}
	if s.headAudited(ctx, pr) {
		return
	}
	owner := s.babysitOwnerFor(ctx, pr)
	if owner == 0 {
		return
	}
	_, err = s.RequestAudit(ctx, SystemActor(), AuditInput{PRID: pr.ID, Via: "system", owner: owner,
		requestKey: fmt.Sprintf(prAuditKey, pr.ID, strings.ToLower(pr.HeadSHA))})
	if err != nil {
		s.log.Info("audit a pull request skipped", "repo", pr.Repo, "number", pr.Number, "err", err)
	}
}

// prAuthorModel names what wrote a pull request's change, for keeping its
// audit from the same family of model (gatewayExcludeFor): the model of the
// newest agent session on its branch, and the coding agent its own text
// names. "" when neither is known (a person's pull request).
func (s *Service) prAuthorModel(ctx context.Context, pr *PullRequest) string {
	var model string
	if pr.HeadRef != "" && pr.HeadRef != pr.BaseRef {
		if err := s.db.R.QueryRowContext(ctx, `SELECT model FROM agent_sessions WHERE branch = ? AND lower(repo) = lower(?) AND model != ''
			ORDER BY last_seen_at DESC, id DESC LIMIT 1`, pr.HeadRef, pr.Repo).Scan(&model); err != nil && !errors.Is(err, sql.ErrNoRows) {
			s.log.Info("a pull request's author model", "repo", pr.Repo, "number", pr.Number, "err", err)
		}
	}
	return strings.TrimSpace(model + " " + pr.Agent)
}

// headAudited: some audit of the pull request, in any state, read this
// head (a footprint check isn't one). A review round's audit handed to the
// pull request that ended without an answer doesn't count: it was the
// change's review, and nothing else would replace it (WP-1250). The pull
// request's own audit still counts in any state, even when a round reused
// it: asking again by its key would only find it.
func (s *Service) headAudited(ctx context.Context, pr *PullRequest) bool {
	rows, err := s.db.R.QueryContext(ctx, `SELECT a.ref FROM code_audits a WHERE a.pr_id = ? AND a.purpose = ''
		AND NOT (a.status IN (?, ?) AND a.request_key NOT LIKE 'pr-audit:%'
			AND EXISTS (SELECT 1 FROM work_log w WHERE w.kind = 'review' AND w.audit_id = a.id))`, pr.ID, AuditFailed, AuditLimited)
	if err != nil {
		return true // can't tell: better none than a second one
	}
	defer rows.Close()
	for rows.Next() {
		var ref string
		if rows.Scan(&ref) == nil && sameSHA(ref, pr.HeadSHA) {
			return true
		}
	}
	return false
}

// auditGoing is the pull request's audit that holds the babysitter and the
// merge back: one a runner is reading the pull request for (taken in the
// last auditWaitMax), or one asked for in the last auditQueueWait that no
// runner has taken yet (its job queued, or its models busy).
func (s *Service) auditGoing(ctx context.Context, prID int64) (int64, bool) {
	id, going, rerr := s.auditGoingErr(ctx, prID)
	if rerr != nil {
		if readFault(rerr) {
			s.log.WarnContext(ctx, "audit going err failed", "prid", prID, "err", rerr)
		}
	}
	return id, going
}

// auditGoingErr is auditGoing, also reporting a failed read (not "no audit
// going"), for a caller that must not start a round or merge on it (WP-748).
func (s *Service) auditGoingErr(ctx context.Context, prID int64) (int64, bool, error) {
	now := s.nowMS()
	var id int64
	err := s.db.R.QueryRowContext(ctx, `SELECT a.id FROM code_audits a LEFT JOIN runner_jobs j ON j.id = a.job_id
		WHERE a.pr_id = ? AND a.status IN ('queued','running')
		AND ((j.state = 'running' AND j.started_at > ?) OR (coalesce(j.state, 'queued') = 'queued' AND a.created_at > ?))
		ORDER BY a.id DESC LIMIT 1`,
		prID, now-auditWaitMax.Milliseconds(), now-auditQueueWait.Milliseconds()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// auditKind names what an audit of a pull request is for, after its
// number: " (a footprint check)" for the drift guard's (WP-440), " (for an
// adversarial review)" for a review round's (WP-687), " (from an
// adversarial review)" for one its round handed to the pull request
// (WP-1059), else "".
func (s *Service) auditKind(ctx context.Context, id int64) string {
	var purpose string
	if s.db.R.QueryRowContext(ctx, `SELECT purpose FROM code_audits WHERE id = ?`, id).Scan(&purpose) != nil {
		return ""
	}
	switch purpose {
	case AuditFootprint:
		return " (a footprint check)"
	case AuditReview:
		return " (for an adversarial review)"
	}
	if rounds, err := s.roundsAudit(ctx, id); err == nil && rounds {
		return " (from an adversarial review)"
	}
	return ""
}

// roundsAudit: a review round asked for the audit.
// A read that failed is an error, not "no round asked" (WP-1406); the Warn
// is here, once.
func (s *Service) roundsAudit(ctx context.Context, id int64) (bool, error) {
	var ok bool
	err := s.db.R.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM work_log WHERE kind = 'review' AND audit_id = ?)`, id).Scan(&ok)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read whether a review round asked for an audit", "audit", id, "err", err)
		}
		return false, err
	}
	return ok, nil
}

// roundRunning: a babysitting round's job for the pull request is queued or
// running. A failed read is an error, not "no round running", for the merge
// hold that asks.
func (s *Service) roundRunning(ctx context.Context, prID int64) (bool, error) {
	var n int
	err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM babysits b JOIN runner_jobs j ON j.id = b.job_id
		WHERE b.pr_id = ? AND b.job_id != 0 AND j.state IN ('queued','running')`, prID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// auditDeadline is when the audit auditGoing found stops holding the merge:
// the time at which a re-read of the pull request lets it go.
// An audit that is gone has no deadline (nil, nil); a read that failed is an
// error, so the caller keeps a time to look again (WP-1406).
func (s *Service) auditDeadline(ctx context.Context, id int64) (*time.Time, error) {
	var created int64
	var state string
	var started *int64
	err := s.db.R.QueryRowContext(ctx, `SELECT a.created_at, coalesce(j.state, 'queued'), j.started_at FROM code_audits a
		LEFT JOIN runner_jobs j ON j.id = a.job_id WHERE a.id = ?`, id).Scan(&created, &state, &started)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	at := time.UnixMilli(created).Add(auditQueueWait)
	if state == "running" && started != nil {
		at = time.UnixMilli(*started).Add(auditWaitMax)
	}
	return &at, nil
}

// newestAudit is the pull request's audit that finished last, whatever its
// end (0: none yet).
func (s *Service) newestAudit(ctx context.Context, prID int64) (int64, string, error) {
	return s.newestOf(ctx, prID, "")
}

// newestOf is newestAudit for audits of a purpose ("" or AuditFootprint):
// a footprint check finishing doesn't make an audit's findings go away.
// No finished audit is the answer (0, ""); a failed read is an error.
func (s *Service) newestOf(ctx context.Context, prID int64, purpose string) (int64, string, error) {
	var id int64
	var status string
	err := s.db.R.QueryRowContext(ctx, `SELECT id, status FROM code_audits WHERE pr_id = ? AND purpose = ? AND status IN ('done','failed','limited','cancelled')
		ORDER BY coalesce(finished_at, created_at) DESC, id DESC LIMIT 1`, prID, purpose).Scan(&id, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", err
	}
	return id, status, nil
}

// carriedAudits are the finished audits a review round's release carried to
// the pull request: the round's own and its check of the session's marks.
// They hold its merge and show in its work packet beside the newest audit,
// which would otherwise shadow them (WP-1370).
func (s *Service) carriedAudits(ctx context.Context, prID int64) ([]int64, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM code_audits WHERE pr_id = ? AND purpose = '' AND status = ? AND request_key LIKE 'review-round:%' ORDER BY id`, prID, AuditDone)
	if err != nil {
		return nil, err
	}
	return scanIDs(rows)
}

// auditTodo is the findings still to do (findingsToDo), worst first, of
// the pull request's newest finished audit and newest footprint check
// (WP-440), each when it is done.
// A read that fails is logged and leaves out what it could not read: the
// callers start rounds and fill packets, which the next pass redoes; the
// merge hold reads through auditTodoErr.
func (s *Service) auditTodo(ctx context.Context, prID int64) []TodoFinding {
	out, err := s.auditTodoErr(ctx, prID)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read the pull request's audit findings", "pr", prID, "err", err)
		}
	}
	return out
}

// auditTodoErr is auditTodo, also reporting a failed read.
func (s *Service) auditTodoErr(ctx context.Context, prID int64) ([]TodoFinding, error) {
	var out []TodoFinding
	seen := map[int64]bool{}
	for _, purpose := range []string{"", AuditFootprint} {
		id, status, err := s.newestOf(ctx, prID, purpose)
		if err != nil {
			return out, err
		}
		if id == 0 || status != AuditDone {
			continue
		}
		seen[id] = true
		todo, err := s.findingsToDo(ctx, id, purpose == AuditFootprint)
		if err != nil {
			return out, err
		}
		for _, f := range todo {
			out = append(out, TodoFinding{CodeAuditFinding: f, Audit: id, Footprint: purpose == AuditFootprint})
		}
		if purpose == "" {
			un, err := s.uncheckedRejections(ctx, id)
			if err != nil {
				return out, err
			}
			out = append(out, un...)
		}
	}
	carried, err := s.carriedAudits(ctx, prID)
	if err != nil {
		return out, err
	}
	for _, id := range carried {
		if seen[id] {
			continue
		}
		todo, err := s.findingsToDo(ctx, id, false)
		if err != nil {
			return out, err
		}
		for _, f := range todo {
			out = append(out, TodoFinding{CodeAuditFinding: f, Audit: id})
		}
		un, err := s.uncheckedRejections(ctx, id)
		if err != nil {
			return out, err
		}
		out = append(out, un...)
	}
	return out, nil
}

// uncheckedRejections are the findings of an audit the session rejected and
// nothing checked that reopenUnverified left rejected (medium and low): the
// packet lists them, and they hold nothing.
func (s *Service) uncheckedRejections(ctx context.Context, id int64) ([]TodoFinding, error) {
	a := &CodeAudit{ID: id}
	if err := s.loadFindings(ctx, a); err != nil {
		return nil, err
	}
	var out []TodoFinding
	for _, f := range a.Findings {
		if f.State == "rejected" && strings.HasPrefix(f.Note, uncheckedNote) {
			out = append(out, TodoFinding{CodeAuditFinding: f, Audit: id, Unchecked: true})
		}
	}
	return out, nil
}

// findingsToDo are an audit's findings still to do (findingToDo). A
// footprint check's (footprint) are so while confirmed, filed or not:
// filing a change beyond the task leaves it in the pull request, which only
// undoing it or a rejection settles (WP-440).
func (s *Service) findingsToDo(ctx context.Context, id int64, footprint bool) ([]CodeAuditFinding, error) {
	a := &CodeAudit{ID: id}
	if err := s.loadFindings(ctx, a); err != nil {
		return nil, err
	}
	var todo []CodeAuditFinding
	for _, f := range a.Findings {
		if findingToDo(f) || (footprint && f.State == "confirmed") {
			todo = append(todo, f)
		}
	}
	return todo, nil
}

// auditRoundWork is the ids of the findings that start a babysitting round:
// still to do, medium or worse, not handed to an earlier round already
// (those it left are for a person, not for the same work again), and not
// held by an autopilot review round.
func (s *Service) auditRoundWork(ctx context.Context, b *Babysit, prID int64) []string {
	var out []string
	held := map[int64]bool{}
	for _, f := range s.auditTodo(ctx, prID) {
		if _, ok := held[f.Audit]; !ok {
			held[f.Audit] = s.roundHoldsAudit(ctx, f.Audit)
		}
		if held[f.Audit] {
			continue // an autopilot review round hands them to its session (WP-687)
		}
		if f.startsRound() && !slices.ContainsFunc(b.Rounds, func(r BabysitRound) bool { return slices.Contains(r.Findings, f.ID) }) {
			out = append(out, f.ID)
		}
	}
	return out
}

// auditHolds are the reasons the newest finished audit holds a pull request,
// and, when that audit ended without findings, a note saying it holds
// nothing.
// A failed read is an error: the audit's findings are unknown, so the merge
// must not go on them.
func (s *Service) auditHolds(ctx context.Context, prID, going int64) ([]string, string, error) {
	// An audit going (auditGoing) holds the merge until it ends, so an armed
	// pull request doesn't merge under it. One that waited too long for a
	// runner, or ran too long, holds nothing: the feature fails open.
	if going != 0 {
		return []string{fmt.Sprintf("waiting for audit #%d%s to finish", going, s.auditKind(ctx, going))}, "", nil
	}
	// Scope creep a footprint check named holds it, whatever its severity,
	// until it is undone, split out or turned down (WP-440).
	var reasons []string
	fid, fstatus, err := s.newestOf(ctx, prID, AuditFootprint)
	if err != nil {
		return nil, "", err
	}
	if id := fid; id != 0 && fstatus == AuditDone {
		var ids []string
		todo, err := s.findingsToDo(ctx, id, true)
		if err != nil {
			return nil, "", err
		}
		for _, f := range todo {
			ids = append(ids, f.ID)
		}
		if len(ids) > 0 {
			reasons = append(reasons, fmt.Sprintf("audit #%d, a footprint check, named %d change%s beyond the task (%s): undo or split out, or close with wp audit mark ID fixed|rejected -m WHY",
				id, len(ids), plural(len(ids)), strings.Join(ids, ", ")))
		}
	}
	id, status, err := s.newestAudit(ctx, prID)
	if err != nil {
		return nil, "", err
	}
	var held []string
	if id != 0 && status == AuditDone {
		var ids []string
		todo, err := s.findingsToDo(ctx, id, false)
		if err != nil {
			return nil, "", err
		}
		// A check of the session's marks that is the newest audit holds on
		// whatever it still disagrees with, of any severity: its findings
		// are marks nobody has settled.
		check := s.isCheckAudit(ctx, id)
		for _, f := range todo {
			if auditHoldsMerge(f.Severity) || check {
				ids = append(ids, f.ID)
			}
		}
		if len(ids) > 0 {
			what := "critical or high finding%s still to do"
			if check {
				what = "finding%s of the check of the session's marks nobody decided"
			}
			held = append(held, fmt.Sprintf("audit #%d has %d "+what+" (%s): fix, or close with wp audit mark ID fixed|rejected -m WHY",
				id, len(ids), plural(len(ids)), strings.Join(ids, ", ")))
		}
	}
	carried, err := s.carriedAudits(ctx, prID)
	if err != nil {
		return nil, "", err
	}
	for _, cid := range carried {
		if cid == id {
			continue
		}
		var ids []string
		todo, err := s.findingsToDo(ctx, cid, false)
		if err != nil {
			return nil, "", err
		}
		for _, f := range todo {
			if auditHoldsMerge(f.Severity) || s.isCheckAudit(ctx, cid) {
				ids = append(ids, f.ID)
			}
		}
		if len(ids) > 0 {
			held = append(held, fmt.Sprintf("audit #%d, carried from the review before the pull request opened, has %d finding%s nobody decided (%s): fix, or close with wp audit mark ID fixed|rejected -m WHY",
				cid, len(ids), plural(len(ids)), strings.Join(ids, ", ")))
		}
	}
	switch {
	case len(held) > 0:
		return append(held, reasons...), "", nil
	case id != 0 && status != AuditDone:
		return reasons, fmt.Sprintf("audit #%d ended %s: not holding the merge", id, status), nil
	}
	return reasons, "", nil
}

// isCheckAudit: the audit is a review round's check of the session's marks
// (its findings are marks the check disagrees with, whatever their severity).
func (s *Service) isCheckAudit(ctx context.Context, id int64) bool {
	var n int
	return s.db.R.QueryRowContext(ctx, `SELECT 1 FROM code_audits WHERE id = ? AND request_key LIKE 'review-round:%:check'`, id).Scan(&n) == nil
}

// auditHoldReason: a readiness reason an audit gave (only a fix or a verdict
// clears it).
func auditHoldReason(r string) bool { return strings.HasPrefix(r, "audit #") }

// TodoFinding is an audit finding a pull request's work packet carries.
type TodoFinding struct {
	CodeAuditFinding
	Audit int64 `json:"audit"`
	// Footprint: a footprint check named it, a change beyond the task
	// (WP-440). Every one is work for a round, whatever its severity.
	Footprint bool `json:"footprint,omitempty"`
	// Unchecked: the session rejected it and the check of its marks never
	// ran. Listed for whoever reads the packet; it holds nothing and starts
	// no round.
	Unchecked bool `json:"unchecked,omitempty"`
}

// startsRound: still to do, it is work for a babysitting round.
func (f TodoFinding) startsRound() bool {
	return !f.Unchecked && (f.Footprint || auditStartsRound(f.Severity))
}

// todoFindings are the findings still to do in the pull request's newest
// finished audit and footprint check, for its work packet.
func (s *Service) todoFindings(ctx context.Context, prID int64) []TodoFinding {
	out := s.auditTodo(ctx, prID)
	if out == nil {
		out = []TodoFinding{}
	}
	return out
}

// renderFindings is the packet's sections for audit findings and a
// footprint check's. Their text is the model's, so it is shown as untrusted.
func renderFindings(b *strings.Builder, all []TodoFinding) {
	var audit, creep []TodoFinding
	for _, f := range all {
		if f.Footprint {
			creep = append(creep, f)
		} else {
			audit = append(audit, f)
		}
	}
	if len(audit) > 0 {
		fmt.Fprintf(b, "\n## Audit findings (%d), from audit #%d\n", len(audit), audit[0].Audit)
		b.WriteString("A model's claims: check each, fix the real ones, then mark each: " + markHint("ID", "WHY") + ". Low ones are optional.\n")
		writeFindings(b, audit)
	}
	if len(creep) > 0 {
		fmt.Fprintf(b, "\n## Changes beyond the task (%d), from footprint check #%d\n", len(creep), creep[0].Audit)
		b.WriteString("Fix-up rounds grew the PR past what it shipped with; a model says the task doesn't need these. Check each against the task: undo it (file it with `wp new` if it's worth doing) and `wp audit mark ID fixed`, or `wp audit mark ID rejected -m WHY` if the task needs it.\n")
		writeFindings(b, creep)
	}
}

// writeFindings is the findings themselves.
func writeFindings(b *strings.Builder, list []TodoFinding) {
	for _, f := range list {
		where := ""
		if f.Path != "" {
			where = " " + f.Path
			if f.Line > 0 {
				where += fmt.Sprintf(":%d", f.Line)
			}
		}
		state := ""
		if f.Unchecked {
			state = " [rejected by the session, unchecked]"
		} else if f.State != "open" {
			state = " [" + f.State + "]"
		}
		fmt.Fprintf(b, "\n### %s %s%s%s\n", f.ID, f.Severity, where, state)
		b.WriteString("<untrusted>\n" + untrusted(excerpt(f.Title, 300)) + "\n\n" + untrusted(excerpt(f.Problem, 4000)))
		if f.Confirm != "" {
			b.WriteString("\n\nTo confirm: " + untrusted(excerpt(f.Confirm, 2000)))
		}
		b.WriteString("\n</untrusted>\n")
	}
}
