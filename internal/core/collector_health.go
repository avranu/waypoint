package core

// Collector health (WP-999). Some work is collected, not triggered: runners
// trace merged bug fixes to the change that caused them and prove the fixes,
// and an hourly job scores what agents were told against what they changed.
// When one of them stops, nothing errs: the ledger just stops growing, and
// the escape ledger's due query timing out went unseen for days.
//
// A collector is stalled when something has waited for it longer than
// collectorStallAfter and it has produced nothing newer than that (or ever).
// Waiting without a stall is a slow queue; output without waiting is idle.
// Fix traces and proofs are judged per repo: a runner that keeps tracing one
// repo's fixes says nothing about a repo no runner can reach, so a repo is
// stalled when its own oldest fix has waited and nothing in it was collected
// since (a trace that found no cause counts: the collector worked).
// A fix begins to wait when it can be traced: at the later of its pull
// request's merge and the link that ties a bug task to it, so a bug linked
// today to a fix merged last week has not waited a week.
// Only collectors whose waiting predicate is the one the collector itself
// works from are listed (a copy that drifted would count the wrong things);
// each says where that is. Left out: CI evidence (ci_evidence.go) reads
// jobs only evidenceWindow (48 hours) back and keeps what it read for
// evidenceKeep (30 days), so what waits longer than the window is simply
// never read, and it records no time of its own reads.
//
// The hourly check keeps one row per collector (collector_health), logs a
// warning for each stalled one, and tells the admins once a day (and at once
// when another repo begins to stall) while it
// lasts, as the deploy lag alert does (env_lag.go). Recovery clears the row
// and says nothing. stalled_since is the first hourly check that saw the
// stall, not when the first item began to wait (that is oldest_wait_at).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	// collectorStallAfter is how long something waits, with no output at
	// all, before a collector counts as stalled.
	collectorStallAfter = 48 * time.Hour
	// collectorRepeat: a stall that lasts is told again this often.
	collectorRepeat = 24 * time.Hour
)

// EvCollectorStalled: a collector has had work waiting and produced nothing
// for 48 hours (WP-999). + name, waiting, oldest_ms, last_ms (0: never),
// since_ms (the first hourly check that saw it), again, per_repo, new_repos
// (count; told early because repos began to stall after the last telling,
// with new_by_repo naming them), and for a
// per-repo collector the stalled repos: stalled_repos (count), by_repo (text)
// and repos (repo, waiting, oldest_ms).
const EvCollectorStalled = "collector.stalled"

// NotifyCollector is the notification kind of a stalled collector.
const NotifyCollector = "collector"

// collector is one thing that collects: the query for what waits for it and
// the query for what it has produced.
type collector struct {
	name string
	// what it is and who does it, for the notice
	about string
	// waiting selects count(*) and the earliest ms any of it began to wait
	// (NULL when none does); args are its bound values at a time.
	waiting string
	args    func(now time.Time) []any
	// output selects the ms of the newest thing it produced, NULL for none.
	output string
	// repoWaiting and repoOutput, when set, make the stall per repo.
	// repoWaiting selects, for each repo with something waiting, (lower(repo),
	// repo, count, earliest ms it began to wait); it uses the waiting query's
	// predicate. repoOutput takes one argument, the stall cutoff in ms, and
	// selects (lower(repo), newest output ms) for repos with output since.
	repoWaiting string
	repoOutput  string
	// scans says, by plan alias, why a query reads a whole table: each hourly
	// check pays for it, so it is stated (TestCollectorQueriesUseIndexes fails
	// on any scan of a table not listed here).
	scans map[string]string
}

func noArgs(time.Time) []any { return nil }

// prepWaitingSQL counts what prepDoneTasks (ScorePreps's own query) would
// pick, with the earliest merge among them, without its random batch.
var prepWaitingSQL = "SELECT COUNT(*), MIN(m) FROM (" +
	strings.TrimSuffix(strings.Replace(prepDoneTasks, "MIN(p.merged_at) FROM", "MIN(p.merged_at) AS m FROM", 1), " ORDER BY random() LIMIT ?") + ")"

// collectorReposNamed is how many stalled repos a notice names; the rest
// are counted.
const collectorReposNamed = 5

// escapeTracesWaitingFrom is the rest of the query behind the escape-trace
// collector: the fixes escapesPendingSQL counts, in every project.
const escapeTracesWaitingFrom = `FROM issues i
			CROSS JOIN external_links el ON el.issue_id = i.id
			CROSS JOIN pull_requests p ON ` + pullLinkFromLinkSQL + `
			WHERE ` + issueIsBugSQL + ` AND (p.state = 'merged' OR p.landed_at IS NOT NULL) AND ` + fixUntracedSQL

// proofsWaitingWhere is proofsDueIn's predicate, in every project, over ft.
const proofsWaitingWhere = `ft.fix_sha != '' AND NOT EXISTS (SELECT 1 FROM fix_proofs fp WHERE fp.pr_id = ft.pr_id)
				AND EXISTS (SELECT 1 FROM escapes e WHERE e.pr_id = ft.pr_id)`

var collectors = []collector{
	{
		name:  "escape traces",
		about: "a runner with a checkout traces each merged bug fix to the change that caused it (wp code trace; EscapesDue)",
		// The fixes escapesPendingSQL counts, in every project.
		waiting: `SELECT COUNT(DISTINCT p.id), MIN(max(COALESCE(p.merged_at, p.landed_at, p.updated_at), el.created_at)) ` + escapeTracesWaitingFrom,
		// A repo no runner holds a checkout of stays here for good, whatever
		// runners trace elsewhere.
		repoWaiting: `SELECT lower(p.repo), MIN(p.repo), COUNT(DISTINCT p.id), MIN(max(COALESCE(p.merged_at, p.landed_at, p.updated_at), el.created_at)) ` +
			escapeTracesWaitingFrom + ` GROUP BY lower(p.repo)`,
		repoOutput: `SELECT lower(p.repo), MAX(ft.traced_at) FROM fix_traces ft CROSS JOIN pull_requests p ON p.id = ft.pr_id
			WHERE ft.traced_at >= ? GROUP BY lower(p.repo)`,
		args:   noArgs,
		output: `SELECT MAX(traced_at) FROM fix_traces`,
		scans: map[string]string{
			"i":  "the bug issues are the driving table: each is looked up by its links, once an hour",
			"ft": "fix_traces has no index on traced_at: one row per traced fix, read once an hour",
		},
	},
	{
		name:    "fix proofs",
		about:   "a runner runs the tests a traced fix added with and without the fix (EscapeProofsDue)",
		waiting: `SELECT COUNT(*), MIN(ft.traced_at) FROM fix_traces ft WHERE ` + proofsWaitingWhere,
		repoWaiting: `SELECT lower(p.repo), MIN(p.repo), COUNT(*), MIN(ft.traced_at) FROM fix_traces ft
			CROSS JOIN pull_requests p ON p.id = ft.pr_id WHERE ` + proofsWaitingWhere + ` GROUP BY lower(p.repo)`,
		repoOutput: `SELECT lower(p.repo), MAX(fp.proved_at) FROM fix_proofs fp CROSS JOIN pull_requests p ON p.id = fp.pr_id
			WHERE fp.proved_at >= ? GROUP BY lower(p.repo)`,
		args:   noArgs,
		output: `SELECT MAX(proved_at) FROM fix_proofs`,
		scans: map[string]string{
			"ft": "fix_traces has no index on what has no proof: one row per traced fix, read once an hour",
			"fp": "fix_proofs has no index on proved_at: one row per proven fix, read once an hour",
		},
	},
	{
		name:    "prep scores",
		about:   "the hourly job scores each done task's prep and trials against the files its work changed (ScorePreps)",
		waiting: prepWaitingSQL,
		args:    func(now time.Time) []any { return []any{now.Add(-prepScoreWindow).UnixMilli()} },
		output:  `SELECT MAX(scored_at) FROM prep_scores`,
		scans: map[string]string{
			"p":  "prepDoneTasks joins external_links to pull_requests with OR, as ScorePreps does every hour",
			"op": "the same OR-join, for the task's other pull requests",
		},
	},
}

// CollectorHealth is how one collector stood at the last hourly check.
type CollectorHealth struct {
	Name         string     `json:"name"`
	Waiting      int        `json:"waiting"`
	OldestWaitAt *time.Time `json:"oldest_wait_at,omitempty"`
	LastOutputAt *time.Time `json:"last_output_at,omitempty"`
	Stalled      bool       `json:"stalled"`
	StalledSince *time.Time `json:"stalled_since,omitempty"`
	CheckedAt    time.Time  `json:"checked_at"`
}

// collectorReading is what one collector's queries said.
type collectorReading struct {
	waiting      int
	oldest, last int64 // ms; 0: none
	// perRepo: the stall is judged by repo; repos are the stalled ones,
	// the longest waiting first.
	perRepo bool
	repos   []repoWait
}

// repoWait is one repo's waiting work.
type repoWait struct {
	key, label string // lower-cased and as the pull requests name it
	n          int
	oldest     int64 // ms
}

func (r collectorReading) stalled(now time.Time) bool {
	if r.perRepo {
		return len(r.repos) > 0
	}
	cutoff := now.Add(-collectorStallAfter).UnixMilli()
	return r.waiting > 0 && r.oldest > 0 && r.oldest < cutoff && r.last < cutoff
}

// stalledWaiting is how much waits where the collector is stalled, and the
// earliest it began.
func (r collectorReading) stalledWaiting() (n int, oldest int64) {
	if !r.perRepo {
		return r.waiting, r.oldest
	}
	for _, w := range r.repos {
		n += w.n
		if oldest == 0 || w.oldest < oldest {
			oldest = w.oldest
		}
	}
	return n, oldest
}

// repoKeys is the stalled repos' keys, sorted and newline-joined, as
// collector_health.alerted_repos keeps them.
func (r collectorReading) repoKeys() string {
	keys := make([]string, 0, len(r.repos))
	for _, w := range r.repos {
		keys = append(keys, w.key)
	}
	sort.Strings(keys)
	return strings.Join(keys, "\n")
}

// newRepos are the stalled repos the last telling (told, as repoKeys wrote
// it) did not name. A telling from before the repos were kept names none, so
// every repo then counts as new once.
func (r collectorReading) newRepos(told string) []repoWait {
	named := map[string]bool{}
	for _, k := range strings.Split(told, "\n") {
		named[k] = true
	}
	var out []repoWait
	for _, w := range r.repos {
		if !named[w.key] {
			out = append(out, w)
		}
	}
	return out
}

// repoLabels names repos as the pull requests do, at most collectorReposNamed.
func repoLabels(repos []repoWait) string {
	var parts []string
	for i, w := range repos {
		if i == collectorReposNamed {
			parts = append(parts, fmt.Sprintf("and %d more", len(repos)-i))
			break
		}
		parts = append(parts, w.label)
	}
	return strings.Join(parts, ", ")
}

// reposText names the stalled repos, oldest first: "o/r 12 (72h00m), x/y 3
// (50h00m)", at most collectorReposNamed of them.
func (r collectorReading) reposText(now time.Time) string {
	var parts []string
	for i, w := range r.repos {
		if i == collectorReposNamed {
			parts = append(parts, fmt.Sprintf("and %d more", len(r.repos)-i))
			break
		}
		parts = append(parts, fmt.Sprintf("%s %d (%s)", w.label, w.n, ShortDuration(now.Sub(msTime(w.oldest)))))
	}
	return strings.Join(parts, ", ")
}

// stalledRepos reads c's per-repo waiting and output: the repos with
// something waiting past the cutoff and no output since, longest waiting first.
func (s *Service) stalledRepos(ctx context.Context, c collector, now time.Time) ([]repoWait, error) {
	cutoff := now.Add(-collectorStallAfter).UnixMilli()
	fresh := map[string]bool{}
	out, err := s.db.R.QueryContext(ctx, c.repoOutput, cutoff)
	if err != nil {
		return nil, fmt.Errorf("repo output: %w", err)
	}
	defer func() { _ = out.Close() }()
	for out.Next() {
		var key string
		var last int64
		if err := out.Scan(&key, &last); err != nil {
			return nil, fmt.Errorf("repo output: %w", err)
		}
		fresh[key] = last >= cutoff
	}
	if err := out.Err(); err != nil {
		return nil, err
	}
	rows, err := s.db.R.QueryContext(ctx, c.repoWaiting, c.args(now)...)
	if err != nil {
		return nil, fmt.Errorf("repo waiting: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var stalled []repoWait
	for rows.Next() {
		var w repoWait
		var oldest sql.NullInt64
		if err := rows.Scan(&w.key, &w.label, &w.n, &oldest); err != nil {
			return nil, fmt.Errorf("repo waiting: %w", err)
		}
		w.oldest = oldest.Int64
		if w.oldest > 0 && w.oldest < cutoff && !fresh[w.key] {
			stalled = append(stalled, w)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(stalled, func(i, j int) bool {
		if stalled[i].oldest != stalled[j].oldest {
			return stalled[i].oldest < stalled[j].oldest
		}
		return stalled[i].key < stalled[j].key
	})
	return stalled, nil
}

func (s *Service) readCollector(ctx context.Context, c collector, now time.Time) (collectorReading, error) {
	var r collectorReading
	var oldest, last sql.NullInt64
	if err := s.db.R.QueryRowContext(ctx, c.waiting, c.args(now)...).Scan(&r.waiting, &oldest); err != nil {
		return r, fmt.Errorf("waiting: %w", err)
	}
	if err := s.db.R.QueryRowContext(ctx, c.output).Scan(&last); err != nil {
		return r, fmt.Errorf("output: %w", err)
	}
	r.oldest, r.last = oldest.Int64, last.Int64
	if c.repoWaiting != "" {
		r.perRepo = true
		repos, err := s.stalledRepos(ctx, c, now)
		if err != nil {
			return r, err
		}
		r.repos = repos
	}
	return r, nil
}

// CheckCollectorHealth reads every collector, keeps its row, and tells the
// admins about the stalled ones. Hourly.
func (s *Service) CheckCollectorHealth(ctx context.Context) {
	now := s.now()
	for _, c := range collectors {
		if ctx.Err() != nil {
			return
		}
		r, err := s.readCollector(ctx, c, now)
		if err != nil {
			s.log.WarnContext(ctx, "collector health: read", "collector", c.name, "err", err)
			continue
		}
		if err := s.recordCollector(ctx, c, r, now); err != nil {
			s.log.WarnContext(ctx, "collector health: record", "collector", c.name, "err", err)
		}
	}
}

func nullMS(ms int64) any {
	if ms == 0 {
		return nil
	}
	return ms
}

// recordCollector writes c's row and, for a stalled collector not told about
// today, records the telling and emits EvCollectorStalled (notifyCollector
// sends it). The conditional UPDATE lets only one server's check tell.
func (s *Service) recordCollector(ctx context.Context, c collector, r collectorReading, now time.Time) error {
	stalled := r.stalled(now)
	return s.write(ctx, SystemActor(), func(t *tx) error {
		var since, told sql.NullInt64
		var toldRepos string
		err := t.QueryRowContext(ctx, `SELECT stalled_since, alerted_at, alerted_repos FROM collector_health WHERE name = ?`, c.name).Scan(&since, &told, &toldRepos)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		stalledSince := any(nil)
		if stalled {
			stalledSince = now.UnixMilli()
			if since.Valid {
				stalledSince = since.Int64
			}
		}
		keepTold, keepRepos := any(nil), ""
		if stalled && told.Valid {
			keepTold, keepRepos = told.Int64, toldRepos
		}
		if _, err := t.ExecContext(ctx, `INSERT INTO collector_health(name, stalled_since, alerted_at, alerted_repos, waiting, oldest_wait_at, last_output_at, checked_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET stalled_since = excluded.stalled_since, alerted_at = excluded.alerted_at, alerted_repos = excluded.alerted_repos,
				waiting = excluded.waiting, oldest_wait_at = excluded.oldest_wait_at, last_output_at = excluded.last_output_at, checked_at = excluded.checked_at`,
			c.name, stalledSince, keepTold, keepRepos, r.waiting, nullMS(r.oldest), nullMS(r.last), now.UnixMilli()); err != nil {
			return err
		}
		if !stalled {
			return nil
		}
		stalledN, stalledOldest := r.stalledWaiting()
		s.log.WarnContext(ctx, "collector stalled", "collector", c.name, "waiting", stalledN,
			"oldest_wait", ShortDuration(now.Sub(msTime(stalledOldest))), "last_output", lastOutputText(r.last, now), "repos", r.reposText(now))
		// A repo that began to stall after the last telling is told at once,
		// not at the next repeat.
		fresh := r.newRepos(toldRepos)
		today := told.Valid && now.Sub(msTime(told.Int64)) < collectorRepeat
		if today && len(fresh) == 0 {
			return nil // told about this stall today
		}
		early := today // told again before the repeat, for the repos in fresh
		res, err := t.ExecContext(ctx, `UPDATE collector_health SET alerted_at = ?, alerted_repos = ? WHERE name = ? AND coalesce(alerted_at, 0) = ?`,
			now.UnixMilli(), r.repoKeys(), c.name, told.Int64)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		data := map[string]any{
			"name": c.name, "about": c.about, "waiting": stalledN, "oldest_ms": stalledOldest, "last_ms": r.last,
			"since_ms": stalledSinceMS(since, now), "again": told.Valid && !early, "per_repo": r.perRepo,
		}
		if early {
			data["new_repos"], data["new_by_repo"] = len(fresh), repoLabels(fresh)
		}
		if r.perRepo {
			repos := make([]map[string]any, 0, len(r.repos))
			for _, w := range r.repos {
				repos = append(repos, map[string]any{"repo": w.label, "waiting": w.n, "oldest_ms": w.oldest})
			}
			data["stalled_repos"], data["by_repo"], data["repos"] = len(r.repos), r.reposText(now), repos
		}
		t.emit(&Event{Kind: EvCollectorStalled, Data: data})
		return nil
	})
}

func stalledSinceMS(since sql.NullInt64, now time.Time) int64 {
	if since.Valid {
		return since.Int64
	}
	return now.UnixMilli()
}

func lastOutputText(last int64, now time.Time) string {
	if last == 0 {
		return "never"
	}
	return ShortDuration(now.Sub(msTime(last))) + " ago"
}

// notifyCollector tells the admins a collector is stalled.
func (s *Service) notifyCollector(ctx context.Context, e *Event) {
	now := s.now()
	name := dataStr(e, "name")
	title := fmt.Sprintf("%s has stalled: %d waiting, nothing collected for %s", name, dataInt(e, "waiting"),
		ShortDuration(collectorStallAfter))
	lastWhere := "the last output was "
	if e.Data["per_repo"] == true {
		title = fmt.Sprintf("%s has stalled in %s: %d waiting, nothing collected there for %s", name,
			plural1(int(dataInt(e, "stalled_repos")), "repo"), dataInt(e, "waiting"), ShortDuration(collectorStallAfter))
		lastWhere = "the last output, in any repo, was "
	}
	body := fmt.Sprintf("%s. The oldest has waited %s; %s%s. Likely no runner holds a checkout to do it, or the work fails before it records anything: wp server status lists the collectors, wp server log --level warn the failures.",
		dataStr(e, "about"), ShortDuration(now.Sub(msTime(dataInt(e, "oldest_ms")))), lastWhere, lastOutputText(dataInt(e, "last_ms"), now))
	if by := dataStr(e, "by_repo"); by != "" {
		body += " Stalled repos (waiting, oldest): " + by + ". A repo no runner holds a checkout of waits for good: attach a runner with one, or detach the repo."
	}
	if e.Data["again"] == true {
		body = "Still stalled. " + body
	}
	if by := dataStr(e, "new_by_repo"); by != "" {
		body = "Newly stalled: " + by + ". " + body
	}
	for _, id := range s.userIDsWhere(ctx, `SELECT id FROM users WHERE is_admin = 1 AND disabled = 0 ORDER BY id`) {
		s.notifyURL(ctx, id, nil, NotifyCollector, title, body, "")
	}
}

// CollectorHealthList is the collectors as the last check found them, in
// the order they are listed; one not checked yet is left out.
func (s *Service) CollectorHealthList(ctx context.Context) ([]CollectorHealth, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT name, stalled_since, waiting, oldest_wait_at, last_output_at, checked_at FROM collector_health`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	by := map[string]CollectorHealth{}
	for rows.Next() {
		var h CollectorHealth
		var since, oldest, last sql.NullInt64
		var checked int64
		if err := rows.Scan(&h.Name, &since, &h.Waiting, &oldest, &last, &checked); err != nil {
			return nil, err
		}
		h.Stalled = since.Valid
		if since.Valid {
			h.StalledSince = msPtr(since.Int64)
		}
		if oldest.Valid {
			h.OldestWaitAt = msPtr(oldest.Int64)
		}
		if last.Valid {
			h.LastOutputAt = msPtr(last.Int64)
		}
		h.CheckedAt = msTime(checked)
		by[h.Name] = h
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]CollectorHealth, 0, len(collectors))
	for _, c := range collectors {
		if h, ok := by[c.name]; ok {
			out = append(out, h)
		}
	}
	return out, nil
}

func msPtr(ms int64) *time.Time { t := msTime(ms); return &t }
