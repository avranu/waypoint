package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// prepModel answers a task's pointer prep with nothing and its evidence check
// with one claim.
type prepModel struct{}

func (prepModel) Models(context.Context, *core.AIProvider) ([]core.AIModel, error) { return nil, nil }

func (prepModel) Complete(_ context.Context, _ *core.AIProvider, _, _, prompt string, _ map[string]any) (json.RawMessage, string, error) {
	if strings.HasPrefix(prompt, "Evidence check for task ") {
		return json.RawMessage(`{"evidence": [], "claims": [{"quote": "checks are slow", "test": "time them", "ref": null}]}`), "", nil
	}
	return json.RawMessage(`{"files": [], "symbols": [], "tests": [], "related": [], "terms": [], "questions": []}`), "", nil
}

// GET /prep gives evidence, and draws the holder's experiment arm, only when
// asked with evidence=1: a client that can't print it isn't measured
// (WP-ADR157).
func TestPrepEvidenceOnRequest(t *testing.T) {
	ts, svc, database := setupServerDB(t, nil)
	ctx := context.Background()
	svc.LLM = prepModel{}
	if err := svc.EnsureDefaultAI(ctx, "http://opencode:4096", "", "opencode/big-pickle"); err != nil {
		t.Fatal(err)
	}
	u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "robin@example.com", Password: "correct-horse-battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	me := &core.Actor{User: u}
	agentTok, _, _ := svc.CreateToken(ctx, me, core.TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	agent := &client{t: t, base: ts.URL, http: http.DefaultClient, token: agentTok}
	if code, b := agent.do("POST", "/api/v1/projects", map[string]any{"name": "Prep", "key": "PRP"}); code != 200 {
		t.Fatalf("project: %d %s", code, b)
	}
	var issue map[string]any
	agent.json("POST", "/api/v1/issues", map[string]any{"project": "PRP", "title": "Speed up merges", "description": "The checks are slow."}, &issue)
	key := issue["key"].(string)
	if _, err := svc.UploadPrep(ctx, me, key, core.PrepBundle{Repo: "robin/app", Files: []core.PrepFile{{Path: "merge.go", Lines: 10}}}); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	if _, err := svc.AgentTake(ctx, &core.Actor{User: u, Agent: "anthropic/claude-opus-5-5"}, key, ""); err != nil {
		t.Fatal(err)
	}
	if code, b := agent.do("GET", "/api/v1/issues/"+key, nil); code != 200 || !strings.Contains(string(b), "claude-opus-5-5") {
		t.Fatalf("not claimed: %d %s", code, b)
	}
	units := func() (n int) {
		t.Helper()
		if err := database.R.QueryRow(`SELECT count(*) FROM experiment_units u JOIN experiment_runs r ON r.id = u.run_id WHERE r.experiment = 'prep_evidence'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, q := range []string{"?shown=1", "?shown=1&evidence=0", ""} {
		if code, b := agent.do("GET", "/api/v1/issues/"+key+"/prep"+q, nil); code != 200 || strings.Contains(string(b), "checks are slow") {
			t.Errorf("%q: %d %s", q, code, b)
		}
	}
	if n := units(); n != 0 {
		t.Fatalf("%d units without evidence=1", n)
	}
	// Asked with evidence=1, the task draws an arm.
	if code, b := agent.do("GET", "/api/v1/issues/"+key+"/prep?shown=1&evidence=1", nil); code != 200 || units() != 1 {
		t.Fatalf("evidence=1: %d units %d %s", code, units(), b)
	}
	// Which one is by chance, so what each arm is given is checked on tasks
	// given theirs beforehand.
	for _, arm := range []string{"none", "shown"} {
		var made map[string]any
		agent.json("POST", "/api/v1/issues", map[string]any{"project": "PRP", "title": "Speed up merges, " + arm, "description": "The checks are slow."}, &made)
		key := made["key"].(string)
		if _, err := svc.UploadPrep(ctx, me, key, core.PrepBundle{Repo: "robin/app", Files: []core.PrepFile{{Path: "merge.go", Lines: 10}}}); err != nil {
			t.Fatal(err)
		}
		svc.Wait()
		if _, err := svc.AgentTake(ctx, &core.Actor{User: u, Agent: "anthropic/claude-opus-5-5"}, key, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := database.W.Exec(`INSERT INTO experiment_units(run_id, issue_id, arm, via, assigned_at)
			SELECT r.id, i.id, ?, 'prep', 0 FROM experiment_runs r, issues i JOIN projects p ON p.id = i.project_id
			WHERE r.experiment = 'prep_evidence' AND r.stopped_at IS NULL AND p.key || '-' || i.number = ?`, arm, key); err != nil {
			t.Fatal(err)
		}
		code, b := agent.do("GET", "/api/v1/issues/"+key+"/prep?shown=1&evidence=1", nil)
		if got := strings.Contains(string(b), "checks are slow"); code != 200 || got != (arm == "shown") {
			t.Errorf("arm %s given the evidence: %v (%d %s)", arm, got, code, b)
		}
	}
	if n := units(); n != 3 {
		t.Errorf("%d units; want the three tasks'", n)
	}
}
