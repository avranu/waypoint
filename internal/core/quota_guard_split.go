package core

import (
	"context"
	"math"
	"sort"
)

// The two ways a guard figure is made.
const (
	GuardPathStored = "stored" // the daily check, kept when it ran
	GuardPathNow    = "now"    // computed for this request
)

// Sources of a part of the guard's dollars.
const (
	GuardSourceCLI    = "cli"    // a Claude Code session, as `wp usage sync` reported its hours
	GuardSourceRunner = "runner" // a runner job's own cost
)

// QuotaGuardPart is some of a check's dollars from one source. Machine
// names the computer that reported a CLI session's hours; "" is a row that
// names none (an old row, or an upload that did not say), shown as "machine
// unknown".
type QuotaGuardPart struct {
	Source  string  `json:"source"`
	Machine string  `json:"machine,omitempty"`
	CostUSD float64 `json:"cost_usd"`
}

// QuotaGuardSplit is a check's cost_usd by source, recomputed from the
// usage hours and runner jobs when the report is read, at today's prices,
// so a stored check whose usage or prices have changed since shows the
// difference as RemainderUSD (the check's cost_usd less the parts) rather
// than hiding it.
type QuotaGuardSplit struct {
	Path         string           `json:"path"` // always "recomputed"
	Parts        []QuotaGuardPart `json:"parts"`
	PartsUSD     float64          `json:"parts_usd"`
	RemainderUSD float64          `json:"remainder_usd"`
}

// guardSplitInto fills c.Split and each hour's split.
func (s *Service) guardSplitInto(ctx context.Context, uid int64, c *QuotaGuardCheck) error {
	from, to := c.From.UnixMilli(), c.To.UnixMilli()
	hours, err := s.guardSpending(ctx, uid, from, to, 0, 0)
	if err != nil {
		return err
	}
	total := map[QuotaGuardPart]float64{}
	for i := range c.Hours {
		g := hours[c.Hours[i].Hour.UnixMilli()]
		if g == nil {
			continue
		}
		for _, p := range guardParts(g) {
			total[QuotaGuardPart{Source: p.Source, Machine: p.Machine}] += p.CostUSD
			p.CostUSD = round2(p.CostUSD)
			if p.CostUSD != 0 {
				c.Hours[i].Split = append(c.Hours[i].Split, p)
			}
		}
	}
	sp := &QuotaGuardSplit{Path: "recomputed", Parts: []QuotaGuardPart{}}
	for k, v := range total {
		k.CostUSD = round2(v)
		if k.CostUSD != 0 {
			sp.Parts = append(sp.Parts, k)
			sp.PartsUSD += k.CostUSD
		}
	}
	sort.Slice(sp.Parts, func(i, j int) bool {
		a, b := sp.Parts[i], sp.Parts[j]
		if a.CostUSD != b.CostUSD {
			return a.CostUSD > b.CostUSD
		}
		return a.Source+a.Machine < b.Source+b.Machine
	})
	sp.PartsUSD = round2(sp.PartsUSD)
	sp.RemainderUSD = math.Round((c.CostUSD-sp.PartsUSD)*100) / 100
	if math.Abs(sp.RemainderUSD) < 0.005 {
		sp.RemainderUSD = 0
	}
	c.Split = sp
	return nil
}

// guardParts is one hour's cost by source, machines in name order.
func guardParts(g *guardHour) []QuotaGuardPart {
	var out []QuotaGuardPart
	for m, v := range g.machines {
		out = append(out, QuotaGuardPart{Source: GuardSourceCLI, Machine: m, CostUSD: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Machine < out[j].Machine })
	if g.jobs != 0 {
		out = append(out, QuotaGuardPart{Source: GuardSourceRunner, CostUSD: g.jobs})
	}
	return out
}
