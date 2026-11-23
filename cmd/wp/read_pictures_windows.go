//go:build windows

package main

import "os"

// ownedByMe: no file is known to be the user's on Windows, so no picture is
// let through unasked there (WP-1137): owners aren't checked, the paths here
// are compared with "/", and where Claude Code keeps a scratchpad on Windows
// isn't known. Claude Code's own rules decide.
func ownedByMe(os.FileInfo) bool { return false }

// pathPrivate: not known on Windows either (ownedByMe).
func pathPrivate(string, os.FileInfo) bool { return false }
