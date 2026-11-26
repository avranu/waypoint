package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/sandbox"
	"git.jmann.me/claude/waypoint/internal/sandbox/probe"
)

// JobSandboxDrill is the drill's runner job kind.
const JobSandboxDrill = "sandbox_drill"

// FeatureSandboxDrill: the runner's wp runs the drill (Linux only).
const FeatureSandboxDrill = "sandbox_drill"

// sandboxDrillSource is the drill job's source; its source_id is the
// drill's.
const sandboxDrillSource = "sandbox_drill"

// EvSandboxDrill: a drill ended. Data: drill, runner, name, runner_owner,
// state, escaped, failed_controls, inconclusive, error, again (the last
// drill on the machine didn't finish either).
const EvSandboxDrill = "sandbox.drill_finished"

const (
	sandboxDrillEvery = 7 * 24 * time.Hour
	// sandboxDrillRetry: a drill that couldn't run goes again after this.
	sandboxDrillRetry = 24 * time.Hour
	// sandboxDrillTimeout bounds the job: the probe session's own limit,
	// the two short ones and room to start them.
	sandboxDrillTimeout = 15 * 60
	// sandboxDrillSpecLife: the specs are used one after another, the last
	// up to the job's timeout after the claim.
	sandboxDrillSpecLife = 30 * time.Minute
	// sandboxDrillCases bounds what a runner can make Waypoint store.
	sandboxDrillCases = 500
)

// Drill states.
const (
	DrillQueued  = "queued"
	DrillRunning = "running"
	DrillPassed  = "passed"
	DrillFailed  = "failed"
	DrillError   = "error"
)

// SandboxDrill is one drill on a machine.
type SandboxDrill struct {
	ID            int64        `json:"id"`
	JobID         int64        `json:"job_id"`
	Reason        string       `json:"reason"`
	StartedBy     *UserRef     `json:"started_by,omitempty"`
	HelperVersion string       `json:"helper_version"`
	RepoUser      string       `json:"repo_user,omitempty"`
	State         string       `json:"state"`
	Passed        int          `json:"passed"`
	Escaped       int          `json:"escaped"`
	Controls      int          `json:"failed_controls"`
	Inconclusive  int          `json:"inconclusive"`
	Cases         []probe.Case `json:"cases"`
	Error         string       `json:"error,omitempty"`
	CreatedAt     time.Time    `json:"created_at"`
	FinishedAt    *time.Time   `json:"finished_at,omitempty"`
	// NotCovered are the escape items the drill doesn't try yet.
	NotCovered []string `json:"not_covered"`
}

// drillUser is the machine user a drill's sessions run as: a repo user the
// helper reported present, the most recently used. A machine with none
// can't run a session yet.
func drillUser(h *SandboxHelper) *sandbox.User {
	if h == nil || h.Status == nil {
		return nil
	}
	var best *sandbox.User
	for i := range h.Status.Users {
		u := &h.Status.Users[i]
		if u.Kind == sandbox.KindRepo && u.Present && (best == nil || u.LastUsed > best.LastUsed) {
			best = u
		}
	}
	return best
}

// drillReady says why a machine can't run a drill now, "" when it can.
func (s *Service) drillReady(ctx context.Context, q queryer, r *Runner) (*SandboxHelper, string, error) {
	if !slices.Contains(r.Features, FeatureSandboxDrill) {
		return nil, "its runner's wp can't run the drill (update wp; Linux only)", nil
	}
	h, err := s.sandboxHelper(ctx, q, r.ID)
	if err != nil {
		return nil, "", err
	}
	if h == nil || !h.Ready {
		why := "it never reported one"
		if h != nil {
			why = h.Problem
		}
		return h, "its sandbox helper isn't ready: " + why, nil
	}
	if paused, err := s.sandboxPaused(ctx, q, r.ID); err != nil || paused {
		return h, "its sandboxing is paused", err
	}
	if drillUser(h) == nil {
		return h, "it has no repo user yet: one is made when a project's job first runs there", nil
	}
	return h, "", nil
}

// lastSandboxDrill is a machine's newest drill, nil when none.
func (s *Service) lastSandboxDrill(ctx context.Context, q queryer, runnerID int64, full bool) (*SandboxDrill, error) {
	var d SandboxDrill
	var by, fin sql.NullInt64
	var at int64
	var cases string
	err := q.QueryRowContext(ctx, `SELECT id, job_id, reason, started_by, helper_version, repo_user, state, passed, escaped, failed_controls, inconclusive, cases, error, created_at, finished_at
		FROM sandbox_drills WHERE runner_id = ? ORDER BY id DESC LIMIT 1`, runnerID).
		Scan(&d.ID, &d.JobID, &d.Reason, &by, &d.HelperVersion, &d.RepoUser, &d.State, &d.Passed, &d.Escaped, &d.Controls, &d.Inconclusive, &cases, &d.Error, &at, &fin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d.CreatedAt, d.FinishedAt = msTime(at), msTimePtr(fin)
	d.Cases = []probe.Case{}
	if full {
		d.StartedBy = s.userRefByID(ctx, q, by)
		_ = json.Unmarshal([]byte(cases), &d.Cases)
		d.NotCovered = probe.NotCovered
	}
	return &d, nil
}

// drillDue says why a machine's drill is due, "" when it isn't.
func drillDue(last *SandboxDrill, helperVersion string, now time.Time) string {
	switch {
	case last == nil:
		return "first"
	case last.State == DrillQueued || last.State == DrillRunning:
		return ""
	case last.HelperVersion != helperVersion:
		return "helper"
	case last.State == DrillError && !now.Before(last.CreatedAt.Add(sandboxDrillRetry)):
		return "retry"
	case !now.Before(last.CreatedAt.Add(sandboxDrillEvery)):
		return "weekly"
	}
	return ""
}

// maybeSandboxDrills (hourly) queues a drill on each ready machine that is
// due one.
func (s *Service) maybeSandboxDrills(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM runners ORDER BY id`)
	if err != nil {
		s.log.WarnContext(ctx, "sandbox drill", "err", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		r, err := s.runnerByID(ctx, s.db.R, id)
		if err != nil {
			continue
		}
		h, why, err := s.drillReady(ctx, s.db.R, r)
		if err != nil {
			s.log.WarnContext(ctx, "sandbox drill", "runner", r.ID, "err", err)
			continue
		}
		if why != "" {
			continue
		}
		last, err := s.lastSandboxDrill(ctx, s.db.R, r.ID, false)
		if err != nil {
			s.log.WarnContext(ctx, "sandbox drill", "runner", r.ID, "err", err)
			continue
		}
		if reason := drillDue(last, h.Status.Version, s.now()); reason != "" {
			if _, err := s.queueSandboxDrill(ctx, SystemActor(), r, h, reason); err != nil {
				s.log.WarnContext(ctx, "sandbox drill", "runner", r.ID, "err", err)
			}
		}
	}
}

// StartSandboxDrill runs a drill on a machine now: its owner or an admin,
// in the browser.
func (s *Service) StartSandboxDrill(ctx context.Context, actor *Actor, runnerID int64) (*SandboxDrill, error) {
	if !actor.inBrowser() {
		return nil, ErrSandboxUI
	}
	r, err := s.runnerByID(ctx, s.db.R, runnerID)
	if err != nil {
		return nil, err
	}
	if !actor.IsAdmin() && r.Owner.ID != actor.ID() {
		return nil, notFound("runner")
	}
	h, why, err := s.drillReady(ctx, s.db.R, r)
	if err != nil {
		return nil, err
	}
	if why != "" {
		return nil, invalid("%s can't run the escape drill: %s", r.Name, why)
	}
	last, err := s.lastSandboxDrill(ctx, s.db.R, r.ID, false)
	if err != nil {
		return nil, err
	}
	if last != nil && (last.State == DrillQueued || last.State == DrillRunning) {
		return nil, invalid("a drill on %s is already %s", r.Name, last.State)
	}
	id, err := s.queueSandboxDrill(ctx, actor, r, h, "person")
	if err != nil {
		return nil, err
	}
	d, err := s.lastSandboxDrill(ctx, s.db.R, r.ID, true)
	if err == nil && (d == nil || d.ID != id) {
		err = errors.New("the drill went missing")
	}
	return d, err
}

// queueSandboxDrill records a drill and queues its job for the machine's
// runner, as the runner's owner's job.
func (s *Service) queueSandboxDrill(ctx context.Context, actor *Actor, r *Runner, h *SandboxHelper, reason string) (int64, error) {
	var by any
	if actor.Person() {
		by = actor.ID()
	}
	var id int64
	err := s.write(ctx, actor, func(t *tx) error {
		now := s.nowMS()
		if err := t.QueryRowContext(ctx, `INSERT INTO sandbox_drills(runner_id, reason, started_by, helper_version, created_at) VALUES (?,?,?,?,?) RETURNING id`,
			r.ID, reason, by, h.Status.Version, now).Scan(&id); err != nil {
			return err
		}
		input, _ := json.Marshal(map[string]int64{"drill_id": id})
		res, err := t.ExecContext(ctx, `INSERT INTO runner_jobs(owner_id, created_by, target_runner_id, kind, title, input, timeout, source, source_id, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`, r.Owner.ID, by, r.ID, JobSandboxDrill, "Sandbox escape drill on "+r.Name, string(input), sandboxDrillTimeout, sandboxDrillSource, id, now)
		if err != nil {
			return err
		}
		job, _ := res.LastInsertId()
		if _, err := t.ExecContext(ctx, `INSERT INTO runner_job_logs(job_id) VALUES (?)`, job); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `UPDATE sandbox_drills SET job_id = ? WHERE id = ?`, job, id); err != nil {
			return err
		}
		t.emit(jobUpdated(&RunnerClaim{ID: job, Kind: JobSandboxDrill}, "queued", r.ID, r.Owner.ID, 0))
		return nil
	})
	if err != nil {
		return 0, err
	}
	s.kickRunners()
	return id, nil
}

// drillSessions signs the drill's three start specs at its claim, as the
// machine's most recently used repo user. Nil when it can't: the runner
// then fails the job, and the drill records why.
func (s *Service) drillSessions(ctx context.Context, t *tx, sg *specSigner, r *Runner, c *RunnerClaim, drillID int64) []*sandbox.Signed {
	fail := func(why string) []*sandbox.Signed {
		s.log.WarnContext(ctx, "sandbox drill specs", "job", c.ID, "why", why)
		return nil
	}
	if sg == nil {
		return fail("the machine's helper isn't ready, or its sandboxing is paused")
	}
	h, err := s.sandboxHelper(ctx, t, r.ID)
	if err != nil {
		return fail(err.Error())
	}
	u := drillUser(h)
	if u == nil {
		return fail("no repo user")
	}
	if _, err := t.ExecContext(ctx, `UPDATE sandbox_drills SET state = ?, repo_user = ? WHERE id = ? AND state = ?`, DrillRunning, u.Name, drillID, DrillQueued); err != nil {
		return fail(err.Error())
	}
	now := s.now()
	var out []*sandbox.Signed
	for _, ses := range probe.Sessions(c.ID) {
		sd, err := sandbox.Sign(sg.priv, &sandbox.Spec{
			Op: sandbox.OpStart, Nonce: sandbox.NewNonce(),
			Issued: now.Unix(), Expiry: now.Add(sandboxDrillSpecLife).Unix(),
			Runner: sg.runner, Job: c.ID, Level: sandbox.LevelContained,
			Repos:   []sandbox.Repo{{ID: u.Key, Slug: u.Slug}},
			Session: &ses,
		})
		if err != nil {
			return fail(err.Error())
		}
		out = append(out, sd)
	}
	return out
}

// finishSandboxDrill records how a drill's job ended: from its result when
// it ran, else why it didn't.
func (t *tx) finishSandboxDrill(ctx context.Context, drillID int64, state string, result json.RawMessage, msg string) error {
	var runnerID int64
	var cur string
	err := t.QueryRowContext(ctx, `SELECT runner_id, state FROM sandbox_drills WHERE id = ?`, drillID).Scan(&runnerID, &cur)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if cur != DrillQueued && cur != DrillRunning {
		return nil
	}
	var res probe.Result
	if len(result) > 0 {
		_ = json.Unmarshal(result, &res)
	}
	for i := range res.Cases {
		c := &res.Cases[i]
		c.Name, c.Group, c.Want, c.Got, c.Detail = clip(c.Name, 200), clip(c.Group, 20), clip(c.Want, 20), clip(c.Got, 20), clip(c.Detail, maxReportText)
	}
	sum := probe.Summarize(res.Cases)
	missing := probe.Missing(res.Cases)
	reported := len(res.Cases)
	if reported > sandboxDrillCases {
		res.Cases = res.Cases[:sandboxDrillCases]
	}
	drill, why := DrillError, ""
	switch {
	case sum.Escaped > 0 || sum.Controls > 0:
		drill = DrillFailed
	case state != "done":
		why = cmp(strings.TrimSpace(msg), "the job "+state)
	case reported == 0:
		why = "the runner reported no tries"
	case len(missing) > 0:
		why = "it stopped part way: no " + strings.Join(missing, ", ")
	case sum.OK():
		drill = DrillPassed
	default:
		why = fmt.Sprintf("%d tries couldn't be judged", sum.Inconclusive)
	}
	if res.Cases == nil {
		res.Cases = []probe.Case{}
	}
	cases, _ := json.Marshal(res.Cases)
	if _, err := t.ExecContext(ctx, `UPDATE sandbox_drills SET state = ?, passed = ?, escaped = ?, failed_controls = ?, inconclusive = ?, cases = ?, error = ?, finished_at = ? WHERE id = ?`,
		drill, sum.Passed, sum.Escaped, sum.Controls, sum.Inconclusive, string(cases), clip(why, maxReportText), t.s.nowMS(), drillID); err != nil {
		return err
	}
	var prev []string
	rows, err := t.QueryContext(ctx, `SELECT state FROM sandbox_drills WHERE runner_id = ? AND id < ? ORDER BY id DESC LIMIT 2`, runnerID, drillID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var st string
		if rows.Scan(&st) == nil {
			prev = append(prev, st)
		}
	}
	rows.Close()
	again := drill == DrillError && len(prev) > 0 && prev[0] == DrillError && (len(prev) == 1 || prev[1] != DrillError)
	var name string
	var owner int64
	_ = t.QueryRowContext(ctx, `SELECT name, owner_id FROM runners WHERE id = ?`, runnerID).Scan(&name, &owner)
	t.emit(&Event{Kind: EvSandboxDrill, AdminOnly: true, Data: map[string]any{"drill": drillID, "runner": runnerID, "name": name, "runner_owner": owner,
		"state": drill, "escaped": sum.Escaped, "failed_controls": sum.Controls, "inconclusive": sum.Inconclusive, "error": why, "again": again}})
	return nil
}

// notifySandboxDrill tells the admins and the runner's owner when a drill
// failed, or couldn't run twice in a row (PL7 §12: kind sandbox).
func (s *Service) notifySandboxDrill(ctx context.Context, e *Event) {
	state, _ := e.Data["state"].(string)
	name, _ := e.Data["name"].(string)
	var title, body string
	switch {
	case state == DrillFailed:
		title = "Sandbox escape drill failed on " + name
		var parts []string
		if n := dataInt(e, "escaped"); n > 0 {
			parts = append(parts, fmt.Sprintf("Tries the sandbox must refuse that got through: %d", n))
		}
		if n := dataInt(e, "failed_controls"); n > 0 {
			parts = append(parts, fmt.Sprintf("Things a session must be able to do that were refused: %d", n))
		}
		body = strings.Join(parts, ". ") + ". Sessions there may not be contained: see the drill, and pause the machine's sandboxing or its runner until it's fixed."
	case state == DrillError && e.Data["again"] == true:
		why, _ := e.Data["error"].(string)
		title = "Sandbox escape drill can't run on " + name
		body = "Twice in a row: " + why + ". Until it runs, nothing checks that sessions there are contained."
	default:
		return
	}
	owner := dataInt(e, "runner_owner")
	seen := map[int64]bool{0: true}
	if !seen[owner] {
		seen[owner] = true
		s.notifyURL(ctx, owner, nil, NotifySandbox, title, body, "/settings?tab=runners")
	}
	for _, id := range s.userIDsWhere(ctx, `SELECT id FROM users WHERE is_admin = 1 AND disabled = 0`) {
		if !seen[id] {
			seen[id] = true
			s.notifyURL(ctx, id, nil, NotifySandbox, title, body, "/admin?tab=runners")
		}
	}
}
