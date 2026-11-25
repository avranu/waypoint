package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// PushSubscription is a browser's push endpoint, as the Push API gives it.
type PushSubscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

var vapidMu sync.Mutex

// vapidKeys returns the server's VAPID key pair, creating it once.
func (s *Service) vapidKeys(ctx context.Context) (priv, pub string, err error) {
	vapidMu.Lock()
	defer vapidMu.Unlock()
	var v string
	err = s.db.R.QueryRowContext(ctx, `SELECT value FROM server_settings WHERE key = 'vapid'`).Scan(&v)
	if err == nil {
		priv, pub, _ = strings.Cut(v, " ")
		return priv, pub, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}
	if priv, pub, err = webpush.GenerateVAPIDKeys(); err != nil {
		return "", "", err
	}
	_, err = s.db.W.ExecContext(ctx, `INSERT INTO server_settings(key, value) VALUES ('vapid', ?)`, priv+" "+pub)
	return priv, pub, err
}

// PushPublicKey is the VAPID public key browsers subscribe with.
func (s *Service) PushPublicKey(ctx context.Context) (string, error) {
	_, pub, err := s.vapidKeys(ctx)
	return pub, err
}

// SavePushSubscription registers this browser for the actor's notifications.
func (s *Service) SavePushSubscription(ctx context.Context, actor *Actor, sub PushSubscription, userAgent string) error {
	if err := actor.canWrite(); err != nil {
		return err
	}
	u, err := url.Parse(sub.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || len(sub.Endpoint) > 1000 {
		return invalid("push endpoint must be an https URL")
	}
	if sub.Keys.P256dh == "" || sub.Keys.Auth == "" || len(sub.Keys.P256dh) > 200 || len(sub.Keys.Auth) > 100 {
		return invalid("push subscription keys are missing")
	}
	return s.write(ctx, actor, func(t *tx) error {
		if _, err := t.ExecContext(ctx, `INSERT INTO push_subscriptions(user_id, endpoint, p256dh, auth, user_agent, created_at) VALUES (?,?,?,?,?,?)
			ON CONFLICT(endpoint) DO UPDATE SET user_id = excluded.user_id, p256dh = excluded.p256dh, auth = excluded.auth`,
			actor.id(), sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth, truncateBytes(userAgent, 200), s.nowMS()); err != nil {
			return err
		}
		_, err := t.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE user_id = ? AND id NOT IN
			(SELECT id FROM push_subscriptions WHERE user_id = ? ORDER BY created_at DESC LIMIT 10)`, actor.id(), actor.id())
		return err
	})
}

// dropPushSubscriptions forgets every browser the user subscribed, when
// their sessions are reset (password change, "sign out everywhere").
func dropPushSubscriptions(ctx context.Context, q queryer, userID int64) error {
	_, err := q.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE user_id = ?`, userID)
	return err
}

// DeletePushSubscription stops pushes to one browser.
func (s *Service) DeletePushSubscription(ctx context.Context, actor *Actor, endpoint string) error {
	if err := actor.canWrite(); err != nil {
		return err
	}
	_, err := s.db.W.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE user_id = ? AND endpoint = ?`, actor.id(), endpoint)
	return err
}

// PushDevices counts the actor's subscribed browsers.
func (s *Service) PushDevices(ctx context.Context, actor *Actor) (int, error) {
	var n int
	err := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM push_subscriptions WHERE user_id = ?`, actor.id()).Scan(&n)
	return n, err
}

type pushMessage struct {
	Title   string       `json:"title"`
	Body    string       `json:"body"`
	URL     string       `json:"url,omitempty"`
	Tag     string       `json:"tag,omitempty"`
	Actions []pushAction `json:"actions,omitempty"`
}

// pushAction is a button on a push notification; the service worker (sw.js)
// knows what each action does.
type pushAction struct {
	Action string `json:"action"`
	Title  string `json:"title"`
}

// pushPauseSessions is the quota alert's Pause button (WP-146).
const pushPauseSessions = "pause-sessions"

// sendPush delivers a notification to every browser the user subscribed,
// in the background, and forgets subscriptions the push service has dropped.
func (s *Service) sendPush(ctx context.Context, userID int64, msg pushMessage) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, endpoint, p256dh, auth FROM push_subscriptions WHERE user_id = ?`, userID)
	if err != nil {
		s.log.WarnContext(ctx, "load push subscriptions", "err", err)
		return
	}
	type target struct {
		id  int64
		sub webpush.Subscription
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.sub.Endpoint, &t.sub.Keys.P256dh, &t.sub.Keys.Auth); err == nil {
			targets = append(targets, t)
		}
	}
	rows.Close()
	if len(targets) == 0 {
		return
	}
	priv, pub, err := s.vapidKeys(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "push keys", "err", err)
		return
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return
	}
	subscriber := s.BaseURL()
	if !strings.HasPrefix(subscriber, "https://") {
		subscriber = "mailto:waypoint@localhost"
	}
	for _, t := range targets {
		body := slices.Clone(payload)
		s.goBG(func() {
			resp, err := webpush.SendNotificationWithContext(context.Background(), body, &t.sub, &webpush.Options{
				HTTPClient: s.pushHTTP, Subscriber: subscriber, VAPIDPublicKey: pub, VAPIDPrivateKey: priv, TTL: 24 * 3600,
				Urgency: webpush.UrgencyNormal,
			})
			if err != nil {
				s.log.WarnContext(ctx, "web push failed", "err", PublicError(err))
				return
			}
			resp.Body.Close()
			switch {
			case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
				if _, err := s.db.W.ExecContext(context.Background(), `DELETE FROM push_subscriptions WHERE id = ?`, t.id); err != nil {
					s.log.WarnContext(ctx, "drop push subscription", "err", err)
				}
			case resp.StatusCode >= 300:
				s.log.WarnContext(ctx, "web push rejected", "status", resp.StatusCode)
			}
		})
	}
}
