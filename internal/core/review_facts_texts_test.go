package core

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// The notice of a ship that can't go through says what the round did from
// roundFacts (its audit found how many), not that its review "is done".
func TestShipTriedNoticeTakesTheRoundsFacts(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	round, now := e.roundForShipTries()
	e.s.shipTried(e.ctx, round, e.task.Key, now, errors.New("forge unavailable"))
	var body string
	if err := e.s.db.R.QueryRow(`SELECT body FROM notifications WHERE title LIKE 'Waypoint can''t open%'`).Scan(&body); err != nil {
		t.Fatalf("no notice: %v", err)
	}
	facts, _ := e.s.factsOfRound(e.ctx, round)
	if facts == "" || !strings.Contains(body, facts) || strings.Contains(body, "review is done") {
		t.Errorf("notice %q doesn't carry the round's facts %q", body, facts)
	}
}

// heldUnshipped says what the round found too.
func TestHeldUnshippedTakesTheRoundsFacts(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	round, _ := e.roundForShipTries()
	e.exec(`UPDATE work_log SET state = 'done' WHERE id = ?`, round)
	var run int64
	if err := e.s.db.R.QueryRow(`SELECT run_id FROM work_log WHERE id = ?`, round).Scan(&run); err != nil {
		t.Fatal(err)
	}
	st := &stepRow{runID: sql.NullInt64{Int64: run, Valid: true}, reviewID: sql.NullInt64{Int64: round, Valid: true}}
	why, err := e.s.heldUnshipped(e.ctx, st)
	facts, _ := e.s.factsOfRound(e.ctx, round)
	if err != nil || facts == "" || !strings.Contains(why, facts) {
		t.Errorf("heldUnshipped = %q, %v; facts %q", why, err, facts)
	}
}
