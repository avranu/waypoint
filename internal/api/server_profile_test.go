package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func profileClients(t *testing.T) (admin, sam *client) {
	ts, svc := setup(t)
	ctx := context.Background()
	mk := func(name string, isAdmin bool) *client {
		u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: name, Email: name + "@example.com", Password: "correct-horse-battery", IsAdmin: isAdmin})
		if err != nil {
			t.Fatal(err)
		}
		tok, _, err := svc.CreateAPIToken(ctx, &core.Actor{User: u}, name, false, 0)
		if err != nil {
			t.Fatal(err)
		}
		return &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	}
	return mk("robin", true), mk("sam", false)
}

func TestServerProfileGoroutineAdminOnly(t *testing.T) {
	admin, sam := profileClients(t)
	code, b := admin.do("GET", "/api/v1/server/profile?kind=goroutine", nil)
	if code != 200 || !strings.Contains(string(b), "goroutine ") {
		t.Fatalf("admin: %d %.200s", code, b)
	}
	if code, b := sam.do("GET", "/api/v1/server/profile?kind=goroutine", nil); code != 403 {
		t.Fatalf("non-admin: %d %s", code, b)
	}
	if code, b := admin.do("GET", "/api/v1/server/profile?kind=heap", nil); code != 200 || len(b) == 0 {
		t.Fatalf("heap: %d", code)
	}
	if code, _ := admin.do("GET", "/api/v1/server/profile?kind=nope", nil); code != 400 {
		t.Fatalf("bad kind: %d", code)
	}
}

func TestServerProfileOneCPUAtATime(t *testing.T) {
	admin, _ := profileClients(t)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	var bodies [2][]byte
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], bodies[i] = admin.do("GET", "/api/v1/server/profile?kind=cpu&seconds=2", nil)
		}()
	}
	wg.Wait()
	ok, busy := 0, 0
	for i, c := range codes {
		switch c {
		case 200:
			ok++
		case 409:
			busy++
			if !strings.Contains(string(bodies[i]), "already running") {
				t.Errorf("409 text: %s", bodies[i])
			}
		}
	}
	if ok != 1 || busy != 1 {
		t.Fatalf("codes %v", codes)
	}
}
