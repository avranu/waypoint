package main

import (
	"fmt"
	"sort"
	"strings"
)

func whMoney(x float64) string { return fmt.Sprintf("$%.2f", x) }

func whPct(x, of float64) string {
	if of <= 0 {
		return "  -"
	}
	return fmt.Sprintf("%2.0f%%", 100*x/of)
}

func whTokens(n int64) string {
	if n >= 1000 {
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprint(n)
}

func whMaxRows[T any](rows []T, n int) []T {
	if n > 0 && len(rows) > n {
		return rows[:n]
	}
	return rows
}

// whereText is the compact report; --json carries all of it.
func whereText(r *whReport) string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	p("Spend %s to %s (list price): %s, %d calls, %d sessions", r.Since, r.Until, whMoney(r.Cost), r.Calls, r.Sessions)
	p("Cost parts: cache read %s, cache write 5m %s, 1h %s, output %s, uncached input %s",
		whMoney(r.Parts.Read), whMoney(r.Parts.Write5m), whMoney(r.Parts.Write1h), whMoney(r.Parts.Output), whMoney(r.Parts.Input))

	p("\nBy model (calls, median context)")
	for _, m := range r.Models {
		p("  %-26s %10s %5s  %6d calls  %5s ctx  [read %s, write %s, out %s, in %s]", m.Model, whMoney(m.Cost), whPct(m.Cost, r.Cost), m.Calls, whTokens(m.MedianCtx),
			whMoney(m.Read), whMoney(m.Write5m+m.Write1h), whMoney(m.Output), whMoney(m.Input))
	}

	p("\nBy agent (runs; median / max cost per run)")
	for _, a := range whMaxRows(r.Agents, 10) {
		p("  %-26s %10s %5s  %5d runs  %8s / %s", a.Agent, whMoney(a.Cost), whPct(a.Cost, r.Cost), a.Runs, whMoney(a.Median), whMoney(a.Max))
	}
	p("By project")
	for _, g := range whMaxRows(r.Projects, 8) {
		p("  %-26s %10s %5s  %4d sessions", truncRunes(g.Name, 26), whMoney(g.Cost), whPct(g.Cost, r.Cost), g.Sessions)
	}
	p("By how the session started")
	for _, g := range r.Launches {
		p("  %-26s %10s %5s  %4d sessions", g.Name, whMoney(g.Cost), whPct(g.Cost, r.Cost), g.Sessions)
	}

	for _, a := range r.Activity {
		p("\nWhat %s main-thread calls did (%s, %d calls; a call's cost split over its tool blocks)", a.Model, whMoney(a.Cost), a.Calls)
		for _, k := range a.Classes {
			p("  %-14s %10s %5s  %6d calls", k.Class, whMoney(k.Cost), whPct(k.Cost, a.Cost), k.Calls)
		}
	}

	p("\nCostliest commands in %s (a block's cost split over its commands; --json has all)", r.Commands.Model)
	for _, c := range whMaxRows(r.Commands.Commands, r.TopN) {
		p("  %-26s %-13s %9s  %6d calls", truncRunes(c.Head, 26), c.Class, whMoney(c.Cost), c.Calls)
	}
	if len(r.Commands.OtherTools) > 0 {
		var parts []string
		for _, c := range whMaxRows(r.Commands.OtherTools, 5) {
			parts = append(parts, fmt.Sprintf("%s %s (%d)", c.Head, whMoney(c.Cost), c.Calls))
		}
		p("  tools other than Bash in \"other\": %s", strings.Join(parts, ", "))
	}

	p("\nWhat started each call")
	for _, t := range whMaxRows(r.Triggers, 10) {
		p("  %-34s %10s %5s  %6d calls", truncRunes(t.Trigger, 34), whMoney(t.Cost), whPct(t.Cost, r.Cost), t.Calls)
		for _, d := range whMaxRows(t.Detail, 3) {
			p("      %-30s %10s %5s  %6d calls", truncRunes(d.Trigger, 30), whMoney(d.Cost), whPct(d.Cost, t.Cost), d.Calls)
		}
	}

	ph := r.Phase
	p("\nBefore and after a session's first wp ship (main threads)")
	p("  %d sessions shipped (%d ships; %d of them first shipped before the window): %s before the first, %s after (%s of their spend)", ph.Shipped, ph.Ships, ph.BeforeWindow, whMoney(ph.Before), whMoney(ph.After), strings.TrimSpace(whPct(ph.After, ph.Before+ph.After)))
	p("  %d sessions without a ship: %s", ph.NoShipSession, whMoney(ph.NoShipCost))
	for _, s := range whMaxRows(ph.Sessions, 5) {
		note := ""
		if s.FirstShipBeforeWindow {
			note = "  (first ship before the window)"
		}
		p("  %s %-14s %2d ships  before %s  after %s%s", s.Session, truncRunes(s.Project, 14), s.Ships, whMoney(s.Before), whMoney(s.After), note)
	}

	p("\nTop %d sessions", len(r.Top))
	for _, s := range r.Top {
		p("  %s %-20s %9s (main %s) %5d calls %2d ships  %s %s ctx  %s", s.Session, truncRunes(s.Project, 20), whMoney(s.Cost), whMoney(s.Main), s.Calls, s.Ships, s.Model, whTokens(s.AvgCtx), s.Prompt)
	}

	p("\nLimit events")
	if len(r.Limits.Events) == 0 {
		p("  none in the window")
	}
	for _, e := range r.Limits.Events {
		p("  %s session %s: %s", e.TS, shortID(e.Session), e.Text)
	}
	if len(r.Limits.Events) > 0 {
		p("  spend from the first one to the end of the window: %s", whMoney(r.Limits.SpendAfter))
	}

	n := r.NotKnown
	p("\nNot counted or not known")
	p("  %d <synthetic> records (no usage) left out; they ran %s", n.Synthetic, whClassCounts(n.ScriptTools))
	p("  %d compaction boundaries: their model calls carry no usage in transcripts", n.Compactions)
	p("  %d of %d files unreadable", n.UnreadableCnt, n.FilesRead)
	if len(n.AssumedPrices) > 0 {
		p("  no price is known for %s: costed at the dearest model's", strings.Join(n.AssumedPrices, ", "))
	}
	for _, line := range n.Notes {
		if !strings.HasPrefix(line, "<synthetic>") && !strings.HasPrefix(line, "compaction") {
			p("  %s", line)
		}
	}
	return b.String()
}

func whClassCounts(m map[string]int) string {
	if len(m) == 0 {
		return "no tool blocks"
	}
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool {
		return m[ks[i]] > m[ks[j]] || m[ks[i]] == m[ks[j]] && ks[i] < ks[j]
	})
	var parts []string
	for _, k := range ks {
		parts = append(parts, fmt.Sprintf("%d %s", m[k], k))
	}
	return "tool blocks: " + strings.Join(parts, ", ")
}
