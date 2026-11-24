package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Kinds of use.
const (
	UseLookup  = "lookup"
	UseMention = "mention"
	UseAvoided = "avoided"
)

// lookupRepeat: the same reader (a session, else the person and agent)
// reading the same item again this soon is the same use (an agent re-reading
// a term in a loop isn't using it more); the row counts the repeats.
const lookupRepeat = 10 * time.Minute

// knowledgeTables are the tables of the items whose use is recorded.
var knowledgeTables = map[string]string{KGlossary: "glossary", KPage: "pages", KDesign: "design_notes"}

// NoteLookup records that a tool read a glossary entry, page or design
// note. word is what it asked for, when that differs from the item's name
// (a word to avoid that led to the entry). It never fails the read.
func (s *Service) NoteLookup(ctx context.Context, actor *Actor, typ string, id int64, word string) {
	table := knowledgeTables[typ]
	if actor == nil || actor.User == nil || actor.User.ID == 0 || actor.session != "" || table == "" || id == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	now := s.nowMS()
	agent := actor.agentName()
	sessID, sessTask := sessionStamp(ctx, s.db.R, actor)
	who, whoArg := `user_id = ? AND agent = ? AND session_id IS NULL`, []any{actor.User.ID, agent}
	if sessID != nil {
		who, whoArg = `session_id = ?`, []any{sessID}
	}
	args := append([]any{typ, id}, whoArg...)
	args = append(args, now-lookupRepeat.Milliseconds())
	for range 3 {
		// #nosec G202 -- who is one of two fixed clauses; values are bound
		res, err := s.db.W.ExecContext(ctx, `UPDATE knowledge_uses SET repeats = coalesce(repeats, 0) + 1 WHERE id = (
			SELECT max(id) FROM knowledge_uses WHERE item_type = ? AND item_id = ? AND kind = 'lookup' AND `+who+` AND created_at > ?)`, args...)
		if err != nil {
			s.log.WarnContext(ctx, "count repeated knowledge lookup", "type", typ, "id", id, "err", err)
			return
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return
		}
		// #nosec G202 -- table and who are fixed; values are bound
		res, err = s.db.W.ExecContext(ctx, `INSERT INTO knowledge_uses(item_type, item_id, kind, word, project_id, user_id, agent, created_at, session_id, session_issue_id, repeats)
			SELECT ?, ?, 'lookup', ?, (SELECT project_id FROM `+table+` WHERE id = ?), ?, ?, ?, ?, ?, 0
			WHERE NOT EXISTS (SELECT 1 FROM knowledge_uses WHERE item_type = ? AND item_id = ? AND kind = 'lookup' AND `+who+` AND created_at > ?)`,
			append([]any{typ, id, excerpt(word, 100), id, actor.User.ID, agent, now, sessID, sessTask}, args...)...)
		if err != nil {
			s.log.WarnContext(ctx, "record knowledge lookup", "type", typ, "id", id, "err", err)
			return
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return
		}
	}
}

// ShownRef names a shown item: Type glossary (Ref a term), page or design
// (Ref its code); Project is the scope a term or page code resolves in.
type ShownRef struct {
	Type    string `json:"type"`
	Ref     string `json:"ref"`
	Project string `json:"project,omitempty"`
}

// NoteShown records that items were put in front of an agent without its
// asking: glossary entries, pages and design notes `wp brief` listed, or a
// task's prep named. One row per showing and no dedupe, against the lookups
// in knowledge_uses: "shown but never read" needs both counts (WP-ADR145
// point 7). refs are what the client printed; ones that name nothing are
// counted in the result, not dropped silently.
func (s *Service) NoteShown(ctx context.Context, actor *Actor, via string, refs []ShownRef) (recorded, unknown int, err error) {
	if actor == nil || actor.User == nil || actor.User.ID == 0 || actor.session != "" || actor.ReadOnly || !actor.IsAgent() {
		return 0, 0, nil
	}
	if via != "brief" && via != "prep" {
		return 0, 0, invalid("via is brief or prep")
	}
	if len(refs) > 50 {
		return 0, 0, invalid("at most 50 items at a time")
	}
	ctx = context.WithoutCancel(ctx)
	sessID, sessTask := sessionStamp(ctx, s.db.R, actor)
	now := s.nowMS()
	for _, r := range refs {
		typ, id, ok := s.shownItem(ctx, actor, r)
		if !ok {
			unknown++
			continue
		}
		table := knowledgeTables[typ]
		// #nosec G202 -- table is one of three fixed names; values are bound
		if _, err := s.db.W.ExecContext(ctx, `INSERT INTO knowledge_shown(item_type, item_id, via, session_id, session_issue_id, project_id, user_id, agent, created_at)
			SELECT ?, ?, ?, ?, ?, (SELECT project_id FROM `+table+` WHERE id = ?), ?, ?, ?`,
			typ, id, via, sessID, sessTask, id, actor.User.ID, actor.agentName(), now); err != nil {
			return recorded, unknown, fmt.Errorf("record shown %s %d: %w", typ, id, err)
		}
		recorded++
	}
	return recorded, unknown, nil
}

// mentionSource is text that was written: where it lives and who wrote it.
type mentionSource struct {
	typ     string
	id      int64
	project *int64
	text    string
	user    int64
	agent   string
	at      int64
}

// codeMentionRE finds page codes written with their project, as code
// comments and commit messages cite decisions: WP-D11, APP-PL13.
var codeMentionRE = lazyre.New(`\b[A-Z][A-Z0-9]{1,9}-[A-Z]{1,2}\d{1,9}\b`)

// unscannedRE is text that names things without using the words: code,
// links and URLs.
var unscannedRE = lazyre.New("(?s)```.*?```|`[^`\n]*`|\\[\\[[^\\]\n]*\\]\\]|\\]\\([^)\n]*\\)|https?://\\S+")

type useKey struct {
	typ  string
	id   int64
	kind string
}

// recordMentions records what a text names. Saving the text never fails
// over it: problems are logged.
func (s *Service) recordMentions(ctx context.Context, t *tx, src mentionSource) {
	if strings.TrimSpace(src.text) == "" || (src.typ == KPage && src.project == nil) {
		return
	}
	found := map[useKey]string{}
	add := func(typ string, id int64, kind, word string) {
		if typ == src.typ && id == src.id {
			return
		}
		k := useKey{typ, id, kind}
		if _, ok := found[k]; !ok {
			found[k] = word
		}
	}
	for _, target := range Wikilinks(src.text) {
		if typ, id, ok := s.resolveWikilink(ctx, t, target, src.project); ok && knowledgeTables[typ] != "" && s.itemExists(ctx, t, typ, id) {
			add(typ, id, UseMention, "")
		}
	}
	seen := map[string]bool{}
	for _, m := range codeMentionRE.FindAllString(src.text, 100) {
		if seen[m] {
			continue
		}
		seen[m] = true
		if c, ok := ParsePageCode(m); ok && c.Project != "" {
			if id, ok := s.pageByCodeIn(ctx, t, c, src.project); ok {
				add(KPage, id, UseMention, c.String())
			}
		}
	}
	if src.typ != "pr" {
		idx, err := s.glossaryWords(ctx, t)
		if err != nil {
			s.log.WarnContext(ctx, "read glossary words", "err", err)
		} else {
			var chain []int64
			if src.project != nil {
				var rerr error
				chain, rerr = s.projectChainIDs(ctx, t, *src.project)
				if rerr != nil {
					if readFault(rerr) {
						s.log.WarnContext(ctx, "project chain ids failed", "err", rerr)
					}
				}
			}
			for _, h := range idx.find(unscannedRE.ReplaceAllString(src.text, " "), chain, s.senseGate(idx)) {
				if h.kind == UseAvoided && src.typ == KGlossary {
					continue
				}
				add(KGlossary, h.entry, h.kind, h.word)
			}
		}
	}
	if len(found) == 0 {
		return
	}
	at := src.at
	if at == 0 {
		at = s.nowMS()
	}
	var user any
	if src.user != 0 {
		user = src.user
	}
	sessID, sessTask := t.creditSession(ctx, src.agent)
	for k, word := range found {
		if _, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO knowledge_uses(item_type, item_id, kind, src_type, src_id, word, project_id, user_id, agent, created_at, session_id, session_issue_id)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, k.typ, k.id, k.kind, src.typ, src.id, word, nullInt(src.project), user, src.agent, at, sessID, sessTask); err != nil {
			s.log.WarnContext(ctx, "record knowledge mention", "source", src.typ, "id", src.id, "err", err)
			return
		}
	}
}

// itemExists: [[page:12]] resolves without looking, and a use must not
// wait for an item that has no row yet.
func (s *Service) itemExists(ctx context.Context, q queryer, typ string, id int64) bool {
	var n int
	// #nosec G202 -- the table name comes from knowledgeTables
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM `+knowledgeTables[typ]+` WHERE id = ?`, id).Scan(&n)
	if err != nil {
		if readFault(err) {
			s.log.WarnContext(ctx, "read item exists", "id", id, "err", err)
		}
		return false
	}
	return n > 0
}

// recordPRMentions records the page codes a pull request's title and
// description cite, in each project its repo is attached to.
func (s *Service) recordPRMentions(ctx context.Context, t *tx, forgeID int64, repo string, prID int64, text string) {
	if !codeMentionRE.MatchString(text) {
		return
	}
	projects, err := queryIDs(ctx, t, `SELECT DISTINCT project_id FROM project_repos WHERE forge_id = ? AND repo = ? COLLATE NOCASE`, forgeID, repo)
	if err != nil {
		s.log.WarnContext(ctx, "pull request mentions", "err", err)
		return
	}
	for _, pid := range projects {
		s.recordMentions(ctx, t, mentionSource{typ: "pr", id: prID, project: &pid, text: text})
	}
}

// termWord is a term, alias or word to avoid, as the words it's made of.
type termWord struct {
	tokens  []string
	entry   int64
	project int64
	word    string
	kind    string
}

// termIndex finds every glossary word in text in one pass: words by their
// first token.
type termIndex struct {
	key   string
	first map[string][]*termWord
	sense map[int64]map[string]int

	senseMu  sync.Mutex
	senses   map[int64][]float64
	senseGen int
}

// termsCache keeps the index until the glossary changes.
type termsCache struct {
	mu  sync.Mutex
	idx *termIndex
}

// wordTokens splits text into lower-case words (letters and digits).
func wordTokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// glossaryWords is every entry's term, aliases and words to avoid, indexed.
func (s *Service) glossaryWords(ctx context.Context, q queryer) (*termIndex, error) {
	var n, updated, ids, size int64
	if err := q.QueryRowContext(ctx, `SELECT count(*), coalesce(max(updated_at), 0), coalesce(sum(id), 0),
		coalesce(sum(length(term) + length(aliases) + length(avoid) + length(definition)), 0) FROM glossary`).Scan(&n, &updated, &ids, &size); err != nil {
		return nil, err
	}
	key := fmt.Sprint(n, updated, ids, size)
	s.terms.mu.Lock()
	idx := s.terms.idx
	s.terms.mu.Unlock()
	if idx != nil && idx.key == key {
		return idx, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT id, coalesce(project_id, 0), term, aliases, avoid, definition FROM glossary`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	idx = &termIndex{key: key, first: map[string][]*termWord{}, sense: map[int64]map[string]int{}}
	put := func(w *termWord) {
		w.tokens = wordTokens(w.word)
		if len(w.tokens) == 0 || len(strings.Join(w.tokens, "")) < 2 {
			return
		}
		idx.first[w.tokens[0]] = append(idx.first[w.tokens[0]], w)
	}
	for rows.Next() {
		var id, pid int64
		var term, aliases, avoid, def string
		if err := rows.Scan(&id, &pid, &term, &aliases, &avoid, &def); err != nil {
			return nil, err
		}
		idx.sense[id] = senseCounts(term, aliases, def)
		put(&termWord{entry: id, project: pid, word: term, kind: UseMention})
		for _, a := range jsonStrings(aliases) {
			put(&termWord{entry: id, project: pid, word: a, kind: UseMention})
		}
		for _, a := range jsonStrings(avoid) {
			put(&termWord{entry: id, project: pid, word: a, kind: UseAvoided})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.terms.mu.Lock()
	s.terms.idx = idx
	s.terms.mu.Unlock()
	return idx, nil
}

// termHit is a glossary word found in text.
type termHit struct {
	entry int64
	word  string
	kind  string
}

// find lists the glossary words in text that apply in a scope chain (a
// project and its ancestors, nearest first; global entries always apply).
// Words match whole and in any case, and a last word may take a plural s;
// where several entries define the same word, the nearest one is meant.
// A use keep refuses (not in the entry's sense) doesn't count; words to
// avoid always do.
func (idx *termIndex) find(text string, chain []int64, keep senseGate) []termHit {
	rank := func(project int64) int {
		if project == 0 {
			return len(chain)
		}
		if n := slices.Index(chain, project); n >= 0 {
			return n
		}
		return -1
	}
	type pick struct {
		w    *termWord
		rank int
	}
	best := map[string]pick{}
	toks := wordTokens(text)
	for i, tok := range toks {
		cands := []string{tok}
		if len(tok) > 3 {
			if t := strings.TrimSuffix(tok, "s"); t != tok {
				cands = append(cands, t)
			}
			if t := strings.TrimSuffix(tok, "es"); t != tok {
				cands = append(cands, t)
			}
		}
		for _, c := range cands {
			for _, w := range idx.first[c] {
				r := rank(w.project)
				if r < 0 || !matchAt(toks, i, w.tokens) {
					continue
				}
				if keep != nil && w.kind == UseMention && !keep(toks, i, len(w.tokens), w.entry) {
					continue
				}
				k := w.kind + "\x00" + strings.Join(w.tokens, " ")
				if b, ok := best[k]; !ok || r < b.rank {
					best[k] = pick{w, r}
				}
			}
		}
	}
	out := make([]termHit, 0, len(best))
	for _, b := range best {
		out = append(out, termHit{entry: b.w.entry, word: b.w.word, kind: b.w.kind})
	}
	slices.SortFunc(out, func(a, b termHit) int { return strings.Compare(a.word, b.word) })
	return out
}

// matchAt says whether want's tokens start at toks[i] (the last may be plural).
func matchAt(toks []string, i int, want []string) bool {
	if i+len(want) > len(toks) {
		return false
	}
	for j, w := range want {
		tok := toks[i+j]
		if tok == w {
			continue
		}
		if j == len(want)-1 && len(w) > 2 && (tok == w+"s" || tok == w+"es") {
			continue
		}
		return false
	}
	return true
}

// BackfillKnowledgeUses records the mentions in text written before use
// was recorded, dated when the text was written. It runs once, in the
// background at start, a few hundred items per transaction.
func (s *Service) BackfillKnowledgeUses(ctx context.Context) {
	var done int
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM server_settings WHERE key = 'knowledge_uses_v1'`).Scan(&done); err != nil || done > 0 {
		return
	}
	sources := []struct{ typ, query string }{
		{KIssue, `SELECT id, project_id, title || char(10) || description, created_at, coalesce(text_by, reporter_id, 0), text_agent
			FROM issues WHERE id > ? ORDER BY id LIMIT 200`},
		{"comment", `SELECT c.id, i.project_id, c.body, c.created_at, coalesce(c.author_id, 0), c.agent
			FROM comments c JOIN issues i ON i.id = c.issue_id WHERE c.id > ? ORDER BY c.id LIMIT 200`},
		{KPage, `SELECT id, project_id, title || char(10) || body, updated_at, coalesce(text_by, owner_id, 0), text_agent
			FROM pages WHERE id > ? ORDER BY id LIMIT 200`},
		{KDesign, `SELECT n.id, n.project_id, n.title || char(10) || n.body || char(10) || coalesce((SELECT group_concat(r.body, char(10))
				FROM design_revisions r WHERE r.note_id = n.id AND r.kind = 'addition' AND r.state IN ('approved', 'applied')), ''),
			n.updated_at, coalesce(n.updated_by, 0), n.updated_agent FROM design_notes n WHERE n.id > ? ORDER BY n.id LIMIT 200`},
		{KGlossary, `SELECT id, project_id, definition, updated_at, coalesce(updated_by, 0), updated_agent
			FROM glossary WHERE id > ? ORDER BY id LIMIT 200`},
	}
	for _, src := range sources {
		var after int64
		for {
			if ctx.Err() != nil {
				return
			}
			batch, err := s.mentionBatch(ctx, src.typ, src.query, after)
			if err != nil {
				s.log.WarnContext(ctx, "backfill knowledge use", "source", src.typ, "err", err)
				return
			}
			if len(batch) == 0 {
				break
			}
			after = batch[len(batch)-1].id
			if err := s.write(ctx, SystemActor(), func(t *tx) error {
				for _, m := range batch {
					s.recordMentions(ctx, t, m)
				}
				return nil
			}); err != nil {
				s.log.WarnContext(ctx, "backfill knowledge use", "source", src.typ, "err", err)
				return
			}
		}
	}
	if _, err := s.db.W.ExecContext(ctx, `INSERT OR IGNORE INTO server_settings(key, value) VALUES ('knowledge_uses_v1', '1')`); err != nil {
		s.log.WarnContext(ctx, "backfill knowledge use", "err", err)
	}
}

func (s *Service) mentionBatch(ctx context.Context, typ, query string, after int64) ([]mentionSource, error) {
	rows, err := s.db.R.QueryContext(ctx, query, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []mentionSource
	for rows.Next() {
		m := mentionSource{typ: typ}
		var pid sql.NullInt64
		if err := rows.Scan(&m.id, &pid, &m.text, &m.at, &m.user, &m.agent); err != nil {
			return nil, err
		}
		m.project = ptrInt(pid)
		out = append(out, m)
	}
	return out, rows.Err()
}

// KnowledgeUse is one use of an item.
type KnowledgeUse struct {
	Kind  string    `json:"kind"` // lookup, mention, avoided
	At    time.Time `json:"at"`
	User  *UserRef  `json:"user,omitempty"`
	Agent string    `json:"agent,omitempty"`
	// Word is what named it: the term or alias, the word to avoid, the code.
	Word string `json:"word,omitempty"`
	// Source is where the text is (nil for lookups); Comment says the text
	// was a comment on that issue.
	Source  *KItem `json:"source,omitempty"`
	Comment bool   `json:"comment,omitempty"`
	// Item is the entry, page or note used (in overviews).
	Item *KItem `json:"item,omitempty"`
}

// UseCounts sums an item's use.
type UseCounts struct {
	Lookups  int `json:"lookups"`
	Mentions int `json:"mentions"` // distinct places that name it
	Avoided  int `json:"avoided"`  // places a word to avoid was written instead
	People   int `json:"people"`
	Agents   int `json:"agents"`
	// Shown is how many times it was put in front of an agent unasked (wp
	// brief, a prep); Rereads the extra lookups the 10-minute window folded
	// into a lookup row (rows from before it are unknown, left out).
	Shown   int `json:"shown"`
	Rereads int `json:"rereads"`
}

// ItemUsage is how an item has been used.
type ItemUsage struct {
	Counts     UseCounts      `json:"counts"`
	LastUsedAt *time.Time     `json:"last_used_at"`
	Uses       []KnowledgeUse `json:"uses"` // latest first, where the actor can see the source
}

// useRow is a knowledge_uses row.
type useRow struct {
	id, itemID     int64
	itemType, kind string
	srcType        string
	srcID          int64
	word, agent    string
	projectID      int64
	userID         int64
	at             int64
}

const useCols = `id, item_type, item_id, kind, src_type, coalesce(src_id, 0), word, coalesce(project_id, 0), coalesce(user_id, 0), agent, created_at`

func scanUses(rows *sql.Rows) ([]useRow, error) {
	defer rows.Close()
	var out []useRow
	for rows.Next() {
		var u useRow
		if err := rows.Scan(&u.id, &u.itemType, &u.itemID, &u.kind, &u.srcType, &u.srcID, &u.word, &u.projectID, &u.userID, &u.agent, &u.at); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// usesVisible limits uses to where the actor can see them happen and to
// items the actor can read, so counts never reveal an item in a project
// they're not in (or that a code names one there).
func usesVisible(actor *Actor) (string, []any) {
	vis, args := visibleSQL("project_id", actor)
	xvis, xargs := visibleSQL("x.project_id", actor)
	cond := vis + ` AND (
		(item_type = 'glossary' AND EXISTS (SELECT 1 FROM glossary x WHERE x.id = item_id AND ` + xvis + `))
		OR (item_type = 'design' AND EXISTS (SELECT 1 FROM design_notes x WHERE x.id = item_id AND ` + xvis + `))
		OR (item_type = 'page' AND EXISTS (SELECT 1 FROM pages x WHERE x.id = item_id
			AND ((x.project_id IS NULL AND x.owner_id = ?) OR (x.project_id IS NOT NULL AND ` + xvis + `)))))`
	args = append(args, xargs...)
	args = append(args, xargs...)
	args = append(args, actor.id())
	return cond, append(args, xargs...)
}

// ItemUsage reports how a glossary entry, page or design note was used.
func (s *Service) ItemUsage(ctx context.Context, actor *Actor, typ string, id int64) (*ItemUsage, error) {
	if knowledgeTables[typ] == "" {
		return nil, invalid("usage is kept for glossary entries, pages and design notes")
	}
	if _, err := s.kitem(ctx, actor, typ, id, ""); err != nil {
		return nil, err
	}
	vis, vargs := usesVisible(actor)
	out := &ItemUsage{Uses: []KnowledgeUse{}}
	var last sql.NullInt64
	// #nosec G202 -- vis is a fixed condition; values are bound
	if err := s.db.R.QueryRowContext(ctx, `SELECT
			coalesce(sum(kind = 'lookup'), 0),
			count(DISTINCT CASE WHEN kind = 'mention' THEN src_type || ':' || src_id END),
			count(DISTINCT CASE WHEN kind = 'avoided' THEN src_type || ':' || src_id END),
			count(DISTINCT CASE WHEN agent = '' THEN user_id END),
			count(DISTINCT nullif(agent, '')),
			max(created_at),
			coalesce(sum(CASE WHEN kind = 'lookup' THEN repeats END), 0)
		FROM knowledge_uses WHERE item_type = ? AND item_id = ? AND `+vis, append([]any{typ, id}, vargs...)...).
		Scan(&out.Counts.Lookups, &out.Counts.Mentions, &out.Counts.Avoided, &out.Counts.People, &out.Counts.Agents, &last, &out.Counts.Rereads); err != nil {
		return nil, err
	}
	// #nosec G202 -- vis is a fixed condition; values are bound
	if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM knowledge_shown WHERE item_type = ? AND item_id = ? AND `+vis,
		append([]any{typ, id}, vargs...)...).Scan(&out.Counts.Shown); err != nil {
		return nil, err
	}
	if last.Valid {
		t := msTime(last.Int64)
		out.LastUsedAt = &t
	}
	// #nosec G202 -- vis is a fixed condition; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+useCols+` FROM knowledge_uses WHERE item_type = ? AND item_id = ? AND `+vis+`
		ORDER BY created_at DESC, id DESC LIMIT 200`, append([]any{typ, id}, vargs...)...)
	if err != nil {
		return nil, err
	}
	list, err := scanUses(rows)
	if err != nil {
		return nil, err
	}
	r := s.newUseReader(ctx, actor)
	seen := map[string]bool{}
	for _, u := range list {
		if len(out.Uses) == 50 {
			break
		}
		if u.srcType != "" {
			k := u.kind + ":" + u.srcType + ":" + strconv.FormatInt(u.srcID, 10)
			if seen[k] {
				continue
			}
			seen[k] = true
		}
		ku, ok := r.use(u)
		if ok {
			out.Uses = append(out.Uses, ku)
		}
	}
	return out, nil
}

// useReader turns rows into uses the actor may see, caching what it looks up.
type useReader struct {
	s       *Service
	ctx     context.Context
	actor   *Actor
	users   map[int64]*UserRef
	items   map[string]*KItem
	issueOf map[int64]int64
}

func (s *Service) newUseReader(ctx context.Context, actor *Actor) *useReader {
	return &useReader{s: s, ctx: ctx, actor: actor, users: map[int64]*UserRef{}, items: map[string]*KItem{}, issueOf: map[int64]int64{}}
}

// item is an item the actor can see, or nil.
func (r *useReader) item(typ string, id int64) *KItem {
	k := typ + ":" + strconv.FormatInt(id, 10)
	if it, ok := r.items[k]; ok {
		return it
	}
	var it *KItem
	if typ == "pr" {
		it = r.s.prItem(r.ctx, r.actor, id)
	} else if x, err := r.s.kitem(r.ctx, r.actor, typ, id, ""); err == nil {
		it = x
	}
	r.items[k] = it
	return it
}

// use is a row as a use, false if the actor can't see its source.
func (r *useReader) use(u useRow) (KnowledgeUse, bool) {
	ku := KnowledgeUse{Kind: u.kind, At: msTime(u.at), Agent: u.agent, Word: u.word}
	if u.userID != 0 {
		if _, ok := r.users[u.userID]; !ok {
			r.users[u.userID] = r.s.userRefByID(r.ctx, r.s.db.R, sql.NullInt64{Int64: u.userID, Valid: true})
		}
		ku.User = r.users[u.userID]
	}
	switch u.srcType {
	case "":
	case "comment":
		issue, ok := r.issueOf[u.srcID]
		if !ok {
			_ = r.s.db.R.QueryRowContext(r.ctx, `SELECT issue_id FROM comments WHERE id = ?`, u.srcID).Scan(&issue)
			r.issueOf[u.srcID] = issue
		}
		if ku.Source = r.item(KIssue, issue); ku.Source == nil {
			return ku, false
		}
		ku.Comment = true
	default:
		if ku.Source = r.item(u.srcType, u.srcID); ku.Source == nil {
			return ku, false
		}
	}
	return ku, true
}

// prItem is a recorded pull request as a link item, if one of the actor's
// projects has its repo.
func (s *Service) prItem(ctx context.Context, actor *Actor, id int64) *KItem {
	var repo, title, url string
	var number int64
	var visible bool
	member, margs := actor.memberProjects(RoleViewer)
	// #nosec G202 -- member is placeholders; values are bound
	err := s.db.R.QueryRowContext(ctx, `SELECT pr.repo, pr.number, pr.title, pr.url, EXISTS (SELECT 1 FROM project_repos r
			WHERE r.forge_id = pr.forge_id AND r.repo = pr.repo COLLATE NOCASE
			AND (? OR r.project_id IN `+member+`))
		FROM pull_requests pr WHERE pr.id = ?`, append(append([]any{actor.isAdmin()}, margs...), id)...).Scan(&repo, &number, &title, &url, &visible)
	if err != nil || !visible {
		return nil
	}
	return &KItem{Type: KURL, Kind: "pull", Ref: url, Title: title, Repo: repo, Number: &number}
}

// KnowledgeQuery narrows the overview.
type KnowledgeQuery struct {
	Agent   string // an agent spec: only these agents' use
	Project string // a project key: use in it
	Days    int
}

// UsedItem is an item and how much it was used in the period.
type UsedItem struct {
	Item       KItem     `json:"item"`
	Lookups    int       `json:"lookups"`
	Mentions   int       `json:"mentions"`
	Avoided    int       `json:"avoided"`
	LastUsedAt time.Time `json:"last_used_at"`
}

// AvoidedWord is a word to avoid that was written, and where last.
type AvoidedWord struct {
	Entry  KItem        `json:"entry"`
	Word   string       `json:"word"`
	Count  int          `json:"count"`
	Latest KnowledgeUse `json:"latest"`
}

// UnusedItem is an item nobody used in the period.
type UnusedItem struct {
	Item       KItem      `json:"item"`
	LastUsedAt *time.Time `json:"last_used_at"` // ever; nil if never
}

// KnowledgeOverview is how the knowledge was used in a period.
type KnowledgeOverview struct {
	Totals struct {
		Lookups   int `json:"lookups"`
		Mentions  int `json:"mentions"`
		Avoided   int `json:"avoided"`
		ItemsUsed int `json:"items_used"`
	} `json:"totals"`
	Top         []UsedItem     `json:"top"`
	Avoided     []AvoidedWord  `json:"avoided"`
	Lookups     []KnowledgeUse `json:"lookups"` // what agents read lately
	Unused      []UnusedItem   `json:"unused"`
	UnusedTotal int            `json:"unused_total"`
}

// Overview limits.
const (
	overviewRows   = 50_000
	overviewTop    = 12
	overviewList   = 15
	overviewUnused = 20
)

// KnowledgeOverview reports which glossary entries, pages and design notes
// were used in a period, by whom, and which words to avoid were written.
func (s *Service) KnowledgeOverview(ctx context.Context, actor *Actor, q KnowledgeQuery) (*KnowledgeOverview, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	if q.Days <= 0 || q.Days > 365 {
		q.Days = 30
	}
	since := s.now().Add(-time.Duration(q.Days) * 24 * time.Hour).UnixMilli()
	vis, args := usesVisible(actor)
	where := `created_at >= ? AND ` + vis
	args = append([]any{since}, args...)
	var projectID int64
	if q.Project != "" {
		p, err := s.projectByRef(ctx, s.db.R, q.Project)
		if err != nil {
			return nil, err
		}
		if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
			return nil, err
		}
		projectID = p.ID
		where += ` AND project_id = ?`
		args = append(args, p.ID)
	}
	// #nosec G202 -- where is built from fixed conditions; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+useCols+` FROM knowledge_uses WHERE `+where+`
		ORDER BY created_at DESC, id DESC LIMIT ?`, append(args, overviewRows)...)
	if err != nil {
		return nil, err
	}
	all, err := scanUses(rows)
	if err != nil {
		return nil, err
	}
	spec := strings.TrimSpace(q.Agent)
	list := all[:0]
	for _, u := range all {
		if spec == "" || (u.agent != "" && AgentMatches(spec, u.agent)) {
			list = append(list, u)
		}
	}
	out := &KnowledgeOverview{Top: []UsedItem{}, Avoided: []AvoidedWord{}, Lookups: []KnowledgeUse{}, Unused: []UnusedItem{}}
	r := s.newUseReader(ctx, actor)

	type tally struct {
		typ      string
		id       int64
		lookups  int
		sources  map[string]bool
		avoided  int
		lastUsed int64
	}
	byItem := map[string]*tally{}
	var order []string
	type avoidKey struct {
		entry int64
		word  string
	}
	avoided := map[avoidKey][]useRow{}
	var avoidOrder []avoidKey
	for _, u := range list {
		k := u.itemType + ":" + strconv.FormatInt(u.itemID, 10)
		tl := byItem[k]
		if tl == nil {
			tl = &tally{typ: u.itemType, id: u.itemID, sources: map[string]bool{}, lastUsed: u.at}
			byItem[k] = tl
			order = append(order, k)
		}
		switch u.kind {
		case UseLookup:
			tl.lookups++
			out.Totals.Lookups++
		case UseMention:
			src := u.srcType + ":" + strconv.FormatInt(u.srcID, 10)
			if !tl.sources[src] {
				tl.sources[src] = true
				out.Totals.Mentions++
			}
		case UseAvoided:
			tl.avoided++
			out.Totals.Avoided++
			ak := avoidKey{u.itemID, strings.ToLower(u.word)}
			if _, ok := avoided[ak]; !ok {
				avoidOrder = append(avoidOrder, ak)
			}
			avoided[ak] = append(avoided[ak], u)
		}
	}
	out.Totals.ItemsUsed = len(byItem)

	slices.SortStableFunc(order, func(a, b string) int {
		ta, tb := byItem[a], byItem[b]
		return (tb.lookups + len(tb.sources) + tb.avoided) - (ta.lookups + len(ta.sources) + ta.avoided)
	})
	for _, k := range order {
		if len(out.Top) == overviewTop {
			break
		}
		tl := byItem[k]
		it := r.item(tl.typ, tl.id)
		if it == nil {
			continue
		}
		out.Top = append(out.Top, UsedItem{Item: *it, Lookups: tl.lookups, Mentions: len(tl.sources), Avoided: tl.avoided, LastUsedAt: msTime(tl.lastUsed)})
	}

	for _, ak := range avoidOrder {
		if len(out.Avoided) == overviewList {
			break
		}
		uses := avoided[ak]
		entry := r.item(KGlossary, ak.entry)
		if entry == nil {
			continue
		}
		aw := AvoidedWord{Entry: *entry, Word: uses[0].word}
		for _, u := range uses {
			ku, ok := r.use(u)
			if !ok {
				continue
			}
			if aw.Count == 0 {
				aw.Latest = ku
			}
			aw.Count++
		}
		if aw.Count > 0 {
			out.Avoided = append(out.Avoided, aw)
		}
	}

	for _, u := range list {
		if len(out.Lookups) == overviewList {
			break
		}
		if u.kind != UseLookup || u.agent == "" {
			continue
		}
		it := r.item(u.itemType, u.itemID)
		if it == nil {
			continue
		}
		ku, _ := r.use(u)
		ku.Item = it
		out.Lookups = append(out.Lookups, ku)
	}

	if err := s.unusedKnowledge(ctx, actor, r, out, projectID, since); err != nil {
		return nil, err
	}
	return out, nil
}

// unusedKnowledge fills the items nobody used in the period: glossary
// entries, categorised project pages and active design notes older than
// the period, the never-used first.
func (s *Service) unusedKnowledge(ctx context.Context, actor *Actor, r *useReader, out *KnowledgeOverview, projectID, since int64) error {
	where := map[string]string{}
	var args []any
	if projectID != 0 {
		for _, t := range []string{KGlossary, KPage, KDesign} {
			where[t] = "x.project_id = ?"
		}
		args = []any{projectID, projectID, projectID}
	} else {
		vis, vargs := visibleSQL("x.project_id", actor)
		where[KGlossary], where[KDesign] = vis, vis
		where[KPage] = "x.project_id IS NOT NULL AND " + vis
		args = append(append(append(args, vargs...), vargs...), vargs...)
	}
	// #nosec G202 -- the conditions are fixed strings; values are bound
	rows, err := s.db.R.QueryContext(ctx, `WITH last AS (SELECT item_type, item_id, max(created_at) AS at FROM knowledge_uses GROUP BY item_type, item_id),
		items AS (
			SELECT 'glossary' AS typ, x.id, x.created_at FROM glossary x WHERE `+where[KGlossary]+`
			UNION ALL SELECT 'page', x.id, x.created_at FROM pages x WHERE x.category != '' AND `+where[KPage]+`
			UNION ALL SELECT 'design', x.id, x.created_at FROM design_notes x WHERE x.status = 'active' AND `+where[KDesign]+`)
		SELECT i.typ, i.id, l.at FROM items i LEFT JOIN last l ON l.item_type = i.typ AND l.item_id = i.id
		WHERE i.created_at < ? AND coalesce(l.at, 0) < ?
		ORDER BY l.at IS NOT NULL, l.at, i.created_at`, append(args, since, since)...)
	if err != nil {
		return err
	}
	type cand struct {
		typ  string
		id   int64
		last sql.NullInt64
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.typ, &c.id, &c.last); err != nil {
			rows.Close()
			return err
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	out.UnusedTotal = len(cands)
	for _, c := range cands {
		if len(out.Unused) == overviewUnused {
			break
		}
		it := r.item(c.typ, c.id)
		if it == nil {
			out.UnusedTotal--
			continue
		}
		u := UnusedItem{Item: *it}
		if c.last.Valid {
			t := msTime(c.last.Int64)
			u.LastUsedAt = &t
		}
		out.Unused = append(out.Unused, u)
	}
	return nil
}

// jsonStrings reads a JSON array of strings ([] for anything else).
func jsonStrings(v string) []string {
	var out []string
	_ = json.Unmarshal([]byte(v), &out)
	return out
}

// recordTextMentions records what a page's, note's or entry's text names,
// credited to whoever wrote the text as it stands.
func (s *Service) recordTextMentions(ctx context.Context, t *tx, typ string, id int64, project *int64, text string) {
	by, agent := s.textAuthor(ctx, t, typ, id)
	user, _ := by.(int64)
	s.recordMentions(ctx, t, mentionSource{typ: typ, id: id, project: project, text: text, user: user, agent: agent})
}

// glossaryUsesDays is the period a glossary list's use counts cover.
const glossaryUsesDays = 90

// fillGlossaryUses sets each entry's recent use count and when it was
// last used, from the uses the actor can see.
func (s *Service) fillGlossaryUses(ctx context.Context, actor *Actor, list []*GlossaryEntry) error {
	if len(list) == 0 {
		return nil
	}
	ids := make([]string, len(list))
	byID := make(map[int64]*GlossaryEntry, len(list))
	for n, g := range list {
		ids[n] = strconv.FormatInt(g.ID, 10)
		byID[g.ID] = g
	}
	vis, args := usesVisible(actor)
	since := s.now().Add(-glossaryUsesDays * 24 * time.Hour).UnixMilli()
	// #nosec G202 -- ids are integers and vis a fixed condition; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT item_id,
			count(DISTINCT CASE WHEN created_at >= ? THEN CASE WHEN kind = 'lookup' THEN 'l' || id ELSE kind || src_type || ':' || src_id END END),
			max(created_at)
		FROM knowledge_uses WHERE item_type = 'glossary' AND item_id IN (`+strings.Join(ids, ",")+`) AND `+vis+`
		GROUP BY item_id`, append([]any{since}, args...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, last int64
		var n int
		if err := rows.Scan(&id, &n, &last); err != nil {
			return err
		}
		if g := byID[id]; g != nil {
			t := msTime(last)
			g.Uses, g.LastUsedAt = n, &t
		}
	}
	return rows.Err()
}

// NoteGlossaryLookup records a tool's lookup of a word: the entry it found
// (the nearest), or the entry that says to use another word instead.
func (s *Service) NoteGlossaryLookup(ctx context.Context, actor *Actor, res *GlossaryLookup) {
	if res == nil {
		return
	}
	switch {
	case len(res.Matches) > 0:
		g, word := res.Matches[0], ""
		if !strings.EqualFold(g.Term, res.Term) {
			word = res.Term
		}
		s.NoteLookup(ctx, actor, KGlossary, g.ID, word)
	case len(res.Instead) > 0:
		s.NoteLookup(ctx, actor, KGlossary, res.Instead[0].ID, res.Term)
	}
}

// NoteSpecLookup records a tool reading a spec: its design note.
func (s *Service) NoteSpecLookup(ctx context.Context, actor *Actor, sp *Spec) {
	if sp != nil && sp.Note != nil {
		s.NoteLookup(ctx, actor, KDesign, sp.Note.ID, "")
	}
}

// shownItem resolves what a client printed to a record the actor can see.
func (s *Service) shownItem(ctx context.Context, actor *Actor, r ShownRef) (typ string, id int64, ok bool) {
	var err error
	switch r.Type {
	case KGlossary:
		var res *GlossaryLookup
		if res, err = s.LookupGlossary(ctx, actor, r.Ref, r.Project); err == nil && len(res.Matches) > 0 {
			return KGlossary, res.Matches[0].ID, true
		}
	case KDesign:
		if id, err = s.ResolveDesign(ctx, actor, r.Ref, r.Project); err == nil && id > 0 {
			if _, err = s.kitem(ctx, actor, KDesign, id, ""); err == nil {
				return KDesign, id, true
			}
		}
	case KPage:
		if id, err = s.ResolvePage(ctx, actor, r.Ref, r.Project); err == nil && id > 0 {
			if _, err = s.kitem(ctx, actor, KPage, id, ""); err == nil {
				return KPage, id, true
			}
		}
	}
	return "", 0, false
}
