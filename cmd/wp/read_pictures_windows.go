//go:build windows

package main

import "os"

// ownedByMe: Windows keeps each user's temp directory private.
func ownedByMe(os.FileInfo) bool { return true }
