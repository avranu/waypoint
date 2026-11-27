package main

import (
	"encoding/json"

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

// traceVars is the variables a line starts with: HOME as the session's own
// machine had it, inferred from its cwd as the server does
// (trscript.ShellSession.Edit). Outside any home there is none, as there:
// this machine's home is not the session's (a transcript uploaded from
// another machine than it ran on), and the two readings must agree.
func traceVars(cwd string) map[string]string {
	vars := map[string]string{}
	if home := trscript.ShellHomeOf(cwd); home != "" {
		vars["HOME"] = home
	}
	return vars
}

// shell is the session's memory in the form the shared recogniser takes; put
// keeps what a call added to it.
func (t *traceState) shell() trscript.ShellSession {
	return trscript.ShellSession{Roots: t.Roots, Scripts: t.Scripts}
}

func (t *traceState) put(s trscript.ShellSession) { t.Roots, t.Scripts = s.Roots, s.Scripts }

// note records a directory a record of the transcript was in.
func (t *traceState) note(cwd string) {
	if n := len(t.Roots); n > 0 && t.Roots[n-1] == cwd {
		return
	}
	s := t.shell()
	s.Note(cwd)
	t.put(s)
}

// shellEdit says a Bash call wrote a file of the work. It is the server's
// own reading (trscript.ShellSession.Edit: the session's directories decide,
// not the disk of the machine that uploads), so the shell_edit rows and
// session facts count the same calls. known is false when the command could
// not be read whole (over the reader's limit, or it runs a script too long to
// read in which the part read writes nothing): "no write" is then not the
// answer. cwd is the record's own directory, as on the server.
// (Each Bash call gets one key: trscript.ShellEditKind if it wrote,
// ShellNoneKind if it was read whole and wrote nothing, ShellUnknownKind if
// it could not be read. Rows uploaded by an older client have none of the
// three, which is "unknown".)
func (t *traceState) shellEdit(input json.RawMessage, cwd string) (wrote, known bool) {
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(input, &in) != nil {
		return false, false // the command was never read
	}
	s := t.shell()
	wrote, known = s.Edit(in.Command, cwd)
	t.put(s)
	return wrote, known
}

const traceMaxScripts = trscript.ShellMaxScripts
