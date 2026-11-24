package main

import (
	"context"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
)

// WAYPOINT_NO_AUTO_UPDATE is the one WAYPOINT_ variable a runner's children
// keep (WP-1773): every launcher builds its environment through jobEnvKeeps,
// so each builder is checked. "0" and "false" pass through unchanged: the
// child's autoUpdateOff reads them as off, as it does here.
func TestEveryChildEnvironmentCarriesTheUpdateOptOut(t *testing.T) {
	const key = "WAYPOINT_NO_AUTO_UPDATE"
	builders := map[string]func(base []string) []string{
		"jobEnv":         func(b []string) []string { return jobEnv(b, nil, false) },
		"jobEnv extra":   func(b []string) []string { return jobEnv(b, map[string]string{key: "0", "X": "1"}, false) },
		"autopilotEnv":   func(b []string) []string { return autopilotEnv(b, "s1", "agent", "http://x", "tok") },
		"bgSessionEnv":   bgSessionEnv,
		"autopilotStart": func([]string) []string { return autopilotStartEnv() },
	}
	for name, build := range builders {
		for _, tc := range []struct {
			val  string
			set  bool
			want string // the value the child sees, "" for none
		}{
			{"1", true, "1"},
			{"true", true, "true"},
			{"", false, ""},
			{"0", true, "0"},
			{"false", true, "false"},
		} {
			t.Run(name+"/"+tc.val, func(t *testing.T) {
				var base []string
				if tc.set {
					t.Setenv(key, tc.val)
					base = []string{key + "=" + tc.val}
				} else {
					t.Setenv(key, "")
					_ = os.Unsetenv(key) // t.Setenv restores it
				}
				// autopilotStartEnv reads the process environment.
				env := build(append(base, "PATH=/bin", "WAYPOINT_TOKEN=secret"))
				var got []string
				for _, kv := range env {
					if strings.HasPrefix(kv, key+"=") {
						got = append(got, kv)
					}
				}
				if name == "jobEnv extra" {
					// a job's own variables can't set or unset it
					if tc.set && !slices.Equal(got, []string{key + "=" + tc.val}) {
						t.Fatalf("got %v", got)
					}
					if !tc.set && len(got) != 0 {
						t.Fatalf("a job set it: %v", got)
					}
					return
				}
				if tc.want == "" {
					if len(got) != 0 {
						t.Fatalf("present when unset: %v", got)
					}
					return
				}
				if !slices.Equal(got, []string{key + "=" + tc.want}) {
					t.Fatalf("got %v, want %s=%s", got, key, tc.want)
				}
				if slices.Contains(env, "WAYPOINT_TOKEN=secret") && name != "autopilotEnv" {
					t.Fatalf("the runner's token leaked: %v", env)
				}
			})
		}
	}
}

// A runner that doesn't update puts the switch in its own environment, which
// is where every child's comes from.
func TestRunnerStartSetsTheUpdateOptOutForItsChildren(t *testing.T) {
	for name, tc := range map[string]struct {
		opts runnerOpts
		want bool
	}{
		"updating":         {runnerOpts{}, false},
		"--no-auto-update": {runnerOpts{NoAutoUpdate: true}, true},
		"--once":           {runnerOpts{Once: true}, true},
	} {
		t.Run(name, func(t *testing.T) {
			e := cliServer(t, "v1.1.0", "v1.1.0", false)
			e.runnerUser("robin")
			t.Setenv("WAYPOINT_NO_AUTO_UPDATE", "") // restored when the test ends
			c := &cli{dir: e.dir, out: io.Discard, err: io.Discard}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			tc.opts.Name, tc.opts.MaxJobs, tc.opts.PollWait, tc.opts.NoUsageSync = "box", 1, -1, true
			_ = runRunner(ctx, c, tc.opts)
			if got := autoUpdateOff(); got != tc.want {
				t.Fatalf("WAYPOINT_NO_AUTO_UPDATE off = %v, want %v", got, tc.want)
			}
		})
	}
}
