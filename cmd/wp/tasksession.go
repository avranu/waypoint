package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Task sessions (WP-34): a session Waypoint starts for a task is bound to it
// from birth by WP_TASK=KEY[,KEY…] in its environment, which its hooks and
// every wp it runs see. wp start launches one here; the runner's
// start_session job (and the queue, later) use the same launcher.

var startCmd = &command{
	name: "start", group: "Tasks", args: "KEY [--bg] [FLAGS]",
	summary: "start a fresh Claude Code session bound to task KEY",
	about: `Starts a session in this directory with WP_TASK=KEY; its first prompt
takes the task and does it. Without --bg it opens in this terminal; with
--bg it prints an id for claude attach (claude attach KEY works too).
Inside a Claude Code session only --bg is allowed. Your Claude Code
settings (Settings → Claude Code: Remote Control, Code Mode, model…) fill
in what the flags leave out.`,
	examples: []string{
		"wp start WP-12 --bg",
		"wp start WP-12 --model opus --permission-mode auto",
	},
	run: cmdStart,
}

// taskLaunch is how a task session (or a follow-up's fresh session) starts.
type taskLaunch struct {
	tasks          []string // WP_TASK; none: not a task session
	prompt         string
	name           string
	model          string
	permissionMode string
	effort         string
	bg             bool
	fanout         int64   // WP_FANOUT: the fan-out this package's session belongs to (WP-148)
	budget         float64 // WP_BUDGET_USD: its budget
	// From the person's session settings (WP-1265): --rc, --chrome or
	// --no-chrome, and variables for its environment (Code Mode).
	remoteControl bool
	chrome        *bool
	env           []string
}

// unattendedSettings is the --settings a session nobody is driving starts
// with (WP-977): an autopilot start or restart, a --bg session, a runner's
// claude -p job. Its deny rules take the tools such a session has no use for
// out of the request altogether: nobody answers AskUserQuestion, views an
// Artifact, opts into a Workflow or runs a /loop for ScheduleWakeup. Over
// 1,737 transcripts on devhost (2026-10-13..11-12, X16) Artifact and Workflow
// were never called and the other two only where a person was present, while
// the four are ~18k tokens of a 41k-token first call that every later call
// reads again. --disallowedTools only refuses calls; the schemas stay.
const unattendedSettings = `{"permissions":{"deny":["Artifact","Workflow","AskUserQuestion","ScheduleWakeup"]}}`

// withUnattendedSettings adds unattendedSettings to args unless they already
// name a settings file or JSON: that one is the person's and Claude Code
// takes one --settings.
func withUnattendedSettings(args []string) []string {
	for _, a := range args {
		if a == "--settings" || strings.HasPrefix(a, "--settings=") {
			return args
		}
	}
	return append(args, "--settings", unattendedSettings)
}

// withRemoteControl adds --rc to args unless they already turn Remote Control
// on (WP-1044), so a session autopilot starts or restarts can be followed
// from the person's Claude app. The user setting remoteControlAtStartup does
// the same, but only on a machine where it is set.
func withRemoteControl(args []string) []string {
	for _, a := range args {
		if name, _, _ := strings.Cut(a, "="); name == "--rc" || name == "--remote-control" {
			return args
		}
	}
	return append(args, "--rc")
}

// claudeArgs are the arguments to claude for a launch.
func (l taskLaunch) claudeArgs() []string {
	var args []string
	if l.bg {
		args = append(args, "--bg")
		args = withUnattendedSettings(args)
	}
	if l.name != "" {
		args = append(args, "--name", l.name)
	}
	if l.model != "" {
		args = append(args, "--model", l.model)
	}
	if l.permissionMode != "" {
		args = append(args, "--permission-mode", l.permissionMode)
	}
	if l.effort != "" {
		args = append(args, "--effort", l.effort)
	}
	if l.remoteControl {
		args = append(args, "--rc")
	}
	if l.chrome != nil {
		args = append(args, chromeFlag(*l.chrome))
	}
	return append(args, "--", l.prompt)
}

// sessionEnv is base without what ties a process to the Claude Code session
// (and the Waypoint session) it runs in, plus the launch's WP_TASK: a new
// session is its own, whoever launched it. WAYPOINT_UNATTENDED stays (WP-270):
// a session an unattended one launches is unattended too.
func (l taskLaunch) sessionEnv(base []string) []string {
	// CLAUDE_ENV_FILE and CLAUDE_PROJECT_DIR point at the launching session's own
	// files: the new session's hooks would write into them.
	drop := map[string]bool{"CLAUDE_ENV_FILE": true, "CLAUDE_PROJECT_DIR": true, "WAYPOINT_SESSION": true, "WAYPOINT_FOLLOWUP": true, "WAYPOINT_TRANSCRIPT": true, "WP_TASK": true, "CLAUDECODE": true, "WP_FANOUT": true, "WP_BUDGET_USD": true, "WAYPOINT_PRESTART": true}
	var out []string
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if k = strings.ToUpper(k); drop[k] || strings.HasPrefix(k, "CLAUDE_CODE_") {
			continue
		}
		out = append(out, kv)
	}
	if len(l.tasks) > 0 {
		out = append(out, "WP_TASK="+strings.Join(l.tasks, ","))
	}
	if l.fanout > 0 {
		out = append(out, "WP_FANOUT="+strconv.FormatInt(l.fanout, 10), "WP_BUDGET_USD="+strconv.FormatFloat(l.budget, 'f', 2, 64))
	}
	return append(out, l.env...)
}

// splitKeys reads a WP_TASK binding: issue keys, upper-cased, each once;
// anything else is dropped.
func splitKeys(s string) []string {
	var out []string
	for _, k := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
		if k = strings.ToUpper(k); keyDashRE.MatchString(k) && !slices.Contains(out, k) && len(out) < 20 {
			out = append(out, k)
		}
	}
	return out
}

// transcriptTails are how much of a transcript's end is read for its
// context: a little first, more when tool output buried the last call.
var transcriptTails = []int64{4 << 20, 32 << 20}

// currentContext is the context the agent session this runs in holds now:
// what its last model call read, from the end of its transcript (the
// SessionStart hook exported its path). 0 outside a session, or unknown.
func currentContext() int64 {
	path := os.Getenv("WAYPOINT_TRANSCRIPT")
	if path == "" || !filepath.IsAbs(path) {
		return 0
	}
	f, err := os.Open(path) // #nosec G304 G703 -- the transcript path the hook exported for this session
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return 0
	}
	for _, tail := range transcriptTails {
		u, err := readTranscriptUsage(f, max(0, fi.Size()-tail), nil, time.Time{})
		if err != nil {
			return 0
		}
		if u.context > 0 || tail >= fi.Size() {
			return u.context
		}
	}
	return 0 // the server uses what the last Stop reported
}

// startKickoff is a task session's first prompt (WP-1037, WP-ADR99), made as
// autopilot makes its kickoff: this launcher takes the task with wp next for
// the session (the same agent identity and config; the server hands the
// claim to the session when it registers), and the prompt is the project's
// kickoff template filled with that answer, which carries the task, how to
// finish it, and how to ask about it or give it back. tmpl empty: the
// default ({next}). wp next gets autopilot's 2 minutes: a hung server
// leaves the prompt without the answer rather than holding up the launch.
func startKickoff(ctx context.Context, dir string, env []string, key, title, tmpl string) string {
	env = append(slices.Clip(env), "CLAUDECODE=1", "WAYPOINT_PRESTART=1")
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	answer, err := runWpNext(ctx, dir, env, key, "--started")
	return kickoffPrompt(kickoff{key: key, title: title, template: tmpl,
		opener: "Waypoint started this session for " + key}, answer, err)
}

// kickoffTemplate is the project's kickoff template for key, as autopilot
// uses it: empty (the default) when the server is older or can't say.
func (c *cli) kickoffTemplate(key string) string {
	var out struct {
		Prompt string `json:"prompt"`
	}
	if err := c.do("GET", "/api/v1/agents/kickoff/"+esc(key), nil, &out); err != nil {
		return ""
	}
	return out.Prompt
}

func cmdStart(c *cli, args []string) error {
	fs := c.flags()
	bg := fs.Bool("bg", false, "start it in the background (claude attach opens it)")
	model := fs.String("model", "", "the `MODEL` (claude --model)")
	perm := fs.String("permission-mode", "", "claude's permission `MODE`")
	effort := fs.String("effort", "", "reasoning `EFFORT`")
	prompt := fs.String("prompt", "", "first `PROMPT` (default: KEY, taken for the session, with what wp next says about it)")
	dir := fs.String("dir", "", "start it in `DIR` instead of here")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("which task? Give one KEY, like wp start APP-12")
	}
	key := strings.ToUpper(pos[0])
	if !keyRE.MatchString(key) {
		return c.usage("%q isn't an issue key like APP-12", pos[0])
	}
	if !*bg && os.Getenv("CLAUDECODE") == "1" {
		// wp start used to mean wp next: an agent that still reaches for it
		// mustn't open a nested claude in its Bash tool.
		return errors.New("inside a Claude Code session: `wp next KEY` takes it here; `wp start KEY --bg` runs it in a fresh session")
	}
	// The server's spelling (APP-12), and a task that exists and that you can see.
	var issue struct {
		Key   string `json:"key"`
		Title string `json:"title"`
	}
	if err := c.do("GET", "/api/v1/issues/"+esc(key), nil, &issue); err != nil {
		return err
	}
	if issue.Key != "" {
		key = issue.Key
	}
	l := taskLaunch{tasks: []string{key}, prompt: *prompt, model: *model, permissionMode: *perm, effort: *effort, bg: *bg,
		name: strings.TrimSpace(key + " " + truncRunes(issue.Title, 60))}
	where := *dir
	if where == "" {
		if where, err = os.Getwd(); err != nil {
			return err
		}
	}
	if where, err = filepath.Abs(where); err != nil {
		return err
	}
	if fi, err := os.Stat(where); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s isn't a directory", where)
	}
	bin, err := exec.LookPath(cliBinaries["claude"])
	if err != nil {
		return errors.New("claude (Claude Code) isn't installed here")
	}
	if !l.bg && !stdinIsTerminal() {
		return c.usage("there's no terminal here to open the session in: add --bg to start it in the background")
	}
	c.mySessionPrefs().forDir(where).fill(&l)
	if strings.TrimSpace(l.prompt) == "" {
		l.prompt = startKickoff(context.Background(), where, l.sessionEnv(os.Environ()), key, issue.Title, c.kickoffTemplate(key))
	}
	if l.bg {
		return c.startBackground(bin, where, l, key)
	}
	cmd := exec.CommandContext(context.Background(), bin, l.claudeArgs()...) // #nosec G204 -- claude, with a validated key and flags after which -- ends the options
	cmd.Dir, cmd.Env = where, l.sessionEnv(os.Environ())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, c.out, c.err
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claude: %w", err)
	}
	return nil
}

// launchBackground runs claude --bg and returns the id claude attach takes
// (empty if claude printed none) and what claude said.
func launchBackground(bin, dir string, l taskLaunch) (id, text string, err error) {
	cmd := exec.CommandContext(context.Background(), bin, l.claudeArgs()...) // #nosec G204 -- claude, with a validated key and flags after which -- ends the options
	cmd.Dir, cmd.Env = dir, l.sessionEnv(os.Environ())
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	text = out.String()
	if err != nil {
		if strings.Contains(text, "not trusted") {
			return "", text, fmt.Errorf("claude doesn't trust %s yet: run claude there once and accept the prompt, then try again", dir)
		}
		return "", text, fmt.Errorf("claude --bg failed: %v\n%s", err, strings.TrimSpace(text))
	}
	if m := bgIDRE.FindStringSubmatch(text); m != nil {
		id = m[1]
	}
	return id, text, nil
}

// startBackground runs claude --bg and says how to open the session.
func (c *cli) startBackground(bin, dir string, l taskLaunch, key string) error {
	id, text, err := launchBackground(bin, dir, l)
	if err != nil {
		return err
	}
	if c.asJSON {
		res := map[string]any{"task": key, "attach_id": id, "dir": dir}
		if id == "" {
			res["output"] = text // no id found: what claude said instead
		}
		b, _ := json.Marshal(res)
		return c.emit(b, nil)
	}
	if id == "" {
		_, _ = io.WriteString(c.out, text)
		return nil
	}
	fmt.Fprintf(c.out, "Started a session for %s in the background (in %s).\nOpen it: claude attach %s\n", key, dir, id)
	return nil
}

// taskBinding is this process's WP_TASK, normalized (at most 20 keys), as the
// hooks and wp next send it: the same value in an event and a header.
func taskBinding() string {
	return strings.Join(splitKeys(os.Getenv("WP_TASK")), ",")
}
