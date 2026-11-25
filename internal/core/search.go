package core

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"git.jmann.me/claude/waypoint/internal/wql"
)

// SearchParams scope an issue search.
type SearchParams struct {
	Query   string `json:"q"`
	Project string `json:"project"` // optional project key to scope to
	Limit   int    `json:"limit"`
	Offset  int    `json:"offset"`
	// WithDescription includes descriptions in results (off for lists).
	WithDescription bool `json:"with_description"`
	// DefaultOrder applies when the query has no order: term:
	// "rank" (manual order) or "updated" (most recent first).
	DefaultOrder string `json:"-"`
	// RawFTS is an FTS5 MATCH expression the issues must match, besides the
	// query's own words.
	RawFTS string `json:"-"`
	// Near is the project the searcher is in (key): when ranking by
	// relevance its issues count double. It orders, it never filters.
	Near string `json:"near"`
}

// SearchResult is a page of issues.
type SearchResult struct {
	Issues []*Issue `json:"issues"`
	Total  int      `json:"total"`
	Limit  int      `json:"limit"`
	Offset int      `json:"offset"`
}

// compiled is a query turned into SQL fragments.
type compiled struct {
	with   string
	joins  string
	where  []string
	args   []any
	jargs  []any
	order  string
	hasFTS bool
	score  string
}

func (c *compiled) add(cond string, args ...any) {
	c.where = append(c.where, cond)
	c.args = append(c.args, args...)
}

func (c *compiled) sql(selectCols string) (string, []any) {
	q := c.with + " " + selectCols + " " + c.joins
	if len(c.where) > 0 {
		q += " WHERE " + strings.Join(c.where, " AND ")
	}
	return q, append(append([]any{}, c.jargs...), c.args...)
}

// compileQuery turns a parsed query into SQL over issues i/projects p/
// statuses st/sprints sp, limited to what the actor may see.
func (s *Service) compileQuery(ctx context.Context, actor *Actor, q wql.Query, projectID int64) (*compiled, error) {
	c := &compiled{}
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	c.with = `WITH RECURSIVE shared(id, depth) AS (
		SELECT issue_id, 0 FROM issue_shares WHERE user_id = ?
		UNION ALL SELECT x.id, sh.depth + 1 FROM issues x JOIN shared sh ON x.parent_id = sh.id WHERE sh.depth < 50)`
	c.jargs = append(c.jargs, actor.id())
	switch member, margs := actor.memberProjects(RoleViewer); {
	case actor.scope != nil:
		c.add(`i.project_id IN `+member, margs...)
	case !actor.isAdmin():
		c.add(`(i.project_id IN `+member+` OR i.id IN (SELECT id FROM shared))`, margs...)
	}
	if actor.HidesDrafts() {
		c.add(`i.draft = 0`)
	}
	if projectID != 0 {
		c.add(`i.project_id = ?`, projectID)
	}
	loc := s.actorLocation(actor)
	now := s.now().In(loc)
	today := now.Format(dateLayout)
	for _, t := range q.Terms {
		cond, args, err := s.compileTerm(ctx, actor, t, now, today, loc)
		if err != nil {
			return nil, err
		}
		if cond == "" {
			continue
		}
		if t.Negate {
			cond = "coalesce((" + cond + "), 0) = 0"
		}
		c.add(cond, args...)
	}
	var words []string
	for _, w := range q.Text {
		if keyPattern.MatchString(w) {
			c.add(`(p.key || '-' || i.number) = ? COLLATE NOCASE`, strings.ToUpper(w))
			continue
		}
		if looksLikeKey(w) {
			if id, err := s.resolveIssueID(ctx, s.db.R, w); err == nil {
				c.add(`i.id = ?`, id)
				continue
			}
		}
		words = append(words, w)
	}
	if len(words) > 0 {
		if m := ftsQuery(words); m != "" {
			c.hasFTS = true
			c.score = "f.score"
			c.joins += ` JOIN (SELECT rowid AS rid, bm25(issue_search, 10.0, 5.0, 1.0) AS score FROM issue_search WHERE issue_search MATCH ?) f ON f.rid = i.id`
			c.jargs = append(c.jargs, m)
			if t := ftsTitleQuery(words); t != "" {
				c.score = "f.score * (CASE WHEN ft.rid IS NULL THEN 1 ELSE 2 END)"
				c.joins += ` LEFT JOIN (SELECT rowid AS rid FROM issue_search WHERE issue_search MATCH ?) ft ON ft.rid = i.id`
				c.jargs = append(c.jargs, t)
			}
		}
	}
	var orders []string
	for _, o := range q.Order {
		expr, ok := orderExprs[o.Field]
		if !ok {
			return nil, invalid("can't order by %q (try %s)", o.Field, strings.Join(orderFieldNames(), ", "))
		}
		dir := " ASC"
		if o.Desc {
			dir = " DESC"
		}
		if strings.Contains(expr, "due_date") || strings.Contains(expr, "start_date") || strings.Contains(expr, "points") || strings.Contains(expr, "resolved") {
			orders = append(orders, expr+" IS NULL")
		}
		orders = append(orders, expr+dir)
	}
	c.order = strings.Join(orders, ", ")
	return c, nil
}

var orderExprs = map[string]string{
	"rank": "i.rank", "priority": "i.priority", "updated": "i.updated_at", "created": "i.created_at",
	"due": "i.due_date", "start": "i.start_date", "points": "i.points", "key": "i.number", "number": "i.number",
	"title": "lower(i.title)", "status": "st.position", "resolved": "i.resolved_at", "type": "i.type",
}

func orderFieldNames() []string {
	out := make([]string, 0, len(orderExprs))
	for k := range orderExprs {
		out = append(out, k)
	}
	return out
}

// ftsRaw starts a search text that is already an FTS5 MATCH expression
// (WP-122: a code's spellings, which the word search would take for prefixes).
const ftsRaw = "\x00fts:"

// ftsMatch is the MATCH expression for a search text: the words, or the
// expression a ftsRaw text carries.
func ftsMatch(text string) string {
	if strings.HasPrefix(text, ftsRaw) {
		return text[len(ftsRaw):]
	}
	return ftsQuery(strings.Fields(text))
}

// ftsQuery builds an FTS5 MATCH expression: every word must match. A word
// of three or more letters matches as itself or as a prefix (typing "proc"
// finds "process"), with the whole word scoring higher; a shorter one only
// as itself, since "pr" as a prefix is "project", "process", "prompt"…
// (WP-610). A quoted phrase matches as written.
func ftsQuery(words []string) string {
	pos, neg := ftsTerms(words)
	if len(pos) == 0 {
		return ""
	}
	q := strings.Join(pos, " AND ")
	for _, t := range neg {
		q += " NOT " + t
	}
	return q
}

// ftsTitleQuery matches the issues whose key or title has every word.
func ftsTitleQuery(words []string) string {
	pos, _ := ftsTerms(words)
	if len(pos) == 0 {
		return ""
	}
	return "{key title} : (" + strings.Join(pos, " AND ") + ")"
}

// ftsTerms turns words into FTS5 terms, wanted and unwanted ("-word").
func ftsTerms(words []string) (pos, neg []string) {
	for _, w := range words {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		isNeg := false
		if strings.HasPrefix(w, "-") && len(w) > 1 {
			isNeg, w = true, w[1:]
		}
		quoted := `"` + strings.ReplaceAll(w, `"`, `""`) + `"`
		term := quoted
		if !strings.Contains(w, " ") && !shortWord(w) {
			term = "(" + quoted + " OR " + quoted + "*)"
		}
		if isNeg {
			neg = append(neg, term)
		} else {
			pos = append(pos, term)
		}
	}
	return pos, neg
}

// shortWord is a word of one or two letters or digits ("PR", "PR?", "(PR)"),
// which matches only as itself. Not in scripts written without spaces,
// where a whole run is one token and "日本" must still find "日本語".
func shortWord(w string) bool {
	n := 0
	for _, r := range w {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul, unicode.Thai) {
			return false
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			n++
		}
	}
	return n <= 2
}

// likeContains is a LIKE pattern matching v literally anywhere.
func likeContains(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(v) + "%"
}

func opSQL(op string) string {
	if op == "=" {
		return "="
	}
	return op
}

func inList(col string, vals []string) (string, []any) {
	args := make([]any, len(vals))
	for i, v := range vals {
		args[i] = v
	}
	if len(vals) == 1 {
		return col + " = ?", args
	}
	return col + " IN (" + placeholders(len(vals)) + ")", args
}

// userCond matches a user column/EXISTS against me/none/usernames.
func (s *Service) userIDs(ctx context.Context, actor *Actor, vals []string) ([]int64, bool, error) {
	var ids []int64
	none := false
	for _, v := range vals {
		if strings.EqualFold(v, "none") || strings.EqualFold(v, "nobody") {
			none = true
			continue
		}
		u, err := s.resolveUser(ctx, s.db.R, actor, v)
		if err != nil {
			return nil, false, invalid("no user %q", v)
		}
		ids = append(ids, u.ID)
	}
	return ids, none, nil
}

func (s *Service) compileTerm(ctx context.Context, actor *Actor, t wql.Term, now time.Time, today string, loc *time.Location) (string, []any, error) {
	vals := t.Values
	switch t.Key {
	case "project", "proj", "p":
		up := make([]string, len(vals))
		for i, v := range vals {
			up[i] = strings.ToUpper(v)
		}
		c, a := inList("p.key", up)
		return c, a, nil
	case "key", "id", "issue":
		var ids []any
		for _, v := range vals {
			id, err := s.resolveIssueID(ctx, s.db.R, v)
			if err != nil {
				id = -1
			}
			ids = append(ids, id)
		}
		return "i.id IN (" + placeholders(len(ids)) + ")", ids, nil
	case "type", "t":
		lower := make([]string, len(vals))
		for i, v := range vals {
			lower[i] = strings.ToLower(v)
		}
		c, a := inList("i.type", lower)
		return c, a, nil
	case "status", "s":
		c, a := inList("st.name", vals)
		return c, a, nil
	case "category", "cat", "kind":
		var statusCats, issueCats []string
		for _, v := range vals {
			v = strings.ToLower(strings.ReplaceAll(v, " ", "_"))
			if validCategory(v) {
				statusCats = append(statusCats, v)
			} else if v == "none" {
				issueCats = append(issueCats, "")
			} else {
				issueCats = append(issueCats, normalizeIssueCategory(v))
			}
		}
		var parts []string
		var args []any
		if len(statusCats) > 0 {
			c, a := inList("st.category", statusCats)
			parts, args = append(parts, c), append(args, a...)
		}
		if len(issueCats) > 0 {
			c, a := inList("i.category", issueCats)
			parts, args = append(parts, c), append(args, a...)
		}
		return "(" + strings.Join(parts, " OR ") + ")", args, nil
	case "agent", "ai", "model":
		var parts []string
		var args []any
		for _, v := range vals {
			switch n := normalizeAgent(v); n {
			case "none", "", "human", "humans", "humansonly":
				parts = append(parts, "i.agent = 'none'")
			case "anyone", "everyone":
				parts = append(parts, "i.agent = ''")
			case "any":
				parts = append(parts, "i.agent = 'any'")
			default:
				parts = append(parts, `(i.agent != 'none' AND replace(replace(replace(replace(lower(i.agent), '-', ''), '.', ''), '_', ''), ' ', '') LIKE ? ESCAPE '\')`)
				args = append(args, likeContains(n))
			}
		}
		return "(" + strings.Join(parts, " OR ") + ")", args, nil
	case "step":
		lower := make([]string, len(vals))
		for i, v := range vals {
			lower[i] = strings.ToLower(v)
		}
		if lower[0] == "none" {
			return "i.step = ''", nil, nil
		}
		c, a := inList("lower(i.step)", lower)
		return c, a, nil
	case "workflow", "wf":
		if strings.EqualFold(vals[0], "none") {
			return "i.workflow_id IS NULL", nil, nil
		}
		c, a := inList("(SELECT lower(w.name) FROM workflows w WHERE w.id = i.workflow_id)", lowerAll(vals))
		return c, a, nil
	case "risk":
		levels, err := riskLevelsFor(t.Op, vals)
		if err != nil {
			return "", nil, err
		}
		return issueRiskLevelCond(actor, levels), nil, nil
	case "goal", "goals", "traces":
		return goalCond(vals)
	case "is", "has", "no":
		var conds []string
		var args []any
		for _, v := range vals {
			cond, a, err := s.isCond(actor, strings.ToLower(v), today, t.Key)
			if err != nil {
				return "", nil, err
			}
			if t.Key == "no" {
				cond = "NOT (" + cond + ")"
			}
			conds = append(conds, cond)
			args = append(args, a...)
		}
		return "(" + strings.Join(conds, " OR ") + ")", args, nil
	case "assignee", "assigned", "a":
		ids, none, err := s.userIDs(ctx, actor, vals)
		if err != nil {
			return "", nil, err
		}
		var parts []string
		var args []any
		if len(ids) > 0 {
			parts = append(parts, "EXISTS (SELECT 1 FROM issue_assignees x WHERE x.issue_id = i.id AND x.user_id IN ("+placeholders(len(ids))+"))")
			args = append(args, int64Args(ids)...)
		}
		if none {
			parts = append(parts, "NOT EXISTS (SELECT 1 FROM issue_assignees x WHERE x.issue_id = i.id)")
		}
		return "(" + strings.Join(parts, " OR ") + ")", args, nil
	case "reporter", "creator", "author":
		ids, none, err := s.userIDs(ctx, actor, vals)
		if err != nil {
			return "", nil, err
		}
		var parts []string
		var args []any
		if len(ids) > 0 {
			parts = append(parts, "i.reporter_id IN ("+placeholders(len(ids))+")")
			args = append(args, int64Args(ids)...)
		}
		if none {
			parts = append(parts, "i.reporter_id IS NULL")
		}
		return "(" + strings.Join(parts, " OR ") + ")", args, nil
	case "watcher", "watching", "watched":
		ids, _, err := s.userIDs(ctx, actor, vals)
		if err != nil {
			return "", nil, err
		}
		if len(ids) == 0 {
			return "1=0", nil, nil
		}
		return "EXISTS (SELECT 1 FROM issue_watchers x WHERE x.issue_id = i.id AND x.user_id IN (" + placeholders(len(ids)) + "))", int64Args(ids), nil
	case "label", "labels", "l", "tag":
		var parts []string
		var args []any
		var names []string
		for _, v := range vals {
			if strings.EqualFold(v, "none") {
				parts = append(parts, "NOT EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id)")
			} else {
				names = append(names, v)
			}
		}
		if len(names) > 0 {
			parts = append(parts, "EXISTS (SELECT 1 FROM issue_labels il JOIN labels lb ON lb.id = il.label_id WHERE il.issue_id = i.id AND lb.name IN ("+placeholders(len(names))+"))")
			for _, n := range names {
				args = append(args, n)
			}
		}
		return "(" + strings.Join(parts, " OR ") + ")", args, nil
	case "priority", "pri", "prio":
		var ps []any
		for _, v := range vals {
			p, err := ParsePriority(v)
			if err != nil {
				return "", nil, err
			}
			ps = append(ps, p)
		}
		if t.Op == "=" {
			return "i.priority IN (" + placeholders(len(ps)) + ")", ps, nil
		}
		return "i.priority " + opSQL(t.Op) + " ?", ps[:1], nil
	case "points", "pts", "estimate", "sp":
		if strings.EqualFold(vals[0], "none") {
			return "i.points IS NULL", nil, nil
		}
		f, err := strconv.ParseFloat(vals[0], 64)
		if err != nil {
			return "", nil, invalid("points must be a number")
		}
		return "i.points " + opSQL(t.Op) + " ?", []any{f}, nil
	case "sprint":
		var parts []string
		var args []any
		for _, v := range vals {
			switch strings.ToLower(v) {
			case "active", "current":
				parts = append(parts, "sp.state = 'active'")
			case "none", "backlog":
				parts = append(parts, "(i.sprint_id IS NULL OR sp.state = 'closed')")
			case "planned", "future":
				parts = append(parts, "sp.state = 'planned'")
			case "next":
				parts = append(parts, "i.sprint_id = (SELECT n.id FROM sprints n WHERE n.project_id = i.project_id AND n.state = 'planned' ORDER BY coalesce(n.start_date, '9999'), n.id LIMIT 1)")
			case "open":
				parts = append(parts, "sp.state IN ('active','planned')")
			default:
				parts = append(parts, "(sp.name = ? COLLATE NOCASE OR CAST(sp.id AS TEXT) = ?)")
				args = append(args, v, v)
			}
		}
		return "(" + strings.Join(parts, " OR ") + ")", args, nil
	case "parent":
		var parts []string
		var args []any
		for _, v := range vals {
			if strings.EqualFold(v, "none") {
				parts = append(parts, "i.parent_id IS NULL")
				continue
			}
			id, err := s.resolveIssueID(ctx, s.db.R, v)
			if err != nil {
				id = -1
			}
			parts = append(parts, "i.parent_id = ?")
			args = append(args, id)
		}
		return "(" + strings.Join(parts, " OR ") + ")", args, nil
	case "under", "epic", "in", "descendant":
		var parts []string
		var args []any
		for _, v := range vals {
			id, err := s.resolveIssueID(ctx, s.db.R, v)
			if err != nil {
				id = -1
			}
			parts = append(parts, `i.id IN (WITH RECURSIVE d(id, depth) AS (SELECT id, 0 FROM issues WHERE parent_id = ?
				UNION ALL SELECT x.id, d.depth + 1 FROM issues x JOIN d ON x.parent_id = d.id WHERE d.depth < 50) SELECT id FROM d)`)
			args = append(args, id)
		}
		return "(" + strings.Join(parts, " OR ") + ")", args, nil
	case "due", "start":
		col := "i.due_date"
		if t.Key == "start" {
			col = "i.start_date"
		}
		if strings.EqualFold(vals[0], "none") {
			return col + " IS NULL", nil, nil
		}
		d, err := ParseDate(vals[0], now)
		if err != nil || d == nil {
			return "", nil, invalid("can't understand date %q", vals[0])
		}
		return col + " " + opSQL(t.Op) + " ?", []any{*d}, nil
	case "created", "updated", "resolved", "closed":
		col := map[string]string{"created": "i.created_at", "updated": "i.updated_at", "resolved": "i.resolved_at", "closed": "i.resolved_at"}[t.Key]
		if strings.EqualFold(vals[0], "none") {
			return col + " IS NULL", nil, nil
		}
		d, err := ParseDate(vals[0], now)
		if err != nil || d == nil {
			return "", nil, invalid("can't understand date %q", vals[0])
		}
		day, _ := time.ParseInLocation(dateLayout, *d, loc)
		start, end := day.UnixMilli(), day.AddDate(0, 0, 1).UnixMilli()
		switch t.Op {
		case ">":
			return col + " >= ?", []any{end}, nil
		case ">=":
			return col + " >= ?", []any{start}, nil
		case "<":
			return col + " < ?", []any{start}, nil
		case "<=":
			return col + " < ?", []any{end}, nil
		case "!=":
			return "(" + col + " < ? OR " + col + " >= ?)", []any{start, end}, nil
		}
		return "(" + col + " >= ? AND " + col + " < ?)", []any{start, end}, nil
	case "title":
		var parts []string
		var args []any
		for _, v := range vals {
			parts = append(parts, `i.title LIKE ? ESCAPE '\'`)
			args = append(args, likeContains(v))
		}
		return "(" + strings.Join(parts, " OR ") + ")", args, nil
	case "link", "pr", "repo":
		var parts []string
		var args []any
		for _, v := range vals {
			cond := "EXISTS (SELECT 1 FROM external_links el WHERE el.issue_id = i.id AND (el.url LIKE ? ESCAPE '\\' OR el.repo = ? COLLATE NOCASE))"
			args = append(args, likeContains(v), v)
			if t.Key == "repo" {
				cond = "(" + cond + " OR EXISTS (SELECT 1 FROM project_repos pr WHERE pr.project_id = i.project_id AND pr.repo = ? COLLATE NOCASE))"
				args = append(args, v)
			}
			parts = append(parts, cond)
		}
		return "(" + strings.Join(parts, " OR ") + ")", args, nil
	case "text":
		if m := ftsQuery(vals); m != "" {
			return "i.id IN (SELECT rowid FROM issue_search WHERE issue_search MATCH ?)", []any{m}, nil
		}
		return "", nil, nil
	}
	return "", nil, invalid("unknown filter %q (see /api/v1/help/query)", t.Key)
}

// isCond handles is:/has:/no: values.
func (s *Service) isCond(actor *Actor, v, today, key string) (string, []any, error) {
	switch v {
	case "open", "unresolved":
		return "st.category NOT IN ('done','canceled')", nil, nil
	case "closed", "resolved":
		return "st.category IN ('done','canceled')", nil, nil
	case "done", "complete", "completed":
		return "st.category = 'done'", nil, nil
	case "canceled", "cancelled":
		return "st.category = 'canceled'", nil, nil
	case "backlog":
		return "st.category = 'backlog'", nil, nil
	case "todo":
		return "st.category = 'todo'", nil, nil
	case "active", "started", "in_progress", "wip":
		return "st.category = 'in_progress'", nil, nil
	case "mine":
		return "EXISTS (SELECT 1 FROM issue_assignees x WHERE x.issue_id = i.id AND x.user_id = ?)", []any{actor.id()}, nil
	case "unassigned":
		return "NOT EXISTS (SELECT 1 FROM issue_assignees x WHERE x.issue_id = i.id)", nil, nil
	case "assigned", "assignee", "assignees":
		return "EXISTS (SELECT 1 FROM issue_assignees x WHERE x.issue_id = i.id)", nil, nil
	case "overdue":
		return "(i.due_date < ? AND st.category NOT IN ('done','canceled'))", []any{today}, nil
	case "blocked":
		return `EXISTS (SELECT 1 FROM issue_relations r JOIN issues b ON b.id = r.source_id JOIN statuses bs ON bs.id = b.status_id
			WHERE r.target_id = i.id AND r.kind = 'blocks' AND bs.category NOT IN ('done','canceled'))`, nil, nil
	case "followup", "follow_up", "follows":
		return "EXISTS (SELECT 1 FROM issue_relations r WHERE r.source_id = i.id AND r.kind = 'follows')", nil, nil
	case "followed", "followups", "follow_ups":
		return "EXISTS (SELECT 1 FROM issue_relations r WHERE r.target_id = i.id AND r.kind = 'follows')", nil, nil
	case "tension", "tensions", "conflict", "conflicts":
		return "EXISTS (SELECT 1 FROM issue_relations r WHERE (r.source_id = i.id OR r.target_id = i.id) AND r.kind = 'tension')", nil, nil
	case "superseded", "replaced":
		return "EXISTS (SELECT 1 FROM issue_relations r WHERE r.target_id = i.id AND r.kind = 'supersedes')", nil, nil
	case "watching", "watched":
		return "EXISTS (SELECT 1 FROM issue_watchers x WHERE x.issue_id = i.id AND x.user_id = ?)", []any{actor.id()}, nil
	case "shared":
		return "EXISTS (SELECT 1 FROM issue_shares x WHERE x.issue_id = i.id)", nil, nil
	case "points", "estimate", "estimated":
		return "i.points IS NOT NULL", nil, nil
	case "due", "duedate":
		return "i.due_date IS NOT NULL", nil, nil
	case "start", "startdate":
		return "i.start_date IS NOT NULL", nil, nil
	case "parent", "subtask":
		return "i.parent_id IS NOT NULL", nil, nil
	case "children", "subtasks":
		return "EXISTS (SELECT 1 FROM issues c WHERE c.parent_id = i.id)", nil, nil
	case "sprint":
		return "(i.sprint_id IS NOT NULL AND sp.state != 'closed')", nil, nil
	case "label", "labels":
		return "EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id)", nil, nil
	case "link", "links", "pr":
		return "EXISTS (SELECT 1 FROM external_links el WHERE el.issue_id = i.id)", nil, nil
	case "description":
		return "i.description != ''", nil, nil
	case "epic", "story", "task", "bug":
		return "i.type = ?", []any{v}, nil
	case "agent", "agents", "delegated":
		return sqlDelegated, nil, nil
	case "human", "humans":
		return "i.agent = 'none'", nil, nil
	case "anyone":
		return "i.agent = ''", nil, nil
	case "agent-made", "agentmade", "agent_made":
		return "i.reporter_agent != ''", nil, nil
	case "claimed":
		return "(i.claimed_by != '' AND i.claimed_until > ?)", []any{s.nowMS()}, nil
	case "draft", "drafts":
		return "i.draft = 1", nil, nil
	case "published":
		return "i.draft = 0", nil, nil
	case "outside":
		if actor.HidesDrafts() {
			return "0", nil, nil
		}
		return "(i.outside = 1 OR EXISTS (SELECT 1 FROM outside_text o WHERE o.issue_id = i.id AND o.discarded = 0))", nil, nil
	case "unreviewed":
		return sqlUnreviewed, nil, nil
	case "approved":
		return sqlApproved, nil, nil
	case "planned", "prepped":
		return sqlPlanned, nil, nil
	case "agent-closed", "agentclosed", "agent_closed":
		return "(i.resolved_at IS NOT NULL AND i.resolved_agent != '')", nil, nil
	case "close-unreviewed", "closeunreviewed", "close_unreviewed":
		return "(" + closeUnreviewed("i") + ")", nil, nil
	case "on-hold", "onhold", "hold", "on_hold":
		return "(i.hold_at IS NOT NULL AND (i.hold_until IS NULL OR i.hold_until > ? OR i.hold_when != ''))", []any{s.nowMS()}, nil
	case "held":
		return sqlUnreviewed + " AND EXISTS (SELECT 1 FROM task_safety ts WHERE ts.issue_id = i.id AND ts.verdict = 'hold')", nil, nil
	}
	return "", nil, invalid("unknown value %s:%s", key, v)
}

// Search finds issues with a query.
func (s *Service) Search(ctx context.Context, actor *Actor, p SearchParams) (*SearchResult, error) {
	if p.RawFTS == "" {
		if c := bareRequestCode(p.Query); c != nil {
			p.Query, p.RawFTS = "", mentionExpr([]searchCode{*c})
		}
	}
	q, err := wql.Parse(p.Query)
	if err != nil {
		return nil, invalid("%s", err.Error())
	}
	var projectID int64
	if p.Project != "" {
		proj, err := s.projectByRef(ctx, s.db.R, p.Project)
		if err != nil {
			return nil, err
		}
		projectID = proj.ID
	}
	c, err := s.compileQuery(ctx, actor, q, projectID)
	if err != nil {
		return nil, err
	}
	score := c.score
	if p.RawFTS != "" {
		score = "fr.score"
		c.hasFTS = true
		c.joins += ` JOIN (SELECT rowid AS rid, bm25(issue_search, 10.0, 5.0, 1.0) AS score FROM issue_search WHERE issue_search MATCH ?) fr ON fr.rid = i.id`
		c.jargs = append(c.jargs, p.RawFTS)
	}
	if p.Limit <= 0 {
		p.Limit = 100
	}
	p.Limit = min(p.Limit, 1000)
	if p.Offset < 0 {
		p.Offset = 0
	}
	var orderArgs []any
	if p.Near != "" && c.hasFTS && c.order == "" {
		if near, err := s.projectByRef(ctx, s.db.R, p.Near); err == nil {
			score = "(" + score + ") * (CASE WHEN i.project_id = ? THEN 2 ELSE 1 END)"
			orderArgs = append(orderArgs, near.ID)
		}
	}
	order := c.order
	if order == "" {
		switch {
		case c.hasFTS:
			order = score + ", i.updated_at DESC"
		case p.DefaultOrder == "updated":
			order = "i.updated_at DESC"
		default:
			order = "i.rank"
		}
	}
	base := `FROM issues i
	JOIN projects p ON p.id = i.project_id
	JOIN statuses st ON st.id = i.status_id
	LEFT JOIN sprints sp ON sp.id = i.sprint_id`
	countSQL, countArgs := c.sql("SELECT count(*) " + base)
	res := &SearchResult{Limit: p.Limit, Offset: p.Offset}
	if err := s.db.R.QueryRowContext(ctx, countSQL, countArgs...).Scan(&res.Total); err != nil {
		return nil, fmt.Errorf("count: %w", err)
	}
	idSQL, idArgs := c.sql("SELECT i.id " + base)
	idSQL += " ORDER BY " + order + ", i.id LIMIT ? OFFSET ?"
	idArgs = append(append(idArgs, orderArgs...), p.Limit, p.Offset)
	rows, err := s.db.R.QueryContext(ctx, idSQL, idArgs...)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if res.Issues, err = s.issuesByIDs(ctx, ids, p.WithDescription); err != nil {
		return nil, err
	}
	if err := s.listExtras(ctx, actor, res.Issues); err != nil {
		return nil, err
	}
	return res, nil
}

// issuesByIDs loads issues preserving the order of ids.
func (s *Service) issuesByIDs(ctx context.Context, ids []int64, withDesc bool) ([]*Issue, error) {
	if len(ids) == 0 {
		return []*Issue{}, nil
	}
	list, err := s.loadIssues(ctx, s.db.R, issueSelect(withDesc)+` WHERE i.id IN (`+placeholders(len(ids))+`)`, int64Args(ids)...)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*Issue, len(list))
	for _, i := range list {
		byID[i.ID] = i
	}
	out := make([]*Issue, 0, len(ids))
	for _, id := range ids {
		if i, ok := byID[id]; ok {
			out = append(out, i)
		}
	}
	return out, nil
}

// BoardColumn is one status column.
type BoardColumn struct {
	Status Status   `json:"status"`
	Issues []*Issue `json:"issues"`
	Points float64  `json:"points"`
}

// Board is a project's kanban board.
type Board struct {
	Mode    string        `json:"mode"`
	Sprint  *Sprint       `json:"sprint"`
	Columns []BoardColumn `json:"columns"`
}

// GetBoard returns the board: in sprint mode the active sprint's issues, in
// kanban mode everything out of the backlog, with finished work fading off
// after the project's done-visible window. filter is an optional query.
func (s *Service) GetBoard(ctx context.Context, actor *Actor, projectRef, filter string) (*Board, error) {
	p, err := s.GetProject(ctx, actor, projectRef)
	if err != nil {
		return nil, err
	}
	b := &Board{Mode: p.Settings.Mode}
	q, err := wql.Parse(filter)
	if err != nil {
		return nil, invalid("%s", err.Error())
	}
	c, err := s.compileQuery(ctx, actor, q, p.ID)
	if err != nil {
		return nil, err
	}
	c.add(`st.category NOT IN ('backlog','canceled')`)
	if !q.Has("type") && !q.Has("t") {
		c.add(`i.type != 'epic'`)
	}
	if p.Settings.Mode == "sprints" {
		for _, sp := range p.Sprints {
			if sp.State == "active" {
				b.Sprint = sp
			}
		}
		if b.Sprint == nil {
			c.add("1 = 0")
		} else {
			c.add("i.sprint_id = ?", b.Sprint.ID)
		}
	} else {
		cutoff := s.now().AddDate(0, 0, -p.Settings.DoneVisibleDays).UnixMilli()
		c.add(`(st.category != 'done' OR i.resolved_at >= ?)`, cutoff)
	}
	order := c.order
	if order == "" {
		order = "i.rank"
	}
	sqlText, args := s.boardSQL(c, order)
	issues, err := s.loadIssues(ctx, s.db.R, sqlText, args...)
	if err != nil {
		return nil, err
	}
	if err := s.listExtras(ctx, actor, issues); err != nil {
		return nil, err
	}
	byStatus := map[int64][]*Issue{}
	for _, i := range issues {
		byStatus[i.Status.ID] = append(byStatus[i.Status.ID], i)
	}
	for _, st := range p.Statuses {
		if st.Category == CatBacklog || st.Category == CatCanceled {
			continue
		}
		col := BoardColumn{Status: st, Issues: byStatus[st.ID]}
		if col.Issues == nil {
			col.Issues = []*Issue{}
		}
		for _, i := range col.Issues {
			if i.Points != nil {
				col.Points += *i.Points
			}
		}
		b.Columns = append(b.Columns, col)
	}
	return b, nil
}

// boardSQL composes a full issue select with a compiled filter.
func (s *Service) boardSQL(c *compiled, order string) (string, []any) {
	sel := issueSelect(false)
	q := c.with + " " + sel + " " + c.joins
	if len(c.where) > 0 {
		q += " WHERE " + strings.Join(c.where, " AND ")
	}
	q += " ORDER BY " + order + ", i.id"
	return q, append(append([]any{}, c.jargs...), c.args...)
}

// SprintIssues is a sprint with its issues, for the backlog view.
type SprintIssues struct {
	Sprint *Sprint  `json:"sprint"`
	Issues []*Issue `json:"issues"`
}

// Backlog is the planning view.
type Backlog struct {
	Mode    string         `json:"mode"`
	Sprints []SprintIssues `json:"sprints"`
	Backlog []*Issue       `json:"backlog"`
	Epics   []*Issue       `json:"epics"`
}

// GetBacklog returns planned sprints and the unscheduled backlog. In kanban
// mode the backlog is everything in a backlog-category status.
func (s *Service) GetBacklog(ctx context.Context, actor *Actor, projectRef, filter string) (*Backlog, error) {
	p, err := s.GetProject(ctx, actor, projectRef)
	if err != nil {
		return nil, err
	}
	q, err := wql.Parse(filter)
	if err != nil {
		return nil, invalid("%s", err.Error())
	}
	out := &Backlog{Mode: p.Settings.Mode, Sprints: []SprintIssues{}}
	run := func(extra string, args ...any) ([]*Issue, error) {
		c, err := s.compileQuery(ctx, actor, q, p.ID)
		if err != nil {
			return nil, err
		}
		c.add(extra, args...)
		order := c.order
		if order == "" {
			order = "i.rank"
		}
		sqlText, a := s.boardSQL(c, order)
		list, err := s.loadIssues(ctx, s.db.R, sqlText, a...)
		if err == nil {
			err = s.listExtras(ctx, actor, list)
		}
		return list, err
	}
	if p.Settings.Mode == "sprints" {
		for _, sp := range p.Sprints {
			if sp.State == "closed" {
				continue
			}
			issues, err := run("i.sprint_id = ? AND i.type != 'epic'", sp.ID)
			if err != nil {
				return nil, err
			}
			out.Sprints = append(out.Sprints, SprintIssues{Sprint: sp, Issues: issues})
		}
		out.Backlog, err = run("(i.sprint_id IS NULL OR sp.state = 'closed') AND st.category NOT IN ('done','canceled') AND i.type != 'epic'")
	} else {
		out.Backlog, err = run("st.category = 'backlog' AND i.type != 'epic'")
	}
	if err != nil {
		return nil, err
	}
	out.Epics, err = run("i.type = 'epic' AND st.category NOT IN ('done','canceled')")
	return out, err
}
