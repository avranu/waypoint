package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// wp env read (WP-1087): a GET to a vouched environment's own address,
// credentials by name and blanked out of the output, redirects on the host
// only; wp env vouch from an agent's token asks a person.
func TestCLIEnvReadVouch(t *testing.T) {
	const secret = "s3cret-admin-key"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "read only", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/admin/health":
			if r.Header.Get("Authorization") != "Bearer "+secret || r.Header.Get("X-Tenant") != "acme" {
				http.Error(w, "no key", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"empty":0.01,"echo":"` + secret + `","q":"` + r.URL.RawQuery + `"}`))
		case "/here":
			http.Redirect(w, r, "/admin/health", http.StatusFound)
		case "/away":
			http.Redirect(w, r, "http://elsewhere.example.test/steal", http.StatusFound)
		case "/healthz":
			_, _ = w.Write([]byte("ok"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	e := newEnv(t)
	me := e.login()
	ctx := context.Background()
	name := "Harbor"
	p, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	person := &core.Actor{User: me.User}
	prod, url, health := "production", srv.URL, srv.URL+"/healthz"
	if _, err := e.svc.CreateEnvironment(ctx, person, p.Key, core.EnvironmentInput{Name: &prod, URL: &url, HealthURL: &health}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARBOR_ADMIN_KEY", secret)
	for _, k := range []string{"CLAUDECODE", "GEMINI_CLI", "OPENCODE", "CODEX_SANDBOX", "CODEX_SANDBOX_NETWORK_DISABLED", "WAYPOINT_AGENT"} {
		t.Setenv(k, "") // a person's shell, whatever runs the test
	}

	out := e.wp("env", "read", "production", "/admin/health?limit=5", "--bearer", "HARBOR_ADMIN_KEY", "-H", "X-Tenant: acme", "-p", p.Key)
	if !strings.Contains(out, `"empty":0.01`) || !strings.Contains(out, `"q":"limit=5"`) || strings.Contains(out, secret) || !strings.Contains(out, "[redacted]") {
		t.Errorf("read: %s", out)
	}
	file := filepath.Join(t.TempDir(), "health.json")
	e.wp("env", "read", "production", "/admin/health", "--bearer", "HARBOR_ADMIN_KEY", "-H", "X-Tenant: acme", "-o", file, "-p", p.Key)
	if b, err := os.ReadFile(file); err != nil || !strings.Contains(string(b), `"empty":0.01`) || strings.Contains(string(b), secret) {
		t.Errorf("-o: %v %s", err, b)
	}
	if out := e.wp("env", "read", "production", "health", "-p", p.Key); strings.TrimSpace(out) != "ok" {
		t.Errorf("health: %s", out)
	}
	if out := e.wp("env", "read", "production", "/here", "--bearer", "HARBOR_ADMIN_KEY", "-H", "X-Tenant: acme", "-p", p.Key); !strings.Contains(out, `"empty"`) {
		t.Errorf("redirect on the host: %s", out)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"/away", "--bearer", "HARBOR_ADMIN_KEY"}, "not following a redirect"},
		{[]string{"/nope"}, "HTTP 404"},
		{[]string{"/admin/health"}, "HTTP 401"},
		{[]string{"http://elsewhere.example.test/x"}, "isn't one of production's addresses"},
		{[]string{"/x", "--bearer", secret}, "isn't a variable name"},
		{[]string{"/x", "--bearer", "HARBOR_UNSET_KEY"}, "isn't set"},
		{[]string{"/x", "-H", "X-HTTP-Method-Override: DELETE"}, "only reads"},
		{[]string{"/x", "-H", "X-HTTP-Method-Override=HARBOR_ADMIN_KEY"}, "only reads"},
		{[]string{"/x", "-H", "X-Original-Method: DELETE"}, "only reads"},
		{[]string{"version"}, "no version address"},
		{[]string{"admin"}, "start it with /"},
	} {
		out, code := e.run(append(append([]string{"env", "read", "production"}, c.args...), "-p", p.Key)...)
		if code == 0 || !strings.Contains(out, c.want) || strings.Contains(out, secret) {
			t.Errorf("%v: exit %d, want %q: %s", c.args, code, c.want, out)
		}
	}

	t.Setenv("CLAUDECODE", "1")
	if out, code := e.run("env", "read", "production", "health", "-p", p.Key); code == 0 || !strings.Contains(out, "a person runs it with -p "+p.Key) {
		t.Errorf("agent outside the repo's projects: exit %d: %s", code, out)
	}
	t.Setenv("CLAUDECODE", "")

	staging, surl := "staging", srv.URL+"/s"
	agent := &core.Actor{User: me.User, Agent: "anthropic/claude-opus-5-5"}
	if _, err := e.svc.CreateEnvironment(ctx, agent, p.Key, core.EnvironmentInput{Name: &staging, URL: &surl}); err != nil {
		t.Fatal(err)
	}
	if out, code := e.run("env", "read", "staging", "/healthz", "-p", p.Key); code == 0 || !strings.Contains(out, "wp env vouch staging") {
		t.Errorf("agent's environment: %s", out)
	}
	if out := e.wp("env", "show", "staging", "-p", p.Key); !strings.Contains(out, "`wp env vouch staging`") {
		t.Errorf("show: %s", out)
	}
	out = e.wp("env", "vouch", "staging", "-d", "check the queue", "-p", p.Key)
	if !strings.Contains(out, "Asked a person: "+p.Key+":R-") || !strings.Contains(out, "wp env read staging") {
		t.Errorf("vouch: %s", out)
	}
	if out := e.wp("env", "vouch", "staging", "-p", p.Key); !strings.Contains(out, "Already asked") {
		t.Errorf("vouch again: %s", out)
	}
	if out := e.wp("env", "vouch", "production", "-p", p.Key); !strings.Contains(out, "agents may read from") {
		t.Errorf("vouched already: %s", out)
	}
}

func TestDotenvValue(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.env"), filepath.Join(dir, "b.env")
	if err := os.WriteFile(a, []byte("# keys\nexport ADMIN_KEY=\"one two\"\nOTHER=x # note\nEMPTY=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("EMPTY=filled\nADMIN_KEY=later\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := []string{filepath.Join(dir, "missing"), a, b}
	for k, want := range map[string]string{"ADMIN_KEY": "one two", "OTHER": "x", "EMPTY": "filled", "NONE": ""} {
		if got := dotenvValue(files, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}
