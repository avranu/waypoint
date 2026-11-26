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
is inferred from timing, and says so. Your own person's sessions, also from an agent;
an admin (a person, not an agent) can read anyone's with --user.`,
	examples: []string{"wp usage facts 0b1c…", "wp usage facts WP-1467 --json", "wp usage facts --session abc123"},
	run:      cmdUsageFacts,
}

func init() {
	usageCmd.subs = append(usageCmd.subs, usageFactsCmd)
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
	raw, err := c.call("GET", "/api/v1/usage/facts?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	var f core.SessionFacts
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	// With --json the body is printed as the server sent it: re-marshalling
	// the struct would invent shell_edits: 0 for a server too old to count them.
	return c.emit(raw, func() string {
		text := factsText(&f)
		if !factsHaveShellEdits(raw) {
			text = strings.Replace(text, shellEditsText(f.Changes.ShellEdits, f.Changes.ShellUnknown, f.Changes.ShellNone, f.Changes.ScriptShellEdits, f.Changes.ScriptShellUnknown), "shell edits unknown (server too old to say)", 1)
		}
		return text
	})
}

// factsHaveShellEdits is whether the reply's changes carry shell_edits at
// all: an older server leaves it out, and the int field then reads 0.
func factsHaveShellEdits(raw []byte) bool {
	var r struct {
		Changes struct {
			ShellEdits *int `json:"shell_edits"`
		} `json:"changes"`
	}
	return json.Unmarshal(raw, &r) == nil && r.Changes.ShellEdits != nil
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
	p("tool calls: %d (%d errors): %s; run by REPL scripts: %d", f.ToolCalls.Total, f.ToolCalls.Errors, countsText(f.ToolCalls.ByTool), f.ToolCalls.ByScript)
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
	p("changes: %d edits (%d failed; %d more run by REPL scripts), %s; the edits changed %d files, lines added %s, removed %s (what shell edits changed is not counted)", c.Edits, c.Failed, c.ScriptEdits, shellEditsText(c.ShellEdits, c.ShellUnknown, c.ShellNone, c.ScriptShellEdits, c.ScriptShellUnknown), c.FilesChanged,
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

// shellEditsText says the Bash calls that wrote files, beside the edits of
// the editing tools; unknown for Bash calls with no answer (stored before it
// was recorded, or too long to read). viaScript is the Bash calls a REPL
// script ran that wrote files, told apart from the rest as the edits of
// scripts are. none is the Bash calls classified as not writing: with some,
// a zero beside unknown calls is "0+", not unknown.
func shellEditsText(n, unknown, none, viaScript, scriptUnknown int) string {
	var s string
	switch {
	case unknown == 0:
		s = fmt.Sprintf("%d shell edits", n)
	case n == 0 && none > 0:
		s = fmt.Sprintf("0+ shell edits (%d Bash calls not classified)", unknown)
	case n == 0:
		s = fmt.Sprintf("shell edits unknown (%d Bash calls not classified)", unknown)
	default:
		s = fmt.Sprintf("%d+ shell edits (%d Bash calls not classified)", n, unknown)
	}
	switch {
	case scriptUnknown > 0:
		s += fmt.Sprintf(" and %d+ more run by REPL scripts (%d of their Bash calls not classified)", viaScript, scriptUnknown)
	case viaScript > 0:
		s += fmt.Sprintf(" and %d more run by REPL scripts", viaScript)
	}
	return s
}
