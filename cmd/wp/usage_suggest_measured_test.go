package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// wp usage suggest reads the sizes the hooks measured at session start, says
// how many sessions they come from, and says so when there are none (WP-1470).
func TestSuggestUsesMeasuredSessionStartSizes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/sessions/current" && r.URL.Query().Get("cli_id") == "a":
			_, _ = w.Write([]byte(`{"id":11}`))
		case r.URL.Path == "/api/v1/sessions/current" && r.URL.Query().Get("cli_id") == "b":
			_, _ = w.Write([]byte(`{"id":12}`))
		case r.URL.Path == "/api/v1/sessions/11/snapshots":
			_, _ = w.Write([]byte(`[{"memory_files":3,"memory_bytes":8000,"memory_index_bytes":2000,"inventory":{"instruction_files":[{"path":"/r/AGENTS.md","scope":"ancestor","bytes":12000},{"path":"/h/CLAUDE.md","scope":"user","bytes":4000}]}}]`))
		default: // session b has no snapshot; c is unknown to the server
			if strings.HasSuffix(r.URL.Path, "/snapshots") {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	dropEnv(t)
	t.Setenv("WAYPOINT_SERVER", srv.URL)

	m, why := measuredStart(freshCLI(), []string{"a", "b", "c"})
	if m == nil {
		t.Fatalf("no measurement: %q", why)
	}
	if m.Sessions != 1 || m.Of != 3 || m.InstructionBytes != 16000 || m.InstructionFiles != 2 || m.MemoryIndexBytes != 2000 || m.MemoryBytes != 8000 || m.MemoryFiles != 3 {
		t.Errorf("measured = %+v", m)
	}
	text := suggestText(&sugReport{Repo: "r", Sessions: 3, StartMedian: 40000, DocTokens: 1000, Measured: m, Suggestions: []sugAdvice{}})
	for _, want := range []string{"median of 1 of 3", "instruction files 4k", "memory index 500"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "are about") {
		t.Errorf("the on-disk estimate is still presented as measured:\n%s", text)
	}

	// no snapshot for any of them: it says so, and calls the disk figure an estimate
	none, why := measuredStart(freshCLI(), []string{"b", "c"})
	if none != nil || !strings.Contains(why, "no session-start snapshot") {
		t.Errorf("measured = %+v, why %q", none, why)
	}
	text = suggestText(&sugReport{Repo: "r", Sessions: 2, StartMedian: 40000, DocTokens: 1000, MeasuredNote: why, Suggestions: []sugAdvice{}})
	if !strings.Contains(text, "no measurement from the sessions themselves") || !strings.Contains(text, "an estimate") {
		t.Errorf("text:\n%s", text)
	}
}
