package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core/udiff"
	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Request kinds.
const (
	RequestQuestion   = "question"
	RequestChange     = "change"
	RequestCommand    = "command"
	RequestPermission = "permission"
	RequestAction     = "action"
)

// RequestKinds in the order people read them.
var RequestKinds = []string{RequestQuestion, RequestChange, RequestCommand, RequestPermission, RequestAction}

// Request states: open, then one decision; changes go on through applying.
const (
	RequestOpen      = "open"
	RequestAnswered  = "answered"
	RequestApproved  = "approved"
	RequestDenied    = "denied"
	RequestDone      = "done"
	RequestDismissed = "dismissed"
	RequestWithdrawn = "withdrawn"
	RequestApplying  = "applying"
	RequestApplied   = "applied"
	RequestFailed    = "failed"
	RequestConflict  = "conflict"
	RequestExpired   = "expired"
)

// decidedStates are the states an agent is told about.
var decidedStates = []string{RequestAnswered, RequestApproved, RequestDenied, RequestDone, RequestDismissed, RequestApplied, RequestFailed, RequestConflict}

const (
	maxRequestBody = 40_000
	// maxRequestTitle: a longer title is split into a summary and the details.
	maxRequestTitle     = 200
	maxRequestFile      = 512 << 10
	maxRequestFiles     = 50
	maxRequestTotal     = 1 << 20
	maxCellOutput       = 256 << 10
	permissionDedupe    = 10 * time.Minute
	maxRequestsOpenIn   = 200
	maxRequestQuestions = 8
)

// errRequestsFull is what a request refused for the open cap matches (errors.Is),
// so a caller that must not fail on that can tell it from any other invalid input.
var errRequestsFull = errors.New("too many open requests")

type requestsFullError struct{ invalid error }

func (e *requestsFullError) Error() string   { return e.invalid.Error() }
func (e *requestsFullError) Unwrap() []error { return []error{errRequestsFull, e.invalid} }

// Question is one thing asked: free text, or one or several choices.
type Question struct {
	Question string   `json:"question"`
	Header   string   `json:"header,omitempty"`
	Choices  []Choice `json:"choices,omitempty"`
	Multi    bool     `json:"multi,omitempty"`
	Default  string   `json:"default,omitempty"`
}

// Choice is an option of a question.
type Choice struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Answer answers one question: the choices picked and/or typed text.
type Answer struct {
	Choices []string `json:"choices,omitempty"`
	Text    string   `json:"text,omitempty"`
}

// ToolCall is what a permission request is about.
type ToolCall struct {
	Name   string          `json:"name"`
	Input  json.RawMessage `json:"input,omitempty"`
	Reason string          `json:"reason,omitempty"`
}

// RequestFile is a file a change request writes. Content is kept on the
// server and isn't listed; Diff is computed here from the base the agent
// sent (checked against its blob id), so what a person approves is what is
// written.
type RequestFile struct {
	Path      string `json:"path"`
	BaseSHA   string `json:"base_sha,omitempty"`
	Delete    bool   `json:"delete,omitempty"`
	New       bool   `json:"new,omitempty"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Diff      string `json:"diff,omitempty"`
	// Outside: Path is an absolute path outside any git repository (WP-115),
	// which only a runner on the asking machine can write.
	Outside bool `json:"outside,omitempty"`
}

// RequestFileInput is a file as the agent sends it.
type RequestFileInput struct {
	Path    string `json:"path"`
	BaseSHA string `json:"base_sha"`          // git blob of the file the change was made against; "" for a new file
	Base    string `json:"base,omitempty"`    // that file's content, to compute the diff here
	Content string `json:"content,omitempty"` // the new content
	Delete  bool   `json:"delete,omitempty"`
	Outside bool   `json:"outside,omitempty"` // Path is absolute, outside any git repo
}

type storedFile struct {
	RequestFile
	Content string `json:"content,omitempty"`
}

// CellInput is a command to run, as the agent asks.
type CellInput struct {
	Command string `json:"command"`
	Dir     string `json:"dir,omitempty"`
}

// CellRun is one run of a command cell.
type CellRun struct {
	ID         int64      `json:"id"`
	Command    string     `json:"command"`
	State      string     `json:"state"` // queued, running, done, failed, cancelled, expired
	ExitCode   *int       `json:"exit_code,omitempty"`
	Output     string     `json:"output,omitempty"`
	Truncated  bool       `json:"truncated,omitempty"`
	Error      string     `json:"error,omitempty"`
	RunBy      *UserRef   `json:"run_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationMS int64      `json:"duration_ms,omitempty"`
	// Waiting: queued, what its runner job waits for (WP-503).
	Waiting string `json:"waiting,omitempty"`
}

// RequestCell is a command a person runs from a request, like a notebook
// cell. Runs are newest first; Run again adds to them.
type RequestCell struct {
	N       int        `json:"n"`
	Command string     `json:"command"`
	Dir     string     `json:"dir,omitempty"`
	Machine string     `json:"machine,omitempty"`
	Runs    []*CellRun `json:"runs"`
	id      int64
}

// RequestResult is what applying or deciding produced.
type RequestResult struct {
	Commit    string   `json:"commit,omitempty"`
	Branch    string   `json:"branch,omitempty"`
	PRURL     string   `json:"pr_url,omitempty"`
	PRTitle   string   `json:"pr_title,omitempty"`
	PRNumber  int      `json:"pr_number,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	Conflicts []string `json:"conflicts,omitempty"`
	// NoChange is set when the approved change altered no file: every file
	// already had the requested content (WP-948). Reason says so.
	NoChange bool `json:"no_change,omitempty"`
	// Rule: the allow rule a person added when approving a permission
	// request (WP-233); the pull request adding it is PRURL, or Reason says
	// it was already there. A rule picked that couldn't be added leaves it
	// unset, and Reason says why.
	Rule string `json:"rule,omitempty"`
}

// RequestSession is the session a request came from, in brief.
type RequestSession struct {
	ID      int64  `json:"id"`
	Machine string `json:"machine"`
	Repo    string `json:"repo,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	Title   string `json:"title,omitempty"`
	State   string `json:"state"`
}

// RequestRunner says whether a runner of the person looking is online on the
// request's machine (WP-D17), so the page can offer it.
type RequestRunner struct {
	Machine string `json:"machine"`
	Online  bool   `json:"online"`
}

// Request is something an agent asks of a person.
type Request struct {
	ID      int64           `json:"id"`
	Code    string          `json:"code"` // R-12
	Ref     string          `json:"ref"`  // WP:R-12; me:R-3 outside a project
	Number  int             `json:"number"`
	Project *SessionProject `json:"project,omitempty"`
	Kind    string          `json:"kind"`
	Title   string          `json:"title"`
	Body    string          `json:"body,omitempty"`
	State   string          `json:"state"`
	User    *UserRef        `json:"user"` // the person it is for
	Agent   string          `json:"agent,omitempty"`
	Session *RequestSession `json:"session,omitempty"`
	Issue   *IssueRef       `json:"issue,omitempty"`

	Questions []Question     `json:"questions,omitempty"`
	Answers   []Answer       `json:"answers,omitempty"`
	Files     []RequestFile  `json:"files,omitempty"`
	Message   string         `json:"message,omitempty"` // a change's commit message
	Tool      *ToolCall      `json:"tool,omitempty"`
	Cells     []*RequestCell `json:"cells,omitempty"`
	// EnvRead: answering "Allow reads" vouches for these environment
	// addresses (WP-1087); only wp env vouch files one.
	EnvRead *EnvReadAsk `json:"env_read,omitempty"`
	// Watches: the conditions that close it by themselves, with their last
	// check (WP-126).
	Watches []*RequestWatch `json:"watches,omitempty"`
	// Refused: what the classifier refused in its session in the hour
	// before it was filed (WP-232), for the person deciding it.
	Refused []*ToolRefusal `json:"refused,omitempty"`
	// Rules: the allow rules a person may add with an approval of an open
	// permission request, narrowest first, to RuleRepo's
	// .claude/settings.json (WP-233); none when no rule covers the call
	// safely or the session's repo isn't known.
	Rules    []PermissionRule `json:"rules,omitempty"`
	RuleRepo string           `json:"rule_repo,omitempty"`

	Answer   string `json:"answer,omitempty"`
	Decision string `json:"decision,omitempty"`
	Note     string `json:"note,omitempty"`
	Via      string `json:"via,omitempty"`  // terminal: answered in the agent's terminal
	Auto     bool   `json:"auto,omitempty"` // filed by Waypoint from a question at the end of the agent's turn
	// Followup: whether the decision wakes the agent (WP-176): any (the
	// default), never, or on (only the choices or decisions in FollowupOn).
	// Otherwise it waits for the agent's next prompt or start, and the agent
	// can look any time (wp request show).
	Followup   string   `json:"followup"`
	FollowupOn []string `json:"followup_on,omitempty"`
	// Blocking: the agent waits on the answer to do more work (WP-209), so
	// it goes within minutes instead of waiting in the session's batch.
	Blocking bool `json:"blocking,omitempty"`
	// Quiet: a dismissal the agent isn't told about (WP-197, see quiet).
	Quiet        bool           `json:"quiet,omitempty"`
	DecidedBy    *UserRef       `json:"decided_by,omitempty"`
	DecidedAt    *time.Time     `json:"decided_at,omitempty"`
	DeliveredAt  *time.Time     `json:"delivered_at,omitempty"`
	SnoozedUntil *time.Time     `json:"snoozed_until,omitempty"`
	ApplyPath    string         `json:"apply_path,omitempty"` // runner, pr
	Result       RequestResult  `json:"result"`
	Error        string         `json:"error,omitempty"`
	CanDecide    bool           `json:"can_decide"`
	Runner       *RequestRunner `json:"runner,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	URL          string         `json:"url"`

	userID    int64
	projectID sql.NullInt64
	sessionID sql.NullInt64
	issueID   sql.NullInt64
	files     []storedFile
}

func (r *Request) snoozed(now int64) bool {
	return r.SnoozedUntil != nil && r.SnoozedUntil.UnixMilli() > now
}

// IsDecided: a person has decided it.
func (r *Request) IsDecided() bool { return slices.Contains(decidedStates, r.State) }

type requestPayload struct {
	Questions []Question   `json:"questions,omitempty"`
	Answers   []Answer     `json:"answers,omitempty"`
	Files     []storedFile `json:"files,omitempty"`
	Message   string       `json:"message,omitempty"`
	Tool      *ToolCall    `json:"tool,omitempty"`
	EnvRead   *EnvReadAsk  `json:"env_read,omitempty"`
}

// RequestInput creates a request.
type RequestInput struct {
	Kind      string             `json:"kind"`
	Title     string             `json:"title"`
	Body      string             `json:"body"`
	Project   string             `json:"project"` // a key; the session's project if empty
	Issue     string             `json:"issue"`
	Questions []Question         `json:"questions"`
	Files     []RequestFileInput `json:"files"`
	Message   string             `json:"message"` // a change's commit message
	Cells     []CellInput        `json:"cells"`
	Tool      string             `json:"tool"`
	ToolInput json.RawMessage    `json:"tool_input"`
	Reason    string             `json:"reason"`
	ToolUseID string             `json:"tool_use_id"`
	// Organization files the request in the parent (organization) project of
	// the one it would go to, when there is one: set for a file in a directory
	// above the repo (WP-115).
	Organization bool `json:"organization"`
	// Followup: does an answer need the agent to act? any (default), never,
	// or on, with FollowupOn the choices (or decisions: approved, denied,
	// done, dismissed) that do (WP-176).
	Followup   string   `json:"followup"`
	FollowupOn []string `json:"followup_on"`
	// Blocking: the agent's other work waits on the answer, so it goes
	// within minutes; otherwise it waits to share a wake-up (WP-209).
	Blocking bool `json:"blocking"`
	// ClosesWhen: conditions that close the request when one holds (WP-126).
	ClosesWhen []WatchInput `json:"closes_when"`
	Source     string       `json:"-"` // requestAuto: filed by Waypoint from the end of a turn (WP-96)
	envRead    *EnvReadAsk
}

var requestRefRE = lazyre.New(`(?i)^(?:([a-z0-9_.-]+):)?r-?(\d+)$`)

func requestRef(projectKey string, number int) string {
	if projectKey == "" {
		return fmt.Sprintf("me:R-%d", number)
	}
	return fmt.Sprintf("%s:R-%d", projectKey, number)
}

// requestVisible is the SQL for requests an actor may see: their own, and
// project requests in projects they edit (who may decide them).
func requestVisible(actor *Actor, alias string) (string, []any) {
	if actor.IsAdmin() {
		return "1=1", nil
	}
	editor, eargs := actor.MemberProjects(RoleEditor)
	if actor.Scope != nil {
		member, margs := actor.MemberProjects(RoleViewer)
		return fmt.Sprintf(`((%[1]s.user_id = ? AND %[1]s.project_id IN %[2]s) OR %[1]s.project_id IN %[3]s)`, alias, member, editor),
			append(append([]any{actor.ID()}, margs...), eargs...)
	}
	return fmt.Sprintf(`(%[1]s.user_id = ? OR %[1]s.project_id IN %[2]s)`, alias, editor), append([]any{actor.ID()}, eargs...)
}

// resolveRequestID finds a request the actor may see from a reference: a
// number (the id), R-12 (the session's, then the only one), or
// KEY:R-12 / me:R-12.
func (s *Service) resolveRequestID(ctx context.Context, q queryer, actor *Actor, ref string) (int64, error) {
	ref = strings.TrimSpace(ref)
	if actor == nil || actor.User == nil {
		return 0, ErrUnauthorized
	}
	vis, vargs := requestVisible(actor, "r")
	if n, err := strconv.ParseInt(ref, 10, 64); err == nil {
		var id int64
		err := q.QueryRowContext(ctx, `SELECT r.id FROM requests r WHERE r.id = ? AND `+vis, append([]any{n}, vargs...)...).Scan(&id)
		if err != nil {
			return 0, notFound("request " + ref)
		}
		return id, nil
	}
	m := requestRefRE.FindStringSubmatch(ref)
	if m == nil {
		return 0, notFound("request " + ref)
	}
	number, _ := strconv.Atoi(m[2])
	var rows *sql.Rows
	var err error
	switch {
	case strings.EqualFold(m[1], "me"):
		rows, err = q.QueryContext(ctx, `SELECT r.id, r.session_id, r.project_id FROM requests r WHERE r.project_id IS NULL AND r.user_id = ? AND r.number = ?`, actor.ID(), number)
	case m[1] != "":
		rows, err = q.QueryContext(ctx, `SELECT r.id, r.session_id, r.project_id FROM requests r JOIN projects p ON p.id = r.project_id
			WHERE p.key = ? COLLATE NOCASE AND r.number = ? AND `+vis, append([]any{m[1], number}, vargs...)...)
	default:
		rows, err = q.QueryContext(ctx, `SELECT r.id, r.session_id, r.project_id FROM requests r WHERE r.number = ? AND `+vis+` ORDER BY r.id DESC LIMIT 20`,
			append([]any{number}, vargs...)...)
	}
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	type cand struct {
		id      int64
		session sql.NullInt64
		project sql.NullInt64
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.session, &c.project); err != nil {
			return 0, err
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(cands) == 0 {
		return 0, notFound("request " + ref)
	}
	if len(cands) == 1 {
		return cands[0].id, nil
	}
	for _, c := range cands {
		if actor.Session != 0 && c.session.Valid && c.session.Int64 == actor.Session {
			return c.id, nil
		}
	}
	var keys []string
	for _, c := range cands {
		var key string
		if c.project.Valid {
			_ = q.QueryRowContext(ctx, `SELECT key FROM projects WHERE id = ?`, c.project.Int64).Scan(&key)
		}
		keys = append(keys, requestRef(key, number))
	}
	return 0, invalid("%s could be %s; use the full reference", strings.ToUpper(ref), strings.Join(keys, " or "))
}

const requestCols = `r.id, r.project_id, r.number, r.user_id, r.session_id, r.issue_id, r.agent, r.kind, r.title, r.body, r.state, r.payload,
	r.answer, r.decision, r.note, r.via, r.decided_by, r.decided_at, r.delivered_at, r.snoozed_until, r.apply_path, r.result, r.error, r.created_at, r.updated_at, r.source, r.followup, r.followup_on, r.blocking`

func (s *Service) scanRequest(sc interface{ Scan(...any) error }) (*Request, error) {
	var r Request
	var payload, result string
	var decidedBy, decidedAt, deliveredAt, snoozed sql.NullInt64
	var created, updated int64
	var source, followOn string
	if err := sc.Scan(&r.ID, &r.projectID, &r.Number, &r.userID, &r.sessionID, &r.issueID, &r.Agent, &r.Kind, &r.Title, &r.Body, &r.State, &payload,
		&r.Answer, &r.Decision, &r.Note, &r.Via, &decidedBy, &decidedAt, &deliveredAt, &snoozed, &r.ApplyPath, &result, &r.Error, &created, &updated, &source, &r.Followup, &followOn, &r.Blocking); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(followOn), &r.FollowupOn)
	r.Auto = source == requestAuto
	var p requestPayload
	_ = json.Unmarshal([]byte(payload), &p)
	r.Questions, r.Answers, r.Message, r.Tool, r.files, r.EnvRead = p.Questions, p.Answers, p.Message, p.Tool, p.Files, p.EnvRead
	_ = json.Unmarshal([]byte(result), &r.Result)
	if decidedBy.Valid {
		r.DecidedBy = &UserRef{ID: decidedBy.Int64}
	}
	r.DecidedAt, r.DeliveredAt, r.SnoozedUntil = ptrTime(decidedAt), ptrTime(deliveredAt), ptrTime(snoozed)
	r.CreatedAt, r.UpdatedAt = msTime(created), msTime(updated)
	r.Quiet = r.quiet()
	return &r, nil
}

// loadRequests runs a query selecting requestCols (from requests r) and
// fills people, projects, sessions, tasks and cells in batched queries. full
// adds diffs and every cell run.
func (s *Service) loadRequests(ctx context.Context, actor *Actor, full bool, where string, args ...any) ([]*Request, error) {
	// #nosec G202 -- requestCols is a constant and where is built from fixed clauses with bound args
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+requestCols+` FROM requests r WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	var list []*Request
	for rows.Next() {
		r, err := s.scanRequest(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, s.fillRequests(ctx, actor, full, list)
}

// requestRefs looks up the projects, sessions and issues requests (and
// refused calls) name, by id; issues the actor mayn't see are left out.
func (s *Service) requestRefs(ctx context.Context, actor *Actor, projects, sessions, issues []int64) (map[int64]*SessionProject, map[int64]*RequestSession, map[int64]*IssueRef, error) {
	pnames := map[int64]*SessionProject{}
	if len(projects) > 0 {
		// #nosec G202 -- only ? placeholders are joined in
		rows, err := s.db.R.QueryContext(ctx, `SELECT id, key, name FROM projects WHERE id IN (`+placeholders(len(projects))+`)`, int64Args(projects)...)
		if err != nil {
			return nil, nil, nil, err
		}
		for rows.Next() {
			var p SessionProject
			if err := rows.Scan(&p.ID, &p.Key, &p.Name); err != nil {
				rows.Close()
				return nil, nil, nil, err
			}
			pnames[p.ID] = &p
		}
		rows.Close()
	}
	snames := map[int64]*RequestSession{}
	if len(sessions) > 0 {
		now := s.nowMS()
		// #nosec G202 -- only ? placeholders are joined in
		rows, err := s.db.R.QueryContext(ctx, `SELECT id, machine, repo, branch, cwd, title, state, last_seen_at FROM agent_sessions WHERE id IN (`+placeholders(len(sessions))+`)`, int64Args(sessions)...)
		if err != nil {
			return nil, nil, nil, err
		}
		for rows.Next() {
			var x RequestSession
			var stored string
			var seen int64
			if err := rows.Scan(&x.ID, &x.Machine, &x.Repo, &x.Branch, &x.Cwd, &x.Title, &stored, &seen); err != nil {
				rows.Close()
				return nil, nil, nil, err
			}
			x.State = sessionState(stored, seen, now)
			snames[x.ID] = &x
		}
		rows.Close()
	}
	irefs := map[int64]*IssueRef{}
	if len(issues) > 0 {
		// #nosec G202 -- only ? placeholders are joined in
		rows, err := s.db.R.QueryContext(ctx, `SELECT i.id, p.key || '-' || i.number, i.title, i.type FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.id IN (`+placeholders(len(issues))+`) AND `+draftsHidden(actor, "i"), int64Args(issues)...)
		if err != nil {
			return nil, nil, nil, err
		}
		for rows.Next() {
			var ir IssueRef
			if err := rows.Scan(&ir.ID, &ir.Key, &ir.Title, &ir.Type); err != nil {
				rows.Close()
				return nil, nil, nil, err
			}
			irefs[ir.ID] = &ir
		}
		rows.Close()
	}
	return pnames, snames, irefs, nil
}

func (s *Service) fillRequests(ctx context.Context, actor *Actor, full bool, list []*Request) error {
	if len(list) == 0 {
		return nil
	}
	var projects, sessions, issues, ids []int64
	for _, r := range list {
		ids = append(ids, r.ID)
		if r.projectID.Valid {
			projects = append(projects, r.projectID.Int64)
		}
		if r.sessionID.Valid {
			sessions = append(sessions, r.sessionID.Int64)
		}
		if r.issueID.Valid {
			issues = append(issues, r.issueID.Int64)
		}
	}
	pnames, snames, irefs, err := s.requestRefs(ctx, actor, projects, sessions, issues)
	if err != nil {
		return err
	}
	people := map[int64]*UserRef{}
	roles := map[int64]int{}
	byID := map[int64]*Request{}
	for _, r := range list {
		byID[r.ID] = r
		r.User = s.cachedUser(ctx, people, sql.NullInt64{Int64: r.userID, Valid: true})
		if r.DecidedBy != nil {
			r.DecidedBy = s.cachedUser(ctx, people, sql.NullInt64{Int64: r.DecidedBy.ID, Valid: true})
		}
		key := ""
		if r.projectID.Valid {
			r.Project = pnames[r.projectID.Int64]
			if r.Project != nil {
				key = r.Project.Key
			}
		}
		r.Code = fmt.Sprintf("R-%d", r.Number)
		r.Ref = requestRef(key, r.Number)
		r.URL = "/requests/" + r.Ref
		if r.sessionID.Valid {
			r.Session = snames[r.sessionID.Int64]
		}
		if r.Kind == RequestPermission && r.State == RequestOpen && r.Tool != nil && r.Session != nil && r.Session.Repo != "" {
			if r.Rules = permissionRules(r.Tool.Name, r.Tool.Input); len(r.Rules) > 0 {
				r.RuleRepo = r.Session.Repo
			}
		}
		if r.issueID.Valid {
			r.Issue = irefs[r.issueID.Int64]
		}
		r.CanDecide = s.canDecide(ctx, actor, r, roles)
		r.Files = nil
		for _, f := range r.files {
			rf := f.RequestFile
			if !full {
				rf.Diff = ""
			}
			r.Files = append(r.Files, rf)
		}
	}
	// #nosec G202 -- only ? placeholders are joined in
	crows, err := s.db.R.QueryContext(ctx, `SELECT id, request_id, n, command, dir, machine FROM request_cells WHERE request_id IN (`+placeholders(len(ids))+`) ORDER BY request_id, n`, int64Args(ids)...)
	if err != nil {
		return err
	}
	cells := map[int64]*RequestCell{}
	var cellIDs []int64
	for crows.Next() {
		var c RequestCell
		var rid int64
		if err := crows.Scan(&c.id, &rid, &c.N, &c.Command, &c.Dir, &c.Machine); err != nil {
			crows.Close()
			return err
		}
		c.Runs = []*CellRun{}
		cells[c.id] = &c
		cellIDs = append(cellIDs, c.id)
		byID[rid].Cells = append(byID[rid].Cells, &c)
	}
	crows.Close()
	if len(cellIDs) > 0 {
		// #nosec G202 -- only ? placeholders are joined in
		q := `SELECT id, cell_id, command, state, exit_code, output, truncated, error, run_by, created_at, finished_at, job_id FROM request_cell_runs WHERE cell_id IN (` + placeholders(len(cellIDs)) + `) ORDER BY id DESC`
		rrows, err := s.db.R.QueryContext(ctx, q, int64Args(cellIDs)...)
		if err != nil {
			return err
		}
		defer rrows.Close()
		for rrows.Next() {
			var run CellRun
			var cid int64
			var exit, runBy, finished, job sql.NullInt64
			var trunc int
			var created int64
			if err := rrows.Scan(&run.ID, &cid, &run.Command, &run.State, &exit, &run.Output, &trunc, &run.Error, &runBy, &created, &finished, &job); err != nil {
				return err
			}
			c := cells[cid]
			if !full && len(c.Runs) >= 1 {
				continue
			}
			if len(c.Runs) >= 10 {
				continue
			}
			run.Truncated, run.CreatedAt, run.FinishedAt = trunc != 0, msTime(created), ptrTime(finished)
			if exit.Valid {
				e := int(exit.Int64)
				run.ExitCode = &e
			}
			if finished.Valid {
				run.DurationMS = finished.Int64 - created
			}
			run.RunBy = s.cachedUser(ctx, people, runBy)
			if run.State == "queued" && job.Valid {
				run.Waiting = s.jobWaiting(ctx, job.Int64)
			}
			c.Runs = append(c.Runs, &run)
		}
		if err := rrows.Err(); err != nil {
			return err
		}
	}
	if full && !actor.IsAgent() {
		for _, r := range list {
			if r.Refused, err = s.refusalsBefore(ctx, actor, r); err != nil {
				return err
			}
		}
	}
	return s.fillWatches(ctx, ids, byID)
}

// canDecide: a person (never an agent) may decide a request when it is
// theirs, or they edit its project.
func (s *Service) canDecide(ctx context.Context, actor *Actor, r *Request, roles map[int64]int) bool {
	if actor == nil || actor.User == nil || actor.IsAgent() || actor.ReadOnly {
		return false
	}
	if r.userID == actor.ID() || actor.IsAdmin() {
		return true
	}
	if !r.projectID.Valid {
		return false
	}
	role, ok := roles[r.projectID.Int64]
	if !ok {
		var rerr error
		role, rerr = s.projectRole(ctx, s.db.R, actor, r.projectID.Int64)
		if rerr != nil {
			if readFault(rerr) {
				s.log.WarnContext(ctx, "project role failed", "projectid", r.projectID.Int64, "err", rerr)
			}
		}
		roles[r.projectID.Int64] = role
	}
	return role >= RoleEditor
}

// RequestQuery narrows a request list.
type RequestQuery struct {
	State   string // open (default), answered (decided, agent not told yet), decided, snoozed, all, or one state
	Mine    bool   // only those for the actor
	Session int64
	Project string
	Kind    string
	Agent   string
	Limit   int
	Offset  int
}

// RequestPage is a page of requests, newest first.
type RequestPage struct {
	Requests []*Request `json:"requests"`
	Total    int        `json:"total"`
	Next     int        `json:"next,omitempty"`
}

// ListRequests lists requests the actor may see: theirs, and those in
// projects they edit.
func (s *Service) ListRequests(ctx context.Context, actor *Actor, q RequestQuery) (*RequestPage, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}
	vis, args := requestVisible(actor, "r")
	where := []string{vis}
	now := s.nowMS()
	switch q.State {
	case "", "open":
		where, args = append(where, "r.state = 'open' AND (r.snoozed_until IS NULL OR r.snoozed_until <= ?)"), append(args, now)
	case "snoozed":
		where, args = append(where, "r.state = 'open' AND r.snoozed_until > ?"), append(args, now)
	case "answered":
		where = append(where, "r.decided_at IS NOT NULL AND r.delivered_at IS NULL AND r.via = '' AND r.state IN ("+placeholders(len(decidedStates))+")")
		for _, st := range decidedStates {
			args = append(args, st)
		}
	case "decided":
		where = append(where, "r.state IN ("+placeholders(len(decidedStates))+")")
		for _, st := range decidedStates {
			args = append(args, st)
		}
	case "all":
	default:
		if !slices.Contains([]string{RequestOpen, RequestAnswered, RequestApproved, RequestDenied, RequestDone, RequestDismissed, RequestWithdrawn, RequestApplying, RequestApplied, RequestFailed, RequestConflict, RequestExpired}, q.State) {
			return nil, invalid("state must be open, answered, decided, snoozed, all, or one request state")
		}
		where, args = append(where, "r.state = ?"), append(args, q.State)
	}
	if q.Mine {
		where, args = append(where, "r.user_id = ?"), append(args, actor.ID())
	}
	if q.Session != 0 {
		where, args = append(where, "r.session_id = ?"), append(args, q.Session)
	}
	if q.Project != "" {
		if strings.EqualFold(q.Project, "me") {
			where = append(where, "r.project_id IS NULL")
		} else {
			p, err := s.projectByRef(ctx, s.db.R, q.Project)
			if err != nil {
				return nil, err
			}
			if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
				return nil, err
			}
			where, args = append(where, "r.project_id = ?"), append(args, p.ID)
		}
	}
	if q.Kind != "" {
		if !slices.Contains(RequestKinds, q.Kind) {
			return nil, invalid("kind must be one of %s", strings.Join(RequestKinds, ", "))
		}
		where, args = append(where, "r.kind = ?"), append(args, q.Kind)
	}
	if q.Agent != "" {
		where, args = append(where, "lower(r.agent) LIKE ?"), append(args, "%"+strings.ToLower(q.Agent)+"%")
	}
	cond := strings.Join(where, " AND ")
	page := &RequestPage{Requests: []*Request{}}
	// #nosec G202 -- fixed fragments; values are bound
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM requests r WHERE `+cond, args...).Scan(&page.Total); err != nil {
		return nil, err
	}
	list, err := s.loadRequests(ctx, actor, false, cond+` ORDER BY r.id DESC LIMIT ? OFFSET ?`, append(args, q.Limit, q.Offset)...)
	if err != nil {
		return nil, err
	}
	if list != nil {
		page.Requests = list
	}
	if q.Offset+len(page.Requests) < page.Total {
		page.Next = q.Offset + len(page.Requests)
	}
	return page, nil
}

// sessionRequests are a session's requests, newest first.
func (s *Service) sessionRequests(ctx context.Context, actor *Actor, sessionID int64) ([]*Request, error) {
	vis, args := requestVisible(actor, "r")
	return s.loadRequests(ctx, actor, false, `r.session_id = ? AND `+vis+` ORDER BY r.id DESC LIMIT 200`, append([]any{sessionID}, args...)...)
}

// GetRequest returns a request the actor may see. An agent reading its own
// decided request counts as being told (the Stop hook won't repeat it). With
// wait, a request that is still open is held for up to that long (at most
// 25 s) until it is decided, for `wp request wait`.
func (s *Service) GetRequest(ctx context.Context, actor *Actor, ref string, wait time.Duration) (*Request, error) {
	return s.getRequest(ctx, actor, ref, wait, true)
}

// PeekRequest is GetRequest without the telling: a lookup that only asks
// whether the request exists (wp show R1 deciding between request R-1 and
// page R1) leaves its answer still to be delivered.
func (s *Service) PeekRequest(ctx context.Context, actor *Actor, ref string) (*Request, error) {
	return s.getRequest(ctx, actor, ref, 0, false)
}

func (s *Service) getRequest(ctx context.Context, actor *Actor, ref string, wait time.Duration, tell bool) (*Request, error) {
	id, err := s.resolveRequestID(ctx, s.db.R, actor, ref)
	if err != nil {
		return nil, err
	}
	if wait > 25*time.Second {
		wait = 25 * time.Second
	}
	start := time.Now()
	for {
		list, err := s.loadRequests(ctx, actor, true, `r.id = ?`, id)
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			return nil, notFound("request " + ref)
		}
		r := list[0]
		if r.State == RequestOpen && wait > 0 && time.Since(start) < wait {
			select {
			case <-ctx.Done():
				return r, nil
			case <-time.After(time.Second):
				continue
			}
		}
		s.fillRunner(ctx, actor, r)
		if tell && r.IsDecided() && r.DeliveredAt == nil && r.userID == actor.ID() && actor.IsAgent() {
			now := s.nowMS()
			if _, err := s.db.W.ExecContext(ctx, `UPDATE requests SET delivered_at = ? WHERE id = ? AND delivered_at IS NULL`, now, r.ID); err == nil {
				t := msTime(now)
				r.DeliveredAt = &t
			}
		}
		return r, nil
	}
}

// fillRunner says whether the actor has a runner online on the request's machine.
func (s *Service) fillRunner(ctx context.Context, actor *Actor, r *Request) {
	if r.Session == nil || (r.Kind != RequestChange && r.Kind != RequestCommand && r.Kind != RequestAction) || !r.CanDecide {
		return
	}
	r.Runner = &RequestRunner{Machine: r.Session.Machine, Online: runnerOnline(ctx, s, actor.ID(), r.Session.Machine)}
}

var toolNameRE = lazyre.New(`^[A-Za-z0-9_.:*-]{1,100}$`)

// CreateRequest files a request for the actor's person. Agents ask; people
// may too (a note to themselves from a script). Returns the request and
// whether it is new (a permission request repeated within ten minutes is the
// same one).
func (s *Service) CreateRequest(ctx context.Context, actor *Actor, in RequestInput) (*Request, bool, error) {
	if err := actor.CanWrite(); err != nil {
		return nil, false, err
	}
	var id int64
	var created bool
	err := s.write(ctx, actor, func(t *tx) error {
		var sess *AgentSession
		if actor.Session != 0 {
			x, err := scanSession(t.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM agent_sessions s WHERE s.id = ? AND s.user_id = ?`, actor.Session, actor.ID()), s.nowMS())
			if err == nil {
				sess = x
			}
		}
		var err error
		id, created, err = s.createRequestTx(ctx, t, actor, sess, in, s.nowMS())
		return err
	})
	if err != nil {
		return nil, false, err
	}
	list, err := s.loadRequests(ctx, actor, true, `r.id = ?`, id)
	if err != nil || len(list) == 0 {
		return nil, false, err
	}
	return list[0], created, nil
}

func (s *Service) createRequestTx(ctx context.Context, t *tx, actor *Actor, sess *AgentSession, in RequestInput, at int64) (int64, bool, error) {
	in.Kind = strings.TrimSpace(in.Kind)
	if !slices.Contains(RequestKinds, in.Kind) {
		return 0, false, invalid("kind must be one of %s", strings.Join(RequestKinds, ", "))
	}
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		return 0, false, invalid("a request needs a title")
	}
	fullTitle := in.Title
	if len([]rune(in.Title)) > maxRequestTitle {
		in.Title, in.Body = splitTitle(in.Title, in.Body, true)
	}
	if len(in.Body) > maxRequestBody {
		return 0, false, invalid("the details can be at most %d characters", maxRequestBody)
	}
	pay := requestPayload{Message: strings.TrimSpace(in.Message)}
	if len(pay.Message) > 2000 {
		return 0, false, invalid("the commit message can be at most 2000 characters")
	}
	var cells []CellInput
	dedupe := ""
	switch in.Kind {
	case RequestQuestion:
		if in.envRead != nil {
			in.Questions = []Question{in.envRead.question()}
			pay.EnvRead = in.envRead
			dedupe = in.envRead.dedupe()
		}
		qs, err := checkQuestions(in.Questions)
		if err != nil {
			return 0, false, err
		}
		pay.Questions = qs
	case RequestChange:
		files, err := checkChangeFiles(in.Files)
		if err != nil {
			return 0, false, err
		}
		pay.Files = files
		if pay.Message == "" {
			pay.Message = fullTitle
		}
	case RequestCommand, RequestAction:
		c, err := checkCells(in.Cells)
		if err != nil {
			return 0, false, err
		}
		if in.Kind == RequestCommand && len(c) == 0 {
			return 0, false, invalid("a command request needs at least one command")
		}
		cells = c
	case RequestPermission:
		if !toolNameRE.MatchString(in.Tool) {
			return 0, false, invalid("a permission request needs the tool's name")
		}
		input := in.ToolInput
		if len(input) > 20_000 {
			input = nil
		}
		pay.Tool = &ToolCall{Name: in.Tool, Input: input, Reason: excerpt(in.Reason, 1000)}
		dedupe = toolDedupe(in.Tool, in.ToolInput)
	}
	var projectID sql.NullInt64
	var issueID sql.NullInt64
	if in.Issue != "" {
		i, _, err := s.getIssueFor(ctx, t, actor, in.Issue, RoleViewer, false)
		if err != nil {
			return 0, false, err
		}
		issueID = sql.NullInt64{Int64: i.ID, Valid: true}
		projectID = sql.NullInt64{Int64: i.ProjectID, Valid: true}
	}
	switch {
	case in.Project != "" && !strings.EqualFold(in.Project, "me"):
		p, err := s.projectByRef(ctx, t, in.Project)
		if err != nil {
			return 0, false, err
		}
		if err := s.requireProjectRole(ctx, t, actor, p.ID, RoleViewer); err != nil {
			return 0, false, err
		}
		projectID = sql.NullInt64{Int64: p.ID, Valid: true}
	case strings.EqualFold(in.Project, "me"):
		projectID = sql.NullInt64{}
	case !projectID.Valid && sess != nil && sess.projectID.Valid:
		r, rerr := s.projectRole(ctx, t, actor, sess.projectID.Int64)
		if rerr != nil {
			if readFault(rerr) {
				s.log.WarnContext(ctx, "project role failed", "projectid", sess.projectID.Int64, "err", rerr)
			}
		}
		if r > RoleNone {
			projectID = sess.projectID
		}
	}
	if in.Organization && projectID.Valid {
		var parent sql.NullInt64
		_ = t.QueryRowContext(ctx, `SELECT parent_id FROM projects WHERE id = ?`, projectID.Int64).Scan(&parent)
		if parent.Valid {
			r, rerr := s.projectRole(ctx, t, actor, parent.Int64)
			if rerr != nil {
				if readFault(rerr) {
					s.log.WarnContext(ctx, "project role failed", "project", projectID.Int64, "err", rerr)
				}
			}
			if r > RoleNone {
				projectID = parent
			}
		}
	}
	var sessionID sql.NullInt64
	if sess != nil {
		sessionID = sql.NullInt64{Int64: sess.ID, Valid: true}
	}
	if in.envRead != nil && projectID.Valid {
		var dup int64
		err := t.QueryRowContext(ctx, `SELECT id FROM requests WHERE project_id = ? AND dedupe_key = ? AND state = 'open' ORDER BY id DESC LIMIT 1`, projectID.Int64, dedupe).Scan(&dup)
		if err == nil {
			return dup, false, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, false, err
		}
	}
	if sessionID.Valid {
		var dup int64
		var err error
		switch {
		case dedupe != "":
			err = t.QueryRowContext(ctx, `SELECT id FROM requests WHERE session_id = ? AND dedupe_key = ? AND state = 'open' AND created_at > ? ORDER BY id DESC LIMIT 1`,
				sessionID.Int64, dedupe, at-permissionDedupe.Milliseconds()).Scan(&dup)
		case in.ToolUseID != "":
			err = t.QueryRowContext(ctx, `SELECT id FROM requests WHERE session_id = ? AND tool_use_id = ? LIMIT 1`, sessionID.Int64, in.ToolUseID).Scan(&dup)
		default:
			err = sql.ErrNoRows
		}
		if err == nil {
			return dup, false, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, false, err
		}
	}
	var open int
	if err := t.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE user_id = ? AND state = 'open'`, actor.ID()).Scan(&open); err != nil {
		return 0, false, err
	}
	if open >= maxRequestsOpenIn {
		return 0, false, &requestsFullError{invalid("%d requests are already waiting; decide or dismiss some first", open)}
	}
	var number int
	scope := actor.ID()
	scopeExpr := "-?"
	if projectID.Valid {
		scope, scopeExpr = projectID.Int64, "?"
	}
	if err := t.QueryRowContext(ctx, `SELECT coalesce(max(number), 0) + 1 FROM requests WHERE coalesce(project_id, -user_id) = `+scopeExpr, scope).Scan(&number); err != nil {
		return 0, false, err
	}
	payload, _ := json.Marshal(pay)
	follow, followOn, err := checkFollowup(in.Followup, in.FollowupOn)
	if err != nil {
		return 0, false, err
	}
	if in.Blocking && follow == "never" {
		return 0, false, invalid("blocking is for an answer that wakes you, not followup never")
	}
	var notifyAt any
	if in.Source == requestAuto {
		notifyAt = at + autoQuestionDelay.Milliseconds()
	}
	res, err := t.ExecContext(ctx, `INSERT INTO requests(project_id, number, user_id, session_id, issue_id, agent, kind, title, body, state, payload, tool_use_id, dedupe_key, created_at, updated_at, source, notify_at, followup, followup_on, blocking)
		VALUES (?,?,?,?,?,?,?,?,?,'open',?,?,?,?,?,?,?,?,?,?)`,
		nullInt(ptrInt(projectID)), number, actor.ID(), nullInt(ptrInt(sessionID)), nullInt(ptrInt(issueID)), cmp(actor.AgentName(), agentOf(sess)), in.Kind, in.Title, in.Body,
		string(payload), in.ToolUseID, dedupe, at, at, in.Source, notifyAt, follow, followOn, in.Blocking)
	if err != nil {
		return 0, false, err
	}
	id, _ := res.LastInsertId()
	machine := ""
	if sess != nil {
		machine = sess.Machine
	}
	for n, c := range cells {
		if _, err := t.ExecContext(ctx, `INSERT INTO request_cells(request_id, n, command, dir, machine) VALUES (?,?,?,?,?)`, id, n+1, c.Command, c.Dir, machine); err != nil {
			return 0, false, err
		}
	}
	if len(in.ClosesWhen) > maxWatches {
		return 0, false, invalid("a request can watch at most %d conditions", maxWatches)
	}
	for _, c := range in.ClosesWhen {
		w, err := s.checkWatch(ctx, t, actor, sess, c)
		if err != nil {
			return 0, false, err
		}
		if err := s.insertWatch(ctx, t, id, w, at); err != nil {
			return 0, false, err
		}
	}
	key := ""
	if projectID.Valid {
		_ = t.QueryRowContext(ctx, `SELECT key FROM projects WHERE id = ?`, projectID.Int64).Scan(&key)
	}
	ref := requestRef(key, number)
	if sessionID.Valid {
		if err := s.sessionTimeline(ctx, t, sessionID.Int64, at, "request", "request", id, fmt.Sprintf("R-%d", number), "/requests/"+ref, in.Title, projectID, map[string]any{"kind": in.Kind}); err != nil {
			return 0, false, err
		}
	}
	if in.Source == requestAuto {
		t.emit(requestEvent(EvRequestUpdated, id, ref, number, in.Kind, RequestOpen, actor.ID(), projectID, sessionID, in.Title, actor.AgentName()))
		return id, true, nil
	}
	t.emit(requestEvent(EvRequestCreated, id, ref, number, in.Kind, RequestOpen, actor.ID(), projectID, sessionID, in.Title, actor.AgentName()))
	return id, true, nil
}

func agentOf(x *AgentSession) string {
	if x == nil {
		return ""
	}
	return x.Agent
}

// canonicalJSON re-encodes JSON with sorted keys, so the same tool input
// hashes the same however it was spelled.
func canonicalJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func requestEvent(kind string, id int64, ref string, number int, rkind, state string, user int64, project, session sql.NullInt64, title, agent string) *Event {
	e := &Event{Kind: kind, UserID: user, Data: map[string]any{
		"request": id, "code": fmt.Sprintf("R-%d", number), "ref": ref, "kind": rkind, "state": state, "title": title, "for": user,
	}}
	if session.Valid {
		e.Data["session"] = session.Int64
	}
	if agent != "" {
		e.Data["agent"] = agent
	}
	if project.Valid {
		e.ProjectID = project.Int64
	}
	return e
}

func checkQuestions(in []Question) ([]Question, error) {
	if len(in) == 0 {
		return nil, invalid("a question request needs at least one question")
	}
	if len(in) > maxRequestQuestions {
		return nil, invalid("at most %d questions in one request", maxRequestQuestions)
	}
	out := make([]Question, 0, len(in))
	for _, q := range in {
		q.Question = strings.TrimSpace(q.Question)
		if q.Question == "" {
			return nil, invalid("a question can't be empty")
		}
		if len(q.Question) > 4000 {
			return nil, invalid("a question can be at most 4000 characters")
		}
		if len(q.Choices) > 30 {
			return nil, invalid("at most 30 choices")
		}
		seen := map[string]bool{}
		for i := range q.Choices {
			q.Choices[i].Label = strings.TrimSpace(q.Choices[i].Label)
			if q.Choices[i].Label == "" || len(q.Choices[i].Label) > 300 {
				return nil, invalid("a choice needs a label of up to 300 characters")
			}
			if seen[q.Choices[i].Label] {
				return nil, invalid("%q is listed twice", q.Choices[i].Label)
			}
			seen[q.Choices[i].Label] = true
		}
		if q.Default != "" && len(q.Choices) > 0 && !seen[q.Default] {
			return nil, invalid("the default %q isn't one of the choices", q.Default)
		}
		out = append(out, q)
	}
	return out, nil
}

func checkCells(in []CellInput) ([]CellInput, error) {
	if len(in) > 20 {
		return nil, invalid("at most 20 commands in one request")
	}
	out := make([]CellInput, 0, len(in))
	for _, c := range in {
		c.Command = strings.TrimSpace(c.Command)
		if c.Command == "" {
			return nil, invalid("a command can't be empty")
		}
		if len(c.Command) > 8000 || len(c.Dir) > 1000 {
			return nil, invalid("a command can be at most 8000 characters and its directory 1000")
		}
		out = append(out, c)
	}
	return out, nil
}

// cleanRepoPath validates a path relative to the repo root.
func cleanRepoPath(p string) (string, error) {
	if p == "" || strings.ContainsAny(p, "\x00\\") || strings.HasPrefix(p, "/") {
		return "", invalid("%q isn't a path inside the repository", p)
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") || c != p && strings.TrimSuffix(p, "/") != c {
		return "", invalid("%q isn't a path inside the repository", p)
	}
	for _, seg := range strings.Split(c, "/") {
		if seg == ".git" {
			return "", invalid("%q is inside .git", p)
		}
	}
	return c, nil
}

// cleanOutsidePath checks an absolute path of a file outside any repo: no
// "..", no empty or "." segments, nothing inside .git, not a bare root.
func cleanOutsidePath(p string) (string, error) {
	if len(p) > 1000 || strings.ContainsAny(p, "\x00\\") || !strings.HasPrefix(p, "/") {
		return "", invalid("%q isn't an absolute path", p)
	}
	if p == "/" || path.Clean(p) != p {
		return "", invalid("%q isn't a clean absolute path to a file (no .., ., doubled or trailing slashes)", p)
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if seg == ".git" {
			return "", invalid("%q is inside .git", p)
		}
	}
	return p, nil
}

func checkChangeFiles(in []RequestFileInput) ([]storedFile, error) {
	if len(in) == 0 {
		return nil, invalid("a change request needs at least one file")
	}
	if len(in) > maxRequestFiles {
		return nil, invalid("at most %d files in one change", maxRequestFiles)
	}
	total := 0
	seen := map[string]bool{}
	out := make([]storedFile, 0, len(in))
	for _, f := range in {
		var p string
		var err error
		if f.Outside {
			p, err = cleanOutsidePath(f.Path)
		} else {
			p, err = cleanRepoPath(f.Path)
		}
		if err != nil {
			return nil, err
		}
		if f.Outside != in[0].Outside {
			return nil, invalid("a change request names files in the repository or files outside any repository, not both; file one request for each")
		}
		if f.Outside && path.Dir(p) != path.Dir(in[0].Path) {
			return nil, invalid("files outside any repository in one request must share a directory (%s); file one request for each directory", path.Dir(in[0].Path))
		}
		if seen[p] {
			return nil, invalid("%s is listed twice", p)
		}
		seen[p] = true
		if len(f.Content) > maxRequestFile || len(f.Base) > maxRequestFile {
			return nil, invalid("%s is over %d KB; change request files are text up to that size", p, maxRequestFile>>10)
		}
		total += len(f.Content) + len(f.Base)
		if total > maxRequestTotal {
			return nil, invalid("a change request can carry at most %d MB of files", maxRequestTotal>>20)
		}
		if strings.ContainsRune(f.Content, 0) || strings.ContainsRune(f.Base, 0) {
			return nil, invalid("%s looks binary; change requests carry text files", p)
		}
		isNew := f.BaseSHA == ""
		switch {
		case isNew && f.Base != "":
			return nil, invalid("%s has base content but no base_sha", p)
		case !isNew && udiff.GitBlobSHA(f.Base) != strings.ToLower(f.BaseSHA):
			return nil, invalid("%s: base_sha %s isn't the git blob of the base content sent", p, f.BaseSHA)
		case isNew && f.Delete:
			return nil, invalid("%s is new, so there is nothing to delete", p)
		case !f.Delete && f.Content == "" && isNew:
			return nil, invalid("%s: new files need content", p)
		}
		d, add, del := udiff.Unified(p, f.Base, f.Content, isNew, f.Delete)
		if f.Delete {
			f.Content = ""
		}
		out = append(out, storedFile{RequestFile: RequestFile{Path: p, Outside: f.Outside, BaseSHA: strings.ToLower(f.BaseSHA), Delete: f.Delete, New: isNew, Additions: add, Deletions: del, Diff: d}, Content: f.Content})
	}
	return out, nil
}

// toolSummary is the part of a tool call a person recognises: the command
// of Bash, the path of a file tool.
func toolSummary(tool string, input json.RawMessage) (short, full string) {
	var in map[string]any
	if json.Unmarshal(input, &in) != nil {
		return "", ""
	}
	for _, k := range []string{"command", "file_path", "path", "url", "pattern", "query", "description"} {
		if v, ok := in[k].(string); ok && v != "" {
			return excerpt(v, 160), strings.TrimSpace(v)
		}
	}
	return "", ""
}

// requestFromAskUserQuestion files a question request from the agent's
// AskUserQuestion tool call, so it is not lost if the person is away.
func (s *Service) requestFromAskUserQuestion(ctx context.Context, t *tx, actor *Actor, x *AgentSession, ev *HookEvent, at int64) error {
	var in struct {
		Questions []struct {
			Question    string `json:"question"`
			Header      string `json:"header"`
			MultiSelect bool   `json:"multiSelect"`
			Options     []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(ev.ToolInput, &in); err != nil || len(in.Questions) == 0 {
		return nil
	}
	var qs []Question
	for _, q := range in.Questions {
		item := Question{Question: q.Question, Header: q.Header, Multi: q.MultiSelect}
		for _, o := range q.Options {
			item.Choices = append(item.Choices, Choice{Label: o.Label, Description: o.Description})
		}
		qs = append(qs, item)
	}
	qs, err := checkQuestions(qs)
	if err != nil {
		return nil
	}
	_, _, err = s.createRequestTx(ctx, t, actor, x, RequestInput{Kind: RequestQuestion, Title: qs[0].Question, Questions: qs, ToolUseID: ev.ToolUseID}, at)
	return err
}

// answeredInTerminal closes the question a person answered in the agent's
// own terminal (PostToolUse of AskUserQuestion).
func (s *Service) answeredInTerminal(ctx context.Context, t *tx, x *AgentSession, ev *HookEvent, at int64) error {
	if ev.ToolUseID == "" {
		return nil
	}
	var id int64
	var ref string
	var number int
	var project sql.NullInt64
	err := t.QueryRowContext(ctx, `SELECT id, number, project_id FROM requests WHERE session_id = ? AND tool_use_id = ? AND state = 'open'`, x.ID, ev.ToolUseID).Scan(&id, &number, &project)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	answer := terminalAnswer(ev.ToolResponse)
	if _, err := t.ExecContext(ctx, `UPDATE requests SET state = 'answered', decision = 'answer', via = 'terminal', answer = ?, decided_at = ?, delivered_at = ?, updated_at = ? WHERE id = ?`,
		answer, at, at, at, id); err != nil {
		return err
	}
	key := ""
	if project.Valid {
		_ = t.QueryRowContext(ctx, `SELECT key FROM projects WHERE id = ?`, project.Int64).Scan(&key)
	}
	ref = requestRef(key, number)
	e := requestEvent(EvRequestUpdated, id, ref, number, RequestQuestion, RequestAnswered, x.userID, project, sql.NullInt64{Int64: x.ID, Valid: true}, "", x.Agent)
	e.Data["decision"] = "answered"
	e.Data["via"] = "terminal"
	t.emit(e)
	return nil
}

// terminalAnswer is the answer text out of an AskUserQuestion tool response.
func terminalAnswer(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "Answered in the terminal."
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return "Answered in the terminal."
	}
	var parts []string
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch x := v.(type) {
		case string:
			if x != "" {
				parts = append(parts, strings.TrimSpace(prefix+x))
			}
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				if s, ok := x[k].(string); ok {
					parts = append(parts, k+": "+s)
					continue
				}
				walk("", x[k])
			}
		case []any:
			for _, e := range x {
				walk("", e)
			}
		}
	}
	walk("", v)
	if len(parts) == 0 {
		return "Answered in the terminal."
	}
	return excerpt(strings.Join(parts, "\n"), 1500)
}

// takeDecisions returns the decisions the agent hasn't been told yet and
// marks them delivered: those of this session at Stop; at SessionStart also
// those of earlier sessions in the same worktree.
//
// A decision that needs no follow-up (WP-176: the request said never, or only
// for other answers) is left for bundled calls, the start of a turn (SessionStart,
// UserPromptSubmit), and not used to continue an agent at a Stop.
func (s *Service) takeDecisions(ctx context.Context, t *tx, actor *Actor, x *AgentSession, start, bundled bool) ([]*Request, error) {
	where, args := decisionsWhere(actor, x, start)
	ids, err := queryIDs(ctx, t, `SELECT r.id FROM requests r WHERE `+where+` ORDER BY r.id LIMIT 50`, args...)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	// #nosec G202 -- only ? placeholders are joined in
	list, err := s.loadRequests(ctx, actor, true, `r.id IN (`+placeholders(len(ids))+`) ORDER BY r.id`, int64Args(ids)...)
	if err != nil {
		return nil, err
	}
	if !bundled {
		var quiet []int64
		for _, r := range list {
			if r.quiet() {
				quiet = append(quiet, r.ID)
			}
		}
		if len(quiet) > 0 {
			// #nosec G202 -- only ? placeholders are joined in
			if _, err := t.ExecContext(ctx, `UPDATE requests SET delivered_at = ? WHERE id IN (`+placeholders(len(quiet))+`)`, append([]any{s.nowMS()}, int64Args(quiet)...)...); err != nil {
				return nil, err
			}
		}
		list = slices.DeleteFunc(list, func(r *Request) bool { return !r.wakes() })
		ids = ids[:0]
		for _, r := range list {
			ids = append(ids, r.ID)
		}
		if len(ids) == 0 {
			return nil, nil
		}
	} else {
		list = slices.DeleteFunc(list, func(r *Request) bool { return r.quiet() })
	}
	// #nosec G202 -- only ? placeholders are joined in
	if _, err := t.ExecContext(ctx, `UPDATE requests SET delivered_at = ? WHERE id IN (`+placeholders(len(ids))+`)`, append([]any{s.nowMS()}, int64Args(ids)...)...); err != nil {
		return nil, err
	}
	return list, nil
}

// decisionsWhere selects the decisions takeDecisions gives: the session's
// (at a start, also those of earlier sessions in its worktree) not told yet.
func decisionsWhere(actor *Actor, x *AgentSession, start bool) (string, []any) {
	where := `r.user_id = ? AND r.decided_at IS NOT NULL AND r.delivered_at IS NULL AND r.via = '' AND r.state IN (` + placeholders(len(decidedStates)) + `)`
	args := []any{actor.ID()}
	for _, st := range decidedStates {
		args = append(args, st)
	}
	if start && x.Cwd != "" {
		where += ` AND (r.session_id = ? OR r.session_id IN (SELECT id FROM agent_sessions WHERE user_id = ? AND machine = ? AND cwd = ?))`
		args = append(args, x.ID, actor.ID(), x.Machine, x.Cwd)
	} else {
		where += ` AND r.session_id = ?`
		args = append(args, x.ID)
	}
	return where, args
}

// checkFollowup validates a request's follow-up setting.
func checkFollowup(mode string, on []string) (string, string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	var keep []string
	for _, o := range on {
		if o = strings.TrimSpace(o); o != "" {
			keep = append(keep, o)
		}
	}
	switch {
	case mode == "" && len(keep) > 0:
		mode = "on"
	case mode == "":
		mode = "any"
	case mode == "none":
		mode = "never"
	}
	switch mode {
	case "any", "never":
		keep = nil
	case "on":
		if len(keep) == 0 {
			return "", "", invalid("followup on needs the choices or decisions that need a follow-up")
		}
		if len(keep) > 20 {
			return "", "", invalid("at most 20 follow-up choices")
		}
	default:
		return "", "", invalid("followup must be any, never or on")
	}
	raw, _ := json.Marshal(keep)
	if keep == nil {
		raw = []byte("[]")
	}
	return mode, string(raw), nil
}

// wakes: does this decision need the agent to act, so that it continues the
// agent from a stop (WP-176)? A failed or conflicting apply always does: the
// agent must know its change did not land.
func (r *Request) wakes() bool {
	switch r.State {
	case RequestFailed, RequestConflict:
		return true
	}
	if r.quiet() {
		return false
	}
	if r.Kind == RequestPermission && !r.Blocking {
		return false
	}
	if r.Decision == closedByWatch || r.Decision == closedInSession {
		return false
	}
	switch r.Followup {
	case "never":
		return false
	case "on":
		words := []string{strings.ToLower(r.Decision), strings.ToLower(r.State)}
		for _, a := range r.Answers {
			for _, c := range a.Choices {
				words = append(words, strings.ToLower(strings.TrimSpace(c)))
			}
		}
		for _, o := range r.FollowupOn {
			if slices.Contains(words, strings.ToLower(strings.TrimSpace(o))) {
				return true
			}
		}
		return false
	}
	return true
}

// quiet: a person dismissed it without a note for the agent (WP-197). That
// says only "no answer needed; carry on", which is what the agent did when it
// filed it, so the agent isn't told at all: not at a stop, not with its next
// prompt, and it isn't counted as news. It is still told when its other work
// waits on the request (blocking) or it asked to hear of a dismissal
// (--followup-on dismissed). "not a question" is Waypoint's own note, not the
// person's.
func (r *Request) quiet() bool {
	if r.State != RequestDismissed || r.Decision != "dismiss" || (r.Note != "" && r.Note != notAQuestion) || r.Blocking {
		return false
	}
	if r.Followup == "on" {
		for _, o := range r.FollowupOn {
			if o = strings.ToLower(strings.TrimSpace(o)); o == "dismiss" || o == "dismissed" {
				return false
			}
		}
	}
	return true
}

// AnsweredRequests counts the actor's decided requests the agent hasn't been
// told about, for the line `next` prints.
func (s *Service) AnsweredRequests(ctx context.Context, actor *Actor) int {
	if actor == nil || actor.User == nil || actor.User.ID == 0 {
		return 0
	}
	var n int
	// #nosec G202 -- only ? placeholders are joined in
	args := []any{actor.ID()}
	for _, st := range decidedStates {
		args = append(args, st)
	}
	mine := ""
	if actor.Session != 0 {
		mine, args = " AND session_id = ?", append(args, actor.Session)
	}
	_ = s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE user_id = ? AND decided_at IS NOT NULL AND delivered_at IS NULL AND via = '' AND state IN (`+placeholders(len(decidedStates))+`)`+mine, args...).Scan(&n)
	return n
}

// DecisionsText is what an agent is told about decided requests: one block,
// used as the Stop hook's reason and by SessionStart.
func DecisionsText(list []*Request) string {
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	if len(list) == 1 {
		b.WriteString("Waypoint: a request you filed was decided.\n")
	} else {
		fmt.Fprintf(&b, "Waypoint: %d requests you filed were decided.\n", len(list))
	}
	for _, r := range list {
		who := "a person"
		if r.DecidedBy != nil {
			who = r.DecidedBy.DisplayName
		}
		fmt.Fprintf(&b, "\n%s (%s) %q: ", r.Ref, r.Kind, excerpt(r.Title, 120))
		if r.Decision == closedByWatch || r.Decision == closedInSession {
			fmt.Fprintf(&b, "%s\n", cmp(r.Note, "Waypoint closed it."))
			continue
		}
		switch r.State {
		case RequestAnswered:
			fmt.Fprintf(&b, "%s answered:\n%s\n", who, r.Answer)
		case RequestApproved:
			fmt.Fprintf(&b, "%s approved it.\n", who)
			if r.Kind == RequestPermission && r.Tool != nil {
				b.WriteString(permissionApprovedText(r))
			}
		case RequestDenied:
			fmt.Fprintf(&b, "%s denied it.\n", who)
		case RequestDone:
			fmt.Fprintf(&b, "%s did it.\n", who)
		case RequestDismissed:
			fmt.Fprintf(&b, "%s dismissed it; no answer needed.\n", who)
		case RequestApplied:
			switch {
			case r.Result.NoChange:
				fmt.Fprintf(&b, "%s approved it, but it changed nothing: %s Nothing was written and no commit or PR was made, so the fix has NOT landed.\n", who, cmp(r.Result.Reason, "every file already had the requested content."))
			case r.Result.PRURL != "":
				fmt.Fprintf(&b, "%s approved it and Waypoint opened %s on branch %s. The change is on that branch, not in your worktree.\n", who, r.Result.PRURL, r.Result.Branch)
			case r.Result.Commit != "":
				fmt.Fprintf(&b, "%s approved it and it was applied in your worktree as commit %s.\n", who, shortSHA(r.Result.Commit))
			default:
				fmt.Fprintf(&b, "%s approved it and it was applied.\n", who)
			}
		case RequestFailed:
			if r.Kind == RequestCommand {
				fmt.Fprintf(&b, "%s ran it and it failed (%s), so it came back to you.\n", who, cmp(r.Error, "no exit code"))
				break
			}
			fmt.Fprintf(&b, "%s approved it but applying failed: %s\n", who, cmp(r.Error, "no reason given"))
		case RequestConflict:
			fmt.Fprintf(&b, "%s approved it, but the files changed since you made it, so nothing was written (%s).\n", who, cmp(r.Result.Reason, r.Error))
		}
		if r.Note != "" {
			fmt.Fprintf(&b, "Their note: %s\n", r.Note)
		}
		if r.Kind == RequestCommand || r.Kind == RequestAction {
			for _, c := range r.Cells {
				if len(c.Runs) == 0 {
					continue
				}
				run := c.Runs[0]
				if run.State == "queued" || run.State == "running" {
					continue
				}
				code := "?"
				if run.ExitCode != nil {
					code = strconv.Itoa(*run.ExitCode)
				}
				fmt.Fprintf(&b, "$ %s  (exit %s)\n%s\n", excerpt(run.Command, 200), code, tail(run.Output, 1500))
			}
		}
	}
	return b.String()
}

// tail is the last n bytes of s, on a line boundary where possible.
func tail(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	return "…\n" + s
}

// RequestDecision is what a person decides.
type RequestDecision struct {
	Decision string   `json:"decision"` // answer, approve, deny, done, dismiss
	Answers  []Answer `json:"answers,omitempty"`
	Text     string   `json:"text,omitempty"` // a one-question answer, typed
	Note     string   `json:"note,omitempty"`
	// NotAQuestion marks a dismissal of an auto-filed question that wasn't
	// one (WP-96), so false positives can be counted.
	NotAQuestion bool   `json:"not_a_question,omitempty"`
	ApplyPath    string `json:"apply_path,omitempty"` // runner, pr; "" picks the runner if online, else a pull request
	// Rule: approving a permission request also adds this allow rule to the
	// repo's .claude/settings.json, by pull request (WP-233).
	Rule string `json:"rule,omitempty"`
}

// allowed decisions per kind.
var requestDecisions = map[string][]string{
	RequestQuestion:   {"answer", "dismiss"},
	RequestChange:     {"approve", "deny", "dismiss"},
	RequestPermission: {"approve", "deny", "dismiss"},
	RequestCommand:    {"done", "dismiss"},
	RequestAction:     {"done", "dismiss"},
}

// DecideRequest records a person's decision and, for an approved change,
// applies it. Only people decide: agents can't answer, approve or run a
// request. An agent closing one (done, dismiss) means it no longer needs
// it, so its own request is withdrawn, the note kept as the reason (WP-198).
func (s *Service) DecideRequest(ctx context.Context, actor *Actor, ref string, in RequestDecision) (*Request, error) {
	in.Decision = strings.ToLower(strings.TrimSpace(in.Decision))
	if !actor.ActsAsPerson() {
		if in.Decision == "done" || in.Decision == "dismiss" {
			r, err := s.WithdrawRequest(ctx, actor, ref, in.Note)
			var perm *PermissionError
			if errors.Is(err, ErrForbidden) && !errors.As(err, &perm) {
				return nil, fmt.Errorf("deciding a request is for people, not agents: %w", err)
			}
			return r, err
		}
		if err := actor.RequirePersonDirect("deciding a request"); err != nil {
			return nil, fmt.Errorf("%w; the agent that asked can take it back: wp request withdraw %s -m WHY", err, ref)
		}
	}
	if err := actor.Need(PermRequestsAnswer); err != nil {
		return nil, err
	}
	if err := actor.RequirePersonDirect("deciding a request"); err != nil {
		return nil, err
	}
	if err := actor.CanWrite(); err != nil {
		return nil, err
	}
	id, err := s.resolveRequestID(ctx, s.db.R, actor, ref)
	if err != nil {
		return nil, err
	}
	in.Note = strings.TrimSpace(in.Note)
	if len(in.Note) > 2000 {
		return nil, invalid("a note can be at most 2000 characters")
	}
	if in.NotAQuestion && in.Decision == "dismiss" && in.Note == "" {
		in.Note = notAQuestion
	}
	in.Rule = strings.TrimSpace(in.Rule)
	var ruleRepo *setupRepo
	if in.Rule != "" {
		if in.Decision != "approve" {
			return nil, invalid("a rule goes with an approval")
		}
		list, err := s.loadRequests(ctx, actor, false, `r.id = ?`, id)
		if err != nil {
			return nil, err
		}
		if len(list) != 1 {
			return nil, notFound("request")
		}
		if list[0].State != RequestOpen {
			return nil, invalid("%s is already %s", list[0].Code, list[0].State)
		}
		if ruleRepo, err = s.checkRuleTarget(ctx, actor, list[0], in.Rule); err != nil {
			return nil, err
		}
	}
	if in.Decision == "answer" {
		list, err := s.loadRequests(ctx, actor, true, `r.id = ?`, id)
		if err != nil {
			return nil, err
		}
		if len(list) == 1 && list[0].State == RequestOpen && list[0].EnvRead != nil {
			r := list[0]
			if !s.canDecide(ctx, actor, r, map[int64]int{}) {
				return nil, fmt.Errorf("only %s or an editor of the project can decide this: %w", userName(r.User), ErrForbidden)
			}
			answers, _, err := checkAnswers(r.Questions, in)
			if err != nil {
				return nil, err
			}
			if len(answers) != 1 || len(answers[0].Choices) == 0 {
				return nil, invalid("pick %q or %q: a typed answer records nothing", EnvReadAllow, envReadDeny)
			}
			if err := s.vouchAnswered(ctx, actor, r, answers); err != nil {
				return nil, err
			}
		}
	}
	var applyChange bool
	var tmp *Request
	err = s.write(ctx, actor, func(t *tx) error {
		list, err := s.loadRequestsTx(ctx, t, actor, id)
		if err != nil {
			return err
		}
		r := list
		tmp = r
		if !s.canDecide(ctx, actor, r, map[int64]int{}) {
			return fmt.Errorf("only %s or an editor of the project can decide this: %w", userName(r.User), ErrForbidden)
		}
		retry := r.State == RequestFailed && r.Kind == RequestChange && in.Decision == "approve"
		if r.State != RequestOpen && !retry {
			return invalid("%s is already %s", r.Code, r.State)
		}
		if !slices.Contains(requestDecisions[r.Kind], in.Decision) {
			return invalid("a %s request is decided with: %s", r.Kind, strings.Join(requestDecisions[r.Kind], ", "))
		}
		now := s.nowMS()
		state, answer := "", ""
		var answers []Answer
		switch in.Decision {
		case "answer":
			answers, answer, err = checkAnswers(r.Questions, in)
			if err != nil {
				return err
			}
			state = RequestAnswered
		case "approve":
			state = RequestApproved
			if r.Kind == RequestChange {
				state = RequestApplying
				applyChange = true
				switch in.ApplyPath {
				case "", "runner", "pr":
				default:
					return invalid("apply_path must be runner or pr")
				}
				if in.ApplyPath == "pr" && len(r.files) > 0 && r.files[0].Outside {
					return invalid("%s changes files outside any git repository, so there is no pull request to open; approve it with --apply runner", r.Code)
				}
			}
		case "deny":
			state = RequestDenied
		case "done":
			state = RequestDone
		case "dismiss":
			state = RequestDismissed
		}
		if answers != nil {
			var p requestPayload
			var raw string
			if err := t.QueryRowContext(ctx, `SELECT payload FROM requests WHERE id = ?`, r.ID).Scan(&raw); err != nil {
				return err
			}
			_ = json.Unmarshal([]byte(raw), &p)
			p.Answers = answers
			b, _ := json.Marshal(p)
			if _, err := t.ExecContext(ctx, `UPDATE requests SET payload = ? WHERE id = ?`, string(b), r.ID); err != nil {
				return err
			}
		}
		r.State, r.Decision, r.Note = state, in.Decision, in.Note
		var delivered any
		if r.quiet() {
			delivered = now
		}
		if _, err := t.ExecContext(ctx, `UPDATE requests SET state = ?, decision = ?, note = ?, answer = ?, decided_by = ?, decided_at = ?, delivered_at = ?, snoozed_until = NULL,
			apply_path = ?, error = '', updated_at = ? WHERE id = ?`,
			state, in.Decision, in.Note, answer, actor.ID(), now, delivered, in.ApplyPath, now, r.ID); err != nil {
			return err
		}
		e := requestEvent(EvRequestUpdated, r.ID, r.Ref, r.Number, r.Kind, state, r.userID, r.projectID, r.sessionID, r.Title, r.Agent)
		e.Data["decision"] = decisionWord(in.Decision, state)
		if ruleRepo != nil {
			e.permRule = &permRuleJob{request: r.ID, repo: ruleRepo, rule: in.Rule}
		}
		t.emit(e)
		t.audit(AuditEntry{Action: "request." + in.Decision, Category: AuditApproval, ProjectID: derefInt(ptrInt(r.projectID)), TargetType: "request", TargetID: r.ID,
			Target: r.Ref + " " + r.Title, URL: r.URL, Detail: excerpt(cmp(answer, in.Note), 200), About: r.Agent})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if applyChange && tmp != nil {
		s.applyChange(ctx, actor, tmp.ID, in.ApplyPath)
	} else if tmp != nil {
		s.tellDecision(ctx, id)
	}
	return s.GetRequest(ctx, actor, strconv.FormatInt(id, 10), 0)
}

func userName(u *UserRef) string {
	if u == nil {
		return "the person it is for"
	}
	return u.DisplayName
}

func decisionWord(decision, state string) string {
	switch decision {
	case "answer":
		return "answered"
	case "approve":
		return cmp(map[string]string{RequestApplying: "approved"}[state], "approved")
	case "deny":
		return "denied"
	case "done":
		return "done"
	}
	return "dismissed"
}

// loadRequestsTx loads one request inside a transaction (so it sees the
// state the writer holds).
func (s *Service) loadRequestsTx(ctx context.Context, t *tx, actor *Actor, id int64) (*Request, error) {
	r, err := s.scanRequest(t.QueryRowContext(ctx, `SELECT `+requestCols+` FROM requests r WHERE r.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("request")
	}
	if err != nil {
		return nil, err
	}
	var key string
	if r.projectID.Valid {
		_ = t.QueryRowContext(ctx, `SELECT key FROM projects WHERE id = ?`, r.projectID.Int64).Scan(&key)
	}
	r.Code = fmt.Sprintf("R-%d", r.Number)
	r.Ref = requestRef(key, r.Number)
	r.URL = "/requests/" + r.Ref
	r.User = &UserRef{ID: r.userID}
	if u, err := s.userByID(ctx, t, r.userID); err == nil {
		ref := u.Ref()
		r.User = &ref
	}
	return r, nil
}

// checkAnswers validates a person's answers against the questions and
// composes the text the agent reads.
func checkAnswers(qs []Question, in RequestDecision) ([]Answer, string, error) {
	answers := in.Answers
	if len(answers) == 0 && strings.TrimSpace(in.Text) != "" && len(qs) == 1 {
		answers = []Answer{{Text: in.Text}}
	}
	if len(answers) != len(qs) {
		return nil, "", invalid("answer all %d question%s", len(qs), plural(len(qs)))
	}
	for i, a := range answers {
		q := qs[i]
		a.Text = strings.TrimSpace(a.Text)
		if len(a.Text) > 20_000 {
			return nil, "", invalid("an answer can be at most 20000 characters")
		}
		if len(a.Choices) == 0 && a.Text == "" {
			return nil, "", invalid("answer %q", excerpt(q.Question, 80))
		}
		if len(a.Choices) > 1 && !q.Multi {
			return nil, "", invalid("%q takes one choice", excerpt(q.Question, 80))
		}
		for _, c := range a.Choices {
			if !slices.ContainsFunc(q.Choices, func(x Choice) bool { return x.Label == c }) {
				return nil, "", invalid("%q isn't a choice of %q", c, excerpt(q.Question, 80))
			}
		}
		answers[i] = a
	}
	var parts []string
	for i, a := range answers {
		line := strings.Join(a.Choices, ", ")
		if a.Text != "" {
			if line != "" {
				line += ": "
			}
			line += a.Text
		}
		if len(qs) == 1 {
			parts = append(parts, line)
		} else {
			parts = append(parts, qs[i].Question+"\n→ "+line)
		}
	}
	return answers, strings.Join(parts, "\n\n"), nil
}

// SnoozeRequest hides an open request until a time (nil brings it back).
func (s *Service) SnoozeRequest(ctx context.Context, actor *Actor, ref string, until *time.Time) (*Request, error) {
	if err := actor.Need(PermRequestsAnswer); err != nil {
		return nil, err
	}
	if err := actor.RequirePersonDirect("snoozing a request"); err != nil {
		return nil, err
	}
	if until != nil && (until.Before(s.now()) || until.After(s.now().AddDate(1, 0, 0))) {
		return nil, invalid("snooze until a time within the next year")
	}
	id, err := s.resolveRequestID(ctx, s.db.R, actor, ref)
	if err != nil {
		return nil, err
	}
	err = s.write(ctx, actor, func(t *tx) error {
		r, err := s.loadRequestsTx(ctx, t, actor, id)
		if err != nil {
			return err
		}
		if !s.canDecide(ctx, actor, r, map[int64]int{}) {
			return ErrForbidden
		}
		if r.State != RequestOpen {
			return invalid("%s is already %s", r.Code, r.State)
		}
		var v any
		if until != nil {
			v = until.UnixMilli()
		}
		if _, err := t.ExecContext(ctx, `UPDATE requests SET snoozed_until = ?, updated_at = ? WHERE id = ?`, v, s.nowMS(), r.ID); err != nil {
			return err
		}
		e := requestEvent(EvRequestUpdated, r.ID, r.Ref, r.Number, r.Kind, r.State, r.userID, r.projectID, r.sessionID, r.Title, r.Agent)
		e.Data["decision"] = "snoozed"
		t.emit(e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetRequest(ctx, actor, strconv.FormatInt(id, 10), 0)
}

// WithdrawRequest takes back an open request: the agent that asked, or the
// person it is for, no longer needs the answer. reason, if given, is kept
// as the request's note for the person to read (WP-198).
func (s *Service) WithdrawRequest(ctx context.Context, actor *Actor, ref, reason string) (*Request, error) {
	if err := actor.Need(PermRequestsCreate); err != nil {
		return nil, err
	}
	if err := actor.CanWrite(); err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 2000 {
		return nil, invalid("a reason can be at most 2000 characters")
	}
	id, err := s.resolveRequestID(ctx, s.db.R, actor, ref)
	if err != nil {
		return nil, err
	}
	err = s.write(ctx, actor, func(t *tx) error {
		r, err := s.loadRequestsTx(ctx, t, actor, id)
		if err != nil {
			return err
		}
		if r.userID != actor.ID() || ((actor.Restricted() || actor.AgentName() != "") && r.Agent != actor.AgentName()) {
			return fmt.Errorf("only the agent that asked can withdraw %s: %w", r.Code, ErrForbidden)
		}
		if r.State != RequestOpen {
			return invalid("%s is already %s", r.Code, r.State)
		}
		now := s.nowMS()
		if _, err := t.ExecContext(ctx, `UPDATE requests SET state = 'withdrawn', decision = 'withdraw', note = ?, delivered_at = ?, snoozed_until = NULL, updated_at = ? WHERE id = ?`,
			reason, now, now, r.ID); err != nil {
			return err
		}
		e := requestEvent(EvRequestUpdated, r.ID, r.Ref, r.Number, r.Kind, RequestWithdrawn, r.userID, r.projectID, r.sessionID, r.Title, r.Agent)
		e.Data["decision"] = "withdrawn"
		t.emit(e)
		if reason != "" {
			t.audit(AuditEntry{Action: "request.withdraw", Category: AuditApproval, ProjectID: derefInt(ptrInt(r.projectID)), TargetType: "request", TargetID: r.ID,
				Target: r.Ref + " " + r.Title, URL: r.URL, Detail: excerpt(reason, 200), About: r.Agent})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetRequest(ctx, actor, strconv.FormatInt(id, 10), 0)
}

// Waiting is everything waiting on the actor, in one place (WP-D16).
type Waiting struct {
	Requests []*Request `json:"requests"` // open requests for you, newest first
	// Design counts the design notes and edits to approve; Review the
	// agent-written content to review. Pulls would count pull requests
	// asking for your review; Waypoint doesn't hold that yet, so it is 0.
	Design int `json:"design"`
	Review int `json:"review"`
	Pulls  int `json:"pulls"`
	// Closes counts the issues agents closed that no person has reviewed
	// the close of (WP-272). A person may review them; nothing waits on it,
	// so they don't count in Total.
	Closes int `json:"closes"`
	// Outside counts the issues holding text from outside (WP-279) for you
	// to read and approve.
	Outside int `json:"outside"`
	// OutsidePulls counts the open pull requests holding text from outside
	// (WP-281) for you to read and approve.
	OutsidePulls int `json:"outside_pulls"`
	// Hooks are the repos of projects you own whose webhook isn't getting
	// through (WP-31); they count once, as one fix usually mends them all.
	Hooks []*HookProblem `json:"hooks"`
	// Total is what the Inbox badge counts, besides unread notifications.
	Total int `json:"total"`
}

// WaitingOnYou gathers what waits on the actor.
func (s *Service) WaitingOnYou(ctx context.Context, actor *Actor) (*Waiting, error) {
	return s.waitingOnYou(ctx, actor, "", "")
}

// waitingOnYou is WaitingOnYou narrowed to a project (a key) and an agent
// (a spec), for the Agents overview. Narrowed, the counts are of what the
// narrowing leaves.
func (s *Service) waitingOnYou(ctx context.Context, actor *Actor, project, agent string) (*Waiting, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	w := &Waiting{Requests: []*Request{}, Hooks: []*HookProblem{}}
	list, err := s.loadRequests(ctx, actor, false, `r.user_id = ? AND r.state = 'open' AND (r.snoozed_until IS NULL OR r.snoozed_until <= ?) ORDER BY r.id DESC LIMIT 50`, actor.ID(), s.nowMS())
	if err != nil {
		return nil, err
	}
	for _, r := range list {
		if project != "" && (r.Project == nil || !strings.EqualFold(r.Project.Key, project)) {
			continue
		}
		if agent != "" && !AgentMatches(agent, r.Agent) {
			continue
		}
		w.Requests = append(w.Requests, r)
	}
	open := len(w.Requests)
	if project == "" && agent == "" {
		if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE user_id = ? AND state = 'open' AND (snoozed_until IS NULL OR snoozed_until <= ?)`, actor.ID(), s.nowMS()).Scan(&open); err != nil {
			return nil, err
		}
	}
	if items, err := s.DesignReview(ctx, actor, project); err == nil {
		for _, it := range items {
			by := it.Agent
			if by == "" && it.Revision != nil {
				by = it.Revision.Agent
			}
			if agent == "" || AgentMatches(agent, by) {
				w.Design++
			}
		}
	}
	if items, err := s.listUnreviewed(ctx, actor, project, 500, []string{ReviewIssue, ReviewComment, ReviewPage, ReviewGlossary}); err == nil {
		for _, it := range items {
			if agent == "" || AgentMatches(agent, it.Agent) {
				w.Review++
			}
		}
	}
	if items, err := s.listUnreviewed(ctx, actor, project, 500, []string{ReviewClose}); err == nil {
		for _, it := range items {
			if agent == "" || AgentMatches(agent, it.Agent) {
				w.Closes++
			}
		}
	}
	if agent == "" {
		if w.Hooks, err = s.hookProblems(ctx, actor, project); err != nil {
			return nil, err
		}
		w.Outside = s.outsideWaiting(ctx, actor, project)
		w.OutsidePulls = s.outsidePullsWaiting(ctx, actor, project)
	}
	w.Total = open + w.Design + w.Review + w.Pulls + w.Outside + w.OutsidePulls
	if len(w.Hooks) > 0 {
		w.Total++
	}
	return w, nil
}
