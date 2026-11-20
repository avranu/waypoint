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

// Decision codes (WP-113): a decision note is known by ADR and a number,
// numbered per project (global notes have their own run): ADR7, or
// APP-ADR7 from another project. ADR-0007 and appADR7 mean the same. Codes
// stay with a note when its kind or project changes (a move adds the new
// project's code and the old one keeps leading here) and are never reused,
// even after the note is deleted.

var (
	designCodeRE = lazyre.New(`(?i)^(?:([A-Z][A-Z0-9]{1,9})-?)?ADR-?0*([0-9]{1,9})$`)
	// "ADR-0007: Title", "ADR7 — Title", as imported decisions start.
	designTitleCodeRE = lazyre.New(`(?i)^ADR-?0*([0-9]{1,9})\s*(?::|—|–|-)\s+(\S.*)$`)
)

// DesignCode is a parsed decision code; Project is set for APP-ADR7.
type DesignCode struct {
	Project string
	Number  int64
}

func (c DesignCode) String() string {
	s := "ADR" + strconv.FormatInt(c.Number, 10)
	if c.Project != "" {
		return c.Project + "-" + s
	}
	return s
}

// ParseDesignCode reads ADR7, ADR-0007, APP-ADR7 or appADR-7 (any case); ok
// is false for anything else.
func ParseDesignCode(v string) (DesignCode, bool) {
	m := designCodeRE.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return DesignCode{}, false
	}
	n, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil || n <= 0 {
		return DesignCode{}, false
	}
	return DesignCode{Project: strings.ToUpper(m[1]), Number: n}, true
}

func designCodeOf(number sql.NullInt64) string {
	if number.Valid {
		return "ADR" + strconv.FormatInt(number.Int64, 10)
	}
	return ""
}

// designScope is where a note's codes are numbered: its project, or 0.
func designScope(projectID sql.NullInt64) int64 {
	if projectID.Valid {
		return projectID.Int64
	}
	return 0
}

// designCodeTaken says which note holds a code in a scope: 0 and true for
// a deleted note's code.
func designCodeTaken(ctx context.Context, q queryer, scope, n int64) (int64, bool, error) {
	var id sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT note_id FROM design_codes WHERE scope = ? AND number = ?`, scope, n).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id.Int64, err == nil, err
}

// codeDesign gives a note the code its kind and place call for, if it
// needs one: a decision without one, or a note that moved to another
// project. want (from an explicit code) if free, else the code it had there
// before, else the next free number. A title starting with its own code
// ("ADR-0007: …", as imported) adopts it and loses the prefix.
func (s *Service) codeDesign(ctx context.Context, t *tx, id, want int64) error {
	var title, kind string
	var project, number sql.NullInt64
	if err := t.QueryRowContext(ctx, `SELECT title, kind, project_id, number FROM design_notes WHERE id = ?`, id).
		Scan(&title, &kind, &project, &number); err != nil {
		return err
	}
	scope := designScope(project)
	if kind != "decision" && !number.Valid {
		return nil
	}
	if number.Valid && want == 0 {
		if holder, ok, err := designCodeTaken(ctx, t, scope, number.Int64); err != nil {
			return err
		} else if ok && holder == id {
			return nil
		}
	}
	if m := designTitleCodeRE.FindStringSubmatch(title); m != nil && want == 0 {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		holder, taken, err := designCodeTaken(ctx, t, scope, n)
		if err != nil {
			return err
		}
		if n > 0 && (!taken || holder == id) {
			want = n
			if _, err := t.ExecContext(ctx, `UPDATE design_notes SET title = ? WHERE id = ?`, m[2], id); err != nil {
				return err
			}
			if _, err := t.ExecContext(ctx, `UPDATE design_revisions SET title = ? WHERE note_id = ? AND kind = 'edit' AND title = ?`, m[2], id, title); err != nil {
				return err
			}
		}
	}
	n := want
	if n > 0 {
		holder, taken, err := designCodeTaken(ctx, t, scope, n)
		if err != nil {
			return err
		}
		if taken && holder != id {
			if holder == 0 {
				return invalid("ADR%d was used before (codes aren't reused); leave the code out for the next free one", n)
			}
			return invalid("ADR%d is already design #%d", n, holder)
		}
	} else {
		// Back to a place it had a code in: its old code.
		err := t.QueryRowContext(ctx, `SELECT number FROM design_codes WHERE scope = ? AND note_id = ? ORDER BY number LIMIT 1`, scope, id).Scan(&n)
		if errors.Is(err, sql.ErrNoRows) {
			if err := t.QueryRowContext(ctx, `SELECT coalesce(max(number), 0) + 1 FROM design_codes WHERE scope = ?`, scope).Scan(&n); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	if _, err := t.ExecContext(ctx, `INSERT OR IGNORE INTO design_codes(scope, number, note_id) VALUES (?,?,?)`, scope, n, id); err != nil {
		return err
	}
	_, err := t.ExecContext(ctx, `UPDATE design_notes SET number = ? WHERE id = ?`, n, id)
	return err
}

// recodeDesignSubtree re-codes a note and everything beneath it (after a
// move into another project).
func (s *Service) recodeDesignSubtree(ctx context.Context, t *tx, id int64) error {
	ids, err := queryIDs(ctx, t, `WITH RECURSIVE d(id) AS (SELECT ? UNION SELECT x.id FROM design_notes x JOIN d ON x.parent_id = d.id)
		SELECT id FROM d ORDER BY id`, id)
	if err != nil {
		return err
	}
	for _, v := range ids {
		if err := s.codeDesign(ctx, t, v, 0); err != nil {
			return err
		}
	}
	return nil
}

// AssignDesignCodes codes every decision note that has no code yet,
// adopting codes their titles carry (titled ones first, so a later
// "ADR2: …" isn't beaten to ADR2). The server runs it at start.
func (s *Service) AssignDesignCodes(ctx context.Context) error {
	ids, err := queryIDs(ctx, s.db.R, `SELECT id FROM design_notes WHERE number IS NULL AND kind = 'decision' ORDER BY id`)
	if err != nil || len(ids) == 0 {
		return err
	}
	return s.write(ctx, SystemActor(), func(t *tx) error {
		for _, pass := range []bool{true, false} {
			for _, id := range ids {
				var title string
				if err := t.QueryRowContext(ctx, `SELECT title FROM design_notes WHERE id = ? AND number IS NULL`, id).Scan(&title); errors.Is(err, sql.ErrNoRows) {
					continue
				} else if err != nil {
					return err
				}
				if designTitleCodeRE.MatchString(title) == pass {
					if err := s.codeDesign(ctx, t, id, 0); err != nil {
						return fmt.Errorf("design #%d: %w", id, err)
					}
					if err := s.afterDesignChange(ctx, t, id); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}

// explicitDesignCode reads the code a caller asked for on a new decision
// ("ADR12", for a decision imported with one): its number.
func explicitDesignCode(v *string) (int64, error) {
	if v == nil || strings.TrimSpace(*v) == "" {
		return 0, nil
	}
	c, ok := ParseDesignCode(*v)
	if !ok || c.Project != "" {
		return 0, invalid("%q isn't a decision code like ADR7", *v)
	}
	return c.Number, nil
}

// designByCode finds the note a code names for the actor: in project if
// given (then the projects above it, then global), else wherever it's
// unambiguous among the notes they can read.
func (s *Service) designByCode(ctx context.Context, actor *Actor, c DesignCode, project string) (int64, error) {
	if c.Project != "" {
		project = c.Project
	}
	readable := func(id int64) bool {
		_, _, err := s.designFor(ctx, s.db.R, actor, id, RoleViewer)
		if err != nil {
			// Not found or not allowed is "can't read it"; anything else is a failed read.
			if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrForbidden) {
				if readFault(err) {
					s.log.WarnContext(ctx, "read whether a design note is readable", "note", id, "err", err)
				}
			}
			return false
		}
		return true
	}
	if project != "" {
		var chain []int64
		if strings.EqualFold(project, "global") {
			chain = []int64{0}
		} else {
			p, err := s.projectByRef(ctx, s.db.R, project)
			if err != nil {
				return 0, err
			}
			if chain, err = s.projectChainIDs(ctx, s.db.R, p.ID); err != nil {
				return 0, err
			}
			if c.Project == "" {
				chain = append(chain, 0)
			} else {
				chain = chain[:1]
			}
		}
		for _, sc := range chain {
			if id, ok, err := designCodeTaken(ctx, s.db.R, sc, c.Number); err != nil {
				return 0, err
			} else if ok && id != 0 && readable(id) {
				return id, nil
			}
		}
		if c.Project != "" {
			return 0, notFound("design note " + c.String())
		}
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT dc.note_id, coalesce(p.key, 'global') FROM design_codes dc
		JOIN design_notes n ON n.id = dc.note_id LEFT JOIN projects p ON p.id = n.project_id
		WHERE dc.number = ? ORDER BY dc.note_id`, c.Number)
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
		if readable(h.id) {
			hits = append(hits, h)
		}
	}
	rows.Close()
	switch len(hits) {
	case 0:
		return 0, notFound("design note " + c.String())
	case 1:
		return hits[0].id, nil
	}
	var opts []string
	for _, h := range hits {
		opts = append(opts, h.key+"-"+c.String())
	}
	slices.Sort(opts)
	return 0, invalid("%s is in several projects; say which: %s", c.String(), strings.Join(opts, ", "))
}

// ResolveDesign finds a design note by number (12, #12, design:12) or code
// (ADR7, ADR-0007, APP-ADR7; any case). A bare code is looked up in project
// first (may be empty).
func (s *Service) ResolveDesign(ctx context.Context, actor *Actor, ref, project string) (int64, error) {
	ref = strings.TrimSpace(ref)
	if len(ref) > 7 && strings.EqualFold(ref[:7], "design:") {
		ref = strings.TrimSpace(ref[7:])
	}
	if n, err := strconv.ParseInt(strings.TrimPrefix(ref, "#"), 10, 64); err == nil && n > 0 {
		return n, nil
	}
	c, ok := ParseDesignCode(ref)
	if !ok {
		return 0, invalid("%q isn't a design note number or code (like 12, ADR7 or APP-ADR7)", ref)
	}
	return s.designByCode(ctx, actor, c, project)
}

// designByCodeIn finds the note a code names for an item in a project
// (scope): APP-ADR7 in APP; ADR7 in the project, the nearest one above it
// that has it, or among the global notes.
func (s *Service) designByCodeIn(ctx context.Context, q queryer, c DesignCode, scope *int64) (int64, bool) {
	var chain []int64
	switch {
	case strings.EqualFold(c.Project, "global"):
		chain = []int64{0}
	case c.Project != "":
		p, err := s.projectByRef(ctx, q, c.Project)
		if err != nil {
			return 0, false
		}
		chain = []int64{p.ID}
	default:
		if scope != nil {
			var rerr error
			chain, rerr = s.projectChainIDs(ctx, q, *scope)
			if rerr != nil {
				if readFault(rerr) {
					s.log.WarnContext(ctx, "project chain ids failed", "err", rerr)
				}
			}
		}
		chain = append(chain, 0)
	}
	for _, sc := range chain {
		if id, ok, err := designCodeTaken(ctx, q, sc, c.Number); err == nil && ok && id != 0 {
			return id, true
		}
	}
	return 0, false
}

// designByCodeLoose is designByCodeIn for any spelling of a code.
func (s *Service) designByCodeLoose(ctx context.Context, q queryer, ref string, scope *int64) (int64, bool) {
	if c, ok := ParseDesignCode(ref); ok {
		return s.designByCodeIn(ctx, q, c, scope)
	}
	return 0, false
}
