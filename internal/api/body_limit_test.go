package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// A body over the 2 MiB a route reads says so, with 413, instead of being cut
// off and read as broken JSON ("unexpected EOF").
func TestOversizedBodyAnswers413(t *testing.T) {
	ts, svc := setup(t)
	u, err := svc.CreateUser(context.Background(), core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "robin@example.com", Password: "correct-horse-battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, _ := svc.CreateAPIToken(context.Background(), &core.Actor{User: u}, "robin", false, 0)
	c := &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	big := `{"name":"` + strings.Repeat("x", 3<<20) + `","key":"BIG"}`
	code, b := c.do("POST", "/api/v1/projects", big)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d %.120s, want 413", code, b)
	}
	if !strings.Contains(string(b), "request body too large (limit 2097152)") || strings.Contains(string(b), "EOF") {
		t.Errorf("message = %.200s", b)
	}
	if code, b := c.do("POST", "/api/v1/projects", map[string]any{"name": "Fine", "key": "FIN"}); code != 200 {
		t.Errorf("a small body: %d %s", code, b)
	}
}
