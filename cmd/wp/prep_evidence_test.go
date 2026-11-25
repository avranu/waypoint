package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

const evidenceJSON = `{"evidence": [
	{"ref": "WP-X20", "kind": "page", "title": "Prep backtests", "category": "experiment", "date": "2026-11-17", "by": "agent, unreviewed", "bears_on": "how often picks are used"},
	{"ref": "WP-R3", "kind": "page", "title": "Model survey", "category": "reference", "date": "2026-10-02", "by": "person", "bears_on": "which models point"},
	{"ref": "WP-ADR89", "kind": "design", "title": "Gate on checks", "category": "decision", "date": "2026-09-30", "by": "reviewed by a person", "bears_on": "check times"}],
	"claims": [
	{"quote": "most merges fail", "test": "count why merges failed", "ref": "WP-X20", "ref_kind": "page"},
	{"quote": "checks are slow", "test": "time them for a week"},
	{"quote": "nobody reads reviews", "test": "count reads"}]}`

// Evidence prints after the notes, or alone for held-out pointers, two of
// each in the compact form; an agent's showing records the pages and
// decisions listed (WP-ADR157).
func TestRenderPrepEvidence(t *testing.T) {
	var paths, bodies []string
	prep := `{"key": "WP-12", "state": "ready", "held_out": true, "evidence": ` + evidenceJSON + `}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/knowledge/shown":
			b, _ := io.ReadAll(r.Body)
			var in struct {
				Via   string      `json:"via"`
				Items []shownItem `json:"items"`
			}
			_ = json.Unmarshal(b, &in)
			var refs []string
			for _, it := range in.Items {
				refs = append(refs, it.Type+":"+it.Ref+"@"+it.Project)
			}
			bodies = append(bodies, in.Via+" "+strings.Join(refs, ","))
			_, _ = w.Write([]byte("{}"))
		default:
			paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
			_, _ = w.Write([]byte(prep))
		}
	}))
	defer ts.Close()
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_SERVER", ts.URL)
	t.Setenv("WAYPOINT_TOKEN", "wpt_x")
	t.Setenv("WAYPOINT_AGENT", "anthropic/claude-code")
	t.Setenv("WP_NO_BRIEF", "")
	t.Chdir(t.TempDir())
	var printed bytes.Buffer
	c, _ := newCLI(&printed, &bytes.Buffer{})

	full := c.prepSection(nil, "WP-12", 10, false, true)
	want := `Recorded evidence that may bear on this task (matched by a weaker model):
  WP-X20 Prep backtests [experiment, 2026-11-17, agent, unreviewed] — bears on: how often picks are used
  WP-R3 Model survey [reference, 2026-10-02, person] — bears on: which models point
  WP-ADR89 Gate on checks [decision assumptions, 2026-09-30, reviewed by a person] — bears on: check times
Claims in the task text worth testing (picked by a weaker model):
  - "most merges fail" — test: count why merges failed (see WP-X20)
  - "checks are slow" — test: time them for a week
  - "nobody reads reviews" — test: count reads
  Read one: wp page show WP-X20 · wp design show WP-ADR89
Judge these before relying on them: could more or better data be collected, or better controls used? Check how and when each figure was measured. A claim listed here is not known to be wrong.
`
	// The notes' budget doesn't cut it, and there is no pointer header.
	if full != want {
		t.Errorf("full:\n%s\nwant:\n%s", full, want)
	}
	if len(paths) != 1 || paths[0] != "/api/v1/issues/WP-12/prep?evidence=1&shown=1" {
		t.Errorf("asked %q", paths)
	}
	if len(bodies) != 1 || bodies[0] != "prep page:WP-X20@WP,page:WP-R3@WP,design:WP-ADR89@WP" {
		t.Errorf("recorded %q", bodies)
	}

	paths, bodies = nil, nil
	compact := c.showPrep("WP-12")
	for _, w := range []string{"WP-X20 Prep backtests", "WP-R3 Model survey", "most merges fail", "checks are slow", "Read one: wp page show WP-X20\n", "Judge these"} {
		if !strings.Contains(compact, w) {
			t.Errorf("compact lacks %q:\n%s", w, compact)
		}
	}
	if strings.Contains(compact, "WP-ADR89") || strings.Contains(compact, "nobody reads") || strings.Contains(compact, "wp brief") {
		t.Errorf("compact prints too much:\n%s", compact)
	}
	if len(paths) != 1 || paths[0] != "/api/v1/issues/WP-12/prep?evidence=1&shown=1" {
		t.Errorf("wp show asked %q", paths)
	}
	if len(bodies) != 1 || bodies[0] != "prep page:WP-X20@WP,page:WP-R3@WP" {
		t.Errorf("compact recorded %q", bodies)
	}

	// Not taking the task: asked for, never claimed as shown to the experiment.
	paths = nil
	c.prepSection(nil, "WP-12", 100, true, false)
	if err := c.prepShow("WP-12"); err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if p != "/api/v1/issues/WP-12/prep?evidence=1" {
			t.Errorf("asked %q", p)
		}
	}
	if !strings.Contains(printed.String(), "WP-ADR89 Gate on checks") {
		t.Errorf("wp prep show:\n%s", printed.String())
	}

	// With pointers, it follows them.
	prep = `{"key": "WP-12", "state": "ready", "notes": {"files": [{"path": "merge.go", "why": "the gate"}]}, "evidence": ` + evidenceJSON + `}`
	both := c.prepSection(nil, "WP-12", 4000, false, true)
	if n, ev := strings.Index(both, "merge.go — the gate\n"), strings.Index(both, "\nRecorded evidence"); n < 0 || ev < n {
		t.Errorf("notes and evidence:\n%s", both)
	}

	// Sections without content are left out.
	one := func(raw string) string {
		var e prepEvidence
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			t.Fatal(err)
		}
		out, _ := renderPrepEvidence(&e, "WP", false)
		return out
	}
	if out := one(`{"claims": [{"quote": "checks are slow", "test": "time them"}]}`); strings.Contains(out, "Recorded evidence") || strings.Contains(out, "Read one") || !strings.Contains(out, "Claims in") || !strings.Contains(out, "Judge these") {
		t.Errorf("claims alone:\n%s", out)
	}
	// The server says what kind a claim's record is: its code doesn't.
	if out := one(`{"claims": [{"quote": "checks are slow", "test": "time them", "ref": "WP-D7", "ref_kind": "design"}]}`); !strings.Contains(out, "Read one: wp design show WP-D7\n") {
		t.Errorf("a claim's decision:\n%s", out)
	}
	// What was edited after it was matched says so.
	edited := one(`{"evidence": [{"ref": "WP-X20", "kind": "page", "title": "Backtests", "category": "experiment", "date": "2026-11-17", "by": "person", "changed_since": true},
		{"ref": "WP-ADR89", "kind": "design", "title": "Gate", "category": "decision", "date": "2026-09-30", "by": "person", "changed_since": true}],
		"claims": [{"quote": "checks are slow", "test": "time them", "ref": "WP-R3", "ref_kind": "page", "changed_since": true},
		{"quote": "most merges fail", "test": "count them", "ref": "WP-R4", "ref_kind": "page"}]}`)
	for _, w := range []string{"WP-X20 Backtests [experiment, 2026-11-17, person, edited since it was matched]\n",
		"WP-ADR89 Gate [decision assumptions, 2026-09-30, person, edited since it was matched]\n",
		"time them (see WP-R3, edited since it was matched)\n", "count them (see WP-R4)\n"} {
		if !strings.Contains(edited, w) {
			t.Errorf("edited lacks %q:\n%s", w, edited)
		}
	}
	if out := one(`{"evidence": [{"ref": "WP-ADR89", "kind": "design", "title": "Gate", "category": "decision", "date": "2026-09-30", "by": "person"}]}`); strings.Contains(out, "Claims in") || strings.Contains(out, "bears on") || !strings.Contains(out, "Read one: wp design show WP-ADR89\n") {
		t.Errorf("evidence alone:\n%s", out)
	}
	// A decision listed and a claim's page: a command reads each, and what a
	// claim names is recorded as shown, once.
	var e prepEvidence
	if err := json.Unmarshal([]byte(`{"evidence": [{"ref": "WP-ADR89", "kind": "design", "title": "Gate", "category": "decision", "date": "2026-09-30", "by": "person"}],
		"claims": [{"quote": "most merges fail", "test": "count them", "ref": "WP-R9", "ref_kind": "page"},
		{"quote": "checks are slow", "test": "time them", "ref": "WP-R9", "ref_kind": "page"},
		{"quote": "the gate is flaky", "test": "rerun it", "ref": "WP-ADR89", "ref_kind": "design"}]}`), &e); err != nil {
		t.Fatal(err)
	}
	out, items := renderPrepEvidence(&e, "WP", false)
	if !strings.Contains(out, "Read one: wp page show WP-R9 · wp design show WP-ADR89\n") {
		t.Errorf("a claim's page beside a decision:\n%s", out)
	}
	wantItems := []shownItem{{Type: "design", Ref: "WP-ADR89", Project: "WP"}, {Type: "page", Ref: "WP-R9", Project: "WP"}}
	if !slices.Equal(items, wantItems) {
		t.Errorf("shown = %+v", items)
	}
	if out := one(`{}`); out != "" {
		t.Errorf("nothing found: %q", out)
	}

	// A ready prep with neither notes nor evidence prints nothing.
	prep = `{"key": "WP-12", "state": "ready"}`
	if out := c.prepSection(nil, "WP-12", 4000, false, true); out != "" {
		t.Errorf("an empty prep: %q", out)
	}
}
