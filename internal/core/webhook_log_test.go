package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// hookReceiver records what a webhook receives and answers with status.
type hookReceiver struct {
	mu         sync.Mutex
	status     int
	bodies     []string
	sigs       []string
	deliveries []string
}

func (h *hookReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bodies = append(h.bodies, string(b))
	h.sigs = append(h.sigs, r.Header.Get("X-Waypoint-Signature"))
	h.deliveries = append(h.deliveries, r.Header.Get("X-Waypoint-Delivery"))
	w.WriteHeader(h.status)
	_, _ = w.Write([]byte(strings.Repeat("no thanks. ", 400))) // ~4.4 KB
}

func TestWebhookDeliveryLogAndReplay(t *testing.T) {
	e := newEnv(t)
	e.s.http = EgressPolicy{AllowLoopback: true}.client(0)
	rec := &hookReceiver{status: 400}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	p := e.project(e.admin, "Hooks", "kanban")
	secret := "first"
	h, err := e.s.CreateWebhook(e.ctx, e.admin, p.Key, WebhookInput{URL: &srv.URL, Secret: &secret, Events: ptr("issue.created")})
	if err != nil {
		t.Fatal(err)
	}
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "Logged"})
	e.s.Wait()
	log, err := e.s.WebhookDeliveries(e.ctx, e.admin, h.ID)
	if err != nil || len(log) != 1 {
		t.Fatalf("log: %+v %v", log, err)
	}
	d := log[0]
	if d.Event != "issue.created" || d.Status == nil || *d.Status != 400 || d.Error == "" || d.Attempts != 1 || !d.Replayable {
		t.Errorf("entry: %+v", d)
	}
	if len(d.Response) != maxLoggedResponse || !strings.HasPrefix(d.Response, "no thanks.") {
		t.Errorf("response kept: %d bytes", len(d.Response))
	}
	if d.Delivery != rec.deliveries[0] {
		t.Errorf("delivery id %q, sent %q", d.Delivery, rec.deliveries[0])
	}

	// Replay: the same body, a new delivery id, signed with today's secret.
	rec.status = 200
	if _, err := e.s.UpdateWebhook(e.ctx, e.admin, h.ID, WebhookInput{Secret: ptr("second")}); err != nil {
		t.Fatal(err)
	}
	r, err := e.s.ReplayWebhookDelivery(e.ctx, e.admin, h.ID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.ReplayOf == nil || *r.ReplayOf != d.ID || r.Status == nil || *r.Status != 200 || r.Error != "" {
		t.Errorf("replay entry: %+v", r)
	}
	if len(rec.bodies) != 2 || rec.bodies[1] != rec.bodies[0] || rec.deliveries[1] == rec.deliveries[0] {
		t.Fatalf("replayed request: %d bodies, deliveries %v", len(rec.bodies), rec.deliveries)
	}
	mac := hmac.New(sha256.New, []byte("second"))
	mac.Write([]byte(rec.bodies[1]))
	if rec.sigs[1] != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Error("replay not signed with the current secret")
	}
	if log, _ := e.s.WebhookDeliveries(e.ctx, e.admin, h.ID); len(log) != 2 || log[0].ID != r.ID {
		t.Errorf("log after replay (newest first): %+v", log)
	}
	// A test ping is logged too.
	if _, err := e.s.TestWebhook(e.ctx, e.admin, h.ID); err != nil {
		t.Fatal(err)
	}
	if log, _ := e.s.WebhookDeliveries(e.ctx, e.admin, h.ID); len(log) != 3 || log[0].Event != "ping" {
		t.Errorf("ping not logged: %+v", log[0])
	}
}

func TestWebhookDeliveryAccess(t *testing.T) {
	e := newEnv(t)
	e.s.http = EgressPolicy{AllowLoopback: true}.client(0)
	rec := &hookReceiver{status: 200}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	p := e.project(e.admin, "Hooks", "kanban")
	only := ptr("issue.created")
	h, _ := e.s.CreateWebhook(e.ctx, e.admin, p.Key, WebhookInput{URL: &srv.URL, Events: only})
	global, _ := e.s.CreateWebhook(e.ctx, e.admin, "", WebhookInput{URL: &srv.URL, Events: only})
	e.issue(e.admin, IssueInput{Project: p.Key, Title: "Logged"})
	e.s.Wait()
	log, _ := e.s.WebhookDeliveries(e.ctx, e.admin, h.ID)
	if len(log) != 1 {
		t.Fatalf("log: %+v", log)
	}
	glog, _ := e.s.WebhookDeliveries(e.ctx, e.admin, global.ID)
	if len(glog) != 1 {
		t.Fatalf("global log: %+v", glog)
	}
	bob := e.user("bob", false)
	if err := e.s.SetMember(e.ctx, e.admin, p.Key, "bob", "editor"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.WebhookDeliveries(e.ctx, bob, h.ID); err == nil {
		t.Error("an editor read the delivery log")
	}
	if _, err := e.s.ReplayWebhookDelivery(e.ctx, bob, h.ID, log[0].ID); err == nil {
		t.Error("an editor replayed a delivery")
	}
	if _, err := e.s.WebhookDeliveries(e.ctx, bob, global.ID); err == nil {
		t.Error("a non-admin read a global webhook's log")
	}
	ro := &Actor{User: e.admin.User, ReadOnly: true}
	if _, err := e.s.ReplayWebhookDelivery(e.ctx, ro, h.ID, log[0].ID); err == nil {
		t.Error("a read-only token replayed a delivery")
	}
	// A delivery only replays through its own webhook.
	if _, err := e.s.ReplayWebhookDelivery(e.ctx, e.admin, global.ID, log[0].ID); err == nil {
		t.Error("replayed another webhook's delivery")
	}
	e.s.Wait()
	if n := len(rec.bodies); n != 2 {
		t.Errorf("receiver got %d requests, want the 2 real deliveries only", n)
	}
}

func TestWebhookDeliveryPruneAndBigBodies(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Hooks", "kanban")
	u := "https://example.com/hook"
	h, _ := e.s.CreateWebhook(e.ctx, e.admin, p.Key, WebhookInput{URL: &u})
	other, _ := e.s.CreateWebhook(e.ctx, e.admin, p.Key, WebhookInput{URL: &u})
	now := e.clockNow().UnixMilli()
	old := e.clockNow().Add(-(deliveryDays + 1) * 24 * time.Hour).UnixMilli()
	for i := 0; i < 60; i++ {
		if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO webhook_deliveries(webhook_id, delivery, event, request, created_at) VALUES (?,?,?,?,?)`, h.ID, "d", "issue.created", "{}", now); err != nil {
			t.Fatal(err)
		}
	}
	for _, at := range []int64{old, now} {
		if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO webhook_deliveries(webhook_id, delivery, event, request, created_at) VALUES (?,?,?,?,?)`, other.ID, "d", "issue.created", "", at); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.s.pruneDeliveries(e.ctx); err != nil {
		t.Fatal(err)
	}
	var n, m int
	_ = e.s.db.R.QueryRowContext(e.ctx, `SELECT count(*) FROM webhook_deliveries WHERE webhook_id = ?`, h.ID).Scan(&n)
	_ = e.s.db.R.QueryRowContext(e.ctx, `SELECT count(*) FROM webhook_deliveries WHERE webhook_id = ?`, other.ID).Scan(&m)
	if n != keepDeliveries || m != 1 {
		t.Errorf("after pruning: %d and %d, want %d and 1", n, m, keepDeliveries)
	}
	// A body too big to keep can't be replayed.
	log, _ := e.s.WebhookDeliveries(e.ctx, e.admin, other.ID)
	if len(log) != 1 || log[0].Replayable {
		t.Fatalf("big body entry: %+v", log)
	}
	if _, err := e.s.ReplayWebhookDelivery(e.ctx, e.admin, other.ID, log[0].ID); err == nil || !IsInvalid(err) {
		t.Errorf("replayed a delivery without a body: %v", err)
	}
}
