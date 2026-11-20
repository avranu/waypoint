//go:build darwin

package main

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// procAge reads this process's start time from the kernel's process table.
func procAge() (time.Duration, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", os.Getpid())
	if err != nil {
		return 0, false
	}
	st := kp.Proc.P_starttime
	if st.Sec == 0 {
		return 0, false
	}
	age := time.Since(time.Unix(int64(st.Sec), int64(st.Usec)*1000))
	if age < 0 {
		return 0, false
	}
	return age, true
}
