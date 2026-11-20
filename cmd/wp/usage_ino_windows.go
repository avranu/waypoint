//go:build windows

package main

import "os"

func inodeOf(os.FileInfo) uint64 { return 0 }
