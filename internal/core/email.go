package core

import (
	"bytes"
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
)

// Email (WP-D14, WP-7): the server's SMTP settings, set under Admin → Email,
// and the email automation action, which stays out of the editor until
// they are set. The password is stored like AI provider keys and never
// returned.

const emailSettingsKey = "smtp"

// Ways to secure the connection to the SMTP server.
const (
	SMTPStartTLS = "starttls" // plain connection upgraded with STARTTLS (port 587)
	SMTPTLS      = "tls"      // TLS from the start (port 465)
	SMTPNone     = "none"     // no encryption: only for a relay on the same host or network
)

const smtpTimeout = 30 * time.Second

// EmailSettings are the server's SMTP settings.
type EmailSettings struct {
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

// EmailSettingsInput changes the settings. An empty Host turns email off.
// An empty Password keeps the one set unless ClearPassword.
type EmailSettingsInput struct {
	Host          string `json:"host"`
	Port          int    `json:"port"`
	Security      string `json:"security"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	ClearPassword bool   `json:"clear_password"`
	From          string `json:"from"`
}

func (s *Service) emailSettings(ctx context.Context) (*EmailSettings, error) {
	var v string
	err := s.db.R.QueryRowContext(ctx, `SELECT value FROM server_settings WHERE key = ?`, emailSettingsKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return &EmailSettings{Security: SMTPStartTLS, Port: 587}, nil
	}
	if err != nil {
		return nil, err
	}
	var es EmailSettings
	if err := json.Unmarshal([]byte(v), &es); err != nil {
		return nil, err
	}
	es.Configured = es.Host != "" && es.From != ""
	es.HasPassword = es.Password != ""
	return &es, nil
}

// EmailConfigured says whether email can be sent.
func (s *Service) EmailConfigured(ctx context.Context) bool {
	es, err := s.emailSettings(ctx)
	return err == nil && es.Configured
}

// GetEmailSettings shows the SMTP settings to an admin, without the password.
func (s *Service) GetEmailSettings(ctx context.Context, actor *Actor) (*EmailSettings, error) {
	if !actor.isAdmin() {
		return nil, forbidden("only admins can see the email settings")
	}
	es, err := s.emailSettings(ctx)
	if err != nil {
		return nil, err
	}
	es.Password = ""
	return es, nil
}

// SetEmailSettings sets the SMTP settings (admins, people only).
func (s *Service) SetEmailSettings(ctx context.Context, actor *Actor, in EmailSettingsInput) (*EmailSettings, error) {
	if !actor.isAdmin() {
		return nil, forbidden("only admins can set up email")
	}
	if err := actor.requireHuman("setting up email"); err != nil {
		return nil, err
	}
	old, err := s.emailSettings(ctx)
	if err != nil {
		return nil, err
	}
	es := EmailSettings{
		Host:     strings.TrimSpace(in.Host),
		Port:     in.Port,
		Security: cmp(strings.ToLower(strings.TrimSpace(in.Security)), SMTPStartTLS),
		Username: strings.TrimSpace(in.Username),
		Password: in.Password,
		From:     strings.TrimSpace(in.From),
	}
	if es.Password == "" && !in.ClearPassword {
		es.Password = old.Password
	}
	if es.Host != "" {
		if err := checkEmailSettings(&es); err != nil {
			return nil, err
		}
	}
	err = s.write(ctx, actor, func(t *tx) error {
		detail := "turned email off"
		if es.Host == "" {
			_, err := t.ExecContext(ctx, `DELETE FROM server_settings WHERE key = ?`, emailSettingsKey)
			if err != nil {
				return err
			}
		} else {
			b, err := json.Marshal(es) // #nosec G117 -- stored server-side, like AI provider keys; never returned
			if err != nil {
				return err
			}
			if _, err := t.ExecContext(ctx, `INSERT INTO server_settings(key, value) VALUES (?, ?)
				ON CONFLICT(key) DO UPDATE SET value = excluded.value`, emailSettingsKey, string(b)); err != nil {
				return err
			}
			detail = fmt.Sprintf("sends through %s:%d (%s) from %s", es.Host, es.Port, es.Security, es.From)
		}
		t.audit(AuditEntry{Action: "settings.email", Category: AuditSettings, TargetType: "setting",
			Target: "Email", URL: "/admin?tab=email", Detail: detail})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetEmailSettings(ctx, actor)
}

func checkEmailSettings(es *EmailSettings) error {
	if strings.ContainsAny(es.Host, " /:\r\n") {
		return invalid("the SMTP host is a host name or address, without a port or scheme")
	}
	switch es.Security {
	case SMTPStartTLS, SMTPTLS, SMTPNone:
	default:
		return invalid("security is starttls, tls or none")
	}
	if es.Port == 0 {
		es.Port = map[string]int{SMTPStartTLS: 587, SMTPTLS: 465, SMTPNone: 25}[es.Security]
	}
	if es.Port < 1 || es.Port > 65535 {
		return invalid("the SMTP port is 1-65535")
	}
	if es.From == "" {
		return invalid("say who the email is from")
	}
	if _, err := mail.ParseAddress(es.From); err != nil {
		return invalid("the sender %q isn't an email address", es.From)
	}
	if strings.ContainsAny(es.Username+es.Password, "\r\n") {
		return invalid("the SMTP login can't contain line breaks")
	}
	return nil
}

// SendTestEmail sends a test message to the admin asking, or to the
// address given, with the settings as saved.
func (s *Service) SendTestEmail(ctx context.Context, actor *Actor, to string) error {
	if !actor.isAdmin() {
		return forbidden("only admins can send a test email")
	}
	to = cmp(strings.TrimSpace(to), actor.User.Email)
	if to == "" {
		return invalid("your account has no email address: give one to send the test to")
	}
	es, err := s.emailSettings(ctx)
	if err != nil {
		return err
	}
	if !es.Configured {
		return invalid("set up email first")
	}
	return sendEmail(ctx, es, to, "Waypoint test email",
		"This is a test from Waypoint's Admin → Email. If you can read it, email works.\n", "")
}

// errEmailOff: the email action ran with no SMTP settings.
var errEmailOff = invalid("email isn't set up: an admin sets it under Admin → Email")

// parseRecipients reads a comma-separated list of addresses.
func parseRecipients(v string) ([]string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	list, err := mail.ParseAddressList(v)
	if err != nil {
		return nil, invalid("%q isn't a list of email addresses", v)
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
		return nil, invalid("bad sender or recipient")
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

// sendEmail sends one message over SMTP.
func sendEmail(ctx context.Context, es *EmailSettings, to, subject, body, link string) error {
	from, err := mail.ParseAddress(es.From)
	if err != nil {
		return invalid("the sender %q isn't an email address", es.From)
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
	if es.Security == SMTPTLS {
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
	if es.Security == SMTPStartTLS {
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
		return invalid("%q isn't an email address", to)
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
