package core

// Behind-since counts merges, not just the latest head push, which a moving branch always has; only the latest push per branch is stored.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	// envLagDefault is the lag limit production and staging start with.
	envLagDefault = 45
	// envLagAlertMax is the longest lag limit: a week.
	envLagAlertMax = 7 * 24 * 60
	// envLagRepeat: a stall that lasts is told again this often.
	envLagRepeat = 24 * time.Hour
)

// NotifyDeploy is the notification kind of an environment that runs behind.
const NotifyDeploy = "deploy"

// envDefaultLag is the lag limit a new environment starts with: production
// and staging are expected to follow their branch, the rest aren't.
func envDefaultLag(e *Environment) int {
	if !e.Ephemeral && (e.Kind == EnvProduction || e.Kind == EnvStaging) {
		return envLagDefault
	}
	return 0
}

// envLag is how an environment stands against its branch.
type envLag struct {
	env   *Environment
	head  string
	since time.Time
}

// checkEnvLag tells the owners about environments that have run behind
// their branch longer than their lag limit. It runs with environment upkeep.
func (s *Service) checkEnvLag(ctx context.Context) {
	envs, err := s.queryEnvs(ctx, `e.stopped_at IS NULL AND p.archived = 0 AND e.lag_alert_m > 0 AND e.branch != '' AND e.commit_sha != ''`)
	if err != nil {
		s.log.WarnContext(ctx, "deploy lag: list environments", "err", err)
		return
	}
	now := s.now()
	for _, e := range envs {
		if !e.Behind || e.repoRef == nil {
			continue
		}
		lag, err := s.envBehindSince(ctx, e)
		if err != nil {
			s.log.WarnContext(ctx, "deploy lag", "environment", e.ID, "err", err)
			continue
		}
		if lag == nil {
			continue
		}
		if now.Sub(lag.since) < time.Duration(e.LagAlertM)*time.Minute {
			continue
		}
		if lag.toldAt() && now.Sub(*e.lagAlertedAt) < envLagRepeat {
			continue
		}
		if err := s.alertEnvLag(ctx, lag, now); err != nil {
			s.log.WarnContext(ctx, "deploy lag: alert", "environment", e.ID, "err", err)
		}
	}
}

// envBehindSince works out since when e has been behind its branch's head;
// nil when the head on record can't be trusted. e is Behind, with its repo
// resolved.
func (s *Service) envBehindSince(ctx context.Context, e *Environment) (*envLag, error) {
	var pushed int64
	err := s.db.R.QueryRowContext(ctx, `SELECT pushed_at FROM branch_heads WHERE repo = ? AND branch = ?`, e.repoRef.repo, e.Branch).Scan(&pushed)
	if err != nil {
		return nil, err
	}
	since := msTime(pushed)
	got, err := s.envGotCommit(ctx, e)
	if err != nil || got == nil {
		return &envLag{env: e, head: e.Head, since: since}, err
	}
	if since.After(*got) {
		first, err := s.firstMergeAfter(ctx, e, *got)
		if err != nil {
			return nil, err
		}
		if first != nil && first.Before(since) {
			since = *first
		}
		return &envLag{env: e, head: e.Head, since: since}, nil
	}
	// If a merge came after the head's push, that push may never have reached Waypoint, so the head on record can be stale and the lag unknown.
	missed, err := s.firstMergeAfter(ctx, e, since.Add(envLagHeadSlack))
	if err != nil || missed != nil {
		return nil, err
	}
	return &envLag{env: e, head: e.Head, since: *got}, nil
}

// firstMergeAfter is when the first pull request merged into e's branch
// after t; nil if none did.
func (s *Service) firstMergeAfter(ctx context.Context, e *Environment, t time.Time) (*time.Time, error) {
	var first sql.NullInt64
	err := s.db.R.QueryRowContext(ctx, `SELECT min(merged_at) FROM pull_requests
		WHERE repo = ? COLLATE NOCASE AND base_ref = ? AND state = 'merged' AND merged_at > ?`,
		e.repoRef.repo, e.Branch, t.UnixMilli()).Scan(&first)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if !first.Valid {
		return nil, nil
	}
	m := msTime(first.Int64)
	return &m, nil
}

// envLagHeadSlack: a merge this long after the head's push means a push
// was missed.
const envLagHeadSlack = 2 * time.Minute

// envGotCommit is when e first got the commit it runs: its earliest
// deployment of that commit since one of anything else, so redeploying the
// same release doesn't restart the clock. Without deployments on record,
// when it was last deployed.
func (s *Service) envGotCommit(ctx context.Context, e *Environment) (*time.Time, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT commit_sha, coalesce(finished_at, started_at) FROM deployments
		WHERE environment_id = ? AND status IN ('succeeded','detected') AND commit_sha != ''
		ORDER BY coalesce(finished_at, started_at) DESC, id DESC LIMIT 200`, e.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	got, seen := e.DeployedAt, false
	for rows.Next() {
		var sha string
		var at int64
		if err := rows.Scan(&sha, &at); err != nil {
			return nil, err
		}
		if !sameCommit(sha, e.Commit) {
			break
		}
		if t := msTime(at); !seen || t.Before(*got) {
			got, seen = &t, true
		}
	}
	return got, rows.Err()
}

// alertEnvLag records that the owners were told about this stall and emits
// env.lagging; dispatch tells them (notifyEnvLag).
func (s *Service) alertEnvLag(ctx context.Context, l *envLag, now time.Time) error {
	e := l.env
	return s.write(ctx, SystemActor(), func(t *tx) error {
		// Only if nothing changed since it was read: a deploy that landed
		// meanwhile, or another server's check, wins.
		res, err := t.ExecContext(ctx, `UPDATE environments SET lag_alerted_at = ?
			WHERE id = ? AND commit_sha = ? AND coalesce(lag_alerted_at, 0) = ?`,
			now.UnixMilli(), e.ID, e.Commit, msOrZero(e.lagAlertedAt))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		t.emit(envEvent(EvEnvLagging, e, map[string]any{
			"commit": e.Commit, "version": e.Version, "head": l.head, "branch": e.Branch, "repo": e.Repo,
			"since": l.since.UTC().Format(time.RFC3339), "behind_m": int(now.Sub(l.since).Minutes()), "limit_m": e.LagAlertM,
			"again": l.toldAt(),
		}))
		return nil
	})
}

// toldAt: the owners were told about this stall already (since it began).
func (l *envLag) toldAt() bool {
	return l.env.lagAlertedAt != nil && l.env.lagAlertedAt.After(l.since)
}

func msOrZero(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.UnixMilli()
}

// notifyEnvLag tells the project's owners (the admins, if it has none) that
// an environment runs behind its branch.
func (s *Service) notifyEnvLag(ctx context.Context, e *Event) {
	name, project := dataStr(e, "name"), dataStr(e, "project")
	branch, version := dataStr(e, "branch"), dataStr(e, "version")
	behind := time.Duration(dataInt(e, "behind_m")) * time.Minute
	title := fmt.Sprintf("%s %s is %s behind %s", project, name, ShortDuration(behind), branch)
	runs := short(dataStr(e, "commit"))
	if version != "" {
		runs += " (" + version + ")"
	}
	body := fmt.Sprintf("%s is at %s; %s still runs %s. Unless it was rolled back on purpose, a release or deploy didn't happen: look at main's release and deploy runs (wp ci status) and the deployer.",
		branch, short(dataStr(e, "head")), name, runs)
	if e.Data["again"] == true {
		body = "Still behind. " + body
	}
	people := s.userIDsWhere(ctx, `SELECT m.user_id FROM project_members m JOIN users u ON u.id = m.user_id
		WHERE m.project_id = ? AND m.role = 'owner' AND u.disabled = 0 ORDER BY m.user_id`, e.ProjectID)
	if len(people) == 0 {
		people = s.userIDsWhere(ctx, `SELECT id FROM users WHERE is_admin = 1 AND disabled = 0`)
	}
	link := "/p/" + project + "/environments/" + name
	for _, id := range people {
		s.notifyURL(ctx, id, nil, NotifyDeploy, title, body, link)
	}
}
