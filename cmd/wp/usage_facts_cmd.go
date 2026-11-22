package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"git.jmann.me/claude/waypoint/internal/core"
)

// wp usage facts (WP-1467): what a session, or a task's share of its sessions,
// did, counted from the facts the server derives from the transcript archive.

var usageFactsCmd = &command{
	name: "facts", args: "SESSION|TASK-KEY [--machine M] [--user NAME]",
	summary: "what a session or task did, counted from its archived transcript",
	about: `Turns by what started them, model calls and tokens, tool calls, what was put
into the context (instruction and memory files, with sizes), documentation read,
files and lines changed, tests written, repeated tool calls, thinking tokens,
compactions, sub-agents and cache expiry. A task key counts the stretches of
sessions the task's traces cover. The server derives the facts a few minutes
after a transcript stops growing: a session still running, or archived recently,
may not be counted yet, and the output says how much of the archive it covers.
A value the transcript did not carry is shown as unknown, never 0; cache expiry
is inferred from timing, and says so. Your own sessions; an admin can read
anyone's with --user.`,
	examples: []string{"wp usage facts 0b1c…", "wp usage facts WP-1467 --json"},
	run:      cmdUsageFacts,
}

func init() {
	usageCmd.subs = append(usageCmd.subs, usageFactsCmd)
}

var factsTaskKeyRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{1,9}-\d+$`)

func cmdUsageFacts(c *cli, args []string) error {
	fs := c.flags()
	machine := fs.String("machine", "", "only this `MACHINE`")
	user := fs.String("user", "", "admins: whose (a `USERNAME`)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("give a session id or a task key")
	}
	q := url.Values{}
	if factsTaskKeyRE.MatchString(pos[0]) {
		q.Set("task", strings.ToUpper(pos[0]))
	} else {
		q.Set("session", pos[0])
	}
	if *machine != "" {
		q.Set("machine", *machine)
	}
	if *user != "" {
		q.Set("user", *user)
	}
	var f core.SessionFacts
	if err := c.do("GET", "/api/v1/usage/facts?"+q.Encode(), nil, &f); err != nil {
		return err
	}
	raw, _ := json.Marshal(f)
	return c.emit(raw, func() string { return factsText(&f) })
}

// tallyText: the sum, or unknown when no row carried it, or the sum with how
// many rows did not.
func tallyText(t core.Tally, unit func(int64) string) string {
	if unit == nil {
		unit = func(n int64) string { return fmt.Sprint(n) }
	}
	switch {
	case t.Known == 0 && t.Unknown == 0:
		return "0"
	case t.Known == 0:
		return "unknown"
	case t.Unknown > 0:
		return fmt.Sprintf("%s (unknown for %d of %d)", unit(t.Sum), t.Unknown, t.Known+t.Unknown)
	}
	return unit(t.Sum)
}

func countsText(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool {
		if m[ks[i]] != m[ks[j]] {
			return m[ks[i]] > m[ks[j]]
		}
		return ks[i] < ks[j]
	})
	parts := make([]string, len(ks))
	for i, k := range ks {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return strings.Join(parts, ", ")
}

func factsText(f *core.SessionFacts) string {
	var b strings.Builder
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(&b, format+"\n", a...) }
	p("%s %s", f.Scope, f.Key)
	for _, s := range f.Sources {
		span := "whole session"
		if s.From != nil {
			span = "from " + fmt.Sprint(*s.From)
			if s.To != nil {
				span += " to " + fmt.Sprint(*s.To)
			} else {
				span += " (no end)"
			}
		}
		p("  source: %s %s %s (%s)", s.User, s.Machine, s.Session, span)
	}
	st := f.Status
	p("archive: %d files, %d derived, %d waiting, %d failed", st.FilesArchived, st.FilesDerived, st.FilesWaiting, st.FilesFailed)
	if st.FilesWaiting > 0 {
		p("  counts below cover only the derived files; the rest are derived a few minutes after a file stops growing")
	}
	if st.WholeSession {
		p("  (the file figures above are for the task's whole sessions; the counts below are for the task's time windows only)")
	}
	if f.UntimedLeftOut > 0 {
		p("  %d rows carry no timestamp, so no task window holds them: they are left out of the counts below", f.UntimedLeftOut)
	}
	if len(st.Undecoded) > 0 {
		var parts []string
		for k, n := range st.Undecoded {
			parts = append(parts, fmt.Sprintf("%s %d", k, n))
		}
		sort.Strings(parts)
		p("  fields the parser could not decode (facts may be missing): %s", strings.Join(parts, ", "))
	}
	if len(st.Unknown) > 0 {
		var parts []string
		for k, n := range st.Unknown {
			parts = append(parts, fmt.Sprintf("%s %d", k, n))
		}
		sort.Strings(parts)
		p("  records the parser does not understand (facts may be missing): %s", strings.Join(parts, ", "))
	}
	p("")
	p("turns: %d (by what started them: %s)", f.Turns.Total, countsText(f.Turns.ByOrigin))
	m := f.ModelCalls
	p("model calls: %d (%d by sub-agents); models: %s", m.Total, m.Sidechain, countsText(m.ByModel))
	p("  output tokens %s; thinking tokens %s (part of the output); cache read %s; cache write %s",
		tallyText(m.Output, nil), tallyText(m.Thinking, nil), tallyText(m.CacheRead, nil), tallyText(m.CacheWrit, nil))
	p("tool calls: %d (%d errors): %s", f.ToolCalls.Total, f.ToolCalls.Errors, countsText(f.ToolCalls.ByTool))
	p("injected into the context: %d items, %s", f.Injected.Count, tallyText(f.Injected.Bytes, bytesText))
	for _, i := range f.Injected.Top {
		size := "unknown size"
		if i.Bytes != nil {
			size = bytesText(*i.Bytes)
		}
		kind := i.Kind
		if i.FileType != "" {
			kind += "/" + i.FileType
		}
		p("  %-24s %-60s x%d, %s each", kind, i.Name, i.Times, size)
	}
	p("documentation read: %d reads of %d files", f.DocsRead.Calls, f.DocsRead.Distinct)
	for _, d := range f.DocsRead.Top {
		p("  %s x%d", d.Path, d.Times)
	}
	c := f.Changes
	p("changes: %d edits (%d failed), %d files; lines added %s, removed %s", c.Edits, c.Failed, c.FilesChanged,
		tallyText(c.LinesAdded, nil), tallyText(c.LinesRemoved, nil))
	p("  tests written: %d edits of %d test files, lines added %s", c.TestEdits, c.TestFiles, tallyText(c.TestLinesAdd, nil))
	r := f.Repeated
	p("repeated tool calls: %d of %d calls that name a target repeat an earlier one", r.Repeats, r.Calls)
	for _, t := range r.Top {
		p("  %s %s x%d", t.Tool, t.Target, t.Times)
	}
	k := f.Compactions
	p("compactions: %d (%s); tokens before %s, after %s", k.Total, countsText(k.ByTrigger), tallyText(k.PreTokens, nil), tallyText(k.PostTokens, nil))
	a := f.Subagents
	p("sub-agents: %d (%s); duration ms %s; tokens %s", a.Total, countsText(a.ByType), tallyText(a.DurationMS, nil), tallyText(a.Tokens, nil))
	e := f.CacheExpiry
	p("cache expiry (inferred from timing, not recorded): %d of %d calls wrote the cache after a gap longer than its life; cache written then %s; %d calls cannot be judged",
		e.Cold, e.Determinable, tallyText(e.ColdWrite, nil), e.Undeterminate)
	return b.String()
}
