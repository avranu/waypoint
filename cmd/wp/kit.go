package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

var kitCmd = &command{
	name: "kit", group: "Setup", args: "SUBCOMMAND",
	summary: "sync this checkout's shared Claude Code plugins",
	about: `The agent kit is a git repo laid out as a plugin marketplace; kit.toml
says which plugins go to which repos. An admin picks it in Admin → Forges.

wp kit sync registers the marketplace, installs and enables the plugins
this repo gets at local scope (.claude/settings.local.json, git-ignored)
and disables kit plugins no longer sent. It runs on wp setup, wp upgrade
and at session start; plugins load in the next session.`,
	examples: []string{
		"wp kit sync",
		"wp kit sync --check --diff",
		"wp kit sync --all",
		"wp kit status",
		"wp kit ls",
		"wp kit add skill ~/.claude/skills/forgejo-prs --plugin forgejo -m \"PR commands for Forgejo\"",
	},
}

func init() {
	kitCmd.subs = []*command{kitSyncCmd, kitStatusCmd, kitLsCmd, kitAddCmd, kitReportCmd}
	kitCmd.run = func(c *cli, args []string) error {
		if len(args) == 0 {
			return kitStatus(c, args)
		}
		return c.usage("no kit command %q", args[0])
	}
}

// kitResolved is GET /api/v1/kit/resolve.
type kitResolved struct {
	Repo        string `json:"repo"`
	Attached    bool   `json:"attached"`
	Commit      string `json:"commit"`
	Marketplace string `json:"marketplace"`
	GitURL      string `json:"git_url"`
	// CompactWindow caps autoCompactWindow here (WP-147; 0: any).
	CompactWindow int       `json:"compact_window"`
	Plugins       []kitItem `json:"plugins"`
	Notes         []kitItem `json:"notes"`
	All           []string  `json:"all"`
	At            time.Time `json:"at,omitzero"` // when it was asked (the cache)
}

type kitItem struct {
	Name     string   `json:"name"`
	Text     string   `json:"text"`
	When     []string `json:"when"`
	Machines []string `json:"machines"`
}

// kitCheckout is what wp remembers of one checkout (the state file).
type kitCheckout struct {
	Repo    string    `json:"repo"`
	Commit  string    `json:"commit"`  // the kit commit last applied
	Managed []string  `json:"managed"` // plugin ids wp enabled here
	Wanted  []string  `json:"wanted"`  // what the kit sent at that commit (after conditions)
	At      time.Time `json:"at"`
	// Tried is a commit a session start began syncing to in the background,
	// so the next one doesn't start another.
	Tried   string    `json:"tried,omitempty"`
	TriedAt time.Time `json:"tried_at,omitzero"`
}

// kitResolveTTL is how long SessionStart trusts what it last asked.
const kitResolveTTL = 10 * time.Minute

// resolveKit asks the server what repo gets.
func (c *cli) resolveKit(repo string) (*kitResolved, error) {
	var r kitResolved
	if err := c.do("GET", "/api/v1/kit/resolve?repo="+url.QueryEscape(repo), nil, &r); err != nil {
		return nil, err
	}
	r.At = time.Now()
	return &r, nil
}

// holds says whether a kit item's conditions hold in the checkout at dir on
// this machine: every when, and one of machines (runner labels or the
// machine's name).
func (c *cli) holds(it kitItem, dir string) bool {
	for _, w := range it.When {
		p := ""
		switch {
		case strings.HasPrefix(w, "exists:"):
			p = strings.TrimPrefix(w, "exists:")
		}
		if p == "" || strings.Contains(p, "..") {
			return false
		}
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p))); err != nil {
			return false
		}
	}
	if len(it.Machines) == 0 {
		return true
	}
	mine := []string{machineName(), os.Getenv("WAYPOINT_MACHINE")}
	if c.state.Runner != nil {
		mine = append(mine, c.state.Runner.Labels...)
		mine = append(mine, c.state.Runner.Name)
	}
	return slices.ContainsFunc(it.Machines, func(m string) bool {
		return slices.ContainsFunc(mine, func(x string) bool { return x != "" && strings.EqualFold(x, m) })
	})
}

// want is what the kit wants in the checkout at dir.
func (c *cli) kitWantFor(r *kitResolved, dir string) kitWant {
	w := kitWant{Marketplace: r.Marketplace, GitURL: r.GitURL, Commit: r.Commit, All: r.All, Plugins: []string{}, CompactWindow: r.CompactWindow}
	for _, p := range r.Plugins {
		if c.holds(p, dir) {
			w.Plugins = append(w.Plugins, p.Name)
		}
	}
	return w
}

// notesFor are the kit's notes for the checkout at dir.
func (c *cli) kitNotesFor(r *kitResolved, dir string) []string {
	var out []string
	for _, n := range r.Notes {
		if c.holds(n, dir) && strings.TrimSpace(n.Text) != "" {
			out = append(out, strings.TrimSpace(n.Text))
		}
	}
	return out
}

// gitTop is the top of the git work tree at dir ("" outside one).
func gitTop(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output() // #nosec G204 -- fixed arguments
	if err != nil {
		return ""
	}
	return filepath.Clean(strings.TrimSpace(string(out)))
}

var kitSyncCmd = &command{
	name: "sync", args: "[DIR...] [FLAGS]",
	summary: "make this checkout's kit plugins match what the kit sends it",
	about: `Syncs DIR (default the current checkout) with the agent kit. Only plugins
wp enabled are touched; a kit plugin you disabled by hand is drift,
reported and left alone unless --force. Plugins load in the next session.`,
	examples: []string{"wp kit sync", "wp kit sync --check --diff", "wp kit sync --all", "wp kit sync --force"},
	run:      kitSync,
}

type kitSyncOpts struct {
	check, diff, force, quiet bool
}

func kitSync(c *cli, args []string) error {
	fs := c.flags()
	check := fs.Bool("check", false, "change nothing; print the plan")
	diff := fs.Bool("diff", false, "show how settings.local.json changes")
	all := fs.Bool("all", false, "every attached checkout under the runner's directories (or DIRs)")
	force := fs.Bool("force", false, "undo hand changes (drift); sync even if unchanged")
	quiet := fs.Bool("quiet", false, "print only problems")
	dirs, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	o := kitSyncOpts{check: *check, diff: *diff, force: *force, quiet: *quiet}
	if *all {
		return kitSyncAll(c, dirs, o)
	}
	if len(dirs) > 1 {
		return c.usage("one DIR, or --all")
	}
	dir := "."
	if len(dirs) == 1 {
		dir = dirs[0]
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	res, err := c.syncKitDir(context.Background(), abs, nil, o)
	if errors.Is(err, errKitNone) {
		if !o.quiet {
			c.println(res.summary)
		}
		if !o.check {
			c.reportMachineQuietly()
		}
		return nil
	}
	if err != nil {
		return err
	}
	if c.asJSON {
		b, _ := json.Marshal(res)
		return c.emit(b, nil)
	}
	if res.failed {
		return errors.New("the kit sync didn't finish; see above")
	}
	if !o.check {
		c.reportMachineQuietly()
	}
	return nil
}

// kitSyncResult is what syncing one checkout did.
type kitSyncResult struct {
	Dir     string   `json:"dir"`
	Repo    string   `json:"repo"`
	Commit  string   `json:"commit"`
	Plan    kitPlan  `json:"plan"`
	Errors  []string `json:"errors"`
	Skipped bool     `json:"skipped"` // nothing changed since the last sync
	summary string
	failed  bool
}

// syncKitDir syncs the checkout at dir. resolved is what its repo gets,
// if already asked (--all asks once per repo).
func (c *cli) syncKitDir(ctx context.Context, dir string, resolved *kitResolved, o kitSyncOpts) (*kitSyncResult, error) {
	top := gitTop(dir)
	if top == "" {
		return &kitSyncResult{Dir: dir, summary: dir + " isn't a git checkout: nothing to sync."}, errKitNone
	}
	repo, _, _ := gitInfo(top)
	res := &kitSyncResult{Dir: top, Repo: repo, Errors: []string{}}
	if repo == "" {
		res.summary = top + " has no forge remote Waypoint knows: nothing to sync."
		return res, errKitNone
	}
	r := resolved
	if r == nil {
		var err error
		if r, err = c.resolveKit(repo); err != nil {
			return res, err
		}
	}
	st := c.state.Kit[top]
	if r.Commit == "" || (!r.Attached && (st == nil || len(st.Managed) == 0)) {
		res.summary = "No agent kit for " + repo + ": nothing to sync."
		if r.Commit != "" {
			res.summary = repo + " isn't attached to a Waypoint project, so the kit sends it nothing."
		}
		return res, errKitNone
	}
	want := c.kitWantFor(r, top)
	res.Commit = r.Commit
	var managed []string
	if st != nil {
		managed = st.Managed
	}
	if st != nil && !o.force && !o.check && !o.diff && st.Commit == r.Commit && slices.Equal(st.Wanted, want.Plugins) {
		on := enabledPlugins(top)
		capped, _ := planCompact(want.CompactWindow, readCompactHave(top))
		if !slices.ContainsFunc(st.Managed, func(id string) bool { return !on[id] }) && len(capped) == 0 {
			res.Skipped = true
			res.summary = fmt.Sprintf("%s: kit %s, up to date.", repo, short7(r.Commit))
			if !o.quiet {
				c.println(res.summary)
			}
			return res, nil
		}
	}
	have := readKitHave(top, want.Marketplace, managed)
	plan := planKitSync(want, have, o.force)
	res.Plan = plan
	if o.diff {
		path := filepath.Join(top, ".claude", "settings.local.json")
		before, _ := os.ReadFile(path) // #nosec G304 -- the checkout's settings
		after, err := settingsAfter(before, plan)
		if err == nil && string(after) != string(before) {
			c.printf("--- %s\n+++ %s (after sync)\n%s", path, path, lineDiff(string(before), string(after)))
		}
	}
	if o.check {
		c.printKitPlan(res, true, false)
		return res, nil
	}
	cl := kitClaude()
	if cl == nil && len(plan.Actions) > 0 && !o.quiet {
		c.println("claude isn't on PATH: writing .claude/settings.local.json directly; Claude Code offers to install the plugins when this folder is next opened.")
	}
	for _, err := range applyKitPlan(ctx, cl, top, want, plan) {
		res.Errors = append(res.Errors, err.Error())
	}
	if n, err := forgetGoneInstalls(want.Marketplace); err != nil {
		_, _ = fmt.Fprintln(c.err, "wp: couldn't forget the kit installs of removed checkouts:", err)
	} else if n > 0 && !o.quiet {
		c.printf("Forgot %d kit plugin install%s of checkouts that are gone.\n", n, plural(n))
	}
	res.failed = len(res.Errors) > 0
	applied := r.Commit
	if res.failed && st != nil {
		applied = st.Commit // On failure keep the last applied commit, so the next run sees the checkout as behind and retries.
	} else if res.failed {
		applied = ""
	}
	_ = c.updateState(func(s *State) {
		if s.Kit == nil {
			s.Kit = map[string]*kitCheckout{}
		}
		for d := range s.Kit {
			if _, err := os.Stat(d); err != nil {
				delete(s.Kit, d)
			}
		}
		s.Kit[top] = &kitCheckout{Repo: repo, Commit: applied, Managed: plan.Managed, Wanted: want.Plugins, At: time.Now()}
	})
	c.printKitPlan(res, false, o.quiet)
	c.reportKit(top, repo, applied, plan, res.Errors)
	return res, nil
}

// kitClaude is Claude Code's plugin CLI (tests swap it).
var kitClaude = findClaude

// reportKit tells Waypoint how the checkout stands (best effort).
func (c *cli) reportKit(dir, repo, commit string, plan kitPlan, errs []string) {
	body := map[string]any{"machine": cmpStr(os.Getenv("WAYPOINT_MACHINE"), machineName()), "path": dir, "repo": repo,
		"kit_commit": commit, "plugins": plan.Enabled, "drift": plan.Drift, "wp_version": version, "error": strings.Join(errs, "; ")}
	if _, err := c.call("POST", "/api/v1/kit/reports", body); err != nil {
		_, _ = fmt.Fprintln(c.err, "wp: couldn't report the kit sync:", err)
	}
}

func (c *cli) printf(format string, a ...any) { _, _ = fmt.Fprintf(c.out, format, a...) }

// printKitPlan prints what a sync did (or, checking, would do).
func (c *cli) printKitPlan(res *kitSyncResult, checking, quiet bool) {
	if c.asJSON {
		return
	}
	p := res.Plan
	verb := "changed"
	if checking {
		verb = "would change"
	}
	if len(p.Actions) == 0 && len(p.Drift) == 0 && len(res.Errors) == 0 {
		if !quiet {
			c.printf("%s: kit %s, nothing to change (plugins: %s).\n", res.Repo, short7(res.Commit), orDash(strings.Join(p.Enabled, ", ")))
		}
		return
	}
	if len(p.Actions) > 0 && !quiet {
		c.printf("%s: kit %s, %s:\n", res.Repo, short7(res.Commit), verb)
		for _, a := range p.Actions {
			c.printf("  %-18s %-28s %s\n", a.Kind, cmpStr(a.Plugin, a.Setting), a.Why)
		}
	}
	for _, d := range p.Drift {
		c.printf("  %s drift: %s %s\n", res.Repo, d.Plugin, d.What)
	}
	for _, e := range res.Errors {
		c.printf("  %s failed: %s\n", res.Repo, e)
	}
	if !checking && !quiet && len(p.Actions) > 0 && len(res.Errors) == 0 {
		c.println("  New plugins load in the next Claude Code session (or /reload-plugins).")
	}
}

// kitSyncAll syncs every checkout under the runner's directories (or roots).
func kitSyncAll(c *cli, roots []string, o kitSyncOpts) error {
	if len(roots) == 0 {
		if env := os.Getenv("WAYPOINT_RUNNER_ALLOW"); env != "" {
			roots = filepath.SplitList(env)
		} else if c.state.Runner != nil {
			roots = c.state.Runner.Allow
		}
	}
	if len(roots) == 0 {
		return errors.New("no directories to look in: this machine's runner allows none (wp runner install --allow DIR), so name them: wp kit sync --all DIR")
	}
	dirs, err := parseAllowed(roots)
	if err != nil {
		return err
	}
	var checkouts []string
	for _, root := range dirs {
		checkouts = append(checkouts, findCheckouts(root, 4)...)
	}
	cache := map[string]*kitResolved{}
	failed, synced := 0, 0
	for _, dir := range checkouts {
		repo, _, _ := gitInfo(dir)
		if repo == "" {
			continue
		}
		r, ok := cache[strings.ToLower(repo)]
		if !ok {
			if r, err = c.resolveKit(repo); err != nil {
				return err
			}
			cache[strings.ToLower(repo)] = r
		}
		res, err := c.syncKitDir(context.Background(), dir, r, o)
		switch {
		case errors.Is(err, errKitNone):
		case err != nil:
			failed++
			_, _ = fmt.Fprintf(c.err, "wp: %s: %v\n", dir, err)
		case res.failed:
			failed++
		default:
			synced++
		}
	}
	if !o.quiet {
		c.printf("Synced %d checkout%s under %s.\n", synced, plural(synced), strings.Join(dirs, ", "))
	}
	if !o.check {
		c.reportMachineQuietly()
	}
	if failed > 0 {
		return fmt.Errorf("%d checkout%s didn't sync", failed, plural(failed))
	}
	return nil
}

// findCheckouts lists the git checkouts at or below root, depth levels
// down, not looking inside a checkout or dependency folders.
func findCheckouts(root string, depth int) []string {
	var out []string
	// #nosec G703 -- the folders the runner's owner allowed (or named)
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if rel != "." && strings.Count(rel, string(filepath.Separator)) >= depth {
			return filepath.SkipDir
		}
		switch d.Name() {
		case "node_modules", ".venv", "venv", "vendor", ".cache", "target", ".git":
			if p != root {
				return filepath.SkipDir
			}
		}
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil { // #nosec G703 -- walking the folders the runner's owner allowed
			out = append(out, p)
			return filepath.SkipDir
		}
		return nil
	})
	return out
}

var kitStatusCmd = &command{
	name: "status", args: "[DIR]",
	summary:  "this checkout's kit commit, plugins and drift",
	examples: []string{"wp kit status"},
	run:      kitStatus,
}

func kitStatus(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	dir := "."
	if len(pos) > 0 {
		dir = pos[0]
	}
	abs, _ := filepath.Abs(dir)
	top := gitTop(abs)
	if top == "" {
		return errors.New(abs + " isn't a git checkout")
	}
	repo, _, _ := gitInfo(top)
	if repo == "" {
		return errors.New(top + " has no forge remote")
	}
	r, err := c.resolveKit(repo)
	if err != nil {
		return err
	}
	st := c.state.Kit[top]
	if r.Commit == "" {
		c.println("No agent kit yet (an admin picks its repo in Admin → Forges).")
		return nil
	}
	want := c.kitWantFor(r, top)
	var managed []string
	applied := ""
	if st != nil {
		managed, applied = st.Managed, st.Commit
	}
	plan := planKitSync(want, readKitHave(top, want.Marketplace, managed), false)
	if c.asJSON {
		b, _ := json.Marshal(map[string]any{"repo": repo, "dir": top, "kit_commit": r.Commit, "applied": applied, "wanted": want.Plugins, "plan": plan, "notes": c.kitNotesFor(r, top)})
		return c.emit(b, nil)
	}
	state := "up to date"
	switch {
	case applied == "":
		state = "never synced here"
	case applied != r.Commit:
		state = "behind (applied " + short7(applied) + ")"
	}
	c.printf("%s in %s\nKit: %s at %s, %s\n", repo, top, r.Marketplace, short7(r.Commit), state)
	c.printf("Sends here: %s\n", orDash(strings.Join(want.Plugins, ", ")))
	if len(plan.Actions) > 0 {
		c.println("A sync would:")
		for _, a := range plan.Actions {
			c.printf("  %-18s %-28s %s\n", a.Kind, cmpStr(a.Plugin, a.Setting), a.Why)
		}
	}
	for _, d := range plan.Drift {
		c.printf("Drift: %s %s\n", d.Plugin, d.What)
	}
	if notes := c.kitNotesFor(r, top); len(notes) > 0 && c.agentHeader() != "" {
		c.printf("Notes: %d, added at session start\n", len(notes)) // An agent shell already got these notes from the session-start hook, so only the count is printed.
	} else if len(notes) > 0 {
		c.printf("Notes (added at session start):\n")
		for _, n := range notes {
			c.printf("  - %s\n", n)
		}
	}
	return nil
}

var kitLsCmd = &command{
	name: "ls", aliases: []string{"list"}, args: "",
	summary:  "the kit's plugins and what this repo gets",
	examples: []string{"wp kit ls", "wp kit ls --json"},
	run:      kitLs,
}

type kitInfoCLI struct {
	Configured bool   `json:"configured"`
	Repo       string `json:"repo"`
	Commit     string `json:"commit"`
	Kit        *struct {
		Marketplace string `json:"marketplace"`
		Plugins     []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Targeted    bool   `json:"targeted"`
			Skills      []struct {
				Name string `json:"name"`
			} `json:"skills"`
			Agents   []struct{ Name string } `json:"agents"`
			Commands []struct{ Name string } `json:"commands"`
			Hooks    []struct{ Name string } `json:"hooks"`
			MCP      []struct{ Name string } `json:"mcp"`
			Bin      []struct{ Name string } `json:"bin"`
		} `json:"plugins"`
	} `json:"kit"`
	Errors []string            `json:"errors"`
	Reach  map[string][]string `json:"reach"`
}

func kitLs(c *cli, args []string) error {
	fs := c.flags()
	if _, err := c.parse(fs, args); err != nil {
		return err
	}
	raw, err := c.call("GET", "/api/v1/kit", nil)
	if err != nil {
		return err
	}
	var k kitInfoCLI
	if err := json.Unmarshal(raw, &k); err != nil {
		return err
	}
	return c.emit(raw, func() string {
		if !k.Configured || k.Kit == nil {
			return "No agent kit yet (an admin picks its repo in Admin → Forges)."
		}
		here := map[string]bool{}
		cwd, _ := os.Getwd()
		if top := gitTop(cwd); top != "" {
			if repo, _, _ := gitInfo(top); repo != "" {
				if r, err := c.resolveKit(repo); err == nil {
					for _, p := range c.kitWantFor(r, top).Plugins {
						here[p] = true
					}
				}
			}
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Kit %s from %s at %s\n", k.Kit.Marketplace, k.Repo, short7(k.Commit))
		for _, e := range k.Errors {
			fmt.Fprintf(&b, "  ! %s\n", e)
		}
		for _, p := range k.Kit.Plugins {
			mark := " "
			if here[p.Name] {
				mark = "*"
			}
			var parts []string
			count := func(n int, what string) {
				if n > 0 {
					parts = append(parts, fmt.Sprintf("%d %s", n, what))
				}
			}
			count(len(p.Skills), "skills")
			count(len(p.Agents), "agents")
			count(len(p.Commands), "commands")
			count(len(p.Hooks), "hook events")
			count(len(p.MCP), "MCP")
			count(len(p.Bin), "tools")
			reach := "goes nowhere (not in kit.toml)"
			if p.Targeted {
				reach = fmt.Sprintf("%d repos", len(k.Reach[p.Name]))
			}
			fmt.Fprintf(&b, "%s %-14s %-40s %s · %s\n", mark, p.Name, excerptCLI(p.Description, 40), orDash(strings.Join(parts, ", ")), reach)
		}
		if len(here) > 0 {
			b.WriteString("* this repo gets it\n")
		}
		return b.String()
	})
}

func excerptCLI(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

var kitAddCmd = &command{
	name: "add", args: "skill|agent|command|hook PATH --plugin NAME [-m MSG]",
	summary: "share a skill, agent, command or hook via a PR on the kit repo",
	about: `Opens a pull request on the kit repo adding one thing to a plugin.
PATH is a skill's directory, a subagent or command .md file, or a hook:
a JSON file of hooks, or a script run on --event (with --matcher).
A new plugin is added to the marketplace in the same PR and goes nowhere
until kit.toml targets it. Text files only.`,
	examples: []string{
		"wp kit add skill ~/.claude/skills/run-tests --plugin harbor",
		"wp kit add agent .claude/agents/reviewer.md --plugin waypoint -m \"Our diff reviewer\"",
		"wp kit add hook .claude/hooks/format_python.sh --plugin python --event PostToolUse --matcher 'Edit|Write'",
	},
	run: kitAdd,
}

func kitAdd(c *cli, args []string) error {
	flags := c.flags()
	plugin := flags.String("plugin", "", "`NAME`: kit plugin to add it to (required)")
	msg := flags.String("m", "", "`MSG`: why, for the pull request")
	name := flags.String("name", "", "`NAME`: name in the kit (default: from PATH)")
	event := flags.String("event", "", "`EVENT`: hook script's event (PostToolUse, …)")
	matcher := flags.String("matcher", "", "`TOOLS`: tools a hook script matches (Edit|Write)")
	desc := flags.String("description", "", "`TEXT`: description of a new plugin")
	pos, err := c.parse(flags, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return c.usage("say what to add and its path")
	}
	if *plugin == "" {
		return c.usage("--plugin NAME is required")
	}
	kind, path := pos[0], pos[1]
	in := map[string]any{"kind": kind, "plugin": *plugin, "message": *msg, "description": *desc}
	type file struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	var files []file
	readText := func(p string) (string, error) {
		b, err := os.ReadFile(p) // #nosec G304 -- the file the person asked to share
		if err != nil {
			return "", err
		}
		if !utf8.Valid(b) || slices.Contains(b, 0) {
			return "", fmt.Errorf("%s isn't a text file: the kit takes text files only", p)
		}
		return string(b), nil
	}
	n := *name
	switch kind {
	case "skill":
		dir := path
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			dir = filepath.Dir(path)
		}
		if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
			return fmt.Errorf("%s has no SKILL.md", dir)
		}
		abs, _ := filepath.Abs(dir)
		n = cmpStr(n, filepath.Base(abs))
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == ".git" || d.Name() == "__pycache__" || d.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			rel, _ := filepath.Rel(dir, p)
			text, err := readText(p)
			if err != nil {
				return err
			}
			files = append(files, file{Path: filepath.ToSlash(rel), Content: text})
			return nil
		})
		if err != nil {
			return err
		}
	case "agent", "command":
		text, err := readText(path)
		if err != nil {
			return err
		}
		n = cmpStr(n, strings.TrimSuffix(filepath.Base(path), ".md"))
		files = append(files, file{Path: n + ".md", Content: text})
	case "hook":
		text, err := readText(path)
		if err != nil {
			return err
		}
		base := filepath.Base(path)
		n = cmpStr(n, strings.TrimSuffix(base, filepath.Ext(base)))
		if strings.HasSuffix(base, ".json") {
			var v struct {
				Hooks json.RawMessage `json:"hooks"`
			}
			if err := json.Unmarshal([]byte(text), &v); err != nil || len(v.Hooks) == 0 {
				return fmt.Errorf("%s has no hooks", path)
			}
			b, _ := json.Marshal(map[string]json.RawMessage{"hooks": v.Hooks})
			in["hooks"] = string(b)
			break
		}
		if *event == "" {
			return c.usage("a hook script needs --event (PreToolUse, PostToolUse, SessionStart, …)")
		}
		files = append(files, file{Path: base, Content: text})
		group := map[string]any{"hooks": []map[string]string{{"type": "command", "command": hookScriptCommand(base)}}}
		if *matcher != "" {
			group["matcher"] = *matcher
		}
		b, _ := json.Marshal(map[string]any{"hooks": map[string]any{*event: []any{group}}})
		in["hooks"] = string(b)
	default:
		return c.usage("add a skill, agent, command or hook, not %q", kind)
	}
	in["name"], in["files"] = n, files
	if files == nil {
		in["files"] = []file{}
	}
	raw, err := c.call("POST", "/api/v1/kit/add", in)
	if err != nil {
		return err
	}
	var res struct {
		PRURL     string   `json:"pr_url"`
		Branch    string   `json:"branch"`
		Files     []string `json:"files"`
		NewPlugin bool     `json:"new_plugin"`
	}
	_ = json.Unmarshal(raw, &res)
	return c.emit(raw, func() string {
		s := fmt.Sprintf("Opened %s (%d file%s on %s).", res.PRURL, len(res.Files), plural(len(res.Files)), res.Branch)
		if res.NewPlugin {
			s += "\n" + *plugin + " is a new plugin: add [plugins." + *plugin + "] to kit.toml to send it somewhere."
		}
		return s
	})
}

// hookScriptCommand runs a hook script shipped in a plugin through its
// interpreter (the forge API commits it without the executable bit).
func hookScriptCommand(base string) string {
	p := `"${CLAUDE_PLUGIN_ROOT}/hooks/` + base + `"`
	switch filepath.Ext(base) {
	case ".sh", ".bash":
		return "bash " + p
	case ".py":
		return "python3 " + p
	case ".js", ".mjs":
		return "node " + p
	}
	return p
}

// kitSyncHere syncs the current checkout's kit plugins for wp setup and wp
// upgrade, and says how it went in one line.
func (c *cli) kitSyncHere() string {
	const all = " (wp kit sync --all does every checkout under the runner's folders)"
	cwd, err := os.Getwd()
	if err != nil || gitTop(cwd) == "" {
		return "Agent kit: run wp kit sync in a checkout" + all + "."
	}
	out := c.out
	var buf strings.Builder
	c.out = &buf
	res, err := c.syncKitDir(context.Background(), cwd, nil, kitSyncOpts{quiet: true})
	c.out = out
	switch {
	case errors.Is(err, errKitNone):
		return "Agent kit: " + res.summary
	case err != nil:
		return "Agent kit: couldn't sync: " + err.Error()
	case res.failed:
		return "Agent kit: sync failed: " + strings.Join(res.Errors, "; ")
	}
	s := fmt.Sprintf("Agent kit: %s at %s, plugins %s", res.Repo, short7(res.Commit), orDash(strings.Join(res.Plan.Enabled, ", ")))
	if res.Skipped {
		s = fmt.Sprintf("Agent kit: %s up to date at %s", res.Repo, short7(res.Commit))
	}
	if len(res.Plan.Drift) > 0 {
		s += fmt.Sprintf("; %d drift item%s (wp kit status)", len(res.Plan.Drift), plural(len(res.Plan.Drift)))
	}
	return s + all + "."
}
