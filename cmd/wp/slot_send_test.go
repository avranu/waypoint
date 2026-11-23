package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// ssEnv is a slot dir and a worktree, with holds kept for the server at url
// (sending is on). The returned cli has a token, so a send is tried.
func ssEnv(t *testing.T, url string) (*cli, string) {
	t.Helper()
	slotEnv(t)
	t.Setenv(slotSendEnv, "")
	t.Setenv("WAYPOINT_SERVER", url)
	t.Setenv("WAYPOINT_TOKEN", "tok")
	t.Setenv(slotHeldEnv, "")
	sqArgs(t)
	sqRepo(t)
	c, _, _ := sqCLI()
	dir, _ := slotDir()
	return c, dir
}

// ssDown is a server that refuses everything, so the sender in the
// background leaves the outbox as it is.
func ssDown(t *testing.T) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 500) }))
	t.Cleanup(ts.Close)
	return ts.URL
}

func ssOutbox(t *testing.T, dir string) []core.SlotHold {
	t.Helper()
	return readOutbox(dir)
}

func TestSlotHoldRecordsWhatTheClassWasDecidedFrom(t *testing.T) {
	c, dir := ssEnv(t, ssDown(t))
	sqSeed(t, "tests", sqCmd, 400, 3)
	// another run holds slot 3; a live process waits
	other, _, _ := sqCLI()
	_, rel0, err := other.takeSlotFor(context.Background(), "tests", 4, "other cmd")
	if err != nil {
		t.Fatal(err)
	}
	defer rel0()
	ppid := os.Getppid()
	q, _ := json.Marshal(queueRec{Since: 123, WD: "/x", Cmd: "c", Class: "long", BehindOwn: true})
	_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("tests.%d.queue", ppid)), q, 0o600)

	_, rel, setExit, err := c.takeSlotExit(context.Background(), "tests", 4, sqCmd)
	if err != nil {
		t.Fatal(err)
	}
	setExit(7)
	rel()
	recs := ssOutbox(t, dir)
	if len(recs) != 1 {
		t.Fatalf("outbox has %d records", len(recs))
	}
	h := recs[0]
	if len(h.ID) != 32 || h.Name != "tests" || h.Cmd != sqCmd || h.Class != "long" || h.Policy != slotPolicyVersion ||
		h.ThresholdSecs != 300 || h.Runs != 3 || h.MedianSecs == nil || *h.MedianSecs != 400 || h.Slots != 4 || h.LongSlots != 2 ||
		h.Slot == nil || *h.Slot != 0 || h.GaveUp || h.Exit == nil || *h.Exit != 7 || h.Tree == "" || h.Repo == "" || h.QueuedAt <= 0 {
		t.Errorf("record %+v", h)
	}
	if len(h.Holders) != 1 || h.Holders[0].Slot != 3 || h.Holders[0].PID != os.Getpid() {
		t.Errorf("holders %+v", h.Holders)
	}
	if len(h.Waiters) != 1 || h.Waiters[0].PID != ppid || h.Waiters[0].Since != 123 || h.Waiters[0].Class != "long" || !h.Waiters[0].BehindOwn {
		t.Errorf("waiters %+v", h.Waiters)
	}
	if len(readWaitRecs(dir)) != 4 { // the three seeded and this one: waits.jsonl is as it was
		t.Errorf("waits.jsonl has %d records", len(readWaitRecs(dir)))
	}
}

func TestSlotHoldShortRunWithNoHistory(t *testing.T) {
	c, dir := ssEnv(t, ssDown(t))
	_, rel, err := c.takeSlot(context.Background(), "tests", 4)
	if err != nil {
		t.Fatal(err)
	}
	rel()
	h := ssOutbox(t, dir)[0]
	if h.Class != "short" || h.Runs != 0 || h.MedianSecs != nil || h.Slot == nil || *h.Slot != 3 || h.Exit != nil || len(h.Holders) != 0 || len(h.Waiters) != 0 {
		t.Errorf("record %+v", h)
	}
}

func TestSlotHoldGaveUpAndTreeWait(t *testing.T) {
	c, dir := ssEnv(t, ssDown(t))
	_, rel, err := c.takeSlot(context.Background(), "tests", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, _, err := c.takeSlot(ctx, "tests", 1); err == nil {
		t.Fatal("got a slot")
	}
	var gave core.SlotHold
	for _, h := range ssOutbox(t, dir) {
		if h.GaveUp {
			gave = h
		}
	}
	if !gave.GaveUp || gave.Slot != nil || gave.HoldMS != 0 || gave.WaitMS < 250 || gave.Exit != nil {
		t.Errorf("gave-up record %+v", gave)
	}

	// a long run waits behind its worktree's own earlier one
	sqSeed(t, "tests", "make check", 1800, 3)
	_, first, err := c.takeSlotFor(context.Background(), "tests", 4, "make check")
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan sqTake, 1)
	go func() {
		_, r, err := c.takeSlotFor(context.Background(), "tests", 4, "make check")
		ch <- sqTake{r, err}
	}()
	time.Sleep(400 * time.Millisecond)
	first()
	second := sqGot(t, ch)
	second()
	var tw core.SlotHold
	for _, h := range ssOutbox(t, dir) {
		if h.Cmd == "make check" && h.TreeWaitMS > 0 {
			tw = h
		}
	}
	if tw.TreeWaitMS < 300 || tw.WaitMS < tw.TreeWaitMS {
		t.Errorf("tree wait %d, wait %d", tw.TreeWaitMS, tw.WaitMS)
	}
}

func TestSlotSendOffSpoolsNothing(t *testing.T) {
	c, dir := ssEnv(t, ssDown(t))
	t.Setenv(slotSendEnv, "off")
	_, rel, err := c.takeSlot(context.Background(), "tests", 2)
	if err != nil {
		t.Fatal(err)
	}
	rel()
	if _, err := os.Stat(filepath.Join(dir, slotOutboxFile)); err == nil {
		t.Error("spooled with sending off")
	}
	if len(readWaitRecs(dir)) != 1 {
		t.Error("waits.jsonl no longer written")
	}
	t.Setenv(slotSendEnv, "")
	t.Setenv("WAYPOINT_SERVER", "")
	c2, _, _ := sqCLI()
	_, rel, err = c2.takeSlot(context.Background(), "tests", 2)
	if err != nil {
		t.Fatal(err)
	}
	rel()
	if _, err := os.Stat(filepath.Join(dir, slotOutboxFile)); err == nil {
		t.Error("spooled with no server")
	}
}

func ssHold(i int) core.SlotHold {
	return core.SlotHold{ID: "id" + strconv.Itoa(i), Name: "tests", Class: "short", QueuedAt: int64(1000 + i)}
}

func TestSlotOutboxCapDropsOldestAndCounts(t *testing.T) {
	old := slotOutboxMax
	slotOutboxMax = 50
	t.Cleanup(func() { slotOutboxMax = old })
	_, dir := ssEnv(t, ssDown(t))
	for i := 0; i < 53; i++ {
		appendWaitRecFull(dir, waitRec{Name: "tests"}, &[]core.SlotHold{ssHold(i)}[0])
	}
	recs := readOutbox(dir)
	if len(recs) != 50 || recs[0].ID != "id3" || recs[49].ID != "id52" || readDropped(dir) != 3 {
		t.Errorf("%d records, first %q, dropped %d", len(recs), recs[0].ID, readDropped(dir))
	}
	for i := 53; i < 58; i++ {
		appendWaitRecFull(dir, waitRec{Name: "tests"}, &[]core.SlotHold{ssHold(i)}[0])
	}
	if n := readDropped(dir); n != 8 || len(readOutbox(dir)) != 50 {
		t.Errorf("dropped %d, %d records", n, len(readOutbox(dir)))
	}
}

func TestSendSlotHoldsAcksOnlyWhatWasSent(t *testing.T) {
	var mu sync.Mutex
	var queries []map[string][]string
	var sent []string
	var c *cli
	var dir string
	extra := ssHold(9999)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Holds []core.SlotHold `json:"holds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		queries = append(queries, r.URL.Query())
		for _, h := range body.Holds {
			sent = append(sent, h.ID)
		}
		if len(queries) == 3 { // a run finishes while the last batch is in flight
			appendWaitRecFull(dir, waitRec{Name: "tests"}, &extra)
		}
		_, _ = w.Write([]byte(`{"stored":1}`))
	}))
	defer ts.Close()
	c, dir = ssEnv(t, ts.URL)
	t.Setenv("WAYPOINT_MACHINE", "box")
	_ = os.MkdirAll(dir, 0o700)
	addDropped(dir, 4)
	for i := 0; i < 450; i++ {
		appendWaitRec(dir, waitRec{}) // keep the helper honest: waits only
		appendOutbox(dir, ssHold(i))
	}
	n, remaining, err := c.sendSlotHolds(context.Background())
	// the run that finished mid-send is not lost and not acked early: the next batch carries it
	if err != nil || n != 451 || remaining != 0 {
		t.Fatalf("sent %d, remaining %d, err %v", n, remaining, err)
	}
	if len(queries) != 4 || len(sent) != 451 || sent[450] != extra.ID {
		t.Fatalf("%d requests, %d holds", len(queries), len(sent))
	}
	q := queries[0]
	if q["machine"][0] != "box" || q["pending"][0] != "450" || q["oldest"][0] != "1000" || q["dropped"][0] != "4" {
		t.Errorf("first query %v", q)
	}
	if left := readOutbox(dir); len(left) != 0 {
		t.Errorf("outbox after: %+v", left)
	}
}

func TestSendSlotHoldsFailureLeavesOutboxAndIsFast(t *testing.T) {
	var hits int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Error(w, "boom", http.StatusServiceUnavailable)
	}))
	defer ts.Close()
	c, dir := ssEnv(t, ts.URL)
	_ = os.MkdirAll(dir, 0o700)
	for i := 0; i < 3; i++ {
		appendOutbox(dir, ssHold(i))
	}
	start := time.Now()
	n, remaining, err := c.sendSlotHolds(context.Background())
	if err == nil || n != 0 || remaining != 3 || hits != 1 || time.Since(start) > 3*time.Second {
		t.Errorf("sent %d remaining %d hits %d err %v in %v", n, remaining, hits, err, time.Since(start))
	}
	if len(readOutbox(dir)) != 3 {
		t.Error("outbox changed")
	}
}

func TestSendSlotHoldsSkipsWhenAnotherIsSending(t *testing.T) {
	var hits int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer ts.Close()
	c, dir := ssEnv(t, ts.URL)
	_ = os.MkdirAll(dir, 0o700)
	appendOutbox(dir, ssHold(1))
	lf, _ := os.OpenFile(filepath.Join(dir, slotSendLockFile), os.O_CREATE|os.O_RDWR, 0o600)
	defer lf.Close()
	if ok, _ := slotTryLock(lf); !ok {
		t.Fatal("no lock")
	}
	if n, remaining, _ := c.sendSlotHolds(context.Background()); n != 0 || remaining != 1 || hits != 0 {
		t.Errorf("sent %d remaining %d hits %d", n, remaining, hits)
	}
}

func TestSlotSendCommandPrintsCounts(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer ts.Close()
	c, dir := ssEnv(t, ts.URL)
	_ = os.MkdirAll(dir, 0o700)
	appendOutbox(dir, ssHold(1))
	appendOutbox(dir, ssHold(2))
	var out, errb = new(testBuf), new(testBuf)
	c.out, c.err = out, errb
	if err := cmdSlotSend(c, nil); err != nil || out.String() != "sent 2 slot holds, 0 remain\n" {
		t.Errorf("%q %v", out.String(), err)
	}
}

type testBuf struct{ b []byte }

func (b *testBuf) Write(p []byte) (int, error) { b.b = append(b.b, p...); return len(p), nil }
func (b *testBuf) String() string              { return string(b.b) }
