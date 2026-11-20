package main

import (
	"strings"
)

// The person's session settings (WP-1265): how the Claude Code sessions
// Waypoint starts for them start, for all sessions and per repo. The server
// keeps them in the person's preferences (core.SessionPrefs); a runner gets
// them with each job that starts a session, wp start and wp fanout read them
// from /me.

// sessionSettings mirrors core.SessionSettings.
type sessionSettings struct {
	RemoteControl  *bool  `json:"remote_control,omitempty"`
	CodeMode       *bool  `json:"code_mode,omitempty"`
	Chrome         *bool  `json:"chrome,omitempty"`
	Model          string `json:"model,omitempty"`
	Effort         string `json:"effort,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
}

// sessionPrefs mirrors core.SessionPrefs.
type sessionPrefs struct {
	All   sessionSettings            `json:"all"`
	Repos map[string]sessionSettings `json:"repos,omitempty"`
}

// mySessionPrefs is the session settings of the person this wp acts for;
// nil when the server has none (an older server) or can't be read, which
// it says on stderr: the session still starts, without them.
func (c *cli) mySessionPrefs() *sessionPrefs {
	var me struct {
		Prefs struct {
			Sessions *sessionPrefs `json:"sessions"`
		} `json:"prefs"`
	}
	if err := c.do("GET", "/api/v1/me", nil, &me); err != nil {
		c.note("wp: couldn't read your Claude Code settings (%v); starting without them", err)
		return nil
	}
	return me.Prefs.Sessions
}

// codeModeVar is the variable that turns Claude Code's Code Mode on (1) or off (0).
const codeModeVar = "CLAUDE_CODE_EXPERIMENTAL_CODE_MODE"

// over is s with o's set fields in their place.
func (s sessionSettings) over(o sessionSettings) sessionSettings {
	if o.RemoteControl != nil {
		s.RemoteControl = o.RemoteControl
	}
	if o.CodeMode != nil {
		s.CodeMode = o.CodeMode
	}
	if o.Chrome != nil {
		s.Chrome = o.Chrome
	}
	if o.Model != "" {
		s.Model = o.Model
	}
	if o.Effort != "" {
		s.Effort = o.Effort
	}
	if o.PermissionMode != "" {
		s.PermissionMode = o.PermissionMode
	}
	return s
}

// forRepo is the settings for a session in repo (owner/name, "" for none).
func (p *sessionPrefs) forRepo(repo string) sessionSettings {
	if p == nil {
		return sessionSettings{}
	}
	return p.All.over(p.Repos[strings.ToLower(repo)])
}

// forDir is the settings for a session started in dir: its origin's repo's.
func (p *sessionPrefs) forDir(dir string) sessionSettings {
	if p == nil {
		return sessionSettings{}
	}
	repo := ""
	if len(p.Repos) > 0 {
		repo = originRepo(dir)
	}
	return p.forRepo(repo)
}

// env is what the settings add to a session's environment.
func (s sessionSettings) env() []string {
	switch {
	case s.CodeMode == nil:
		return nil
	case *s.CodeMode:
		return []string{codeModeVar + "=1"}
	default:
		return []string{codeModeVar + "=0"}
	}
}

// autopilotRC says whether an autopilot session gets Remote Control: as set,
// else on (WP-1044) unless Code Mode is, which Remote Control turns off.
func (s sessionSettings) autopilotRC() bool {
	if s.RemoteControl != nil {
		return *s.RemoteControl
	}
	return s.CodeMode == nil || !*s.CodeMode
}

// fill gives l the settings' choices it has no own for.
func (s sessionSettings) fill(l *taskLaunch) {
	l.model = cmpStr(l.model, s.Model)
	l.effort = cmpStr(l.effort, s.Effort)
	l.permissionMode = cmpStr(l.permissionMode, s.PermissionMode)
	l.remoteControl = s.RemoteControl != nil && *s.RemoteControl
	l.chrome = s.Chrome
	l.env = s.env()
}

// applyTo is args, the flags of an autopilot start or restart, with the
// settings in place of the flags they set, and Remote Control as
// autopilotRC says. A permission mode replaces --dangerously-skip-permissions
// too, which is bypassPermissions by another name.
func (s sessionSettings) applyTo(args []string) []string {
	if s.PermissionMode != "" {
		args = withoutFlag(args, "--dangerously-skip-permissions", false)
	}
	for _, f := range []struct{ name, value string }{{"--model", s.Model}, {"--effort", s.Effort}, {"--permission-mode", s.PermissionMode}} {
		if f.value != "" {
			args = append(withoutFlag(args, f.name, true), f.name, f.value)
		}
	}
	if s.Chrome != nil {
		args = withoutFlag(withoutFlag(args, "--chrome", false), "--no-chrome", false)
		args = append(args, chromeFlag(*s.Chrome))
	}
	if s.autopilotRC() {
		return withRemoteControl(args)
	}
	return withoutFlag(withoutFlag(args, "--rc", false), "--remote-control", false)
}

func chromeFlag(on bool) string {
	if on {
		return "--chrome"
	}
	return "--no-chrome"
}

// withoutFlag is args without flag name, and its value when it takes one
// (or has one after =).
func withoutFlag(args []string, name string, takes bool) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		n, _, inline := strings.Cut(args[i], "=")
		if n != name {
			out = append(out, args[i])
			continue
		}
		if takes && !inline {
			i++
		}
	}
	return out
}
