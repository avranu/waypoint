package core

import (
	"context"
	"encoding/json"
	"strings"
)

// StallPolicy is when a session is told that its background job has made
// no progress. Times are minutes.
type StallPolicy struct {
	// Off: sessions aren't told; runners still report, so the spells are
	// kept for the figures.
	Off bool `json:"off"`
	// MinAge: jobs younger than this aren't told about.
	MinAge int `json:"min_age"`
	// Quiet: no progress this long is a stall. The person's floor: the
	// evidence may make it longer for a kind of job (Fixed turns that off),
	// never shorter.
	Quiet int `json:"quiet"`
	// SkipTimeout: a job whose whole command runs under a timeout shorter
	// than this ends soon enough by itself; 0 skips none.
	SkipTimeout int `json:"skip_timeout"`
	// Early: when the session is about to be kept warm anyway (a keep-warm
	// ping is due), a job quiet this long is told about then, in place of
	// the ping, rather than after its cache cools. 0: never early.
	Early int `json:"early"`
	// ColdWake: a session whose cache is cold is woken once its job has made
	// no progress this long; until then the notice waits for its next turn.
	// 0: a cold session is never woken for it.
	ColdWake int `json:"cold_wake"`
	// Fixed: use these times as they are; the evidence doesn't lengthen the
	// quiet time or stop cold wakes.
	Fixed bool `json:"fixed"`
}

// DefaultStallPolicy is the policy until someone sets one: WP-605's times
// (30 minutes old, 15 quiet, timeouts under an hour skipped), told early in
// place of a keep-warm ping from 10 quiet minutes, and a cold session woken
// after an hour without progress.
var DefaultStallPolicy = StallPolicy{MinAge: 30, Quiet: 15, SkipTimeout: 60, Early: 10, ColdWake: 60}

// stallFloor is the least time a runner waits before it reports a quiet job
// (cmd/wp stallFloor): no setting can be shorter.
const (
	stallFloorMin = 5
	stallMaxMin   = 24 * 60
)

func (p *StallPolicy) validate() error {
	in := func(v, lo int) bool { return v >= lo && v <= stallMaxMin }
	switch {
	case !in(p.MinAge, stallFloorMin):
		return invalid("a job's age before it is looked at must be %d-%d minutes", stallFloorMin, stallMaxMin)
	case !in(p.Quiet, stallFloorMin):
		return invalid("the time without progress must be %d-%d minutes", stallFloorMin, stallMaxMin)
	case !in(p.SkipTimeout, 0):
		return invalid("the timeout that skips a job must be 0-%d minutes", stallMaxMin)
	case p.Early != 0 && (p.Early < stallFloorMin || p.Early > p.Quiet):
		return invalid("telling early needs %d minutes without progress at least, and no more than the stall time (%d); 0 turns it off", stallFloorMin, p.Quiet)
	case p.ColdWake != 0 && (p.ColdWake < p.Quiet || p.ColdWake > stallMaxMin):
		return invalid("waking a cold session needs at least the stall time (%d minutes), at most %d; 0 turns it off", p.Quiet, stallMaxMin)
	}
	return nil
}

const stallPolicySetting = "stalled_jobs"

// StallPolicies is the policy for every project (Admin → AI), or as one
// project uses it.
type StallPolicies struct {
	// Policy is what applies.
	Policy StallPolicy `json:"policy"`
	// Default is the built-in policy.
	Default StallPolicy `json:"default"`
	// Custom: set where asked (the project's own for a project, the admins'
	// for every project).
	Custom bool `json:"custom"`
	// Inherited, for a project only: what applies without its own.
	Inherited *StallPolicy `json:"inherited,omitempty"`
}

// globalStallPolicy is the admins' policy, else the default. A failure to
// read it falls back to the default.
func (s *Service) globalStallPolicy(ctx context.Context, q queryer) (StallPolicy, bool) {
	raw, err := s.serverSetting(ctx, q, stallPolicySetting)
	if err != nil {
		s.log.WarnContext(ctx, "stalled jobs: read the policy", "err", err)
		return DefaultStallPolicy, false
	}
	if raw == "" {
		return DefaultStallPolicy, false
	}
	p := DefaultStallPolicy
	if json.Unmarshal([]byte(raw), &p) != nil || p.validate() != nil {
		return DefaultStallPolicy, false
	}
	return p, true
}

// stallPolicyFor is the policy in project pid (0: none): its own, else the
// admins', else the default.
func (s *Service) stallPolicyFor(ctx context.Context, q queryer, pid int64) StallPolicy {
	g, _ := s.globalStallPolicy(ctx, q)
	if pid == 0 {
		return g
	}
	var raw string
	if err := q.QueryRowContext(ctx, `SELECT settings FROM projects WHERE id = ?`, pid).Scan(&raw); err != nil {
		return g
	}
	var st ProjectSettings
	if json.Unmarshal([]byte(raw), &st) != nil || st.StalledJobs == nil || st.StalledJobs.validate() != nil {
		return g
	}
	return *st.StalledJobs
}

// GetStallPolicy is the policy for every project; any signed-in person may
// read it.
func (s *Service) GetStallPolicy(ctx context.Context, actor *Actor) (*StallPolicies, error) {
	if actor == nil || actor.User == nil {
		return nil, forbidden("sign in to see when stalled jobs are told")
	}
	p, custom := s.globalStallPolicy(ctx, s.db.R)
	return &StallPolicies{Policy: p, Default: DefaultStallPolicy, Custom: custom}, nil
}

// ProjectStallPolicy is the policy a project uses, with what it inherits.
func (s *Service) ProjectStallPolicy(ctx context.Context, actor *Actor, project string) (*StallPolicies, error) {
	p, err := s.GetProject(ctx, actor, project)
	if err != nil {
		return nil, err
	}
	g, err := s.GetStallPolicy(ctx, actor)
	if err != nil {
		return nil, err
	}
	out := &StallPolicies{Policy: g.Policy, Default: DefaultStallPolicy, Inherited: &g.Policy}
	if own := p.Settings.StalledJobs; own != nil && own.validate() == nil {
		out.Policy, out.Custom = *own, true
	}
	return out, nil
}

// SetStallPolicy sets the policy for every project (admins, people only);
// nil goes back to the default.
func (s *Service) SetStallPolicy(ctx context.Context, actor *Actor, in *StallPolicy) (*StallPolicies, error) {
	if !actor.IsAdmin() {
		return nil, forbidden("only admins can set when stalled jobs are told for every project")
	}
	if err := actor.RequireHuman("setting when stalled jobs are told"); err != nil {
		return nil, err
	}
	val := ""
	if in != nil && *in != DefaultStallPolicy {
		if err := in.validate(); err != nil {
			return nil, err
		}
		b, _ := json.Marshal(in)
		val = string(b)
	}
	err := s.write(ctx, actor, func(t *tx) error {
		before, err := s.serverSetting(ctx, t, stallPolicySetting)
		if err != nil || before == val {
			return err
		}
		if val == "" {
			_, err = t.ExecContext(ctx, `DELETE FROM server_settings WHERE key = ?`, stallPolicySetting)
		} else {
			_, err = t.ExecContext(ctx, `INSERT INTO server_settings(key, value) VALUES (?, ?)
				ON CONFLICT(key) DO UPDATE SET value = excluded.value`, stallPolicySetting, val)
		}
		if err != nil {
			return err
		}
		detail := "set when stalled background jobs are told back to the default"
		if val != "" {
			detail = "set when stalled background jobs are told: " + in.describe()
		}
		t.audit(AuditEntry{Action: "settings.stalled_jobs", Category: AuditSettings, TargetType: "setting",
			Target: "Stalled background jobs", URL: "/admin?tab=ai", Detail: detail})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetStallPolicy(ctx, actor)
}

// describe says a policy in a few words, for the audit log.
func (p *StallPolicy) describe() string {
	if p.Off {
		return "off"
	}
	parts := []string{
		"jobs " + minText(p.MinAge) + " old",
		minText(p.Quiet) + " without progress",
	}
	if p.SkipTimeout > 0 {
		parts = append(parts, "timeouts under "+minText(p.SkipTimeout)+" skipped")
	}
	if p.Early > 0 {
		parts = append(parts, "early from "+minText(p.Early)+" in place of a keep-warm ping")
	}
	if p.ColdWake > 0 {
		parts = append(parts, "cold sessions woken after "+minText(p.ColdWake))
	} else {
		parts = append(parts, "cold sessions never woken")
	}
	if p.Fixed {
		parts = append(parts, "times fixed")
	}
	return strings.Join(parts, ", ")
}

func minText(m int) string {
	if m%60 == 0 && m >= 60 {
		if m == 60 {
			return "1 h"
		}
		return itoa(m/60) + " h"
	}
	return itoa(m) + " min"
}
