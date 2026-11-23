package main

import (
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// rj is a job queued at q seconds that holds a slot for hold seconds. cmd
// and tree name it; its recorded wait is none.
func rj(q, hold int64, cmd, tree string) core.SlotHold {
	return core.SlotHold{Name: "tests", Repo: "/r", Cmd: cmd, Tree: tree, Class: "short", QueuedAt: q * 1000, HoldMS: hold * 1000, Slots: 2}
}

// rhist is n finished holds of cmd, a run time of secs each, ending well before time 1000.
func rhist(cmd string, secs int64, n int) []core.SlotHold {
	var out []core.SlotHold
	for i := 0; i < n; i++ {
		out = append(out, rj(int64(i)*10-5000, secs, cmd, ""))
	}
	return out
}

func waitsOf(out []replayOut) []int64 {
	var w []int64
	for _, o := range out {
		w = append(w, o.WaitMS/1000)
	}
	return w
}

// replayJobs is history followed by the jobs under test; the result is the
// jobs' (the history's tail is cut off).
func replayRun(hist, jobs []core.SlotHold, n int, p replayParams) []replayOut {
	all := append(append([]core.SlotHold{}, hist...), jobs...)
	return replaySim(all, n, p)[len(hist):]
}

func eq(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var recordedPolicy = replayParams{Threshold: 5 * time.Minute, LongShare: 0.5}

func replayHist() []core.SlotHold { return append(rhist("long", 600, 3), rhist("short", 10, 3)...) }

func classesOf(out []replayOut) []string {
	var c []string
	for _, o := range out {
		c = append(c, o.Class)
	}
	return c
}

// Two slots, two long runs (600 s) and three short ones (10 s, queued apart).
// With half the slots for long runs they queue behind each other in slot 0
// and the shorts, which take slot 1, never wait; with all of them the longs
// fill both slots and the shorts wait for one to end.
func TestReplayLongShareDecidesWhetherShortsWait(t *testing.T) {
	jobs := []core.SlotHold{
		rj(1000, 600, "long", ""), rj(1001, 600, "long", ""),
		rj(1002, 10, "short", ""), rj(1020, 10, "short", ""), rj(1040, 10, "short", ""),
	}
	// long 1 holds slot 0 from 1000 to 1600 and long 2 waits for it: 599 s
	if got, want := waitsOf(replayRun(replayHist(), jobs, 2, recordedPolicy)), []int64{0, 599, 0, 0, 0}; !eq(got, want) {
		t.Errorf("share 0.5: waits %v, want %v", got, want)
	}
	// the longs take slots 0 and 1 until 1600 and 1601: the shorts go as slots free
	// (1002 at 1600: 598; 1020 at 1601: 581; 1040 at 1610: 570)
	if got, want := waitsOf(replayRun(replayHist(), jobs, 2, replayParams{Threshold: 5 * time.Minute, LongShare: 1})), []int64{0, 0, 598, 581, 570}; !eq(got, want) {
		t.Errorf("share 1.0: waits %v, want %v", got, want)
	}
}

// A threshold above the longs' 600 s makes them short: they may take any slot,
// the highest first, and the shorts wait as if the longs had all the slots.
func TestReplayThresholdReclassifies(t *testing.T) {
	jobs := []core.SlotHold{rj(1000, 600, "long", ""), rj(1001, 600, "long", ""), rj(1002, 10, "short", "")}
	out := replayRun(replayHist(), jobs, 2, recordedPolicy)
	if c := classesOf(out); c[0] != "long" || c[1] != "long" || c[2] != "short" {
		t.Errorf("at 5m: %v", c)
	}
	out = replayRun(replayHist(), jobs, 2, replayParams{Threshold: 11 * time.Minute, LongShare: 0.5})
	if c := classesOf(out); c[0] != "short" || c[1] != "short" || c[2] != "short" {
		t.Errorf("at 11m: %v", c)
	}
	if got, want := waitsOf(out), []int64{0, 0, 598}; !eq(got, want) {
		t.Errorf("at 11m: waits %v, want %v", got, want)
	}
}

// One long run per worktree: the second of one tree waits for the first
// though a slot is free; another tree's does not. A short run from the tree
// does not wait.
func TestReplayOneLongRunPerTree(t *testing.T) {
	p := replayParams{Threshold: 5 * time.Minute, LongShare: 1}
	same := []core.SlotHold{rj(1000, 600, "long", "/w1"), rj(1001, 600, "long", "/w1"), rj(1002, 10, "short", "/w1")}
	if got, want := waitsOf(replayRun(replayHist(), same, 3, p)), []int64{0, 599, 0}; !eq(got, want) {
		t.Errorf("same tree: %v, want %v", got, want)
	}
	other := []core.SlotHold{rj(1000, 600, "long", "/w1"), rj(1001, 600, "long", "/w2"), rj(1002, 10, "short", "/w1")}
	if got, want := waitsOf(replayRun(replayHist(), other, 3, p)), []int64{0, 0, 0}; !eq(got, want) {
		t.Errorf("two trees: %v, want %v", got, want)
	}
	none := []core.SlotHold{rj(1000, 600, "long", ""), rj(1001, 600, "long", "")}
	if got, want := waitsOf(replayRun(replayHist(), none, 3, p)), []int64{0, 0}; !eq(got, want) {
		t.Errorf("no tree known: %v, want %v", got, want)
	}
}

func TestReplayFirstComeAmongEligible(t *testing.T) {
	// one slot, three runs of 100 s queued 10 s apart: each goes in turn
	jobs := []core.SlotHold{rj(1000, 100, "short", ""), rj(1010, 100, "short", ""), rj(1020, 100, "short", "")}
	if got, want := waitsOf(replaySim(jobs, 1, recordedPolicy)), []int64{0, 90, 180}; !eq(got, want) {
		t.Errorf("waits %v, want %v", got, want)
	}
}

func TestReplayLongSlotsAndPercentile(t *testing.T) {
	for _, c := range []struct {
		n     int
		share float64
		want  int
	}{{8, 0.25, 2}, {5, 0.5, 2}, {4, 0.5, 2}, {1, 0.1, 1}, {4, 1, 4}, {6, 0.25, 1}} {
		if got := replayLongSlots(c.n, c.share); got != c.want {
			t.Errorf("replayLongSlots(%d, %v) = %d, want %d", c.n, c.share, got, c.want)
		}
		if c.share == 0.5 && replayLongSlots(c.n, 0.5) != len(slotOrder(slotClassLong, c.n)) {
			t.Errorf("share 0.5 of %d differs from slotOrder", c.n)
		}
	}
	v := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if slotPct(v, 0.5) != 5 || slotPct(v, 0.95) != 10 || slotPct(v[:1], 0.95) != 1 {
		t.Errorf("percentiles %d %d", slotPct(v, 0.5), slotPct(v, 0.95))
	}
}

func TestReplayRowsRecordedAndModel(t *testing.T) {
	jobs := []core.SlotHold{rj(1000, 600, "long", ""), rj(1001, 600, "long", ""), rj(1002, 10, "short", "")}
	jobs[1].Class, jobs[1].WaitMS = "long", 599000
	jobs[0].Class = "long"
	g := replayGroup{Machine: "box", Name: "tests", Jobs: append(replayHist(), jobs...), Slots: 2}
	rec, model, _ := replayRows(g, recordedPolicy)
	if rec.Jobs != len(g.Jobs) || len(rec.Long) != 2 || rec.Long[1] != 599000 {
		t.Errorf("recorded %+v", rec)
	}
	if model.ClassedLong < 2 {
		t.Errorf("model %+v", model)
	}
}

func TestReplayFromLocalWaits(t *testing.T) {
	now := time.Now().Unix()
	recs := []waitRec{
		{Name: "tests", Repo: "/r", Cmd: "c", Class: "short", At: now - 100, Wait: 5, Secs: 20},
		{Name: "tests", Repo: "/r", Cmd: "c", Class: "short", At: now - 40*86400, Wait: 5, Secs: 20},
		{Name: "tests", Repo: "/r", Cmd: "c", Class: "long", At: now - 10, Wait: 9, GaveUp: true},
	}
	hs := holdsFromWaits(recs, time.Now().Add(-7*24*time.Hour), "box", 4)
	if len(hs) != 2 || hs[0].QueuedAt != (now-100-20-5)*1000 || hs[0].HoldMS != 20000 || hs[0].WaitMS != 5000 || hs[0].Slots != 4 || hs[0].Tree != "" || !hs[1].GaveUp {
		t.Errorf("holds %+v", hs)
	}
	gs := replayGroups(hs, 0)
	if len(gs) != 1 || len(gs[0].Jobs) != 1 || gs[0].GaveUp != 1 || gs[0].Slots != 4 {
		t.Errorf("groups %+v", gs)
	}
}

func TestSlotReplayLocalCommand(t *testing.T) {
	slotEnv(t)
	dir, _ := slotDir()
	now := time.Now().Unix()
	for i := 0; i < 4; i++ {
		appendWaitRec(dir, waitRec{Name: "tests", Repo: "/r", Cmd: "c", Class: "short", At: now - int64(i)*3600, Wait: 2, Secs: 30})
	}
	c, out, _ := sqCLI()
	if err := cmdSlotReplay(c, []string{"--local", "--days", "3", "--slots", "2"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tests on ", "4 holds", "2 slots", "worktree", "skipped", "recorded", "simulated, recorded policy", "simulated, threshold 5m0s, share 0.50", "not first come"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if err := cmdSlotReplay(c, []string{"--local", "--long-share", "2"}); err == nil {
		t.Error("a share above 1 was accepted")
	}
}
