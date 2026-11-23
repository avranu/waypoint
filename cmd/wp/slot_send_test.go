package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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

	_, rel, setExit, err := c.takeSlotExit(context.Background(), "tests", 4, sqCmd, "")
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

// slotEnv turns sending off; ssRecorder is a server that keeps every body it
// is sent and answers 200.
type ssRecorder struct {
	mu     sync.Mutex
	bodies []string
	ids    []string
	hits   int
}

func ssServer(t *testing.T) (*ssRecorder, string) {
	t.Helper()
	rec := &ssRecorder{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Holds []core.SlotHold `json:"holds"`
		}
		_ = json.Unmarshal(b, &body)
		rec.mu.Lock()
		rec.hits++
		rec.bodies = append(rec.bodies, string(b))
		for _, h := range body.Holds {
			rec.ids = append(rec.ids, h.ID)
		}
		rec.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(ts.Close)
	return rec, ts.URL
}

func (r *ssRecorder) seen(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.ids, id)
}

func (r *ssRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits
}

func TestSlotHoldSentToServerHasNoInlineSecrets(t *testing.T) {
	c, dir := ssEnv(t, ssDown(t))
	const cmd = "go test --token=abc123secret --password hunter2 ./x"
	_, rel, err := c.takeSlotFor(context.Background(), "tests", 2, cmd)
	if err != nil {
		t.Fatal(err)
	}
	rel()
	raw, err := os.ReadFile(filepath.Join(dir, slotOutboxFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"abc123secret", "hunter2"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("outbox holds %q: %s", secret, raw)
		}
	}
	if h := ssOutbox(t, dir)[0]; !strings.Contains(h.Cmd, "go test") {
		t.Errorf("cmd %q", h.Cmd)
	}
	// and what is posted
	rec, url := ssServer(t)
	t.Setenv("WAYPOINT_SERVER", url)
	c2, _, _ := sqCLI()
	deadline := time.Now().Add(5 * time.Second)
	for len(readOutbox(dir)) > 0 && time.Now().Before(deadline) {
		_, _, _ = c2.sendSlotHolds(context.Background()) // the earlier background send may still hold the lock
		time.Sleep(50 * time.Millisecond)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.bodies) == 0 {
		t.Fatal("nothing posted")
	}
	for _, b := range rec.bodies {
		if strings.Contains(b, "abc123secret") || strings.Contains(b, "hunter2") {
			t.Errorf("posted a secret: %s", b)
		}
	}
	// the local key is unchanged (the classifier and waits.jsonl use it)
	if recs := readWaitRecs(dir); len(recs) != 1 || recs[0].Cmd != cmd {
		t.Errorf("waits.jsonl %+v", recs)
	}
}

func TestRunInSlotThenRecordsExitOnlyWhenTheCommandRan(t *testing.T) {
	c, dir := ssEnv(t, ssDown(t))
	var out, errb bytes.Buffer
	code, err := c.runInSlotThen("tests", 2, []string{"sh", "-c", "exit 7"}, &out, &errb, nil, nil)
	if code != 7 || err != nil {
		t.Fatalf("exit 7: %d %v", code, err)
	}
	if recs := ssOutbox(t, dir); len(recs) != 1 || recs[0].Exit == nil || *recs[0].Exit != 7 {
		t.Fatalf("recorded %+v", recs)
	}
	if _, err := c.runInSlotThen("tests", 2, []string{"/nonexistent/wp-no-such-command"}, &out, &errb, nil, nil); err == nil {
		t.Fatal("a missing command ran")
	}
	recs := ssOutbox(t, dir)
	if len(recs) != 2 || recs[1].Exit != nil {
		t.Errorf("a command that could not start has an exit: %+v", recs)
	}
}

func TestSendSlotHoldsStopsWhenTheOutboxCannotBeRewritten(t *testing.T) {
	var c *cli
	var dir string
	var hits int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 { // a record arrives meanwhile, so the outbox must be rewritten
			extra := ssHold(99)
			appendOutbox(dir, extra)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()
	c, dir = ssEnv(t, ts.URL)
	_ = os.MkdirAll(dir, 0o700)
	appendOutbox(dir, ssHold(1))
	old := rewriteOutbox
	rewriteOutbox = func(string, []byte) bool { return false }
	t.Cleanup(func() { rewriteOutbox = old })
	_, _, err := c.sendSlotHolds(context.Background())
	if err == nil || !strings.Contains(err.Error(), "could not be rewritten") || hits != 1 {
		t.Errorf("err %v, %d posts", err, hits)
	}
}

func TestSlotSendBatchFitsTheServerLimit(t *testing.T) {
	if slotSendBatch > core.SlotHoldsMax {
		t.Errorf("batch %d is over what the server takes (%d)", slotSendBatch, core.SlotHoldsMax)
	}
}

// slotPolicyVersion names the rules; this pins it to them, so a change to a
// rule without a bump fails here.
func TestSlotPolicyVersionPinnedToTheRules(t *testing.T) {
	var b strings.Builder
	fmt.Fprintf(&b, "long=%s runs=%d", slotLongRun, slotClassRuns)
	for _, class := range []string{slotClassShort, slotClassLong} {
		for n := 1; n <= 8; n++ {
			fmt.Fprintf(&b, " %s/%d=%v", class, n, slotOrder(class, n))
		}
	}
	const golden = "long=5m0s runs=5 short/1=[0] short/2=[1 0] short/3=[2 1 0] short/4=[3 2 1 0] short/5=[4 3 2 1 0] short/6=[5 4 3 2 1 0] short/7=[6 5 4 3 2 1 0] short/8=[7 6 5 4 3 2 1 0] long/1=[0] long/2=[0] long/3=[0] long/4=[0 1] long/5=[0 1] long/6=[0 1 2] long/7=[0 1 2] long/8=[0 1 2 3]"
	if b.String() != golden {
		t.Errorf("the slot rules changed: bump slotPolicyVersion and update this test\ngot %q", b.String())
	}
}

func TestEarlierHoldIsPostedWhenASlotIsTaken(t *testing.T) {
	rec, url := ssServer(t)
	c, dir := ssEnv(t, url)
	_ = os.MkdirAll(dir, 0o700)
	appendOutbox(dir, ssHold(1))
	_, rel, err := c.takeSlot(context.Background(), "tests", 2)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	deadline := time.Now().Add(5 * time.Second)
	for !rec.seen("id1") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !rec.seen("id1") {
		t.Error("the earlier hold was not posted")
	}
}

func TestNoRequestWhenSendingIsOff(t *testing.T) {
	rec, url := ssServer(t)
	c, dir := ssEnv(t, url)
	t.Setenv(slotSendEnv, "off")
	_ = os.MkdirAll(dir, 0o700)
	appendOutbox(dir, ssHold(1))
	_, rel, err := c.takeSlot(context.Background(), "tests", 2)
	if err != nil {
		t.Fatal(err)
	}
	rel()
	time.Sleep(300 * time.Millisecond)
	if n := rec.count(); n != 0 {
		t.Errorf("%d requests with sending off", n)
	}
}

func TestOutboxSkipsMalformedLinesAndRemoveSentDropsThem(t *testing.T) {
	_, dir := ssEnv(t, ssDown(t))
	_ = os.MkdirAll(dir, 0o700)
	good := func(i int) string { b, _ := json.Marshal(ssHold(i)); return string(b) }
	body := good(1) + "\nnot json\n" + `{"name":"no id"}` + "\n" + good(2) + "\n" + good(3) + "\n"
	p := filepath.Join(dir, slotOutboxFile)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if recs := readOutbox(dir); len(recs) != 3 {
		t.Fatalf("read %d records", len(recs))
	}
	if !removeSent(dir, map[string]bool{"id2": true}) {
		t.Fatal("removeSent failed")
	}
	recs := readOutbox(dir)
	if len(recs) != 2 || recs[0].ID != "id1" || recs[1].ID != "id3" {
		t.Errorf("after: %+v", recs)
	}
	if b, _ := os.ReadFile(p); strings.Contains(string(b), "not json") || strings.Contains(string(b), "no id") {
		t.Errorf("a malformed line stayed: %s", b)
	}
	if !removeSent(dir, map[string]bool{"id1": true, "id3": true}) {
		t.Fatal("removeSent failed")
	}
	if _, err := os.Stat(p); err == nil {
		t.Error("an emptied outbox stayed")
	}
}

func TestSlotSnapshotLeavesOutDeadAndOwnQueueFiles(t *testing.T) {
	_, dir := ssEnv(t, ssDown(t))
	_ = os.MkdirAll(dir, 0o700)
	q, _ := json.Marshal(queueRec{Since: 5, Class: "short"})
	for _, pid := range []int{2147483646, os.Getpid(), os.Getppid()} {
		_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("tests.%d.queue", pid)), q, 0o600)
	}
	_, waiters := slotSnapshot(dir, "tests")
	if len(waiters) != 1 || waiters[0].PID != os.Getppid() {
		t.Errorf("waiters %+v", waiters)
	}
}
