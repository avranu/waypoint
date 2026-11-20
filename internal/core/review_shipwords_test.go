package core

import (
	"strings"
	"testing"
)

// shipWords is the message and summary the round holds for the pull request
// Waypoint opens (WP-1370).
func (e *loopEnv) shipWords() (message, summary string) {
	e.t.Helper()
	var id int64
	if err := e.s.db.R.QueryRow(`SELECT max(id) FROM work_log WHERE kind = 'review'`).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	bp := roundBeforePR(e.ctx, e.s.db.R, id)
	if bp == nil {
		e.t.Fatal("the round holds no before-PR record")
	}
	return bp.Message, bp.Summary
}

func shipWith(message, summary *string) func(*OpenPRInput) {
	return func(in *OpenPRInput) { in.Message, in.Summary = message, summary }
}

// B: a ship that gives no -m or --summary keeps what the round holds; one
// that gives them empty clears them; the newest words win.
func TestShipWordsAbsentKeepsEmptyClears(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	if res, err := e.ship(e.task.Key, ShipAuto, shipWith(ptrOf("did it"), ptrOf("All done."))); err != nil || res.Held == "" {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	if m, s := e.shipWords(); m != "did it" || s != "All done." {
		t.Fatalf("after the first ship: %q, %q", m, s)
	}
	if _, err := e.ship(e.task.Key, ShipAuto, shipWith(nil, nil)); err != nil {
		t.Fatal(err)
	}
	if m, s := e.shipWords(); m != "did it" || s != "All done." {
		t.Errorf("a ship that gave neither changed them: %q, %q", m, s)
	}
	if _, err := e.ship(e.task.Key, ShipAuto, shipWith(ptrOf(""), ptrOf(""))); err != nil {
		t.Fatal(err)
	}
	if m, s := e.shipWords(); m != "" || s != "" {
		t.Errorf("empty didn't clear them: %q, %q", m, s)
	}
	if _, err := e.ship(e.task.Key, ShipAuto, shipWith(ptrOf("again"), nil)); err != nil {
		t.Fatal(err)
	}
	if m, s := e.shipWords(); m != "again" || s != "" {
		t.Errorf("the newest ship's message should win and the summary stay cleared: %q, %q", m, s)
	}
}

// B: what is stored is bounded like a comment, and over it is an error.
func TestShipWordsTooLongRejected(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	long := strings.Repeat("x", maxCommentLen+1)
	for name, f := range map[string]func(*OpenPRInput){"message": shipWith(&long, nil), "summary": shipWith(nil, &long)} {
		_, err := e.ship(e.task.Key, ShipAuto, f)
		if !IsInvalid(err) || !strings.Contains(err.Error(), name) {
			t.Errorf("a too long %s: %v", name, err)
		}
	}
	var n int
	_ = e.s.db.R.QueryRow(`SELECT count(*) FROM work_log WHERE kind = 'review'`).Scan(&n)
	if n != 0 {
		t.Errorf("a rejected ship scheduled %d rounds", n)
	}
}
