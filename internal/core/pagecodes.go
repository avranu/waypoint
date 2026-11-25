package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// PagePrefixes are the code prefixes of the page categories, as an
// earlier set of repos numbered their documents.
var PagePrefixes = map[string]string{
	"plan": "PL", "idea": "I", "experiment": "X", "reference": "R", "note": "N",
	"decision": "D", "problem": "P", "feature": "F", "bug": "B",
}

// retiredPrefixes are prefixes no category has any more, still recognised
// in imported titles (an old task, T7, became a page) so the old
// code keeps leading to the page.
var retiredPrefixes = []string{"T"}

func knownPrefix(p string) bool {
	for _, v := range PagePrefixes {
		if v == p {
			return true
		}
	}
	return slices.Contains(retiredPrefixes, p)
}

// categoryOfPrefix is the category a prefix stands for ("" if none does).
func categoryOfPrefix(p string) string {
	for c, v := range PagePrefixes {
		if v == p {
			return c
		}
	}
	return ""
}

var (
	pageCodeRE = lazyre.New(`^(?:([A-Za-z][A-Za-z0-9]{1,9})-)?([A-Za-z]{1,2})(\d{1,9})$`)
	// "PL13: Title", "PL13 — Title", as imported documents start.
	titleCodeRE = lazyre.New(`^([A-Z]{1,2})(\d{1,9})\s*(?::|—|–|-)\s+(\S.*)$`)
)

// PageCode is a parsed code: Project is set for APP-PL13.
type PageCode struct {
	Project string
	Prefix  string
	Number  int64
}

func (c PageCode) String() string {
	s := c.Prefix + strconv.FormatInt(c.Number, 10)
	if c.Project != "" {
		return c.Project + "-" + s
	}
	return s
}

// ParsePageCode reads PL13 or APP-PL13 (any case); ok is false for
// anything else, including issue keys and unknown prefixes.
func ParsePageCode(v string) (PageCode, bool) {
	m := pageCodeRE.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return PageCode{}, false
	}
	c := PageCode{Project: strings.ToUpper(m[1]), Prefix: strings.ToUpper(m[2])}
	if !knownPrefix(c.Prefix) {
		return PageCode{}, false
	}
	n, err := strconv.ParseInt(m[3], 10, 64)
	if err != nil || n <= 0 {
		return PageCode{}, false
	}
	c.Number = n
	return c, true
}

func pageCode(category string, number sql.NullInt64) string {
	if p := PagePrefixes[category]; p != "" && number.Valid {
		return p + strconv.FormatInt(number.Int64, 10)
	}
	return ""
}

// pageScope is where a page's codes are numbered: its project, or its
// owner for a personal page.
func pageScope(projectID, ownerID sql.NullInt64) int64 {
	if projectID.Valid {
		return projectID.Int64
	}
	return -ownerID.Int64
}

// codeTaken says which page holds a code in a scope: 0 and true for a
// reserved or deleted page's code; false if the code is free.
func codeTaken(ctx context.Context, q queryer, scope int64, prefix string, n int64) (int64, bool, error) {
	var id sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT page_id FROM page_codes WHERE scope = ? AND prefix = ? AND number = ?`, scope, prefix, n).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id.Int64, err == nil, err
}

// codePage gives a page the code its category and scope call for, if it
// lacks one: want (from an explicit code) if free, else the code it had
// there before, else the next free number. A title starting with its own
// code ("PL13: …", as imported) adopts that code and loses the prefix; a
// title starting with another known code (T7) keeps it as an alias.
func (s *Service) codePage(ctx context.Context, t *tx, id, want int64) error {
	var title, category string
	var project, owner, number sql.NullInt64
	if err := t.QueryRowContext(ctx, `SELECT title, category, project_id, owner_id, number FROM pages WHERE id = ?`, id).
		Scan(&title, &category, &project, &owner, &number); err != nil {
		return err
	}
	scope := pageScope(project, owner)
	if scope == 0 {
		return nil
	}
	prefix := PagePrefixes[category]
	if number.Valid && prefix != "" && want == 0 {
		if holder, ok, err := codeTaken(ctx, t, scope, prefix, number.Int64); err != nil {
			return err
		} else if ok && holder == id {
			return nil
		}
	}
	if m := titleCodeRE.FindStringSubmatch(title); m != nil && knownPrefix(m[1]) {
		n, _ := strconv.ParseInt(m[2], 10, 64)
		holder, taken, err := codeTaken(ctx, t, scope, m[1], n)
		if err != nil {
			return err
		}
		switch {
		case taken && holder != id:
		case m[1] == prefix && (want == 0 || want == n):
			want = n
			if _, err := t.ExecContext(ctx, `UPDATE pages SET title = ? WHERE id = ?`, m[3], id); err != nil {
				return err
			}
			if err := s.reindexPage(ctx, t, id); err != nil {
				return err
			}
		case m[1] != prefix && !taken:
			if _, err := t.ExecContext(ctx, `INSERT INTO page_codes(scope, prefix, number, page_id) VALUES (?,?,?,?)`, scope, m[1], n, id); err != nil {
				return err
			}
		}
	}
	if prefix == "" {
		_, err := t.ExecContext(ctx, `UPDATE pages SET number = NULL WHERE id = ?`, id)
		return err
	}
	n := want
	if n > 0 {
		holder, taken, err := codeTaken(ctx, t, scope, prefix, n)
		if err != nil {
			return err
		}
		if taken && holder != id {
			if holder == 0 {
				return invalid("%s%d was used before (codes aren't reused); leave the code out for the next free one", prefix, n)
			}
			return invalid("%s%d is already page #%d", prefix, n, holder)
		}
	} else {
		err := t.QueryRowContext(ctx, `SELECT number FROM page_codes WHERE scope = ? AND prefix = ? AND page_id = ? ORDER BY rowid LIMIT 1`, scope, prefix, id).Scan(&n)
		if errors.Is(err, sql.ErrNoRows) {
			if err := t.QueryRowContext(ctx, `SELECT coalesce(max(number), 0) + 1 FROM page_codes WHERE scope = ? AND prefix = ?`, scope, prefix).Scan(&n); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	if _, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO page_codes(scope, prefix, number, page_id) VALUES (?,?,?,?)`, scope, prefix, n, id); err != nil {
		return err
	}
	_, err := t.ExecContext(ctx, `UPDATE pages SET number = ? WHERE id = ?`, n, id)
	return err
}

// recodeSubtree re-codes a page and everything beneath it (after a move
// into another project).
func (s *Service) recodeSubtree(ctx context.Context, t *tx, id int64) error {
	rows, err := t.QueryContext(ctx, `WITH RECURSIVE d(id) AS (SELECT id FROM pages WHERE id = ?
		UNION SELECT x.id FROM pages x JOIN d ON x.parent_id = d.id) SELECT id FROM d`, id)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, v)
	}
	rows.Close()
	for _, v := range ids {
		if err := s.codePage(ctx, t, v, 0); err != nil {
			return err
		}
	}
	return nil
}

// AssignPageCodes codes every categorised page that has no code yet
// (pages from before codes, oldest first, adopting codes their titles
// carry). The server runs it at start; it's quick when there's nothing to do.
func (s *Service) AssignPageCodes(ctx context.Context) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM pages WHERE number IS NULL AND category != '' ORDER BY id`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) == 0 {
		return nil
	}
	return s.write(ctx, SystemActor(), func(t *tx) error {
		for _, pass := range []bool{true, false} {
			for _, id := range ids {
				var title string
				if err := t.QueryRowContext(ctx, `SELECT title FROM pages WHERE id = ? AND number IS NULL`, id).Scan(&title); errors.Is(err, sql.ErrNoRows) {
					continue
				} else if err != nil {
					return err
				}
				if titleCodeRE.MatchString(title) == pass {
					if err := s.codePage(ctx, t, id, 0); err != nil {
						return fmt.Errorf("page #%d: %w", id, err)
					}
				}
			}
		}
		return nil
	})
}

// pageByCode finds the page a code names for the actor: in project (a key)
// if the code doesn't name one, else wherever it's unambiguous among the
// pages they can read. A code whose page was deleted is "not found".
func (s *Service) pageByCode(ctx context.Context, actor *Actor, c PageCode, project string) (int64, error) {
	if c.Project != "" {
		project = c.Project
	}
	if project != "" {
		p, err := s.projectByRef(ctx, s.db.R, project)
		if err != nil {
			return 0, err
		}
		id, ok, err := codeTaken(ctx, s.db.R, p.ID, c.Prefix, c.Number)
		if err != nil {
			return 0, err
		}
		if ok && id != 0 {
			if _, _, err := s.pageFor(ctx, s.db.R, actor, id, RoleViewer, false); err != nil {
				return 0, notFound("page " + c.String() + " in " + strings.ToUpper(project))
			}
			return id, nil
		}
		if c.Project != "" {
			return 0, notFound("page " + c.String() + " in " + strings.ToUpper(project))
		}
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT pc.page_id, coalesce(p.key, '') FROM page_codes pc
		JOIN pages pg ON pg.id = pc.page_id LEFT JOIN projects p ON p.id = pg.project_id
		WHERE pc.prefix = ? AND pc.number = ? ORDER BY pc.scope < 0 DESC, pc.page_id`, c.Prefix, c.Number)
	if err != nil {
		return 0, err
	}
	type hit struct {
		id  int64
		key string
	}
	var hits []hit
	for rows.Next() {
		var h hit
		if err := rows.Scan(&h.id, &h.key); err != nil {
			rows.Close()
			return 0, err
		}
		hits = append(hits, h)
	}
	rows.Close()
	var seen []hit
	for _, h := range hits {
		if slices.ContainsFunc(seen, func(x hit) bool { return x.id == h.id }) {
			continue
		}
		if _, _, err := s.pageFor(ctx, s.db.R, actor, h.id, RoleViewer, false); err == nil {
			seen = append(seen, h)
		}
	}
	switch len(seen) {
	case 0:
		where := ""
		if project != "" {
			where = " in " + strings.ToUpper(project)
		}
		return 0, notFound("page " + c.String() + where)
	case 1:
		return seen[0].id, nil
	}
	var opts []string
	for _, h := range seen {
		if h.key != "" {
			opts = append(opts, h.key+"-"+c.String())
		}
	}
	return 0, invalid("%s is in several projects; say which: %s", c.String(), strings.Join(opts, ", "))
}

// ResolvePage finds a page by number (7, #7) or code (PL13, APP-PL13 or
// APPPL13, any case); a bare code is looked up in project first (may be empty).
func (s *Service) ResolvePage(ctx context.Context, actor *Actor, ref, project string) (int64, error) {
	ref = strings.TrimSpace(ref)
	if n, err := strconv.ParseInt(strings.TrimPrefix(ref, "#"), 10, 64); err == nil && n > 0 {
		return n, nil
	}
	cands := pageCodeCandidates(ref)
	var firstErr error
	for _, c := range cands {
		id, err := s.pageByCode(ctx, actor, c, project)
		if err == nil {
			return id, nil
		}
		if firstErr == nil || (errors.Is(firstErr, ErrNotFound) && !errors.Is(err, ErrNotFound)) {
			firstErr = err
		}
	}
	if firstErr != nil {
		if len(cands) > 1 && errors.Is(firstErr, ErrNotFound) {
			return 0, notFound("page " + strings.ToUpper(ref))
		}
		return 0, firstErr
	}
	return 0, invalid("%q isn't a page number or code (like 7, PL13 or APP-PL13)", ref)
}

// PageCodeNext is the next free number for each prefix in a project.
type PageCodeNext map[string]int64

// PageCodesNext lists a project's next free page codes, for every prefix.
func (s *Service) PageCodesNext(ctx context.Context, actor *Actor, project string) (PageCodeNext, error) {
	p, err := s.projectByRef(ctx, s.db.R, project)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleViewer); err != nil {
		return nil, err
	}
	out := PageCodeNext{}
	for _, pre := range append(prefixList(), retiredPrefixes...) {
		out[pre] = 1
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT prefix, max(number) + 1 FROM page_codes WHERE scope = ? GROUP BY prefix`, p.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pre string
		var n int64
		if err := rows.Scan(&pre, &n); err != nil {
			return nil, err
		}
		out[pre] = n
	}
	return out, rows.Err()
}

func prefixList() []string {
	var out []string
	for _, c := range PageCategories {
		out = append(out, PagePrefixes[c])
	}
	return out
}

// ReservePageCodes moves a project's next free codes up (never down), so
// codes used elsewhere before (an imported repo's PL1…PL14) aren't given
// to new pages. next maps prefixes to the first number new pages may get.
func (s *Service) ReservePageCodes(ctx context.Context, actor *Actor, project string, next PageCodeNext) (PageCodeNext, error) {
	if err := actor.requireHuman("reserving page codes"); err != nil {
		return nil, err
	}
	p, err := s.projectByRef(ctx, s.db.R, project)
	if err != nil {
		return nil, err
	}
	err = s.write(ctx, actor, func(t *tx) error {
		if err := s.requireProjectRole(ctx, t, actor, p.ID, RoleOwner); err != nil {
			return err
		}
		for pre, n := range next {
			pre = strings.ToUpper(pre)
			if !knownPrefix(pre) {
				return invalid("%q isn't a page code prefix (%s)", pre, strings.Join(append(prefixList(), retiredPrefixes...), ", "))
			}
			if n < 1 || n > 1e9 {
				return invalid("%s: the next number must be at least 1", pre)
			}
			if n == 1 {
				continue
			}
			if _, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO page_codes(scope, prefix, number, page_id) VALUES (?,?,?,NULL)`, p.ID, pre, n-1); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.PageCodesNext(ctx, actor, project)
}

// pageByCodeIn finds the page a code names for an item in a project
// (scope): APP-PL13 in APP; PL13 in the project or the nearest one above it
// that has it. Personal pages aren't anyone's link targets.
func (s *Service) pageByCodeIn(ctx context.Context, q queryer, c PageCode, scope *int64) (int64, bool) {
	var chain []int64
	if c.Project != "" {
		p, err := s.projectByRef(ctx, q, c.Project)
		if err != nil {
			return 0, false
		}
		chain = []int64{p.ID}
	} else if scope != nil {
		var rerr error
		chain, rerr = s.projectChainIDs(ctx, q, *scope)
		if rerr != nil {
			if readFault(rerr) {
				s.log.WarnContext(ctx, "project chain ids failed", "err", rerr)
			}
		}
	}
	for _, pid := range chain {
		if id, ok, err := codeTaken(ctx, q, pid, c.Prefix, c.Number); err == nil && ok && id != 0 {
			return id, true
		}
	}
	return 0, false
}

// DropPageCode stops an old code (not the page's current one) from leading
// to a page: for a code given by mistake, like one a project's documents
// used elsewhere before. The code stays reserved, never reused. code is
// PL1, in the page's project, or APP-PL1.
func (s *Service) DropPageCode(ctx context.Context, actor *Actor, pageID int64, code string) error {
	if err := actor.requireHuman("dropping page codes"); err != nil {
		return err
	}
	c, ok := ParsePageCode(code)
	if !ok {
		return invalid("%q isn't a page code like PL13", code)
	}
	return s.write(ctx, actor, func(t *tx) error {
		pg, _, err := s.pageFor(ctx, t, actor, pageID, RoleEditor, false)
		if err != nil {
			return err
		}
		var project, owner sql.NullInt64
		if pg.ProjectID != nil {
			project = sql.NullInt64{Int64: *pg.ProjectID, Valid: true}
		}
		if pg.Owner != nil {
			owner = sql.NullInt64{Int64: pg.Owner.ID, Valid: true}
		}
		own := pageScope(project, owner)
		scope := own
		if c.Project != "" {
			p, err := s.projectByRef(ctx, t, c.Project)
			if err != nil {
				return err
			}
			scope = p.ID
		}
		if scope == own && c.Prefix+strconv.FormatInt(c.Number, 10) == pg.Code {
			return invalid("%s is the page's code; give it another (code) first", pg.Code)
		}
		res, err := t.ExecContext(ctx, `UPDATE page_codes SET page_id = NULL WHERE scope = ? AND prefix = ? AND number = ? AND page_id = ?`, scope, c.Prefix, c.Number, pageID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return notFound(c.String() + " on this page")
		}
		s.publishPage(pg.ProjectID, pageID)
		return nil
	})
}

// pageCodeCandidates is what a page reference could mean as a code:
// "PL13", "APP-PL13", and "APPPL13" (a project key run into the code), any
// case. Nil if it can't be one.
func pageCodeCandidates(ref string) []PageCode {
	if c, ok := ParsePageCode(ref); ok {
		return []PageCode{c}
	}
	up := strings.ToUpper(strings.TrimSpace(ref))
	var out []PageCode
	for i := 2; i < len(up)-1; i++ {
		if !projectKeyOnly.MatchString(up[:i]) {
			continue
		}
		if c, ok := ParsePageCode(up[i:]); ok && c.Project == "" {
			c.Project = up[:i]
			out = append(out, c)
		}
	}
	return out
}

// pageByCodeLoose is pageByCodeIn for any spelling of a code.
func (s *Service) pageByCodeLoose(ctx context.Context, q queryer, ref string, scope *int64) (int64, bool) {
	for _, c := range pageCodeCandidates(ref) {
		if id, ok := s.pageByCodeIn(ctx, q, c, scope); ok {
			return id, true
		}
	}
	return 0, false
}
