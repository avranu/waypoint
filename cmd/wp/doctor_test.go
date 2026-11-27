//go:build !windows

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Both spellings of a doctor are one tool, so a guess at either costs no
// error (WP-797), and a doctor added under any command joins by itself.
func TestDoctorBothWays(t *testing.T) {
	oldCLIs, oldClaude, oldSvc := detectCLIsFn, claudeInstalled, runnerServiceFn
	detectCLIsFn = func(context.Context) []runnerCLI { return nil }
	claudeInstalled = func() bool { return false }
	runnerServiceFn = func() (bool, bool, string) { return false, false, "" }
	t.Cleanup(func() { detectCLIsFn, claudeInstalled, runnerServiceFn = oldCLIs, oldClaude, oldSvc })
	e := newEnv(t)
	e.login()
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	linkCommands()

	found := 0
	for _, top := range commands {
		if top.sub("doctor") == nil {
			continue
		}
		found++
		t.Run(top.name, func(t *testing.T) {
			if doctorCmd.sub(top.name) == nil {
				t.Fatalf("no wp doctor %s", top.name)
			}
			if top.name == "sandbox" && runtime.GOOS != "linux" {
				t.Skip("the sandbox helper runs on Linux only")
			}
			a, codeA := e.run("doctor", top.name)
			b, codeB := e.run(top.name, "doctor")
			if a != b || codeA != codeB {
				t.Errorf("wp doctor %s (exit %d):\n%s\nwp %s doctor (exit %d):\n%s", top.name, codeA, a, top.name, codeB, b)
			}
		})
	}
	if found < 4 {
		t.Errorf("found only %d doctors", found)
	}

	first, codeFirst := e.run("doctor", "--json", "ci")
	last, codeLast := e.run("ci", "doctor", "--json")
	if first != last || codeFirst != codeLast {
		t.Errorf("wp doctor --json ci (exit %d):\n%s\nwp ci doctor --json (exit %d):\n%s", codeFirst, first, codeLast, last)
	}

	out, code := e.run("doctor", "bogus")
	if code != 2 || !strings.Contains(out, `no doctor tool "bogus"`) || !strings.Contains(out, "wp doctor ci") {
		t.Errorf("wp doctor bogus: exit %d\n%s", code, out)
	}
	if help := e.wp("doctor", "ci", "-h"); !strings.Contains(help, "The same as wp ci doctor") || !strings.Contains(help, "wp doctor ci --repo") {
		t.Errorf("wp doctor ci -h:\n%s", help)
	}
}

func TestSetupDoctor(t *testing.T) {
	oldCLIs, oldClaude := detectCLIsFn, claudeInstalled
	detectCLIsFn = func(context.Context) []runnerCLI {
		return []runnerCLI{{Name: "claude", SignedIn: boolp(true)}, {Name: "codex", SignedIn: boolp(false)}, {Name: "cursor", Detail: probeUnfinished}}
	}
	claudeInstalled = func() bool { return false }
	t.Cleanup(func() { detectCLIsFn, claudeInstalled = oldCLIs, oldClaude })
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())

	t.Run("signed in", func(t *testing.T) {
		e := newEnv(t)
		e.login()
		out := e.wp("doctor")
		for _, want := range []string{"sign-in", "Claude Code", "Codex is signed out: codex login", "Cursor: the sign-in check didn't finish", "not in a git repo", "none on this machine", "More: wp doctor ci"} {
			if !strings.Contains(out, want) {
				t.Errorf("no %q in\n%s", want, out)
			}
		}
		if strings.Contains(e.wp("doctor", "setup"), "More:") {
			t.Error("wp doctor setup names the other tools; only bare wp doctor should")
		}
		if js := e.wp("doctor", "--json"); strings.Contains(js, "More:") || !strings.Contains(js, `"state"`) {
			t.Errorf("--json:\n%s", js)
		}
	})

	t.Run("token open to others", func(t *testing.T) {
		e := newEnv(t)
		if err := os.WriteFile(filepath.Join(e.dir, "config.json"), []byte(`{"token":"wpt_x"}`), 0o644); err != nil { // #nosec G306 -- the case under test
			t.Fatal(err)
		}
		out, _ := e.run("doctor", "setup")
		if !strings.Contains(out, "others can read it: chmod 600") {
			t.Errorf("no permission warning in\n%s", out)
		}
	})

	t.Run("refused token", func(t *testing.T) {
		e := newEnv(t)
		t.Setenv("WAYPOINT_TOKEN", "wpt_not_a_token")
		out, code := e.run("doctor", "setup")
		if code != 1 || !strings.Contains(out, "refuses this token: wp auth login") {
			t.Errorf("exit %d\n%s", code, out)
		}
	})

	t.Run("server stalls", func(t *testing.T) {
		old := doctorCallTimeout
		doctorCallTimeout = 200 * time.Millisecond
		t.Cleanup(func() { doctorCallTimeout = old })
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" {
				_, _ = w.Write([]byte(`{"version":"v1.0.0"}`))
				return
			}
			<-r.Context().Done()
		}))
		t.Cleanup(srv.Close)
		t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
		t.Setenv("WAYPOINT_SERVER", srv.URL)
		t.Setenv("WAYPOINT_TOKEN", "wpt_x")
		start := time.Now()
		e := &env{t: t}
		out, code := e.run("doctor", "setup")
		if took := time.Since(start); took > 10*time.Second {
			t.Errorf("took %v", took)
		}
		if code != 0 || !strings.Contains(out, "warn  sign-in") {
			t.Errorf("exit %d\n%s", code, out)
		}
	})

	t.Run("no server", func(t *testing.T) {
		t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
		t.Setenv("WAYPOINT_SERVER", "")
		e := &env{t: t}
		out, code := e.run("config", "doctor")
		if code != 1 || !strings.Contains(out, "none set: wp setup") {
			t.Errorf("exit %d\n%s", code, out)
		}
	})
}

func TestCheckVersion(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	for _, tc := range []struct{ wp, server, state, want string }{
		{"v0.9.0", "v0.9.0", checkOK, "the same as the server"},
		{"v0.9.0", "v0.10.0", checkWarn, "the server runs v0.10.0: wp upgrade"},
		{"v0.10.0", "v0.9.0", checkInfo, "newer than the server's v0.9.0"},
		{"dev", "v0.9.0", checkInfo, "a development build"},
		{"v0.9.0", "", checkInfo, "doesn't say its version"},
	} {
		version = tc.wp
		var got []doctorCheck
		checkVersion(func(state, name, format string, a ...any) {
			got = append(got, doctorCheck{state, name, fmt.Sprintf(format, a...)})
		}, tc.server)
		if len(got) != 1 || got[0].State != tc.state || !strings.Contains(got[0].Detail, tc.want) {
			t.Errorf("wp %s, server %q: %+v", tc.wp, tc.server, got)
		}
	}
}
