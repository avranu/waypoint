package core

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// searchCode is one code-shaped word of a search.
type searchCode struct {
	Word   string   // as typed
	Tokens []string // what it could be, in the canonical spelling resolveRef reads
	Phrase []string // the ways text writes it, as FTS phrases: "r 28", "r28"
	strict bool
}

var phraseRE = lazyre.New(`"([^"]+)"`)

// parseSearchCodes finds the code-shaped words in a search. rest is the
// search to run as words: a colon code (API:R-28), which the query language
// would take for a filter, is reduced to its bare R-28.
func parseSearchCodes(text string) (codes []searchCode, rest string) {
	var keep []string
	for _, f := range strings.Fields(text) {
		w := strings.Trim(f, ",.;()")
		if w == "" || strings.ContainsAny(w, `"`) || strings.HasPrefix(w, "-") {
			keep = append(keep, f)
			continue
		}
		if m := refNumberedRE.FindStringSubmatch(w); m != nil {
			keep = append(keep, f)
			continue
		}
		c := searchCodeOf(w)
		if c == nil {
			keep = append(keep, f)
			continue
		}
		if !slices.ContainsFunc(codes, func(x searchCode) bool { return strings.EqualFold(x.Word, c.Word) }) {
			codes = append(codes, *c)
		}
		if i := strings.IndexByte(w, ':'); i > 0 {
			f = strings.Replace(f, w[:i+1], "", 1)
		}
		keep = append(keep, f)
	}
	return codes, strings.Join(keep, " ")
}

// numberedSearchRefs reads "page #60" style references out of a search,
// returning them canonical and the search without them.
func numberedSearchRefs(text string) (toks []string, rest string) {
	re := regexp.MustCompile(`(?i)\b(page|design note|design|decision|request|pull request|pr|pull) ?#(\d{1,9})\b`)
	rest = re.ReplaceAllStringFunc(text, func(m string) string {
		toks = append(toks, m)
		return ""
	})
	return toks, strings.Join(strings.Fields(rest), " ")
}

func searchCodeOf(w string) *searchCode {
	c := &searchCode{Word: w}
	if m := refRequestRE.FindStringSubmatch(w); m != nil {
		n := m[2]
		tok := "R-" + n
		if m[1] != "" {
			tok = m[1] + ":R-" + n
			if !strings.EqualFold(m[1], "me") {
				tok = strings.ToUpper(m[1]) + ":R-" + n
			}
		}
		c.Tokens = []string{tok}
		dash := strings.Contains(w, "-") || m[1] != ""
		if !dash {
			if pc, ok := ParsePageCode(w); ok {
				c.Tokens = append(c.Tokens, pc.String())
			}
		}
		c.Phrase = []string{`"r ` + n + `"`, `"r` + n + `"`}
		c.strict = dash
		return c
	}
	if dc, ok := ParseDesignCode(w); ok {
		n := strconv.FormatInt(dc.Number, 10)
		c.Tokens = []string{dc.String()}
		c.Phrase = []string{`"adr` + n + `"`, `"adr ` + n + `"`, `"adr ` + fmt.Sprintf("%04d", dc.Number) + `"`}
		c.strict = true
		return c
	}
	if pcs := pageCodeCandidates(w); len(pcs) > 0 {
		for _, pc := range pcs {
			c.Tokens = append(c.Tokens, pc.String())
		}
		n := strconv.FormatInt(pcs[0].Number, 10)
		p := strings.ToLower(pcs[0].Prefix)
		c.Phrase = []string{`"` + p + n + `"`, `"` + p + ` ` + n + `"`}
		c.strict = strings.Contains(w, "-")
		return c
	}
	if cands := keyCandidates(w); len(cands) > 0 {
		for _, k := range cands {
			c.Tokens = append(c.Tokens, k[0]+"-"+k[1])
			c.Phrase = append(c.Phrase, `"`+strings.ToLower(k[0])+` `+k[1]+`"`, `"`+strings.ToLower(k[0])+k[1]+`"`)
		}
		c.strict = strings.Contains(w, "-")
		return c
	}
	return nil
}

// exactRefs finds what a code names for the actor. A code with no project
// (R-28, PL3, ADR7) lists every readable thing with it, the one in the
// searcher's own project first.
func (s *Service) exactRefs(ctx context.Context, actor *Actor, c searchCode, sc refScope) []Ref {
	var out []Ref
	add := func(r *Ref) {
		if r != nil && !slices.ContainsFunc(out, func(x Ref) bool { return x.Kind == r.Kind && x.ID == r.ID }) {
			out = append(out, *r)
		}
	}
	for _, tok := range c.Tokens {
		if r := s.resolveRef(ctx, actor, tok, sc); r != nil {
			add(r)
		}
		if strings.Contains(tok, ":") || refIssueRE.MatchString(tok) {
			continue
		}
		switch {
		case refBareReqRE.MatchString(tok):
			n, _ := strconv.Atoi(tok[2:])
			for _, id := range s.requestIDsNumbered(ctx, actor, n) {
				add(s.requestRef(ctx, id))
			}
		default:
			if dc, ok := ParseDesignCode(tok); ok && dc.Project == "" {
				ids, _ := queryIDs(ctx, s.db.R, `SELECT DISTINCT note_id FROM design_codes WHERE number = ? AND note_id IS NOT NULL ORDER BY note_id`, dc.Number)
				for _, id := range ids {
					add(s.itemRef(ctx, actor, KDesign, id))
				}
			} else if pc, ok := ParsePageCode(tok); ok && pc.Project == "" {
				ids, _ := queryIDs(ctx, s.db.R, `SELECT DISTINCT page_id FROM page_codes WHERE prefix = ? AND number = ? AND page_id IS NOT NULL ORDER BY page_id`, pc.Prefix, pc.Number)
				for _, id := range ids {
					add(s.itemRef(ctx, actor, KPage, id))
				}
			}
		}
	}
	return out
}

// requestIDsNumbered lists the requests the actor may see with a number,
// newest first.
func (s *Service) requestIDsNumbered(ctx context.Context, actor *Actor, n int) []int64 {
	vis, vargs := requestVisible(actor, "r")
	ids, _ := queryIDs(ctx, s.db.R, `SELECT r.id FROM requests r WHERE r.number = ? AND `+vis+` ORDER BY r.id DESC LIMIT 10`, append([]any{n}, vargs...)...)
	return ids
}

// hitOf is a search hit for something a code names.
func (s *Service) hitOf(ctx context.Context, r Ref) SearchHit {
	h := SearchHit{Kind: r.Kind, ID: r.ID, Key: r.Key, Title: r.Title, ProjectKey: r.ProjectKey, State: r.State, Sub: r.sub, Match: MatchExact}
	var upd int64
	switch r.Kind {
	case KIssue:
		_ = s.db.R.QueryRowContext(ctx, `SELECT i.updated_at, st.name FROM issues i JOIN statuses st ON st.id = i.status_id WHERE i.id = ?`, r.ID).Scan(&upd, &h.Sub)
	case KRequest:
		_ = s.db.R.QueryRowContext(ctx, `SELECT updated_at FROM requests WHERE id = ?`, r.ID).Scan(&upd)
	case KPage:
		_ = s.db.R.QueryRowContext(ctx, `SELECT updated_at FROM pages WHERE id = ?`, r.ID).Scan(&upd)
	case KDesign:
		_ = s.db.R.QueryRowContext(ctx, `SELECT updated_at FROM design_notes WHERE id = ?`, r.ID).Scan(&upd)
	}
	if upd != 0 {
		h.UpdatedAt = msTime(upd)
	}
	return h
}

// Where a hit stands in a search that has codes in it.
const (
	MatchExact   = "exact"
	MatchMention = "mention"
	MatchText    = "text"
)

// bareRequestCode is the code a query consists of alone when it is a
// project-qualified request code (API:R-28, me:R-3), which would otherwise
// parse as a key:value filter.
func bareRequestCode(query string) *searchCode {
	w := strings.TrimSpace(query)
	if w == "" || strings.ContainsAny(w, " \t\n\"") {
		return nil
	}
	if m := refRequestRE.FindStringSubmatch(w); m == nil || m[1] == "" {
		return nil
	}
	return searchCodeOf(w)
}

// mentionExpr is the FTS expression for text writing any of the codes.
func mentionExpr(codes []searchCode) string {
	var parts []string
	for _, c := range codes {
		for _, p := range c.Phrase {
			if !slices.Contains(parts, p) {
				parts = append(parts, p)
			}
		}
	}
	return strings.Join(parts, " OR ")
}

// mergeTiers joins hit lists best first, each thing once, up to limit.
func mergeTiers(limit int, tiers ...[]SearchHit) []SearchHit {
	var out []SearchHit
	for _, tier := range tiers {
		for _, h := range tier {
			if slices.ContainsFunc(out, func(x SearchHit) bool { return x.Kind == h.Kind && x.ID == h.ID }) {
				continue
			}
			out = append(out, h)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func markHits(hs []SearchHit, how string) []SearchHit {
	for i := range hs {
		if hs[i].Match == "" {
			hs[i].Match = how
		}
	}
	return hs
}

// searchRequestsFor finds requests whose title, body, answer, note or asked
// questions have the words (or, for a ftsRaw text, write one of its
// phrases), the ones with the words in their title first.
func (s *Service) searchRequestsFor(ctx context.Context, actor *Actor, words string, projects []*Project, limit int) (SearchGroup, error) {
	var needles []string
	var find func(string) (int, int)
	if strings.HasPrefix(words, ftsRaw) {
		var res []*regexp.Regexp
		for _, m := range phraseRE.FindAllStringSubmatch(words[len(ftsRaw):], -1) {
			toks := strings.Fields(m[1])
			for i := range toks {
				toks[i] = regexp.QuoteMeta(toks[i])
			}
			res = append(res, regexp.MustCompile(`(?i)\b`+strings.Join(toks, `[\W_]*`)+`\b`))
			if len(needles) == 0 {
				needles = []string{toks[len(toks)-1]}
			}
		}
		find = func(t string) (int, int) {
			for _, re := range res {
				if loc := re.FindStringIndex(t); loc != nil {
					return loc[0], loc[1]
				}
			}
			return -1, -1
		}
		if len(needles) > 0 {
			needles = []string{regexp.MustCompile(`\d+`).FindString(needles[0])}
			if needles[0] == "" {
				return SearchGroup{}, nil
			}
		}
	} else {
		for _, w := range strings.Fields(words) {
			if !strings.HasPrefix(w, "-") {
				needles = append(needles, strings.Trim(w, `"`))
			}
		}
	}
	if len(needles) == 0 {
		return SearchGroup{}, nil
	}
	vis, args := requestVisible(actor, "r")
	where := []string{vis}
	for _, n := range needles {
		cols := make([]string, 0, 5)
		for _, c := range []string{"r.title", "r.body", "r.answer", "r.note", "r.payload"} {
			cols = append(cols, c+` LIKE ? ESCAPE '\'`)
			args = append(args, likeContains(n))
		}
		where = append(where, "("+strings.Join(cols, " OR ")+")")
	}
	if len(projects) > 0 {
		ids := make([]int64, len(projects))
		for i, p := range projects {
			ids[i] = p.ID
		}
		where, args = append(where, "r.project_id IN ("+placeholders(len(ids))+")"), append(args, int64Args(ids)...)
	}
	// #nosec G202 -- fixed conditions and ? placeholders
	rows, err := s.db.R.QueryContext(ctx, `SELECT r.id, r.number, p.key, r.title, r.body, r.answer, r.note, r.kind, r.state, r.updated_at, r.payload
		FROM requests r LEFT JOIN projects p ON p.id = r.project_id WHERE `+strings.Join(where, " AND ")+` ORDER BY r.id DESC LIMIT 300`, args...)
	if err != nil {
		return SearchGroup{}, err
	}
	defer rows.Close()
	type scored struct {
		hit  SearchHit
		rank int
	}
	var list []scored
	for rows.Next() {
		var id int64
		var number int
		var key sql.NullString
		var title, body, answer, note, kind, state, payload string
		var upd int64
		if err := rows.Scan(&id, &number, &key, &title, &body, &answer, &note, &kind, &state, &upd, &payload); err != nil {
			return SearchGroup{}, err
		}
		text := strings.Join([]string{title, body, answer, note, payload}, "\n")
		rank := 2
		if find != nil {
			has := func(t string) bool { i, _ := find(t); return i >= 0 }
			if !has(text) {
				continue
			}
			if has(title) {
				rank = 0
			} else if has(body) || has(answer) || has(note) {
				rank = 1
			}
		} else {
			low := strings.ToLower
			all := func(t string) bool {
				for _, n := range needles {
					if !strings.Contains(low(t), low(n)) {
						return false
					}
				}
				return true
			}
			if !all(text) {
				continue
			}
			if all(title) {
				rank = 0
			} else if all(body) || all(answer) || all(note) {
				rank = 1
			}
		}
		snip := requestSnippet(needles, find, title, body, answer, note)
		list = append(list, scored{SearchHit{Kind: KRequest, ID: id, Key: requestRef(key.String, number), Title: title, Snippet: snip, ProjectKey: key.String,
			State: state, Sub: kind, UpdatedAt: msTime(upd)}, rank})
	}
	if err := rows.Err(); err != nil {
		return SearchGroup{}, err
	}
	slices.SortStableFunc(list, func(a, b scored) int { return a.rank - b.rank })
	g := SearchGroup{Total: len(list)}
	for _, x := range list {
		if len(g.Hits) >= limit {
			break
		}
		g.Hits = append(g.Hits, x.hit)
	}
	return g, nil
}

// requestSnippet is a line around the first match in the request's text,
// with it marked. find places a code; nil means the needles are words.
func requestSnippet(needles []string, find func(string) (int, int), texts ...string) string {
	for _, t := range texts {
		start, end := -1, -1
		if find != nil {
			start, end = find(t)
		} else {
			for _, n := range needles {
				if i := strings.Index(strings.ToLower(t), strings.ToLower(n)); i >= 0 {
					start, end = i, i+len(n)
					break
				}
			}
		}
		if start < 0 {
			continue
		}
		from, to := max(0, start-60), min(len(t), end+80)
		for from > 0 && !isRuneStart(t[from]) {
			from--
		}
		for to < len(t) && !isRuneStart(t[to]) {
			to++
		}
		return cleanSnippet("…" + t[from:start] + MarkStart + t[start:end] + MarkEnd + t[end:to] + "…")
	}
	return ""
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
