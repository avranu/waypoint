package core

import (
	"slices"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// SessionSettings are the Claude Code options a session starts with. Unset
// (nil, "") leaves the choice to the launch, then to Claude Code.
type SessionSettings struct {
	// RemoteControl: --rc, so the session can be followed from the Claude
	// app. Unset: autopilot's sessions get it (WP-1044) unless CodeMode is
	// on, the others don't.
	RemoteControl *bool `json:"remote_control,omitempty"`
	// CodeMode sets CLAUDE_CODE_EXPERIMENTAL_CODE_MODE to 1 or 0. Claude
	// Code turns it off in a session with Remote Control or in the background.
	CodeMode *bool `json:"code_mode,omitempty"`
	// Chrome: --chrome or --no-chrome.
	Chrome *bool `json:"chrome,omitempty"`
	// Model, Effort and PermissionMode: --model, --effort, --permission-mode,
	// where the launch doesn't name its own.
	Model          string `json:"model,omitempty"`
	Effort         string `json:"effort,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
}

// SessionPrefs are a person's session settings: All for every session, and
// Repos for the sessions in one repo (owner/name, lower case), whose set
// fields win.
type SessionPrefs struct {
	All   SessionSettings            `json:"all"`
	Repos map[string]SessionSettings `json:"repos,omitempty"`
}

// SessionEfforts and SessionPermissionModes are the values the settings
// take. bypassPermissions is left out: a setting that turns every check off
// for every unattended session is one a person passes by hand, per session.
var (
	SessionEfforts         = []string{"low", "medium", "high", "xhigh", "max"}
	SessionPermissionModes = []string{"acceptEdits", "auto", "dontAsk", "manual", "plan"}
)

// sessionModelRE: an alias (opus) or a full name (claude-opus-5-5[1m]).
var sessionModelRE = lazyre.New(`^[A-Za-z0-9][A-Za-z0-9._:\[\]-]{0,79}$`)

// maxSessionRepos bounds Repos.
const maxSessionRepos = 200

// Empty says nothing is set.
func (s SessionSettings) Empty() bool { return s == SessionSettings{} }

// Over is s with o's set fields in their place.
func (s SessionSettings) Over(o SessionSettings) SessionSettings {
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

// For is the settings of a session in repo (owner/name; "" outside one).
func (p *SessionPrefs) For(repo string) SessionSettings {
	if p == nil {
		return SessionSettings{}
	}
	return p.All.Over(p.Repos[strings.ToLower(repo)])
}

func (s *SessionSettings) Validate(where string) error {
	s.Model = strings.TrimSpace(s.Model)
	switch {
	case s.Model != "" && !sessionModelRE.MatchString(s.Model):
		return invalid("%s: the model is an alias like opus or a model's name", where)
	case s.Effort != "" && !slices.Contains(SessionEfforts, s.Effort):
		return invalid("%s: effort is one of %s", where, strings.Join(SessionEfforts, ", "))
	case s.PermissionMode != "" && !slices.Contains(SessionPermissionModes, s.PermissionMode):
		return invalid("%s: the permission mode is one of %s", where, strings.Join(SessionPermissionModes, ", "))
	}
	return nil
}

// validate checks the settings and tidies them: repo names lower case,
// repos with nothing set dropped. Empty is nil.
func (p *SessionPrefs) Validate() (*SessionPrefs, error) {
	if p == nil {
		return nil, nil
	}
	if err := p.All.Validate("all sessions"); err != nil {
		return nil, err
	}
	if len(p.Repos) > maxSessionRepos {
		return nil, invalid("session settings for at most %d repos", maxSessionRepos)
	}
	repos := map[string]SessionSettings{}
	for name, s := range p.Repos {
		key := strings.ToLower(strings.TrimSpace(name))
		if !nestedRE.MatchString(key) || len(key) > 200 {
			return nil, invalid("%q isn't a repo like owner/name", name)
		}
		if _, dup := repos[key]; dup {
			return nil, invalid("%s has two sets of session settings", key)
		}
		if err := s.Validate(key); err != nil {
			return nil, err
		}
		if !s.Empty() {
			repos[key] = s
		}
	}
	out := &SessionPrefs{All: p.All}
	if len(repos) > 0 {
		out.Repos = repos
	}
	if out.All.Empty() && out.Repos == nil {
		return nil, nil
	}
	return out, nil
}
