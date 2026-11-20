package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// wp usage suggest (WP-193): how this repository could cost its agents
// fewer tokens, the way Claude Code's /fewer-permission-prompts finds the
// approvals to stop asking for. It reads the repository (instruction
// files, comments) and this machine's transcripts of the sessions that
// worked in it, and prints each finding with its evidence and what to
// change. Everything stays on this machine; no model is involved.

var usageSuggestCmd = &command{
	name: "suggest", args: "[--since 30d] [--until 7d] [--dir CLAUDE_CONFIG_DIR]",
	summary: "how this repository could cost its agents fewer tokens",
	about: `Run it in a repository. Reads the repository and this machine's Claude Code
transcripts of sessions that worked in it, and lists what makes them cost
more than needed, with evidence and what to change: big instruction files,
sessions past 200k context, repeated help lookups, searches and rereads,
comment-heavy files, raw forge calls and sleep polling, unused MCP servers
and skills, search subagents on a premium model, and grep chains where wp
code or wp brief answer in one call. --until compares a past window with now
(--since 14d --until 7d is the week before last). Nothing is sent anywhere.`,
	examples: []string{"wp usage suggest", "wp usage suggest --since 14d --until 7d"},
	run:      cmdUsageSuggest,
}

func init() {
	usageCmd.subs = append(usageCmd.subs, usageSuggestCmd)
}

// sugReport is what wp usage suggest prints.
type sugReport struct {
	Repo        string          `json:"repo"`
	Days        float64         `json:"days"`
	Sessions    int             `json:"sessions"`
	Subagents   int             `json:"subagent_runs"`
	StartMedian int64           `json:"start_context_median,omitempty"`
	Until       float64         `json:"until_days,omitempty"`
	Orientation *sugOrientation `json:"orientation,omitempty"`
	DocTokens   int64           `json:"instruction_tokens"`
	Docs        []sugDoc        `json:"instruction_files"`
	Suggestions []sugAdvice     `json:"suggestions"`
}

const defaultSuggestSince = 30 * 24 * time.Hour

func cmdUsageSuggest(c *cli, args []string) error {
	fs := c.flags()
	since := fs.String("since", "", "sessions active within `DURATION` (default 30d)")
	until := fs.String("until", "", "sessions that started over `DURATION` ago")
	dir := fs.String("dir", "", "Claude Code config `DIR` (default CLAUDE_CONFIG_DIR, ~/.claude)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	d, err := parseSince(*since)
	if err != nil {
		return c.usage("%v", err)
	}
	if d == 0 {
		d = defaultSuggestSince
	}
	u, err := parseSince(*until)
	if err != nil {
		return c.usage("--until: %v", err)
	}
	if u >= d {
		return c.usage("--until %s is not after --since %s", *until, *since)
	}
	root, err := gitOutput("", "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("run wp usage suggest in a git checkout of the repository: %w", err)
	}
	rep, err := buildSuggestReport(root, claudeDir(*dir), d, u, c.server() != "")
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(rep)
	return c.emit(raw, func() string { return suggestText(rep) })
}

func buildSuggestReport(root, claudeRoot string, since, until time.Duration, waypoint bool) (*sugReport, error) {
	mainRoot := root
	if common, err := gitOutput(root, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil && filepath.Base(common) == ".git" {
		mainRoot = filepath.Dir(common)
	}
	paths := &sugPaths{roots: map[string]bool{root: true, mainRoot: true}, mainRoot: mainRoot, tracked: map[string]bool{}}
	if out, err := gitOutput(root, "worktree", "list", "--porcelain"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if p, ok := strings.CutPrefix(line, "worktree "); ok {
				paths.roots[filepath.Clean(p)] = true
			}
		}
	}
	var tracked []string
	if out, err := gitOutput(root, "ls-files", "-z"); err == nil {
		for _, p := range strings.Split(out, "\x00") {
			if p != "" {
				tracked = append(tracked, p)
				paths.tracked[p] = true
			}
		}
	}
	repo := &sugRepo{key: resolveRepo(root), mainRoot: mainRoot, resolver: newRepoResolver()}
	var sessions []*sugSession
	if _, err := os.Stat(filepath.Join(claudeRoot, "projects")); err == nil {
		s, err := scanSuggestSessions(claudeRoot, since, repo)
		if err != nil {
			return nil, err
		}
		sessions = s
	}
	if until > 0 {
		sessions = startedBefore(sessions, time.Now().Add(-until).UnixMilli())
	}
	rep := &sugReport{Repo: repo.key, Days: since.Hours() / 24, Until: until.Hours() / 24}
	memDirs := []string{mainRoot, root}
	seen := map[string]bool{mainRoot: true, root: true}
	var start []int64
	for _, s := range sessions {
		if s.Kind != "main" {
			rep.Subagents++
			continue
		}
		rep.Sessions++
		if s.StartCtx > 0 {
			start = append(start, s.StartCtx)
		}
		if top := checkoutOf(s.Cwd); top != "" && !seen[top] {
			seen[top] = true
			memDirs = append(memDirs, top)
		}
	}
	rep.StartMedian = median(start)
	rep.Docs = instructionDocs(claudeRoot, root, memDirs)
	for _, d := range rep.Docs {
		rep.DocTokens += tokOf(d.Bytes)
	}
	in := &sugInput{
		Sessions:  sessions,
		Docs:      rep.Docs,
		Sources:   scanComments(root, tracked),
		Paths:     paths,
		Inventory: collectInventory(claudeRoot, []string{root}),
		Window:    compactWindowFor(readCompactHave(root)),
		Waypoint:  waypoint,
	}
	rep.Suggestions = runSuggestChecks(in)
	rep.Orientation = orientation(sessions)
	if rep.Docs == nil {
		rep.Docs = []sugDoc{}
	}
	if rep.Suggestions == nil {
		rep.Suggestions = []sugAdvice{}
	}
	return rep, nil
}

// compactWindowFor is the autoCompactWindow Claude Code uses at root for
// a model, and where it comes from: the environment beats the files; in
// the files (merged, the local file over the project's over the user's),
// modelSettings.<model> beats the top-level value.
func compactWindowFor(h compactHave) func(model string) string {
	return func(model string) string {
		if h.Env != "" {
			return h.Env + " (CLAUDE_CODE_AUTO_COMPACT_WINDOW)"
		}
		for i := len(h.Files) - 1; i >= 0; i-- {
			if w := h.Files[i].Models[model]; w != 0 {
				return compactShow(w) + " (" + h.Files[i].Name + ", modelSettings)"
			}
		}
		for i := len(h.Files) - 1; i >= 0; i-- {
			if w := h.Files[i].Top; w != 0 {
				return compactShow(w) + " (" + h.Files[i].Name + ")"
			}
		}
		return "the model's own (none set)"
	}
}

// startedBefore keeps the sessions that started before cut (ms), and the
// subagents of those.
func startedBefore(sessions []*sugSession, cut int64) []*sugSession {
	keep := map[string]bool{}
	var out []*sugSession
	for _, s := range sessions {
		if s.Kind == "main" && s.FirstAt < cut {
			keep[s.ID] = true
		}
	}
	for _, s := range sessions {
		if keep[s.ID] || s.Kind != "main" && s.FirstAt < cut {
			out = append(out, s)
		}
	}
	return out
}

func untilText(days float64) string {
	if days == 0 {
		return ""
	}
	return ", up to " + strconv.FormatFloat(days, 'f', -1, 64) + " days ago"
}

func suggestText(r *sugReport) string {
	var b strings.Builder
	days := strconv.FormatFloat(r.Days, 'f', -1, 64)
	if r.Sessions == 0 {
		fmt.Fprintf(&b, "No sessions of %s on this machine in the last %s days%s; only its files were checked.\n", r.Repo, days, untilText(r.Until))
	} else {
		fmt.Fprintf(&b, "%s: %s and %s on this machine in the last %s days%s.\n", r.Repo, plural1(r.Sessions, "session"), plural1(r.Subagents, "subagent run"), days, untilText(r.Until))
		if r.StartMedian > 0 {
			fmt.Fprintf(&b, "Sessions start at a median of %s tokens of context; instruction files and memory are about %s of it.\n", tok(r.StartMedian), tok(r.DocTokens))
		}
		b.WriteString(orientationText(r.Orientation))
	}
	if len(r.Suggestions) == 0 {
		b.WriteString("\nNothing to suggest: no check found a habit worth changing.\n")
		return b.String()
	}
	for i, a := range r.Suggestions {
		mark := ""
		if a.Level == "warn" {
			mark = " (!)"
		}
		fmt.Fprintf(&b, "\n%d. %s%s  [%s]\n", i+1, a.Title, mark, a.ID)
		for _, e := range a.Evidence {
			fmt.Fprintf(&b, "   - %s\n", e)
		}
		fmt.Fprintf(&b, "   → %s\n", a.Action)
	}
	return b.String()
}
