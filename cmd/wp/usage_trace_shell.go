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

// traceMaxCommand is the longest command the reader reads at all; a longer
// one gives no commands, which must not read as "wrote nothing".
const traceMaxCommand = trscript.ShellMaxCommand

// traceVars is the variables a line starts with: HOME as the session's own
// machine had it, inferred from its cwd as the server does, else this
// machine's (a transcript uploaded from another machine than it ran on).
func traceVars(cwd string) map[string]string {
	home := trscript.ShellHomeOf(cwd)
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return map[string]string{"HOME": home}
}

// bashWritesWork says a Bash call wrote a file of the work, by the trace's
// reading of the command (a write to a scratch directory or one that is only
// a report does not). known is false when the command could not be read whole
// (over the reader's limit, or it runs a script too long to read in which
// the part read writes nothing): "no write" is then not the answer.
// mem is the scripts the transcript kept in files so far
// (trscript.ShellEditKind is what the count is uploaded as: a row for every
// hour a session ran Bash, calls = those that did; rows uploaded before that
// kind have none, which is "unknown").
func bashWritesWork(input json.RawMessage, cwd string, mem *traceScripts) (wrote, known bool) {
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(input, &in) != nil || in.Command == "" {
		return false, true
	}
	if len(in.Command) > traceMaxCommand {
		return false, false
	}
	var cmds []traceCommand
	traceCommands(in.Command, cwd, traceVars(cwd), mem, &cmds)
	class, _, _ := classifyTraceBash(cmds, cwd)
	if class == trEdit {
		return true, true
	}
	for _, c := range cmds {
		if c.Unread {
			return false, false
		}
	}
	return false, true
}

const traceMaxScripts = trscript.ShellMaxScripts
