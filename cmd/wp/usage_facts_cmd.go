package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"git.jmann.me/claude/waypoint/internal/core"
)

var usageFactsCmd = &command{
	name: "facts", args: "SESSION|TASK-KEY | --session ID | --task KEY [--machine M] [--user NAME]",
	summary: "what a session or task did, counted from its archived transcript",
	about: `Turns by what started them, model calls and tokens, tool calls, what was put
into the context (instruction and memory files, with sizes), documentation read,
files and lines changed, test files edited, repeated tool calls, thinking tokens,
compactions, sub-agents and cache expiry. A task key counts the stretches of
sessions the task's traces cover. The server derives the facts 10 to 15 minutes
after a transcript stops growing: a session still running, or archived recently,
may not be counted yet, and the output says how much of the archive it covers.
A bare argument that looks like a task key (APP-12) is one; for a session id
that looks like a key, use --session.
A value the transcript did not carry is shown as unknown, never 0; cache expiry
is inferred from timing, and says so. Your own person's sessions, also from an agent (which sees repeated commands redacted);
an admin (a person, not an agent) can read anyone's with --user.`,
	examples: []string{"wp report behavior facts 0b1c…", "wp report behavior facts WP-1467 --json", "wp report behavior facts --session abc123"},
	run:      cmdUsageFacts,
}

// factsTarget says which of --task, --session or the bare argument names what
// to read: a bare argument is a task when it looks like an issue key (keyRE),
// else a session id.
func factsTarget(task, session string, pos []string) (kind, value string, err error) {
	switch {
	case task != "" && session != "":
		return "", "", fmt.Errorf("give --task or --session, not both")
	case (task != "" || session != "") && len(pos) > 0:
		return "", "", fmt.Errorf("give the session or task as a flag or as an argument, not both")
	case task != "":
		return "task", strings.ToUpper(task), nil
	case session != "":
		return "session", session, nil
	case len(pos) != 1:
		return "", "", fmt.Errorf("give a session id or a task key")
	case keyRE.MatchString(strings.ToUpper(pos[0])):
		return "task", strings.ToUpper(pos[0]), nil
	}
	return "session", pos[0], nil
}

func cmdUsageFacts(c *cli, args []string) error {
	fs := c.flags()
	machine := fs.String("machine", "", "only this `MACHINE`")
	user := fs.String("user", "", "admins: whose (a `USERNAME`)")
	task := fs.String("task", "", "the task `KEY` to read")
	session := fs.String("session", "", "the session `ID` to read (for an id that looks like a task key)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	kind, value, err := factsTarget(*task, *session, pos)
	if err != nil {
		return c.usage("%s", err)
	}
	q := url.Values{}
	q.Set(kind, value)
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

func countsText[N int | int64](m map[string]N) string {
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
	p("archive: %d files (each rewrite of a file counts): %d derived, %d waiting, %d failed", st.FilesArchived, st.FilesDerived, st.FilesWaiting, st.FilesFailed)
	if st.FilesWaiting > 0 || st.FilesFailed > 0 {
		p("  the counts below cover the derived files and the rows an earlier derive left for %d of the others; a waiting file is derived 10 to 15 minutes after it stops growing,", st.FilesEarlierRows)
		p("  a failed one is tried again when it grows, the parser changes, or after 6 hours")
	}
	if st.WholeSession {
		p("  (the file figures above are for the task's whole sessions; the counts below are for the task's time windows only)")
	}
	if f.UntimedLeftOut > 0 {
		p("  %d rows carry no timestamp, so no task window holds them: they are left out of the counts below", f.UntimedLeftOut)
	}
	if len(st.Undecoded) > 0 {
		p("  fields the parser could not decode (facts may be missing): %s", countsText(st.Undecoded))
	}
	if len(st.Unknown) > 0 {
		p("  records the parser does not understand (facts may be missing): %s", countsText(st.Unknown))
	}
	p("")
	p("turns: %d (by what started them: %s)", f.Turns.Total, countsText(f.Turns.ByOrigin))
	p("  by prompt source: %s; slash commands: %s", countsText(f.Turns.ByPromptSource), countsText(f.Turns.BySlashCommand))
	m := f.ModelCalls
	p("model calls: %d (%d by sub-agents); models: %s", m.Total, m.Sidechain, countsText(m.ByModel))
	p("  output tokens %s; thinking tokens %s (part of the output); cache read %s; cache write %s",
		tallyText(m.Output, nil), tallyText(m.Thinking, nil), tallyText(m.CacheRead, nil), tallyText(m.CacheWrite, nil))
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
	p("documentation read: %d reads of %d files; %d fetches of %d URLs", f.DocsRead.Calls, f.DocsRead.Distinct, f.DocsFetched.Calls, f.DocsFetched.Distinct)
	for _, d := range f.DocsRead.Top {
		p("  %s x%d", d.Path, d.Times)
	}
	for _, d := range f.DocsFetched.Top {
		p("  %s x%d", d.URL, d.Times)
	}
	c := f.Changes
	p("changes: %d edits (%d failed), %d files; lines added %s, removed %s", c.Edits, c.Failed, c.FilesChanged,
		tallyText(c.LinesAdded, nil), tallyText(c.LinesRemoved, nil))
	p("  test files edited: %d edits of %d test files, lines added %s", c.TestEdits, c.TestFiles, tallyText(c.TestLinesAdd, nil))
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
		e.Cold, e.Determinable, tallyText(e.ColdWrite, nil), e.Undeterminable)
	return b.String()
}
