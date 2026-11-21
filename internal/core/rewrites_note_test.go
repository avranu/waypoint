package core

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// rewriteFixture is a task b whose change replaces most of task a's pull
// request 413, merged three days ago.
type rewriteFixture struct {
	*escapeEnv
	a, b *IssueDetail
	prs  []RewrittenPR
	// session of the round f.round() returns, once made.
	session int64
}

func newRewriteFixture(t *testing.T) *rewriteFixture {
	t.Helper()
	e := newEscapeEnv(t)
	day := 24 * time.Hour
	a := e.issue(e.admin, IssueInput{Project: e.proj.Key, Title: "Accepting a request adds an allow rule"})
	e.link(t, a.ID, 413)
	b := e.issue(e.admin, IssueInput{Project: e.proj.Key, Title: "Refused calls are logged"})
	return &rewriteFixture{escapeEnv: e, a: a, b: b,
		prs: []RewrittenPR{{Number: 413, Lines: 181, Share: 0.96, At: e.s.now().Add(-3 * day).UnixMilli()}}}
}

// round is a review round of b, in a session of the admin.
func (f *rewriteFixture) round() *roundRow {
	if f.session == 0 {
		res, err := f.s.db.W.ExecContext(f.ctx, `INSERT INTO agent_sessions(user_id, machine, cli_id, issue_id, started_at, last_seen_at) VALUES (?, 'box', 'c1', ?, 1, 1)`, f.admin.User.ID, f.b.ID)
		if err != nil {
			f.env.t.Fatal(err)
		}
		f.session, _ = res.LastInsertId()
	}
	return &roundRow{id: 1, issueID: f.b.ID, sessionID: sql.NullInt64{Int64: f.session, Valid: true}}
}

func (f *rewriteFixture) relations(t *testing.T, key string) []Relation {
	t.Helper()
	return f.get(f.admin, key).Relations
}

func TestNoteRewritesRelatesOnceAsInferred(t *testing.T) {
	f := newRewriteFixture(t)
	got, err := f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, "o/r", "head1", f.prs)
	if err != nil || len(got) != 1 || got[0].Key != f.a.Key || !got[0].Recorded {
		t.Fatalf("got %+v, %v", got, err)
	}
	rels := f.relations(t, f.b.Key)
	if len(rels) != 1 || rels[0].Kind != "relates" || rels[0].Issue.Key != f.a.Key || rels[0].Basis != "inferred" || rels[0].Found != "rewrite" {
		t.Fatalf("relations = %+v", rels)
	}
	var lines int
	var head, note string
	if err := f.s.db.R.QueryRowContext(f.ctx, `SELECT lines, head, note FROM rewrite_evidence WHERE issue_id = ? AND other_id = ?`, f.b.ID, f.a.ID).Scan(&lines, &head, &note); err != nil {
		t.Fatalf("evidence: %v", err)
	}
	if lines != 181 || head != "head1" || note != "replaces 181 lines from PR 413, 96% of the lines it replaces" {
		t.Errorf("evidence = %d %q %q", lines, head, note)
	}
	// The history says what Waypoint saw.
	var data string
	if err := f.s.db.R.QueryRowContext(f.ctx, `SELECT data FROM events WHERE issue_id = ? AND field = 'relation' ORDER BY id DESC LIMIT 1`, f.b.ID).Scan(&data); err != nil || !strings.Contains(data, "96% of the lines it replaces") {
		t.Errorf("event data = %q, %v", data, err)
	}

	// A later ship (new head) reads it again: still one relation, not
	// recorded again, the evidence the newer one.
	f.prs[0].Lines = 190
	got, err = f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, "o/r", "head2", f.prs)
	if err != nil || len(got) != 1 || got[0].Recorded {
		t.Fatalf("second ship: %+v, %v", got, err)
	}
	if rels := f.relations(t, f.b.Key); len(rels) != 1 {
		t.Fatalf("relations after the second ship = %+v", rels)
	}
	if err := f.s.db.R.QueryRowContext(f.ctx, `SELECT lines, head FROM rewrite_evidence WHERE issue_id = ? AND other_id = ?`, f.b.ID, f.a.ID).Scan(&lines, &head); err != nil || lines != 190 || head != "head2" {
		t.Errorf("evidence after the second ship = %d %q, %v", lines, head, err)
	}
}

func TestNoteRewritesLeavesAnExistingRelationAlone(t *testing.T) {
	for _, kind := range []string{"relates", "follows", "supersedes", "tension", "blocks"} {
		f := newRewriteFixture(t)
		if err := f.s.AddRelation(f.ctx, f.admin, f.b.Key, kind, f.a.Key); err != nil {
			t.Fatal(kind, err)
		}
		before := f.relations(t, f.b.Key)
		got, err := f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, "o/r", "h", f.prs)
		if err != nil || len(got) != 0 {
			t.Errorf("%s: named %+v, %v", kind, got, err)
		}
		after := f.relations(t, f.b.Key)
		if len(after) != 1 || after[0].ID != before[0].ID || after[0].Kind != before[0].Kind || after[0].Basis != before[0].Basis {
			t.Errorf("%s: relations %+v, were %+v", kind, after, before)
		}
		var n int
		_ = f.s.db.R.QueryRowContext(f.ctx, `SELECT count(*) FROM rewrite_evidence`).Scan(&n)
		if n != 0 {
			t.Errorf("%s: %d evidence rows for a pair someone related", kind, n)
		}
	}
}

func TestRelatingAgainReplacesTheInferredRelation(t *testing.T) {
	for _, kind := range []string{"supersedes", "follows", "tension", "relates"} {
		f := newRewriteFixture(t)
		if _, err := f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, "o/r", "h", f.prs); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.Relate(f.ctx, f.admin, f.b.Key, RelationInput{Kind: kind, Issue: f.a.Key}); err != nil {
			t.Fatal(kind, err)
		}
		rels := f.relations(t, f.b.Key)
		if len(rels) != 1 || rels[0].Kind != kind || rels[0].Found == "rewrite" || rels[0].Basis == "inferred" {
			t.Errorf("%s: relations = %+v", kind, rels)
		}
		// And a later ship doesn't put the inferred one back.
		got, err := f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, "o/r", "h2", f.prs)
		if err != nil || len(got) != 0 {
			t.Errorf("%s: later ship named %+v, %v", kind, got, err)
		}
		if rels := f.relations(t, f.b.Key); len(rels) != 1 || rels[0].Kind != kind {
			t.Errorf("%s: relations after a later ship = %+v", kind, rels)
		}
	}
}

func TestNoteRewritesBelowTheBarRecordsNothing(t *testing.T) {
	f := newRewriteFixture(t)
	f.prs[0].Lines, f.prs[0].Share = 14, 0.9
	if got, err := f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, "o/r", "h", f.prs); err != nil || len(got) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
	f.prs[0] = RewrittenPR{Number: 413, Lines: 90, Share: 0.9, At: f.s.now().Add(-20 * 24 * time.Hour).UnixMilli()} // too old
	if got, err := f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, "o/r", "h", f.prs); err != nil || len(got) != 0 {
		t.Fatalf("old work: got %+v, %v", got, err)
	}
	var n int
	_ = f.s.db.R.QueryRowContext(f.ctx, `SELECT (SELECT count(*) FROM rewrite_evidence) + (SELECT count(*) FROM issue_relations)`).Scan(&n)
	if n != 0 {
		t.Errorf("%d rows written", n)
	}
}

func TestNoteRewritesSkipsOwnFamilyAndInvisibleTasks(t *testing.T) {
	f := newRewriteFixture(t)
	child := f.issue(f.admin, IssueInput{Project: f.proj.Key, Title: "Its subtask", Parent: f.b.Key})
	f.link(t, child.ID, 414)
	parent := f.issue(f.admin, IssueInput{Project: f.proj.Key, Title: "Its parent"})
	f.link(t, parent.ID, 415)
	if _, err := f.s.db.W.ExecContext(f.ctx, `UPDATE issues SET parent_id = ? WHERE id = ?`, parent.ID, f.b.ID); err != nil {
		t.Fatal(err)
	}
	lee := f.user("lee", false)
	sam := f.user("sam", false)
	lp := f.project(lee, "Lee", "kanban")
	sp := f.project(sam, "Sam", "kanban")
	hidden := f.issue(sam, IssueInput{Project: sp.Key, Title: "Hidden"})
	f.link(t, hidden.ID, 416)
	mine := f.issue(lee, IssueInput{Project: lp.Key, Title: "Lee's change"})
	at := f.s.now().Add(-24 * time.Hour).UnixMilli()
	big := func(n int) RewrittenPR { return RewrittenPR{Number: n, Lines: 80, Share: 0.5, At: at} }

	got, err := f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, "o/r", "h", []RewrittenPR{big(414), big(415)})
	if err != nil || len(got) != 0 {
		t.Fatalf("a subtask's and a parent's work: %+v, %v", got, err)
	}
	got, err = f.s.NoteRewrites(f.ctx, lee, mine.Key, "o/r", "h", []RewrittenPR{big(416)})
	if err != nil || len(got) != 0 {
		t.Fatalf("a task the actor can't see: %+v, %v", got, err)
	}
	var n int
	_ = f.s.db.R.QueryRowContext(f.ctx, `SELECT (SELECT count(*) FROM rewrite_evidence) + (SELECT count(*) FROM issue_relations WHERE found = 'rewrite')`).Scan(&n)
	if n != 0 {
		t.Errorf("%d rows written for them", n)
	}
}

func askOf(h *AuditHelpers, check string) string {
	for _, p := range h.Panel {
		if p.Name == check {
			return p.Ask
		}
	}
	return ""
}

// The round's check of earlier work undone is handed each rewrite as a
// lead, and only while the inferred relation stands.
func TestRoundsRegressionCheckGetsTheRewrites(t *testing.T) {
	f := newRewriteFixture(t)
	if _, err := f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, "o/r", "h", f.prs); err != nil {
		t.Fatal(err)
	}
	h, err := f.s.roundHelpers(f.ctx, f.round(), "")
	if err != nil {
		t.Fatal(err)
	}
	want := "this change replaces 181 lines that " + f.a.Key + " (\"Accepting a request adds an allow rule\") added in PR 413, 3 days ago: does it undo what " + f.a.Key + " did, on purpose?"
	if ask := askOf(h, "regressions"); !strings.Contains(ask, want) || !strings.HasSuffix(ask, panelRules) {
		t.Errorf("regressions ask:\n%s\nwant %q before the panel rules", ask, want)
	}
	for _, other := range []string{"complete", "quality", "claims"} {
		if strings.Contains(askOf(h, other), "181 lines") {
			t.Errorf("%s was handed the rewrite", other)
		}
	}
	// The agent said what it was: nothing to ask the reviewer.
	if _, err := f.s.Relate(f.ctx, f.admin, f.b.Key, RelationInput{Kind: "supersedes", Issue: f.a.Key}); err != nil {
		t.Fatal(err)
	}
	h, _ = f.s.roundHelpers(f.ctx, f.round(), "")
	if strings.Contains(askOf(h, "regressions"), "181 lines") {
		t.Error("the lead stayed after the agent related the tasks")
	}
}

func TestRoundsCompletenessCheckGetsThePutOffNotes(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Open", "")
	task := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Handle 400s", Description: "Retry on a 400. Step 2 only if real 400s show it's needed."})
	plain := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Rename a flag", Description: "Rename --x to --y."})
	h, err := e.s.roundHelpers(e.ctx, &roundRow{id: 1, issueID: task.ID}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := `the task says: "Step 2 only if real 400s show it's needed." (description): is this done in the change, or filed as a task?`
	if ask := askOf(h, "complete"); !strings.Contains(ask, want) || !strings.HasSuffix(ask, panelRules) {
		t.Errorf("complete ask:\n%s\nwant %q", ask, want)
	}
	if strings.Contains(askOf(h, "regressions"), "the task says") {
		t.Error("the regressions check was handed put-off notes")
	}
	h, _ = e.s.roundHelpers(e.ctx, &roundRow{id: 1, issueID: plain.ID}, "")
	if strings.Contains(askOf(h, "complete"), "the task says") {
		t.Error("a task with nothing put off got leads")
	}
}

// wp show's relations carry what Waypoint saw, and read as inferred; the
// event is the one an ordinary wp relate records.
func TestNoteRewritesShowsAsInferredWithItsNote(t *testing.T) {
	f := newRewriteFixture(t)
	if _, err := f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, "o/r", "h", f.prs); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{f.b.Key, f.a.Key} { // seen from either end
		rels := f.relations(t, key)
		if len(rels) != 1 || rels[0].Basis != "inferred" || rels[0].Note != "replaces 181 lines from PR 413, 96% of the lines it replaces" {
			t.Errorf("%s: relations = %+v", key, rels)
		}
	}
	var kind, field string
	if err := f.s.db.R.QueryRowContext(f.ctx, `SELECT kind, field FROM events WHERE issue_id = ? AND field = 'relation'`, f.b.ID).Scan(&kind, &field); err != nil {
		t.Fatal(err)
	}
	g := newRewriteFixture(t)
	if err := g.s.AddRelation(g.ctx, g.admin, g.b.Key, "relates", g.a.Key); err != nil {
		t.Fatal(err)
	}
	var want string
	if err := g.s.db.R.QueryRowContext(g.ctx, `SELECT kind FROM events WHERE issue_id = ? AND field = 'relation'`, g.b.ID).Scan(&want); err != nil || kind != want {
		t.Errorf("event kind = %q, wp relate's = %q (%v)", kind, want, err)
	}
}

// The relation, its evidence and its event are one transaction: when the
// evidence can't be written, nothing is.
func TestNoteRewritesIsOneTransaction(t *testing.T) {
	f := newRewriteFixture(t)
	if _, err := f.s.db.W.ExecContext(f.ctx, `ALTER TABLE rewrite_evidence RENAME TO rewrite_evidence_away`); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, "o/r", "h", f.prs)
	if _, e := f.s.db.W.ExecContext(f.ctx, `ALTER TABLE rewrite_evidence_away RENAME TO rewrite_evidence`); e != nil {
		t.Fatal(e)
	}
	if err == nil {
		t.Fatal("no error with the evidence table gone")
	}
	if rels := f.relations(t, f.b.Key); len(rels) != 0 {
		t.Errorf("relation left behind: %+v", rels)
	}
	var n int
	_ = f.s.db.R.QueryRowContext(f.ctx, `SELECT count(*) FROM events WHERE issue_id = ? AND field = 'relation'`, f.b.ID).Scan(&n)
	if n != 0 {
		t.Errorf("%d relation events left behind", n)
	}
}

// A person who removes the inferred relation by hand isn't given it again,
// and neither is one who relates the pair and later removes that.
func TestNoteRewritesDoesNotRecreateARemovedRelation(t *testing.T) {
	for _, how := range []string{"removed", "replaced then removed"} {
		f := newRewriteFixture(t)
		if _, err := f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, "o/r", "h", f.prs); err != nil {
			t.Fatal(err)
		}
		if how != "removed" {
			if _, err := f.s.Relate(f.ctx, f.admin, f.b.Key, RelationInput{Kind: "supersedes", Issue: f.a.Key}); err != nil {
				t.Fatal(err)
			}
		}
		rels := f.relations(t, f.b.Key)
		if err := f.s.RemoveRelation(f.ctx, f.admin, f.b.Key, rels[len(rels)-1].ID); err != nil {
			t.Fatal(err)
		}
		if how == "removed" {
			// Only the inferred one existed.
			if rels := f.relations(t, f.b.Key); len(rels) != 0 {
				t.Fatalf("%s: %+v", how, rels)
			}
		}
		got, err := f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, "o/r", "h2", f.prs)
		if err != nil || len(got) != 0 {
			t.Errorf("%s: named %+v, %v", how, got, err)
		}
		if rels := f.relations(t, f.b.Key); len(rels) != 0 {
			t.Errorf("%s: relation made again: %+v", how, rels)
		}
		h, _ := f.s.roundHelpers(f.ctx, f.round(), "")
		if strings.Contains(askOf(h, "regressions"), "181 lines") {
			t.Errorf("%s: the lead stayed", how)
		}
	}
}

// What the round is told is what the latest ship saw: a pair the change no
// longer replaces drops its evidence (the relation stays), and evidence an
// older wp took without its commit says it may be out of date.
func TestRewriteLeadsFollowTheLatestShip(t *testing.T) {
	f := newRewriteFixture(t)
	if _, err := f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, "o/r", "", f.prs); err != nil {
		t.Fatal(err)
	}
	h, _ := f.s.roundHelpers(f.ctx, f.round(), "")
	if ask := askOf(h, "regressions"); !strings.Contains(ask, "may be out of date") {
		t.Errorf("no head, no caveat:\n%s", ask)
	}
	if _, err := f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, "o/r", "h2", f.prs); err != nil {
		t.Fatal(err)
	}
	h, _ = f.s.roundHelpers(f.ctx, f.round(), "")
	if ask := askOf(h, "regressions"); !strings.Contains(ask, "181 lines") || strings.Contains(ask, "may be out of date") {
		t.Errorf("a head given:\n%s", ask)
	}
	// The next ship's change no longer touches that work.
	if _, err := f.s.NoteRewrites(f.ctx, f.admin, f.b.Key, "o/r", "h3", nil); err != nil {
		t.Fatal(err)
	}
	h, _ = f.s.roundHelpers(f.ctx, f.round(), "")
	if strings.Contains(askOf(h, "regressions"), "181 lines") {
		t.Error("the lead outlived the change that replaced those lines")
	}
	if rels := f.relations(t, f.b.Key); len(rels) != 1 {
		t.Errorf("relation = %+v", rels)
	}
}
