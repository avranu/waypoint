// Command wp is the Waypoint command-line client: issues, pages and the
// agent work queue from a terminal or an agent's shell.
//
//	curl -fsSL https://waypoint.example.com/cli | sh   # install
//	wp setup                                            # once per machine: sign in, hooks, runner
//	wp next                                             # take the next task (agents)
//	wp ls                                               # open issues in this repo's project
//
// Commands work in a scope: the project of the git repo they run in,
// unless told otherwise (see `wp help scope`). Output is short plain text;
// add --json for the API's JSON. `wp help --all` prints the full reference
// of every command.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"

	"git.jmann.me/claude/waypoint/internal/wql"
)

var version = "dev"

// command is one `wp` command, or a group of subcommands (page, project…).
type command struct {
	name     string
	aliases  []string
	args     string
	summary  string
	about    string
	examples []string
	run      func(c *cli, args []string) error
	subs     []*command
	group    string
	hidden   bool
	parent   *command
}

func (cmd *command) path() string {
	if cmd.parent != nil {
		return cmd.parent.path() + " " + cmd.name
	}
	return cmd.name
}

func (cmd *command) is(name string) bool {
	return cmd.name == name || slices.Contains(cmd.aliases, name)
}

func (cmd *command) sub(name string) *command {
	for _, s := range cmd.subs {
		if s.is(name) {
			return s
		}
	}
	return nil
}

var commands []*command

// groups orders the headings of `wp help`.
var groups = []string{"Start here", "Tasks", "Code and CI", "Knowledge", "Issues", "Projects and time", "Pages", "Setup", "Hooks"}

func init() {
	commands = []*command{
		contextCmd,
		runnerCmd,
		nextCmd, batchCmd, startCmd, shipCmd, shotCmd, previewCmd, releaseCmd, declineCmd, claimsCmd, pauseCmd, resumeCmd, usageCmd, queueCmd, agentsCmd, autopilotCmd, followupCmd, activityCmd,
		specCmd, defineCmd, searchCmd, briefCmd, prepCmd, codeCmd, glossaryCmd, instructionCmd, designCmd, reviewCmd, riskCmd, goalsCmd, goalCmd, traceCmd, graphCmd, unlinkCmd,
		prCmd, tidyCmd, worktreeCmd, unshippedCmd, slotCmd, testsumCmd, benchCmd, burnCmd, fanoutCmd, ciCmd, codeqlCmd, auditCmd, mistakesCmd, escapesCmd, serverCmd, aiCmd, experimentCmd, objectiveCmd,
		lsCmd, showCmd, newCmd, editCmd, publishCmd, draftCmd, holdCmd, mvCmd, assignCmd, unassignCmd, commentCmd, worklogCmd, attachCmd, filesCmd, linkCmd, relateCmd, rmCmd, openCmd,
		projectCmd, envCmd, timerCmd, logCmd, automationCmd,
		pageCmd,
		requestCmd, askCmd,
		setupCmd, authCmd, configCmd, doctorCmd, hookCmd, kitCmd, sandboxCmd, statuslineCmd, updateCmd, versionCmd, helpCmd,
	}
}

// linkCommands points subcommands at their parents. Groups add their
// subcommands in their own files' init, so this runs on first use.
var linkCommands = sync.OnceFunc(func() {
	addDoctorTools()
	var link func(parent *command, list []*command)
	link = func(parent *command, list []*command) {
		for _, c := range list {
			c.parent = parent
			link(c, c.subs)
		}
	}
	link(nil, commands)
})

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes one invocation and returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
	linkCommands()
	c, err := newCLI(stdout, stderr)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "wp:", err)
		return 1
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		c.printHelp()
		return 0
	}
	if args[0] == "--version" {
		args[0] = "version"
	}
	if (args[0] == "issue" || args[0] == "issues" || args[0] == "task" || args[0] == "tasks") && (len(args) == 1 || findCommand(args[1]) != nil) {
		args = args[1:]
		if len(args) == 0 {
			args = []string{"ls"}
		}
	}
	cmd := findCommand(args[0])
	if cmd == nil {
		fmt.Fprintf(stderr, "wp: unknown command %q; run wp help for the list\n", args[0])
		return 2
	}
	if err := c.dispatch(cmd, args[1:]); err != nil {
		if errors.Is(err, errHelpShown) {
			return 0
		}
		var ec exitCode
		if errors.As(err, &ec) {
			return int(ec)
		}
		var ue usageError
		if errors.As(err, &ue) {
			c.printUsageError(stderr, ue)
			return 2
		}
		_, _ = fmt.Fprintln(stderr, "wp:", err)
		return 1
	}
	return 0
}

// dispatch runs a command, or routes a group's first argument to its
// subcommand.
func (c *cli) dispatch(cmd *command, args []string) error {
	c.cur = cmd
	if len(cmd.subs) == 0 {
		return cmd.run(c, args)
	}
	if len(args) > 0 {
		if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
			if cmd.run != nil && args[0] != "help" {
				// A group that also runs by itself lists its own flags too;
				// one whose run takes no flags (wp design) says it has no
				// command "-h", so it gets the group's help instead.
				if err := cmd.run(c, args); !errors.As(err, new(usageError)) {
					return err
				}
			}
			c.commandHelp(cmd, nil)
			return errHelpShown
		}
		if s := cmd.sub(args[0]); s != nil {
			return c.dispatch(s, args[1:])
		}
	}
	if cmd.run != nil {
		return cmd.run(c, args)
	}
	if len(args) == 0 {
		c.commandHelp(cmd, nil)
		return errHelpShown
	}
	return c.usage("no %s command %q", cmd.name, args[0])
}

// aliases maps other names people (and models) reach for to commands.
var aliases = map[string]string{
	"list": "ls", "view": "show", "get": "show", "cat": "show",
	"create": "new", "add": "new", "update": "edit", "set": "edit", "move": "mv", "status": "mv", "transition": "mv",
	"delete": "rm", "remove": "rm", "del": "rm", "take": "next", "claim": "next", "pickup": "next", "finish": "done", "complete": "done",
	"projects": "project", "pages": "page", "login": "auth", "whoami": "context", "where": "context", "time": "log",
}

func findCommand(name string) *command {
	if a, ok := aliases[name]; ok {
		name = a
	}
	for _, c := range commands {
		if c.is(name) {
			return c
		}
	}
	return nil
}

// findPath finds a command from words like ["page", "new"].
func findPath(words []string) *command {
	cmd := findCommand(words[0])
	for _, w := range words[1:] {
		if cmd == nil {
			return nil
		}
		cmd = cmd.sub(w)
	}
	return cmd
}

const scopeHelp = `Scope: which projects a command works in

ls, next, batch, new, page ls and page new use the first of:
  1. -p REF                 project key, name, repo (owner/name), repo name, or a
                            path through parents (ACME/APP); any case
  2. wp ls REF [FILTERS]    a first word naming a project, followed only by
                            key:value filters
  3. WAYPOINT_PROJECT=REF   same as -p
  4. the git repo here      the projects its origin remote is attached to
  5. wp config set project  your default project
  6. every project (ls, next, batch and search stop and ask for -p or -A instead)
-A covers every project. Sub-projects are included; new items go in the project itself.
In a git repo attached to no project, these commands fail rather than fall back.

Commands given an issue key or page code find it anywhere, in any case, with
or without the dash (app12, apppl13). wp context shows the scope here;
wp project show takes the project as an argument.
`

// commonSyntax is how the commands agents looked up most are called (WP-239:
// wp pr, request, ship, new, page, ask, design, comment, in 274 sessions).
// Every agent session starts with it (not wp help: that would repeat it), so a call
// needs no wp X -h first. TestCommonSyntax checks each flag exists. The
// wp next line is there because agents capped its answer and ran wp ls
// beside it, not knowing it is short and whole (WP-242). KEY... marks the
// commands that take several at once, saving calls (WP-221).
var commonSyntax = []string{
	`wp next [KEY] (read it whole, no | head) · wp ship [KEY] -m "SUMMARY" (ships any code, else finishes; also wp done)`,
	`wp new [bug|story|subtask] "TITLE" -d "MARKDOWN" [-f FILE] [--parent KEY] [--for claude|none] [--priority P]`,
	`wp show CODE... (a KEY, R-N, ADRn, PLn or #N) · wp comment KEY "TEXT" · wp edit KEY... [--status S] [--title T] [-m COMMENT]`,
	`wp pr show|check|log|todo N... · wp pr wait N · wp pr merge N... --when-ready`,
	`wp pr ack ID... --accept|--reject -m WHY · wp pr reply ID -m TEXT · wp pr fixed ID... · wp pr dismiss ID... -m NOTE`,
	`wp ci status · wp ci log SHA|--run N [--job NAME] [--full]`,
	`wp request ls [--answered] · wp request show|withdraw R-N... · wp request todo "WHAT" [-d DETAILS] · wp request run "COMMAND" -d WHY`,
	`wp page new plan "TITLE" -f FILE [--issue KEY] · wp page show CODE · wp design new decision "TITLE" -f FILE --for KEY · wp design show ADRn...`,
}

func (c *cli) printHelp() {
	fmt.Fprintf(c.out, "wp %s: the Waypoint command-line client (issues, pages and the agent work queue)\n\n", version)
	c.println("Usage: wp COMMAND [ARGUMENTS] [FLAGS]")
	c.println()
	c.println("Works in the project of the git repo here (-p PROJECT; -A every project).")
	c.println()
	for i, g := range groups {
		if i > 0 {
			c.println()
		}
		c.println(g + ":")
		for _, cmd := range commands {
			if cmd.group == g && !cmd.hidden {
				fmt.Fprintf(c.out, "  %-10s %s\n", cmd.name, cmd.summary)
			}
		}
	}
	c.println()
	c.println("Help topics:")
	c.println("  wp help COMMAND   a command's flags and examples (or wp COMMAND -h)")
	c.println("  wp help scope     how the project is chosen")
	c.println("  wp help query     the query language")
	c.println("  wp help --all     everything")
	c.println()
	c.println("KEY..., R-N..., ID...: several, by spaces or commas (R-5,6,7). Every command takes --json.")
	if c.cfg.Server == "" && c.server() == "" {
		c.println("\nNot set up yet: wp setup https://your-waypoint")
	}
}

var helpCmd = &command{
	name: "help", group: "Setup", args: "[COMMAND [SUBCOMMAND] | scope | query | --all]",
	summary: "show help for a command or topic",

	examples: []string{"wp help ls", "wp help page new", "wp help query", "wp help --all"},
	run:      cmdHelp,
}

func cmdHelp(c *cli, args []string) error {
	fs := c.flags()
	all := fs.Bool("all", false, "every command, flag and topic")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	switch {
	case *all:
		c.fullReference()
		return nil
	case len(pos) == 0:
		c.printHelp()
		return nil
	case pos[0] == "scope":
		c.print(scopeHelp)
		return nil
	case pos[0] == "query" || pos[0] == "wql":
		c.print(wql.Help)
		return nil
	}
	cmd := findPath(pos)
	if cmd == nil {
		return c.usage("no command %q", strings.Join(pos, " "))
	}
	return c.showHelp(cmd)
}

// showHelp prints a command's help, flags included: commands define their
// flags as they run, so it runs them with -h.
func (c *cli) showHelp(cmd *command) error {
	if len(cmd.subs) > 0 && cmd.run == nil {
		c.commandHelp(cmd, nil)
		return nil
	}
	c.cur = cmd
	if err := cmd.run(c, []string{"-h"}); err != nil && !errors.Is(err, errHelpShown) {
		return err
	}
	return nil
}

// fullReference is `wp help --all`.
func (c *cli) fullReference() {
	c.println("# wp: the Waypoint command-line client")
	c.println()
	c.println("Generated by `wp help --all`; each section is also `wp help COMMAND`.")
	c.println()
	c.println("```")
	v := version
	version = "VERSION"
	c.printHelp()
	version = v
	c.println("```")
	c.println()
	c.println("## Scope")
	c.println()
	c.println("```")
	c.print(scopeHelp)
	c.println("```")
	var walk func(list []*command)
	walk = func(list []*command) {
		for _, cmd := range list {
			if cmd.hidden {
				continue
			}
			c.println()
			c.println("## wp " + cmd.path())
			c.println()
			c.println("```")
			_ = c.showHelp(cmd)
			c.println("```")
			walk(cmd.subs)
		}
	}
	walk(commands)
	c.println()
	c.println("## Query language")
	c.println()
	c.print(strings.ReplaceAll(strings.Replace(wql.Help, "# Waypoint query language\n", "", 1), "\n## ", "\n### "))
}

// commandHelp prints a command's help; fs (when it has flags) lists them.
func (c *cli) commandHelp(cmd *command, fs *flag.FlagSet) {
	c.println(strings.TrimSpace("wp " + cmd.path() + " " + cmd.args))
	fmt.Fprintf(c.out, "  %s\n", cmd.summary)
	if len(cmd.aliases) > 0 {
		fmt.Fprintf(c.out, "  Also: %s\n", strings.Join(cmd.aliases, ", "))
	}
	if cmd.about != "" {
		c.println()
		c.println(strings.TrimRight(cmd.about, "\n"))
	}
	if len(cmd.subs) > 0 {
		c.println()
		c.println("Subcommands:")
		for _, s := range cmd.subs {
			fmt.Fprintf(c.out, "  %s\n      %s\n", strings.TrimSpace("wp "+s.path()+" "+s.args), s.summary)
		}
		fmt.Fprintf(c.out, "Run wp %s SUBCOMMAND -h for its flags.\n", cmd.path())
	}
	if fs != nil {
		printFlags(c.out, fs)
	}
	if len(cmd.examples) > 0 {
		c.println()
		c.println("Examples:")
		for _, e := range cmd.examples {
			c.println("  " + e)
		}
	}
}

// printUsageError says what was wrong and how the command is called: its
// subcommands, or its flags and examples, so the next try needs no wp -h
// (agents looked one up ~4 times a session, often after an error: WP-239).
func (c *cli) printUsageError(w io.Writer, ue usageError) {
	fmt.Fprintf(w, "wp %s: %s\nusage: %s\n", ue.cmd.path(), ue.msg, strings.TrimSpace("wp "+ue.cmd.path()+" "+ue.cmd.args))
	if len(ue.cmd.subs) > 0 {
		for _, s := range ue.cmd.subs {
			fmt.Fprintf(w, "  %s\n", strings.TrimSpace("wp "+s.path()+" "+s.args))
		}
		fmt.Fprintf(w, "Run wp %s SUBCOMMAND -h for its flags.\n", ue.cmd.path())
		return
	}
	if ue.fs != nil {
		printFlags(w, ue.fs)
	}
	if n := min(len(ue.cmd.examples), 3); n > 0 {
		_, _ = fmt.Fprintln(w, "Examples:")
		for _, e := range ue.cmd.examples[:n] {
			_, _ = fmt.Fprintln(w, "  "+e)
		}
	}
}

// printFlags lists a command's flags, aliases together (-p, --project),
// leaving out the ones every command takes.
func printFlags(w io.Writer, fs *flag.FlagSet) {
	type entry struct {
		names      []string
		arg, usage string
		def        string
		boolean    bool
	}
	var list []*entry
	byValue := map[flag.Value]*entry{}
	fs.VisitAll(func(f *flag.Flag) {
		if f.Name == "json" || f.Name == "agent" && f.Usage == agentFlagUsage || f.Name == "h" || f.Name == "help" || strings.HasPrefix(f.Usage, "(hidden)") {
			return
		}
		if e := byValue[f.Value]; e != nil {
			e.names = append(e.names, f.Name)
			return
		}
		arg, usage := flag.UnquoteUsage(f)
		e := &entry{names: []string{f.Name}, usage: usage, def: f.DefValue}
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			e.boolean = true
		} else {
			e.arg = strings.ToUpper(map[string]string{"string": "value", "value": "value", "int": "n", "int64": "n", "float": "n", "float64": "n"}[arg])
			if e.arg == "" {
				e.arg = arg
			}
		}
		byValue[f.Value] = e
		list = append(list, e)
	})
	if len(list) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Flags:")
	for _, e := range list {
		slices.SortFunc(e.names, func(a, b string) int { return len(a) - len(b) })
		var names []string
		for _, n := range e.names {
			if len(n) == 1 {
				names = append(names, "-"+n)
			} else {
				names = append(names, "--"+n)
			}
		}
		head := strings.Join(names, ", ")
		if !e.boolean {
			head += " " + e.arg
		}
		usage := strings.TrimPrefix(e.usage, e.arg+": ")
		if e.def != "" && e.def != "0" && e.def != "false" && !e.boolean {
			usage += " (default " + e.def + ")"
		}
		lines := wrap(usage, 80-25)
		if len(head) <= 22 {
			fmt.Fprintf(w, "  %-22s %s\n", head, lines[0])
			lines = lines[1:]
		} else {
			fmt.Fprintf(w, "  %s\n", head)
		}
		for _, l := range lines {
			fmt.Fprintf(w, "  %-22s %s\n", "", l)
		}
	}
}

// hideFlags keeps flags working but out of the help, for detail most
// callers never need (wp done's shot tuning: wp shot -h lists it).
func hideFlags(fs *flag.FlagSet, names ...string) {
	for _, n := range names {
		if f := fs.Lookup(n); f != nil && !strings.HasPrefix(f.Usage, "(hidden)") {
			f.Usage = "(hidden) " + f.Usage
		}
	}
}

// wrap breaks text into lines of at most width runes, at spaces.
func wrap(text string, width int) []string {
	var lines []string
	line := ""
	for _, w := range strings.Fields(text) {
		if line != "" && len([]rune(line))+1+len([]rune(w)) > width {
			lines = append(lines, line)
			line = w
			continue
		}
		if line != "" {
			line += " "
		}
		line += w
	}
	return append(lines, line)
}

var versionCmd = &command{
	name: "version", group: "Setup", summary: "print the version",
	examples: []string{"wp version"},
	run: func(c *cli, args []string) error {
		if _, err := c.parse(c.flags(), args); err != nil {
			return err
		}
		c.println("wp", version)
		return nil
	},
}

// usageError is a mistake in how a command was called.
type usageError struct {
	msg string
	cmd *command
	fs  *flag.FlagSet
}

func (e usageError) Error() string { return e.msg }

// usage makes a usageError for the command being run.
func (c *cli) usage(format string, a ...any) error {
	cmd := c.cur
	if cmd == nil {
		cmd = &command{name: "help"}
	}
	return usageError{msg: fmt.Sprintf(format, a...), cmd: cmd, fs: c.fs}
}

var errHelpShown = errors.New("help shown")

// exitCode ends wp with a code of the command's own after it printed why
// (wp pr wait's verdicts).
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func joinArgs(a []string) string { return strings.Join(a, " ") }
