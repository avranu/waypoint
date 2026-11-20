package core

// Run features (WP-256, WP-ADR76): what an agent had while it worked on a task,
// recorded as it happened. A release's date can't answer that: the change
// may have merged and not deployed, wp on that machine may be older, or the
// session may have started before it and still hold the old hooks and
// instructions. Recording never gets in the way of the work it describes.

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// What is recorded on a run: what it began with, and what it had last where
// that changed. Whether hooks ran is read from the run when asked for.
const (
	featureServer        = "server"         // this server's version
	featureWP            = "wp"             // the wp that claimed it (the first seen, if a web claim)
	featureSessionServer = "session_server" // what its session started on: hooks and
	featureSessionWP     = "session_wp"     // instructions stay as they were then
	featureKit           = "kit"            // the agent kit commit synced to that checkout
	featureKitDrift      = "kit_drift"      // files that differ from the kit there
	featureCLIVersion    = "cli_version"    // Claude Code's, as far as the session had reported

	featureServerEnd        = "server_end"
	featureWPEnd            = "wp_end"
	featureSessionServerEnd = "session_server_end" // the run moved to a later session
	featureSessionWPEnd     = "session_wp_end"
)

// runEndOf is what a change in a run's version is recorded as.
var runEndOf = map[string]string{
	featureServer:        featureServerEnd,
	featureWP:            featureWPEnd,
	featureSessionServer: featureSessionServerEnd,
	featureSessionWP:     featureSessionWPEnd,
}

var clientVersionRE = lazyre.New(`^[A-Za-z0-9.+_-]{1,64}$`)

// ClientVersionFrom is wp's version from its User-Agent ("wp/v0.80.0"); ""
// for any other client.
func ClientVersionFrom(userAgent string) string {
	v, ok := strings.CutPrefix(strings.TrimSpace(userAgent), "wp/")
	if !ok || !clientVersionRE.MatchString(v) {
		return ""
	}
	return v
}

// latestRun is the agent's newest run on an issue.
func latestRun(ctx context.Context, q queryer, issueID int64, agent string) (run, session int64, err error) {
	err = q.QueryRowContext(ctx, `SELECT id, coalesce(session_id, 0) FROM agent_runs WHERE issue_id = ? AND agent = ? ORDER BY id DESC LIMIT 1`, issueID, agent).Scan(&run, &session)
	return run, session, err
}

// runNow is what a run has at this moment.
func (s *Service) runNow(ctx context.Context, q queryer, actor *Actor, session int64) map[string]string {
	values := map[string]string{featureServer: s.cfg.Version, featureWP: actor.ClientVersion}
	if session == 0 {
		return values
	}
	commit, drift, err := kitAt(ctx, q, session)
	if err != nil {
		s.log.WarnContext(ctx, "run features: kit", "session", session, "err", err)
	}
	var server, wp, cli string
	if err := q.QueryRowContext(ctx, `SELECT a.started_server, a.started_wp, coalesce(u.version, '') FROM agent_sessions a
		LEFT JOIN usage_sessions u ON u.user_id = a.user_id AND u.machine = a.machine AND u.session = a.cli_id WHERE a.id = ?`, session).Scan(&server, &wp, &cli); err != nil {
		s.log.WarnContext(ctx, "run features: session", "session", session, "err", err)
	}
	values[featureKit], values[featureKitDrift] = commit, drift
	values[featureSessionServer], values[featureSessionWP], values[featureCLIVersion] = server, wp, cli
	return values
}

// recordRunFeatures notes what a run that has just begun has. Failing only
// costs analytics, so it logs and never fails the claim or the finish.
func (s *Service) recordRunFeatures(ctx context.Context, q queryer, actor *Actor, issueID int64, agent string) {
	if actor == nil {
		return
	}
	run, session, err := latestRun(ctx, q, issueID, agent)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.log.WarnContext(ctx, "run features", "issue", issueID, "err", err)
		}
		return
	}
	s.putRunFeatures(ctx, q, run, s.runNow(ctx, q, actor, session), false)
}

// recordRun records a claim: a new run's start if the insert opened one, or
// what changed on the run it left open.
func (s *Service) recordRun(ctx context.Context, q queryer, actor *Actor, issueID int64, agent string, opened sql.Result) {
	if n, _ := opened.RowsAffected(); n > 0 {
		s.recordRunFeatures(ctx, q, actor, issueID, agent)
		return
	}
	s.recordRunChanges(ctx, q, actor, issueID, agent)
}

// recordRunChanges notes, on a run already under way, what it has now that
// it didn't begin with: such a run had a release for only part of its work.
// A run with no record of its start began before recording did, and what it
// has now says nothing about what it began with, so it stays unrecorded.
func (s *Service) recordRunChanges(ctx context.Context, q queryer, actor *Actor, issueID int64, agent string) {
	if actor == nil {
		return
	}
	run, session, err := latestRun(ctx, q, issueID, agent)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.log.WarnContext(ctx, "run features: changes", "issue", issueID, "err", err)
		}
		return
	}
	have, err := runFeaturesOf(ctx, q, run)
	if err != nil {
		s.log.WarnContext(ctx, "run features: changes", "run", run, "err", err)
		return
	}
	if have[featureServer] == "" {
		return
	}
	first, last := map[string]string{}, map[string]string{}
	for name, value := range s.runNow(ctx, q, actor, session) {
		end, tracked := runEndOf[name]
		switch {
		case have[name] == "": // its session, or a wp, only showed up now
			first[name] = value
		case tracked && value != "" && value != have[name]:
			last[end] = value
		}
	}
	s.putRunFeatures(ctx, q, run, first, false)
	s.putRunFeatures(ctx, q, run, last, true)
}

// noteDeployDuringRuns records this server's version on the runs still open
// from an earlier one. The version only changes when the server restarts, so
// they were there for the deploy, and a run that later times out or expires
// has no agent left to say so.
func (s *Service) noteDeployDuringRuns(ctx context.Context) {
	if s.cfg.Version == "" {
		return
	}
	if _, err := s.db.W.ExecContext(ctx, `INSERT INTO run_features(run_id, feature, value, recorded_at)
		SELECT r.id, ?, ?, ? FROM agent_runs r JOIN run_features f ON f.run_id = r.id AND f.feature = ?
		WHERE r.ended_at IS NULL AND f.value != ?
		ON CONFLICT(run_id, feature) DO UPDATE SET value = excluded.value, recorded_at = excluded.recorded_at WHERE run_features.value != excluded.value`,
		featureServerEnd, s.cfg.Version, s.nowMS(), featureServer, s.cfg.Version); err != nil {
		s.log.WarnContext(ctx, "run features: deploy", "err", err)
	}
}

func runFeaturesOf(ctx context.Context, q queryer, run int64) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT feature, value FROM run_features WHERE run_id = ?`, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return nil, err
		}
		out[name] = value
	}
	return out, rows.Err()
}

// putRunFeatures writes values, keeping what's there unless replace.
func (s *Service) putRunFeatures(ctx context.Context, q queryer, run int64, values map[string]string, replace bool) {
	stmt := `INSERT OR IGNORE INTO run_features(run_id, feature, value, recorded_at) VALUES (?,?,?,?)`
	if replace {
		stmt = `INSERT OR REPLACE INTO run_features(run_id, feature, value, recorded_at) VALUES (?,?,?,?)`
	}
	now := s.nowMS()
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if values[name] == "" {
			continue
		}
		if _, err := q.ExecContext(ctx, stmt, run, name, values[name], now); err != nil {
			s.log.WarnContext(ctx, "run features: write", "run", run, "feature", name, "err", err)
		}
	}
}

// kitAt is the kit commit last synced to a session's repo on its machine,
// and how many files there differ from it ("" for none).
func kitAt(ctx context.Context, q queryer, session int64) (commit, drift string, err error) {
	var n int
	err = q.QueryRowContext(ctx, `SELECT k.kit_commit, CASE WHEN json_valid(k.drift) THEN json_array_length(k.drift) ELSE 0 END
		FROM kit_reports k JOIN agent_sessions s ON s.user_id = k.user_id AND s.machine = k.machine AND s.repo = k.repo COLLATE NOCASE
		WHERE s.id = ? AND s.repo != '' ORDER BY k.reported_at DESC LIMIT 1`, session).Scan(&commit, &n)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	if n > 0 {
		drift = strconv.Itoa(n)
	}
	return commit, drift, nil
}
