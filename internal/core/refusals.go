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
)

const (
	// refusalRepeat: the same call refused again in a session this soon
	// counts on the same row.
	refusalRepeat = 10 * time.Minute
	// refusalLead: a request shows the session's refusals this long before
	// it was filed.
	refusalLead = time.Hour
	// maxRefusalInput: a tool input larger than this isn't kept; its
	// command or path still is.
	maxRefusalInput = 20_000
)

// ToolRefusal is one refused tool call, with how often it was refused again.
type ToolRefusal struct {
	ID      int64           `json:"id"`
	At      time.Time       `json:"at"`
	LastAt  time.Time       `json:"last_at"`
	Repeats int             `json:"repeats"`
	Source  string          `json:"source"` // classifier
	Tool    string          `json:"tool"`
	Command string          `json:"command,omitempty"` // the Bash command, the file path, the URL…; hidden from readers who mustn't see command text
	Reason  string          `json:"reason,omitempty"`
	Agent   string          `json:"agent,omitempty"`
	Project *SessionProject `json:"project,omitempty"`
	Session *RequestSession `json:"session,omitempty"`
	Issue   *IssueRef       `json:"issue,omitempty"`
	// RanAt: the same call later ran in its session (a person allowed it
	// in the terminal).
	RanAt *time.Time `json:"ran_at,omitempty"`
	// Request: the permission request it was filed as, before WP-232.
	Request string `json:"request,omitempty"`
	// Rules: the allow rules a person may add for this call, narrowest
	// first, to RuleRepo's .claude/settings.json (WP-233); none once one was
	// picked, when no rule covers it safely, or with no repo known.
	Rules    []PermissionRule `json:"rules,omitempty"`
	RuleRepo string           `json:"rule_repo,omitempty"`
	// Rule: the allow rule a person picked from it, in effect or on its way
	// (RulePR adds it, or the file already had it); RuleReason says why
	// one picked wasn't added.
	Rule       string `json:"rule,omitempty"`
	RulePR     string `json:"rule_pr,omitempty"`
	RuleReason string `json:"rule_reason,omitempty"`

	userID    int64
	projectID sql.NullInt64
	sessionID sql.NullInt64
	issueID   sql.NullInt64
	requestID sql.NullInt64
	input     string
}

// RefusalQuery narrows the refusal log.
type RefusalQuery struct {
	Project string // a project key
	Agent   string // an agent spec: opus, claude-code, haiku 5.5…
	Days    int    // the period, ending now (default 30, at most 365)
	Session int64
	Before  int64 // the page after: rows with a smaller id
	Limit   int
}

// RefusalPage is a page of the log, newest first, with counts for the
// whole period on the first page.
type RefusalPage struct {
	Refusals []*ToolRefusal `json:"refusals"`
	// Counts: the period's refused calls (repeats included), distinct rows,
	// sessions, and calls per tool. Only on the first page.
	Counts *RefusalCounts `json:"counts,omitempty"`
	// Rules: the allow rules that would have let most of the period's
	// refusals through, on the first page, for people who see commands.
	Rules []*RuleSuggestion `json:"rules,omitempty"`
	More  bool              `json:"more"`
}

// RuleSuggestion is an allow rule (WP-233) with the refusals in the period
// it covers: one a person might add because the block is common, repeated
// or got allowed by hand (Ran).
type RuleSuggestion struct {
	Rule   string `json:"rule"`
	Covers string `json:"covers"`
	Tool   string `json:"tool"`
	Repo   string `json:"repo"`
	// Refusal: the newest refused call it covers; AllowRefusedCall takes it.
	Refusal  int64 `json:"refusal"`
	Refusals int64 `json:"refusals"`
	Calls    int64 `json:"calls"`
	Sessions int64 `json:"sessions"`
	Ran      int64 `json:"ran"`
	// Added: a person already picked it for Repo; PR is the pull request.
	Added bool   `json:"added,omitempty"`
	PR    string `json:"pr,omitempty"`

	rank    int
	members []int64
	sess    map[int64]bool
}

const (
	// maxRuleSuggestions: rules offered at the top of the log.
	maxRuleSuggestions = 5
	// ruleScanRows bounds the rows the suggestions are worked out from.
	ruleScanRows = 5000
	// refusalPickStale: after this a pick still without an outcome (its
	// dispatch died in a restart) can be made again.
	refusalPickStale = 10 * time.Minute
)

// RefusalCounts sums the period.
type RefusalCounts struct {
	Refusals int64            `json:"refusals"`
	Calls    int64            `json:"calls"`
	Sessions int64            `json:"sessions"`
	Ran      int64            `json:"ran"`
	Tools    map[string]int64 `json:"tools"`
}

// logRefusal records a call the classifier refused (the PermissionDenied
// hook) in the actor's session x.
func (s *Service) logRefusal(ctx context.Context, t *tx, actor *Actor, x *AgentSession, ev *HookEvent, at int64) error {
	if !toolNameRE.MatchString(ev.Tool) {
		return nil
	}
	key := toolDedupe(ev.Tool, ev.ToolInput)
	var id int64
	err := t.QueryRowContext(ctx, `SELECT id FROM tool_refusals WHERE session_id = ? AND dedupe_key = ? AND last_at > ? ORDER BY id DESC LIMIT 1`,
		x.ID, key, at-refusalRepeat.Milliseconds()).Scan(&id)
	switch {
	case err == nil:
		_, err = t.ExecContext(ctx, `UPDATE tool_refusals SET repeats = repeats + 1, last_at = max(last_at, ?) WHERE id = ?`, at, id)
		return err
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	_, command := toolSummary(ev.Tool, ev.ToolInput)
	command = truncateBytes(command, maxRefusalInput)
	input := string(ev.ToolInput)
	if len(input) > maxRefusalInput {
		input = ""
	}
	var project sql.NullInt64
	if x.projectID.Valid {
		r, rerr := s.projectRole(ctx, t, actor, x.projectID.Int64)
		if rerr != nil {
			if readFault(rerr) {
				s.log.WarnContext(ctx, "project role failed", "projectid", x.projectID.Int64, "err", rerr)
			}
		}
		if r > RoleNone {
			project = x.projectID
		}
	}
	reason := truncateBytes(strings.TrimSpace(ev.Reason), 10_000)
	if _, err := t.ExecContext(ctx, `INSERT INTO tool_refusals(user_id, project_id, session_id, issue_id, agent, source, tool, command, input, reason, tool_use_id, dedupe_key, created_at, last_at)
		VALUES (?,?,?,?,?,'classifier',?,?,?,?,?,?,?,?)`,
		actor.id(), nullInt(ptrInt(project)), x.ID, nullInt(ptrInt(x.issueID)), cmp(actor.agentName(), x.Agent), ev.Tool, command, input, reason, ev.ToolUseID, key, at, at); err != nil {
		return err
	}
	return s.sessionTimeline(ctx, t, x.ID, at, "refused", "", 0, ev.Tool, "", excerpt(command, 300), project, map[string]any{"tool": ev.Tool})
}

// markRefusalRan notes that a refused call has since run in its session.
func markRefusalRan(ctx context.Context, t *tx, sessionID int64, key string, at int64) error {
	_, err := t.ExecContext(ctx, `UPDATE tool_refusals SET ran_at = ? WHERE session_id = ? AND dedupe_key = ? AND ran_at IS NULL AND created_at <= ?`, at, sessionID, key, at)
	return err
}

const refusalCols = `f.id, f.user_id, f.project_id, f.session_id, f.issue_id, f.agent, f.source, f.tool, f.command, f.reason, f.repeats, f.created_at, f.last_at, f.ran_at, f.request_id,
	f.input, f.rule, f.rule_pr, f.rule_reason`

// ListRefusals reads the refusal log the actor may see: refusals in their
// own sessions and in projects they edit (all, for admins), newest first.
func (s *Service) ListRefusals(ctx context.Context, actor *Actor, q RefusalQuery) (*RefusalPage, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	if q.Days <= 0 {
		q.Days = 30
	}
	q.Days = min(q.Days, 365)
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}
	vis, args := requestVisible(actor, "f")
	where := []string{vis, "f.last_at >= ?"}
	args = append(args, s.nowMS()-int64(q.Days)*24*time.Hour.Milliseconds())
	if q.Project != "" {
		p, err := s.projectByRef(ctx, s.db.R, q.Project)
		if err != nil {
			return nil, err
		}
		if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
			return nil, err
		}
		where, args = append(where, "f.project_id = ?"), append(args, p.ID)
	}
	if q.Session != 0 {
		where, args = append(where, "f.session_id = ?"), append(args, q.Session)
	}
	if spec := strings.TrimSpace(q.Agent); spec != "" {
		// #nosec G202 -- where is built from fixed conditions; values are bound
		rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT f.agent FROM tool_refusals f WHERE `+strings.Join(where, " AND "), args...)
		if err != nil {
			return nil, err
		}
		var agents []any
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				rows.Close()
				return nil, err
			}
			if AgentMatches(spec, a) || strings.Contains(strings.ToLower(a), strings.ToLower(spec)) {
				agents = append(agents, a)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(agents) == 0 {
			where = append(where, "0")
		} else {
			where, args = append(where, "f.agent IN ("+placeholders(len(agents))+")"), append(args, agents...)
		}
	}
	cond := strings.Join(where, " AND ")
	page := &RefusalPage{Refusals: []*ToolRefusal{}}
	if q.Before <= 0 {
		c := &RefusalCounts{Tools: map[string]int64{}}
		// #nosec G202 -- cond is built from fixed conditions; values are bound
		rows, err := s.db.R.QueryContext(ctx, `SELECT f.tool, count(*), sum(f.repeats), sum(f.ran_at IS NOT NULL) FROM tool_refusals f WHERE `+cond+` GROUP BY f.tool`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var tool string
			var n, calls, ran int64
			if err := rows.Scan(&tool, &n, &calls, &ran); err != nil {
				rows.Close()
				return nil, err
			}
			c.Refusals += n
			c.Calls += calls
			c.Ran += ran
			c.Tools[tool] = calls
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		// #nosec G202 -- cond is built from fixed conditions; values are bound
		if err := s.db.R.QueryRowContext(ctx, `SELECT count(DISTINCT f.session_id) FROM tool_refusals f WHERE `+cond, args...).Scan(&c.Sessions); err != nil {
			return nil, err
		}
		page.Counts = c
		if !actor.IsAgent() && !actor.HidesCommandText() {
			if page.Rules, err = s.ruleSuggestions(ctx, cond, args); err != nil {
				return nil, err
			}
		}
	}
	if q.Before > 0 {
		cond, args = cond+" AND f.id < ?", append(args, q.Before)
	}
	list, err := s.loadRefusals(ctx, actor, cond+" ORDER BY f.id DESC LIMIT ?", append(args, q.Limit+1)...)
	if err != nil {
		return nil, err
	}
	if len(list) > q.Limit {
		list, page.More = list[:q.Limit], true
	}
	page.Refusals = list
	return page, nil
}

// refusalsBefore is what the classifier refused in a request's session in
// the hour before the agent filed it: the context of what it now asks for.
func (s *Service) refusalsBefore(ctx context.Context, actor *Actor, r *Request) ([]*ToolRefusal, error) {
	if !r.sessionID.Valid || r.Kind == RequestPermission {
		return nil, nil
	}
	created := r.CreatedAt.UnixMilli()
	vis, args := requestVisible(actor, "f")
	args = append(args, r.sessionID.Int64, created-refusalLead.Milliseconds(), created)
	return s.loadRefusals(ctx, actor, vis+` AND f.session_id = ? AND f.last_at >= ? AND f.created_at <= ? ORDER BY f.id DESC LIMIT 5`, args...)
}

func (s *Service) loadRefusals(ctx context.Context, actor *Actor, where string, args ...any) ([]*ToolRefusal, error) {
	// #nosec G202 -- refusalCols is a constant and where is built from fixed clauses with bound args
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+refusalCols+` FROM tool_refusals f WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	var list []*ToolRefusal
	var projects, sessions, issues, requests []int64
	for rows.Next() {
		var f ToolRefusal
		var created, last int64
		var ran sql.NullInt64
		if err := rows.Scan(&f.ID, &f.userID, &f.projectID, &f.sessionID, &f.issueID, &f.Agent, &f.Source, &f.Tool, &f.Command, &f.Reason, &f.Repeats,
			&created, &last, &ran, &f.requestID, &f.input, &f.Rule, &f.RulePR, &f.RuleReason); err != nil {
			rows.Close()
			return nil, err
		}
		f.At, f.LastAt, f.RanAt = msTime(created), msTime(last), ptrTime(ran)
		if actor.HidesCommandText() {
			f.Command, f.Reason, f.Rule, f.RuleReason = "", "", "", ""
		}
		for _, p := range []struct {
			id   sql.NullInt64
			list *[]int64
		}{{f.projectID, &projects}, {f.sessionID, &sessions}, {f.issueID, &issues}, {f.requestID, &requests}} {
			if p.id.Valid && !slices.Contains(*p.list, p.id.Int64) {
				*p.list = append(*p.list, p.id.Int64)
			}
		}
		list = append(list, &f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return list, nil
	}
	pnames, snames, irefs, err := s.requestRefs(ctx, actor, projects, sessions, issues)
	if err != nil {
		return nil, err
	}
	reqs := map[int64]string{}
	if len(requests) > 0 {
		// #nosec G202 -- only ? placeholders are joined in
		rows, err := s.db.R.QueryContext(ctx, `SELECT r.id, coalesce(p.key, ''), r.number FROM requests r LEFT JOIN projects p ON p.id = r.project_id WHERE r.id IN (`+placeholders(len(requests))+`)`, int64Args(requests)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var key string
			var n int
			if err := rows.Scan(&id, &key, &n); err != nil {
				rows.Close()
				return nil, err
			}
			reqs[id] = requestRef(key, n)
		}
		rows.Close()
	}
	for _, f := range list {
		if f.projectID.Valid {
			f.Project = pnames[f.projectID.Int64]
		}
		if f.sessionID.Valid {
			f.Session = snames[f.sessionID.Int64]
		}
		if f.issueID.Valid {
			f.Issue = irefs[f.issueID.Int64]
		}
		if f.requestID.Valid {
			f.Request = reqs[f.requestID.Int64]
		}
		if f.Rule == "" && f.Session != nil && f.Session.Repo != "" && !actor.IsAgent() && !actor.HidesCommandText() {
			if f.Rules = permissionRules(f.Tool, json.RawMessage(f.input)); len(f.Rules) > 0 {
				f.RuleRepo = f.Session.Repo
			}
		}
	}
	return list, nil
}

// ruleSuggestions ranks the allow rules that would have let the period's
// refusals (cond) through: by sessions, then calls, then those someone
// allowed by hand. Only rules covering a call refused twice or more, or one
// that then ran, are offered; of rules covering the same refusals, the
// narrowest.
func (s *Service) ruleSuggestions(ctx context.Context, cond string, args []any) ([]*RuleSuggestion, error) {
	// #nosec G202 -- cond is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT f.id, f.tool, f.input, coalesce(f.session_id, 0), f.repeats, f.ran_at IS NOT NULL, coalesce(a.repo, ''), f.rule, f.rule_pr,
		f.rule = '' AND (f.rule_at IS NULL OR f.rule_reason != '')
		FROM tool_refusals f LEFT JOIN agent_sessions a ON a.id = f.session_id WHERE `+cond+` ORDER BY f.id DESC LIMIT ?`, append(slices.Clone(args), ruleScanRows)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := map[string]*RuleSuggestion{}
	added := map[string]string{}
	for rows.Next() {
		var id, session, repeats int64
		var ran, open bool
		var tool, input, repo, rule, pr string
		if err := rows.Scan(&id, &tool, &input, &session, &repeats, &ran, &repo, &rule, &pr, &open); err != nil {
			return nil, err
		}
		if repo == "" {
			continue
		}
		if rule != "" {
			if added[repo+"\x00"+rule] == "" {
				added[repo+"\x00"+rule] = pr
			}
		}
		for rank, r := range permissionRules(tool, json.RawMessage(input)) {
			k := repo + "\x00" + r.Rule
			g := groups[k]
			if g == nil {
				g = &RuleSuggestion{Rule: r.Rule, Covers: r.Covers, Tool: tool, Repo: repo, rank: rank, sess: map[int64]bool{}}
				groups[k] = g
			}
			if open && g.Refusal == 0 {
				g.Refusal = id
			}
			g.Refusals++
			g.Calls += repeats
			if ran {
				g.Ran++
			}
			if session != 0 {
				g.sess[session] = true
			}
			g.members = append(g.members, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	best := map[string]*RuleSuggestion{}
	for _, g := range groups {
		if g.Calls < 2 && g.Ran == 0 {
			continue
		}
		g.Sessions = int64(len(g.sess))
		k := g.Repo + "\x00" + fmt.Sprint(g.members)
		if b := best[k]; b == nil || g.rank < b.rank || (g.rank == b.rank && g.Rule < b.Rule) {
			best[k] = g
		}
	}
	list := make([]*RuleSuggestion, 0, len(best))
	for _, g := range best {
		if pr, ok := added[g.Repo+"\x00"+g.Rule]; ok {
			g.Added, g.PR = true, pr
		}
		if !g.Added && g.Refusal == 0 {
			continue
		}
		list = append(list, g)
	}
	slices.SortFunc(list, func(a, b *RuleSuggestion) int {
		switch {
		case a.Added != b.Added:
			if a.Added {
				return 1
			}
			return -1
		case a.Sessions != b.Sessions:
			return int(b.Sessions - a.Sessions)
		case a.Calls != b.Calls:
			return int(b.Calls - a.Calls)
		case a.Ran != b.Ran:
			return int(b.Ran - a.Ran)
		}
		return strings.Compare(a.Rule, b.Rule)
	})
	if len(list) > maxRuleSuggestions {
		list = list[:maxRuleSuggestions]
	}
	return list, nil
}

// AllowRefusedCall adds an allow rule a person picked for a refused call
// (WP-233, WP-ADR114) to its session repo's .claude/settings.json, by a pull
// request labelled automerge, so later sessions there skip the classifier
// for it. The rule passes the same checks as a suggestion. Nothing else is
// remembered and no agent is woken: the agent long since moved on.
func (s *Service) AllowRefusedCall(ctx context.Context, actor *Actor, id int64, rule string) (*ToolRefusal, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	if actor.IsAgent() {
		return nil, forbidden("a person picks an allow rule (WP-ADR114); file a request instead: wp request ask")
	}
	if actor.HidesCommandText() {
		return nil, forbidden("you don't see refused calls' commands, so you can't pick a rule for one")
	}
	vis, args := requestVisible(actor, "f")
	list, err := s.loadRefusals(ctx, actor, vis+` AND f.id = ?`, append(args, id)...)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, notFound("refused call")
	}
	f := list[0]
	if f.Rule != "" {
		return nil, invalid("%s was already added for this call", f.Rule)
	}
	rule = strings.TrimSpace(rule)
	if err := checkPermissionRule(f.Tool, rule); err != nil {
		return nil, err
	}
	if f.Session == nil || f.Session.Repo == "" {
		return nil, invalid("the session didn't say which repo it ran in, so there is no .claude/settings.json to add the rule to")
	}
	repo, err := s.setupRepoFor(ctx, actor, 0, f.Session.Repo, RoleEditor)
	if errors.Is(err, ErrNotFound) {
		return nil, invalid("%s isn't attached to a project you edit, so Waypoint can't add the rule there", f.Session.Repo)
	}
	if err != nil {
		return nil, err
	}
	err = s.write(ctx, actor, func(t *tx) error {
		now := s.nowMS()
		res, err := t.ExecContext(ctx, `UPDATE tool_refusals SET rule_by = ?, rule_at = ?, rule_reason = ''
			WHERE id = ? AND rule = '' AND (rule_at IS NULL OR rule_reason != '' OR rule_at < ?)`, actor.id(), now, f.ID, now-refusalPickStale.Milliseconds())
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return invalid("a rule for this call is already on its way; its pull request shows here once it opens")
		}
		e := &Event{Kind: EvRefusalAllowed, Data: map[string]any{"refusal": f.ID, "rule": rule, "repo": repo.repo, "session": f.Session.ID}}
		if f.projectID.Valid {
			e.ProjectID = f.projectID.Int64
		} else {
			e.UserID = f.userID
		}
		e.permRule = &permRuleJob{refusal: f.ID, at: now, repo: repo, rule: rule}
		t.emit(e)
		t.audit(AuditEntry{Action: "refusal.allow", Category: AuditApproval, ProjectID: derefInt(ptrInt(f.projectID)), TargetType: "refusal", TargetID: f.ID,
			Target: rule, Detail: excerpt(f.Command, 200), About: f.Agent})
		return nil
	})
	if err != nil {
		return nil, err
	}
	list, err = s.loadRefusals(ctx, actor, `f.id = ?`, f.ID)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, notFound("refused call")
	}
	return list[0], nil
}

// addRefusalRule opens the pull request for a rule picked from a refused
// call (permRuleJob.refusal) and records the outcome on it.
func (s *Service) addRefusalRule(ctx context.Context, actor *Actor, j *permRuleJob) {
	var tool, input, command string
	var repeats int
	if err := s.db.R.QueryRowContext(ctx, `SELECT tool, input, command, repeats FROM tool_refusals WHERE id = ?`, j.refusal).Scan(&tool, &input, &command, &repeats); err != nil {
		s.log.ErrorContext(ctx, "add a refusal's rule: load", "refusal", j.refusal, "err", err)
		return
	}
	call := &ToolCall{Name: tool, Input: json.RawMessage(input)}
	if input == "" {
		call.Input, _ = json.Marshal(map[string]string{"command": command})
	}
	from := "for a call auto mode refused"
	if repeats > 1 {
		from += fmt.Sprintf(" %d times in a row", repeats)
	}
	var rule, pr, reason string
	res, err := s.openRulePR(ctx, actor, call, from, j.repo, j.rule)
	switch {
	case errors.Is(err, errRulePresent):
		rule, reason = j.rule, fmt.Sprintf("%s already allows %s on its default branch, so no pull request was needed.", j.repo.repo, j.rule)
	case err != nil:
		reason = fmt.Sprintf("The rule %s wasn't added: %s", j.rule, excerpt(err.Error(), 400))
		s.log.WarnContext(ctx, "add a refusal's rule", "refusal", j.refusal, "err", err)
	default:
		rule, pr = j.rule, res.URL
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE tool_refusals SET rule = ?, rule_pr = ?, rule_reason = ? WHERE id = ? AND rule_at = ?`, rule, pr, reason, j.refusal, j.at); err != nil {
		s.log.ErrorContext(ctx, "record a refusal's rule", "refusal", j.refusal, "rule", j.rule, "pr", pr, "err", err)
	}
}
