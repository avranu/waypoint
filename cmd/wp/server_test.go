package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestServerDiagnosticsCLI(t *testing.T) {
	e := newEnv(t)
	t.Chdir(t.TempDir())
	e.login()

	if out := e.wp("server", "status"); !strings.Contains(out, "Schema: ") || !strings.Contains(out, "Queues (waiting, running):") {
		t.Errorf("status:\n%s", out)
	}
	if out := e.wp("server", "metrics", "--grep", "go_goroutines"); !strings.Contains(out, "# TYPE go_goroutines gauge") || strings.Contains(out, "go_threads") {
		t.Errorf("metrics:\n%s", out)
	}
	if out := e.wp("ai", "usage", "--since", "2d", "--by", "model"); !strings.Contains(out, "No AI requests in this window") {
		t.Errorf("usage:\n%s", out)
	}
	if out, code := e.run("ai", "usage", "--by", "colour"); code == 0 || !strings.Contains(out, "say ") {
		t.Errorf("bad --by: exit %d\n%s", code, out)
	}
	if out, code := e.run("server", "log", "--since", "soon"); code == 0 || !strings.Contains(out, "soon") {
		t.Errorf("bad --since: exit %d\n%s", code, out)
	}
	if out, code := e.run("server", "log"); code == 0 || !strings.Contains(out, "keeps no log records") {
		t.Errorf("log without a ring: exit %d\n%s", code, out)
	}

	ctx := context.Background()
	u, err := e.svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "sam", Email: "s@example.com", Password: "correct horse battery"})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := e.svc.CreateToken(ctx, &core.Actor{User: u}, core.TokenSpec{Name: "sam"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAYPOINT_TOKEN", tok)
	for _, args := range [][]string{{"server", "status"}, {"server", "log"}, {"server", "metrics"}, {"ai", "usage"}} {
		if out, code := e.run(args...); code == 0 || strings.Contains(out, "go_goroutines") || strings.Contains(out, "Schema:") {
			t.Errorf("wp %s by a non-admin: exit %d\n%s", strings.Join(args, " "), code, out)
		}
	}
}

// wp server problems, job, ack and unack against a real server (WP-ADR139).
func TestServerProblemsCLI(t *testing.T) {
	e := newEnv(t)
	t.Chdir(t.TempDir())
	robin := e.login()
	ctx := context.Background()

	if out := e.wp("server", "problems"); !strings.Contains(out, "Nothing is going wrong") {
		t.Errorf("no problem yet:\n%s", out)
	}
	if out, code := e.run("server", "job", "forge sync"); code == 0 || !strings.Contains(out, `nothing is recorded for "forge sync"`) {
		t.Errorf("a job that never ran: exit %d\n%s", code, out)
	}
	for range 3 {
		e.svc.Log().Error("repo sync failed", "repo", "acme/app", "err", "404 Not Found")
	}
	e.svc.Log().Warn("slow write", "ms", 900)

	var listed []struct {
		ID      int64  `json:"id"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(e.wp("server", "problems", "--json")), &listed); err != nil {
		t.Fatal(err)
	}
	id := ""
	for _, p := range listed {
		if p.Message == "repo sync failed" {
			id = strconv.FormatInt(p.ID, 10)
		}
	}
	if len(listed) != 2 || id == "" {
		t.Fatalf("problems: %+v", listed)
	}

	out := e.wp("server", "problems")
	if !strings.Contains(out, "#"+id+" error background: repo sync failed (acme/app); 3× over ") || !strings.Contains(out, "    repo=acme/app err=\"404 Not Found\"") || !strings.Contains(out, "background: slow write; 1× over ") {
		t.Errorf("problems:\n%s", out)
	}
	if out := e.wp("server", "problems", "--source", "requests"); !strings.Contains(out, "Nothing is going wrong") {
		t.Errorf("--source requests:\n%s", out)
	}
	if out := e.wp("server", "problems", "--limit", "1", "--json"); strings.Count(out, `"source"`) != 1 {
		t.Errorf("--limit 1 --json:\n%s", out)
	}
	if out := e.wp("server", "status"); !strings.Contains(out, "Problems: 2 open, none persistent and unacknowledged (wp server problems).") {
		t.Errorf("status:\n%s", out)
	}
	if out := e.wp("server", "job", "background", "--since", "2h"); !strings.Contains(out, "background: what is logged there, not a scheduled job") || !strings.Contains(out, "By hour (UTC)") || !strings.Contains(out, "#"+id+" error background: repo sync failed (acme/app)") {
		t.Errorf("job background:\n%s", out)
	}
	if out, code := e.run("server", "job", "background", "--by", "week"); code == 0 || !strings.Contains(out, "say hour or day") {
		t.Errorf("--by week: exit %d\n%s", code, out)
	}
	if out, code := e.run("server", "job"); code == 0 || !strings.Contains(out, "say which job") {
		t.Errorf("job with no name: exit %d\n%s", code, out)
	}

	if out, code := e.run("server", "ack", id, "-m", "known"); code == 0 || !strings.Contains(out, "acknowledging a server problem") {
		t.Errorf("ack by an agent: exit %d\n%s", code, out)
	}
	tok, _, err := e.svc.CreateToken(ctx, robin, core.TokenSpec{Name: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAYPOINT_TOKEN", tok)
	t.Setenv("CLAUDECODE", "1")
	if out, code := e.run("server", "ack", id, "-m", "known"); code == 0 || !strings.Contains(out, "is for people, not agents") {
		t.Errorf("ack from Claude Code: exit %d\n%s", code, out)
	}
	t.Setenv("WAYPOINT_AGENT", "none") // WAYPOINT_AGENT=none marks a person's shell.
	if out, code := e.run("server", "ack", id, "-m", "known"); code == 0 || !strings.Contains(out, "not in an agent's shell") {
		t.Errorf("ack in an agent's shell: exit %d\n%s", code, out)
	}
	t.Setenv("CLAUDECODE", "")
	for args, want := range map[string]string{
		"server ack":           "say which problem",
		"server ack one -m x":  "is not a problem's number",
		"server ack 1":         "say why it is expected",
		"server ack 99 -m x":   "no server problem 99",
		"server unack 99":      "no server problem 99",
		"server unack 1 extra": "say which problem",
	} {
		if out, code := e.run(strings.Fields(args)...); code == 0 || !strings.Contains(out, want) {
			t.Errorf("wp %s: exit %d, want %q in\n%s", args, code, want, out)
		}
	}
	if out := e.wp("server", "ack", "#"+id, "-m", "the repo is archived"); !strings.Contains(out, "Acknowledged #"+id+" background: repo sync failed (acme/app).") {
		t.Errorf("ack:\n%s", out)
	}
	if out := e.wp("server", "problems"); !strings.Contains(out, "; acknowledged by robin: the repo is archived") {
		t.Errorf("after ack:\n%s", out)
	}
	if out := e.wp("server", "unack", id); !strings.Contains(out, "#"+id+" background: repo sync failed (acme/app) is no longer acknowledged") {
		t.Errorf("unack:\n%s", out)
	}
	if out := e.wp("server", "problems"); strings.Contains(out, "acknowledged") {
		t.Errorf("after unack:\n%s", out)
	}

	u, err := e.svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "sam", Email: "s@example.com", Password: "correct horse battery"})
	if err != nil {
		t.Fatal(err)
	}
	if tok, _, err = e.svc.CreateToken(ctx, &core.Actor{User: u}, core.TokenSpec{Name: "sam"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAYPOINT_TOKEN", tok)
	for _, args := range [][]string{{"server", "problems"}, {"server", "job", "background"}, {"server", "ack", id, "-m", "x"}, {"server", "unack", id}} {
		if out, code := e.run(args...); code == 0 || strings.Contains(out, "repo sync failed") {
			t.Errorf("wp %s by a non-admin: exit %d\n%s", strings.Join(args, " "), code, out)
		}
	}
}
