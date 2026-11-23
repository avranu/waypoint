package core

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

func slotHoldIn(id string, queued int64) SlotHold {
	return SlotHold{ID: id, Name: "tests", Repo: "/r/.git", Tree: "/r", Cmd: "go test", Class: "short", ThresholdSecs: 300, Slots: 4, LongSlots: 2,
		QueuedAt: queued, WaitMS: 10, HoldMS: 2000}
}

type slotMachineRow struct {
	pending, dropped, rejected int64
	since, stored              *int64
}

func slotMachine(t *testing.T, e *env, a *Actor, machine string) slotMachineRow {
	t.Helper()
	var r slotMachineRow
	if err := e.s.db.R.QueryRow(`SELECT pending, pending_since, dropped, rejected, last_stored_at FROM slot_machines WHERE user_id = ? AND machine = ?`,
		a.User.ID, machine).Scan(&r.pending, &r.since, &r.dropped, &r.rejected, &r.stored); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRecordSlotHoldsCountsAndIsIdempotent(t *testing.T) {
	e := newEnv(t)
	now := e.clockNow().UnixMilli()
	bad := slotHoldIn("bad", now)
	bad.Class = "medium"
	in := SlotHoldsIn{Machine: "box", Pending: 5, Dropped: 2, Holds: []SlotHold{slotHoldIn("a", now), slotHoldIn("b", now+1), bad}}
	res, err := e.s.RecordSlotHolds(e.ctx, e.admin, in)
	if err != nil || *res != (SlotHoldsResult{Stored: 2, Rejected: 1}) {
		t.Fatalf("first: %+v %v", res, err)
	}
	m := slotMachine(t, e, e.admin, "box")
	if m.pending != 2 || m.dropped != 2 || m.rejected != 1 || m.since == nil || m.stored == nil {
		t.Errorf("machine after first: %+v", m)
	}
	// the same two again, and the pending count of what is left
	in.Pending, in.Holds = 2, in.Holds[:2]
	res, err = e.s.RecordSlotHolds(e.ctx, e.admin, in)
	if err != nil || *res != (SlotHoldsResult{Duplicate: 2}) {
		t.Fatalf("resend: %+v %v", res, err)
	}
	if m := slotMachine(t, e, e.admin, "box"); m.pending != 0 || m.since != nil || m.rejected != 1 {
		t.Errorf("machine after resend: %+v", m)
	}
	got, err := e.s.SlotHolds(e.ctx, e.admin, SlotHoldsFilter{})
	if err != nil || len(got) != 2 || got[0].ID != "a" || got[0].Machine != "box" || got[0].User != "robin" {
		t.Fatalf("read: %+v %v", got, err)
	}
}

func TestRecordSlotHoldsKeepsAllFields(t *testing.T) {
	e := newEnv(t)
	h := slotHoldIn("a", e.clockNow().UnixMilli())
	med, slot, exit := 612.5, 1, 3
	h.MedianSecs, h.Slot, h.Exit, h.Runs, h.Policy = &med, &slot, &exit, 5, "adr150-1"
	h.TreeWaitMS = 77
	h.Holders = []SlotHolder{{Slot: 0, PID: 9, Since: 100}}
	h.Waiters = []SlotWaiter{{PID: 10, Since: 101, Class: "long", BehindOwn: true}}
	if _, err := e.s.RecordSlotHolds(e.ctx, e.admin, SlotHoldsIn{Machine: "box", Holds: []SlotHold{h}}); err != nil {
		t.Fatal(err)
	}
	got, err := e.s.SlotHolds(e.ctx, e.admin, SlotHoldsFilter{})
	if err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
	g := got[0]
	if *g.MedianSecs != 612.5 || *g.Slot != 1 || *g.Exit != 3 || g.Runs != 5 || g.Policy != "adr150-1" || g.TreeWaitMS != 77 ||
		len(g.Holders) != 1 || g.Holders[0].PID != 9 || len(g.Waiters) != 1 || !g.Waiters[0].BehindOwn || g.Cmd != "go test" || g.LongSlots != 2 {
		t.Errorf("read back %+v", g)
	}
}

func TestRecordSlotHoldsBounds(t *testing.T) {
	e := newEnv(t)
	now := e.clockNow().UnixMilli()
	mk := func(f func(h *SlotHold)) SlotHold {
		h := slotHoldIn("x", now)
		f(&h)
		return h
	}
	many := make([]SlotHolder, 65)
	for name, h := range map[string]SlotHold{
		"no id":        mk(func(h *SlotHold) { h.ID = "" }),
		"no name":      mk(func(h *SlotHold) { h.Name = "" }),
		"no queued_at": mk(func(h *SlotHold) { h.QueuedAt = 0 }),
		"negative":     mk(func(h *SlotHold) { h.HoldMS = -1 }),
		"huge":         mk(func(h *SlotHold) { h.WaitMS = slotDurMaxMS + 1 }),
		"65 holders":   mk(func(h *SlotHold) { h.Holders = many }),
	} {
		res, err := e.s.RecordSlotHolds(e.ctx, e.admin, SlotHoldsIn{Machine: "box", Holds: []SlotHold{h}})
		if err != nil || res.Rejected != 1 || res.Stored != 0 {
			t.Errorf("%s: %+v %v", name, res, err)
		}
	}
	long := mk(func(h *SlotHold) { h.Cmd = string(make([]byte, 5000)) })
	if _, err := e.s.RecordSlotHolds(e.ctx, e.admin, SlotHoldsIn{Machine: "box", Holds: []SlotHold{long}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.s.SlotHolds(e.ctx, e.admin, SlotHoldsFilter{}); len(got) != 1 || len(got[0].Cmd) > 4000 {
		t.Errorf("a long cmd is cut, not refused: %d holds", len(got))
	}
}

func TestRecordSlotHoldsTooManyKeepsMachineRow(t *testing.T) {
	e := newEnv(t)
	var hs []SlotHold
	for i := range SlotHoldsMax + 1 {
		hs = append(hs, slotHoldIn("h"+strconv.Itoa(i), e.clockNow().UnixMilli()))
	}
	if _, err := e.s.RecordSlotHolds(e.ctx, e.admin, SlotHoldsIn{Machine: "box", Pending: 501, Dropped: 4, Holds: hs}); !IsInvalid(err) {
		t.Fatalf("err = %v", err)
	}
	if m := slotMachine(t, e, e.admin, "box"); m.pending != 501 || m.dropped != 4 {
		t.Errorf("machine row %+v", m)
	}
	if got, _ := e.s.SlotHolds(e.ctx, e.admin, SlotHoldsFilter{}); len(got) != 0 {
		t.Errorf("stored %d", len(got))
	}
}

func TestSlotHoldsAreSeparatePerUserAndAccess(t *testing.T) {
	e := newEnv(t)
	jo := e.user("jo", false)
	now := e.clockNow().UnixMilli()
	for _, a := range []*Actor{e.admin, jo} {
		if res, err := e.s.RecordSlotHolds(e.ctx, a, SlotHoldsIn{Machine: "box", Holds: []SlotHold{slotHoldIn("same", now)}}); err != nil || res.Stored != 1 {
			t.Fatalf("%s: %+v %v", a.User.Username, res, err)
		}
	}
	if got, _ := e.s.SlotHolds(e.ctx, jo, SlotHoldsFilter{}); len(got) != 1 || got[0].User != "jo" {
		t.Errorf("jo reads %+v", got)
	}
	if _, err := e.s.SlotHolds(e.ctx, jo, SlotHoldsFilter{User: "robin"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("another person's holds: %v", err)
	}
	if got, err := e.s.SlotHolds(e.ctx, e.admin, SlotHoldsFilter{User: "all"}); err != nil || len(got) != 2 {
		t.Errorf("admin all: %+v %v", got, err)
	}
	if got, _ := e.s.SlotHolds(e.ctx, e.admin, SlotHoldsFilter{User: "jo", Machine: "nope"}); len(got) != 0 {
		t.Errorf("machine filter: %+v", got)
	}
	// who may write
	ro := *jo
	ro.ReadOnly = true
	job := &Actor{User: jo.User, RunnerJob: 5, scope: map[int64]int{1: RoleEditor}}
	for name, a := range map[string]*Actor{"read-only": &ro, "runner job": job, "nobody": nil} {
		if _, err := e.s.RecordSlotHolds(e.ctx, a, SlotHoldsIn{Machine: "box"}); err == nil {
			t.Errorf("%s may send", name)
		}
	}
	if _, err := e.s.RecordSlotHolds(e.ctx, jo, SlotHoldsIn{}); !IsInvalid(err) {
		t.Errorf("no machine: %v", err)
	}
}

func TestSlotHoldsReadWindow(t *testing.T) {
	e := newEnv(t)
	base := e.clockNow().UnixMilli()
	var hs []SlotHold
	for i := range 4 {
		hs = append(hs, slotHoldIn("h"+strconv.Itoa(i), base+int64(i)*1000))
	}
	if _, err := e.s.RecordSlotHolds(e.ctx, e.admin, SlotHoldsIn{Machine: "box", Holds: hs}); err != nil {
		t.Fatal(err)
	}
	got, _ := e.s.SlotHolds(e.ctx, e.admin, SlotHoldsFilter{Since: time.UnixMilli(base + 1000), Until: time.UnixMilli(base + 3000)})
	if len(got) != 2 || got[0].ID != "h1" || got[1].ID != "h2" {
		t.Errorf("window: %+v", got)
	}
	if got, _ := e.s.SlotHolds(e.ctx, e.admin, SlotHoldsFilter{Limit: 3}); len(got) != 3 {
		t.Errorf("limit: %d", len(got))
	}
}

func TestSlotHoldsCollector(t *testing.T) {
	e := newEnv(t)
	const name = "slot holds"
	get := func() CollectorHealth {
		e.s.CheckCollectorHealth(e.ctx)
		got, err := e.s.CollectorHealthList(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range got {
			if h.Name == name {
				return h
			}
		}
		t.Fatalf("no collector %q", name)
		return CollectorHealth{}
	}
	e.setClock(time.Now().Add(-72 * time.Hour))
	// a machine that says holds are waiting and gets none stored, and one that is caught up
	if _, err := e.s.RecordSlotHolds(e.ctx, e.admin, SlotHoldsIn{Machine: "stuck", Pending: 3, Dropped: 4, Holds: []SlotHold{{ID: "bad"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.RecordSlotHolds(e.ctx, e.admin, SlotHoldsIn{Machine: "fine", Pending: 1, Holds: []SlotHold{slotHoldIn("a", e.clockNow().UnixMilli())}}); err != nil {
		t.Fatal(err)
	}
	e.setClock(time.Now())
	h := get()
	if h.Waiting != 1 || !h.Stalled || h.LastOutputAt == nil {
		t.Errorf("health %+v", h)
	}
	if h.Dropped == nil || *h.Dropped != 5 { // 4 dropped by the machine + 1 refused here
		t.Errorf("dropped = %v", h.Dropped)
	}
	if c := collectorByName(t, name); c.unit != "machines" || c.group != "machine" {
		t.Errorf("collector %+v", c)
	}
}
