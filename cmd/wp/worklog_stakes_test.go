package main

import (
	"strings"
	"testing"
)

func TestCLIWorklogStakes(t *testing.T) {
	e := newEnv(t)
	e.login()
	if out := e.wp("worklog", "stakes", "--days", "7"); !strings.Contains(out, "No review rounds in the last 7 days") {
		t.Fatalf("%s", out)
	}
	if out, code := e.run("worklog", "stakes", "--days", "0"); code != 2 || !strings.Contains(out, "--days must be 1 to 365") {
		t.Fatalf("%d %s", code, out)
	}
	if out := e.wp("worklog", "stakes", "--json", "--rows"); !strings.Contains(out, `"rule": "stakes-v2"`) && !strings.Contains(out, `"rule":"stakes-v2"`) {
		t.Fatalf("%s", out)
	}
}
