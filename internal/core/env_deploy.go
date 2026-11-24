package core

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Deployment statuses.
const (
	DeployStarted   = "started"
	DeploySucceeded = "succeeded"
	DeployFailed    = "failed"
	DeployDetected  = "detected"
)

// Who made a deployment.
const (
	DeployByPerson   = "person"
	DeployByAgent    = "agent"
	DeployByCI       = "ci"
	DeployByDetected = "detected"
)

// Deployment is one rollout of a commit to an environment.
type Deployment struct {
	ID            int64      `json:"id"`
	EnvironmentID int64      `json:"environment_id"`
	Commit        string     `json:"commit"`
	CommitURL     string     `json:"commit_url,omitempty"`
	Branch        string     `json:"branch"`
	Version       string     `json:"version"`
	Status        string     `json:"status"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at"`
	// ByKind is person, agent, ci or detected; By is the person (the token's
	// owner for CI and agents) and Agent the agent's identity.
	ByKind string   `json:"by_kind"`
	By     *UserRef `json:"by"`
	Agent  string   `json:"agent,omitempty"`
	// URL is the CI run that did it.
	URL    string `json:"url,omitempty"`
	Notes  string `json:"notes,omitempty"`
	byUser *int64
}

const deployCols = `d.id, d.environment_id, d.commit_sha, d.branch, d.version, d.status, d.started_at, d.finished_at, d.by_kind, d.by_user, d.by_agent, d.ci_url, d.notes`

func scanDeployment(sc interface{ Scan(...any) error }) (*Deployment, error) {
	var d Deployment
	var started int64
	var finished, by sql.NullInt64
	if err := sc.Scan(&d.ID, &d.EnvironmentID, &d.Commit, &d.Branch, &d.Version, &d.Status, &started, &finished, &d.ByKind, &by, &d.Agent, &d.URL, &d.Notes); err != nil {
		return nil, err
	}
	d.StartedAt, d.FinishedAt, d.byUser = msTime(started), ptrTime(finished), ptrInt(by)
	return &d, nil
}

func (s *Service) fillDeployment(ctx context.Context, cache map[int64]*UserRef, d *Deployment) {
	d.By = s.cachedUser(ctx, cache, nullInt64(d.byUser))
}

func (s *Service) listDeployments(ctx context.Context, envID int64, limit int) ([]*Deployment, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+deployCols+` FROM deployments d WHERE d.environment_id = ? ORDER BY d.started_at DESC, d.id DESC LIMIT ?`, envID, limit)
	if err != nil {
		return nil, err
	}
	var out []*Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	cache := map[int64]*UserRef{}
	for _, d := range out {
		s.fillDeployment(ctx, cache, d)
	}
	if out == nil {
		out = []*Deployment{}
	}
	return out, nil
}

// ListDeployments is an environment's deployments, newest first.
func (s *Service) ListDeployments(ctx context.Context, actor *Actor, envID int64, limit int) ([]*Deployment, error) {
	e, err := s.envFor(ctx, s.db.R, actor, envID, RoleViewer)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	list, err := s.listDeployments(ctx, envID, limit)
	if err != nil {
		return nil, err
	}
	if d, err := s.queryEnvs(ctx, `e.id = ?`, e.ID); err == nil && len(d) == 1 && d[0].repoRef != nil {
		for _, dep := range list {
			dep.CommitURL = d[0].repoRef.commitURL(dep.Commit)
		}
	}
	return list, nil
}

// DeployInput is what CI (or a person) reports. Status is started,
// succeeded (the default) or failed.
type DeployInput struct {
	Commit  string `json:"commit"`
	Branch  string `json:"branch"`
	Version string `json:"version"`
	Status  string `json:"status"`
	// URL is the CI run's address.
	URL   string `json:"url"`
	Notes string `json:"notes"`
	// Source overrides who is credited: "ci" or "person". Without it, a run
	// address means CI, an agent identity means an agent, else a person.
	Source string `json:"source"`
}

// RecordDeployment records a deployment to an environment by id. A token
// with editor access on the project may (this is what CI uses).
func (s *Service) RecordDeployment(ctx context.Context, actor *Actor, envID int64, in DeployInput) (*Deployment, error) {
	if err := actor.need(PermEnvironmentsWrite); err != nil {
		return nil, err
	}
	e, err := s.envFor(ctx, s.db.R, actor, envID, RoleEditor)
	if err != nil {
		return nil, err
	}
	var d *Deployment
	err = s.write(ctx, actor, func(t *tx) error {
		cur, err := s.envRow(ctx, t, e.ID)
		if err != nil {
			return err
		}
		d, err = s.recordDeployTx(t, cur, in)
		return err
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

// RecordDeploymentNamed records a deployment to a project's environment by
// name, creating the environment when it doesn't exist yet: its kind from
// its name (production, staging and dev by name, else a preview). Creating
// needs what recording does: editor access to the project.
func (s *Service) RecordDeploymentNamed(ctx context.Context, actor *Actor, projectRef, name string, in DeployInput) (*Deployment, error) {
	if err := actor.need(PermEnvironmentsWrite); err != nil {
		return nil, err
	}
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleEditor); err != nil {
		return nil, err
	}
	n, err := cleanEnvName(name)
	if err != nil {
		return nil, err
	}
	var d *Deployment
	err = s.write(ctx, actor, func(t *tx) error {
		cur, err := s.envRowByName(ctx, t, p.ID, n)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err != nil {
			kind := InferEnvKind(n)
			cur = &Environment{ProjectID: p.ID, Project: p.Key, ProjectName: p.Name, Name: n, Kind: kind, IntervalS: envDefaultInterval,
				Ephemeral: kind == EnvPreview, Status: EnvUnknown}
			if err := s.insertEnv(t, cur); err != nil {
				return err
			}
			t.emit(envEvent(EvEnvCreated, cur, nil))
		}
		d, err = s.recordDeployTx(t, cur, in)
		return err
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

// recordDeployTx applies a deployment report to e (loaded in this
// transaction) and emits its events.
func (s *Service) recordDeployTx(t *tx, e *Environment, in DeployInput) (*Deployment, error) {
	ctx := context.Background()
	status := in.Status
	if status == "" {
		status = DeploySucceeded
	}
	if status != DeployStarted && status != DeploySucceeded && status != DeployFailed {
		return nil, invalid("status is started, succeeded or failed")
	}
	commit := strings.ToLower(strings.TrimSpace(in.Commit))
	if commit != "" && !commitRE.MatchString(commit) {
		return nil, invalid("a commit is a hex id such as 3fa9c1e")
	}
	version := cleanLine(in.Version, 100)
	branch := cleanLine(in.Branch, 200)
	if commit == "" && version == "" {
		return nil, invalid("say which commit (or version) was deployed")
	}
	if err := validEnvURL("the CI run address", strings.TrimSpace(in.URL)); err != nil {
		return nil, err
	}
	ci := strings.TrimSpace(in.URL)
	notes := excerpt(in.Notes, 1000)
	by, agent := DeployByPerson, t.actor.agentName()
	switch {
	case in.Source == "ci":
		by = DeployByCI
	case agent != "":
		by = DeployByAgent
	case in.Source == "" && ci != "":
		by = DeployByCI
	}
	var byUser any
	if t.actor != nil && t.actor.User != nil && t.actor.User.ID != 0 {
		byUser = t.actor.User.ID
	}
	now := s.nowMS()

	var open *Deployment
	{
		rows, err := t.QueryContext(ctx, `SELECT `+deployCols+` FROM deployments d WHERE d.environment_id = ? AND d.status = 'started' ORDER BY d.started_at DESC, d.id DESC LIMIT 20`, e.ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			d, err := scanDeployment(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			same := commit != "" && sameCommit(commit, d.Commit) || commit == "" && d.Commit == "" && version == d.Version
			if same && open == nil {
				open = d
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	var d *Deployment
	if open != nil {
		d = open
		d.Commit = longer(commit, d.Commit)
		d.Branch, d.Version = cmp(branch, d.Branch), cmp(version, d.Version)
		d.URL, d.Notes = cmp(ci, d.URL), cmp(notes, d.Notes)
		d.Status = status
		if status != DeployStarted {
			f := msTime(now)
			d.FinishedAt = &f
		}
		var fin any
		if d.FinishedAt != nil {
			fin = d.FinishedAt.UnixMilli()
		}
		if _, err := t.ExecContext(ctx, `UPDATE deployments SET commit_sha = ?, branch = ?, version = ?, status = ?, finished_at = ?, ci_url = ?, notes = ? WHERE id = ?`,
			d.Commit, d.Branch, d.Version, d.Status, fin, d.URL, d.Notes, d.ID); err != nil {
			return nil, err
		}
	} else {
		d = &Deployment{EnvironmentID: e.ID, Commit: commit, Branch: branch, Version: version, Status: status, StartedAt: msTime(now),
			ByKind: by, Agent: agent, URL: ci, Notes: notes}
		var fin any
		if status != DeployStarted {
			f := msTime(now)
			d.FinishedAt = &f
			fin = now
		}
		res, err := t.ExecContext(ctx, `INSERT INTO deployments(environment_id, commit_sha, branch, version, status, started_at, finished_at, by_kind, by_user, by_agent, ci_url, notes)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, e.ID, commit, branch, version, status, now, fin, by, byUser, agent, ci, notes)
		if err != nil {
			return nil, err
		}
		d.ID, _ = res.LastInsertId()
	}
	if t.actor != nil && t.actor.User != nil && t.actor.User.ID != 0 && d.By == nil {
		r := t.actor.User.Ref()
		d.By = &r
	}

	revived := e.Stopped
	if revived || e.TTLS > 0 || status == DeploySucceeded {
		var expires any
		if e.TTLS > 0 {
			expires = s.now().Add(time.Duration(e.TTLS) * time.Second).UnixMilli()
		}
		if status == DeploySucceeded {
			newCommit, newBranch, newVersion := d.Commit, d.Branch, d.Version
			if d.Commit != "" && sameCommit(d.Commit, e.Commit) {
				newCommit, newBranch, newVersion = longer(d.Commit, e.Commit), cmp(newBranch, e.Branch), cmp(newVersion, e.Version)
			}
			e.Commit, e.Branch, e.Version = newCommit, newBranch, newVersion
			if _, err := t.ExecContext(ctx, `UPDATE environments SET commit_sha = ?, branch = ?, version = ?, deployed_at = ?, mismatch = 0, updated_at = ? WHERE id = ?`,
				e.Commit, e.Branch, e.Version, now, now, e.ID); err != nil {
				return nil, err
			}
		}
		if e.TTLS > 0 || revived {
			if _, err := t.ExecContext(ctx, `UPDATE environments SET expires_at = ?, stopped_at = NULL, updated_at = ?, next_check_at = CASE WHEN ? THEN NULL ELSE next_check_at END WHERE id = ?`,
				expires, now, revived, e.ID); err != nil {
				return nil, err
			}
		}
		if revived {
			t.emit(envEvent(EvEnvCreated, e, map[string]any{"restarted": true}))
		}
	}

	extra := map[string]any{"deployment": d.ID, "commit": d.Commit, "branch": d.Branch, "version": d.Version, "status": d.Status}
	switch d.Status {
	case DeploySucceeded:
		t.emit(envEvent(EvEnvDeployed, e, extra))
	case DeployFailed:
		t.emit(envEvent(EvEnvDeployFailed, e, extra))
	default:
		t.emit(envEvent(EvEnvUpdated, e, extra))
	}
	return d, nil
}

func longer(a, b string) string {
	if len(b) > len(a) {
		return b
	}
	return a
}
