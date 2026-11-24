package core

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// The token is minted only when the CLI collects it, so no usable token sits in the database while the login waits.

// EvTokenCreated is a CLI token being handed out; it reaches only its owner.
const EvTokenCreated = "token.created"

// NotifyToken is the notification kind for a new CLI sign-in.
const NotifyToken = "token"

// DeviceLoginTTL is how long a code can be approved.
const DeviceLoginTTL = 10 * time.Minute

// DeviceLogin is what the CLI shows and polls with.
type DeviceLogin struct {
	DeviceCode string `json:"device_code"` // secret; only the CLI has it
	UserCode   string `json:"user_code"`   // shown to the person, e.g. KTBX-QWNR
	VerifyURL  string `json:"verification_url"`
	Interval   int    `json:"interval"` // seconds between polls
	ExpiresIn  int    `json:"expires_in"`
}

// DeviceRequest describes a pending login, for the approval page.
type DeviceRequest struct {
	UserCode  string    `json:"user_code"`
	Name      string    `json:"name"`
	Agent     string    `json:"agent,omitempty"`
	IP        string    `json:"ip"`         // where the login was started
	UserAgent string    `json:"user_agent"` // and with what
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

const userCodeLetters = "BCDFGHJKLMNPQRSTVWXZ"

func newUserCode() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, 0, 9)
	for i, c := range b {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, userCodeLetters[int(c)%len(userCodeLetters)])
	}
	return string(out), nil
}

func normalizeUserCode(c string) string {
	c = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(c), " ", ""))
	if len(c) == 8 && !strings.Contains(c, "-") {
		c = c[:4] + "-" + c[4:]
	}
	return c
}

// StartDeviceLogin begins a CLI login. Name describes the machine; agent,
// if set, makes the resulting token an agent token.
func (s *Service) StartDeviceLogin(ctx context.Context, name, agent, ip, userAgent string) (*DeviceLogin, error) {
	name, agent = truncateBytes(strings.TrimSpace(name), 80), strings.TrimSpace(agent)
	if name == "" {
		name = "wp"
	}
	if len(agent) > 100 {
		return nil, invalid("agent names can be at most 100 characters")
	}
	device := newToken("wpd_")
	user, err := newUserCode()
	if err != nil {
		return nil, err
	}
	// A few waiting logins per address, so one client can't crowd out the
	// rest. Counting and inserting is one statement, so a burst of
	// concurrent starts can't all pass the count.
	now := s.nowMS()
	res, err := s.db.W.ExecContext(ctx, `INSERT INTO device_logins(device_hash, user_code, name, agent, ip, user_agent, created_at, expires_at)
		SELECT ?,?,?,?,?,?,?,? WHERE
			(SELECT count(*) FROM device_logins WHERE expires_at > ? AND user_id IS NULL AND ip = ?) < 5 AND
			(SELECT count(*) FROM device_logins WHERE expires_at > ? AND user_id IS NULL) < 500`,
		hashToken(device), user, name, agent, ip, truncateBytes(userAgent, 200), now, now+DeviceLoginTTL.Milliseconds(),
		now, ip, now)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, invalid("too many logins are waiting; try again in a few minutes")
	}
	return &DeviceLogin{DeviceCode: device, UserCode: user, VerifyURL: s.BaseURL() + "/device?code=" + user,
		Interval: 2, ExpiresIn: int(DeviceLoginTTL.Seconds())}, nil
}

// DeviceLoginRequest shows a pending login to the person about to approve it.
func (s *Service) DeviceLoginRequest(ctx context.Context, actor *Actor, userCode string) (*DeviceRequest, error) {
	if actor == nil || actor.User == nil {
		return nil, ErrUnauthorized
	}
	var r DeviceRequest
	var created, exp int64
	err := s.db.R.QueryRowContext(ctx, `SELECT user_code, name, agent, ip, user_agent, created_at, expires_at FROM device_logins
		WHERE user_code = ? AND expires_at > ? AND user_id IS NULL`, normalizeUserCode(userCode), s.nowMS()).
		Scan(&r.UserCode, &r.Name, &r.Agent, &r.IP, &r.UserAgent, &created, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("login code (it may have expired)")
	}
	r.CreatedAt, r.ExpiresAt = msTime(created), msTime(exp)
	return &r, err
}

// approvedGrace is how long an approved login waits for the CLI to collect
// its token, even if the code itself is about to expire.
const approvedGrace = 2 * time.Minute

// ApproveDeviceLogin lets the waiting CLI sign in as the actor. Only the
// first approval counts; the token is made when the CLI collects it.
func (s *Service) ApproveDeviceLogin(ctx context.Context, actor *Actor, userCode string) error {
	if err := actor.requirePersonDirect("approving a sign-in"); err != nil {
		return err
	}
	if err := actor.canWrite(); err != nil {
		return err
	}
	if actor.User == nil || actor.id() == 0 {
		return ErrUnauthorized
	}
	now := s.nowMS()
	return s.write(ctx, actor, func(t *tx) error {
		var name, agent, ip string
		err := t.QueryRowContext(ctx, `UPDATE device_logins SET user_id = ?, expires_at = max(expires_at, ?)
			WHERE user_code = ? AND user_id IS NULL AND expires_at > ? RETURNING name, agent, ip`,
			actor.id(), now+approvedGrace.Milliseconds(), normalizeUserCode(userCode), now).Scan(&name, &agent, &ip)
		if errors.Is(err, sql.ErrNoRows) {
			return notFound("login code (it may have expired)")
		} else if err != nil {
			return err
		}
		detail := "signed in the wp CLI"
		if name != "" {
			detail += " on " + name
		}
		if ip != "" {
			detail += " from " + ip
		}
		if agent != "" {
			detail += ", for " + agent
		}
		t.audit(AuditEntry{Action: "device.approve", Category: AuditAccess, TargetType: "device", Target: cmp(name, "a device"),
			Detail: detail, About: agent})
		return nil
	})
}

// PollDeviceLogin returns a new token once the login is approved (exactly
// once: the login is gone afterwards), "" while it's pending, or an error
// once it has expired.
func (s *Service) PollDeviceLogin(ctx context.Context, deviceCode string) (string, error) {
	hash := hashToken(deviceCode)
	var token string
	err := s.write(ctx, nil, func(t *tx) error {
		var uid int64
		var name, agent, ip string
		var exp int64
		err := t.QueryRowContext(ctx, `SELECT coalesce(user_id, 0), name, agent, ip, expires_at FROM device_logins WHERE device_hash = ?`, hash).
			Scan(&uid, &name, &agent, &ip, &exp)
		if errors.Is(err, sql.ErrNoRows) {
			return notFound("login (it expired or was already used)")
		} else if err != nil {
			return err
		}
		if exp < s.nowMS() {
			return invalid("the login code expired; run `wp auth login` again")
		}
		if uid == 0 {
			return nil
		}
		// Reading and deleting share one write transaction, so two polls can't both collect the same login.
		if _, err := t.ExecContext(ctx, `DELETE FROM device_logins WHERE device_hash = ?`, hash); err != nil {
			return err
		}
		u, err := s.userByID(ctx, t, uid)
		if err != nil || u.Disabled {
			return notFound("login (the account that approved it is gone)")
		}
		tok, at, err := s.createToken(ctx, t, &Actor{User: u}, TokenSpec{Name: "wp · " + name, Agent: agent})
		if err != nil {
			return err
		}
		token = tok
		t.emit(&Event{Kind: EvTokenCreated, UserID: uid, New: at.Name, Data: map[string]any{"ip": ip, "agent": agent}})
		return nil
	})
	if err != nil {
		return "", err
	}
	return token, nil
}
