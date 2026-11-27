//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProcMeterReadsTheMachine(t *testing.T) {
	root := t.TempDir()
	write := func(name, text string) {
		p := filepath.Join(root, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("proc/loadavg", "14.18 11.57 11.67 3/2000 12345\n")
	write("proc/pressure/cpu", "some avg10=28.98 avg60=24.19 avg300=22.80 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")
	write("proc/pressure/memory", "some avg10=0.00 avg60=1.50 avg300=0.04 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.01 total=1\n")
	write("proc/pressure/io", "some avg10=5.21 avg60=6.36 avg300=4.60 total=1\nfull avg10=2.53 avg60=2.81 avg300=2.16 total=1\n")
	write("proc/meminfo", "MemTotal:       49000000 kB\nMemFree:  2000000 kB\nMemAvailable:   36750000 kB\n")
	write("proc/stat", "cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 1 1 1 1\n")
	m := &procMeter{root: root}
	s := m.sample()
	if !s.measured || s.Load1 != 14.18 || s.CPUSome != 24.19 || s.MemSome != 1.5 || s.IOFull != 2.81 || s.MemFree != 0.75 || s.Busy != -1 {
		t.Fatalf("first look %+v", s)
	}
	write("proc/stat", "cpu  250 0 250 750 150 0 0 0 0 0\n")
	if s = m.sample(); s.Busy != 0.75*float64(s.CPUs) {
		t.Fatalf("busy %v of %d CPUs, want three quarters", s.Busy, s.CPUs)
	}
	if s.Own != -1 {
		t.Errorf("own CPU known without a cgroup: %v", s.Own)
	}
	m = &procMeter{root: filepath.Join(root, "nothing")}
	if s := m.sample(); s.measured {
		t.Errorf("measured without /proc: %+v", s)
	}
}
