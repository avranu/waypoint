//go:build linux

package main

import (
	"os"
	"time"
)

// procAge reads this process's age from /proc: the start time in clock ticks
// since boot (field 22 of /proc/self/stat) against /proc/uptime.
func procAge() (time.Duration, bool) {
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, false
	}
	up, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, false
	}
	return ageFromProc(string(stat), string(up))
}
