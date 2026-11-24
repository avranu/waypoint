package core

import (
	"os"
	"strings"
	"testing"
	"time"
)

// WP-460: an issue records what its project's production environment ran
// when it was written, and its detail says what runs now and how many
// releases came between.
func TestIssueWritten(t *testing.T) {
	x := newEnvT(t)
	deploy := func(env, commit, version string) {
		t.Helper()
		x.setClock(x.clockNow().Add(time.Minute))
		if _, err := x.s.RecordDeploymentNamed(x.ctx, x.editor, x.p.Key, env, DeployInput{Commit: commit, Version: version, Status: "succeeded"}); err != nil {
			t.Fatal(err)
		}
	}
	create := func(title string) *IssueDetail {
		t.Helper()
		x.setClock(x.clockNow().Add(time.Minute))
		d, err := x.s.CreateIssue(x.ctx, x.editor, IssueInput{Project: x.p.Key, Title: title})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	get := func(key string) *Written {
		t.Helper()
		d, err := x.s.GetIssue(x.ctx, x.viewer, key)
		if err != nil {
			t.Fatal(err)
		}
		return d.Written
	}

	before := create("Written before anything was deployed")
	deploy("staging", "5555555aaaa", "0.9.0")
	onStaging := create("Written when only staging ran")
	deploy("production", "1111111aaaa", "1.0.0")
	first := create("Written against 1.0.0")
	deploy("production", "2222222bbbb", "1.1.0")
	deploy("production", "2222222bbbb", "1.1.0")
	deploy("production", "3333333cccc", "1.2.0")

	if w := get(before.Key); w != nil {
		t.Fatalf("nothing was live: %+v", w)
	}
	if w := get(onStaging.Key); w == nil || w.Environment != "staging" || w.Version != "0.9.0" || w.LiveVersion != "0.9.0" || w.Releases != 0 {
		t.Fatalf("staging stands in where there's no production: %+v", w)
	}
	w := get(first.Key)
	if w == nil || w.Environment != "production" || w.Version != "1.0.0" || w.Commit != "1111111aaaa" ||
		w.LiveVersion != "1.2.0" || w.LiveCommit != "3333333cccc" || w.Releases != 2 {
		t.Fatalf("written against 1.0.0, two releases since: %+v", w)
	}
	now := create("Written against 1.2.0")
	if w := get(now.Key); w == nil || w.Version != "1.2.0" || w.Releases != 0 {
		t.Fatalf("written against what runs now: %+v", w)
	}

	if _, err := x.s.db.W.ExecContext(x.ctx, `UPDATE issues SET written_env = NULL, written_version = '', written_commit = ''`); err != nil {
		t.Fatal(err)
	}
	sqlText, err := os.ReadFile("../db/migrations/0119_issue_written.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range strings.Split(string(sqlText), ";\n") {
		var lines []string
		for _, l := range strings.Split(stmt, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(l), "--") {
				lines = append(lines, l)
			}
		}
		stmt = strings.TrimSpace(strings.Join(lines, "\n"))
		if stmt == "" || strings.HasPrefix(stmt, "ALTER") {
			continue
		}
		if _, err := x.s.db.W.ExecContext(x.ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if w := get(before.Key); w != nil {
		t.Fatalf("backfill: nothing was live: %+v", w)
	}
	if w := get(first.Key); w == nil || w.Environment != "production" || w.Version != "1.0.0" || w.Releases != 2 {
		t.Fatalf("backfill: written against 1.0.0: %+v", w)
	}
	if w := get(onStaging.Key); w != nil {
		t.Fatalf("backfill: production had nothing yet: %+v", w)
	}
}
