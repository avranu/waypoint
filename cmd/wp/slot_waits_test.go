package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSlotWaitReader(t *testing.T) {
	base := time.UnixMilli(1_000_000_000_000)
	at := func(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }
	ms := func(sec int) string { return strconv.FormatInt(at(sec).UnixMilli(), 10) }
	write := func(lines ...string) string {
		path := filepath.Join(t.TempDir(), "waits")
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	live := map[int]bool{10: true, 11: true}
	newReader := func(path string) *slotWaitReader {
		r := newSlotWaitReader(path)
		r.alive = func(pid int) bool { return live[pid] }
		return r
	}

	r := newReader(write(
		"wait "+ms(0)+" 10 tests", "got "+ms(30)+" 10 tests",
		"wait "+ms(20)+" 11 tests", "got "+ms(50)+" 11 tests",
		"wait "+ms(100)+" 10 tests", "got "+ms(110)+" 10 tests",
	))
	if total, since := r.read(at(200)); total != 60*time.Second || !since.IsZero() {
		t.Errorf("union: %v %v", total, since)
	}

	r = newReader(write(
		"wait "+ms(0)+" 10 tests", "got "+ms(30)+" 10 tests",
		"wait "+ms(20)+" 11 tests",
	))
	if total, since := r.read(at(100)); total != 100*time.Second || !since.Equal(at(0)) {
		t.Errorf("open: %v %v", total, since)
	}

	live[11] = false
	if total, since := r.read(at(120)); total != 120*time.Second || !since.IsZero() {
		t.Errorf("dead pid: %v %v", total, since)
	}
	if total, _ := r.read(at(500)); total != 120*time.Second {
		t.Errorf("dead pid's wait kept growing: %v", total)
	}

	r = newReader(write(
		"garbage", "wait x 10 tests", "wait "+ms(0)+" 0 tests", "wait "+ms(0)+" 10",
		"got "+ms(5)+" 10 tests", "wait "+ms(10)+" 10 tests", "got "+ms(15)+" 10 tests",
	))
	if total, since := r.read(at(60)); total != 5*time.Second || !since.IsZero() {
		t.Errorf("malformed: %v %v", total, since)
	}
	if total, since := newReader(filepath.Join(t.TempDir(), "none")).read(at(1)); total != 0 || !since.IsZero() {
		t.Errorf("missing file: %v %v", total, since)
	}
}

func TestTakeSlotNotesItsWait(t *testing.T) {
	slotEnv(t)
	t.Setenv(slotHeldEnv, "")
	waits := filepath.Join(t.TempDir(), "waits")
	read := func() string {
		b, _ := os.ReadFile(waits)
		return string(b)
	}
	take := func() {
		c := &cli{out: &strings.Builder{}, err: &strings.Builder{}}
		_, release, err := c.takeSlot(t.Context(), "tests", 1)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}

	t.Setenv(slotWaitsEnv, "")
	take()
	if _, err := os.Stat(waits); err == nil {
		t.Fatal("wrote a waits file without the variable")
	}

	t.Setenv(slotWaitsEnv, waits)
	if err := os.WriteFile(waits, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	take()
	lines := strings.Split(strings.TrimSpace(read()), "\n")
	pid := strconv.Itoa(os.Getpid())
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "wait ") || !strings.HasSuffix(lines[0], " "+pid+" tests") ||
		!strings.HasPrefix(lines[1], "got ") || !strings.HasSuffix(lines[1], " "+pid+" tests") ||
		!strings.HasPrefix(lines[2], "free ") || !strings.HasSuffix(lines[2], " "+pid+" tests") {
		t.Fatalf("waits file: %q", read())
	}

	if err := os.WriteFile(waits, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dir, _ := slotDir()
	t.Setenv(slotHeldEnv, "tests="+url.PathEscape(dir)+"@"+pid)
	take()
	if read() != "" {
		t.Fatalf("a nested run wrote %q", read())
	}
}

func TestSlotWaitReaderSaysWhetherAProcessHoldsASlot(t *testing.T) {
	base := time.UnixMilli(1_000_000_000_000)
	at := func(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }
	ms := func(sec int) string { return strconv.FormatInt(at(sec).UnixMilli(), 10) }
	live := map[int]bool{10: true, 11: true}
	scan := func(lines ...string) (waiting time.Time, holding bool) {
		path := filepath.Join(t.TempDir(), "waits")
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		r := newSlotWaitReader(path)
		r.alive = func(pid int) bool { return live[pid] }
		_, waiting, holding = r.scan(at(100))
		return waiting, holding
	}
	if w, h := scan("wait " + ms(0) + " 10 tests"); w.IsZero() || h {
		t.Errorf("waiting: since %v holding %v", w, h)
	}
	if w, h := scan("wait "+ms(0)+" 10 tests", "got "+ms(5)+" 10 tests"); !w.IsZero() || !h {
		t.Errorf("holding: since %v holding %v", w, h)
	}
	if w, h := scan("wait "+ms(0)+" 10 tests", "got "+ms(5)+" 10 tests", "free "+ms(9)+" 10 tests"); !w.IsZero() || h {
		t.Errorf("released: since %v holding %v", w, h)
	}
	if _, h := scan("got "+ms(5)+" 11 tests", "free "+ms(9)+" 10 tests"); !h {
		t.Errorf("another pid's free line ended pid 11's hold")
	}
	if w, h := scan("wait "+ms(0)+" 10 tests", "got "+ms(5)+" 10 tests", "wait "+ms(20)+" 10 tests"); w.IsZero() || h {
		t.Errorf("waited again after a hold that never said free: since %v holding %v", w, h)
	}
	live[11] = false
	if _, h := scan("wait "+ms(0)+" 11 tests", "got "+ms(5)+" 11 tests"); h {
		t.Errorf("a holder that died (no free line) still holds")
	}
	live[11] = true
	if _, h := scan("got "+ms(5)+" 10 tests", "got "+ms(6)+" 11 autopush", "free "+ms(7)+" 10 tests"); !h {
		t.Errorf("two holders, one freed: holding %v", h)
	}
}
