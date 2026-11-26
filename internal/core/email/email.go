// Package email is the SMTP settings and sending. It imports the kernel only.
package email

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/core/kernel"
)

const settingsKey = "smtp"

// Ways to secure the connection to the SMTP server.
const (
	StartTLS = "starttls"
	TLS      = "tls"
	None     = "none"
)

const smtpTimeout = 30 * time.Second

// Settings are the server's SMTP settings.
type Settings struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Security string `json:"security"`
	Username string `json:"username"`
	// Password is write-only: GetEmailSettings leaves it empty and says
	// whether one is set in HasPassword.
	Password    string `json:"password,omitempty"`
	HasPassword bool   `json:"has_password"`
	// From is the sender, "Waypoint <waypoint@example.com>" or a bare address.
	From       string `json:"from"`
	Configured bool   `json:"configured"`
}

// SettingsInput changes the settings. An empty Host turns email off.
// An empty Password keeps the one set unless ClearPassword.
type SettingsInput struct {
	Host          string `json:"host"`
	Port          int    `json:"port"`
	Security      string `json:"security"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	ClearPassword bool   `json:"clear_password"`
	From          string `json:"from"`
}

// Mailer reads the settings and sends. It checks no access: other domains
// use it, and the API cannot reach it.
type Mailer struct{ k *kernel.Kernel }

// NewMailer returns a Mailer over k.
func NewMailer(k *kernel.Kernel) *Mailer { return &Mailer{k: k} }

// Settings reads the saved settings, the defaults when none are saved.
func (m *Mailer) Settings(ctx context.Context) (*Settings, error) {
	var v string
	err := m.k.DB.R.QueryRowContext(ctx, `SELECT value FROM server_settings WHERE key = ?`, settingsKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return &Settings{Security: StartTLS, Port: 587}, nil
	}
	if err != nil {
		return nil, err
	}
	var es Settings
	if err := json.Unmarshal([]byte(v), &es); err != nil {
		return nil, err
	}
	es.Configured = es.Host != "" && es.From != ""
	es.HasPassword = es.Password != ""
	return &es, nil
}

// Configured says whether email can be sent.
func (m *Mailer) Configured(ctx context.Context) bool {
	es, err := m.Settings(ctx)
	if err != nil {
		if kernel.ReadFault(err) {
			m.k.Log.WarnContext(ctx, "read email configured", "err", err)
		}
		return false
	}
	return es.Configured
}

// Service is the checked operations: every exported method takes an actor.
type Service struct{ m *Mailer }

// New returns the email operations over m.
func New(m *Mailer) *Service { return &Service{m: m} }

// GetEmailSettings shows the SMTP settings to an admin, without the password.
func (s *Service) GetEmailSettings(ctx context.Context, actor *kernel.Actor) (*Settings, error) {
	if !actor.IsAdmin() {
		return nil, kernel.Forbidden("only admins can see the email settings")
	}
	es, err := s.m.Settings(ctx)
	if err != nil {
		return nil, err
	}
	es.Password = ""
	return es, nil
}

// SetEmailSettings sets the SMTP settings (admins, people only).
func (s *Service) SetEmailSettings(ctx context.Context, actor *kernel.Actor, in SettingsInput) (*Settings, error) {
	if !actor.IsAdmin() {
		return nil, kernel.Forbidden("only admins can set up email")
	}
	if err := actor.RequireHuman("setting up email"); err != nil {
		return nil, err
	}
	old, err := s.m.Settings(ctx)
	if err != nil {
		return nil, err
	}
	es := Settings{
		Host:     strings.TrimSpace(in.Host),
		Port:     in.Port,
		Security: cmp.Or(strings.ToLower(strings.TrimSpace(in.Security)), StartTLS),
		Username: strings.TrimSpace(in.Username),
		Password: in.Password,
		From:     strings.TrimSpace(in.From),
	}
	if es.Password == "" && !in.ClearPassword {
		es.Password = old.Password
	}
	if es.Host != "" {
		if err := checkSettings(&es); err != nil {
			return nil, err
		}
	}
	err = s.m.k.Write(ctx, actor, func(t *kernel.Tx) error {
		detail := "turned email off"
		if es.Host == "" {
			_, err := t.ExecContext(ctx, `DELETE FROM server_settings WHERE key = ?`, settingsKey)
			if err != nil {
				return err
			}
		} else {
			b, err := json.Marshal(es) // #nosec G117 -- stored server-side, like AI provider keys; never returned
			if err != nil {
				return err
			}
			if _, err := t.ExecContext(ctx, `INSERT INTO server_settings(key, value) VALUES (?, ?)
				ON CONFLICT(key) DO UPDATE SET value = excluded.value`, settingsKey, string(b)); err != nil {
				return err
			}
			detail = fmt.Sprintf("sends through %s:%d (%s) from %s", es.Host, es.Port, es.Security, es.From)
		}
		t.Audit(kernel.AuditEntry{Action: "settings.email", Category: kernel.AuditSettings, TargetType: "setting",
			Target: "Email", URL: "/admin?tab=email", Detail: detail})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetEmailSettings(ctx, actor)
}

func checkSettings(es *Settings) error {
	if strings.ContainsAny(es.Host, " /:\r\n") {
		return kernel.Invalid("the SMTP host is a host name or address, without a port or scheme")
	}
	switch es.Security {
	case StartTLS, TLS, None:
	default:
		return kernel.Invalid("security is starttls, tls or none")
	}
	if es.Port == 0 {
		es.Port = map[string]int{StartTLS: 587, TLS: 465, None: 25}[es.Security]
	}
	if es.Port < 1 || es.Port > 65535 {
		return kernel.Invalid("the SMTP port is 1-65535")
	}
	if es.From == "" {
		return kernel.Invalid("say who the email is from")
	}
	if _, err := mail.ParseAddress(es.From); err != nil {
		return kernel.Invalid("the sender %q isn't an email address", es.From)
	}
	if strings.ContainsAny(es.Username+es.Password, "\r\n") {
		return kernel.Invalid("the SMTP login can't contain line breaks")
	}
	return nil
}

// SendTestEmail sends a test message to the admin asking, or to the
// address given, with the settings as saved.
func (s *Service) SendTestEmail(ctx context.Context, actor *kernel.Actor, to string) error {
	if !actor.IsAdmin() {
		return kernel.Forbidden("only admins can send a test email")
	}
	to = cmp.Or(strings.TrimSpace(to), actor.User.Email)
	if to == "" {
		return kernel.Invalid("your account has no email address: give one to send the test to")
	}
	es, err := s.m.Settings(ctx)
	if err != nil {
		return err
	}
	if !es.Configured {
		return kernel.Invalid("set up email first")
	}
	return s.m.Send(ctx, es, to, "Waypoint test email",
		"This is a test from Waypoint's Admin → Email. If you can read it, email works.\n", "")
}

// ErrOff: the email action ran with no SMTP settings.
var ErrOff = kernel.Invalid("email isn't set up: an admin sets it under Admin → Email")

// ParseRecipients reads a comma-separated list of addresses.
func ParseRecipients(v string) ([]string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	list, err := mail.ParseAddressList(v)
	if err != nil {
		return nil, kernel.Invalid("%q isn't a list of email addresses", v)
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.Address)
	}
	return out, nil
}

// emailMessage builds a plain-text message for one recipient.
func emailMessage(from, to, subject, body, link string, now time.Time) ([]byte, error) {
	if strings.ContainsAny(to, "\r\n") || strings.ContainsAny(from, "\r\n") {
		return nil, kernel.Invalid("bad sender or recipient")
	}
	subject = strings.Join(strings.Fields(subject), " ")
	if link != "" {
		body = strings.TrimRight(body, "\n") + "\n\n" + link + "\n"
	}
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	domain := "waypoint"
	if a, err := mail.ParseAddress(from); err == nil {
		if i := strings.LastIndexByte(a.Address, '@'); i >= 0 {
			domain = a.Address[i+1:]
		}
	}
	from, to = headerSafe(from), headerSafe(to)
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", hex.EncodeToString(id[:]), domain)
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\nAuto-Submitted: auto-generated\r\n\r\n")
	w := quotedprintable.NewWriter(&b)
	if _, err := w.Write([]byte(strings.ReplaceAll(body, "\n", "\r\n"))); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// headerSafe drops CR and LF so a value can't start a new header line.
func headerSafe(v string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(v)
}

// Send sends one message over SMTP.
func (m *Mailer) Send(ctx context.Context, es *Settings, to, subject, body, link string) error {
	from, err := mail.ParseAddress(es.From)
	if err != nil {
		return kernel.Invalid("the sender %q isn't an email address", es.From)
	}
	msg, err := emailMessage(es.From, to, subject, body, link, time.Now())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, smtpTimeout)
	defer cancel()
	addr := net.JoinHostPort(es.Host, strconv.Itoa(es.Port))
	tlsConf := &tls.Config{ServerName: es.Host, MinVersion: tls.VersionTLS12}
	var conn net.Conn
	if es.Security == TLS {
		conn, err = (&tls.Dialer{Config: tlsConf}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, es.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp: %w", err)
	}
	defer func() { _ = c.Close() }()
	if es.Security == StartTLS {
		if err := c.StartTLS(tlsConf); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	}
	if es.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", es.Username, es.Password, es.Host)); err != nil {
			return fmt.Errorf("smtp login: %w", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	rcpt, err := mail.ParseAddress(to)
	if err != nil {
		return kernel.Invalid("%q isn't an email address", to)
	}
	if err := c.Rcpt(rcpt.Address); err != nil {
		return fmt.Errorf("smtp: %s refused: %w", to, err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	return c.Quit()
}
