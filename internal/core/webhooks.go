package core

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	automationRuns = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "waypoint_automation_runs_total", Help: "Automation runs by result.",
	}, []string{"result"})
	webhookDeliveries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "waypoint_webhook_deliveries_total", Help: "Outbound webhook deliveries by result.",
	}, []string{"result"})
	// A queued babysitting round taken back because the pull request's
	// head moved before a runner took it (WP-1618).
	babysitRoundsWithdrawn = promauto.NewCounter(prometheus.CounterOpts{
		Name: "waypoint_babysit_rounds_withdrawn_total", Help: "Queued babysitting rounds withdrawn because the pull request was pushed to before a runner took them.",
	})
	aiRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "waypoint_ai_requests_total", Help: "AI model requests by task and result.",
	}, []string{"task", "result"})
)

// Webhook sends events to a URL as JSON.
type Webhook struct {
	ID         int64     `json:"id"`
	ProjectID  *int64    `json:"project_id"`
	Name       string    `json:"name"`
	URL        string    `json:"url"`
	HasSecret  bool      `json:"has_secret"`
	Events     string    `json:"events"`
	Enabled    bool      `json:"enabled"`
	LastStatus *int64    `json:"last_status"`
	LastError  string    `json:"last_error"`
	CreatedAt  time.Time `json:"created_at"`
}

// WebhookInput creates or edits a webhook.
type WebhookInput struct {
	Name    *string `json:"name"`
	URL     *string `json:"url"`
	Secret  *string `json:"secret"`
	Events  *string `json:"events"` // comma separated kinds, "issue.*", or "*"
	Enabled *bool   `json:"enabled"`
}

const webhookCols = `id, project_id, name, url, secret != '', events, enabled, last_status, last_error, created_at`

func scanWebhook(sc interface{ Scan(...any) error }) (*Webhook, error) {
	var w Webhook
	var pid, status sql.NullInt64
	var c int64
	if err := sc.Scan(&w.ID, &pid, &w.Name, &w.URL, &w.HasSecret, &w.Events, &w.Enabled, &status, &w.LastError, &c); err != nil {
		return nil, err
	}
	w.ProjectID, w.LastStatus, w.CreatedAt = ptrInt(pid), ptrInt(status), msTime(c)
	return &w, nil
}

// webhookScope returns the project id for a ref; "" means global (admin only).
func (s *Service) webhookScope(ctx context.Context, actor *Actor, projectRef string) (*int64, error) {
	if projectRef == "" {
		if !actor.IsAdmin() {
			return nil, ErrForbidden
		}
		return nil, nil
	}
	p, err := s.projectByRef(ctx, s.db.R, projectRef)
	if err != nil {
		return nil, err
	}
	if err := s.requireProjectRole(ctx, s.db.R, actor, p.ID, RoleOwner); err != nil {
		return nil, err
	}
	return &p.ID, nil
}

// ListWebhooks lists a project's webhooks, or global ones for "".
func (s *Service) ListWebhooks(ctx context.Context, actor *Actor, projectRef string) ([]*Webhook, error) {
	pid, err := s.webhookScope(ctx, actor, projectRef)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+webhookCols+` FROM webhooks WHERE project_id IS ? ORDER BY id`, nullInt(pid))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Webhook{}
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func validHookURL(u string) bool {
	return strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")
}

// CreateWebhook adds a webhook.
func (s *Service) CreateWebhook(ctx context.Context, actor *Actor, projectRef string, in WebhookInput) (*Webhook, error) {
	if err := actor.CanWrite(); err != nil {
		return nil, err
	}
	pid, err := s.webhookScope(ctx, actor, projectRef)
	if err != nil {
		return nil, err
	}
	if in.URL == nil || !validHookURL(*in.URL) {
		return nil, invalid("webhook url must start with http:// or https://")
	}
	events := "*"
	if in.Events != nil && strings.TrimSpace(*in.Events) != "" {
		events = strings.TrimSpace(*in.Events)
	}
	name := deref(in.Name)
	if name == "" {
		name = *in.URL
	}
	res, err := s.db.W.ExecContext(ctx, `INSERT INTO webhooks(project_id, name, url, secret, events, enabled, created_at) VALUES (?,?,?,?,?,1,?)`,
		nullInt(pid), name, *in.URL, deref(in.Secret), events, s.nowMS())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return scanWebhook(s.db.W.QueryRowContext(ctx, `SELECT `+webhookCols+` FROM webhooks WHERE id = ?`, id))
}

func (s *Service) webhookFor(ctx context.Context, actor *Actor, id int64) (*Webhook, error) {
	w, err := scanWebhook(s.db.R.QueryRowContext(ctx, `SELECT `+webhookCols+` FROM webhooks WHERE id = ?`, id))
	if err != nil {
		return nil, notFound("webhook")
	}
	if w.ProjectID == nil {
		if !actor.IsAdmin() {
			return nil, notFound("webhook")
		}
	} else if err := s.requireProjectRole(ctx, s.db.R, actor, *w.ProjectID, RoleOwner); err != nil {
		return nil, err
	}
	return w, nil
}

// UpdateWebhook edits a webhook.
func (s *Service) UpdateWebhook(ctx context.Context, actor *Actor, id int64, in WebhookInput) (*Webhook, error) {
	if err := actor.CanWrite(); err != nil {
		return nil, err
	}
	if _, err := s.webhookFor(ctx, actor, id); err != nil {
		return nil, err
	}
	if in.URL != nil {
		if !validHookURL(*in.URL) {
			return nil, invalid("webhook url must start with http:// or https://")
		}
		if _, err := s.db.W.ExecContext(ctx, `UPDATE webhooks SET url = ? WHERE id = ?`, *in.URL, id); err != nil {
			return nil, err
		}
	}
	if in.Name != nil {
		if _, err := s.db.W.ExecContext(ctx, `UPDATE webhooks SET name = ? WHERE id = ?`, *in.Name, id); err != nil {
			return nil, err
		}
	}
	if in.Secret != nil {
		if _, err := s.db.W.ExecContext(ctx, `UPDATE webhooks SET secret = ? WHERE id = ?`, *in.Secret, id); err != nil {
			return nil, err
		}
	}
	if in.Events != nil {
		if _, err := s.db.W.ExecContext(ctx, `UPDATE webhooks SET events = ? WHERE id = ?`, cmp(strings.TrimSpace(*in.Events), "*"), id); err != nil {
			return nil, err
		}
	}
	if in.Enabled != nil {
		if _, err := s.db.W.ExecContext(ctx, `UPDATE webhooks SET enabled = ? WHERE id = ?`, boolInt(*in.Enabled), id); err != nil {
			return nil, err
		}
	}
	return scanWebhook(s.db.W.QueryRowContext(ctx, `SELECT `+webhookCols+` FROM webhooks WHERE id = ?`, id))
}

// DeleteWebhook removes a webhook.
func (s *Service) DeleteWebhook(ctx context.Context, actor *Actor, id int64) error {
	if err := actor.CanWrite(); err != nil {
		return err
	}
	if _, err := s.webhookFor(ctx, actor, id); err != nil {
		return err
	}
	_, err := s.db.W.ExecContext(ctx, `DELETE FROM webhooks WHERE id = ?`, id)
	return err
}

// TestWebhook sends a ping to a webhook and returns the HTTP status.
func (s *Service) TestWebhook(ctx context.Context, actor *Actor, id int64) (int, error) {
	if _, err := s.webhookFor(ctx, actor, id); err != nil {
		return 0, err
	}
	var url, secret string
	if err := s.db.R.QueryRowContext(ctx, `SELECT url, secret FROM webhooks WHERE id = ?`, id).Scan(&url, &secret); err != nil {
		return 0, err
	}
	body, _ := json.Marshal(map[string]any{"event": "ping", "at": s.now().UTC()})
	d := s.sendLogged(ctx, id, url, secret, "ping", body, 1, nil)
	return d.status, d.err
}

func eventMatches(filter, kind string) bool {
	for _, f := range strings.Split(filter, ",") {
		f = strings.TrimSpace(f)
		if f == "*" || f == kind || (strings.HasSuffix(f, ".*") && strings.HasPrefix(kind, strings.TrimSuffix(f, "*"))) {
			return true
		}
	}
	return false
}

// webhookPayload is the JSON body sent for an event.
func (s *Service) webhookPayload(e *Event, issue *Issue) map[string]any {
	p := map[string]any{
		"event": e.Kind,
		"at":    e.At,
		"actor": e.Actor,
	}
	if e.Field != "" || e.Old != "" || e.New != "" {
		p["change"] = map[string]any{"field": e.Field, "old": e.Old, "new": e.New}
	}
	if e.ViaAutomation != "" {
		p["via_automation"] = e.ViaAutomation
	}
	if e.SprintID != 0 {
		p["sprint_id"] = e.SprintID
	}
	if isEnvEvent(e.Kind) {
		p["data"] = e.Data
	}
	if issue != nil {
		p["issue"] = issue
		p["url"] = s.BaseURL() + "/issue/" + issue.Key
	} else if e.IssueKey != "" {
		p["issue"] = map[string]any{"key": e.IssueKey, "id": e.IssueID}
	}
	return p
}

// deliverWebhooks queues deliveries of a persisted event to matching webhooks.
func (s *Service) deliverWebhooks(ctx context.Context, e *Event) {
	if !persisted[e.Kind] && e.Kind != EvIssueDeleted && e.Kind != EvIssueChildrenDone && e.Kind != EvIssueReopened && !isEnvEvent(e.Kind) {
		return
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, url, secret, events FROM webhooks WHERE enabled = 1 AND (project_id IS NULL OR project_id = ?)`, e.ProjectID)
	if err != nil {
		return
	}
	type hook struct {
		id                  int64
		url, secret, events string
	}
	var hooks []hook
	for rows.Next() {
		var h hook
		if err := rows.Scan(&h.id, &h.url, &h.secret, &h.events); err != nil {
			s.log.ErrorContext(ctx, "load webhooks", "err", err)
			continue
		}
		if eventMatches(h.events, e.Kind) {
			hooks = append(hooks, h)
		}
	}
	rows.Close()
	if len(hooks) == 0 {
		return
	}
	var issue *Issue
	if e.IssueID != 0 && e.Kind != EvIssueDeleted {
		var rerr error
		issue, rerr = s.issueByID(ctx, s.db.R, e.IssueID, true)
		if readFault(rerr) && !errors.Is(rerr, ErrNotFound) {
			s.log.WarnContext(ctx, "read the issue of a webhook event", "issue", e.IssueID, "event", e.Kind, "err", rerr)
		}
	}
	body, err := json.Marshal(s.webhookPayload(e, issue))
	if err != nil {
		s.log.ErrorContext(ctx, "webhook payload", "err", err)
		return
	}
	for _, h := range hooks {
		s.goBG(func() {
			s.sendLogged(context.Background(), h.id, h.url, h.secret, e.Kind, body, 3, nil)
		})
	}
}

// sent is how one delivery (with its retries) went.
type sent struct {
	status   int
	response string
	err      error
}

// Delivery log limits: bodies above maxLoggedBody aren't kept (so can't be
// replayed), answers are cut to maxLoggedResponse, and each webhook keeps
// its last keepDeliveries deliveries for at most deliveryDays.
const (
	maxLoggedBody     = 256 << 10
	maxLoggedResponse = 2 << 10
	keepDeliveries    = 50
	deliveryDays      = 14
)

// sendLogged delivers a body to a webhook, up to `tries` times for server
// errors and network failures (one delivery id across retries, so
// receivers can drop repeats), and records it in the webhook's delivery log.
func (s *Service) sendLogged(ctx context.Context, hookID int64, url, secret, kind string, body []byte, tries int, replayOf *int64) sent {
	delivery := newToken("")[:20]
	start := s.now()
	stored := ""
	if len(body) <= maxLoggedBody {
		stored = string(body)
	}
	res, err := s.db.W.ExecContext(ctx, `INSERT INTO webhook_deliveries(webhook_id, delivery, event, attempts, request, replay_of, created_at) VALUES (?,?,?,0,?,?,?)`,
		hookID, delivery, kind, stored, nullInt(replayOf), start.UnixMilli())
	var rowID int64
	if err != nil {
		s.log.WarnContext(ctx, "log webhook delivery", "err", err)
	} else {
		rowID, _ = res.LastInsertId()
	}
	var d sent
	attempts := 0
	for wait := time.Second; ; wait *= 5 {
		attempts++
		d = s.post(ctx, url, secret, kind, body, delivery)
		if d.err == nil || attempts >= tries || !retryable(d.status, d.err) {
			break
		}
		time.Sleep(wait)
	}
	msg := ""
	if d.err != nil {
		msg = PublicError(d.err)
		webhookDeliveries.WithLabelValues("error").Inc()
	} else {
		webhookDeliveries.WithLabelValues("ok").Inc()
	}
	var status any
	if d.status != 0 {
		status = d.status
	}
	if rowID != 0 {
		if _, err := s.db.W.ExecContext(ctx, `UPDATE webhook_deliveries SET attempts = ?, status = ?, error = ?, duration_ms = ?, response = ? WHERE id = ?`,
			attempts, status, msg, s.now().Sub(start).Milliseconds(), d.response, rowID); err != nil {
			s.log.WarnContext(ctx, "log webhook delivery", "err", err)
		}
	}
	if _, err := s.db.W.ExecContext(ctx, `UPDATE webhooks SET last_status = ?, last_error = ? WHERE id = ?`, d.status, msg, hookID); err != nil {
		s.log.WarnContext(ctx, "record webhook delivery", "err", err)
	}
	return d
}

// WebhookDelivery is one entry in a webhook's delivery log.
type WebhookDelivery struct {
	ID         int64     `json:"id"`
	Delivery   string    `json:"delivery"`
	Event      string    `json:"event"`
	Attempts   int       `json:"attempts"`
	Status     *int64    `json:"status"`
	Error      string    `json:"error"`
	DurationMS int64     `json:"duration_ms"`
	Response   string    `json:"response"`
	Replayable bool      `json:"replayable"`
	ReplayOf   *int64    `json:"replay_of"`
	CreatedAt  time.Time `json:"created_at"`
}

const deliveryCols = `id, delivery, event, attempts, status, error, duration_ms, response, request != '', replay_of, created_at`

func scanDelivery(sc interface{ Scan(...any) error }) (*WebhookDelivery, error) {
	var d WebhookDelivery
	var status, replayOf sql.NullInt64
	var c int64
	if err := sc.Scan(&d.ID, &d.Delivery, &d.Event, &d.Attempts, &status, &d.Error, &d.DurationMS, &d.Response, &d.Replayable, &replayOf, &c); err != nil {
		return nil, err
	}
	d.Status, d.ReplayOf, d.CreatedAt = ptrInt(status), ptrInt(replayOf), msTime(c)
	return &d, nil
}

// WebhookDeliveries lists a webhook's recent deliveries, newest first.
func (s *Service) WebhookDeliveries(ctx context.Context, actor *Actor, id int64) ([]*WebhookDelivery, error) {
	if _, err := s.webhookFor(ctx, actor, id); err != nil {
		return nil, err
	}
	rows, err := s.db.R.QueryContext(ctx, `SELECT `+deliveryCols+` FROM webhook_deliveries WHERE webhook_id = ? ORDER BY id DESC LIMIT ?`, id, keepDeliveries)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*WebhookDelivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ReplayWebhookDelivery sends a logged delivery's body to the webhook again
// (signed with its current secret, under a new delivery id) and returns
// the new log entry.
func (s *Service) ReplayWebhookDelivery(ctx context.Context, actor *Actor, id, deliveryID int64) (*WebhookDelivery, error) {
	if err := actor.CanWrite(); err != nil {
		return nil, err
	}
	if _, err := s.webhookFor(ctx, actor, id); err != nil {
		return nil, err
	}
	var event, body, url, secret string
	err := s.db.R.QueryRowContext(ctx, `SELECT d.event, d.request, w.url, w.secret FROM webhook_deliveries d JOIN webhooks w ON w.id = d.webhook_id
		WHERE d.id = ? AND d.webhook_id = ?`, deliveryID, id).Scan(&event, &body, &url, &secret)
	if err != nil {
		return nil, notFound("delivery")
	}
	if body == "" {
		return nil, invalid("this delivery was too big to keep, so it can't be sent again")
	}
	s.sendLogged(ctx, id, url, secret, event, []byte(body), 1, &deliveryID)
	return scanDelivery(s.db.R.QueryRowContext(ctx, `SELECT `+deliveryCols+` FROM webhook_deliveries WHERE webhook_id = ? AND replay_of = ? ORDER BY id DESC LIMIT 1`, id, deliveryID))
}

// pruneDeliveries keeps each webhook's last keepDeliveries deliveries from
// the last deliveryDays days.
func (s *Service) pruneDeliveries(ctx context.Context) error {
	cutoff := s.now().Add(-deliveryDays * 24 * time.Hour).UnixMilli()
	_, err := s.db.W.ExecContext(ctx, `DELETE FROM webhook_deliveries WHERE created_at < ? OR id IN (
		SELECT id FROM (SELECT id, row_number() OVER (PARTITION BY webhook_id ORDER BY id DESC) AS n FROM webhook_deliveries) WHERE n > ?)`,
		cutoff, keepDeliveries)
	return err
}

// retryable: server errors, rate limits and network failures may pass on a
// later attempt; other client errors and refused addresses won't.
func retryable(status int, err error) bool {
	switch {
	case errors.Is(err, ErrEgressBlocked):
		return false
	case status == 0, status == http.StatusTooManyRequests, status >= 500:
		return true
	}
	return false
}

// postJSON POSTs a body, signing it with HMAC-SHA256 when secret is set.
// The X-Waypoint-Delivery header identifies the delivery (stable across
// retries) and X-Waypoint-Timestamp says when it was sent.
func (s *Service) postJSON(ctx context.Context, url, secret, kind string, body []byte, delivery string) (int, error) {
	d := s.post(ctx, url, secret, kind, body, delivery)
	return d.status, d.err
}

// post is postJSON that also keeps the start of the answer.
func (s *Service) post(ctx context.Context, url, secret, kind string, body []byte, delivery string) sent {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return sent{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Waypoint-Webhook/1")
	req.Header.Set("X-Waypoint-Event", kind)
	if delivery != "" {
		req.Header.Set("X-Waypoint-Delivery", delivery)
	}
	req.Header.Set("X-Waypoint-Timestamp", strconv.FormatInt(s.now().Unix(), 10))
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req.Header.Set("X-Waypoint-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return sent{err: err}
	}
	defer resp.Body.Close()
	head, _ := io.ReadAll(io.LimitReader(resp.Body, maxLoggedResponse))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	d := sent{status: resp.StatusCode, response: strings.ToValidUTF8(string(head), "")}
	if resp.StatusCode >= 300 {
		d.err = &HTTPStatusError{resp.StatusCode}
	}
	return d
}
