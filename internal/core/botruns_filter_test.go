package core

import "testing"

// The Bots page narrows runs by status and rule, and pages from an id.
func TestListBotRunsFiltered(t *testing.T) {
	pe := newPREnv(t, "github")
	pe.llm.answers["opencode/muse-spark-1.3-contributor-free"] = oneFinding
	pe.pullHook("opened", ForgePR{Number: 5, Title: "Fix login", Author: "sam", HeadRef: "fix-login", HeadSHA: "1111111aaaa", State: "open"})
	pe.pullHook("opened", ForgePR{Number: 6, Title: "Docs", Author: "sam", HeadRef: "docs", HeadSHA: "2222222bbbb", State: "open"})
	// Both runs are queued: live.
	list := func(f BotRunFilter) []*BotRun {
		rs, err := pe.s.ListBotRunsFiltered(pe.ctx, pe.admin, pe.p.Key, f)
		if err != nil {
			t.Fatal(err)
		}
		return rs
	}
	if n := len(list(BotRunFilter{Status: "live"})); n != 2 {
		t.Fatalf("live before = %d", n)
	}
	if n := len(list(BotRunFilter{Status: "done"})); n != 0 {
		t.Fatalf("done before = %d", n)
	}
	pe.drain()
	if n := len(list(BotRunFilter{Status: "live"})); n != 0 {
		t.Errorf("live after = %d", n)
	}
	all := list(BotRunFilter{Status: "all"})
	if len(all) != 2 || len(list(BotRunFilter{Status: "done"})) != 2 || len(list(BotRunFilter{Status: "failed"})) != 0 {
		t.Fatalf("all %d, done %d, failed %d", len(all), len(list(BotRunFilter{Status: "done"})), len(list(BotRunFilter{Status: "failed"})))
	}
	if rs := list(BotRunFilter{Before: all[0].ID}); len(rs) != 1 || rs[0].ID != all[1].ID {
		t.Errorf("before = %+v", rs)
	}
	if rs := list(BotRunFilter{Automation: 999999}); len(rs) != 0 {
		t.Errorf("another rule's runs = %d", len(rs))
	}
	if all[0].AutomationID != nil {
		if rs := list(BotRunFilter{Automation: *all[0].AutomationID}); len(rs) == 0 {
			t.Error("a rule's own runs are missing")
		}
	}
}
