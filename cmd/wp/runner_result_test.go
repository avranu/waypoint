package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// bigResult is an audit's result with findings and a helper run holding cut
// leads and evidence of the sizes that made live results pass the limit.
func bigResult() outcome {
	var cut, leads []panelLead
	for i := range 40 {
		cut = append(cut, panelLead{ID: fmt.Sprintf("goals.%d", i), Title: "cut", Why: "refuted by a second read", Severity: "high", Path: "a.go", Line: 3,
			Problem: strings.Repeat("p", 4000), Evidence: strings.Repeat("e", 4000), ReadProblem: strings.Repeat("r", 1000)})
	}
	for i := range 5 {
		leads = append(leads, panelLead{ID: fmt.Sprintf("tests.%d", i), Title: "kept", Severity: "high", Problem: "kept problem", Confirmed: true, Evidence: strings.Repeat("v", 4000)})
	}
	return outcome{State: "done", Result: map[string]any{
		"answer":   "ok",
		"findings": []map[string]any{{"title": "F1", "problem": strings.Repeat("f", 3000)}},
		"helpers":  []helperRun{{Check: "goals", State: "done", Cut: cut, Leads: leads, Error: "ran long"}},
	}}
}

// A result over the limit is shrunk in steps, each said in the job's output,
// and never the findings: the cut leads keep their id, title and why; then
// they are dropped and counted; then the kept leads' evidence is clipped.
func TestFitResultShrinksInSteps(t *testing.T) {
	o := bigResult()
	full := resultSize(o)
	findings := fmt.Sprint(o.Result.(map[string]any)["findings"])

	// Under the limit: untouched, nothing said.
	var log bytes.Buffer
	if got := fitResult(o, full+1, &log); resultSize(got) != full || log.Len() != 0 {
		t.Fatalf("a result that fits was changed: %d -> %d, %q", full, resultSize(got), log.String())
	}

	// Just under the size after step 1: only step 1 runs.
	step1 := fitResult(o, full-1, &log)
	_, runs := helperRunsOf(step1)
	if len(runs[0].Cut) != 40 || runs[0].Cut[0].Problem != "" || runs[0].Cut[0].Evidence != "" || runs[0].Cut[0].ID != "goals.0" || runs[0].Cut[0].Title != "cut" || runs[0].Cut[0].Why == "" ||
		runs[0].Dropped != 0 || !strings.Contains(log.String(), "step 1") || strings.Contains(log.String(), "step 2") {
		t.Fatalf("step 1: %+v\n%s", runs[0].Cut[0], log.String())
	}
	// The original is as it was: the steps work on a copy.
	if _, orig := helperRunsOf(o); orig[0].Cut[0].Problem == "" || resultSize(o) != full {
		t.Fatal("the steps changed the runs they were given")
	}

	// Under the size after step 1: the cut leads are dropped and counted.
	log.Reset()
	s1 := resultSize(step1)
	step2 := fitResult(o, s1-1, &log)
	_, runs = helperRunsOf(step2)
	if len(runs[0].Cut) != 0 || runs[0].Dropped != 40 || !strings.Contains(runs[0].Error, "ran long") || !strings.Contains(runs[0].Error, "40 cut leads were not kept: the result was too large") ||
		!strings.Contains(log.String(), "step 2") || strings.Contains(log.String(), "step 3") || len(runs[0].Leads[0].Evidence) != 4000 {
		t.Fatalf("step 2: cut %d dropped %d error %q\n%s", len(runs[0].Cut), runs[0].Dropped, runs[0].Error, log.String())
	}

	// Under that: the kept leads' evidence is clipped; findings untouched throughout.
	log.Reset()
	step3 := fitResult(o, resultSize(step2)-1, &log)
	_, runs = helperRunsOf(step3)
	if !strings.Contains(log.String(), "step 3") || len([]rune(runs[0].Leads[0].Evidence)) > resultEvidenceMax+1 || runs[0].Leads[0].Problem != "kept problem" || !runs[0].Leads[0].Confirmed {
		t.Fatalf("step 3: %d chars\n%s", len(runs[0].Leads[0].Evidence), log.String())
	}
	for name, got := range map[string]outcome{"1": step1, "2": step2, "3": step3} {
		if f := fmt.Sprint(got.Result.(map[string]any)["findings"]); f != findings {
			t.Errorf("step %s changed the findings: %s", name, f)
		}
	}

	// Still over after every step: it is returned, and says so.
	log.Reset()
	if got := fitResult(o, 10, &log); resultSize(got) != resultSize(step3) || !strings.Contains(log.String(), "still") {
		t.Errorf("after every step: %d, %q", resultSize(got), log.String())
	}
}

// The server says the result is too large (its measure differs): the runner
// sends it once more with the helper runs reduced to their kept leads,
// instead of treating the 400 as fatal and losing the round's work.
func TestFinishRetriesTooLargeOnce(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		bodies = append(bodies, in)
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n <= 2 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"` + core.RunnerResultTooLarge + `: 3000000 bytes"}`))
			return
		}
		_, _ = w.Write([]byte(`{"state":"done"}`))
	}))
	defer srv.Close()
	t.Setenv("WAYPOINT_SERVER", srv.URL)
	t.Setenv("WAYPOINT_TOKEN", "wp_test")
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	p := newTestProc(t)
	p.reportCtx = context.Background()
	p.finish(7, bigResult())

	// The first try is as it was; the second is reduced; a second 400 is
	// fatal (it is not tried again), so only two were sent.
	if len(bodies) != 2 {
		t.Fatalf("%d finish calls, want 2 (one retry)", len(bodies))
	}
	size := func(b map[string]any) int { x, _ := json.Marshal(b["result"]); return len(x) }
	if size(bodies[1]) >= size(bodies[0])/4 {
		t.Errorf("the retry is %d bytes, the first %d", size(bodies[1]), size(bodies[0]))
	}
	var res struct {
		Findings []map[string]any `json:"findings"`
		Helpers  []helperRun      `json:"helpers"`
	}
	raw, _ := json.Marshal(bodies[1]["result"])
	if err := json.Unmarshal(raw, &res); err != nil || len(res.Findings) != 1 || len(res.Helpers) != 1 || len(res.Helpers[0].Cut) != 0 ||
		res.Helpers[0].Dropped != 40 || len(res.Helpers[0].Leads) != 5 || len([]rune(res.Helpers[0].Leads[0].Evidence)) > 81 {
		t.Errorf("the retried result: %v %+v", err, res)
	}
}
