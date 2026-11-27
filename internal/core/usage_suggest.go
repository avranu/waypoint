package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// UsageAction is what a suggestion asks the person to do.
type UsageAction struct {
	Kind    string `json:"kind"` // rule (an allow rule to copy), note
	Text    string `json:"text"`
	Snippet string `json:"snippet,omitempty"` // for a rule: settings.json to merge in
}

// UsageAdvice is one suggestion with its evidence.
type UsageAdvice struct {
	ID       string      `json:"id"`
	Kind     string      `json:"kind"`  // cache, output, model, unused, allow, compaction, quota
	Level    string      `json:"level"` // warn, info
	Title    string      `json:"title"`
	Evidence []string    `json:"evidence"`
	Action   UsageAction `json:"action"`
}

const (
	adviceMinMessages  = 40
	adviceMinInput     = 300_000
	adviceCacheLow     = 0.7
	adviceOutputTokens = 50_000
	adviceOutputAvg    = 6_000
	advicePromptTimes  = 3
	adviceCompactions  = 3
	adviceUnusedDays   = 30
)

// riskyHeads are never offered as allow rules.
var riskyHeads = map[string]bool{"rm": true, "sudo": true, "dd": true, "mkfs": true, "chmod": true, "chown": true, "su": true, "shutdown": true, "reboot": true, "doas": true, "pkexec": true, "shred": true}

// commandWrappers run the command that follows them.
var commandWrappers = map[string]bool{"env": true, "command": true, "builtin": true, "exec": true, "nohup": true, "time": true, "nice": true, "ionice": true, "setsid": true, "xargs": true}

// programName is a command word as the program it runs: no quotes or
// directory (/usr/bin/rm is rm), no .exe, lower case.
func programName(w string) string {
	w = strings.Trim(w, `"'`)
	if i := strings.LastIndexAny(w, `/\`); i >= 0 {
		w = w[i+1:]
	}
	return strings.TrimSuffix(strings.ToLower(w), ".exe")
}

// riskyCommand: does any command in this shell line (after ; | & && and
// newlines, past env assignments and wrappers such as env or xargs) run one
// of riskyHeads, however it is spelled?
func riskyCommand(line string) bool {
	segs := strings.FieldsFunc(line, func(r rune) bool { return strings.ContainsRune(";|&\n(`{", r) })
	for _, seg := range segs {
		for _, w := range strings.Fields(seg) {
			if strings.Contains(w, "=") && !strings.HasPrefix(w, "-") {
				continue
			}
			name := programName(strings.TrimPrefix(w, "$("))
			if commandWrappers[name] || strings.HasPrefix(w, "-") {
				continue
			}
			if riskyHeads[name] {
				return true
			}
			break
		}
	}
	return false
}

func kb(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func tok(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func money(v float64) string {
	if v >= 100 {
		return fmt.Sprintf("$%.0f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}

func plural1(n int, one string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", one)
	}
	return fmt.Sprintf("%d %ss", n, one)
}

func (s *Service) usageSuggest(ctx context.Context, sc *usageScope, book *priceBook, o *UsageOverview, b *usageTokenData) error {
	var out []UsageAdvice
	if a := adviceQuota(o); a != nil {
		out = append(out, *a)
	}
	if a := adviceCache(o, b); a != nil {
		out = append(out, *a)
	}
	if a := adviceModel(book, b); a != nil {
		out = append(out, *a)
	}
	if a := adviceAllow(o); a != nil {
		out = append(out, *a)
	}
	if a := adviceOutput(o); a != nil {
		out = append(out, *a)
	}
	if a, err := s.adviceUnused(ctx, sc); err != nil {
		s.log.WarnContext(ctx, "usage advice: unused tools", "err", err)
	} else if a != nil {
		out = append(out, *a)
	}
	if a, err := s.adviceCompactions(ctx, sc); err != nil {
		s.log.WarnContext(ctx, "usage advice: compactions", "err", err)
	} else if a != nil {
		out = append(out, *a)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Level == "warn" && out[j].Level != "warn" })
	o.Advice = append(o.Advice, out...)
	return nil
}

func adviceQuota(o *UsageOverview) *UsageAdvice {
	if o.Quota == nil {
		return nil
	}
	for _, w := range o.Quota.Windows {
		if w.Span != "seven_day" || w.Status != "will_run_out" || w.RunsOutAt == nil {
			continue
		}
		ev := []string{
			fmt.Sprintf("%.0f%% of the 7-day quota used, where an even pace would be at %.0f%%.", w.Percent, w.Even),
			fmt.Sprintf("At the recent rate it ends at about %.0f%% and runs out around %s (it resets %s).", w.Projected, w.RunsOutAt.UTC().Format("Mon 15:04 UTC"), w.ResetsAt.UTC().Format("Mon 15:04 UTC")),
		}
		if len(o.Models) > 0 && o.Totals.Output+o.Totals.Input+o.Totals.CacheWrite > 0 {
			var total int64
			for _, m := range o.Models {
				total += m.Input + m.Output + m.CacheWrite
			}
			top := o.Models[0]
			ev = append(ev, fmt.Sprintf("%s did %.0f%% of the fresh tokens in this period.", top.Label, 100*float64(top.Input+top.Output+top.CacheWrite)/float64(max(total, 1))))
		}
		return &UsageAdvice{ID: "quota", Kind: "quota", Level: "warn", Title: "On pace to run out of the 7-day quota", Evidence: ev,
			Action: UsageAction{Kind: "note", Text: "Move search and bulk-reading work to a cheaper model (Haiku or Sonnet), end long sessions sooner, or spread the work over the window."}}
	}
	return nil
}

func adviceCache(o *UsageOverview, b *usageTokenData) *UsageAdvice {
	titles := map[string]string{}
	for _, t := range o.TopSess {
		titles[t.Key] = t.Label
	}
	type row struct {
		label string
		n     int64
		hit   float64
		cost  float64
	}
	var rows []row
	var cost float64
	for k, a := range b.session {
		all := a.in + a.cw5 + a.cw1 + a.cr
		if a.n < adviceMinMessages || all < adviceMinInput {
			continue
		}
		hit := float64(a.cr) / float64(all)
		if hit >= adviceCacheLow {
			continue
		}
		_, session, _ := strings.Cut(k, "\x00")
		rows = append(rows, row{cmpOr(titles[session], "Session "+session[:min(8, len(session))]), a.n, hit, a.cost})
		cost += a.cost
	}
	if len(rows) == 0 {
		return nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].cost > rows[j].cost })
	ev := []string{fmt.Sprintf("%s of %d messages or more read under %.0f%% of their input from the cache; together they cost %s.", plural1(len(rows), "long session"), adviceMinMessages, adviceCacheLow*100, money(cost))}
	for _, r := range rows[:min(len(rows), 3)] {
		ev = append(ev, fmt.Sprintf("%s: %d messages, %.0f%% cache hits, %s", r.label, r.n, r.hit*100, money(r.cost)))
	}
	level := "info"
	if rows[0].hit < 0.4 {
		level = "warn"
	}
	return &UsageAdvice{ID: "cache", Kind: "cache", Level: level, Title: "Long sessions with a low cache hit rate", Evidence: ev,
		Action: UsageAction{Kind: "note", Text: "The cache starts over when the prompt changes early (CLAUDE.md edits, tools or MCP servers switching, a new model) or after pauses over five minutes. Keep the start steady and work in one sitting per session."}}
}

func adviceModel(book *priceBook, b *usageTokenData) *UsageAdvice {
	var cost float64
	var n, in, out, cw5, cw1, cr int64
	models := map[string]bool{}
	var kinds = map[string]bool{}
	for k, a := range b.kindModel {
		kind, model := strings.ToLower(k[0]), strings.ToLower(k[1])
		if !strings.Contains(kind, "explor") && !strings.Contains(kind, "search") {
			continue
		}
		if !strings.Contains(model, "opus") && !strings.Contains(model, "fable") && !strings.Contains(model, "mythos") {
			continue
		}
		cost += a.cost
		n += a.n
		in += a.in
		out += a.out
		cw5 += a.cw5
		cw1 += a.cw1
		cr += a.cr
		models[k[1]] = true
		kinds[k[0]] = true
	}
	if n < 10 {
		return nil
	}
	alt, ok := book.find("claude-haiku-5-5")
	altName := "Haiku"
	if !ok {
		return nil
	}
	saving := cost - alt.Cost(in, out, cw5, cw1, cr)
	ev := []string{fmt.Sprintf("%s of search-only subagent work (%s) ran on %s, %s at API prices.", plural1(int(n), "message"), joinSet(kinds), joinSet(models), money(cost))}
	if saving > 0.5 {
		ev = append(ev, fmt.Sprintf("The same tokens on %s would cost about %s, saving about %s.", altName, money(cost-saving), money(saving)))
	}
	return &UsageAdvice{ID: "model", Kind: "model", Level: "info", Title: "Search subagents are running on a premium model", Evidence: ev,
		Action: UsageAction{Kind: "note", Text: "Set model: haiku (or sonnet) in the subagent's definition (.claude/agents/NAME.md), or choose the subagent model in Claude Code's settings."}}
}

func joinSet(m map[string]bool) string {
	var l []string
	for k := range m {
		l = append(l, k)
	}
	sort.Strings(l)
	return strings.Join(l, ", ")
}

func adviceAllow(o *UsageOverview) *UsageAdvice {
	type cand struct {
		tool, detail     string
		prompts, refused int64
	}
	seen := map[[2]string]*cand{}
	var order []*cand
	add := func(tool, detail string, prompts, refused int64) {
		k := [2]string{tool, detail}
		c := seen[k]
		if c == nil {
			c = &cand{tool: tool, detail: detail}
			seen[k] = c
			order = append(order, c)
		}
		if prompts > c.prompts {
			c.prompts = prompts
		}
		if refused > c.refused {
			c.refused = refused
		}
	}
	for _, t := range o.Bash {
		if t.Prompts+t.Refusals >= advicePromptTimes && !riskyCommand(t.Name) {
			add("Bash", t.Name, t.Prompts, t.Refusals)
		}
	}
	for _, c := range o.Refusals.Commands {
		if c.Count >= advicePromptTimes && c.Rule != "" && (c.Tool != "Bash" || !riskyCommand(c.Detail)) {
			add(c.Tool, c.Detail, 0, c.Count)
		}
	}
	if len(order) == 0 {
		return nil
	}
	sort.Slice(order, func(i, j int) bool { return order[i].prompts+order[i].refused > order[j].prompts+order[j].refused })
	order = order[:min(len(order), 6)]
	var ev, rules []string
	for _, c := range order {
		name := strings.TrimSpace(c.tool + " " + c.detail)
		if c.tool != "Bash" || c.detail == "" {
			name = cmpOr(c.detail, c.tool)
		}
		var parts []string
		if c.prompts > 0 {
			parts = append(parts, fmt.Sprintf("asked %d×", c.prompts))
		}
		if c.refused > 0 {
			parts = append(parts, fmt.Sprintf("refused %d×", c.refused))
		}
		ev = append(ev, fmt.Sprintf("%s: %s", name, strings.Join(parts, ", ")))
		if r := allowRule(c.tool, c.detail); r != "" && (c.tool != "Bash" || !riskyCommand(c.detail)) {
			rules = append(rules, r)
		}
	}
	snippet, _ := json.MarshalIndent(map[string]any{"permissions": map[string]any{"allow": rules}}, "", "  ")
	return &UsageAdvice{ID: "allow", Kind: "allow", Level: "info", Title: "Commands you are asked about or refused again and again", Evidence: ev,
		Action: UsageAction{Kind: "rule", Text: "If you would allow these every time, merge the rules into ~/.claude/settings.json (or the project's .claude/settings.json). Check each one first: a rule applies to every future session.", Snippet: string(snippet)}}
}

func adviceOutput(o *UsageOverview) *UsageAdvice {
	type cand struct {
		name  string
		calls int64
		bytes int64
	}
	var list []cand
	consider := func(label string, t UsageTool) {
		if t.Calls == 0 || t.Bytes/4 < adviceOutputTokens || t.Bytes/t.Calls < adviceOutputAvg {
			return
		}
		list = append(list, cand{label, t.Calls, t.Bytes})
	}
	for _, t := range o.Tools {
		switch t.Group {
		case "builtin":
			if t.Name != "Bash" {
				consider(t.Name, t)
			}
		case "mcp":
			consider("MCP "+t.Name, t)
		}
	}
	for _, t := range o.Bash {
		consider("Bash: "+t.Name, t)
	}
	if len(list) == 0 {
		return nil
	}
	sort.Slice(list, func(i, j int) bool { return list[i].bytes > list[j].bytes })
	list = list[:min(len(list), 5)]
	var ev []string
	for _, c := range list {
		line := fmt.Sprintf("%s: %s, %s of output (about %s tokens), %s each", c.name, plural1(int(c.calls), "call"), kb(c.bytes), tok(c.bytes/4), kb(c.bytes/c.calls))
		if o.Totals.CacheWrite > 0 {
			line += fmt.Sprintf(" — %.0f%% of the tokens written to the cache", 100*float64(c.bytes/4)/float64(o.Totals.CacheWrite))
		}
		ev = append(ev, line)
	}
	return &UsageAdvice{ID: "output", Kind: "output", Level: "info", Title: "Tools whose output fills the context", Evidence: ev,
		Action: UsageAction{Kind: "note", Text: "Narrow the command (head, tail, grep, --stat, a path), or ask the tool for fewer fields."}}
}

func (s *Service) adviceUnused(ctx context.Context, sc *usageScope) (*UsageAdvice, error) {
	now := sc.to
	from := now - int64(adviceUnusedDays)*24*3_600_000
	wide := &usageScope{uids: sc.uids, everyone: sc.everyone, from: 0, to: now}
	where, args := wide.where("hour")
	var first int64
	// #nosec G202 -- where is built from fixed conditions; values are bound
	if err := s.db.R.QueryRowContext(ctx, `SELECT coalesce(min(hour), ?) FROM usage_hours WHERE `+where, append([]any{now}, args...)...).Scan(&first); err != nil {
		return nil, err
	}
	if first > now-int64(adviceUnusedDays)*24*3_600_000+3*24*3_600_000 {
		return nil, nil
	}
	items, inv, err := s.usageUnusedSince(ctx, sc, from)
	if err != nil || !inv {
		return nil, err
	}
	var mcp, skills []string
	for _, it := range items {
		switch it.Kind {
		case "mcp":
			mcp = append(mcp, it.Name)
		case "skill":
			skills = append(skills, it.Name)
		}
	}
	if len(mcp)+len(skills) == 0 {
		return nil, nil
	}
	var ev []string
	if len(mcp) > 0 {
		ev = append(ev, fmt.Sprintf("%s not called in %d days: %s", plural1(len(mcp), "MCP server"), adviceUnusedDays, listCap(mcp, 8)))
	}
	if len(skills) > 0 {
		ev = append(ev, fmt.Sprintf("%s not used in %d days: %s", plural1(len(skills), "skill"), adviceUnusedDays, listCap(skills, 8)))
	}
	return &UsageAdvice{ID: "unused", Kind: "unused", Level: "info", Title: "MCP servers and skills nobody used this month", Evidence: ev,
		Action: UsageAction{Kind: "note", Text: "Their names and descriptions load into every session. Remove or disable what you don't need (claude mcp remove NAME, or delete the skill's folder)."}}, nil
}

func listCap(l []string, n int) string {
	sort.Strings(l)
	if len(l) <= n {
		return strings.Join(l, ", ")
	}
	return strings.Join(l[:n], ", ") + fmt.Sprintf(" and %d more", len(l)-n)
}

func (s *Service) adviceCompactions(ctx context.Context, sc *usageScope) (*UsageAdvice, error) {
	var conds []string
	var args []any
	switch {
	case sc.everyone:
		conds = append(conds, "1")
	case len(sc.uids) == 1:
		conds, args = append(conds, "user_id = ?"), append(args, sc.uids[0])
	default:
		conds = append(conds, "0")
	}
	conds, args = append(conds, "last_at >= ? AND first_at < ? AND compactions >= ?"), append(args, sc.from, sc.to, adviceCompactions)
	if sc.machine != "" {
		conds, args = append(conds, "machine = ?"), append(args, sc.machine)
	}
	if sc.session != "" {
		conds, args = append(conds, "session = ?"), append(args, sc.session)
	}
	if sc.repoFilter {
		if len(sc.repos) == 0 {
			conds = append(conds, "0")
		} else {
			conds = append(conds, "repo COLLATE NOCASE IN ("+placeholders(len(sc.repos))+")")
			for _, r := range sc.repos {
				args = append(args, r)
			}
		}
	}
	// #nosec G202 -- conds are fixed; values are bound
	rows, err := s.db.R.QueryContext(ctx, `SELECT session, title, compactions FROM usage_sessions WHERE `+strings.Join(conds, " AND ")+` ORDER BY compactions DESC LIMIT 50`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ev []string
	n := 0
	for rows.Next() {
		var session, title string
		var c int64
		if err := rows.Scan(&session, &title, &c); err != nil {
			return nil, err
		}
		n++
		if len(ev) < 3 {
			ev = append(ev, fmt.Sprintf("%s: compacted %d times", cmpOr(title, "Session "+session[:min(8, len(session))]), c))
		}
	}
	if err := rows.Err(); err != nil || n == 0 {
		return nil, err
	}
	ev = append([]string{fmt.Sprintf("%s compacted %d times or more.", plural1(n, "session"), adviceCompactions)}, ev...)
	return &UsageAdvice{ID: "compaction", Kind: "compaction", Level: "info", Title: "Sessions that compacted many times", Evidence: ev,
		Action: UsageAction{Kind: "note", Text: "Start each task in a fresh session instead of compacting, and hand large reading jobs to subagents."}}, nil
}
