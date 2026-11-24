package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
)

// Prep models (WP-906, WP-ADR89): how well a model picks a task's files is
// the gateway's to rate, by purpose, with every other client's reports.
// Waypoint measures and reports; it keeps no ratings of its own.
//
//   - Every prep and trial asks for the gateway's prep purpose (with the
//     free route after it while the gateway has none) under an X-Request-ID
//     Waypoint keeps, so the gateway ranks the models it picks for preps.
//   - When the task is done, the picks (files plus symbols' files) are
//     compared with the files its merged pull requests changed, tests and
//     docs left out (as on page X13), and the request gets a report:
//     verified = the picks the work changed, false positives = the rest. A
//     prep made after the work merged, or a change too large to list, gets
//     none.
//   - Trials: at surplus, a recent current prep's bundle is read again with
//     the prep's model and the task's earlier trial models left out, so
//     another model answers and is measured. Trials are never shown to
//     anyone working the task; they exist to be reported. Until the gateway
//     serves surplus to the models due a test first (a change on the gateway),
//     trials also leave out the models Waypoint has already
//     reported prepTestedFindings findings on, the gateway's own bar for a
//     tested model (ADR 0067), and those with many trials still waiting.
//
// No model checks another's work: a trial is worth its report alone, and a
// prep is used as it is.
const (
	// prepScoreWindow: tasks done this recently are scored.
	prepScoreWindow = 60 * 24 * time.Hour
	// prepFeedbackAge: a request is reported on up to this long after it
	// was made; the gateway keeps its usage log 90 days.
	prepFeedbackAge = 85 * 24 * time.Hour
	// prepTestedFindings: a model with this many reported findings has
	// been tested (the gateway's "due a test" bar).
	prepTestedFindings = 20
	// prepTrialAge: only preps this recent get trials (open tasks, so
	// likely to merge while their requests can still be reported on).
	prepTrialAge = 7 * 24 * time.Hour
	// prepTrialsPerTask: at most this many trials of one task.
	prepTrialsPerTask = 2
	// prepTrialBatch: trials a sweep sends at most.
	prepTrialBatch = 2
	// prepTrialPending: a model with this many trials waiting for their
	// tasks isn't tried more until they are scored.
	prepTrialPending = 20
	// prepScoreBatch: tasks a sweep scores at most.
	prepScoreBatch = 50
)

// PrepModelStat is what Waypoint measured of one model's file picks at
// prep, and how much of it the gateway has taken as reports. The rating is
// the gateway's (its prep purpose); these are only the counts behind it.
type PrepModelStat struct {
	Model          string `json:"model"`
	Scored         int    `json:"scored"`          // preps and trials scored
	Verified       int    `json:"verified"`        // picks the work changed
	FalsePositives int    `json:"false_positives"` // picks it didn't
	// Missed: changed files no pick named. The gateway takes no such
	// count, so it never reaches the model's rating.
	Missed   int  `json:"missed"`
	Reported int  `json:"reported"` // requests the gateway took a report on
	Pending  int  `json:"pending"`  // reports not taken yet
	Waiting  int  `json:"waiting"`  // trials made, their tasks not done
	Tested   bool `json:"tested"`   // prepTestedFindings reported: trials leave it out
}

// prepModelName is the model of a prep's "Provider · model" name.
func prepModelName(s string) string {
	if n := strings.LastIndex(s, " · "); n >= 0 {
		return s[n+len(" · "):]
	}
	return s
}

// PrepModelStats is each model's counts, the most scored first. Agentic
// trials (WP-ADR104) are counted apart, in prepAgentPairs.
func (s *Service) PrepModelStats(ctx context.Context) ([]PrepModelStat, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT model, COUNT(*), SUM(hits), SUM(picked - hits), SUM(truth - hits),
			SUM(reported_at IS NOT NULL), SUM(request_id != '' AND reported_at IS NULL AND asked_at >= ?),
			SUM(CASE WHEN reported_at IS NOT NULL THEN picked ELSE 0 END)
		FROM prep_scores WHERE truth > 0 AND model NOT LIKE ? GROUP BY model`, s.now().Add(-prepFeedbackAge).UnixMilli(), prepAgentModelPrefix+"%")
	if err != nil {
		return nil, err
	}
	by := map[string]*PrepModelStat{}
	for rows.Next() {
		st := &PrepModelStat{}
		var findings int
		if err := rows.Scan(&st.Model, &st.Scored, &st.Verified, &st.FalsePositives, &st.Missed, &st.Reported, &st.Pending, &findings); err != nil {
			rows.Close()
			return nil, err
		}
		st.Tested = findings >= prepTestedFindings
		by[st.Model] = st
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	wrows, err := s.db.R.QueryContext(ctx, `SELECT t.model, COUNT(*) FROM prep_trials t
		WHERE t.created_at >= ? AND t.error = '' AND t.kind = 'chat' AND NOT EXISTS (SELECT 1 FROM prep_scores ps WHERE ps.issue_id = t.issue_id AND ps.source = 'trial' AND ps.model = t.model)
		GROUP BY t.model`, s.now().Add(-prepScoreWindow).UnixMilli())
	if err != nil {
		return nil, err
	}
	for wrows.Next() {
		var m string
		var n int
		if err := wrows.Scan(&m, &n); err != nil {
			wrows.Close()
			return nil, err
		}
		if by[m] == nil {
			by[m] = &PrepModelStat{Model: m}
		}
		by[m].Waiting = n
	}
	wrows.Close()
	if err := wrows.Err(); err != nil {
		return nil, err
	}
	out := make([]PrepModelStat, 0, len(by))
	for _, st := range by {
		out = append(out, *st)
	}
	slices.SortFunc(out, func(a, b PrepModelStat) int {
		if a.Scored != b.Scored {
			return b.Scored - a.Scored
		}
		return strings.Compare(a.Model, b.Model)
	})
	return out, nil
}

// prepRouting names a prep's gateway requests after its task, so each
// answer can be reported on.
func prepRouting(prefix, key, priority string) *gatewayRouting {
	if priority == prioritySurplus {
		return surplusRouting(prefix + key)
	}
	return &gatewayRouting{prefix: prefix + key}
}

// prepGatewayChain is what a surplus prep or a trial asks the gateway for:
// its prep purpose, then the free route. Never a pinned model, which the
// gateway would serve at normal priority.
func prepGatewayChain(openID int64) []ChainModel {
	var chain []ChainModel
	for _, m := range gatewayModels("prep") {
		chain = append(chain, ChainModel{ProviderID: openID, Model: m})
	}
	return chain
}

// prepChain is the context and chain a prep at priority is asked with.
// Normal: the task's chain. Surplus: prepGatewayChain, on a gateway that
// takes routes and routing; a prep made ahead before they were taken away
// goes normal. Either way the gateway's requests are named for their
// reports.
func (s *Service) prepChain(ctx context.Context, key, priority string) (context.Context, []ChainModel, error) {
	if openID := s.gatewayWith(ctx, GatewayRoutes, GatewayRouting); openID != 0 && priority == prioritySurplus {
		return withGatewayRouting(ctx, prepRouting("wp-prep-", key, priority)), prepGatewayChain(openID), nil
	}
	ctx = withGatewayRouting(ctx, prepRouting("wp-prep-", key, "normal"))
	chain, err := s.taskChain(ctx, "prep")
	return ctx, chain, err
}

// RunPrepTrials sends a few prep trials at surplus, while the gateway has
// spare capacity. Every five minutes.
func (s *Service) RunPrepTrials(ctx context.Context) {
	s.aiMu.Lock()
	busy := s.trialing
	s.trialing = true
	s.aiMu.Unlock()
	if busy {
		jobBusy(ctx, "the last trial is still going")
		return
	}
	defer func() {
		s.aiMu.Lock()
		s.trialing = false
		s.aiMu.Unlock()
	}()
	switch ok, why, err := s.surplusState(ctx); {
	case err != nil:
		return
	case !ok:
		jobSkip(ctx, why)
		return
	}
	switch ok, err := s.aiTaskSetUp(ctx, "prep"); {
	case err != nil:
		if readFault(err) {
			s.log.WarnContext(ctx, "prep trials: could not tell whether a model is set up", "err", err)
		}
		return
	case !ok:
		jobSkip(ctx, "no AI model is set up for prep")
		return
	}
	if _, _, why := s.gatewayTasksLLM(ctx); why == "" {
		s.queuePrepAgent(ctx)
	}
	stats, err := s.PrepModelStats(ctx)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "prep trials: read the counts", "err", err)
		}
		return
	}
	var skip []string
	for _, st := range stats {
		if st.Tested || st.Waiting >= prepTrialPending {
			skip = append(skip, st.Model)
		}
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT tp.issue_id, tp.text_hash, tp.bundle, tp.model FROM task_preps tp JOIN issues i ON i.id = tp.issue_id
		WHERE tp.state = 'ready' AND i.resolved_at IS NULL AND tp.updated_at >= ?
		  AND (SELECT COUNT(*) FROM prep_trials t WHERE t.issue_id = tp.issue_id AND t.kind = 'chat') < ?
		ORDER BY tp.updated_at DESC LIMIT 20`, s.now().Add(-prepTrialAge).UnixMilli(), prepTrialsPerTask)
	if err != nil {
		s.log.WarnContext(ctx, "prep trials: find preps", "err", err)
		return
	}
	type cand struct {
		id                  int64
		hash, bundle, model string
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.hash, &c.bundle, &c.model); err != nil {
			s.log.WarnContext(ctx, "prep trials: find preps", "err", err)
			break
		}
		cands = append(cands, c)
	}
	rows.Close()
	sent := 0
	for _, c := range cands {
		if sent >= prepTrialBatch || ctx.Err() != nil {
			return
		}
		ok, err := s.prepTrial(ctx, c.id, c.hash, c.bundle, append([]string{prepModelName(c.model)}, skip...))
		var wait *aiWaitError
		if errors.As(err, &wait) {
			return
		}
		if err != nil {
			s.log.Info("prep trial failed", "issue", c.id, "err", err)
			s.notePrepTrialTry(ctx, c.id, "", "", err.Error())
		}
		if ok || err != nil {
			sent++
		}
	}
}

// prepTrial has another model read a current prep's bundle at surplus, with
// skip and the task's earlier trial models left out; it keeps the checked
// picks and the request that made them. false with no error when the prep
// is no longer current.
func (s *Service) prepTrial(ctx context.Context, issueID int64, hash, raw string, skip []string) (bool, error) {
	title, desc, comments, cur, err := s.prepText(ctx, issueID)
	if err != nil || cur != hash {
		return false, err
	}
	var b PrepBundle
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return false, err
	}
	i, err := s.issueByID(ctx, s.db.R, issueID, true)
	if err != nil {
		return false, err
	}
	trows, err := s.db.R.QueryContext(ctx, `SELECT model FROM prep_trials WHERE issue_id = ? AND model != '' AND kind = 'chat'`, issueID)
	if err != nil {
		return false, err
	}
	for trows.Next() {
		var m string
		if err := trows.Scan(&m); err != nil {
			trows.Close()
			return false, err
		}
		skip = append([]string{m}, skip...) // The gateway keeps only the first maxExcludes excludes, so this task's own model goes first.
	}
	trows.Close()
	if err := trows.Err(); err != nil {
		return false, err
	}
	openID := s.gatewayWith(ctx, GatewayRoutes, GatewayRouting)
	if openID == 0 {
		return false, nil
	}
	cands := s.prepCandidates(ctx, i, &b, title+"\n"+desc+"\n"+strings.Join(comments, "\n"))
	prompt := prepPrompt(i.Key, i.Type, title, desc, comments, &b, cands)
	tctx := withGatewayExclude(withGatewayRouting(ctx, prepRouting("wp-trial-", i.Key, prioritySurplus)), skip)
	var answer prepAnswer
	name, requestID, err := s.runAIAnswer(tctx, "prep_trial", prepGatewayChain(openID), prepSystem, prompt, prepSchema, &answer)
	if err != nil {
		return false, err
	}
	model := prepModelName(name)
	if slices.Contains(skip, model) {
		s.log.Info("prep trial: served a model left out", "issue", i.Key, "model", model)
		s.notePrepTrialTry(ctx, issueID, model, requestID, "served a model left out")
		return true, nil
	}
	notes, _ := checkPrep(&answer, &b, cands)
	out, err := json.Marshal(notes)
	if err != nil {
		return false, err
	}
	_, err = s.db.W.ExecContext(ctx, `INSERT INTO prep_trials(issue_id, model, notes, request_id, created_at) VALUES (?,?,?,?,?)`,
		issueID, model, string(out), requestID, s.nowMS())
	return err == nil, err
}

// notePrepTrialTry records a trial that kept no picks: it counts toward
// the task's trials and is never scored.
func (s *Service) notePrepTrialTry(ctx context.Context, issueID int64, model, requestID, why string) {
	if _, err := s.db.W.ExecContext(ctx, `INSERT INTO prep_trials(issue_id, model, notes, request_id, error, created_at) VALUES (?,?,'',?,?,?)`,
		issueID, model, requestID, truncateBytes(why, 500), s.nowMS()); err != nil {
		s.log.WarnContext(ctx, "best-effort write failed", "err", err)
	}
}

// prepScoreReports remembers the gateway's taking a prep score's report.
// A score is final, so its report is sent once; the counts are the row's.
var prepScoreReports = feedbackStore{
	took:   `UPDATE prep_scores SET reported_at = ?3, tries = 0, next_try_at = 0, error = '' WHERE request_id = ?4`,
	failed: `UPDATE prep_scores SET tries = tries + 1, next_try_at = ?, error = ? WHERE request_id = ?`,
	window: prepFeedbackAge,
}

// reportPrepFeedback sends the gateway a report for each scored prep and
// trial it hasn't taken one for yet: verified = the picks the work
// changed, false positives = the rest.
func (s *Service) reportPrepFeedback(ctx context.Context, fl feedbackLLM, p *AIProvider) {
	now := s.now()
	rows, err := s.db.R.QueryContext(ctx, `SELECT request_id, asked_at, tries, hits, picked - hits FROM prep_scores
		WHERE request_id != '' AND reported_at IS NULL AND asked_at >= ? AND next_try_at <= ?
		ORDER BY asked_at LIMIT ?`, now.Add(-prepFeedbackAge).UnixMilli(), now.UnixMilli(), feedbackBatch)
	if err != nil {
		s.log.WarnContext(ctx, "prep feedback: read the scores", "err", err)
		return
	}
	var due []reviewFeedbackDue
	for rows.Next() {
		var d reviewFeedbackDue
		if err := rows.Scan(&d.id, &d.createdAt, &d.tries, &d.verified, &d.falsePos); err != nil {
			rows.Close()
			s.log.WarnContext(ctx, "prep feedback: read the scores", "err", err)
			return
		}
		due = append(due, d)
	}
	rows.Close()
	if rows.Err() != nil {
		return
	}
	s.sendFeedback(ctx, fl, p, due, prepScoreReports)
}

// prepDoneTasks finds done tasks with merged pull requests whose preps or
// trials have no score yet, or a pull request merged since they were
// scored (the work grew: scored again, and reported again if that changes
// the score, WP-250), once none of their pull requests is still open, so
// the truth is the whole of the work.
const prepDoneTasks = `SELECT i.id, MIN(p.merged_at) FROM issues i JOIN statuses st ON st.id = i.status_id
	JOIN external_links el ON el.issue_id = i.id
	JOIN pull_requests p ON el.kind = 'pull' AND (el.url = p.url OR (el.repo = p.repo COLLATE NOCASE AND el.number = p.number AND el.provider = (SELECT f.kind FROM forges f WHERE f.id = p.forge_id)))
	WHERE i.resolved_at IS NOT NULL AND i.resolved_at >= ? AND st.category = 'done' AND p.state = 'merged' AND p.merged_at IS NOT NULL
	  AND EXISTS (SELECT 1 FROM project_repos r WHERE r.project_id = i.project_id AND r.forge_id = p.forge_id AND r.repo = p.repo COLLATE NOCASE)
	  AND (EXISTS (SELECT 1 FROM task_preps tp WHERE tp.issue_id = i.id AND tp.state = 'ready'
	               AND NOT EXISTS (SELECT 1 FROM prep_scores ps WHERE ps.issue_id = i.id AND ps.source = 'prep'))
	    OR EXISTS (SELECT 1 FROM prep_trials t WHERE t.issue_id = i.id AND t.error = ''
	               AND (t.kind != 'agent' OR t.text_hash = (SELECT tp.text_hash FROM task_preps tp WHERE tp.issue_id = i.id AND tp.state = 'ready'))
	               AND NOT EXISTS (SELECT 1 FROM prep_scores ps WHERE ps.issue_id = i.id AND ps.source = 'trial' AND ps.model = t.model))
	    OR EXISTS (SELECT 1 FROM prep_scores ps WHERE ps.issue_id = i.id AND ps.scored_at < p.merged_at))
	  AND NOT EXISTS (SELECT 1 FROM external_links ol JOIN pull_requests op ON ol.kind = 'pull' AND (ol.url = op.url OR (ol.repo = op.repo COLLATE NOCASE AND ol.number = op.number AND ol.provider = (SELECT f.kind FROM forges f WHERE f.id = op.forge_id)))
	               WHERE ol.issue_id = i.id AND op.state IN ('open','draft'))
	GROUP BY i.id ORDER BY random() LIMIT ?`

// prepDoneTask is prepDoneTasks for one task.
var prepDoneTask = strings.Replace(prepDoneTasks, "i.resolved_at >= ?", "i.resolved_at >= ? AND i.id = ?", 1)

// scorePrepNow scores one task's prep and trials if it is done and has
// something unscored (an agentic backtest that just came in), and marks
// the prep policy's estimate out of date: its offline scores moved.
func (s *Service) scorePrepNow(ctx context.Context, issueID int64) {
	var id, merged int64
	err := s.db.R.QueryRowContext(ctx, prepDoneTask, s.now().Add(-prepScoreWindow).UnixMilli(), issueID, 1).Scan(&id, &merged)
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err == nil {
		err = s.scorePrep(ctx, id, merged)
	}
	if err != nil {
		s.log.Info("prep scores: a task", "issue", issueID, "err", err)
		return
	}
	s.prepPolMu.Lock()
	s.prepPolStale = true
	s.prepPolMu.Unlock()
}

// ScorePreps scores the preps and trials of tasks done since the window
// began. Hourly.
func (s *Service) ScorePreps(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, prepDoneTasks, s.now().Add(-prepScoreWindow).UnixMilli(), prepScoreBatch)
	if err != nil {
		s.log.WarnContext(ctx, "prep scores: find tasks", "err", err)
		return
	}
	type done struct{ id, merged int64 }
	var tasks []done
	for rows.Next() {
		var d done
		if err := rows.Scan(&d.id, &d.merged); err != nil {
			s.log.WarnContext(ctx, "prep scores: find tasks", "err", err)
			break
		}
		tasks = append(tasks, d)
	}
	rows.Close()
	for _, d := range tasks {
		if ctx.Err() != nil {
			return
		}
		if err := s.scorePrep(ctx, d.id, d.merged); err != nil {
			s.log.Info("prep scores: a task", "issue", d.id, "err", err)
		}
	}
}

// scorePrep scores one done task's prep and trials. A pick made from what
// was there after the first merge, a change too large to list, or notes
// that can't be read, is kept with no truth: done, and not counted.
func (s *Service) scorePrep(ctx context.Context, issueID, merged int64) error {
	truth, ok, err := s.prepTruth(ctx, issueID)
	if err != nil || !ok {
		return err
	}
	type pick struct {
		source, priority, model, notes, requestID string
		at, basis                                 int64
	}
	var picks []pick
	var p pick
	var prepHash string
	switch err := s.db.R.QueryRowContext(ctx, `SELECT priority, model, notes, request_id, updated_at, text_hash FROM task_preps WHERE issue_id = ? AND state = 'ready'`, issueID).
		Scan(&p.priority, &p.model, &p.notes, &p.requestID, &p.at, &prepHash); {
	case err == nil:
		p.source, p.model, p.basis = "prep", prepModelName(p.model), p.at
		picks = append(picks, p)
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	prepAt := p.at
	rows, err := s.db.R.QueryContext(ctx, `SELECT model, notes, request_id, created_at, kind, text_hash FROM prep_trials WHERE issue_id = ? AND error = ''`, issueID)
	if err != nil {
		return err
	}
	for rows.Next() {
		t := pick{source: "trial", priority: prioritySurplus}
		var kind, hash string
		if err := rows.Scan(&t.model, &t.notes, &t.requestID, &t.at, &kind, &hash); err != nil {
			rows.Close()
			return err
		}
		t.basis = t.at
		if kind == prepAgentKind {
			if prepAt == 0 || hash != prepHash {
				continue
			}
			t.basis = prepAt
		}
		picks = append(picks, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	now := s.nowMS()
	for _, p := range picks {
		var n PrepNotes
		unreadable := json.Unmarshal([]byte(p.notes), &n) != nil
		if unreadable {
			s.log.WarnContext(ctx, "prep scores: notes unreadable, not counted", "issue", issueID, "source", p.source, "model", p.model)
			n = PrepNotes{}
		}
		picked := map[string]bool{}
		for _, f := range n.Files {
			if mistakeFile(f.Path) {
				picked[f.Path] = true
			}
		}
		for _, y := range n.Symbols {
			if mistakeFile(y.Path) {
				picked[y.Path] = true
			}
		}
		hits := 0
		for f := range picked {
			if truth[f] {
				hits++
			}
		}
		t := len(truth)
		if p.basis >= merged || truth == nil || unreadable {
			t, hits = 0, 0 // picks made after the work landed would name its files: not counted
		}
		report := p.requestID
		if t == 0 || len(picked) == 0 || p.at < now-prepFeedbackAge.Milliseconds() {
			report = "" // an empty report withdraws the one sent before; a prep older than the gateway's log can't be reported
		}
		// re-sent when more work merged would change the report; the gateway keeps the latest, however late
		if _, err := s.db.W.ExecContext(ctx, `INSERT INTO prep_scores(issue_id, source, priority, model, picked, truth, hits, scored_at, request_id, asked_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(issue_id, source, model) DO UPDATE SET picked = excluded.picked, truth = excluded.truth, hits = excluded.hits,
				scored_at = excluded.scored_at, request_id = excluded.request_id,
				reported_at = CASE WHEN picked = excluded.picked AND hits = excluded.hits AND request_id = excluded.request_id THEN reported_at END,
				tries = CASE WHEN picked = excluded.picked AND hits = excluded.hits AND request_id = excluded.request_id THEN tries ELSE 0 END,
				next_try_at = CASE WHEN picked = excluded.picked AND hits = excluded.hits AND request_id = excluded.request_id THEN next_try_at ELSE 0 END`,
			issueID, p.source, p.priority, p.model, len(picked), t, hits, now, report, p.at); err != nil {
			return err
		}
	}
	return nil
}

// prepTruth is the non-test, non-doc files a done task's merged pull
// requests changed. ok is false while some pull request's files aren't read
// yet (it reads them); a nil set with ok means a change too large to list.
func (s *Service) prepTruth(ctx context.Context, issueID int64) (map[string]bool, bool, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT p.id FROM issues i
		JOIN external_links el ON el.issue_id = i.id
		JOIN pull_requests p ON el.kind = 'pull' AND (el.url = p.url OR (el.repo = p.repo COLLATE NOCASE AND el.number = p.number AND el.provider = (SELECT f.kind FROM forges f WHERE f.id = p.forge_id)))
		WHERE i.id = ? AND p.state = 'merged'
		  AND EXISTS (SELECT 1 FROM project_repos r WHERE r.project_id = i.project_id AND r.forge_id = p.forge_id AND r.repo = p.repo COLLATE NOCASE)`, issueID)
	if err != nil {
		return nil, false, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, false, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truth := map[string]bool{}
	for _, id := range ids {
		pr, err := s.prByID(ctx, s.db.R, id)
		if err != nil {
			return nil, false, err
		}
		s.loadPRFiles(ctx, pr)
		if pr.tooLarge {
			return nil, true, nil
		}
		if !pr.filesKnown {
			return nil, false, nil
		}
		for _, f := range pr.files {
			if mistakeFile(f) {
				truth[f] = true
			}
		}
	}
	return truth, true, nil
}
