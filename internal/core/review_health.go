package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// The review loop's own health (WP-1320, WP-ADR135): whenever the loop's
// record of a project changes (a round's decision, or an audit with Haiku
// helpers ending: EvReviewLoopRecorded), the rounds' record and the
// helpers' record of that project are read against the decision's reopen
// conditions, and a condition that trips files a task in the project
// (once: an open task of the same title stands for it). Nobody has to
// remember to look at `wp pr rounds`.
//
// Not a poll: every figure the conditions read changes only when one of
// those two records is written, or when a session marks a finding, and a
// run's marks come before its next decision. Marks made after a run's last
// decision are read at the project's next one.

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
	healthMinFollowed = 20 // judged rounds after an again at an index
	healthMinProbes   = 10 // judged rounds after a probe at an index
	healthMinAgree    = 50 // findings scored both ways
	healthMinRuns     = 20 // a helper's runs
	healthMinLeads    = 20 // a panel check's leads
	healthMinVerdicts = 10 // verdicts of one kind
	healthMinReasked  = 10 // refuted findings asked about again
	// healthMinNeverOverturned: re-asked refutations, none overturned,
	// before the second ask is called useless. With none of n overturned
	// the 95% upper bound on the overturn rate is about 3/n (the rule of
	// three): 6% at 50, 30% at 10, too high to drop a check on.
	healthMinNeverOverturned = 50
)

// reviewHealthIssues reads a project's record against the reopen
// conditions. Every rule needs a minimum record, so a few rounds can't
// trip one.
func reviewHealthIssues(byRound []AgainRoundStat, agree *AgainAgreement, helpers []AgainHelperStat) []reviewHealthIssue {
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
				"Of the %d judged rounds that followed an again decision at round %s in the last %d days, %d found something new of medium or worse that held (%.0f%%; the condition is under 25%%). Either the record (againUsefulSQL) or the signal floor and bars (againFloors, againBars) are wrong for that place.",
				r.Followed, idx, reviewHealthDays, r.Useful, pct(r.Useful, r.Followed))
		}
		if r.Arms[AgainCap] > 0 {
			add("Review loop: a run reached the round cap",
				"%d decision(s) at round %s in the last %d days stopped at the cap of %d rounds. Rounds that run that long mean the repeat detection missed churn or the floor is too low; read those runs' rounds (wp pr rounds, wp worklog --review).",
				r.Arms[AgainCap], idx, reviewHealthDays, reviewRoundsCap)
		}
		// Probes that found nothing say nothing of the bar, however little
		// the again rounds found (that's the condition above).
		if r.ProbeFollowed >= healthMinProbes && r.Followed >= healthMinFollowed && r.ProbeUseful > 0 && r.ProbeUseful*r.Followed*5 >= r.Useful*r.ProbeFollowed*4 {
			add("Review loop: probes at round "+idx+" find as much as again rounds",
				"At round %s in the last %d days, %d of %d judged probe rounds were useful (%.0f%%) against %d of %d after again decisions (%.0f%%). Rounds the scores turned down find about as much as the ones they chose: the bar there is too high.",
				idx, reviewHealthDays, r.ProbeUseful, r.ProbeFollowed, pct(r.ProbeUseful, r.ProbeFollowed), r.Useful, r.Followed, pct(r.Useful, r.Followed))
		}
	}
	// Rounds a signal sent on count too: their findings were still sent to
	// be scored (Scoring), so the model failing there is the same failure.
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
	for _, h := range helpers {
		if h.Check == helperVerify {
			if h.Refuted >= healthMinVerdicts && h.RefutedHeld*2 > h.Refuted {
				add("Review loop: the check of findings refutes findings sessions fix",
					"Of %d findings Haiku refuted against the code in the last %d days, sessions fixed or confirmed %d anyway (%.0f%%). A refuted finding doesn't count toward another round, so the check is costing real findings: read those findings' verdict notes.",
					h.Refuted, reviewHealthDays, h.RefutedHeld, pct(h.RefutedHeld, h.Refuted))
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
		if h.Guarded > 0 {
			add("Review loop: Haiku check "+h.Check+"'s leads were cut by the prompt's size guard",
				"In the last %d days, %d of the %s check's leads were cut past the size guard on the leads a reviewer's prompt takes (panelLeadsBytes, cmd/wp/runner_helpers.go), unread, not for anything they said (audit_helper_runs.cut, why \"past the …\"). The check returns far more than it should, or the guard is too low for what it finds.",
				reviewHealthDays, h.Guarded, h.Check)
		}
		// Runs stopped with their job (cancelled) say nothing of the check.
		if ran := h.Runs - h.Stopped; ran >= healthMinRuns && (h.Failed+h.Skipped+h.Partial)*2 > ran {
			add("Review loop: Haiku check "+h.Check+" mostly doesn't run",
				"Of %d runs of the %s check in the last %d days that weren't stopped with their job, %d failed, %d failed in part and %d were skipped. Skipped runs are runners without a signed-in claude (a gateway audit's runner needs it too); failed ones say why in audit_helper_runs.error.",
				ran, h.Check, reviewHealthDays, h.Failed, h.Partial, h.Skipped)
		}
		if h.Check != helperVerify && h.Answered >= healthMinRuns && h.Leads >= healthMinLeads && h.TakenUp*20 < h.Leads {
			add("Review loop: Haiku check "+h.Check+"'s leads are rarely taken up",
				"The %s check handed reviewers %d leads in %d runs over the last %d days, and %d findings came from them (%.0f%%; the condition is under 5%%). It costs $%.2f there: its question or its pruning needs changing, or it should go.",
				h.Check, h.Leads, h.Answered, reviewHealthDays, h.TakenUp, pct(h.TakenUp, h.Leads), h.CostUSD)
		}
		if h.Check != helperVerify && h.TakenUp >= healthMinVerdicts && h.Held*4 < h.TakenUp {
			add("Review loop: findings from Haiku check "+h.Check+" rarely hold",
				"Of %d findings reviewers took from the %s check's leads in the last %d days, sessions fixed or confirmed %d (%.0f%%; the condition is under 25%%). The reviewer passes its leads on without checking them.",
				h.TakenUp, h.Check, reviewHealthDays, h.Held, pct(h.Held, h.TakenUp))
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
	for _, h := range reviewHealthIssues(byRound, agree, helpers) {
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
		return err // a failed lookup isn't "no open task": don't file a duplicate
	}
	// A tripped condition nobody can be given is an error, so the sweep
	// logs it: the alarm mustn't go quiet.
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
