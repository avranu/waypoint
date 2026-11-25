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

// EvReviewLoopRecorded: the review loop's record of a project changed. Not
// recorded in history; it sets off the health check.
const EvReviewLoopRecorded = "review.loop_recorded"

// reviewHealthDays is how far back the record is read.
const reviewHealthDays = 30

// reviewHealthIssue is one tripped condition: a task's title (fixed, so an
// open task is found again) and its text (the figures).
type reviewHealthIssue struct {
	title, body string
}

// The least record each condition reads before it can trip.
const (
	healthMinFollowed = 20
	healthMinProbes   = 10
	healthMinAgree    = 50
	healthMinRuns     = 20
	healthMinLeads    = 20
	healthMinVerdicts = 10
	healthMinReasked  = 10
	// healthMinNeverOverturned: re-asked refutations, none overturned,
	// before the second ask is called useless. With none of n overturned
	// the 95% upper bound on the overturn rate is about 3/n (the rule of
	// three): 6% at 50, 30% at 10, too high to drop a check on.
	healthMinNeverOverturned = 50
	// healthMinStanding: standing findings (not rejected) before the share
	// of them filed can be called mostly filed (WP-1555). A count of
	// findings, set as the runs' and leads' minimums are (20): a run or two
	// can't trip it alone. The line is half: filing is the exception a
	// finding outside the change gets, so more filed than fixed means it
	// has become the way rounds end. Nothing measured sets either number
	// yet; the record this condition reads is what can.
	healthMinStanding = 20
	// healthMinRepeats: repeat holds of one task inside one day
	// (shipHoldRepeatSQL) before they are called a loop. A session that
	// marks what a hold names makes none, however many rounds or findings:
	// every hold after its first comes with fewer left to decide. One or
	// two are a session shipping again before it marked; three are marks
	// that don't clear the hold (WP-1547's filed findings did that) or a
	// session that won't decide.
	healthMinRepeats = 3
	healthHoldDay    = 24 * time.Hour
)

// reviewHealthIssues reads a project's record against the reopen
// conditions. A rule about a rate needs a minimum record, so a few rounds
// can't trip it; a run that reached the round cap and a check whose leads
// the prompt's size guard cut are said at the first one, since one is
// already exceptional.
func reviewHealthIssues(byRound []AgainRoundStat, agree *AgainAgreement, helpers []AgainHelperStat, holds []AgainHoldStat) []reviewHealthIssue {
	var out []reviewHealthIssue
	add := func(title, format string, a ...any) {
		out = append(out, reviewHealthIssue{title: title, body: fmt.Sprintf(format, a...)})
	}
	for _, r := range byRound {
		idx := fmt.Sprint(r.Index)
		if r.Index >= 3 {
			idx = "3 or later"
		}
		if r.Followed >= healthMinFollowed && r.Useful*4 < r.Followed {
			add("Review loop: rounds after round "+idx+" rarely find anything",
				"Of the %d judged rounds that followed an again decision at round %s in the last %d days, %d found something new of medium or worse that the session fixed or confirmed, filed or not (%.0f%%; the condition is under 25%%). Either the record (againUsefulSQL) or the signal floor and bars (againFloors, againBars) are wrong for that place.",
				r.Followed, idx, reviewHealthDays, r.Useful, pct(r.Useful, r.Followed))
		}
		if r.Arms[AgainCap] > 0 {
			add("Review loop: a run reached the round cap",
				"%d decision(s) at round %s in the last %d days stopped at the cap of %d rounds. Rounds that run that long mean the repeat detection missed churn or the floor is too low; read those runs' rounds (wp pr rounds, wp worklog --review).",
				r.Arms[AgainCap], idx, reviewHealthDays, reviewRoundsCap)
		}
		if r.ProbeFollowed >= healthMinProbes && r.Followed >= healthMinFollowed && r.ProbeUseful > 0 && r.ProbeUseful*r.Followed*5 >= r.Useful*r.ProbeFollowed*4 {
			add("Review loop: probes at round "+idx+" find as much as again rounds",
				"At round %s in the last %d days, %d of %d judged probe rounds were useful (%.0f%%) against %d of %d after again decisions (%.0f%%). Rounds the scores turned down find about as much as the ones they chose: the bar there is too high.",
				idx, reviewHealthDays, r.ProbeUseful, r.ProbeFollowed, pct(r.ProbeUseful, r.ProbeFollowed), r.Useful, r.Followed, pct(r.Useful, r.Followed))
		}
	}
	var checks, passed, unchecked, scoring, unscored int
	for _, r := range byRound {
		checks, passed, unchecked = checks+r.Checks, passed+r.Passed, unchecked+r.Unchecked
		scoring, unscored = scoring+r.Scoring, unscored+r.Labels
	}
	if scoring >= healthMinRuns && unscored*2 > scoring {
		add("Review loop: the decision model mostly can't score rounds",
			"Of %d review rounds whose findings went to the decision model in the last %d days, it scored none of the findings in %d (%.0f%%): it wasn't set, was waiting on its budget, or failed. Those rounds were read by their reviewers' labels alone, with no repeat told and the bars unread. Read the server log's \"decision model failed\" lines and wp ai usage.",
			scoring, reviewHealthDays, unscored, pct(unscored, scoring))
	}
	if checks >= healthMinRuns && passed*2 > checks {
		add("Review loop: the check of sessions' marks mostly doesn't answer",
			"Of %d rounds whose marks were sent to the Haiku check in the last %d days, it was passed over in %d (%.0f%%): it couldn't be asked, failed, or ran past its 30 minutes. Those rounds' marks stood on the sessions' word, and %d of them went on to another round only because rejections were left unchecked. Read the check audits' errors (purpose check) and the runners' claude sign-in.",
			checks, reviewHealthDays, passed, pct(passed, checks), unchecked)
	}
	var standing, filed int
	for _, r := range byRound {
		standing, filed = standing+r.Standing, filed+r.Filed
	}
	if standing >= healthMinStanding && filed*2 > standing {
		add("Review loop: findings are mostly filed, not fixed",
			"Of %d findings that stood (weren't rejected) in review rounds decided in the last %d days, sessions filed %d as issues instead of fixing them (%.0f%%; the condition is over 50%%). Filing ends a round as a fix does, so a session can end a review by filing. Read the filed findings (wp audit show, review_finding_scores.filed): either the findings are real work the change should do, or the rounds are asking for what doesn't belong in it.",
			standing, reviewHealthDays, filed, pct(filed, standing))
	}
	var sampled, unsettled int
	for _, r := range byRound {
		sampled, unsettled = sampled+r.Sampled, unsettled+r.Unsettled
	}
	if sampled >= healthMinRuns && unsettled*2 > sampled {
		add("Review loop: the decision model's asks mostly don't settle the decision",
			"Of %d review rounds the decision model scored in the last %d days, in %d (%.0f%%) the decision the asks' mean gave still changed with the last asks, up to %d. Those rounds sit near a bar: the decision there is close to a coin toss however many asks it takes. Read those decisions' spread (review_again_decisions.spread: \"unsettled\", \"outcome\" and the calls that split): a question the model can't settle wants its wording or the evidence it's given changed, not more asks.",
			sampled, reviewHealthDays, unsettled, pct(unsettled, sampled), againSamplesMax)
	}
	if agree != nil && agree.Findings >= healthMinAgree && agree.MeanGap > 3 {
		add("Review loop: reviewers' labels and the decision model's scores disagree",
			"Over %d findings scored both ways in the last %d days, the reviewers' labels and the decision model's scores are %.1f points apart on average (the condition is over 3); %d are within two points. One of them should get more weight in the decision, or be dropped.",
			agree.Findings, reviewHealthDays, agree.MeanGap, agree.Within2)
	}
	for _, h := range holds {
		if h.Repeats >= healthMinRepeats {
			add("Review loop: "+h.Issue+"'s ship held again and again with nothing marked",
				"%s's ship was refused %d times within one day in the last %d days for the same findings, with none of them decided since the refusal before (the condition is %d or more; %d refusals in all), held for %s. A session that marks what a refusal names never repeats one: either its marks don't clear the hold (a mark the hold doesn't count, as filed findings once weren't), or the session ships without deciding. Read those audits' findings and how they were marked (`wp audit show N`).",
				h.Issue, h.Repeats, reviewHealthDays, healthMinRepeats, h.Holds, auditList(h.Audits))
		}
	}
	for _, h := range helpers {
		if h.Check == helperVerify {
			if held := h.RefutedHeld + h.RefutedFiled; h.Refuted >= healthMinVerdicts && held*2 > h.Refuted {
				add("Review loop: the check of findings refutes findings sessions fix",
					"Of %d findings Haiku refuted against the code in the last %d days, sessions fixed or confirmed %d anyway (%.0f%%; %d of them filed as issues, not fixed). A refuted finding doesn't count toward another round, so the check is costing real findings: read those findings' verdict notes.",
					h.Refuted, reviewHealthDays, held, pct(held, h.Refuted), h.RefutedFiled)
			}
			if h.Reasked >= healthMinReasked && h.Overturned*2 > h.Reasked {
				add("Review loop: the check of findings' refutations don't repeat",
					"Of %d findings Haiku refuted against the code in the last %d days and was asked about again, the second answer didn't refute %d (%.0f%%). A refutation that doesn't survive one more ask is noise: the check's question, the evidence it reads, or its model needs changing. Read the verdict notes that start \"Asked twice, the check answered refuted, then\".",
					h.Reasked, reviewHealthDays, h.Overturned, pct(h.Overturned, h.Reasked))
			}
			if h.Reasked >= healthMinNeverOverturned && h.Overturned == 0 {
				add("Review loop: asking again never overturns a refutation",
					"All %d findings Haiku refuted and was asked about again in the last %d days were refuted the second time too. The second ask costs a Haiku call per refuted finding and has changed nothing: it can go.",
					h.Reasked, reviewHealthDays)
			}
			if h.Confirmed >= healthMinVerdicts && h.ConfirmedRejected*2 > h.Confirmed {
				add("Review loop: the check of findings confirms findings sessions reject",
					"Of %d findings Haiku confirmed against the code in the last %d days, sessions rejected %d (%.0f%%). Either the check passes what doesn't hold, or sessions reject what does: read both sides.",
					h.Confirmed, reviewHealthDays, h.ConfirmedRejected, pct(h.ConfirmedRejected, h.Confirmed))
			}
		}
		if ran := h.Runs - h.Stopped; ran >= healthMinRuns && (h.Failed+h.Skipped+h.Partial)*2 > ran {
			add("Review loop: Haiku check "+h.Check+" mostly doesn't run",
				"Of %d runs of the %s check in the last %d days that weren't stopped with their job, %d failed, %d failed in part and %d were skipped. Skipped runs are runners without a signed-in claude (a gateway audit's runner needs it too); failed ones say why in audit_helper_runs.error.",
				ran, h.Check, reviewHealthDays, h.Failed, h.Partial, h.Skipped)
		}
		if h.Check != helperVerify && h.Check != helperCombine && h.Answered >= healthMinRuns && h.Raised >= healthMinLeads && h.Confirmed*20 < h.Raised {
			add("Review loop: Haiku check "+h.Check+"'s problems are rarely confirmed",
				"The %s check raised %d problems in %d runs over the last %d days, and a second read confirmed %d (%.0f%%; the condition is under 5%%). It costs $%.2f there: its question or its pruning needs changing, or it should go.",
				h.Check, h.Raised, h.Answered, reviewHealthDays, h.Confirmed, pct(h.Confirmed, h.Raised), h.CostUSD)
		}
		if held := h.Fixed + h.Filed; h.Check != helperVerify && h.Check != helperCombine && held+h.Rejected >= healthMinVerdicts && held*4 < held+h.Rejected {
			add("Review loop: findings from Haiku check "+h.Check+" are mostly rejected by sessions",
				"Of %d findings from the %s check's problems that sessions marked in the last %d days, they fixed or confirmed %d (%.0f%%; the condition is under 25%%; %d of them filed as issues) and rejected %d. The second read passes what a session then finds doesn't hold.",
				held+h.Rejected, h.Check, reviewHealthDays, held, pct(held, held+h.Rejected), h.Filed, h.Rejected)
		}
	}
	return out
}

// checkReviewHealth reads one project's record after it changed
// (EvReviewLoopRecorded) and files a task for each tripped condition that
// has no open task yet. One check runs at a time, so two records written
// together can't both find no open task and file it twice.
func (s *Service) checkReviewHealth(ctx context.Context, pid int64) {
	if pid == 0 {
		return
	}
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	since := s.now().AddDate(0, 0, -reviewHealthDays).UnixMilli()
	if err := s.sweepProjectHealth(ctx, pid, since); err != nil {
		s.log.WarnContext(ctx, "review loop health: the project's record unreadable", "project", pid, "err", err)
	}
}

// sweepProjectHealth reads one project's record and files a task for each
// tripped condition that has no open task yet.
func (s *Service) sweepProjectHealth(ctx context.Context, pid, since int64) error {
	if s.healthReadFault != nil {
		if err := s.healthReadFault(pid); err != nil {
			return err
		}
	}
	ivis, iargs := "i.project_id = ?", []any{pid}
	byRound, agree, err := s.againReport(ctx, ivis, iargs, since)
	if err != nil {
		return err
	}
	helpers, err := s.againHelperStats(ctx, ivis, iargs, since)
	if err != nil {
		return err
	}
	holds, err := s.againHolds(ctx, ivis, iargs, since)
	if err != nil {
		return err
	}
	for _, h := range reviewHealthIssues(byRound, agree, helpers, holds) {
		if err := s.fileReviewHealth(ctx, pid, h); err != nil {
			s.log.WarnContext(ctx, "review loop health task not filed", "project", pid, "condition", h.title, "err", err)
		}
	}
	return nil
}

// fileReviewHealth files one tripped condition's task, unless an open task
// of the project has its title.
func (s *Service) fileReviewHealth(ctx context.Context, projectID int64, h reviewHealthIssue) error {
	var open int64
	err := s.db.R.QueryRowContext(ctx, `SELECT i.id FROM issues i JOIN statuses st ON st.id = i.status_id
		WHERE i.project_id = ? AND i.title = ? AND st.category NOT IN ('done','canceled') LIMIT 1`, projectID, h.title).Scan(&open)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	ownerID := s.leftoverOwner(ctx, s.db.R, projectID)
	if ownerID == 0 {
		return errors.New("the project has no owner or editor to file it as")
	}
	owner, err := s.userByID(ctx, s.db.R, ownerID)
	if err != nil {
		return err
	}
	if owner.Disabled {
		return fmt.Errorf("the project's owner %s is disabled", owner.Name())
	}
	writer := Actor{User: owner}
	writer.textAgent = "waypoint"
	var b strings.Builder
	b.WriteString(h.body)
	b.WriteString("\n\nFiled by Waypoint's check of the review loop's own record against WP-ADR135's reopen conditions (review_health.go). `wp pr rounds` shows the record. Change what the figures point at, then let the record show whether it worked; close this task when it does.")
	_, err = s.CreateIssue(ctx, &writer, IssueInput{Project: itoa64(projectID), Type: "story", Agent: "claude", Priority: "medium",
		Labels: []string{"recurring"}, Title: h.title, Description: b.String()})
	return err
}

// AgainHoldStat is one task's ship holds since a time (recordShipHold):
// Holds in all, and Repeats, the most repeat holds (shipHoldRepeatSQL) that
// fell within one day (healthHoldDay), and Audits, the audits a repeat
// held the ship for, in the order first held.
type AgainHoldStat struct {
	Issue   string  `json:"issue"`
	Holds   int     `json:"holds"`
	Repeats int     `json:"repeats"`
	Audits  []int64 `json:"audits,omitempty"`
}

// shipHoldRepeatSQL is, over review_ship_holds as h, whether the hold is a
// repeat: the task's hold before it for the same audit's findings by the
// same refusal had no more left to decide. A hold with fewer left came
// after marks, and a refusal's first hold of an audit is a session being
// told what to decide; neither is a loop.
const shipHoldRepeatSQL = `coalesce((SELECT h0.findings <= h.findings FROM review_ship_holds h0
	WHERE h0.issue_id = h.issue_id AND h0.audit_id = h.audit_id AND h0.check_name = h.check_name AND h0.id < h.id
	ORDER BY h0.id DESC LIMIT 1), 0)`

// againHolds reads the ship holds of the issues the condition (on i) lets
// through, in the order the tasks were made.
func (s *Service) againHolds(ctx context.Context, ivis string, iargs []any, since int64) ([]AgainHoldStat, error) {
	// #nosec G202 -- fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT p.key || '-' || i.number, h.created_at, h.audit_id, `+shipHoldRepeatSQL+`
		FROM review_ship_holds h JOIN issues i ON i.id = h.issue_id JOIN projects p ON p.id = i.project_id
		WHERE `+ivis+` AND h.created_at >= ? ORDER BY h.issue_id, h.created_at, h.id`, append(slices.Clone(iargs), since)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgainHoldStat
	var repeats []int64
	day := healthHoldDay.Milliseconds()
	settle := func() {
		for i, j := 0, 0; j < len(repeats); j++ {
			for repeats[j]-repeats[i] >= day {
				i++
			}
			out[len(out)-1].Repeats = max(out[len(out)-1].Repeats, j-i+1)
		}
		repeats = repeats[:0]
	}
	for rows.Next() {
		var key string
		var at, audit int64
		var repeat bool
		if err := rows.Scan(&key, &at, &audit, &repeat); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].Issue != key {
			if len(out) > 0 {
				settle()
			}
			out = append(out, AgainHoldStat{Issue: key})
		}
		out[len(out)-1].Holds++
		if repeat {
			repeats = append(repeats, at)
			if cur := &out[len(out)-1]; !slices.Contains(cur.Audits, audit) {
				cur.Audits = append(cur.Audits, audit)
			}
		}
	}
	if len(out) > 0 {
		settle()
	}
	return out, rows.Err()
}

// recordShipHold records a ship refused for findings still to decide, for
// the review health (WP-1555). Only a repeat (shipHoldRepeatSQL) can trip
// its condition, so only a repeat sets off the check. A record that can't
// be written is logged and the hold goes on: the refusal must reach the
// session either way.
func (s *Service) recordShipHold(ctx context.Context, actor *Actor, issue, audit, round int64, check string, findings int) {
	err := s.write(ctx, actor, func(t *tx) error {
		res, err := t.ExecContext(ctx, `INSERT INTO review_ship_holds(issue_id, audit_id, round_id, check_name, findings, created_at) VALUES (?,?,?,?,?,?)`,
			issue, audit, round, check, findings, s.nowMS())
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		var repeat bool
		var project int64
		if err := t.QueryRowContext(ctx, `SELECT `+shipHoldRepeatSQL+`, i.project_id FROM review_ship_holds h JOIN issues i ON i.id = h.issue_id WHERE h.id = ?`, id).Scan(&repeat, &project); err != nil {
			return err
		}
		if repeat {
			t.emit(&Event{Kind: EvReviewLoopRecorded, ProjectID: project})
		}
		return nil
	})
	if err != nil {
		s.log.WarnContext(ctx, "couldn't record a ship hold", "issue", issue, "audit", audit, "check", check, "err", err)
	}
}

// auditList names audits: "audit #4", "audits #4, #7".
func auditList(ids []int64) string {
	codes := make([]string, len(ids))
	for i, id := range ids {
		codes[i] = fmt.Sprintf("#%d", id)
	}
	if len(ids) == 1 {
		return "audit " + codes[0]
	}
	return "audits " + strings.Join(codes, ", ")
}
