package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Fan-outs (WP-148, X6): parallel work as budgeted task sessions, not
// ad-hoc agents in one session. wp fanout PLAN.toml files a task per work
// package and starts them as background task sessions here, a few at a
// time, each in its own worktree with its own model and budget. Each
// session's wp done (or its end) starts the next; the coordinator hears one
// line per finish.

var fanoutCmd = &command{
	name: "fanout", group: "Code and CI", args: "PLAN.toml [--dry-run] | status|start|stop …",
	summary: "run work packages as budgeted background task sessions",
	about: `Files one Backlog task per [[package]] of a plan and runs each as a
background task session here, in its own worktree, with its own model and
dollar budget; 3 at a time (parallel = N, up to 6). finishing (wp ship) starts the next
package. At its budget a session is told to hand off; at 1.5x it is stopped.
This session hears one line per finished package.

Before starting, it estimates the cost and share of a week's quota; over 3%
it files a request and nothing starts until a person chooses Start.

A plan:

  project = "WP"            # default: this repo's project
  parent = "WP-33"          # optional: the tasks become its sub-tasks
  model = "sonnet"          # default model
  budget = 8                # default $ per package (else twice its estimate)
  parallel = 3
  # base = "origin/main"    # worktree start (default: remote's default branch)

  [[package]]
  title = "Split the importer"
  body = "What to do, what done looks like."   # or file = "packages/x.md"
  model = "opus"            # optional: model, budget, branch, calls
  budget = 15
  calls = 200               # expected model calls, for the estimate`,
	examples: []string{
		"wp fanout plan.toml --dry-run",
		"wp fanout plan.toml",
		"wp fanout status",
	},
	run: cmdFanout,
}

func init() {
	fanoutCmd.subs = []*command{
		{name: "status", aliases: []string{"ls", "show"}, args: "[N] [--all]", summary: "live fan-outs (or #N, or --all) and their packages",
			run: cmdFanoutStatus},
		{name: "start", args: "N", summary: "start the packages of fan-out #N that can start now",
			run: cmdFanoutStart},
		{name: "stop", args: "N [--now]", summary: "start nothing more of fan-out #N; --now stops its sessions",
			run: cmdFanoutStop},
		{name: "next", args: "N --ended KEY --outcome done|budget|ended", summary: "(hooks) record how a package's session ended and start the next", hidden: true, run: cmdFanoutNext},
	}
}

// ---- the plan ----

type fanoutPlan struct {
	Project  string              `toml:"project"`
	Parent   string              `toml:"parent"`
	Model    string              `toml:"model"`
	Budget   float64             `toml:"budget"`
	Parallel int                 `toml:"parallel"`
	Base     string              `toml:"base"`
	Packages []fanoutPlanPackage `toml:"package"`
}

type fanoutPlanPackage struct {
	Title  string  `toml:"title" json:"title"`
	Body   string  `toml:"body" json:"body"`
	File   string  `toml:"file" json:"-"`
	Model  string  `toml:"model" json:"model,omitempty"`
	Budget float64 `toml:"budget" json:"budget,omitempty"`
	Branch string  `toml:"branch" json:"branch,omitempty"`
	Calls  int     `toml:"calls" json:"calls,omitempty"`
}

const fanoutMaxBodyFile = 64 << 10

// readFanoutPlan reads a plan: unknown keys are mistakes, a package's file
// is read relative to the plan.
func readFanoutPlan(path string) (*fanoutPlan, error) {
	var p fanoutPlan
	md, err := toml.DecodeFile(path, &p)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if un := md.Undecoded(); len(un) > 0 {
		var keys []string
		for _, k := range un {
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("%s: unknown keys %s (wp fanout -h shows a plan)", path, strings.Join(keys, ", "))
	}
	for n := range p.Packages {
		pk := &p.Packages[n]
		if pk.File == "" {
			continue
		}
		if pk.Body != "" {
			return nil, fmt.Errorf("%s: package %d has both body and file; use one", path, n+1)
		}
		f := pk.File
		if !filepath.IsAbs(f) {
			f = filepath.Join(filepath.Dir(path), f)
		}
		fi, err := os.Stat(f)
		if err != nil {
			return nil, fmt.Errorf("package %d: %w", n+1, err)
		}
		if fi.Size() > fanoutMaxBodyFile {
			return nil, fmt.Errorf("package %d: %s is over %d KB; a package's body says what to do, not everything", n+1, pk.File, fanoutMaxBodyFile>>10)
		}
		b, err := os.ReadFile(f) // #nosec G304 -- a file the plan names
		if err != nil {
			return nil, fmt.Errorf("package %d: %w", n+1, err)
		}
		pk.Body = string(b)
	}
	return &p, nil
}

// repoRoot is the main checkout of the repository dir is in: worktrees hang
// off it, whichever worktree runs wp fanout.
func repoRoot(dir string) (string, error) {
	common, err := gitIn(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || common == "" {
		return "", errors.New("not in a git repository: run wp fanout in the repository the packages change")
	}
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common), nil
	}
	top, err := gitIn(dir, "rev-parse", "--show-toplevel")
	if err != nil || top == "" {
		return "", errors.New("can't find this repository's top directory")
	}
	return top, nil
}

// ---- what the server says ----

type fanoutItemView struct {
	Pos      int     `json:"pos"`
	Key      string  `json:"key"`
	Title    string  `json:"title"`
	Status   string  `json:"status"`
	State    string  `json:"state"`
	Model    string  `json:"model"`
	Budget   float64 `json:"budget"`
	Estimate float64 `json:"estimate"`
	Spent    float64 `json:"spent"`
	Branch   string  `json:"branch"`
	AttachID string  `json:"attach_id"`
	Note     string  `json:"note"`
	PRNumber int64   `json:"pr_number"`
}

type fanoutView struct {
	ID       int64             `json:"id"`
	Project  string            `json:"project"`
	State    string            `json:"state"`
	Parallel int               `json:"parallel"`
	Machine  string            `json:"machine"`
	Dir      string            `json:"dir"`
	Base     string            `json:"base"`
	Estimate float64           `json:"estimate"`
	WeekPct  float64           `json:"week_pct"`
	Basis    string            `json:"basis"`
	Request  string            `json:"request"`
	Spent    float64           `json:"spent"`
	AskOver  float64           `json:"ask_over"`
	Items    []*fanoutItemView `json:"items"`
}

type fanoutClaimView struct {
	Fanout  *fanoutView       `json:"fanout"`
	Start   []*fanoutItemView `json:"start"`
	Waiting string            `json:"waiting"`
}

// ---- wp fanout PLAN.toml ----

func cmdFanout(c *cli, args []string) error {
	fs := c.flags()
	dry := fs.Bool("dry-run", false, "print the estimate only")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("give the plan: wp fanout PLAN.toml (wp fanout -h shows one)")
	}
	plan, err := readFanoutPlan(pos[0])
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err := repoRoot(cwd)
	if err != nil {
		return err
	}
	project := plan.Project
	if project == "" && plan.Parent == "" {
		sc, err := c.scopeFor(scopeFlags{})
		if err != nil {
			return err
		}
		if project, err = sc.home(); err != nil {
			return err
		}
	}
	base := plan.Base
	if base == "" {
		_, _ = gitIn(root, "fetch", "--quiet", "origin")
		base = defaultBranchRef(root)
	}
	pkgs := make([]map[string]any, 0, len(plan.Packages))
	for _, p := range plan.Packages {
		b, _ := json.Marshal(p)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		pkgs = append(pkgs, m)
	}
	body := map[string]any{"project": project, "parent": plan.Parent, "model": plan.Model, "budget": plan.Budget, "parallel": plan.Parallel,
		"base": base, "machine": machineName(), "dir": root, "packages": pkgs, "dry_run": *dry}
	if c.agentHeader() == "" {
		body["agent"] = "anthropic/claude-code" // the sessions are Claude Code's
	}
	raw, err := c.call("POST", "/api/v1/fanouts", body)
	if err != nil {
		return err
	}
	var f fanoutView
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	if c.asJSON && *dry {
		return c.emit(raw, nil)
	}
	w := c.out
	if c.asJSON {
		w = c.err
	}
	_, _ = fmt.Fprint(w, fanoutEstimateText(&f, *dry))
	switch {
	case *dry:
		return nil
	case f.State == "asked":
		fmt.Fprintf(w, "That is over %.0f%% of a week, so your person was asked (%s). Nothing starts until they answer, then `wp fanout start %d` starts it.\n",
			f.AskOver, f.Request, f.ID)
		if c.asJSON {
			return c.emit(raw, nil)
		}
		return nil
	}
	return c.fanoutStart(f.ID, true)
}

// fanoutEstimateText is the plan's packages and estimate.
func fanoutEstimateText(f *fanoutView, dry bool) string {
	var b strings.Builder
	if dry {
		fmt.Fprintf(&b, "Fan-out (dry run) in %s, %d at a time:\n", f.Project, f.Parallel)
	} else {
		fmt.Fprintf(&b, "Fan-out #%d in %s, %d at a time:\n", f.ID, f.Project, f.Parallel)
	}
	for _, it := range f.Items {
		key := it.Key
		if key == "" {
			key = fmt.Sprintf("#%d", it.Pos)
		}
		fmt.Fprintf(&b, "  %-8s %-8s est $%6.2f  stops at $%6.2f  %s\n", key, it.Model, it.Estimate, it.Budget, truncRunes(it.Title, 60))
	}
	fmt.Fprintf(&b, "Estimate: $%.2f, about %.1f%% of a week (asks first over %.0f%%).\nBasis: %s.\n", f.Estimate, f.WeekPct, f.AskOver, f.Basis)
	return b.String()
}

// ---- starting ----

// fanoutStart claims what can start now and starts it; wait: when nothing
// can start only because packages wait for the safety screen, wait a while.
func (c *cli) fanoutStart(id int64, wait bool) error {
	deadline := time.Now().Add(90 * time.Second)
	for {
		claim, err := c.claimFanout(id)
		if err != nil {
			return err
		}
		if len(claim.Start) > 0 || !wait || !strings.Contains(claim.Waiting, "safety screen") || time.Now().After(deadline) {
			started := c.launchFanout(claim, func(format string, a ...any) { fmt.Fprintf(c.out, format+"\n", a...) })
			if claim.Waiting != "" {
				c.println(claim.Waiting)
			}
			if started > 0 {
				fmt.Fprintf(c.out, "Each package's session starts the next when it finishes (wp ship); this session hears one line per finish (wp fanout status %d).\n", id)
			} else if strings.Contains(claim.Waiting, "safety screen") {
				fmt.Fprintf(c.out, "Waypoint tells this session when a package can start; then `wp fanout start %d`.\n", id)
			}
			return nil
		}
		time.Sleep(5 * time.Second)
	}
}

func (c *cli) claimFanout(id int64) (*fanoutClaimView, error) {
	var claim fanoutClaimView
	if err := c.do("POST", "/api/v1/fanouts/"+strconv.FormatInt(id, 10)+"/claim", map[string]any{"machine": machineName()}, &claim); err != nil {
		return nil, err
	}
	if claim.Fanout == nil {
		return nil, errors.New("the server returned no fan-out")
	}
	return &claim, nil
}

// tellFanoutItem posts a package's start or failure, trying a few times: the
// session is already running (or not) here, and the server must know.
func (c *cli) tellFanoutItem(path string, body map[string]any) error {
	var err error
	for i := 0; i < 3; i++ {
		if _, err = c.call("POST", path, body); err == nil {
			return nil
		}
		time.Sleep(time.Duration(i+1) * time.Second)
	}
	return err
}

// launchFanout starts a session for each claimed package and tells the
// server how each went; it returns how many started.
func (c *cli) launchFanout(claim *fanoutClaimView, say func(string, ...any)) int {
	f := claim.Fanout
	started := 0
	prefs := c.mySessionPrefs()
	for _, it := range claim.Start {
		path := "/api/v1/fanouts/" + strconv.FormatInt(f.ID, 10) + "/items/" + esc(it.Key)
		attach, err := launchFanoutItem(f, it, c.kickoffTemplate(it.Key), prefs, c.noteTypedStart)
		if err != nil {
			reason := strings.SplitN(err.Error(), "\n", 2)[0]
			if err := c.tellFanoutItem(path+"/failed", map[string]any{"note": reason}); err != nil {
				say("%s not started: %s (and the server wasn't told: %v; its sweep gives up on it within 10 minutes)", it.Key, reason, err)
				continue
			}
			say("%s not started: %s", it.Key, reason)
			continue
		}
		if err := c.tellFanoutItem(path+"/started", map[string]any{"attach_id": attach}); err != nil {
			say("Warning: %s is running but the server wasn't told (%v); wp fanout stop can't find its session until it reports in", it.Key, err)
		}
		started++
		open := ""
		if attach != "" {
			open = "; open it: claude attach " + attach
		}
		say("Started %s (%s, budget $%.2f) on branch %s%s", it.Key, it.Model, it.Budget, it.Branch, open)
	}
	return started
}

// fanoutWorktree is where a package's branch is checked out.
func fanoutWorktree(dir, branch string) string {
	return filepath.Join(dir, ".claude", "worktrees", branch)
}

// launchFanoutItem makes (or reuses) the package's worktree and starts its
// session there in the background, its first prompt made from the kickoff
// template tmpl (WP-1037); the id claude attach takes.
func launchFanoutItem(f *fanoutView, it *fanoutItemView, tmpl string, prefs *sessionPrefs, note func(context.Context, string) error) (string, error) {
	bin, err := exec.LookPath(cliBinaries["claude"])
	if err != nil {
		return "", errors.New("claude (Claude Code) isn't installed here")
	}
	wt := fanoutWorktree(f.Dir, it.Branch)
	if fi, err := os.Stat(wt); err != nil || !fi.IsDir() {
		if _, err := gitIn(f.Dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+it.Branch); err == nil {
			_, err = gitIn(f.Dir, "worktree", "add", "--", wt, it.Branch)
			if err != nil {
				return "", fmt.Errorf("git worktree add: %w", err)
			}
		} else {
			args := []string{"worktree", "add", "-b", it.Branch, "--", wt}
			if f.Base != "" {
				args = append(args, f.Base)
			}
			if _, err := gitIn(f.Dir, args...); err != nil {
				return "", fmt.Errorf("git worktree add: %w", err)
			}
		}
	}
	l := taskLaunch{tasks: []string{it.Key}, bg: true, model: it.Model, fanout: f.ID, budget: it.Budget,
		name: strings.TrimSpace(it.Key + " " + truncRunes(it.Title, 60))}
	prefs.forDir(wt).fill(&l)
	l.prompt = fanoutPrompt(f, it, startKickoff(context.Background(), wt, l.sessionEnv(os.Environ()), it.Key, it.Title, tmpl))
	warnTypedStart(os.Stderr, note(context.Background(), l.prompt))
	id, text, err := launchBackground(bin, wt, l)
	if err != nil {
		return "", err
	}
	if id == "" && strings.TrimSpace(text) == "" {
		return "", errors.New("claude --bg said nothing")
	}
	return id, nil
}

// fanoutPrompt is a package session's first prompt: its kickoff, then
// what the package is.
func fanoutPrompt(f *fanoutView, it *fanoutItemView, kickoff string) string {
	return kickoff + fmt.Sprintf("\n\nThis session is package %d of fan-out #%d: it has its own worktree (branch %s) and a $%.2f budget; if it runs out, you will be told to hand off.",
		it.Pos, f.ID, it.Branch, it.Budget)
}

// ---- status, start, stop ----

func fanoutID(c *cli, s string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(s, "#"), 10, 64)
	if err != nil || id <= 0 {
		return 0, c.usage("%q isn't a fan-out number (wp fanout status lists them)", s)
	}
	return id, nil
}

func cmdFanoutStatus(c *cli, args []string) error {
	fs := c.flags()
	all := fs.Bool("all", false, "recent fan-outs too")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	var list []*fanoutView
	var raw []byte
	switch len(pos) {
	case 0:
		path := "/api/v1/fanouts"
		if *all {
			path += "?all=1"
		}
		if raw, err = c.call("GET", path, nil); err == nil {
			err = json.Unmarshal(raw, &list)
		}
	case 1:
		id, err2 := fanoutID(c, pos[0])
		if err2 != nil {
			return err2
		}
		var f fanoutView
		if raw, err = c.call("GET", "/api/v1/fanouts/"+strconv.FormatInt(id, 10), nil); err == nil {
			err = json.Unmarshal(raw, &f)
		}
		list = []*fanoutView{&f}
	default:
		return c.usage("one fan-out at a time: wp fanout status N")
	}
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		if len(list) == 0 {
			return "No live fan-outs (--all shows recent ones)."
		}
		var b strings.Builder
		for _, f := range list {
			b.WriteString(fanoutStatusText(f))
		}
		return b.String()
	})
}

// fanoutStatusText is a fan-out and a line per package.
func fanoutStatusText(f *fanoutView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#%d %s %s: %d at a time, spent $%.2f of an estimated $%.2f (%.1f%% of a week)", f.ID, f.Project, f.State, f.Parallel, f.Spent, f.Estimate, f.WeekPct)
	if f.Request != "" && f.State == "asked" {
		b.WriteString(", waiting on " + f.Request)
	}
	b.WriteString("\n")
	for _, it := range f.Items {
		line := fmt.Sprintf("  %-8s %-9s %-7s $%.2f/$%.2f  %s", it.Key, it.State, it.Model, it.Spent, it.Budget, it.Branch)
		if it.PRNumber > 0 {
			line += fmt.Sprintf("  PR #%d", it.PRNumber)
		}
		if it.Note != "" && it.Note != it.State {
			line += "  (" + it.Note + ")"
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func cmdFanoutStart(c *cli, args []string) error {
	pos, err := c.parse(c.flags(), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("which fan-out? wp fanout start N")
	}
	id, err := fanoutID(c, pos[0])
	if err != nil {
		return err
	}
	return c.fanoutStart(id, false)
}

// stopAttached runs `claude stop ID` for each running or starting item and
// says how many stopped. The id came from the server: only the shape
// claude --bg prints reaches the command line, where anything else (a leading
// dash) would be read as an option of claude stop. It takes no "--" (checked:
// "unknown option '--'"), so the shape is the guard.
func stopAttached(bin string, items []*fanoutItemView) int {
	stopped := 0
	for _, it := range items {
		if (it.State == "running" || it.State == "starting") && core.ValidAttachID(it.AttachID) {
			cmd := exec.Command(bin, "stop", it.AttachID) // #nosec G204 -- claude, with the id claude --bg printed
			if cmd.Run() == nil {
				stopped++
			}
		}
	}
	return stopped
}

func cmdFanoutStop(c *cli, args []string) error {
	fs := c.flags()
	now := fs.Bool("now", false, "also stop its running sessions here")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("which fan-out? wp fanout stop N")
	}
	id, err := fanoutID(c, pos[0])
	if err != nil {
		return err
	}
	var f fanoutView
	if err := c.do("POST", "/api/v1/fanouts/"+strconv.FormatInt(id, 10)+"/stop", nil, &f); err != nil {
		return err
	}
	stopped := 0
	if *now && f.Machine == machineName() {
		bin, err := exec.LookPath(cliBinaries["claude"])
		if err == nil {
			stopped = stopAttached(bin, f.Items)
		}
	}
	if c.asJSON {
		b, _ := json.Marshal(f)
		return c.emit(b, nil)
	}
	running := 0
	for _, it := range f.Items {
		if it.State == "running" || it.State == "starting" {
			running++
		}
	}
	switch {
	case stopped > 0:
		fmt.Fprintf(c.out, "Fan-out #%d: nothing more starts; stopped %d running session(s).\n", id, stopped)
	case running > 0:
		fmt.Fprintf(c.out, "Fan-out #%d: nothing more starts; %d session(s) still run to their end (--now stops them).\n", id, running)
	default:
		fmt.Fprintf(c.out, "Fan-out #%d: nothing more starts.\n", id)
	}
	return nil
}

// ---- how a package's session ended ----

// cmdFanoutNext is run by the hooks (detached) when a package's session
// stops at its budget or ends: it records that and starts the next.
func cmdFanoutNext(c *cli, args []string) error {
	fs := c.flags()
	ended := fs.String("ended", "", "the `KEY` of the package whose session ended")
	outcome := fs.String("outcome", "ended", "done, budget or ended")
	spent := fs.Float64("spent", -1, "what its session spent, in USD")
	transcript := fs.String("transcript", "", "its session's transcript, to count what it spent")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *ended == "" {
		return c.usage("wp fanout next N --ended KEY --outcome done|budget|ended")
	}
	id, err := fanoutID(c, pos[0])
	if err != nil {
		return err
	}
	cost := *spent
	if cost < 0 {
		cost = c.sessionSpent(*transcript)
	}
	return c.fanoutAdvance(id, strings.ToUpper(*ended), *outcome, cost, func(format string, a ...any) { fmt.Fprintf(c.out, format+"\n", a...) })
}

// fanoutAdvance records how a package ended and starts what can start
// now; it returns how many started.
func (c *cli) fanoutAdvance(id int64, key, outcome string, spent float64, say func(string, ...any)) error {
	_, err := c.call("POST", "/api/v1/fanouts/"+strconv.FormatInt(id, 10)+"/items/"+esc(key)+"/end", map[string]any{"outcome": outcome, "spent": spent})
	if err != nil {
		return err
	}
	claim, err := c.claimFanout(id)
	if err != nil {
		return err
	}
	if c.launchFanout(claim, say) == 0 && claim.Waiting != "" {
		say("%s", claim.Waiting)
	}
	return nil
}

// sessionSpent is what a session tree has spent so far, at the cached
// prices: its transcript, else this session's.
func (c *cli) sessionSpent(transcript string) float64 {
	if transcript == "" {
		transcript = os.Getenv("WAYPOINT_TRANSCRIPT")
	}
	if transcript == "" || !filepath.IsAbs(transcript) {
		return 0
	}
	t := newBurnTree()
	t.update(burnFiles(transcript), c.loadBurnPrices(false))
	return t.totals().Cost
}

// fanoutEnv is the fan-out and budget a package's session runs under (0, 0
// outside one).
func fanoutEnv() (id int64, budget float64) {
	id, _ = strconv.ParseInt(strings.TrimSpace(os.Getenv("WP_FANOUT")), 10, 64)
	budget, _ = strconv.ParseFloat(strings.TrimSpace(os.Getenv("WP_BUDGET_USD")), 64)
	if id <= 0 {
		return 0, 0
	}
	return id, max(budget, 0)
}

// fanoutHandOff is a package session's wp done: its package is done, and
// the next package starts in its own session. The session then ends.
func (c *cli) fanoutHandOff(id int64) {
	keys := splitKeys(os.Getenv("WP_TASK"))
	if len(keys) == 0 {
		return
	}
	w := c.out
	if c.asJSON {
		w = c.err
	}
	var started []string
	err := c.fanoutAdvance(id, keys[0], "done", c.sessionSpent(""), func(format string, a ...any) {
		line := fmt.Sprintf(format, a...)
		if strings.HasPrefix(line, "Started ") {
			started = append(started, strings.Fields(line)[1])
			return
		}
		_, _ = fmt.Fprintln(w, line) // not started, not told, waiting: keep them
	})
	if err != nil {
		fmt.Fprintf(w, "Fan-out #%d not told: %v. The coordinator starts the next package.\n", id, err)
		c.reportHandoff("none", "fan-out: "+err.Error())
		return
	}
	if len(started) > 0 {
		c.reportHandoff("started", "fan-out #"+strconv.FormatInt(id, 10)+": "+strings.Join(started, ","))
		return
	}
	c.reportHandoff("none", "fan-out #"+strconv.FormatInt(id, 10))
}

// ---- the hooks' part ----

// fanoutBudgetLine is what the PostToolUse hook says when a package's
// session reaches its budget (once), and whether it must stop now (at 1.5
// times it). Both empty below the budget or outside a fan-out.
func fanoutBudgetLine(st *burnState, cost float64) (line, stop string, changed bool) {
	id, budget := fanoutEnv()
	if id == 0 || budget <= 0 {
		return "", "", false
	}
	keys := splitKeys(os.Getenv("WP_TASK"))
	key := "this task"
	if len(keys) > 0 {
		key = keys[0]
	}
	if cost >= 1.5*budget && !st.BudgetStopped {
		st.BudgetStopped, st.BudgetSaid = true, true
		return "", fmt.Sprintf("Stopped: %s (fan-out #%d) spent $%.2f, 1.5 times its $%.2f budget.", key, id, cost, budget), true
	}
	if cost >= budget && !st.BudgetSaid {
		st.BudgetSaid = true
		return fmt.Sprintf("Budget reached: %s (fan-out #%d) has spent $%.2f of its $%.2f. Hand off now: commit what you have on this branch, run `wp release %s -m \"what is done, what is left, where to start\"`, and stop. This session ends at your next stop; at 1.5 times the budget it is stopped.",
			key, id, cost, budget, key), "", true
	}
	return "", "", false
}

// fanoutSessionOver: a package's session reached its budget (the burn
// hook told it so), and what it has spent.
func (c *cli) fanoutSessionOver(session string) (bool, float64) {
	if id, _ := fanoutEnv(); id == 0 {
		return false, 0
	}
	path := c.burnPath(session)
	if path == "" {
		return false, 0
	}
	var st burnState
	if readJSON(path, &st) != nil || st.Tree == nil {
		return false, 0
	}
	return st.BudgetSaid, st.Tree.totals().Cost
}

// fanoutEnded starts `wp fanout next` detached, for a package's session
// that stopped (outcome budget) or ended: the hook returns first.
func fanoutEnded(outcome, transcript string, spent float64) {
	id, _ := fanoutEnv()
	keys := splitKeys(os.Getenv("WP_TASK"))
	if id == 0 || len(keys) == 0 {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	args := []string{"fanout", "next", strconv.FormatInt(id, 10), "--ended", keys[0], "--outcome", outcome}
	if spent > 0 {
		args = append(args, "--spent", strconv.FormatFloat(spent, 'f', 4, 64))
	} else if filepath.IsAbs(transcript) {
		args = append(args, "--transcript", transcript)
	}
	backgroundStart(exe, args...)
}
