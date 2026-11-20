package api

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// The ledger's routes answer through the real handler chain, even empty.
func TestEscapeRoutes(t *testing.T) {
	ts, svc := setup(t)
	link, err := svc.BootstrapInvite(context.Background())
	if err != nil || link == nil {
		t.Fatal("expected bootstrap invite", err)
	}
	tok := link.URL[strings.LastIndex(link.URL, "/")+1:]
	jar, _ := cookiejar.New(nil)
	web := &client{t: t, base: ts.URL, http: &http.Client{Jar: jar}, web: true}
	if code := web.json("POST", "/api/v1/auth/invite/"+tok, map[string]any{"username": "robin", "email": "robin@example.com", "password": "correct horse battery"}, nil); code != 200 {
		t.Fatalf("accept: %d", code)
	}

	for _, path := range []string{"/api/v1/escapes/due", "/api/v1/escapes/proofs/due", "/api/v1/escapes", "/api/v1/escapes/files?repo=o/r"} {
		if code, b := web.do("GET", path, nil); code != 200 {
			t.Errorf("%s: %d %s", path, code, b)
		}
	}
	code, b := web.do("GET", "/api/v1/escapes/stats?format=text", nil)
	if code != 200 || !strings.Contains(string(b), "Bug fixes, last 90 days: 0") {
		t.Errorf("stats: %d %s", code, b)
	}
	if code, b := web.do("POST", "/api/v1/escapes/traces", "not json"); code != 400 {
		t.Errorf("bad body: %d %s", code, b)
	}
	if code, b := web.do("POST", "/api/v1/escapes/traces", map[string]any{"pr_id": 999, "method": "szz"}); code < 400 || code >= 500 {
		t.Errorf("unknown pull request: %d %s", code, b)
	}
	if code, b := web.do("POST", "/api/v1/escapes/proofs", map[string]any{"pr_id": 999, "outcome": "passes"}); code < 400 || code >= 500 {
		t.Errorf("proof of an unknown pull request: %d %s", code, b)
	}
	anon := &client{t: t, base: ts.URL, http: http.DefaultClient}
	if code, _ := anon.do("GET", "/api/v1/escapes/stats", nil); code != 401 {
		t.Errorf("anonymous read: %d", code)
	}
}

func TestProofsText(t *testing.T) {
	var o strings.Builder
	proofsText(&o, &core.EscapeStats{ProofsPending: 1, Proofs: []core.EscapeProof{
		{Outcome: "proven", Fixes: 3, Refixed: 0}, {Outcome: "no_test", Fixes: 1, Refixed: 1}, {Outcome: "", Fixes: 1}}})
	want := "Fixes proven by a failing test: 3 of 4 checked (75%); 1 waiting\nFixed again later, by proof: proven 0 of 3, no_test 1 of 1, not checked 0 of 1.\n"
	if o.String() != want {
		t.Fatalf("got %q", o.String())
	}
}
