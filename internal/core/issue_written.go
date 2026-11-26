package core

import (
	"context"
	"database/sql"
	"errors"
)

// Written is what was live when an issue was written (WP-460): the version
// its project's production environment ran then and what it runs now, so a
// reader can tell what the task assumed and how much has shipped since.
type Written struct {
	Environment string `json:"environment,omitempty"` // its name; "" once it's deleted
	Version     string `json:"version,omitempty"`
	Commit      string `json:"commit,omitempty"`
	// What the environment runs now, and how many other versions it has
	// been deployed with since the issue was written.
	LiveVersion string `json:"live_version,omitempty"`
	LiveCommit  string `json:"live_commit,omitempty"`
	Releases    int    `json:"releases_since"`
}

// writtenEnvOrder picks the environment that says what's live for a project:
// production, else staging; running ones first. The migration that filled
// issues from before WP-460 (0119) orders them the same way.
const writtenEnvOrder = `CASE kind WHEN 'production' THEN 0 ELSE 1 END, stopped_at IS NOT NULL, position, id`

// recordWritten notes on a new issue what its project's live environment
// runs now. Nothing when the project has no deployed production or staging
// environment.
func recordWritten(ctx context.Context, t queryer, issueID, projectID int64) error {
	var env int64
	var version, commit string
	err := t.QueryRowContext(ctx, `SELECT id, version, commit_sha FROM environments
		WHERE project_id = ? AND kind IN ('production', 'staging') ORDER BY `+writtenEnvOrder+` LIMIT 1`, projectID).Scan(&env, &version, &commit)
	if errors.Is(err, sql.ErrNoRows) || err == nil && version == "" && commit == "" {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = t.ExecContext(ctx, `UPDATE issues SET written_env = ?, written_version = ?, written_commit = ? WHERE id = ?`, env, version, commit, issueID)
	return err
}

// issueWritten is what was live when the issue was written, and what is
// now; nil when nothing was recorded.
func issueWritten(ctx context.Context, q queryer, issueID int64) (*Written, error) {
	var w Written
	var env sql.NullInt64
	var name, liveVersion, liveCommit sql.NullString
	var created int64
	err := q.QueryRowContext(ctx, `SELECT i.written_env, i.written_version, i.written_commit, i.created_at, e.name, e.version, e.commit_sha
		FROM issues i LEFT JOIN environments e ON e.id = i.written_env WHERE i.id = ?`, issueID).
		Scan(&env, &w.Version, &w.Commit, &created, &name, &liveVersion, &liveCommit)
	if err != nil {
		return nil, err
	}
	if w.Version == "" && w.Commit == "" {
		return nil, nil
	}
	if !env.Valid || !name.Valid {
		return &w, nil
	}
	w.Environment, w.LiveVersion, w.LiveCommit = name.String, liveVersion.String, liveCommit.String
	err = q.QueryRowContext(ctx, `SELECT count(DISTINCT CASE WHEN version != '' THEN version ELSE commit_sha END) FROM deployments
		WHERE environment_id = ? AND status IN ('succeeded', 'detected') AND coalesce(finished_at, started_at) > ?
		  AND (version != '' OR commit_sha != '') AND CASE WHEN version != '' THEN version != ? ELSE commit_sha != ? END`,
		env.Int64, created, w.Version, w.Commit).Scan(&w.Releases)
	return &w, err
}
