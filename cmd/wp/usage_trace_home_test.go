package main

import "testing"

// Outside any home the trace sets no HOME, as the server's reading sets none:
// both leave $HOME/x a path they cannot place (audit 2103.8).
func TestTraceVarsSetNoHomeOutsideAHome(t *testing.T) {
	t.Setenv("HOME", "/home/claude")
	if h, ok := traceVars("/srv/app")["HOME"]; ok {
		t.Errorf("HOME = %q in /srv/app, want none", h)
	}
	var cmds []traceCommand
	var mem traceScripts
	traceCommands("echo x > $HOME/app/a.go", "/srv/app", traceVars("/srv/app"), &mem, &cmds)
	if len(cmds) != 1 || len(cmds[0].Wrote) != 1 || cmds[0].Wrote[0].Path != "$HOME/app/a.go" {
		t.Errorf("commands %+v, want one write to $HOME/app/a.go", cmds)
	}
}
