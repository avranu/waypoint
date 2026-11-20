package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestPullsAndReviewNoteAPI(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	admin, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "robin@example.com", Password: "correct-horse-battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	sam, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "sam", Email: "sam@example.com", Password: "correct-horse-battery"})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, _ := svc.CreateAPIToken(ctx, &core.Actor{User: admin}, "robin", false, 0)
	samTok, _, _ := svc.CreateAPIToken(ctx, &core.Actor{User: sam}, "sam", false, 0)
	robin := &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	other := &client{t: t, base: ts.URL, http: http.DefaultClient, token: samTok}

	var list struct {
		Totals map[string]any   `json:"totals"`
		Pulls  []map[string]any `json:"pulls"`
	}
	if code := robin.json("GET", "/api/v1/pulls?days=7&state=open", nil, &list); code != 200 || list.Pulls == nil {
		t.Fatalf("list: %d %+v", code, list)
	}
	if code, b := robin.do("GET", "/api/v1/pulls?state=weird", nil); code != 400 {
		t.Errorf("bad state: %d %s", code, b)
	}
	if code, _ := robin.do("GET", "/api/v1/pulls/99", nil); code != 404 {
		t.Errorf("missing pull request: %d", code)
	}

	var note map[string]any
	if code := robin.json("GET", "/api/v1/admin/review-note", nil, &note); code != 200 || note["custom"] != false {
		t.Fatalf("note: %d %v", code, note)
	}
	if code := robin.json("PUT", "/api/v1/admin/review-note", map[string]string{"note": "Bot review by {models}."}, &note); code != 200 || note["note"] != "Bot review by {models}." {
		t.Errorf("set note: %d %v", code, note)
	}
	if code, b := other.do("PUT", "/api/v1/admin/review-note", map[string]string{"note": "x"}); code != 403 {
		t.Errorf("non-admin set the note: %d %s", code, b)
	}
	if code, b := other.do("GET", "/api/v1/admin/review-note", nil); code != 403 || !strings.Contains(string(b), "admins") {
		t.Errorf("non-admin read the note: %d %s", code, b)
	}
}
