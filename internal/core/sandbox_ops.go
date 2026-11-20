package core

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/sandbox"
)

// Sandbox helper actions (WP-448, WP-ADR28, PL7 §5.3): from Settings →
// Runners → Sandbox a person removes a machine user or has the helper
// finish what a crash left half done. Waypoint queues the action; the
// machine's runner picks it up on its next poll, with a spec signed then,
// hands it to its helper and reports the outcome on the poll after. Like
// the policy, only a person in the browser asks for one.

// EvSandboxOp: a person asked a machine's sandbox helper for an action.
// Data: runner (id), name, runner_owner, op, user.
const EvSandboxOp = "sandbox.op_requested"

// Sandbox action kinds.
const (
	SandboxRemoveUser = sandbox.OpRemoveUser
	SandboxReconcile  = sandbox.OpReconcile
)

const (
	// sandboxOpResend: a runner that took an action but never answered
	// gets it again, freshly signed, after this.
	sandboxOpResend = 5 * time.Minute
	// sandboxOpExpiry: an action no runner finished by then failed (its
	// wp may be older than this server, or the machine is off).
	sandboxOpExpiry = time.Hour
	// sandboxOpsShown: the actions a machine lists.
	sandboxOpsShown = 10
)

// SandboxOp is an action a person asked of a machine's helper.
type SandboxOp struct {
	ID          int64      `json:"id"`
	Op          string     `json:"op"`
	User        string     `json:"user,omitempty"`
	RequestedBy *UserRef   `json:"requested_by,omitempty"`
	RequestedAt time.Time  `json:"requested_at"`
	SentAt      *time.Time `json:"sent_at,omitempty"`
	DoneAt      *time.Time `json:"done_at,omitempty"`
	Error       string     `json:"error,omitempty"`
	// State is waiting (for the runner), sent, done or failed.
	State string `json:"state"`
}

// SandboxOpInput asks for an action; User names the machine user to
// remove.
type SandboxOpInput struct {
	Op   string `json:"op"`
	User string `json:"user"`
}

// RunnerSandboxOp is an action handed to a runner with its signed spec.
type RunnerSandboxOp struct {
	ID   int64           `json:"id"`
	Op   string          `json:"op"`
	Spec *sandbox.Signed `json:"spec"`
}

// RunnerSandboxResult is how an action went, from the runner.
type RunnerSandboxResult struct {
	ID    int64  `json:"id"`
	Error string `json:"error"`
}

// SandboxUserInfo is what Waypoint knows of a machine user beyond the
// helper's report.
type SandboxUserInfo struct {
	// LastJob is the last job whose spec named the user's repository.
	LastJob *SandboxLastJob `json:"last_job,omitempty"`
	// Linked: a project still links the user's repository (or, for an org
	// user, one of the owner's).
	Linked bool `json:"linked"`
}

// SandboxLastJob is a runner job, by id and title.
type SandboxLastJob struct {
	ID    int64     `json:"id"`
	Title string    `json:"title"`
	At    time.Time `json:"at"`
}

// RequestSandboxOp queues an action for a runner machine's helper: its
// owner or an admin, in the browser.
func (s *Service) RequestSandboxOp(ctx context.Context, actor *Actor, runnerID int64, in SandboxOpInput) (*SandboxOp, error) {
	if !actor.inBrowser() {
		return nil, ErrSandboxUI
	}
	r, err := s.runnerByID(ctx, s.db.R, runnerID)
	if err != nil {
		return nil, err
	}
	if !actor.isAdmin() && r.Owner.ID != actor.id() {
		return nil, notFound("runner")
	}
	h, err := s.sandboxHelper(ctx, s.db.R, r.ID)
	if err != nil {
		return nil, err
	}
	if h == nil || !h.Ready {
		why := "it never reported one"
		if h != nil {
			why = h.Problem
		}
		return nil, invalid("%s's sandbox helper can't take actions: %s", r.Name, why)
	}
	var kind, key string
	switch in.Op {
	case SandboxRemoveUser:
		i := slices.IndexFunc(h.Status.Users, func(u sandbox.User) bool { return u.Name == in.User })
		if i < 0 {
			return nil, invalid("%s has no machine user %q", r.Name, in.User)
		}
		kind, key = h.Status.Users[i].Kind, h.Status.Users[i].Key
	case SandboxReconcile:
		in.User = ""
	default:
		return nil, invalid("unknown sandbox action %q", in.Op)
	}
	var id int64
	err = s.write(ctx, actor, func(t *tx) error {
		// The same action still pending is the one asked for.
		err := t.QueryRowContext(ctx, `SELECT id FROM sandbox_ops WHERE runner_id = ? AND op = ? AND user_name = ? AND done_at IS NULL`,
			r.ID, in.Op, in.User).Scan(&id)
		if err == nil || !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := t.QueryRowContext(ctx, `INSERT INTO sandbox_ops(runner_id, op, user_name, user_kind, user_key, requested_by, requested_at)
			VALUES (?,?,?,?,?,?,?) RETURNING id`, r.ID, in.Op, in.User, kind, key, actor.id(), s.nowMS()).Scan(&id); err != nil {
			return err
		}
		t.emit(&Event{Kind: EvSandboxOp, AdminOnly: true, Data: map[string]any{"runner": r.ID, "name": r.Name,
			"runner_owner": r.Owner.ID, "op": in.Op, "user": in.User}})
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.kickRunners()
	ops, err := s.sandboxOps(ctx, s.db.R, r.ID, id)
	if err != nil || len(ops) == 0 {
		return nil, err
	}
	return ops[0], nil
}

// sandboxOps lists a runner's latest actions, or the one with id.
func (s *Service) sandboxOps(ctx context.Context, q queryer, runnerID, id int64) ([]*SandboxOp, error) {
	query := `SELECT id, op, user_name, requested_by, requested_at, sent_at, done_at, error FROM sandbox_ops WHERE runner_id = ?`
	args := []any{runnerID}
	if id > 0 {
		query, args = query+` AND id = ?`, append(args, id)
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY id DESC LIMIT `+strconv.Itoa(sandboxOpsShown), args...)
	if err != nil {
		return nil, err
	}
	type row struct {
		op           SandboxOp
		by           sql.NullInt64
		at           int64
		sent, doneAt sql.NullInt64
	}
	var raw []row
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.op.ID, &x.op.Op, &x.op.User, &x.by, &x.at, &x.sent, &x.doneAt, &x.op.Error); err != nil {
			rows.Close()
			return nil, err
		}
		raw = append(raw, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	expired := s.nowMS() - sandboxOpExpiry.Milliseconds()
	out := make([]*SandboxOp, 0, len(raw))
	for _, x := range raw {
		op := x.op
		op.RequestedBy = s.userRefByID(ctx, q, x.by)
		op.RequestedAt = msTime(x.at)
		op.SentAt, op.DoneAt = msTimePtr(x.sent), msTimePtr(x.doneAt)
		switch {
		case x.doneAt.Valid && op.Error != "":
			op.State = "failed"
		case x.doneAt.Valid:
			op.State = "done"
		case x.at < expired:
			op.State, op.Error = "failed", "The runner didn't take it within an hour: is it running, with a wp as new as this server?"
		case x.sent.Valid:
			op.State = "sent"
		default:
			op.State = "waiting"
		}
		out = append(out, &op)
	}
	return out, nil
}

func msTimePtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := msTime(v.Int64)
	return &t
}

// sandboxOpsFor hands a polling runner the actions waiting for it, each
// with a spec signed now; nil when there are none.
func (s *Service) sandboxOpsFor(ctx context.Context, actor *Actor, r *Runner) ([]RunnerSandboxOp, error) {
	now := s.nowMS()
	resend, expired := now-sandboxOpResend.Milliseconds(), now-sandboxOpExpiry.Milliseconds()
	pending := `runner_id = ? AND done_at IS NULL AND requested_at >= ? AND (sent_at IS NULL OR sent_at < ?)`
	var n int
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM sandbox_ops WHERE `+pending, r.ID, expired, resend).Scan(&n); err != nil || n == 0 {
		return nil, err
	}
	priv, err := s.sandboxKey(ctx)
	if err != nil {
		return nil, err
	}
	var out []RunnerSandboxOp
	err = s.write(ctx, actor, func(t *tx) error {
		rows, err := t.QueryContext(ctx, `SELECT id, op, user_name, user_kind, user_key FROM sandbox_ops WHERE `+pending+` ORDER BY id LIMIT 5`, r.ID, expired, resend)
		if err != nil {
			return err
		}
		type pend struct {
			id                  int64
			op, name, kind, key string
		}
		var ps []pend
		for rows.Next() {
			var p pend
			if err := rows.Scan(&p.id, &p.op, &p.name, &p.kind, &p.key); err != nil {
				rows.Close()
				return err
			}
			ps = append(ps, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		at := s.now()
		for _, p := range ps {
			spec := &sandbox.Spec{Op: p.op, Nonce: sandbox.NewNonce(), Issued: at.Unix(), Expiry: at.Add(sandboxSpecLife).Unix(), Runner: r.ID}
			if p.op == SandboxRemoveUser {
				spec.User = &sandbox.Target{Name: p.name, Kind: p.kind, Key: p.key}
			}
			sd, err := sandbox.Sign(priv, spec)
			if err != nil {
				return err
			}
			if _, err := t.ExecContext(ctx, `UPDATE sandbox_ops SET sent_at = ? WHERE id = ?`, now, p.id); err != nil {
				return err
			}
			out = append(out, RunnerSandboxOp{ID: p.id, Op: p.op, Spec: sd})
		}
		return nil
	})
	return out, err
}

// finishSandboxOps records what a runner said of its actions.
func (t *tx) finishSandboxOps(ctx context.Context, runnerID int64, results []RunnerSandboxResult) error {
	for _, res := range results[:min(len(results), 20)] {
		msg := clip(strings.TrimSpace(res.Error), maxReportText)
		if _, err := t.ExecContext(ctx, `UPDATE sandbox_ops SET done_at = ?, error = ? WHERE id = ? AND runner_id = ? AND done_at IS NULL`,
			t.s.nowMS(), msg, res.ID, runnerID); err != nil {
			return err
		}
	}
	return nil
}

// sandboxUserInfo is, for each machine user in a helper's report, its
// last job and whether a project still links its repository.
func (s *Service) sandboxUserInfo(ctx context.Context, q queryer, runnerID int64, users []sandbox.User) (map[string]*SandboxUserInfo, error) {
	if len(users) == 0 {
		return nil, nil
	}
	repos, owners := map[string]bool{}, map[string]bool{}
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT forge_id, forge_repo_id, repo FROM project_repos WHERE forge_repo_id > 0`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var forgeID, repoID int64
		var slug string
		if err := rows.Scan(&forgeID, &repoID, &slug); err != nil {
			rows.Close()
			return nil, err
		}
		f := strconv.FormatInt(forgeID, 10)
		repos[f+"/"+strconv.FormatInt(repoID, 10)] = true
		if owner, _, ok := strings.Cut(slug, "/"); ok {
			owners[f+"/"+strings.ToLower(owner)] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	jobs := map[string]*SandboxLastJob{}
	rows, err = q.QueryContext(ctx, `SELECT u.kind, u.user_key, u.job_id, u.at, COALESCE(j.title, '') FROM sandbox_user_jobs u
		LEFT JOIN runner_jobs j ON j.id = u.job_id WHERE u.runner_id = ?`, runnerID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var kind, key, title string
		var id, at int64
		if err := rows.Scan(&kind, &key, &id, &at, &title); err != nil {
			rows.Close()
			return nil, err
		}
		jobs[kind+" "+key] = &SandboxLastJob{ID: id, Title: title, At: msTime(at)}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]*SandboxUserInfo, len(users))
	for _, u := range users {
		linked := repos[u.Key]
		if u.Kind == sandbox.KindOrg {
			linked = owners[u.Key]
		}
		out[u.Name] = &SandboxUserInfo{LastJob: jobs[u.Kind+" "+u.Key], Linked: linked}
	}
	return out, nil
}
