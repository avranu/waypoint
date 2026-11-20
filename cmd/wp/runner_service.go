package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Running the runner at login: a systemd user unit on Linux and a launchd
// agent on macOS. Both start `wp runner` with the flags given to
// `wp runner install`, restart it if it crashes (but not when Waypoint says
// it was removed), and start it again at login. The service gets the PATH
// of the shell that installed it, because that's where the agent CLIs are.

const (
	unitName     = "waypoint-runner.service"
	launchdLabel = "org.waypoint.runner"
)

// onlineWait is how long install waits between looks for the runner to come online.
var onlineWait = 2 * time.Second

// sysRun runs a service manager command (swapped in tests).
var sysRun = func(name string, args ...string) (string, error) {
	var b bytes.Buffer
	cmd := exec.Command(name, args...) // #nosec G204 -- systemctl, launchctl, loginctl with fixed arguments
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	return strings.TrimSpace(b.String()), err
}

// systemdQuote quotes one word of an ExecStart line.
func systemdQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"'\\$%;") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`)
	return `"` + r.Replace(s) + `"`
}

// runnerArgs is the command line the service runs, after the wp binary.
func runnerArgs(o runnerOpts) []string {
	args := []string{"runner"}
	if o.Name != "" {
		args = append(args, "--name", o.Name)
	}
	for _, l := range o.Labels {
		args = append(args, "--label", l)
	}
	for _, d := range o.Allow {
		args = append(args, "--allow", d)
	}
	for _, e := range o.Env {
		args = append(args, "--env", e)
	}
	if o.MaxJobs > 1 {
		args = append(args, "--max-jobs", fmt.Sprint(o.MaxJobs))
	}
	if o.NoUsageSync {
		args = append(args, "--no-usage-sync")
	}
	if o.NoPrep {
		args = append(args, "--no-prep")
	}
	if o.NoAutoUpdate {
		args = append(args, "--no-auto-update")
	}
	return args
}

// systemdUnit is the unit file text.
func systemdUnit(exe string, args []string, env map[string]string, server string) string {
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=Waypoint runner for " + strings.NewReplacer("\n", " ", "%", "%%").Replace(server) + "\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\n")
	words := []string{systemdQuote(exe)}
	for _, a := range args {
		words = append(words, systemdQuote(a))
	}
	b.WriteString("ExecStart=" + strings.Join(words, " ") + "\n")
	for _, k := range []string{"PATH", "WAYPOINT_CONFIG_DIR"} {
		if v := env[k]; v != "" {
			b.WriteString("Environment=" + systemdQuote(k+"="+v) + "\n")
		}
	}
	// A runner that was removed in Waypoint exits cleanly and stays stopped.
	b.WriteString("Restart=on-failure\nRestartSec=10\n\n[Install]\nWantedBy=default.target\n")
	return b.String()
}

func xmlEsc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// launchdPlist is the launch agent's property list.
func launchdPlist(exe string, args []string, env map[string]string, logPath string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>` + launchdLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEsc(exe) + `</string>
`)
	for _, a := range args {
		b.WriteString("\t\t<string>" + xmlEsc(a) + "</string>\n")
	}
	b.WriteString("\t</array>\n\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	for _, k := range []string{"PATH", "WAYPOINT_CONFIG_DIR"} {
		if v := env[k]; v != "" {
			b.WriteString("\t\t<key>" + k + "</key><string>" + xmlEsc(v) + "</string>\n")
		}
	}
	b.WriteString("\t</dict>\n\t<key>RunAtLoad</key><true/>\n\t<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>\n")
	b.WriteString("\t<key>StandardOutPath</key><string>" + xmlEsc(logPath) + "</string>\n\t<key>StandardErrorPath</key><string>" + xmlEsc(logPath) + "</string>\n</dict>\n</plist>\n")
	return b.String()
}

func unitPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user", unitName), nil
}

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

func launchdDomain() string {
	if u, err := user.Current(); err == nil {
		return "gui/" + u.Uid
	}
	return "gui/501"
}

func cmdRunnerInstall(c *cli, args []string) error {
	var o runnerOpts
	parse := runnerFlags(c, &o, true)
	pos, err := parse(args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	c.agent = "none"
	keepInstalled(c, &o)
	exe, err := prepareRunnerInstall(c, &o)
	if err != nil {
		return err
	}
	if err := c.ensureLogin(); err != nil {
		return err
	}
	return installRunner(c, exe, o)
}

// keepInstalled fills in the settings not given on the command line from the
// installed runner (the saved settings, else the service's command line), so
// `wp runner install --max-jobs 3` changes only the number of jobs and
// doesn't drop the runner's name and allowed directories. --allow none
// clears the allowed directories.
func keepInstalled(c *cli, o *runnerOpts) {
	prev := c.state.Runner
	if prev == nil {
		prev = installedService()
	}
	if prev != nil {
		var kept []string
		keep := func(flag string, set bool) {
			if set && !o.given[flag] {
				kept = append(kept, "--"+flag)
			}
		}
		keep("name", prev.Name != "")
		keep("label", len(prev.Labels) > 0)
		keep("allow", len(prev.Allow) > 0)
		keep("env", len(prev.Env) > 0)
		keep("max-jobs", prev.MaxJobs > 1)
		if !o.given["name"] {
			o.Name = prev.Name
		}
		if !o.given["label"] {
			o.Labels = prev.Labels
		}
		if !o.given["allow"] {
			o.Allow = prev.Allow
		}
		if !o.given["env"] {
			o.Env = prev.Env
		}
		if !o.given["max-jobs"] && prev.MaxJobs > 0 {
			o.MaxJobs = prev.MaxJobs
		}
		if !o.given["no-usage-sync"] {
			o.NoUsageSync = prev.NoUsageSync
		}
		if !o.given["no-prep"] {
			o.NoPrep = prev.NoPrep
		}
		if !o.given["no-auto-update"] {
			o.NoAutoUpdate = prev.NoAutoUpdate
		}
		if len(kept) > 0 {
			c.println("Keeping the installed runner's " + strings.Join(kept, ", ") + ": " + strings.Join(runnerArgs(*o)[1:], " "))
		}
	}
	if len(o.Allow) == 1 && strings.EqualFold(o.Allow[0], "none") {
		o.Allow = nil
	}
	if len(o.Env) == 1 && strings.EqualFold(o.Env[0], "none") {
		o.Env = nil
	}
}

// installedService reads the settings from the installed systemd unit's
// command line, for a runner installed before wp saved them; nil when there
// is none.
func installedService() *runnerInstalled {
	if runtime.GOOS != "linux" {
		return nil
	}
	path, err := unitPath()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(path) // #nosec G304 -- our own unit file
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "ExecStart="); ok {
			return parseRunnerArgs(systemdWords(rest))
		}
	}
	return nil
}

// systemdWords splits an ExecStart line as systemdQuote wrote it.
func systemdWords(s string) []string {
	var words []string
	var w strings.Builder
	in, started := false, false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case in && ch == '\\' && i+1 < len(s):
			i++
			w.WriteByte(s[i])
		case ch == '"':
			in, started = !in, true
		case (ch == '$' || ch == '%') && i+1 < len(s) && s[i+1] == ch:
			i++
			w.WriteByte(ch)
		case !in && (ch == ' ' || ch == '\t'):
			if started {
				words = append(words, w.String())
				w.Reset()
				started = false
			}
		default:
			w.WriteByte(ch)
			started = true
		}
	}
	if started {
		words = append(words, w.String())
	}
	return words
}

// parseRunnerArgs reads the settings from `wp runner …` words (the wp
// first); nil when they aren't a runner's.
func parseRunnerArgs(words []string) *runnerInstalled {
	if len(words) < 2 || words[1] != "runner" {
		return nil
	}
	r := &runnerInstalled{MaxJobs: 1}
	for i := 2; i < len(words); i++ {
		name, val, hasVal := strings.Cut(strings.TrimLeft(words[i], "-"), "=")
		next := func() string {
			if hasVal {
				return val
			}
			if i+1 < len(words) {
				i++
				return words[i]
			}
			return ""
		}
		switch name {
		case "name":
			r.Name = next()
		case "label":
			r.Labels = append(r.Labels, next())
		case "allow":
			r.Allow = append(r.Allow, next())
		case "env":
			r.Env = append(r.Env, next())
		case "max-jobs":
			if n, err := strconv.Atoi(next()); err == nil {
				r.MaxJobs = n
			}
		case "no-usage-sync":
			r.NoUsageSync = true
		case "no-prep":
			r.NoPrep = true
		case "no-auto-update":
			r.NoAutoUpdate = true
		}
	}
	return r
}

// runtimeDirBase is where systemd keeps each user's runtime directory.
var runtimeDirBase = "/run/user"

// ensureUserBus points systemctl --user at the user's bus when the shell
// doesn't say where it is (no XDG_RUNTIME_DIR, as after su or in some ssh
// and agent shells) but systemd runs one for the user (logged in, or
// lingering).
func ensureUserBus() {
	if os.Getenv("XDG_RUNTIME_DIR") != "" {
		return
	}
	dir := filepath.Join(runtimeDirBase, strconv.Itoa(os.Getuid()))
	if _, err := os.Stat(filepath.Join(dir, "bus")); err == nil {
		_ = os.Setenv("XDG_RUNTIME_DIR", dir)
	}
}

// runnerInstalled is the runner service's settings, in the state file, so
// wp setup can install it again as it was.
type runnerInstalled struct {
	Name         string   `json:"name,omitempty"`
	Labels       []string `json:"labels,omitempty"`
	Allow        []string `json:"allow,omitempty"`
	Env          []string `json:"env,omitempty"`
	MaxJobs      int      `json:"max_jobs,omitempty"`
	NoUsageSync  bool     `json:"no_usage_sync,omitempty"`
	NoPrep       bool     `json:"no_prep,omitempty"`
	NoAutoUpdate bool     `json:"no_auto_update,omitempty"`
}

// prepareRunnerInstall checks the settings before anything is installed,
// makes the allowed directories absolute, and returns the wp the service
// will run.
func prepareRunnerInstall(c *cli, o *runnerOpts) (string, error) {
	if o.MaxJobs < 1 || o.MaxJobs > maxRunnerJobs {
		return "", c.usage("--max-jobs is between 1 and %d", maxRunnerJobs)
	}
	// The runner would refuse a bad --env name; say so now.
	if err := passJobEnv(o.Env); err != nil {
		return "", c.usage("%v", err)
	}
	// The runner would refuse a directory that isn't there; say so now.
	if _, err := parseAllowed(o.Allow); err != nil {
		return "", err
	}
	if err := checkJobEnv(o.Env); err != nil {
		return "", c.usage("%v", err)
	}
	// Absolute paths, so the service doesn't depend on where install ran.
	for i, d := range o.Allow {
		if d == "~" || strings.HasPrefix(d, "~/") {
			continue // the runner expands it
		}
		if abs, err := filepath.Abs(d); err == nil {
			o.Allow[i] = abs
		}
	}
	exe, err := serviceExe()
	if err != nil {
		return "", err
	}
	if throwawayBuild(exe) {
		return "", errors.New("install the runner from an installed wp (curl -fsSL " + orDash(c.server()) + "/cli | sh), not a temporary build")
	}
	return exe, nil
}

// serviceExe is the wp a service runs: this one, symlinks resolved (tests
// swap it).
var serviceExe = func() (string, error) { return selfExe() }

// installRunner sets the service up and remembers its settings.
func installRunner(c *cli, exe string, o runnerOpts) error {
	if err := passJobEnv(o.Env); err != nil {
		return c.usage("%v", err)
	}
	env := map[string]string{"PATH": os.Getenv("PATH"), "WAYPOINT_CONFIG_DIR": os.Getenv("WAYPOINT_CONFIG_DIR")}
	set := map[string]bool{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && v != "" {
			set[strings.ToUpper(k)] = true
		}
	}
	for _, name := range o.Env {
		if n := strings.ToUpper(strings.TrimSpace(name)); n != "NONE" && !set[n] {
			c.println("warning: --env " + name + " is unset in this shell; the installed runner may not get it")
		}
	}
	var err error
	switch runtime.GOOS {
	case "linux":
		err = installSystemd(c, exe, o, env)
	case "darwin":
		err = installLaunchd(c, exe, o, env)
	default:
		printManualRunner(c, exe, o)
	}
	if err == nil {
		_ = c.updateState(func(s *State) {
			s.Runner = &runnerInstalled{Name: o.Name, Labels: o.Labels, Allow: o.Allow, Env: o.Env, MaxJobs: o.MaxJobs, NoUsageSync: o.NoUsageSync, NoPrep: o.NoPrep, NoAutoUpdate: o.NoAutoUpdate}
		})
	}
	return err
}

// printManualRunner says how to keep the runner running where wp can't set
// a service up (Windows).
func printManualRunner(c *cli, exe string, o runnerOpts) {
	c.println("wp can't set up a background service on", runtime.GOOS, "yet. Start the runner yourself in a terminal that stays open:")
	c.println()
	c.println("  " + strings.Join(append([]string{exe}, runnerArgs(o)...), " "))
	c.println()
	c.println(`Or have Task Scheduler run it at logon: schtasks /Create /SC ONLOGON /TN "Waypoint runner" /TR "` + schtasksCommand(exe, runnerArgs(o)) + `"`)
}

// schtasksCommand is the command line for schtasks' /TR "..." value: inside
// it, a path with spaces needs its own (escaped) quotes.
func schtasksCommand(exe string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	for _, a := range append([]string{exe}, args...) {
		if strings.ContainsAny(a, " \t") {
			a = `\"` + a + `\"`
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// ensureLogin makes sure there is a token for the server: the device login,
// started here when it is missing, so one command sets a new machine up.
func (c *cli) ensureLogin() error {
	if c.server() == "" {
		return errors.New("no server set: wp auth login https://your-waypoint")
	}
	if c.token() != "" {
		if _, err := c.call("GET", "/api/v1/me", nil); err == nil {
			return nil
		} else if ae, ok := err.(*apiError); !ok || ae.status != 401 {
			return err
		}
		c.println("The saved token no longer works; signing in again.")
	} else {
		c.println("Not signed in to", c.server(), "yet; signing in first.")
	}
	return authLogin(c, nil)
}

func installSystemd(c *cli, exe string, o runnerOpts, env map[string]string) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		c.println("systemd isn't available here. Start the runner in a terminal that stays open (tmux, screen):")
		c.println()
		c.println("  " + strings.Join(append([]string{exe}, runnerArgs(o)...), " "))
		return nil
	}
	ensureUserBus()
	path, err := unitPath()
	if err != nil {
		return err
	}
	unit := systemdUnit(exe, runnerArgs(o), env, c.server())
	// Installing again changes nothing, and doesn't restart (and so stop the
	// jobs of) a runner that is running as asked.
	if old, err := os.ReadFile(path); err == nil && string(old) == unit { // #nosec G304 -- our own unit file
		if out, _ := sysRun("systemctl", "--user", "is-active", unitName); out == "active" {
			c.println("The runner is installed and running already (systemd user unit " + unitName + ", unchanged).")
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { // #nosec G301 -- ~/.config/systemd/user
		return err
	}
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil { // #nosec G306 -- a unit file holds no secret
		return err
	}
	c.println("Wrote", path)
	for _, step := range [][]string{{"daemon-reload"}, {"enable", "--now", unitName}, {"restart", unitName}} {
		if out, err := sysRun("systemctl", append([]string{"--user"}, step...)...); err != nil {
			if strings.Contains(out, "Failed to connect to bus") {
				return fmt.Errorf("systemctl --user %s: %v %s\nsystemd isn't running a user session for %s here: once, sudo loginctl enable-linger %s, then run this again", strings.Join(step, " "), err, out, currentUser(), currentUser())
			}
			return fmt.Errorf("systemctl --user %s: %v %s", strings.Join(step, " "), err, out)
		}
	}
	c.println("Started the runner (systemd user unit " + unitName + "), and it starts at login.")
	if out, _ := sysRun("loginctl", "show-user", currentUser(), "-p", "Linger"); strings.Contains(out, "Linger=no") {
		c.println()
		c.println("To keep it running when you're logged out (a server, say), once:")
		c.println("  sudo loginctl enable-linger " + currentUser())
	}
	c.println()
	c.println("Logs:   journalctl --user -u waypoint-runner -f")
	c.println("Status: wp runner status")
	c.println("Remove: wp runner uninstall")
	awaitOnline(c, o)
	return nil
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

func installLaunchd(c *cli, exe string, o runnerOpts, env map[string]string) error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	logPath := filepath.Join(home, "Library", "Logs", "waypoint-runner.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { // #nosec G301 -- ~/Library/LaunchAgents
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil { // #nosec G301 -- ~/Library/Logs
		return err
	}
	plist := launchdPlist(exe, runnerArgs(o), env, logPath)
	if old, err := os.ReadFile(path); err == nil && string(old) == plist { // #nosec G304 -- our own plist
		if _, err := sysRun("launchctl", "print", launchdDomain()+"/"+launchdLabel); err == nil {
			c.println("The runner is installed and running already (launchd agent " + launchdLabel + ", unchanged).")
			return nil
		}
	}
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil { // #nosec G306 -- a plist holds no secret
		return err
	}
	c.println("Wrote", path)
	_, _ = sysRun("launchctl", "bootout", launchdDomain()+"/"+launchdLabel) // not loaded yet is fine
	if out, err := sysRun("launchctl", "bootstrap", launchdDomain(), path); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v %s", err, out)
	}
	c.println("Started the runner (launchd agent " + launchdLabel + "), and it starts at login.")
	c.println()
	c.println("Logs:   tail -f " + logPath)
	c.println("Status: wp runner status")
	c.println("Remove: wp runner uninstall")
	awaitOnline(c, o)
	return nil
}

// awaitOnline waits a few seconds for the runner to show up as online.
func awaitOnline(c *cli, o runnerOpts) {
	name := o.Name
	if name == "" {
		name = machineName()
	}
	for range 5 {
		time.Sleep(onlineWait)
		if rs, err := c.listRunners(context.Background(), false); err == nil {
			for _, r := range rs {
				if r.Name == name && r.Online {
					c.println()
					c.println("Online in Waypoint as", name+".")
					return
				}
			}
		}
	}
	c.println()
	c.println("Not online in Waypoint yet; check the logs above.")
}

func cmdRunnerUninstall(c *cli, args []string) error {
	pos, err := c.parse(c.flags(), args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	switch runtime.GOOS {
	case "linux":
		path, err := unitPath()
		if err != nil {
			return err
		}
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			c.println("The runner isn't installed.")
			return nil
		}
		ensureUserBus()
		_, _ = sysRun("systemctl", "--user", "disable", "--now", unitName)
		if err := os.Remove(path); err != nil {
			return err
		}
		_, _ = sysRun("systemctl", "--user", "daemon-reload")
		c.println("Stopped the runner and removed", path)
	case "darwin":
		path, err := plistPath()
		if err != nil {
			return err
		}
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			c.println("The runner isn't installed.")
			return nil
		}
		_, _ = sysRun("launchctl", "bootout", launchdDomain()+"/"+launchdLabel)
		if err := os.Remove(path); err != nil {
			return err
		}
		c.println("Stopped the runner and removed", path)
	default:
		c.println("Nothing was installed on", runtime.GOOS+"; stop the runner with Ctrl-C.")
		return nil
	}
	_ = c.updateState(func(s *State) { s.Runner = nil })
	c.println("It stays listed in Waypoint (Settings → Runners) until you remove it there.")
	return nil
}

// runnerInfo is a runner as the API returns it.
type runnerInfo struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	Hostname    string   `json:"hostname"`
	OS          string   `json:"os"`
	Arch        string   `json:"arch"`
	Version     string   `json:"version"`
	Labels      []string `json:"labels"`
	AllowedDirs []string `json:"allowed_dirs"`
	CLIs        []struct {
		Name     string `json:"name"`
		SignedIn *bool  `json:"signed_in"`
	} `json:"clis"`
	Online     bool       `json:"online"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	Running    int        `json:"running"`
	Capacity   *struct {
		Limit  int         `json:"limit"`
		Max    int         `json:"max"`
		Level  string      `json:"level"`
		Reason string      `json:"reason"`
		BusyAt []time.Time `json:"busy_at"`
		Heavy  int         `json:"heavy"`
		Light  int         `json:"light"`
	} `json:"capacity"`
}

func (c *cli) listRunners(ctx context.Context, all bool) ([]runnerInfo, error) {
	var rs []runnerInfo
	path := "/api/v1/runners"
	if all {
		path += "?all=1"
	}
	err := c.callInto(ctx, "GET", path, nil, &rs)
	return rs, err
}

// runnerServiceFn is runnerService, swapped in tests.
var runnerServiceFn = runnerService

// runnerService says whether this machine has the runner's service, whether
// it is running, and how to describe that.
func runnerService() (installed, running bool, desc string) {
	switch runtime.GOOS {
	case "linux":
		if path, err := unitPath(); err == nil {
			if _, err := os.Stat(path); err == nil {
				ensureUserBus()
				out, _ := sysRun("systemctl", "--user", "is-active", unitName)
				return true, out == "active", "systemd user unit " + unitName + ": " + orDash(out)
			}
		}
	case "darwin":
		if path, err := plistPath(); err == nil {
			if _, err := os.Stat(path); err == nil {
				if _, err := sysRun("launchctl", "print", launchdDomain()+"/"+launchdLabel); err != nil {
					return true, false, "launchd agent " + launchdLabel + ": not loaded"
				}
				return true, true, "launchd agent " + launchdLabel + ": loaded"
			}
		}
	}
	return false, false, "not installed (wp runner install)"
}

func cmdRunnerStatus(c *cli, args []string) error {
	pos, err := c.parse(c.flags(), args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	_, _, service := runnerService()
	raw, err := c.call("GET", "/api/v1/runners", nil)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		var rs []runnerInfo
		if err := jsonInto(raw, &rs); err != nil {
			return "service: " + service
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Service on this machine: %s\nWaypoint: %s\n", service, c.server())
		if len(rs) == 0 {
			b.WriteString("\nYou have no runners. Set this computer up with: wp setup\n")
		}
		for _, r := range rs {
			state := "offline"
			if r.Online {
				state = "online"
			}
			seen := ""
			if r.LastSeenAt != nil {
				seen = ", last seen " + humanSince(*r.LastSeenAt)
			}
			fmt.Fprintf(&b, "\n%s  %s%s  (%s/%s, wp %s)\n", r.Name, state, seen, r.OS, r.Arch, r.Version)
			if len(r.Labels) > 0 {
				fmt.Fprintf(&b, "  labels:  %s\n", strings.Join(r.Labels, ", "))
			}
			fmt.Fprintf(&b, "  allowed: %s\n", orDash(strings.Join(r.AllowedDirs, ", ")))
			var clis []string
			for _, cl := range r.CLIs {
				mark := "?"
				if cl.SignedIn != nil {
					mark = map[bool]string{true: "signed in", false: "signed out"}[*cl.SignedIn]
				}
				clis = append(clis, cl.Name+" ("+mark+")")
			}
			fmt.Fprintf(&b, "  CLIs:    %s\n", orDash(strings.Join(clis, ", ")))
			if r.Running > 0 {
				fmt.Fprintf(&b, "  running: %d job%s\n", r.Running, pluralS(r.Running))
			}
			if cp := r.Capacity; cp != nil && cp.Level != "" {
				fmt.Fprintf(&b, "  jobs:    %s\n", capacityLine(cp.Level, cp.Reason, cp.Limit, cp.Max))
				if len(cp.BusyAt) > 0 {
					fmt.Fprintf(&b, "  usually busy: %s\n", hourRanges(cp.BusyAt, time.Local))
				}
			}
		}
		return b.String()
	})
}

func humanSince(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// capacityLine says how many jobs a runner takes now and why (WP-366).
func capacityLine(level, reason string, limit, maxJobs int) string {
	what := fmt.Sprintf("up to %d heavy job%s at once now, of --max-jobs %d", limit, pluralS(limit), maxJobs)
	if level == "overloaded" {
		what = "one job at a time now"
	}
	return fmt.Sprintf("%s (%s: %s)", what, level, reason)
}

// hourRanges writes hour starts as ranges in loc: "03:00-05:00, 13:00-14:00".
func hourRanges(hours []time.Time, loc *time.Location) string {
	var out []string
	for i := 0; i < len(hours); {
		j := i
		for j+1 < len(hours) && hours[j+1].Sub(hours[j]) == time.Hour {
			j++
		}
		out = append(out, hours[i].In(loc).Format("15:04")+"-"+hours[j].Add(time.Hour).In(loc).Format("15:04"))
		i = j + 1
	}
	return strings.Join(out, ", ")
}
