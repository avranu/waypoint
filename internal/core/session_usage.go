package core

import "context"

// SessionUsage is what one agent session used, from the transcripts `wp
// usage sync` reads (WP-D20): tokens by model, the cache, which agents and
// tools, the Bash command heads and what was refused. It is the owner's
// data; others see that there is none for them.
type SessionUsage struct {
	HasData   bool             `json:"has_data"`
	Private   bool             `json:"private,omitempty"` // it is another person's session
	Totals    UsageTotals      `json:"totals"`
	Models    []UsageBreakdown `json:"models"`
	Kinds     []UsageBreakdown `json:"kinds"` // the session itself and its subagents
	Tools     []UsageTool      `json:"tools"`
	Bash      []UsageTool      `json:"bash"`
	Refusals  UsageRefusals    `json:"refusals"`
	Truncated bool             `json:"truncated,omitempty"`
}

// GetSessionUsage reads the usage of one session (a session row Waypoint
// holds), joining the usage tables by person, machine and CLI session id.
func (s *Service) GetSessionUsage(ctx context.Context, actor *Actor, id int64) (*SessionUsage, error) {
	x, err := s.sessionByID(ctx, s.db.R, actor, id)
	if err != nil {
		return nil, err
	}
	out := &SessionUsage{Models: []UsageBreakdown{}, Kinds: []UsageBreakdown{}, Tools: []UsageTool{}, Bash: []UsageTool{},
		Refusals: UsageRefusals{Reasons: []UsageReason{}, Daily: []UsageCountDay{}, Commands: []UsageRefusedCommand{}, Recent: []UsageRefusalRow{}}}
	who := ""
	if x.userID != actor.id() {
		if !actor.isAdmin() || actor.IsAgent() {
			out.Private = true
			return out, nil
		}
		if who, err = s.usernameOf(ctx, x.userID); err != nil {
			return nil, err
		}
	}
	days := int(s.now().Sub(x.StartedAt).Hours()/24) + 2
	q := UsageQuery{Days: min(days, 365), Machine: x.Machine, Session: x.CLIID, User: who}
	sc, loc, first, _, err := s.usageScopeFor(ctx, actor, &q)
	if err != nil {
		return nil, err
	}
	book, _, err := s.loadPrices(ctx)
	if err != nil {
		return nil, err
	}
	b, err := s.usageTokens(ctx, sc, book, loc)
	if err != nil {
		return nil, err
	}
	o := &UsageOverview{From: first.UTC(), Days: q.Days, Tools: []UsageTool{}, Bash: []UsageTool{}}
	b.fill(o)
	if err := s.usageToolsInto(ctx, sc, o); err != nil {
		return nil, err
	}
	if err := s.usageRefusalsInto(ctx, sc, loc, o); err != nil {
		return nil, err
	}
	out.Totals, out.Models, out.Kinds, out.Tools, out.Bash, out.Refusals = o.Totals, o.Models, o.Kinds, o.Tools, o.Bash, o.Refusals
	out.HasData = o.Totals.Messages > 0 || len(o.Tools) > 0 || len(o.Bash) > 0 || o.Refusals.Total > 0
	return out, nil
}

func (s *Service) usernameOf(ctx context.Context, id int64) (string, error) {
	var name string
	err := s.db.R.QueryRowContext(ctx, `SELECT username FROM users WHERE id = ?`, id).Scan(&name)
	return name, err
}
