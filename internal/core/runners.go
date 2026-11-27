package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"git.jmann.me/claude/waypoint/internal/sandbox"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

const (
	runnerOnlineFor = 90 * time.Second
	runnerMaxWait   = 30 * time.Second
	runnerLostGrace = 20 * time.Second
	// A job Waypoint ended that its runner still lists as running is one the
	// runner wasn't told about (it prints nothing, so no log upload carried the
	// word): it is told to stop after this. Longer than a held poll, whose
	// list is as old as the poll, so a job that ended in the meantime isn't
	// taken for one (WP-870).
	runnerEndedGrace  = runnerMaxWait + 15*time.Second
	runnerEndedMax    = 200
	runnerExpireAfter = 5 * time.Minute
	runnerQueuedFor   = 24 * time.Hour
	runnerKeepJobs    = 30 * 24 * time.Hour
	maxRunnersPerUser = 50
)

// Names of the coding-agent CLIs a runner can report (AgentJob.CLI).
var runnerCLIs = []string{"claude", "codex", "opencode", "cursor"}

// RunnerCLI is an agent CLI found on a runner's machine.
type RunnerCLI struct {
	Name     string `json:"name"`
	Version  string `json:"version,omitempty"`
	SignedIn *bool  `json:"signed_in"` // nil: the CLI can't say
	Detail   string `json:"detail,omitempty"`
}

// Runner is one machine's `wp runner`.
type Runner struct {
	ID          int64       `json:"id"`
	Owner       UserRef     `json:"owner"`
	Name        string      `json:"name"`
	Hostname    string      `json:"hostname"`
	OS          string      `json:"os"`
	Arch        string      `json:"arch"`
	Version     string      `json:"version"`
	Labels      []string    `json:"labels"`
	AllowedDirs []string    `json:"allowed_dirs"`
	CLIs        []RunnerCLI `json:"clis"`
	Features    []string    `json:"features"` // what its wp reports it can do: "confine", "autopilot"
	MaxJobs     int         `json:"max_jobs"`
	Online      bool        `json:"online"`
	LastSeenAt  *time.Time  `json:"last_seen_at"`
	Running     int         `json:"running"` // jobs on it now
	// Capacity is what it last said about its machine's load (WP-366).
	Capacity  *RunnerCapacity `json:"capacity,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	online    bool
}

// RunnerRegistration is what a runner reports about itself.
type RunnerRegistration struct {
	Name        string      `json:"name"`
	Hostname    string      `json:"hostname"`
	OS          string      `json:"os"`
	Arch        string      `json:"arch"`
	Version     string      `json:"version"`
	Labels      []string    `json:"labels"`
	AllowedDirs []string    `json:"allowed_dirs"`
	CLIs        []RunnerCLI `json:"clis"`
	Features    []string    `json:"features"`
	MaxJobs     int         `json:"max_jobs"`
}

// Features a runner's wp may report at registration.
const FeatureConfine = "confine"

// FeatureAutopilot: the runner's wp runs autopilot jobs (WP-ADR9).
const FeatureAutopilot = "autopilot"

// FeatureAutopilotStart: the runner's wp can start a session of autopilot's
// own in tmux, and tmux is there (WP-ADR72).
const FeatureAutopilotStart = "autopilot_start"

// FeatureAudit: the runner's wp runs audit jobs, with the change's diff in
// the prompt (WP-348), a period's start found from a time and the diff
// limited to paths (WP-548). A wp from before WP-548 says "audit" and would
// read a recent-changes audit as the whole repository, so it gets none.
const FeatureAudit = "audit-scope"

// FeatureAutopilotReview: the runner's wp runs autopilot's adversarial
// review rounds (compact, then the review prompt) (WP-285).
const FeatureAutopilotReview = "autopilot_review"

// FeatureRequestLane: the runner's wp runs a request's jobs in a lane of
// their own, which its load governor never holds (LaneRequest, WP-503).
const FeatureRequestLane = "request_lane"

// FeatureMessageLane: the runner's wp types inbox messages in a lane of
// their own, which shares its slots with nothing and its load governor
// never holds (LaneMessage, WP-886).
const FeatureMessageLane = "message_lane"

// FeatureAgentLane: the runner's wp runs agent jobs (JobAgent) in a lane of
// their own, ceilinged by --max-agents, and counts one toward its heavy limit
// only while a command of it holds a test slot (LaneAgent, WP-1915).
const FeatureAgentLane = "agent_lane"

// FeatureLightLane: the runner's wp runs reviews and audits in a lane of
// their own, ceilinged at LightLaneSlots and stopped only by short memory,
// beside its heavy jobs rather than sharing --max-jobs and the load governor
// with them (LaneLight, WP-1943).
const FeatureLightLane = "light_lane"

// FeatureGatewayAudit: the runner's wp runs an audit whose model is the
// LLM gateway's (GatewayCLI): it sends its checkout through Waypoint to an
// agent task there, and needs no CLI of its own for it (WP-617). A wp from
// before WP-1187 says "gateway_audit" and knows the entry by its old name,
// so it gets none.
const FeatureGatewayAudit = "gateway_agent"

// FeatureGatewayLane: the runner's wp runs audits through the gateway in a
// lane of their own, beside --max-jobs, which its load governor never holds
// (LaneGateway, WP-1064).
const FeatureGatewayLane = "gateway_lane"

// FeatureTaskKickoff: the runner's wp starts a task session with the task's
// wp next answer as its first prompt, made as autopilot makes its own
// (SessionJob.Kickoff, WP-1037).
const FeatureTaskKickoff = "task_kickoff"

// FeatureWorkClock: the runner's wp stops an agent job at its work limit
// (AgentJob.WorkTimeout), leaving its commands' waits for a test slot out.
// One without it gets the work limit as the job's whole limit at the claim,
// as before WP-1618.
const FeatureWorkClock = "work_clock"

var runnerFeatures = []string{FeatureWorkClock, FeatureConfine, FeatureAutopilot, FeatureAutopilotStart, FeatureAudit, FeatureAutopilotReview, FeatureRequestLane, FeatureMessageLane, FeatureGatewayAudit, FeatureGatewayLane, FeatureSandboxDrill, FeatureTaskKickoff, FeatureObjective, FeatureAgentLane, FeatureLightLane}

// runnerState is the Service's in-memory part of runners: a broadcast that
// wakes held polls, and when each job's last live update went out.
type runnerState struct {
	mu       sync.Mutex
	wake     chan struct{}
	lastEmit map[int64]time.Time
	pruned   time.Time
	capacity map[int64]RunnerCapacity
}

// kickRunners wakes every held poll to look for work again.
func (s *Service) kickRunners() {
	s.runners.mu.Lock()
	if s.runners.wake != nil {
		close(s.runners.wake)
		s.runners.wake = nil
	}
	s.runners.mu.Unlock()
}

// runnerWake is the channel the next kick closes.
func (s *Service) runnerWake() <-chan struct{} {
	s.runners.mu.Lock()
	defer s.runners.mu.Unlock()
	if s.runners.wake == nil {
		s.runners.wake = make(chan struct{})
	}
	return s.runners.wake
}

var winDrive = lazyre.New(`^[A-Za-z]:[\\/]`)

// cleanDir normalises an absolute directory a runner reports or a job names:
// slash separated, no "..", no trailing slash, Windows drive letters upper
// case. ok is false for anything relative, odd or empty.
func cleanDir(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" || len(p) > 512 || strings.ContainsRune(p, 0) {
		return "", false
	}
	win := winDrive.MatchString(p)
	if win {
		p = strings.ReplaceAll(p, `\`, "/")
	} else if !strings.HasPrefix(p, "/") {
		return "", false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", false
		}
	}
	c := path.Clean(p)
	if win {
		c = strings.ToUpper(c[:1]) + c[1:]
		if len(c) == 2 {
			c += "/"
		}
	}
	return c, true
}

// isRootDir: "/" or "C:/", which no one may allow whole.
func isRootDir(c string) bool { return c == "/" || (winDrive.MatchString(c) && len(c) == 3) }

// dirInside reports whether dir is allowed or below it (both cleaned).
func dirInside(dir, allowed string) bool {
	win := winDrive.MatchString(dir)
	if win != winDrive.MatchString(allowed) {
		return false
	}
	if win {
		dir, allowed = strings.ToLower(dir), strings.ToLower(allowed)
	}
	if dir == allowed {
		return true
	}
	return strings.HasPrefix(dir, strings.TrimRight(allowed, "/")+"/")
}

func dirAllowed(dir string, allowed []string) bool {
	return slices.ContainsFunc(allowed, func(a string) bool { return dirInside(dir, a) })
}

var labelRE = lazyre.New(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,39}$`)

func cleanWord(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > n {
		s = string(r[:n])
	}
	return s
}

func (r *RunnerRegistration) clean() error {
	r.Name = cleanWord(r.Name, 60)
	if r.Name == "" {
		return invalid("a runner needs a name")
	}
	r.Hostname, r.OS, r.Arch, r.Version = cleanWord(r.Hostname, 100), cleanWord(r.OS, 40), cleanWord(r.Arch, 40), cleanWord(r.Version, 80)
	if len(r.Labels) > 20 {
		return invalid("a runner can have at most 20 labels")
	}
	labels := []string{}
	for _, l := range r.Labels {
		l = strings.TrimSpace(l)
		if !labelRE.MatchString(l) {
			return invalid("label %q: use letters, digits and . _ : - (up to 40)", l)
		}
		if !slices.Contains(labels, l) {
			labels = append(labels, l)
		}
	}
	r.Labels = labels
	if len(r.AllowedDirs) > 50 {
		return invalid("a runner can allow at most 50 directories")
	}
	dirs := []string{}
	for _, d := range r.AllowedDirs {
		c, ok := cleanDir(d)
		if !ok {
			return invalid("allowed directory %q must be an absolute path without ..", d)
		}
		if isRootDir(c) {
			return invalid("allow a projects folder, not the whole disk (%s)", c)
		}
		if !slices.Contains(dirs, c) {
			dirs = append(dirs, c)
		}
	}
	r.AllowedDirs = dirs
	clis := make([]RunnerCLI, 0, len(runnerCLIs))
	for _, c := range r.CLIs {
		if !slices.Contains(runnerCLIs, c.Name) || slices.ContainsFunc(clis, func(k RunnerCLI) bool { return k.Name == c.Name }) {
			continue
		}
		clis = append(clis, RunnerCLI{Name: c.Name, Version: cleanWord(c.Version, 80), SignedIn: c.SignedIn, Detail: cleanWord(c.Detail, 200)})
	}
	r.CLIs = clis
	feats := []string{}
	for _, f := range r.Features {
		if slices.Contains(runnerFeatures, f) && !slices.Contains(feats, f) {
			feats = append(feats, f)
		}
	}
	r.Features = feats
	if r.MaxJobs <= 0 {
		r.MaxJobs = 1
	}
	r.MaxJobs = min(r.MaxJobs, MaxRunnerJobs)
	return nil
}

// runnerActor checks that a person (not an agent, not a read-only token) is
// acting: runners run commands on a machine, so an agent can't be one.
func runnerActor(a *Actor) error {
	if err := a.CanWrite(); err != nil {
		return err
	}
	if a.User.ID == 0 || a.Automation != nil {
		return ErrForbidden
	}
	if a.IsAgent() {
		return invalid("a runner signs in as you, not as an agent: run wp auth login with --agent none")
	}
	return nil
}

const runnerCols = `r.id, r.owner_id, u.username, u.display_name, u.avatar_version, r.name, r.hostname, r.os, r.arch, r.version, r.labels, r.allowed_dirs, r.clis, r.features, r.max_jobs, r.online, r.last_seen_at, r.created_at,
	(SELECT count(*) FROM runner_jobs j WHERE j.runner_id = r.id AND j.state = 'running')`

func (s *Service) scanRunner(row interface{ Scan(...any) error }) (*Runner, error) {
	var r Runner
	var disp string
	var avatar int64
	var labels, dirs, clis, features string
	var seen sql.NullInt64
	var created int64
	if err := row.Scan(&r.ID, &r.Owner.ID, &r.Owner.Username, &disp, &avatar, &r.Name, &r.Hostname, &r.OS, &r.Arch, &r.Version, &labels, &dirs, &clis, &features, &r.MaxJobs, &r.online, &seen, &created, &r.Running); err != nil {
		return nil, err
	}
	r.Owner.DisplayName = disp
	if disp == "" {
		r.Owner.DisplayName = r.Owner.Username
	}
	r.Owner.AvatarURL = avatarURL(r.Owner.ID, avatar)
	r.Labels, r.AllowedDirs, r.CLIs = []string{}, []string{}, []RunnerCLI{}
	_ = json.Unmarshal([]byte(labels), &r.Labels)
	_ = json.Unmarshal([]byte(dirs), &r.AllowedDirs)
	_ = json.Unmarshal([]byte(clis), &r.CLIs)
	r.Features = []string{}
	_ = json.Unmarshal([]byte(features), &r.Features)
	r.CreatedAt = msTime(created)
	if seen.Valid {
		t := msTime(seen.Int64)
		r.LastSeenAt = &t
		r.Online = s.nowMS()-seen.Int64 < runnerOnlineFor.Milliseconds()
	}
	if r.Online {
		r.Capacity = s.capacityOf(r.ID)
	}
	return &r, nil
}

func (s *Service) runnerByID(ctx context.Context, q queryer, id int64) (*Runner, error) {
	r, err := s.scanRunner(q.QueryRowContext(ctx, `SELECT `+runnerCols+` FROM runners r JOIN users u ON u.id = r.owner_id WHERE r.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("runner")
	}
	return r, err
}

// RegisterRunner records the machine the caller's `wp runner` runs on (the
// same name for the same person is the same runner) and marks it online.
func (s *Service) RegisterRunner(ctx context.Context, actor *Actor, reg RunnerRegistration) (*Runner, error) {
	if err := actor.Need(PermRunnersHost); err != nil {
		return nil, err
	}
	if err := runnerActor(actor); err != nil {
		return nil, err
	}
	if err := reg.clean(); err != nil {
		return nil, err
	}
	labels, _ := json.Marshal(reg.Labels)
	dirs, _ := json.Marshal(reg.AllowedDirs)
	clis, _ := json.Marshal(reg.CLIs)
	features, _ := json.Marshal(reg.Features)
	var id int64
	err := s.write(ctx, actor, func(t *tx) error {
		now := s.nowMS()
		wasOnline := false
		err := t.QueryRowContext(ctx, `SELECT id, online FROM runners WHERE owner_id = ? AND name = ?`, actor.User.ID, reg.Name).Scan(&id, &wasOnline)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			var n int
			if err := t.QueryRowContext(ctx, `SELECT count(*) FROM runners WHERE owner_id = ?`, actor.User.ID).Scan(&n); err != nil {
				return err
			}
			if n >= maxRunnersPerUser {
				return invalid("you have %d runners already; remove some in Settings → Runners", n)
			}
			res, err := t.ExecContext(ctx, `INSERT INTO runners(owner_id, name, hostname, os, arch, version, labels, allowed_dirs, clis, features, max_jobs, online, last_seen_at, created_at)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,1,?,?)`, actor.User.ID, reg.Name, reg.Hostname, reg.OS, reg.Arch, reg.Version, string(labels), string(dirs), string(clis), string(features), reg.MaxJobs, now, now)
			if err != nil {
				return err
			}
			id, _ = res.LastInsertId()
		case err != nil:
			return err
		default:
			if _, err := t.ExecContext(ctx, `UPDATE runners SET hostname = ?, os = ?, arch = ?, version = ?, labels = ?, allowed_dirs = ?, clis = ?, features = ?, max_jobs = ?, online = 1, last_seen_at = ? WHERE id = ?`,
				reg.Hostname, reg.OS, reg.Arch, reg.Version, string(labels), string(dirs), string(clis), string(features), reg.MaxJobs, now, id); err != nil {
				return err
			}
		}
		if !wasOnline {
			t.emit(runnerStatus(id, reg.Name, actor.User.ID, true))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.kickRunners()
	return s.runnerByID(ctx, s.db.R, id)
}

func runnerStatus(id int64, name string, owner int64, online bool) *Event {
	return &Event{Kind: EvRunnerStatus, UserID: owner, Data: map[string]any{"runner": id, "name": name, "owner": owner, "online": online}}
}

// ListRunners lists the caller's runners; all (admins only) lists everyone's.
func (s *Service) ListRunners(ctx context.Context, actor *Actor, all bool) ([]*Runner, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	q, args := ` WHERE r.owner_id = ?`, []any{actor.User.ID}
	if all {
		if !actor.IsAdmin() {
			return nil, ErrForbidden
		}
		q, args = ``, nil
	}
	// #nosec G202 -- q is one of two fixed clauses; the owner is bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+runnerCols+` FROM runners r JOIN users u ON u.id = r.owner_id`+q+` ORDER BY r.name COLLATE NOCASE, r.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Runner{}
	for rows.Next() {
		r, err := s.scanRunner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRunner returns one runner: its owner's, or any for an admin.
func (s *Service) GetRunner(ctx context.Context, actor *Actor, id int64) (*Runner, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	r, err := s.runnerByID(ctx, s.db.R, id)
	if err != nil {
		return nil, err
	}
	if r.Owner.ID != actor.User.ID && !actor.IsAdmin() {
		return nil, notFound("runner")
	}
	return r, nil
}

// DeleteRunner removes a runner. What was waiting for it is cancelled and
// what it was running fails; finished jobs keep their record.
func (s *Service) DeleteRunner(ctx context.Context, actor *Actor, id int64) error {
	if err := actor.CanWrite(); err != nil {
		return err
	}
	if actor.IsAgent() || actor.Automation != nil {
		return ErrForbidden
	}
	r, err := s.GetRunner(ctx, actor, id)
	if err != nil {
		return err
	}
	err = s.write(ctx, actor, func(t *tx) error {
		rows, err := t.QueryContext(ctx, `SELECT id, state FROM runner_jobs WHERE state IN ('queued','running') AND (runner_id = ? OR target_runner_id = ?)`, id, id)
		if err != nil {
			return err
		}
		type open struct {
			id    int64
			state string
		}
		var jobs []open
		for rows.Next() {
			var o open
			if err := rows.Scan(&o.id, &o.state); err != nil {
				rows.Close()
				return err
			}
			jobs = append(jobs, o)
		}
		rows.Close()
		for _, o := range jobs {
			state, msg := "cancelled", "The runner was removed."
			if o.state == "running" {
				state = "failed"
			}
			if err := t.finishRunnerJob(ctx, o.id, state, nil, nil, msg); err != nil {
				return err
			}
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM runners WHERE id = ?`, id); err != nil {
			return err
		}
		if r.Online {
			t.emit(runnerStatus(id, r.Name, r.Owner.ID, false))
		}
		t.audit(AuditEntry{Action: "runner.remove", Category: AuditSettings, TargetType: "runner", TargetID: id, Target: r.Name, Detail: "removed the runner " + r.Name + " of " + r.Owner.Username})
		return nil
	})
	s.kickRunners()
	return err
}

// RunnerPoll is a runner asking for work (and saying it's alive).
type RunnerPoll struct {
	Runner int64 `json:"runner"`
	// Running lists the jobs the runner is working on; any other job the
	// server thinks it holds was lost (the runner restarted), and one of
	// these the server has ended is in the answer's Cancel.
	Running []int64 `json:"running"`
	// Free is how many more jobs it can take now (default 1).
	Free *int `json:"free"`
	// CLIs re-reports the agent CLIs found and signed in.
	CLIs *[]RunnerCLI `json:"clis"`
	// Wait is how many seconds to hold the poll (at most 30; tests use less).
	Wait *float64 `json:"wait"`
	// Lanes are the kinds of job it takes now (LaneHeavy, LaneLight,
	// LaneRequest, LaneMessage, LaneGateway, LaneAgent); nil (an older runner): any
	// (WP-366, WP-503).
	Lanes []string `json:"lanes"`
	// Capacity is how busy its machine is and what it takes because of it.
	Capacity *RunnerCapacity `json:"capacity"`
	// Sandbox re-reports the machine's sandbox helper (WP-418).
	Sandbox *sandbox.Report `json:"sandbox"`
	// SandboxDone says how the helper actions it was handed went (WP-448).
	SandboxDone []RunnerSandboxResult `json:"sandbox_done,omitempty"`
}

// RunnerWork is what a poll returns: a job to start, and jobs to kill.
type RunnerWork struct {
	Job    *RunnerClaim `json:"job"`
	Cancel []int64      `json:"cancel"`
	// Sandbox are actions for the machine's sandbox helper that a person
	// asked for (WP-448), each with its signed spec.
	Sandbox []RunnerSandboxOp `json:"sandbox,omitempty"`
}

// RunnerClaim is a job handed to a runner.
type RunnerClaim struct {
	ID      int64           `json:"id"`
	Kind    string          `json:"kind"`
	Title   string          `json:"title"`
	Input   json.RawMessage `json:"input"`
	Timeout int             `json:"timeout"` // seconds
	// Token is the job's own Waypoint token, for the jobs that run wp
	// (WP-418): the runner hands it to them instead of its own.
	Token string `json:"token,omitempty"`
	// Sandbox is the signed spec the runner hands its sandbox helper
	// before the job starts (WP-418); only where the helper is ready.
	Sandbox *sandbox.Signed `json:"sandbox,omitempty"`
	// Sessions are start specs for the job's Contained sessions, in the
	// order it runs them: so far only the escape drill's (WP-473).
	Sessions []*sandbox.Signed `json:"sessions,omitempty"`
	// Lane is the lane of the poll's it was taken in (WP-503): the runner
	// counts it there.
	Lane string `json:"lane,omitempty"`
	// Launch is the owner's session settings (WP-1265), for the jobs that
	// start or restart a Claude Code session; the runner picks the set for
	// the session's repo.
	Launch *SessionPrefs `json:"launch,omitempty"`
}

// launchesSession says whether a job of kind starts Claude Code sessions.
func launchesSession(kind string) bool {
	return kind == JobAutopilot || kind == JobAutopilotStart || kind == JobSession
}

// Poll holds up to 30 seconds for a job this runner may take. It is also
// the heartbeat: a runner that polled in the last 90 seconds is online.
func (s *Service) RunnerPoll(ctx context.Context, actor *Actor, in RunnerPoll) (*RunnerWork, error) {
	if err := actor.Need(PermRunnersHost); err != nil {
		return nil, err
	}
	if err := runnerActor(actor); err != nil {
		return nil, err
	}
	r, err := s.runnerByID(ctx, s.db.R, in.Runner)
	if err != nil || r.Owner.ID != actor.User.ID {
		return nil, notFound("runner")
	}
	if err := s.runnerSeen(ctx, actor, r, in); err != nil {
		return nil, err
	}
	if in.CLIs != nil {
		if r, err = s.runnerByID(ctx, s.db.R, r.ID); err != nil {
			return nil, err
		}
	}
	free := 1
	if in.Free != nil {
		free = *in.Free
	}
	if in.Lanes != nil && len(in.Lanes) == 0 {
		free = 0
	}
	if in.Capacity != nil {
		s.noteCapacity(r.ID, in.Capacity)
	}
	wait := runnerMaxWait
	if in.Wait != nil {
		wait = min(max(time.Duration(*in.Wait*float64(time.Second)), 0), runnerMaxWait)
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for first := true; ; first = false {
		wake := s.runnerWake()
		if !first {
			if r, err = s.runnerByID(ctx, s.db.R, r.ID); err != nil {
				return nil, err
			}
		}
		cancel, err := s.cancelsFor(ctx, r.ID, in.Running)
		if err != nil {
			return nil, err
		}
		ops, err := s.sandboxOpsFor(ctx, actor, r)
		if err != nil {
			return nil, err
		}
		var claim *RunnerClaim
		if free > 0 && len(cancel) == 0 {
			if claim, err = s.claimRunnerJob(ctx, actor, r, in.Lanes); err != nil {
				return nil, err
			}
		}
		if claim != nil || len(cancel) > 0 || len(ops) > 0 || wait == 0 {
			return &RunnerWork{Job: claim, Cancel: cancel, Sandbox: ops}, nil
		}
		select {
		case <-wake:
		case <-timer.C:
			return &RunnerWork{Cancel: []int64{}}, nil
		case <-ctx.Done():
			return &RunnerWork{Cancel: []int64{}}, nil
		case <-s.bgCtx.Done():
			return &RunnerWork{Cancel: []int64{}}, nil
		}
	}
}

// runnerSeen stamps the poll, announces coming online, takes a new CLI
// report and fails jobs the runner no longer knows about.
func (s *Service) runnerSeen(ctx context.Context, actor *Actor, r *Runner, in RunnerPoll) error {
	now := s.nowMS()
	return s.write(ctx, actor, func(t *tx) error {
		if in.CLIs != nil {
			var reg RunnerRegistration
			reg.Name, reg.CLIs = "x", *in.CLIs
			if err := reg.clean(); err != nil {
				return err
			}
			b, _ := json.Marshal(reg.CLIs)
			if _, err := t.ExecContext(ctx, `UPDATE runners SET clis = ? WHERE id = ?`, string(b), r.ID); err != nil {
				return err
			}
		}
		if in.Sandbox != nil {
			if err := t.saveSandboxReport(ctx, r.ID, *in.Sandbox); err != nil {
				return err
			}
		}
		if len(in.SandboxDone) > 0 {
			if err := t.finishSandboxOps(ctx, r.ID, in.SandboxDone); err != nil {
				return err
			}
		}
		if _, err := t.ExecContext(ctx, `UPDATE runners SET last_seen_at = ?, online = 1 WHERE id = ?`, now, r.ID); err != nil {
			return err
		}
		if !r.online {
			t.emit(runnerStatus(r.ID, r.Name, r.Owner.ID, true))
		}
		if in.Running == nil {
			return nil
		}
		rows, err := t.QueryContext(ctx, `SELECT id FROM runner_jobs WHERE runner_id = ? AND state = 'running' AND started_at < ?`, r.ID, now-runnerLostGrace.Milliseconds())
		if err != nil {
			return err
		}
		var lost []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			if !slices.Contains(in.Running, id) {
				lost = append(lost, id)
			}
		}
		rows.Close()
		for _, id := range lost {
			if err := t.finishRunnerJob(ctx, id, "failed", nil, nil, "The runner lost this job (it restarted, or never received it)."); err != nil {
				return err
			}
		}
		return nil
	})
}

// cancelsFor lists the jobs a runner is to stop: the running ones a person
// asked to stop, and those of the ones it says it runs that Waypoint ended
// itself (an audit called off, a step stopped). The answer to a log upload
// tells it too, but a job that prints nothing uploads none, and ran on to
// its own time limit in a slot the queue needed (WP-870).
func (s *Service) cancelsFor(ctx context.Context, runnerID int64, running []int64) ([]int64, error) {
	q := `SELECT id FROM runner_jobs WHERE runner_id = ? AND state = 'running' AND cancel_requested = 1`
	args := []any{runnerID}
	if n := min(len(running), runnerEndedMax); n > 0 {
		q += ` UNION SELECT id FROM runner_jobs WHERE runner_id = ? AND state NOT IN ('queued', 'running') AND finished_at < ? AND id IN (?` + strings.Repeat(", ?", n-1) + `)`
		args = append(args, runnerID, s.nowMS()-runnerEndedGrace.Milliseconds())
		for _, id := range running[:n] {
			args = append(args, id)
		}
	}
	rows, err := s.db.R.QueryContext(ctx, q+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// jobNeeds is what a job asks of a runner.
type jobNeeds struct {
	kind, label, dir, cli string
	source                string
	feature               string
	patch                 bool
}

// runnerCanTake: labels match, the directory is allowed, the CLI is there
// (and not known to be signed out).
func runnerCanTake(r *Runner, n jobNeeds) bool {
	if n.kind == JobReview && !slices.Contains(r.Features, FeatureConfine) {
		return false
	}
	if n.kind == JobAutopilotStart && !slices.Contains(r.Features, FeatureAutopilotStart) {
		return false
	}
	if n.kind == JobAutopilot && !slices.Contains(r.Features, FeatureAutopilot) {
		return false
	}
	if n.kind == JobAudit && !slices.Contains(r.Features, FeatureAudit) {
		return false
	}
	if n.kind == JobObjective && !slices.Contains(r.Features, FeatureObjective) {
		return false
	}
	if n.kind == JobSandboxDrill && !slices.Contains(r.Features, FeatureSandboxDrill) {
		return false
	}
	if n.feature != "" && !slices.Contains(r.Features, n.feature) {
		return false
	}
	if n.label != "" && !slices.Contains(r.Labels, n.label) {
		return false
	}
	if n.dir != "" && !dirAllowed(n.dir, r.AllowedDirs) {
		return false
	}
	if n.cli == GatewayCLI {
		return slices.Contains(r.Features, FeatureGatewayAudit)
	}
	if n.cli != "" {
		i := slices.IndexFunc(r.CLIs, func(c RunnerCLI) bool { return c.Name == n.cli })
		if i < 0 || (r.CLIs[i].SignedIn != nil && !*r.CLIs[i].SignedIn) {
			return false
		}
	}
	return true
}

// claimOrder is the queue a runner takes from, in turn: the rounds of
// urgent pull requests (WP-686), then the rounds at the front of a merge
// line, which claimRunnerJob takes by frontRounds, not from this list
// (WP-1618, WP-ADR149), every kind but audits, then audits of pull
// requests, then the other audits. Each lists its jobs oldest first; the
// parameters are the owner, the runner, JobAudit, auditSource, the id to
// start after and a page size.
const claimUrgent = 0

var claimOrder = []string{
	claimUrgent: `SELECT id, kind, source, label, dir, cli, ` + jobFeatureCol + ` FROM runner_jobs WHERE owner_id = ?1 AND state = 'queued' AND (target_runner_id IS NULL OR target_runner_id = ?2)
		AND kind != ?3 AND ` + urgentRoundJob + ` AND id > ?5 ORDER BY id LIMIT ?6`,
	`SELECT id, kind, source, label, dir, cli, ` + jobFeatureCol + ` FROM runner_jobs WHERE owner_id = ?1 AND state = 'queued' AND (target_runner_id IS NULL OR target_runner_id = ?2)
		AND kind != ?3 AND id > ?5 ORDER BY id LIMIT ?6`,
	`SELECT id, kind, source, label, dir, cli, ` + jobFeatureCol + ` FROM runner_jobs WHERE owner_id = ?1 AND state = 'queued' AND (target_runner_id IS NULL OR target_runner_id = ?2)
		AND kind = ?3 AND source = ?4 AND source_id IN (SELECT id FROM code_audits WHERE pr_id IS NOT NULL) AND id > ?5 ORDER BY id LIMIT ?6`,
	`SELECT id, kind, source, label, dir, cli, ` + jobFeatureCol + ` FROM runner_jobs WHERE owner_id = ?1 AND state = 'queued' AND (target_runner_id IS NULL OR target_runner_id = ?2)
		AND kind = ?3 AND NOT (source = ?4 AND source_id IN (SELECT id FROM code_audits WHERE pr_id IS NOT NULL)) AND id > ?5 ORDER BY id LIMIT ?6`,
}

// jobFeatureCol is the runner feature a queued job needs beyond its kind,
// read from its input (checkRunnerInput's jobNeeds.feature): a task
// session's kickoff (WP-1037).
const jobFeatureCol = `CASE WHEN kind = '` + JobSession + `' AND coalesce(json_extract(input, '$.kickoff'), '') != '' THEN '` + FeatureTaskKickoff + `' ELSE '' END`

// jobFeatureColJ is jobFeatureCol for queries that name the job j (jobCols),
// so what a job waits for (queuedWhy) agrees with what a runner may claim.
const jobFeatureColJ = `CASE WHEN j.kind = '` + JobSession + `' AND coalesce(json_extract(j.input, '$.kickoff'), '') != '' THEN '` + FeatureTaskKickoff + `' ELSE '' END`

// claimable lists up to ten queued jobs of one claimOrder query that the
// runner may take. It looks through the queue a page at a time: older jobs
// this runner can't take must not hide the ones it can.
func (s *Service) claimable(ctx context.Context, r *Runner, q string, lanes []string) ([]int64, error) {
	const page = 100
	var ids []int64
	inGateway, err := s.gatewayRunning(ctx)
	if err != nil {
		return nil, err
	}
	gatewayFull, prepFull := inGateway >= GatewayTasksInFlight, inGateway >= GatewayTasksInFlight-prepAgentReserve
	for after := int64(0); len(ids) < 10; {
		rows, err := s.db.R.QueryContext(ctx, q, r.Owner.ID, r.ID, JobAudit, auditSource, after, page)
		if err != nil {
			return nil, err
		}
		seen := 0
		for rows.Next() {
			var id int64
			var n jobNeeds
			if err := rows.Scan(&id, &n.kind, &n.source, &n.label, &n.dir, &n.cli, &n.feature); err != nil {
				rows.Close()
				return nil, err
			}
			seen, after = seen+1, id
			if n.cli == GatewayCLI && (gatewayFull || (prepFull && n.source == prepAgentSource)) {
				continue
			}
			if _, ok := claimLane(r, n, lanes); ok && runnerCanTake(r, n) {
				ids = append(ids, id)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if seen < page {
			break
		}
	}
	return ids, nil
}

// frontRound is a queued round of a babysit whose pull request is at the
// front of its merge line, with what a runner needs to know to take it.
type frontRound struct {
	id     int64
	needs  jobNeeds
	target sql.NullInt64
}

// queuedFrontRounds lists the owner's queued rounds of the pull requests at
// the front of their merge lines (lineFrontPR), in the order runners take
// them: the line whose babysit rounds least recently got a runner first
// (never: first), so two repos' fronts alternate, then the oldest job. At
// most one pull request per line is the front, so the list is short. Both
// claiming and jobsAhead read it, so they agree (WP-1618, WP-ADR149).
func (s *Service) queuedFrontRounds(ctx context.Context, owner int64) ([]frontRound, error) {
	// #nosec G202 -- fixed SQL fragments; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT j.id, j.kind, j.source, j.label, j.dir, j.cli, `+jobFeatureColJ+`, j.target_runner_id
		FROM runner_jobs j JOIN babysits b ON b.id = j.source_id JOIN pull_requests p ON p.id = b.pr_id
		WHERE j.owner_id = ? AND j.state = 'queued' AND j.source = ? AND j.kind != ? AND b.ended_at IS NULL AND `+lineFrontPR+`
		ORDER BY coalesce((SELECT max(j2.started_at) FROM runner_jobs j2 JOIN babysits b2 ON b2.id = j2.source_id JOIN pull_requests p2 ON p2.id = b2.pr_id
			WHERE j2.source = ? AND p2.forge_id = p.forge_id AND lower(p2.repo) = lower(p.repo) AND p2.base_ref = p.base_ref), 0), j.id`,
		owner, babysitSource, JobAudit, babysitSource)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []frontRound
	for rows.Next() {
		var f frontRound
		n := &f.needs
		if err := rows.Scan(&f.id, &n.kind, &n.source, &n.label, &n.dir, &n.cli, &n.feature, &f.target); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// frontRounds lists up to ten queued rounds at the front of a merge line
// that the runner may take, in queuedFrontRounds's order: those not kept
// for another runner, of a lane the poll asks for, that the runner has the
// CLI for. A round is never a gateway job (EnqueueRunnerJob), so
// claimable's limit on those has nothing to hold here.
func (s *Service) frontRounds(ctx context.Context, r *Runner, lanes []string) ([]int64, error) {
	all, err := s.queuedFrontRounds(ctx, r.Owner.ID)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, f := range all {
		if f.target.Valid && f.target.Int64 != r.ID {
			continue
		}
		if _, ok := claimLane(r, f.needs, lanes); ok && runnerCanTake(r, f.needs) {
			ids = append(ids, f.id)
			if len(ids) == 10 {
				break
			}
		}
	}
	return ids, nil
}

// claimRunnerJob takes the oldest queued job of the runner's owner that
// this runner may run, atomically. The round of an urgent pull request
// goes first, then the round of a pull request at the front of its merge
// line, the line served least recently first (frontRounds): a round of a
// pull request further back must not hold up the one the line waits for
// (WP-1618, WP-ADR149). Audits come after every other kind, and
// an audit of a pull request before other audits: those are background
// reads, and a batch of them queued at once would otherwise hold the runner
// for hours while babysit rounds, autopilot steps, sessions and the audit a
// pull request waits for queue behind them (WP-356). Only jobs of the
// poll's lanes, when it names some (WP-366). A round audit's job gets the
// round's finished panel runs set in its input as it is claimed
// (withPriorPanel), one read of the round's helper runs per such claim.
func (s *Service) claimRunnerJob(ctx context.Context, actor *Actor, r *Runner, lanes []string) (*RunnerClaim, error) {
	var ids []int64
	var err error
	for i, q := range claimOrder {
		if i == claimUrgent+1 {
			if ids, err = s.frontRounds(ctx, r, lanes); err != nil || len(ids) > 0 {
				break
			}
		}
		if ids, err = s.claimable(ctx, r, q, lanes); err != nil || len(ids) > 0 {
			break
		}
	}
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	signer := s.sandboxSigner(ctx, r)
	var claim *RunnerClaim
	var claimSource string
	var claimSID int64
	err = s.write(ctx, actor, func(t *tx) error {
		for _, id := range ids {
			var c RunnerClaim
			var input string
			var source, cli string
			var sid int64
			var project sql.NullInt64
			err := t.QueryRowContext(ctx, `UPDATE runner_jobs SET state = 'running', runner_id = ?, runner_name = ?, started_at = ?, attempts = attempts + 1
				WHERE id = ? AND state = 'queued' RETURNING id, kind, title, input, timeout, source, source_id, project_id, cli`, r.ID, r.Name, s.nowMS(), id).Scan(&c.ID, &c.Kind, &c.Title, &input, &c.Timeout, &source, &sid, &project, &cli)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			c.Input = json.RawMessage(input)
			if !slices.Contains(r.Features, FeatureWorkClock) {
				var in struct {
					WorkTimeout int `json:"work_timeout"`
				}
				if json.Unmarshal(c.Input, &in) == nil && in.WorkTimeout > 0 && in.WorkTimeout < c.Timeout {
					c.Timeout = in.WorkTimeout
				}
			}
			if err := s.recordHandedOver(ctx, t, c.Kind, c.Input, source, c.Timeout, r.Owner.ID); err != nil {
				return err
			}
			c.Lane, _ = claimLane(r, jobNeeds{kind: c.Kind, source: source, cli: cli}, lanes)
			if agent, ok := jobTokenAgent(c.Kind, c.Input); ok {
				if c.Token, err = t.mintJobToken(ctx, c.ID, r.Owner.ID, agent, c.Timeout); err != nil {
					return err
				}
				if signer != nil && project.Valid {
					if c.Sandbox, err = signer.sign(ctx, t, s.now(), &c, project.Int64); err != nil {
						s.log.WarnContext(ctx, "sandbox spec", "job", c.ID, "err", err)
						c.Sandbox = nil
					}
				}
			}
			if launchesSession(c.Kind) {
				var prefs string
				if err := t.QueryRowContext(ctx, `SELECT prefs FROM users WHERE id = ?`, r.Owner.ID).Scan(&prefs); err != nil {
					return err
				}
				var p UserPrefs
				if err := json.Unmarshal([]byte(prefs), &p); err != nil {
					s.log.WarnContext(ctx, "session settings unreadable; the session starts without them", "job", c.ID, "user", r.Owner.ID, "err", err)
				} else if p.Sessions != nil && (!p.Sessions.All.Empty() || len(p.Sessions.Repos) > 0) {
					c.Launch = p.Sessions
				}
			}
			if c.Kind == JobSandboxDrill && source == sandboxDrillSource {
				c.Sessions = s.drillSessions(ctx, t, signer, r, &c, sid)
			}
			claim, claimSource, claimSID = &c, source, sid
			t.emit(jobUpdated(&c, "running", r.ID, r.Owner.ID, 0))
			return nil
		}
		return nil
	})
	if err == nil && claim != nil && claim.Kind == JobAudit && claimSource == auditSource {
		claim.Input = s.withPriorPanel(ctx, claim.Input, claimSID)
	}
	return claim, err
}

// withPriorPanel returns an audit job's input with the round's earlier
// panel runs set (priorPanel), read now, when a runner takes the job: a job
// that waited in the queue takes what finished meanwhile (WP-1481). Every
// field of the input is kept, those this version doesn't know too; the
// input is returned as it is when it has no panel, nothing finished, or it
// can't be read. Two audits of one round that run at the same time still
// both run the panel: a sibling that hasn't finished has recorded nothing
// to take (WP-1544).
func (s *Service) withPriorPanel(ctx context.Context, input json.RawMessage, auditID int64) json.RawMessage {
	var in map[string]json.RawMessage
	if json.Unmarshal(input, &in) != nil {
		return input
	}
	var h map[string]json.RawMessage
	if json.Unmarshal(in["helpers"], &h) != nil || h == nil {
		return input
	}
	var panel []json.RawMessage
	if json.Unmarshal(h["panel"], &panel) != nil || len(panel) == 0 {
		return input
	}
	s.settleRoundSiblings(ctx, auditID)
	prior := s.priorPanel(ctx, auditID)
	if len(prior) == 0 {
		if _, stale := h["prior"]; !stale {
			return input
		}
		delete(h, "prior")
	} else {
		pb, err := json.Marshal(prior)
		if err != nil {
			return input
		}
		h["prior"] = pb
		s.log.InfoContext(ctx, "prior panel runs attached at claim", "audit", auditID, "runs", len(prior))
	}
	hb, err := json.Marshal(h)
	if err != nil {
		return input
	}
	in["helpers"] = hb
	out, err := json.Marshal(in)
	if err != nil {
		return input
	}
	return out
}

func jobUpdated(c *RunnerClaim, state string, runner, owner, size int64) *Event {
	return &Event{Kind: EvRunnerJobUpdated, UserID: owner, Data: map[string]any{"job": c.ID, "kind": c.Kind, "state": state, "runner": runner, "size": size}}
}

// CheckRunners (every 30 s from the scheduler) announces runners that went
// quiet, expires jobs nobody finished or took, and forgets old finished ones.
func (s *Service) CheckRunners(ctx context.Context) {
	now := s.nowMS()
	if err := s.write(ctx, SystemActor(), func(t *tx) error {
		rows, err := t.QueryContext(ctx, `SELECT id, name, owner_id FROM runners WHERE online = 1 AND (last_seen_at IS NULL OR last_seen_at < ?)`, now-runnerOnlineFor.Milliseconds())
		if err != nil {
			return err
		}
		var gone []*Runner
		for rows.Next() {
			var r Runner
			if err := rows.Scan(&r.ID, &r.Name, &r.Owner.ID); err != nil {
				rows.Close()
				return err
			}
			gone = append(gone, &r)
		}
		rows.Close()
		for _, r := range gone {
			if _, err := t.ExecContext(ctx, `UPDATE runners SET online = 0 WHERE id = ?`, r.ID); err != nil {
				return err
			}
			t.emit(runnerStatus(r.ID, r.Name, r.Owner.ID, false))
		}
		rows, err = t.QueryContext(ctx, `SELECT id, state FROM runner_jobs WHERE (state = 'running' AND started_at + (timeout + ?) * 1000 < ?) OR (state = 'queued' AND created_at < ?)`,
			int(runnerExpireAfter.Seconds()), now, now-runnerQueuedFor.Milliseconds())
		if err != nil {
			return err
		}
		type stale struct {
			id    int64
			state string
		}
		var old []stale
		for rows.Next() {
			var o stale
			if err := rows.Scan(&o.id, &o.state); err != nil {
				rows.Close()
				return err
			}
			old = append(old, o)
		}
		rows.Close()
		for _, o := range old {
			msg := "No runner took this job within a day."
			if o.state == "running" {
				msg = "The runner didn't report back before the job's timeout (plus 5 minutes)."
			}
			if err := t.finishRunnerJob(ctx, o.id, "expired", nil, nil, msg); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		s.log.WarnContext(ctx, "check runners", "err", err)
	}
	s.runners.mu.Lock()
	prune := s.now().Sub(s.runners.pruned) > time.Hour
	if prune {
		s.runners.pruned = s.now()
	}
	s.runners.mu.Unlock()
	if prune {
		if _, err := s.db.W.ExecContext(ctx, `DELETE FROM runner_jobs WHERE state NOT IN ('queued','running') AND finished_at < ?`, now-runnerKeepJobs.Milliseconds()); err != nil {
			s.log.WarnContext(ctx, "prune runner jobs", "err", err)
		}
	}
}
