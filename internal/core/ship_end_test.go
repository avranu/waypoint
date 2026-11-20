package core

import "testing"

// A Stop a hook sent when wp ship ended the turn (WP-1093) puts the summary
// on the session like any last message, and marks its timeline line as one
// no model call wrote, so turns ended at wp ship can be counted.
func TestStopEndedByShipMarksTheUpdate(t *testing.T) {
	be := newBabysitEnv(t)
	be.hookEv(HookEvent{Event: "Stop", Pending: &HookPending{}, LastMessage: "Written by the model."})
	be.hookEv(HookEvent{Event: "Stop", Pending: &HookPending{}, Reason: StopEndedByShip, LastMessage: "WP-1 shipped in PR #4."})
	rows, err := be.s.db.R.Query(`SELECT detail, data FROM session_events WHERE kind = 'update' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got [][2]string
	for rows.Next() {
		var d, data string
		if err := rows.Scan(&d, &data); err != nil {
			t.Fatal(err)
		}
		got = append(got, [2]string{d, data})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != [2]string{"Written by the model.", "{}"} || got[1] != [2]string{"WP-1 shipped in PR #4.", `{"ended_by":"ship"}`} {
		t.Errorf("update lines: %q", got)
	}
	if x := be.session(); x.LastUpdate != "WP-1 shipped in PR #4." {
		t.Errorf("last update: %q", x.LastUpdate)
	}
}
