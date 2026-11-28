package core

import (
	"strings"
	"testing"
)

func TestInviteLinkReadsTheBaseURLLive(t *testing.T) {
	e := newEnv(t)
	e.s.cfg.BaseURL = "https://moved.test/"
	link, err := e.s.CreateInvite(e.ctx, e.admin, "", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link.URL, "https://moved.test/invite/") {
		t.Errorf("invite URL %q does not start with the new base", link.URL)
	}
}
