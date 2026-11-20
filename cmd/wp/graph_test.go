package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// wp graph: what lies around a task, and the route between two (WP-385).
func TestCLIGraph(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	ctx := context.Background()
	p, _ := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Atlas")})
	issue := func(title, parent string) string {
		t.Helper()
		i, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: title, Parent: parent})
		if err != nil {
			t.Fatal(err)
		}
		return i.Key
	}
	epic := issue("Offline maps", "")
	tiles := issue("Cache map tiles", epic)
	evict := issue("Evict stale tiles", epic)
	lone := issue("Rename the settings page", "")
	if err := e.svc.AddRelation(ctx, me, evict, "blocks", tiles); err != nil {
		t.Fatal(err)
	}

	out := e.wp("graph", tiles, "--sure")
	for _, want := range []string{tiles + " <untrusted>Cache map tiles</untrusted>", evict + " <untrusted>Evict stale tiles</untrusted>", "blocked by", epic + " <untrusted>Offline maps</untrusted>", "subtask of"} {
		if !strings.Contains(out, want) {
			t.Errorf("graph lacks %q:\n%s", want, out)
		}
	}
	out = e.wp("graph", tiles, lone, "--sure")
	if !strings.Contains(out, "Nothing links") {
		t.Errorf("no route:\n%s", out)
	}
	out = e.wp("graph", tiles, evict)
	if !strings.Contains(out, "distance 1.0, 1 steps") || !strings.Contains(out, "blocked by "+evict) {
		t.Errorf("route:\n%s", out)
	}
	var v core.GraphView
	if err := json.Unmarshal([]byte(e.wp("graph", epic, "--json", "--hops", "1")), &v); err != nil || len(v.Nodes) != 2 {
		t.Errorf("json: %v %+v", err, v)
	}
	if out, code := e.run("graph"); code == 0 {
		t.Errorf("graph with nothing:\n%s", out)
	}
}
