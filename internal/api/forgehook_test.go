package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// A webhook bigger than the limit is refused outright, not cut short (a cut
// body fails its signature and looks like a forged hook).
func TestForgeHookSizeLimit(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	u, _ := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "j@example.com", Password: "correct horse battery", IsAdmin: true})
	kind, base := "github", "https://github.com"
	f, err := svc.CreateForge(ctx, &core.Actor{User: u}, core.ForgeInput{Kind: &kind, BaseURL: &base})
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := svc.ForgeSecret(ctx, &core.Actor{User: u}, f.ID)
	send := func(body []byte) int {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/v1/hooks/forge/%d", ts.URL, f.ID), bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "ping")
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	pad := bytes.Repeat([]byte("x"), 6<<20)
	if code := send([]byte(`{"zen":"` + string(pad) + `"}`)); code != 200 {
		t.Errorf("6 MiB hook: %d", code)
	}
	big := bytes.Repeat([]byte("x"), 25<<20+1)
	if code := send(big); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized hook: %d", code)
	}
}
