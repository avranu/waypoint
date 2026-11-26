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
func traceCommands(cmd, cwd string, vars map[string]string, mem *traceScripts, out *[]traceCommand) string {
	cmds, dir := trscript.ReadShell(cmd, cwd, vars, mem, true)
	*out = append(*out, cmds...)
	return dir
}

// bashWritesWork says a Bash call wrote a file of the work, by the trace's
// reading of the command (a write to a scratch directory or one that is only
// a report does not). mem is the scripts the transcript kept in files so far
// (trscript.ShellEditKind is what the count is uploaded as: a row for every
// hour a session ran Bash, calls = those that did; rows uploaded before that
// kind have none, which is "unknown").
func bashWritesWork(input json.RawMessage, cwd string, mem *traceScripts) bool {
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(input, &in) != nil || in.Command == "" {
		return false
	}
	home, _ := os.UserHomeDir()
	var cmds []traceCommand
	traceCommands(in.Command, cwd, map[string]string{"HOME": home}, mem, &cmds)
	class, _, _ := classifyTraceBash(cmds, cwd)
	return class == trEdit
}

const traceMaxScripts = trscript.ShellMaxScripts
