//go:build !linux

// waypoint-sandboxd runs on Linux only.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "waypoint-sandboxd: sandboxes need Linux")
	os.Exit(1)
}
