package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

var kitReportCmd = &command{
	name: "report", args: "[FLAGS]",
	summary: "send this machine's agent files and setup checks to Waypoint",
	about: `Sends ~/.claude (CLAUDE.md, agents, commands, skills, hook scripts,
settings.json with env and secret values masked), MCP server names,
~/.codex/AGENTS.md and wp setup checks to Agents → Setup, visible to you
and admins. wp kit sync and session starts send it too. --check lists
the files and checks that would be sent; with --json it includes the contents.`,
	examples: []string{"wp kit report", "wp kit report --check"},
	run:      kitReport,
}

// machineReportEvery is how often a session's start sends the report.
const machineReportEvery = 6 * time.Hour

// Bounds on what is sent (the server keeps 64 KB a file).
const (
	maxReportFile  = 64 << 10
	maxReportFiles = 300
)

type machineFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type machineCheck struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	State string `json:"state"` // ok, warn, fail
	Note  string `json:"note,omitempty"`
}

type machineReport struct {
	Machine   string         `json:"machine"`
	OS        string         `json:"os"`
	WPVersion string         `json:"wp_version"`
	Files     []machineFile  `json:"files"`
	Checks    []machineCheck `json:"checks"`
}

func kitReport(c *cli, args []string) error {
	fs := c.flags()
	check := fs.Bool("check", false, "print what would be sent; send nothing")
	quiet := fs.Bool("quiet", false, "print only problems")
	if _, err := c.parse(fs, args); err != nil {
		return err
	}
	r := c.machineReport()
	if *check {
		if c.asJSON {
			return c.emitValue(r)
		}
		c.printf("%s (%s, wp %s): %d files\n", r.Machine, r.OS, r.WPVersion, len(r.Files))
		for _, f := range r.Files {
			c.printf("  ~/%s (%d bytes)\n", f.Path, len(f.Content))
		}
		for _, k := range r.Checks {
			c.printf("  %-4s %s%s\n", k.State, k.Label, prefixed(": ", k.Note))
		}
		return nil
	}
	if err := c.sendMachineReport(r); err != nil {
		return err
	}
	if !*quiet {
		c.printf("Reported %s: %d agent files, %d checks. See Agents → Setup.\n", r.Machine, len(r.Files), len(r.Checks))
	}
	return nil
}

func prefixed(p, s string) string {
	if s == "" {
		return ""
	}
	return p + s
}

// sendMachineReport sends a report and notes when.
func (c *cli) sendMachineReport(r machineReport) error {
	if _, err := c.call("POST", "/api/v1/agent-setup/machines", r); err != nil {
		return err
	}
	return c.updateState(func(s *State) { s.MachineReport = time.Now() })
}

// reportMachineQuietly is the report wp kit sync sends: best effort, never
// the sync's failure (wp kit report says what went wrong).
func (c *cli) reportMachineQuietly() {
	_ = c.sendMachineReport(c.machineReport())
}

// maybeReportMachine starts a report in the background when the last one
// is older than machineReportEvery (a session's start).
func (c *cli) maybeReportMachine() {
	if time.Since(c.state.MachineReport) < machineReportEvery {
		return
	}
	exe, err := selfExe()
	if err != nil || throwawayBuild(exe) {
		return
	}
	// Claimed under the state lock before it runs, checked against the state
	// as saved, so of sessions starting together only one sends.
	claimed := false
	err = c.updateState(func(s *State) {
		if time.Since(s.MachineReport) >= machineReportEvery {
			s.MachineReport, claimed = time.Now(), true
		}
	})
	if err != nil || !claimed {
		return
	}
	backgroundStart(exe, "kit", "report", "--quiet")
}

// machineReport reads this machine's agent files and checks.
func (c *cli) machineReport() machineReport {
	r := machineReport{Machine: machineName(), OS: runtime.GOOS + "/" + runtime.GOARCH, WPVersion: version, Files: []machineFile{}, Checks: []machineCheck{}}
	home, _ := os.UserHomeDir()
	claude := claudeConfigDir()
	add := func(rel, abs string) {
		if len(r.Files) >= maxReportFiles {
			return
		}
		// #nosec G304 -- the user's own agent files, read to report them
		b, err := os.ReadFile(abs)
		if err != nil {
			return
		}
		body := string(b)
		switch {
		case rel == ".claude.json":
			body = mcpNamesOnly(body)
		case strings.HasSuffix(rel, ".json"):
			body = redactJSON(body)
		case strings.HasPrefix(rel, ".claude/hooks/"):
			// Scripts embed tokens inline (curl -H 'Authorization: ...').
			body = redactCommand(body)
		}
		// Capped after cleaning. A cut JSON file wouldn't parse, so a JSON one
		// over the cap is left out; text is cut on a character boundary.
		if len(body) > maxReportFile {
			if strings.HasSuffix(rel, ".json") {
				return
			}
			body = strings.ToValidUTF8(body[:maxReportFile], "")
		}
		r.Files = append(r.Files, machineFile{Path: rel, Content: body})
	}
	add(".claude/CLAUDE.md", filepath.Join(claude, "CLAUDE.md"))
	add(".claude/settings.json", filepath.Join(claude, "settings.json"))
	for _, sub := range []string{"agents", "commands"} {
		root := filepath.Join(claude, sub)
		var found []string
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
				return nil
			}
			found = append(found, p)
			return nil
		})
		sort.Strings(found)
		for _, p := range found {
			rel, _ := filepath.Rel(claude, p)
			add(".claude/"+filepath.ToSlash(rel), p)
		}
	}
	if skills, err := filepath.Glob(filepath.Join(claude, "skills", "*", "SKILL.md")); err == nil {
		sort.Strings(skills)
		for _, p := range skills {
			add(".claude/skills/"+filepath.Base(filepath.Dir(p))+"/SKILL.md", p)
		}
	}
	if hooks, err := os.ReadDir(filepath.Join(claude, "hooks")); err == nil {
		for _, h := range hooks {
			if h.Type().IsRegular() {
				add(".claude/hooks/"+h.Name(), filepath.Join(claude, "hooks", h.Name()))
			}
		}
	}
	if home != "" {
		add(".claude.json", filepath.Join(home, ".claude.json"))
		add(".codex/AGENTS.md", filepath.Join(home, ".codex", "AGENTS.md"))
	}
	r.Checks = append(r.Checks, hooksCheck())
	if k, ok := c.kitMarketplaceCheck(); ok {
		r.Checks = append(r.Checks, k)
	}
	return r
}

// hooksCheck: are wp's Claude Code hooks installed here, and current?
func hooksCheck() machineCheck {
	ch := machineCheck{Key: "hooks", Label: "wp hooks installed"}
	if !claudeInstalled() {
		ch.State, ch.Note = "warn", "Claude Code isn't installed here"
		return ch
	}
	path, err := claudeSettingsPath()
	if err != nil {
		ch.State, ch.Note = "warn", err.Error()
		return ch
	}
	res, err := installClaudeHooks(path, hookInstallOpts{exe: hookExe(), keepNoPrompts: true, dryRun: true})
	switch {
	case err != nil:
		ch.State, ch.Note = "fail", "couldn't read "+path+": "+err.Error()
	case !res.changed:
		ch.State = "ok"
	case res.present:
		ch.State, ch.Note = "warn", "an older set: run wp setup"
	default:
		ch.State, ch.Note = "fail", "not installed: run wp setup (sessions here aren't seen by Waypoint)"
	}
	return ch
}

// kitMarketplaceCheck: is the agent kit's marketplace registered with
// Claude Code here? Not a check when there is no kit.
func (c *cli) kitMarketplaceCheck() (machineCheck, bool) {
	raw, err := c.call("GET", "/api/v1/kit", nil)
	if err != nil {
		return machineCheck{}, false
	}
	var info kitInfoCLI
	if json.Unmarshal(raw, &info) != nil || !info.Configured || info.Kit == nil || info.Kit.Marketplace == "" {
		return machineCheck{}, false
	}
	ch := machineCheck{Key: "kit", Label: "Agent kit marketplace"}
	var known map[string]json.RawMessage
	// #nosec G304 -- Claude Code's own record of its marketplaces
	b, err := os.ReadFile(filepath.Join(claudeConfigDir(), "plugins", "known_marketplaces.json"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		ch.State, ch.Note = "warn", "no marketplaces registered: run wp kit sync in a checkout"
	case err != nil || json.Unmarshal(b, &known) != nil:
		ch.State, ch.Note = "warn", "couldn't read Claude Code's marketplaces"
	case known[info.Kit.Marketplace] == nil:
		ch.State, ch.Note = "warn", info.Kit.Marketplace+" isn't registered: run wp kit sync in a checkout"
	default:
		ch.State = "ok"
	}
	return ch, true
}

// The client cleans settings before they leave the machine; the server
// cleans them again.
var secretKeyCLI = lazyre.New(`(?i)(token|secret|password|passwd|api[_-]?key|apikey|auth|credential|private|cookie|session)|(^|[^a-z])(key|pat|jwt|bearer)([^a-z]|$)`)

// redactJSON replaces every env value, header and secret-looking value with
// •••; text that isn't JSON isn't sent.
func redactJSON(raw string) string {
	var v any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return ""
	}
	var walk func(x any, under string) any
	walk = func(x any, under string) any {
		switch t := x.(type) {
		case map[string]any:
			for k, val := range t {
				m, isMap := val.(map[string]any)
				switch {
				case k == "env" || k == "headers" || under == "env":
					if isMap {
						for ek := range m {
							m[ek] = "•••"
						}
					} else {
						t[k] = "•••"
					}
				case k == "apiKeyHelper" || k == "awsAuthRefresh" || k == "awsCredentialExport":
					t[k] = "•••"
				case k == "command" && isStr(val):
					t[k] = redactCommand(val.(string))
				case k == "url" && isStr(val):
					t[k] = redactURL(val.(string))
				case k == "args" && isArr(val):
					t[k] = redactArgs(val.([]any))
				case under != "hooks" && secretKeyCLI.MatchString(k):
					if isMap {
						t[k] = walk(val, "env")
					} else {
						t[k] = "•••"
					}
				default:
					t[k] = walk(val, k)
				}
			}
			return t
		case []any:
			for i := range t {
				if under == "hooks" {
					// Hook entries are not event names: walk them as 'hook' so their secret-named fields are still blanked.
					t[i] = walk(t[i], "hook")
				} else {
					t[i] = walk(t[i], under)
				}
			}
			return t
		}
		return x
	}
	b, err := json.MarshalIndent(walk(v, ""), "", "  ")
	if err != nil {
		return ""
	}
	return string(b) + "\n"
}

// secretAssignRE finds NAME=value (quoted or bare) and --flag value pairs in a
// command line; only those whose name sounds secret are blanked.
var secretAssignRE = lazyre.New(`([A-Za-z_][A-Za-z0-9_-]*)=('[^']*'|"(?:\\.|[^"\\])*"|\S*)`)
var secretFlagRE = lazyre.New(`(--?[A-Za-z][A-Za-z0-9_-]*)(\s+)('[^']*'|"(?:\\.|[^"\\])*"|[^\s-]\S*)`)

// attachedHeaderRE finds a header glued to its flag: -H'Authorization: x'.
var attachedHeaderRE = lazyre.New(`(^|\s)-H('[^']*'|"(?:\\.|[^"\\])*"|\S+)`)

// redactCommand blanks secrets typed inline in a command (API_TOKEN='a b' wp
// hook, --token abc), keeping the rest.
func redactCommand(s string) string {
	s = attachedHeaderRE.ReplaceAllString(s, "${1}-H"+"•••")
	s = secretAssignRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := secretAssignRE.FindStringSubmatch(m)
		if secretKeyCLI.MatchString(sub[1]) || sub[1] == "header" {
			return sub[1] + "=" + "•••"
		}
		return m
	})
	return secretFlagRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := secretFlagRE.FindStringSubmatch(m)
		if secretKeyCLI.MatchString(sub[1]) || sub[1] == "-H" || sub[1] == "--header" {
			return sub[1] + sub[2] + "•••"
		}
		return m
	})
}

// redactArgs blanks the value after a secret-looking flag (--api-key X) and
// the value of --flag=value ones.
func redactArgs(args []any) []any {
	hide := false
	for i, a := range args {
		s, ok := a.(string)
		if !ok {
			if hide {
				args[i], hide = "•••", false
			}
			continue
		}
		switch {
		case hide:
			args[i], hide = "•••", false
		case strings.HasPrefix(s, "-H") && len(s) > 2 && !strings.HasPrefix(s, "--"):
			args[i] = "-H•••"
		case strings.HasPrefix(s, "-") && strings.Contains(s, "="):
			k, v, _ := strings.Cut(s, "=")
			switch {
			case secretKeyCLI.MatchString(k) || k == "--header":
				args[i] = k + "=" + "•••"
			case strings.Contains(v, "://"):
				args[i] = k + "=" + redactURL(v)
			}
		case strings.HasPrefix(s, "-") && (secretKeyCLI.MatchString(s) || s == "--header" || s == "-H"):
			hide = true
		case strings.Contains(s, "://"):
			args[i] = redactURL(s)
		case strings.Contains(s, "="):
			if k, _, _ := strings.Cut(s, "="); secretKeyCLI.MatchString(k) {
				args[i] = k + "=" + "•••"
			}
		}
	}
	return args
}

// redactURL drops a URL's query string, credentials and path (webhook URLs
// carry secrets there).
func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return "•••"
	}
	if u.RawQuery == "" && u.Fragment == "" && u.RawFragment == "" && u.User == nil && (u.Path == "" || u.Path == "/") {
		return s
	}
	u.RawQuery, u.Fragment, u.RawFragment, u.User, u.Path, u.RawPath = "", "", "", nil, "", ""
	return u.String()
}

func isStr(v any) bool { _, ok := v.(string); return ok }
func isArr(v any) bool { _, ok := v.([]any); return ok }

// mcpNamesOnly keeps ~/.claude.json's MCP server names: the rest is Claude
// Code's own state, and servers carry keys.
func mcpNamesOnly(raw string) string {
	var cj struct {
		Servers  map[string]json.RawMessage `json:"mcpServers"`
		Projects map[string]struct {
			Servers map[string]json.RawMessage `json:"mcpServers"`
		} `json:"projects"`
	}
	if json.Unmarshal([]byte(raw), &cj) != nil {
		return ""
	}
	names := map[string]map[string]string{}
	for n := range cj.Servers {
		names[n] = map[string]string{}
	}
	for _, p := range cj.Projects {
		for n := range p.Servers {
			names[n] = map[string]string{}
		}
	}
	b, _ := json.MarshalIndent(map[string]any{"mcpServers": names}, "", "  ")
	return string(b) + "\n"
}
