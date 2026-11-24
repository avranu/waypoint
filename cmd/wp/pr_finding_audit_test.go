package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A carried audit finding (12.1, as wp pr todo prints it and wp audit mark
// takes it) is dismissed and reopened through the call wp audit mark makes; a
// finding's own number still goes to its own route.
func TestPRDismissReopenAuditFinding(t *testing.T) {
	type call struct {
		path string
		body map[string]any
	}
	var calls []call
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		calls = append(calls, call{r.URL.Path, body})
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()
	t.Setenv("WAYPOINT_SERVER", ts.URL)
	t.Setenv("WAYPOINT_TOKEN", "wpt_x")
	run := func(name string, f func(*cli, []string) error, args ...string) (string, error) {
		var out bytes.Buffer
		c, _ := newCLI(&out, new(bytes.Buffer))
		c.cur = prSub(t, name)
		err := f(c, args)
		return out.String(), err
	}

	out, err := run("dismiss", cmdPRDismiss, "12.1", "#12.3", "-m", "the caller holds the lock", "--evidence", "a.go:12 takes mu")
	if err != nil || !strings.Contains(out, "12.1") {
		t.Fatalf("dismiss 12.1: %v\n%s", err, out)
	}
	if len(calls) != 2 || calls[0].path != "/api/v1/audits/findings" || calls[0].body["state"] != "rejected" ||
		calls[0].body["note"] != "the caller holds the lock" || calls[0].body["evidence"] != "a.go:12 takes mu" ||
		!strings.Contains(strings.Join(anyStrings(calls[0].body["ids"]), ","), "12.1") {
		t.Fatalf("dismiss calls: %+v", calls)
	}

	calls = nil
	if _, err := run("reopen", cmdPRReopen, "12.1"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].path != "/api/v1/audits/findings" || calls[0].body["state"] != "open" {
		t.Fatalf("reopen calls: %+v", calls)
	}

	calls = nil
	if _, err := run("dismiss", cmdPRDismiss, "280", "-m", "n"); err != nil || len(calls) != 1 || calls[0].path != "/api/v1/findings/280/dismiss" {
		t.Fatalf("integer id: %v %+v", err, calls)
	}
	// A bot's finding or a review comment takes no evidence.
	calls = nil
	if _, err := run("dismiss", cmdPRDismiss, "c14", "-m", "n", "--evidence", "a.go:1"); err == nil || !strings.Contains(err.Error(), "--evidence is for an audit finding") || len(calls) != 0 {
		t.Fatalf("review comment with evidence: %v %+v", err, calls)
	}
	if _, err := run("dismiss", cmdPRDismiss, "280", "-m", "n", "--evidence", "a.go:1"); err == nil || !strings.Contains(err.Error(), "--evidence is for an audit finding") || len(calls) != 0 {
		t.Fatalf("integer id with evidence: %v %+v", err, calls)
	}
	calls = nil
	if _, err := run("reopen", cmdPRReopen, "#280"); err != nil || len(calls) != 1 || calls[0].path != "/api/v1/findings/280/reopen" {
		t.Fatalf("integer id reopen: %v %+v", err, calls)
	}
	// Not a finding in either form.
	calls = nil
	if _, err := run("dismiss", cmdPRDismiss, "12.", "-m", "n"); err == nil || len(calls) != 0 {
		t.Errorf("12. accepted: %v %+v", err, calls)
	}
	if _, err := run("reopen", cmdPRReopen, "1.2.3"); err == nil || len(calls) != 0 {
		t.Errorf("1.2.3 accepted: %v %+v", err, calls)
	}
}

func anyStrings(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// The help names both forms.
func TestPRFindingHelpNamesBothForms(t *testing.T) {
	for _, name := range []string{"dismiss", "reopen"} {
		sub := prSub(t, name)
		if !strings.Contains(sub.summary, "AUDIT.N") {
			t.Errorf("wp pr %s help doesn't name AUDIT.N", name)
		}
	}
}

func prSub(t *testing.T, name string) *command {
	t.Helper()
	for _, s := range prCmd.subs {
		if s.name == name {
			return s
		}
	}
	t.Fatalf("no wp pr %s", name)
	return nil
}
