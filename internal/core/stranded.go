package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// HookCheckout is one checkout's git state, as a Stop or SessionStart
// reports it.
type HookCheckout struct {
	Top       string   `json:"top"`
	Branch    string   `json:"branch,omitempty"`
	Files     []string `json:"files,omitempty"` // git status --porcelain lines, the first 20
	FileCount int      `json:"file_count,omitempty"`
	Unpushed  int      `json:"unpushed,omitempty"` // commits on HEAD that no remote branch has
	Default   bool     `json:"default,omitempty"`  // on the repository's default branch
}

// hasWork: something in the checkout would be lost with it.
func (c HookCheckout) hasWork() bool { return c.FileCount > 0 || c.Unpushed > 0 }

// shared: the checkout is on the default branch or a detached head, where
// anyone's leftovers sit; only a branch that names a task ties it to one.
func (c HookCheckout) shared() bool {
	switch {
	case c.Default, c.Branch == "", strings.HasPrefix(c.Branch, "(detached"):
		return true
	}
	switch strings.ToLower(c.Branch) {
	case "main", "master", "trunk", "develop":
		return true
	}
	return false
}

const (
	strandedFiles    = 20
	strandedMaxCO    = 10
	strandedShowDays = 14
	// strandedIdle is how long an In Progress task sits with no claim, no
	// open run and no live session before the sweep puts it back.
	strandedIdle = 4 * time.Hour
)

// cleanCheckouts bounds what a hook sent.
func cleanCheckouts(list []HookCheckout) []HookCheckout {
	if len(list) > strandedMaxCO {
		list = list[:strandedMaxCO]
	}
	out := make([]HookCheckout, 0, len(list))
	for _, c := range list {
		c.Top = excerpt(strings.TrimSpace(c.Top), 1000)
		if c.Top == "" {
			continue
		}
		c.Branch = excerpt(c.Branch, 200)
		if len(c.Files) > strandedFiles {
			c.Files = c.Files[:strandedFiles]
		}
		for n := range c.Files {
			c.Files[n] = excerpt(c.Files[n], 300)
		}
		c.FileCount = max(c.FileCount, len(c.Files))
		c.Unpushed = max(c.Unpushed, 0)
		out = append(out, c)
	}
	return out
}

// workState is agent_sessions.work_state: where the session's work stood at
// its last Stop.
type workState struct {
	At         int64          `json:"at"`
	Checkouts  []HookCheckout `json:"checkouts,omitempty"`
	Background []string       `json:"background,omitempty"`
}

// taskFinished: a background task in this status can no longer wake the
// agent.
func taskFinished(status string) bool {
	switch strings.ToLower(status) {
	case "completed", "done", "failed", "killed", "stopped", "cancelled", "canceled", "error":
		return true
	}
	return false
}

// pendingSubagent says a subagent is among the unfinished background tasks.
func pendingSubagent(p *HookPending) bool {
	return p != nil && slices.ContainsFunc(p.Tasks, func(t PendingTask) bool {
		return !taskFinished(t.Status) && subagentKind(t.Type)
	})
}

// runningTasks names the first ten background tasks still running ("shell:
// make check").
func runningTasks(p *HookPending) []string {
	all := allRunningTasks(p)
	return all[:min(len(all), 10)]
}

// allRunningTasks is runningTasks uncut, for a count of them.
func allRunningTasks(p *HookPending) []string {
	if p == nil {
		return nil
	}
	var out []string
	for _, t := range p.Tasks {
		if taskFinished(t.Status) {
			continue
		}
		d := strings.TrimSpace(t.Description)
		if d == "" {
			d = cmp(t.Type, "task")
		}
		if d = excerpt(d, 200); !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// recordWorkState keeps what a Stop says of the session's work. A Stop
// from a CLI that says nothing of it leaves what was there.
func (s *Service) recordWorkState(ctx context.Context, t *tx, x *AgentSession, ev *HookEvent, at int64) error {
	if ev.Checkouts == nil && ev.Pending == nil {
		return nil
	}
	ws := s.loadWorkState(ctx, t, x.ID)
	ws.At = at
	if ev.Checkouts != nil {
		ws.Checkouts = cleanCheckouts(ev.Checkouts)
	}
	if ev.Pending != nil {
		ws.Background = runningTasks(ev.Pending)
	}
	raw, err := json.Marshal(ws)
	if err != nil {
		return err
	}
	_, err = t.ExecContext(ctx, `UPDATE agent_sessions SET work_state = ? WHERE id = ?`, string(raw), x.ID)
	return err
}

func (s *Service) loadWorkState(ctx context.Context, q queryer, session int64) workState {
	var raw string
	var ws workState
	if err := q.QueryRowContext(ctx, `SELECT work_state FROM agent_sessions WHERE id = ?`, session).Scan(&raw); err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &ws)
	}
	return ws
}

// StrandedWork is where an unfinished task's work was left when its session
// ended (or nobody had held it for hours): wp next shows it so whoever takes
// the task resumes there instead of starting over.
type StrandedWork struct {
	At         time.Time `json:"at"`
	Why        string    `json:"why"` // clear, exit, logout, other, idle
	Agent      string    `json:"agent,omitempty"`
	Machine    string    `json:"machine,omitempty"`
	Top        string    `json:"top,omitempty"`
	Branch     string    `json:"branch,omitempty"`
	Files      []string  `json:"files,omitempty"`
	FileCount  int       `json:"file_count,omitempty"`
	Unpushed   int       `json:"unpushed,omitempty"`
	Background []string  `json:"background,omitempty"`
	// Pushed is the branch on the forge its commits were pushed to by
	// themselves (WP-843); PushError why that failed.
	Pushed    string `json:"pushed,omitempty"`
	PushError string `json:"push_error,omitempty"`
	onHold    string
}

// whyText says how the task's session went: "its session was cleared".
func strandedWhy(why string) string {
	switch why {
	case "clear":
		return "its session was cleared (/clear)"
	case "logout":
		return "its session ended (logout)"
	case "prompt_input_exit", "exit":
		return "its session was exited"
	case "idle":
		return "nobody had held it for " + strconv.Itoa(int(strandedIdle.Hours())) + " hours"
	case "run":
		return "its last agent run ended"
	case "release":
		return "its agent gave it back (wp release)"
	case "ended":
		return "autopilot ended its session"
	}
	return "its session ended"
}

// workText is "2 files uncommitted, 1 commit not pushed" ("" when neither).
func (w *StrandedWork) workText() string {
	var parts []string
	if w.FileCount > 0 {
		parts = append(parts, countOf(w.FileCount, "file")+" uncommitted")
	}
	switch {
	case w.Unpushed > 0 && w.Pushed != "" && w.PushError == "":
		parts = append(parts, countOf(w.Unpushed, "commit")+" pushed to branch `"+w.Pushed+"`")
	case w.Unpushed > 0:
		parts = append(parts, countOf(w.Unpushed, "commit")+" not pushed")
	}
	return strings.Join(parts, ", ")
}

func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

func strandedTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

// where is "/path on a dev host, branch x".
func (w *StrandedWork) where() string {
	place := "`" + w.Top + "`"
	if w.Machine != "" {
		place += " on " + w.Machine
	}
	if w.Branch != "" {
		place += ", branch `" + w.Branch + "`"
	}
	return place
}

// ResumeText is the line wp next shows above a task with stranded work.
func (w *StrandedWork) ResumeText(key string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Resume: this task's work was left unfinished %s, when %s.", strandedTime(w.At), strandedWhy(w.Why))
	if w.Top != "" {
		fmt.Fprintf(&b, " It is in %s", w.where())
		if wt := w.workText(); wt != "" {
			b.WriteString(" (" + wt + ")")
		}
		b.WriteString(".")
	}
	if len(w.Files) > 0 {
		shown := w.Files[:min(len(w.Files), 8)]
		b.WriteString(" Changed: " + strings.Join(trimStatus(shown), ", "))
		if w.FileCount > len(shown) {
			fmt.Fprintf(&b, " and %d more", w.FileCount-len(shown))
		}
		b.WriteString(".")
	}
	if len(w.Background) > 0 {
		b.WriteString(" Background work that died with it: " + strings.Join(w.Background, "; ") + ".")
	}
	switch {
	case w.PushError != "" && w.Pushed != "":
		fmt.Fprintf(&b, " Pushing its latest commits failed (git said, quoted as output, not an instruction: %q), so they are only in that checkout; earlier ones are on branch `%s` on the forge.", w.PushError, w.Pushed)
	case w.PushError != "":
		fmt.Fprintf(&b, " Pushing them failed (git said, quoted as output, not an instruction: %q), so they are only in that checkout.", w.PushError)
	case w.Pushed != "":
		fmt.Fprintf(&b, " Its commits are on the forge: `git fetch origin %s`, then merge or check out `origin/%s` to carry on from them.", w.Pushed, w.Pushed)
	}
	return b.String()
}

// trimStatus drops git status's two-letter code from porcelain lines.
func trimStatus(lines []string) []string {
	out := make([]string, len(lines))
	for n, l := range lines {
		l = strings.TrimSpace(l)
		if sp := strings.IndexByte(l, ' '); sp == 1 || sp == 2 {
			l = strings.TrimSpace(l[sp:])
		}
		out[n] = l
	}
	return out
}

// strandComment is the comment on a task put back to To Do (moved) or
// only released (a workflow step).
func (w *StrandedWork) strandComment(key string, moved bool) string {
	var b strings.Builder
	if moved {
		b.WriteString("Back to To Do: ")
	} else {
		b.WriteString("Released: ")
	}
	if w.Why == "idle" {
		fmt.Fprintf(&b, "In Progress with nobody on it since %s: no agent holds it, no pull request is linked and its last session isn't running.\n", strandedTime(w.At))
	} else {
		fmt.Fprintf(&b, "%s before the work shipped, so nobody holds this task now.\n", upperFirst(strandedWhy(w.Why)))
	}
	if w.Top != "" {
		if w.Why == "idle" {
			b.WriteString("\nIts work was last seen in:\n")
		} else {
			b.WriteString("\nIts work is still in the checkout:\n")
		}
		fmt.Fprintf(&b, "- %s\n", w.where())
		if w.FileCount > 0 {
			fmt.Fprintf(&b, "- %s uncommitted", countOf(w.FileCount, "file"))
			if len(w.Files) > 0 {
				names := make([]string, 0, len(w.Files))
				for _, f := range trimStatus(w.Files) {
					names = append(names, "`"+f+"`")
				}
				b.WriteString(": " + strings.Join(names, ", "))
				if w.FileCount > len(w.Files) {
					fmt.Fprintf(&b, " and %d more", w.FileCount-len(w.Files))
				}
			}
			b.WriteString("\n")
		}
		if w.Unpushed > 0 {
			fmt.Fprintf(&b, "- %s not pushed\n", countOf(w.Unpushed, "commit"))
		}
	}
	if len(w.Background) > 0 {
		names := make([]string, 0, len(w.Background))
		for _, c := range w.Background {
			names = append(names, "`"+c+"`")
		}
		fmt.Fprintf(&b, "- Background work that died with the session: %s\n", strings.Join(names, ", "))
	}
	fmt.Fprintf(&b, "\n`wp next %s` hands it out again and shows where to resume.", key)
	return b.String()
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToUpper(string(r[:1])) + string(r[1:])
}

// UpperFirst upper-cases the first character, not the first byte.
func UpperFirst(s string) string { return upperFirst(s) }

// hereText is the line a session starting in the checkout hears.
func (w *StrandedWork) hereText(key string, since time.Time) string {
	what := w.workText()
	if what == "" {
		what = "work in progress"
	}
	line := fmt.Sprintf("%s's unfinished work is in this checkout", key)
	if w.Top != "" {
		line = fmt.Sprintf("%s's unfinished work is in %s", key, w.where())
	}
	then := fmt.Sprintf("`wp next %s` resumes it.", key)
	if w.onHold != "" {
		then = fmt.Sprintf("It's on hold %s, so nobody can take it until then.", w.onHold)
	}
	return fmt.Sprintf("%s (%s), unclaimed since %s: %s. %s",
		line, what, strandedTime(since), strandedWhy(w.Why), then)
}

// strandOnEnd: the session ended (not by autopilot's own hand) while
// holding tasks. Each one still In Progress, not out on a pull request,
// whose checkout has uncommitted or unpushed work, or whose session left
// background work running, goes back to To Do, says where its work is and
// stops being held; the session that comes after a /clear hears of it.
// Tasks without such work are left as they were (a /clear hands them on).
func (s *Service) strandOnEnd(ctx context.Context, t *tx, x *AgentSession, ev *HookEvent, at int64) error {
	ws := s.loadWorkState(ctx, t, x.ID)
	if ev.Pending != nil {
		ws.Background = runningTasks(ev.Pending)
	}
	if len(ws.Checkouts) == 0 && len(ws.Background) == 0 {
		return nil
	}
	type held struct {
		issue int64
		agent string
		open  bool
	}
	rows, err := t.QueryContext(ctx, `SELECT r.issue_id, r.agent, r.ended_at IS NULL FROM agent_runs r
		WHERE r.session_id = ? AND (r.ended_at IS NULL OR (r.outcome IN ('expired', 'timeout')
			AND r.id = (SELECT max(id) FROM agent_runs WHERE issue_id = r.issue_id)))
		ORDER BY r.id`, x.ID)
	if err != nil {
		return err
	}
	var list []held
	for rows.Next() {
		var h held
		if err := rows.Scan(&h.issue, &h.agent, &h.open); err != nil {
			rows.Close()
			return err
		}
		if !slices.ContainsFunc(list, func(o held) bool { return o.issue == h.issue }) {
			list = append(list, h)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(list) == 0 {
		return err
	}
	now := s.nowMS()
	successor := int64(0)
	if ev.EndReason == "clear" {
		successor = s.clearSuccessor(ctx, t, x, at)
	}
	for _, h := range list {
		i, err := s.issueByID(ctx, t, h.issue, false)
		if err != nil {
			continue
		}
		if i.Closed() || i.Status.Category != CatInProgress || s.outOnPR(ctx, t, i) != nil {
			continue
		}
		if handed, err := s.handedOn(ctx, t, i); handed || err != nil {
			continue
		}
		if i.ClaimedBy != "" && i.ClaimedBy != h.agent && i.ClaimedUntil != nil && i.ClaimedUntil.UnixMilli() > now {
			continue
		}
		w := pickWork(i.Key, len(list), ws, func(k string) bool { return s.taskExists(ctx, t, k) })
		if w == nil {
			continue
		}
		w.At, w.Why, w.Agent, w.Machine = msTime(at), cmp(ev.EndReason, "other"), h.agent, x.Machine
		if w.Top == "" {
			w.Top, w.Branch = x.Cwd, x.Branch
		}
		if err := s.strand(ctx, t, i, h.agent, h.open, x.ID, w); err != nil {
			return err
		}
		if successor != 0 {
			if err := s.tellStranded(ctx, t, successor, i.Key, i.ID, w, w.At); err != nil {
				return err
			}
		}
	}
	return nil
}

// pickWork is the stranded work for the task key from the session's last
// known state: the checkouts whose branch names it (or, for a session's
// only task, those on a branch of their own that names no existing task, per
// exists), when one has uncommitted or unpushed work or background work was
// still running there; nil otherwise. A checkout on the default branch or a
// detached head is never a task's by default: its dirt may be anyone's.
func pickWork(key string, tasks int, ws workState, exists func(key string) bool) *StrandedWork {
	var mine []HookCheckout
	for _, c := range ws.Checkouts {
		if branchNames(c.Branch, key) {
			mine = append(mine, c)
		}
	}
	if len(mine) == 0 && tasks == 1 {
		for _, c := range ws.Checkouts {
			if !c.shared() && !slices.ContainsFunc(branchRefs(c.Branch), func(r keyRef) bool { return exists(r.Key) }) {
				mine = append(mine, c)
			}
		}
	}
	if len(mine) == 0 {
		return nil
	}
	var pick *HookCheckout
	for n := range mine {
		if mine[n].hasWork() {
			pick = &mine[n]
			break
		}
	}
	if pick == nil && len(ws.Background) == 0 {
		return nil
	}
	w := &StrandedWork{Background: ws.Background}
	if pick == nil {
		pick = &mine[0]
	}
	w.Top, w.Branch, w.Files, w.FileCount, w.Unpushed = pick.Top, pick.Branch, pick.Files, pick.FileCount, pick.Unpushed
	return w
}

// taskExists reports whether a task with this key exists.
func (s *Service) taskExists(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, key string) bool {
	n := strings.LastIndexByte(key, '-')
	if n <= 0 {
		return true
	}
	var ok bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM issues i JOIN projects p ON p.id = i.project_id WHERE p.key = ? AND i.number = ?)`, key[:n], key[n+1:]).Scan(&ok); err != nil {
		return true
	}
	return ok
}

// branchNames: the branch carries key (wp-437-packet-triage names WP-437).
func branchNames(branch, key string) bool {
	for _, r := range branchRefs(branch) {
		if strings.EqualFold(r.Key, key) {
			return true
		}
	}
	return false
}

// strand records w on the task, releases the agent's hold on it and, outside
// a workflow step, moves it back to To Do with a comment saying where the
// work is. Waypoint itself does it: the comment is no agent's text.
func (s *Service) strand(ctx context.Context, t *tx, i *Issue, agent string, open bool, session int64, w *StrandedWork) error {
	files, _ := json.Marshal(nonNil(w.Files))
	bg, _ := json.Marshal(nonNil(w.Background))
	if _, err := t.ExecContext(ctx, `INSERT INTO stranded_work(issue_id, session_id, agent, machine, top, branch, files, file_count, unpushed, background, why, at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, i.ID, nullUser(session), agent, w.Machine, w.Top, w.Branch, string(files), w.FileCount, w.Unpushed, string(bg), w.Why, w.At.UnixMilli()); err != nil {
		return err
	}
	if _, err := t.ExecContext(ctx, `UPDATE issues SET claimed_by = '', claimed_until = NULL, claimed_at = NULL WHERE id = ? AND claimed_by = ?`, i.ID, agent); err != nil {
		return err
	}
	if open {
		if err := s.endRun(ctx, t, i.ID, agent, "released", nil); err != nil {
			return err
		}
	}
	sys := SystemActor()
	moved := false
	if i.WorkflowID == nil || i.Step == "" {
		st := CatTodo
		if err := s.applyPatch(ctx, t, sys, i, IssuePatch{Status: &st}); err != nil && !IsInvalid(err) {
			return err
		} else if err == nil {
			moved = true
		}
	}
	body := w.strandComment(i.Key, moved)
	if i.Hold.on(s.now()) {
		body = strings.Replace(body, fmt.Sprintf("`wp next %s` hands it out again and shows where to resume.", i.Key),
			fmt.Sprintf("It is on hold %s; it is handed out again once that ends.", holdWaits(i.Hold, time.UTC)), 1)
	}
	_, err := s.addLocalComment(ctx, t, sys, i, body)
	return err
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// clearSuccessor is the session a /clear of x started, when its start was
// heard already (0 otherwise: it will find the work itself).
func (s *Service) clearSuccessor(ctx context.Context, t *tx, x *AgentSession, at int64) int64 {
	if x.Cwd == "" {
		return 0
	}
	var to int64
	_ = t.QueryRowContext(ctx, `SELECT s.id FROM agent_sessions s WHERE s.user_id = ? AND s.machine = ? AND s.cwd = ? AND s.id != ? AND s.ended_at IS NULL
		AND s.started_at BETWEEN ? AND ? AND EXISTS (SELECT 1 FROM session_events e WHERE e.session_id = s.id AND e.kind = 'started' AND e.detail = 'clear')
		ORDER BY s.started_at DESC LIMIT 1`, x.userID, x.Machine, x.Cwd, x.ID, at-clearWindow.Milliseconds(), at+clearWindow.Milliseconds()).Scan(&to)
	return to
}

// tellStranded queues the one line a session hears of a task's unfinished
// work in its checkout, once per session and task.
func (s *Service) tellStranded(ctx context.Context, t *tx, session int64, key string, issue int64, w *StrandedWork, since time.Time) error {
	_, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO session_messages(session_id, body, step_key, created_by, created_at, passive) VALUES (?,?,?,NULL,?,1)`,
		session, w.hereText(key, since), "stranded:"+itoa64(session)+":"+itoa64(issue), s.nowMS())
	return err
}

// noteStrandedHere: a session starts in a checkout with uncommitted or
// unpushed work for a task that is unfinished and that nobody holds; it
// hears so in one line. The task is the one stranded there, or the one its
// branch names that an agent worked on.
func (s *Service) noteStrandedHere(ctx context.Context, t *tx, actor *Actor, x *AgentSession, ev *HookEvent) error {
	now := s.nowMS()
	told := 0
	seen := map[int64]bool{}
	for _, c := range cleanCheckouts(ev.Checkouts) {
		if !c.hasWork() {
			continue
		}
		type cand struct {
			issue int64
			w     *StrandedWork
			since time.Time
		}
		var cands []cand
		var id, when int64
		var why, branch string
		err := t.QueryRowContext(ctx, `SELECT issue_id, at, why, branch FROM stranded_work WHERE machine = ? AND top = ? AND at > ?
			ORDER BY at DESC LIMIT 1`, x.Machine, c.Top, now-strandedShowDays*24*time.Hour.Milliseconds()).Scan(&id, &when, &why, &branch)
		if err == nil && (branch == "" || branch == c.Branch) {
			cands = append(cands, cand{id, &StrandedWork{Why: why}, msTime(when)})
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		for _, r := range branchRefs(c.Branch) {
			iid, err := s.resolveIssueID(ctx, t, r.Key)
			if err != nil {
				continue
			}
			var ended sql.NullInt64
			var open bool
			if err := t.QueryRowContext(ctx, `SELECT max(ended_at), coalesce(sum(ended_at IS NULL), 0) > 0 FROM agent_runs WHERE issue_id = ?`, iid).Scan(&ended, &open); err != nil || !ended.Valid || open {
				continue
			}
			cands = append(cands, cand{iid, &StrandedWork{Why: "run"}, msTime(ended.Int64)})
		}
		for _, k := range cands {
			if seen[k.issue] || told == 3 {
				continue
			}
			seen[k.issue] = true
			i, err := s.issueByID(ctx, t, k.issue, false)
			if err != nil || !s.canSee(ctx, t, actor, i.ID, i.ProjectID) {
				continue
			}
			if i.Closed() || s.outOnPR(ctx, t, i) != nil {
				continue
			}
			if handed, err := s.handedOn(ctx, t, i); handed || err != nil {
				continue
			}
			if i.ClaimedBy != "" && i.ClaimedUntil != nil && i.ClaimedUntil.UnixMilli() > now {
				continue
			}
			var live bool
			if err := t.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_runs r JOIN agent_sessions x ON x.id = r.session_id
				WHERE r.issue_id = ? AND r.ended_at IS NULL AND x.state != 'ended' AND x.last_seen_at > ?)`, i.ID, now-sessionIdleAfter.Milliseconds()).Scan(&live); err != nil || live {
				continue
			}
			w := k.w
			w.Top, w.Branch, w.Machine, w.FileCount, w.Unpushed = c.Top, c.Branch, "", c.FileCount, c.Unpushed
			if i.Hold.on(s.now()) {
				w.onHold = holdWaits(i.Hold, s.actorLocation(actor))
			}
			if err := s.tellStranded(ctx, t, x.ID, i.Key, i.ID, w, k.since); err != nil {
				return err
			}
			told++
		}
	}
	return nil
}

// strandedFor is the latest work left stranded on the issue since it was
// last finished, within strandedShowDays; nil when there is none.
func (s *Service) strandedFor(ctx context.Context, q queryer, issueID int64) *StrandedWork {
	now := s.nowMS()
	var w StrandedWork
	var at int64
	var files, bg string
	err := q.QueryRowContext(ctx, `SELECT agent, machine, top, branch, files, file_count, unpushed, background, why, at, pushed, push_error FROM stranded_work w
		WHERE w.issue_id = ? AND w.at > ? AND w.at > coalesce((SELECT max(ended_at) FROM agent_runs WHERE issue_id = w.issue_id AND outcome = 'done'), 0)
		ORDER BY w.at DESC LIMIT 1`, issueID, now-strandedShowDays*24*time.Hour.Milliseconds()).
		Scan(&w.Agent, &w.Machine, &w.Top, &w.Branch, &files, &w.FileCount, &w.Unpushed, &bg, &w.Why, &at, &w.Pushed, &w.PushError)
	if err != nil {
		return nil
	}
	_ = json.Unmarshal([]byte(files), &w.Files)
	_ = json.Unmarshal([]byte(bg), &w.Background)
	w.At = msTime(at)
	return &w
}

// strandedHere is the tasks the agent left stranded from this terminal
// (the session's machine and directory) within strandedShowDays, still
// unclaimed: next hands them out first, so the session a /clear started
// carries on with the work in its checkout as it did before (WP-253).
func (s *Service) strandedHere(ctx context.Context, actor *Actor, agent string) map[int64]bool {
	if actor == nil || actor.Session == 0 {
		return nil
	}
	ids, err := queryIDs(ctx, s.db.R, `SELECT DISTINCT w.issue_id FROM stranded_work w
		JOIN agent_sessions o ON o.id = w.session_id
		JOIN agent_sessions c ON c.id = ?
		JOIN issues i ON i.id = w.issue_id
		WHERE w.agent = ? AND w.at > ? AND o.user_id = c.user_id AND o.machine = c.machine AND o.cwd = c.cwd AND c.cwd != ''
		  AND i.claimed_by = ''`, actor.Session, agent, s.nowMS()-strandedShowDays*24*time.Hour.Milliseconds())
	if err != nil || len(ids) == 0 {
		return nil
	}
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// sweepStranded puts back to To Do the In Progress tasks an agent worked on
// that nobody has held for strandedIdle: no claim, no open run, no run
// ended since, no pull request, and the last run's session not live. The
// comment says where its work was last seen.
func (s *Service) sweepStranded(ctx context.Context) error {
	now := s.nowMS()
	cut := now - strandedIdle.Milliseconds()
	sys := SystemActor()
	for after := int64(0); ; {
		ids, err := queryIDs(ctx, s.db.R, `SELECT i.id FROM issues i JOIN statuses st ON st.id = i.status_id
			WHERE st.category = ? AND i.draft = 0 AND i.agent != ? AND i.updated_at < ?
			  AND (i.claimed_by = '' OR i.claimed_until IS NULL OR i.claimed_until <= ?)
			  AND EXISTS (SELECT 1 FROM agent_runs r WHERE r.issue_id = i.id)
			  AND NOT EXISTS (SELECT 1 FROM agent_runs r WHERE r.issue_id = i.id AND (r.ended_at IS NULL OR r.ended_at > ?))
			  AND NOT EXISTS (SELECT 1 FROM issue_assignees a WHERE a.issue_id = i.id)
			  AND i.id > ?
			ORDER BY i.id LIMIT 100`, CatInProgress, AgentHumans, cut, now, cut, after)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		after = ids[len(ids)-1]
		for _, id := range ids {
			err := s.write(ctx, sys, func(t *tx) error {
				i, err := s.issueByID(ctx, t, id, false)
				if err != nil {
					return err
				}
				if i.Closed() || i.Status.Category != CatInProgress || (i.WorkflowID != nil && i.Step != "") || i.Hold.on(s.now()) ||
					s.outOnPR(ctx, t, i) != nil {
					return nil
				}
				if handed, err := s.handedOn(ctx, t, i); handed || err != nil {
					return nil
				}
				if i.Draft || i.Agent == AgentHumans || i.UpdatedAt.UnixMilli() >= cut {
					return nil
				}
				if i.ClaimedBy != "" && i.ClaimedUntil != nil && i.ClaimedUntil.UnixMilli() > now {
					return nil
				}
				var recent bool
				if err := t.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_runs WHERE issue_id = ? AND (ended_at IS NULL OR ended_at > ?))
						OR EXISTS(SELECT 1 FROM issue_assignees WHERE issue_id = ?)`, i.ID, cut, i.ID).Scan(&recent); err != nil {
					return err
				}
				if recent {
					return nil
				}
				var agent string
				var session sql.NullInt64
				var ended int64
				if err := t.QueryRowContext(ctx, `SELECT agent, session_id, coalesce(ended_at, 0) FROM agent_runs WHERE issue_id = ? ORDER BY id DESC LIMIT 1`, i.ID).
					Scan(&agent, &session, &ended); err != nil {
					return err
				}
				w := &StrandedWork{At: msTime(cmp64(ended, now)), Why: "idle", Agent: agent}
				if session.Valid {
					var state, cwd, branch, machine string
					var seen int64
					if err := t.QueryRowContext(ctx, `SELECT state, last_seen_at, cwd, branch, machine FROM agent_sessions WHERE id = ?`, session.Int64).
						Scan(&state, &seen, &cwd, &branch, &machine); err != nil && !errors.Is(err, sql.ErrNoRows) {
						return err
					} else if err == nil {
						if sessionStillAbout(state, seen, now) {
							return nil
						}
						w.Machine, w.Top, w.Branch = machine, cwd, branch
						ws := s.loadWorkState(ctx, t, session.Int64)
						ws.Background = nil
						if p := pickWork(i.Key, 1, ws, func(k string) bool { return s.taskExists(ctx, t, k) }); p != nil {
							w.Top, w.Branch, w.Files, w.FileCount, w.Unpushed = p.Top, p.Branch, p.Files, p.FileCount, p.Unpushed
						}
					}
				}
				return s.strand(ctx, t, i, agent, false, ptrInt64(session), w)
			})
			if err != nil {
				s.log.WarnContext(ctx, "stranded sweep", "issue", id, "err", err)
			}
		}
	}
}

// cmp64 is a unless it is zero, else b.
func cmp64(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}

// ptrInt64 is the value of n, or 0 when it is null.
func ptrInt64(n sql.NullInt64) int64 {
	if n.Valid {
		return n.Int64
	}
	return 0
}
