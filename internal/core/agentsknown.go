package core

import (
	"context"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Which agents exist (WP-D15): every identity that has worked on, held or
// may sign in for work the actor can see, so pickers and filters offer real
// names and show what a spec like "claude" covers.

// KnownAgent is an agent identity seen at work.
type KnownAgent struct {
	Identity string `json:"identity"` // provider/model, as agents say it
	Provider string `json:"provider"` // anthropic, openai, … ('' when the identity has none)
	Family   string `json:"family"`   // the model without its version: claude-opus, claude-code
	// Models are the models its runs reported (Claude Code says which model
	// a session used; the identity may not).
	Models   []string   `json:"models"`
	Runs     int        `json:"runs"`
	Holding  int        `json:"holding"`   // tasks it has claimed now
	Token    bool       `json:"token"`     // one of the actor's agent tokens is this identity
	LastSeen *time.Time `json:"last_seen"` // its latest run or claim
}

// versionTail is a model id's version: -5-5, -4.1, -20250514, -v2, -latest.
var versionTail = lazyre.New(`(?i)([-_.@ ]+(v?\d[\d.]*|latest|preview|exp))+$`)

// AgentFamily is an identity's model without provider or version:
// "anthropic/claude-opus-5-5" → "claude-opus".
func AgentFamily(identity string) string {
	m := identity
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	if f := versionTail.ReplaceAllString(m, ""); f != "" {
		return strings.ToLower(f)
	}
	return strings.ToLower(m)
}

// agentProvider is the part of an identity before its slash.
func agentProvider(identity string) string {
	if p, _, ok := strings.Cut(identity, "/"); ok {
		return strings.ToLower(p)
	}
	return ""
}

// KnownAgents lists the agents that worked on or hold work the actor can
// see, and the actor's own agent tokens, most recently active first.
func (s *Service) KnownAgents(ctx context.Context, actor *Actor) ([]*KnownAgent, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	vis, vargs := visibleSQL("i.project_id", actor)
	now := s.nowMS()
	// #nosec G202 -- vis is a fixed condition; values are bound
	rows, err := s.db.R.QueryContext(ctx, `
		SELECT r.agent, r.model, count(*), max(coalesce(r.ended_at, r.started_at)), 0
			FROM agent_runs r JOIN issues i ON i.id = r.issue_id WHERE `+vis+` GROUP BY r.agent, r.model
		UNION ALL
		SELECT i.claimed_by, '', 0, max(coalesce(i.claimed_at, 0)), count(*)
			FROM issues i WHERE i.claimed_by != '' AND i.claimed_until > ? AND `+vis+` GROUP BY i.claimed_by
		UNION ALL
		SELECT agent, '', 0, 0, 0 FROM api_tokens WHERE user_id = ? AND agent != '' AND runner_job_id IS NULL`,
		append(append(append(slices.Clone(vargs), now), vargs...), actor.id())...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[string]*KnownAgent{}
	var order []string
	tokens := map[string]bool{}
	for rows.Next() {
		var id, model string
		var runs, holding int
		var last int64
		if err := rows.Scan(&id, &model, &runs, &last, &holding); err != nil {
			return nil, err
		}
		if strings.TrimSpace(id) == "" {
			continue
		}
		key := normalizeAgent(id)
		a := byID[key]
		if a == nil {
			a = &KnownAgent{Identity: id, Provider: agentProvider(id), Family: AgentFamily(id), Models: []string{}}
			byID[key] = a
			order = append(order, key)
		}
		if runs == 0 && holding == 0 && last == 0 {
			tokens[key] = true
		}
		a.Runs += runs
		a.Holding += holding
		if model != "" && !slices.Contains(a.Models, model) {
			a.Models = append(a.Models, model)
		}
		if last > 0 && (a.LastSeen == nil || a.LastSeen.UnixMilli() < last) {
			t := msTime(last)
			a.LastSeen = &t
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]*KnownAgent, 0, len(order))
	for _, k := range order {
		a := byID[k]
		a.Token = tokens[k]
		out = append(out, a)
	}
	slices.SortStableFunc(out, func(a, b *KnownAgent) int {
		switch {
		case a.LastSeen == nil && b.LastSeen == nil:
			return strings.Compare(a.Identity, b.Identity)
		case a.LastSeen == nil:
			return 1
		case b.LastSeen == nil:
			return -1
		}
		return b.LastSeen.Compare(*a.LastSeen)
	})
	return out, nil
}
