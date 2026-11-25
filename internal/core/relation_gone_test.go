package core

import (
	"encoding/json"
	"testing"
)

// relationRemovedEvents reads the removal events of an issue's relations.
func relationRemovedEvents(t *testing.T, e *env, issue int64) (olds []string, datas []map[string]any) {
	t.Helper()
	rows, err := e.s.db.R.QueryContext(e.ctx, `SELECT old_value, data FROM events WHERE issue_id = ? AND kind = ? AND field = 'relation' AND old_value != '' ORDER BY id`, issue, EvIssueUpdated)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var old, raw string
		if err := rows.Scan(&old, &raw); err != nil {
			t.Fatal(err)
		}
		d := map[string]any{}
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatal(err)
		}
		olds, datas = append(olds, old), append(datas, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return olds, datas
}

func TestRemoveRelationKeepsWhatWasRemoved(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "kanban")
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "A"})
	b := e.issue(e.admin, IssueInput{Project: p.Key, Title: "B"})
	res, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO issue_relations(source_id, target_id, kind, subkind, created_at, basis, score, created_agent)
		VALUES (?, ?, 'follows', 'fix', 1000, 'inferred', 0.75, 'bot')`, a.ID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if err := e.s.RemoveRelation(e.ctx, e.admin, b.Key, id); err != nil {
		t.Fatal(err)
	}
	olds, datas := relationRemovedEvents(t, e, b.ID)
	if len(olds) != 1 {
		t.Fatalf("want one removal event, got %v", olds)
	}
	if olds[0] != "followed_by (fix) "+a.Key {
		t.Errorf("old = %q", olds[0])
	}
	d := datas[0]
	for k, want := range map[string]any{"kind": "follows", "subkind": "fix", "other": a.Key, "direction": "target", "basis": "inferred",
		"created_at": float64(1000), "created_agent": "bot", "score": 0.75, "relation_id": float64(id)} {
		if d[k] != want {
			t.Errorf("data[%s] = %v, want %v", k, d[k], want)
		}
	}
	if age, _ := d["age_ms"].(float64); age <= 0 {
		t.Errorf("age_ms = %v", d["age_ms"])
	}
	if _, ok := d["by"]; ok {
		t.Errorf("a person's removal isn't automatic: %v", d)
	}
	if err := e.s.RemoveRelation(e.ctx, e.admin, b.Key, id); err == nil {
		t.Error("removing it twice should fail")
	}
}

func TestDroppedTextRelationKeepsWhatWasRemoved(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "kanban")
	b := e.issue(e.admin, IssueInput{Project: p.Key, Title: "B"})
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "A", Description: "Blocked by " + b.Key + "."})
	var n int
	if err := e.s.db.R.QueryRow(`SELECT COUNT(*) FROM issue_relations WHERE named_in = ?`, a.ID).Scan(&n); err != nil || n == 0 {
		t.Fatalf("text made no relation (n=%d, err=%v)", n, err)
	}
	none := "Nothing to see here."
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, a.Key, IssuePatch{Description: &none}); err != nil {
		t.Fatal(err)
	}
	olds, datas := relationRemovedEvents(t, e, a.ID)
	if len(olds) != 1 {
		t.Fatalf("want one removal event, got %v", olds)
	}
	d := datas[0]
	if d["by"] != "automatic" || d["reason"] == "" || d["other"] != b.Key || d["basis"] != "inferred" || d["kind"] == "" || d["created_at"] == nil {
		t.Errorf("data = %v", d)
	}
	if olds[0] == "removed" || olds[0] == "" {
		t.Errorf("old = %q", olds[0])
	}
}

// A relation someone chooses takes the place of the link the text made; the
// replaced one is recorded as a removal, with what replaced it.
func TestReplacedTextRelationKeepsWhatWasRemoved(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "kanban")
	b := e.issue(e.admin, IssueInput{Project: p.Key, Title: "B"})
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "A", Description: "See " + b.Key + "."})
	var n int
	if err := e.s.db.R.QueryRow(`SELECT COUNT(*) FROM issue_relations WHERE named_in = ? AND kind = 'relates'`, a.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("text made %d plain relations, err=%v", n, err)
	}
	if _, err := e.s.Relate(e.ctx, e.admin, a.Key, RelationInput{Kind: "relates", Issue: b.Key}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.db.R.QueryRow(`SELECT COUNT(*) FROM issue_relations WHERE (source_id = ? OR target_id = ?)`, a.ID, a.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d relations after replacing, err=%v", n, err)
	}
	olds, datas := relationRemovedEvents(t, e, a.ID)
	if len(olds) != 1 || olds[0] != "relates "+b.Key {
		t.Fatalf("removal events = %v", olds)
	}
	d := datas[0]
	if d["reason"] != "replaced by relates" || d["basis"] != "inferred" || d["other"] != b.Key || d["relation_id"] == nil {
		t.Errorf("data = %v", d)
	}
	if _, ok := d["by"]; ok {
		t.Errorf("the person replaced it, nothing automatic: %v", d)
	}
}

// eventsNaming counts the recorded events of an issue that carry key in their
// old or new text or their data.
func eventsNaming(t *testing.T, e *env, issue int64, key string) (n int) {
	t.Helper()
	if err := e.s.db.R.QueryRow(`SELECT COUNT(*) FROM events WHERE issue_id = ? AND field = 'relation' AND (old_value LIKE '%'||?||'%' OR new_value LIKE '%'||?||'%' OR data LIKE '%'||?||'%')`,
		issue, key, key, key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A draft stays hidden (WP-42): removing a relation to one is recorded on the
// draft, so the published issue's history doesn't carry the draft's key.
func TestRemovingARelationToADraftStaysOffThePublishedIssue(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "kanban")
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "A"})
	d := e.issue(e.admin, IssueInput{Project: p.Key, Title: "D", Agent: "claude", Draft: true})
	if !d.Draft {
		t.Fatal("not a draft")
	}
	if _, err := e.s.Relate(e.ctx, e.admin, a.Key, RelationInput{Kind: "relates", Issue: d.Key}); err != nil {
		t.Fatal(err)
	}
	var rel int64
	if err := e.s.db.R.QueryRow(`SELECT id FROM issue_relations WHERE source_id = ? OR target_id = ?`, a.ID, a.ID).Scan(&rel); err != nil {
		t.Fatal(err)
	}
	if err := e.s.RemoveRelation(e.ctx, e.admin, a.Key, rel); err != nil {
		t.Fatal(err)
	}
	if n := eventsNaming(t, e, a.ID, d.Key); n != 0 {
		t.Errorf("%d events on the published issue name the draft %s", n, d.Key)
	}
	olds, datas := relationRemovedEvents(t, e, d.ID)
	if len(olds) != 1 || olds[0] != "relates "+a.Key || datas[0]["other"] != a.Key {
		t.Errorf("the draft's removal events = %v %v", olds, datas)
	}
}

// The same when the text that named the draft stops naming it.
func TestDroppingATextRelationToADraftStaysOffThePublishedIssue(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "kanban")
	d := e.issue(e.admin, IssueInput{Project: p.Key, Title: "D", Agent: "claude", Draft: true})
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "A", Description: "Blocked by " + d.Key + "."})
	var n int
	if err := e.s.db.R.QueryRow(`SELECT COUNT(*) FROM issue_relations WHERE named_in = ?`, a.ID).Scan(&n); err != nil || n == 0 {
		t.Skipf("text naming a draft makes no relation here (n=%d, err=%v)", n, err)
	}
	none := "Nothing to see here."
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, a.Key, IssuePatch{Description: &none}); err != nil {
		t.Fatal(err)
	}
	if got := eventsNaming(t, e, a.ID, d.Key); got != 0 {
		t.Errorf("%d events on the published issue name the draft %s", got, d.Key)
	}
	if olds, _ := relationRemovedEvents(t, e, d.ID); len(olds) != 1 {
		t.Errorf("the draft's removal events = %v", olds)
	}
}
