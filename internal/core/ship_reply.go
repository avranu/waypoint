package core

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// The ship reply (WP-390): the template wp ship prints for an agent's last
// message, so wrap-ups say what shipped and stop. Admins word it under
// Admin → AI. wp ship fills in {task} (the issue key, else the pull
// request's title), {pr} (its number) and {url}.

// DefaultShipReply is the template until an admin words their own.
const DefaultShipReply = `Reply template (adapt it where it doesn't fit):
  {task} shipped in PR #{pr} ({url}): <what it does, one or two sentences>.
  Changes from the task: <what you did differently, or "none">. Deferred: <task keys for what isn't done here, or "nothing">.
  Tests: <the tests that cover it>.
  The session can be cleared without losing work.`

const maxShipReply = 2000

// ShipReply is the template as set, with the default it falls back to.
type ShipReply struct {
	Reply   string `json:"reply"`
	Default string `json:"default"`
	Custom  bool   `json:"custom"` // an admin worded it
}

func (s *Service) customShipReply(ctx context.Context) (string, error) {
	var v string
	err := s.db.R.QueryRowContext(ctx, `SELECT value FROM server_settings WHERE key = 'ship_reply'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return strings.TrimSpace(v), err
}

// ShipReplyTemplate is the template wp ship hands the agent that opened a
// pull request; it is not secret, so anyone who may open one gets it.
func (s *Service) ShipReplyTemplate(ctx context.Context, actor *Actor) string {
	if actor == nil || actor.User == nil {
		return ""
	}
	if v, err := s.customShipReply(ctx); err == nil && v != "" {
		return v
	}
	return DefaultShipReply
}

// GetShipReply shows the template to an admin.
func (s *Service) GetShipReply(ctx context.Context, actor *Actor) (*ShipReply, error) {
	if !actor.isAdmin() {
		return nil, forbidden("only admins can see the ship reply")
	}
	v, err := s.customShipReply(ctx)
	if err != nil {
		return nil, err
	}
	return &ShipReply{Reply: cmp(v, DefaultShipReply), Default: DefaultShipReply, Custom: v != ""}, nil
}

// SetShipReply words the template (admins, people only); empty, or the
// default itself, goes back to the default.
func (s *Service) SetShipReply(ctx context.Context, actor *Actor, reply string) (*ShipReply, error) {
	if !actor.isAdmin() {
		return nil, forbidden("only admins can word the ship reply")
	}
	if err := actor.requireHuman("wording the ship reply"); err != nil {
		return nil, err
	}
	reply = strings.TrimSpace(strings.ReplaceAll(reply, "\r\n", "\n"))
	switch {
	case len(reply) > maxShipReply:
		return nil, invalid("the ship reply can be at most %d characters", maxShipReply)
	case reply == DefaultShipReply:
		reply = ""
	}
	before, err := s.customShipReply(ctx)
	if err != nil {
		return nil, err
	}
	err = s.write(ctx, actor, func(t *tx) error {
		var err error
		if reply == "" {
			_, err = t.ExecContext(ctx, `DELETE FROM server_settings WHERE key = 'ship_reply'`)
		} else {
			_, err = t.ExecContext(ctx, `INSERT INTO server_settings(key, value) VALUES ('ship_reply', ?)
				ON CONFLICT(key) DO UPDATE SET value = excluded.value`, reply)
		}
		if err != nil {
			return err
		}
		if reply != before {
			detail := "reworded it"
			if reply == "" {
				detail = "set it back to the default"
			}
			t.audit(AuditEntry{Action: "settings.ship_reply", Category: AuditSettings, TargetType: "setting",
				Target: "Ship reply", URL: "/admin?tab=ai", Detail: detail})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetShipReply(ctx, actor)
}
