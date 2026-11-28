package kernel

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// User is an account. Secrets in Prefs are only returned to the user themself.
type User struct {
	ID            int64     `json:"id"`
	Username      string    `json:"username"`
	Email         string    `json:"email"`
	DisplayName   string    `json:"display_name"`
	IsAdmin       bool      `json:"is_admin"`
	Disabled      bool      `json:"disabled"`
	AvatarURL     string    `json:"avatar_url"`
	Timezone      string    `json:"timezone"`
	Prefs         UserPrefs `json:"prefs"`
	CreatedAt     time.Time `json:"created_at"`
	AvatarVersion int64     `json:"-"`
	PasswordHash  string    `json:"-"`
}

// UserPrefs are per-user settings.
type UserPrefs struct {
	Theme       string `json:"theme,omitempty"` // "", "light", "dark"
	GotifyURL   string `json:"gotify_url,omitempty"`
	GotifyToken string `json:"gotify_token,omitempty"`
	// Notification kinds that are *not* forwarded to Gotify.
	GotifyMuted []string `json:"gotify_muted,omitempty"`
	// Notification kinds that are *not* pushed to subscribed browsers.
	PushMuted []string `json:"push_muted,omitempty"`
	// QuotaJump is the rise in the week's quota, in points within an hour,
	// that pushes an alert (WP-146). Unset means 4; 0 turns it off.
	QuotaJump *int `json:"quota_jump,omitempty"`
	// WeekStart: 0 Sunday, 1 Monday.
	WeekStart int `json:"week_start"`
	// HiddenHelp names the explainers this person folded away.
	HiddenHelp []string `json:"hidden_help,omitempty"`
	// OpenHelp names the explainers this person opened; the rest start folded.
	OpenHelp []string `json:"open_help,omitempty"`
	// ProjectSort is how the sidebar orders this person's projects: "recent"
	// (latest activity first) or "custom" (ProjectOrder). Empty means custom
	// once they have dragged a project, recent before.
	ProjectSort string `json:"project_sort,omitempty"`
	// ProjectOrder is the sidebar's custom order, project ids first to last.
	// Projects missing from it (new ones) come first.
	ProjectOrder []int64 `json:"project_order,omitempty"`
	// CacheWarm is how Waypoint keeps the prompt cache of this person's
	// sessions warm (WP-192, WP-534): "" pings them, CacheWarmCompact
	// compacts them instead.
	CacheWarm string `json:"cache_warm,omitempty"`
	// DismissedWarnings are the goal wording warnings this person waved off,
	// each "NOTE_ID:WARNING", newest last, so they stay gone on every device.
	DismissedWarnings []string `json:"dismissed_warnings,omitempty"`
	// Sessions is how Waypoint starts this person's Claude Code sessions
	// (WP-1265): for all of them, and for single repos.
	Sessions *SessionPrefs `json:"sessions,omitempty"`
}

// GotifyMask stands in for a saved Gotify token in API responses; sending
// it back unchanged keeps the token.
const GotifyMask = "••••••••"

// MarshalJSON hides the Gotify token: it's write-only over the API.
func (p UserPrefs) MarshalJSON() ([]byte, error) {
	type plain UserPrefs
	out := plain(p)
	if out.GotifyToken != "" {
		out.GotifyToken = GotifyMask
	}
	return json.Marshal(out)
}

// UserRef is the public view of a user.
type UserRef struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	AvatarURL   string `json:"avatar_url,omitempty"`
}

// Name is the display name, falling back to the username.
func (u *User) Name() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

func (u *User) Ref() UserRef {
	return UserRef{ID: u.ID, Username: u.Username, DisplayName: u.Name(), AvatarURL: u.AvatarURL}
}

// DefaultQuotaJump is the rise in the week's quota, in points within an hour,
// that pushes an alert when the person set none.
const DefaultQuotaJump = 4

// QuotaJumpThreshold is the person's setting: unset means the default, 0 off.
func (p UserPrefs) QuotaJumpThreshold() int {
	if p.QuotaJump == nil {
		return DefaultQuotaJump
	}
	return *p.QuotaJump
}

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
		return Invalid("%s: the model is an alias like opus or a model's name", where)
	case s.Effort != "" && !slices.Contains(SessionEfforts, s.Effort):
		return Invalid("%s: effort is one of %s", where, strings.Join(SessionEfforts, ", "))
	case s.PermissionMode != "" && !slices.Contains(SessionPermissionModes, s.PermissionMode):
		return Invalid("%s: the permission mode is one of %s", where, strings.Join(SessionPermissionModes, ", "))
	}
	return nil
}

// maxSessionRepos bounds Repos.
const maxSessionRepos = 200

// NestedRepoRE is a repo name as a forge shows it: owner/name, or
// group/subgroup/…/name.
var NestedRepoRE = lazyre.New(`^[\w.-]+(/[\w.-]+)+$`)

// ValidateSessionPrefs checks the settings and tidies them: repo names lower case,
// repos with nothing set dropped. Empty is nil.
func ValidateSessionPrefs(p *SessionPrefs) (*SessionPrefs, error) {
	if p == nil {
		return nil, nil
	}
	if err := p.All.Validate("all sessions"); err != nil {
		return nil, err
	}
	if len(p.Repos) > maxSessionRepos {
		return nil, Invalid("session settings for at most %d repos", maxSessionRepos)
	}
	repos := map[string]SessionSettings{}
	for name, s := range p.Repos {
		key := strings.ToLower(strings.TrimSpace(name))
		if !NestedRepoRE.MatchString(key) || len(key) > 200 {
			return nil, Invalid("%q isn't a repo like owner/name", name)
		}
		if _, dup := repos[key]; dup {
			return nil, Invalid("%s has two sets of session settings", key)
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
