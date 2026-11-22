package core

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func apiErr(status int, body string, hdr map[string]string) error {
	resp := &http.Response{StatusCode: status, Header: http.Header{}}
	for k, v := range hdr {
		resp.Header.Set(k, v)
	}
	return apiErrorOf("POST", "/x", resp, []byte(body))
}

// What passes by waiting is retried and what doesn't is refused, by what the
// forge or the network said (WP-1370).
func TestShipRefusedClassifiesForgeAnswers(t *testing.T) {
	var syn *json.SyntaxError
	_ = json.Unmarshal([]byte("<html>"), new(any))
	jerr := json.Unmarshal([]byte("<html>"), new(map[string]any))
	if !errors.As(jerr, &syn) {
		t.Fatalf("setup: %T", jerr)
	}
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"400":                   {apiErr(400, "bad", nil), true},
		"401":                   {apiErr(401, "", nil), true},
		"403 plain":             {apiErr(403, "forbidden", nil), true},
		"403 with Retry-After":  {apiErr(403, "", map[string]string{"Retry-After": "60"}), false},
		"403 secondary limit":   {apiErr(403, "You have exceeded a secondary rate limit", nil), false},
		"403 limit remaining 0": {apiErr(403, "", map[string]string{"X-RateLimit-Remaining": "0"}), false},
		"404":                   {apiErr(404, "", nil), true},
		"409":                   {apiErr(409, "", nil), false},
		"410":                   {apiErr(410, "", nil), true},
		"422":                   {apiErr(422, "", nil), true},
		"423":                   {apiErr(423, "", nil), false},
		"429":                   {apiErr(429, "", nil), false},
		"500":                   {apiErr(500, "", nil), false},
		"502":                   {apiErr(502, "", nil), false},
		"504":                   {apiErr(504, "", nil), false},
		"302":                   {apiErr(302, "", nil), true},
		"tls":                   {fmt.Errorf("get: %w", &tls.CertificateVerificationError{}), true},
		"200 with a login page": {fmt.Errorf("decode: %w", jerr), true},
		"truncated body":        {fmt.Errorf("decode: %w", errors.New("unexpected EOF")), false},
	} {
		if got := shipRefused(tc.err); got != tc.want {
			t.Errorf("%s: shipRefused = %v, want %v", name, got, tc.want)
		}
	}
}

func TestShipWaitHonoursRetryAfterAndSpreadsRounds(t *testing.T) {
	err := apiErr(429, "", map[string]string{"Retry-After": "7200"})
	if w := shipWait(1, 1, err); w < 2*time.Hour {
		t.Fatalf("wait %s ignores Retry-After", w)
	}
	if w := shipWait(1, 1, apiErr(429, "", map[string]string{"Retry-After": "999999"})); w > 7*time.Hour {
		t.Fatalf("wait %s isn't capped", w)
	}
	if shipWait(1, 3, nil) == shipWait(2, 3, nil) {
		t.Fatal("two rounds that failed together wait the same")
	}
}

func TestRecordPRRefusesNoNumber(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical"})
	if _, err := e.s.recordPR(e.ctx, &Forge{}, "acme/app", &ForgePR{Title: "x", State: "open"}, false); err == nil {
		t.Fatal("a pull request with no number was recorded")
	}
}
