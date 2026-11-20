package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// What a review left open at merge (WP-367, story 4 of WP-362).
//
// Our own review's findings that nobody judged before the pull request
// merged would otherwise sit open forever on a closed pull request, and
// nothing would ever look at them again. At merge they become one audit
// (WP-ADR20): its findings are the review's claims, in the audit's own
// words, each linked back to the bot finding it came from. An audit with a
// finding of medium severity or worse gets a fix task for Claude, the same
// one `wp audit fix` makes; otherwise the audit waits in `wp audit ls` for a
// person. Marking an audit's finding fixed or rejected settles the bot
// finding the same way.
//
// Other people's review comments still open at merge are only counted
// (pull_requests.open_others_at_merge) and shown on `wp pr show`. They are
// never a task and never read by a model as instructions: whoever wrote them
// may be a stranger (WP-ADR13, WP-ADR14). The exception is comments by bots
// Waypoint can vouch for (nitpickBot) that didn't hold the merge, waived nitpicks and late minor findings: they
// go into the pull request's nitpick log (WP-695, nitpicks.go), whose
// findings wp next lists, as a bot's words, to the next agent on their file.

const (
	// leftoverAgent is the requesting agent of the audits made at merge.
	leftoverAgent = "review-leftover"
	// leftoverSetting marks the one-time backfill of pull requests merged
	// before this existed as done.
	leftoverSetting = "review_leftover_v1"
	// leftoverFixWithin is how recently a pull request must have merged for
	// the backfill to ask for its fix task; an older one waits in the list.
	leftoverFixWithin = 14 * 24 * time.Hour
	// leftoverInstructions is what these audits say they looked for.
	leftoverInstructions = "Findings Waypoint's own review raised on this pull request and nobody judged before it merged."
)

// PRLeftOpen is what was still open when a pull request merged.
type PRLeftOpen struct {
	// Others counts review comments by other reviewers, people and bots,
	// open at merge. They are recorded and shown, never acted on.
	Others int `json:"others"`
	// Audits are the audits made from our own review's open findings.
	Audits []PRLeftAudit `json:"audits,omitempty"`
}

// PRLeftAudit is one such audit.
type PRLeftAudit struct {
	ID       int64  `json:"id"`
	Findings int    `json:"findings"`
	Open     int    `json:"open"`
	FixTask  string `json:"fix_task,omitempty"`
}

// leftOpen reads what a merged pull request had left open; nil when
// nothing is known or there was nothing.
func (s *Service) leftOpen(ctx context.Context, prID int64) *PRLeftOpen {
	var state string
	var others sql.NullInt64
	if err := s.db.R.QueryRowContext(ctx, `SELECT state, open_others_at_merge FROM pull_requests WHERE id = ?`, prID).Scan(&state, &others); err != nil || state != "merged" {
		return nil
	}
	out := &PRLeftOpen{Others: int(others.Int64)}
	rows, err := s.db.R.QueryContext(ctx, `SELECT a.id, (SELECT count(*) FROM code_audit_findings WHERE audit_id = a.id),
		(SELECT count(*) FROM code_audit_findings WHERE audit_id = a.id AND state IN ('open','confirmed')),
		coalesce(ip.key || '-' || i.number, '')
		FROM code_audits a LEFT JOIN issues i ON i.id = a.fix_issue_id LEFT JOIN projects ip ON ip.id = i.project_id
		WHERE a.pr_id = ? AND a.requested_agent = ? ORDER BY a.id`, prID, leftoverAgent)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var a PRLeftAudit
			if rows.Scan(&a.ID, &a.Findings, &a.Open, &a.FixTask) == nil {
				out.Audits = append(out.Audits, a)
			}
		}
	}
	if out.Others == 0 && len(out.Audits) == 0 {
		return nil
	}
	return out
}

// onLeftoverEvent starts the sweep when a pull request merges, and when a
// review finishes on one that already has (a late review's findings go the
// same way). It runs from dispatch, after the commit.
func (s *Service) onLeftoverEvent(ctx context.Context, e *Event) {
	var prID int64
	switch {
	case e.Kind == EvPRActivity:
		prID, _ = e.Data["pr_id"].(int64)
	case e.Kind == EvBotRun && e.New == "done":
		run, _ := e.Data["bot_run"].(int64)
		if run == 0 {
			return
		}
		_ = s.db.R.QueryRowContext(ctx, `SELECT pr_id FROM bot_runs WHERE id = ?`, run).Scan(&prID)
	}
	if prID == 0 {
		return
	}
	merged := e.Kind == EvPRActivity && e.New == "merged"
	due := s.leftoverDue(ctx, prID)
	nits := merged || s.nitpicksDue(ctx, prID)
	if !due && !nits {
		return
	}
	s.goBG(func() {
		bg := context.Background()
		if merged {
			// The comments as they stand at merge, if the forge answers.
			if pr, err := s.prByID(bg, s.db.R, prID); err == nil && !pr.Outside {
				c, cancel := context.WithTimeout(bg, 30*time.Second)
				_, _ = s.syncReviewComments(c, pr)
				cancel()
			}
		}
		if due {
			if _, err := s.sweepLeftover(bg, prID, time.Time{}); err != nil {
				s.log.WarnContext(ctx, "sweep what a review left open", "pr", prID, "err", err)
			}
		}
		if nits {
			if _, err := s.sweepNitpicks(bg, prID); err != nil {
				s.log.WarnContext(ctx, "sweep the nitpicks left open", "pr", prID, "err", err)
			}
		}
	})
}

// nitpicksDue: pr merged with waived nitpicks and no nitpick sweep has
// finished, so its merge's sweep failed or never ran (WP-695); a later
// event on it sweeps again.
func (s *Service) nitpicksDue(ctx context.Context, prID int64) bool {
	var due bool
	err := s.db.R.QueryRowContext(ctx, `SELECT p.state = 'merged' AND p.outside = 0 AND p.nitpicks_swept_at IS NULL AND EXISTS (
			SELECT 1 FROM review_comment_scores c WHERE c.pr_id = p.id AND c.waived_at IS NOT NULL)
		FROM pull_requests p WHERE p.id = ?`, prID).Scan(&due)
	return err == nil && due
}

// leftoverDue is the cheap check before a sweep: the pull request is merged,
// belongs to a project, and has findings to turn into an audit or no count
// of others' comments yet.
func (s *Service) leftoverDue(ctx context.Context, prID int64) bool {
	var due bool
	err := s.db.R.QueryRowContext(ctx, `SELECT p.state = 'merged' AND p.outside = 0 AND (p.open_others_at_merge IS NULL OR EXISTS (
			SELECT 1 FROM bot_findings f WHERE f.pr_id = p.id AND f.state = 'open' AND f.audit_finding_id IS NULL AND f.placement != 'withheld'))
		FROM pull_requests p WHERE p.id = ?`, prID).Scan(&due)
	return err == nil && due
}

type leftoverFinding struct {
	id       int64
	path     string
	line     int
	severity string
	claim    string
	model    string
}

// claimTitle is a claim's first sentence, for a finding's title.
func claimTitle(claim string) string {
	c := strings.TrimSpace(claim)
	if i := strings.IndexAny(c, "\n"); i >= 0 {
		c = c[:i]
	}
	if i := strings.Index(c, ". "); i > 20 {
		c = c[:i+1]
	}
	return excerpt(strings.TrimSpace(c), 120)
}

// leftoverOwner is whose audit it is: the project's owner, else an editor,
// else whoever made the project. An audit belongs to a person; this one has
// no runner, so only the fix task is written as them.
func (s *Service) leftoverOwner(ctx context.Context, q queryer, projectID int64) int64 {
	var id int64
	_ = q.QueryRowContext(ctx, `SELECT user_id FROM project_members WHERE project_id = ? AND role IN ('owner','editor')
		ORDER BY role = 'owner' DESC, created_at, user_id LIMIT 1`, projectID).Scan(&id)
	if id == 0 {
		_ = q.QueryRowContext(ctx, `SELECT coalesce(created_by, 0) FROM projects WHERE id = ?`, projectID).Scan(&id)
	}
	return id
}

// countOthersOpen counts the review comments by others still open on a
// pull request: not Waypoint's own findings, not dealt with, not an agent's
// forge account of a project member, plus strangers' comments held for a
// person. Only the count is kept.
func (s *Service) countOthersOpen(ctx context.Context, q queryer, pr *PullRequest, botLogin string) (int, error) {
	list, err := s.reviewComments(ctx, q, pr.ID)
	if err != nil {
		return 0, err
	}
	agents := map[string]bool{}
	rows, err := q.QueryContext(ctx, `SELECT lower(login) FROM forge_people WHERE forge_id = ? AND user_id IS NULL`, pr.ForgeID)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			rows.Close()
			return 0, err
		}
		agents[l] = true
	}
	rows.Close()
	n := 0
	for _, rc := range list {
		who := strings.ToLower(rc.Author)
		if rc.FindingID != 0 || rc.Kind == "finding" || rc.dealt() || agents[who] || (botLogin != "" && who == strings.ToLower(botLogin)) {
			continue
		}
		n++
	}
	var held int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM outside_pr_text WHERE pr_id = ? AND kind != 'pull' AND state = 'held'`, pr.ID).Scan(&held); err != nil {
		return 0, err
	}
	return n + held, nil
}

// sweepLeftover turns a merged pull request's open review findings into one
// audit, and records how many comments by others it merged with. fixSince,
// when set (the backfill), is the cut-off for asking for a fix task: a pull
// request that merged before it only waits in the list. It reports how many
// findings it moved.
func (s *Service) sweepLeftover(ctx context.Context, prID int64, fixSince time.Time) (int, error) {
	pr, err := s.prByID(ctx, s.db.R, prID)
	if err != nil {
		return 0, err
	}
	if pr.State != "merged" || pr.Outside {
		return 0, nil
	}
	projectID := s.attachedProject(ctx, s.db.R, pr.ForgeID, pr.Repo)
	if projectID == 0 {
		return 0, nil
	}
	botLogin := ""
	if f, err := s.forgeByID(ctx, pr.ForgeID); err == nil {
		botLogin = s.forgeBotLogin(ctx, f)
	}
	moved := 0
	var auditID int64
	var fix bool
	err = s.write(ctx, SystemActor(), func(t *tx) error {
		// Read again inside the write: another sweep may have won.
		cur, err := s.prByID(ctx, t, prID)
		if err != nil || cur.State != "merged" {
			return err
		}
		var counted sql.NullInt64
		if err := t.QueryRowContext(ctx, `SELECT open_others_at_merge FROM pull_requests WHERE id = ?`, prID).Scan(&counted); err != nil {
			return err
		}
		if !counted.Valid {
			n, err := s.countOthersOpen(ctx, t, cur, botLogin)
			if err != nil {
				return err
			}
			if _, err := t.ExecContext(ctx, `UPDATE pull_requests SET open_others_at_merge = ? WHERE id = ?`, n, prID); err != nil {
				return err
			}
		}
		rows, err := t.QueryContext(ctx, `SELECT id, path, coalesce(line, 0), severity, claim, model FROM bot_findings
			WHERE pr_id = ? AND state = 'open' AND audit_finding_id IS NULL AND placement != 'withheld' ORDER BY id`, prID)
		if err != nil {
			return err
		}
		var found []leftoverFinding
		for rows.Next() {
			var f leftoverFinding
			if err := rows.Scan(&f.id, &f.path, &f.line, &f.severity, &f.claim, &f.model); err != nil {
				rows.Close()
				return err
			}
			found = append(found, f)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(found) == 0 {
			return nil
		}
		// Worst first, as an audit lists them.
		slices.SortStableFunc(found, func(a, b leftoverFinding) int { return severityRank(a.severity) - severityRank(b.severity) })
		var models []string
		var chain []AgentModel
		for _, f := range found {
			if m := strings.TrimSpace(f.model); m != "" && !slices.Contains(models, m) {
				models = append(models, m)
				chain = append(chain, AgentModel{CLI: "review", Model: m})
			}
		}
		if chain == nil {
			chain = []AgentModel{{CLI: "review"}}
		}
		chainJSON, _ := json.Marshal(chain)
		fix = false
		for _, f := range found {
			if blocksGate(f.severity, "medium") {
				fix = true
			}
		}
		if !fixSince.IsZero() && (cur.MergedAt == nil || cur.MergedAt.Before(fixSince)) {
			fix = false
		}
		owner := s.leftoverOwner(ctx, t, projectID)
		if owner == 0 {
			return fmt.Errorf("no one to own an audit in project %d", projectID)
		}
		var seq int
		if err := t.QueryRowContext(ctx, `SELECT count(*) FROM code_audits WHERE pr_id = ? AND requested_agent = ?`, prID, leftoverAgent).Scan(&seq); err != nil {
			return err
		}
		ref := cmp(cur.MergeSHA, cur.HeadSHA)
		base := ""
		if cur.MergeSHA != "" {
			base = cur.MergeSHA + "^1"
		}
		title := fmt.Sprintf("%s#%d: %s", cur.Repo, cur.Number, excerpt(cur.Title, 160))
		now := s.nowMS()
		if err := t.QueryRowContext(ctx, `INSERT INTO code_audits(project_id, forge_id, repo, target, ref, base, pr_id, pr_number, title, instructions, models,
				post_comment, fix, status, model, summary, owner_id, requested_agent, requested_via, request_key, created_at, started_at, finished_at)
			VALUES (?,?,?,'pr',?,?,?,?,?,?,?,0,?,?,?,?,?,?,'automation',?,?,?,?) RETURNING id`,
			projectID, cur.ForgeID, cur.Repo, ref, base, cur.ID, cur.Number, title, leftoverInstructions, string(chainJSON),
			fix, AuditDone, strings.Join(models, ", "),
			fmt.Sprintf("%d finding%s of Waypoint's review were still open when %s#%d merged.", len(found), plural(len(found)), cur.Repo, cur.Number),
			owner, leftoverAgent, fmt.Sprintf("leftover:%d:%d", prID, seq+1), now, now, now).Scan(&auditID); err != nil {
			return err
		}
		for i, f := range found {
			res, err := t.ExecContext(ctx, `INSERT INTO code_audit_findings(audit_id, ordinal, title, path, line, severity, problem) VALUES (?,?,?,?,?,?,?)`,
				auditID, i+1, claimTitle(f.claim), f.path, f.line, f.severity, f.claim)
			if err != nil {
				return err
			}
			fid, err := res.LastInsertId()
			if err != nil {
				return err
			}
			// The bot finding stays open until the audit's verdict.
			if _, err := t.ExecContext(ctx, `UPDATE bot_findings SET audit_finding_id = ? WHERE id = ?`, fid, f.id); err != nil {
				return err
			}
		}
		moved = len(found)
		// An audit that ended: its fix task, if asked for, comes from the
		// same code as for any audit. Made done, never queued: no Old.
		t.emit(&Event{Kind: EvCodeAudit, ProjectID: projectID, New: AuditDone, Data: map[string]any{"audit": auditID, "title": title}})
		return nil
	})
	if err != nil {
		return 0, err
	}
	if moved > 0 {
		s.log.Info("review findings left open at merge became an audit", "pr", prID, "audit", auditID, "findings", moved, "fix", fix)
	}
	return moved, nil
}

// settleLeftoverFindings carries an audit finding's verdict to the bot
// finding it came from, inside the marking's transaction: fixed settles it,
// rejected dismisses it, with the note. Open and confirmed leave it open.
func (s *Service) settleLeftoverFindings(ctx context.Context, t *tx, actor *Actor, auditID int64, ordinal int, state, note string) error {
	to := ""
	switch state {
	case "fixed":
		to = FindingFixed
	case "rejected":
		to = FindingDismissed
	default:
		return nil
	}
	rows, err := t.QueryContext(ctx, `SELECT b.id, b.pr_id FROM bot_findings b JOIN code_audit_findings f ON f.id = b.audit_finding_id
		WHERE f.audit_id = ? AND f.ordinal = ?`, auditID, ordinal)
	if err != nil {
		return err
	}
	type link struct{ id, pr int64 }
	var links []link
	for rows.Next() {
		var l link
		if err := rows.Scan(&l.id, &l.pr); err != nil {
			rows.Close()
			return err
		}
		links = append(links, l)
	}
	rows.Close()
	by := findingBy{kind: "person"}
	if actor.User != nil {
		by.name, by.userID = actor.User.DisplayName, actor.User.ID
	}
	if actor.IsAgent() {
		by.kind, by.name = "agent", fmt.Sprintf("%s (%s)", by.name, cmp(actor.agentName(), "agent"))
	}
	why := fmt.Sprintf("audit %d.%d", auditID, ordinal)
	if note = strings.TrimSpace(note); note != "" {
		why += ": " + note
	}
	for _, l := range links {
		changed, err := s.setFindingState(ctx, t, l.id, to, by, "", why)
		if err != nil {
			return err
		}
		if changed {
			var projectID int64
			if pr, err := s.prByID(ctx, t, l.pr); err == nil {
				projectID = s.attachedProject(ctx, t, pr.ForgeID, pr.Repo)
			}
			t.emit(&Event{Kind: EvPRActivity, ProjectID: projectID, New: "finding", Data: map[string]any{"pr_id": l.pr}})
		}
	}
	return nil
}

// LeftoverBackfill is what the backfill did.
type LeftoverBackfill struct {
	PRs      int `json:"prs"`
	Findings int `json:"findings"`
}

// BackfillLeftover does for pull requests merged before this existed what
// the sweep does at merge. It runs once, in the background at start, and is
// safe to run again (force reruns it after it has run): a finding already in
// an audit is not moved twice. A pull request that merged long ago gets its
// audit but no fix task; those wait in `wp audit ls` for a person, so one
// start does not fill the queue with Claude tasks.
func (s *Service) BackfillLeftover(ctx context.Context, force bool) (LeftoverBackfill, error) {
	var res LeftoverBackfill
	if !force {
		var done int
		if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM server_settings WHERE key = ?`, leftoverSetting).Scan(&done); err != nil || done > 0 {
			return res, err
		}
	}
	ids, err := queryIDs(ctx, s.db.R, `SELECT p.id FROM pull_requests p WHERE p.state = 'merged' AND p.outside = 0 AND EXISTS (
			SELECT 1 FROM bot_findings f WHERE f.pr_id = p.id AND f.state = 'open' AND f.audit_finding_id IS NULL AND f.placement != 'withheld') ORDER BY p.id`)
	if err != nil {
		return res, err
	}
	since := s.now().Add(-leftoverFixWithin)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		n, err := s.sweepLeftover(ctx, id, since)
		if err != nil {
			s.log.WarnContext(ctx, "backfill what a review left open", "pr", id, "err", err)
			continue
		}
		if n > 0 {
			res.PRs++
			res.Findings += n
		}
	}
	_, err = s.db.W.ExecContext(ctx, `INSERT OR REPLACE INTO server_settings(key, value) VALUES (?, '1')`, leftoverSetting)
	return res, err
}

// RunLeftoverBackfill is BackfillLeftover for the server's start.
func (s *Service) RunLeftoverBackfill(ctx context.Context) {
	res, err := s.BackfillLeftover(ctx, false)
	if err != nil {
		s.log.WarnContext(ctx, "backfill what reviews left open", "err", err)
		return
	}
	if res.PRs > 0 {
		s.log.Info("backfilled what reviews left open", "prs", res.PRs, "findings", res.Findings)
	}
}
