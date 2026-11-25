package main

import (
	"encoding/json"
	"os"

	trscript "git.jmann.me/claude/waypoint/internal/transcript"
)

// The shell-write recogniser lives in internal/transcript (WP-1883), so the
// server's session facts read commands as the client's trace does.
type (
	traceCommand = trscript.ShellCommand
	traceScripts = trscript.ShellScripts
	traceScript  = trscript.ShellScript
)

const traceNowhere = trscript.ShellNowhere

func traceFileShape(w string) bool { return trscript.ShellFileShape(w) }

// traceCommands lists the commands of a line (see trscript.ReadShell) and
// returns the directory the line ends in.
func traceCommands(cmd, cwd string, depth int, vars map[string]string, mem *traceScripts, out *[]traceCommand) string {
	cmds, dir := trscript.ReadShell(cmd, cwd, vars, mem, true)
	*out = append(*out, cmds...)
	return dir
}

// shellEditKind is the tool_hours kind of the per-hour shell-edit count: one
// row (tool "Bash") for every hour a session ran Bash, with calls = the Bash
// calls that wrote a file of the work (a row with 0 calls says the hour was
// counted). Rows uploaded before this kind have none: that is "unknown".
const shellEditKind = "shell_edit"

// bashWritesWork says a Bash call wrote a file of the work, by the trace's
// reading of the command (a write to a scratch directory or one that is only
// a report does not).
func bashWritesWork(input json.RawMessage, cwd string) bool {
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(input, &in) != nil || in.Command == "" {
		return false
	}
	home, _ := os.UserHomeDir()
	var cmds []traceCommand
	traceCommands(in.Command, cwd, 0, map[string]string{"HOME": home}, nil, &cmds)
	class, _, _ := classifyTraceBash(cmds, cwd)
	return class == trEdit
}

const traceMaxScripts = trscript.ShellMaxScripts
