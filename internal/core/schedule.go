package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Time-based automation triggers: an issue coming due, an issue gone quiet,
// schedules (daily, weekly, cron …) and count thresholds. The scheduler
// checks them every few minutes; automation_fires makes each fire once per
// issue and period.

// RunTimedAutomations fires every time-based automation that is due.
func (s *Service) RunTimedAutomations(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+automationCols+` FROM automations WHERE enabled = 1 AND session_id IS NULL ORDER BY project_id, position, id`)
	if err != nil {
		s.log.ErrorContext(ctx, "load automations", "err", err)
		return
	}
	var list []*Automation
	for rows.Next() {
		a, err := scanAutomation(rows)
		if err != nil {
			s.log.ErrorContext(ctx, "load automations", "err", err)
			continue
		}
		if d := triggerDef(a.Rule.Trigger.Event); d != nil && d.Timed {
			list = append(list, a)
		}
	}
	rows.Close()
	for _, a := range list {
		if err := s.runTimed(ctx, a); err != nil {
			s.log.WarnContext(ctx, "timed automation", "automation", a.Name, "err", err)
			s.recordFailure(ctx, a, &Event{Kind: EvScheduled, ProjectID: a.ProjectID, At: s.now().UTC()}, err)
		}
	}
}

// recordFailure writes a run that couldn't even start.
func (s *Service) recordFailure(ctx context.Context, a *Automation, e *Event, err error) {
	now := s.now().UTC()
	run := &AutomationRun{AutomationID: a.ID, Automation: a.Name, ProjectID: a.ProjectID, Via: "schedule", Trigger: a.Rule.Trigger.Event, Summary: eventSummary(e), Event: e,
		Concurrency: a.Rule.concurrency(), CreatedAt: now, StartedAt: &now, depth: 1}
	_, _ = s.finishInstant(ctx, a, run, nil, err)
}

// ruleLocation is the time zone a schedule runs in: the rule's, else the
// project owner's, else the first admin's.
func (s *Service) ruleLocation(ctx context.Context, a *Automation) *time.Location {
	if a.Rule.TimeZone != "" {
		if loc, err := time.LoadLocation(a.Rule.TimeZone); err == nil {
			return loc
		}
	}
	if a.ProjectID != 0 {
		return s.projectLocation(ctx, a.ProjectID)
	}
	var tz string
	if err := s.db.R.QueryRowContext(ctx, `SELECT timezone FROM users WHERE is_admin = 1 AND disabled = 0 ORDER BY id LIMIT 1`).Scan(&tz); err == nil && tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.UTC
}

func (s *Service) runTimed(ctx context.Context, a *Automation) error {
	var projectKey string
	if a.ProjectID != 0 {
		p, err := s.projectByID(ctx, s.db.R, a.ProjectID)
		if err != nil || p.Archived {
			return err
		}
		projectKey = p.Key
	}
	loc := s.ruleLocation(ctx, a)
	now := s.now().In(loc)
	today := now.Format(dateLayout)
	tr := a.Rule.Trigger
	var kind, extra, period string
	switch tr.Event {
	case "count_crossed":
		return s.checkCount(ctx, a)
	case "due_soon":
		kind = EvDueSoon
		until := now.AddDate(0, 0, tr.Days).Format(dateLayout)
		extra = fmt.Sprintf("is:open due:>=%s due:<=%s", today, until)
	case "stale":
		kind = EvStale
		extra = fmt.Sprintf("is:open updated:<-%dd", tr.Days)
	case "schedule":
		kind = EvScheduled
		period = today
		if tr.Every == "cron" {
			c, err := ParseCron(tr.str("cron"))
			if err != nil {
				return err
			}
			// The scheduler looks every five minutes: an occurrence in the
			// last ten is due (and fires once, by its period).
			at, ok := c.Latest(now, 10*time.Minute)
			if !ok {
				return nil
			}
			period = "cron:" + strconv.FormatInt(at.Unix(), 10)
		} else if !scheduleDue(tr, now) {
			return nil
		}
		if strings.TrimSpace(a.Rule.If) == "" {
			// A rule about no issue in particular, e.g. a recurring issue.
			return s.fire(ctx, a, &Event{Kind: kind, ProjectID: a.ProjectID, New: today, At: s.now().UTC()}, period)
		}
	}
	q := strings.TrimSpace(a.Rule.If + " " + extra)
	// Page through every match: issues that already fired stay in the
	// results (a rule that only notifies changes nothing), so the first page
	// alone could hide the rest forever.
	var issues []*Issue
	for offset := 0; offset < 10000; offset += 500 {
		res, err := s.Search(ctx, SystemActor(), SearchParams{Query: q, Project: projectKey, Limit: 500, Offset: offset})
		if err != nil {
			return err
		}
		issues = append(issues, res.Issues...)
		if len(res.Issues) < 500 {
			break
		}
	}
	for _, i := range issues {
		p := cmp(period, today)
		switch tr.Event {
		case "due_soon":
			p = deref(i.DueDate) // again if the due date moves
		case "stale":
			p = strconv.FormatInt(i.UpdatedAt.UnixMilli(), 10) // once per quiet spell
		}
		e := &Event{Kind: kind, ProjectID: i.ProjectID, IssueID: i.ID, IssueKey: i.Key, New: today, At: s.now().UTC()}
		if err := s.fire(ctx, a, e, p); err != nil {
			return err
		}
	}
	return nil
}

// fire runs the automation once for (issue, period); later calls for the
// same pair do nothing.
func (s *Service) fire(ctx context.Context, a *Automation, e *Event, period string) error {
	res, err := s.db.W.ExecContext(ctx, `INSERT OR IGNORE INTO automation_fires(automation_id, issue_id, period, fired_at) VALUES (?,?,?,?)`,
		a.ID, e.IssueID, period, s.nowMS())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	_, err = s.startRun(ctx, SystemActor(), a, e, fireOpts{Via: "schedule"})
	return err
}

// scheduleDue reports whether a schedule's occurrence for today has come.
func scheduleDue(tr Trigger, now time.Time) bool {
	switch tr.Every {
	case "weekday":
		if now.Weekday() == time.Saturday || now.Weekday() == time.Sunday {
			return false
		}
	case "week":
		if int(now.Weekday()) != tr.Weekday {
			return false
		}
	case "month":
		if now.Day() != tr.MonthDay {
			return false
		}
	}
	at, err := time.ParseInLocation("15:04", tr.At, now.Location())
	if err != nil {
		return false
	}
	return now.Hour()*60+now.Minute() >= at.Hour()*60+at.Minute()
}

// projectLocation is the timezone of the project's first owner, which
// schedules run in.
func (s *Service) projectLocation(ctx context.Context, projectID int64) *time.Location {
	var tz string
	err := s.db.R.QueryRowContext(ctx, `SELECT u.timezone FROM project_members m JOIN users u ON u.id = m.user_id
		WHERE m.project_id = ? AND m.role = 'owner' ORDER BY m.created_at LIMIT 1`, projectID).Scan(&tz)
	if err == nil && tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return time.UTC
}

// ---- count thresholds ----

// countDebounce is how long after an issue changes the counts are checked.
var countDebounce = 30 * time.Second

// scheduleCountCheck checks a count automation soon after issues change,
// once however many changes come in the meantime.
func (s *Service) scheduleCountCheck(a *Automation) {
	s.autoRuns.init()
	s.autoRuns.mu.Lock()
	defer s.autoRuns.mu.Unlock()
	if _, waiting := s.autoRuns.timers[a.ID]; waiting {
		return
	}
	id := a.ID
	s.autoRuns.timers[id] = time.AfterFunc(countDebounce, func() {
		s.autoRuns.mu.Lock()
		delete(s.autoRuns.timers, id)
		s.autoRuns.mu.Unlock()
		if s.bgCtx.Err() != nil {
			return
		}
		s.bg.Add(1)
		defer s.bg.Done()
		cur, err := s.automationByID(s.bgCtx, id)
		if err != nil || !cur.Enabled || cur.Rule.Trigger.Event != "count_crossed" {
			return
		}
		if err := s.checkCount(s.bgCtx, cur); err != nil {
			s.log.Warn("count check", "automation", cur.Name, "err", err)
		}
	})
}

// checkCount counts the issues a count automation watches and fires it when
// the count has crossed its limit since the last look. The first look only
// records where things stand.
func (s *Service) checkCount(ctx context.Context, a *Automation) error {
	s.countMu.Lock()
	defer s.countMu.Unlock()
	tr := a.Rule.Trigger
	query, op, n := tr.str("query"), tr.str("op"), int(tr.num("n"))
	p := SearchParams{Query: query, Limit: 1}
	if a.ProjectID != 0 {
		pr, err := s.projectByID(ctx, s.db.R, a.ProjectID)
		if err != nil {
			return err
		}
		p.Project = pr.Key
	}
	res, err := s.Search(ctx, SystemActor(), p)
	if err != nil {
		return err
	}
	count := res.Total
	met := op == ">=" && count >= n || op == "<=" && count <= n
	var was string
	err = s.db.R.QueryRowContext(ctx, `SELECT value FROM automation_state WHERE automation_id = ? AND key = 'count'`, a.ID).Scan(&was)
	known := !errors.Is(err, sql.ErrNoRows)
	if err != nil && known {
		return err
	}
	prev := false
	if known {
		var st struct {
			Met bool `json:"met"`
		}
		_ = json.Unmarshal([]byte(was), &st)
		prev = st.Met
	}
	b, _ := json.Marshal(map[string]any{"met": met, "count": count})
	if _, err := s.db.W.ExecContext(ctx, `INSERT INTO automation_state(automation_id, key, value, updated_at) VALUES (?, 'count', ?, ?)
		ON CONFLICT(automation_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, a.ID, string(b), s.nowMS()); err != nil {
		return err
	}
	if !known || !met || prev {
		return nil
	}
	e := &Event{Kind: EvCountCrossed, ProjectID: a.ProjectID, At: s.now().UTC(),
		Data: map[string]any{"count": count, "n": n, "op": op, "query": query}}
	_, err = s.startRun(ctx, SystemActor(), a, e, fireOpts{Via: "event"})
	return err
}
