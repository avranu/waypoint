package core

import (
	"database/sql"
	"strings"
	"testing"
)

// sessionOf reads the session columns of the row `where` picks.
func (e *env) sessionOf(table, where string, args ...any) (session, task sql.NullInt64) {
	e.t.Helper()
	q := `SELECT session_id, session_issue_id FROM ` + table + ` WHERE ` + where // #nosec G202 -- test tables are fixed
	if err := e.s.db.R.QueryRowContext(e.ctx, q, args...).Scan(&session, &task); err != nil {
		e.t.Fatalf("%s where %s: %v", table, where, err)
	}
	return session, task
}

// An agent's writes carry its session, and the task the session held, on
// the row; a person's carry neither (NULL, not 0).
func TestRowsCarryTheSessionThatWroteThem(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Acme", "")
	own := e.issue(e.admin, IssueInput{Project: p.Key, Title: "The agent's task"})
	other := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Somebody else's task"})
	agent := agentActor(e.admin, "anthropic/claude-code")
	sess, err := e.s.SessionHook(e.ctx, agent, []HookEvent{hook("box", "row-a", "SessionStart", func(ev *HookEvent) { ev.Task = own.Key })})
	if err != nil {
		t.Fatal(err)
	}
	agent.Session = sess.Session
	// The session holds its own task (a take does this in production).
	if _, err := e.s.db.W.ExecContext(e.ctx, `UPDATE agent_sessions SET issue_id = ? WHERE id = ?`, own.ID, sess.Session); err != nil {
		t.Fatal(err)
	}
	want := func(what string, session, task sql.NullInt64) {
		t.Helper()
		if !session.Valid || session.Int64 != sess.Session || !task.Valid || task.Int64 != own.ID {
			t.Errorf("%s: session %v task %v, want session %d task %d", what, session, task, sess.Session, own.ID)
		}
	}

	// A comment on a task that is not its own: countable per session.
	c, err := e.s.AddComment(e.ctx, agent, other.Key, "Seen on your task.")
	if err != nil {
		t.Fatal(err)
	}
	s1, t1 := e.sessionOf("comments", "id = ?", c.ID)
	want("comment", s1, t1)

	made := e.issue(agent, IssueInput{Project: p.Key, Title: "Found while there"})
	s2, t2 := e.sessionOf("issues", "id = ?", made.ID)
	want("issue", s2, t2)

	if err := e.s.AddRelation(e.ctx, agent, made.Key, "relates", other.Key); err != nil {
		t.Fatal(err)
	}
	s3, t3 := e.sessionOf("issue_relations", "kind = 'relates' AND (source_id = ? OR target_id = ?)", made.ID, made.ID)
	want("relation", s3, t3)

	pg, err := e.s.CreatePage(e.ctx, agent, PageInput{Title: ptr("Notes"), Body: ptr("x"), Project: Opt[string]{Set: true, Value: ptr(p.Key)}})
	if err != nil {
		t.Fatal(err)
	}
	s4, t4 := e.sessionOf("pages", "id = ?", pg.ID)
	want("page", s4, t4)

	note, err := e.s.CreateDesign(e.ctx, agent, DesignInput{Title: ptr("Plan"), Kind: ptr("note"), Body: ptr("b"), Project: ptr(p.Key)})
	if err != nil {
		t.Fatal(err)
	}
	s5, t5 := e.sessionOf("design_revisions", "note_id = ?", note.ID)
	want("design revision", s5, t5)

	if _, err := e.s.AddItemLink(e.ctx, agent, LinkInput{From: LinkEnd{Type: KIssue, Key: made.Key}, To: LinkEnd{Type: KDesign, ID: note.ID}, Rel: "relates"}); err != nil {
		t.Fatal(err)
	}
	s6, t6 := e.sessionOf("knowledge_links", "src_type = 'issue' AND src_id = ? AND rel = 'relates'", made.ID)
	want("knowledge link", s6, t6)

	// A person's writes record no session: unknown is NULL.
	pc, err := e.s.AddComment(e.ctx, e.admin, other.Key, "A person's.")
	if err != nil {
		t.Fatal(err)
	}
	if s, k := e.sessionOf("comments", "id = ?", pc.ID); s.Valid || k.Valid {
		t.Errorf("a person's comment has session %v task %v, want NULL", s, k)
	}
}

// A lookup is recorded with the session; a repeat inside the dedupe window is
// one row that counts the repeat, so rereads are not hidden.
func TestLookupCountsRepeatsPerSession(t *testing.T) {
	e := newEnv(t)
	g, err := e.s.CreateGlossary(e.ctx, e.admin, GlossaryInput{Term: ptr("Take"), Definition: ptr("One go at a task.")})
	if err != nil {
		t.Fatal(err)
	}
	a := agentActor(e.admin, "anthropic/claude-code")
	sa, err := e.s.SessionHook(e.ctx, a, []HookEvent{hook("box", "look-a", "SessionStart", nil)})
	if err != nil {
		t.Fatal(err)
	}
	a.Session = sa.Session
	for range 3 {
		e.s.NoteLookup(e.ctx, a, KGlossary, g.ID, "")
	}
	b := agentActor(e.admin, "anthropic/claude-code")
	sb, err := e.s.SessionHook(e.ctx, b, []HookEvent{hook("box", "look-b", "SessionStart", nil)})
	if err != nil {
		t.Fatal(err)
	}
	b.Session = sb.Session
	e.s.NoteLookup(e.ctx, b, KGlossary, g.ID, "") // the same person and agent, another session: its own read

	rows, err := e.s.db.R.QueryContext(e.ctx, `SELECT session_id, repeats FROM knowledge_uses WHERE kind = 'lookup' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s, r int64
		if err := rows.Scan(&s, &r); err != nil {
			t.Fatal(err)
		}
		got = append(got, itoa64(s)+":"+itoa64(r))
	}
	if want := itoa64(sa.Session) + ":2," + itoa64(sb.Session) + ":0"; strings.Join(got, ",") != want {
		t.Errorf("lookups (session:repeats) = %v, want %s", got, want)
	}
}

// Shown is recorded apart from looked up, per showing, with the session; a
// ref that names nothing is counted, not dropped silently.
func TestShownIsCountedApartFromLookedUp(t *testing.T) {
	e := newEnv(t)
	g, err := e.s.CreateGlossary(e.ctx, e.admin, GlossaryInput{Term: ptr("Claim"), Definition: ptr("A statement to check.")})
	if err != nil {
		t.Fatal(err)
	}
	a := agentActor(e.admin, "anthropic/claude-code")
	sa, err := e.s.SessionHook(e.ctx, a, []HookEvent{hook("box", "shown-a", "SessionStart", nil)})
	if err != nil {
		t.Fatal(err)
	}
	a.Session = sa.Session
	refs := []ShownRef{{Type: KGlossary, Ref: "Claim"}, {Type: KGlossary, Ref: "No such term"}}
	for range 2 {
		rec, unknown, err := e.s.NoteShown(e.ctx, a, "brief", refs)
		if err != nil || rec != 1 || unknown != 1 {
			t.Fatalf("NoteShown = %d recorded, %d unknown, %v; want 1, 1", rec, unknown, err)
		}
	}
	e.s.NoteLookup(e.ctx, a, KGlossary, g.ID, "")
	var shown, session, lookups int64
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT count(*), min(session_id) FROM knowledge_shown WHERE item_type = 'glossary' AND item_id = ?`, g.ID).Scan(&shown, &session); err != nil {
		t.Fatal(err)
	}
	if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT count(*) FROM knowledge_uses WHERE kind = 'lookup' AND item_id = ?`, g.ID).Scan(&lookups); err != nil {
		t.Fatal(err)
	}
	if shown != 2 || session != sa.Session || lookups != 1 {
		t.Errorf("shown %d (session %d), looked up %d; want 2 showings in session %d and 1 lookup", shown, session, lookups, sa.Session)
	}
	if _, _, err := e.s.NoteShown(e.ctx, a, "elsewhere", refs); !IsInvalid(err) {
		t.Errorf("an unknown via: %v, want invalid", err)
	}
}

// sessionExempt lists the tables that name an agent but carry no session_id,
// each with why. A table an agent writes through core.Service takes the
// columns instead (session_id, session_issue_id, filled by sessionStamp in
// row_session.go): TestAgentWritableTablesCarryASession fails for a new table
// that names an agent and is in neither place (WP-ADR145 point 6).
var sessionExempt = map[string]string{
	"agent_sessions":      "not written by an agent session on its own behalf (a session, token, mirror, log or setting)",
	"agent_usage_reports": "not written by an agent session on its own behalf (a session, token, mirror, log or setting)",
	"api_tokens":          "not written by an agent session on its own behalf (a session, token, mirror, log or setting)",
	"assistants":          "not written by an agent session on its own behalf (a session, token, mirror, log or setting)",
	"audit_log":           "not written by an agent session on its own behalf (a session, token, mirror, log or setting)",
	"automation_runs":     "not written by an agent session on its own behalf (a session, token, mirror, log or setting)",
	"cli_hours":           "not written by an agent session on its own behalf (a session, token, mirror, log or setting)",
	"deployments":         "not written by an agent session on its own behalf (a session, token, mirror, log or setting)",
	"device_logins":       "not written by an agent session on its own behalf (a session, token, mirror, log or setting)",
	"environments":        "not written by an agent session on its own behalf (a session, token, mirror, log or setting)",
	"prep_assignments":    "not written by an agent session on its own behalf (a session, token, mirror, log or setting)",
	"pull_requests":       "not written by an agent session on its own behalf (a session, token, mirror, log or setting)",
	"push_subscriptions":  "not written by an agent session on its own behalf (a session, token, mirror, log or setting)",
	"sessions":            "not written by an agent session on its own behalf (a session, token, mirror, log or setting)",
	"attachments":         "agent-written, not yet stamped: WP-1477",
	"code_audit_findings": "agent-written, not yet stamped: WP-1477",
	"design_notes":        "agent-written, not yet stamped: WP-1477",
	"external_links":      "agent-written, not yet stamped: WP-1477",
	"glossary":            "agent-written, not yet stamped: WP-1477",
	"glossary_revisions":  "agent-written, not yet stamped: WP-1477",
	"goal_dismissals":     "agent-written, not yet stamped: WP-1477",
	"issue_unlinks":       "agent-written, not yet stamped: WP-1477",
	"objective_rounds":    "agent-written, not yet stamped: WP-1477",
	"objectives":          "agent-written, not yet stamped: WP-1477",
	"page_issues":         "agent-written, not yet stamped: WP-1477",
	"pr_merge_after":      "agent-written, not yet stamped: WP-1477",
	"time_entries":        "agent-written, not yet stamped: WP-1477",
	"try_links":           "agent-written, not yet stamped: WP-1477",
}

func TestAgentWritableTablesCarryASession(t *testing.T) {
	e := newEnv(t)
	rows, err := e.s.db.R.QueryContext(e.ctx, `SELECT m.name,
		(SELECT count(*) FROM pragma_table_info(m.name) WHERE name = 'session_id'),
		(SELECT group_concat(name) FROM pragma_table_info(m.name) WHERE name LIKE '%agent%')
		FROM sqlite_master m WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' ORDER BY m.name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var has int
		var agentCols sql.NullString
		if err := rows.Scan(&name, &has, &agentCols); err != nil {
			t.Fatal(err)
		}
		if agentCols.Valid && has == 0 && sessionExempt[name] == "" {
			t.Errorf("table %s names an agent (%s) but has no session_id column: add session_id and session_issue_id and fill them with sessionStamp (row_session.go), or list it in sessionExempt with the reason", name, agentCols.String)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for name := range sessionExempt {
		var n int
		if err := e.s.db.R.QueryRowContext(e.ctx, `SELECT count(*) FROM pragma_table_info(?) WHERE name = 'session_id'`, name).Scan(&n); err != nil || n != 0 {
			t.Errorf("sessionExempt lists %s, which has a session_id column or does not exist: take it off the list", name)
		}
	}
}
