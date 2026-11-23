package main

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
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
	return replaySim(jobs, hist, n, p)
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
	if got, want := waitsOf(replaySim(jobs, nil, 1, recordedPolicy)), []int64{0, 90, 180}; !eq(got, want) {
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
	}
	for n := 1; n <= 16; n++ {
		if got, want := replayLongSlots(n, 0.5), len(slotOrder(slotClassLong, n)); got != want {
			t.Errorf("share 0.5 of %d is %d long slots, slotOrder has %d", n, got, want)
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
	jobs = append(jobs, rj(1020, 10, "short", ""), rj(1040, 10, "short", ""))
	g := replayGroup{Machine: "box", Name: "tests", Hist: replayHist(), Jobs: jobs, Slots: 2, LongSlots: 1}
	rec, model, asked := replayRows(g, replayParams{Threshold: 5 * time.Minute, LongShare: 1})
	if rec.Jobs != len(g.Jobs) || len(rec.Long) != 2 || rec.Long[1] != 599000 {
		t.Errorf("recorded %+v", rec)
	}
	// recorded policy: one long slot, so the second long waits 599 s and the shorts, in slot 1, do not wait
	if model.ClassedLong != 2 || len(model.Short) != 3 || slotPct(model.Short, 0.5) != 0 || slotPct(model.Short, 0.95) != 0 ||
		len(model.Long) != 2 || slotPct(model.Long, 0.95) != 599000 {
		t.Errorf("model %+v", model)
	}
	// share 1: both longs take a slot each and the shorts wait for them (598, 581, 570 s)
	if len(asked.Short) != 3 || slotPct(asked.Short, 0.5) != 581000 || slotPct(asked.Short, 0.95) != 598000 || slotPct(asked.Long, 0.95) != 0 {
		t.Errorf("asked %+v", asked)
	}
	// the model uses the group's recorded threshold, not today's: at 11 minutes the 600 s runs are short
	g.ThresholdSecs = 660
	if _, other, _ := replayRows(g, recordedPolicy); other.ClassedLong != 0 {
		t.Errorf("model at the recorded threshold of 11m: %+v", other)
	}
	if got := replayDistance(rec, model); got != "p50 +0s, p95 +0s" {
		t.Errorf("distance %q", got)
	}
	rec.Short = []int64{0, 530000}
	if got := replayDistance(rec, model); got != "p50 +0s, p95 +8m50s" {
		t.Errorf("distance %q", got)
	}
	if got := replayDistance(replaySummary{}, model); got != "-" {
		t.Errorf("distance with no recorded shorts %q", got)
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
	gs := replayGroups(hs, 0, time.Now().Add(-7*24*time.Hour))
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

// A run still going when a job queued is not one of the runs its class comes
// from; one that had ended is.
func TestReplayClassesIgnoreSameKeyRunStillGoing(t *testing.T) {
	hist := []core.SlotHold{rj(-9000, 10, "c", ""), rj(-8000, 10, "c", ""), rj(-7000, 600, "c", "")}
	// without the 600 s run of job A, B has runs [600 10 10]: median 10, short;
	// counting A (ends 1600, after B queued at 1100; B ends 1700, so the third job sees both) would make it [600 600 10 10]: 305 s, long
	jobs := []core.SlotHold{rj(1000, 600, "c", ""), rj(1100, 600, "c", ""), rj(1701, 10, "c", "")}
	if got := replayClasses(jobs[:2], hist, 5*time.Minute); got[0] != "short" || got[1] != "short" {
		t.Errorf("classes %v, want short short", got)
	}
	if got := replayClasses(jobs, hist, 5*time.Minute); got[0] != "short" || got[1] != "short" || got[2] != "long" {
		t.Errorf("classes %v, want short short long", got)
	}
}

// The walk back through a key's holds gives what the plain scan does.
func TestReplayClassesMatchPlainScan(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	var all []core.SlotHold
	for i := 0; i < 120; i++ {
		h := rj(int64(r.IntN(3000)), int64(r.IntN(900)), []string{"a", "b"}[r.IntN(2)], "")
		h.WaitMS = int64(r.IntN(100)) * 1000
		all = append(all, h)
	}
	hist, jobs := all[:40], all[40:]
	got := replayClasses(jobs, hist, 5*time.Minute)
	for i, j := range jobs {
		var cand []core.SlotHold
		for k, o := range all {
			if o.Cmd == j.Cmd && k != 40+i && replayEnd(o) <= j.QueuedAt {
				cand = append(cand, o)
			}
		}
		sort.SliceStable(cand, func(a, b int) bool { return replayEnd(cand[a]) < replayEnd(cand[b]) })
		var secs []int64
		for k := len(cand) - 1; k >= 0 && len(secs) < slotClassRuns; k-- {
			secs = append(secs, int64(math.Round(float64(cand[k].HoldMS)/1000)))
		}
		if want, _ := slotClassOf(secs, 5*time.Minute); got[i] != want {
			t.Fatalf("job %d: %s, plain scan %s", i, got[i], want)
		}
	}
}

func TestReplayGroupsHistoryAndRecordedPolicy(t *testing.T) {
	since := time.UnixMilli(1_000_000)
	mk := func(queued int64, mod func(h *core.SlotHold)) core.SlotHold {
		h := core.SlotHold{Machine: "box", Name: "tests", Policy: "p1", Repo: "/r", Cmd: "c", Class: "short", QueuedAt: queued, HoldMS: 1000, Slots: 4, LongSlots: 2, ThresholdSecs: 600}
		if mod != nil {
			mod(&h)
		}
		return h
	}
	holds := []core.SlotHold{
		mk(10_000, nil), // before the window: history
		mk(20_000, func(h *core.SlotHold) { h.GaveUp = true }), // gave up before the window: ignored
		mk(1_000_000, nil),
		mk(1_100_000, func(h *core.SlotHold) { h.Slots, h.LongSlots = 8, 3 }),
		mk(1_200_000, func(h *core.SlotHold) { h.Policy = "p2"; h.ThresholdSecs = 0; h.LongSlots = 0 }),
		mk(1_300_000, func(h *core.SlotHold) { h.Policy = ""; h.GaveUp = true }),
	}
	gs := replayGroups(holds, 0, since)
	if len(gs) != 3 {
		t.Fatalf("%d groups: %+v", len(gs), gs)
	}
	var p1, p2, none replayGroup
	for _, g := range gs {
		switch g.Policy {
		case "p1":
			p1 = g
		case "p2":
			p2 = g
		default:
			none = g
		}
	}
	if len(p1.Jobs) != 2 || len(p1.Hist) != 1 || p1.GaveUp != 0 || p1.Slots != 8 || p1.LongSlots != 3 || p1.threshold() != 10*time.Minute {
		t.Errorf("p1 %+v", p1)
	}
	if p2.threshold() != slotLongRun || p2.LongSlots != len(slotOrder(slotClassLong, 4)) {
		t.Errorf("p2 %+v", p2)
	}
	if none.GaveUp != 1 || len(none.Jobs) != 0 {
		t.Errorf("no policy %+v", none)
	}
	// --slots overrides the count, and the long slots follow the rule
	if g := replayGroups(holds, 6, since); g[0].Slots != 6 && g[1].Slots != 6 || g[1].LongSlots != 3 && g[0].LongSlots != 3 {
		t.Errorf("--slots 6: %+v", g)
	}
	for _, g := range replayGroups(holds, 5, since) {
		if g.Slots != 5 || g.LongSlots != len(slotOrder(slotClassLong, 5)) {
			t.Errorf("--slots 5: %+v", g)
		}
	}
	// a group with only history is not printed
	if g := replayGroups(holds[:1], 0, since); len(g) != 0 {
		t.Errorf("history alone made a group: %+v", g)
	}
}

func TestSlotReplayRefusals(t *testing.T) {
	slotEnv(t)
	for _, args := range [][]string{
		{"--days", "0"}, {"--threshold", "0"}, {"--long-share", "0"}, {"--slots", "-1"}, {"--local", "--machine", "another-box"},
		{"extra"},
	} {
		c, _, _ := sqCLI()
		if err := cmdSlotReplay(c, args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestSlotReplayServerPath(t *testing.T) {
	var gotQuery url.Values
	holds := []core.SlotHold{}
	for i := 0; i < 3; i++ {
		holds = append(holds, core.SlotHold{ID: fmt.Sprint("h", i), Machine: "box", Name: "tests", Policy: "adr150-1", Repo: "/r", Cmd: "c", Class: "short",
			ThresholdSecs: 600, Slots: 6, LongSlots: 3, QueuedAt: time.Now().Add(-time.Hour).UnixMilli() + int64(i)*1000, HoldMS: 5000})
	}
	holds = append(holds, core.SlotHold{ID: "old", Machine: "box", Name: "tests", Policy: "adr150-1", Repo: "/r", Cmd: "c", Class: "short",
		Slots: 6, LongSlots: 3, QueuedAt: time.Now().Add(-10 * 24 * time.Hour).UnixMilli(), HoldMS: 5000}) // before the 7-day window
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_ = json.NewEncoder(w).Encode(holds)
	}))
	defer ts.Close()
	slotEnv(t)
	t.Setenv("WAYPOINT_SERVER", ts.URL)
	t.Setenv("WAYPOINT_TOKEN", "tok")
	t.Setenv("WAYPOINT_MACHINE", "box")
	old := slotReplayLimit
	slotReplayLimit = len(holds)
	t.Cleanup(func() { slotReplayLimit = old })
	c, out, errb := sqCLI()
	if err := cmdSlotReplay(c, nil); err != nil {
		t.Fatal(err)
	}
	if want := time.Now().Add(-21 * 24 * time.Hour).UnixMilli(); gotQuery.Get("machine") != "box" || gotQuery.Get("limit") != strconv.Itoa(len(holds)) ||
		abs64(mustInt(t, gotQuery.Get("since"))-want) > 60_000 {
		t.Errorf("query %v, since should be about %d", gotQuery, want)
	}
	if !strings.Contains(errb.String(), fmt.Sprintf("limit of %d holds", len(holds))) {
		t.Errorf("no limit warning: %q", errb.String())
	}
	for _, want := range []string{"tests on box (policy adr150-1): 3 holds", "6 slots", "up to 14 days of earlier holds (1 here)",
		"simulated, recorded policy (10m0s, 3 of 6 slots for long)", "model distance, short waits: p50 "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	// under the limit: no warning
	slotReplayLimit = 1000
	c, _, errb = sqCLI()
	if err := cmdSlotReplay(c, nil); err != nil || errb.Len() != 0 {
		t.Errorf("%v %q", err, errb.String())
	}
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func mustInt(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSlotReplayLocalSaysTheSlotCountIsAssumed(t *testing.T) {
	slotEnv(t)
	dir, _ := slotDir()
	appendWaitRec(dir, waitRec{Name: "tests", Repo: "/r", Cmd: "c", Class: "short", At: time.Now().Unix() - 100, Wait: 2, Secs: 30})
	c, out, _ := sqCLI()
	if err := cmdSlotReplay(c, []string{"--local"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "assumed: this machine's default") || !strings.Contains(out.String(), fmt.Sprintf("%d slots (assumed", defaultSlots())) {
		t.Errorf("no assumption note:\n%s", out.String())
	}
	c, out, _ = sqCLI()
	if err := cmdSlotReplay(c, []string{"--local", "--slots", "3"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "assumed") || !strings.Contains(out.String(), "3 slots") {
		t.Errorf("--slots output:\n%s", out.String())
	}
}
