package main

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func slotEnv(t *testing.T) {
	t.Helper()
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_SERVER", "https://waypoint.example.com")
	t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
}

func TestSlotSerializesWithOneSlot(t *testing.T) {
	slotEnv(t)
	ctx := context.Background()
	first, err := acquireSlot(ctx, "tests", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	old := slotWaitNotice
	slotWaitNotice = 50 * time.Millisecond
	t.Cleanup(func() { slotWaitNotice = old })

	var notices atomic.Int32
	got := make(chan *slotHold, 1)
	go func() {
		h, err := acquireSlot(ctx, "tests", 1, func() { notices.Add(1) })
		if err != nil {
			t.Error(err)
		}
		got <- h
	}()
	select {
	case <-got:
		t.Fatal("second holder got the only slot while the first held it")
	case <-time.After(300 * time.Millisecond):
	}
	if n := notices.Load(); n != 1 {
		t.Errorf("waiting notice printed %d times, want once", n)
	}
	first.release()
	select {
	case h := <-got:
		h.release()
	case <-time.After(5 * time.Second):
		t.Fatal("second holder never got the slot after release")
	}
	// A second slot lets both hold at once.
	a, _ := acquireSlot(ctx, "pair", 2, nil)
	b, err := acquireSlot(ctx, "pair", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.release()
	b.release()
}

func TestSlotRunPassesExitCodeAndFreesSlot(t *testing.T) {
	slotEnv(t)
	for _, tc := range []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"slot", "run", "t", "-n", "1", "--", "sh", "-c", "echo hi"}, 0, "hi"},
		{[]string{"slot", "run", "t", "-n", "1", "--", "sh", "-c", "exit 7"}, 7, ""},
		{[]string{"slot", "run", "t", "-n", "1", "--", "sh", "-c", "kill -9 $$"}, 137, ""},
	} {
		var out, errb bytes.Buffer
		if code := run(tc.args, &out, &errb); code != tc.code || !strings.Contains(out.String(), tc.out) {
			t.Errorf("%v: code %d out %q err %q", tc.args, code, out.String(), errb.String())
		}
	}
	// Each run freed the slot, or the next would have waited forever.
	h, err := acquireSlot(context.Background(), "t", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"slot", "ls"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "t.0") || !strings.Contains(out.String(), "held pid") {
		t.Errorf("slot ls: %d %q %q", code, out.String(), errb.String())
	}
	h.release()
	out.Reset()
	run([]string{"slot", "ls"}, &out, &errb)
	if !strings.Contains(out.String(), "free") {
		t.Errorf("slot ls after release: %q", out.String())
	}
	if run([]string{"slot", "run", "t", "sh"}, &out, &errb) == 0 {
		t.Error("slot run without -- should fail")
	}
}

// A run inside a run of the same NAME uses the slot it is in: with one slot,
// waiting for a second would never end.
func TestSlotNestedRunReusesTheSlot(t *testing.T) {
	slotEnv(t)
	t.Setenv(slotHeldEnv, "")
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The test binary runs wp's main when WP_TEST_MAIN is set (TestMain).
	t.Setenv("WP_TEST_MAIN", "1")
	var out, errb bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"slot", "run", "tests", "-n", "1", "--", bin, "slot", "run", "tests", "-n", "1", "--", "sh", "-c", "echo inner $WAYPOINT_SLOTS_HELD"}, &out, &errb)
	}()
	select {
	case code := <-done:
		if code != 0 || !strings.Contains(out.String(), "inner tests") {
			t.Fatalf("nested run: %d %q %q", code, out.String(), errb.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nested run waited on its own slot")
	}
	// Only the outer run is a run: the inner one, inside its slot and time,
	// recorded nothing.
	dir, _ := slotDir()
	b, _ := os.ReadFile(filepath.Join(dir, slotRunsFile))
	if lines := strings.Split(strings.TrimSpace(string(b)), "\n"); len(lines) != 1 || !strings.HasPrefix(lines[0], `{"name":"tests","cmd":"`+bin+" slot run") {
		t.Errorf("runs.jsonl after a nested run:\n%s", b)
	}
	// A different NAME is still taken separately.
	t.Setenv(slotHeldEnv, "other="+url.PathEscape(dir)+"@"+strconv.Itoa(os.Getpid()))
	if !slotHeld("other") || slotHeld("tests") {
		t.Fatal("slotHeld misreads the list")
	}
	// A marker whose holder has gone (a background leftover) doesn't count.
	t.Setenv(slotHeldEnv, "other="+url.PathEscape(dir)+"@2147483646")
	if slotHeld("other") {
		t.Fatal("a marker with a dead holder counted")
	}
	t.Setenv(slotHeldEnv, "other="+url.PathEscape(dir)+"@"+strconv.Itoa(os.Getpid()))
	t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
	if slotHeld("other") {
		t.Fatal("a slot held in another directory counted")
	}
}

func TestSlotFieldStaysOneField(t *testing.T) {
	got := slotField("go test\t-run\nX\r" + strings.Repeat("a", 300))
	if strings.ContainsAny(got, "\t\n\r") || len(got) > 210 || !strings.HasPrefix(got, "go test -run X ") {
		t.Fatalf("slotField = %q", got)
	}
}
