package core

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.jmann.me/claude/waypoint/internal/tracing"
)

const (
	envCheckTimeout = 10 * time.Second
	envMaxParallel  = 8

	// A deployment this recent explains a version endpoint still showing the
	// old release (a rollout under way); later, the endpoint is the truth.
	envRolloutGrace = 15 * time.Minute
	// Failed checks in a row before an environment is called down.
	envDownAfter    = 2
	envMinuteKeep   = 48 * time.Hour
	envHourlyKeep   = 90 * 24 * time.Hour
	envMaxBody      = 256 << 10
	envHeadsKeep    = 60 * 24 * time.Hour
	envTickInterval = 5 * time.Second
)

// envSlow: slower than this is degraded (a variable so tests can lower it).
var envSlow = 3 * time.Second

// envRunner is the state of the check worker.
type envRunner struct {
	http *http.Client
	kick chan struct{}
}

func newEnvRunner(p EgressPolicy) *envRunner {
	return &envRunner{http: p.client(envCheckTimeout), kick: make(chan struct{}, 1)}
}

// kickEnvChecks wakes the worker: something changed that wants a look soon.
func (s *Service) kickEnvChecks() {
	select {
	case s.envs.kick <- struct{}{}:
	default:
	}
}

// RunEnvChecks runs health checks and environment upkeep until ctx ends.
func (s *Service) RunEnvChecks(ctx context.Context) {
	tick := time.NewTicker(envTickInterval)
	minute := time.NewTicker(time.Minute)
	defer tick.Stop()
	defer minute.Stop()
	s.job(ctx, jobEnvUpkeep, time.Minute, s.envUpkeep)
	for {
		s.untracedJob(ctx, jobEnvChecks, envTickInterval, func(ctx context.Context) { s.runDueEnvChecks(ctx) })
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-s.envs.kick:
		case <-minute.C:
			s.job(ctx, jobEnvUpkeep, time.Minute, s.envUpkeep)
		}
	}
}

// jitter spreads checks: the interval, give or take a tenth.
func jitter(d time.Duration) time.Duration {
	// #nosec G404 -- spreading load, not security
	return d + time.Duration((rand.Float64()-0.5)*0.2*float64(d))
}

// runDueEnvChecks checks every environment whose time has come, at most
// envMaxParallel at once, and waits for them. It returns how many it ran.
func (s *Service) runDueEnvChecks(ctx context.Context) int {
	type due struct {
		id       int64
		interval int
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, interval_s FROM environments
		WHERE stopped_at IS NULL AND interval_s > 0 AND (url != '' OR health_url != '') AND (next_check_at IS NULL OR next_check_at <= ?)
		ORDER BY COALESCE(next_check_at, 0), id LIMIT 200`, s.nowMS())
	if err != nil {
		s.log.WarnContext(ctx, "list due environments", "err", err)
		return 0
	}
	var list []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.interval); err != nil {
			rows.Close()
			return 0
		}
		list = append(list, d)
	}
	rows.Close()
	if len(list) == 0 {
		return 0
	}
	ctx, span := tracing.Start(ctx, "job environment checks")
	defer span.End()
	sem := make(chan struct{}, envMaxParallel)
	var wg sync.WaitGroup
	ran := 0
	for _, d := range list {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return ran
		}
		// Move next_check_at forward before the check runs, so a slow check isn't picked up and started again by the next tick.
		next := s.now().Add(jitter(time.Duration(d.interval) * time.Second)).UnixMilli()
		if _, err := s.db.W.ExecContext(ctx, `UPDATE environments SET next_check_at = ? WHERE id = ?`, next, d.id); err != nil {
			<-sem
			s.log.WarnContext(ctx, "claim environment check", "err", err)
			continue
		}
		ran++
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := s.checkEnv(ctx, d.id); err != nil && !errors.Is(err, context.Canceled) {
				s.log.WarnContext(ctx, "environment check", "id", d.id, "err", err)
			}
		}()
	}
	wg.Wait()
	return ran
}

// CheckEnvironmentNow checks an environment right away ("check now").
// Editors may; a check within the last few seconds is reused.
func (s *Service) CheckEnvironmentNow(ctx context.Context, actor *Actor, id int64) (*EnvironmentDetail, error) {
	e, err := s.envFor(ctx, s.db.R, actor, id, RoleEditor)
	if err != nil {
		return nil, err
	}
	if e.Stopped {
		return nil, invalid("%s is stopped", e.Name)
	}
	if e.target() == "" {
		return nil, invalid("%s has no address to check", e.Name)
	}
	if e.LastCheckedAt == nil || s.now().Sub(*e.LastCheckedAt) > 3*time.Second {
		if err := s.checkEnv(ctx, id); err != nil {
			return nil, err
		}
	}
	return s.envDetail(ctx, id)
}

type envReport struct{ commit, branch, version string }

func (r envReport) empty() bool { return r.commit == "" && r.version == "" }

type probeResult struct {
	ok         bool
	code       int
	latency    time.Duration
	err        string
	tlsExpires *time.Time
	report     *envReport
}

// envGet fetches an address through the egress policy and returns the
// status, the start of the body, and when its certificate expires.
func (s *Service) envGet(ctx context.Context, target string) (code int, body []byte, tlsExp *time.Time, err error) {
	ctx, cancel := context.WithTimeout(ctx, envCheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("User-Agent", "Waypoint-HealthCheck/1")
	req.Header.Set("Accept", "application/json, */*;q=0.5")
	resp, err := s.envs.http.Do(req) // #nosec G107 G704 -- an environment's address, through the egress policy
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, envMaxBody))
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		t := resp.TLS.PeerCertificates[0].NotAfter.UTC()
		tlsExp = &t
	}
	if err != nil {
		return resp.StatusCode, body, tlsExp, err
	}
	return resp.StatusCode, body, tlsExp, nil
}

// envErrText words a failed request for the page.
func envErrText(err error) string {
	var inv x509.CertificateInvalidError
	var unk x509.UnknownAuthorityError
	var host x509.HostnameError
	switch {
	case errors.As(err, &inv) && inv.Reason == x509.Expired:
		return "certificate expired"
	case errors.As(err, &inv), errors.As(err, &unk):
		return "certificate not trusted"
	case errors.As(err, &host):
		return "certificate is for another host"
	}
	return PublicError(err)
}

// probe checks one environment: its health address, and the version
// endpoint when it has one.
func (s *Service) probe(ctx context.Context, e *Environment) probeResult {
	var res probeResult
	target := e.target()
	start := time.Now()
	code, body, tlsExp, err := s.envGet(ctx, target)
	res.latency, res.code, res.tlsExpires = time.Since(start), code, tlsExp
	switch {
	case err != nil:
		res.err = envErrText(err)
		return res
	case e.ExpectedStatus != 0 && code != e.ExpectedStatus, e.ExpectedStatus == 0 && (code < 200 || code > 299):
		res.err = (&HTTPStatusError{Code: code}).Error()
		return res
	}
	res.ok = true
	if e.VersionURL == "" {
		return res
	}
	vbody := body
	if e.VersionURL != target {
		vcode, b, _, verr := s.envGet(ctx, e.VersionURL)
		if verr != nil || vcode < 200 || vcode > 299 {
			return res
		}
		vbody = b
	}
	if r := parseEnvReport(vbody, e.CommitField, e.BranchField, e.VersionField); !r.empty() {
		res.report = &r
	}
	return res
}

// jsonPath reads a dot path (git.commit, items.0.sha) from decoded JSON as
// text; objects and nulls give "".
func jsonPath(doc any, path string) string {
	cur := doc
	for _, part := range strings.Split(path, ".") {
		switch v := cur.(type) {
		case map[string]any:
			cur = v[part]
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(v) {
				return ""
			}
			cur = v[i]
		default:
			return ""
		}
	}
	switch v := cur.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func parseEnvReport(body []byte, commitField, branchField, versionField string) envReport {
	var doc any
	if json.Unmarshal(body, &doc) != nil {
		return envReport{}
	}
	var r envReport
	if commitField != "" {
		c := strings.ToLower(strings.TrimSpace(jsonPath(doc, commitField)))
		n := 0
		for n < len(c) && (c[n] >= '0' && c[n] <= '9' || c[n] >= 'a' && c[n] <= 'f') {
			n++
		}
		if n >= 7 && n <= 64 {
			r.commit = c[:n]
		}
	}
	if branchField != "" {
		r.branch = cleanLine(jsonPath(doc, branchField), 200)
	}
	if versionField != "" {
		r.version = cleanLine(jsonPath(doc, versionField), 100)
	}
	return r
}

// sameRelease: the endpoint reports the release e (or a deployment) is.
func sameRelease(r *envReport, commit, version string) bool {
	if r.commit != "" {
		return sameCommit(r.commit, commit)
	}
	return r.version != "" && r.version == version
}

// checkEnv probes an environment and records the result.
func (s *Service) checkEnv(ctx context.Context, id int64) error {
	e, err := s.envRow(ctx, s.db.R, id)
	if err != nil {
		return err
	}
	if e.Stopped || e.target() == "" {
		return nil
	}
	res := s.probe(ctx, e)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return s.write(ctx, SystemActor(), func(t *tx) error { return s.applyProbe(t, id, res) })
}

// applyProbe stores a check's result and works out what it means: the
// status (two failures in a row before "down"), uptime counts, a new version.
func (s *Service) applyProbe(t *tx, id int64, res probeResult) error {
	ctx := context.Background()
	e, err := s.envRow(ctx, t, id)
	if err != nil {
		return err
	}
	if e.Stopped {
		return nil
	}
	nowT := s.now()
	now := nowT.UnixMilli()
	lat := int(res.latency.Milliseconds())
	ok := boolInt(res.ok)

	// Minute samples: a failure within the minute wins over later successes.
	minute := nowT.Truncate(time.Minute).UnixMilli()
	if _, err := t.ExecContext(ctx, `INSERT INTO environment_checks(environment_id, at, ok, latency_ms, code, error) VALUES (?,?,?,?,?,?)
		ON CONFLICT(environment_id, at) DO UPDATE SET ok = ok AND excluded.ok, latency_ms = max(latency_ms, excluded.latency_ms),
		code = CASE WHEN excluded.ok = 0 THEN excluded.code ELSE code END, error = CASE WHEN excluded.ok = 0 THEN excluded.error ELSE error END`,
		id, minute, ok, lat, res.code, res.err); err != nil {
		return err
	}
	okLat := 0
	if res.ok {
		okLat = lat
	}
	if _, err := t.ExecContext(ctx, `INSERT INTO environment_hourly(environment_id, hour, checks, ok_checks, latency_sum, latency_max) VALUES (?,?,1,?,?,?)
		ON CONFLICT(environment_id, hour) DO UPDATE SET checks = checks + 1, ok_checks = ok_checks + excluded.ok_checks,
		latency_sum = latency_sum + excluded.latency_sum, latency_max = max(latency_max, excluded.latency_max)`,
		id, nowT.Truncate(time.Hour).UnixMilli(), ok, okLat, okLat); err != nil {
		return err
	}

	mismatch := !res.ok && e.Mismatch
	if res.ok && res.report != nil {
		if mismatch, err = s.reconcileVersion(t, e, *res.report, now); err != nil {
			return err
		}
	}

	prev, next, streak := e.Status, e.Status, e.failStreak
	if res.ok {
		streak = 0
		next = EnvUp
		if res.latency > envSlow || mismatch {
			next = EnvDegraded
		}
	} else {
		streak++
		if streak >= envDownAfter {
			next = EnvDown
		}
	}
	since := nullTime(e.StatusSince)
	if next != prev || e.StatusSince == nil {
		since = now
	}
	var latCol, codeCol, tlsCol, lastOK any
	if res.ok {
		latCol, lastOK = lat, now
	} else if e.lastOKAt != nil {
		lastOK = e.lastOKAt.UnixMilli()
	}
	if res.code != 0 {
		codeCol = res.code
	}
	if res.tlsExpires != nil {
		tlsCol = res.tlsExpires.UnixMilli()
	} else if e.TLSExpiresAt != nil {
		tlsCol = e.TLSExpiresAt.UnixMilli()
	}
	if _, err := t.ExecContext(ctx, `UPDATE environments SET status = ?, status_since = ?, latency_ms = ?, http_code = ?, error = ?, tls_expires_at = ?,
		fail_streak = ?, mismatch = ?, last_checked_at = ?, last_ok_at = ? WHERE id = ?`,
		next, since, latCol, codeCol, res.err, tlsCol, streak, boolInt(mismatch), now, lastOK, id); err != nil {
		return err
	}
	if next != prev && (prev != EnvUnknown || next != EnvUp) {
		extra := map[string]any{"status": next, "previous": prev}
		if res.err != "" {
			extra["error"] = res.err
		}
		if res.code != 0 {
			extra["http_code"] = res.code
		}
		ev := envEvent(EvEnvStatus, e, extra)
		ev.Old, ev.New = prev, next
		t.emit(ev)
	}
	return nil
}

// reconcileVersion compares what the version endpoint reports with what
// deployments say runs. It returns true when the endpoint still shows the
// old release shortly after a deployment (a mismatch); otherwise an
// unknown release is recorded as a "detected" deployment.
func (s *Service) reconcileVersion(t *tx, e *Environment, r envReport, now int64) (bool, error) {
	ctx := context.Background()
	if r.empty() {
		return false, nil
	}
	if sameRelease(&r, e.Commit, e.Version) {
		branch, version, commit := cmp(e.Branch, r.branch), cmp(e.Version, r.version), e.Commit
		if r.commit != "" {
			commit = longer(e.Commit, r.commit)
		}
		if branch != e.Branch || version != e.Version || commit != e.Commit {
			_, err := t.ExecContext(ctx, `UPDATE environments SET branch = ?, version = ?, commit_sha = ? WHERE id = ?`, branch, version, commit, e.ID)
			return false, err
		}
		return false, nil
	}
	var lastCommit, lastVersion string
	var started int64
	var finished *int64
	err := t.QueryRowContext(ctx, `SELECT commit_sha, version, started_at, finished_at FROM deployments
		WHERE environment_id = ? AND status IN ('started','succeeded') ORDER BY started_at DESC, id DESC LIMIT 1`, e.ID).Scan(&lastCommit, &lastVersion, &started, &finished)
	have := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if have {
		if sameRelease(&r, lastCommit, lastVersion) {
			return false, nil
		}
		at := started
		if finished != nil && *finished > at {
			at = *finished
		}
		if now-at < envRolloutGrace.Milliseconds() {
			return true, nil
		}
	}
	res, err := t.ExecContext(ctx, `INSERT INTO deployments(environment_id, commit_sha, branch, version, status, started_at, finished_at, by_kind, notes)
		VALUES (?,?,?,?, 'detected', ?, ?, 'detected', 'seen on the version endpoint')`, e.ID, r.commit, r.branch, r.version, now, now)
	if err != nil {
		return false, err
	}
	depID, _ := res.LastInsertId()
	e.Commit, e.Branch, e.Version = r.commit, r.branch, r.version
	if _, err := t.ExecContext(ctx, `UPDATE environments SET commit_sha = ?, branch = ?, version = ?, deployed_at = ?, updated_at = ? WHERE id = ?`,
		e.Commit, e.Branch, e.Version, now, now, e.ID); err != nil {
		return false, err
	}
	t.emit(envEvent(EvEnvDeployed, e, map[string]any{"deployment": depID, "commit": r.commit, "branch": r.branch, "version": r.version, "status": DeployDetected}))
	return false, nil
}

// envUpkeep ends leases and unreachable dev environments, forgets old
// stopped ones, drops history past its retention, and tells owners about
// environments that run behind their branch (WP-199).
func (s *Service) envUpkeep(ctx context.Context) {
	now := s.now()
	ms := now.UnixMilli()
	stop := func(query, reason string, args ...any) {
		ids, err := queryIDs(ctx, s.db.R, query, args...)
		if err != nil {
			s.log.WarnContext(ctx, "environment upkeep", "err", err)
			return
		}
		for _, id := range ids {
			err := s.write(ctx, SystemActor(), func(t *tx) error {
				e, err := s.envRow(ctx, t, id)
				if err != nil {
					return err
				}
				return s.stopEnvTx(t, e, reason)
			})
			if err != nil {
				s.log.WarnContext(ctx, "stop environment", "id", id, "reason", reason, "err", err)
			}
		}
	}
	stop(`SELECT id FROM environments WHERE stopped_at IS NULL AND expires_at IS NOT NULL AND expires_at <= ?`, "ttl", ms)
	stop(`SELECT id FROM environments WHERE ephemeral = 1 AND stopped_at IS NULL AND interval_s > 0 AND status = 'down' AND status_since IS NOT NULL
		AND status_since <= ? - COALESCE(NULLIF(ttl_s, 0), ?) * 1000`, "unreachable", ms, int64(envDefaultUnreachable/time.Second))
	for _, q := range []struct {
		sql string
		arg int64
	}{
		{`DELETE FROM environments WHERE ephemeral = 1 AND stopped_at IS NOT NULL AND stopped_at <= ?`, now.Add(-envStoppedKeep).UnixMilli()},
		{`DELETE FROM environment_checks WHERE at < ?`, now.Add(-envMinuteKeep).UnixMilli()},
		{`DELETE FROM environment_hourly WHERE hour < ?`, now.Add(-envHourlyKeep).UnixMilli()},
		{`DELETE FROM branch_heads WHERE pushed_at < ?`, now.Add(-envHeadsKeep).UnixMilli()},
	} {
		if _, err := s.db.W.ExecContext(ctx, q.sql, q.arg); err != nil {
			s.log.WarnContext(ctx, "environment upkeep", "err", err)
		}
	}
	s.checkEnvLag(ctx)
}

// onRepoPushed remembers the head of a branch, to say how far behind an
// environment running it is (WP-D19).
func (s *Service) onRepoPushed(ctx context.Context, e *Event) {
	repo, _ := e.Data["repo"].(string)
	branch, _ := e.Data["branch"].(string)
	after, _ := e.Data["after"].(string)
	branch = strings.TrimPrefix(branch, "refs/heads/")
	if repo == "" || branch == "" || strings.Trim(after, "0") == "" {
		return
	}
	if _, err := s.db.W.ExecContext(ctx, `INSERT INTO branch_heads(repo, branch, sha, pushed_at) VALUES (?,?,?,?)
		ON CONFLICT(repo, branch) DO UPDATE SET sha = excluded.sha, pushed_at = excluded.pushed_at`, repo, branch, strings.ToLower(after), s.nowMS()); err != nil {
		s.log.WarnContext(ctx, "record branch head", "repo", repo, "err", err)
		return
	}
	pids, err := queryIDs(ctx, s.db.R, `SELECT DISTINCT e.project_id FROM environments e JOIN project_repos r ON r.project_id = e.project_id
		WHERE e.branch = ? AND e.stopped_at IS NULL AND r.repo = ? COLLATE NOCASE`, branch, repo)
	if err != nil {
		return
	}
	for _, pid := range pids {
		s.Hub.Publish(&Event{Kind: EvEnvUpdated, ProjectID: pid, At: s.now().UTC(), Data: map[string]any{"push": branch}})
	}
}
