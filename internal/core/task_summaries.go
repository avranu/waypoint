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

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Task summaries (WP-194, WP-ADR9): the agent's last message after it
// finished a task, kept for its person to read when they come back, from
// every session (autopilot or not). "Waiting on you" lines in it that no
// request of the session covers are filed as one auto request, so an agent
// that forgot wp ask doesn't leave its person's decision in scrollback.

const (
	summaryMax     = 20000 // characters of a summary kept
	summaryAsksMax = 50    // asks read from a summary, before the covered ones are dropped
)

// TaskSummary is one finished task's summary.
type TaskSummary struct {
	ID         int64      `json:"id"`
	Project    string     `json:"project,omitempty"`
	Issue      *IssueRef  `json:"issue,omitempty"`
	Session    int64      `json:"session"`
	SessionURL string     `json:"session_url"`
	Machine    string     `json:"machine,omitempty"`
	Agent      string     `json:"agent,omitempty"`
	Body       string     `json:"body"`
	Asks       []string   `json:"asks"`
	Request    string     `json:"request,omitempty"` // the request filed for Asks (WP:R-12)
	RequestURL string     `json:"request_url,omitempty"`
	Autopilot  bool       `json:"autopilot"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	ReviewedAt *time.Time `json:"reviewed_at,omitempty"`
}

// recordTaskSummary keeps a Stop's last message as the summary of the task
// the session finished last, when it finished one and took nothing since.
// Later Stops of the agent's own replace it; once a person has prompted the
// session after the task, the summary stays as it was.
func (s *Service) recordTaskSummary(ctx context.Context, t *tx, actor *Actor, x *AgentSession, msg string, at int64) error {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return nil
	}
	key, run, err := lastDoneInSession(ctx, t, x.ID)
	if err != nil || key == "" {
		return err
	}
	var later int
	if err := t.QueryRowContext(ctx, `SELECT count(*) FROM agent_runs WHERE session_id = ? AND (ended_at IS NULL OR id > ?)`, x.ID, run).Scan(&later); err != nil || later > 0 {
		return err // it took other work since: this message is about that
	}
	// Once the person has prompted the session after the task, what the
	// agent says is an answer to them, not the task's summary.
	if prompted, err := s.promptedAfterDone(ctx, t, x.ID, run); err != nil || prompted {
		return err
	}
	// The Stops of an adversarial review are its report, kept in the work
	// log (WP-285), not the task's summary.
	var doneIssue int64
	if err := t.QueryRowContext(ctx, `SELECT issue_id FROM agent_runs WHERE id = ?`, run).Scan(&doneIssue); err != nil {
		return err
	}
	if reviewStarted(ctx, t, x.ID, doneIssue) {
		return nil
	}
	var id int64
	var reqID, reviewed sql.NullInt64
	err = t.QueryRowContext(ctx, `SELECT id, request_id, reviewed_at FROM task_summaries WHERE run_id = ?`, run).Scan(&id, &reqID, &reviewed)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	case reviewed.Valid:
		return nil // read already: it stays what the person read
	}
	var issueID sql.NullInt64
	var agent string
	if err := t.QueryRowContext(ctx, `SELECT issue_id, agent FROM agent_runs WHERE id = ?`, run).Scan(&issueID, &agent); err != nil {
		return err
	}
	asks, err := s.unfiledAsks(ctx, t, x, msg)
	if err != nil {
		return err
	}
	rawAsks, _ := json.Marshal(asks)
	body := msg
	if r := []rune(body); len(r) > summaryMax {
		body = string(r[:summaryMax]) + "…"
	}
	var project any
	if x.projectID.Valid {
		project = x.projectID.Int64
	}
	if id == 0 {
		res, err := t.ExecContext(ctx, `INSERT INTO task_summaries(user_id, project_id, session_id, run_id, issue_id, agent, body, asks, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`, x.userID, project, x.ID, run, issueID, agent, body, string(rawAsks), at, at)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
	} else if _, err := t.ExecContext(ctx, `UPDATE task_summaries SET body = ?, asks = ?, updated_at = ? WHERE id = ?`,
		body, string(rawAsks), at, id); err != nil {
		return err
	}
	if len(asks) == 0 || reqID.Valid {
		return nil
	}
	// The trailing question this same Stop filed (WP-96) quotes the message
	// already: point at it rather than file a second guess.
	var same int64
	if err := t.QueryRowContext(ctx, `SELECT coalesce(max(id), 0) FROM requests WHERE session_id = ? AND source = ? AND created_at = ?`, x.ID, requestAuto, at).Scan(&same); err != nil {
		return err
	}
	if same != 0 {
		_, err := t.ExecContext(ctx, `UPDATE task_summaries SET request_id = ? WHERE id = ?`, same, id)
		return err
	}
	return s.fileSummaryAsks(ctx, t, actor, x, id, key, asks, at)
}

// fileSummaryAsks files the summary's unfiled asks as one auto request: a
// guess, so it holds nobody (like WP-96's trailing questions).
func (s *Service) fileSummaryAsks(ctx context.Context, t *tx, actor *Actor, x *AgentSession, summaryID int64, key string, asks []string, at int64) error {
	var b strings.Builder
	fmt.Fprintf(&b, "The agent's summary of %s says these wait on you, and it filed no request for them:\n\n", key)
	for _, a := range asks {
		fmt.Fprintf(&b, "- %s\n", a)
	}
	fmt.Fprintf(&b, "\n_Found in the summary by Waypoint (WP-194), not asked by the agent. Answer it here, or dismiss it if nothing waits on you._")
	qs := make([]Question, 0, len(asks))
	for _, a := range asks {
		qs = append(qs, Question{Question: excerpt(a, 300)})
	}
	title := excerpt(key+": "+asks[0], 200)
	if len(asks) > 1 {
		title = excerpt(fmt.Sprintf("%s: %s (and %d more)", key, asks[0], len(asks)-1), 200)
	}
	reqID, _, err := s.createRequestTx(ctx, t, actor, x, RequestInput{Kind: RequestQuestion, Title: title, Source: requestAuto, Body: b.String(), Questions: qs}, at)
	if errors.Is(err, errRequestsFull) {
		return nil // too many open already: the Stop must still succeed
	}
	if err != nil {
		return err
	}
	_, err = t.ExecContext(ctx, `UPDATE task_summaries SET request_id = ? WHERE id = ?`, reqID, summaryID)
	return err
}

var (
	requestCodeRE = lazyre.New(`\b(?:[A-Z][A-Z0-9]{0,9}:)?R-\d+\b`)
	// A line that heads what waits on a person: "Waiting on you:", "**Decisions
	// for you**", "## Needs your call", "Waiting on you (2) — …". What
	// follows the colon or dash is the first ask.
	askHeadRE = lazyre.New(`(?i)^(?:#{1,6}\s*)?\**\s*(?:still\s+)?(?:waiting (?:on|for) you|what(?:'s| is) waiting on you|open questions?|questions? for you|decisions? (?:for you|needed)|needs? your (?:decision|call|input|approval)|for you to (?:decide|do|check))(?:\s+(?:now|today|still))?\s*(?:\([^)]{0,20}\))?\s*\**\s*(?:$|[:—–]|\s-\s)\s*\**\s*(.*)$`)
	// What says nothing waits: "none", "nothing", "N/A", "no action needed".
	nothingRE   = lazyre.New(`(?i)^\(?(?:nothing|none|nobody|no ?one|n/?a|no action|not needed|no (?:decisions?|questions?|asks?|open|pending|waiting))\b`)
	tableSepRE  = lazyre.New(`^\|?[\s:|-]+\|?$`)
	tableCellRE = lazyre.New(`\s*\|\s*`)
)

// summaryAsks finds what a summary says waits on its person: the items
// under a "Waiting on you"-style heading (bullets, table rows or the
// paragraph that follows it, and what follows its colon), and sentences that
// ask a person to decide (the WP-96 rules) anywhere in it.
func summaryAsks(msg string) []string {
	text := fencedBlockRE.ReplaceAllString(msg, "\n")
	text = inlineCodeRE.ReplaceAllStringFunc(text, func(c string) string {
		return strings.NewReplacer("?", string(maskQ), ".", string(maskStop), "!", string(maskStop)).Replace(c)
	})
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "*_"))
		s = questionTitle(s)
		if s == "" || nothingRE.MatchString(s) || len(out) >= summaryAsksMax {
			return
		}
		for _, o := range out {
			if o == s || similarTitles(o, s) {
				return
			}
		}
		out = append(out, s)
	}
	lines := strings.Split(text, "\n")
	inSection, blank := false, false
	for i, line := range lines {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, ">") {
			continue
		}
		if l == "" {
			blank = true
			continue
		}
		bullet := bulletRE.MatchString(l)
		item := strings.TrimSpace(bulletRE.ReplaceAllString(l, ""))
		if m := askHeadRE.FindStringSubmatch(strings.TrimSpace(strings.ReplaceAll(item, "**", ""))); m != nil && !notWaitingRE.MatchString(item) {
			inSection, blank = true, false
			if rest := strings.TrimSpace(m[1]); rest != "" {
				add(rest)
			}
			continue
		}
		table := strings.HasPrefix(l, "|")
		// The section runs through its list or table, and the one paragraph
		// right under the heading; a later paragraph or heading ends it.
		if inSection && (bullet || table || !blank || len(out) == 0) && !headingRE.MatchString(l) && !boldLineRE.MatchString(l) {
			blank = false
			switch {
			case table:
				if tableSepRE.MatchString(l) || (i+1 < len(lines) && tableSepRE.MatchString(strings.TrimSpace(lines[i+1]))) {
					continue // the separator, or the header row above it
				}
				add(strings.Join(slices.DeleteFunc(tableCellRE.Split(strings.Trim(l, "| "), -1), func(c string) bool { return c == "" }), ": "))
			default:
				add(item)
			}
			continue
		}
		inSection, blank = false, false
		for _, sentence := range splitSentences(item) {
			plain := strings.TrimLeft(strings.TrimSpace(strings.ReplaceAll(sentence, "**", "")), "*_ ")
			if courtesyRE.MatchString(plain) || notWaitingRE.MatchString(plain) {
				continue
			}
			if decisionRE.MatchString(plain) || needsColonRE.MatchString(plain) || (waitingRE.MatchString(plain) && !strings.HasPrefix(strings.ToLower(plain), "nothing")) {
				add(sentence)
			}
		}
	}
	return out
}

// unfiledAsks is summaryAsks less what the session's requests already
// cover: an ask naming a request (R-12), or about the same thing as one
// the session filed (or had filed for it).
func (s *Service) unfiledAsks(ctx context.Context, q queryer, x *AgentSession, msg string) ([]string, error) {
	asks := summaryAsks(msg)
	if len(asks) == 0 {
		return []string{}, nil
	}
	// What the agent asked itself, title and text; a guessed (auto) request
	// by its title only: its text quotes the whole message.
	rows, err := q.QueryContext(ctx, `SELECT title, CASE WHEN source = ? THEN '' ELSE body END FROM requests WHERE session_id = ? ORDER BY id DESC LIMIT 200`, requestAuto, x.ID)
	if err != nil {
		return nil, err
	}
	var filed []string
	for rows.Next() {
		var title, body string
		if err := rows.Scan(&title, &body); err != nil {
			rows.Close()
			return nil, err
		}
		filed = append(filed, title)
		if body != "" {
			filed = append(filed, excerpt(body, 600))
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []string{}
	for _, a := range asks {
		if requestCodeRE.MatchString(a) || askCovered(a, filed) {
			continue
		}
		if len(out) == maxRequestQuestions {
			break // one request holds this many
		}
		out = append(out, a)
	}
	return out, nil
}

// askWords are the words an ask is about (not the asking itself).
func askWords(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range wordRE.FindAllString(strings.ToLower(s), -1) {
		if len(w) > 2 && !askStopwords[w] {
			m[w] = true
		}
	}
	return m
}

var askStopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`the and for you your should can could would will want need needs approve approval decide decision choose pick confirm
		whether which what when where who how this that these those with from into onto about after before then than there here its it's are was were been
		being have has had not but or nor yet also just only still waiting wait call input let know tell say said ask asked please may might must shall`) {
		askStopwords[w] = true
	}
}

// askCovered: most of what the ask is about (half its words, at least two)
// is in one of the filed requests' titles or bodies.
func askCovered(ask string, filed []string) bool {
	words := askWords(ask)
	if len(words) == 0 {
		return true // nothing to tell it by: don't file noise
	}
	for _, f := range filed {
		fw := askWords(f)
		hit := 0
		for w := range words {
			if fw[w] {
				hit++
			}
		}
		if hit >= 2 && float64(hit)/float64(len(words)) >= 0.5 || hit == len(words) {
			return true
		}
	}
	return false
}

// ---- reading them ----

// TaskSummaryQuery filters the summaries list.
type TaskSummaryQuery struct {
	Project string
	All     bool  // reviewed ones too
	Before  int64 // summary id: the page after
	Limit   int
}

// TaskSummaryPage is a page of summaries, newest first.
type TaskSummaryPage struct {
	Summaries  []*TaskSummary `json:"summaries"`
	Unreviewed int            `json:"unreviewed"`
	More       bool           `json:"more"`
}

// TaskSummaries lists the person's task summaries: unreviewed ones, or all.
func (s *Service) TaskSummaries(ctx context.Context, a *Actor, q TaskSummaryQuery) (*TaskSummaryPage, error) {
	if err := a.need(PermAgentsRead); err != nil {
		return nil, err
	}
	if q.Limit <= 0 || q.Limit > 100 {
		q.Limit = 30
	}
	var pid int64
	if q.Project != "" {
		p, err := s.GetProject(ctx, a, q.Project)
		if err != nil {
			return nil, err
		}
		pid = p.ID
	}
	where, args := `ts.user_id = ? AND (? = 0 OR ts.project_id = ?)`, []any{a.id(), pid, pid}
	if !q.All {
		where += ` AND ts.reviewed_at IS NULL`
	}
	if q.Before > 0 {
		where, args = where+` AND ts.id < ?`, append(args, q.Before)
	}
	limit, limitArgs := taskSummariesFor(a)
	where, args = where+limit, append(args, limitArgs...)
	// #nosec G202 -- where is built from constant clauses; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT ts.id, coalesce(p.key, ''), coalesce(i.id, 0), coalesce(ip.key || '-' || i.number, ''), coalesce(i.title, ''), coalesce(i.type, ''),
		ts.session_id, coalesce(x.machine, ''), ts.agent, ts.body, ts.asks, CASE WHEN rq.id IS NULL THEN '' ELSE coalesce(rp.key, 'me') || ':R-' || rq.number END, ts.autopilot,
		ts.created_at, ts.updated_at, ts.reviewed_at
		FROM task_summaries ts LEFT JOIN projects p ON p.id = ts.project_id LEFT JOIN issues i ON i.id = ts.issue_id LEFT JOIN projects ip ON ip.id = i.project_id
		LEFT JOIN agent_sessions x ON x.id = ts.session_id LEFT JOIN requests rq ON rq.id = ts.request_id LEFT JOIN projects rp ON rp.id = rq.project_id
		WHERE `+where+` ORDER BY ts.id DESC LIMIT ?`, append(args, q.Limit+1)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := &TaskSummaryPage{Summaries: []*TaskSummary{}}
	for rows.Next() {
		var ts TaskSummary
		var issue IssueRef
		var asks string
		var auto int
		var created, updated int64
		var reviewed sql.NullInt64
		if err := rows.Scan(&ts.ID, &ts.Project, &issue.ID, &issue.Key, &issue.Title, &issue.Type, &ts.Session, &ts.Machine, &ts.Agent, &ts.Body, &asks,
			&ts.Request, &auto, &created, &updated, &reviewed); err != nil {
			return nil, err
		}
		if len(page.Summaries) == q.Limit {
			page.More = true
			break
		}
		if issue.ID != 0 {
			ts.Issue = &issue
		}
		ts.Asks = []string{}
		_ = json.Unmarshal([]byte(asks), &ts.Asks)
		ts.Autopilot, ts.CreatedAt, ts.UpdatedAt, ts.ReviewedAt = auto != 0, msTime(created), msTime(updated), ptrTime(reviewed)
		ts.SessionURL = fmt.Sprintf("/agents/sessions/%d", ts.Session)
		if ts.Request != "" {
			ts.RequestURL = "/requests/" + ts.Request
		}
		page.Summaries = append(page.Summaries, &ts)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// #nosec G202 -- limit is built from constant clauses; values are bound
	err = s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM task_summaries ts WHERE ts.user_id = ? AND ts.reviewed_at IS NULL AND (? = 0 OR ts.project_id = ?)`+limit,
		append([]any{a.id(), pid, pid}, limitArgs...)...).Scan(&page.Unreviewed)
	return page, err
}

// taskSummariesFor narrows a query of the person's summaries (alias ts) to
// what the actor may read: no draft's, for whoever drafts are hidden from,
// and for a runner job's token only its projects'.
func taskSummariesFor(a *Actor) (string, []any) {
	var where string
	var args []any
	if a.HidesDrafts() {
		where = ` AND NOT EXISTS (SELECT 1 FROM issues d WHERE d.id = ts.issue_id AND d.draft = 1)`
	}
	if a.scope != nil {
		if len(a.scope) == 0 {
			return where + ` AND 0`, args
		}
		where += ` AND ts.project_id IN (` + placeholders(len(a.scope)) + `)`
		for id := range a.scope {
			args = append(args, id)
		}
	}
	return where, args
}

// ReviewTaskSummaries marks summaries read (ids, or every unreviewed one of
// the project, or of the person with project ""), or unread again.
func (s *Service) ReviewTaskSummaries(ctx context.Context, a *Actor, ids []int64, all bool, project string, unread bool) (int, error) {
	if err := a.need(PermAgentsManage); err != nil {
		return 0, err
	}
	if err := a.requirePersonDirect("reviewing task summaries"); err != nil {
		return 0, err
	}
	if err := a.canWrite(); err != nil {
		return 0, err
	}
	if !all && len(ids) == 0 {
		return 0, invalid("which summaries? Give ids, or all")
	}
	if len(ids) > 500 {
		return 0, invalid("at most 500 at once")
	}
	var pid int64
	if project != "" {
		p, err := s.GetProject(ctx, a, project)
		if err != nil {
			return 0, err
		}
		pid = p.ID
	}
	n := 0
	err := s.write(ctx, a, func(t *tx) error {
		set, args := `reviewed_at = ?, reviewed_by = ?`, []any{s.nowMS(), a.id()}
		if unread {
			set, args = `reviewed_at = NULL, reviewed_by = NULL`, nil
		}
		where, wargs := `user_id = ? AND (? = 0 OR project_id = ?) AND reviewed_at IS NULL`, []any{a.id(), pid, pid}
		if unread {
			where = `user_id = ? AND (? = 0 OR project_id = ?) AND reviewed_at IS NOT NULL`
		}
		if !all {
			where += ` AND id IN (` + placeholders(len(ids)) + `)`
			wargs = append(wargs, int64Args(ids)...)
		}
		// #nosec G202 -- constant clauses and ? placeholders only
		res, err := t.ExecContext(ctx, `UPDATE task_summaries SET `+set+` WHERE `+where, append(args, wargs...)...)
		if err != nil {
			return err
		}
		k, _ := res.RowsAffected()
		n = int(k)
		return nil
	})
	return n, err
}
