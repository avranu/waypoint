package core

import (
	"slices"
	"testing"
)

// The guard reads only from addresses a person set (WP-749): an agent's
// address, or an agent's change to a person's, takes the environment off
// the list until a person saves it; a SessionStart in the repo carries the
// list, and one elsewhere carries none.
func TestEnvGuardHosts(t *testing.T) {
	x := newEnvT(t)
	res, err := x.s.db.W.Exec(`INSERT INTO forges(kind, name, base_url, created_at) VALUES ('forgejo','Forge','https://git.example.com', 0)`)
	if err != nil {
		t.Fatal(err)
	}
	fid, _ := res.LastInsertId()
	if _, err := x.s.db.W.Exec(`INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?,?, 'acme/app', 0)`, x.p.ID, fid); err != nil {
		t.Fatal(err)
	}
	hosts := func() []string { return x.s.EnvGuardHosts(x.ctx, x.viewer, "ACME/app") }

	prod := x.create("production", "https://App.example.com", func(in *EnvironmentInput) {
		in.HealthURL, in.VersionURL = ptr("https://app.example.com/healthz"), ptr("https://status.example.com/version")
		in.VersionField = ptr("version")
	})
	if prod.AddressAgent != "" {
		t.Fatalf("a person's environment: address_agent %q", prod.AddressAgent)
	}
	if got := hosts(); !slices.Equal(got, []string{"app.example.com:443", "status.example.com:443"}) {
		t.Fatalf("hosts %v", got)
	}
	if got := x.s.EnvGuardHosts(x.ctx, x.nobody, "acme/app"); len(got) != 0 {
		t.Errorf("someone outside the project: %v", got)
	}
	if got := x.s.EnvGuardHosts(x.ctx, x.viewer, "acme/other"); got == nil || len(got) != 0 {
		t.Errorf("another repo: %#v", got)
	}

	ag := agentActor(x.editor, "anthropic/claude-code")
	name := "staging"
	st, err := x.s.CreateEnvironment(x.ctx, ag, x.p.Key, EnvironmentInput{Name: &name, URL: ptr("https://evil.example")})
	if err != nil {
		t.Fatal(err)
	}
	if st.AddressAgent != "anthropic/claude-code" || slices.Contains(hosts(), "evil.example:443") {
		t.Fatalf("agent's environment: %q, hosts %v", st.AddressAgent, hosts())
	}

	same := "https://App.example.com"
	if d, err := x.s.UpdateEnvironment(x.ctx, ag, prod.ID, EnvironmentInput{URL: &same}); err != nil || d.AddressAgent != "" {
		t.Fatalf("agent's unchanged save: %v %+v", err, d)
	}
	moved := "https://evil.example"
	if d, err := x.s.UpdateEnvironment(x.ctx, ag, prod.ID, EnvironmentInput{URL: &moved}); err != nil || d.AddressAgent != "anthropic/claude-code" {
		t.Fatalf("agent's change: %v %+v", err, d)
	}
	if got := hosts(); len(got) != 0 {
		t.Fatalf("after an agent's change: %v", got)
	}
	if d, err := x.s.UpdateEnvironment(x.ctx, x.editor, st.ID, EnvironmentInput{URL: ptr("https://staging.example.com")}); err != nil || d.AddressAgent != "" {
		t.Fatalf("person's save: %v %+v", err, d)
	}
	if got := hosts(); !slices.Equal(got, []string{"staging.example.com:443"}) {
		t.Fatalf("after a person's save: %v", got)
	}
	qa := "qa"
	q, err := x.s.CreateEnvironment(x.ctx, ag, x.p.Key, EnvironmentInput{Name: &qa, URL: ptr("https://qa.example.com"), HealthURL: ptr("https://evil.example/h")})
	if err != nil {
		t.Fatal(err)
	}
	if d, err := x.s.UpdateEnvironment(x.ctx, x.editor, q.ID, EnvironmentInput{URL: ptr("https://qa2.example.com")}); err != nil || d.AddressAgent != "anthropic/claude-code" {
		t.Fatalf("person's save of one address: %v %+v", err, d)
	}
	if _, err := x.s.UpEnvironment(x.ctx, x.editor, x.p.Key, EnvUpInput{Name: "qa", URL: "https://qa3.example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := hosts(); slices.Contains(got, "evil.example:443") || slices.Contains(got, "qa3.example.com:443") {
		t.Fatalf("after a person's up of one address: %v", got)
	}
	if d, err := x.s.UpdateEnvironment(x.ctx, x.editor, q.ID, EnvironmentInput{URL: ptr("https://qa.example.com"), HealthURL: ptr("")}); err != nil || d.AddressAgent != "" {
		t.Fatalf("person's save of every address: %v %+v", err, d)
	}
	if _, err := x.s.StopEnvironment(x.ctx, x.editor, q.ID); err != nil {
		t.Fatal(err)
	}

	dev, err := x.s.UpEnvironment(x.ctx, x.editor, x.p.Key, EnvUpInput{Name: "a23x42s", URL: "https://a23x42s.example.com"})
	if err != nil || dev.AddressAgent != "" {
		t.Fatalf("person's up: %v %+v", err, dev)
	}
	shell := *x.editor
	shell.AgentEnv = "claude"
	if dev, err = x.s.UpEnvironment(x.ctx, &shell, x.p.Key, EnvUpInput{Name: "a23x42s", URL: "https://x.evil.example"}); err != nil || dev.AddressAgent != "agent-shell" {
		t.Fatalf("agent shell's up: %v %+v", err, dev)
	}
	if _, err := x.s.StopEnvironment(x.ctx, x.editor, st.ID); err != nil {
		t.Fatal(err)
	}
	if got := hosts(); len(got) != 0 {
		t.Fatalf("after stopping: %v", got)
	}

	if _, err := x.s.UpdateEnvironment(x.ctx, x.editor, prod.ID, EnvironmentInput{URL: ptr("https://app.example.com"),
		HealthURL: ptr("https://app.example.com/healthz"), VersionURL: ptr("https://status.example.com/version")}); err != nil {
		t.Fatal(err)
	}
	r, err := x.s.SessionHook(x.ctx, ag, []HookEvent{hook("box", "cli-1", "SessionStart", func(h *HookEvent) { h.Repo = "acme/app" })})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.EnvHosts, []string{"app.example.com:443", "status.example.com:443"}) {
		t.Errorf("SessionStart env_hosts %v", r.EnvHosts)
	}
	r, err = x.s.SessionHook(x.ctx, ag, []HookEvent{hook("box", "cli-1", "UserPromptSubmit", func(h *HookEvent) { h.Repo = "acme/app" })})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.EnvHosts, []string{"app.example.com:443", "status.example.com:443"}) {
		t.Errorf("UserPromptSubmit env_hosts %v", r.EnvHosts)
	}
	r, err = x.s.SessionHook(x.ctx, ag, []HookEvent{hook("box", "cli-1", "Stop", func(h *HookEvent) { h.Repo = "acme/app" })})
	if err != nil {
		t.Fatal(err)
	}
	if r.EnvHosts != nil {
		t.Errorf("Stop env_hosts %v", r.EnvHosts)
	}
}
