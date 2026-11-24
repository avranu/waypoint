package core

// CI evidence (WP-704). The backend test jobs print, at the end of their
// log, each test's result, the shards' merged cover profiles and (one shard)
// the tree's #nosec, //nolint and //wplint:ignore comments, as "wp-evidence" lines
// (internal/testevidence, .forgejo/scripts/gotest-shard.sh). Before, CI
// computed all of it and threw it away: nothing said which tests were
// flaky or slow, what internal/core's coverage was (each of four shards
// printed a partial number), or how many findings had been waved through.
//
// The CI history pass (every ten minutes) reads the logs of finished jobs
// it has kept (ci_jobs) and keeps what they carry. Which jobs carry
// evidence is learned per job name, so a repo whose CI prints none costs a
// few reads a week. A re-run replaces a job's log, so the babysitter's
// flake re-run first reads the failed jobs' evidence: those failures are
// what a flake score is made of. Decision: WP-ADR62.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/testevidence"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

const (
	// evidenceReads is how many job logs a repo has read per history pass:
	// a full run has four backend shards.
	evidenceReads = 24
	// evidenceSettle: a job's log may still be uploading just after it ends.
	evidenceSettle = 2 * time.Minute
	// evidenceWindow is how far back unread jobs are read; older ones stay
	// unread.
	evidenceWindow = 48 * time.Hour
	// A kind of job whose last evidenceMisses logs carried none (or
	// evidenceMissesKept, if it once did) is skipped, and probed again an
	// hour later, then after twice as long each time it still has none, at
	// most a week (evidenceProbeAfter).
	evidenceMisses     = 3
	evidenceMissesKept = 20
	evidenceProbeFirst = time.Hour
	evidenceReprobe    = 7 * 24 * time.Hour
	// evidenceKeep is how long test results and coverage files are kept;
	// per package coverage and the suppression ledger stay.
	evidenceKeep = 30 * 24 * time.Hour
)

// Evidence verdicts in ci_jobs.evidence.
const (
	evidenceKept       = 1
	evidenceNone       = 0
	evidenceUnreadable = -1
	evidenceSkipped    = -2
)

var digitsRE = lazyre.New(`\d+`)

// evidenceKind is a job name with its numbers as N, so the shards of a
// matrix are one kind.
func evidenceKind(name string) string {
	return digitsRE.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "N")
}

// evidenceJob is what is kept of a job alongside its evidence.
type evidenceJob struct {
	task      int64
	sha, ref  string
	event     string
	tier      string
	at        int64
	pushed    int64 // Its run's creation time (0 if unknown) orders scans of the default branch.
	onDefault bool
}

type evidenceKindState struct {
	kept, misses int
	readAt       int64
}

// readCIEvidence reads the evidence of a repo's finished jobs that haven't
// been read, oldest first, at most evidenceReads logs.
func (s *Service) readCIEvidence(ctx context.Context, f *Forge, repo string) error {
	cl, ok := s.Forges.(ForgeActionLogs)
	if !ok || f.token == "" {
		return nil
	}
	now := s.now()
	rows, err := s.db.R.QueryContext(ctx, `SELECT j.task_id, j.run_number, j.workflow, j.name, j.event, j.ref, j.sha, j.tier, j.ended_at, coalesce(r.created_at, 0)
		FROM ci_jobs j LEFT JOIN ci_runs r ON r.forge_id = j.forge_id AND r.repo = j.repo AND r.number = j.run_number
		WHERE j.forge_id = ? AND j.repo = ? AND j.evidence IS NULL AND j.status IN ('success', 'failure') AND j.ended_at >= ? AND j.ended_at < ?
		ORDER BY j.ended_at, j.task_id LIMIT 500`,
		f.ID, repo, now.Add(-evidenceWindow).UnixMilli(), now.Add(-evidenceSettle).UnixMilli())
	if err != nil {
		return err
	}
	type unread struct {
		evidenceJob
		run            int64
		workflow, name string
	}
	var list []unread
	for rows.Next() {
		var u unread
		if rows.Scan(&u.task, &u.run, &u.workflow, &u.name, &u.event, &u.ref, &u.sha, &u.tier, &u.at, &u.pushed) == nil {
			list = append(list, u)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(list) == 0 {
		return err
	}
	kinds, err := s.evidenceKinds(ctx, f.ID, repo)
	if err != nil {
		return err
	}
	def := ""
	defKnown := false
	jobs := newRunJobs(cl, f, repo)
	reads := 0
	for _, u := range list {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		kk := u.workflow + "\x00" + evidenceKind(u.name)
		st := kinds[kk]
		limit := evidenceMisses
		if st.kept > 0 {
			limit = evidenceMissesKept
		}
		skipping := st.misses >= limit
		if skipping && now.UnixMilli()-st.readAt < evidenceProbeAfter(st.misses, limit).Milliseconds() {
			if err := s.setJobEvidence(ctx, f.ID, repo, u.task, evidenceSkipped); err != nil {
				return err
			}
			continue
		}
		if reads >= evidenceReads {
			break
		}
		reads++
		verdict := evidenceUnreadable
		j, ok, err := jobs.job(ctx, u.run, u.task, u.name)
		if err != nil {
			return err
		}
		if ok {
			if u.event == "push" && !defKnown {
				defKnown = true
				def = s.defaultBranchOf(ctx, f, repo)
			}
			u.onDefault = u.event == "push" && onDefaultBranch(u.ref, def)
			verdict, err = s.keepJobEvidence(ctx, cl, f, repo, j.ID, u.evidenceJob)
			if err != nil {
				return err
			}
			reopen := false
			switch verdict {
			case evidenceKept:
				reopen = skipping
				st.kept++
				st.misses = 0
			case evidenceNone:
				st.misses++
			}
			st.readAt = now.UnixMilli()
			kinds[kk] = st
			if _, err := s.db.W.ExecContext(ctx, `INSERT INTO ci_evidence_kinds(forge_id, repo, workflow, kind, kept, misses, read_at) VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(forge_id, repo, workflow, kind) DO UPDATE SET kept = excluded.kept, misses = excluded.misses, read_at = excluded.read_at`,
				f.ID, repo, u.workflow, evidenceKind(u.name), st.kept, st.misses, st.readAt); err != nil {
				return err
			}
			if reopen {
				if err := s.reopenSkippedEvidence(ctx, f.ID, repo, u.workflow, evidenceKind(u.name), now); err != nil {
					return err
				}
			}
		}
		if err := s.setJobEvidence(ctx, f.ID, repo, u.task, verdict); err != nil {
			return err
		}
	}
	return nil
}

// evidenceProbeAfter is how long a skipped kind waits after its last read
// before the next: evidenceProbeFirst once it reaches limit misses, twice as
// long for each probe since that found none, at most evidenceReprobe. A kind
// that starts carrying evidence (a deploy, a branch that merges the change)
// is found again within hours, not a week.
func evidenceProbeAfter(misses, limit int) time.Duration {
	d := evidenceProbeFirst
	for i := limit; i < misses && d < evidenceReprobe; i++ {
		d *= 2
	}
	return min(d, evidenceReprobe)
}

// reopenSkippedEvidence marks the jobs of a kind that were skipped while it
// seemed to carry no evidence as unread again, once one of its logs had
// some: the skip was a guess, and those logs may have it too. Only jobs in
// the reading window; older ones would not be read anyway.
func (s *Service) reopenSkippedEvidence(ctx context.Context, forgeID int64, repo, workflow, kind string, now time.Time) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT task_id, name FROM ci_jobs WHERE forge_id = ? AND repo = ? AND workflow = ? AND evidence = ? AND ended_at >= ?`,
		forgeID, repo, workflow, evidenceSkipped, now.Add(-evidenceWindow).UnixMilli())
	if err != nil {
		return err
	}
	var tasks []int64
	for rows.Next() {
		var task int64
		var name string
		if err := rows.Scan(&task, &name); err != nil {
			rows.Close()
			return err
		}
		if evidenceKind(name) == kind {
			tasks = append(tasks, task)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, task := range tasks {
		if _, err := s.db.W.ExecContext(ctx, `UPDATE ci_jobs SET evidence = NULL WHERE forge_id = ? AND repo = ? AND task_id = ? AND evidence = ?`,
			forgeID, repo, task, evidenceSkipped); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) evidenceKinds(ctx context.Context, forgeID int64, repo string) (map[string]evidenceKindState, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT workflow, kind, kept, misses, read_at FROM ci_evidence_kinds WHERE forge_id = ? AND repo = ?`, forgeID, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]evidenceKindState{}
	for rows.Next() {
		var w, k string
		var st evidenceKindState
		if err := rows.Scan(&w, &k, &st.kept, &st.misses, &st.readAt); err != nil {
			return nil, err
		}
		out[w+"\x00"+k] = st
	}
	return out, rows.Err()
}

func (s *Service) setJobEvidence(ctx context.Context, forgeID int64, repo string, task int64, verdict int) error {
	_, err := s.db.W.ExecContext(ctx, `UPDATE ci_jobs SET evidence = ? WHERE forge_id = ? AND repo = ? AND task_id = ?`, verdict, forgeID, repo, task)
	return err
}

// defaultBranchOf is repo's default branch as the forge says, or "" (main
// or master) when it can't say.
func (s *Service) defaultBranchOf(ctx context.Context, f *Forge, repo string) string {
	if dc, ok := s.Forges.(branchDefaulter); ok {
		if d, err := dc.DefaultBranch(ctx, f, repo); err == nil {
			return d
		}
	}
	return ""
}

// keepJobEvidence reads one job's log and keeps the evidence in it; the
// verdict says what it found. err is the forge or the database failing.
func (s *Service) keepJobEvidence(ctx context.Context, cl ForgeActionLogs, f *Forge, repo string, jobID int64, j evidenceJob) (int, error) {
	log, err := cl.ActionJobLog(ctx, f, repo, jobID)
	if errors.Is(err, ErrNotFound) {
		return evidenceUnreadable, nil
	}
	if err != nil {
		return 0, err
	}
	lines := cleanLog(log)
	p, err := testevidence.Decode(lines)
	var known []string
	if err == nil {
		for _, t := range p.Tests {
			if !slices.Contains(known, t.Package) {
				known = append(known, t.Package)
			}
		}
	}
	if err := s.keepJobPackages(ctx, f.ID, repo, j, goPackages(lines, known)); err != nil {
		s.log.WarnContext(ctx, "CI evidence: keep package times", "repo", repo, "job", jobID, "err", err)
	}
	if errors.Is(err, testevidence.ErrNone) {
		return evidenceNone, nil
	}
	if err != nil {
		s.log.Debug("CI evidence: unreadable", "repo", repo, "job", jobID, "err", err)
		return evidenceUnreadable, nil
	}
	if err := s.keepEvidence(ctx, f.ID, repo, j, p); err != nil {
		return 0, err
	}
	return evidenceKept, nil
}

// keepFailedEvidence reads the evidence of a commit's failed jobs before
// they are re-run (which replaces their logs): a flaky test's failure is
// the half of its score the re-run would otherwise erase. Best effort.
func (s *Service) keepFailedEvidence(ctx context.Context, f *Forge, repo, sha string) {
	cl, ok := s.Forges.(ForgeActionLogs)
	if !ok || f.token == "" {
		return
	}
	runs, err := cl.ActionRunsAt(ctx, f, repo, sha)
	if err != nil {
		s.log.Debug("CI evidence before a re-run: list runs", "repo", repo, "sha", short(sha), "err", err)
		return
	}
	def := ""
	defKnown := false
	for _, run := range runs {
		jobs, err := cl.ActionJobs(ctx, f, repo, run.ID)
		if err != nil {
			s.log.Debug("CI evidence before a re-run: list jobs", "repo", repo, "run", run.Index, "err", err)
			continue
		}
		for _, j := range jobs {
			if j.Status != "failure" || j.TaskID == 0 {
				continue
			}
			at := run.Stopped
			if at.IsZero() {
				at = s.now()
			}
			ej := evidenceJob{task: j.TaskID, sha: run.CommitSHA, ref: run.PrettyRef, event: run.Event, tier: run.tier(), at: at.UnixMilli()}
			if !run.Created.IsZero() {
				ej.pushed = run.Created.UnixMilli()
			}
			if ej.sha == "" {
				ej.sha = sha
			}
			if ej.event == "push" {
				if !defKnown {
					defKnown = true
					def = s.defaultBranchOf(ctx, f, repo)
				}
				ej.onDefault = onDefaultBranch(ej.ref, def)
			}
			verdict, err := s.keepJobEvidence(ctx, cl, f, repo, j.ID, ej)
			if err != nil {
				s.log.Debug("CI evidence before a re-run", "repo", repo, "job", j.ID, "err", err)
				continue
			}
			if verdict == evidenceKept {
				// Mark it kept first: the history pass would otherwise read the re-run's log under this task.
				if err := s.setJobEvidence(ctx, f.ID, repo, j.TaskID, evidenceKept); err != nil {
					s.log.Debug("CI evidence before a re-run: mark", "err", err)
				}
			}
		}
	}
}

// keepEvidence stores one job's evidence: its tests' results, its coverage
// merged into the commit's, and (a scan of the default branch) the
// suppression ledger.
func (s *Service) keepEvidence(ctx context.Context, forgeID int64, repo string, j evidenceJob, p *testevidence.Payload) error {
	tx, err := s.db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := keepTestRuns(ctx, tx, forgeID, repo, j, p.Tests); err != nil {
		return err
	}
	if p.Cover != nil {
		if err := mergeCoverage(ctx, tx, forgeID, repo, j, p); err != nil {
			return err
		}
	}
	if p.ScannedSuppressions && j.onDefault {
		if err := keepSuppressions(ctx, tx, forgeID, repo, j, p.Suppressions); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func keepTestRuns(ctx context.Context, tx *sql.Tx, forgeID int64, repo string, j evidenceJob, tests []testevidence.Test) error {
	for _, t := range tests {
		if t.Name == "" || (t.Outcome != testevidence.Pass && t.Outcome != testevidence.Fail && t.Outcome != testevidence.Skip) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ci_tests(forge_id, repo, package, name) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
			forgeID, repo, t.Package, t.Name); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ci_test_runs(test_id, task_id, sha, ref, event, tier, on_default, outcome, ms, at)
			SELECT id, ?, ?, ?, ?, ?, ?, ?, ?, ? FROM ci_tests WHERE forge_id = ? AND repo = ? AND package = ? AND name = ?
			ON CONFLICT(test_id, task_id) DO UPDATE SET outcome = excluded.outcome, ms = excluded.ms`,
			j.task, j.sha, j.ref, j.event, j.tier, j.onDefault, t.Outcome, max(t.Ms, 0), j.at, forgeID, repo, t.Package, t.Name); err != nil {
			return err
		}
	}
	return nil
}

// mergeCoverage adds a shard's coverage to its commit's: a block ran if any
// shard ran it. The shards test the same tree, so a file's blocks are the
// same in each; a file whose blocks differ takes the newer.
func mergeCoverage(ctx context.Context, tx *sql.Tx, forgeID int64, repo string, j evidenceJob, p *testevidence.Payload) error {
	var blob []byte
	var shards string
	err := tx.QueryRowContext(ctx, `SELECT files, shards FROM ci_coverage WHERE forge_id = ? AND repo = ? AND sha = ?`, forgeID, repo, j.sha).Scan(&blob, &shards)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && len(blob) == 0 {
		return nil // Detail already pruned: a late shard cannot be merged in, so nothing is done.
	}
	byPath := map[string]testevidence.File{}
	if len(blob) > 0 {
		old, err := unzipFiles(blob)
		if err != nil {
			return err
		}
		for _, f := range old {
			byPath[f.Path] = f
		}
	}
	for _, f := range p.Cover {
		if o, ok := byPath[f.Path]; ok && slices.Equal(o.Stmts, f.Stmts) && len(o.Hit) == len(f.Hit) {
			hit := slices.Clone(o.Hit)
			for i := range hit {
				hit[i] |= f.Hit[i]
			}
			f.Hit = hit
		}
		byPath[f.Path] = f
	}
	files := make([]testevidence.File, 0, len(byPath))
	for _, f := range byPath {
		files = append(files, f)
	}
	slices.SortFunc(files, func(a, b testevidence.File) int { return strings.Compare(a.Path, b.Path) })
	blob, err = zipFiles(files)
	if err != nil {
		return err
	}
	shard := p.Shard
	if shard == "" {
		shard = "?"
	}
	set := strings.FieldsFunc(shards, func(r rune) bool { return r == ',' })
	if !slices.Contains(set, shard) {
		set = append(set, shard)
	}
	slices.Sort(set)
	pkgs := map[string][2]int{}
	var stmts, covered int
	for _, f := range files {
		t, c := f.Statements()
		stmts += t
		covered += c
		d := path.Dir(f.Path)
		n := pkgs[d]
		pkgs[d] = [2]int{n[0] + t, n[1] + c}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ci_coverage(forge_id, repo, sha, ref, event, on_default, shards, complete, files, stmts, covered, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(forge_id, repo, sha) DO UPDATE SET ref = CASE WHEN ci_coverage.on_default THEN ci_coverage.ref ELSE excluded.ref END,
			event = CASE WHEN ci_coverage.on_default THEN ci_coverage.event ELSE excluded.event END,
			on_default = max(ci_coverage.on_default, excluded.on_default), shards = excluded.shards, complete = excluded.complete,
			files = excluded.files, stmts = excluded.stmts, covered = excluded.covered, updated_at = excluded.updated_at`,
		forgeID, repo, j.sha, j.ref, j.event, j.onDefault, strings.Join(set, ","), shardsComplete(set), blob, stmts, covered, j.at); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ci_coverage_pkgs WHERE forge_id = ? AND repo = ? AND sha = ?`, forgeID, repo, j.sha); err != nil {
		return err
	}
	for d, n := range pkgs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ci_coverage_pkgs(forge_id, repo, sha, package, stmts, covered) VALUES (?, ?, ?, ?, ?, ?)`,
			forgeID, repo, j.sha, d, n[0], n[1]); err != nil {
			return err
		}
	}
	return nil
}

// shardsComplete says whether shards ("I/N") hold every shard of some N.
func shardsComplete(shards []string) bool {
	have := map[int]map[int]bool{}
	for _, sh := range shards {
		i, n, ok := testevidence.ShardOf(sh)
		if !ok {
			continue
		}
		if have[n] == nil {
			have[n] = map[int]bool{}
		}
		have[n][i] = true
		if len(have[n]) == n {
			return true
		}
	}
	return false
}

func zipFiles(files []testevidence.File) ([]byte, error) {
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	if err := json.NewEncoder(z).Encode(files); err != nil {
		return nil, err
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func unzipFiles(blob []byte) ([]testevidence.File, error) {
	z, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return nil, err
	}
	defer func() { _ = z.Close() }()
	var files []testevidence.File
	if err := json.NewDecoder(io.LimitReader(z, 256<<20)).Decode(&files); err != nil {
		return nil, fmt.Errorf("kept coverage: %w", err)
	}
	return files, nil
}

// keepSuppressions updates the ledger from a scan of the default branch:
// what the scan has is current (and new if unseen), what it lacks is gone.
// A scan is dated by its run's creation, the push, not by when its job
// ended: an older commit's job (or its re-run) that ends after a newer
// commit's has the older tree, and is only counted.
func keepSuppressions(ctx context.Context, tx *sql.Tx, forgeID int64, repo string, j evidenceJob, sups []testevidence.Suppression) error {
	if j.pushed > 0 {
		j.at = j.pushed
	}
	var newest sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT max(at) FROM ci_suppression_scans WHERE forge_id = ? AND repo = ?`, forgeID, repo).Scan(&newest); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ci_suppression_scans(forge_id, repo, sha, at, count) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(forge_id, repo, sha) DO UPDATE SET at = excluded.at, count = excluded.count`, forgeID, repo, j.sha, j.at, len(sups)); err != nil {
		return err
	}
	if newest.Valid && j.at < newest.Int64 {
		return nil
	}
	seen := map[string]int{}
	for _, sp := range sups {
		fp := suppressionFingerprint(sp)
		seen[fp]++
		if n := seen[fp]; n > 1 {
			fp += "-" + strconv.Itoa(n)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ci_suppressions(forge_id, repo, fingerprint, kind, rules, path, line, code, reason, first_sha, first_at, last_sha, last_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(forge_id, repo, fingerprint) DO UPDATE SET line = excluded.line, reason = excluded.reason,
				last_sha = excluded.last_sha, last_at = excluded.last_at, gone_at = NULL`,
			forgeID, repo, fp, sp.Kind, strings.Join(sp.Rules, ","), sp.Path, sp.Line, truncateBytes(sp.Code, 300), truncateBytes(sp.Reason, 500),
			j.sha, j.at, j.sha, j.at); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE ci_suppressions SET gone_at = ? WHERE forge_id = ? AND repo = ? AND gone_at IS NULL AND last_at < ?`,
		j.at, forgeID, repo, j.at)
	return err
}

// suppressionFingerprint names a suppression by what it suppresses, not
// where the file has it: kind, rules, file and squeezed code line.
func suppressionFingerprint(sp testevidence.Suppression) string {
	h := sha256.Sum256([]byte(sp.Kind + "\x00" + strings.Join(sp.Rules, ",") + "\x00" + sp.Path + "\x00" + sp.Code))
	return hex.EncodeToString(h[:8])
}

// pruneCIEvidence forgets test results and coverage files older than
// evidenceKeep; per package coverage and the suppression ledger stay.
func (s *Service) pruneCIEvidence(ctx context.Context) {
	cut := s.nowMS() - evidenceKeep.Milliseconds()
	for _, q := range []string{
		`DELETE FROM ci_test_runs WHERE at < ?`,
		`DELETE FROM ci_job_packages WHERE at < ?`,
		`UPDATE ci_coverage SET files = NULL WHERE files IS NOT NULL AND updated_at < ?`,
	} {
		if _, err := s.db.W.ExecContext(ctx, q, cut); err != nil {
			s.log.WarnContext(ctx, "best-effort write failed", "err", err)
		}
	}
	if _, err := s.db.W.ExecContext(ctx, `DELETE FROM server_settings WHERE key LIKE ? AND NOT EXISTS
		(SELECT 1 FROM babysits b WHERE b.ended_at IS NULL AND ? || b.id = server_settings.key)`, sharedTimeoutPrefix+"%", sharedTimeoutPrefix); err != nil {
		s.log.WarnContext(ctx, "best-effort write failed", "err", err)
	}
	if _, err := s.db.W.ExecContext(ctx, `DELETE FROM ci_tests WHERE NOT EXISTS (SELECT 1 FROM ci_test_runs r WHERE r.test_id = ci_tests.id)`); err != nil {
		s.log.WarnContext(ctx, "best-effort write failed", "err", err)
	}
}
