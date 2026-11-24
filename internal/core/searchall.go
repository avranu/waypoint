package core

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/wql"
)

// SearchKinds are what SearchAll covers, in the order groups are returned.
var SearchKinds = []string{KIssue, KRequest, KDesign, KGlossary, KPage, "comment"}

// AllQuery is a search across kinds.
type AllQuery struct {
	Text string `json:"q"` // words; issues also understand the query language
	// Kinds limits the kinds searched (issue, request, page, comment,
	// glossary, design; "logs" means comment). Empty: all.
	Kinds []string `json:"kinds"`
	// Projects narrows to these project keys (plus, for the glossary and
	// design notes, their ancestors and global items). Empty: everything.
	Projects []string `json:"projects"`
	// Archived includes closed issues, their comments, and superseded or
	// retired notes.
	Archived bool `json:"archived"`
	Limit    int  `json:"limit"` // per kind; default 10
	// Near is the project key the searcher is looking at: of several things
	// a bare code names (ADR3 in three projects), its own comes first. It
	// orders, it never filters.
	Near string `json:"near"`
}

// Snippets mark the matched words ⟦like this⟧: brackets alone would clash
// with [[wikilinks]] and markdown links.
const (
	MarkStart = "⟦"
	MarkEnd   = "⟧"
)

// cleanSnippet flattens a snippet to one line.
func cleanSnippet(s string) string { return strings.Join(strings.Fields(s), " ") }

// SearchHit is one match.
type SearchHit struct {
	Kind       string `json:"kind"`
	ID         int64  `json:"id"`
	Key        string `json:"key,omitempty"` // the issue, for issues and comments; a page's code
	Title      string `json:"title"`
	Snippet    string `json:"snippet,omitempty"` // one line; matched words marked ⟦like this⟧
	ProjectKey string `json:"project_key,omitempty"`
	State      string `json:"state,omitempty"` // issue status category; design status or "proposed"; "locked" glossary entries
	Sub        string `json:"sub,omitempty"`   // issue status, page category, design kind, request kind, comment author
	// Match, when the search has a code in it, says why this is here: exact
	// (it is what the code names), mention (its text writes the code) or text.
	Match     string    `json:"match,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SearchGroup is the matches of one kind.
type SearchGroup struct {
	Kind  string      `json:"kind"`
	Total int         `json:"total"` // matches found (issues: exact; others: up to the cap)
	Hits  []SearchHit `json:"hits"`
}

// AllResult is SearchAll's answer.
type AllResult struct {
	Query    string   `json:"query"`
	Kinds    []string `json:"kinds"`
	Archived bool     `json:"archived"`
	// Exact is what the codes in the query name, best first: the thing a
	// person searching R-28 wants on top, before the groups.
	Exact  []SearchHit   `json:"exact"`
	Groups []SearchGroup `json:"groups"`
}

// ParseKinds reads a kinds list ("issues,pages", "logs"), defaulting to all.
func ParseKinds(list []string) ([]string, error) {
	var out []string
	for _, item := range list {
		for _, k := range strings.Split(item, ",") {
			k = strings.ToLower(strings.TrimSpace(k))
			switch k {
			case "":
				continue
			case "issues", "tasks", "task":
				k = KIssue
			case "requests", "req", "reqs":
				k = KRequest
			case "pages":
				k = KPage
			case "comments", "logs", "log":
				k = "comment"
			case "terms", "term":
				k = KGlossary
			case "designs", "notes", "note", "decisions", "spec", "specs":
				k = KDesign
			}
			if !slices.Contains(SearchKinds, k) {
				return nil, invalid("kinds are issues, requests, pages, comments (logs), glossary and design")
			}
			if !slices.Contains(out, k) {
				out = append(out, k)
			}
		}
	}
	if len(out) == 0 {
		return slices.Clone(SearchKinds), nil
	}
	slices.SortFunc(out, func(a, b string) int { return slices.Index(SearchKinds, a) - slices.Index(SearchKinds, b) })
	return out, nil
}

// SearchAll searches every kind asked for.
func (s *Service) SearchAll(ctx context.Context, actor *Actor, q AllQuery) (*AllResult, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	text := strings.TrimSpace(q.Text)
	if text == "" {
		return nil, invalid("search for something")
	}
	kinds, err := ParseKinds(q.Kinds)
	if err != nil {
		return nil, err
	}
	numbered, text2 := numberedSearchRefs(text)
	codes, rest := parseSearchCodes(text2)
	parsed, err := wql.Parse(rest)
	if err != nil {
		return nil, invalid("%s", err.Error())
	}
	looseOK := strings.TrimSpace(rest) != ""
	words := strings.Join(quoteWords(parsed.Text), " ")
	if q.Limit <= 0 {
		q.Limit = 10
	}
	q.Limit = min(q.Limit, 100)
	var projects []*Project
	for _, k := range q.Projects {
		if k = strings.TrimSpace(k); k == "" {
			continue
		}
		p, err := s.projectByRef(ctx, s.db.R, k)
		if err != nil {
			return nil, err
		}
		if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	var sc refScope
	if len(projects) == 1 {
		sc = refScope{projects[0].Key, projects[0].ID}
	} else if near := strings.TrimSpace(q.Near); near != "" {
		if p, err := s.projectByRef(ctx, s.db.R, near); err == nil {
			sc = refScope{p.Key, p.ID}
		}
	}
	var exact []SearchHit
	var spelled []searchCode
	for _, c := range codes {
		refs := s.exactRefs(ctx, actor, c, sc)
		if len(refs) > 0 || c.strict {
			spelled = append(spelled, c)
		}
		for _, r := range refs {
			exact = append(exact, s.hitOf(ctx, r))
		}
	}
	for _, n := range numbered {
		r := s.resolveRef(ctx, actor, n, sc)
		if r != nil {
			exact = append(exact, s.hitOf(ctx, *r))
		}
		if m := refNumberedRE.FindStringSubmatch(n); m != nil && strings.EqualFold(m[1], "request") {
			num, _ := strconv.Atoi(m[2])
			for _, id := range s.requestIDsNumbered(ctx, actor, num) {
				if r == nil || r.ID != id {
					if rr := s.requestRef(ctx, id); rr != nil {
						exact = append(exact, s.hitOf(ctx, *rr))
					}
				}
			}
		}
	}
	exact = slices.DeleteFunc(exact, func(h SearchHit) bool {
		return !slices.Contains(kinds, h.Kind) || (len(projects) > 0 && !slices.Contains(projectKeys(projects), h.ProjectKey))
	})
	if sc.key != "" {
		slices.SortStableFunc(exact, func(a, b SearchHit) int {
			return nearRank(a, sc.key) - nearRank(b, sc.key)
		})
	}
	mention := mentionExpr(spelled)
	coded := len(codes) > 0 || len(numbered) > 0
	res := &AllResult{Query: text, Kinds: kinds, Archived: q.Archived, Exact: mergeTiers(100, exact), Groups: []SearchGroup{}}
	if res.Exact == nil {
		res.Exact = []SearchHit{}
	}
	run := func(kind string, w string, raw bool) (SearchGroup, error) {
		if raw {
			w = ftsRaw + w
		}
		switch kind {
		case KIssue:
			if raw {
				return s.searchIssuesFor(ctx, actor, parsed, "", projects, q, w[len(ftsRaw):])
			}
			return s.searchIssuesFor(ctx, actor, parsed, rest, projects, q, "")
		case KRequest:
			return s.searchRequestsFor(ctx, actor, w, projects, q.Limit)
		case KPage:
			return s.searchPagesFor(ctx, actor, w, projects, q.Limit)
		case "comment":
			return s.searchCommentsFor(ctx, actor, w, projects, q)
		case KGlossary:
			return s.searchGlossaryFor(ctx, actor, w, projects, q.Limit)
		case KDesign:
			return s.searchDesignFor(ctx, actor, w, projects, q)
		}
		return SearchGroup{}, nil
	}
	for _, kind := range kinds {
		var g SearchGroup
		var err error
		if looseOK {
			if g, err = run(kind, words, false); err != nil {
				return nil, err
			}
		}
		if coded {
			var ment SearchGroup
			if mention != "" {
				if ment, err = run(kind, mention, true); err != nil {
					return nil, err
				}
			}
			var ex []SearchHit
			for _, h := range res.Exact {
				if h.Kind == kind {
					ex = append(ex, h)
				}
			}
			loose := len(g.Hits)
			hits := mergeTiers(q.Limit, ex, markHits(ment.Hits, MatchMention), markHits(g.Hits, MatchText))
			extra := 0
			for _, h := range mergeTiers(1000, ex, ment.Hits) {
				if !slices.ContainsFunc(g.Hits, func(x SearchHit) bool { return x.ID == h.ID }) {
					extra++
				}
			}
			g.Hits, g.Total = hits, max(g.Total, loose)+extra
		}
		g.Kind = kind
		if g.Hits == nil {
			g.Hits = []SearchHit{}
		}
		res.Groups = append(res.Groups, g)
	}
	slices.SortStableFunc(res.Groups, func(a, b SearchGroup) int {
		return exactCount(res.Exact, b.Kind) - exactCount(res.Exact, a.Kind)
	})
	return res, nil
}

func nearRank(h SearchHit, key string) int {
	if h.ProjectKey == key {
		return 0
	}
	return 1
}

func exactCount(hits []SearchHit, kind string) int {
	n := 0
	for _, h := range hits {
		if h.Kind == kind {
			n++
		}
	}
	return min(n, 1)
}

// quoteWords keeps phrases (with spaces) quoted for full-text search.
func quoteWords(words []string) []string {
	out := make([]string, len(words))
	for i, w := range words {
		if strings.Contains(w, " ") {
			w = `"` + strings.ReplaceAll(w, `"`, "") + `"`
		}
		out[i] = w
	}
	return out
}

func projectKeys(ps []*Project) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Key
	}
	return out
}

func (s *Service) searchIssuesFor(ctx context.Context, actor *Actor, parsed wql.Query, text string, projects []*Project, q AllQuery, raw string) (SearchGroup, error) {
	query := text
	if len(projects) > 0 {
		query += " project:" + strings.Join(projectKeys(projects), ",")
	}
	if !q.Archived && !parsed.Has("is") && !parsed.Has("status") && !parsed.Has("category") && !parsed.Has("cat") && !parsed.Has("resolved") {
		query += " is:open"
	}
	r, err := s.Search(ctx, actor, SearchParams{Query: query, Limit: q.Limit, DefaultOrder: "updated", RawFTS: raw, Near: q.Near})
	if err != nil {
		return SearchGroup{}, err
	}
	g := SearchGroup{Total: r.Total}
	snips := map[int64]string{}
	m := ftsQuery(parsed.Text)
	if raw != "" {
		m = raw
	}
	if m != "" && len(r.Issues) > 0 {
		ids := make([]int64, len(r.Issues))
		for i, is := range r.Issues {
			ids[i] = is.ID
		}
		snips = s.snippets(ctx, `SELECT rowid, snippet(issue_search, 2, '⟦', '⟧', '…', 12) FROM issue_search WHERE issue_search MATCH ? AND rowid IN (`+placeholders(len(ids))+`)`,
			append([]any{m}, int64Args(ids)...)...)
	}
	for _, i := range r.Issues {
		g.Hits = append(g.Hits, SearchHit{Kind: KIssue, ID: i.ID, Key: i.Key, Title: i.Title, Snippet: snips[i.ID],
			ProjectKey: i.ProjectKey, State: i.Status.Category, Sub: i.Status.Name, UpdatedAt: i.UpdatedAt})
	}
	return g, nil
}

// snippets runs a query returning (id, snippet) pairs.
func (s *Service) snippets(ctx context.Context, query string, args ...any) map[int64]string {
	out := map[int64]string{}
	// #nosec G202 -- callers pass fixed SQL with ? placeholders
	rows, err := s.db.R.QueryContext(ctx, query, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var snip string
		if rows.Scan(&id, &snip) == nil {
			out[id] = cleanSnippet(snip)
		}
	}
	return out
}

func (s *Service) searchPagesFor(ctx context.Context, actor *Actor, words string, projects []*Project, limit int) (SearchGroup, error) {
	if words == "" {
		return SearchGroup{}, nil
	}
	var pages []*Page
	if len(projects) == 0 {
		var err error
		if pages, err = s.ListPages(ctx, actor, PageQuery{Text: words, Limit: limit * 3}); err != nil {
			return SearchGroup{}, err
		}
	}
	for _, p := range projects {
		list, err := s.ListPages(ctx, actor, PageQuery{Project: p.Key, Text: words, Limit: limit * 3})
		if err != nil {
			return SearchGroup{}, err
		}
		pages = append(pages, list...)
	}
	m := ftsMatch(words)
	ids := make([]int64, len(pages))
	for i, p := range pages {
		ids[i] = p.ID
	}
	rank := map[int64]int{}
	snips := map[int64]string{}
	if m != "" && len(ids) > 0 {
		// #nosec G202 -- only ? placeholders are joined in
		rows, err := s.db.R.QueryContext(ctx, `SELECT rowid, snippet(page_search, 1, '⟦', '⟧', '…', 12) FROM page_search
			WHERE page_search MATCH ? AND rowid IN (`+placeholders(len(ids))+`) ORDER BY bm25(page_search, 10.0, 1.0)`, append([]any{m}, int64Args(ids)...)...)
		if err == nil {
			n := 0
			for rows.Next() {
				var id int64
				var snip string
				if rows.Scan(&id, &snip) == nil {
					rank[id], snips[id] = n, cleanSnippet(snip)
					n++
				}
			}
			rows.Close()
		}
	}
	slices.SortStableFunc(pages, func(a, b *Page) int { return rank[a.ID] - rank[b.ID] })
	g := SearchGroup{Total: len(pages)}
	for _, p := range pages {
		if len(g.Hits) >= limit {
			break
		}
		g.Hits = append(g.Hits, SearchHit{Kind: KPage, ID: p.ID, Key: p.Code, Title: p.Title, Snippet: snips[p.ID], ProjectKey: p.ProjectKey, Sub: p.Category, UpdatedAt: p.UpdatedAt})
	}
	return g, nil
}

func (s *Service) searchCommentsFor(ctx context.Context, actor *Actor, words string, projects []*Project, q AllQuery) (SearchGroup, error) {
	m := ftsMatch(words)
	if m == "" {
		return SearchGroup{}, nil
	}
	with, wargs := "", []any{}
	where, args := []string{"comment_search MATCH ?", draftsHidden(actor, "i")}, []any{m}
	switch member, margs := actor.memberProjects(RoleViewer); {
	case actor.scope != nil:
		where, args = append(where, "i.project_id IN "+member), append(args, margs...)
	case !actor.isAdmin():
		with = `WITH RECURSIVE shared(id, depth) AS (
			SELECT issue_id, 0 FROM issue_shares WHERE user_id = ?
			UNION ALL SELECT x.id, sh.depth + 1 FROM issues x JOIN shared sh ON x.parent_id = sh.id WHERE sh.depth < 50) `
		wargs = append(wargs, actor.id())
		where, args = append(where, "(i.project_id IN "+member+" OR i.id IN (SELECT id FROM shared))"), append(args, margs...)
	}
	if len(projects) > 0 {
		ids := make([]int64, len(projects))
		for i, p := range projects {
			ids[i] = p.ID
		}
		where, args = append(where, "i.project_id IN ("+placeholders(len(ids))+")"), append(args, int64Args(ids)...)
	}
	if !q.Archived {
		where = append(where, "st.category NOT IN ('done', 'canceled')")
	}
	// #nosec G202 -- fixed conditions and ? placeholders
	rows, err := s.db.R.QueryContext(ctx, with+`SELECT c.id, c.updated_at, snippet(comment_search, 0, '⟦', '⟧', '…', 14),
		i.id, i.project_id, p.key || '-' || i.number, i.title, st.category, coalesce(u.display_name, u.username, '')
		FROM comment_search JOIN comments c ON c.id = comment_search.rowid
		JOIN issues i ON i.id = c.issue_id JOIN projects p ON p.id = i.project_id JOIN statuses st ON st.id = i.status_id
		LEFT JOIN users u ON u.id = c.author_id
		WHERE `+strings.Join(where, " AND ")+` ORDER BY bm25(comment_search) LIMIT 300`, append(wargs, args...)...)
	if err != nil {
		return SearchGroup{}, err
	}
	type row struct {
		hit       SearchHit
		issue, pj int64
	}
	var list []row
	for rows.Next() {
		var r row
		var upd int64
		var author sql.NullString
		if err := rows.Scan(&r.hit.ID, &upd, &r.hit.Snippet, &r.issue, &r.pj, &r.hit.Key, &r.hit.Title, &r.hit.State, &author); err != nil {
			rows.Close()
			return SearchGroup{}, err
		}
		r.hit.Kind, r.hit.UpdatedAt, r.hit.Sub = "comment", msTime(upd), author.String
		r.hit.Snippet = cleanSnippet(r.hit.Snippet)
		r.hit.ProjectKey, _, _ = strings.Cut(r.hit.Key, "-")
		list = append(list, r)
	}
	rows.Close()
	g := SearchGroup{}
	seen := map[int64]bool{}
	for _, r := range list {
		ok, known := seen[r.issue]
		if !known {
			ok = s.canSee(ctx, s.db.R, actor, r.issue, r.pj)
			seen[r.issue] = ok
		}
		if !ok {
			continue
		}
		g.Total++
		if len(g.Hits) < q.Limit {
			g.Hits = append(g.Hits, r.hit)
		}
	}
	return g, nil
}

func (s *Service) searchGlossaryFor(ctx context.Context, actor *Actor, words string, projects []*Project, limit int) (SearchGroup, error) {
	if words == "" {
		return SearchGroup{}, nil
	}
	var entries []*GlossaryEntry
	seen := map[int64]bool{}
	add := func(list []*GlossaryEntry) {
		for _, e := range list {
			if !seen[e.ID] {
				seen[e.ID] = true
				entries = append(entries, e)
			}
		}
	}
	if len(projects) == 0 {
		list, err := s.ListGlossary(ctx, actor, GlossaryQuery{Text: words, Limit: 200})
		if err != nil {
			return SearchGroup{}, err
		}
		add(list)
	}
	for _, p := range projects {
		list, err := s.ListGlossary(ctx, actor, GlossaryQuery{Project: p.Key, Text: words, Limit: 200})
		if err != nil {
			return SearchGroup{}, err
		}
		add(list)
	}
	g := SearchGroup{Total: len(entries)}
	for _, e := range entries {
		if len(g.Hits) >= limit {
			break
		}
		snip := cleanSnippet(e.Snippet)
		if snip == "" {
			snip = summary(e.Definition)
		}
		st := ""
		if e.Locked {
			st = "locked"
		}
		g.Hits = append(g.Hits, SearchHit{Kind: KGlossary, ID: e.ID, Title: e.Term, Snippet: snip, ProjectKey: e.ProjectKey,
			State: st, Sub: strings.Join(e.Aliases, ", "), UpdatedAt: e.UpdatedAt})
	}
	return g, nil
}

func (s *Service) searchDesignFor(ctx context.Context, actor *Actor, words string, projects []*Project, q AllQuery) (SearchGroup, error) {
	if words == "" {
		return SearchGroup{}, nil
	}
	status := ""
	if q.Archived {
		status = "all"
	}
	var notes []*DesignNote
	seen := map[int64]bool{}
	add := func(list []*DesignNote) {
		for _, n := range list {
			if !seen[n.ID] {
				seen[n.ID] = true
				notes = append(notes, n)
			}
		}
	}
	if len(projects) == 0 {
		list, err := s.ListDesign(ctx, actor, DesignQuery{Text: words, Status: status, Limit: 200})
		if err != nil {
			return SearchGroup{}, err
		}
		add(list)
	}
	for _, p := range projects {
		list, err := s.ListDesign(ctx, actor, DesignQuery{Project: p.Key, Text: words, Status: status, Limit: 200})
		if err != nil {
			return SearchGroup{}, err
		}
		add(list)
	}
	g := SearchGroup{Total: len(notes)}
	for _, n := range notes {
		if len(g.Hits) >= q.Limit {
			break
		}
		st := n.Status
		if !n.Approved {
			st = "proposed"
		}
		g.Hits = append(g.Hits, SearchHit{Kind: KDesign, ID: n.ID, Key: n.Code, Title: n.Title, Snippet: cleanSnippet(n.Snippet), ProjectKey: n.ProjectKey,
			State: st, Sub: n.Kind, UpdatedAt: n.UpdatedAt})
	}
	return g, nil
}
