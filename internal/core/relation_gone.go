package core

import (
	"context"
	"database/sql"
	"errors"
)

// relationGone reads the relation id before it is deleted, as seen from
// issue i: the text the add path puts in New ("follows (fix) WP-3") for the
// event's Old, and its data. A removal is the only label that a suggested,
// inferred or agent-made relation was wrong (WP-999), so the row is kept in
// the event. by says who removed it when it wasn't the actor ("automatic");
// reason says why, when it is known ("replaced by relates").
// sql.ErrNoRows: i has no such relation.
func (s *Service) relationGone(ctx context.Context, t *tx, i *Issue, id int64, by, reason string) (string, map[string]any, error) {
	var src, dst, createdAt int64
	var kind, subkind, createdAgent string
	var found, basis sql.NullString
	var createdBy sql.NullInt64
	var score sql.NullFloat64
	err := t.QueryRowContext(ctx, `SELECT source_id, target_id, kind, subkind, found, basis, created_by, created_agent, score, created_at
		FROM issue_relations WHERE id = ? AND (source_id = ? OR target_id = ?)`, id, i.ID, i.ID).
		Scan(&src, &dst, &kind, &subkind, &found, &basis, &createdBy, &createdAgent, &score, &createdAt)
	if err != nil {
		return "", nil, err
	}
	direction, otherID := "source", dst
	if src != i.ID {
		direction, otherID = "target", src
	}
	other, err := s.issueByID(ctx, t, otherID, false)
	if err != nil {
		return "", nil, err
	}
	said := relationFrom(kind, direction == "target")
	if subkind != "" {
		said += " (" + subkind + ")"
	}
	data := map[string]any{
		"relation_id":   id,
		"kind":          kind,
		"subkind":       subkind,
		"other":         other.Key,
		"direction":     direction,
		"basis":         relationBasisName(basis),
		"found":         found.String,
		"created_agent": createdAgent,
		"created_at":    createdAt,
		"age_ms":        s.nowMS() - createdAt,
	}
	if createdBy.Valid {
		data["created_by"] = createdBy.Int64
	}
	if score.Valid {
		data["score"] = score.Float64
	}
	if by != "" {
		data["by"] = by
	}
	if reason != "" {
		data["reason"] = reason
	}
	return said + " " + other.Key, data, nil
}

// relationHolder is the issue a removed relation's event is recorded on: i,
// unless the other end is a draft and i isn't. A draft's key stays out of a
// published issue's history (WP-42), so the removal goes on the draft, as the
// add path records it. An id i has no relation by gives i (relationGone then
// says so).
func (s *Service) relationHolder(ctx context.Context, t *tx, i *Issue, id int64) (*Issue, error) {
	var other int64
	err := t.QueryRowContext(ctx, `SELECT CASE source_id WHEN ? THEN target_id ELSE source_id END FROM issue_relations
		WHERE id = ? AND (source_id = ? OR target_id = ?)`, i.ID, id, i.ID, i.ID).Scan(&other)
	if errors.Is(err, sql.ErrNoRows) {
		return i, nil
	}
	if err != nil {
		return nil, err
	}
	o, err := s.issueByID(ctx, t, other, false)
	if err != nil {
		return nil, err
	}
	holder, _ := relationEventHome(i, o)
	return holder, nil
}

// relationEventHome is where the event of a relation between i and o is
// recorded and which one it names: on i naming o, unless o is a draft and i
// isn't, when it goes on the draft naming i. A draft's key stays out of a
// published issue's history (WP-42). Every path that records a relation event
// (relate, a removal, NoteRewrites) asks this, so none can differ.
func relationEventHome(i, o *Issue) (holder, named *Issue) {
	if o.Draft && !i.Draft {
		return o, i
	}
	return i, o
}
