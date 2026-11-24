package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Mentions are automatic links rebuilt on every save, kept apart from deliberate links.

// termLinkCap is the most glossary terms one issue's text links on their
// own; [[term]] is never counted.
const termLinkCap = 10

// termMinLen is the shortest term (letters and digits) matched without [[ ]].
const termMinLen = 4

var (
	proseKeyRE  = lazyre.New(`\b[A-Z][A-Z0-9]{1,9}-\d{1,9}\b`)
	proseCodeRE = lazyre.New(`\b(?:[A-Z][A-Z0-9]{1,9}-)?[A-Z]{1,2}\d{1,9}\b`)
	proseADRRE  = lazyre.New(`\b(?:[A-Z][A-Z0-9]{1,9}-)?ADR-?\d{1,9}\b`)
	// codeSpanRE and the URLs after it hold text that isn't prose.
	codeSpanRE = lazyre.New("(?s)```.*?```|~~~.*?~~~|`[^`\n]*`")
	linkURLRE  = lazyre.New(`https?://\S+|\]\([^)\n]*\)`)
)

// proseOnly drops code and URLs, which name things without meaning them.
func proseOnly(text string) string {
	return linkURLRE.ReplaceAllString(codeSpanRE.ReplaceAllString(text, " "), " ")
}

// MentionScan is what a text names: issue keys and page codes as written,
// and [[wikilink]] targets.
type MentionScan struct {
	Keys    []string
	Codes   []string
	Designs []string // decision codes: ADR7, WP-ADR7, ADR-0007
	Links   []string
}

// ScanMentions finds the issue keys, page codes and [[wikilinks]] in
// markdown, once each, ignoring code and URLs. Keys and codes are upper
// case, as people write them in prose.
func ScanMentions(text string) MentionScan {
	text = proseOnly(text)
	var sc MentionScan
	seen := map[string]bool{}
	for _, m := range proseKeyRE.FindAllString(text, 500) {
		if !seen[m] {
			seen[m] = true
			sc.Keys = append(sc.Keys, m)
		}
	}
	for _, m := range proseCodeRE.FindAllString(text, 500) {
		if !seen[m] {
			seen[m] = true
			if _, ok := ParsePageCode(m); ok {
				sc.Codes = append(sc.Codes, m)
			}
		}
	}
	for _, m := range proseADRRE.FindAllString(text, 500) {
		if !seen[m] {
			seen[m] = true
			sc.Designs = append(sc.Designs, m)
		}
	}
	sc.Links = Wikilinks(text)
	return sc
}

// findLongest lists the glossary words in tokens, longest first at each
// place in the text and never inside a longer match, in order of
// appearance; the nearest entry wins where several define a word. A
// match keep refuses (a word not used in its entry's sense, WP-779) is
// passed over for the next shorter one at that place.
func (idx *termIndex) findLongest(toks []string, chain []int64, keep senseGate) []termHit {
	rank := func(project int64) int {
		if project == 0 {
			return len(chain)
		}
		for n, c := range chain {
			if c == project {
				return n
			}
		}
		return -1
	}
	var out []termHit
	for i := 0; i < len(toks); {
		type cand struct {
			w *termWord
			r int
		}
		var found []cand
		cands := []string{toks[i]}
		if len(toks[i]) > 3 {
			if t := strings.TrimSuffix(toks[i], "s"); t != toks[i] {
				cands = append(cands, t)
			}
			if t := strings.TrimSuffix(toks[i], "es"); t != toks[i] {
				cands = append(cands, t)
			}
		}
		for _, c := range cands {
			for _, w := range idx.first[c] {
				r := rank(w.project)
				if r < 0 || w.kind != UseMention || !matchAt(toks, i, w.tokens) {
					continue
				}
				found = append(found, cand{w, r})
			}
		}
		var best *termWord
		for len(found) > 0 {
			bi := 0
			for n, f := range found {
				b := found[bi]
				if len(f.w.tokens) > len(b.w.tokens) || (len(f.w.tokens) == len(b.w.tokens) && f.r < b.r) {
					bi = n
				}
			}
			w := found[bi].w
			found = append(found[:bi], found[bi+1:]...)
			if keep == nil || keep(toks, i, len(w.tokens), w.entry) {
				best = w
				break
			}
		}
		if best == nil {
			i++
			continue
		}
		out = append(out, termHit{entry: best.entry, word: best.word, kind: best.kind})
		i += len(best.tokens)
	}
	return out
}

// mentionTargets resolves what an issue's text names, before any
// permission check. Terms come from the description only: titles are
// short and full of common words.
func (s *Service) mentionTargets(ctx context.Context, q queryer, projectID, issueID int64, title, desc, comments string) []autoEnd {
	var out []autoEnd
	seen := map[autoEnd]bool{}
	add := func(typ string, id int64) {
		e := autoEnd{typ, id}
		if (typ == KIssue && id == issueID) || seen[e] {
			return
		}
		seen[e] = true
		out = append(out, e)
	}
	body := title + "\n" + desc
	sc := ScanMentions(body + "\n" + comments)
	for _, k := range sc.Keys {
		if id, err := s.resolveIssueID(ctx, q, k); err == nil {
			add(KIssue, id)
		}
	}
	for _, c := range sc.Codes {
		if pc, ok := ParsePageCode(c); ok {
			if id, ok := s.pageByCodeIn(ctx, q, pc, &projectID); ok {
				add(KPage, id)
			}
		}
	}
	for _, c := range sc.Designs {
		if id, ok := s.designByCodeLoose(ctx, q, c, &projectID); ok {
			add(KDesign, id)
		}
	}
	for _, target := range sc.Links {
		if typ, id, ok := s.resolveWikilink(ctx, q, target, &projectID); ok && (typ == KIssue || (knowledgeTables[typ] != "" && s.itemExists(ctx, q, typ, id))) {
			add(typ, id)
		}
	}
	// Glossary terms come from the description only: comment saves pass an empty one, so they skip the glossary query.
	words := wordTokens(unscannedRE.ReplaceAllString(proseOnly(desc), " "))
	if len(words) == 0 {
		return out
	}
	if idx, err := s.glossaryWords(ctx, q); err != nil {
		s.log.WarnContext(ctx, "read glossary words", "err", err)
	} else {
		chain, rerr := s.projectChainIDs(ctx, q, projectID)
		if rerr != nil {
			if readFault(rerr) {
				s.log.WarnContext(ctx, "project chain ids failed", "projectid", projectID, "err", rerr)
			}
		}
		n := 0
		for _, h := range idx.findLongest(words, chain, s.senseGate(idx)) {
			if n >= termLinkCap {
				break
			}
			if len(strings.Join(wordTokens(h.word), "")) < termMinLen {
				continue
			}
			if e := (autoEnd{KGlossary, h.entry}); !seen[e] {
				add(KGlossary, h.entry)
				n++
			}
		}
	}
	return out
}

// syncIssueMentions makes an issue's automatic mention links match its
// title, description and comments as saved. A link already there stays
// while the text still names it; a new one needs the actor to be able to
// read what it points at, so text never reaches into projects its author
// can't see. With no real user (imported text) only the issue's own
// project counts.
func (s *Service) syncIssueMentions(ctx context.Context, t *tx, actor *Actor, issueID int64) (added, removed int, err error) {
	var projectID int64
	var title, desc string
	if err := t.QueryRowContext(ctx, `SELECT project_id, title, description FROM issues WHERE id = ?`, issueID).Scan(&projectID, &title, &desc); err != nil {
		return 0, 0, err
	}
	// a link is credited to whoever wrote the text naming it; a person's text beats an agent's for the same target
	want := map[autoEnd]autoCredit{}
	credit := func(c autoCredit, ends []autoEnd) {
		for _, e := range ends {
			if have, ok := want[e]; ok {
				want[e] = have.prefer(c)
				continue
			}
			want[e] = c
		}
	}
	by, agent := s.textAuthor(ctx, t, KIssue, issueID)
	credit(autoCredit{by: by, agent: agent}, s.mentionTargets(ctx, t, projectID, issueID, title, desc, ""))
	rows, err := t.QueryContext(ctx, `SELECT body, author_id, agent, external_author FROM comments WHERE issue_id = ? ORDER BY id`, issueID)
	if err != nil {
		return 0, 0, err
	}
	type comment struct {
		body string
		c    autoCredit
	}
	var comments []comment
	for rows.Next() {
		var b, ag, ext string
		var author sql.NullInt64
		if err := rows.Scan(&b, &author, &ag, &ext); err != nil {
			rows.Close()
			return 0, 0, err
		}
		c := autoCredit{agent: ag}
		switch {
		case ext != "":
			c = autoCredit{agent: "forge:" + ext}
		case author.Valid:
			c.by = author.Int64
		}
		comments = append(comments, comment{b, c})
	}
	rows.Close()
	for _, cm := range comments {
		credit(cm.c, s.mentionTargets(ctx, t, projectID, issueID, "", "", cm.body))
	}
	allow := func(e autoEnd) bool {
		if actor == nil || actor.User == nil {
			var pid sql.NullInt64
			table := knowledgeTables[e.typ]
			if e.typ == KIssue {
				table = "issues"
			}
			// #nosec G202 -- the table name comes from knowledgeTables
			if err := t.QueryRowContext(ctx, `SELECT project_id FROM `+table+` WHERE id = ?`, e.id).Scan(&pid); err != nil {
				if !errors.Is(err, sql.ErrNoRows) {
					if readFault(err) {
						s.log.WarnContext(ctx, "read the project of a mentioned item", "type", e.typ, "id", e.id, "err", err)
					}
				}
				return false
			}
			return pid.Valid && pid.Int64 == projectID
		}
		ok, rerr := s.kitemTx(ctx, t, actor, e.typ, e.id)
		if readFault(rerr) && !missingOrDenied(rerr) {
			s.log.WarnContext(ctx, "read whether the author may see a mentioned item", "type", e.typ, "id", e.id, "issue", issueID, "err", rerr)
		}
		return ok
	}
	return s.applyAutoLinks(ctx, t, KIssue, issueID, want, allow)
}

// noteIssueMentions is syncIssueMentions for the places an issue's text
// is saved: a problem is logged, never fails the save.
func (s *Service) noteIssueMentions(ctx context.Context, t *tx, actor *Actor, issueID int64) {
	if _, _, err := s.syncIssueMentions(ctx, t, actor, issueID); err != nil {
		s.log.WarnContext(ctx, "issue mentions", "issue", issueID, "err", err)
	}
}

// MentionBackfill is what BackfillMentionLinks did.
type MentionBackfill struct {
	Issues     int `json:"issues"`
	Comments   int `json:"comments"`
	Added      int `json:"added"`       // automatic mention links made
	Removed    int `json:"removed"`     // stale ones dropped
	IssueLinks int `json:"issue_links"` // issue→issue links in all after the run
	Unrelated  int `json:"unrelated"`   // …between issues with no relation, parent or child
}

func (b MentionBackfill) String() string {
	return fmt.Sprintf("%d issues (%d comments) scanned, %d links added, %d removed; %d issue mentions, %d with no relation",
		b.Issues, b.Comments, b.Added, b.Removed, b.IssueLinks, b.Unrelated)
}

// v2: links credited per comment, not to the issue author (WP-ADR50).
const mentionsSetting = "issue_mentions_v2"

// BackfillMentionLinks links the issues written before issue text was
// scanned, as their authors would have (WP-101). It runs once, in the
// background at start, and is safe to run again: it only makes the
// automatic links match the text. force reruns it after it has run.
func (s *Service) BackfillMentionLinks(ctx context.Context, force bool) (MentionBackfill, error) {
	var res MentionBackfill
	if !force {
		var done int
		if err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM server_settings WHERE key = ?`, mentionsSetting).Scan(&done); err != nil || done > 0 {
			return res, err
		}
	}
	var after int64
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		ids, err := queryIDs(ctx, s.db.R, `SELECT id FROM issues WHERE id > ? ORDER BY id LIMIT 100`, after)
		if err != nil {
			return res, err
		}
		if len(ids) == 0 {
			break
		}
		after = ids[len(ids)-1]
		err = s.write(ctx, SystemActor(), func(t *tx) error {
			for _, id := range ids {
				var by sql.NullInt64
				var agent string
				var comments int
				if err := t.QueryRowContext(ctx, `SELECT coalesce(text_by, reporter_id), text_agent, (SELECT count(*) FROM comments WHERE issue_id = issues.id)
					FROM issues WHERE id = ?`, id).Scan(&by, &agent, &comments); err != nil {
					return err
				}
				actor := &Actor{}
				if by.Valid {
					if u, err := s.userByID(ctx, t, by.Int64); err == nil {
						actor = &Actor{User: u, Agent: agent}
					}
				}
				a, r, err := s.syncIssueMentions(ctx, t, actor, id)
				if err != nil {
					return err
				}
				res.Issues++
				res.Comments += comments
				res.Added += a
				res.Removed += r
			}
			return nil
		})
		if err != nil {
			return res, err
		}
	}
	err := s.db.R.QueryRowContext(ctx, `SELECT count(*),
		coalesce(sum(NOT EXISTS (SELECT 1 FROM issue_relations r WHERE (r.source_id = l.src_id AND r.target_id = l.dst_id) OR (r.source_id = l.dst_id AND r.target_id = l.src_id))
			AND NOT EXISTS (SELECT 1 FROM issues a, issues b WHERE a.id = l.src_id AND b.id = l.dst_id AND (a.parent_id = b.id OR b.parent_id = a.id))), 0)
		FROM knowledge_links l WHERE l.src_type = 'issue' AND l.dst_type = 'issue' AND l.auto = 1`).Scan(&res.IssueLinks, &res.Unrelated)
	if err != nil {
		return res, err
	}
	_, err = s.db.W.ExecContext(ctx, `INSERT OR REPLACE INTO server_settings(key, value) VALUES (?, '1')`, mentionsSetting)
	return res, err
}

// RunMentionBackfill is BackfillMentionLinks for the server's start.
func (s *Service) RunMentionBackfill(ctx context.Context) {
	res, err := s.BackfillMentionLinks(ctx, false)
	if err != nil {
		s.log.WarnContext(ctx, "backfill mention links", "err", err)
		return
	}
	if res.Issues > 0 {
		s.log.Info("backfilled mention links", "issues", res.Issues, "comments", res.Comments, "added", res.Added,
			"issue_links", res.IssueLinks, "unrelated", res.Unrelated)
	}
}
