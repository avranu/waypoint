package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// addHeader sends a header with every request, as a shell does.
type addHeader struct {
	key, value string
}

func (a addHeader) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(a.key, a.value)
	return http.DefaultTransport.RoundTrip(r)
}

// An objective over the API: an agent proposes, a person confirms and
// steers, and an agent (or a person's token in an agent's shell) can't
// steer. The new fields come back as they were written.
func TestObjectiveAPIProposeConfirmAndSteer(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "robin@example.com", Password: "correct-horse-battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	a := &core.Actor{User: u}
	kind, base := "forgejo", "https://git.example.com"
	f, err := svc.CreateForge(ctx, a, core.ForgeInput{Kind: &kind, BaseURL: &base})
	if err != nil {
		t.Fatal(err)
	}
	name := "App"
	p, err := svc.CreateProject(ctx, a, core.ProjectInput{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddProjectRepo(ctx, a, p.Key, f.ID, "me/app", false); err != nil {
		t.Fatal(err)
	}
	task, err := svc.CreateIssue(ctx, a, core.IssueInput{Project: p.Key, Title: "Fast parse", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/issues/" + task.Key + "/objective"
	cl := func(spec core.TokenSpec, h *addHeader) *client {
		spec.Name = "t"
		tok, _, err := svc.CreateToken(ctx, a, spec)
		if err != nil {
			t.Fatal(err)
		}
		hc := http.DefaultClient
		if h != nil {
			hc = &http.Client{Transport: *h}
		}
		return &client{t: t, base: ts.URL, http: hc, token: tok}
	}
	person := cl(core.TokenSpec{}, nil)
	agent := cl(core.TokenSpec{Agent: "anthropic/claude-opus-5-5"}, nil)
	shell := cl(core.TokenSpec{}, &addHeader{"X-Waypoint-Agent-Env", "claude"})
	readOnly := cl(core.TokenSpec{ReadOnly: true}, nil)

	// The agent proposes it; nothing runs, and what it wrote comes back.
	var ob core.Objective
	body := map[string]any{"check": "./parse", "until": "< 1.5ms", "keep_passing": []string{"go test ./..."}, "judge": "Parsing is simple to read.", "judge_bar": "high", "max_rounds": 4, "pace": "spare"}
	if code := agent.json("PUT", path, body, &ob); code != 200 {
		t.Fatalf("proposing: %d", code)
	}
	if ob.State != core.ObjectiveProposed || ob.Phase != "" || ob.Rev == "" || ob.Check != "./parse" || len(ob.KeepPassing) != 1 || ob.KeepPassing[0] != "go test ./..." ||
		ob.Judge != "Parsing is simple to read." || ob.JudgeBar != "high" || ob.MaxRounds != 4 || ob.Pace != "spare" || ob.Repo != "me/app" || ob.Kind == "" {
		t.Fatalf("the proposal = %+v", ob)
	}
	rev := ob.Rev
	var got core.Objective
	if code := person.json("GET", path, nil, &got); code != 200 || got.Rev != rev || got.State != core.ObjectiveProposed || got.Judge != ob.Judge || got.KeepPassing[0] != "go test ./..." {
		t.Fatalf("reading it back: %d %+v", code, got)
	}

	// Confirming is a person's, of the proposal they read.
	for who, c := range map[string]*client{"an agent": agent, "a person's token in an agent's shell": shell, "a read-only token": readOnly} {
		if code, _ := c.do("POST", path+"/confirm", map[string]any{"rev": rev}); code != 403 {
			t.Errorf("%s confirming: %d", who, code)
		}
	}
	if code, _ := person.do("POST", path+"/confirm", map[string]any{"rev": "stale"}); code != 400 {
		t.Errorf("confirming another proposal than the one read: %d", code)
	}
	if code := person.json("POST", path+"/confirm", map[string]any{"rev": rev}, &ob); code != 200 {
		t.Fatalf("confirming: %d", code)
	}
	if ob.State != core.ObjectiveActive || ob.Phase != core.PhaseBaseline || ob.Health != "reading" || ob.ConfirmedBy == nil || ob.ConfirmedBy.Username != "robin" {
		t.Fatalf("confirmed = state %s phase %q health %q by %+v", ob.State, ob.Phase, ob.Health, ob.ConfirmedBy)
	}

	// Steering it: a person may, an agent may not.
	goPath := path + "/go"
	for who, c := range map[string]*client{"an agent": agent, "a person's token in an agent's shell": shell, "a read-only token": readOnly} {
		for _, act := range []string{core.ObjectiveRetry, core.ObjectiveEnd, core.ObjectiveNext} {
			if code, b := c.do("POST", goPath, map[string]any{"action": act}); code != 403 {
				t.Errorf("%s: %s: %d %s", who, act, code, b)
			}
		}
	}
	got = core.Objective{}
	if person.json("GET", path, nil, &got) != 200 || got.State != core.ObjectiveActive {
		t.Fatalf("an agent's refused steering changed it: %+v", got)
	}
	if code, _ := person.do("POST", goPath, map[string]any{"action": "explode"}); code != 400 {
		t.Errorf("an action that isn't one: %d", code)
	}
	if code, _ := person.do("POST", goPath, "not json"); code != 400 {
		t.Errorf("a body that isn't json: %d", code)
	}
	if code := person.json("POST", goPath, map[string]any{"action": core.ObjectiveRetry}, &ob); code != 200 || ob.State != core.ObjectiveActive || ob.Phase != core.PhaseBaseline {
		t.Fatalf("a person's retry: %d %s %q", code, ob.State, ob.Phase)
	}
	// As text, for the terminal.
	if code, b := person.do("POST", goPath+"?format=text", map[string]any{"action": core.ObjectiveRetry}); code != 200 || !strings.Contains(string(b), task.Key) {
		t.Logf("text answer: %d %s", code, b)
	}
	if code := person.json("POST", goPath, map[string]any{"action": core.ObjectiveEnd}, &ob); code != 200 || ob.State != core.ObjectiveEnded || !strings.Contains(ob.EndedWhy, "ended by") {
		t.Fatalf("a person's end: %d %s %q", code, ob.State, ob.EndedWhy)
	}
	if code := person.json("GET", path, nil, &got); code != 200 || got.State != core.ObjectiveEnded || got.Health != "ended" || got.Judge != ob.Judge || got.KeepPassing[0] != "go test ./..." {
		t.Fatalf("what is left of it: %d %+v", code, got)
	}
	// It is over: nothing is left to retry.
	if code, _ := person.do("POST", goPath, map[string]any{"action": core.ObjectiveRetry}); code != 400 {
		t.Errorf("retry of an ended objective: %d", code)
	}
	// Set again by a person, it starts on a new round and keeps what was written.
	if code := person.json("PUT", path, map[string]any{"until": "< 1ms"}, &ob); code != 200 || ob.State != core.ObjectiveActive || ob.Round != 2 || ob.Judge != "Parsing is simple to read." || ob.Pace != "spare" {
		t.Fatalf("set again: %d state %s round %d %+v", code, ob.State, ob.Round, ob)
	}
	// Steering what isn't there.
	if code, _ := person.do("POST", "/api/v1/issues/NOPE-1/objective/go", map[string]any{"action": core.ObjectiveRetry}); code != 404 {
		t.Errorf("steering a task that isn't there: %d", code)
	}
}

// The diagnose route (WP-1449): diagnose_after round-trips, a read-only
// token is refused, and a round that isn't a diagnose round is told so.
func TestObjectiveAPIDiagnose(t *testing.T) {
	ts, svc := setup(t)
	ctx := context.Background()
	u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "robin@example.com", Password: "correct-horse-battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	a := &core.Actor{User: u}
	kind, base := "forgejo", "https://git.example.com"
	f, err := svc.CreateForge(ctx, a, core.ForgeInput{Kind: &kind, BaseURL: &base})
	if err != nil {
		t.Fatal(err)
	}
	name := "App"
	p, err := svc.CreateProject(ctx, a, core.ProjectInput{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddProjectRepo(ctx, a, p.Key, f.ID, "me/app", false); err != nil {
		t.Fatal(err)
	}
	task, err := svc.CreateIssue(ctx, a, core.IssueInput{Project: p.Key, Title: "Fast parse", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/issues/" + task.Key + "/objective"
	cl := func(spec core.TokenSpec) *client {
		spec.Name = "t"
		tok, _, err := svc.CreateToken(ctx, a, spec)
		if err != nil {
			t.Fatal(err)
		}
		return &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	}
	person := cl(core.TokenSpec{})
	agent := cl(core.TokenSpec{Agent: "anthropic/claude-opus-5-5"})
	readOnly := cl(core.TokenSpec{ReadOnly: true})

	var ob core.Objective
	if code := person.json("PUT", path, map[string]any{"check": "./parse", "until": "< 1.5ms", "diagnose_after": 3}, &ob); code != 200 || ob.DiagnoseAfter != 3 {
		t.Fatalf("setting it: %d %+v", code, ob)
	}
	if code, _ := person.do("PUT", path, map[string]any{"diagnose_after": 99}); code != 400 {
		t.Errorf("diagnose_after 99: %d", code)
	}
	if code, _ := readOnly.do("POST", path+"/diagnose", map[string]any{"text": "x"}); code != 403 {
		t.Errorf("read-only token: %d", code)
	}
	if code, _ := person.do("POST", path+"/diagnose", "not json"); code != 400 {
		t.Errorf("a body that isn't json: %d", code)
	}
	for who, c := range map[string]*client{"a person": person, "an agent": agent} {
		code, b := c.do("POST", path+"/diagnose", map[string]any{"text": "the check is wrong"})
		if code != 400 || !strings.Contains(string(b), "diagnose") {
			t.Errorf("%s, not a diagnose round: %d %s", who, code, b)
		}
	}
}

// A diagnosis recorded over the API: a person's as JSON, an agent's (which
// has taken the round) as text; each opens the round again as a work round.
func TestObjectiveAPIDiagnoseRecords(t *testing.T) {
	ts, svc, d := setupServerDB(t, nil)
	ctx := context.Background()
	u, err := svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "robin@example.com", Password: "correct-horse-battery", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	a := &core.Actor{User: u}
	kind, base := "forgejo", "https://git.example.com"
	f, err := svc.CreateForge(ctx, a, core.ForgeInput{Kind: &kind, BaseURL: &base})
	if err != nil {
		t.Fatal(err)
	}
	name := "App"
	p, err := svc.CreateProject(ctx, a, core.ProjectInput{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddProjectRepo(ctx, a, p.Key, f.ID, "me/app", false); err != nil {
		t.Fatal(err)
	}
	task, err := svc.CreateIssue(ctx, a, core.IssueInput{Project: p.Key, Title: "Fast parse", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/issues/" + task.Key + "/objective"
	cl := func(spec core.TokenSpec) *client {
		spec.Name = "t"
		tok, _, err := svc.CreateToken(ctx, a, spec)
		if err != nil {
			t.Fatal(err)
		}
		return &client{t: t, base: ts.URL, http: http.DefaultClient, token: tok}
	}
	person := cl(core.TokenSpec{})
	agent := cl(core.TokenSpec{Agent: "anthropic/claude-opus-5-5"})
	var ob core.Objective
	if code := person.json("PUT", path, map[string]any{"check": "./parse", "until": "< 1.5ms"}, &ob); code != 200 {
		t.Fatalf("setting it: %d", code)
	}
	// The rounds before this came no nearer: the state the service reaches
	// over several readings.
	diagnoseRound := func() {
		t.Helper()
		svc.Wait() // what setting it set off has settled
		res, err := d.W.ExecContext(ctx, `UPDATE objectives SET phase = '', round_kind = 'diagnose', no_better = 2, base_sha = ? WHERE issue_id = ?`, strings.Repeat("a", 40), task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("forcing the diagnose state touched %d rows (issue %d)", n, task.ID)
		}
	}
	diagnoseRound()
	var got core.Objective
	if code, raw := person.do("POST", path+"/diagnose", map[string]any{"text": "The parser is fast; the check times the build."}); code != 200 {
		t.Fatalf("a person's diagnosis: %d %s", code, raw)
	} else if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Diagnosis == nil || got.Diagnosis.Text != "The parser is fast; the check times the build." || got.RoundKind != "" || got.NoBetter != 0 || got.Phase != core.PhaseWork {
		t.Fatalf("after a person's diagnosis: %+v", got)
	}

	diagnoseRound()
	if code, b := agent.do("POST", "/api/v1/agent/take/"+task.Key, map[string]any{}); code != 200 {
		t.Fatalf("take: %d %s", code, b)
	}
	code, b := agent.do("POST", path+"/diagnose?format=text", map[string]any{"text": "Noise: runs vary more than a round moves them."})
	if code != 200 || !strings.Contains(string(b), "Noise: runs vary") || !strings.Contains(string(b), "diagnosis") {
		t.Fatalf("an agent's diagnosis as text: %d %s", code, b)
	}
	if code := person.json("GET", path, nil, &got); code != 200 || got.Diagnosis == nil || !strings.HasPrefix(got.Diagnosis.Text, "Noise:") || got.Diagnosis.By != "anthropic/claude-opus-5-5" {
		t.Fatalf("the diagnosis kept: %d %+v", code, got.Diagnosis)
	}
}
