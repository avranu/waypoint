package core

import (
	"strings"
	"testing"
)

func TestShipReply(t *testing.T) {
	pe := newPREnv(t, "github")
	n, err := pe.s.GetShipReply(pe.ctx, pe.admin)
	if err != nil || n.Custom || n.Reply != DefaultShipReply {
		t.Fatalf("default: %+v %v", n, err)
	}
	agent := &Actor{User: pe.admin.User, Agent: "anthropic/claude-code"}
	if got := pe.s.ShipReplyTemplate(pe.ctx, agent); got != DefaultShipReply {
		t.Errorf("an agent's template = %q", got)
	}
	sam := pe.user("sam", false)
	if _, err := pe.s.GetShipReply(pe.ctx, sam); err == nil {
		t.Error("a non-admin saw the settings")
	}
	if _, err := pe.s.SetShipReply(pe.ctx, sam, "x"); err == nil {
		t.Error("a non-admin worded it")
	}
	if _, err := pe.s.SetShipReply(pe.ctx, agent, "x"); err == nil {
		t.Error("an agent worded it")
	}
	if _, err := pe.s.SetShipReply(pe.ctx, pe.admin, strings.Repeat("x", maxShipReply+1)); err == nil {
		t.Error("too long")
	}
	n, err = pe.s.SetShipReply(pe.ctx, pe.admin, "  {task} shipped in #{pr}.\r\nClear away.\n")
	if err != nil || !n.Custom || n.Reply != "{task} shipped in #{pr}.\nClear away." {
		t.Fatalf("set: %+v %v", n, err)
	}
	if got := pe.s.ShipReplyTemplate(pe.ctx, agent); got != n.Reply {
		t.Errorf("template after set = %q", got)
	}
	if n, err = pe.s.SetShipReply(pe.ctx, pe.admin, DefaultShipReply); err != nil || n.Custom {
		t.Errorf("the default itself: %+v %v", n, err)
	}
	if a := pe.audits(); len(a) != 2 || !strings.HasPrefix(a[0], "settings.ship_reply by /") {
		t.Errorf("audit = %v", a)
	}
}
