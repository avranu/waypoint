package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

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
	avatarVersion int64
	passwordHash  string
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

// maxDismissedWarnings bounds DismissedWarnings; the oldest go first.
const maxDismissedWarnings = 500

// CacheWarmCompact: compact a session whose cache is to be kept warm,
// instead of pinging it.
const CacheWarmCompact = "compact"

// maxProjectOrder bounds ProjectOrder; no one keeps more projects than this.
const maxProjectOrder = 2000

// gotifyMask stands in for a saved Gotify token in API responses; sending
// it back unchanged keeps the token.
const gotifyMask = "••••••••"

// MarshalJSON hides the Gotify token: it's write-only over the API.
func (p UserPrefs) MarshalJSON() ([]byte, error) {
	type plain UserPrefs
	out := plain(p)
	if out.GotifyToken != "" {
		out.GotifyToken = gotifyMask
	}
	return json.Marshal(out)
}

// storedPrefs is UserPrefs as written to the database, token included.
type storedPrefs UserPrefs

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

func avatarURL(id, version int64) string {
	if version == 0 {
		return ""
	}
	return fmt.Sprintf("/api/v1/users/%d/avatar?v=%d", id, version)
}

const userCols = `id, username, email, display_name, password_hash, is_admin, disabled, avatar_version, timezone, prefs, created_at`

func scanUser(sc interface{ Scan(...any) error }) (*User, error) {
	var u User
	var prefs string
	var created int64
	if err := sc.Scan(&u.ID, &u.Username, &u.Email, &u.DisplayName, &u.passwordHash, &u.IsAdmin, &u.Disabled,
		&u.avatarVersion, &u.Timezone, &prefs, &created); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(prefs), &u.Prefs)
	u.CreatedAt = msTime(created)
	u.AvatarURL = avatarURL(u.ID, u.avatarVersion)
	if u.DisplayName == "" {
		u.DisplayName = u.Username
	}
	return &u, nil
}

func (s *Service) userByID(ctx context.Context, q queryer, id int64) (*User, error) {
	u, err := scanUser(q.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("user")
	}
	return u, err
}

// UserByID loads a user.
func (s *Service) UserByID(ctx context.Context, id int64) (*User, error) {
	return s.userByID(ctx, s.db.R, id)
}

// userByName finds a user by username, email, or numeric id.
// userByName finds a user by username or email, or by id when name is a
// number (usernames can't be all digits, so the two never collide).
func (s *Service) userByName(ctx context.Context, q queryer, name string) (*User, error) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "@")
	if id, err := strconv.ParseInt(name, 10, 64); err == nil {
		return s.userByID(ctx, q, id)
	}
	return s.userByLogin(ctx, q, name)
}

// userByLogin finds a user by username or email (both case-insensitive).
func (s *Service) userByLogin(ctx context.Context, q queryer, login string) (*User, error) {
	u, err := scanUser(q.QueryRowContext(ctx, `SELECT `+userCols+` FROM users
		WHERE username = ? OR email = ? ORDER BY username = ? DESC LIMIT 1`, login, login, login))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound(fmt.Sprintf("user %q", login))
	}
	return u, err
}

// resolveUser turns "me", a username, email or id into a user.
func (s *Service) resolveUser(ctx context.Context, q queryer, actor *Actor, ref string) (*User, error) {
	if strings.EqualFold(ref, "me") && actor != nil && actor.User != nil && actor.User.ID != 0 {
		return actor.User, nil
	}
	return s.userByName(ctx, q, ref)
}

// ListUsers returns every account. Non-admins get public fields only.
func (s *Service) ListUsers(ctx context.Context, actor *Actor) ([]*User, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY disabled, lower(display_name), username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		u.Prefs = UserPrefs{}
		if !actor.isAdmin() && u.ID != actor.id() {
			u.Email = ""
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

var usernameRE = lazyre.New(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,38}$`)

// Names that mean something else wherever a user can be named.
var reservedUsernames = []string{"me", "none", "system", "actor", "reporter", "assignees", "watchers", "unassigned"}

func validateUsername(u string) error {
	if !usernameRE.MatchString(u) || slices.ContainsFunc(reservedUsernames, func(r string) bool { return strings.EqualFold(u, r) }) {
		return invalid("username must be 1-39 letters, digits, '.', '_' or '-', and not a reserved word")
	}
	if _, err := strconv.ParseInt(u, 10, 64); err == nil {
		return invalid("username can't be only digits")
	}
	return nil
}

func validateEmail(e string) error {
	if _, err := mail.ParseAddress(e); err != nil || strings.ContainsAny(e, "<> ") {
		return invalid("invalid email address")
	}
	return nil
}

func validatePassword(p string) error {
	if len(p) < 10 {
		return invalid("password must be at least 10 characters")
	}
	if len(p) > 1024 {
		return invalid("password is too long")
	}
	return nil
}

const (
	argonTime    = 2
	argonMemory  = 19 * 1024
	argonThreads = 1
	argonKeyLen  = 32
)

// HashPassword returns an argon2id PHC string.
func HashPassword(pw string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// CheckPassword verifies pw against a PHC string from HashPassword.
func CheckPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	b64 := base64.RawStdEncoding
	salt, err1 := b64.DecodeString(parts[4])
	want, err2 := b64.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(want) == 0 || len(want) > 1024 {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want))) // #nosec G115 -- bounded above
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash makes failed logins for unknown users cost the same as real ones.
// It is computed on first use: argon2 allocates ~19 MB and ran at every process
// start, which the 2 s hook deadline cannot afford (WP-1398).
var dummyHash = sync.OnceValue(func() string { return HashPassword("not-a-real-password-just-timing") })

// WarmLogin computes the dummy hash, so the server's first failed login for an
// unknown user takes no longer than later ones. The server calls it at start.
func WarmLogin() { dummyHash() }

func newToken(prefix string) string {
	b := make([]byte, 32)
	rand.Read(b)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

const sessionTTL = 60 * 24 * time.Hour

// Login checks credentials and creates a session, returning its token.
func (s *Service) Login(ctx context.Context, login, password, userAgent, ip string) (string, *User, error) {
	u, err := s.userByLogin(ctx, s.db.R, strings.TrimSpace(login))
	if err != nil {
		CheckPassword(dummyHash(), password)
		return "", nil, ErrUnauthorized
	}
	if !CheckPassword(u.passwordHash, password) || u.Disabled {
		return "", nil, ErrUnauthorized
	}
	tok, err := s.createSession(ctx, u.ID, userAgent, ip)
	return tok, u, err
}

func (s *Service) createSession(ctx context.Context, userID int64, userAgent, ip string) (string, error) {
	tok := newToken("wps_")
	now := s.nowMS()
	if len(userAgent) > 300 {
		userAgent = userAgent[:300]
	}
	_, err := s.db.W.ExecContext(ctx, `INSERT INTO sessions(token_hash, user_id, created_at, last_seen_at, expires_at, user_agent, ip, reauth_at)
		VALUES (?,?,?,?,?,?,?,?)`, hashToken(tok), userID, now, now, now+sessionTTL.Milliseconds(), userAgent, ip, now)
	return tok, err
}

// SessionUser returns the user for a session token, sliding its expiry.
// SessionActor is SessionUser as an actor that knows its own session, so
// "sign out other sessions" can keep it.
func (s *Service) SessionActor(ctx context.Context, tok string) (*Actor, error) {
	u, err := s.SessionUser(ctx, tok)
	if err != nil {
		return nil, err
	}
	return &Actor{User: u, session: hashToken(tok)}, nil
}

func (s *Service) SessionUser(ctx context.Context, tok string) (*User, error) {
	h := hashToken(tok)
	var uid, lastSeen, expires int64
	err := s.db.R.QueryRowContext(ctx, `SELECT user_id, last_seen_at, expires_at FROM sessions WHERE token_hash = ?`, h).Scan(&uid, &lastSeen, &expires)
	if err != nil {
		return nil, ErrUnauthorized
	}
	now := s.nowMS()
	if expires < now {
		return nil, ErrUnauthorized
	}
	u, err := s.userByID(ctx, s.db.R, uid)
	if err != nil || u.Disabled {
		return nil, ErrUnauthorized
	}
	if now-lastSeen > 10*60*1000 {
		if _, err := s.db.W.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE token_hash = ?`, now, now+sessionTTL.Milliseconds(), h); err != nil {
			s.log.WarnContext(ctx, "best-effort write failed", "err", err)
		}
	}
	return u, nil
}

// Logout deletes a session.
func (s *Service) Logout(ctx context.Context, tok string) error {
	_, err := s.db.W.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hashToken(tok))
	return err
}

// APIToken describes a personal access token (never the secret itself).
type APIToken struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Prefix   string `json:"prefix"`
	ReadOnly bool   `json:"read_only"`
	Agent    string `json:"agent,omitempty"` // agent identity, for agent tokens
	// Permissions is what the token may do (WP-111); null when unrestricted.
	Permissions []Permission   `json:"permissions"`
	Refusals    []TokenRefusal `json:"refusals,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	LastUsedAt  *time.Time     `json:"last_used_at"`
	ExpiresAt   *time.Time     `json:"expires_at"`
}

// TokenSpec describes a new API token.
type TokenSpec struct {
	Name     string
	ReadOnly bool
	TTL      time.Duration
	// Agent makes it an agent token: requests made with it act as this agent
	// (e.g. "anthropic/claude-opus-5-5").
	Agent string
	// Permissions limits what the token may do (WP-111); nil: unrestricted.
	Permissions []Permission
}

// CreateAPIToken makes a token for the actor; the secret is returned once.
func (s *Service) CreateAPIToken(ctx context.Context, actor *Actor, name string, readOnly bool, ttl time.Duration) (string, *APIToken, error) {
	return s.CreateToken(ctx, actor, TokenSpec{Name: name, ReadOnly: readOnly, TTL: ttl})
}

// CreateToken makes a token for the actor; the secret is returned once.
func (s *Service) CreateToken(ctx context.Context, actor *Actor, spec TokenSpec) (string, *APIToken, error) {
	tok, t, err := s.createToken(ctx, s.db.W, actor, spec)
	if err == nil {
		s.audit(ctx, actor, tokenAudit("token.create", t))
	}
	return tok, t, err
}

// tokenAudit is a token change for the log: an agent token names its agent.
func tokenAudit(action string, t *APIToken) AuditEntry {
	detail := "an API token"
	switch {
	case t.Agent != "":
		detail = "an agent token for " + t.Agent
	case t.Permissions != nil:
		detail = "an API token with permissions"
	case t.ReadOnly:
		detail = "a read-only API token"
	}
	if action == "token.create" {
		detail = "created " + detail
		if t.ExpiresAt != nil {
			detail += ", expiring " + t.ExpiresAt.UTC().Format("2006-01-02")
		}
		if t.Permissions != nil {
			detail += ", limited to " + permissionNames(t.Permissions)
		}
	} else {
		detail = "revoked " + detail
	}
	return AuditEntry{Action: action, Category: AuditAccess, TargetType: "token", TargetID: t.ID, Target: t.Name, Detail: detail, About: t.Agent}
}

func (s *Service) createToken(ctx context.Context, q queryer, actor *Actor, spec TokenSpec) (string, *APIToken, error) {
	if err := actor.canManageTokens(); err != nil {
		return "", nil, err
	}
	perms, err := normalizePermissions(spec.Permissions)
	if err != nil {
		return "", nil, err
	}
	if perms != nil && spec.ReadOnly {
		return "", nil, invalid("a token is read-only or has permissions, not both")
	}
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return "", nil, invalid("token name is required")
	}
	agent := strings.TrimSpace(spec.Agent)
	if len(agent) > 100 {
		return "", nil, invalid("agent names can be at most 100 characters")
	}
	tok := newToken("wpt_")
	now := s.nowMS()
	var exp any
	if spec.TTL > 0 {
		exp = now + spec.TTL.Milliseconds()
	}
	res, err := q.ExecContext(ctx, `INSERT INTO api_tokens(user_id, name, token_hash, prefix, read_only, agent, permissions, created_at, expires_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, actor.id(), name, hashToken(tok), tok[:12], boolInt(spec.ReadOnly), agent, permissionsJSON(perms), now, exp)
	if err != nil {
		return "", nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return "", nil, err
	}
	t := &APIToken{ID: id, Name: name, Prefix: tok[:12], ReadOnly: spec.ReadOnly, Agent: agent, Permissions: perms, CreatedAt: msTime(now)}
	if exp != nil {
		t.ExpiresAt = ptrTime(sql.NullInt64{Int64: exp.(int64), Valid: true})
	}
	return tok, t, nil
}

// ListAPITokens lists the actor's tokens.
func (s *Service) ListAPITokens(ctx context.Context, actor *Actor) ([]APIToken, error) {
	return s.apiTokens(ctx, actor, 0)
}

// apiToken is one of the actor's tokens.
func (s *Service) apiToken(ctx context.Context, actor *Actor, id int64) (*APIToken, error) {
	list, err := s.apiTokens(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, notFound("token")
	}
	return &list[0], nil
}

// apiTokens lists the actor's tokens, or just token id when it isn't 0.
func (s *Service) apiTokens(ctx context.Context, actor *Actor, id int64) ([]APIToken, error) {
	if actor.Restricted() {
		return nil, actor.canManageTokens()
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, name, prefix, read_only, agent, permissions, created_at, last_used_at, expires_at
		FROM api_tokens WHERE user_id = ? AND (? = 0 OR id = ?) AND runner_job_id IS NULL ORDER BY id DESC`, actor.id(), id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIToken{}
	var limited []int64
	for rows.Next() {
		var t APIToken
		var created int64
		var used, exp sql.NullInt64
		var perms sql.NullString
		if err := rows.Scan(&t.ID, &t.Name, &t.Prefix, &t.ReadOnly, &t.Agent, &perms, &created, &used, &exp); err != nil {
			return nil, err
		}
		t.CreatedAt, t.LastUsedAt, t.ExpiresAt = msTime(created), ptrTime(used), ptrTime(exp)
		if t.Permissions = parsePermissions(perms); t.Permissions != nil {
			limited = append(limited, t.ID)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	refusals, err := s.tokenRefusals(ctx, limited)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Refusals = refusals[out[i].ID]
	}
	return out, nil
}

// RevokeAPIToken deletes one of the actor's tokens.
func (s *Service) RevokeAPIToken(ctx context.Context, actor *Actor, id int64) error {
	if err := actor.canManageTokens(); err != nil {
		return err
	}
	t := APIToken{ID: id}
	err := s.db.W.QueryRowContext(ctx, `DELETE FROM api_tokens WHERE id = ? AND user_id = ? AND runner_job_id IS NULL RETURNING name, read_only, agent`, id, actor.id()).
		Scan(&t.Name, &t.ReadOnly, &t.Agent)
	if errors.Is(err, sql.ErrNoRows) {
		return notFound("token")
	} else if err != nil {
		return err
	}
	s.audit(ctx, actor, tokenAudit("token.revoke", &t))
	return nil
}

// TokenActor authenticates an API token.
func (s *Service) TokenActor(ctx context.Context, tok string) (*Actor, error) {
	return s.tokenActor(ctx, hashToken(tok), 0, true)
}

// tokenActor is the one way an API token becomes an actor, for a request
// carrying it (touch: it was just used) and for the server finishing what a
// session shipped with it (WP-1370): the same expiry and disabled-user
// checks, the same read-only, permissions and runner-job scope. col is the
// hash it is found by, else its id.
func (s *Service) tokenActor(ctx context.Context, hash string, tokenID int64, touch bool) (*Actor, error) {
	var id, uid int64
	var ro bool
	var agent string
	var exp, used sql.NullInt64
	var perms sql.NullString
	var name string
	var job sql.NullInt64
	const cols = `SELECT id, user_id, name, read_only, agent, permissions, expires_at, last_used_at, runner_job_id FROM api_tokens WHERE `
	q, key := cols+`token_hash = ?`, any(hash)
	if hash == "" {
		q, key = cols+`id = ?`, tokenID
	}
	err := s.db.R.QueryRowContext(ctx, q, key).
		Scan(&id, &uid, &name, &ro, &agent, &perms, &exp, &used, &job)
	if err != nil {
		return nil, ErrUnauthorized
	}
	now := s.nowMS()
	if exp.Valid && exp.Int64 < now {
		return nil, ErrUnauthorized
	}
	u, err := s.userByID(ctx, s.db.R, uid)
	if err != nil || u.Disabled {
		return nil, ErrUnauthorized
	}
	if touch && (!used.Valid || now-used.Int64 > 60*1000) {
		if _, err := s.db.W.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, now, id); err != nil {
			s.log.WarnContext(ctx, "best-effort write failed", "err", err)
		}
	}
	ps := permissionSet(parsePermissions(perms))
	if ps != nil && agent == "" {
		agent = "token/" + name
	}
	a := &Actor{User: u, ReadOnly: ro, Agent: agent, Permissions: ps, TokenID: id, RunnerJob: job.Int64}
	if job.Valid {
		if a.scope, err = s.jobScope(ctx, job.Int64, u); err != nil {
			return nil, err
		}
		nu := *u
		nu.IsAdmin = false
		a.User = &nu
	}
	return a, nil
}

// UserLink is a one-time invite or password reset link.
type UserLink struct {
	Kind      string    `json:"kind"`
	Email     string    `json:"email,omitempty"`
	Username  string    `json:"username,omitempty"`
	IsAdmin   bool      `json:"is_admin"`
	ExpiresAt time.Time `json:"expires_at"`
	URL       string    `json:"url,omitempty"`
}

func (s *Service) linkURL(kind, tok string) string {
	return fmt.Sprintf("%s/%s/%s", s.BaseURL(), map[string]string{"invite": "invite", "reset": "reset"}[kind], tok)
}

// CreateInvite makes a single-use signup link. Only admins can invite:
// there is no public signup.
func (s *Service) CreateInvite(ctx context.Context, actor *Actor, email string, isAdmin bool, ttl time.Duration) (*UserLink, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	if !actor.isAdmin() {
		return nil, ErrForbidden
	}
	email = strings.TrimSpace(email)
	if email != "" {
		if err := validateEmail(email); err != nil {
			return nil, err
		}
	}
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	tok := newToken("")
	now := s.nowMS()
	var by any
	if actor.id() != 0 {
		by = actor.id()
	}
	_, err := s.db.W.ExecContext(ctx, `INSERT INTO user_links(token_hash, kind, email, is_admin, created_by, created_at, expires_at)
		VALUES (?, 'invite', ?, ?, ?, ?, ?)`, hashToken(tok), email, boolInt(isAdmin), by, now, now+ttl.Milliseconds())
	if err != nil {
		return nil, err
	}
	detail := "invited " + cmp(email, "someone (an open link)")
	if isAdmin {
		detail += " as an admin"
	}
	s.audit(ctx, actor, AuditEntry{Action: "invite.create", Category: AuditAccess, TargetType: "invite", Target: cmp(email, "an invite link"), Detail: detail})
	return &UserLink{Kind: "invite", Email: email, IsAdmin: isAdmin, ExpiresAt: msTime(now + ttl.Milliseconds()), URL: s.linkURL("invite", tok)}, nil
}

// CreateResetLink makes a password reset link for a user (admin only, or
// the CLI via SystemActor).
func (s *Service) CreateResetLink(ctx context.Context, actor *Actor, userID int64) (*UserLink, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	if !actor.isAdmin() {
		return nil, ErrForbidden
	}
	u, err := s.userByID(ctx, s.db.R, userID)
	if err != nil {
		return nil, err
	}
	tok := newToken("")
	now := s.nowMS()
	ttl := 48 * time.Hour
	var by any
	if actor.id() != 0 {
		by = actor.id()
	}
	_, err = s.db.W.ExecContext(ctx, `INSERT INTO user_links(token_hash, kind, user_id, created_by, created_at, expires_at)
		VALUES (?, 'reset', ?, ?, ?, ?)`, hashToken(tok), u.ID, by, now, now+ttl.Milliseconds())
	if err != nil {
		return nil, err
	}
	s.audit(ctx, actor, AuditEntry{Action: "user.reset_link", Category: AuditAccess, TargetType: "user", TargetID: u.ID, Target: u.Name(),
		Detail: "made a password reset link for " + u.Name()})
	return &UserLink{Kind: "reset", Username: u.Username, ExpiresAt: msTime(now + ttl.Milliseconds()), URL: s.linkURL("reset", tok)}, nil
}

// PendingInvite is an unused invite, for the admin dashboard.
type PendingInvite struct {
	Email     string    `json:"email"`
	IsAdmin   bool      `json:"is_admin"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	ID        string    `json:"id"` // token hash prefix, for revoking
}

// ListInvites returns unused, unexpired invites.
func (s *Service) ListInvites(ctx context.Context, actor *Actor) ([]PendingInvite, error) {
	if !actor.isAdmin() {
		return nil, ErrForbidden
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT token_hash, email, is_admin, created_at, expires_at FROM user_links
		WHERE kind = 'invite' AND used_at IS NULL AND expires_at > ? ORDER BY created_at DESC`, s.nowMS())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PendingInvite{}
	for rows.Next() {
		var p PendingInvite
		var h string
		var c, e int64
		if err := rows.Scan(&h, &p.Email, &p.IsAdmin, &c, &e); err != nil {
			return nil, err
		}
		p.ID, p.CreatedAt, p.ExpiresAt = h[:16], msTime(c), msTime(e)
		out = append(out, p)
	}
	return out, rows.Err()
}

// RevokeInvite deletes a pending invite by the ID from ListInvites.
func (s *Service) RevokeInvite(ctx context.Context, actor *Actor, id string) error {
	if err := actor.canWrite(); err != nil {
		return err
	}
	if !actor.isAdmin() {
		return ErrForbidden
	}
	if len(id) != 16 {
		return notFound("invite")
	}
	var email string
	err := s.db.W.QueryRowContext(ctx, `DELETE FROM user_links WHERE kind = 'invite' AND used_at IS NULL AND substr(token_hash, 1, 16) = ? RETURNING email`, id).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		return notFound("invite")
	} else if err != nil {
		return err
	}
	s.audit(ctx, actor, AuditEntry{Action: "invite.revoke", Category: AuditAccess, TargetType: "invite", Target: cmp(email, "an invite link"),
		Detail: "revoked the invite for " + cmp(email, "an open link")})
	return nil
}

func (s *Service) loadLink(ctx context.Context, q queryer, tok string) (kind string, userID sql.NullInt64, email string, isAdmin bool, err error) {
	var exp int64
	var used sql.NullInt64
	err = q.QueryRowContext(ctx, `SELECT kind, user_id, email, is_admin, expires_at, used_at FROM user_links WHERE token_hash = ?`, hashToken(tok)).
		Scan(&kind, &userID, &email, &isAdmin, &exp, &used)
	if err != nil {
		return "", userID, "", false, notFound("link")
	}
	if used.Valid || exp < s.nowMS() {
		return "", userID, "", false, invalid("this link has expired or was already used")
	}
	return
}

// GetLink describes a link so the UI can show the right form.
func (s *Service) GetLink(ctx context.Context, tok string) (*UserLink, error) {
	kind, uid, email, isAdmin, err := s.loadLink(ctx, s.db.R, tok)
	if err != nil {
		return nil, err
	}
	l := &UserLink{Kind: kind, Email: email, IsAdmin: isAdmin}
	if uid.Valid {
		if u, err := s.userByID(ctx, s.db.R, uid.Int64); err == nil {
			l.Username = u.Username
		}
	}
	return l, nil
}

// AcceptInviteInput is the signup form.
type AcceptInviteInput struct {
	Username    string `json:"username"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
}

// AcceptInvite creates the account for an invite and logs it in.
func (s *Service) AcceptInvite(ctx context.Context, tok string, in AcceptInviteInput, userAgent, ip string) (string, *User, error) {
	in.Username = strings.TrimSpace(in.Username)
	in.Email = strings.TrimSpace(in.Email)
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	if err := validateUsername(in.Username); err != nil {
		return "", nil, err
	}
	if err := validatePassword(in.Password); err != nil {
		return "", nil, err
	}
	var uid int64
	err := s.write(ctx, nil, func(t *tx) error {
		kind, _, email, isAdmin, err := s.loadLink(ctx, t, tok)
		if err != nil {
			return err
		}
		if kind != "invite" {
			return invalid("not an invite link")
		}
		if email != "" {
			in.Email = email
		}
		if err := validateEmail(in.Email); err != nil {
			return err
		}
		now := s.nowMS()
		res, err := t.ExecContext(ctx, `INSERT INTO users(username, email, display_name, password_hash, is_admin, calendar_token, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?)`, in.Username, in.Email, in.DisplayName, HashPassword(in.Password), boolInt(isAdmin), newToken(""), now, now)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return invalid("that username or email is already taken")
			}
			return err
		}
		uid, _ = res.LastInsertId()
		_, err = t.ExecContext(ctx, `UPDATE user_links SET used_at = ?, user_id = ? WHERE token_hash = ?`, now, uid, hashToken(tok))
		return err
	})
	if err != nil {
		return "", nil, err
	}
	u, err := s.userByID(ctx, s.db.W, uid)
	if err != nil {
		return "", nil, err
	}
	sess, err := s.createSession(ctx, uid, userAgent, ip)
	return sess, u, err
}

// ResetPassword sets a new password from a reset link, ending other sessions.
func (s *Service) ResetPassword(ctx context.Context, tok, password string) error {
	if err := validatePassword(password); err != nil {
		return err
	}
	return s.write(ctx, nil, func(t *tx) error {
		kind, uid, _, _, err := s.loadLink(ctx, t, tok)
		if err != nil {
			return err
		}
		if kind != "reset" || !uid.Valid {
			return invalid("not a password reset link")
		}
		now := s.nowMS()
		if _, err := t.ExecContext(ctx, `UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`, HashPassword(password), now, uid.Int64); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, uid.Int64); err != nil {
			return err
		}
		if err := dropPushSubscriptions(ctx, t, uid.Int64); err != nil {
			return err
		}
		_, err = t.ExecContext(ctx, `UPDATE user_links SET used_at = ? WHERE used_at IS NULL AND (token_hash = ? OR (user_id = ? AND kind = 'reset'))`,
			now, hashToken(tok), uid.Int64)
		return err
	})
}

// ChangePassword changes the actor's own password.
func (s *Service) ChangePassword(ctx context.Context, actor *Actor, oldPW, newPW string) error {
	if err := actor.canWrite(); err != nil {
		return err
	}
	u, err := s.userByID(ctx, s.db.R, actor.id())
	if err != nil {
		return err
	}
	if !CheckPassword(u.passwordHash, oldPW) {
		return invalid("current password is incorrect")
	}
	if err := validatePassword(newPW); err != nil {
		return err
	}
	now := s.nowMS()
	return s.write(ctx, actor, func(t *tx) error {
		if _, err := t.ExecContext(ctx, `UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`, HashPassword(newPW), now, u.ID); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND token_hash != ?`, u.ID, actor.session); err != nil {
			return err
		}
		if err := dropPushSubscriptions(ctx, t, u.ID); err != nil {
			return err
		}
		_, err := t.ExecContext(ctx, `UPDATE user_links SET used_at = ? WHERE user_id = ? AND kind = 'reset' AND used_at IS NULL`, now, u.ID)
		return err
	})
}

// Session is a signed-in browser.
type Session struct {
	ID         string    `json:"id"` // a prefix of the token hash; not usable to sign in
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	UserAgent  string    `json:"user_agent"`
	IP         string    `json:"ip"`
	Current    bool      `json:"current"`
}

const sessionIDLen = 16

// ListSessions returns the actor's active sessions, most recent first.
func (s *Service) ListSessions(ctx context.Context, actor *Actor) ([]Session, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT token_hash, created_at, last_seen_at, user_agent, ip FROM sessions
		WHERE user_id = ? AND expires_at > ? ORDER BY last_seen_at DESC`, actor.id(), s.nowMS())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		var h string
		var c, l int64
		var se Session
		if err := rows.Scan(&h, &c, &l, &se.UserAgent, &se.IP); err != nil {
			return nil, err
		}
		se.ID, se.CreatedAt, se.LastSeenAt, se.Current = h[:sessionIDLen], msTime(c), msTime(l), h == actor.session
		out = append(out, se)
	}
	return out, rows.Err()
}

// RevokeSession signs out one of the actor's sessions, or with id "others"
// every session except the current one.
func (s *Service) RevokeSession(ctx context.Context, actor *Actor, id string) error {
	if err := actor.canWrite(); err != nil {
		return err
	}
	if id == "others" {
		if _, err := s.db.W.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND token_hash != ?`, actor.id(), actor.session); err != nil {
			return err
		}
		return dropPushSubscriptions(ctx, s.db.W, actor.id())
	}
	if len(id) != sessionIDLen {
		return notFound("session")
	}
	res, err := s.db.W.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND substr(token_hash, 1, ?) = ?`, actor.id(), sessionIDLen, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notFound("session")
	}
	return nil
}

// CreateUserInput is used by admins and the CLI to create an account directly.
type CreateUserInput struct {
	Username    string `json:"username"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
	IsAdmin     bool   `json:"is_admin"`
}

// CreateUser creates an account directly (admin only).
func (s *Service) CreateUser(ctx context.Context, actor *Actor, in CreateUserInput) (*User, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	if !actor.isAdmin() {
		return nil, ErrForbidden
	}
	if err := validateUsername(in.Username); err != nil {
		return nil, err
	}
	if err := validateEmail(in.Email); err != nil {
		return nil, err
	}
	hash := ""
	if in.Password != "" {
		if err := validatePassword(in.Password); err != nil {
			return nil, err
		}
		hash = HashPassword(in.Password)
	}
	now := s.nowMS()
	res, err := s.db.W.ExecContext(ctx, `INSERT INTO users(username, email, display_name, password_hash, is_admin, calendar_token, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)`, in.Username, in.Email, strings.TrimSpace(in.DisplayName), hash, boolInt(in.IsAdmin), newToken(""), now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, invalid("that username or email is already taken")
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	detail := "created the account " + in.Username
	if in.IsAdmin {
		detail += ", an admin"
	}
	s.audit(ctx, actor, AuditEntry{Action: "user.create", Category: AuditAccess, TargetType: "user", TargetID: id, Target: in.Username, Detail: detail})
	return s.userByID(ctx, s.db.W, id)
}

// UserPatch updates a user. Admin-only fields are ignored for non-admins.
type UserPatch struct {
	Username    *string    `json:"username"`
	Email       *string    `json:"email"`
	DisplayName *string    `json:"display_name"`
	Timezone    *string    `json:"timezone"`
	Prefs       *UserPrefs `json:"prefs"`
	IsAdmin     *bool      `json:"is_admin"`
	Disabled    *bool      `json:"disabled"`
}

// UpdateUser edits a user: themself, or anyone when the actor is an admin.
func (s *Service) UpdateUser(ctx context.Context, actor *Actor, id int64, p UserPatch) (*User, error) {
	if err := actor.canWrite(); err != nil {
		return nil, err
	}
	if actor.id() != id && !actor.isAdmin() {
		return nil, ErrForbidden
	}
	u, err := s.userByID(ctx, s.db.R, id)
	if err != nil {
		return nil, err
	}
	sets, args := []string{}, []any{}
	if p.Username != nil {
		if !actor.isAdmin() {
			return nil, invalid("only an admin can change a username")
		}
		if err := validateUsername(*p.Username); err != nil {
			return nil, err
		}
		sets, args = append(sets, "username = ?"), append(args, *p.Username)
	}
	if p.Email != nil {
		if err := validateEmail(*p.Email); err != nil {
			return nil, err
		}
		sets, args = append(sets, "email = ?"), append(args, *p.Email)
	}
	if p.DisplayName != nil {
		sets, args = append(sets, "display_name = ?"), append(args, strings.TrimSpace(*p.DisplayName))
	}
	if p.Timezone != nil {
		if _, err := time.LoadLocation(*p.Timezone); err != nil {
			return nil, invalid("unknown timezone %q", *p.Timezone)
		}
		sets, args = append(sets, "timezone = ?"), append(args, *p.Timezone)
	}
	if p.Prefs != nil {
		if actor.id() != id {
			return nil, invalid("preferences can only be changed by their owner")
		}
		next := *p.Prefs
		if next.GotifyURL != "" {
			if u, err := url.Parse(next.GotifyURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return nil, invalid("Gotify URL must be like https://gotify.example.com")
			}
		}
		if next.Theme != "" && next.Theme != "light" && next.Theme != "dark" {
			return nil, invalid("theme must be light, dark or empty")
		}
		if next.WeekStart != 0 && next.WeekStart != 1 {
			return nil, invalid("week_start must be 0 (Sunday) or 1 (Monday)")
		}
		if next.QuotaJump != nil && (*next.QuotaJump < 0 || *next.QuotaJump > 100) {
			return nil, invalid("quota_jump must be 0 (off) to 100")
		}
		if len(next.HiddenHelp) > 200 {
			return nil, invalid("too many hidden help panels")
		}
		if len(next.OpenHelp) > 200 {
			return nil, invalid("too many open help panels")
		}
		for _, h := range append(append([]string{}, next.HiddenHelp...), next.OpenHelp...) {
			if h == "" || len(h) > 64 {
				return nil, invalid("help ids are 1 to 64 characters")
			}
		}
		if n := len(next.DismissedWarnings); n > maxDismissedWarnings {
			next.DismissedWarnings = next.DismissedWarnings[n-maxDismissedWarnings:]
		}
		for _, w := range next.DismissedWarnings {
			if n := utf8.RuneCountInString(w); n == 0 || n > 200 {
				return nil, invalid("a dismissed warning is 1 to 200 characters")
			}
		}
		if next.ProjectSort != "" && next.ProjectSort != "recent" && next.ProjectSort != "custom" {
			return nil, invalid("project_sort must be recent, custom or empty")
		}
		if next.CacheWarm != "" && next.CacheWarm != CacheWarmCompact {
			return nil, invalid("cache_warm must be compact or empty")
		}
		var err error
		if next.Sessions, err = next.Sessions.validate(); err != nil {
			return nil, err
		}
		if len(next.ProjectOrder) > maxProjectOrder {
			return nil, invalid("too many projects in the custom order")
		}
		seen := make(map[int64]bool, len(next.ProjectOrder))
		for _, id := range next.ProjectOrder {
			if id <= 0 || seen[id] {
				return nil, invalid("project_order lists each project id once")
			}
			seen[id] = true
		}
		if next.GotifyToken == gotifyMask {
			next.GotifyToken = u.Prefs.GotifyToken
		}
		b, err := json.Marshal(storedPrefs(next))
		if err != nil {
			return nil, err
		}
		sets, args = append(sets, "prefs = ?"), append(args, string(b))
	}
	if p.IsAdmin != nil || p.Disabled != nil {
		if !actor.isAdmin() {
			return nil, ErrForbidden
		}
		if actor.id() == id && ((p.IsAdmin != nil && !*p.IsAdmin) || (p.Disabled != nil && *p.Disabled)) {
			return nil, invalid("you cannot remove your own admin access or disable yourself")
		}
		if p.IsAdmin != nil {
			sets, args = append(sets, "is_admin = ?"), append(args, boolInt(*p.IsAdmin))
		}
		if p.Disabled != nil {
			sets, args = append(sets, "disabled = ?"), append(args, boolInt(*p.Disabled))
		}
	}
	if len(sets) > 0 {
		sets, args = append(sets, "updated_at = ?"), append(args, s.nowMS(), u.ID)
		// #nosec G202 -- only fixed column names and ? placeholders are joined in; values are bound
		if _, err := s.db.W.ExecContext(ctx, `UPDATE users SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return nil, invalid("that username or email is already taken")
			}
			return nil, err
		}
		if p.Disabled != nil && *p.Disabled {
			if _, err := s.db.W.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, u.ID); err != nil {
				return nil, err
			}
		}
		var changes []string
		if p.IsAdmin != nil && *p.IsAdmin != u.IsAdmin {
			changes = append(changes, map[bool]string{true: "made " + u.Name() + " an admin", false: "removed " + u.Name() + "'s admin access"}[*p.IsAdmin])
		}
		if p.Disabled != nil && *p.Disabled != u.Disabled {
			changes = append(changes, map[bool]string{true: "disabled " + u.Name(), false: "re-enabled " + u.Name()}[*p.Disabled])
		}
		if p.Username != nil && *p.Username != u.Username && actor.id() != u.ID {
			changes = append(changes, "renamed "+u.Username+" to "+*p.Username)
		}
		if len(changes) > 0 {
			s.audit(ctx, actor, AuditEntry{Action: "user.update", Category: AuditAccess, TargetType: "user", TargetID: u.ID, Target: u.Name(),
				Detail: strings.Join(changes, ", ")})
		}
	}
	return s.userByID(ctx, s.db.W, id)
}

// DeleteUser removes an account (admin only). Their issues remain.
func (s *Service) DeleteUser(ctx context.Context, actor *Actor, id int64) error {
	if err := actor.canWrite(); err != nil {
		return err
	}
	if !actor.isAdmin() {
		return ErrForbidden
	}
	if actor.id() == id {
		return invalid("you cannot delete yourself")
	}
	return s.write(ctx, actor, func(t *tx) error {
		if actor.id() != 0 {
			if _, err := t.ExecContext(ctx, `INSERT INTO project_members(project_id, user_id, role, created_at)
				SELECT m.project_id, ?, 'owner', ? FROM project_members m
				WHERE m.user_id = ? AND m.role = 'owner'
				  AND NOT EXISTS (SELECT 1 FROM project_members o WHERE o.project_id = m.project_id AND o.role = 'owner' AND o.user_id != m.user_id)
				ON CONFLICT(project_id, user_id) DO UPDATE SET role = 'owner'`, actor.id(), s.nowMS(), id); err != nil {
				return err
			}
		}
		var name string
		err := t.QueryRowContext(ctx, `DELETE FROM users WHERE id = ? RETURNING coalesce(nullif(display_name, ''), username)`, id).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return notFound("user")
		} else if err != nil {
			return err
		}
		detail := "deleted the account; their issues stay"
		if s.cfg.TranscriptDir != "" {
			detail += "; their archived transcripts are removed from the disk next"
		}
		t.audit(AuditEntry{Action: "user.delete", Category: AuditAccess, TargetType: "user", TargetID: id, Target: name, Detail: detail})
		t.emit(&Event{Kind: EvUserDeleted, UserID: id, AdminOnly: true, Data: map[string]any{"user": id, "name": name, "by": actor.id()}})
		return nil
	})
}

const maxAvatarBytes = 2 << 20

// SetAvatar stores an avatar image for a user (themself, or by an admin).
func (s *Service) SetAvatar(ctx context.Context, actor *Actor, id int64, data []byte) error {
	if err := actor.canWrite(); err != nil {
		return err
	}
	if actor.id() != id && !actor.isAdmin() {
		return ErrForbidden
	}
	if len(data) > maxAvatarBytes {
		return invalid("avatar must be under 2 MB")
	}
	ct := sniffImage(data)
	if ct == "" && len(data) > 0 {
		return invalid("avatar must be a PNG, JPEG, GIF or WebP image")
	}
	var blob any
	if len(data) > 0 {
		blob = data
	}
	_, err := s.db.W.ExecContext(ctx, `UPDATE users SET avatar = ?, avatar_type = ?,
		avatar_version = CASE WHEN ? IS NULL THEN 0 ELSE avatar_version + 1 END, updated_at = ? WHERE id = ?`,
		blob, ct, blob, s.nowMS(), id)
	return err
}

// Avatar returns a user's avatar image and content type.
func (s *Service) Avatar(ctx context.Context, id int64) ([]byte, string, error) {
	var data []byte
	var ct string
	err := s.db.R.QueryRowContext(ctx, `SELECT avatar, avatar_type FROM users WHERE id = ?`, id).Scan(&data, &ct)
	if err != nil || len(data) == 0 {
		return nil, "", notFound("avatar")
	}
	return data, ct, nil
}

func sniffImage(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg"
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return "image/gif"
	case len(b) > 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// UserCount returns how many accounts exist.
func (s *Service) UserCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

// BootstrapInvite returns an admin invite link when no users exist yet, so
// the first person to start Waypoint can create the admin account.
func (s *Service) BootstrapInvite(ctx context.Context) (*UserLink, error) {
	n, err := s.UserCount(ctx)
	if err != nil || n > 0 {
		return nil, err
	}
	return s.CreateInvite(ctx, SystemActor(), "", true, 24*time.Hour)
}

// CalendarToken returns (creating if needed) the secret for the user's ICS feed.
func (s *Service) CalendarToken(ctx context.Context, actor *Actor, rotate bool) (string, error) {
	var tok string
	if err := s.db.R.QueryRowContext(ctx, `SELECT calendar_token FROM users WHERE id = ?`, actor.id()).Scan(&tok); err != nil {
		return "", err
	}
	if tok == "" || rotate {
		if err := actor.canWrite(); err != nil {
			return "", err
		}
		tok = newToken("")
		if _, err := s.db.W.ExecContext(ctx, `UPDATE users SET calendar_token = ? WHERE id = ?`, tok, actor.id()); err != nil {
			return "", err
		}
	}
	return tok, nil
}

// userByCalendarToken authenticates an ICS feed request.
func (s *Service) userByCalendarToken(ctx context.Context, tok string) (*User, error) {
	if len(tok) < 20 {
		return nil, ErrUnauthorized
	}
	u, err := scanUser(s.db.R.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE calendar_token = ? AND disabled = 0`, tok))
	if err != nil {
		return nil, ErrUnauthorized
	}
	return u, nil
}
