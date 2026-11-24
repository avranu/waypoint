package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// A request closes itself when a condition it watches holds, checked deterministically with no model.

// Watch kinds.
const (
	WatchPRMerged  = "pr-merged"
	WatchIssueDone = "issue-done" // A canceled issue counts as done too.
	WatchURLOK     = "url-ok"
	WatchCmdOK     = "cmd-ok"
)

// WatchKinds in the order help lists them.
var WatchKinds = []string{WatchPRMerged, WatchIssueDone, WatchURLOK, WatchCmdOK}

const (
	maxWatches       = 5
	watchURLEvery    = 5 * time.Minute
	watchCmdEvery    = 10 * time.Minute
	watchMinURLEvery = time.Minute
	watchMinCmdEvery = 5 * time.Minute
	watchMaxEvery    = 24 * time.Hour
)

// WatchInput is a condition to close a request when it holds.
type WatchInput struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Every  int    `json:"every,omitempty"` // seconds between polls of url-ok and cmd-ok
}

// RequestWatch is a condition as people read it: what it watches and its
// last check.
type RequestWatch struct {
	ID          int64      `json:"id"`
	Kind        string     `json:"kind"`
	Target      string     `json:"target"`
	Summary     string     `json:"summary"`
	State       string     `json:"state"` // watching, satisfied
	Every       int        `json:"every,omitempty"`
	CheckedAt   *time.Time `json:"checked_at,omitempty"`
	Note        string     `json:"note,omitempty"` // what the last check found
	SatisfiedAt *time.Time `json:"satisfied_at,omitempty"`
}

func watchSummary(kind, target string) string {
	switch kind {
	case WatchPRMerged:
		return "closes when pull request " + target + " merges"
	case WatchIssueDone:
		return "closes when " + target + " is done"
	case WatchURLOK:
		return "closes when " + target + " answers 2xx"
	case WatchCmdOK:
		return "closes when `" + excerpt(target, 80) + "` succeeds"
	}
	return kind + " " + target
}

var prTargetRE = lazyre.New(`^(?:([\w.-]+(?:/[\w.-]+)+))?#?(\d+)$`)
var prURLRE = lazyre.New(`^/(.+?)/(?:-/)?(?:pulls?|merge_requests)/(\d+)`)

// parsePRTarget turns "owner/repo#15", "#15", "15" or a pull request's URL
// into owner/repo#15; repo is the session's repo for a bare number.
func parsePRTarget(target, repo string) (string, error) {
	target = strings.TrimSpace(target)
	if strings.Contains(target, "://") {
		u, err := url.Parse(target)
		if err != nil {
			return "", invalid("%q isn't a pull request address", target)
		}
		if m := prURLRE.FindStringSubmatch(u.Path); m != nil {
			return m[1] + "#" + m[2], nil
		}
		return "", invalid("%q isn't a pull request address", target)
	}
	m := prTargetRE.FindStringSubmatch(target)
	if m == nil {
		return "", invalid("a pull request is owner/repo#15, #15 or its URL, not %q", target)
	}
	if m[1] != "" {
		return m[1] + "#" + m[2], nil
	}
	if repo == "" {
		return "", invalid("name the repository: owner/repo#%s", m[2])
	}
	return repo + "#" + m[2], nil
}

// checkWatch validates a condition and resolves what it points at. sess is
// the request's session (nil for a personal request).
func (s *Service) checkWatch(ctx context.Context, t *tx, actor *Actor, sess *AgentSession, in WatchInput) (*watchRow, error) {
	w := &watchRow{kind: strings.ToLower(strings.TrimSpace(in.Kind)), target: strings.TrimSpace(in.Target)}
	if w.target == "" {
		return nil, invalid("a condition needs a target: %s=…", w.kind)
	}
	if len(w.target) > 2000 {
		return nil, invalid("a condition's target can be at most 2000 characters")
	}
	every := time.Duration(in.Every) * time.Second
	switch w.kind {
	case WatchPRMerged:
		repo := ""
		if sess != nil {
			repo = sess.Repo
		}
		target, err := parsePRTarget(w.target, repo)
		if err != nil {
			return nil, err
		}
		w.target = target
	case WatchIssueDone:
		i, _, err := s.getIssueFor(ctx, t, actor, w.target, RoleViewer, false)
		if err != nil {
			return nil, err
		}
		w.target, w.refID = i.Key, i.ID
	case WatchURLOK:
		u, err := url.Parse(w.target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return nil, invalid("url-ok needs an http or https URL without credentials")
		}
		w.every = clampEvery(every, watchURLEvery, watchMinURLEvery)
	case WatchCmdOK:
		// A command runs on a person's machine: only a person sets one.
		if actor.IsAgent() {
			return nil, invalid("a command poll runs on your person's machine, so only a person can set one; use pr-merged, issue-done or url-ok, or ask them")
		}
		if sess == nil || sess.Machine == "" || sess.Cwd == "" {
			return nil, invalid("cmd-ok runs on the machine of the request's session; this request has none")
		}
		w.every = clampEvery(every, watchCmdEvery, watchMinCmdEvery)
		w.machine, w.dir, w.runBy = sess.Machine, sess.Cwd, actor.id()
	default:
		return nil, invalid("a condition is one of %s", strings.Join(WatchKinds, ", "))
	}
	return w, nil
}

func clampEvery(d, def, lo time.Duration) int {
	if d == 0 {
		d = def
	}
	return int(min(max(d, lo), watchMaxEvery) / time.Second)
}

type watchRow struct {
	id, requestID, refID, runBy, jobID int64
	kind, target, machine, dir         string
	every                              int
	nextAt                             int64
}

func (s *Service) insertWatch(ctx context.Context, t *tx, requestID int64, w *watchRow, at int64) error {
	var runBy any
	if w.runBy != 0 {
		runBy = w.runBy
	}
	_, err := t.ExecContext(ctx, `INSERT INTO request_watches(request_id, kind, target, ref_id, every_sec, machine, dir, run_by, next_at, created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		requestID, w.kind, w.target, w.refID, w.every, w.machine, w.dir, runBy, at, at)
	return err
}

// AddRequestWatch adds a condition to an open request: its owner (the agent
// that filed it) or someone who may decide it.
func (s *Service) AddRequestWatch(ctx context.Context, actor *Actor, ref string, in WatchInput) (*Request, error) {
	id, err := s.resolveRequestID(ctx, s.db.R, actor, ref)
	if err != nil {
		return nil, err
	}
	err = s.write(ctx, actor, func(t *tx) error {
		r, err := s.loadRequestsTx(ctx, t, actor, id)
		if err != nil {
			return err
		}
		if r.userID != actor.id() && !s.canDecide(ctx, actor, r, map[int64]int{}) {
			return fmt.Errorf("only %s or an editor of the project can change this: %w", userName(r.User), ErrForbidden)
		}
		if r.State != RequestOpen {
			return invalid("%s is already %s", r.Code, r.State)
		}
		var n int
		if err := t.QueryRowContext(ctx, `SELECT count(*) FROM request_watches WHERE request_id = ?`, id).Scan(&n); err != nil {
			return err
		}
		if n >= maxWatches {
			return invalid("a request can watch at most %d conditions", maxWatches)
		}
		var sess *AgentSession
		if r.sessionID.Valid {
			var err error
			if sess, err = scanSession(t.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM agent_sessions s WHERE s.id = ?`, r.sessionID.Int64), s.nowMS()); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		w, err := s.checkWatch(ctx, t, actor, sess, in)
		if err != nil {
			return err
		}
		if err := s.insertWatch(ctx, t, id, w, s.nowMS()); err != nil {
			return err
		}
		t.emit(requestEvent(EvRequestUpdated, r.ID, r.Ref, r.Number, r.Kind, RequestOpen, r.userID, r.projectID, r.sessionID, r.Title, r.Agent))
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.nudgeRequestWatches()
	return s.GetRequest(ctx, actor, strconv.FormatInt(id, 10), 0)
}

// fillWatches adds each request's conditions.
func (s *Service) fillWatches(ctx context.Context, ids []int64, byID map[int64]*Request) error {
	// #nosec G202 -- only ? placeholders are joined in
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, request_id, kind, target, every_sec, checked_at, note, satisfied_at FROM request_watches WHERE request_id IN (`+placeholders(len(ids))+`) ORDER BY id`, int64Args(ids)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var w RequestWatch
		var rid int64
		var checked, satisfied sql.NullInt64
		if err := rows.Scan(&w.ID, &rid, &w.Kind, &w.Target, &w.Every, &checked, &w.Note, &satisfied); err != nil {
			return err
		}
		w.Summary, w.State = watchSummary(w.Kind, w.Target), "watching"
		w.CheckedAt, w.SatisfiedAt = ptrTime(checked), ptrTime(satisfied)
		if satisfied.Valid {
			w.State = "satisfied"
		}
		byID[rid].Watches = append(byID[rid].Watches, &w)
	}
	return rows.Err()
}

// Why a request was closed by Waypoint (decision "auto" or "session").
const (
	closedByWatch   = "auto"
	closedInSession = "session"
)

// autoClosed: the state a request closes in, by what it asked.
func autoClosedState(kind string) string {
	switch kind {
	case RequestPermission:
		return RequestApproved
	case RequestCommand, RequestAction:
		return RequestDone
	}
	return RequestDismissed
}

// closeRequestTx closes an open request: decided by Waypoint, with the note.
// told says the agent already knows (it ran the command itself); otherwise
// its next prompt carries the closing. It reports whether it closed.
func (s *Service) closeRequestTx(ctx context.Context, t *tx, id int64, how, note string, told bool) (bool, error) {
	var number int
	var kind, title, agent, key string
	var user int64
	var project, session sql.NullInt64
	err := t.QueryRowContext(ctx, `SELECT r.number, r.kind, r.title, r.agent, r.user_id, r.project_id, r.session_id, coalesce(p.key, '') FROM requests r LEFT JOIN projects p ON p.id = r.project_id
		WHERE r.id = ? AND r.state = 'open'`, id).Scan(&number, &kind, &title, &agent, &user, &project, &session, &key)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	state := autoClosedState(kind)
	now := s.nowMS()
	var delivered any
	if told {
		delivered = now
	}
	res, err := t.ExecContext(ctx, `UPDATE requests SET state = ?, decision = ?, note = ?, decided_at = ?, delivered_at = ?, snoozed_until = NULL, updated_at = ? WHERE id = ? AND state = 'open'`,
		state, how, note, now, delivered, now, id)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	ref := requestRef(key, number)
	e := requestEvent(EvRequestUpdated, id, ref, number, kind, state, user, project, session, title, agent)
	e.Data["decision"] = how
	e.Data["note"] = note
	t.emit(e)
	t.audit(AuditEntry{Action: "request." + how, Category: AuditApproval, ProjectID: derefInt(ptrInt(project)), TargetType: "request", TargetID: id,
		Target: ref + " " + title, URL: "/requests/" + ref, Detail: excerpt(note, 200), About: agent})
	return true, nil
}

// toolDedupe identifies a tool call: a refused call's request and a later run
// of the same call have the same key.
func toolDedupe(tool string, input json.RawMessage) string {
	sum := sha256.Sum256([]byte(tool + "\x00" + canonicalJSON(input)))
	return hex.EncodeToString(sum[:8])
}

// closeRanInSession closes the session's open requests for a tool call that
// has now run there (PostToolUse): a person approves the command in the
// agent's own terminal, the agent runs it, and the request in Waypoint is
// satisfied. A permission request matches the same tool call; a command
// request matches a single command that is exactly the Bash call.
func (s *Service) closeRanInSession(ctx context.Context, t *tx, x *AgentSession, ev *HookEvent) error {
	if ev.Tool == "" || len(ev.ToolInput) == 0 {
		return nil
	}
	key := toolDedupe(ev.Tool, ev.ToolInput)
	if err := markRefusalRan(ctx, t, x.ID, key, s.nowMS()); err != nil {
		return err
	}
	var cmd string
	if ev.Tool == "Bash" {
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(ev.ToolInput, &in)
		cmd = strings.TrimSpace(in.Command)
	}
	rows, err := t.QueryContext(ctx, `SELECT r.id, r.kind, r.title FROM requests r
		WHERE r.session_id = ? AND r.state = 'open' AND ((r.kind = 'permission' AND r.dedupe_key = ?) OR (r.kind = 'command' AND ? != '')) ORDER BY r.id`, x.ID, key, cmd)
	if err != nil {
		return err
	}
	type cand struct {
		id    int64
		kind  string
		title string
	}
	var list []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.kind, &c.title); err != nil {
			rows.Close()
			return err
		}
		list = append(list, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range list {
		if c.kind == RequestCommand {
			var n int
			var only string
			if err := t.QueryRowContext(ctx, `SELECT count(*), coalesce(min(command), '') FROM request_cells WHERE request_id = ?`, c.id).Scan(&n, &only); err != nil {
				return err
			}
			if n != 1 || strings.TrimSpace(only) != cmd {
				continue
			}
		}
		if _, err := s.closeRequestTx(ctx, t, c.id, closedInSession, "Approved and run in the agent's own session.", true); err != nil {
			return err
		}
	}
	return nil
}

// nudgeRequestWatches checks the conditions that need no network (a pull
// request merged, an issue done) soon, after something changed.
func (s *Service) nudgeRequestWatches() {
	s.goBG(func() { s.checkRequestWatches(context.Background(), false) })
}

// CheckRequestWatches is the minute sweep: every condition of an open
// request that is due, polled ones included.
func (s *Service) CheckRequestWatches(ctx context.Context) {
	s.checkRequestWatches(ctx, true)
	s.closeStaleRequests(ctx)
}

func (s *Service) checkRequestWatches(ctx context.Context, polls bool) {
	now := s.nowMS()
	q := `SELECT w.id, w.request_id, w.kind, w.target, w.ref_id, w.every_sec, w.machine, w.dir, coalesce(w.run_by, 0), w.job_id, w.next_at
		FROM request_watches w JOIN requests r ON r.id = w.request_id
		WHERE w.satisfied_at IS NULL AND r.state = 'open' AND (w.kind IN ('pr-merged', 'issue-done') OR (? AND w.next_at <= ?)) ORDER BY w.id LIMIT 500`
	rows, err := s.db.R.QueryContext(ctx, q, polls, now)
	if err != nil {
		s.log.WarnContext(ctx, "list request conditions", "err", err)
		return
	}
	var list []*watchRow
	for rows.Next() {
		w := &watchRow{}
		if err := rows.Scan(&w.id, &w.requestID, &w.kind, &w.target, &w.refID, &w.every, &w.machine, &w.dir, &w.runBy, &w.jobID, &w.nextAt); err != nil {
			rows.Close()
			s.log.WarnContext(ctx, "read a request condition", "err", err)
			return
		}
		list = append(list, w)
	}
	rows.Close()
	for _, w := range list {
		ok, note := s.evalWatch(ctx, w)
		if note == "" && !ok {
			continue
		}
		s.recordCheck(ctx, w, ok, note)
	}
}

// evalWatch checks one condition. An empty note with false: nothing to
// record yet.
func (s *Service) evalWatch(ctx context.Context, w *watchRow) (bool, string) {
	switch w.kind {
	case WatchPRMerged:
		repo, num, _ := strings.Cut(w.target, "#")
		var state string
		err := s.db.R.QueryRowContext(ctx, `SELECT state FROM pull_requests WHERE lower(repo) = lower(?) AND number = ? ORDER BY (state = 'merged') DESC LIMIT 1`, repo, num).Scan(&state)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return false, "Waypoint has not seen " + w.target + " yet."
		case err != nil:
			return false, "could not read the pull request: " + err.Error()
		case state == "merged":
			return true, "Pull request " + w.target + " merged."
		case state == "closed":
			return false, w.target + " is closed without merging; still watching."
		}
		return false, w.target + " is " + state + "."
	case WatchIssueDone:
		var name, cat string
		err := s.db.R.QueryRowContext(ctx, `SELECT st.name, st.category FROM issues i JOIN statuses st ON st.id = i.status_id WHERE i.id = ?`, w.refID).Scan(&name, &cat)
		switch {
		case err != nil:
			return false, "could not read " + w.target + "."
		case cat == "done" || cat == "canceled":
			return true, w.target + " is " + name + "."
		}
		return false, w.target + " is " + name + "."
	case WatchURLOK:
		code, _, _, err := s.envGet(ctx, w.target)
		switch {
		case err != nil && code == 0:
			return false, "no answer: " + envErrText(err)
		case code >= 200 && code < 300:
			return true, fmt.Sprintf("%s answered %d.", w.target, code)
		}
		return false, fmt.Sprintf("%s answered %d.", w.target, code)
	case WatchCmdOK:
		return s.pollCommand(ctx, w)
	}
	return false, "unknown condition " + w.kind
}

// pollCommand starts the command on the person's runner; the job's end
// decides (watchJobFinished).
func (s *Service) pollCommand(ctx context.Context, w *watchRow) (bool, string) {
	if w.jobID != 0 {
		var state string
		if err := s.db.R.QueryRowContext(ctx, `SELECT state FROM runner_jobs WHERE id = ?`, w.jobID).Scan(&state); err == nil && (state == "queued" || state == "running" || state == "claimed") {
			return false, ""
		}
	}
	raw, _ := json.Marshal(CommandJob{Command: w.target, Dir: w.dir})
	job, err := enqueueRunnerJob(ctx, s, &Actor{User: &User{ID: w.runBy}}, RunnerJobInput{Kind: JobCommand, OwnerID: w.runBy, Label: w.machine, Input: raw,
		Source: "request_watch", SourceID: w.id, Timeout: 120})
	if err != nil {
		return false, "could not start the check: " + err.Error()
	}
	_, _ = s.db.W.ExecContext(ctx, `UPDATE request_watches SET job_id = ?, next_at = ? WHERE id = ?`, job.ID, s.nowMS()+int64(max(w.every, 60))*1000, w.id)
	return false, ""
}

// watchJobFinished records a command poll's result.
func (s *Service) watchJobFinished(ctx context.Context, e *Event) {
	if src, _ := e.Data["source"].(string); src != "request_watch" {
		return
	}
	id := dataInt(e, "source_id")
	jobID := dataInt(e, "job")
	state, _ := e.Data["state"].(string)
	var w watchRow
	if err := s.db.R.QueryRowContext(ctx, `SELECT id, request_id, every_sec FROM request_watches WHERE id = ? AND satisfied_at IS NULL`, id).Scan(&w.id, &w.requestID, &w.every); err != nil {
		return
	}
	job := &RunnerJob{ID: jobID, State: state}
	if lookupRunnerJob != nil && jobID != 0 {
		if j, err := lookupRunnerJob(ctx, s, jobID); err == nil && j != nil {
			job = j
		}
	}
	if _, ok := e.Data["exit_code"]; ok && job.ExitCode == nil {
		c := int(dataInt(e, "exit_code"))
		job.ExitCode = &c
	}
	switch {
	case job.ExitCode != nil && *job.ExitCode == 0:
		s.recordCheck(ctx, &w, true, "The command succeeded.")
	case job.ExitCode != nil:
		s.recordCheck(ctx, &w, false, fmt.Sprintf("The command exited %d.", *job.ExitCode))
	default:
		s.recordCheck(ctx, &w, false, "The command did not run: "+cmp(job.Error, state)+".")
	}
}

// recordCheck keeps the last check and, when the condition holds, closes the
// request.
func (s *Service) recordCheck(ctx context.Context, w *watchRow, ok bool, note string) {
	now := s.nowMS()
	next := now + int64(max(w.every, 60))*1000
	if _, err := s.db.W.ExecContext(ctx, `UPDATE request_watches SET checked_at = ?, note = ?, next_at = ?, job_id = 0 WHERE id = ? AND satisfied_at IS NULL`, now, note, next, w.id); err != nil {
		s.log.WarnContext(ctx, "record a request check", "watch", w.id, "err", err)
		return
	}
	if !ok {
		return
	}
	err := s.write(ctx, SystemActor(), func(t *tx) error {
		if _, err := t.ExecContext(ctx, `UPDATE request_watches SET satisfied_at = ? WHERE id = ?`, now, w.id); err != nil {
			return err
		}
		_, err := s.closeRequestTx(ctx, t, w.requestID, closedByWatch, "Closed automatically: "+note, false)
		return err
	})
	if err != nil {
		s.log.WarnContext(ctx, "close a request whose condition holds", "request", w.requestID, "err", err)
	}
}

// closeStaleRequests closes open requests whose work has landed (WP-126):
// the pull request for the branch the agent asked from, or for the issue it
// asked about, has merged since. The agent can't act on a decision about
// work that has shipped, and the person shouldn't have to click it away.
// Only questions, permissions and file changes go moot that way: an action
// or a command is something a person does outside the code (set a key on a
// server, run a migration), and a merge says nothing about whether it's done.
func (s *Service) closeStaleRequests(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT r.id, pr.repo, pr.number FROM requests r
		JOIN pull_requests pr ON pr.state = 'merged' AND pr.merged_at IS NOT NULL AND pr.merged_at > r.created_at
		WHERE r.state = 'open' AND r.kind IN ('question', 'permission', 'change') AND (
		  (r.issue_id IS NOT NULL AND EXISTS (SELECT 1 FROM external_links l WHERE l.issue_id = r.issue_id AND l.kind = 'pull' AND l.number = pr.number AND lower(l.repo) = lower(pr.repo)))
		  OR (r.session_id IS NOT NULL AND EXISTS (SELECT 1 FROM agent_sessions x WHERE x.id = r.session_id AND x.branch = pr.head_ref AND x.branch != pr.base_ref AND lower(x.repo) = lower(pr.repo))))
		ORDER BY r.id LIMIT 200`)
	if err != nil {
		s.log.WarnContext(ctx, "find stale requests", "err", err)
		return
	}
	type stale struct {
		id     int64
		repo   string
		number int
	}
	var list []stale
	for rows.Next() {
		var x stale
		if err := rows.Scan(&x.id, &x.repo, &x.number); err != nil {
			rows.Close()
			return
		}
		list = append(list, x)
	}
	rows.Close()
	for _, x := range list {
		note := fmt.Sprintf("Closed automatically: pull request %s#%d merged since this was asked, so it is moot.", x.repo, x.number)
		err := s.write(ctx, SystemActor(), func(t *tx) error {
			_, err := s.closeRequestTx(ctx, t, x.id, closedByWatch, note, false)
			return err
		})
		if err != nil {
			s.log.WarnContext(ctx, "close a stale request", "request", x.id, "err", err)
		}
	}
}
