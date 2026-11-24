//go:build linux

package main

import (
	"bufio"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// procMeter reads the machine's load from /proc and the runner's own CPU
// time from its cgroup, when it runs as a systemd unit (a runner started in
// a terminal shares the terminal's, so its own share is unknown and counts
// as other work).
type procMeter struct {
	root      string
	cgroup    string
	at        time.Time
	busy, all uint64
	own       uint64
}

func newLoadMeter() loadMeter {
	m := &procMeter{root: "/"}
	if b, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if p, ok := strings.CutPrefix(l, "0::"); ok && strings.HasSuffix(p, ".service") {
				m.cgroup = path.Join("/sys/fs/cgroup", p)
			}
		}
	}
	return m
}

func (m *procMeter) read(p string) string {
	b, err := os.ReadFile(path.Join(m.root, p)) // #nosec G304 -- fixed /proc and cgroup files
	if err != nil {
		return ""
	}
	return string(b)
}

func (m *procMeter) sample() loadSample {
	s := loadSample{CPUs: runtime.NumCPU(), Load1: -1, CPUSome: -1, MemSome: -1, IOFull: -1, MemFree: -1, Busy: -1, Own: -1}
	if f := strings.Fields(m.read("proc/loadavg")); len(f) > 0 {
		if v, err := strconv.ParseFloat(f[0], 64); err == nil {
			s.Load1, s.measured = v, true
		}
	}
	if !s.measured {
		return s
	}
	s.CPUSome = psi(m.read("proc/pressure/cpu"), "some")
	s.MemSome = psi(m.read("proc/pressure/memory"), "some")
	s.IOFull = psi(m.read("proc/pressure/io"), "full")
	var total, avail float64
	sc := bufio.NewScanner(strings.NewReader(m.read("proc/meminfo")))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseFloat(f[1], 64)
		switch f[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			avail = v
		}
	}
	if total > 0 && avail > 0 {
		s.MemFree = avail / total
	}
	now := time.Now()
	busy, all := cpuJiffies(m.read("proc/stat"))
	own, ownOK := cgroupUsage(m.cgroup)
	if m.all > 0 && all > m.all {
		s.Busy = float64(busy-m.busy) / float64(all-m.all) * float64(s.CPUs)
		if ownOK && m.own > 0 && own >= m.own {
			if d := now.Sub(m.at).Seconds(); d > 0 {
				s.Own = float64(own-m.own) / 1e6 / d
			}
		}
	}
	m.at, m.busy, m.all, m.own = now, busy, all, own
	return s
}

// psi is the avg60 of a /proc/pressure line ("some" or "full"); -1 if none.
func psi(text, kind string) float64 {
	for _, l := range strings.Split(text, "\n") {
		f := strings.Fields(l)
		if len(f) < 3 || f[0] != kind {
			continue
		}
		for _, kv := range f[1:] {
			if v, ok := strings.CutPrefix(kv, "avg60="); ok {
				if x, err := strconv.ParseFloat(v, 64); err == nil {
					return x
				}
			}
		}
	}
	return -1
}

// cpuJiffies sums /proc/stat's first line: time busy, and all of it.
func cpuJiffies(stat string) (busy, all uint64) {
	line, _, _ := strings.Cut(stat, "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0, 0
	}
	for i, x := range f[1:] {
		if i >= 8 { // guest time is already in user
			break
		}
		v, _ := strconv.ParseUint(x, 10, 64)
		all += v
		if i != 3 && i != 4 {
			busy += v
		}
	}
	return busy, all
}

// cgroupUsage is the cgroup's CPU time so far, in microseconds.
func cgroupUsage(dir string) (uint64, bool) {
	if dir == "" {
		return 0, false
	}
	b, err := os.ReadFile(path.Join(dir, "cpu.stat")) // #nosec G304 -- the runner's own cgroup
	if err != nil {
		return 0, false
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "usage_usec "); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}
