package core

import "context"

// newRelation is a row for issue_relations, as insertRelation takes it.
type newRelation struct {
	Src, Dst      int64
	Kind, Subkind string
	NamedIn       *int64
	Found         string // "": NULL, someone chose the kind
	Basis         any
	CreatedBy     int64 // 0: nobody (Waypoint itself)
	CreatedAgent  string
	Score         *float64
	// Unrelated: made only when the pair has no relation at all, of any
	// kind, in either direction. For relations Waypoint makes on its own
	// reading: what a person or an agent chose first stands alone.
	Unrelated bool
}

// insertRelation is the one way a row gets into issue_relations (WP-1370),
// inside a write. It says whether it wrote one: a relation the pair already
// has is not an error but a relation that is already there (the same kind
// the same way round, or, for a kind that reads the same from both ends such
// as relates, the other way round), so two writers meeting on one pair,
// whichever end each names, leave one row and both succeed. The survivor is
// whichever write ran first: writes are serialised, and the later one finds
// it here. A person's own relation of a pair replaces Waypoint's inferred
// one before it gets here (replaceRelations), so a person's wins either way.
func (s *Service) insertRelation(ctx context.Context, t *tx, r newRelation) (bool, error) {
	if r.Src == r.Dst {
		return false, invalid("an issue cannot be related to itself")
	}
	if r.Unrelated {
		var related bool
		if err := t.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM issue_relations
			WHERE (source_id = ? AND target_id = ?) OR (source_id = ? AND target_id = ?))`, r.Src, r.Dst, r.Dst, r.Src).Scan(&related); err != nil {
			return false, err
		}
		if related {
			return false, nil
		}
	}
	if k, _, ok := storedRelation(r.Kind); ok && k.Inverse == "" {
		var back bool
		if err := t.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM issue_relations WHERE kind = ? AND source_id = ? AND target_id = ?)`,
			r.Kind, r.Dst, r.Src).Scan(&back); err != nil {
			return false, err
		}
		if back {
			return false, nil
		}
	}
	var found, score any
	if r.Found != "" {
		found = r.Found
	}
	if r.Score != nil {
		score = *r.Score
	}
	res, err := t.ExecContext(ctx, `INSERT INTO issue_relations(source_id, target_id, kind, subkind, created_at, named_in, found, basis, created_by, created_agent, score)
		VALUES (?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(source_id, target_id, kind) DO NOTHING`,
		r.Src, r.Dst, r.Kind, r.Subkind, s.nowMS(), nullInt(r.NamedIn), found, r.Basis, nullUser(r.CreatedBy), r.CreatedAgent, score)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
