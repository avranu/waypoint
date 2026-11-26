package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// NotifyJobs is the notification kind for scheduled jobs that keep failing,
// are stuck or have stopped running.
const NotifyJobs = "server"

const (
	// problemHold is how long a problem must keep happening before the
	// admins are told. A day, so a forge's bad hour or a deploy tells
	// nobody. It is a choice, not a measurement (WP-ADR139).
	problemHold = 24 * time.Hour
	// problemGap is the quiet after which a problem counts as over. Just
	// over a day, so something that fails once a day is one problem, not a
	// new one each day.
	problemGap = 25 * time.Hour
	// problemMinCount is how many times it must have happened: one record
	// and then a day of nothing is not a problem that persists. A job that
	// runs less than daily needs one fewer (its second run).
	problemMinCount = 3
	// problemRemind is how often the admins hear again of a problem nobody
	// has acknowledged.
	problemRemind = 7 * 24 * time.Hour
	// ackQuiet: an acknowledged problem that is over, stays quiet for this
	// long and comes back is new again.
	ackQuiet = 7 * 24 * time.Hour
	// jobOverdueMin is the least time before a job counts as stuck or
	// silent, whatever its period.
	jobOverdueMin = 15 * time.Minute
	// startupJobStuck is that time for a job with no period.
	startupJobStuck = time.Hour
	jobHoursKeep    = 90 * 24 * time.Hour
	// jobRowKeep is how long the row of a job this server no longer has
	// stays (a rollback brings the job back).
	jobRowKeep = 7 * 24 * time.Hour
	// maxOpenProblems bounds one source's open problems on disk; past it
	// they are counted together.
	maxOpenProblems = 500
	// noGap is a gap no quiet is longer than.
	noGap = 100 * 365 * 24 * time.Hour
)

// problemGapFor stretches the gap for a job that runs seldom: four runs
// must pass without the problem.
func problemGapFor(period time.Duration) time.Duration { return max(problemGap, 4*period) }

// jobOverdue is how long after a run should have started, or ended, a job
// counts as silent, or stuck.
func jobOverdue(period time.Duration) time.Duration {
	if period <= 0 {
		return startupJobStuck
	}
	return max(jobOverdueMin, 3*period)
}

// JobStatus is one scheduled job: its last run and whether it works.
type JobStatus struct {
	Name     string `json:"name"`
	PeriodMS int64  `json:"period_ms"` // 0: runs once when the server starts
	// Lane is what it waits its turn in: "" the scheduler's loop, else a
	// background lane (its own name when it runs alone).
	Lane    string `json:"lane,omitempty"`
	Runs    int64  `json:"runs"`
	Skips   int64  `json:"skips,omitempty"`
	Running bool   `json:"running"`
	// RunningSince is when the oldest run still going began.
	RunningSince *time.Time `json:"running_since,omitempty"`
	// LastStart to LastEnd is the last run to end.
	LastStart *time.Time `json:"last_start,omitempty"`
	LastEnd   *time.Time `json:"last_end,omitempty"`
	LastMS    int64      `json:"last_ms"`
	MaxMS     int64      `json:"max_ms"`
	// LastResult is clean, warned, failed, skipped, stopped or crashed; LastNote the
	// message that made it so.
	LastResult  string     `json:"last_result,omitempty"`
	LastNote    string     `json:"last_note,omitempty"`
	LastCleanAt *time.Time `json:"last_clean_at,omitempty"`
	// BusySince is when it began to skip every run because the last was
	// still going.
	BusySince *time.Time `json:"busy_since,omitempty"`
	// State is "" (working), stuck (a run has gone on too long, or every
	// run since BusySince has found the last one still going) or silent (it
	// should have started and hasn't).
	State string `json:"state,omitempty"`
}

// JobProblem is one thing that keeps going wrong: a warning or an error by
// where it was logged, its message and what it was about.
type JobProblem struct {
	ID          int64     `json:"id"`
	Source      string    `json:"source"`
	Message     string    `json:"message"`
	Target      string    `json:"target,omitempty"`
	Level       string    `json:"level"`
	FirstAt     time.Time `json:"first_at"`
	LastAt      time.Time `json:"last_at"`
	Count       int64     `json:"count"`
	StreakSince time.Time `json:"streak_since"`
	StreakCount int64     `json:"streak_count"`
	// StreakUpMS is how long the streak has lasted with the server running:
	// the time it was down is not counted.
	StreakUpMS int64      `json:"streak_up_ms"`
	Detail     string     `json:"detail,omitempty"`
	Persistent bool       `json:"persistent"`
	ClearedAt  *time.Time `json:"cleared_at,omitempty"`
	AckedAt    *time.Time `json:"acked_at,omitempty"`
	AckedBy    string     `json:"acked_by,omitempty"`
	AckReason  string     `json:"ack_reason,omitempty"`
}

// label is the problem in a line: "forge sync: repo sync failed (owner/repo)".
func (p JobProblem) label() string {
	s := p.Source + ": " + p.Message
	if p.Target != "" {
		s += " (" + p.Target + ")"
	}
	return s
}

// jobSnapshot is what the ledger counted since the last save.
type jobSnapshot struct {
	jobs                []jobState
	periods             map[string]time.Duration
	problems            map[problemKey]*problemSeen
	hours               map[hourKey]*hourCount
	running             map[string]int64
	busy                map[string]int64
	wasRunning, wasBusy map[string]int64
	learned             int
}

func (sn *jobSnapshot) empty() bool {
	return len(sn.jobs) == 0 && len(sn.problems) == 0 && len(sn.hours) == 0 && len(sn.running) == 0 && len(sn.busy) == 0
}

// take hands over the counts since the last save and starts afresh.
func (l *jobLedger) take() *jobSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	sn := &jobSnapshot{problems: l.problems, hours: l.hours, periods: map[string]time.Duration{}, running: map[string]int64{}, busy: map[string]int64{}, wasRunning: map[string]int64{}, wasBusy: map[string]int64{}, learned: len(l.learned)}
	l.problems, l.hours, l.perSrc = map[problemKey]*problemSeen{}, map[hourKey]*hourCount{}, map[string]int{}
	for _, st := range l.jobs {
		sn.periods[st.name] = st.period
		var since int64
		if len(st.running) > 0 && !l.stopping.Load() && st.name != jobHealthName {
			since = slices.MinFunc(st.running, time.Time.Compare).UnixMilli()
		}
		if l.savedRunning[st.name] != since {
			sn.running[st.name], sn.wasRunning[st.name] = since, l.savedRunning[st.name]
			l.savedRunning[st.name] = since
		}
		var busy int64
		if !st.busySince.IsZero() && !l.stopping.Load() {
			busy = st.busySince.UnixMilli()
		}
		if l.savedBusy[st.name] != busy {
			sn.busy[st.name], sn.wasBusy[st.name] = busy, l.savedBusy[st.name]
			l.savedBusy[st.name] = busy
		}
		if !st.dirty {
			continue
		}
		c := *st
		c.running = nil
		sn.jobs = append(sn.jobs, c)
		st.dirty, st.ended, st.cleanRun, st.runs, st.skips = false, false, false, 0, 0
	}
	sort.Slice(sn.jobs, func(a, b int) bool { return sn.jobs[a].name < sn.jobs[b].name })
	return sn
}

// giveBack returns a snapshot that could not be saved, so its counts go
// with the next save.
func (l *jobLedger) giveBack(sn *jobSnapshot) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for name, was := range sn.wasRunning {
		l.savedRunning[name] = was
	}
	for name, was := range sn.wasBusy {
		l.savedBusy[name] = was
	}
	for k, p := range sn.problems {
		if cur := l.problems[k]; cur != nil {
			cur.count += p.count
			cur.first = p.first
			cur.level = max(cur.level, p.level)
		} else {
			l.problems[k] = p
			l.perSrc[k.source]++
		}
	}
	for _, secret := range l.learned[sn.learned:] {
		l.scrubProblems(secret)
	}
	for k, h := range sn.hours {
		cur := l.hours[k]
		if cur == nil {
			l.hours[k] = h
			continue
		}
		cur.runs += h.runs
		cur.unclean += h.unclean
		cur.skips += h.skips
		cur.warnings += h.warnings
		cur.errors += h.errors
		cur.noted += h.noted
		cur.msTotal += h.msTotal
		cur.msMax = max(cur.msMax, h.msMax)
	}
	for _, old := range sn.jobs {
		st := l.jobs[old.name]
		if st == nil {
			continue
		}
		st.dirty = true
		st.runs += old.runs
		st.skips += old.skips
		st.cleanRun = st.cleanRun || old.cleanRun
		if !st.ended && old.ended {
			st.ended = true
		}
	}
}

// saveJobHealth writes what the ledger counted since the last save.
func (s *Service) saveJobHealth(ctx context.Context) {
	s.jobs.saving.Lock()
	defer s.jobs.saving.Unlock()
	sn := s.jobs.take()
	if sn.empty() {
		return
	}
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		for _, j := range sn.jobs {
			if err := saveJob(ctx, t, j); err != nil {
				return err
			}
		}
		open := map[string]int{}
		keys := make([]problemKey, 0, len(sn.problems))
		for k := range sn.problems {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, func(a, b problemKey) int {
			if c := strings.Compare(a.source, b.source); c != 0 {
				return c
			}
			if c := strings.Compare(a.message, b.message); c != 0 {
				return c
			}
			return strings.Compare(a.target, b.target)
		})
		for _, k := range keys {
			p := sn.problems[k]
			k, err := capProblem(ctx, t, k, open)
			if err != nil {
				return err
			}
			gap := problemGapFor(sn.periods[k.source])
			if period, isJob := sn.periods[k.source]; isJob && period <= 0 {
				gap = noGap
			}
			if err := saveProblem(ctx, t, k, p, gap); err != nil {
				return err
			}
		}
		for name, since := range sn.running {
			var v any
			if since != 0 {
				v = since
			}
			if _, err := t.ExecContext(ctx, `UPDATE job_health SET running_since = ? WHERE name = ?`, v, name); err != nil {
				return err
			}
		}
		for name, since := range sn.busy {
			var v any
			if since != 0 {
				v = since
			}
			if _, err := t.ExecContext(ctx, `UPDATE job_health SET busy_since = ? WHERE name = ?`, v, name); err != nil {
				return err
			}
		}
		for k, h := range sn.hours {
			if _, err := t.ExecContext(ctx, `INSERT INTO job_hours(source, hour, runs, unclean, skips, warnings, errors, noted, ms_total, ms_max)
				VALUES (?,?,?,?,?,?,?,?,?,?)
				ON CONFLICT(source, hour) DO UPDATE SET runs = runs + excluded.runs, unclean = unclean + excluded.unclean, skips = skips + excluded.skips,
					warnings = warnings + excluded.warnings, errors = errors + excluded.errors, noted = noted + excluded.noted,
					ms_total = ms_total + excluded.ms_total, ms_max = max(ms_max, excluded.ms_max)`,
				k.source, k.hour, h.runs, h.unclean, h.skips, h.warnings, h.errors, h.noted, h.msTotal, h.msMax); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.jobs.giveBack(sn)
		if ctx.Err() == nil {
			s.log.ErrorContext(ctx, "save job health", "err", err)
		}
	}
}

func saveJob(ctx context.Context, t *tx, j jobState) error {
	var start, end, clean any
	if j.ended {
		start, end = j.lastStart.UnixMilli(), j.lastEnd.UnixMilli()
	}
	if j.cleanRun {
		clean = j.lastClean.UnixMilli()
	}
	if _, err := t.ExecContext(ctx, `INSERT INTO job_health(name, period_ms, runs, skips, last_start, last_end, last_ms, max_ms, last_result, last_note, last_clean_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET period_ms = excluded.period_ms, runs = runs + excluded.runs, skips = skips + excluded.skips,
			last_start = coalesce(excluded.last_start, last_start), last_end = coalesce(excluded.last_end, last_end),
			last_ms = CASE WHEN excluded.last_end IS NULL THEN last_ms ELSE excluded.last_ms END,
			max_ms = max(max_ms, excluded.max_ms),
			last_result = CASE WHEN excluded.last_end IS NULL THEN last_result ELSE excluded.last_result END,
			last_note = CASE WHEN excluded.last_end IS NULL THEN last_note ELSE excluded.last_note END,
			last_clean_at = coalesce(excluded.last_clean_at, last_clean_at)`,
		j.name, j.period.Milliseconds(), j.runs, j.skips, start, end, j.lastMS, j.maxMS, j.lastResult, excerpt(j.lastNote, 200), clean); err != nil {
		return err
	}
	if j.cleanRun && j.period <= 0 {
		if _, err := t.ExecContext(ctx, `UPDATE job_problems SET cleared_at = ? WHERE source = ? AND cleared_at IS NULL AND last_at < ?`,
			j.lastClean.UnixMilli(), j.name, j.lastStart.UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}

// capProblem is k, or the catch-all of its source when k would be a new row
// and the source already has maxOpenProblems open: a message built from
// data must not grow the table without bound.
func capProblem(ctx context.Context, t *tx, k problemKey, open map[string]int) (problemKey, error) {
	if k.message == otherProblems {
		return k, nil
	}
	var exists bool
	if err := t.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM job_problems WHERE source = ? AND message = ? AND target = ?)`,
		k.source, k.message, k.target).Scan(&exists); err != nil || exists {
		return k, err
	}
	n, ok := open[k.source]
	if !ok {
		if err := t.QueryRowContext(ctx, `SELECT count(*) FROM job_problems WHERE source = ? AND cleared_at IS NULL`, k.source).Scan(&n); err != nil {
			return k, err
		}
	}
	if n >= maxOpenProblems {
		open[k.source] = n
		return problemKey{source: k.source, message: otherProblems}, nil
	}
	open[k.source] = n + 1
	return k, nil
}

func saveProblem(ctx context.Context, t *tx, k problemKey, p *problemSeen, gap time.Duration) error {
	level := "warn"
	if p.level >= slog.LevelError {
		level = "error"
	}
	const restart = `(cleared_at IS NOT NULL OR excluded.first_at - last_at - away_ms > ?9)`
	// #nosec G202 -- restart is the fixed text above
	_, err := t.ExecContext(ctx, `INSERT INTO job_problems(source, message, target, level, first_at, last_at, count, streak_since, streak_count, detail)
		VALUES (?1,?2,?3,?4,?5,?6,?7,?5,?7,?8)
		ON CONFLICT(source, message, target) DO UPDATE SET level = CASE WHEN `+restart+` OR excluded.level = 'error' THEN excluded.level ELSE level END, detail = excluded.detail,
			last_at = max(last_at, excluded.last_at), count = count + excluded.count,
			streak_since = CASE WHEN `+restart+` THEN excluded.first_at ELSE streak_since END,
			streak_count = CASE WHEN `+restart+` THEN excluded.count ELSE streak_count + excluded.count END,
			streak_away_ms = CASE WHEN `+restart+` THEN 0 ELSE streak_away_ms + away_ms END,
			alerted_at = CASE WHEN `+restart+` THEN NULL ELSE alerted_at END,
			acked_at = CASE WHEN cleared_at IS NOT NULL AND excluded.first_at - last_at - away_ms > ?10 THEN NULL ELSE acked_at END,
			acked_by = CASE WHEN cleared_at IS NOT NULL AND excluded.first_at - last_at - away_ms > ?10 THEN NULL ELSE acked_by END,
			ack_reason = CASE WHEN cleared_at IS NOT NULL AND excluded.first_at - last_at - away_ms > ?10 THEN '' ELSE ack_reason END,
			away_ms = 0,
			cleared_at = NULL`,
		k.source, k.message, k.target, level, p.first.UnixMilli(), p.last.UnixMilli(), p.count, p.detail, gap.Milliseconds(), ackQuiet.Milliseconds())
	return err
}

// rowsOf runs a query and hands each row to row.
func rowsOf(ctx context.Context, q queryer, query string, args []any, row func(*sql.Rows) error) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := row(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// CheckJobHealth saves the counts, marks the problems that are over, and
// tells the admins what became persistent, stuck or silent and what
// recovered: one notification for all of it, stored in the write that marks
// them told. It runs on its own timer, off the scheduler's loop, so a job
// that holds the loop is still reported.
func (s *Service) CheckJobHealth(ctx context.Context) {
	s.saveJobHealth(ctx)
	now := s.now()
	running := s.jobs.jobsRunning()
	declared := s.jobs.declared()
	if err := s.write(ctx, SystemActor(), func(t *tx) error { return clearProblemsOver(ctx, t, now) }); err != nil {
		if ctx.Err() == nil {
			s.log.ErrorContext(ctx, "check job health", "err", err)
		}
		return
	}
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		over, err := problemsRecovered(ctx, t)
		if err != nil {
			return err
		}
		stuck, back, err := s.jobsStuck(ctx, t, now, running, declared)
		if err != nil {
			return err
		}
		persistent, err := problemsToTell(ctx, t, now)
		if err != nil {
			return err
		}
		bad, good := append(stuck, persistent...), append(back, over...)
		if len(bad) == 0 && len(good) == 0 {
			return nil
		}
		title, body := jobDigest(bad, good)
		n, err := t.storeAdminNotice(ctx, NotifyJobs, title, body, "")
		if err != nil {
			return err
		}
		if n == 0 {
			return errNoAdminToTell
		}
		return nil
	})
	switch {
	case errors.Is(err, errNoAdminToTell):
		s.warnNoAdmin(ctx, now)
	case err != nil && ctx.Err() == nil:
		s.log.ErrorContext(ctx, "check job health", "err", err)
	}
	if err == nil || errors.Is(err, errNoAdminToTell) {
		s.markHealthChecked(ctx, now)
	}
	s.pruneJobHealth(ctx, now, declared)
}

// errNoAdminToTell fails the health check's write of the told marks when it
// had something to tell and no admin to tell it to.
var errNoAdminToTell = errors.New("there is no admin to tell")

// noAdminWarnEvery is how often that is logged: the check runs each minute.
const noAdminWarnEvery = time.Hour

func (s *Service) warnNoAdmin(ctx context.Context, now time.Time) {
	s.jobs.mu.Lock()
	due := now.Sub(s.jobs.noAdminWarned) >= noAdminWarnEvery
	if due {
		s.jobs.noAdminWarned = now
	}
	s.jobs.mu.Unlock()
	if due {
		s.log.WarnContext(ctx, "job health: there is no admin to tell")
	}
}

// clearProblemsOver marks the problems quiet for longer than their gap as
// over. A startup job's problem is not over by being quiet (it has not run
// again); a clean run of it ends it (saveJob). The time the server was down
// does not count as quiet (away_ms): a problem told of before a long stop is
// still open after it.
func clearProblemsOver(ctx context.Context, t *tx, now time.Time) error {
	_, err := t.ExecContext(ctx, `UPDATE job_problems SET cleared_at = ?1
		WHERE cleared_at IS NULL
		AND last_at + away_ms < ?1 - max(?2, 4 * coalesce((SELECT period_ms FROM job_health j WHERE j.name = job_problems.source), 0))
		AND NOT EXISTS (SELECT 1 FROM job_health j WHERE j.name = job_problems.source AND j.period_ms = 0)`,
		now.UnixMilli(), problemGap.Milliseconds())
	return err
}

// problemsRecovered returns the problems that are over and that the admins
// had been told of, and marks them as told it is over.
func problemsRecovered(ctx context.Context, t *tx) ([]string, error) {
	var out []string
	var ids []int64
	err := rowsOf(ctx, t, `SELECT id, source, message, target FROM job_problems
		WHERE cleared_at IS NOT NULL AND alerted_at IS NOT NULL ORDER BY source, message, target`, nil, func(rows *sql.Rows) error {
		var p JobProblem
		if err := rows.Scan(&p.ID, &p.Source, &p.Message, &p.Target); err != nil {
			return err
		}
		ids = append(ids, p.ID)
		out = append(out, p.label())
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := t.ExecContext(ctx, `UPDATE job_problems SET alerted_at = NULL WHERE id = ?`, id); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// jobsBack clears the stuck or silent mark of the jobs that have ended a
// run since and are neither now, and returns them. A job with a run that
// hangs for good while later runs of it end stays stuck: it is told once,
// not as recovered and stuck again with every run.
func jobsBack(ctx context.Context, t *tx, state map[string]string) ([]string, error) {
	var out, names []string
	err := rowsOf(ctx, t, `SELECT name, alerted_for FROM job_health
		WHERE alerted_at IS NOT NULL AND last_end IS NOT NULL AND last_end >= alerted_at AND last_result != ? ORDER BY name`, []any{jobInterrupted}, func(rows *sql.Rows) error {
		var name, was string
		if err := rows.Scan(&name, &was); err != nil {
			return err
		}
		if state[name] != "" {
			return nil
		}
		names = append(names, name)
		out = append(out, name+" runs again (it was "+was+")")
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if _, err := t.ExecContext(ctx, `UPDATE job_health SET alerted_at = NULL, alerted_for = '' WHERE name = ?`, name); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Job states.
const (
	jobStuck  = "stuck"
	jobSilent = "silent"
)

// jobTrouble finds the jobs whose run has gone on too long and the ones
// that should have started and haven't. behind names, for a silent job, the
// stuck job it waits its turn after.
func (s *Service) jobTrouble(now time.Time, running []JobStatus, declared []jobDecl, lastActive map[string]int64, busy map[string]time.Time) (state, line, behind map[string]string) {
	state, line, behind = map[string]string{}, map[string]string{}, map[string]string{}
	isRunning := map[string]bool{}
	stuckIn := map[string]string{}
	for _, j := range running {
		isRunning[j.Name] = true
		if took := now.Sub(*j.RunningSince); took > jobOverdue(time.Duration(j.PeriodMS)*time.Millisecond) {
			state[j.Name] = jobStuck
			line[j.Name] = fmt.Sprintf("%s has been running for %s", j.Name, spanText(took))
			if _, ok := stuckIn[j.Lane]; !ok {
				stuckIn[j.Lane] = j.Name
			}
		}
	}
	for _, d := range declared {
		since, ok := busy[d.name]
		if !ok || state[d.name] != "" || now.Sub(since) <= jobOverdue(d.period) {
			continue
		}
		state[d.name] = jobStuck
		line[d.name] = fmt.Sprintf("%s has skipped every run since %s: the last run is still going", d.name, since.UTC().Format("15:04 MST"))
		if _, ok := stuckIn[d.lane]; !ok {
			stuckIn[d.lane] = d.name
		}
	}
	for _, d := range declared {
		if d.period <= 0 || isRunning[d.name] || state[d.name] != "" {
			continue
		}
		since := s.started
		if at := lastActive[d.name]; at > since.UnixMilli() {
			since = msTime(at)
		}
		quiet := now.Sub(since)
		if quiet <= jobOverdue(d.period) {
			continue
		}
		state[d.name] = jobSilent
		line[d.name] = fmt.Sprintf("%s has not started for %s (it runs every %s)", d.name, spanText(quiet), spanText(d.period))
		if by, ok := stuckIn[d.lane]; ok {
			behind[d.name] = by
		}
	}
	return state, line, behind
}

// jobsStuck marks the stuck and the silent jobs and returns the ones the
// admins have not been told of, or not for problemRemind, and the ones that
// run again. Jobs waiting behind a stuck one are silent because of it, so
// they are counted, not listed.
func (s *Service) jobsStuck(ctx context.Context, t *tx, now time.Time, running []JobStatus, declared []jobDecl) (bad, back []string, err error) {
	lastActive := map[string]int64{}
	err = rowsOf(ctx, t, `SELECT name, max(coalesce(last_start, 0), coalesce(last_end, 0)) FROM job_health`, nil, func(rows *sql.Rows) error {
		var name string
		var at int64
		if err := rows.Scan(&name, &at); err != nil {
			return err
		}
		lastActive[name] = at
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	state, line, behind := s.jobTrouble(now, running, declared, lastActive, s.jobs.busySinces())
	if back, err = jobsBack(ctx, t, state); err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(state))
	for name := range state {
		names = append(names, name)
	}
	sort.Slice(names, func(a, b int) bool {
		if sa, sb := state[names[a]], state[names[b]]; sa != sb {
			return sa == jobStuck
		}
		return names[a] < names[b]
	})
	waiting := map[string]int{}
	for _, name := range names {
		res, err := t.ExecContext(ctx, `UPDATE job_health SET alerted_at = ?1, alerted_for = ?2
			WHERE name = ?3 AND (alerted_at IS NULL OR alerted_for != ?2 OR alerted_at <= ?4)`,
			now.UnixMilli(), state[name], name, now.Add(-problemRemind).UnixMilli())
		if err != nil {
			return nil, nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if by := behind[name]; by != "" {
			waiting[by]++
			continue
		}
		bad = append(bad, line[name])
	}
	by := make([]string, 0, len(waiting))
	for name := range waiting {
		by = append(by, name)
	}
	sort.Strings(by)
	for _, name := range by {
		verb := "have"
		if waiting[name] == 1 {
			verb = "has"
		}
		bad = append(bad, fmt.Sprintf("%s waiting behind %s %s not started since", plural1(waiting[name], "job"), name, verb))
	}
	return bad, back, nil
}

// persistentSQL is the problems that persist: open, and either a startup
// job's (its one run had it), or one that has gone on for the hold and came
// back: its streak is at least a day old, spans at least half of the longer
// of a day and the job's period (a second run of a slow job; half a day
// apart for anything fast, so one bad hour is not enough), and has been seen
// problemMinCount times, one fewer for a job that runs less than daily. Every
// source's warnings count, not only its errors.
//
// ?1 now, ?2 the hold (also the period below which a job is "fast"), ?3 the
// least count.
const persistentSQL = `(p.cleared_at IS NULL AND (
	(j.name IS NOT NULL AND j.period_ms = 0)
	OR (?1 - p.streak_since - p.streak_away_ms - p.away_ms >= ?2
		AND p.last_at - p.streak_since - p.streak_away_ms >= max(?2, coalesce(j.period_ms, 0)) / 2
		AND p.streak_count >= CASE WHEN coalesce(j.period_ms, 0) < ?2 THEN ?3 ELSE ?3 - 1 END)))`

// problemsToTell marks the persistent problems the admins have not been told
// of, or not for problemRemind, and returns them.
func problemsToTell(ctx context.Context, t *tx, now time.Time) ([]string, error) {
	var out []string
	var ids []int64
	err := rowsOf(ctx, t, `SELECT p.id, p.source, p.message, p.target, p.streak_since, p.streak_count, p.alerted_at IS NOT NULL, ?1 - p.streak_since - p.streak_away_ms - p.away_ms
		FROM job_problems p LEFT JOIN job_health j ON j.name = p.source
		WHERE `+persistentSQL+` AND p.acked_at IS NULL AND (p.alerted_at IS NULL OR p.alerted_at <= ?1 - ?4)
		ORDER BY p.source, p.message, p.target`,
		[]any{now.UnixMilli(), problemHold.Milliseconds(), problemMinCount, problemRemind.Milliseconds()}, func(rows *sql.Rows) error {
			var p JobProblem
			var since, up int64
			var again bool
			if err := rows.Scan(&p.ID, &p.Source, &p.Message, &p.Target, &since, &p.StreakCount, &again, &up); err != nil {
				return err
			}
			ids = append(ids, p.ID)
			l := fmt.Sprintf("#%d %s: %d× over %s", p.ID, excerpt(p.label(), 60), p.StreakCount, spanText(time.Duration(max(up, 0))*time.Millisecond))
			if again {
				l += ", still unacknowledged"
			}
			out = append(out, l)
			return nil
		})
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := t.ExecContext(ctx, `UPDATE job_problems SET alerted_at = ? WHERE id = ?`, now.UnixMilli(), id); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// spanText is a length of time in a word: 40s, 12m, 5h, 3d.
func spanText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(max(d, 0).Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		if m := int(d.Minutes()) % 60; m != 0 && d < 10*time.Hour {
			return fmt.Sprintf("%dh%02dm", int(d.Hours()), m)
		}
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours())/24)
}

// jobDigest words the one notification. A notification's body is kept as
// one line (excerpt), so the lists are sentences, the worst first.
func jobDigest(bad, good []string) (title, body string) {
	const most = 4
	list := func(lines []string) string {
		short := make([]string, 0, most+1)
		for i, l := range lines {
			if i == most {
				short = append(short, fmt.Sprintf("and %d more (`wp server problems --all` lists them)", len(lines)-most))
				break
			}
			short = append(short, excerpt(l, 110))
		}
		return strings.Join(short, "; ")
	}
	switch {
	case len(bad) == 1:
		title = "The server needs a look: " + bad[0]
	case len(bad) > 1:
		title = fmt.Sprintf("%d things keep going wrong on the server", len(bad))
	case len(good) == 1:
		title = "Recovered: " + good[0]
	default:
		title = fmt.Sprintf("%d server problems are over", len(good))
	}
	var b strings.Builder
	if len(bad) > 0 {
		b.WriteString("Going wrong: " + list(bad) + ". `wp server problems` lists them with the last record of each; `wp server ack N -m WHY` for one that is expected.")
	}
	if len(good) > 0 {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString("Over: " + list(good) + ".")
	}
	return excerpt(title, 200), b.String()
}

// pruneJobHealth drops hourly counts and finished problems past their keep,
// and the rows of jobs this server no longer has. Once an hour is plenty.
func (s *Service) pruneJobHealth(ctx context.Context, now time.Time, declared []jobDecl) {
	s.jobs.mu.Lock()
	due := now.Sub(s.jobs.pruned) >= time.Hour
	if due {
		s.jobs.pruned = now
	}
	s.jobs.mu.Unlock()
	if !due {
		return
	}
	has := map[string]bool{}
	for _, d := range declared {
		has[d.name] = true
	}
	cut := now.Add(-jobHoursKeep).UnixMilli()
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		for _, q := range []string{
			`DELETE FROM job_hours WHERE hour < ?`,
			`DELETE FROM job_problems WHERE cleared_at IS NOT NULL AND cleared_at < ?`,
		} {
			if _, err := t.ExecContext(ctx, q, cut); err != nil {
				return err
			}
		}
		var gone []string
		err := rowsOf(ctx, t, `SELECT name FROM job_health WHERE coalesce(last_end, last_start, 0) < ?`, []any{now.Add(-jobRowKeep).UnixMilli()}, func(rows *sql.Rows) error {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			if !has[name] {
				gone = append(gone, name)
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, name := range gone {
			if _, err := t.ExecContext(ctx, `DELETE FROM job_health WHERE name = ?`, name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil && ctx.Err() == nil {
		s.log.WarnContext(ctx, "prune job health", "err", err)
	}
}

// jobDecl is a scheduled job this process has.
type jobDecl struct {
	name   string
	period time.Duration
	lane   string
}

// declared is the scheduled jobs this process knows, by name.
func (l *jobLedger) declared() []jobDecl {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]jobDecl, 0, len(l.jobs))
	for _, st := range l.jobs {
		out = append(out, jobDecl{st.name, st.period, st.lane})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].name < out[b].name })
	return out
}

// jobHealth is every scheduled job this server has: what was saved of it,
// whether it runs now, and whether it is stuck or silent as of now.
func (s *Service) jobHealth(ctx context.Context) ([]JobStatus, error) {
	saved := map[string]JobStatus{}
	lastActive := map[string]int64{}
	err := rowsOf(ctx, s.db.R, `SELECT name, period_ms, runs, skips, last_start, last_end, last_ms, max_ms, last_result, last_note, last_clean_at
		FROM job_health`, nil, func(rows *sql.Rows) error {
		var j JobStatus
		var start, end, clean sql.NullInt64
		if err := rows.Scan(&j.Name, &j.PeriodMS, &j.Runs, &j.Skips, &start, &end, &j.LastMS, &j.MaxMS, &j.LastResult, &j.LastNote, &clean); err != nil {
			return err
		}
		j.LastStart, j.LastEnd, j.LastCleanAt = ptrTime(start), ptrTime(end), ptrTime(clean)
		saved[j.Name] = j
		lastActive[j.Name] = max(start.Int64, end.Int64)
		return nil
	})
	if err != nil {
		return nil, err
	}
	declared := s.jobs.declared()
	running := s.jobs.jobsRunning()
	busy := s.jobs.busySinces()
	state, _, _ := s.jobTrouble(s.now(), running, declared, lastActive, busy)
	since := map[string]*time.Time{}
	for _, r := range running {
		since[r.Name] = r.RunningSince
	}
	out := make([]JobStatus, 0, len(declared))
	for _, d := range declared {
		j := saved[d.name]
		j.Name, j.PeriodMS, j.Lane = d.name, d.period.Milliseconds(), d.lane
		j.RunningSince = since[d.name]
		j.Running = j.RunningSince != nil
		if at, ok := busy[d.name]; ok {
			j.BusySince = &at
		}
		j.State = state[d.name]
		out = append(out, j)
	}
	return out, nil
}

// JobProblemQuery picks problems.
type JobProblemQuery struct {
	// All includes the ones that are over.
	All bool
	// Source keeps one source's.
	Source string
	Limit  int
}

// JobProblems lists what goes wrong on the server: the persistent ones
// nobody acknowledged first, then the latest. Admins only.
func (s *Service) JobProblems(ctx context.Context, a *Actor, q JobProblemQuery) ([]JobProblem, error) {
	if err := a.canReadServer(); err != nil {
		return nil, err
	}
	s.saveJobHealth(ctx)
	return s.jobProblems(ctx, q, 0)
}

func (s *Service) jobProblems(ctx context.Context, q JobProblemQuery, id int64) ([]JobProblem, error) {
	where := []string{"1"}
	args := []any{s.nowMS(), problemHold.Milliseconds(), problemMinCount}
	if !q.All {
		where = append(where, "p.cleared_at IS NULL")
	}
	if q.Source != "" {
		where = append(where, "p.source = ?")
		args = append(args, q.Source)
	}
	if id != 0 {
		where = append(where, "p.id = ?")
		args = append(args, id)
	}
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	args = append(args, limit)
	out := []JobProblem{}
	// #nosec G202 -- where is fixed text; the values are bound
	err := rowsOf(ctx, s.db.R, `SELECT p.id, p.source, p.message, p.target, p.level, p.first_at, p.last_at, p.count, p.streak_since, p.streak_count,
			max(?1 - p.streak_since - p.streak_away_ms - p.away_ms, 0), p.detail, `+persistentSQL+` AS persistent, p.cleared_at, p.acked_at, coalesce(u.username, ''), p.ack_reason
		FROM job_problems p LEFT JOIN job_health j ON j.name = p.source LEFT JOIN users u ON u.id = p.acked_by
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY p.cleared_at IS NOT NULL, p.acked_at IS NOT NULL, persistent DESC, p.last_at DESC, p.level = 'error' DESC, p.id LIMIT ?`, args, func(rows *sql.Rows) error {
		var p JobProblem
		var first, last, since int64
		var cleared, acked sql.NullInt64
		if err := rows.Scan(&p.ID, &p.Source, &p.Message, &p.Target, &p.Level, &first, &last, &p.Count, &since, &p.StreakCount,
			&p.StreakUpMS, &p.Detail, &p.Persistent, &cleared, &acked, &p.AckedBy, &p.AckReason); err != nil {
			return err
		}
		p.FirstAt, p.LastAt, p.StreakSince = msTime(first), msTime(last), msTime(since)
		p.ClearedAt, p.AckedAt = ptrTime(cleared), ptrTime(acked)
		out = append(out, p)
		return nil
	})
	return out, err
}

// AckJobProblem records that an admin knows of a problem and why it is
// expected: it stays listed and is not told again, unless it stops for
// ackQuiet and comes back. An empty reason takes the acknowledgement back.
// A person's to say: an agent that could acknowledge would silence the
// alert about its own failures.
func (s *Service) AckJobProblem(ctx context.Context, a *Actor, id int64, reason string) (*JobProblem, error) {
	if err := a.canReadServer(); err != nil {
		return nil, err
	}
	if err := a.Need(PermAdmin); err != nil {
		return nil, err
	}
	if err := a.RequirePersonDirect("acknowledging a server problem"); err != nil {
		return nil, err
	}
	if a.User == nil || a.User.ID == 0 {
		return nil, forbidden("acknowledging a server problem is for a person")
	}
	reason = excerpt(strings.TrimSpace(reason), 300)
	err := s.write(ctx, a, func(t *tx) error {
		var p JobProblem
		err := t.QueryRowContext(ctx, `SELECT source, message, target FROM job_problems WHERE id = ?`, id).Scan(&p.Source, &p.Message, &p.Target)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: no server problem %d (wp server problems lists them)", ErrNotFound, id)
		}
		if err != nil {
			return err
		}
		action := "server.problem.ack"
		if reason == "" {
			action = "server.problem.unack"
			_, err = t.ExecContext(ctx, `UPDATE job_problems SET acked_at = NULL, acked_by = NULL, ack_reason = '' WHERE id = ?`, id)
		} else {
			_, err = t.ExecContext(ctx, `UPDATE job_problems SET acked_at = ?, acked_by = ?, ack_reason = ? WHERE id = ?`, s.nowMS(), a.User.ID, reason, id)
		}
		if err != nil {
			return err
		}
		t.audit(AuditEntry{Action: action, Category: AuditControl, TargetType: "job_problem", TargetID: id, Target: p.label(), Detail: reason})
		return nil
	})
	if err != nil {
		return nil, err
	}
	ps, err := s.jobProblems(ctx, JobProblemQuery{All: true}, id)
	if err != nil {
		return nil, err
	}
	if len(ps) == 0 {
		return nil, ErrNotFound
	}
	return &ps[0], nil
}

// JobSpan is what a source did in an hour or a day.
type JobSpan struct {
	From     time.Time `json:"from"`
	Runs     int64     `json:"runs"`
	Unclean  int64     `json:"unclean"`
	Skips    int64     `json:"skips"`
	Warnings int64     `json:"warnings"`
	Errors   int64     `json:"errors"`
	// Noted is the failures logged below a warning, and of work that was called
	// off (the server stopping, a caller hanging up).
	Noted int64 `json:"noted"`
	AvgMS int64 `json:"avg_ms"`
	MaxMS int64 `json:"max_ms"`
}

// JobHistory is one source over time: a scheduled job, or requests, events
// or background.
type JobHistory struct {
	Source string `json:"source"`
	// Job is its state now; nil for a source that is not a scheduled job.
	Job      *JobStatus   `json:"job,omitempty"`
	By       string       `json:"by"` // hour or day (UTC)
	Since    time.Time    `json:"since"`
	Spans    []JobSpan    `json:"spans"`
	Problems []JobProblem `json:"problems"`
}

// JobHistory is what one source did since a time, by hour or by day, with
// its problems, past ones included. Admins only.
func (s *Service) JobHistory(ctx context.Context, a *Actor, source string, since time.Time, by string) (*JobHistory, error) {
	if err := a.canReadServer(); err != nil {
		return nil, err
	}
	source = strings.TrimSpace(source)
	if source == "" {
		return nil, invalid("say which job: wp server status lists them")
	}
	now := s.now()
	if since.IsZero() {
		since = now.Add(-7 * 24 * time.Hour)
	}
	if by == "" {
		by = "day"
		if now.Sub(since) <= 48*time.Hour {
			by = "hour"
		}
	}
	width := int64(time.Hour / time.Millisecond)
	switch by {
	case "hour":
	case "day":
		width *= 24
	default:
		return nil, invalid("by %q: say hour or day", by)
	}
	s.saveJobHealth(ctx)
	out := &JobHistory{Source: source, By: by, Since: since, Spans: []JobSpan{}}
	jobs, err := s.jobHealth(ctx)
	if err != nil {
		return nil, err
	}
	for i := range jobs {
		if jobs[i].Name == source {
			out.Job = &jobs[i]
		}
	}
	err = rowsOf(ctx, s.db.R, `SELECT hour / ?1 * ?1, sum(runs), sum(unclean), sum(skips), sum(warnings), sum(errors), sum(noted), sum(ms_total), max(ms_max)
		FROM job_hours WHERE source = ?2 AND hour >= ?3 GROUP BY 1 ORDER BY 1`,
		[]any{width, source, since.Truncate(time.Hour).UnixMilli()}, func(rows *sql.Rows) error {
			var sp JobSpan
			var from, total int64
			if err := rows.Scan(&from, &sp.Runs, &sp.Unclean, &sp.Skips, &sp.Warnings, &sp.Errors, &sp.Noted, &total, &sp.MaxMS); err != nil {
				return err
			}
			sp.From = msTime(from)
			if sp.Runs > 0 {
				sp.AvgMS = total / sp.Runs
			}
			out.Spans = append(out.Spans, sp)
			return nil
		})
	if err != nil {
		return nil, err
	}
	if out.Problems, err = s.jobProblems(ctx, JobProblemQuery{All: true, Source: source, Limit: 50}, 0); err != nil {
		return nil, err
	}
	if out.Job == nil && len(out.Spans) == 0 && len(out.Problems) == 0 {
		return nil, fmt.Errorf("%w: nothing is recorded for %q (wp server status lists the jobs; requests, events and background are the other sources)", ErrNotFound, source)
	}
	return out, nil
}

// blankStoredSecrets blanks the secrets learned since the last pass (and the
// pass before, for a save that was in flight) in the text job health keeps:
// the problems' messages, targets and details and the jobs' last notes. The
// ledger blanks its own copies as the secret is learned (jobLedger.blank).
// A problem whose message or target changes by it may now equal another:
// the two are one, their counts added.
func (s *Service) blankStoredSecrets(ctx context.Context) {
	secrets := s.jobs.secretsToBlank()
	if len(secrets) == 0 {
		return
	}
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		for _, sec := range secrets {
			if err := blankProblems(ctx, t, sec); err != nil {
				return err
			}
			if _, err := t.ExecContext(ctx, `UPDATE job_health SET last_note = replace(last_note, ?1, ?2) WHERE instr(last_note, ?1) > 0`, sec, redacted); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil && ctx.Err() == nil {
		s.log.WarnContext(ctx, "log secrets: query failed, these secrets are not blanked in the job problems", "err", err)
	}
}

func blankProblems(ctx context.Context, t *tx, sec string) error {
	type row struct {
		id                      int64
		source, message, target string
		detail                  string
	}
	var rows []row
	err := rowsOf(ctx, t, `SELECT id, source, message, target, detail FROM job_problems
		WHERE instr(message, ?1) > 0 OR instr(target, ?1) > 0 OR instr(detail, ?1) > 0 OR instr(message, ?2) > 0 OR instr(target, ?2) > 0 OR instr(detail, ?2) > 0`,
		[]any{sec, quotedInner(sec)}, func(r *sql.Rows) error {
			var x row
			if err := r.Scan(&x.id, &x.source, &x.message, &x.target, &x.detail); err != nil {
				return err
			}
			rows = append(rows, x)
			return nil
		})
	if err != nil {
		return err
	}
	for _, x := range rows {
		msg, target, detail := blankSecret(x.message, sec), blankSecret(x.target, sec), blankSecret(x.detail, sec)
		var other int64
		err := t.QueryRowContext(ctx, `SELECT id FROM job_problems WHERE source = ? AND message = ? AND target = ? AND id != ?`,
			x.source, msg, target, x.id).Scan(&other)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := t.ExecContext(ctx, `UPDATE job_problems SET message = ?, target = ?, detail = ? WHERE id = ?`, msg, target, detail, x.id); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if _, err := t.ExecContext(ctx, `UPDATE job_problems AS o SET
					count = o.count + m.count,
					streak_count = o.streak_count + m.streak_count,
					first_at = min(o.first_at, m.first_at),
					streak_away_ms = CASE WHEN m.streak_since < o.streak_since THEN m.streak_away_ms ELSE o.streak_away_ms END,
					streak_since = min(o.streak_since, m.streak_since),
					away_ms = max(o.away_ms, m.away_ms),
					alerted_at = CASE WHEN o.alerted_at IS NULL OR m.alerted_at IS NULL THEN coalesce(o.alerted_at, m.alerted_at) ELSE min(o.alerted_at, m.alerted_at) END,
					detail = CASE WHEN m.last_at > o.last_at THEN ?2 ELSE o.detail END,
					level = CASE WHEN m.level = 'error' THEN 'error' ELSE o.level END,
					last_at = max(o.last_at, m.last_at),
					cleared_at = CASE WHEN o.cleared_at IS NULL OR m.cleared_at IS NULL THEN NULL ELSE max(o.cleared_at, m.cleared_at) END,
					acked_at = coalesce(o.acked_at, m.acked_at),
					acked_by = CASE WHEN o.acked_at IS NOT NULL THEN o.acked_by ELSE m.acked_by END,
					ack_reason = CASE WHEN o.acked_at IS NOT NULL THEN o.ack_reason ELSE m.ack_reason END
				FROM (SELECT * FROM job_problems WHERE id = ?1) AS m WHERE o.id = ?3`, x.id, detail, other); err != nil {
				return err
			}
			if _, err := t.ExecContext(ctx, `DELETE FROM job_problems WHERE id = ?`, x.id); err != nil {
				return err
			}
		}
	}
	return nil
}

// quotedInner is secret as an attribute's quoted text holds it.
func quotedInner(secret string) string {
	q := strconv.Quote(secret)
	return q[1 : len(q)-1]
}
