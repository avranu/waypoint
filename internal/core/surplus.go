package core

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// Surplus work (WP-906, WP-ADR89): the gateway's "surplus" priority
// serves a request only with capacity that would
// otherwise go unused, and refuses it rather than take quota other work
// needs. Waypoint sends it optional work only, work worth having on its own
// that nothing stronger checks:
//
//   - prep ahead: tasks behind the front of an agent queue are prepared too,
//     so a prep is ready when one reaches the front (prep_ahead.go);
//   - prep trials: another model's picks on a prep's bundle, never shown,
//     scored when the task's work merges and reported to the gateway, so
//     models untested at prep get rated there (prep_models.go).
//
// A refusal is no model's answer and nobody's fault: the request isn't
// counted against a model, surplus work waits for the gateway's
// Retry-After (or surplusRetry), and normal work goes on as before.

const (
	prioritySurplus = "surplus"
	// surplusRetry is how long surplus work waits after a refusal that
	// named no Retry-After.
	surplusRetry = 15 * time.Minute
)

// priorityOf is the routing.priority ctx's gateway requests are sent at.
func priorityOf(ctx context.Context) string {
	return gatewayRoutingOf(ctx).priorityOrNone()
}

func (r *gatewayRouting) priorityOrNone() string {
	if r == nil {
		return ""
	}
	return r.priority
}

// surplusRouting is the routing of surplus work for task, its requests
// named prefix-….
func surplusRouting(prefix string) *gatewayRouting {
	return &gatewayRouting{prefix: prefix, paid: "never", priority: prioritySurplus}
}

// surplusRefusal says whether err is the gateway not serving a surplus
// request: any HTTP answer but a key's spent day (which is every request's
// wait) and a request too large. Under surplus the gateway may refuse with a
// 503 (no_surplus), a 429, a 403 (a key that may not ask for it), or a 4xx
// when every model was left out; none of them says a model answered.
func surplusRefusal(err error) bool {
	var le *llmError
	if !errors.As(err, &le) || le.dailyQuota() {
		return false
	}
	return le.status >= 400 && le.status != 413
}

// noteSurplusRefusal holds every surplus request to provider pid until the
// gateway's Retry-After, else for surplusRetry; normal requests aren't held.
func (s *Service) noteSurplusRefusal(pid int64, err error) *aiWaitError {
	wait := surplusRetry
	var le *llmError
	if errors.As(err, &le) && le.retryAfter > 0 && le.retryAfter <= maxGatewayWait {
		wait = le.retryAfter
	}
	until := s.now().Add(wait)
	s.parkAsk(pid, "", prioritySurplus, "surplus", until)
	return &aiWaitError{until: until, why: "surplus"}
}

// surplusAllowed says whether surplus work may be sent now: a gateway with
// routes and routing (its priority) is set up, it hasn't refused surplus
// work lately, and it hasn't asked any normal request to wait (free
// capacity is short then, whatever it would say).
func (s *Service) surplusAllowed(ctx context.Context) bool {
	ok, _, err := s.surplusState(ctx)
	if err != nil {
		return false // logged there: surplus work is held while the gateway can't be read
	}
	return ok
}

// surplusState is surplusAllowed with the reason it is not, and the error
// when the gateway could not be looked up: that is not "none is set up", and
// a failed lookup is (false, "", err).
func (s *Service) surplusState(ctx context.Context) (allowed bool, why string, err error) {
	pid, err := s.gatewayWithErr(ctx, GatewayRoutes, GatewayRouting)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			s.log.WarnContext(ctx, "read the gateway setting", "err", err)
		}
		return false, "", err
	case pid == 0:
		return false, "no gateway that takes routes and routing is set up for surplus work", nil
	case s.normalWaits(pid):
		return false, "the gateway has asked normal requests to wait, so there is no spare capacity", nil
	case s.askParked(pid, "", prioritySurplus) != nil:
		return false, "the gateway refused surplus work lately", nil
	}
	return true, "", nil
}

// SurplusReport is `wp ai surplus`: what surplus work did and what the
// evidence says about it.
type SurplusReport struct {
	Allowed bool            `json:"allowed"` // surplus work may be sent now
	Calls   []SurplusCalls  `json:"calls"`   // the last day's surplus requests
	Depth   int             `json:"depth"`   // tasks behind each queue's front prepared ahead
	Ahead   []prepAheadUse  `json:"ahead"`   // by position, over prepAheadWindow
	Models  []PrepModelStat `json:"models"`  // what was measured and reported, by model
	Agent   *PrepAgentPairs `json:"agent"`   // agentic prep trials against chat preps (WP-ADR104)
	// Policy is which pointers agents are given, and the evidence for it.
	Policy *PrepPolicyReport `json:"policy"`
}

// SurplusCalls counts one task's surplus requests by outcome.
type SurplusCalls struct {
	Task     string `json:"task"`
	OK       int    `json:"ok"`
	Refused  int    `json:"refused"`
	Failed   int    `json:"failed"`
	TokensIn int64  `json:"tokens_in"`
	Out      int64  `json:"tokens_out"`
}

// SurplusReport reads it (admins: the server's own record).
func (s *Service) SurplusReport(ctx context.Context, a *Actor) (*SurplusReport, error) {
	if err := a.canReadServer(); err != nil {
		return nil, err
	}
	rep := &SurplusReport{Allowed: s.surplusAllowed(ctx)}
	rows, err := s.db.R.QueryContext(ctx, `SELECT task, outcome, COUNT(*), SUM(tokens_in), SUM(tokens_out) FROM ai_calls
		WHERE priority = ? AND at >= ? GROUP BY task, outcome ORDER BY task`, prioritySurplus, s.now().Add(-24*time.Hour).UnixMilli())
	if err != nil {
		return nil, err
	}
	by := map[string]*SurplusCalls{}
	for rows.Next() {
		var task, outcome string
		var n int
		var in, out int64
		if err := rows.Scan(&task, &outcome, &n, &in, &out); err != nil {
			rows.Close()
			return nil, err
		}
		c := by[task]
		if c == nil {
			c = &SurplusCalls{Task: task}
			by[task] = c
		}
		switch outcome {
		case "ok":
			c.OK += n
		case "refused":
			c.Refused += n
		default:
			c.Failed += n
		}
		c.TokensIn += in
		c.Out += out
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rep.Calls = []SurplusCalls{}
	for _, c := range by {
		rep.Calls = append(rep.Calls, *c)
	}
	slices.SortFunc(rep.Calls, func(x, y SurplusCalls) int { return strings.Compare(x.Task, y.Task) })
	if rep.Ahead, err = s.prepAheadUses(ctx); err != nil {
		return nil, err
	}
	rep.Depth = aheadDepth(rep.Ahead)
	if rep.Models, err = s.PrepModelStats(ctx); err != nil {
		return nil, err
	}
	if rep.Agent, err = s.prepAgentPairs(ctx); err != nil {
		return nil, err
	}
	if rep.Policy, err = s.prepPolicyReport(ctx); err != nil {
		return nil, err
	}
	return rep, nil
}

func (r *SurplusReport) String() string {
	var b strings.Builder
	if r.Allowed {
		b.WriteString("Surplus work: sent now (a gateway with routes and routing, no waits).\n")
	} else {
		b.WriteString("Surplus work: held now (no gateway with routes and routing, a refusal's wait, or normal requests waiting).\n")
	}
	if len(r.Calls) == 0 {
		b.WriteString("\nNo surplus requests in the last 24h.\n")
	} else {
		b.WriteString("\nLast 24h (refused = no spare capacity: no model answered):\n")
		for _, c := range r.Calls {
			fmt.Fprintf(&b, "  %-12s %d ok, %d refused, %d failed; %d tokens in, %d out\n", c.Task, c.OK, c.Refused, c.Failed, c.TokensIn, c.Out)
		}
	}
	fmt.Fprintf(&b, "\nPrep ahead, last %d days: depth %d (a position stops at %d+ decided with use under %.0f%% at 95%%).\n",
		int(prepAheadWindow.Hours()/24), r.Depth, prepAheadMinN, prepAheadUseBar*100)
	for _, u := range r.Ahead {
		decided := u.Used + u.Redone + u.Unused
		if decided+u.Open+u.Refused+u.Failed == 0 {
			fmt.Fprintf(&b, "  +%d  no preps yet\n", u.Position)
			continue
		}
		fmt.Fprintf(&b, "  +%d  used %d of %d (%.0f-%.0f%%); redone %d, unused %d; open %d; refused %d, failed %d",
			u.Position, u.Used, decided, u.UseLow*100, u.UseHigh*100, u.Redone, u.Unused, u.Open, u.Refused, u.Failed)
		if u.Dropped {
			b.WriteString(" — dropped")
		}
		b.WriteByte('\n')
	}
	b.WriteString("\nPrep picks reported to the gateway (verified = files the merged work changed, tests and docs left out;\n" +
		"the ratings are the gateway's, by its prep purpose: Admin → Purposes there):\n")
	if len(r.Models) == 0 {
		b.WriteString("  nothing yet: preps are scored when their task is done\n")
	}
	for _, m := range r.Models {
		if m.Scored == 0 {
			fmt.Fprintf(&b, "  %-40s %d trials wait for their tasks to finish\n", m.Model, m.Waiting)
			continue
		}
		fmt.Fprintf(&b, "  %-40s %d scored: %d verified, %d false positives, %d missed; %d reported, %d to send",
			m.Model, m.Scored, m.Verified, m.FalsePositives, m.Missed, m.Reported, m.Pending)
		if m.Waiting > 0 {
			fmt.Fprintf(&b, "; %d trials waiting", m.Waiting)
		}
		if m.Tested {
			b.WriteString(" — tested, no more trials")
		}
		b.WriteByte('\n')
	}
	if r.Agent != nil {
		b.WriteString(r.Agent.String())
	}
	if r.Policy != nil {
		b.WriteString(r.Policy.String())
	}
	return b.String()
}

// ---- confidence ----

// wilsonUpper is the upper end of the 95% Wilson interval of k successes in
// n tries (1 when n is 0).
func wilsonUpper(k, n int) float64 {
	if n <= 0 {
		return 1
	}
	const z = 1.96
	p := float64(k) / float64(n)
	nn := float64(n)
	d := 1 + z*z/nn
	c := p + z*z/(2*nn)
	m := z * math.Sqrt(p*(1-p)/nn+z*z/(4*nn*nn))
	return math.Min(1, (c+m)/d)
}

// wilsonLower is the lower end of that interval (0 when n is 0).
func wilsonLower(k, n int) float64 {
	if n <= 0 {
		return 0
	}
	const z = 1.96
	p := float64(k) / float64(n)
	nn := float64(n)
	d := 1 + z*z/nn
	c := p + z*z/(2*nn)
	m := z * math.Sqrt(p*(1-p)/nn+z*z/(4*nn*nn))
	return math.Max(0, (c-m)/d)
}
