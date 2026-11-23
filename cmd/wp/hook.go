package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Claude Code integration: hooks report each session's token usage to the
// task taken in the session's directory, so the agent spends nothing on
// it, and stop the task's clock while the session is idle (WP-D11).

const (
	hookCommand = "hook claude-report" // Stop, SessionEnd: report usage, pause the clock
	hookResume  = "hook claude-resume" // UserPromptSubmit: restart the clock
	hookEvent   = "hook claude-event"  // everything else: sessions and requests (WP-D16)
	hookNudge   = "hook claude-nudge"  // PreToolUse(Bash|Read): the better tool, in one line (WP-47); notes a poll of a running background task (WP-191); lets a Read of a project, task or scratchpad picture through unasked (WP-1137); never refuses (WP-1411)
	hookGate    = "hook claude-gate"   // PreToolUse(Agent): forks, depth, width, model (WP-143); holds a start while Claude Code's agent limit is reached (WP-1443)
	hookBurn    = "hook claude-burn"   // PostToolUse(all): silent unless the tree burns above $100/h (WP-144)
	hookGuard   = "hook claude-guard"  // PreToolUse(Bash|Write|Edit|web): refuses risky calls in unattended sessions (WP-270), and the nudge refusals in every session (WP-1411)
	hookPause   = "hook claude-pause"  // PostToolUse(all), async: pulls the person's session pause to a local file (WP-146)
	hookSlash   = "hook claude-slash"  // UserPromptSubmit: /wp help, btw and exe, and a waiting /wp btw note (WP-562)
)

// hookSlashTimeout is the slash hook's timeout: /wp exe waits up to exeWait
// for its command.
const hookSlashTimeout = 300

// The PreToolUse hooks' timeouts in seconds. Claude Code fails open when one
// is cancelled: the tool runs, no guard check, no note. The nudge is advisory
// so it stays short; the guard and the gate are refusals, so they wait longer
// rather than leave a gap (WP-1398). The nudge's own refusals (grok-cut-short,
// pgrep-self, worktree-place, task-wait) are emitted by the guard for that
// reason (WP-1411). hookLate measures a run against these. The gate's is long
// because it holds an Agent call while Claude Code's agent limit is reached:
// the longest wait (gateQueueBound) plus a minute (WP-1443).
const (
	hookNudgeTimeout = 2
	hookGateTimeout  = 300
	hookGuardTimeout = 10
)

// hookSessionStartTimeout is the SessionStart hook's timeout in seconds.
const hookSessionStartTimeout = 10

// hookSubagentStartTimeout is how long a sub-agent's start waits for its
// instructions (ADR146) before it starts without them.
const hookSubagentStartTimeout = 5

// hookOpts are the settings of a hook entry beyond its command.
type hookOpts struct {
	matcher string // which tools or notifications (empty: all)
	async   bool   // runs in the background, so the agent never waits on it
	timeout int    // seconds; 0: wp's default (30)
}

// claudeHook is a hook wp installs.
type claudeHook struct {
	event, command string
	opts           hookOpts
	prompts        bool // passes --no-prompts when asked
}

// claudeHooks are the hooks wp installs. The first three report usage to the
// task taken and pause its clock; the rest tell Waypoint about the session
// (WP-D16): async wherever the event allows, so the agent never waits on
// Waypoint. SessionStart and Stop answer the agent, so they run in the
// foreground with a short timeout and fail open; SessionEnd has Claude
// Code's 1.5 s to finish.
var claudeHooks = []claudeHook{
	{event: "Stop", command: hookCommand},
	{event: "SessionEnd", command: hookCommand},
	{event: "UserPromptSubmit", command: hookResume},
	{event: "SessionStart", command: hookEvent, opts: hookOpts{timeout: hookSessionStartTimeout}},
	// + the news that waits for a prompt (WP-176); + the stop a background wp ship sends when it reports (WP-1093)
	{event: "UserPromptSubmit", command: hookEvent, opts: hookOpts{timeout: 20}, prompts: true},
	// Local only, silent for any other prompt; /wp exe may wait minutes (WP-562).
	{event: "UserPromptSubmit", command: hookSlash, opts: hookOpts{timeout: hookSlashTimeout}},
	{event: "Notification", command: hookEvent, opts: hookOpts{async: true}},
	{event: "PermissionDenied", command: hookEvent, opts: hookOpts{async: true}},
	{event: "PreToolUse", command: hookEvent, opts: hookOpts{matcher: "AskUserQuestion", async: true}},
	// + a Bash call that satisfies a request (WP-126), silent unless the session has one; + a screenshot the agent read, to its task (WP-263); + who started a background agent (WP-1443)
	{event: "PostToolUse", command: hookEvent, opts: hookOpts{matcher: "AskUserQuestion|Bash|Read|Agent|Task", async: true}},
	{event: "Stop", command: hookEvent, opts: hookOpts{timeout: 20}}, // + a follow-up's message (WP-D22)
	// Answered with the sub-agent's instructions (ADR146), so it waits; fails open.
	{event: "SubagentStart", command: hookEvent, opts: hookOpts{timeout: hookSubagentStartTimeout}},
	{event: "SubagentStop", command: hookEvent, opts: hookOpts{async: true}, prompts: true}, // + an adversarial review's report and prompt (WP-1089)
	{event: "SessionEnd", command: hookEvent, opts: hookOpts{timeout: 2}},
	// What the session runs with (WP-1468): the snapshot after a compaction and after a settings change, each instruction file as it loads, a model switch.
	{event: "PostCompact", command: hookEvent, opts: hookOpts{async: true}},
	{event: "ConfigChange", command: hookEvent, opts: hookOpts{async: true}},
	{event: "InstructionsLoaded", command: hookEvent, opts: hookOpts{async: true}},
	{event: "PostModelSwitch", command: hookEvent, opts: hookOpts{async: true}},
	{event: "PreToolUse", command: hookNudge, opts: hookOpts{matcher: "Bash|Read|Monitor", timeout: hookNudgeTimeout}}, // no network; Read for big whole-file reads (WP-145), task output (WP-191) and the pictures it lets through unasked (WP-1137); Monitor by name, or a Bash matcher gets it as a Bash call (WP-1443)
	{event: "PreToolUse", command: hookGate, opts: hookOpts{matcher: "Agent|Task", timeout: hookGateTimeout}},          // local only; refuses in every session
	// The guard: local only, no subprocess; refuses risky calls in unattended sessions (WP-270, WP-ADR10) and the nudge refusals in every session (WP-1411).
	{event: "PreToolUse", command: hookGuard, opts: hookOpts{matcher: "Bash|Write|Edit|MultiEdit|NotebookEdit", timeout: hookGuardTimeout}},
	// Local, incremental, under 50 ms; + the stop after a wp ship with --summary, which waits on the server like the Stop hook (WP-1093).
	{event: "PostToolUse", command: hookBurn, opts: hookOpts{timeout: 20}},
	// The pause copy: async, asks the server at most every 30 s (WP-146).
	{event: "PostToolUse", command: hookPause, opts: hookOpts{async: true}},
}

// hookMarker finds our entries when removing them, whichever of the hooks.
const hookMarker = "hook claude-"

var hookCmd = &command{
	name: "hook", group: "Hooks", args: "claude [--uninstall]",
	summary:  "install the Claude Code hooks",
	examples: []string{"wp hook claude", "wp hook claude --uninstall"},
}

func init() {
	hookCmd.subs = []*command{
		{name: "claude", args: "[--uninstall] [--no-prompts]", summary: "install Claude Code hooks for sessions, requests and usage",
			about: `Adds hooks to ~/.claude/settings.json (a backup is kept) and upgrades an
older install; wp setup does this too. The hooks report each session to
Waypoint (start, prompts, notifications, final message, end), report token
usage to the task taken with wp next and pause its clock while the agent
waits, turn refused tool calls and questions into requests, and add short
guidance: better-tool hints, limits on Agent calls, and refusals of risky
calls in unattended sessions. They never fail a session, and transcripts are read
only for token counts, message times and model names, and for the report of an
adversarial-review subagent, kept in its task's work log (WP-1089).

--no-prompts leaves prompt text out of the reports, a review's prompt and description included. --uninstall removes
every Waypoint hook.`,
			examples: []string{"wp hook claude", "wp hook claude --uninstall"}, run: hookClaude},
		{name: "claude-event", hidden: true, summary: "the session hooks (run by Claude Code)",
			run: func(c *cli, args []string) error {
				fs := c.flags()
				noPrompts := fs.Bool("no-prompts", false, "")
				if _, err := c.parse(fs, args); err != nil {
					return nil
				}
				hookClaudeEvent(c, os.Stdin, c.out, *noPrompts)
				c.refreshHooks()
				return nil // a hook must never fail the session
			}},
		{name: "claude-push", hidden: true, summary: "pushes an ended session's commits (its SessionEnd hook starts it, WP-843)",
			run: hookClaudePush},
		{name: "claude-nudge", hidden: true, summary: "the Bash and Read nudges (run by Claude Code): names the better tool in one line; lets a Read of the project's pictures through unasked",
			run: func(c *cli, args []string) error {
				hookClaudeNudge(c, os.Stdin, c.out)
				return nil // never blocks, never fails
			}},
		{name: "claude-burn", hidden: true, summary: "the burn meter (run by Claude Code): silent unless the session tree burns irregularly fast (over $100/h)",
			run: func(c *cli, args []string) error {
				hookClaudeBurn(c, os.Stdin, c.out)
				return nil // never blocks, never fails
			}},
		{name: "claude-gate", hidden: true, summary: "the Agent-call gate (run by Claude Code): forks, depth, width and model",
			run: func(c *cli, args []string) error {
				hookClaudeGate(c, os.Stdin, c.out)
				return nil // fails open
			}},
		{name: "claude-guard", hidden: true, summary: "the unattended-session guard (run by Claude Code): refuses installs, other machines, host changes and writes to other repos",
			run: func(c *cli, args []string) error {
				hookClaudeGuard(c, os.Stdin, c.out)
				return nil // fails open
			}},
		{name: "claude-slash", hidden: true, summary: "the session commands (run by Claude Code): /wp help, btw and exe",
			run: func(c *cli, args []string) error {
				hookClaudeSlash(c, os.Stdin, c.out)
				return nil // never fails
			}},
		{name: "claude-pause", hidden: true, summary: "the pause pull (run by Claude Code): keeps a local copy of the person's session pause",
			run: func(c *cli, args []string) error {
				hookClaudePause(c, os.Stdin, slices.Contains(args, "--now"))
				return nil // async; never fails
			}},
		{name: "claude-report", hidden: true, summary: "the hook itself (run by Claude Code)",
			run: func(c *cli, args []string) error {
				hookClaudeReport(c, os.Stdin)
				c.refreshHooks()
				return nil // a hook must never fail the session
			}},
		{name: "claude-resume", hidden: true, summary: "the prompt hook (run by Claude Code): restarts the task's clock",
			run: func(c *cli, args []string) error {
				hookClaudeResume(c, os.Stdin)
				c.refreshHooks()
				return nil
			}},
	}
}

func claudeSettingsPath() (string, error) {
	if p := os.Getenv("WP_CLAUDE_SETTINGS"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

func hookClaude(c *cli, args []string) error {
	fs := c.flags()
	uninstall := fs.Bool("uninstall", false, "remove the hooks")
	noPrompts := fs.Bool("no-prompts", false, "leave prompt text out of the reports")
	if _, err := c.parse(fs, args); err != nil {
		return err
	}
	path, err := claudeSettingsPath()
	if err != nil {
		return err
	}
	exe := hookExe()
	res, err := installClaudeHooks(path, hookInstallOpts{exe: exe, noPrompts: *noPrompts, uninstall: *uninstall})
	if err != nil {
		return err
	}
	c.recordHooks(path, res, exe, *uninstall)
	if !res.changed {
		if *uninstall {
			c.println("The hooks weren't installed.")
		} else {
			c.println("The hooks are already installed.")
		}
		return nil
	}
	if res.backup != "" {
		c.println("Backed up", res.path, "to", res.backup)
	}
	if *uninstall {
		c.println("Removed the Waypoint hooks from", res.path)
	} else {
		c.println("Installed hooks in", res.path, "that report sessions and requests to Waypoint, and token usage to\nthe task you took with wp next (pausing its clock while the session is idle).")
		if res.noPrompts {
			c.println("Prompts are not sent.")
		}
		for _, n := range res.notes {
			c.println("Also: " + n + ".")
		}
	}
	return nil
}

// hookSchema is the version of the hook set wp installs: claudeHooks, the
// usage hooks and the status line. Bump it whenever they change (the test
// TestHookSchemaFollowsTheHooks fails until it is): hooks that an older wp
// installed then bring themselves up to date the next time one runs.
const hookSchema = 19

// hookRefreshEvery is how often a hook may try to bring the hooks up to date.
const hookRefreshEvery = 24 * time.Hour

// hooksInstalled is the hook set installed on this machine, in the state file.
type hooksInstalled struct {
	Schema    int       `json:"schema"`
	Exe       string    `json:"exe,omitempty"`
	Settings  string    `json:"settings,omitempty"`
	NoPrompts bool      `json:"no_prompts,omitempty"`
	Tried     time.Time `json:"tried,omitzero"` // the last automatic refresh
}

// hookInstallOpts says how to install the hooks.
type hookInstallOpts struct {
	exe       string
	noPrompts bool
	uninstall bool
	// keepNoPrompts keeps --no-prompts where the installed hooks have it;
	// onlyIfInstalled changes nothing unless some of our hooks are there.
	keepNoPrompts, onlyIfInstalled bool
	dryRun                         bool // only say what would change
}

// hookInstallResult says what installClaudeHooks did.
type hookInstallResult struct {
	path      string // the file written (a symlink's target)
	changed   bool
	present   bool // some of our hooks were there before
	noPrompts bool // what the hooks now do
	backup    string
	notes     []string
}

// hookExe is the wp the hooks run: this one (tests swap it).
var hookExe = func() string {
	exe, err := os.Executable()
	if err != nil {
		return "wp"
	}
	return exe
}

// throwawayBuild is a binary that won't be there tomorrow: go run, go test.
func throwawayBuild(exe string) bool {
	return strings.Contains(exe, "go-build") || strings.HasSuffix(strings.TrimSuffix(exe, ".exe"), ".test")
}

// installClaudeHooks adds (or removes) the Claude Code hooks and the usage
// integration in the settings file at path, keeping a backup of what was
// there. Installing again brings an older install up to date.
func installClaudeHooks(path string, o hookInstallOpts) (hookInstallResult, error) {
	res := hookInstallResult{path: path, noPrompts: o.noPrompts}
	commandsDir := filepath.Join(filepath.Dir(path), "commands")
	if target, err := filepath.EvalSymlinks(path); err == nil {
		res.path = target // a dotfile manager's link: edit (and back up) what it points to
	}
	path = res.path
	settings := map[string]any{}
	raw, err := os.ReadFile(path) // #nosec G304 -- the user's Claude Code settings
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return res, err
	default:
		if err := json.Unmarshal(raw, &settings); err != nil {
			return res, fmt.Errorf("%s isn't valid JSON, so it was left alone: %w", path, err)
		}
	}
	res.present = hasOurHooks(settings)
	if o.onlyIfInstalled && !res.present {
		return res, nil
	}
	if o.keepNoPrompts && !o.uninstall && hooksSkipPrompts(settings) {
		res.noPrompts = true
	}
	for _, h := range claudeHooks {
		var ch bool
		if o.uninstall {
			ch = removeHook(settings, h.event, hookMarker)
		} else {
			cmd := quoteArg(o.exe) + " " + h.command
			if h.prompts && res.noPrompts {
				cmd += " --no-prompts"
			}
			if ch, err = addHookOpts(settings, h.event, cmd, h.command, h.opts); err != nil {
				return res, fmt.Errorf("%s: %w, so it was left alone", path, err)
			}
		}
		res.changed = res.changed || ch
	}
	// usage analytics (WP-D20): background upload hooks and the status line
	ch, notes, err := installUsageIntegration(settings, o.exe, o.uninstall)
	if err != nil {
		return res, fmt.Errorf("%s: %w, so it was left alone", path, err)
	}
	res.changed, res.notes = res.changed || ch, notes
	var keep bool
	for name, body := range slashCommands {
		ch, err := syncSlashCommand(filepath.Join(commandsDir, name), body, o.uninstall, o.dryRun)
		if err != nil {
			return res, err
		}
		keep = keep || ch
	}
	if !res.changed || o.dryRun {
		res.changed = res.changed || keep
		return res, nil
	}
	if raw != nil {
		res.backup = path + ".bak-" + time.Now().Format("20060102-150405")
		if err := os.WriteFile(res.backup, raw, 0o600); err != nil { // #nosec G703 -- next to the user's own settings file
			return res, err
		}
	}
	return res, writeJSON(path, settings)
}

// keepCommand is /keep (WP-ADR9). Claude Code answers a slash command it
// doesn't know with "Unknown command" and never shows it to a hook; with
// this file it reaches UserPromptSubmit, where Waypoint stops it before any
// model call. The body only runs when the hooks didn't answer.
const keepCommand = `---
description: Keep this session from Waypoint autopilot (installed by wp hook claude)
disable-model-invocation: true
---
Waypoint's hooks didn't answer /keep, so autopilot wasn't told. Reply with one line, "Nothing kept: Waypoint's hooks didn't answer.", and do nothing else.
`

// wpCommand is /wp (WP-562), the same way: its hooks answer every /wp
// command, the server's and the machine's.
const wpCommand = `---
description: Waypoint session commands, answered by its hooks (installed by wp hook claude)
argument-hint: next | stop | continue | status | review [KEY] | keep | btw MESSAGE | exe "COMMAND" | help
disable-model-invocation: true
---
Waypoint's hooks didn't answer this /wp command, so nothing was done. Reply with one line, "Nothing done: Waypoint's hooks didn't answer /wp.", and do nothing else.
`

// slashCommands are the command files wp installs beside the settings
// file, by name.
var slashCommands = map[string]string{"keep.md": keepCommand, "wp.md": wpCommand}

// syncSlashCommand writes (or removes) the command body at path, leaving
// alone a file of that name that isn't ours.
func syncSlashCommand(path, body string, uninstall, dryRun bool) (bool, error) {
	old, err := os.ReadFile(path) // #nosec G304 -- beside the user's own Claude Code settings
	ours := err == nil && strings.Contains(string(old), "installed by wp hook claude")
	switch {
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return false, err
	case uninstall:
		if !ours {
			return false, nil
		}
		if dryRun {
			return true, nil
		}
		return true, os.Remove(path)
	case err == nil && (!ours || string(old) == body):
		return false, nil
	case dryRun:
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, []byte(body), 0o600) // #nosec G703 -- beside the user's own settings file
}

// ourHookCommands calls f with the command of each hook of ours in settings.
func ourHookCommands(settings map[string]any, f func(cmd string)) {
	hooks, _ := settings["hooks"].(map[string]any)
	for _, groups := range hooks {
		list, _ := groups.([]any)
		for _, g := range list {
			gm, _ := g.(map[string]any)
			entries, _ := gm["hooks"].([]any)
			for _, h := range entries {
				hm, _ := h.(map[string]any)
				if cmd, ok := hm["command"].(string); ok && strings.Contains(cmd, hookMarker) {
					f(cmd)
				}
			}
		}
	}
}

func hasOurHooks(settings map[string]any) bool {
	found := false
	ourHookCommands(settings, func(string) { found = true })
	return found
}

// hooksSkipPrompts: the installed hooks were installed with --no-prompts.
func hooksSkipPrompts(settings map[string]any) bool {
	skip := false
	ourHookCommands(settings, func(cmd string) {
		if strings.Contains(cmd, hookEvent) && strings.Contains(cmd, "--no-prompts") {
			skip = true
		}
	})
	return skip
}

// recordHooks remembers which hook set is installed, and where.
func (c *cli) recordHooks(settingsPath string, res hookInstallResult, exe string, uninstall bool) {
	_ = c.updateState(func(s *State) {
		if uninstall {
			s.Hooks = nil
			return
		}
		s.Hooks = &hooksInstalled{Schema: hookSchema, Exe: exe, Settings: settingsPath, NoPrompts: res.noPrompts}
	})
}

// refreshHooks brings hooks that an older wp installed (or that run a wp
// at another path) up to date, from inside a hook: quietly, at most once a
// day, keeping --no-prompts, and only in a settings file that has our
// hooks. It never fails, and does nothing for a temporary build.
func (c *cli) refreshHooks() {
	defer func() { _ = recover() }() // a hook must never fail the session
	exe := hookExe()
	if exe == "wp" || throwawayBuild(exe) || os.Getenv("WAYPOINT_NO_HOOK_REFRESH") != "" {
		return
	}
	due := func(h *hooksInstalled) bool {
		stale := h == nil || h.Schema < hookSchema || h.Schema == hookSchema && h.Exe != exe
		return stale && (h == nil || time.Since(h.Tried) >= hookRefreshEvery)
	}
	if !due(c.state.Hooks) {
		return
	}
	var path string
	claimed := false
	// Only one of the hooks that start together does it.
	_ = c.updateState(func(s *State) {
		if !due(s.Hooks) {
			return
		}
		if s.Hooks == nil {
			s.Hooks = &hooksInstalled{}
		}
		s.Hooks.Tried = time.Now()
		path, claimed = s.Hooks.Settings, true
	})
	if !claimed {
		return
	}
	if path == "" {
		var err error
		if path, err = claudeSettingsPath(); err != nil {
			return
		}
	}
	res, err := installClaudeHooks(path, hookInstallOpts{exe: exe, keepNoPrompts: true, onlyIfInstalled: true})
	if err != nil || !res.present {
		return
	}
	c.recordHooks(path, res, exe, false)
}

// quoteArg quotes a path for the shell that runs hook commands, so nothing
// in it ($, backticks, quotes) is ever expanded.
func quoteArg(s string) string {
	plain := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+:@%,=", r)
	}
	safe := s != "" && strings.IndexFunc(s, func(r rune) bool { return !plain(r) }) < 0
	if safe {
		return s
	}
	if runtime.GOOS == "windows" {
		// Windows paths can't contain double quotes; forward slashes keep a
		// POSIX shell (Git Bash) from reading backslashes as escapes.
		return `"` + strings.ReplaceAll(s, `\`, "/") + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// addHook adds command under hooks.<event> unless one containing marker
// is there already; an entry of ours pointing at another binary (wp moved)
// is updated.
func addHook(settings map[string]any, event, command, marker string) (bool, error) {
	return addHookOpts(settings, event, command, marker, hookOpts{})
}

// addHookOpts is addHook with settings: an entry of ours that is there
// already is brought up to date (command, matcher, async, timeout).
func addHookOpts(settings map[string]any, event, command, marker string, o hookOpts) (bool, error) {
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok && settings["hooks"] != nil {
		return false, errors.New(`"hooks" isn't an object`)
	}
	if hooks == nil {
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}
	groups, ok := hooks[event].([]any)
	if !ok && hooks[event] != nil {
		return false, fmt.Errorf(`"hooks.%s" isn't a list`, event)
	}
	found, changed := false, false
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		list, _ := gm["hooks"].([]any)
		for _, h := range list {
			if hm, ok := h.(map[string]any); ok && strings.Contains(fmt.Sprint(hm["command"]), marker) {
				found = true
				if hm["command"] != command {
					hm["command"], changed = command, true
				}
				if o.async != (hm["async"] == true) {
					if o.async {
						hm["async"] = true
					} else {
						delete(hm, "async")
					}
					changed = true
				}
				// An old install of ours has the default timeout; newer entries say theirs.
				if o.timeout != 0 && fmt.Sprint(hm["timeout"]) != fmt.Sprint(o.timeout) {
					hm["timeout"], changed = o.timeout, true
				}
				if o.matcher != "" && gm["matcher"] != o.matcher {
					gm["matcher"], changed = o.matcher, true
				}
			}
		}
	}
	if found {
		return changed, nil
	}
	timeout := 30
	if o.timeout != 0 {
		timeout = o.timeout
	}
	entry := map[string]any{"type": "command", "command": command, "timeout": timeout}
	if o.async {
		entry["async"] = true
	}
	group := map[string]any{"hooks": []any{entry}}
	if o.matcher != "" {
		group["matcher"] = o.matcher
	}
	hooks[event] = append(groups, group)
	return true, nil
}

// removeHook drops our command from hooks.<event> (and groups it leaves
// empty). Anything it doesn't recognise is kept as it is.
func removeHook(settings map[string]any, event, marker string) bool {
	hooks, _ := settings["hooks"].(map[string]any)
	groups, _ := hooks[event].([]any)
	if groups == nil {
		return false
	}
	changed := false
	var keep []any
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		list, ok := gm["hooks"].([]any)
		if !ok {
			keep = append(keep, g)
			continue
		}
		var rest []any
		for _, h := range list {
			if hm, ok := h.(map[string]any); ok && strings.Contains(fmt.Sprint(hm["command"]), marker) {
				changed = true
				continue
			}
			rest = append(rest, h)
		}
		if len(rest) > 0 {
			gm["hooks"] = rest
			keep = append(keep, gm)
		}
	}
	if !changed {
		return false
	}
	if len(keep) == 0 {
		delete(hooks, event)
	} else {
		hooks[event] = keep
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	}
	return true
}

// transcriptUsage is what's new in a transcript since the last report.
type transcriptUsage struct {
	input, output int64
	model         string
	offset        int64
	seen          []string
	// context: what the last main-conversation call read (input, cache
	// reads and writes), whenever it was written; 0 if no call was read.
	context int64
	// ttl: the cache tier ("1h", "5m") the last main-conversation call that
	// wrote the cache asked for; "" if none did (WP-192).
	ttl string
}

// readTranscriptUsage sums assistant usage from offset onwards, counting
// each message id once (a message is written as several lines) and only
// messages written at or after since (the claim), so work done before the
// task was taken isn't charged to it. Input is fresh plus cache-write
// tokens: cache reads are re-reads of the same context on every turn and
// would swamp the real cost.
func readTranscriptUsage(r io.ReadSeeker, offset int64, seen []string, since time.Time) (transcriptUsage, error) {
	u := transcriptUsage{offset: offset}
	known := map[string]bool{}
	for _, id := range seen {
		known[id] = true
	}
	if _, err := r.Seek(offset, io.SeekStart); err != nil {
		return u, err
	}
	type usage struct {
		Input         int64 `json:"input_tokens"`
		Output        int64 `json:"output_tokens"`
		CacheCreation int64 `json:"cache_creation_input_tokens"`
		CacheRead     int64 `json:"cache_read_input_tokens"`
		Tiers         *struct {
			Hour    int64 `json:"ephemeral_1h_input_tokens"`
			Minutes int64 `json:"ephemeral_5m_input_tokens"`
		} `json:"cache_creation"`
	}
	latest := map[string]usage{} // per message: the last (most complete) usage line
	var order []string
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			u.offset += int64(len(line))
			var entry struct {
				Type      string    `json:"type"`
				Timestamp time.Time `json:"timestamp"`
				Sidechain bool      `json:"isSidechain"`
				Message   struct {
					ID    string `json:"id"`
					Model string `json:"model"`
					Usage *usage `json:"usage"`
				} `json:"message"`
			}
			ok := json.Unmarshal(line, &entry) == nil && entry.Type == "assistant" && entry.Message.Usage != nil && entry.Message.ID != ""
			if ok && !entry.Sidechain {
				us := entry.Message.Usage
				if n := us.Input + us.CacheCreation + us.CacheRead; n > 0 {
					u.context = n
				}
				if t := us.Tiers; t != nil && t.Hour > 0 {
					u.ttl = "1h"
				} else if t != nil && t.Minutes > 0 {
					u.ttl = "5m"
				}
			}
			if ok && !entry.Timestamp.Before(since) {
				id := entry.Message.ID
				if !known[id] {
					if _, ok := latest[id]; !ok {
						order = append(order, id)
					}
					latest[id] = *entry.Message.Usage
				}
				if entry.Message.Model != "" && !strings.HasPrefix(entry.Message.Model, "<") {
					u.model = entry.Message.Model
				}
			}
		}
		if err != nil {
			break // EOF; a partial last line is read next time
		}
	}
	for _, id := range order {
		us := latest[id]
		u.input += us.Input + us.CacheCreation
		u.output += us.Output
		seen = append(seen, id)
	}
	if len(seen) > 500 {
		seen = seen[len(seen)-500:]
	}
	u.seen = seen
	return u, nil
}

// hookClaudeReport handles the Stop hook: silent, and never an error. It
// reports only for a session working in a directory (or below one) where
// a task was taken with wp next/batch, so other Claude sessions on the
// machine aren't charged to it.
func hookClaudeReport(c *cli, stdin io.Reader) {
	var in struct {
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
		Cwd            string `json:"cwd"`
	}
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&in); err != nil ||
		in.TranscriptPath == "" || in.SessionID == "" || !filepath.IsAbs(in.Cwd) {
		return
	}
	_, cl := c.state.claimFor(in.Cwd)
	if cl == nil || c.server() == "" || c.token() == "" {
		return
	}
	c.session = in.SessionID // its usage belongs to the session (WP-D16)
	// The session is idle (or gone) once the usage is in: stop the clock.
	defer func() { _, _ = c.clock("pause", cl.Key) }()
	var sess sessionState
	if s := c.state.Sessions[in.SessionID]; s != nil {
		sess = *s
	}
	// A report that may not have landed goes again first, unchanged (same
	// id, so the server counts it once either way).
	if p := sess.Pending; p != nil {
		if !c.sendUsage(p) {
			return // try again next time
		}
		sess = sessionState{Offset: p.Offset, Seen: p.Seen}
		c.saveSession(in.SessionID, &sess)
	}
	f, err := os.Open(in.TranscriptPath) // #nosec G304 -- the path Claude Code gave the hook
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	u, err := readTranscriptUsage(f, sess.Offset, sess.Seen, cl.ClaimedAt)
	if err != nil {
		return
	}
	if u.input == 0 && u.output == 0 {
		c.saveSession(in.SessionID, &sessionState{Offset: u.offset, Seen: u.seen})
		return
	}
	body := map[string]any{"input_tokens": u.input, "output_tokens": u.output}
	agent := cl.Agent
	if c.agent != "" {
		agent = c.agent
	}
	if agent != "" {
		body["agent"] = agent
	}
	if u.model != "" {
		body["model"] = u.model // the model behind the run, whatever the identity says
	}
	id := sha256.Sum256(fmt.Appendf(nil, "%s:%d-%d", in.SessionID, sess.Offset, u.offset))
	body["report_id"] = hex.EncodeToString(id[:16])
	p := &pendingReport{Path: "/api/v1/agent/usage/" + esc(cl.Key), Body: body, Model: u.model, Offset: u.offset, Seen: u.seen}
	// Remembered before sending: if the response is lost or this process is
	// killed mid-request, the next Stop sends this same report again.
	c.saveSession(in.SessionID, &sessionState{Offset: sess.Offset, Seen: sess.Seen, Pending: p})
	if !c.sendUsage(p) {
		return
	}
	c.saveSession(in.SessionID, &sessionState{Offset: u.offset, Seen: u.seen})
}

// sendUsage posts a usage report and says whether the server took it.
func (c *cli) sendUsage(p *pendingReport) bool {
	_, err := c.call("POST", p.Path, p.Body)
	var ae *apiError
	if errors.As(err, &ae) && ae.status == 400 && p.Body["agent"] == nil && p.Model != "" {
		// The token isn't an agent token and nothing is configured: use the
		// model the transcript names.
		p.Body["agent"] = "anthropic/" + p.Model
		_, err = c.call("POST", p.Path, p.Body)
	}
	return err == nil
}

func (c *cli) saveSession(id string, sess *sessionState) {
	_ = c.updateState(func(s *State) {
		if s.Sessions == nil {
			s.Sessions = map[string]*sessionState{}
		}
		if old := s.Sessions[id]; old != nil { // the whole-session totals are kept
			sess.TotOffset, sess.TotSeen, sess.TotIn, sess.TotOut, sess.TotCtx = old.TotOffset, old.TotSeen, old.TotIn, old.TotOut, old.TotCtx
		}
		s.Sessions[id] = sess
		for old := range s.Sessions { // forget old sessions
			if len(s.Sessions) <= 50 {
				break
			}
			if old != id {
				delete(s.Sessions, old)
			}
		}
	})
}

// keepWarmMark starts Waypoint's keep-warm pings (core.KeepWarmMark).
const keepWarmMark = "Waypoint keep-warm"

// harnessPromptMarks start the prompts Claude Code submits when something
// the session started reports back (core.HarnessPromptMarks).
var harnessPromptMarks = []string{"<task-notification>", "Another Claude session sent a message:", `<agent-message from="`}

// harnessPromptMark is the mark the prompt starts with, "" if none.
func harnessPromptMark(prompt string) string {
	p := strings.TrimSpace(prompt)
	for _, mark := range harnessPromptMarks {
		if strings.HasPrefix(p, mark) {
			return mark
		}
	}
	return ""
}

// hookClaudeResume handles UserPromptSubmit: the session in a directory
// where a task was taken is working again, so the task's clock restarts.
// The directory's claim may be one another session there took and left:
// the server resumes it only if this session was working on it (WP-720).
// Silent, and never an error.
func hookClaudeResume(c *cli, stdin io.Reader) {
	var in struct {
		SessionID string `json:"session_id"`
		Cwd       string `json:"cwd"`
		Prompt    string `json:"prompt"`
	}
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&in); err != nil || !filepath.IsAbs(in.Cwd) {
		return
	}
	in.Prompt = core.UnwrapPasted(in.Prompt) // a paste's text (WP-1562)
	if strings.HasPrefix(strings.TrimSpace(in.Prompt), keepWarmMark) || isSessionCommand(in.Prompt) {
		return // a keep-warm ping, or a /wp command, is no work on the task (WP-192, WP-562)
	}
	_, cl := c.state.claimFor(in.Cwd)
	if cl == nil || c.server() == "" || c.token() == "" {
		return
	}
	if in.SessionID == "" {
		return // ownership can't be told without the prompt's session
	}
	c.session = in.SessionID // the prompt's session, not the directory's last one
	_, _ = c.clockWith("resume", cl.Key, map[string]any{"only_own": true})
}
