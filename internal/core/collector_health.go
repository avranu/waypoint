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
// The transcript archive is judged per machine (user and machine), the same
// way: a machine is stalled when its own bytes have waited, or it stopped
// reporting its archive while its usage sync still arrives (silent), and
// nothing of its own was stored since. A machine that switched the archive
// off is not waiting, nor is one not heard from for archiveWaitingWindow
// (Claude Code keeps local transcripts about that long, so nothing more can
// arrive from a retired machine). A collector may also count what it lost (dropped); the
// hourly check tells admins when that total grew, at most daily.
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

	"git.jmann.me/claude/waypoint/internal/transcript"
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

// EvCollectorDropped: the total a collector counted as lost grew (WP-1466).
// + name, about, dropped (the total), grew_by (since the last telling), again.
const EvCollectorDropped = "collector.dropped"

// NotifyCollector is the notification kind of a stalled collector.
const NotifyCollector = "collector"

// collector is one thing that collects: the query for what waits for it and
// the query for what it has produced.
type collector struct {
	name string
	// what it is and who does it, for the notice
	about string
	// unit names what waiting counts when it is not the items the work is
	// about (machines, not files); "" says nothing.
	unit string
	// waiting selects count(*) and the earliest ms any of it began to wait
	// (NULL when none does); args are its bound values at a time.
	waiting string
	args    func(now time.Time) []any
	// output selects the ms of the newest thing it produced, NULL for none.
	output string
	// group names what the stall is judged by when it is not the repo
	// ("machine"), groupHelp what to do about one, groupWhere where the last
	// output may have been ("in any machine").
	group, groupHelp string
	// repoWaiting and repoOutput, when set, make the stall per repo (or group).
	// repoWaiting selects, for each repo with something waiting, (lower(repo),
	// repo, count, earliest ms it began to wait); it uses the waiting query's
	// predicate. repoOutput takes one argument, the stall cutoff in ms, and
	// selects (lower(repo), newest output ms) for repos with output since.
	repoWaiting string
	repoOutput  string
	// dropped, when set, selects one number: the total this collector has
	// counted as lost, so that data it handled did not arrive whole. The hourly
	// check tells admins when it grew. off selects how many of its machines
	// opted out (they are not waiting).
	dropped, off string
	// problem, when set, says in plain words what is wrong beyond a stall (a
	// failing file, a record the collector cannot read); "" for nothing.
	problem func(s *Service, ctx context.Context) (string, error)
	// also names the other queries the collector runs, with their bound values,
	// so TestCollectorQueriesUseIndexes plans them too.
	also map[string]collectorQuery
	// scans says, by plan alias, why a query reads a whole table: each hourly
	// check pays for it, so it is stated (TestCollectorQueriesUseIndexes fails
	// on any scan of a table not listed here).
	scans map[string]string
}

// collectorQuery is a query with its bound values.
type collectorQuery struct {
	sql  string
	args []any
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
	{
		name:    "transcript archive",
		problem: (*Service).transcriptProblem,
		unit:    "machines",
		about:   "each machine's wp uploads its sessions' transcripts to the archive (wp usage sync); what it dropped and what it could not send yet is in wp usage machines",
		group:   "machine", groupHelp: "Check that wp usage sync runs there and that the archive is not full (wp server status); a machine that should not archive sets WP_USAGE_ARCHIVE=off (or its passes use --no-archive), which it tells the server. A machine not heard from for 30 days is not counted.",
		waiting: `SELECT COUNT(*), MIN(` + archiveSince + `) FROM usage_machines m WHERE ` + archiveWaitingWhere,
		args: func(now time.Time) []any {
			return []any{collectorStallAfter.Milliseconds(), now.Add(-archiveWaitingWindow).UnixMilli()}
		},
		repoWaiting: `SELECT m.user_id || '/' || m.machine, u.username || '/' || m.machine, COUNT(*), MIN(` + archiveSince + `) FROM usage_machines m JOIN users u ON u.id = m.user_id WHERE ` + archiveWaitingWhere + ` GROUP BY m.user_id, m.machine`,
		repoOutput:  `SELECT m.user_id || '/' || m.machine, m.transcript_piece_at FROM usage_machines m WHERE m.transcript_piece_at >= ?`,
		output:      `SELECT MAX(m.transcript_piece_at) FROM usage_machines m`,
		dropped:     `SELECT COALESCE(SUM(j.value), 0) FROM usage_machines m, json_each(m.transcript_dropped) j WHERE j.key IN ` + transcriptDroppedReasons,
		off:         `SELECT COUNT(*) FROM usage_machines m WHERE m.transcript_off = 1`,
		scans:       map[string]string{"m": "usage_machines has one row per machine of each user: a small table, read whole once an hour"},
	},
	{
		name:  "slot holds",
		unit:  "machines",
		about: "each machine's wp sends the test slot holds it recorded, so the slot policy can be checked; a machine whose requests do not reach the server at all is not seen here (WP-1646)",
		group: "machine", groupHelp: "Check that wp slot send works there (it prints how many holds wait and how many it sent). A machine not heard from for 30 days is not counted.",
		waiting:     `SELECT COUNT(*), MIN(m.pending_since) FROM slot_machines m WHERE m.pending > 0 AND m.last_report_at >= ?`,
		args:        func(now time.Time) []any { return []any{now.Add(-archiveWaitingWindow).UnixMilli()} },
		repoWaiting: `SELECT m.user_id || '/' || m.machine, u.username || '/' || m.machine, COUNT(*), MIN(m.pending_since) FROM slot_machines m JOIN users u ON u.id = m.user_id WHERE m.pending > 0 AND m.last_report_at >= ? GROUP BY m.user_id, m.machine`,
		repoOutput:  `SELECT m.user_id || '/' || m.machine, m.last_stored_at FROM slot_machines m WHERE m.last_stored_at >= ?`,
		output:      `SELECT MAX(m.last_stored_at) FROM slot_machines m`,
		dropped:     `SELECT COALESCE(SUM(m.dropped + m.rejected), 0) FROM slot_machines m`,
		scans:       map[string]string{"m": "slot_machines has one row per machine of each user: a small table, read whole once an hour"},
	},
	{
		name:    "session facts",
		unit:    "files",
		about:   "the server derives per-event facts (model calls, tool calls, turns, compactions...) from each archived transcript file once it has been quiet for 10 minutes (RunSessionFacts); a file that failed is in session_fact_files.error",
		waiting: `SELECT COUNT(*), MIN(newest) FROM (` + factsWaitingSQL + `)`,
		args: func(now time.Time) []any {
			return []any{now.Add(-factsQuiet).UnixMilli(), transcript.ParserVersion, now.Add(-factsRetry).UnixMilli()}
		},
		output: `SELECT MAX(f.derived_at) FROM session_fact_files f`,
		// A derive failure is not a drop: the collector lost nothing, the file
		// waits for the next attempt. Its failures are in the problem line.
		problem: (*Service).factsProblem,
		also: map[string]collectorQuery{
			"failed":    {factsFailedSQL, nil},
			"unknown":   {factsUnknownSQL, []any{transcript.ParserVersion, "", ""}},
			"undecoded": {factsIgnoredSQL, []any{transcript.ParserVersion, "undecoded/", "undecoded/"}},
		},
		scans: map[string]string{
			"p": "transcript_pieces is read whole, grouped by file, every 5 minutes: one row per stored piece, no index leads with created_at",
			"f": "session_fact_files has one row per archived file: read whole once an hour for the newest derive and the problem line (json_each over each row's counts)",
		},
	},
	{
		// Nothing waits for it and it produces nothing of its own: it is the
		// total of what session data the CLIs' hooks, usage uploads and the
		// server's caps dropped (hook_drops: which, where and how many is in
		// wp server status), so that a drop is never silent (WP-1470).
		name:    "session data",
		about:   "session hooks, usage uploads and the caps on what a session keeps; what each dropped, by reason, is in wp server status (Hook data dropped)",
		waiting: `SELECT 0, NULL`,
		args:    noArgs,
		output:  `SELECT MAX(h.last_at) FROM hook_drops h`,
		dropped: `SELECT COALESCE(SUM(h.dropped), 0) FROM hook_drops h`,
	},
	{
		name:  "prep evidence",
		about: "after a task prep the server asks the prep model which recorded data bears on the task (WP-ADR157), and again when a runner asks for prep work (PrepDue); a check that failed says why in task_prep_evidence.error. A check refused for want of spare capacity is no failure and never waits, nor does a task of a project the live run leaves out. Nothing counts as waiting unless, of the checks tried in the last 48 hours, one failed or has been pending too long to be running still",
		// Open tasks prepared of late whose evidence isn't there, in the
		// projects the live run takes in (ExperimentRun.covers): none is made
		// for others, or without a live run. A prep with no
		// check at all doesn't count: nothing retries one. Nor will much retry
		// an old failure (its task taken, far back in the queue or not
		// prepared by itself), so none counts unless some check, of any task,
		// was tried of late and failed or was lost (pending past
		// prepPendingMax): a row's created_at is its last attempt. A refused
		// one found no spare capacity: it neither waits nor opens that gate.
		waiting: `SELECT COUNT(*), MIN(tp.updated_at) FROM task_preps tp
			CROSS JOIN task_prep_evidence pe ON pe.issue_id = tp.issue_id
			CROSS JOIN issues i ON i.id = tp.issue_id CROSS JOIN statuses st ON st.id = i.status_id
			CROSS JOIN projects pj ON pj.id = i.project_id
			WHERE tp.updated_at >= ? AND tp.state = 'ready' AND pe.state IN ('pending', 'failed')
			AND st.category NOT IN ('` + CatDone + `', '` + CatCanceled + `')
			AND EXISTS (SELECT 1 FROM experiment_runs er WHERE er.experiment = '` + ExpPrepEvidence + `' AND er.stopped_at IS NULL
				AND (coalesce(json_array_length(er.projects), 0) = 0
					OR EXISTS (SELECT 1 FROM json_each(er.projects) named WHERE named.value = pj.key COLLATE NOCASE)))
			AND EXISTS (SELECT 1 FROM task_prep_evidence tried WHERE tried.state IN ('pending', 'failed') AND tried.created_at >= ?
				AND (tried.state = 'failed' OR tried.created_at < ?))`,
		args: func(now time.Time) []any {
			return []any{now.Add(-evidenceWaitingWindow).UnixMilli(), now.Add(-evidenceFailingWindow).UnixMilli(), now.Add(-prepPendingMax).UnixMilli()}
		},
		output: `SELECT MAX(pe.created_at) FROM task_prep_evidence pe WHERE pe.state = 'ready'`,
	},
}

const (
	// evidenceWaitingWindow is how recent a prep still waits for its evidence.
	evidenceWaitingWindow = 14 * 24 * time.Hour
	// evidenceFailingWindow is how recently some check must have been tried
	// that failed or was lost, for any prep to be waiting.
	evidenceFailingWindow = 48 * time.Hour
)

// What waits for the transcript archive, by machine: bytes the machine says
// are pending, or an archive status that stopped (the machine reported before
// and its usage sync still arrives more than ?1 ms after that report; the
// client re-reports every 12 hours, so a healthy idle machine is not this).
// A machine that opted out waits for nothing, nor does one last heard from (the
// later of its usage sync and archive status) before ?2.
const archiveWaitingWhere = `coalesce(m.transcript_off, 0) = 0 AND max(coalesce(m.last_sync_at, 0), coalesce(m.transcript_status_at, 0)) >= ?2 AND (m.transcript_pending_bytes > 0 OR ` + archiveSilent + `)`
const archiveSilent = `(m.transcript_status_at IS NOT NULL AND m.last_sync_at > m.transcript_status_at + ?1)`

// archiveWaitingWindow is how long after a machine was last heard from it can
// still have something to send.
const archiveWaitingWindow = 30 * 24 * time.Hour

// archiveSince is when the machine began to wait: its bytes began to, or its
// status stopped, whichever is earlier.
const archiveSince = `min(coalesce(CASE WHEN m.transcript_pending_bytes > 0 THEN coalesce(m.transcript_pending_since, m.transcript_status_at) END,
	CASE WHEN ` + archiveSilent + ` THEN m.transcript_status_at END),
	coalesce(CASE WHEN ` + archiveSilent + ` THEN m.transcript_status_at END,
	CASE WHEN m.transcript_pending_bytes > 0 THEN coalesce(m.transcript_pending_since, m.transcript_status_at) END))`

// transcriptDroppedReasons are the drop reasons (cmd/wp/usage_archive.go) that
// mean something is not in the archive and will not get there unless someone
// acts: a line replaced by a stub, a file the server refuses for good (the
// machine retries it daily, and an active machine still stores other pieces,
// so it never shows as stalled), and the unsent bytes of a file that was
// deleted. upload_failed, out_of_sync and state_reset stay out: the next pass
// settles them.
const transcriptDroppedReasons = `('line_too_large', 'redaction_failed', 'refused', 'deleted_unsent')`

// CollectorHealth is how one collector stood at the last hourly check.
type CollectorHealth struct {
	Name         string     `json:"name"`
	Waiting      int        `json:"waiting"`
	OldestWaitAt *time.Time `json:"oldest_wait_at,omitempty"`
	LastOutputAt *time.Time `json:"last_output_at,omitempty"`
	Stalled      bool       `json:"stalled"`
	StalledSince *time.Time `json:"stalled_since,omitempty"`
	CheckedAt    time.Time  `json:"checked_at"`
	// Dropped is the total this collector has counted as lost (nil: it counts
	// none); Off how many of its machines opted out (nil: not applicable).
	Dropped *int64 `json:"dropped,omitempty"`
	Off     *int64 `json:"off,omitempty"`
	// Problem is something wrong that is not a stall, in plain words: the
	// transcript archive's stored pieces whose files are gone (WP-1466).
	Problem string `json:"problem,omitempty"`
}

// collectorReading is what one collector's queries said.
type collectorReading struct {
	waiting      int
	oldest, last int64 // ms; 0: none
	// perRepo: the stall is judged by repo; repos are the stalled ones,
	// the longest waiting first.
	perRepo bool
	repos   []repoWait
	// dropped and off, for a collector that has the queries (nil: none).
	dropped, off *int64
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
	for _, q := range []struct {
		sql string
		to  **int64
	}{{c.dropped, &r.dropped}, {c.off, &r.off}} {
		if q.sql == "" {
			continue
		}
		var n int64
		if err := s.db.R.QueryRowContext(ctx, q.sql).Scan(&n); err != nil {
			return r, fmt.Errorf("counts: %w", err)
		}
		*q.to = &n
	}
	return r, nil
}

// CheckCollectorHealth reads every collector, keeps its row, and tells the
// admins about the stalled ones. Hourly.
func (s *Service) CheckCollectorHealth(ctx context.Context) {
	now := s.now()
	s.CheckTranscriptFiles(ctx) // daily, and at the start: the files of stored pieces (WP-1466)
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
		if _, err := t.ExecContext(ctx, `INSERT INTO collector_health(name, stalled_since, alerted_at, alerted_repos, waiting, oldest_wait_at, last_output_at, checked_at, dropped, off)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET stalled_since = excluded.stalled_since, alerted_at = excluded.alerted_at, alerted_repos = excluded.alerted_repos,
				waiting = excluded.waiting, oldest_wait_at = excluded.oldest_wait_at, last_output_at = excluded.last_output_at, checked_at = excluded.checked_at,
				dropped = excluded.dropped, off = excluded.off`,
			c.name, stalledSince, keepTold, keepRepos, r.waiting, nullMS(r.oldest), nullMS(r.last), now.UnixMilli(), ptrVal(r.dropped), ptrVal(r.off)); err != nil {
			return err
		}
		if err := s.noteDropped(ctx, t, c, r, now); err != nil {
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
		if c.unit != "" {
			data["unit"] = c.unit
		}
		if c.group != "" {
			data["group"], data["group_help"] = c.group, c.groupHelp
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

func ptrVal(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// noteDropped tells the admins when the total a collector counted as lost
// grew past what the last telling said, at most once collectorRepeat (what is
// held back by that is told at the next check after it). The conditional
// UPDATE lets only one server's check tell.
func (s *Service) noteDropped(ctx context.Context, t *tx, c collector, r collectorReading, now time.Time) error {
	if r.dropped == nil {
		return nil
	}
	var toldTotal, toldAt sql.NullInt64
	if err := t.QueryRowContext(ctx, `SELECT dropped_told, dropped_alerted_at FROM collector_health WHERE name = ?`, c.name).Scan(&toldTotal, &toldAt); err != nil {
		return err
	}
	if *r.dropped < toldTotal.Int64 {
		// the total fell (a machine's state reset); what grows from here is new
		if _, err := t.ExecContext(ctx, `UPDATE collector_health SET dropped_told = ? WHERE name = ?`, *r.dropped, c.name); err != nil {
			return err
		}
		return nil
	}
	if *r.dropped == toldTotal.Int64 || (toldAt.Valid && now.Sub(msTime(toldAt.Int64)) < collectorRepeat) {
		return nil
	}
	res, err := t.ExecContext(ctx, `UPDATE collector_health SET dropped_told = ?, dropped_alerted_at = ? WHERE name = ? AND coalesce(dropped_alerted_at, 0) = ?`,
		*r.dropped, now.UnixMilli(), c.name, toldAt.Int64)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	s.log.WarnContext(ctx, "collector dropped data", "collector", c.name, "dropped", *r.dropped, "grew_by", *r.dropped-toldTotal.Int64)
	t.emit(&Event{Kind: EvCollectorDropped, Data: map[string]any{"name": c.name, "about": c.about, "dropped": *r.dropped,
		"grew_by": *r.dropped - toldTotal.Int64, "again": toldAt.Valid}})
	return nil
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
	unit := ""
	if u := dataStr(e, "unit"); u != "" {
		unit = " " + u
	}
	title := fmt.Sprintf("%s has stalled: %d%s waiting, nothing collected for %s", name, dataInt(e, "waiting"), unit,
		ShortDuration(collectorStallAfter))
	lastWhere := "the last output was "
	noun := "repo"
	if g := dataStr(e, "group"); g != "" {
		noun = g
	}
	if e.Data["per_repo"] == true {
		title = fmt.Sprintf("%s has stalled in %s: %d waiting, nothing collected there for %s", name,
			plural1(int(dataInt(e, "stalled_repos")), noun), dataInt(e, "waiting"), ShortDuration(collectorStallAfter))
		lastWhere = "the last output, in any " + noun + ", was "
	}
	body := fmt.Sprintf("%s. The oldest has waited %s; %s%s. Likely no runner holds a checkout to do it, or the work fails before it records anything: wp server status lists the collectors, wp server log --level warn the failures.",
		dataStr(e, "about"), ShortDuration(now.Sub(msTime(dataInt(e, "oldest_ms")))), lastWhere, lastOutputText(dataInt(e, "last_ms"), now))
	if by := dataStr(e, "by_repo"); by != "" {
		if help := dataStr(e, "group_help"); help != "" {
			body += " Stalled " + noun + "s (waiting, oldest): " + by + ". " + help
		} else {
			body += " Stalled repos (waiting, oldest): " + by + ". A repo no runner holds a checkout of waits for good: attach a runner with one, or detach the repo."
		}
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

// notifyCollectorDropped tells the admins a collector's lost total grew.
func (s *Service) notifyCollectorDropped(ctx context.Context, e *Event) {
	title := fmt.Sprintf("%s has dropped %d (up %d)", dataStr(e, "name"), dataInt(e, "dropped"), dataInt(e, "grew_by"))
	body := fmt.Sprintf("%s. What it dropped is not in the archive and will not get there unless someone acts (a stubbed line, a file the server refuses, unsent bytes of a deleted file): wp usage machines shows which machine and why. This tells when the total grows, at most once a day.", dataStr(e, "about"))
	if e.Data["again"] == true {
		body = "Still dropping. " + body
	}
	for _, id := range s.userIDsWhere(ctx, `SELECT id FROM users WHERE is_admin = 1 AND disabled = 0 ORDER BY id`) {
		s.notifyURL(ctx, id, nil, NotifyCollector, title, body, "")
	}
}

// CollectorHealthList is the collectors as the last check found them, in
// the order they are listed; one not checked yet is left out.
func (s *Service) CollectorHealthList(ctx context.Context) ([]CollectorHealth, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT name, stalled_since, waiting, oldest_wait_at, last_output_at, checked_at, dropped, off FROM collector_health`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	by := map[string]CollectorHealth{}
	for rows.Next() {
		var h CollectorHealth
		var since, oldest, last, dropped, off sql.NullInt64
		var checked int64
		if err := rows.Scan(&h.Name, &since, &h.Waiting, &oldest, &last, &checked, &dropped, &off); err != nil {
			return nil, err
		}
		if dropped.Valid {
			h.Dropped = &dropped.Int64
		}
		if off.Valid {
			h.Off = &off.Int64
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
			if c.problem != nil {
				var err error
				if h.Problem, err = c.problem(s, ctx); err != nil {
					return nil, err
				}
			}
			out = append(out, h)
		}
	}
	return out, nil
}

func msPtr(ms int64) *time.Time { t := msTime(ms); return &t }
