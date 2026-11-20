package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestAuditAPI(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "robin@example.com", Password: "correct horse battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	robin := &core.Actor{User: u}
	tok, _, err := svc.CreateToken(ctx, robin, core.TokenSpec{Name: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	c := &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}

	var page struct {
		Entries []struct {
			Action, Category, Detail string
			User                     struct{ Username string }
		}
		NextBefore int64 `json:"next_before"`
	}
	if code := c.json("GET", "/api/v1/audit?category=access", nil, &page); code != 200 || len(page.Entries) != 1 ||
		page.Entries[0].Action != "token.create" || page.Entries[0].User.Username != "robin" || page.NextBefore != 0 {
		t.Fatalf("audit: %d %+v", code, page)
	}
	if code, b := c.do("GET", "/api/v1/audit?category=nope", nil); code != 400 {
		t.Errorf("bad category: %d %s", code, b)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/audit.csv?days=30", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), "waypoint-audit-") {
		t.Errorf("csv: %d %v", resp.StatusCode, resp.Header)
	}
}
