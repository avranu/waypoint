package core

import (
	"strings"
	"testing"
)

func TestRiskyCommandsIngestNotifyAndList(t *testing.T) {
	e := newAnalyticsEnv(t)
	uid := e.me.User.ID
	p := e.project(e.me, "Web", "")
	task := e.issue(e.me, IssueInput{Project: p.Key, Title: "Tidy the deploy"})
	if _, err := e.s.db.W.Exec(`INSERT INTO agent_sessions(user_id, machine, cli_id, issue_id, started_at, last_seen_at) VALUES (?,?,?,?,?,?)`,
		uid, "devhost", "s1", task.ID, e.hour(0), e.hour(0)); err != nil {
		t.Fatal(err)
	}
	now := e.clockNow().UnixMilli()
	risky := func(at int64, session, kind, level, outcome, source string) UsageRisky {
		return UsageRisky{At: at, Session: session, Repo: "acme/web", Model: "claude-opus-5-5", Kind: kind, Level: level,
			Label: kind + " label", Command: "git push --force origin main " + strings.Repeat("x", 400), Outcome: outcome, Source: source}
	}
	e.ingest(e.me, "a", "devhost", UsageChunk{Risky: []UsageRisky{
		risky(now-60_000, "s1", "force_push", "high", "ran", ""),
		risky(now-50_000, "s1", "delete", "medium", "ran", ""),
		risky(now-40_000, "s1", "deploy", "high", "refused", "classifier"),
		risky(now-30_000, "s2", "disk", "high", "error", ""),
		risky(now-20_000, "s1", "bogus", "high", "ran", ""),
		risky(now-10_000, "s1", "delete", "severe", "ran", ""),
	}})
	e.ingest(e.me, "a", "devhost", UsageChunk{Risky: []UsageRisky{risky(now-60_000, "s1", "force_push", "high", "ran", "")}})

	var n int
	var title, body, url string
	_ = e.s.db.R.QueryRow(`SELECT count(*), max(title), max(body), max(url) FROM notifications WHERE kind = ? AND user_id = ?`, NotifyRisky, uid).Scan(&n, &title, &body, &url)
	if n != 1 || title != "Agents ran 2 risky commands" || !strings.Contains(body, "force_push label in acme/web: git push --force") || strings.Contains(body, "deploy label") ||
		url != "/agents?tab=audit&view=risky" {
		t.Errorf("notification %d %q %q %q", n, title, body, url)
	}

	page, err := e.s.UsageRiskyCommands(e.ctx, e.me, UsageQuery{Days: 7}, UsageRiskyFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 4 || page.Counts == nil || page.Counts.High != 3 || page.Counts.Medium != 1 || page.Counts.Refused != 1 || page.Counts.Kinds["delete"] != 1 {
		t.Fatalf("page: %d rows, counts %+v", len(page.Rows), page.Counts)
	}
	first := page.Rows[len(page.Rows)-1]
	if first.Kind != "force_push" || first.Issue == nil || first.Issue.Key != task.Key || first.Command != "git push --force origin main "+strings.Repeat("x", 400) || first.Machine != "devhost" {
		t.Errorf("first: %+v", first)
	}
	if page.Rows[0].Issue != nil {
		t.Errorf("s2: %+v", page.Rows[0])
	}
	high, err := e.s.UsageRiskyCommands(e.ctx, e.me, UsageQuery{Days: 7}, UsageRiskyFilter{Level: "high", Outcome: "ran"})
	if err != nil || len(high.Rows) != 2 {
		t.Errorf("high that ran: %+v %v", high, err)
	}
	older, err := e.s.UsageRiskyCommands(e.ctx, e.me, UsageQuery{Days: 7}, UsageRiskyFilter{Before: page.Rows[1].ID, Limit: 1})
	if err != nil || len(older.Rows) != 1 || older.Rows[0].ID != page.Rows[2].ID || older.Counts != nil {
		t.Errorf("older: %+v %v", older, err)
	}
	if _, err := e.s.UsageRiskyCommands(e.ctx, e.me, UsageQuery{Days: 7}, UsageRiskyFilter{Kind: "nope"}); err == nil {
		t.Error("an unknown kind is an error")
	}
	sam, err := e.s.UsageRiskyCommands(e.ctx, e.user("sam", false), UsageQuery{Days: 7}, UsageRiskyFilter{})
	if err != nil || len(sam.Rows) != 0 || sam.Counts.High != 0 {
		t.Errorf("sam: %+v %v", sam, err)
	}
}

func TestRiskyCommandsOldHistoryHasNoTaskOrNotification(t *testing.T) {
	e := newAnalyticsEnv(t)
	uid := e.me.User.ID
	p := e.project(e.me, "Web", "")
	task := e.issue(e.me, IssueInput{Project: p.Key, Title: "Now"})
	if _, err := e.s.db.W.Exec(`INSERT INTO agent_sessions(user_id, machine, cli_id, issue_id, started_at, last_seen_at) VALUES (?,?,?,?,?,?)`,
		uid, "devhost", "s1", task.ID, e.hour(0), e.hour(0)); err != nil {
		t.Fatal(err)
	}
	at := e.hour(-48)
	e.ingest(e.me, "b", "devhost", UsageChunk{Risky: []UsageRisky{{At: at, Session: "s1", Kind: "power", Level: "high", Label: "reboot", Command: "sudo reboot", Outcome: "ran"}}})
	var n int
	_ = e.s.db.R.QueryRow(`SELECT count(*) FROM notifications WHERE kind = ?`, NotifyRisky).Scan(&n)
	page, err := e.s.UsageRiskyCommands(e.ctx, e.me, UsageQuery{Days: 7}, UsageRiskyFilter{})
	if err != nil || n != 0 || len(page.Rows) != 1 || page.Rows[0].Issue != nil {
		t.Errorf("history: %d notifications, %+v %v", n, page, err)
	}
}
