package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Agent sessions (WP-D16): one run of a coding agent on one machine. They
// are filled by the CLI's hooks, never by the model, so they cost no tokens.
// A session is keyed by (user, machine, the CLI's session id), so a resumed
// session is the same row.

// Session states. The stored state is what the last hook said; reading it
// applies the idle and ended rules.
const (
	SessionWorking = "working"
	SessionWaiting = "waiting" // waiting for you
	SessionIdle    = "idle"
	SessionEnded   = "ended"

	sessionIdleAfter  = 30 * time.Minute
	sessionEndedAfter = 24 * time.Hour
)

// SessionStates are the states a session list can be filtered by.
var SessionStates = []string{SessionWorking, SessionWaiting, SessionIdle, SessionEnded}

// sessionState is the state people read: a session not heard from in 30
// minutes reads as idle, in 24 hours as ended.
func sessionState(stored string, lastSeenMS, nowMS int64) string {
	switch {
	case stored == SessionEnded || nowMS-lastSeenMS >= sessionEndedAfter.Milliseconds():
		return SessionEnded
	case nowMS-lastSeenMS >= sessionIdleAfter.Milliseconds():
		return SessionIdle
	}
	return stored
}

// AgentSession is one agent session as people read it.
type AgentSession struct {
	ID           int64           `json:"id"`
	CLIID        string          `json:"cli_id"`
	CLI          string          `json:"cli"`
	Machine      string          `json:"machine"`
	User         *UserRef        `json:"user"`
	Agent        string          `json:"agent"`
	Model        string          `json:"model,omitempty"`
	Cwd          string          `json:"cwd"`
	Repo         string          `json:"repo,omitempty"`
	Branch       string          `json:"branch,omitempty"`
	Worktree     bool            `json:"worktree,omitempty"`
	Project      *SessionProject `json:"project,omitempty"`
	Issue        *IssueRef       `json:"issue,omitempty"` // the task it holds
	Title        string          `json:"title,omitempty"`
	State        string          `json:"state"`
	WaitReason   string          `json:"wait_reason,omitempty"`
	LastPrompt   string          `json:"last_prompt,omitempty"` // only the owner sees prompts
	LastUpdate   string          `json:"last_update,omitempty"`
	InputTokens  int64           `json:"input_tokens"`
	OutputTokens int64           `json:"output_tokens"`
	StartedAt    time.Time       `json:"started_at"`
	LastSeenAt   time.Time       `json:"last_seen_at"`
	EndedAt      *time.Time      `json:"ended_at,omitempty"`
	OpenRequests int             `json:"open_requests"`
	// Pending: what its last Stop said still runs ("2 background tasks"),
	// "?" when its CLI couldn't say (WP-D22).
	Pending    string     `json:"pending,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"` // it truly finished, and hasn't worked since
	// FollowUp: steps queued to run when it finishes (only its person sees it).
	FollowUp *FollowUpBrief `json:"follow_up,omitempty"`
	// Review: where its newest adversarial review has got to, while that
	// matters (WP-752).
	Review *SessionReview `json:"review,omitempty"`
	// Kind: "task" (a launcher bound it to Tasks with WP_TASK) or "yours"
	// (a person started it); "" for sessions recorded before WP-34.
	Kind  string   `json:"kind,omitempty"`
	Tasks []string `json:"tasks,omitempty"`
	// ContextTokens: the context its last model call carried (at its last Stop).
	ContextTokens int64 `json:"context_tokens"`
	// Cache: its prompt cache and what Waypoint decided about keeping it
	// warm (WP-192); nil when nothing is known.
	Cache *SessionCache `json:"cache,omitempty"`
	URL   string        `json:"url"`

	userID    int64
	projectID sql.NullInt64
	issueID   sql.NullInt64
	stored    string
}

// SessionProject names a session's project.
type SessionProject struct {
	ID   int64  `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
}

// StopEndedByShip is a Stop's Reason when a hook ended the turn at wp ship,
// with the agent's --summary as its last message (WP-1093).
const StopEndedByShip = "ship"

// HookEvent is what one agent hook reports (the CLI's hook handler
// translates Claude Code's hook input into it). Events the server could not
// be reached with are sent later, in order, with At set to when they
// happened.
type HookEvent struct {
	// PromptSum: UserPromptSubmit: the SHA-256 of the whole prompt's
	// normalised text (PromptSum), which the hook computes from what it was
	// given, so a hook that withholds the text (--no-prompts) can still say
	// whether Waypoint typed it. Empty: the server digests Prompt.
	PromptSum string `json:"prompt_sum,omitempty"`
	// Key: one hook invocation's own key, the same on every attempt to post
	// it (the CLI's outbox sends the stored event again). A repeat of a
	// UserPromptSubmit with a key the server has already answered gets the
	// same answer and uses no further record. Empty (an older CLI): each
	// arrival is its own, as before.
	Key       string `json:"key,omitempty"`
	Event     string `json:"event"` // SessionStart, UserPromptSubmit, Notification, PermissionDenied, PreToolUse, PostToolUse, Stop, SubagentStart, SubagentStop, SessionEnd, PostCompact, ConfigChange, InstructionsLoaded, PostModelSwitch
	At        int64  `json:"at,omitempty"`
	SessionID string `json:"session_id"` // the CLI's
	Machine   string `json:"machine"`
	CLI       string `json:"cli,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	Repo      string `json:"repo,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Worktree  bool   `json:"worktree,omitempty"`
	Model     string `json:"model,omitempty"`
	Source    string `json:"source,omitempty"` // SessionStart: startup, resume, clear, compact

	Prompt string `json:"prompt,omitempty"` // UserPromptSubmit (omitted with --no-prompts)
	Title  string `json:"title,omitempty"`

	NotificationType string `json:"notification_type,omitempty"`
	Message          string `json:"message,omitempty"`

	LastMessage    string `json:"last_message,omitempty"` // Stop: the agent's final message
	StopHookActive bool   `json:"stop_hook_active,omitempty"`
	InputTokens    int64  `json:"input_tokens,omitempty"` // running totals for the session
	OutputTokens   int64  `json:"output_tokens,omitempty"`

	Tool         string          `json:"tool,omitempty"` // PermissionDenied, PreToolUse, PostToolUse
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	ToolInput    json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse json.RawMessage `json:"tool_response,omitempty"`
	// Reason: PermissionDenied's; on Stop, StopEndedByShip when a hook
	// ended the turn at wp ship (WP-1093).
	Reason string `json:"reason,omitempty"`

	AgentID   string `json:"agent_id,omitempty"` // SubagentStart/Stop
	AgentType string `json:"agent_type,omitempty"`
	// SubagentStop of an adversarial review the session ran itself
	// (WP-1089): what it was asked and what it reported, read from its
	// transcript by the hook, so the task's work log keeps it.
	AgentDescription string `json:"agent_description,omitempty"`
	AgentModel       string `json:"agent_model,omitempty"`
	AgentPrompt      string `json:"agent_prompt,omitempty"`
	AgentReport      string `json:"agent_report,omitempty"`
	EndReason        string `json:"end_reason,omitempty"`

	// Stop: what the agent still runs that can wake it (WP-D22); nil when
	// the CLI didn't say, so whether it finished can't be told.
	Pending *HookPending `json:"pending,omitempty"`
	// SessionStart: the runner job (start_session) that started it, from
	// WAYPOINT_FOLLOWUP in the hook's environment.
	SpawnedBy string `json:"spawned_by,omitempty"`
	// Task: WP_TASK in the hook's environment, KEY[,KEY…]: a launcher bound
	// the session to these tasks, so it is a task session (WP-34). Every
	// event carries it.
	Task string `json:"task,omitempty"`
	// ContextTokens: Stop and SessionEnd: the context the session's last
	// model call carried (input, cache reads and writes), from its transcript.
	ContextTokens int64 `json:"context_tokens,omitempty"`
	// CacheTTL: Stop: the prompt-cache tier the session's last calls wrote
	// ("1h", "5m"), from its transcript; "" when none said (WP-192).
	CacheTTL string `json:"cache_ttl,omitempty"`
	// Checkouts: Stop and SessionStart: the git state of the checkouts the
	// session works in (WP-567).
	Checkouts []HookCheckout `json:"checkouts,omitempty"`

	// What the session is running with, as the hook observed it (WP-1468).
	// PermissionMode and Effort ride on the events Claude Code gives them to
	// (UserPromptSubmit, Stop) and on the tool events the hook sends
	// (PermissionDenied, AskUserQuestion's PreToolUse and PostToolUse, a
	// watched Bash PostToolUse): a change mid-turn is otherwise recorded at the
	// next UserPromptSubmit or Stop, with that event's time; File, MemoryType and LoadReason
	// are InstructionsLoaded's; File and Source are ConfigChange's; FromModel
	// and RequestedModel are PostModelSwitch's (Model is its to_model).
	PermissionMode string `json:"permission_mode,omitempty"`
	Effort         string `json:"effort,omitempty"`
	FromModel      string `json:"from_model,omitempty"`
	RequestedModel string `json:"requested_model,omitempty"`
	File           string `json:"file,omitempty"`
	MemoryType     string `json:"memory_type,omitempty"`
	LoadReason     string `json:"load_reason,omitempty"`
	// Snapshot: SessionStart, PostCompact and ConfigChange carry the
	// inventory the CLI read on the machine.
	Snapshot *HookSnapshot `json:"snapshot,omitempty"`
	// Drops: what the CLI's outbox dropped since it last got through, by
	// "reason/event" (WP-ADR145 point 5).
	Drops map[string]int64 `json:"drops,omitempty"`
	// DropsID names one report of Drops (random, at most 64 characters), so
	// one the client sent again after a lost reply is counted once.
	DropsID string `json:"drops_id,omitempty"`

	// quiet: a keep-warm ping's prompt or stop, or Waypoint's /compact
	// (WP-192): no turn of the session's work, so it records nothing of one.
	quiet bool
	// typed: the kind of text Waypoint typed that this UserPromptSubmit
	// reports (TypedReview, ...), "" for a person's prompt (recogniseTyped).
	typed string
	// kept: this UserPromptSubmit was the person's keep (WP-ADR9) or
	// another session command (WP-562): it goes no further, and they see
	// this line instead of a turn.
	kept string
	// advance: a step /wp next made due now, moved on after the write.
	advance *stepRow
	// review: a /wp review, answered after the write.
	review *reviewAsk
}

// HookPending is the background work a Stop reports: Claude Code's
// background_tasks (shells, subagents, monitors, workflows) and
// session_crons (crons and ScheduleWakeup wakeups).
type HookPending struct {
	Tasks []PendingTask `json:"tasks"`
	Crons []PendingCron `json:"crons"`
}

// PendingTask is one background task.
type PendingTask struct {
	Type        string `json:"type"`
	Status      string `json:"status"`
	Description string `json:"description,omitempty"`
}

// PendingCron is one cron or wakeup.
type PendingCron struct {
	Schedule  string `json:"schedule,omitempty"`
	Recurring bool   `json:"recurring,omitempty"`
	Prompt    string `json:"prompt,omitempty"`
}

// HookResult is the server's answer to a batch of hook events.
type HookResult struct {
	Session int64 `json:"session"`
	// Text is Deliver as the agent should read it.
	Text string `json:"text,omitempty"`
	// Deliver: for Stop and SessionStart, the decisions that arrived since
	// the agent asked. They are marked delivered by being returned.
	Deliver []*Request `json:"deliver,omitempty"`
	// Messages: queued prompts for the agent (WP-D22), delivered with Text.
	Messages []*SessionMessage `json:"messages,omitempty"`
	// Await: at a Stop that set off a follow-up, how many seconds the hook
	// should wait for a message before letting the agent stop.
	Await int `json:"await,omitempty"`
	// Note: at a Stop, one line for the person, never the model (Claude
	// Code's systemMessage): the session is big after a finished task (WP-34).
	Note string `json:"note,omitempty"`
	// End: at a Stop of a task session that finished its tasks, the CLI
	// should end the session (WP-97).
	End bool `json:"end,omitempty"`
	// Block: at a UserPromptSubmit, the prompt goes no further. Claude Code
	// stops before any model call and shows this line to the person
	// (autopilot's keep, WP-ADR9).
	Block string `json:"block,omitempty"`
	// Pause: the person's pause that winds this session down, or null; the
	// hooks keep a local copy for the PreToolUse hooks (WP-146).
	Pause *AgentPause `json:"pause"`
	// Guard: the live quota guard's policy and hold, for the hooks' local
	// copy (WP-932); nil when it can't be read, which keeps their copy.
	Guard *GuardState `json:"guard,omitempty"`
	// EnvHosts: at a SessionStart or prompt in a repo, the hosts of its
	// projects' environments whose addresses a person set, for the guard's
	// local copy (WP-749, WP-1087); null otherwise, which leaves the copy as
	// it is.
	EnvHosts []string `json:"env_hosts"`
}

// SessionForHeader finds the actor's session from the X-Waypoint-Session
// value (the CLI's session id) and, if given, the machine. 0: none known
// (the hook has not registered it, or it isn't this person's).
func (s *Service) SessionForHeader(ctx context.Context, userID int64, cliID, machine string) int64 {
	if userID == 0 || cliID == "" || len(cliID) > 200 {
		return 0
	}
	key := fmt.Sprintf("%d|%s|%s", userID, machine, cliID)
	now := s.now()
	s.sessCache.mu.Lock()
	e, ok := s.sessCache.m[key]
	s.sessCache.mu.Unlock()
	if ok && now.Before(e.until) {
		return e.id
	}
	var id int64
	q := `SELECT id FROM agent_sessions WHERE user_id = ? AND cli_id = ?`
	args := []any{userID, cliID}
	if machine != "" {
		q += ` AND machine = ?`
		args = append(args, machine)
	}
	err := s.db.R.QueryRowContext(ctx, q+` ORDER BY last_seen_at DESC LIMIT 1`, args...).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	if id == 0 {
		return 0 // not registered yet (or not this person's): ask again next time
	}
	s.sessCache.mu.Lock()
	if s.sessCache.m == nil || len(s.sessCache.m) > 1000 {
		s.sessCache.m = map[string]sessionCacheEntry{}
	}
	s.sessCache.m[key] = sessionCacheEntry{id: id, until: now.Add(5 * time.Minute)}
	s.sessCache.mu.Unlock()
	return id
}

type sessionCache struct {
	mu sync.Mutex
	m  map[string]sessionCacheEntry
}

type sessionCacheEntry struct {
	id    int64
	until time.Time
}

const sessionCols = `s.id, s.user_id, s.machine, s.cli_id, s.cli, s.agent, s.model, s.cwd, s.repo, s.branch, s.worktree, s.project_id, s.issue_id,
	s.title, s.state, s.wait_reason, s.last_prompt, s.last_update, s.input_tokens, s.output_tokens, s.started_at, s.last_seen_at, s.ended_at,
	s.pending, s.finished_at, s.kind, s.task_keys, s.context_tokens, s.cache_ttl, s.warm_state, s.warm_note, s.warm_pings, s.warm_at`

func scanSession(sc interface{ Scan(...any) error }, nowMS int64) (*AgentSession, error) {
	var x AgentSession
	var wt int
	var started, seen int64
	var ended, finished, warmAt sql.NullInt64
	var taskKeys string
	var c SessionCache
	if err := sc.Scan(&x.ID, &x.userID, &x.Machine, &x.CLIID, &x.CLI, &x.Agent, &x.Model, &x.Cwd, &x.Repo, &x.Branch, &wt, &x.projectID, &x.issueID,
		&x.Title, &x.stored, &x.WaitReason, &x.LastPrompt, &x.LastUpdate, &x.InputTokens, &x.OutputTokens, &started, &seen, &ended,
		&x.Pending, &finished, &x.Kind, &taskKeys, &x.ContextTokens, &c.TTL, &c.Decision, &c.Note, &c.Pings, &warmAt); err != nil {
		return nil, err
	}
	if c.TTL != "" || c.Decision != "" {
		if c.Decision == warmInPing {
			c.Decision = warmPinged
		}
		c.At = ptrTime(warmAt)
		x.Cache = &c
	}
	x.Tasks = splitTaskKeys(taskKeys)
	x.Worktree = wt != 0
	x.StartedAt, x.LastSeenAt, x.EndedAt, x.FinishedAt = msTime(started), msTime(seen), ptrTime(ended), ptrTime(finished)
	x.State = sessionState(x.stored, seen, nowMS)
	if x.State != SessionWaiting {
		x.WaitReason = ""
	}
	x.URL = fmt.Sprintf("/agents/sessions/%d", x.ID)
	return &x, nil
}

// sessionMeta fills the people, projects, tasks and open-request counts of a
// list of sessions in batched queries, and hides what the viewer may not see.
func (s *Service) sessionMeta(ctx context.Context, actor *Actor, list []*AgentSession) error {
	if len(list) == 0 {
		return nil
	}
	ids := make([]int64, len(list))
	var projects, issues []int64
	for n, x := range list {
		ids[n] = x.ID
		if x.projectID.Valid {
			projects = append(projects, x.projectID.Int64)
		}
		if x.issueID.Valid {
			issues = append(issues, x.issueID.Int64)
		}
	}
	people := map[int64]*UserRef{}
	pnames := map[int64]*SessionProject{}
	if len(projects) > 0 {
		// #nosec G202 -- only ? placeholders are joined in
		rows, err := s.db.R.QueryContext(ctx, `SELECT id, key, name FROM projects WHERE id IN (`+placeholders(len(projects))+`)`, int64Args(projects)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p SessionProject
			if err := rows.Scan(&p.ID, &p.Key, &p.Name); err != nil {
				rows.Close()
				return err
			}
			pnames[p.ID] = &p
		}
		rows.Close()
	}
	irefs := map[int64]*IssueRef{}
	if len(issues) > 0 {
		// #nosec G202 -- only ? placeholders are joined in
		rows, err := s.db.R.QueryContext(ctx, `SELECT i.id, p.key || '-' || i.number, i.title, i.type FROM issues i JOIN projects p ON p.id = i.project_id
			WHERE i.id IN (`+placeholders(len(issues))+`) AND `+draftsHidden(actor, "i"), int64Args(issues)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r IssueRef
			if err := rows.Scan(&r.ID, &r.Key, &r.Title, &r.Type); err != nil {
				rows.Close()
				return err
			}
			irefs[r.ID] = &r
		}
		rows.Close()
	}
	open := map[int64]int{}
	// #nosec G202 -- only ? placeholders are joined in
	rows, err := s.db.R.QueryContext(ctx, `SELECT session_id, count(*) FROM requests WHERE session_id IN (`+placeholders(len(ids))+`)
		AND state = 'open' AND (snoozed_until IS NULL OR snoozed_until <= ?) GROUP BY session_id`, append(int64Args(ids), s.nowMS())...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return err
		}
		open[id] = n
	}
	rows.Close()
	for _, x := range list {
		x.User = s.cachedUser(ctx, people, sql.NullInt64{Int64: x.userID, Valid: true})
		if x.projectID.Valid {
			x.Project = pnames[x.projectID.Int64]
		}
		if x.issueID.Valid {
			x.Issue = irefs[x.issueID.Int64]
		}
		x.OpenRequests = open[x.ID]
		// Prompts are the owner's; colleagues in the project see the work.
		if x.userID != actor.id() && !actor.isAdmin() {
			x.LastPrompt = ""
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := s.sessionReviews(ctx, actor, list); err != nil {
		return err
	}
	return s.followUpBriefs(ctx, actor, list)
}

// sessionVisible: the owner, admins, and members of the session's project.
func (s *Service) sessionVisible(ctx context.Context, q queryer, actor *Actor, x *AgentSession) bool {
	if actor == nil || actor.User == nil {
		return false
	}
	if (x.userID == actor.id() && (actor.scope == nil || (!x.projectID.Valid && actor.Session != 0 && x.ID == actor.Session))) || actor.isAdmin() {
		return true
	}
	if !x.projectID.Valid {
		return false
	}
	r, rerr := s.projectRole(ctx, q, actor, x.projectID.Int64)
	if rerr != nil {
		if readFault(rerr) {
			s.log.WarnContext(ctx, "project role failed", "projectid", x.projectID.Int64, "err", rerr)
		}
	}
	return r > RoleNone
}

// AgentSessionQuery narrows a session list.
type AgentSessionQuery struct {
	User    string // a person (id, username or "me")
	Machine string
	Repo    string
	Branch  string
	Project string // a project key
	State   string // working, waiting, idle, ended; "active": all but ended
	Agent   string // an agent spec
	Limit   int
	Offset  int
}

// AgentSessionPage is a page of sessions, most recently seen first.
type AgentSessionPage struct {
	Sessions []*AgentSession `json:"sessions"`
	Total    int             `json:"total"`
	// Next, when set, is the offset of the following page.
	Next int `json:"next,omitempty"`
}

// ListAgentSessions lists the sessions the actor may see.
func (s *Service) ListAgentSessions(ctx context.Context, actor *Actor, q AgentSessionQuery) (*AgentSessionPage, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}
	now := s.nowMS()
	idleCut, endCut := now-sessionIdleAfter.Milliseconds(), now-sessionEndedAfter.Milliseconds()
	// The state people read, as SQL.
	stateExpr := `(CASE WHEN s.state = 'ended' OR s.last_seen_at <= ? THEN 'ended' WHEN s.last_seen_at <= ? THEN 'idle' ELSE s.state END)`
	where, args := []string{"1=1"}, []any{}
	if !actor.isAdmin() {
		// A job's token sees its own session while that has no project,
		// and those of its own project (WP-418), as sessionVisible does.
		member, margs := actor.memberProjects(RoleViewer)
		where = append(where, `((s.user_id = ? AND (? OR (s.project_id IS NULL AND s.id = ?))) OR s.project_id IN `+member+`)`)
		args = append(append(args, actor.id(), actor.scope == nil, actor.Session), margs...)
	}
	if q.User != "" {
		u, err := s.resolveUser(ctx, s.db.R, actor, q.User)
		if err != nil {
			return nil, err
		}
		where, args = append(where, "s.user_id = ?"), append(args, u.ID)
	}
	if q.Machine != "" {
		where, args = append(where, "s.machine = ? COLLATE NOCASE"), append(args, q.Machine)
	}
	if q.Repo != "" {
		where, args = append(where, "s.repo = ? COLLATE NOCASE"), append(args, q.Repo)
	}
	if q.Branch != "" {
		where, args = append(where, "s.branch = ?"), append(args, q.Branch)
	}
	if q.Project != "" {
		p, err := s.projectByRef(ctx, s.db.R, q.Project)
		if err != nil {
			return nil, err
		}
		if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
			return nil, err
		}
		where, args = append(where, "s.project_id = ?"), append(args, p.ID)
	}
	switch q.State {
	case "":
	case "active":
		where, args = append(where, stateExpr+" != 'ended'"), append(args, endCut, idleCut)
	default:
		if !slices.Contains(SessionStates, q.State) {
			return nil, invalid("state must be one of %s, or active", strings.Join(SessionStates, ", "))
		}
		where, args = append(where, stateExpr+" = ?"), append(args, endCut, idleCut, q.State)
	}
	spec := strings.TrimSpace(q.Agent)
	cond := strings.Join(where, " AND ")
	order := ` ORDER BY s.last_seen_at DESC, s.id DESC`
	page := &AgentSessionPage{Sessions: []*AgentSession{}}
	scan := func(rows *sql.Rows) ([]*AgentSession, error) {
		defer rows.Close()
		var out []*AgentSession
		for rows.Next() {
			x, err := scanSession(rows, now)
			if err != nil {
				return nil, err
			}
			out = append(out, x)
		}
		return out, rows.Err()
	}
	if spec != "" {
		// Agent specs match in Go ("claude" covers every Claude model), so
		// read the newest few thousand and page what matches.
		// #nosec G202 -- fixed fragments; values are bound
		rows, err := s.db.R.QueryContext(ctx, `SELECT `+sessionCols+` FROM agent_sessions s WHERE `+cond+order+` LIMIT 3000`, args...)
		if err != nil {
			return nil, err
		}
		all, err := scan(rows)
		if err != nil {
			return nil, err
		}
		all = slices.DeleteFunc(all, func(x *AgentSession) bool { return !AgentMatches(spec, x.Agent) && !AgentMatches(spec, x.Model) })
		page.Total = len(all)
		if q.Offset < len(all) {
			page.Sessions = all[q.Offset:min(len(all), q.Offset+q.Limit)]
		}
	} else {
		// #nosec G202 -- fixed fragments; values are bound
		if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM agent_sessions s WHERE `+cond, args...).Scan(&page.Total); err != nil {
			return nil, err
		}
		// #nosec G202 -- fixed fragments; values are bound
		rows, err := s.db.R.QueryContext(ctx, `SELECT `+sessionCols+` FROM agent_sessions s WHERE `+cond+order+` LIMIT ? OFFSET ?`, append(args, q.Limit, q.Offset)...)
		if err != nil {
			return nil, err
		}
		if page.Sessions, err = scan(rows); err != nil {
			return nil, err
		}
		if page.Sessions == nil {
			page.Sessions = []*AgentSession{}
		}
	}
	if q.Offset+len(page.Sessions) < page.Total {
		page.Next = q.Offset + len(page.Sessions)
	}
	return page, s.sessionMeta(ctx, actor, page.Sessions)
}

// sessionByID loads a session the actor may see.
func (s *Service) sessionByID(ctx context.Context, q queryer, actor *Actor, id int64) (*AgentSession, error) {
	x, err := scanSession(q.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM agent_sessions s WHERE s.id = ?`, id), s.nowMS())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("session")
	}
	if err != nil {
		return nil, err
	}
	if !s.sessionVisible(ctx, q, actor, x) {
		return nil, notFound("session")
	}
	return x, nil
}

// CurrentAgentSession is one of the actor's own sessions, found the way the
// CLI knows it: by its session id (cliID) and, if given, the machine. With no
// cliID, the session the request's X-Waypoint-Session header named. `wp
// followup` uses it to act on "this session" (WP-D22).
func (s *Service) CurrentAgentSession(ctx context.Context, actor *Actor, cliID, machine string) (*AgentSession, error) {
	if actor == nil || actor.User == nil || actor.User.ID == 0 {
		return nil, ErrUnauthorized
	}
	id := actor.Session
	if cliID != "" {
		// Only the actor's own: SessionForHeader matches by person.
		id = s.SessionForHeader(ctx, actor.id(), cliID, machine)
	}
	if id == 0 {
		return nil, notFound("session")
	}
	x, err := s.sessionByID(ctx, s.db.R, actor, id)
	if err != nil {
		return nil, err
	}
	if err := s.sessionMeta(ctx, actor, []*AgentSession{x}); err != nil {
		return nil, err
	}
	return x, nil
}

// ---- the summary ----

// SessionItem is a record a session touched.
type SessionItem struct {
	Kind   string    `json:"kind"`
	Label  string    `json:"label"` // WP-12, a page title, R-4
	Detail string    `json:"detail,omitempty"`
	URL    string    `json:"url,omitempty"`
	At     time.Time `json:"at"`
	Data   any       `json:"data,omitempty"`
}

// TimelineEntry is one line of a session's timeline.
type TimelineEntry struct {
	ID     int64     `json:"id"`
	Kind   string    `json:"kind"`
	Label  string    `json:"label,omitempty"`
	Detail string    `json:"detail,omitempty"`
	Text   string    `json:"text,omitempty"` // a prompt longer than its detail, whole (WP-860)
	URL    string    `json:"url,omitempty"`
	At     time.Time `json:"at"`
}

const (
	sessionPromptLine = 300      // of a prompt, on the timeline's line and in last_prompt
	sessionPromptMax  = 32 << 10 // a prompt is kept whole up to this many characters
)

// AgentSessionSummary is what a session did, as WP-D16 describes it: what it is
// doing, what it finished, what it put off, and what waits on a person.
type AgentSessionSummary struct {
	Session *AgentSession `json:"session"`
	Doing   struct {
		Title      string    `json:"title,omitempty"`
		Task       *IssueRef `json:"task,omitempty"`
		LastPrompt string    `json:"last_prompt,omitempty"`
		LastUpdate string    `json:"last_update,omitempty"`
	} `json:"doing"`
	Done struct {
		IssuesClosed  []SessionItem `json:"issues_closed"`
		IssuesCreated []SessionItem `json:"issues_created"`
		Comments      int           `json:"comments"`
		Pages         []SessionItem `json:"pages"`
		PullRequests  []SessionItem `json:"pull_requests"`
		Commits       []SessionItem `json:"commits"`
	} `json:"done"`
	// Deferred: issues it filed into the backlog or To Do, and requests
	// that were snoozed.
	Deferred struct {
		Issues   []SessionItem `json:"issues"`
		Requests []*Request    `json:"requests"`
	} `json:"deferred"`
	Waiting  []*Request      `json:"waiting"` // its open requests
	Requests []*Request      `json:"requests"`
	Runs     []SessionRun    `json:"runs"`
	Timeline []TimelineEntry `json:"timeline"`
}

// SessionRun is time an agent held an issue in this session.
type SessionRun struct {
	Issue        string     `json:"issue"`
	Title        string     `json:"title"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	Outcome      string     `json:"outcome,omitempty"`
	InputTokens  int64      `json:"input_tokens"`
	OutputTokens int64      `json:"output_tokens"`
}

// GetSession returns a session with its summary.
func (s *Service) GetAgentSession(ctx context.Context, actor *Actor, id int64) (*AgentSessionSummary, error) {
	x, err := s.sessionByID(ctx, s.db.R, actor, id)
	if err != nil {
		return nil, err
	}
	if err := s.sessionMeta(ctx, actor, []*AgentSession{x}); err != nil {
		return nil, err
	}
	sum := &AgentSessionSummary{Session: x}
	sum.Doing.Title, sum.Doing.Task, sum.Doing.LastPrompt, sum.Doing.LastUpdate = x.Title, x.Issue, x.LastPrompt, x.LastUpdate
	sum.Done.IssuesClosed, sum.Done.IssuesCreated, sum.Done.Pages = []SessionItem{}, []SessionItem{}, []SessionItem{}
	sum.Done.PullRequests, sum.Done.Commits, sum.Deferred.Issues = []SessionItem{}, []SessionItem{}, []SessionItem{}
	sum.Deferred.Requests, sum.Waiting, sum.Requests, sum.Runs, sum.Timeline = []*Request{}, []*Request{}, []*Request{}, []SessionRun{}, []TimelineEntry{}

	// Prompts are the owner's, as in sessionMeta: a colleague in the project
	// sees that a prompt came, not what it said.
	ownPrompts := x.userID == actor.id() || actor.isAdmin()
	// A refused call's command is read as a request's is: by the session's
	// person and its project's editors (WP-232).
	refusedText := ownPrompts
	if !refusedText && x.projectID.Valid {
		role, rerr := s.projectRole(ctx, s.db.R, actor, x.projectID.Int64)
		if rerr != nil {
			if readFault(rerr) {
				s.log.WarnContext(ctx, "project role failed", "projectid", x.projectID.Int64, "err", rerr)
			}
		}
		refusedText = role >= RoleEditor
	}
	refusedText = refusedText && !actor.HidesCommandText()
	// The latest entries; a long session keeps its newest 1,000.
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, kind, ref_type, label, url, detail, data, created_at FROM session_events WHERE session_id = ? ORDER BY id DESC LIMIT 1000`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []TimelineEntry{}
	for rows.Next() {
		var e TimelineEntry
		var refType, data string
		var at int64
		if err := rows.Scan(&e.ID, &e.Kind, &refType, &e.Label, &e.URL, &e.Detail, &data, &at); err != nil {
			return nil, err
		}
		e.At = msTime(at)
		if (e.Kind == "prompt" && !ownPrompts) || (e.Kind == "refused" && !refusedText) {
			e.Detail = ""
		} else if e.Kind == "prompt" && data != "{}" {
			var d struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal([]byte(data), &d)
			e.Text = d.Text
		}
		events = append(events, e)
		item := SessionItem{Kind: e.Kind, Label: e.Label, Detail: e.Detail, URL: e.URL, At: e.At}
		switch e.Kind {
		case "issue_closed":
			sum.Done.IssuesClosed = append(sum.Done.IssuesClosed, item)
		case "issue_created":
			sum.Done.IssuesCreated = append(sum.Done.IssuesCreated, item)
			var d struct {
				Category string `json:"category"`
			}
			_ = json.Unmarshal([]byte(data), &d)
			if d.Category == CatBacklog || d.Category == CatTodo {
				sum.Deferred.Issues = append(sum.Deferred.Issues, item)
			}
		case "comment":
			sum.Done.Comments++
		case "page", "design", "glossary":
			sum.Done.Pages = append(sum.Done.Pages, item)
		case "pr_linked":
			sum.Done.PullRequests = append(sum.Done.PullRequests, item)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sum.Timeline = events
	reqs, err := s.sessionRequests(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	now := s.nowMS()
	for _, r := range reqs {
		sum.Requests = append(sum.Requests, r)
		switch {
		case r.State == RequestOpen && r.snoozed(now):
			sum.Deferred.Requests = append(sum.Deferred.Requests, r)
		case r.State == RequestOpen:
			sum.Waiting = append(sum.Waiting, r)
		}
		if r.Result.Commit != "" {
			sum.Done.Commits = append(sum.Done.Commits, SessionItem{Kind: "commit", Label: shortSHA(r.Result.Commit), Detail: r.Title, URL: r.URL, At: r.UpdatedAt})
		}
		if r.Result.PRURL != "" {
			sum.Done.PullRequests = append(sum.Done.PullRequests, SessionItem{Kind: "pr", Label: r.Result.PRTitle, Detail: r.Title, URL: r.Result.PRURL, At: r.UpdatedAt})
		}
	}
	// Runs on issues from this session.
	rr, err := s.db.R.QueryContext(ctx, `SELECT p.key || '-' || i.number, i.title, r.started_at, r.ended_at, r.outcome, r.input_tokens, r.output_tokens
		FROM agent_runs r JOIN issues i ON i.id = r.issue_id JOIN projects p ON p.id = i.project_id WHERE r.session_id = ? ORDER BY r.id DESC LIMIT 50`, id)
	if err != nil {
		return nil, err
	}
	defer rr.Close()
	for rr.Next() {
		var run SessionRun
		var started int64
		var ended sql.NullInt64
		if err := rr.Scan(&run.Issue, &run.Title, &started, &ended, &run.Outcome, &run.InputTokens, &run.OutputTokens); err != nil {
			return nil, err
		}
		run.StartedAt, run.EndedAt = msTime(started), ptrTime(ended)
		sum.Runs = append(sum.Runs, run)
	}
	return sum, rr.Err()
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// ---- hooks ----

// SessionHook applies a batch of hook events from an agent's CLI, in order.
// The last event decides what comes back: at Stop (unless it is a Stop that
// already continued) and SessionStart, the decisions that arrived since the
// agent asked. It is one transaction: a hook is one HTTP call.
func (s *Service) SessionHook(ctx context.Context, actor *Actor, evs []HookEvent) (*HookResult, error) {
	if actor == nil || actor.User == nil || actor.User.ID == 0 {
		return nil, ErrUnauthorized
	}
	if len(evs) == 0 {
		return nil, invalid("no events")
	}
	if len(evs) > 200 {
		return nil, invalid("at most 200 events at once")
	}
	for _, ev := range evs {
		if strings.TrimSpace(ev.SessionID) == "" || len(ev.SessionID) > 200 {
			return nil, invalid("session_id is required")
		}
		if len(ev.Machine) > 200 || len(ev.Cwd) > 4096 {
			return nil, invalid("machine or cwd too long")
		}
	}
	res := &HookResult{}
	lastQuiet := false
	// A pause that winds sessions down (WP-146): every reply carries it for
	// the hooks' local copy, and a Stop isn't carried on.
	var perr error
	if res.Pause, perr = s.sessionPause(ctx, actor.id(), pausedAgent(actor)); perr != nil {
		return nil, perr
	}
	res.Guard = s.guardState(ctx, actor.id())
	pausedStop := res.Pause != nil && evs[len(evs)-1].Event == "Stop"
	var advance *stepRow  // /wp next's step (WP-562)
	var review *reviewAsk // /wp review's ask
	leftStop := false     // a Stop in a session its person stopped Waypoint in (WP-562)
	err := s.write(ctx, actor, func(t *tx) error {
		for n, ev := range evs {
			x, created, err := s.touchSession(ctx, t, actor, &ev)
			if err != nil {
				return err
			}
			res.Session = x.ID
			// Observational: a failure here is counted, never a reason to
			// refuse the batch (WP-1468).
			if err := s.recordObserved(ctx, t, actor, x, &ev); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				s.log.WarnContext(ctx, "record observed", "session", x.ID, "event", ev.Event, "err", err)
				if err := s.addDrop(ctx, t, actor, cmp(strings.TrimSpace(ev.Machine), "unknown"), "server_error", ev.Event, 1, s.hookTime(&ev)); err != nil {
					return err
				}
			}
			if created {
				var extra map[string]any
				if job := s.linkSpawned(ctx, t, actor, x, &ev); job != 0 {
					extra = map[string]any{"job": job}
				}
				t.emit(sessionEvent(EvSessionStarted, x, extra))
			}
			if err := s.applyHookEvent(ctx, t, actor, x, &ev); err != nil {
				return err
			}
			last := n == len(evs)-1
			if last && ev.kept != "" {
				res.Block, advance, review = ev.kept, ev.advance, ev.review
				continue // no turn: what would start one waits for the next prompt
			}
			if last {
				lastQuiet = ev.quiet
			}
			if last && ev.quiet && ev.Event != "Stop" {
				// A ping's prompt, or the start Waypoint's /compact ends with: news
				// waits for the stop, which carries the agent on with it (WP-192).
				continue
			}
			// A session its person stopped Waypoint in (/wp stop) is carried on
			// by nothing of Waypoint's at a Stop, as a paused one: news waits for
			// their next prompt (WP-562).
			if last && ev.Event == "Stop" {
				leftStop = sessionLeftAlone(ctx, t, x.ID)
			}
			held := pausedStop || leftStop
			// A person's answer, given in the web while the agent sat idle,
			// reaches it at the start of its next turn, not at the end of it.
			if last && (ev.Event == "SessionStart" || ev.Event == "UserPromptSubmit" || (ev.Event == "Stop" && !ev.StopHookActive && !held)) {
				// Answers that need no follow-up (WP-176) wait for the start of a
				// turn; a Stop that would continue the agent leaves them.
				res.Deliver, err = s.takeDecisions(ctx, t, actor, x, ev.Event == "SessionStart", ev.Event != "Stop")
				if err != nil {
					return err
				}
			}
			if last && (ev.Event == "SessionStart" || ev.Event == "UserPromptSubmit" || (ev.Event == "Stop" && !held)) {
				via := "stop"
				switch ev.Event {
				case "SessionStart":
					via = "start"
				case "UserPromptSubmit":
					via = "prompt"
				}
				if res.Messages, err = s.takeMessages(ctx, t, x, via); err != nil {
					return err
				}
			}
			if last && ev.Event == "Stop" {
				// A ping's stop leaves the last real stop's judgment as it was
				// (finished stays finished, with no second session.finished),
				// unless it carries the agent on with news (WP-192).
				if delivered := len(res.Deliver) > 0 || len(res.Messages) > 0; delivered || !ev.quiet {
					if err := s.settleStop(ctx, t, x, &ev, delivered); err != nil {
						return err
					}
				}
				if !ev.StopHookActive && len(res.Deliver) == 0 && len(res.Messages) == 0 {
					if done, err := s.taskSessionDone(ctx, t, x.ID); err != nil {
						s.log.WarnContext(ctx, "task session done", "session", x.ID, "err", err)
					} else {
						res.End = done
					}
				}
				if !ev.StopHookActive && !ev.quiet && len(res.Deliver) == 0 && len(res.Messages) == 0 && len(runningTasks(ev.Pending)) == 0 {
					// A note for the person only: failing it never fails the Stop.
					// Not while the session still works (news to read, background
					// tasks running): "done" would misread there; a later Stop says it.
					if note, err := s.bigSessionNote(ctx, t, x.ID); err != nil {
						s.log.WarnContext(ctx, "big session note", "session", x.ID, "err", err)
					} else {
						res.Note = note
					}
				}
				switch {
				case pausedStop:
					res.Note = pausedStopNote(res.Pause)
				case leftStop:
					// Like the autopilot branch, it never fails the Stop.
					if err := s.stopStep(ctx, t, x.ID, "left", "you stopped Waypoint in the session"); err != nil {
						s.log.WarnContext(ctx, "stop step", "session", x.ID, "err", err)
					}
				case !ev.StopHookActive && !ev.quiet && len(res.Deliver) == 0 && len(res.Messages) == 0:
					// Autopilot (WP-ADR9): a finished session moves on after the grace.
					// Like the size note, it never fails the Stop.
					//wplint:ignore writeclosure agentWork can queue a background safety screen of a candidate task; it screens the task, not this Stop, and records only its own result
					if note, err := s.autopilotAtStop(ctx, t, x, &ev); err != nil {
						s.log.WarnContext(ctx, "autopilot at stop", "session", x.ID, "err", err)
					} else if note != "" {
						res.Note = note // it says more than the size note: the session is about to be cleared
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if review != nil {
		res.Block = s.reviewProgress(ctx, review)
	}
	if advance != nil {
		st := advance
		s.goBG(func() {
			if err := s.advanceStep(context.WithoutCancel(ctx), st); err != nil {
				if readFault(err) && !missingOrDenied(err) {
					s.log.WarnContext(ctx, "advance step failed", "session", st.sessionID, "err", err)
				}
			}
		})
	}
	res.Text = joinText(DecisionsText(res.Deliver), MessagesText(res.Messages))
	if evs[len(evs)-1].Event == "SessionStart" && !lastQuiet {
		res.Text = joinText(res.Text, s.autopilotSessionNote(ctx, res.Session, 0))
	}
	if last := evs[len(evs)-1]; (last.Event == "SessionStart" || last.Event == "UserPromptSubmit") && last.Repo != "" {
		// Each prompt too, so a vouch a person answers mid-session reaches
		// the guard at the agent's next turn (WP-1087).
		res.EnvHosts = s.EnvGuardHosts(ctx, actor, last.Repo)
	}
	if last := evs[len(evs)-1]; last.Event == "Stop" && !lastQuiet && !pausedStop && !leftStop && res.Text == "" && s.followUpTalks(ctx, res.Session) {
		res.Await = int(stopAwait / time.Second)
		s.expectAwait(res.Session)
	}
	if res.Text != "" || res.Await > 0 {
		res.End = false // it carries on, or waits for a follow-up's message
	}
	s.publishSession(ctx, res.Session)
	return res, nil
}

// joinText joins the non-empty parts of what an agent is told.
func joinText(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, "\n\n")
}

// sessionEvent builds the event for a session, scoped so only people who
// may see it receive it.
func sessionEvent(kind string, x *AgentSession, extra map[string]any) *Event {
	d := map[string]any{"session": x.ID, "machine": x.Machine, "repo": x.Repo, "branch": x.Branch, "agent": x.Agent}
	for k, v := range extra {
		d[k] = v
	}
	e := &Event{Kind: kind, Data: d}
	if x.projectID.Valid {
		e.ProjectID = x.projectID.Int64
	} else {
		e.UserID = x.userID
	}
	return e
}

// publishSession tells live clients a session changed, and the automation
// runs waiting for it to (WP-D18: "still waiting after 15 minutes" waits for
// the session to be updated into another state). Automations are not
// triggered by it: session.waiting and the others carry that.
func (s *Service) publishSession(ctx context.Context, id int64) {
	var user int64
	var project sql.NullInt64
	var state string
	if err := s.db.R.QueryRowContext(ctx, `SELECT user_id, project_id, state FROM agent_sessions WHERE id = ?`, id).Scan(&user, &project, &state); err != nil {
		return
	}
	data := map[string]any{"session": id, "state": state}
	s.Hub.Publish(&Event{Kind: EvSessionUpdated, UserID: user, Data: data})
	if project.Valid {
		s.Hub.Publish(&Event{Kind: EvSessionUpdated, ProjectID: project.Int64, Data: data})
	}
	e := &Event{Kind: EvSessionUpdated, ProjectID: project.Int64, At: s.now().UTC(), Data: data}
	s.matchWaits(context.WithoutCancel(ctx), e)
}

// hookTime is when an event happened: its own time if the CLI queued it,
// within reason, else now.
func (s *Service) hookTime(ev *HookEvent) int64 {
	now := s.nowMS()
	if ev.At > 0 && ev.At <= now+60_000 && ev.At > now-sessionEndedAfter.Milliseconds()*7 {
		return ev.At
	}
	return now
}

// touchSession registers the session (or updates its place and last-seen
// time) for a hook event.
func (s *Service) touchSession(ctx context.Context, t *tx, actor *Actor, ev *HookEvent) (*AgentSession, bool, error) {
	at := s.hookTime(ev)
	machine := cmp(strings.TrimSpace(ev.Machine), "unknown")
	x, err := scanSession(t.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM agent_sessions s WHERE s.user_id = ? AND s.machine = ? AND s.cli_id = ?`,
		actor.id(), machine, ev.SessionID), at)
	created := false
	if errors.Is(err, sql.ErrNoRows) {
		created = true
		// Who started it decides its kind (WP-34): a launcher sets WP_TASK.
		kind, keys := SessionYours, ""
		if tasks := splitTaskKeys(ev.Task); len(tasks) > 0 {
			kind, keys = SessionTask, strings.Join(tasks, ",")
		}
		res, err := t.ExecContext(ctx, `INSERT INTO agent_sessions(user_id, machine, cli_id, cli, agent, started_at, last_seen_at, kind, task_keys, started_server, started_wp)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			actor.id(), machine, ev.SessionID, cmp(ev.CLI, "claude"), cmp(actor.agentName(), "anthropic/claude-code"), at, at, kind, keys, s.cfg.Version, actor.ClientVersion)
		if err != nil {
			return nil, false, err
		}
		id, _ := res.LastInsertId()
		if x, err = scanSession(t.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM agent_sessions s WHERE s.id = ?`, id), at); err != nil {
			return nil, false, err
		}
	} else if err != nil {
		return nil, false, err
	}
	sets, args := []string{"last_seen_at = max(last_seen_at, ?)"}, []any{at}
	if ev.Cwd != "" && ev.Cwd != x.Cwd {
		sets, args = append(sets, "cwd = ?"), append(args, ev.Cwd)
		x.Cwd = ev.Cwd
	}
	if ev.Repo != "" && !strings.EqualFold(ev.Repo, x.Repo) || (ev.Repo != "" && !x.projectID.Valid) {
		sets, args = append(sets, "repo = ?"), append(args, ev.Repo)
		x.Repo = ev.Repo
		var pid sql.NullInt64
		if p := s.projectForRepo(ctx, t, actor, ev.Repo); p != 0 {
			pid = sql.NullInt64{Int64: p, Valid: true}
		}
		sets, args = append(sets, "project_id = ?"), append(args, nullInt(ptrInt(pid)))
		x.projectID = pid
	}
	if ev.Branch != "" && ev.Branch != x.Branch {
		sets, args = append(sets, "branch = ?"), append(args, ev.Branch)
		x.Branch = ev.Branch
	}
	if ev.Cwd != "" || ev.Repo != "" {
		sets, args = append(sets, "worktree = ?"), append(args, boolInt(ev.Worktree))
		x.Worktree = ev.Worktree
	}
	if ev.Model != "" && ev.Model != x.Model {
		sets, args = append(sets, "model = ?"), append(args, ev.Model)
		x.Model = ev.Model
	}
	if a := actor.agentName(); a != "" && a != x.Agent && created {
		x.Agent = a
	}
	// #nosec G202 -- fixed column assignments; values are bound
	if _, err := t.ExecContext(ctx, `UPDATE agent_sessions SET `+strings.Join(sets, ", ")+` WHERE id = ?`, append(args, x.ID)...); err != nil {
		return nil, false, err
	}
	if seen := x.LastSeenAt.UnixMilli(); at > seen {
		x.LastSeenAt = msTime(at)
	}
	if created {
		if err := s.sessionTimeline(ctx, t, x.ID, at, "started", "", 0, "", "", ev.Source, x.projectID, nil); err != nil {
			return nil, false, err
		}
		if x.Kind == SessionTask {
			if err := s.adoptLaunchClaims(ctx, t, actor, x); err != nil {
				return nil, false, err
			}
		}
	}
	return x, created, nil
}

// adoptLaunchClaims: a task session's launcher took its tasks for it before
// it started (wp start, a runner's session job: WP-1037), as the agent the
// session runs as, to give it the answer as its first prompt. The session
// didn't exist yet, so those runs have none; from its first hook on they are
// this session's, as if it had taken the tasks itself.
//
//   - Taking an epic hands out one of its open sub-tasks (takeUnder), so the
//     run may be on a task under the bound one.
//   - Starting a new session on a task another of the agent's sessions still
//     holds keeps that run (the launcher's take has no session to move it
//     to); the new session takes it over, as its own wp next KEY would
//     (renewClaim, WP-89). Only the bound task itself: a sub-task another
//     session holds was not this launch's.
func (s *Service) adoptLaunchClaims(ctx context.Context, t *tx, actor *Actor, x *AgentSession) error {
	now := s.nowMS()
	for _, key := range x.Tasks {
		id, err := s.resolveIssueID(ctx, t, key)
		if err != nil {
			continue // a binding to a task that isn't there: nothing was taken
		}
		var run, issue int64
		err = t.QueryRowContext(ctx, `WITH RECURSIVE d(id) AS (SELECT ? UNION SELECT i.id FROM issues i JOIN d ON i.parent_id = d.id)
			SELECT r.id, r.issue_id FROM agent_runs r JOIN issues i ON i.id = r.issue_id
			WHERE r.issue_id IN (SELECT id FROM d) AND r.agent = ? AND r.user_id = ?
			  AND i.claimed_by = ? AND i.claimed_until > ?
			  AND r.id = (SELECT max(id) FROM agent_runs WHERE issue_id = r.issue_id AND agent = r.agent)
			  AND (r.ended_at IS NULL OR r.outcome = 'paused')
			  AND (r.session_id IS NULL OR (r.issue_id = ? AND r.session_id != ?))
			ORDER BY r.session_id IS NULL DESC, r.id DESC LIMIT 1`,
			id, x.Agent, x.userID, x.Agent, now, id, x.ID).Scan(&run, &issue)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `UPDATE agent_runs SET session_id = ? WHERE id = ?`, x.ID, run); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `UPDATE agent_sessions SET issue_id = ? WHERE id = ?`, issue, x.ID); err != nil {
			return err
		}
		x.issueID = sql.NullInt64{Int64: issue, Valid: true}
		s.recordRunChanges(ctx, t, actor, issue, x.Agent)
	}
	return nil
}

// projectForRepo is the project (that the person can see) a repo is attached
// to; 0 if none.
func (s *Service) projectForRepo(ctx context.Context, q queryer, actor *Actor, repo string) int64 {
	ids, err := queryIDs(ctx, q, `SELECT DISTINCT project_id FROM project_repos WHERE repo = ? COLLATE NOCASE ORDER BY project_id`, repo)
	if err != nil {
		return 0
	}
	for _, id := range ids {
		r, rerr := s.projectRole(ctx, q, actor, id)
		if rerr != nil {
			if readFault(rerr) {
				s.log.WarnContext(ctx, "project role failed", "id", id, "err", rerr)
			}
		}
		if r > RoleNone {
			return id
		}
	}
	return 0
}

// sessionTimeline appends to a session's timeline.
func (s *Service) sessionTimeline(ctx context.Context, q queryer, sessionID, at int64, kind, refType string, refID int64, label, url, detail string, project sql.NullInt64, data map[string]any) error {
	d := "{}"
	if len(data) > 0 {
		b, _ := json.Marshal(data)
		d = string(b)
	}
	var ref any
	if refID != 0 {
		ref = refID
	}
	_, err := q.ExecContext(ctx, `INSERT INTO session_events(session_id, kind, ref_type, ref_id, label, url, detail, project_id, data, created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		sessionID, kind, refType, ref, label, url, excerpt(detail, 500), nullInt(ptrInt(project)), d, at)
	return err
}

// applyHookEvent applies one event to the session row (state, excerpts,
// totals) and its side effects (requests).
func (s *Service) applyHookEvent(ctx context.Context, t *tx, actor *Actor, x *AgentSession, ev *HookEvent) error {
	at := s.hookTime(ev)
	setState := func(state, reason string) error {
		_, err := t.ExecContext(ctx, `UPDATE agent_sessions SET state = ?, wait_reason = ? WHERE id = ? AND last_seen_at <= ?`, state, reason, x.ID, at+1)
		return err
	}
	// The prompt cache (WP-192): a keep-warm ping, or Waypoint's /compact,
	// is no turn of the session's work, and leaves it as it was.
	var err error
	switch ev.Event {
	case "SessionStart":
		ev.quiet, err = s.warmSessionStart(ctx, t, x, ev, at)
	case "UserPromptSubmit":
		if ev.typed, err = s.recogniseTyped(ctx, t, x.ID, ev); err == nil {
			ev.quiet, err = s.warmQuietPrompt(ctx, t, x, ev)
		}
	}
	if err != nil || ev.quiet {
		return err
	}
	switch ev.Event {
	case "SessionStart", "UserPromptSubmit":
		// Working again: no longer finished (WP-D22).
		if _, err := t.ExecContext(ctx, `UPDATE agent_sessions SET finished_at = NULL, pending = '' WHERE id = ?`, x.ID); err != nil {
			return err
		}
	}
	switch ev.Event {
	case "SessionStart":
		if _, err := t.ExecContext(ctx, `UPDATE agent_sessions SET ended_at = NULL WHERE id = ?`, x.ID); err != nil {
			return err
		}
		if x.stored == SessionEnded {
			// resumed: same row
			if err := s.sessionTimeline(ctx, t, x.ID, at, "started", "", 0, "", "", "resumed", x.projectID, nil); err != nil {
				return err
			}
		}
		if ev.Source == "clear" {
			if err := s.carryOverClear(ctx, t, x, at, false); err != nil {
				return err
			}
		}
		if err := s.takeProjectNotices(ctx, t, x, at); err != nil { // WP-378
			return err
		}
		if ev.Source != "compact" {
			if err := s.noteStrandedHere(ctx, t, actor, x, ev); err != nil { // WP-567: never fails the start
				s.log.WarnContext(ctx, "stranded work here", "session", x.ID, "err", err)
			}
			if line := s.unpushedHere(ctx, t, cmp(ev.Repo, x.Repo), cleanCheckouts(ev.Checkouts), s.nowMS()); line != "" { // WP-599: never fails the start
				if _, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO session_messages(session_id, body, step_key, created_by, created_at, passive) VALUES (?,?,?,NULL,?,1)`,
					x.ID, line, "unpushed:"+itoa64(x.ID), s.nowMS()); err != nil {
					s.log.WarnContext(ctx, "unpushed at session start", "session", x.ID, "err", err)
				}
			}
		}
		return setState(SessionWorking, "")
	case "UserPromptSubmit":
		if kept, err := s.sessionCommand(ctx, t, x, ev); err != nil {
			if SessionCommand(ev.Prompt) != "" {
				return err // nothing half done; the CLI says Waypoint didn't answer
			}
			s.log.WarnContext(ctx, "autopilot keep", "session", x.ID, "err", err) // the prompt goes on as any other
		} else if kept != "" {
			ev.kept = kept
			// Never a turn. keep is its person taking the session; Waypoint's
			// other commands (status, review, next, stop, continue) only read
			// or steer it, so an autopilot session stays autopilot's (WP-1319).
			if c := SessionCommand(ev.Prompt); c != "" && c != "keep" {
				return nil
			}
			return s.warmKept(ctx, t, x, at)
		}
		if err := s.warmPromptStarted(ctx, t, x, ev, at); err != nil {
			return err
		}
		sets, args := []string{}, []any{}
		if p := strings.TrimSpace(ev.Prompt); p != "" {
			sets, args = append(sets, "last_prompt = ?"), append(args, excerpt(p, sessionPromptLine))
		}
		if title := strings.TrimSpace(ev.Title); title != "" {
			sets, args = append(sets, "title = ?"), append(args, excerpt(title, 200))
		}
		if len(sets) > 0 {
			// #nosec G202 -- fixed column assignments; values are bound
			if _, err := t.ExecContext(ctx, `UPDATE agent_sessions SET `+strings.Join(sets, ", ")+` WHERE id = ?`, append(args, x.ID)...); err != nil {
				return err
			}
		}
		if ev.Prompt != "" {
			// The line shows the start; a longer prompt is kept whole beside
			// it (WP-860), as what an agent was asked is part of the record.
			var data map[string]any
			if p := strings.TrimSpace(ev.Prompt); utf8.RuneCountInString(p) > sessionPromptLine {
				data = map[string]any{"text": clipRunes(p, sessionPromptMax)}
			}
			if err := s.sessionTimeline(ctx, t, x.ID, at, "prompt", "", 0, "", "", excerpt(ev.Prompt, sessionPromptLine), x.projectID, data); err != nil {
				return err
			}
			if ev.typed != "" {
				if _, err := t.ExecContext(ctx, `UPDATE session_events SET typed_kind = ? WHERE id = (SELECT max(id) FROM session_events WHERE session_id = ? AND kind = 'prompt')`, ev.typed, x.ID); err != nil {
					return err
				}
			}
		}
		if err := s.noteNamedRefs(ctx, t, x, ev.Prompt, at); err != nil { // WP-378
			return err
		}
		if err := s.takeProjectNotices(ctx, t, x, at); err != nil {
			return err
		}
		// A new turn: the refused calls and questions of earlier ones are over (WP-66).
		if err := s.expireOnPrompt(ctx, t, x.ID, at); err != nil {
			return err
		}
		if err := s.autopilotOnPrompt(ctx, t, x, ev); err != nil {
			s.log.WarnContext(ctx, "autopilot on prompt", "session", x.ID, "err", err) // never fails the prompt
		}
		return setState(SessionWorking, "")
	case "Notification":
		switch ev.NotificationType {
		case "idle_prompt", "permission_prompt", "agent_needs_input", "elicitation_dialog":
			if err := setState(SessionWaiting, ev.NotificationType); err != nil {
				return err
			}
			if err := s.sessionTimeline(ctx, t, x.ID, at, "waiting", "", 0, "", "", cmp(ev.Message, ev.NotificationType), x.projectID, map[string]any{"reason": ev.NotificationType}); err != nil {
				return err
			}
			t.emit(sessionEvent(EvSessionWaiting, x, map[string]any{"reason": ev.NotificationType}))
		}
		return nil
	case "PermissionDenied":
		// Logged for audit, not asked (WP-232, WP-ADR115).
		return s.logRefusal(ctx, t, actor, x, ev, at)
	case "PreToolUse":
		if ev.Tool == "AskUserQuestion" {
			if err := s.requestFromAskUserQuestion(ctx, t, actor, x, ev, at); err != nil {
				return err
			}
			return setState(SessionWaiting, "agent_needs_input")
		}
		return nil
	case "PostToolUse":
		if ev.Tool == "AskUserQuestion" {
			if err := s.answeredInTerminal(ctx, t, x, ev, at); err != nil {
				return err
			}
			return setState(SessionWorking, "")
		}
		// A tool call that ran may be the very thing a request asked a person
		// to approve (WP-126).
		return s.closeRanInSession(ctx, t, x, ev)
	case "Stop":
		if ev.quiet, err = s.warmStop(ctx, t, x, ev, at); err != nil {
			return err
		}
		if err := s.recordWorkState(ctx, t, x, ev, at); err != nil { // WP-567
			return err
		}
		sets, args := []string{}, []any{}
		if m := strings.TrimSpace(ev.LastMessage); m != "" && !ev.quiet {
			sets, args = append(sets, "last_update = ?"), append(args, excerpt(m, 2000))
		}
		if ev.InputTokens > 0 || ev.OutputTokens > 0 {
			sets, args = append(sets, "input_tokens = max(input_tokens, ?)", "output_tokens = max(output_tokens, ?)"), append(args, ev.InputTokens, ev.OutputTokens)
		}
		if ev.ContextTokens > 0 {
			// The latest, not the largest: /compact shrinks it.
			sets, args = append(sets, "context_tokens = ?"), append(args, ev.ContextTokens)
		}
		if len(sets) > 0 {
			// #nosec G202 -- fixed column assignments; values are bound
			if _, err := t.ExecContext(ctx, `UPDATE agent_sessions SET `+strings.Join(sets, ", ")+` WHERE id = ?`, append(args, x.ID)...); err != nil {
				return err
			}
		}
		if ev.quiet {
			// A ping's "ok": nothing the session did.
			return setState(SessionIdle, "")
		}
		if m := strings.TrimSpace(ev.LastMessage); m != "" {
			var data map[string]any
			if ev.Reason == StopEndedByShip {
				// A hook ended the turn at wp ship with its --summary: no
				// model call wrote this message (WP-1093).
				data = map[string]any{"ended_by": StopEndedByShip}
			}
			if err := s.sessionTimeline(ctx, t, x.ID, at, "update", "", 0, "", "", excerpt(m, 500), x.projectID, data); err != nil {
				return err
			}
		}
		if err := s.noteNamedRefs(ctx, t, x, ev.LastMessage, at); err != nil { // WP-378
			return err
		}
		if err := s.statementsAtStop(ctx, t, x, ev.LastMessage, at); err != nil { // WP-715: never fails the Stop
			s.log.WarnContext(ctx, "claim check", "session", x.ID, "err", err)
		}
		if err := s.autoFileQuestion(ctx, t, actor, x, ev.LastMessage, at); err != nil { // WP-96
			return err
		}
		if err := s.recordTaskSummary(ctx, t, actor, x, ev.LastMessage, at); err != nil { // WP-194: never fails the Stop
			s.log.WarnContext(ctx, "task summary", "session", x.ID, "err", err)
		}
		// Keep the timeline bounded.
		if _, err := t.ExecContext(ctx, `DELETE FROM session_events WHERE session_id = ? AND id < (SELECT id FROM session_events WHERE session_id = ? ORDER BY id DESC LIMIT 1 OFFSET 1500)`, x.ID, x.ID); err != nil {
			return err
		}
		return setState(SessionIdle, "")
	case "SubagentStart", "SubagentStop":
		if err := s.sessionTimeline(ctx, t, x.ID, at, "subagent", "", 0, "", "", strings.ToLower(strings.TrimPrefix(ev.Event, "Subagent"))+" "+ev.AgentType, x.projectID,
			map[string]any{"agent_id": ev.AgentID, "agent_type": ev.AgentType}); err != nil {
			return err
		}
		if ev.Event == "SubagentStop" {
			return s.recordOwnReview(ctx, t, actor, x, ev, at) // WP-1089
		}
		return nil
	case "SessionEnd":
		if ev.InputTokens > 0 || ev.OutputTokens > 0 {
			if _, err := t.ExecContext(ctx, `UPDATE agent_sessions SET input_tokens = max(input_tokens, ?), output_tokens = max(output_tokens, ?) WHERE id = ?`,
				ev.InputTokens, ev.OutputTokens, x.ID); err != nil {
				return err
			}
		}
		if ev.ContextTokens > 0 {
			if _, err := t.ExecContext(ctx, `UPDATE agent_sessions SET context_tokens = ? WHERE id = ?`, ev.ContextTokens, x.ID); err != nil {
				return err
			}
		}
		if _, err := t.ExecContext(ctx, `UPDATE agent_sessions SET state = 'ended', wait_reason = '', ended_at = ? WHERE id = ?`, at, x.ID); err != nil {
			return err
		}
		if err := s.sessionTimeline(ctx, t, x.ID, at, "ended", "", 0, "", "", ev.EndReason, x.projectID, nil); err != nil {
			return err
		}
		if err := s.expireOnEnd(ctx, t, x.ID, at); err != nil { // WP-66
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM typed_prompts WHERE session_id = ?`, x.ID); err != nil { // WP-1370
			return err
		}
		if err := warmEnded(ctx, t, x.ID, at, ev.EndReason); err != nil { // WP-1000
			return err
		}
		// Autopilot's own /exit or /clear ends it while its step is starting
		// (the job reports how that went, and the task stays reserved for the
		// new session); any other end takes the session out of autopilot's
		// hands.
		var starting int
		if err := t.QueryRowContext(ctx, `SELECT count(*) FROM autopilot_steps WHERE session_id = ? AND state = ?`, x.ID, StepStarting).Scan(&starting); err != nil {
			return err
		}
		if starting == 0 {
			// A task it leaves unfinished with work in its checkout goes back
			// to To Do saying where (WP-567), before a /clear hands the rest on.
			if err := s.strandOnEnd(ctx, t, x, ev, at); err != nil {
				return err
			}
		}
		// Commits it left that its pull request hasn't got (WP-599).
		// A failure undoes all of it, so the sweep tells it later.
		if err := t.part(ctx, func() error {
			return s.tellUnpushed(ctx, t, x, s.loadWorkState(ctx, t, x.ID), endedHow(ev.EndReason))
		}); err != nil {
			s.log.WarnContext(ctx, "unpushed at session end", "session", x.ID, "err", err)
		}
		if ev.EndReason == "clear" {
			if err := s.carryOverClear(ctx, t, x, at, true); err != nil {
				return err
			}
		}
		// A clear takes the session: its step stops (WP-1319).
		reason, why := "ended", "the session ended"
		if ev.EndReason == "clear" {
			reason, why = StopCleared, "you cleared the session"
		}
		starting = 0
		_ = t.QueryRowContext(ctx, `SELECT count(*) FROM autopilot_steps WHERE session_id = ? AND state = ?`, x.ID, StepStarting).Scan(&starting)
		if starting == 0 {
			if err := s.stopStep(ctx, t, x.ID, reason, why); err != nil {
				s.log.WarnContext(ctx, "autopilot at session end", "session", x.ID, "err", err)
			}
		}
		t.emit(sessionEvent(EvSessionEnded, x, map[string]any{"reason": ev.EndReason}))
		return nil
	}
	return nil
}

// clearWindow is how far apart the two halves of a /clear may be heard.
const clearWindow = 2 * time.Minute

// carryOverClear: /clear ends a Claude Code session and starts another in
// the same terminal, under a new id. What Waypoint holds for the old one
// goes to the new one: its babysits and their news not yet delivered, and
// the tasks it was working on (WP-253). Autopilot's step doesn't go: a
// person who clears a session has taken it (WP-1319, which ends WP-307's
// carrying it over). The two hooks can arrive in either
// order, so each looks for the other half: ended says x is the old session,
// otherwise x is the new one.
func (s *Service) carryOverClear(ctx context.Context, t *tx, x *AgentSession, at int64, ended bool) error {
	var from, to int64
	var err error
	if ended {
		from = x.ID
		err = t.QueryRowContext(ctx, `SELECT s.id FROM agent_sessions s WHERE s.user_id = ? AND s.machine = ? AND s.cwd = ? AND s.id != ? AND s.ended_at IS NULL
			AND s.started_at BETWEEN ? AND ? AND EXISTS (SELECT 1 FROM session_events e WHERE e.session_id = s.id AND e.kind = 'started' AND e.detail = 'clear')
			ORDER BY s.started_at DESC LIMIT 1`, x.userID, x.Machine, x.Cwd, x.ID, at-clearWindow.Milliseconds(), at+clearWindow.Milliseconds()).Scan(&to)
	} else {
		to = x.ID
		err = t.QueryRowContext(ctx, `SELECT s.id FROM agent_sessions s WHERE s.user_id = ? AND s.machine = ? AND s.cwd = ? AND s.id != ? AND s.state = 'ended'
			AND s.ended_at BETWEEN ? AND ? AND EXISTS (SELECT 1 FROM session_events e WHERE e.session_id = s.id AND e.kind = 'ended' AND e.detail = 'clear')
			ORDER BY s.ended_at DESC LIMIT 1`, x.userID, x.Machine, x.Cwd, x.ID, at-clearWindow.Milliseconds(), at+clearWindow.Milliseconds()).Scan(&from)
	}
	if errors.Is(err, sql.ErrNoRows) || x.Cwd == "" {
		return nil // the other half isn't here yet (it will find this one), or no terminal to match
	} else if err != nil {
		return err
	}
	for _, q := range []string{
		`UPDATE babysits SET session_id = ? WHERE session_id = ? AND ended_at IS NULL`,
		`UPDATE session_messages SET session_id = ? WHERE session_id = ? AND state = 'queued' AND step_key LIKE 'babysit:%'`,
		// Paused tasks too (WP-566): /clear while idle still holds them.
		`UPDATE agent_runs SET session_id = ? WHERE session_id = ? AND (ended_at IS NULL OR (outcome = 'paused'
			AND id = (SELECT max(id) FROM agent_runs r WHERE r.issue_id = agent_runs.issue_id AND r.agent = agent_runs.agent)))`,
	} {
		if _, err := t.ExecContext(ctx, q, to, from); err != nil {
			return err
		}
	}
	return nil
}

// ---- what an actor with a session does ----

// afterDispatch runs the parts of dispatch that belong to agent sessions and
// requests (WP-D16): runner jobs that finish update the requests they ran
// for, and what an actor does in a session is recorded on it.
func (s *Service) afterDispatch(ctx context.Context, actor *Actor, events []*Event) {
	for _, e := range events {
		switch e.Kind {
		case EvPRUpdated, EvPRActivity, EvExternalPRMerged, EvIssueStatus:
			s.nudgeRequestWatches() // WP-126
		case EvRunnerJobFinished:
			s.watchJobFinished(ctx, e)
			s.requestJobFinished(ctx, e)
			s.babysitJobFinished(ctx, e) // WP-134
			s.sessionMessageJobFinished(ctx, e)
			s.autopilotJobFinished(ctx, e)  // WP-194
			s.auditJobFinished(ctx, e)      // WP-348
			s.cacheWarmJobFinished(ctx, e)  // WP-192
			s.reviewCompactFinished(ctx, e) // WP-761
			s.prepAgentJobFinished(ctx, e)  // WP-250
		case EvCodeAudit:
			s.codeAuditEvent(ctx, e)
		case EvRunnerJobUpdated:
			s.requestJobStarted(ctx, e)
		case EvRequestCreated, EvRequestUpdated:
			s.notifyRequestEvent(ctx, e)
		}
		switch e.Kind {
		case EvSessionFinished, EvRequestUpdated:
			s.followUpEvents(ctx, e) // WP-D22
		}
	}
	if actor != nil && actor.Session != 0 {
		s.recordSessionEvents(ctx, actor, events)
	}
}

// recordSessionEvents notes what an actor did in its session, from the
// events its changes emitted (one batched insert).
func (s *Service) recordSessionEvents(ctx context.Context, actor *Actor, events []*Event) {
	type row struct {
		kind, refType, label, url, detail string
		refID                             int64
		project                           int64
		data                              map[string]any
	}
	var rows []row
	var claimed int64
	for _, e := range events {
		switch e.Kind {
		case EvIssueCreated:
			rows = append(rows, row{"issue_created", "issue", e.IssueKey, "/issue/" + e.IssueKey, e.New, e.IssueID, e.ProjectID,
				map[string]any{"category": e.Data["category"]}})
		case EvIssueStatus:
			if cat, _ := e.Data["new_category"].(string); isClosed(cat) {
				if old, _ := e.Data["old_category"].(string); !isClosed(old) {
					rows = append(rows, row{"issue_closed", "issue", e.IssueKey, "/issue/" + e.IssueKey, e.New, e.IssueID, e.ProjectID, nil})
				}
			}
		case EvIssueCommented:
			rows = append(rows, row{"comment", "issue", e.IssueKey, "/issue/" + e.IssueKey, e.New, e.IssueID, e.ProjectID, nil})
		case EvIssueLinked:
			if e.Field == "pull" {
				rows = append(rows, row{"pr_linked", "issue", e.IssueKey, e.New, e.IssueKey + " linked to " + e.New, e.IssueID, e.ProjectID, nil})
			}
		case EvIssueUpdated:
			if e.Field == "claim" && e.New != "" {
				claimed = e.IssueID
				rows = append(rows, row{"claimed", "issue", e.IssueKey, "/issue/" + e.IssueKey, "", e.IssueID, e.ProjectID, nil})
			}
		}
	}
	if len(rows) == 0 {
		return
	}
	at := s.nowMS()
	err := func() error {
		sqlTx, err := s.db.W.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = sqlTx.Rollback() }()
		for _, r := range rows {
			if err := s.sessionTimeline(ctx, sqlTx, actor.Session, at, r.kind, r.refType, r.refID, r.label, r.url, r.detail,
				sql.NullInt64{Int64: r.project, Valid: r.project != 0}, r.data); err != nil {
				return err
			}
		}
		if claimed != 0 {
			if _, err := sqlTx.ExecContext(ctx, `UPDATE agent_sessions SET issue_id = ? WHERE id = ?`, claimed, actor.Session); err != nil {
				return err
			}
		}
		return sqlTx.Commit()
	}()
	if err != nil {
		s.log.WarnContext(ctx, "record session events", "session", actor.Session, "err", err)
		return
	}
	s.publishSession(ctx, actor.Session)
}

// noteSession records something an actor did in its session that isn't an
// event (a page written, a glossary term, a design note).
func (s *Service) noteSession(ctx context.Context, actor *Actor, kind, refType string, refID int64, label, url string, project *int64) {
	if actor == nil || actor.Session == 0 {
		return
	}
	var pid sql.NullInt64
	if project != nil {
		pid = sql.NullInt64{Int64: *project, Valid: true}
	}
	if err := s.sessionTimeline(ctx, s.db.W, actor.Session, s.nowMS(), kind, refType, refID, label, url, "", pid, nil); err != nil {
		s.log.WarnContext(ctx, "record session event", "kind", kind, "err", err)
		return
	}
	s.publishSession(ctx, actor.Session)
}
