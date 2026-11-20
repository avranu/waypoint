package core

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"
)

// PreviousRun is how the server run before this one ended (server_runs).
type PreviousRun struct {
	// LastAlive is when it stopped, or when it was last seen working.
	LastAlive time.Time `json:"last_alive"`
	// Clean is whether it shut down: false means it was killed or died.
	Clean bool `json:"clean"`
	// AwayS is how long the server was not running between the two.
	AwayS int64 `json:"away_s"`
	// Interrupted is the jobs that were running when it died.
	Interrupted []string `json:"interrupted,omitempty"`
}

// beginServerRun records that this process started and what the one before
// it left. The time the server was away is added to every problem, so a
// long stop does not read as the problem having gone quiet (an open one is
// not cleared by it, and an acknowledged one that was over keeps its
// acknowledgement). If the last
// process did not stop, the jobs it was running are marked interrupted and
// the death is logged: that is the only trace a killed server leaves.
//
// It is tried again from the liveness tick while it has not been written
// (markServerAlive): the downtime is measured to when this process started,
// not to when the write got through.
func (s *Service) beginServerRun(ctx context.Context) {
	now := s.now()
	started := s.started
	if started.IsZero() {
		started = now
	}
	var prev *PreviousRun
	var runID int64
	// On the first try every running or busy mark is an earlier process's,
	// whatever its clock said; on a retry only those from before this start.
	s.jobs.mu.Lock()
	retry := s.jobs.startTried
	s.jobs.startTried = true
	s.jobs.mu.Unlock()
	startMS := int64(math.MaxInt64)
	if retry {
		startMS = started.UnixMilli()
	}
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		prev = nil
		var checked, alive int64
		var stopped sql.NullInt64
		err := t.QueryRowContext(ctx, `SELECT checked_at, alive_at, stopped_at FROM server_runs ORDER BY id DESC LIMIT 1`).Scan(&checked, &alive, &stopped)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// It was last seen working when the check last finished or the
		// liveness tick last ran, whichever is later: a check stalled for
		// hours is not the server being gone.
		end := max(checked, alive)
		if err == nil {
			if stopped.Valid {
				end = stopped.Int64
			}
			away := max(started.UnixMilli()-end, 0)
			prev = &PreviousRun{LastAlive: time.UnixMilli(end), Clean: stopped.Valid, AwayS: away / 1000}
			if _, err := t.ExecContext(ctx, `UPDATE job_problems SET away_ms = away_ms + ?`, away); err != nil {
				return err
			}
		}
		res, err := t.ExecContext(ctx, `INSERT INTO server_runs(started_at, checked_at, alive_at, version) VALUES (?,?,?,?)`,
			started.UnixMilli(), started.UnixMilli(), now.UnixMilli(), s.cfg.Version)
		if err != nil {
			return err
		}
		if runID, err = res.LastInsertId(); err != nil {
			return err
		}
		if prev != nil && !prev.Clean {
			if prev.Interrupted, err = interruptJobs(ctx, t, end, startMS); err != nil {
				return err
			}
		}
		// Only what an earlier process left: on a retry, this one's saves
		// have written their own.
		_, err = t.ExecContext(ctx, `UPDATE job_health SET running_since = CASE WHEN running_since < ? THEN NULL ELSE running_since END,
			busy_since = CASE WHEN busy_since < ? THEN NULL ELSE busy_since END
			WHERE running_since < ? OR busy_since < ?`, startMS, startMS, startMS, startMS)
		return err
	})
	if err != nil {
		if ctx.Err() == nil {
			s.log.ErrorContext(ctx, "record server start", "err", err)
		}
		return
	}
	s.jobs.mu.Lock()
	s.jobs.runID, s.jobs.prev = runID, prev
	if s.jobs.checkedAt.IsZero() {
		s.jobs.checkedAt = now
	}
	s.jobs.mu.Unlock()
	if prev == nil || prev.Clean {
		return
	}
	s.log.ErrorContext(ctx, "the server stopped without shutting down", "last_alive", prev.LastAlive.UTC().Format(time.RFC3339), "away_s", prev.AwayS)
	for _, name := range prev.Interrupted {
		s.log.ErrorContext(WithLogSource(ctx, name), "the server died while this job ran")
	}
}

// interruptJobs ends the runs the dead server left going, at the last time it
// was seen alive (or, if a save wrote a later start than that, at the start:
// a run never ends before it began), and returns the jobs. Only runs that
// began before this process started are the dead one's.
func interruptJobs(ctx context.Context, t *tx, end, before int64) ([]string, error) {
	rows, err := t.QueryContext(ctx, `SELECT name FROM job_health WHERE running_since < ? ORDER BY name`, before)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			_ = rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	hour := time.UnixMilli(end).Truncate(time.Hour).UnixMilli()
	for _, n := range names {
		if _, err := t.ExecContext(ctx, `UPDATE job_health SET last_result = ?, last_note = ?, last_start = running_since, last_end = max(?, running_since),
			last_ms = max(? - running_since, 0), runs = runs + 1 WHERE name = ?`,
			jobInterrupted, "the server died while it ran", end, end, n); err != nil {
			return nil, err
		}
		if _, err := t.ExecContext(ctx, `INSERT INTO job_hours(source, hour, runs, unclean) VALUES (?,?,1,1)
			ON CONFLICT(source, hour) DO UPDATE SET runs = runs + 1, unclean = unclean + 1`, n, hour); err != nil {
			return nil, err
		}
	}
	return names, nil
}

// stopServerRun marks this run as ended on purpose.
func (s *Service) stopServerRun(ctx context.Context) {
	s.jobs.mu.Lock()
	id := s.jobs.runID
	s.jobs.mu.Unlock()
	if id == 0 {
		return
	}
	_, err := s.db.W.ExecContext(ctx, `UPDATE server_runs SET stopped_at = ? WHERE id = ?`, s.nowMS(), id)
	if err != nil {
		s.log.WarnContext(ctx, "record server stop", "err", err)
	}
}

// markServerAlive is the liveness tick, run by the watchdog's own ticker
// whether or not the health check finishes: it records that this process is
// alive, and writes the server's start if that has not been. Without a
// start, the next one would count this whole uptime as downtime.
func (s *Service) markServerAlive(ctx context.Context) {
	s.jobs.mu.Lock()
	id := s.jobs.runID
	s.jobs.mu.Unlock()
	if id == 0 {
		s.beginServerRun(ctx)
		return
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE server_runs SET alive_at = max(alive_at, ?) WHERE id = ?`, s.nowMS(), id); err != nil && ctx.Err() == nil {
		s.log.WarnContext(ctx, "record server alive", "err", err)
	}
}

// markHealthChecked records that the health check finished.
func (s *Service) markHealthChecked(ctx context.Context, now time.Time) {
	s.jobs.mu.Lock()
	s.jobs.checkedAt = now
	id := s.jobs.runID
	s.jobs.mu.Unlock()
	if id == 0 {
		return
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE server_runs SET checked_at = ?, alive_at = max(alive_at, ?) WHERE id = ?`, now.UnixMilli(), now.UnixMilli(), id); err != nil && ctx.Err() == nil {
		s.log.WarnContext(ctx, "record health check", "err", err)
	}
}

// healthCheckStale is how long the check may go without finishing before the
// watchdog says it has stopped (it runs each minute).
const (
	healthCheckStale   = 5 * time.Minute
	healthWatchdogName = "job health watchdog"
	healthWatchdogTold = 24 * time.Hour
)

// watchJobHealthCheck is the job that watches the health check: it runs on a
// timer and goroutine of its own (runHealthWatchdog), as the check does, so
// neither is held by the scheduler's loop nor hides the other being stuck. A check that has not finished for healthCheckStale is
// logged each time it is seen so and told to the admins once a day.
func (s *Service) watchJobHealthCheck(ctx context.Context) {
	now := s.now()
	s.jobs.mu.Lock()
	last := s.jobs.checkedAt
	s.jobs.mu.Unlock()
	if last.IsZero() {
		last = s.started
	}
	if now.Sub(last) < healthCheckStale {
		return
	}
	s.log.ErrorContext(ctx, "the job health check has not finished", "stale_s", int64(now.Sub(last).Seconds()))
	s.jobs.mu.Lock()
	due := now.Sub(s.jobs.watchdogTold) >= healthWatchdogTold
	s.jobs.mu.Unlock()
	if !due {
		return
	}
	told := 0
	err := s.write(ctx, SystemActor(), func(t *tx) (err error) {
		told, err = t.storeAdminNotice(ctx, NotifyJobs, "The job health check has stopped",
			"The check that tells you what is failing has not finished for "+now.Sub(last).Round(time.Second).String()+". Nothing is reported until it runs again: see wp server log.", "")
		return err
	})
	if err != nil {
		if ctx.Err() == nil {
			s.log.ErrorContext(ctx, "tell the health check has stopped", "err", err)
		}
		return
	}
	if told == 0 {
		// Nobody was told, so it is due again at the next look.
		return
	}
	s.jobs.mu.Lock()
	s.jobs.watchdogTold = now
	s.jobs.mu.Unlock()
}
