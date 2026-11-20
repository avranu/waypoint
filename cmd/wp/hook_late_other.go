//go:build !linux && !darwin

package main

import "time"

// procAge has no source here: the age falls back to time since init.
func procAge() (time.Duration, bool) { return 0, false }
