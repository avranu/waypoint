// Package users is accounts, sessions, API tokens, invite and reset links,
// token refusals and the permission catalogue. It imports the kernel and leaf
// utility packages (lazyre), no other domain; what it needs of other domains
// arrives as hooks.
package users

import (
	"bytes"
	"cmp"
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

	"git.jmann.me/claude/waypoint/internal/core/kernel"
	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// EvUserDeleted is emitted when an account is deleted. Data: user (id). Seen
// by admins only; what belongs to the account outside the database goes in
// dispatch (the transcript archive's files, WP-1466).
const EvUserDeleted = "user.deleted"

// Options is what the operations read of the server's configuration.
type Options struct {
	// BaseURL is the server's public address, for invite and reset links. It
	// is read at each use; nil is "".
	BaseURL func() string
	// ArchivesTranscripts says whether transcripts are kept on disk; deleting
	// an account says their removal follows. Nil is no.
	ArchivesTranscripts func() bool
}

// Hooks are what the users domain needs of the domains above it. A nil hook
// is skipped. None is an event: each must run inside the operation that asks.
type Hooks struct {
	// CredentialsChanged runs in the transaction that changes a password or
	// signs out a user's other sessions. It is not an event because the push
	// subscriptions must go in the same transaction as the password: a
	// failure rolls the change back.
	CredentialsChanged func(ctx context.Context, t *kernel.Tx, userID int64) error
	// JobScope is the projects a runner job's token reaches, by role. It is
	// not an event because the actor being built needs the answer. When it is
	// nil, or answers nil, a job token's Scope is empty and non-nil: the token
	// reaches no project.
	JobScope func(ctx context.Context, jobID int64, u *kernel.User) (map[int64]int, error)
	// BeforeDelete runs in the transaction that deletes an account, before the
	// row goes: by is who deletes it. It is not called when no one (actor id
	// 0) deletes. It is not an event because it must run before the account's
	// memberships go with the row.
	BeforeDelete func(ctx context.Context, t *kernel.Tx, userID, by int64) error
}

// Store is users, sessions, tokens, invite links and token refusals: sign-in
// (which has no actor yet) and what other domains need. It does not check who
// may see or change a user; token methods that take an actor still check that
// actor's token permissions.
type Store struct {
	k     *kernel.Kernel
	opt   Options
	hooks Hooks
}

// NewStore returns a Store over k.
func NewStore(k *kernel.Kernel, opt Options, hooks Hooks) *Store {
	return &Store{k: k, opt: opt, hooks: hooks}
}

// Service is the checked operations: every exported method takes an *Actor.
type Service struct{ st *Store }

// New returns the checked operations over st.
func New(st *Store) *Service { return &Service{st: st} }

// MaxDismissedWarnings bounds DismissedWarnings; the oldest go first.
const MaxDismissedWarnings = 500

// CacheWarmCompact: compact a session whose cache is to be kept warm,
// instead of pinging it.
const CacheWarmCompact = "compact"

// MaxProjectOrder bounds ProjectOrder; no one keeps more projects than this.
const MaxProjectOrder = 2000

// storedPrefs is UserPrefs as written to the database, token included.
type storedPrefs kernel.UserPrefs

func AvatarURL(id, version int64) string {
	if version == 0 {
		return ""
	}
	return fmt.Sprintf("/api/v1/users/%d/avatar?v=%d", id, version)
}

const userCols = `id, username, email, display_name, password_hash, is_admin, disabled, avatar_version, timezone, prefs, created_at`

func scanUser(sc interface{ Scan(...any) error }) (*kernel.User, error) {
	var u kernel.User
	var prefs string
	var created int64
	if err := sc.Scan(&u.ID, &u.Username, &u.Email, &u.DisplayName, &u.PasswordHash, &u.IsAdmin, &u.Disabled,
		&u.AvatarVersion, &u.Timezone, &prefs, &created); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(prefs), &u.Prefs)
	u.CreatedAt = kernel.MillisToTime(created)
	u.AvatarURL = AvatarURL(u.ID, u.AvatarVersion)
	if u.DisplayName == "" {
		u.DisplayName = u.Username
	}
	return &u, nil
}

// ByID loads a user through q: the database or a transaction.
func (s *Store) ByID(ctx context.Context, q kernel.Queryer, id int64) (*kernel.User, error) {
	u, err := scanUser(q.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.NotFound("user")
	}
	return u, err
}

// UserByID loads a user.
func (s *Store) UserByID(ctx context.Context, id int64) (*kernel.User, error) {
	return s.ByID(ctx, s.k.DB.R, id)
}

// ByName finds a user by username or email, or by id when name is a
// number (usernames can't be all digits, so the two never collide).
func (s *Store) ByName(ctx context.Context, q kernel.Queryer, name string) (*kernel.User, error) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "@")
	if id, err := strconv.ParseInt(name, 10, 64); err == nil {
		return s.ByID(ctx, q, id)
	}
	return s.ByLogin(ctx, q, name)
}

// ByLogin finds a user by username or email (both case-insensitive).
func (s *Store) ByLogin(ctx context.Context, q kernel.Queryer, login string) (*kernel.User, error) {
	u, err := scanUser(q.QueryRowContext(ctx, `SELECT `+userCols+` FROM users
		WHERE username = ? OR email = ? ORDER BY username = ? DESC LIMIT 1`, login, login, login))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, kernel.NotFound(fmt.Sprintf("user %q", login))
	}
	return u, err
}

// Resolve turns "me", a username, email or id into a user.
func (s *Store) Resolve(ctx context.Context, q kernel.Queryer, actor *kernel.Actor, ref string) (*kernel.User, error) {
	if strings.EqualFold(ref, "me") && actor != nil && actor.User != nil && actor.User.ID != 0 {
		return actor.User, nil
	}
	return s.ByName(ctx, q, ref)
}

// ListUsers returns every account. Non-admins get public fields only.
func (s *Service) ListUsers(ctx context.Context, actor *kernel.Actor) ([]*kernel.User, error) {
	if actor == nil || actor.User == nil {
		return nil, kernel.ErrUnauthorized
	}
	rows, err := s.st.k.DB.R.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY disabled, lower(display_name), username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*kernel.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		u.Prefs = kernel.UserPrefs{}
		if !actor.IsAdmin() && u.ID != actor.ID() {
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
		return kernel.Invalid("username must be 1-39 letters, digits, '.', '_' or '-', and not a reserved word")
	}
	if _, err := strconv.ParseInt(u, 10, 64); err == nil {
		return kernel.Invalid("username can't be only digits")
	}
	return nil
}

func validateEmail(e string) error {
	if _, err := mail.ParseAddress(e); err != nil || strings.ContainsAny(e, "<> ") {
		return kernel.Invalid("invalid email address")
	}
	return nil
}

func validatePassword(p string) error {
	if len(p) < 10 {
		return kernel.Invalid("password must be at least 10 characters")
	}
	if len(p) > 1024 {
		return kernel.Invalid("password is too long")
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

func NewToken(prefix string) string {
	b := make([]byte, 32)
	rand.Read(b)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func HashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// credentialsChanged runs the CredentialsChanged hook in t.
func (s *Store) credentialsChanged(ctx context.Context, t *kernel.Tx, userID int64) error {
	if s.hooks.CredentialsChanged == nil {
		return nil
	}
	return s.hooks.CredentialsChanged(ctx, t, userID)
}

const sessionTTL = 60 * 24 * time.Hour

// Login checks credentials and creates a session, returning its token.
func (s *Store) Login(ctx context.Context, login, password, userAgent, ip string) (string, *kernel.User, error) {
	u, err := s.ByLogin(ctx, s.k.DB.R, strings.TrimSpace(login))
	if err != nil {
		CheckPassword(dummyHash(), password)
		return "", nil, kernel.ErrUnauthorized
	}
	if !CheckPassword(u.PasswordHash, password) || u.Disabled {
		return "", nil, kernel.ErrUnauthorized
	}
	tok, err := s.createSession(ctx, u.ID, userAgent, ip)
	return tok, u, err
}

func (s *Store) createSession(ctx context.Context, userID int64, userAgent, ip string) (string, error) {
	tok := NewToken("wps_")
	now := s.k.NowMS()
	if len(userAgent) > 300 {
		userAgent = userAgent[:300]
	}
	_, err := s.k.DB.W.ExecContext(ctx, `INSERT INTO sessions(token_hash, user_id, created_at, last_seen_at, expires_at, user_agent, ip, reauth_at)
		VALUES (?,?,?,?,?,?,?,?)`, HashToken(tok), userID, now, now, now+sessionTTL.Milliseconds(), userAgent, ip, now)
	return tok, err
}

// SessionActor is SessionUser as an actor that knows its own session, so
// "sign out other sessions" can keep it.
func (s *Store) SessionActor(ctx context.Context, tok string) (*kernel.Actor, error) {
	u, err := s.SessionUser(ctx, tok)
	if err != nil {
		return nil, err
	}
	return &kernel.Actor{User: u, SessionHash: HashToken(tok)}, nil
}

// SessionUser returns the user for a session token, sliding its expiry.
func (s *Store) SessionUser(ctx context.Context, tok string) (*kernel.User, error) {
	h := HashToken(tok)
	var uid, lastSeen, expires int64
	err := s.k.DB.R.QueryRowContext(ctx, `SELECT user_id, last_seen_at, expires_at FROM sessions WHERE token_hash = ?`, h).Scan(&uid, &lastSeen, &expires)
	if err != nil {
		return nil, kernel.ErrUnauthorized
	}
	now := s.k.NowMS()
	if expires < now {
		return nil, kernel.ErrUnauthorized
	}
	u, err := s.ByID(ctx, s.k.DB.R, uid)
	if err != nil || u.Disabled {
		return nil, kernel.ErrUnauthorized
	}
	if now-lastSeen > 10*60*1000 {
		if _, err := s.k.DB.W.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE token_hash = ?`, now, now+sessionTTL.Milliseconds(), h); err != nil {
			s.k.Log.WarnContext(ctx, "best-effort write failed", "err", err)
		}
	}
	return u, nil
}

// Logout deletes a session.
func (s *Store) Logout(ctx context.Context, tok string) error {
	_, err := s.k.DB.W.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, HashToken(tok))
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
	Permissions []kernel.Permission `json:"permissions"`
	Refusals    []TokenRefusal      `json:"refusals,omitempty"`
	CreatedAt   time.Time           `json:"created_at"`
	LastUsedAt  *time.Time          `json:"last_used_at"`
	ExpiresAt   *time.Time          `json:"expires_at"`
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
	Permissions []kernel.Permission
}

// CreateAPIToken makes a token for the actor; the secret is returned once.
func (s *Service) CreateAPIToken(ctx context.Context, actor *kernel.Actor, name string, readOnly bool, ttl time.Duration) (string, *APIToken, error) {
	return s.CreateToken(ctx, actor, TokenSpec{Name: name, ReadOnly: readOnly, TTL: ttl})
}

// CreateToken makes a token for the actor; the secret is returned once.
func (s *Service) CreateToken(ctx context.Context, actor *kernel.Actor, spec TokenSpec) (string, *APIToken, error) {
	tok, t, err := s.st.CreateToken(ctx, s.st.k.DB.W, actor, spec)
	if err == nil {
		s.st.k.Audit(ctx, actor, tokenAudit("token.create", t))
	}
	return tok, t, err
}

// tokenAudit is a token change for the log: an agent token names its agent.
func tokenAudit(action string, t *APIToken) kernel.AuditEntry {
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
	return kernel.AuditEntry{Action: action, Category: kernel.AuditAccess, TargetType: "token", TargetID: t.ID, Target: t.Name, Detail: detail, About: t.Agent}
}

// CreateToken makes a token for the actor through q; the secret is returned
// once. It is the part of Service.CreateToken the device login shares.
func (s *Store) CreateToken(ctx context.Context, q kernel.Queryer, actor *kernel.Actor, spec TokenSpec) (string, *APIToken, error) {
	if err := actor.CanManageTokens(); err != nil {
		return "", nil, err
	}
	perms, err := normalizePermissions(spec.Permissions)
	if err != nil {
		return "", nil, err
	}
	if perms != nil && spec.ReadOnly {
		return "", nil, kernel.Invalid("a token is read-only or has permissions, not both")
	}
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return "", nil, kernel.Invalid("token name is required")
	}
	agent := strings.TrimSpace(spec.Agent)
	if len(agent) > 100 {
		return "", nil, kernel.Invalid("agent names can be at most 100 characters")
	}
	tok := NewToken("wpt_")
	now := s.k.NowMS()
	var exp any
	if spec.TTL > 0 {
		exp = now + spec.TTL.Milliseconds()
	}
	res, err := q.ExecContext(ctx, `INSERT INTO api_tokens(user_id, name, token_hash, prefix, read_only, agent, permissions, created_at, expires_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, actor.ID(), name, HashToken(tok), tok[:12], kernel.BoolInt(spec.ReadOnly), agent, PermissionsJSON(perms), now, exp)
	if err != nil {
		return "", nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return "", nil, err
	}
	t := &APIToken{ID: id, Name: name, Prefix: tok[:12], ReadOnly: spec.ReadOnly, Agent: agent, Permissions: perms, CreatedAt: kernel.MillisToTime(now)}
	if exp != nil {
		t.ExpiresAt = kernel.PtrTime(sql.NullInt64{Int64: exp.(int64), Valid: true})
	}
	return tok, t, nil
}

// ListAPITokens lists the actor's tokens.
func (s *Service) ListAPITokens(ctx context.Context, actor *kernel.Actor) ([]APIToken, error) {
	return s.st.Tokens(ctx, actor, 0)
}

// Token is one of the actor's tokens.
func (s *Store) Token(ctx context.Context, actor *kernel.Actor, id int64) (*APIToken, error) {
	list, err := s.Tokens(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, kernel.NotFound("token")
	}
	return &list[0], nil
}

// Tokens lists the actor's tokens, or just token id when it isn't 0.
func (s *Store) Tokens(ctx context.Context, actor *kernel.Actor, id int64) ([]APIToken, error) {
	if actor.Restricted() {
		return nil, actor.CanManageTokens()
	}
	rows, err := s.k.DB.R.QueryContext(ctx, `SELECT id, name, prefix, read_only, agent, permissions, created_at, last_used_at, expires_at
		FROM api_tokens WHERE user_id = ? AND (? = 0 OR id = ?) AND runner_job_id IS NULL ORDER BY id DESC`, actor.ID(), id, id)
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
		t.CreatedAt, t.LastUsedAt, t.ExpiresAt = kernel.MillisToTime(created), kernel.PtrTime(used), kernel.PtrTime(exp)
		if t.Permissions = ParsePermissions(perms); t.Permissions != nil {
			limited = append(limited, t.ID)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	refusals, err := s.TokenRefusals(ctx, limited)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Refusals = refusals[out[i].ID]
	}
	return out, nil
}

// RevokeAPIToken deletes one of the actor's tokens.
func (s *Service) RevokeAPIToken(ctx context.Context, actor *kernel.Actor, id int64) error {
	if err := actor.CanManageTokens(); err != nil {
		return err
	}
	t := APIToken{ID: id}
	err := s.st.k.DB.W.QueryRowContext(ctx, `DELETE FROM api_tokens WHERE id = ? AND user_id = ? AND runner_job_id IS NULL RETURNING name, read_only, agent`, id, actor.ID()).
		Scan(&t.Name, &t.ReadOnly, &t.Agent)
	if errors.Is(err, sql.ErrNoRows) {
		return kernel.NotFound("token")
	} else if err != nil {
		return err
	}
	s.st.k.Audit(ctx, actor, tokenAudit("token.revoke", &t))
	return nil
}

// TokenActor authenticates an API token.
func (s *Store) TokenActor(ctx context.Context, tok string) (*kernel.Actor, error) {
	return s.ActorFor(ctx, HashToken(tok), 0, true)
}

// ActorFor is the one way an API token becomes an actor, for a request
// carrying it (touch: it was just used) and for the server finishing what a
// session shipped with it (WP-1370): the same expiry and disabled-user
// checks, the same read-only, permissions and runner-job scope. The token is
// found by hash, or by tokenID when hash is empty.
func (s *Store) ActorFor(ctx context.Context, hash string, tokenID int64, touch bool) (*kernel.Actor, error) {
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
	err := s.k.DB.R.QueryRowContext(ctx, q, key).
		Scan(&id, &uid, &name, &ro, &agent, &perms, &exp, &used, &job)
	if err != nil {
		return nil, kernel.ErrUnauthorized
	}
	now := s.k.NowMS()
	if exp.Valid && exp.Int64 < now {
		return nil, kernel.ErrUnauthorized
	}
	u, err := s.ByID(ctx, s.k.DB.R, uid)
	if err != nil || u.Disabled {
		return nil, kernel.ErrUnauthorized
	}
	if touch && (!used.Valid || now-used.Int64 > 60*1000) {
		if _, err := s.k.DB.W.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, now, id); err != nil {
			s.k.Log.WarnContext(ctx, "best-effort write failed", "err", err)
		}
	}
	ps := NewPermissionSet(ParsePermissions(perms))
	if ps != nil && agent == "" {
		agent = "token/" + name
	}
	a := &kernel.Actor{User: u, ReadOnly: ro, Agent: agent, Permissions: ps, TokenID: id, RunnerJob: job.Int64}
	if job.Valid {
		if s.hooks.JobScope != nil {
			if a.Scope, err = s.hooks.JobScope(ctx, job.Int64, u); err != nil {
				return nil, err
			}
		}
		if a.Scope == nil { // a job token is never unscoped
			a.Scope = map[int64]int{}
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

// linkURL is the address of an "invite" or "reset" link.
func (s *Store) linkURL(kind, tok string) string {
	base := ""
	if s.opt.BaseURL != nil {
		base = s.opt.BaseURL()
	}
	return fmt.Sprintf("%s/%s/%s", strings.TrimRight(base, "/"), kind, tok)
}

// CreateInvite makes a single-use signup link. Only admins can invite:
// there is no public signup.
func (s *Service) CreateInvite(ctx context.Context, actor *kernel.Actor, email string, isAdmin bool, ttl time.Duration) (*UserLink, error) {
	if err := actor.CanWrite(); err != nil {
		return nil, err
	}
	if !actor.IsAdmin() {
		return nil, kernel.ErrForbidden
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
	tok := NewToken("")
	now := s.st.k.NowMS()
	var by any
	if actor.ID() != 0 {
		by = actor.ID()
	}
	_, err := s.st.k.DB.W.ExecContext(ctx, `INSERT INTO user_links(token_hash, kind, email, is_admin, created_by, created_at, expires_at)
		VALUES (?, 'invite', ?, ?, ?, ?, ?)`, HashToken(tok), email, kernel.BoolInt(isAdmin), by, now, now+ttl.Milliseconds())
	if err != nil {
		return nil, err
	}
	detail := "invited " + cmp.Or(email, "someone (an open link)")
	if isAdmin {
		detail += " as an admin"
	}
	s.st.k.Audit(ctx, actor, kernel.AuditEntry{Action: "invite.create", Category: kernel.AuditAccess, TargetType: "invite", Target: cmp.Or(email, "an invite link"), Detail: detail})
	return &UserLink{Kind: "invite", Email: email, IsAdmin: isAdmin, ExpiresAt: kernel.MillisToTime(now + ttl.Milliseconds()), URL: s.st.linkURL("invite", tok)}, nil
}

// CreateResetLink makes a password reset link for a user (admin only, or
// the CLI via SystemActor).
func (s *Service) CreateResetLink(ctx context.Context, actor *kernel.Actor, userID int64) (*UserLink, error) {
	if err := actor.CanWrite(); err != nil {
		return nil, err
	}
	if !actor.IsAdmin() {
		return nil, kernel.ErrForbidden
	}
	u, err := s.st.ByID(ctx, s.st.k.DB.R, userID)
	if err != nil {
		return nil, err
	}
	tok := NewToken("")
	now := s.st.k.NowMS()
	ttl := 48 * time.Hour
	var by any
	if actor.ID() != 0 {
		by = actor.ID()
	}
	_, err = s.st.k.DB.W.ExecContext(ctx, `INSERT INTO user_links(token_hash, kind, user_id, created_by, created_at, expires_at)
		VALUES (?, 'reset', ?, ?, ?, ?)`, HashToken(tok), u.ID, by, now, now+ttl.Milliseconds())
	if err != nil {
		return nil, err
	}
	s.st.k.Audit(ctx, actor, kernel.AuditEntry{Action: "user.reset_link", Category: kernel.AuditAccess, TargetType: "user", TargetID: u.ID, Target: u.Name(),
		Detail: "made a password reset link for " + u.Name()})
	return &UserLink{Kind: "reset", Username: u.Username, ExpiresAt: kernel.MillisToTime(now + ttl.Milliseconds()), URL: s.st.linkURL("reset", tok)}, nil
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
func (s *Service) ListInvites(ctx context.Context, actor *kernel.Actor) ([]PendingInvite, error) {
	if !actor.IsAdmin() {
		return nil, kernel.ErrForbidden
	}
	rows, err := s.st.k.DB.R.QueryContext(ctx, `SELECT token_hash, email, is_admin, created_at, expires_at FROM user_links
		WHERE kind = 'invite' AND used_at IS NULL AND expires_at > ? ORDER BY created_at DESC`, s.st.k.NowMS())
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
		p.ID, p.CreatedAt, p.ExpiresAt = h[:16], kernel.MillisToTime(c), kernel.MillisToTime(e)
		out = append(out, p)
	}
	return out, rows.Err()
}

// RevokeInvite deletes a pending invite by the ID from ListInvites.
func (s *Service) RevokeInvite(ctx context.Context, actor *kernel.Actor, id string) error {
	if err := actor.CanWrite(); err != nil {
		return err
	}
	if !actor.IsAdmin() {
		return kernel.ErrForbidden
	}
	if len(id) != 16 {
		return kernel.NotFound("invite")
	}
	var email string
	err := s.st.k.DB.W.QueryRowContext(ctx, `DELETE FROM user_links WHERE kind = 'invite' AND used_at IS NULL AND substr(token_hash, 1, 16) = ? RETURNING email`, id).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		return kernel.NotFound("invite")
	} else if err != nil {
		return err
	}
	s.st.k.Audit(ctx, actor, kernel.AuditEntry{Action: "invite.revoke", Category: kernel.AuditAccess, TargetType: "invite", Target: cmp.Or(email, "an invite link"),
		Detail: "revoked the invite for " + cmp.Or(email, "an open link")})
	return nil
}

func (s *Store) loadLink(ctx context.Context, q kernel.Queryer, tok string) (kind string, userID sql.NullInt64, email string, isAdmin bool, err error) {
	var exp int64
	var used sql.NullInt64
	err = q.QueryRowContext(ctx, `SELECT kind, user_id, email, is_admin, expires_at, used_at FROM user_links WHERE token_hash = ?`, HashToken(tok)).
		Scan(&kind, &userID, &email, &isAdmin, &exp, &used)
	if err != nil {
		return "", userID, "", false, kernel.NotFound("link")
	}
	if used.Valid || exp < s.k.NowMS() {
		return "", userID, "", false, kernel.Invalid("this link has expired or was already used")
	}
	return
}

// GetLink describes a link so the UI can show the right form.
func (s *Store) GetLink(ctx context.Context, tok string) (*UserLink, error) {
	kind, uid, email, isAdmin, err := s.loadLink(ctx, s.k.DB.R, tok)
	if err != nil {
		return nil, err
	}
	l := &UserLink{Kind: kind, Email: email, IsAdmin: isAdmin}
	if uid.Valid {
		if u, err := s.ByID(ctx, s.k.DB.R, uid.Int64); err == nil {
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
func (s *Store) AcceptInvite(ctx context.Context, tok string, in AcceptInviteInput, userAgent, ip string) (string, *kernel.User, error) {
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
	err := s.k.Write(ctx, nil, func(t *kernel.Tx) error {
		kind, _, email, isAdmin, err := s.loadLink(ctx, t, tok)
		if err != nil {
			return err
		}
		if kind != "invite" {
			return kernel.Invalid("not an invite link")
		}
		if email != "" {
			in.Email = email
		}
		if err := validateEmail(in.Email); err != nil {
			return err
		}
		now := s.k.NowMS()
		res, err := t.ExecContext(ctx, `INSERT INTO users(username, email, display_name, password_hash, is_admin, calendar_token, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?)`, in.Username, in.Email, in.DisplayName, HashPassword(in.Password), kernel.BoolInt(isAdmin), NewToken(""), now, now)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return kernel.Invalid("that username or email is already taken")
			}
			return err
		}
		uid, _ = res.LastInsertId()
		_, err = t.ExecContext(ctx, `UPDATE user_links SET used_at = ?, user_id = ? WHERE token_hash = ?`, now, uid, HashToken(tok))
		return err
	})
	if err != nil {
		return "", nil, err
	}
	u, err := s.ByID(ctx, s.k.DB.W, uid)
	if err != nil {
		return "", nil, err
	}
	sess, err := s.createSession(ctx, uid, userAgent, ip)
	return sess, u, err
}

// ResetPassword sets a new password from a reset link, ending other sessions.
func (s *Store) ResetPassword(ctx context.Context, tok, password string) error {
	if err := validatePassword(password); err != nil {
		return err
	}
	return s.k.Write(ctx, nil, func(t *kernel.Tx) error {
		kind, uid, _, _, err := s.loadLink(ctx, t, tok)
		if err != nil {
			return err
		}
		if kind != "reset" || !uid.Valid {
			return kernel.Invalid("not a password reset link")
		}
		now := s.k.NowMS()
		if _, err := t.ExecContext(ctx, `UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`, HashPassword(password), now, uid.Int64); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, uid.Int64); err != nil {
			return err
		}
		if err := s.credentialsChanged(ctx, t, uid.Int64); err != nil {
			return err
		}
		_, err = t.ExecContext(ctx, `UPDATE user_links SET used_at = ? WHERE used_at IS NULL AND (token_hash = ? OR (user_id = ? AND kind = 'reset'))`,
			now, HashToken(tok), uid.Int64)
		return err
	})
}

// ChangePassword changes the actor's own password.
func (s *Service) ChangePassword(ctx context.Context, actor *kernel.Actor, oldPW, newPW string) error {
	if err := actor.CanWrite(); err != nil {
		return err
	}
	u, err := s.st.ByID(ctx, s.st.k.DB.R, actor.ID())
	if err != nil {
		return err
	}
	if !CheckPassword(u.PasswordHash, oldPW) {
		return kernel.Invalid("current password is incorrect")
	}
	if err := validatePassword(newPW); err != nil {
		return err
	}
	now := s.st.k.NowMS()
	return s.st.k.Write(ctx, actor, func(t *kernel.Tx) error {
		if _, err := t.ExecContext(ctx, `UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`, HashPassword(newPW), now, u.ID); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND token_hash != ?`, u.ID, actor.SessionHash); err != nil {
			return err
		}
		if err := s.st.credentialsChanged(ctx, t, u.ID); err != nil {
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
func (s *Service) ListSessions(ctx context.Context, actor *kernel.Actor) ([]Session, error) {
	if actor == nil || actor.User == nil {
		return nil, kernel.ErrUnauthorized
	}
	rows, err := s.st.k.DB.R.QueryContext(ctx, `SELECT token_hash, created_at, last_seen_at, user_agent, ip FROM sessions
		WHERE user_id = ? AND expires_at > ? ORDER BY last_seen_at DESC`, actor.ID(), s.st.k.NowMS())
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
		se.ID, se.CreatedAt, se.LastSeenAt, se.Current = h[:sessionIDLen], kernel.MillisToTime(c), kernel.MillisToTime(l), h == actor.SessionHash
		out = append(out, se)
	}
	return out, rows.Err()
}

// RevokeSession signs out one of the actor's sessions, or with id "others"
// every session except the current one.
func (s *Service) RevokeSession(ctx context.Context, actor *kernel.Actor, id string) error {
	if err := actor.CanWrite(); err != nil {
		return err
	}
	if id == "others" {
		// One write, so the sessions and the push subscriptions go together.
		return s.st.k.Write(ctx, actor, func(t *kernel.Tx) error {
			if _, err := t.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND token_hash != ?`, actor.ID(), actor.SessionHash); err != nil {
				return err
			}
			return s.st.credentialsChanged(ctx, t, actor.ID())
		})
	}
	if len(id) != sessionIDLen {
		return kernel.NotFound("session")
	}
	res, err := s.st.k.DB.W.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND substr(token_hash, 1, ?) = ?`, actor.ID(), sessionIDLen, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return kernel.NotFound("session")
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
func (s *Service) CreateUser(ctx context.Context, actor *kernel.Actor, in CreateUserInput) (*kernel.User, error) {
	if err := actor.CanWrite(); err != nil {
		return nil, err
	}
	if !actor.IsAdmin() {
		return nil, kernel.ErrForbidden
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
	now := s.st.k.NowMS()
	res, err := s.st.k.DB.W.ExecContext(ctx, `INSERT INTO users(username, email, display_name, password_hash, is_admin, calendar_token, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)`, in.Username, in.Email, strings.TrimSpace(in.DisplayName), hash, kernel.BoolInt(in.IsAdmin), NewToken(""), now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, kernel.Invalid("that username or email is already taken")
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	detail := "created the account " + in.Username
	if in.IsAdmin {
		detail += ", an admin"
	}
	s.st.k.Audit(ctx, actor, kernel.AuditEntry{Action: "user.create", Category: kernel.AuditAccess, TargetType: "user", TargetID: id, Target: in.Username, Detail: detail})
	return s.st.ByID(ctx, s.st.k.DB.W, id)
}

// UserPatch updates a user. Admin-only fields are ignored for non-admins.
type UserPatch struct {
	Username    *string           `json:"username"`
	Email       *string           `json:"email"`
	DisplayName *string           `json:"display_name"`
	Timezone    *string           `json:"timezone"`
	Prefs       *kernel.UserPrefs `json:"prefs"`
	IsAdmin     *bool             `json:"is_admin"`
	Disabled    *bool             `json:"disabled"`
}

// UpdateUser edits a user: themself, or anyone when the actor is an admin.
func (s *Service) UpdateUser(ctx context.Context, actor *kernel.Actor, id int64, p UserPatch) (*kernel.User, error) {
	if err := actor.CanWrite(); err != nil {
		return nil, err
	}
	if actor.ID() != id && !actor.IsAdmin() {
		return nil, kernel.ErrForbidden
	}
	u, err := s.st.ByID(ctx, s.st.k.DB.R, id)
	if err != nil {
		return nil, err
	}
	sets, args := []string{}, []any{}
	if p.Username != nil {
		if !actor.IsAdmin() {
			return nil, kernel.Invalid("only an admin can change a username")
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
			return nil, kernel.Invalid("unknown timezone %q", *p.Timezone)
		}
		sets, args = append(sets, "timezone = ?"), append(args, *p.Timezone)
	}
	if p.Prefs != nil {
		if actor.ID() != id {
			return nil, kernel.Invalid("preferences can only be changed by their owner")
		}
		next := *p.Prefs
		if next.GotifyURL != "" {
			if u, err := url.Parse(next.GotifyURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return nil, kernel.Invalid("Gotify URL must be like https://gotify.example.com")
			}
		}
		if next.Theme != "" && next.Theme != "light" && next.Theme != "dark" {
			return nil, kernel.Invalid("theme must be light, dark or empty")
		}
		if next.WeekStart != 0 && next.WeekStart != 1 {
			return nil, kernel.Invalid("week_start must be 0 (Sunday) or 1 (Monday)")
		}
		if next.QuotaJump != nil && (*next.QuotaJump < 0 || *next.QuotaJump > 100) {
			return nil, kernel.Invalid("quota_jump must be 0 (off) to 100")
		}
		if len(next.HiddenHelp) > 200 {
			return nil, kernel.Invalid("too many hidden help panels")
		}
		if len(next.OpenHelp) > 200 {
			return nil, kernel.Invalid("too many open help panels")
		}
		for _, h := range append(append([]string{}, next.HiddenHelp...), next.OpenHelp...) {
			if h == "" || len(h) > 64 {
				return nil, kernel.Invalid("help ids are 1 to 64 characters")
			}
		}
		if n := len(next.DismissedWarnings); n > MaxDismissedWarnings {
			next.DismissedWarnings = next.DismissedWarnings[n-MaxDismissedWarnings:]
		}
		for _, w := range next.DismissedWarnings {
			if n := utf8.RuneCountInString(w); n == 0 || n > 200 {
				return nil, kernel.Invalid("a dismissed warning is 1 to 200 characters")
			}
		}
		if next.ProjectSort != "" && next.ProjectSort != "recent" && next.ProjectSort != "custom" {
			return nil, kernel.Invalid("project_sort must be recent, custom or empty")
		}
		if next.CacheWarm != "" && next.CacheWarm != CacheWarmCompact {
			return nil, kernel.Invalid("cache_warm must be compact or empty")
		}
		var err error
		if next.Sessions, err = kernel.ValidateSessionPrefs(next.Sessions); err != nil {
			return nil, err
		}
		if len(next.ProjectOrder) > MaxProjectOrder {
			return nil, kernel.Invalid("too many projects in the custom order")
		}
		seen := make(map[int64]bool, len(next.ProjectOrder))
		for _, id := range next.ProjectOrder {
			if id <= 0 || seen[id] {
				return nil, kernel.Invalid("project_order lists each project id once")
			}
			seen[id] = true
		}
		if next.GotifyToken == kernel.GotifyMask {
			next.GotifyToken = u.Prefs.GotifyToken
		}
		b, err := json.Marshal(storedPrefs(next))
		if err != nil {
			return nil, err
		}
		sets, args = append(sets, "prefs = ?"), append(args, string(b))
	}
	if p.IsAdmin != nil || p.Disabled != nil {
		if !actor.IsAdmin() {
			return nil, kernel.ErrForbidden
		}
		if actor.ID() == id && ((p.IsAdmin != nil && !*p.IsAdmin) || (p.Disabled != nil && *p.Disabled)) {
			return nil, kernel.Invalid("you cannot remove your own admin access or disable yourself")
		}
		if p.IsAdmin != nil {
			sets, args = append(sets, "is_admin = ?"), append(args, kernel.BoolInt(*p.IsAdmin))
		}
		if p.Disabled != nil {
			sets, args = append(sets, "disabled = ?"), append(args, kernel.BoolInt(*p.Disabled))
		}
	}
	if len(sets) > 0 {
		sets, args = append(sets, "updated_at = ?"), append(args, s.st.k.NowMS(), u.ID)
		// #nosec G202 -- only fixed column names and ? placeholders are joined in; values are bound
		if _, err := s.st.k.DB.W.ExecContext(ctx, `UPDATE users SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return nil, kernel.Invalid("that username or email is already taken")
			}
			return nil, err
		}
		if p.Disabled != nil && *p.Disabled {
			if _, err := s.st.k.DB.W.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, u.ID); err != nil {
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
		if p.Username != nil && *p.Username != u.Username && actor.ID() != u.ID {
			changes = append(changes, "renamed "+u.Username+" to "+*p.Username)
		}
		if len(changes) > 0 {
			s.st.k.Audit(ctx, actor, kernel.AuditEntry{Action: "user.update", Category: kernel.AuditAccess, TargetType: "user", TargetID: u.ID, Target: u.Name(),
				Detail: strings.Join(changes, ", ")})
		}
	}
	return s.st.ByID(ctx, s.st.k.DB.W, id)
}

// DeleteUser removes an account (admin only). Their issues remain.
func (s *Service) DeleteUser(ctx context.Context, actor *kernel.Actor, id int64) error {
	if err := actor.CanWrite(); err != nil {
		return err
	}
	if !actor.IsAdmin() {
		return kernel.ErrForbidden
	}
	if actor.ID() == id {
		return kernel.Invalid("you cannot delete yourself")
	}
	return s.st.k.Write(ctx, actor, func(t *kernel.Tx) error {
		if actor.ID() != 0 && s.st.hooks.BeforeDelete != nil {
			if err := s.st.hooks.BeforeDelete(ctx, t, id, actor.ID()); err != nil {
				return err
			}
		}
		var name string
		err := t.QueryRowContext(ctx, `DELETE FROM users WHERE id = ? RETURNING coalesce(nullif(display_name, ''), username)`, id).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return kernel.NotFound("user")
		} else if err != nil {
			return err
		}
		detail := "deleted the account; their issues stay"
		if f := s.st.opt.ArchivesTranscripts; f != nil && f() {
			detail += "; their archived transcripts are removed from the disk next"
		}
		t.Audit(kernel.AuditEntry{Action: "user.delete", Category: kernel.AuditAccess, TargetType: "user", TargetID: id, Target: name, Detail: detail})
		t.Emit(&kernel.Event{Kind: EvUserDeleted, UserID: id, AdminOnly: true, Data: map[string]any{"user": id, "name": name, "by": actor.ID()}})
		return nil
	})
}

const maxAvatarBytes = 2 << 20

// SetAvatar stores an avatar image for a user (themself, or by an admin).
func (s *Service) SetAvatar(ctx context.Context, actor *kernel.Actor, id int64, data []byte) error {
	if err := actor.CanWrite(); err != nil {
		return err
	}
	if actor.ID() != id && !actor.IsAdmin() {
		return kernel.ErrForbidden
	}
	if len(data) > maxAvatarBytes {
		return kernel.Invalid("avatar must be under 2 MB")
	}
	ct := sniffImage(data)
	if ct == "" && len(data) > 0 {
		return kernel.Invalid("avatar must be a PNG, JPEG, GIF or WebP image")
	}
	var blob any
	if len(data) > 0 {
		blob = data
	}
	_, err := s.st.k.DB.W.ExecContext(ctx, `UPDATE users SET avatar = ?, avatar_type = ?,
		avatar_version = CASE WHEN ? IS NULL THEN 0 ELSE avatar_version + 1 END, updated_at = ? WHERE id = ?`,
		blob, ct, blob, s.st.k.NowMS(), id)
	return err
}

// Avatar returns a user's avatar image and content type.
func (s *Store) Avatar(ctx context.Context, id int64) ([]byte, string, error) {
	var data []byte
	var ct string
	err := s.k.DB.R.QueryRowContext(ctx, `SELECT avatar, avatar_type FROM users WHERE id = ?`, id).Scan(&data, &ct)
	if err != nil || len(data) == 0 {
		return nil, "", kernel.NotFound("avatar")
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
func (s *Store) UserCount(ctx context.Context) (int, error) {
	var n int
	err := s.k.DB.R.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

// BootstrapInvite returns an admin invite link when no users exist yet, so
// the first person to start Waypoint can create the admin account.
func (s *Store) BootstrapInvite(ctx context.Context) (*UserLink, error) {
	n, err := s.UserCount(ctx)
	if err != nil || n > 0 {
		return nil, err
	}
	return New(s).CreateInvite(ctx, kernel.SystemActor(), "", true, 24*time.Hour)
}

// CalendarToken returns (creating if needed) the secret for the user's ICS feed.
func (s *Service) CalendarToken(ctx context.Context, actor *kernel.Actor, rotate bool) (string, error) {
	var tok string
	if err := s.st.k.DB.R.QueryRowContext(ctx, `SELECT calendar_token FROM users WHERE id = ?`, actor.ID()).Scan(&tok); err != nil {
		return "", err
	}
	if tok == "" || rotate {
		if err := actor.CanWrite(); err != nil {
			return "", err
		}
		tok = NewToken("")
		if _, err := s.st.k.DB.W.ExecContext(ctx, `UPDATE users SET calendar_token = ? WHERE id = ?`, tok, actor.ID()); err != nil {
			return "", err
		}
	}
	return tok, nil
}

// ByCalendarToken authenticates an ICS feed request.
func (s *Store) ByCalendarToken(ctx context.Context, tok string) (*kernel.User, error) {
	if len(tok) < 20 {
		return nil, kernel.ErrUnauthorized
	}
	u, err := scanUser(s.k.DB.R.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE calendar_token = ? AND disabled = 0`, tok))
	if err != nil {
		return nil, kernel.ErrUnauthorized
	}
	return u, nil
}
