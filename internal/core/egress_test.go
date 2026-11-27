package core

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestEgressPolicy(t *testing.T) {
	def, strict, local := EgressPolicy{}, EgressPolicy{BlockPrivate: true}, EgressPolicy{AllowLoopback: true}
	for _, tc := range []struct {
		ip                 string
		def, strict, local bool
	}{
		{"8.8.8.8", true, true, true},
		{"10.9.0.5", true, false, true},
		{"192.168.1.10", true, false, true},
		{"100.81.30.79", true, false, true},
		{"fd00::1", true, false, true},
		{"127.0.0.1", false, false, true},
		{"::1", false, false, true},
		{"::ffff:127.0.0.1", false, false, true},
		{"169.254.169.254", false, false, false},
		{"fe80::1", false, false, false},
		{"0.0.0.0", false, false, false},
		{"224.0.0.1", false, false, false},
		{"64:ff9b::a9fe:a9fe", false, false, false},
		{"64:ff9b:1:a9fe:a9:fe00::", false, false, false},
		{"2002:a9fe:a9fe::1", false, false, false},
		{"64:ff9b::7f00:1", false, false, true},
		{"64:ff9b::a02:5", true, false, true},
		{"64:ff9b::808:808", true, true, true},
	} {
		ip := netip.MustParseAddr(tc.ip)
		if got := def.allowed(ip); got != tc.def {
			t.Errorf("default %s = %v", tc.ip, got)
		}
		if got := strict.allowed(ip); got != tc.strict {
			t.Errorf("block-private %s = %v", tc.ip, got)
		}
		if got := local.allowed(ip); got != tc.local {
			t.Errorf("allow-loopback %s = %v", tc.ip, got)
		}
	}
}

func TestWebhookDeliveryAndEgress(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Hooks", "")
	var gotSig, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotSig, gotBody = r.Header.Get("X-Waypoint-Signature"), string(b)
	}))
	defer srv.Close()
	redirect := httptest.NewServer(http.RedirectHandler(srv.URL, http.StatusFound))
	defer redirect.Close()
	secret := "s3cret"
	h, err := e.s.CreateWebhook(e.ctx, e.admin, p.Key, WebhookInput{URL: &srv.URL, Secret: &secret})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.TestWebhook(e.ctx, e.admin, h.ID); PublicError(err) != "that address is not allowed" {
		t.Fatalf("loopback webhook: %v (%q)", err, PublicError(err))
	}
	e.s.http = EgressPolicy{AllowLoopback: true}.client(0)
	if _, err := e.s.TestWebhook(e.ctx, e.admin, h.ID); err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(gotBody))
	if gotSig != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Errorf("signature %q", gotSig)
	}
	gotBody = ""
	if _, err := e.s.UpdateWebhook(e.ctx, e.admin, h.ID, WebhookInput{URL: &redirect.URL}); err != nil {
		t.Fatal(err)
	}
	if code, err := e.s.TestWebhook(e.ctx, e.admin, h.ID); code != http.StatusFound || err == nil || gotBody != "" {
		t.Errorf("redirect followed: code %d err %v body %q", code, err, gotBody)
	}
}

// A model's answer may stream for longer than any fixed limit: the stream
// client bounds the wait for the headers, and the request's context the
// rest, and checks addresses like the others.
func TestEgressStreamClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow-headers" {
			time.Sleep(300 * time.Millisecond)
		}
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		for range 3 {
			time.Sleep(100 * time.Millisecond)
			io.WriteString(w, "chunk\n")
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()
	c := EgressPolicy{AllowLoopback: true}.streamClient(150 * time.Millisecond)
	if c.Timeout != 0 {
		t.Errorf("Timeout = %v, want none", c.Timeout)
	}
	get := func(ctx context.Context, path string) (string, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
		resp, err := c.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return string(b), err
	}
	if body, err := get(context.Background(), "/"); err != nil || body != "chunk\nchunk\nchunk\n" {
		t.Errorf("streamed body = %q, %v", body, err)
	}
	if _, err := get(context.Background(), "/slow-headers"); err == nil {
		t.Error("late headers were waited for")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := get(ctx, "/"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("past the deadline: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	if _, err := (EgressPolicy{}).streamClient(time.Second).Do(req); !errors.Is(err, ErrEgressBlocked) {
		t.Errorf("loopback: %v", err)
	}
}
