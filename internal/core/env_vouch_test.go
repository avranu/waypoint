package core

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// wp env vouch (WP-1087): an agent's call asks a person once per project
// and addresses; "Allow reads" records exactly the asked addresses as the
// answering person's, which puts them on the guard's list; a person's own
// call vouches at once.
func TestVouchEnvironment(t *testing.T) {
	x := newEnvT(t)
	res, err := x.s.db.W.Exec(`INSERT INTO forges(kind, name, base_url, created_at) VALUES ('forgejo','Forge','https://git.example.com', 0)`)
	if err != nil {
		t.Fatal(err)
	}
	fid, _ := res.LastInsertId()
	if _, err := x.s.db.W.Exec(`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?,?, 'acme/app', 0)`, x.p.ID, fid); err != nil {
		t.Fatal(err)
	}
	hosts := func() []string { return x.s.EnvGuardHosts(x.ctx, x.viewer, "acme/app") }
	ag := agentActor(x.editor, "anthropic/claude-code")

	if _, err := x.s.VouchEnvironment(x.ctx, ag, x.p.Key, "production", EnvVouchInput{URL: ptr("ftp://app.example.com")}); !IsInvalid(err) {
		t.Fatalf("ftp address: %v", err)
	}
	if _, err := x.s.VouchEnvironment(x.ctx, ag, x.p.Key, "production", EnvVouchInput{}); !IsInvalid(err) {
		t.Fatalf("no address for a new environment: %v", err)
	}
	in := EnvVouchInput{URL: ptr("https://app.example.com"), HealthURL: ptr("https://app.example.com/healthz"), Reason: "read the health row"}
	asked, err := x.s.VouchEnvironment(x.ctx, ag, x.p.Key, "Production", in)
	if err != nil {
		t.Fatal(err)
	}
	if asked.Vouched || asked.Request == nil || !asked.New || asked.Environment != nil {
		t.Fatalf("agent's vouch: %+v", asked)
	}
	r := asked.Request
	if r.Kind != RequestQuestion || r.EnvRead == nil || r.EnvRead.URL != "https://app.example.com" || r.Body != "read the health row" ||
		len(r.Questions) != 1 || len(r.Questions[0].Choices) != 2 || !strings.Contains(r.Title, "app.example.com:443") {
		t.Fatalf("request: %+v %+v", r, r.EnvRead)
	}
	if _, err := x.s.GetEnvironmentNamed(x.ctx, x.editor, x.p.Key, "production"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the ask created the environment: %v", err)
	}
	again, err := x.s.VouchEnvironment(x.ctx, agentActor(x.viewer, "openai/codex"), x.p.Key, "production", in)
	if err != nil || again.New || again.Request == nil || again.Request.ID != r.ID || again.Request.Body != "" || again.Request.Ref != r.Ref {
		t.Fatalf("repeat: %v %+v", err, again)
	}
	if _, err := x.s.VouchEnvironment(x.ctx, ag, x.p.Key, "prod/../x", in); !IsInvalid(err) {
		t.Fatalf("bad name: %v", err)
	}
	if _, err := x.s.VouchEnvironment(x.ctx, ag, x.p.Key, "production", EnvVouchInput{URL: in.URL, VersionURL: ptr("https://app.example.com/version")}); !IsInvalid(err) {
		t.Fatalf("version address without a field: %v", err)
	}
	q, _, err := x.s.CreateRequest(x.ctx, ag, RequestInput{Kind: RequestQuestion, Title: "x", Project: x.p.Key,
		Questions: []Question{{Question: "allow?", Choices: []Choice{{Label: EnvReadAllow}, {Label: "no"}}}}})
	if err != nil || q.EnvRead != nil {
		t.Fatalf("plain question: %v %+v", err, q)
	}

	if _, err := x.s.DecideRequest(x.ctx, x.viewer, r.Ref, RequestDecision{Decision: "answer", Answers: []Answer{{Choices: []string{EnvReadAllow}}}}); err == nil {
		t.Fatal("a viewer answered it")
	}
	if _, err := x.s.DecideRequest(x.ctx, x.editor, r.Ref, RequestDecision{Decision: "answer", Text: EnvReadAllow}); !IsInvalid(err) {
		t.Fatalf("typed answer: %v", err)
	}
	if _, err := x.s.GetEnvironmentNamed(x.ctx, x.editor, x.p.Key, "production"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a typed answer vouched: %v", err)
	}
	if _, err := x.s.DecideRequest(x.ctx, x.editor, r.Ref, RequestDecision{Decision: "answer", Answers: []Answer{{Choices: []string{EnvReadAllow}}}}); err != nil {
		t.Fatal(err)
	}
	d, err := x.s.GetEnvironmentNamed(x.ctx, x.editor, x.p.Key, "production")
	if err != nil || d.AddressAgent != "" || d.URL != "https://app.example.com" || d.HealthURL != "https://app.example.com/healthz" {
		t.Fatalf("after allow: %v %+v", err, d)
	}
	if got := hosts(); !slices.Equal(got, []string{"app.example.com:443"}) {
		t.Fatalf("hosts %v", got)
	}
	if v, err := x.s.VouchEnvironment(x.ctx, ag, x.p.Key, "production", EnvVouchInput{}); err != nil || !v.Vouched || v.Request != nil {
		t.Fatalf("already vouched: %v %+v", err, v)
	}

	st, err := x.s.VouchEnvironment(x.ctx, ag, x.p.Key, "staging", EnvVouchInput{URL: ptr("https://staging.example.com")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.DecideRequest(x.ctx, x.editor, st.Request.Ref, RequestDecision{Decision: "answer", Answers: []Answer{{Choices: []string{envReadDeny}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.GetEnvironmentNamed(x.ctx, x.editor, x.p.Key, "staging"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("denied ask created the environment: %v", err)
	}

	qa := "qa"
	if _, err := x.s.CreateEnvironment(x.ctx, ag, x.p.Key, EnvironmentInput{Name: &qa, URL: ptr("https://qa.example.com"), HealthURL: ptr("https://qa.example.com/h")}); err != nil {
		t.Fatal(err)
	}
	v, err := x.s.VouchEnvironment(x.ctx, x.editor, x.p.Key, "qa", EnvVouchInput{})
	if err != nil || !v.Vouched || v.Request != nil || v.Environment.HealthURL != "https://qa.example.com/h" {
		t.Fatalf("person's vouch: %v %+v", err, v)
	}
	if !slices.Contains(hosts(), "qa.example.com:443") {
		t.Fatalf("hosts after person's vouch: %v", hosts())
	}
	stopped, err := x.s.StopEnvironment(x.ctx, x.editor, v.Environment.ID)
	if err != nil || !stopped.Stopped {
		t.Fatalf("stop: %v", err)
	}
	if _, err := x.s.VouchEnvironment(x.ctx, ag, x.p.Key, "qa", EnvVouchInput{}); !IsInvalid(err) {
		t.Fatalf("stopped: %v", err)
	}
	if _, err := x.s.VouchEnvironment(x.ctx, x.viewer, x.p.Key, "dev", EnvVouchInput{URL: ptr("https://dev.example.com")}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer's vouch: %v", err)
	}
	if _, err := x.s.VouchEnvironment(x.ctx, x.nobody, x.p.Key, "dev", EnvVouchInput{URL: ptr("https://dev.example.com")}); err == nil {
		t.Fatal("someone outside the project")
	}
}
