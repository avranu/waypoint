package main

import trscript "git.jmann.me/claude/waypoint/internal/transcript"

// The shell reader lives in internal/transcript, where the usage report's
// write recogniser needs it too.
type guardCmd = trscript.ShellCmd

const subPlaceholder = trscript.ShellSubPlaceholder

func parseShell(s string) []*guardCmd { return trscript.ParseShell(s) }

func isDigits(s string) bool { return trscript.IsDigits(s) }
