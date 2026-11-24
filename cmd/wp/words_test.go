package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Messages must not hardcode a number that a constant sets.

// The refusal names the limits wp audit really has, from the constants, not
// one number that the default (gateway) audit does not have.
func TestGrokRefusalNamesTheAuditLimits(t *testing.T) {
	out := grokCutShort(`timeout 600 cursor-agent -p --mode ask --model grok-4.7-high "$(cat p.md)"`, true, 0)
	for _, want := range []string{
		fmt.Sprintf("%d minutes for a model run by a CLI", core.AuditCLITimeoutSecs/60),
		fmt.Sprintf("%d for the gateway's agent", core.AuditGatewayTimeoutSecs/60),
		fmt.Sprintf("timeout %d", core.AuditCLITimeoutSecs),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "45-minute limit") {
		t.Errorf("the refusal still gives one limit for every audit:\n%s", out)
	}
}

// A blocking audit is told when it ends, or within five minutes while another
// of the session's pull requests is still being watched: not "straight away".
func TestAuditAskedBlockingSaysTheFiveMinutes(t *testing.T) {
	a := &auditView{ID: 4, Models: []string{"gateway/audit"}}
	got := a.asked(true)
	if strings.Contains(got, "straight away") || strings.Contains(got, "within minutes") || !strings.Contains(got, "within five minutes if one of your pull requests is still being watched") {
		t.Errorf("blocking: %s", got)
	}
	e := newEnv(t)
	if help := strings.Join(strings.Fields(e.wp("audit", "pr", "-h")), " "); strings.Contains(help, "straight away") || !strings.Contains(help, "within five minutes if one of my pull requests is still being watched") {
		t.Errorf("the --blocking help:\n%s", help)
	}
}

// wp ship's help names the cases where a ship is not armed.
func TestShipHelpNamesTheNotArmedCases(t *testing.T) {
	for _, want := range []string{"Merge automatically is off", "other base branches", "outside the repo", "another task is pushed to but not armed"} {
		if !strings.Contains(strings.Join(strings.Fields(shipCmd.about), " "), want) {
			t.Errorf("the ship help lacks %q", want)
		}
	}
}

// noting a start is tried again within a bound; when it still fails the
// caller is told and the session starts all the same.
func TestNoteTypedStartRetriesThenWarns(t *testing.T) {
	old := typedStartBackoff
	typedStartBackoff = []time.Duration{0, 0, 0}
	t.Cleanup(func() { typedStartBackoff = old })
	for name, tc := range map[string]struct {
		failures  int
		wantCalls int
		wantErr   bool
	}{
		"two 503s then success":  {2, 3, false},
		"always 503":             {100, 3, true},
		"refused is not retried": {-1, 1, true},
	} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls++
				n := calls
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				switch {
				case tc.failures < 0:
					w.WriteHeader(http.StatusForbidden)
				case n <= tc.failures:
					w.WriteHeader(http.StatusServiceUnavailable)
				}
				_, _ = w.Write([]byte(`{"error":"no"}`))
			}))
			t.Cleanup(srv.Close)
			t.Setenv("WAYPOINT_SERVER", srv.URL)
			t.Setenv("WAYPOINT_TOKEN", "t")
			c, _ := newCLI(&bytes.Buffer{}, &bytes.Buffer{})
			err := c.noteTypedStart(context.Background(), "the kickoff")
			if (err != nil) != tc.wantErr || calls != tc.wantCalls {
				t.Fatalf("err = %v, calls = %d; want error %v, %d calls", err, calls, tc.wantErr, tc.wantCalls)
			}
			var w bytes.Buffer
			warnTypedStart(&w, err)
			if tc.wantErr != strings.Contains(w.String(), "first prompt will read as a person's") {
				t.Fatalf("warning = %q for err %v", w.String(), err)
			}
		})
	}
}
