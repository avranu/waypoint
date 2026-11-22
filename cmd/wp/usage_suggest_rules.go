package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// The checks of wp usage suggest (WP-193): plain rules over what this
// repository's sessions did, each with its evidence and what to change. No
// model is involved.

// sugAdvice is one suggestion.
type sugAdvice struct {
	ID       string   `json:"id"`
	Level    string   `json:"level"` // warn, info
	Title    string   `json:"title"`
	Evidence []string `json:"evidence"`
	Action   string   `json:"action"`
}

// sugInput is everything the checks read.
type sugInput struct {
	Sessions  []*sugSession // main threads and subagents
	Docs      []sugDoc
	Sources   []sugSource
	Paths     *sugPaths
	Inventory []usageInventoryItem
	Window    func(model string) string // the auto-compact window in effect for a model, as written
	Waypoint  bool                      // the repository is tracked in Waypoint
	// What the hooks measured at session start, when the server had it
	// (Measured nil: MeasuredNote says why not).
	Measured     *sugMeasured
	MeasuredNote string
}

func (in *sugInput) mains() []*sugSession {
	var out []*sugSession
	for _, s := range in.Sessions {
		if s.Kind == "main" {
			out = append(out, s)
		}
	}
	return out
}

const (
	sugDocTokens      = 2_500 // one instruction file worth a look
	sugDocTotal       = 5_000
	sugDocWarn        = 10_000 // Claude Code itself warns at about 40k characters
	sugMinSessions    = 3      // a habit, not a one-off
	sugCommentShare   = 0.25
	sugCommentTokens  = 400
	sugCommentReads   = 3
	sugReadShare      = 0.3
	sugReadRun        = 12     // calls in a row that only read or search
	sugReadRunBytes   = 60_000 // and what they brought into the context
	sugNoWPMessages   = 20
	sugPremiumSearchN = 3
	sugCompactions    = 3
)

func runSuggestChecks(in *sugInput) []sugAdvice {
	var out []sugAdvice
	for _, f := range []func(*sugInput) *sugAdvice{
		adviceDocs, adviceContext, adviceHelp, adviceSyntaxErrors, adviceSearches, adviceRereads,
		adviceComments, adviceWaypointTools, adviceMCP, adviceAgents, adviceOrientation,
	} {
		if a := f(in); a != nil {
			out = append(out, *a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Level == "warn" && out[j].Level != "warn" })
	return out
}

func tokOf(bytes int64) int64 { return bytes / approxTokenOf }

// ---- instruction files ----

func adviceDocs(in *sugInput) *sugAdvice {
	var total int64
	big, unread := false, false
	for _, d := range in.Docs {
		total += tokOf(d.Bytes)
		big = big || tokOf(d.Bytes) >= sugDocTokens
		unread = unread || d.Unread > 0
	}
	if !big && total < sugDocTotal && !unread {
		return nil
	}
	mains := in.mains()
	var msgs int64
	for _, s := range mains {
		msgs += int64(s.Messages)
	}
	head := fmt.Sprintf("%s, about %s tokens, are read into every session here.", plural1(len(in.Docs), "file"), tok(total))
	if len(mains) > 0 {
		head += fmt.Sprintf(" Over %s and %s messages that came to about %s tokens read from the cache.", plural1(len(mains), "session"), commas(msgs), tok(total*msgs))
	}
	ev := []string{head}
	if in.Measured != nil {
		ev = append(ev, in.Measured.text())
	} else {
		ev = append(ev, "These sizes are the files as they are on disk now, not measured in the sessions: "+cmpStr(in.MeasuredNote, "no session-start snapshot for them")+".")
	}
	level := "info"
	for _, d := range in.Docs {
		t := tokOf(d.Bytes)
		if t >= sugDocWarn {
			level = "warn"
		}
		line := fmt.Sprintf("%s: about %s tokens, %d lines", d.Name, tok(t), d.Lines)
		if len(d.Sections) > 0 && t >= sugDocTokens/2 {
			var secs []string
			for _, s := range d.Sections[:min(3, len(d.Sections))] {
				secs = append(secs, fmt.Sprintf("%q %s", truncRunes(s.Heading, 50), tok(tokOf(s.Bytes))))
			}
			line += "; largest sections: " + strings.Join(secs, ", ")
		}
		if d.Unread > 0 {
			line += fmt.Sprintf("; only the first %d lines are read, the other %d never reach a session", memoryLines, d.Unread)
		}
		ev = append(ev, line)
	}
	if total >= sugDocWarn {
		level = "warn"
	}
	return &sugAdvice{ID: "instructions", Level: level, Title: "Instruction files read into every session are long", Evidence: ev,
		Action: "Keep in them what every session needs (how to build, test and ship; rules that are easy to break) and move the rest to docs an agent opens when a task needs them, behind a one-line pointer. Keep MEMORY.md to one short line per memory, with the detail in the memory's own file."}
}

// ---- context growth ----

func adviceContext(in *sugInput) *sugAdvice {
	var over []*sugSession
	var overMsgs, overTok, last int64
	var many []*sugSession
	models := map[string]bool{}
	for _, s := range in.mains() {
		if s.Over > 0 {
			over = append(over, s)
			overMsgs += int64(s.Over)
			overTok += s.OverTokens
			last = max(last, s.LastAt)
			models[s.Model] = true
		}
		if s.Compactions >= sugCompactions {
			many = append(many, s)
		}
	}
	if len(over) == 0 && len(many) == 0 {
		return nil
	}
	var ev []string
	level := "info"
	if len(over) > 0 {
		sort.Slice(over, func(i, j int) bool { return over[i].OverTokens > over[j].OverTokens })
		ev = append(ev, fmt.Sprintf("%s grew past %s tokens of context: %s messages were sent above it, carrying about %s tokens more than a %s window would have. The latest was on %s.",
			plural1(len(over), "session"), tok(sugCtxCeiling), commas(overMsgs), tok(overTok), tok(sugCtxCeiling), time.UnixMilli(last).UTC().Format("2006-01-02")))
		for _, s := range over[:min(3, len(over))] {
			ev = append(ev, fmt.Sprintf("Session %s (%s): peak %s, %d messages over %s", shortID(s.ID), s.Model, tok(s.PeakCtx), s.Over, tok(sugCtxCeiling)))
		}
		if overTok >= 20_000_000 {
			level = "warn"
		}
	}
	if len(many) > 0 {
		ev = append(ev, fmt.Sprintf("%s compacted %d times or more.", plural1(len(many), "session"), sugCompactions))
	}
	if in.Window != nil {
		var ws []string
		for _, m := range sortedKeys(models) {
			ws = append(ws, m+": "+in.Window(m))
		}
		if len(ws) > 0 {
			ev = append(ev, "The auto-compact window in effect here now, for the models of those sessions: "+strings.Join(ws, "; ")+".")
		}
	}
	return &sugAdvice{ID: "context", Level: level, Title: "Sessions carry a large context for many messages", Evidence: ev,
		Action: fmt.Sprintf(`Every message re-reads the whole context, so cost grows with it. Keep the auto-compact window at %d or less for every model ("autoCompactWindow" in .claude/settings.json, and modelSettings.<model>.autoCompactWindow where a model has its own; the kit's compact_window does both through wp kit sync), and start a fresh session between tasks instead of carrying one along.`, sugCtxCeiling)}
}

func shortID(id string) string { return id[:min(8, len(id))] }

// ---- repeated lookups ----

var notHelpH = map[string]bool{"df": true, "du": true, "ls": true, "free": true, "sort": true, "grep": true, "rg": true, "head": true,
	"tail": true, "numfmt": true, "ssh": true, "nl": true, "fold": true, "zgrep": true, "egrep": true, "fgrep": true}

var subWordRE = lazyre.New(`^[a-z][a-z0-9-]*$`)

// helpKey names the command a help lookup is about ("wp ship" for
// `wp ship -h`, `wp help ship` or `wp ship --help`), or "".
func helpKey(words []string) string {
	if len(words) == 0 {
		return ""
	}
	prog := filepath.Base(words[0])
	help := false
	var rest []string
	for i, w := range words[1:] {
		switch {
		case w == "--help" || w == "-h" && !notHelpH[prog]:
			help = true
		case i == 0 && w == "help" && len(words) > 1:
			help = true
		case strings.HasPrefix(w, "-"):
		default:
			rest = append(rest, w)
		}
	}
	if !help {
		return ""
	}
	key := prog
	for _, w := range rest {
		if len(strings.Fields(key)) == 3 || !subWordRE.MatchString(w) {
			break
		}
		key += " " + w
	}
	if key == prog {
		return prog + " (its command list)"
	}
	return key
}

// sessionCounts counts things per session: how many sessions and how many times.
type sessionCounts struct {
	times    map[string]int
	sessions map[string]map[string]bool
}

func newSessionCounts() *sessionCounts {
	return &sessionCounts{times: map[string]int{}, sessions: map[string]map[string]bool{}}
}

func (c *sessionCounts) add(key, session string) {
	c.times[key]++
	if c.sessions[key] == nil {
		c.sessions[key] = map[string]bool{}
	}
	c.sessions[key][session] = true
}

// top lists the keys seen in at least minSessions sessions, most first.
func (c *sessionCounts) top(minSessions, n int) []string {
	var keys []string
	for k, s := range c.sessions {
		if len(s) >= minSessions {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := len(c.sessions[keys[i]]), len(c.sessions[keys[j]])
		if a != b {
			return a > b
		}
		if c.times[keys[i]] != c.times[keys[j]] {
			return c.times[keys[i]] > c.times[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys[:min(n, len(keys))]
}

func (c *sessionCounts) line(k string) string {
	return fmt.Sprintf("%s: %s, %s", k, plural1(len(c.sessions[k]), "session"), plural1(c.times[k], "time"))
}

func adviceHelp(in *sugInput) *sugAdvice {
	c := newSessionCounts()
	for _, s := range in.Sessions {
		for _, call := range s.Calls {
			if call.Tool != "Bash" {
				continue
			}
			for _, seg := range shellWords(call.Command) {
				if k := helpKey(stripWords(seg)); k != "" {
					c.add(k, s.ID)
				}
			}
		}
	}
	keys := c.top(sugMinSessions, 8)
	if len(keys) == 0 {
		return nil
	}
	ev := []string{"Agents looked up the same commands' help in session after session:"}
	for _, k := range keys {
		ev = append(ev, c.line(k))
	}
	return &sugAdvice{ID: "help", Level: "info", Title: "The same command help is looked up again and again", Evidence: ev,
		Action: "Put the syntax agents keep looking up where every session already reads it (a line in CLAUDE.md or AGENTS.md, or the session-start hook's cheat sheet), or make the command's error for a wrong call show the right one."}
}

var syntaxErrRE = lazyre.New(`(?i)unknown (flag|option|command|shorthand|subcommand)|flag provided but not defined|unrecognized (option|argument|arguments|subcommand)|invalid (option|choice)|no such (option|subcommand|command)|requires an argument|accepts (at most |exactly )?\d+ arg|^usage:|\nusage:|did you mean`)

func adviceSyntaxErrors(in *sugInput) *sugAdvice {
	c := newSessionCounts()
	examples := map[string]string{}
	for _, s := range in.Sessions {
		for _, call := range s.Calls {
			if !call.Err || call.Tool != "Bash" || !syntaxErrRE.MatchString(call.ErrText) {
				continue
			}
			k := mainHead(call.Heads)
			if k == "" {
				continue
			}
			c.add(k, s.ID)
			if examples[k] == "" {
				if m := syntaxErrRE.FindStringIndex(call.ErrText); m != nil {
					examples[k] = lineAround(call.ErrText, m[0])
				}
			}
		}
	}
	keys := c.top(2, 8)
	var ev []string
	for _, k := range keys {
		if c.times[k] < sugMinSessions {
			continue
		}
		line := c.line(k)
		if ex := examples[k]; ex != "" {
			line += fmt.Sprintf(" (%q)", truncRunes(ex, 100))
		}
		ev = append(ev, line)
	}
	if len(ev) == 0 {
		return nil
	}
	ev = append([]string{"Commands that failed on their own syntax (an unknown flag, a missing argument) in more than one session:"}, ev...)
	return &sugAdvice{ID: "syntax", Level: "info", Title: "Commands agents keep calling the wrong way", Evidence: ev,
		Action: "Each failed call is a round trip plus whatever the agent reads to recover. Write the right form where sessions read it, or accept the form agents reach for (an alias, a flag spelled the common way) when it is unambiguous."}
}

// lineAround is the line of s that holds byte i.
func lineAround(s string, i int) string {
	start := strings.LastIndexByte(s[:i], '\n') + 1
	end := strings.IndexByte(s[i:], '\n')
	if end < 0 {
		return strings.TrimSpace(s[start:])
	}
	return strings.TrimSpace(s[start : i+end])
}

// searchPattern is what a search command looked for, or "": grep -r, rg,
// git grep, ag, ack, wp code find. A grep with no path filters another
// command's output, and one over named files looks inside files already
// found (^func in a test file, ^ok in a log): neither is a search for
// where something lives.
func searchPattern(words []string) string {
	if len(words) < 2 {
		return ""
	}
	prog := filepath.Base(words[0])
	args := words[1:]
	switch {
	case prog == "git" && args[0] == "grep":
		args = args[1:]
		prog = "git grep"
	case prog == "wp" && len(args) >= 3 && args[0] == "code" && args[1] == "find":
		return normPattern(strings.Join(args[2:], " "))
	case prog == "grep" || prog == "egrep" || prog == "fgrep" || prog == "rg" || prog == "ag" || prog == "ack":
	default:
		return ""
	}
	valueFlags := map[string]bool{"-A": true, "-B": true, "-C": true, "-m": true, "-g": true, "--glob": true, "-t": true, "--type": true,
		"-f": true, "--max-count": true, "--include": true, "--exclude": true, "-T": true, "--type-not": true}
	recursive := prog != "grep" && prog != "egrep" && prog != "fgrep"
	pattern := ""
	var after []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-e" || a == "--regexp":
			if i+1 < len(args) && pattern == "" {
				pattern = args[i+1]
			}
			i++
		case valueFlags[a]:
			i++
		case strings.HasPrefix(a, "--"):
			recursive = recursive || a == "--recursive" || a == "--dereference-recursive"
		case strings.HasPrefix(a, "-") && len(a) > 1:
			recursive = recursive || strings.ContainsAny(a[1:], "rR")
		case pattern == "":
			pattern = a
		default:
			after = append(after, a)
		}
	}
	if !recursive && len(after) == 0 || len(after) > 0 && allFiles(after) {
		return ""
	}
	return normPattern(pattern)
}

// allFiles: every path names a file (it has an extension), not a directory.
func allFiles(paths []string) bool {
	for _, p := range paths {
		b := filepath.Base(p)
		if strings.ContainsAny(p, "*?") || filepath.Ext(b) == "" && !extensionless[b] || b == "." || b == ".." {
			return false
		}
	}
	return true
}

func normPattern(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if len(p) < 3 {
		return ""
	}
	return truncRunes(p, 80)
}

func adviceSearches(in *sugInput) *sugAdvice {
	c := newSessionCounts()
	for _, s := range in.Sessions {
		for _, call := range s.Calls {
			switch call.Tool {
			case "Grep":
				if p := normPattern(call.Pattern); p != "" {
					c.add(p, s.ID)
				}
			case "Bash":
				for _, seg := range shellWords(call.Command) {
					if p := searchPattern(stripWords(seg)); p != "" {
						c.add(p, s.ID)
					}
				}
			}
		}
	}
	keys := c.top(sugMinSessions, 8)
	if len(keys) == 0 {
		return nil
	}
	ev := []string{"The same searches, run in session after session:"}
	for _, k := range keys {
		ev = append(ev, strconv.Quote(k)+strings.TrimPrefix(c.line(k), k))
	}
	return &sugAdvice{ID: "searches", Level: "info", Title: "Agents search for the same things in every session", Evidence: ev,
		Action: "Write down where these live (a pointer in CLAUDE.md or AGENTS.md, a glossary entry, a doc's index) so the next session reads the answer instead of searching for it."}
}

// fileReads counts, per tracked file, the sessions and calls that read it.
func fileReads(in *sugInput) *sessionCounts {
	c := newSessionCounts()
	if in.Paths == nil {
		return c
	}
	for _, s := range in.Sessions {
		for _, call := range s.Calls {
			for _, p := range in.Paths.readPaths(call, s.Cwd) {
				c.add(p, s.ID)
			}
		}
	}
	return c
}

func adviceRereads(in *sugInput) *sugAdvice {
	mains := in.mains()
	if len(mains) < 5 {
		return nil
	}
	c := fileReads(in)
	floor := max(sugMinSessions+1, int(float64(len(mains))*sugReadShare+0.5))
	keys := c.top(floor, 8)
	if len(keys) == 0 {
		return nil
	}
	ev := []string{fmt.Sprintf("Read in at least %d of the %s:", floor, plural1(len(mains), "session"))}
	for _, k := range keys {
		ev = append(ev, c.line(k))
	}
	return &sugAdvice{ID: "rereads", Level: "info", Title: "Files nearly every session reads", Evidence: ev,
		Action: "If sessions read these to find their way around, put the few facts they need in the instruction file and point to the rest; if they read them to change them, a smaller file (or one split by concern) costs less to read each time."}
}

// ---- comments ----

func adviceComments(in *sugInput) *sugAdvice {
	if len(in.Sources) == 0 {
		return nil
	}
	reads := fileReads(in)
	var lines, comments int
	type row struct {
		s     sugSource
		reads int
		cost  int64
	}
	var heavy, blocks []row
	for _, s := range in.Sources {
		lines += s.Lines
		comments += s.CommentLines
		r := row{s: s, reads: reads.times[s.Path]}
		r.cost = tokOf(s.CommentBytes) * int64(max(r.reads, 1))
		if s.Lines > 0 && float64(s.CommentLines)/float64(s.Lines) >= sugCommentShare && tokOf(s.CommentBytes) >= sugCommentTokens && r.reads >= sugCommentReads {
			heavy = append(heavy, r)
		}
		if s.BlockLines >= commentBlock {
			blocks = append(blocks, r)
		}
	}
	if len(heavy) == 0 && len(blocks) == 0 {
		return nil
	}
	ev := []string{fmt.Sprintf("%.0f%% of the %s lines of tracked source are comments.", 100*float64(comments)/float64(max(lines, 1)), commas(int64(lines)))}
	sort.Slice(heavy, func(i, j int) bool { return heavy[i].cost > heavy[j].cost })
	for _, r := range heavy[:min(5, len(heavy))] {
		ev = append(ev, fmt.Sprintf("%s: read %s; %.0f%% of its %d lines are comments, about %s tokens a read", r.s.Path, plural1(r.reads, "time"),
			100*float64(r.s.CommentLines)/float64(r.s.Lines), r.s.Lines, tok(tokOf(r.s.CommentBytes))))
	}
	sort.Slice(blocks, func(i, j int) bool {
		a, b := blocks[i].s.BlockLines*max(blocks[i].reads, 1), blocks[j].s.BlockLines*max(blocks[j].reads, 1)
		if a != b {
			return a > b
		}
		return blocks[i].s.Path < blocks[j].s.Path
	})
	if len(blocks) > 0 {
		var l []string
		for _, r := range blocks[:min(5, len(blocks))] {
			l = append(l, fmt.Sprintf("%s:%d (%d lines)", r.s.Path, r.s.BlockStart, r.s.BlockLines))
		}
		more := ""
		if len(blocks) > 5 {
			more = fmt.Sprintf(" and %d more", len(blocks)-5)
		}
		ev = append(ev, fmt.Sprintf("Comment blocks of %d lines or more: %s%s", commentBlock, strings.Join(l, ", "), more))
	}
	return &sugAdvice{ID: "comments", Level: "info", Title: "Long comments in files agents read often", Evidence: ev,
		Action: "Comments are read with the code every time an agent opens the file. Keep the why in a line or two; move history, alternatives and long explanations to the decision record or doc and cite it."}
}

// ---- Waypoint's own tools ----

var forgeAPIRE = lazyre.New(`/api/v1/repos/|api\.github\.com|/api/v4/projects/|/api/v1/user\b`)

// waitCall: a long sleep before a check, or a loop that polls with sleep
// (the same tests as the wait nudges, WP-191).
func waitCall(cmd string) bool {
	return sleepWait(cmd) || reLoop.MatchString(cmd) && sleepSeconds(cmd) > 0
}

func adviceWaypointTools(in *sugInput) *sugAdvice {
	forge, polls := newSessionCounts(), newSessionCounts()
	var noWP, long int
	ranWP := false
	for _, s := range in.mains() {
		used := false
		for _, call := range s.Calls {
			if call.Tool != "Bash" {
				continue
			}
			for _, h := range call.Heads {
				switch {
				case h == "wp" || strings.HasPrefix(h, "wp "):
					used = true
				case h == "gh pr" || h == "gh api" || h == "gh run" || h == "gh issue" || h == "tea":
					forge.add(h, s.ID)
				}
			}
			if forgeAPIRE.MatchString(call.Command) && (strings.Contains(call.Command, "curl") || strings.Contains(call.Command, "wget")) {
				forge.add("curl to the forge's API", s.ID)
			}
			if waitCall(call.Command) {
				polls.add("sleep", s.ID)
			}
		}
		ranWP = ranWP || used
		if s.Messages >= sugNoWPMessages {
			long++
			if !used {
				noWP++
			}
		}
	}
	if !in.Waypoint && !ranWP {
		return nil
	}
	var ev []string
	for _, k := range forge.top(2, 5) {
		ev = append(ev, forge.line(k)+" (wp pr show, check, log, merge and wait do this)")
	}
	if n := len(polls.sessions["sleep"]); n >= 2 {
		ev = append(ev, fmt.Sprintf("Waited with sleep (%ds or longer, or in a polling loop): %s, %s (wp pr wait blocks until it is decided; wp ship wakes the agent when it is needed)", sleepWaitSecs, plural1(n, "session"), plural1(polls.times["sleep"], "time")))
	}
	if noWP >= 2 && noWP*4 >= long {
		ev = append(ev, fmt.Sprintf("%d of %s with %d messages or more never ran wp.", noWP, plural1(long, "session"), sugNoWPMessages))
	}
	if len(ev) == 0 {
		return nil
	}
	return &sugAdvice{ID: "waypoint", Level: "info", Title: "Work Waypoint's commands do with less", Evidence: ev,
		Action: "Raw API calls and polling loops return whole JSON documents or burn turns waiting. wp's commands answer in a few lines, and wp ship / wp pr wait wake the agent only when it matters. Install the agent kit (wp kit sync) so sessions start with its syntax."}
}

// ---- MCP servers and skills ----

func adviceMCP(in *sugInput) *sugAdvice {
	mains := in.mains()
	if len(mains) < 5 {
		return nil
	}
	usedMCP, usedSkill := map[string]bool{}, map[string]bool{}
	for _, s := range in.Sessions {
		for _, call := range s.Calls {
			if strings.HasPrefix(call.Tool, "mcp__") {
				if parts := strings.SplitN(call.Tool, "__", 3); len(parts) >= 2 {
					usedMCP[parts[1]] = true
				}
			}
		}
		for _, call := range s.Calls {
			if call.Tool == "Skill" {
				usedSkill[call.Skill] = true
				if _, short, ok := strings.Cut(call.Skill, ":"); ok { // plugin:skill
					usedSkill[short] = true
				}
			}
		}
	}
	var mcp, skills []string
	seen := map[string]bool{}
	for _, it := range in.Inventory {
		k := it.Kind + "\x00" + it.Name
		if seen[k] {
			continue
		}
		seen[k] = true
		switch {
		case it.Kind == "mcp" && !usedMCP[it.Name] && !usedMCP[mcpToolName(it.Name)]:
			mcp = append(mcp, it.Name)
		case it.Kind == "skill" && strings.HasPrefix(it.Scope, "project:") && !usedSkill[it.Name]:
			skills = append(skills, it.Name)
		}
	}
	if len(mcp)+len(skills) == 0 {
		return nil
	}
	var start []int64
	for _, s := range mains {
		if s.StartCtx > 0 {
			start = append(start, s.StartCtx)
		}
	}
	var ev []string
	if len(mcp) > 0 {
		ev = append(ev, fmt.Sprintf("%s configured for this repository and never called in its %s: %s", plural1(len(mcp), "MCP server"), plural1(len(mains), "session"), listCap(mcp, 8)))
	}
	if len(skills) > 0 {
		ev = append(ev, fmt.Sprintf("%s of this repository's own never used: %s", plural1(len(skills), "skill"), listCap(skills, 8)))
	}
	if m := median(start); m > 0 {
		ev = append(ev, fmt.Sprintf("Sessions here start at a median of %s tokens of context before any work.", tok(m)))
	}
	return &sugAdvice{ID: "mcp", Level: "info", Title: "MCP servers and skills this repository never uses", Evidence: ev,
		Action: `Their names and descriptions are part of every session's start. Turn off a server where it isn't needed ("disabledMcpjsonServers" in .claude/settings.json for a project server, claude mcp remove NAME for your own), and delete skills nobody uses.`}
}

// mcpToolName is a server name as it appears in tool names (mcp__name__…):
// Claude Code replaces characters other than letters, digits, _ and - with _.
func mcpToolName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func listCap(l []string, n int) string {
	sort.Strings(l)
	if len(l) <= n {
		return strings.Join(l, ", ")
	}
	return strings.Join(l[:n], ", ") + fmt.Sprintf(" and %d more", len(l)-n)
}

func plural1(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %ss", n, one)
}

// tok shows a token count: 950, 12k, 3.4M.
func tok(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	}
	return strconv.FormatInt(n, 10)
}

func joinSet(m map[string]bool) string {
	l := make([]string, 0, len(m))
	for k := range m {
		l = append(l, k)
	}
	sort.Strings(l)
	return strings.Join(l, ", ")
}

func median(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]int64(nil), v...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)/2]
}

// ---- which agent does the work ----

func premiumModel(m string) bool { return premiumFamily(m) != "" }

// premiumFamily names the premium family of model m ("Opus", "Fable",
// "Mythos"), or "" if it is not one.
func premiumFamily(m string) string {
	m = strings.ToLower(m)
	for _, f := range []string{"Opus", "Fable", "Mythos"} {
		if strings.Contains(m, strings.ToLower(f)) {
			return f
		}
	}
	return ""
}

func searchKind(k string) bool {
	k = strings.ToLower(k)
	return strings.Contains(k, "explor") || strings.Contains(k, "search") || strings.Contains(k, "find")
}

// readOnlyCall: a call that only reads or searches.
func readOnlyCall(c sugCall) bool {
	switch c.Tool {
	case "Read", "Grep", "Glob", "LS":
		return true
	case "Bash":
		if len(c.Heads) == 0 {
			return false
		}
		for _, h := range c.Heads {
			switch h {
			case "cat", "head", "tail", "sed", "grep", "rg", "find", "ls", "wc", "nl", "awk", "cd", "echo", "sort", "uniq",
				"git log", "git show", "git diff", "git grep", "git blame", "wp code":
			default:
				return false
			}
		}
		return true
	}
	return false
}

func adviceAgents(in *sugInput) *sugAdvice {
	var ev []string
	// search subagents on a premium model
	var n int
	var t sugTokens
	kinds, models := map[string]bool{}, map[string]bool{}
	for _, s := range in.Sessions {
		if s.Kind == "main" || !searchKind(s.Kind) || !premiumModel(s.Model) {
			continue
		}
		n++
		t.In += s.Tokens.In
		t.Out += s.Tokens.Out
		t.CW += s.Tokens.CW
		t.CR += s.Tokens.CR
		kinds[s.Kind], models[s.Model] = true, true
	}
	if n >= sugPremiumSearchN {
		ev = append(ev, fmt.Sprintf("%s of search-only subagents (%s) ran on %s: %s tokens in, %s out, %s written to and %s read from the cache.",
			plural1(n, "run"), joinSet(kinds), joinSet(models), tok(t.In), tok(t.Out), tok(t.CW), tok(t.CR)))
	}
	// long runs of reading in the main thread
	var runs int
	var runBytes int64
	sessions := map[string]bool{}
	for _, s := range in.mains() {
		count, bytes := 0, int64(0)
		flush := func() {
			if count >= sugReadRun && bytes >= sugReadRunBytes {
				runs++
				runBytes += bytes
				sessions[s.ID] = true
			}
			count, bytes = 0, 0
		}
		for _, c := range s.Calls {
			if readOnlyCall(c) {
				count++
				bytes += c.Bytes
				continue
			}
			flush()
		}
		flush()
	}
	if runs >= 2 {
		ev = append(ev, fmt.Sprintf("The main thread read or searched %d times or more in a row, with nothing else between, %d times in %s, bringing about %s tokens into its context.",
			sugReadRun, runs, plural1(len(sessions), "session"), tok(tokOf(runBytes))))
	}
	if len(ev) == 0 {
		return nil
	}
	return &sugAdvice{ID: "agents", Level: "info", Title: "Reading and searching on the most expensive model", Evidence: ev,
		Action: "A subagent on Haiku or Sonnet (model: haiku in .claude/agents/NAME.md) can search and return the locations and facts, so the main thread keeps the answer rather than everything read to find it."}
}

var extensionless = map[string]bool{"Makefile": true, "GNUmakefile": true, "Dockerfile": true, "Justfile": true, "Containerfile": true}
