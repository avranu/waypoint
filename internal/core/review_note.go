package core

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// DefaultReviewNote is the note until an admin words their own.
const DefaultReviewNote = "**Automated review** by {models} · not checked by a person; verify before acting on it."

const maxReviewNote = 500

// ReviewNote is the note as set, with the default it falls back to.
type ReviewNote struct {
	Note    string `json:"note"`
	Default string `json:"default"`
	Custom  bool   `json:"custom"` // an admin worded it
}

// renderReviewNote fills in the models.
func renderReviewNote(note, models string) string {
	if strings.TrimSpace(note) == "" {
		note = DefaultReviewNote
	}
	return strings.ReplaceAll(note, "{models}", models)
}

func (s *Service) customReviewNote(ctx context.Context) (string, error) {
	var v string
	err := s.db.R.QueryRowContext(ctx, `SELECT value FROM server_settings WHERE key = 'review_note'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return strings.TrimSpace(v), err
}

// reviewNote is the note reviews use now.
func (s *Service) reviewNote(ctx context.Context) string {
	if v, err := s.customReviewNote(ctx); err == nil && v != "" {
		return v
	}
	return DefaultReviewNote
}

// GetReviewNote shows the note to an admin.
func (s *Service) GetReviewNote(ctx context.Context, actor *Actor) (*ReviewNote, error) {
	if !actor.IsAdmin() {
		return nil, forbidden("only admins can see the review note")
	}
	v, err := s.customReviewNote(ctx)
	if err != nil {
		return nil, err
	}
	return &ReviewNote{Note: cmp(v, DefaultReviewNote), Default: DefaultReviewNote, Custom: v != ""}, nil
}

// SetReviewNote words the note (admins). It's one line, since it heads a
// review; empty, or the default itself, goes back to the default.
func (s *Service) SetReviewNote(ctx context.Context, actor *Actor, note string) (*ReviewNote, error) {
	if !actor.IsAdmin() {
		return nil, forbidden("only admins can word the review note")
	}
	if err := actor.RequireHuman("wording the review note"); err != nil {
		return nil, err
	}
	note = strings.Join(strings.Fields(note), " ")
	switch {
	case len(note) > maxReviewNote:
		return nil, invalid("the review note can be at most %d characters", maxReviewNote)
	case strings.Contains(note, "<!--"):
		return nil, invalid("the review note can't contain HTML comments: Waypoint marks its reviews with them")
	case note == DefaultReviewNote:
		note = ""
	}
	before := s.reviewNote(ctx)
	err := s.write(ctx, actor, func(t *tx) error {
		var err error
		if note == "" {
			_, err = t.ExecContext(ctx, `DELETE FROM server_settings WHERE key = 'review_note'`)
		} else {
			_, err = t.ExecContext(ctx, `INSERT INTO server_settings(key, value) VALUES ('review_note', ?)
				ON CONFLICT(key) DO UPDATE SET value = excluded.value`, note)
		}
		if err != nil {
			return err
		}
		if after := cmp(note, DefaultReviewNote); after != before {
			detail := "worded it “" + after + "”"
			if note == "" {
				detail = "set it back to the default"
			}
			t.audit(AuditEntry{Action: "settings.review_note", Category: AuditSettings, TargetType: "setting",
				Target: "Review note", URL: "/admin?tab=ai", Detail: detail})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetReviewNote(ctx, actor)
}
