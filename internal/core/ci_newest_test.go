package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// WP-116: per context the newest status wins, whatever order the forge lists them in.
func TestNewestPerContext(t *testing.T) {
	errFirst := []CommitCheck{
		{Name: "forge-checks/codeql", State: CheckFailed, ID: 1, Created: "2026-10-23T18:29:35Z"},
		{Name: "forge-checks/codeql", State: CheckPending, ID: 2, Created: "2026-10-23T18:30:00Z"},
		{Name: "forge-checks/codeql", State: CheckSuccess, ID: 3, Created: "2026-10-23T18:32:18Z"},
	}
	for name, list := range map[string][]CommitCheck{
		"oldest first": errFirst,
		"newest first": {errFirst[2], errFirst[1], errFirst[0]},
		"no ids, by created_at": {
			{Name: "forge-checks/codeql", State: CheckSuccess, Created: "2026-10-23T18:32:18Z"},
			{Name: "forge-checks/codeql", State: CheckFailed, Created: "2026-10-23T18:29:35Z"},
		},
	} {
		got := newestPerContext(list)
		if len(got) != 1 || got[0].State != CheckSuccess {
			t.Fatalf("%s: got %+v, want one success", name, got)
		}
		if st := ciState(got, 0, 1<<40); st != CIPassed {
			t.Fatalf("%s: ci state %q, want passed", name, st)
		}
	}
	later := []CommitCheck{
		{Name: "a", State: CheckSuccess, ID: 5},
		{Name: "b", State: CheckSuccess, ID: 6},
		{Name: "a", State: CheckFailed, ID: 9},
	}
	got := newestPerContext(later)
	if len(got) != 2 || got[0].Name != "a" || got[0].State != CheckFailed || got[1].State != CheckSuccess {
		t.Fatalf("success then error: got %+v", got)
	}
	if st := ciState(got, 0, 1<<40); st != CIFailed {
		t.Fatalf("ci state %q, want failed", st)
	}
}

func TestCommitChecksKeepsNewestPerContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"statuses": []map[string]any{
			{"id": 3, "context": "forge-checks/codeql", "status": "success", "created_at": "2026-10-23T18:32:18Z"},
			{"id": 1, "context": "forge-checks/codeql", "status": "error", "created_at": "2026-10-23T18:29:35Z"},
		}})
	}))
	defer srv.Close()
	cl := &httpForgeClient{http: srv.Client()}
	got, err := cl.CommitChecks(context.Background(), &Forge{Kind: "forgejo", BaseURL: srv.URL, token: "t"}, "o/r", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].State != CheckSuccess {
		t.Fatalf("got %+v", got)
	}
}

// WP-165: the gate job's status went pending -> skipped. Forgejo lists both,
// newest first; the skipped one wins and counts as passed.
func TestCommitChecksSkippedGateOverPending(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"statuses": []map[string]any{
			{"id": 4, "context": "validate / gate (pull_request)", "status": "skipped", "created_at": "2026-10-25T04:47:00Z"},
			{"id": 3, "context": "validate / validate (pull_request)", "status": "success", "created_at": "2026-10-25T04:46:00Z"},
			{"id": 2, "context": "validate / validate (pull_request)", "status": "pending", "created_at": "2026-10-24T23:10:00Z"},
			{"id": 1, "context": "validate / gate (pull_request)", "status": "pending", "created_at": "2026-10-24T23:10:00Z"},
		}})
	}))
	defer srv.Close()
	cl := &httpForgeClient{http: srv.Client()}
	got, err := cl.CommitChecks(context.Background(), &Forge{Kind: "forgejo", BaseURL: srv.URL, token: "t"}, "o/r", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].State != CheckSuccess || got[1].State != CheckSuccess {
		t.Fatalf("got %+v, want two successes", got)
	}
	if st := ciState(got, 0, 1<<40); st != CIPassed {
		t.Fatalf("ci state %q, want passed", st)
	}
}
