package api

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

func slotHoldBody(t *testing.T, n int) []byte {
	t.Helper()
	var hs []core.SlotHold
	for i := range n {
		hs = append(hs, core.SlotHold{ID: fmt.Sprintf("h%d", i), Name: "tests", Class: "short", QueuedAt: time.Now().UnixMilli() + int64(i), HoldMS: 1000, Slots: 2, LongSlots: 1})
	}
	b, err := json.Marshal(map[string]any{"holds": hs})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSlotHoldsRoundTrip(t *testing.T) {
	a := newTranscriptAPI(t)
	hdr := map[string]string{"Content-Type": "application/json"}
	var res core.SlotHoldsResult
	if code := a.human.json("POST", "/api/v1/slot-holds?machine=box&pending=3&oldest=1&dropped=0", json.RawMessage(slotHoldBody(t, 3)), &res); code != 200 || res.Stored != 3 {
		t.Fatalf("post: %d %+v", code, res)
	}
	if resp, err := a.req("POST", "/api/v1/slot-holds?machine=box&pending=3", slotHoldBody(t, 3), hdr); err != nil || resp.StatusCode != 200 {
		t.Fatalf("resend: %v %v", resp, err)
	}
	var got []core.SlotHold
	if code := a.human.json("GET", "/api/v1/slot-holds?machine=box&limit=2", nil, &got); code != 200 || len(got) != 2 || got[0].ID != "h0" || got[0].Machine != "box" {
		t.Fatalf("get: %d %+v", code, got)
	}
	if code := a.human.json("GET", "/api/v1/slot-holds?since=abc", nil, nil); code != 400 {
		t.Errorf("bad since: %d", code)
	}
}

func TestSlotHoldsOversizeBatchRefusedButMachineRowWritten(t *testing.T) {
	a := newTranscriptAPI(t)
	if code := a.human.json("POST", "/api/v1/slot-holds?machine=box&pending=501&dropped=7", json.RawMessage(slotHoldBody(t, core.SlotHoldsMax+1)), nil); code != 400 {
		t.Fatalf("oversize: %d", code)
	}
	var got []core.SlotHold
	if code := a.human.json("GET", "/api/v1/slot-holds", nil, &got); code != 200 || len(got) != 0 {
		t.Errorf("stored %d (%d)", len(got), code)
	}
	var pending, dropped int64
	if err := a.db.R.QueryRow(`SELECT pending, dropped FROM slot_machines WHERE machine = 'box'`).Scan(&pending, &dropped); err != nil || pending != 501 || dropped != 7 {
		t.Errorf("machine row: %d %d %v", pending, dropped, err)
	}
}
